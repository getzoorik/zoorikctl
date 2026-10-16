package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/refusal"
)

const InstallSetupPath = "/install/v1/setup"

const ConnectTokenPath = "/install/v1/connect-token"

const RequestIDHeader = "Zoorik-Request-Id"

const requestTimeout = 30 * time.Second

var ErrStatusRefused = errors.New("the control plane refused the connect token's status check")

type ConnectTokenStatus struct {
	Phase          string `json:"phase"`
	OrganizationID string `json:"orgId"`
	ClusterID      string `json:"clusterId"`
	ExpiresAt      string `json:"expiresAt"`
}

type AgentSetup struct {
	ImageRef       string `json:"imageRef"`
	ExpiresAt      string `json:"expiresAt"`
	OrganizationID string `json:"organizationId"`
	Setup          string `json:"setup"`
}

type Problem struct {
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: requestTimeout},
	}
}

func (c *Client) FetchAgentSetup(ctx context.Context) (*AgentSetup, error) {
	status, body, header, err := c.get(ctx, InstallSetupPath)
	if err != nil {
		return nil, err
	}
	requestID := requestIDLines(header)
	if status != http.StatusOK {
		return nil, refuse(status, body, requestID)
	}

	var agentSetup AgentSetup
	if err := json.Unmarshal(body, &agentSetup); err != nil {
		return nil, refusal.Wrap(refusal.CodeControlPlane, err,
			"The control plane answered with something that is not an agent setup.",
			"This usually means the address is answering for something else -- a proxy, a login "+
				"page, or the wrong host. Check --api-url.")
	}
	if strings.TrimSpace(agentSetup.Setup) == "" {
		return nil, refusal.New(refusal.CodeControlPlane,
			"The control plane returned an install with no documents in it.",
			"Nothing has been applied. Report this with the request id above: an empty agent setup is a "+
				"control-plane defect, not a cluster problem.").WithDetail(requestID...)
	}
	return &agentSetup, nil
}

func (c *Client) FetchConnectTokenStatus(ctx context.Context) (*ConnectTokenStatus, error) {
	status, body, header, err := c.get(ctx, ConnectTokenPath)
	if err != nil {
		return nil, err
	}
	requestID := requestIDLines(header)
	if status != http.StatusOK {
		return nil, refuseStatus(status, body, requestID)
	}

	var tokenStatus ConnectTokenStatus
	if err := json.Unmarshal(body, &tokenStatus); err != nil {
		return nil, refusal.Wrap(refusal.CodeControlPlane, err,
			"The control plane answered with something that is not an install status.",
			"This usually means the address is answering for something else -- a proxy, a login "+
				"page, or the wrong host. Check --api-url.")
	}
	if strings.TrimSpace(tokenStatus.Phase) == "" {
		return nil, refusal.New(refusal.CodeControlPlane,
			"The control plane answered the connect token's status without a phase.",
			"Nothing on the cluster is affected. Report this with the request id above.").WithDetail(requestID...)
	}
	return &tokenStatus, nil
}

const maxResponseBytes = 8 << 20

func (c *Client) get(ctx context.Context, path string) (int, []byte, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return 0, nil, nil, refusal.Wrap(refusal.CodeUsage, err,
			fmt.Sprintf("%q is not a usable control plane address.", c.BaseURL),
			"Check --api-url, or drop it and use --api-region.")
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", UserAgent())

	response, err := c.HTTP.Do(request)
	if err != nil {
		return 0, nil, nil, refusal.Wrap(refusal.CodeControlPlane, err,
			fmt.Sprintf("The control plane at %s could not be reached.", c.BaseURL),
			"Check network egress and any proxy this machine uses. This call goes out from the "+
				"machine you are typing on, not from the cluster.")
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, nil, refusal.Wrap(refusal.CodeControlPlane, err,
			"The control plane's answer was cut off before it finished.",
			"Run the command again. If it repeats, the connection is being interrupted between "+
				"this machine and the control plane.")
	}
	return response.StatusCode, body, response.Header, nil
}

func refuse(status int, body []byte, requestID []string) error {
	var problem Problem
	_ = json.Unmarshal(body, &problem)

	if problem.Code == "unauthenticated" {
		return refusal.New(refusal.CodeCredential,
			"The control plane will not accept this connect command's token.",
			"Open Connect cluster in the console, generate a fresh command, and run that "+
				"one. Nothing has been applied.").WithDetail(
			"A connect token is single-use and lasts an hour. It stops working when:",
			"  · an agent has already registered with it -- if a pod is running in the zoorik-agent "+
				"namespace, this cluster is CONNECTED and there is nothing to do here;",
			"  · it expired -- open the console and generate a fresh connect command;",
			"  · it was truncated on the way here -- check the whole zoorik_v1_ value made it onto "+
				"the command line, a line wrap in a chat window does exactly this.").
			WithDetail(requestID...)
	}

	detail := strings.TrimSpace(problem.Detail)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	next := "Nothing has been applied. If this persists, report it."
	if len(requestID) > 0 {
		next = "Nothing has been applied. If this persists, report it with the request id above."
	}
	return refusal.New(refusal.CodeControlPlane,
		fmt.Sprintf("The control plane refused to render this install (HTTP %d).", status),
		next).WithDetail(truncate(detail, 400)).WithDetail(requestID...)
}

func refuseStatus(status int, body []byte, requestID []string) error {
	var problem Problem
	_ = json.Unmarshal(body, &problem)

	if status == http.StatusUnauthorized || problem.Code == "unauthenticated" {
		return refusal.New(refusal.CodeAgentNotReady,
			"The control plane no longer answers for this connect token, so zoorikctl cannot confirm "+
				"that the agent registered.",
			"Nothing needs to be re-applied: the agent is installed and running. Look for this cluster "+
				"under Clusters in the console; if it is not listed within a few minutes, the agent's "+
				"log says what the control plane answered it.").
			WithKind(ErrStatusRefused).WithDetail(requestID...)
	}
	return refusal.New(refusal.CodeControlPlane,
		fmt.Sprintf("The control plane could not report the connect token's status (HTTP %d).", status),
		"Nothing needs to be re-applied: the agent is installed and running. Look for this cluster "+
			"under Clusters in the console.").WithDetail(requestID...)
}

const maxRequestIDLength = 64

func requestIDLines(header http.Header) []string {
	if requestID := strings.TrimSpace(header.Get(RequestIDHeader)); isRequestID(requestID) {
		return []string{"Request id: " + requestID}
	}
	return nil
}

func isRequestID(value string) bool {
	if value == "" || len(value) > maxRequestIDLength {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
