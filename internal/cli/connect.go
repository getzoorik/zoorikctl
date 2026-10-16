package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/cloud"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/setup"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type connectOptions struct {
	commonOptions
	ConnectToken         string
	APIRegion            string
	APIURL               string
	ClusterName          string
	ClusterResourceID    string
	ClusterResourceGiven bool
	CloudIdentity        string
	WebhookHostNetwork   bool
}

const agentWait = 3 * time.Minute

var (
	azureClientID = regexp.MustCompile(`^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
	iamRoleARN    = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
)

func parseConnect(args []string, out, errOut io.Writer) (*connectOptions, error) {
	options := &connectOptions{}
	set := flag.NewFlagSet("zoorikctl cluster connect", flag.ContinueOnError)
	commonFlags(set, &options.commonOptions)
	set.StringVar(&options.ConnectToken, "connect-token", "", "connect token from the Connect dialog")
	set.StringVar(&options.APIRegion, "api-region", "us", "which control plane to ask")
	set.StringVar(&options.APIURL, "api-url", "", "control plane address, instead of --api-region")
	set.StringVar(&options.ClusterName, "cluster-name", "",
		"the name the console shows for this cluster, unless it is an AKS cluster whose node "+
			"resource group is the managed MC_ one, which shows its AKS name; defaults to the "+
			"kubeconfig's cluster name")
	set.StringVar(&options.ClusterResourceID, "cluster-resource-id", "",
		"the AKS managed cluster's ARM id or the EKS cluster's ARN; names the console's project "+
			"(resource group) and provider name when the node resource group cannot")
	set.StringVar(&options.CloudIdentity, "cloud-identity-client-id", "",
		"the Azure managed identity's client id, or the IAM role ARN for IRSA, that the agent "+
			"removes nodes with")
	set.BoolVar(&options.WebhookHostNetwork, "webhook-host-network", false,
		"run the pod webhook on the nodes' host network, for a control plane that cannot reach pod IPs")
	if parsed, err := parseCommon(set, args, out, errOut, &options.commonOptions); !parsed {
		return nil, err
	}
	if strings.TrimSpace(options.ConnectToken) == "" {
		return nil, usageError(set.Name(), "--connect-token is required. It is the zoorik_v1_ connect token in "+
			"the Connect dialog's command, and it is the only credential this install needs.")
	}
	options.CloudIdentity = strings.TrimSpace(options.CloudIdentity)
	if options.CloudIdentity != "" && cloudIdentityAnnotation(options.CloudIdentity) == "" {
		return nil, usageError(set.Name(), fmt.Sprintf("--cloud-identity-client-id=%q is neither an Azure "+
			"managed identity's client id (a GUID) nor an IAM role ARN "+
			"(arn:aws:iam::<account>:role/<name>).", options.CloudIdentity))
	}
	options.ClusterResourceID = strings.TrimSpace(options.ClusterResourceID)
	options.ClusterResourceGiven = options.ClusterResourceID != ""
	if options.ClusterResourceGiven {
		if _, ok := kube.ParseClusterResource(options.ClusterResourceID); !ok {
			return nil, usageError(set.Name(), fmt.Sprintf("--cluster-resource-id=%q is neither an AKS managed "+
				"cluster's ARM id (/subscriptions/<subscription>/resourceGroups/<resource-group>/"+
				"providers/Microsoft.ContainerService/managedClusters/<name>) nor an EKS cluster's ARN "+
				"(arn:aws:eks:<region>:<account>:cluster/<name>).", options.ClusterResourceID))
		}
	}
	return options, nil
}

func runConnect(ctx context.Context, args []string, s *session) error {
	options, err := parseConnect(args, s.streams.Out, s.streams.Err)
	if err != nil || options == nil {
		return err
	}

	out := s.out
	out.Banner("Zoorik Onboarding")

	baseURL, err := api.ResolveBaseURL(options.APIRegion, options.APIURL)
	if err != nil {
		return err
	}
	target, err := kube.Load(options.Kubeconfig, options.KubeContext)
	if err != nil {
		return err
	}
	warnings := &kubeWarnings{out: out, seen: map[string]bool{}}
	target.OnWarning = warnings.add

	out.Section("Configuration")
	out.Success("Kubeconfig loaded.")
	out.Field("Zoorik Region", regionLabel(options))
	out.Field("Control plane", baseURL)
	out.Field("Kube context", target.ContextName)
	out.Field("Cluster", text.OrNone(clusterNameFor(options, target)))
	if strings.TrimSpace(options.ClusterName) != "" && target.ClusterName != "" &&
		strings.TrimSpace(options.ClusterName) != target.ClusterName {
		out.Field("Kubeconfig cluster", target.ClusterName)
	}
	if options.ClusterResourceID != "" {
		out.Field("Cluster resource", options.ClusterResourceID)
	}
	if options.CloudIdentity != "" {
		out.Field("Cloud identity", options.CloudIdentity)
	}
	out.Field("API server", target.ServerURL)
	out.Field("User", text.OrNone(target.UserName))
	out.Field("Kubeconfig", text.OrNone(target.Origin))

	out.Section("API Verification")
	spinner := out.Spin("Verifying the connect token...")
	client := api.New(baseURL, options.ConnectToken)
	install, err := client.FetchAgentSetup(ctx)
	if err != nil {
		spinner.Fail("Connect token could not be verified")
		return err
	}
	spinner.Success("Connect token verified")
	out.Field("Organization ID", text.OrText(install.OrganizationID,
		"none yet; one is created when the agent registers"))
	out.Field("Agent image", install.ImageRef)

	docs, err := setup.Split(install.Setup)
	if err != nil {
		return err
	}
	if name := clusterNameFor(options, target); name != "" {
		setAgentConfig(docs, kube.AgentClusterNameKey, name)
	}
	if registry := kube.AgentRegistry(install.ImageRef); registry != "" {
		setReleaseParam(docs, kube.AgentRegistryKey, registry)
	} else {
		out.Warn("The agent image is not in a <registry>/coral/agent repository, so the agent " +
			"cannot move itself to a new release")
	}
	if registry := kube.NodeRegistry(docs); registry != "" {
		setReleaseParam(docs, kube.NodeRegistryKey, registry)
	}
	liveMigration := kube.SetupHasLiveMigration(docs)
	out.Field("Live migration", liveMigrationLabel(liveMigration))
	if options.ClusterResourceID != "" {
		setAgentConfig(docs, kube.AgentClusterResourceIDKey, options.ClusterResourceID)
	}
	if options.CloudIdentity != "" {
		bindCloudIdentity(docs, options.CloudIdentity)
	}
	var hostNetwork []string
	if options.WebhookHostNetwork {
		if hostNetwork, err = kube.RenderWebhookHostNetwork(docs); err != nil {
			return err
		}
	}
	summary := setup.Summarize(docs)
	printPlan(out, target, summary, options.DryRun)
	printWebhookPlan(out, summary, hostNetwork)
	printLiveMigrationPlan(out, liveMigration)

	out.Section("Cluster Access")
	connecting := "Connecting to " + target.ServerURL + "..."
	if target.UsesCredentialPlugin() {
		spinner = out.Pending(connecting)
		out.Detail("Your kubeconfig signs in through a credential plugin; answer any sign-in prompt " +
			"it shows.")
	} else {
		spinner = out.Spin(connecting)
	}
	if err := target.Connect(); err != nil {
		spinner.Fail("Cluster could not be reached")
		return err
	}
	spinner.Success("Cluster access verified.")

	spinner = out.Spin("Identifying the cluster...")
	identity, err := kube.Identify(ctx, target)
	if err != nil {
		spinner.Fail("Cluster could not be identified")
		return err
	}
	spinner.Success("Cluster identified")
	out.Field("Provider", providerName(identity.Cloud))
	if identity.Region != "" {
		out.Field("Region", identity.Region)
	}
	if err := requirePinMatches(options.ClusterResourceID, identity); err != nil {
		return err
	}
	out.Detail(upperFirst(identity.Evidence) + ".")
	warnings.flush()

	out.Section("Preflight Checks")
	out.Success(nodeCount(identity))
	if err := adaptToPlatform(ctx, out, target, docs, options); err != nil {
		return err
	}
	occupant, occupantErr := kube.InspectOccupant(ctx, target)
	if occupantErr != nil {
		occupant = kube.Occupant{}
		out.Warn("The existing agent configuration could not be read, so nothing it holds is kept")
		out.Detail("Reason: " + text.OneLine(occupantErr.Error()) + ". Pass --cluster-resource-id or " +
			"--cloud-identity-client-id to set either again.")
	}
	dropped := ""
	if occupant.Present {
		out.Warn("Existing Zoorik agent found" + occupantSuffix(occupant))
		out.Detail("Applying re-enrols it under this connect token and restarts it with the new " +
			"one; nothing is removed.")
		if kept := keptClusterResource(options, occupant); kept != "" {
			if reason := unkeptReason(kept, identity); reason != "" {
				out.Warn("The existing install's cluster resource " + reason)
				dropped = kept
			} else {
				setAgentConfig(docs, kube.AgentClusterResourceIDKey, kept)
				options.ClusterResourceID = kept
				out.Detail("Cluster resource kept from the existing install: " + kept + ".")
			}
		}
	} else if occupantErr == nil {
		out.Success("No existing Zoorik agent found")
	}
	grant := grantAtConnect(ctx, s, target, identity, occupant, options)
	switch {
	case grant.Binding != nil && grant.Binding.Annotate:
		bindCloudIdentity(docs, grant.Binding.Identity)
		options.CloudIdentity = grant.Binding.Identity
	case grant.Binding == nil && occupant.Present:
		if kept := keptCloudIdentity(options, occupant); kept != "" {
			bindCloudIdentity(docs, kept)
			options.CloudIdentity = kept
			out.Detail("Cloud identity kept from the existing install: " + kept + ".")
		}
	}
	if grant.Cluster != "" && !options.ClusterResourceGiven &&
		!strings.EqualFold(options.ClusterResourceID, grant.Cluster) {
		setAgentConfig(docs, kube.AgentClusterResourceIDKey, grant.Cluster)
		if options.ClusterResourceID != "" {
			out.Detail("Cluster resource " + options.ClusterResourceID + " replaced by " + grant.Cluster +
				", the cluster node deletion was granted on.")
		} else {
			out.Detail("Cluster resource set to " + grant.Cluster + ", the cluster node deletion was granted on.")
		}
		options.ClusterResourceID = grant.Cluster
	}
	if options.DryRun {
		warnings.flush()
		rendered, err := setup.Join(docs)
		if err != nil {
			return err
		}
		out.Section("Dry Run")
		out.Info("Nothing was changed on your cluster or in your cloud. The agent setup follows, adapted to " +
			"this cluster.")
		out.Detail("After applying, a connect deletes every ClusterRole, ClusterRoleBinding, Role and " +
			"RoleBinding labelled " + kube.PartOfSelector() + " that this setup does not contain, then " +
			"every such ValidatingAdmissionPolicy and binding, and every such Service, ConfigMap, DaemonSet " +
			"and CustomResourceDefinition it does not contain. `" + kube.ListGrantsCommand(target) + "`, `" +
			kube.ListPoliciesCommand(target) + "` and `" + kube.ListObjectsCommand(target) +
			"` list the ones on your cluster now.")
		out.Blank()
		out.Raw(strings.TrimRight(rendered, "\n"))
		return nil
	}
	if grant.Binding != nil && !grant.Binding.Annotate && occupant.CloudIdentityClientID != "" {
		if err := kube.RemoveCloudIdentityAnnotations(ctx, target); err != nil {
			return refusal.Wrap(refusal.CodeApply, err,
				"The existing install's cloud identity could not be removed from its service account.",
				"Nothing has been applied. Run this again.")
		}
		out.Detail("Cloud identity removed from the existing install: " + occupant.CloudIdentityClientID + ".")
	}
	if dropped != "" {
		if err := kube.RemoveAgentConfigKey(ctx, target, kube.AgentClusterResourceIDKey); err != nil {
			return refusal.Wrap(refusal.CodeApply, err,
				"The existing install's cluster resource could not be removed.",
				"Nothing has been applied. Run this again.")
		}
		out.Detail("Cluster resource removed from the existing install: " + dropped +
			". Pass --cluster-resource-id with this cluster's own id to set one.")
	}
	warnings.flush()

	out.Section("Install Zoorik Agent")
	total := len(docs)
	spinner = out.Spin(fmt.Sprintf("Installing the Zoorik agent (0/%d)...", total))
	results, err := kube.Apply(ctx, target, docs, func(result kube.Result, done int) {
		spinner.Note(fmt.Sprintf("%-10s %s", verb(result.Created), result.Object))
		spinner.Tick(fmt.Sprintf("Installing the Zoorik agent (%d/%d)...", done, total))
	})
	if err != nil {
		spinner.Fail(fmt.Sprintf("Agent install stopped after %d of %d object(s)", len(results), total))
		if dropped != "" {
			out.Item("removed " + kube.AgentClusterResourceIDKey + " from " + kube.AgentNamespace +
				"/" + kube.AgentConfigMap + " before the install stopped")
		}
		for _, result := range results {
			out.Item(verb(result.Created) + " " + result.Object.String())
		}
		warnings.flush()
		return err
	}
	created := createdCount(results)
	spinner.Success(fmt.Sprintf("Installed the Zoorik agent: %d object(s), %d created, %d updated",
		len(results), created, len(results)-created))
	pruned := pruneStaleGrants(ctx, out, target, docs)
	pruneStaleObjects(ctx, out, target, docs)
	if !liveMigration {
		if pruned {
			removeStaleNodeDaemon(ctx, out, target)
		} else {
			out.Warn("The live-migration daemon from an earlier install was left in place")
			out.Detail("Its admission policies could not all be removed first, and removing its " +
				"namespace before them would stop pods being created in it. Run this command again.")
		}
	}
	warnings.flush()

	out.Section("Wait For Cluster")
	spinner = out.Spin("Waiting for the agent to start...")
	waitCtx, cancel := context.WithTimeout(ctx, agentWait+30*time.Second)
	defer cancel()
	if err := kube.WaitForAgent(waitCtx, target, agentWait, spinner.Note); err != nil {
		spinner.Fail("The agent did not start")
		return err
	}
	spinner.Success("Agent is running")
	if grant.Binding != nil {
		if err := ensureCredentialsInjected(ctx, out, target, *grant.Binding, false, agentWait,
			clusterCommand(grantVerb, target, options.Kubeconfig, options.ClusterResourceID)); err != nil {
			return err
		}
	}

	if kube.SetupHasWebhook(docs) {
		if err := waitForWebhook(ctx, out, target, options); err != nil {
			return err
		}
	}

	spinner = out.Spin("Waiting for the agent to register...")
	status, err := waitForRedemption(ctx, target, client.FetchConnectTokenStatus, install.ExpiresAt,
		agentWait, redemptionPollInterval, spinner.Note)
	if err != nil {
		switch {
		case errors.Is(err, api.ErrStatusRefused), errors.Is(err, errRegistrationUnconfirmed):
			spinner.Warn("Registration could not be confirmed")
		case errors.Is(err, errConnectTokenExpired):
			spinner.Fail("The agent did not register")
		default:
			spinner.Fail("The agent has not registered yet")
		}
		return err
	}
	spinner.Success("Cluster registered: " + status.ClusterID)
	out.Success("Cluster is ready in the Zoorik console")
	warnings.flush()

	printComplete(out, options, target, summary, status, grant.Unfinished)
	printGrantLater(out, grant)
	return nil
}

type connectGrant struct {
	Binding    *cloud.Binding
	Cluster    string
	Later      []string
	Unfinished bool
}

func grantAtConnect(ctx context.Context, s *session, target *kube.Target, identity kube.Identity,
	occupant kube.Occupant, options *connectOptions) connectGrant {
	if identity.Cloud == kube.CloudLocal {
		return connectGrant{}
	}
	out := s.out
	out.Section("Cloud Setup")
	out.Paragraph("", "Zoorik only removes nodes: the cluster's own provisioner or cluster autoscaler adds them "+
		"for Pending pods. A cluster with neither gets its cloud's own provisioner now, and a provisioner with no "+
		"NodePool of the cluster's own gets its default NodePools. Node optimization removes a node only after it "+
		"has cordoned and emptied it, and on many node pools that last step, or the pool's minimum, needs a cloud "+
		"permission the agent cannot grant itself. Each is set up now with your own cloud sign-in and asked for on "+
		"its own; Zoorik never creates a machine or raises a pool's size.")
	var resource, kept *kube.ClusterResource
	if options.ClusterResourceGiven {
		resource = clusterResourceOf(options.ClusterResourceID)
	} else {
		kept = clusterResourceOf(options.ClusterResourceID)
	}
	prepareLater := clusterCommand(prepareVerb, target, options.Kubeconfig, options.ClusterResourceID)
	grantLater := clusterCommand(grantVerb, target, options.Kubeconfig, options.ClusterResourceID)
	provisionerLeft := "Until the cluster has a provisioner or a cluster autoscaler, nothing adds a node for a " +
		"Pending pod."
	additionsLeft := "The cluster's provisioner adds nodes for Pending pods as before; what Zoorik adds to it for " +
		"live migration and rebalancing is not there yet."
	poollessLeft := "The cluster's provisioner has no NodePool of the cluster's own, so nothing adds a node for a " +
		"Pending pod yet; what Zoorik adds to it for live migration and rebalancing is not there yet either."
	deletionLeft := "Node pools a provisioner or the cluster autoscaler manages are consolidated as before; a " +
		"fixed-size node pool is not, and the planner refuses it with NO_REMOVER rather than leave a node cordoned."

	var grant connectGrant
	found, err := kube.DetectAutoprovisioner(ctx, target)
	var provisioner cloud.Plan
	if err == nil {
		provisioner = planProvisioner(ctx, out, target, identity, found, resource, kept)
	}
	switch {
	case err != nil:
		grant.notDone(out, "The node provisioner was not set up", err, provisionerLeft, prepareLater)
	case provisioner.Present && len(provisioner.Steps) == 0 && len(provisioner.Blockers) == 0:
		out.Success(provisioner.Provisioner + " is already on this cluster; connect leaves it as it is")
		warnRebalancingUnavailable(out, provisioner)
		printNotes(out, provisioner.Notes)
	case provisioner.Present:
		left := additionsLeft
		if provisioner.Poolless {
			out.Warn(provisioner.Provisioner + " is on, but the cluster has no NodePool of its own")
			left = poollessLeft
		} else {
			out.Success(provisioner.Provisioner + " is already on this cluster")
		}
		grant.settle(ctx, s, cloud.NodeDeletion{Plan: provisioner}, options, provisionerConsent, left, prepareLater)
	case len(provisioner.Blockers) > 0:
		printFound(out, provisioner)
		grant.notDone(out, provisioner.Provisioner+" was not set up", planBlocked(provisioner), provisionerLeft,
			prepareLater)
	default:
		grant.settle(ctx, s, cloud.NodeDeletion{Plan: provisioner}, options, provisionerConsent, provisionerLeft,
			prepareLater)
	}
	if options.CloudIdentity == "" && ctx.Err() == nil {
		deletion := planNodeDeletion(ctx, out, target, identity, resource, kept, occupant.CloudIdentityClientID)
		if len(deletion.Blockers) > 0 {
			printFound(out, deletion.Plan)
			grant.notDone(out, "Node deletion was not granted", planBlocked(deletion.Plan), deletionLeft, grantLater)
		} else {
			grant.settle(ctx, s, deletion, options, connectDeletionConsent, deletionLeft, grantLater)
		}
	}
	if provisioner.Provisioner == cloud.ProvisionerKarpenter {
		reportStaticCapacity(ctx, out, target, prepareLater)
	}
	grant.Unfinished = slices.Contains(grant.Later, prepareLater)
	return grant
}

func (g *connectGrant) settle(ctx context.Context, s *session, plan cloud.NodeDeletion, options *connectOptions,
	ask consent, left, later string) {
	binding, err := settleNodeDeletion(ctx, s, plan, options.Yes, options.DryRun, ask)
	if err != nil {
		g.notDone(s.out, upperFirst(plan.Provisioner)+" not set up", err, left, later)
		return
	}
	switch {
	case binding != nil && binding.Earlier:
		s.out.Success("Node deletion granted through " + binding.Via + ": " + binding.Identity)
		s.out.Detail("To let Zoorik lower node pool minimums too, run:")
		s.out.Dim("$ " + later)
	case binding != nil:
		s.out.Success("Zoorik may now remove spare machines and adjust node pool sizes in " +
			text.OrText(plan.Cluster, "this cluster") + " (through " + binding.Via + ": " + binding.Identity + ")")
	}
	if binding != nil {
		g.Binding = binding
		g.Cluster = plan.Cluster
	}
}

func reportStaticCapacity(ctx context.Context, out *ui.Printer, target *kube.Target, prepareLater string) {
	state, err := kube.ReadKarpenterStaticCapacity(ctx, target)
	if err == nil && state.Ready() {
		out.Success("Karpenter runs with static capacity on and its NodePools serve spec.replicas, so Coral's " +
			"rebalancing can create its new nodes")
		return
	}
	var found kube.Autoprovisioner
	if err == nil {
		found, err = kube.DetectAutoprovisioner(ctx, target)
	}
	if err != nil {
		out.Warn("Whether Karpenter runs with static capacity could not be read: " + whatOf(err))
		out.Detail("Coral's rebalancing creates its new nodes only with static capacity on. Check that the " +
			"context's credential may run `" + target.Kubectl("get deployments -A") + "` and `" +
			target.Kubectl("get --raw "+kube.KarpenterOpenAPIPath) + "`, then run:")
		out.Dim("$ " + prepareLater)
		return
	}
	remedy, missing := cloud.StaticCapacityRemedy(target, found, state)
	if !missing {
		return
	}
	out.Warn(cloud.StaticCapacityNotInPlace)
	out.Detail(remedy + " Node optimization and workload optimization work as before.")
}

func printFound(out *ui.Printer, plan cloud.Plan) {
	for _, line := range plan.Found {
		out.Dim(line)
	}
	printNotes(out, plan.Notes)
}

func (g *connectGrant) notDone(out *ui.Printer, what string, err error, left string, later ...string) {
	reason := whatOf(err)
	var stopped *declined
	if errors.As(err, &stopped) {
		reason = "you did not agree to the cloud changes"
	}
	out.Warn(what + ": " + reason)
	if cause := causeOf(err); !strings.Contains(reason, cause) {
		out.Dim(cause)
	}
	out.Detail("The install goes ahead. " + left + " To finish this later, run:")
	for _, command := range later {
		out.Dim("$ " + command)
	}
	g.Later = append(g.Later, later...)
}

func printGrantLater(out *ui.Printer, grant connectGrant) {
	if len(grant.Later) == 0 {
		return
	}
	out.Warn("The cloud setup is not finished yet")
	for _, command := range grant.Later {
		out.Dim("$ " + command)
	}
}

func pruneStaleGrants(ctx context.Context, out *ui.Printer, target *kube.Target,
	installed []setup.Document) bool {
	stale, err := kube.StaleGrants(ctx, target, installed)
	if err != nil {
		out.Warn("Grants from an earlier install could not be checked")
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". `" + kube.ListGrantsCommand(target) +
			"` lists them; delete any this setup does not contain.")
		return false
	}
	if !removeStale(ctx, out, target, stale, "grant(s)") {
		return false
	}
	policies, err := kube.StaleAdmissionPolicies(ctx, target, installed)
	if err != nil {
		out.Warn("Admission policies from an earlier install could not be checked")
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". `" + kube.ListPoliciesCommand(target) +
			"` lists them; delete any this setup does not contain.")
		return false
	}
	return removeStale(ctx, out, target, policies, "admission policy object(s)")
}

func pruneStaleObjects(ctx context.Context, out *ui.Printer, target *kube.Target,
	installed []setup.Document) {
	stale, err := kube.StaleObjects(ctx, target, installed)
	if err != nil {
		out.Warn("Objects from an earlier install could not be checked")
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". `" + kube.ListObjectsCommand(target) +
			"` lists them; delete any this setup does not contain.")
		return
	}
	removeStale(ctx, out, target, stale, "object(s)")
}

func removeStale(ctx context.Context, out *ui.Printer, target *kube.Target, stale []setup.Object,
	noun string) bool {
	if len(stale) == 0 {
		return true
	}
	removed, err := kube.DeleteStale(ctx, target, stale, func(grant setup.Object) {
		out.Item("deleted " + grant.String())
	})
	if err != nil {
		out.Warn(fmt.Sprintf("%d %s from an earlier install could not be removed", len(stale)-len(removed),
			noun))
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". The agent does not need them; remove them with:")
		for _, grant := range stale[len(removed):] {
			out.Dim(kube.DeleteGrantCommand(target, grant))
		}
		return false
	}
	out.Success(fmt.Sprintf("Removed %d %s from an earlier install that this setup no longer contains",
		len(removed), noun))
	return true
}

type kubeWarnings struct {
	out     *ui.Printer
	mu      sync.Mutex
	seen    map[string]bool
	pending []string
}

func (w *kubeWarnings) add(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[message] {
		return
	}
	w.seen[message] = true
	w.pending = append(w.pending, message)
	w.out.Log("  ! Kubernetes warning: " + message)
}

func (w *kubeWarnings) flush() {
	w.mu.Lock()
	pending := w.pending
	w.pending = nil
	w.mu.Unlock()
	for _, message := range pending {
		w.out.Warn("Kubernetes: " + message)
	}
}

const redemptionPollInterval = 5 * time.Second

var (
	errConnectTokenExpired = errors.New("the connect token expired before the agent registered")

	errRegistrationUnconfirmed = errors.New("the agent's registration could not be confirmed")
)

func waitForRedemption(ctx context.Context, target *kube.Target,
	fetch func(context.Context) (*api.ConnectTokenStatus, error), expiresAt string,
	timeout, every time.Duration, progress func(string)) (*api.ConnectTokenStatus, error) {
	deadline := time.Now().Add(timeout)
	var lastPhase string
	var lastErr error
	for {
		status, err := fetch(ctx)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, api.ErrStatusRefused):
			return nil, statusRefused(err, target, expiresAt, lastPhase)
		case err != nil:
			lastErr = err
		case status.Phase == "REDEEMED":
			return status, nil
		case status.Phase == "EXPIRED":
			return nil, refusal.New(refusal.CodeCredential,
				"The connect token expired before the agent registered.",
				"Open the Connect dialog again for a new command and run it; the install already "+
					"on the cluster is re-enrolled under the new connect token, nothing is removed.").
				WithKind(errConnectTokenExpired)
		default:
			lastErr = nil
			expiresAt = text.OrText(status.ExpiresAt, expiresAt)
			if status.Phase != lastPhase {
				lastPhase = status.Phase
				progress("connect token " + strings.ToLower(status.Phase) + "; the agent has not registered yet")
			}
		}
		if time.Now().After(deadline) {
			return nil, registrationTimedOut(target, timeout, lastPhase, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

func statusRefused(err error, target *kube.Target, expiresAt, lastPhase string) error {
	var refused *refusal.Refusal
	if !errors.As(err, &refused) {
		return err
	}
	next := refused.Next
	var lines []string
	if expiry, passed := expiredAt(expiresAt); passed {
		lines = append(lines, "The connect token expired at "+expiry+"; the control plane answers "+
			"for it only until then.", "Last phase seen: "+text.OrNone(lastPhase))
		next = "The agent is installed and running. Look for this cluster under Clusters in the " +
			"console. If it is not listed within a few minutes, the agent did not register before " +
			"the connect token expired and cannot register with it now: open the Connect dialog for a " +
			"new command and run it; the install already on the cluster is re-enrolled under the " +
			"new connect token, nothing is removed."
	}
	lines = append(lines, "Agent log: "+enrolmentLog(target))
	return refusal.New(refused.Code, refused.What, next).WithKind(refused.Kind).
		WithDetail(lines...).WithDetail(refused.Detail...)
}

func expiredAt(expiresAt string) (string, bool) {
	expiry, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(expiresAt))
	if err != nil || time.Now().Before(expiry) {
		return "", false
	}
	return expiry.UTC().Format(time.RFC3339), true
}

func registrationTimedOut(target *kube.Target, timeout time.Duration, lastPhase string,
	lastErr error) error {
	if lastErr == nil {
		return refusal.New(refusal.CodeAgentNotReady,
			fmt.Sprintf("The agent is running but has not registered within %s.", timeout),
			"Nothing needs to be re-applied. The agent retries once a minute; what the control "+
				"plane answered is in its log: `"+enrolmentLog(target)+"`.").
			WithDetail("Last phase seen: " + text.OrNone(lastPhase))
	}
	lines := []string{"Last phase seen: " + text.OrNone(lastPhase), "Last status check: " + lastErr.Error()}
	var failed *refusal.Refusal
	if errors.As(lastErr, &failed) {
		if failed.Cause != nil {
			lines = append(lines, "Reason: "+text.OneLine(failed.Cause.Error()))
		}
		lines = append(lines, failed.Detail...)
	}
	return refusal.New(refusal.CodeAgentNotReady,
		fmt.Sprintf("The agent is running, but zoorikctl could not confirm within %s that it "+
			"registered.", timeout),
		"Nothing needs to be re-applied. Look for this cluster under Clusters in the console; if it "+
			"is not listed within a few minutes, what the control plane answered the agent is in its "+
			"log: `"+enrolmentLog(target)+"`.").
		WithKind(errRegistrationUnconfirmed).WithDetail(lines...)
}

func enrolmentLog(target *kube.Target) string {
	return target.Kubectl("-n "+kube.AgentNamespace+" logs deploy/"+kube.AgentWorkload) +
		" | grep -i enrol"
}

func liveMigrationLabel(carried bool) string {
	if carried {
		return "on: installs the " + kube.NodeDaemonWorkload + " node daemon"
	}
	return "off"
}

func printLiveMigrationPlan(out *ui.Printer, carried bool) {
	if !carried {
		return
	}
	out.Warn("Adds the live-migration daemon: DaemonSet " + kube.NodeDaemonNamespace + "/" +
		kube.NodeDaemonWorkload + ", a privileged pod on every node")
	out.Detail("On each node it places one file, a static CRIU that Zoorik builds from pinned upstream source " +
		"and that needs no host libraries, at /usr/local/bin/criu (replacing it atomically and only after its " +
		"sha256 matches), and writes runc's CRIU configuration file, /etc/criu/runc.conf. It installs no " +
		"packages and changes no containerd or node pool settings. The daemon runs AppArmor- and " +
		"seccomp-unconfined so it can read the host's runtime processes, and it can freeze, checkpoint and " +
		"restore containers there. A node is used only after it proves itself: the CRIU at /usr/local/bin/criu " +
		"hashes to the digest the daemon's image carries, and a short-lived Zoorik canary process in the " +
		kube.NodeDaemonNamespace + " namespace is checkpointed from one pod and restored into a second " +
		"through containerd, at " +
		"daemon start and again when the node's CRIU, containerd or its restore setting, or OS image " +
		"changes; where containerd does not restore, only the first pod is created. Those pods " +
		"run the canary program of the daemon's own image as a non-root process with no privilege, no host " +
		"access and no volume, and open no connection; a deny-all NetworkPolicy isolates them where the cluster enforces " +
		"NetworkPolicy. They are deleted with their checkpoint and image when the proof ends, and a failed " +
		"proof is retried after about 1 to 36 minutes, never in a loop. A node whose containerd does not " +
		"restore a checkpoint is a live-migration source only: its canary is only checkpointed, and no pod " +
		"moves onto it. A node whose proof fails stays a normal node that never migrates and names its " +
		"reason. A checkpoint holds the pod's whole memory, secrets included: it is written to disk under " +
		"/var/lib/zoorik/checkpoints on the two nodes of a move, sent between them over mutual TLS, and imported on " +
		"the target as a local image in containerd's image store, which is on disk. Every copy is removed " +
		"when the move ends, whether it succeeds or fails, and the daemon removes any it finds orphaned " +
		"when it starts. Clients of a moved pod see a pause of seconds and reconnect. It moves a pod only " +
		"when node optimization, a rebalancing run or the Evictor moves it off its node and a warm handover " +
		"did not. Disconnect removes the daemon, its " +
		"namespace and its node annotations; the criu binary, /etc/criu/runc.conf and the " +
		"/var/lib/zoorik/checkpoints directory stay on the nodes, with any checkpoint a move the disconnect " +
		"cut off left there.")
}

func removeStaleNodeDaemon(ctx context.Context, out *ui.Printer, target *kube.Target) {
	installed, err := kube.NodeDaemonInstalled(ctx, target)
	if err != nil {
		out.Warn("Whether an earlier install's live-migration daemon is still here could not be checked")
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". `" + target.Kubectl("get namespace "+
			kube.NodeDaemonNamespace) + "` shows it; this setup does not contain it.")
		return
	}
	if !installed {
		return
	}
	detail, err := kube.RemoveNodeDaemon(ctx, target)
	if err != nil {
		out.Warn("The live-migration daemon from an earlier install could not be removed")
		out.Detail("Reason: " + text.OneLine(err.Error()) + ". This setup does not contain it; remove it with `" +
			target.Kubectl("delete namespace "+kube.NodeDaemonNamespace) + "`.")
		return
	}
	out.Success("Removed the live-migration daemon from an earlier install (namespace " +
		kube.NodeDaemonNamespace + " " + detail + "), as this setup does not contain it")
}

func occupantSuffix(occupant kube.Occupant) string {
	if occupant.ClusterName != "" {
		return " for " + occupant.ClusterName
	}
	return ""
}

func verb(created bool) string {
	if created {
		return "created"
	}
	return "configured"
}

func printPlan(out *ui.Printer, target *kube.Target, summary setup.Summary, dryRun bool) {
	out.Section("Agent Setup")
	scope := "no cluster-scoped objects"
	if clusterScoped := len(summary.ClusterScoped); clusterScoped > 0 {
		scope = fmt.Sprintf("%d cluster-scoped; needs cluster-admin", clusterScoped)
	}
	out.Info(fmt.Sprintf("Installs %d object(s) into %s (%s)", len(summary.Objects),
		target.ContextName, scope))
	for _, image := range summary.Images {
		out.Info("Pulls " + image)
	}
	out.Info("Follows each Zoorik release on its own: admission policies " + kube.ReleasePolicy + " and " +
		kube.ReleaseImagesPolicy + " let the agent move only its own images, CRDs and roles, never widen a permission")
	for _, replicas := range summary.Replicas {
		out.Log("  " + replicas)
	}
	for _, object := range summary.Objects {
		if dryRun {
			out.Item(object.String())
		} else {
			out.Log("    " + object.String())
		}
	}
	if !dryRun {
		out.Detail("Run again with --dry-run to print the agent setup itself and the cloud plan, without " +
			"changing anything.")
	}
}

func printComplete(out *ui.Printer, options *connectOptions, target *kube.Target,
	summary setup.Summary, status *api.ConnectTokenStatus, unfinished bool) {
	lines := []ui.BoxLine{
		{Text: "Cluster: " + text.OrNone(clusterNameFor(options, target)), Indent: 2},
		{Text: "Cluster ID: " + status.ClusterID, Indent: 2},
	}
	if options.ClusterResourceID != "" {
		lines = append(lines, ui.BoxLine{Text: "Cluster resource: " + options.ClusterResourceID,
			Indent: 2})
	}
	lines = append(lines, []ui.BoxLine{
		{Text: "Organization ID: " + status.OrganizationID, Indent: 2},
		{Text: "Region: " + regionLabel(options), Indent: 2},
	}...)
	if console := api.ConsoleURL(options.APIRegion, options.APIURL); console != "" {
		lines = append(lines, ui.BoxLine{Text: "Console: " + console + "/clusters/" + status.ClusterID,
			Indent: 2})
	}
	lines = append(lines, resourceLines(summary)...)
	if unfinished {
		out.WarnBox("Zoorik is connected, but the cloud setup is not finished", lines)
		return
	}
	out.SuccessBox("Zoorik setup complete!", lines)
}

func resourceLines(summary setup.Summary) []ui.BoxLine {
	lines := []ui.BoxLine{{}, {Text: "Kubernetes resources applied:", Indent: 2}}
	for _, namespace := range summary.Namespaces {
		lines = append(lines, ui.BoxLine{Text: "Namespace: " + namespace, Indent: 4, Bullet: true})
	}
	for _, object := range summary.Objects {
		switch object.Kind {
		case "Deployment", "DaemonSet":
			lines = append(lines, ui.BoxLine{Text: object.Kind + ": " + object.Name, Indent: 4,
				Bullet: true})
		}
	}
	lines = append(lines, ui.BoxLine{Text: fmt.Sprintf("%d object(s) in total", len(summary.Objects)),
		Indent: 4, Bullet: true})
	return lines
}

func regionLabel(options *connectOptions) string {
	if strings.TrimSpace(options.APIURL) != "" {
		return "custom (--api-url)"
	}
	return options.APIRegion
}

func providerName(cloud string) string {
	switch cloud {
	case kube.CloudAzure:
		return "Azure"
	case kube.CloudAWS:
		return "AWS"
	case kube.CloudLocal:
		return "Local"
	}
	return cloud
}

func nodeCount(identity kube.Identity) string {
	if identity.MoreNodes {
		return fmt.Sprintf("More than %d nodes found", identity.Nodes)
	}
	return fmt.Sprintf("%d node(s) found", identity.Nodes)
}

func createdCount(results []kube.Result) int {
	created := 0
	for _, result := range results {
		if result.Created {
			created++
		}
	}
	return created
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func clusterNameFor(options *connectOptions, target *kube.Target) string {
	if name := strings.TrimSpace(options.ClusterName); name != "" {
		return name
	}
	if target == nil {
		return ""
	}
	return strings.TrimSpace(target.ClusterName)
}

func setReleaseParam(docs []setup.Document, key, value string) {
	for _, doc := range docs {
		if doc.GetKind() == "ConfigMap" && doc.GetNamespace() == kube.AgentNamespace &&
			doc.GetName() == kube.ReleaseParamsConfigMap {
			_ = unstructured.SetNestedField(doc.Object, value, "data", key)
		}
	}
}

func keptClusterResource(options *connectOptions, occupant kube.Occupant) string {
	if options.ClusterResourceGiven {
		return ""
	}
	return occupant.ClusterResourceID
}

func keptCloudIdentity(options *connectOptions, occupant kube.Occupant) string {
	if options.CloudIdentity != "" || cloudIdentityAnnotation(occupant.CloudIdentityClientID) == "" {
		return ""
	}
	return occupant.CloudIdentityClientID
}

func cloudIdentityAnnotation(identity string) string {
	switch {
	case azureClientID.MatchString(identity):
		return kube.AzureWorkloadIdentityClientIDAnnotation
	case iamRoleARN.MatchString(identity):
		return kube.EKSRoleARNAnnotation
	}
	return ""
}

func bindCloudIdentity(docs []setup.Document, identity string) {
	for _, doc := range docs {
		if doc.GetKind() == "ServiceAccount" && doc.GetNamespace() == kube.AgentNamespace &&
			doc.GetName() == kube.AgentServiceAccount {
			_ = unstructured.SetNestedField(doc.Object, identity, "metadata", "annotations",
				cloudIdentityAnnotation(identity))
		}
	}
}

func setAgentConfig(docs []setup.Document, key, value string) {
	for _, doc := range docs {
		if doc.GetKind() == "ConfigMap" && doc.GetNamespace() == kube.AgentNamespace &&
			doc.GetName() == kube.AgentConfigMap {
			_ = unstructured.SetNestedField(doc.Object, value, "data", key)
		}
	}
}

func unkeptReason(pin string, identity kube.Identity) string {
	if _, ok := kube.ParseClusterResource(pin); !ok {
		return "is neither an AKS managed cluster's ARM id nor an EKS cluster's ARN."
	}
	if mismatch := pinMismatch(pin, identity); mismatch != "" {
		return "names another cluster: " + mismatch + "."
	}
	return ""
}

func requirePinMatches(pin string, identity kube.Identity) error {
	mismatch := pinMismatch(pin, identity)
	if mismatch == "" {
		return nil
	}
	return refusal.New(refusal.CodeWrongCluster,
		"--cluster-resource-id names another cluster: "+mismatch+".",
		"Copy the id from this cluster's own page in the cloud console and run this again. Nothing "+
			"has been applied.").WithDetail("Cluster resource: " + pin)
}

func pinMismatch(pin string, identity kube.Identity) string {
	resource, ok := kube.ParseClusterResource(pin)
	if !ok {
		return ""
	}
	switch {
	case identity.Nodes == 0:
		return ""
	case resource.Cloud != identity.Cloud:
		return fmt.Sprintf("it names a cluster on %s, and this cluster's nodes run on %s",
			providerName(resource.Cloud), providerName(identity.Cloud))
	case resource.Cloud == kube.CloudAWS && identity.Region != "" &&
		!strings.EqualFold(resource.Region, identity.Region):
		return fmt.Sprintf("it names a cluster in %s, and this cluster's nodes are in %s",
			resource.Region, identity.Region)
	case resource.Cloud == kube.CloudAzure && len(identity.Subscriptions) > 0 &&
		(len(identity.Subscriptions) != 1 || identity.Subscriptions[0] != resource.Account):
		return fmt.Sprintf("it names subscription %s, and this cluster's nodes run in %s",
			resource.Account, strings.Join(identity.Subscriptions, ", "))
	}
	return ""
}
