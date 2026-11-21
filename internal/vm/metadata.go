package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/kube"
)

const (
	metadataAddress = "http://169.254.169.254"

	azureInstancePath = "/metadata/instance?api-version=2021-02-01"

	awsTokenPath = "/latest/api/token"

	awsIdentityPath = "/latest/dynamic/instance-identity/document"

	awsSignaturePath = "/latest/dynamic/instance-identity/rsa2048"

	awsNamePath = "/latest/meta-data/tags/instance/Name"

	awsTokenHeader = "X-aws-ec2-metadata-token"

	probeTimeout = 2 * time.Second

	readTimeout = 10 * time.Second

	maxMetadataBytes = 1 << 20
)

var metadataClient = &http.Client{Transport: &http.Transport{}, Timeout: readTimeout}

type Machine struct {
	Cloud         string
	Name          string
	ResourceID    string
	Subscription  string
	ResourceGroup string
	Region        string
	Zone          string
	Size          string
	OSType        string
	OSOffer       string
	OSSku         string
	DataDisks     []string
	InstanceID    string
	Account       string
	NameTagged    bool
}

type azureInstance struct {
	Compute struct {
		ResourceID        string `json:"resourceId"`
		Name              string `json:"name"`
		Location          string `json:"location"`
		Zone              string `json:"zone"`
		VMSize            string `json:"vmSize"`
		OSType            string `json:"osType"`
		SubscriptionID    string `json:"subscriptionId"`
		ResourceGroupName string `json:"resourceGroupName"`
		StorageProfile    struct {
			DataDisks []struct {
				ManagedDisk struct {
					ID string `json:"id"`
				} `json:"managedDisk"`
			} `json:"dataDisks"`
			ImageReference struct {
				Offer string `json:"offer"`
				Sku   string `json:"sku"`
			} `json:"imageReference"`
		} `json:"storageProfile"`
	} `json:"compute"`
}

type awsIdentity struct {
	InstanceID       string `json:"instanceId"`
	Region           string `json:"region"`
	AccountID        string `json:"accountId"`
	AvailabilityZone string `json:"availabilityZone"`
	InstanceType     string `json:"instanceType"`
}

func readAzure(ctx context.Context) (*Machine, bool) {
	probe, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body, status, err := metadata(probe, http.MethodGet, azureInstancePath, map[string]string{"Metadata": "true"})
	if err != nil || status != http.StatusOK {
		return nil, false
	}
	var instance azureInstance
	if json.Unmarshal(body, &instance) != nil || instance.Compute.ResourceID == "" {
		return nil, false
	}
	compute := instance.Compute
	machine := &Machine{
		Cloud:         kube.CloudAzure,
		Name:          compute.Name,
		ResourceID:    compute.ResourceID,
		Subscription:  strings.ToLower(compute.SubscriptionID),
		ResourceGroup: compute.ResourceGroupName,
		Region:        compute.Location,
		Zone:          compute.Zone,
		Size:          compute.VMSize,
		OSType:        compute.OSType,
		OSOffer:       compute.StorageProfile.ImageReference.Offer,
		OSSku:         compute.StorageProfile.ImageReference.Sku,
	}
	for _, disk := range compute.StorageProfile.DataDisks {
		machine.DataDisks = append(machine.DataDisks, disk.ManagedDisk.ID)
	}
	return machine, true
}

func readAWS(ctx context.Context) (*Machine, bool) {
	probe, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	token, err := awsToken(probe)
	if err != nil {
		return nil, false
	}
	body, status, err := awsRead(ctx, token, awsIdentityPath)
	if err != nil || status != http.StatusOK {
		return nil, false
	}
	var identity awsIdentity
	if json.Unmarshal(body, &identity) != nil || identity.InstanceID == "" {
		return nil, false
	}
	machine := &Machine{
		Cloud:      kube.CloudAWS,
		Name:       identity.InstanceID,
		Region:     identity.Region,
		Zone:       identity.AvailabilityZone,
		Size:       identity.InstanceType,
		InstanceID: identity.InstanceID,
		Account:    identity.AccountID,
	}
	if name, status, err := awsRead(ctx, token, awsNamePath); err == nil && status == http.StatusOK &&
		strings.TrimSpace(string(name)) != "" {
		machine.Name = strings.TrimSpace(string(name))
		machine.NameTagged = true
	}
	return machine, true
}

func awsToken(ctx context.Context) (string, error) {
	body, status, err := metadata(ctx, http.MethodPut, awsTokenPath,
		map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "60"})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("the instance metadata service answered HTTP %d for a session token", status)
	}
	return string(body), nil
}

func awsRead(ctx context.Context, token, path string) ([]byte, int, error) {
	return metadata(ctx, http.MethodGet, path, map[string]string{awsTokenHeader: token})
}

func metadata(ctx context.Context, method, path string, headers map[string]string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, metadataAddress+path, nil)
	if err != nil {
		return nil, 0, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := metadataClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	return body, response.StatusCode, err
}
