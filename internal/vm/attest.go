package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
)

const azureAttestedPath = "/metadata/attested/document?api-version=2020-09-01&nonce="

func Attest(ctx context.Context, machine *Machine, nonce string) (api.VMAttestation, error) {
	var attestation api.VMAttestation
	var err error
	if machine.Cloud == kube.CloudAzure {
		attestation, err = attestAzure(ctx, nonce)
	} else {
		attestation, err = attestAWS(ctx)
	}
	if err != nil {
		return api.VMAttestation{}, refusal.Wrap(refusal.CodeControlPlane, err,
			"This VM's metadata service did not hand out the signed document that proves which VM this is.",
			"Nothing has been changed. Run this again.")
	}
	return attestation, nil
}

func attestAzure(ctx context.Context, nonce string) (api.VMAttestation, error) {
	body, status, err := metadata(ctx, http.MethodGet, azureAttestedPath+url.QueryEscape(nonce),
		map[string]string{"Metadata": "true"})
	if err != nil {
		return api.VMAttestation{}, err
	}
	if status != http.StatusOK {
		return api.VMAttestation{}, fmt.Errorf("the attested document answered HTTP %d", status)
	}
	var attested struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &attested); err != nil {
		return api.VMAttestation{}, err
	}
	return api.VMAttestation{Signature: attested.Signature}, nil
}

func attestAWS(ctx context.Context) (api.VMAttestation, error) {
	token, err := awsToken(ctx)
	if err != nil {
		return api.VMAttestation{}, err
	}
	document, status, err := awsRead(ctx, token, awsIdentityPath)
	if err != nil {
		return api.VMAttestation{}, err
	}
	if status != http.StatusOK {
		return api.VMAttestation{}, fmt.Errorf("the identity document answered HTTP %d", status)
	}
	signature, status, err := awsRead(ctx, token, awsSignaturePath)
	if err != nil {
		return api.VMAttestation{}, err
	}
	if status != http.StatusOK {
		return api.VMAttestation{}, fmt.Errorf("the identity signature answered HTTP %d", status)
	}
	return api.VMAttestation{
		Document:  base64.StdEncoding.EncodeToString(document),
		Signature: strings.Join(strings.Fields(string(signature)), ""),
	}, nil
}
