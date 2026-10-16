package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/cloud"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type grantOptions struct {
	commonOptions
	ClusterResourceID string
}

const (
	grantCanceled = "Grant canceled."

	grantVerb = "grant-node-deletion"

	prepareVerb = "prepare"
)

type consent struct {
	scope        func(out *ui.Printer, plan cloud.Plan)
	confirm      func(ctx context.Context, s *session, plan cloud.Plan, changes int, yes bool, canceled string) error
	keepsEarlier bool
}

var (
	provisionerConsent = consent{confirm: confirmCloudChanges}

	deletionConsent = consent{scope: printDeletionScope, confirm: confirmNodeDeletion}

	connectDeletionConsent = consent{scope: printDeletionScope, confirm: confirmNodeDeletion, keepsEarlier: true}
)

func parseGrant(args []string, out, errOut io.Writer) (*grantOptions, error) {
	options := &grantOptions{}
	set := flag.NewFlagSet("zoorikctl cluster "+grantVerb, flag.ContinueOnError)
	commonFlags(set, &options.commonOptions)
	set.StringVar(&options.ClusterResourceID, "cluster-resource-id", "",
		"the AKS managed cluster's ARM id or the EKS cluster's ARN")
	if parsed, err := parseCommon(set, args, out, errOut, &options.commonOptions); !parsed {
		return nil, err
	}
	options.ClusterResourceID = strings.TrimSpace(options.ClusterResourceID)
	if options.ClusterResourceID != "" {
		if _, ok := kube.ParseClusterResource(options.ClusterResourceID); !ok {
			return nil, usageError(set.Name(), fmt.Sprintf("--cluster-resource-id=%q is neither an AKS managed "+
				"cluster's ARM id nor an EKS cluster's ARN.", options.ClusterResourceID))
		}
	}
	return options, nil
}

func runGrant(ctx context.Context, args []string, s *session) error {
	options, err := parseGrant(args, s.streams.Out, s.streams.Err)
	if err != nil || options == nil {
		return err
	}

	out := s.out
	out.Banner("Zoorik Node Deletion")
	target, err := kube.Load(options.Kubeconfig, options.KubeContext)
	if err != nil {
		return err
	}
	warnings := &kubeWarnings{out: out, seen: map[string]bool{}}
	target.OnWarning = warnings.add

	out.Section("Configuration")
	out.Success("Kubeconfig loaded.")
	out.Field("Kube context", target.ContextName)
	out.Field("Cluster", text.OrNone(target.ClusterName))
	out.Field("API server", target.ServerURL)
	out.Field("Kubeconfig", text.OrNone(target.Origin))
	if options.ClusterResourceID != "" {
		out.Field("Cluster resource", options.ClusterResourceID)
	}

	out.Section("Cluster Access")
	spinner := out.Spin("Connecting to " + target.ServerURL + "...")
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
	warnings.flush()

	if identity.Cloud == kube.CloudLocal {
		out.Success("A local cluster: Zoorik removes no machine there, so there is nothing to grant")
		return nil
	}
	occupant, err := kube.InspectOccupant(ctx, target)
	if err != nil {
		return refusal.Wrap(refusal.CodePermission, err, "The installed agent's configuration could not be read.",
			"Run this again once your credentials may read "+kube.AgentNamespace+"/"+kube.AgentConfigMap+
				" and the service account "+kube.AgentNamespace+"/"+kube.AgentServiceAccount+". Nothing has been "+
				"changed.")
	}
	if !occupant.Present {
		return refusal.New(refusal.CodeWrongCluster, "The Zoorik agent is not installed on "+target.ContextName+".",
			"Connect it with the command from the Connect dialog; with node optimization on, connect grants node "+
				"deletion itself. Nothing has been changed.")
	}
	out.Success("Zoorik agent found" + occupantSuffix(occupant))
	resource := clusterResourceOf(options.ClusterResourceID)
	kept := keptResourceOf(occupant, identity)
	later := clusterCommand(grantVerb, target, options.Kubeconfig, options.ClusterResourceID)

	out.Section("Plan")
	plan := planNodeDeletion(ctx, out, target, identity, resource, kept, occupant.CloudIdentityClientID)
	binding, err := settleNodeDeletion(ctx, s, plan, options.Yes, options.DryRun, deletionConsent)
	if err != nil || binding == nil {
		return err
	}

	out.Section("Bind The Agent")
	restart, err := bindInstalledAgent(ctx, out, target, occupant, *binding, plan.Cluster)
	if err != nil {
		return err
	}
	if err := ensureCredentialsInjected(ctx, out, target, *binding, restart, agentWait, later); err != nil {
		return err
	}
	warnings.flush()

	out.Blank()
	out.Success("Zoorik may remove spare machines and adjust node pool sizes in " + plan.Cluster + ".")
	out.Paragraph("", "The agent reports the grant on its next capability probe, within a few minutes. From then "+
		"on fixed-size node pools are consolidated, and a pool's minimum is lowered when it alone keeps a node "+
		"Zoorik emptied. Running this again changes nothing.")
	return nil
}

func planNodeDeletion(ctx context.Context, out *ui.Printer, target *kube.Target, identity kube.Identity,
	resource, kept *kube.ClusterResource, occupant string) cloud.NodeDeletion {
	runner := &cloud.Runner{OnRun: func(command cloud.Command) { out.Log("  $ " + command.String()) }}
	input := cloud.Input{Target: target, Resource: resource, Kept: kept, Region: identity.Region, Occupant: occupant}
	var plan cloud.NodeDeletion
	spinner := out.Spin("Reading what " + providerName(identity.Cloud) + " holds for this cluster...")
	switch identity.Cloud {
	case kube.CloudAzure:
		plan = cloud.PlanNodeDeletionAKS(ctx, runner, input)
	case kube.CloudAWS:
		plan = cloud.PlanNodeDeletionEKS(ctx, runner, input)
	}
	spinner.Stop()
	return plan
}

func settleNodeDeletion(ctx context.Context, s *session, plan cloud.NodeDeletion, yes, dryRun bool,
	ask consent) (*cloud.Binding, error) {
	out := s.out
	if plan.Cluster != "" {
		out.Field("Cloud cluster", plan.Cluster)
	}
	for _, line := range plan.Found {
		out.Dim(line)
	}
	if len(plan.Blockers) > 0 {
		printNotes(out, plan.Notes)
		return nil, planBlocked(plan.Plan)
	}
	if err := checkSteps(ctx, out, &plan.Plan); err != nil {
		return nil, err
	}
	pending := plan.Pending()
	if len(pending) > 0 && ask.scope != nil {
		ask.scope(out, plan.Plan)
	}
	printSetupPlan(out, plan.Plan)

	provider := providerName(plan.Cloud)
	if len(pending) == 0 {
		out.Blank()
		out.Success(upperFirst(plan.Provisioner) + ": already set up in " + provider + "; nothing was changed there.")
		binding, err := bindNodeDeletion(ctx, plan)
		if err != nil || binding == nil || !dryRun {
			return binding, err
		}
		out.Field("Identity", binding.Identity+" ("+binding.Via+")")
		out.Info("Dry run: nothing was changed. Without --dry-run, this identity is bound to the installed agent.")
		return nil, nil
	}
	if dryRun {
		out.Blank()
		out.Info("Dry run: nothing was changed. Run it again without --dry-run to make these changes.")
		return nil, nil
	}

	out.Blank()
	if err := ask.confirm(ctx, s, plan.Plan, len(pending), yes, grantCanceled); err != nil {
		if !ask.keepsEarlier || !notAgreed(err) || ctx.Err() != nil || !plan.KeepsEarlier() {
			return nil, err
		}
		out.Success(upperFirst(plan.Provisioner) + ": already set up in " + provider + "; nothing was changed there.")
		out.Detail("Zoorik keeps removing the machines it empties under " + plan.Earlier + ", and does not lower " +
			"node pool minimums.")
		binding, err := bindNodeDeletion(ctx, plan)
		if binding != nil {
			binding.Earlier = true
		}
		return binding, err
	}
	if err := applySteps(ctx, out, plan.Plan, pending); err != nil {
		return nil, err
	}
	out.Success(upperFirst(plan.Provisioner) + ": set up in " + provider + " for " +
		text.OrText(plan.Cluster, "this cluster") + ".")
	return bindNodeDeletion(ctx, plan)
}

func notAgreed(err error) bool {
	var stopped *declined
	var refused *refusal.Refusal
	return errors.As(err, &stopped) || (errors.As(err, &refused) && refused.Code == refusal.CodeNotConfirmed)
}

func printDeletionScope(out *ui.Printer, plan cloud.Plan) {
	out.Info("Zoorik can remove spare machines and adjust node pool sizes in " +
		text.OrText(plan.Cluster, "this cluster") + ":")
	out.Item("delete a machine it has already cordoned and emptied")
	out.Item("lower a node pool's minimum when that minimum alone keeps such a machine, never below 1 on an AKS " +
		"system pool or 0 elsewhere; disconnect puts it back")
	out.Detail("It never adds a machine, never raises a minimum or maximum, and changes nothing else about your " +
		"pools.")
}

func bindNodeDeletion(ctx context.Context, plan cloud.NodeDeletion) (*cloud.Binding, error) {
	if plan.Bind == nil {
		return nil, nil
	}
	binding, err := plan.Bind(ctx)
	if err != nil {
		return nil, refusal.Wrap(refusal.CodePermission, err,
			"The identity that was set up could not be read back: "+text.OneLine(err.Error()),
			"Run this again; every step already done stays done.")
	}
	return &binding, nil
}

func bindInstalledAgent(ctx context.Context, out *ui.Printer, target *kube.Target, occupant kube.Occupant,
	binding cloud.Binding, cluster string) (bool, error) {
	id := binding.Identity
	refused := func(err error) error {
		code := refusal.CodeApply
		if apierrors.IsForbidden(err) {
			code = refusal.CodePermission
		}
		return refusal.Wrap(code, err, "The agent could not be bound to "+id+".",
			"The cloud side is set up and stays. Run this again once your credentials may patch the agent's "+
				"ConfigMap, service account and Deployment in "+kube.AgentNamespace+".")
	}
	restart := false
	switch {
	case binding.Annotate:
		annotation := cloudIdentityAnnotation(id)
		if annotation == "" {
			return false, refused(fmt.Errorf("%s is neither an Azure client id nor an IAM role ARN", id))
		}
		if err := kube.SetCloudIdentityAnnotation(ctx, target, annotation, id); err != nil {
			return false, refused(err)
		}
		out.Success("Bound " + id + " to " + kube.AgentNamespace + "/" + kube.AgentServiceAccount + " (" +
			binding.Via + ")")
	case occupant.CloudIdentityClientID != "":
		if err := kube.RemoveCloudIdentityAnnotations(ctx, target); err != nil {
			return false, refused(err)
		}
		out.Success("Removed " + occupant.CloudIdentityClientID + " from the agent's service account; EKS Pod " +
			"Identity hands it " + id + " instead")
		restart = true
	}
	if cluster != "" && !strings.EqualFold(occupant.ClusterResourceID, cluster) {
		if err := kube.SetAgentConfigKey(ctx, target, kube.AgentClusterResourceIDKey, cluster); err != nil {
			return false, refused(err)
		}
		if occupant.ClusterResourceID == "" {
			out.Success("Set the agent's cluster resource to " + cluster)
		} else {
			out.Success("Corrected the agent's cluster resource from " + occupant.ClusterResourceID + " to " + cluster)
		}
		restart = true
	}
	return restart, nil
}

func clusterResourceOf(pin string) *kube.ClusterResource {
	if parsed, ok := kube.ParseClusterResource(pin); ok {
		return &parsed
	}
	return nil
}

func keptResourceOf(occupant kube.Occupant, identity kube.Identity) *kube.ClusterResource {
	if occupant.ClusterResourceID == "" || unkeptReason(occupant.ClusterResourceID, identity) != "" {
		return nil
	}
	return clusterResourceOf(occupant.ClusterResourceID)
}

func heldCloudOf(occupant kube.Occupant) string {
	if resource, ok := kube.ParseClusterResource(occupant.ClusterResourceID); ok {
		return resource.Cloud
	}
	switch {
	case azureClientID.MatchString(occupant.CloudIdentityClientID):
		return kube.CloudAzure
	case iamRoleARN.MatchString(occupant.CloudIdentityClientID):
		return kube.CloudAWS
	}
	return kube.CloudLocal
}

func ensureCredentialsInjected(ctx context.Context, out *ui.Printer, target *kube.Target, binding cloud.Binding,
	restart bool, wait time.Duration, later string) error {
	name, value := binding.InjectedEnv()
	if !restart {
		injected, err := kube.AgentCredentialsInjected(ctx, target, name, value, "")
		if err != nil {
			out.Warn("The agent's " + binding.Via + " credentials could not be checked: " + text.OneLine(err.Error()))
			out.Detail("If the agent does not report node deletion within a few minutes, run " + later + ".")
			return nil
		}
		restart = !injected
	}
	injected := true
	if restart {
		spinner := out.Spin("Restarting the agent so its cloud credentials are injected...")
		restartedAt, err := kube.RestartAgent(ctx, target, time.Now())
		if err != nil {
			spinner.Fail("The agent could not be restarted")
			code := refusal.CodeApply
			if apierrors.IsForbidden(err) {
				code = refusal.CodePermission
			}
			return refusal.Wrap(code, err, "The agent could not be restarted to pick up "+binding.Identity+".",
				"It is bound. Restart it with `"+target.Kubectl("-n "+kube.AgentNamespace+" rollout restart "+
					"deployment/"+kube.AgentWorkload)+"`, or run "+later+" again.")
		}
		injected, err = kube.WaitForAgentCredentials(ctx, target, name, value, restartedAt, wait)
		if err != nil {
			spinner.Warn("The agent's " + binding.Via + " credentials could not be checked: " +
				text.OneLine(err.Error()))
			out.Detail("If the agent does not report node deletion within a few minutes, run " + later + ".")
			return nil
		}
		if injected {
			waitCtx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
			defer cancel()
			if err := kube.WaitForAgent(waitCtx, target, wait, spinner.Note); err != nil {
				spinner.Fail("The agent did not start after its restart")
				return err
			}
			spinner.Success("Agent restarted with its " + binding.Via + " credentials")
		} else {
			spinner.Stop()
		}
	}
	if injected {
		return nil
	}
	out.Warn("The agent still has no " + binding.Via + " credentials")
	switch binding.Via {
	case cloud.ViaAzureWorkloadIdentity:
		out.Detail("The workload identity webhook in kube-system did not inject them. Check that it runs, then " +
			"run " + later + " again.")
	case cloud.ViaPodIdentity:
		out.Detail("EKS did not inject them. Check that the pod identity association for " + kube.AgentNamespace +
			"/" + kube.AgentServiceAccount + " exists and that the eks-pod-identity-agent DaemonSet runs in " +
			"kube-system, then run " + later + " again.")
	default:
		out.Detail("The EKS pod identity webhook did not inject them. Check the service account's " +
			kube.EKSRoleARNAnnotation + " annotation, then run " + later + " again.")
	}
	return nil
}

func clusterCommand(verb string, target *kube.Target, kubeconfig, clusterResourceID string) string {
	command := "zoorikctl cluster " + verb + " --kube-context=" + kube.ShellWord(target.ContextName)
	if kubeconfig != "" {
		command += " --kubeconfig=" + kube.ShellWord(kubeconfig)
	}
	if clusterResourceID != "" {
		command += " --cluster-resource-id=" + kube.ShellWord(clusterResourceID)
	}
	return command
}

func cleanUpNodeDeletion(ctx context.Context, s *session, target *kube.Target, identity kube.Identity,
	identifyErr error, occupant kube.Occupant, options *commonOptions) {
	held := identifyErr == nil && identity.Cloud == kube.CloudLocal
	if held {
		identity.Cloud = heldCloudOf(occupant)
		if identity.Cloud == kube.CloudLocal {
			return
		}
	}
	out := s.out
	out.Section("Cloud Changes")
	if identifyErr != nil {
		out.Warn("The cloud changes zoorikctl made to grant node deletion were not looked for: " +
			text.OneLine(identifyErr.Error()))
		return
	}
	if held {
		out.Dim("No node names a cloud now; the agent's configuration names " + providerName(identity.Cloud) +
			", so the cloud changes zoorikctl made there are looked for")
	}
	runner := &cloud.Runner{OnRun: func(command cloud.Command) { out.Log("  $ " + command.String()) }}
	input := cloud.Input{Target: target, Kept: keptResourceOf(occupant, identity), Region: identity.Region,
		Occupant: occupant.CloudIdentityClientID}
	var plan cloud.Plan
	var created, lowered string
	spinner := out.Spin("Looking for the cloud changes zoorikctl made for this cluster...")
	switch identity.Cloud {
	case kube.CloudAzure:
		plan = cloud.PlanNodeDeletionCleanupAKS(ctx, runner, input)
		created = "the \"Zoorik Node Removal v2\" or \"Zoorik Node Removal v1\" role assignments on the cluster " +
			"and the managed identity zoorik-agent-<cluster> in its resource group"
		lowered = "each agent pool's minimum tagged coral.zoorik.com_original-min"
	case kube.CloudAWS:
		plan = cloud.PlanNodeDeletionCleanupEKS(ctx, runner, input)
		created = "the inline policy ZoorikNodeRemoval-<cluster>-<hash> on the agent's role and the IAM role " +
			"zoorik-agent-<cluster>"
		lowered = "each node group's and Auto Scaling group's minimum tagged coral.zoorik.com/original-min"
	}
	spinner.Stop()
	if plan.Cluster != "" {
		out.Field("Cloud cluster", plan.Cluster)
	}
	for _, line := range plan.Found {
		out.Dim(line)
	}
	if len(plan.Blockers) > 0 {
		out.Warn("Not looked for: " + plan.Blockers[0].What)
		out.Detail(plan.Blockers[0].Remedy)
		out.Detail("If zoorikctl granted node deletion here, it created " + created + "; remove it by hand, and put " +
			"back " + lowered + " to the value of that tag.")
		return
	}
	if err := checkSteps(ctx, out, &plan); err != nil {
		out.Warn("The cloud changes zoorikctl made could not be checked: " + whatOf(err))
		return
	}
	pending := plan.Pending()
	if len(pending) == 0 {
		printNotes(out, plan.Notes)
		out.Success("No cloud change zoorikctl made for this cluster is left")
		return
	}
	printSetupPlan(out, plan)
	if options.DryRun {
		return
	}

	out.Blank()
	switch {
	case options.Yes:
		s.errs.Success(fmt.Sprintf("Undoing %d cloud change(s) (--yes)", len(pending)))
	case !s.streams.Interactive:
		out.Info("Left in place: nobody is at a terminal to agree, and --yes was not given. Run the commands " +
			"above, or this disconnect again with --yes.")
		return
	default:
		s.errs.Prompt(fmt.Sprintf("Undo these %d cloud change(s) zoorikctl made for %s, with %s? [y/N]",
			len(pending), text.OrText(plan.Cluster, target.ContextName), plan.Credential))
		answer, err := readAnswer(ctx, s.streams.In)
		if err != nil || (!strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes")) {
			out.Info("Left in place. Run the commands above to undo them later, or run this disconnect again.")
			return
		}
	}
	if err := applySteps(ctx, out, plan, pending); err != nil {
		out.Warn("Undoing the cloud changes stopped: " + causeOf(err))
		out.Detail("Every change already undone stays undone; run this disconnect again to resume.")
		return
	}
	if plan.RestoresMinimums() {
		out.Success("Put back every node pool minimum Zoorik lowered, and removed the cloud objects zoorikctl " +
			"created to grant node deletion")
		return
	}
	out.Success("Removed the cloud objects zoorikctl created to grant node deletion")
}

func whatOf(err error) string {
	var typed *refusal.Refusal
	if errors.As(err, &typed) {
		return typed.What
	}
	return text.OneLine(err.Error())
}

func causeOf(err error) string {
	var typed *refusal.Refusal
	if errors.As(err, &typed) && typed.Cause != nil {
		return text.OneLine(typed.Cause.Error())
	}
	return whatOf(err)
}
