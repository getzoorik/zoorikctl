package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"
)

type VMSetup struct {
	ExpiresAt        string  `json:"expiresAt"`
	OrganizationID   string  `json:"orgId"`
	OrganizationName string  `json:"orgName"`
	AttestationNonce string  `json:"attestationNonce"`
	InstallScript    string  `json:"installScript"`
	Grant            VMGrant `json:"grant"`
}

type VMGrant struct {
	Azure AzureVMGrant `json:"azure"`
	AWS   AWSVMGrant   `json:"aws"`
}

type AzureVMGrant struct {
	AppID       string    `json:"appId"`
	OrgTagKey   string    `json:"orgTagKey"`
	DiskRole    AzureRole `json:"diskRole"`
	CatalogRole AzureRole `json:"catalogRole"`
}

type AzureRole struct {
	BaseName    string   `json:"baseName"`
	Description string   `json:"description"`
	Actions     []string `json:"actions"`
}

type AWSVMGrant struct {
	RoleNamePrefix string `json:"roleNamePrefix"`
	PolicyName     string `json:"policyName"`
	VMTagKey       string `json:"vmTagKey"`
	TrustPolicy    string `json:"trustPolicy"`
	PolicyTemplate string `json:"policyTemplate"`
}

type VMRegistration struct {
	VMResourceID string `json:"vmResourceId"`
	Cloud        string `json:"cloud"`
	AgentKey     string `json:"agentKey"`
	ServerURL    string `json:"serverUrl"`
	Outcome      string `json:"outcome"`
}

type VMDetails struct {
	Cloud        string        `json:"cloud"`
	VMResourceID string        `json:"vmResourceId,omitempty"`
	VMName       string        `json:"vmName,omitempty"`
	Region       string        `json:"region,omitempty"`
	Zone         string        `json:"zone,omitempty"`
	VMSize       string        `json:"vmSize,omitempty"`
	OSName       string        `json:"osName,omitempty"`
	OSVersion    string        `json:"osVersion,omitempty"`
	OSOffer      string        `json:"osOffer,omitempty"`
	OSSku        string        `json:"osSku,omitempty"`
	Attestation  VMAttestation `json:"attestation"`
}

type VMAttestation struct {
	Document  string `json:"document,omitempty"`
	Signature string `json:"signature"`
}

type VMConnectStatus struct {
	Agent  string `json:"agent"`
	Access string `json:"access"`
}

func (c *Client) FetchVMSetup(ctx context.Context) (*VMSetup, error) {
	status, body, header, err := c.get(ctx, InstallSetupPath)
	if err != nil {
		return nil, err
	}
	requestID := requestIDLines(header)
	if status != http.StatusOK {
		return nil, refuseVM(status, body, requestID, "send this VM's setup", "")
	}
	var setup VMSetup
	if err := json.Unmarshal(body, &setup); err != nil || strings.TrimSpace(setup.InstallScript) == "" {
		return nil, refusal.New(refusal.CodeControlPlane,
			"The control plane answered with something that is not a VM setup.",
			"This usually means the address is answering for something else -- a proxy, a login page, or the "+
				"wrong host. Check --api-url. Nothing has been changed on this VM.").
			WithDetail(requestID...)
	}
	return &setup, nil
}

func (c *Client) RegisterVM(ctx context.Context, details VMDetails, expiresAt string) (*VMRegistration, error) {
	payload, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	status, body, header, err := c.post(ctx, RegisterPath, payload)
	if err != nil {
		return nil, err
	}
	requestID := requestIDLines(header)
	if status/100 != 2 {
		return nil, refuseVM(status, body, requestID, "register this VM", expiresAt)
	}
	var registered VMRegistration
	if err := json.Unmarshal(body, &registered); err != nil {
		return nil, refusal.New(refusal.CodeControlPlane,
			"The control plane's answer to registering this VM could not be read.",
			"The connect token may be spent: run the command from a new Connect a VM dialog, which replaces this "+
				"registration. If it repeats, report it with the request id above.").WithDetail(requestID...)
	}
	return &registered, nil
}

func (c *Client) FetchVMConnectStatus(ctx context.Context) (*VMConnectStatus, error) {
	status, body, header, err := c.get(ctx, ConnectTokenPath)
	if err != nil {
		return nil, err
	}
	requestID := requestIDLines(header)
	if status == http.StatusUnauthorized {
		return nil, refusal.New(refusal.CodeAgentNotReady,
			"The control plane no longer answers for this connect token, so zoorikctl cannot confirm that the "+
				"agent reported.",
			"The agent is installed. Look for this VM in the console; if it does not show as connected within a "+
				"few minutes, its log says why: `sudo journalctl -u amoeba-agent`.").
			WithKind(ErrStatusRefused).WithDetail(requestID...)
	}
	if status != http.StatusOK {
		return nil, refusal.New(refusal.CodeControlPlane,
			fmt.Sprintf("The control plane could not report this VM's status (HTTP %d).", status), "").
			WithDetail(requestID...)
	}
	var connect VMConnectStatus
	if err := json.Unmarshal(body, &connect); err != nil {
		return nil, refusal.New(refusal.CodeControlPlane,
			"The control plane answered with something that is not this VM's status.", "").
			WithDetail(requestID...)
	}
	return &connect, nil
}

func refuseVM(status int, body []byte, requestID []string, action, expiresAt string) error {
	var problem Problem
	_ = json.Unmarshal(body, &problem)
	detail := strings.TrimSpace(problem.Detail)
	unchanged := "Nothing has been changed on this VM."

	switch problem.Code {
	case "CONNECT_TOKEN_INVALID":
		return refusal.New(refusal.CodeCredential,
			"The control plane will not accept this connect token.",
			"In the console's Connect a VM dialog, choose Generate a new command and run the new lines. "+
				unchanged).WithDetail(
			"A connect token works once and lasts 60 minutes. It stops working when:",
			"  · a VM has already connected with it;",
			"  · it expired;",
			"  · it was cut short on the way here: check that the whole zoorik_v1_ value was pasted.").
			WithDetail(requestID...)
	case "VM_IN_ANOTHER_ORG":
		return refusal.New(refusal.CodeWrongCluster,
			"This VM is connected to another Zoorik organisation.",
			"A VM belongs to one organisation. Run the connect command from that organisation's console. "+
				unchanged).WithDetail(requestID...)
	case "VM_REPLACED":
		what := "This VM does not match the VM of this name that this organisation connected before (" +
			truncate(detail, 200) + ")."
		switch detail {
		case "rebuilt":
			what = "This organisation connected an earlier VM of this name, and this VM was created in its place."
		case "moved":
			what = "This organisation connected this VM in another resource group, and it has moved since."
		}
		return refusal.New(refusal.CodeWrongCluster, what,
			"Contact Zoorik support to close the earlier record, then run the command from a new Connect a VM "+
				"dialog. "+unchanged).WithDetail(requestID...)
	case "VM_BUSY":
		return refusal.New(refusal.CodeControlPlane,
			"A migration step is running on this VM.",
			"Try again in a few minutes; this command works until "+expiresAt+".").WithDetail(requestID...)
	case "VM_IDENTITY_UNPROVEN":
		return refusal.New(refusal.CodeControlPlane,
			"The control plane could not prove that this is the VM it says it is: "+truncate(detail, 400)+".",
			unchanged+" Run this again; if it repeats, report it with the request id above.").
			WithDetail(requestID...)
	}

	failed := refusal.New(refusal.CodeControlPlane,
		fmt.Sprintf("The control plane refused to %s (HTTP %d).", action, status),
		unchanged+" If this persists, report it with the request id above.")
	if detail != "" {
		failed = failed.WithDetail(truncate(detail, 400))
	}
	return failed.WithDetail(requestID...)
}
