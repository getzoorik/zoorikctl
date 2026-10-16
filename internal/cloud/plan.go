package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/kube"
)

const (
	ProvisionerNAP = "AKS Node Auto Provisioning"

	ProvisionerKarpenter = "Karpenter"

	ProvisionerAutoMode = "EKS Auto Mode"

	ProvisionerClusterAutoscaler = "A cluster autoscaler"

	crdWait = 5 * time.Minute

	crdPoll = 5 * time.Second

	stepSettleWait = time.Minute

	NodeDeletionTitle = "node deletion"

	ViaAzureWorkloadIdentity = "Azure workload identity"

	ViaPodIdentity = "EKS Pod Identity"

	ViaIRSA = "IRSA"

	agentIdentityPrefix = "zoorik-agent-"

	agentCredentialName = "zoorik-agent"

	clusterTag = "zoorik-cluster"

	nodeRemovalRole = "Zoorik Node Removal v2"

	legacyNodeRemovalRole = "Zoorik Node Removal v1"

	nodeRemovalPolicyName = "ZoorikNodeRemoval"

	originalMinimumTag = "coral.zoorik.com/original-min"

	loweredMinimumTag = "coral.zoorik.com/lowered-min"

	aksOriginalMinimumTag = "coral.zoorik.com_original-min"

	aksLoweredMinimumTag = "coral.zoorik.com_lowered-min"

	azureIdentityNameLimit = 128

	tokenExchangeAudience = "api://AzureADTokenExchange"

	stsAudience = "sts.amazonaws.com"

	tagValueLimit = 256

	hashedTagPrefix = "sha256:"
)

type Step struct {
	Title           string
	Why             string
	Commands        []string
	NotInPlace      string
	Done            bool
	RestoresMinimum bool
	Upgrade         bool
	Check           func(ctx context.Context) (bool, error)
	Apply           func(ctx context.Context, note func(string)) error
}

type Blocker struct {
	What    string
	Remedy  string
	Tooling bool
}

type Plan struct {
	Cloud           string
	Provisioner     string
	Present         bool
	Cluster         string
	Credential      string
	Found           []string
	Blockers        []Blocker
	Notes           []string
	Steps           []Step
	AutoscaledPools []string
	Poolless        bool
}

type Binding struct {
	Identity string
	Via      string
	Annotate bool
	Earlier  bool
}

func (b Binding) InjectedEnv() (string, string) {
	switch b.Via {
	case ViaAzureWorkloadIdentity:
		return "AZURE_CLIENT_ID", b.Identity
	case ViaIRSA:
		return "AWS_ROLE_ARN", b.Identity
	}
	return "AWS_CONTAINER_CREDENTIALS_FULL_URI", ""
}

type NodeDeletion struct {
	Plan
	Earlier string
	Bind    func(ctx context.Context) (Binding, error)
}

func (d NodeDeletion) KeepsEarlier() bool {
	if d.Earlier == "" {
		return false
	}
	pending := d.Pending()
	for _, step := range pending {
		if !step.Upgrade {
			return false
		}
	}
	return len(pending) > 0
}

type Input struct {
	Target   *kube.Target
	Found    kube.Autoprovisioner
	Resource *kube.ClusterResource
	Kept     *kube.ClusterResource
	Region   string
	Occupant string
}

type purpose struct {
	aim     string
	allowed string
	removal bool
}

var (
	aksProvisioner = purpose{aim: "Node Auto Provisioning cannot be turned on", allowed: "update the cluster"}

	aksDefaultPools = purpose{aim: "Node Auto Provisioning's default NodePools cannot be turned on",
		allowed: "update the cluster"}

	aksNodeDeletion = purpose{aim: "node deletion cannot be granted", allowed: "create managed identities, " +
		"custom roles and role assignments in the cluster's subscription (Owner, or Contributor with User Access " +
		"Administrator)"}

	aksRemoval = purpose{aim: "the cloud changes zoorikctl made for node deletion cannot be looked for",
		allowed: "update the cluster's agent pools and delete managed identities and role assignments in the " +
			"cluster's subscription", removal: true}

	eksProvisioner = purpose{aim: "Karpenter cannot be installed", allowed: "create IAM roles, SQS queues and " +
		"EventBridge rules and update the cluster"}

	eksBuiltInPools = purpose{aim: "EKS Auto Mode's built-in NodePools cannot be turned on",
		allowed: "update the cluster and pass its node role to EKS"}

	eksNodeDeletion = purpose{aim: "node deletion cannot be granted", allowed: "create IAM roles and EKS pod " +
		"identity associations in the cluster's account"}

	eksRemoval = purpose{aim: "the cloud changes zoorikctl made for node deletion cannot be looked for",
		allowed: "update node groups and Auto Scaling groups and delete IAM roles and EKS pod identity " +
			"associations in the cluster's account", removal: true}
)

func (s Step) InPlace(ctx context.Context) (bool, error) {
	return waitWithin(ctx, stepSettleWait, func() (bool, error) { return s.Check(ctx) })
}

func (s Step) NotInPlaceMessage() string {
	if s.NotInPlace != "" {
		return s.NotInPlace
	}
	return "\"" + s.Title + "\" was applied but is not in place"
}

func kubeCredential(target *kube.Target) string {
	return "your kubeconfig credentials (context " + target.ContextName + ")"
}

func AgentSubject() string {
	return "system:serviceaccount:" + kube.AgentNamespace + ":" + kube.AgentServiceAccount
}

func (p *Plan) block(what, remedy string) {
	p.Blockers = append(p.Blockers, Blocker{What: what, Remedy: remedy})
}

func (p *Plan) blockTooling(what, remedy string) {
	p.Blockers = append(p.Blockers, Blocker{What: what, Remedy: remedy, Tooling: true})
}

func (p *Plan) note(text string) {
	p.Notes = append(p.Notes, text)
}

func (p Plan) ToolingBlocked() bool {
	for _, blocker := range p.Blockers {
		if !blocker.Tooling {
			return false
		}
	}
	return len(p.Blockers) > 0
}

func (p Plan) Pending() []Step {
	var pending []Step
	for _, step := range p.Steps {
		if !step.Done {
			pending = append(pending, step)
		}
	}
	return pending
}

func (p Plan) RestoresMinimums() bool {
	for _, step := range p.Pending() {
		if step.RestoresMinimum {
			return true
		}
	}
	return false
}

func (p Plan) Settled() []Step {
	var settled []Step
	for _, step := range p.Steps {
		if step.Done {
			settled = append(settled, step)
		}
	}
	return settled
}

func boundedName(prefix, cluster string, limit int) string {
	name := prefix + cluster
	if len(name) <= limit {
		return name
	}
	sum := sha256.Sum256([]byte(cluster))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	keep := limit - len(prefix) - len(suffix)
	if keep < 1 {
		keep = 1
	}
	return prefix + cluster[:keep] + suffix
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(value)))
	return hex.EncodeToString(sum[:])[:8]
}

func clusterTagValue(id string) string {
	if len(id) <= tagValueLimit {
		return id
	}
	sum := sha256.Sum256([]byte(strings.ToLower(id)))
	return hashedTagPrefix + hex.EncodeToString(sum[:])
}

func taggedFor(tag, id string) bool {
	return tag != "" && strings.EqualFold(tag, clusterTagValue(id))
}

func serverHost(server string) string {
	parsed, err := url.Parse(strings.TrimSpace(server))
	if err != nil || parsed.Hostname() == "" {
		return strings.ToLower(strings.TrimSpace(server))
	}
	return strings.ToLower(parsed.Hostname())
}

func again(removal bool) string {
	if removal {
		return "run this disconnect again"
	}
	return "run this again"
}

func wrongCluster(removal bool) string {
	if removal {
		return "zoorikctl looks only at the cloud objects of the cluster it is connected to. Nothing has been changed."
	}
	return "The cluster resource came from --cluster-resource-id or from the installed agent's configuration. Pass " +
		"--cluster-resource-id with the id of the cluster this context reaches, and run this again. Nothing has " +
		"been changed."
}

func waitFor(ctx context.Context, what func() (bool, error)) (bool, error) {
	return waitWithin(ctx, crdWait, what)
}

func waitWithin(ctx context.Context, limit time.Duration, what func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(limit)
	for {
		done, err := what()
		if err != nil || done {
			return done, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(crdPoll):
		}
	}
}
