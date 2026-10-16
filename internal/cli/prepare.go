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
)

type prepareOptions struct {
	commonOptions
	ClusterResourceID string
}

const prepareCanceled = "Prepare canceled."

const strayEnterWindow = 300 * time.Millisecond

var errStepNotInPlace = errors.New("a step ran without an error, and its check does not find it in place")

func parsePrepare(args []string, out, errOut io.Writer) (*prepareOptions, error) {
	options := &prepareOptions{}
	set := flag.NewFlagSet("zoorikctl cluster prepare", flag.ContinueOnError)
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

func runPrepare(ctx context.Context, args []string, s *session) error {
	options, err := parsePrepare(args, s.streams.Out, s.streams.Err)
	if err != nil || options == nil {
		return err
	}

	out := s.out
	out.Banner("Zoorik Prepare")
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

	out.Section("Node Provisioning")
	found, err := kube.DetectAutoprovisioner(ctx, target)
	if err != nil {
		return err
	}
	printProvisioning(out, identity.Cloud, found)
	warnings.flush()

	out.Section("Live Migration Readiness")
	pools, err := kube.ReadCRIUReadiness(ctx, target)
	if err != nil {
		return err
	}
	printReadiness(out, identity.Cloud, pools, found.NodePoolsServed || found.AutoModeServed)
	warnings.flush()

	out.Section("Plan")
	if identity.Cloud == kube.CloudLocal {
		out.Success("A local cluster: no node provisioner is set up for it")
		out.Detail("Live migration on a local cluster moves pods only between the nodes it already has. The " +
			"readiness report above is what applies.")
		return nil
	}
	plan := planProvisioner(ctx, out, target, identity, found, clusterResourceOf(options.ClusterResourceID), nil)
	if plan.Cluster != "" {
		out.Field("Cloud cluster", plan.Cluster)
	}
	for _, line := range plan.Found {
		out.Dim(line)
	}
	if plan.Present && len(plan.Steps) == 0 && len(plan.Blockers) == 0 {
		out.Success(plan.Provisioner + " is already on this cluster; prepare changes nothing")
		warnRebalancingUnavailable(out, plan)
		printNotes(out, plan.Notes)
		return nil
	}
	if plan.Present {
		out.Success(plan.Provisioner + " is already on this cluster")
	}
	if len(plan.Blockers) > 0 {
		printNotes(out, plan.Notes)
		if options.DryRun && plan.ToolingBlocked() {
			for _, blocker := range plan.Blockers {
				out.Warn(blocker.What)
				out.Detail(blocker.Remedy)
			}
			out.Blank()
			out.Info("Dry run: the cloud was not read, so no step is planned. Nothing was changed.")
			return nil
		}
		return planBlocked(plan)
	}

	if err := checkSteps(ctx, out, &plan); err != nil {
		return err
	}
	printSetupPlan(out, plan)

	pending := plan.Pending()
	if len(pending) == 0 {
		out.Blank()
		out.Success(plan.Provisioner + " is already set up; nothing was changed.")
		return nil
	}
	if options.DryRun {
		out.Blank()
		out.Info("Dry run: nothing was changed. Run it again without --dry-run to make these changes.")
		return nil
	}

	out.Section("Confirm")
	if err := confirmCloudChanges(ctx, s, plan, len(pending), options.Yes, prepareCanceled); err != nil {
		return err
	}

	out.Section("Set Up " + plan.Provisioner)
	if err := applySteps(ctx, out, plan, pending); err != nil {
		return err
	}
	warnings.flush()

	if err := target.Rediscover(); err == nil {
		if after, err := kube.DetectAutoprovisioner(ctx, target); err == nil {
			out.Blank()
			printProvisioning(out, identity.Cloud, after)
		}
	}
	out.Blank()
	where := text.OrText(plan.Cluster, target.ContextName)
	if plan.Present {
		out.Success(fmt.Sprintf("%s was already on %s; the %d change(s) above are made.", plan.Provisioner, where,
			len(pending)))
		adds := " adds nodes for Pending pods as before."
		if plan.Poolless {
			adds = " now adds nodes for Pending pods."
		}
		out.Paragraph("", "Running prepare again changes nothing. "+plan.Provisioner+adds+" Zoorik only removes "+
			"nodes, and adds its own zoorik-* pools on it when live migration or rebalancing needs them.")
		return nil
	}
	out.Success(plan.Provisioner + " is set up on " + where + ".")
	out.Paragraph("", "Running prepare again changes nothing. "+plan.Provisioner+" now adds nodes for Pending pods. "+
		"Zoorik only removes nodes, and adds its own zoorik-* pools on it when live migration or rebalancing needs "+
		"them. Node pools that are not ready for live migration stay as they are.")
	return nil
}

func warnRebalancingUnavailable(out *ui.Printer, plan cloud.Plan) {
	if plan.Provisioner != cloud.ProvisionerClusterAutoscaler {
		return
	}
	pools := "this cluster's node groups"
	if len(plan.AutoscaledPools) > 0 {
		pools = strings.Join(plan.AutoscaledPools, ", ")
	}
	out.Warn("Rebalancing is not available: the cluster autoscaler manages " + pools)
}

func planProvisioner(ctx context.Context, out *ui.Printer, target *kube.Target, identity kube.Identity,
	found kube.Autoprovisioner, resource, kept *kube.ClusterResource) cloud.Plan {
	runner := &cloud.Runner{OnRun: func(command cloud.Command) { out.Log("  $ " + command.String()) }}
	input := cloud.Input{Target: target, Found: found, Resource: resource, Kept: kept, Region: identity.Region}
	var plan cloud.Plan
	spinner := out.Spin("Reading what " + providerName(identity.Cloud) + " holds for this cluster...")
	switch identity.Cloud {
	case kube.CloudAzure:
		plan = cloud.PlanAKS(ctx, runner, input)
	case kube.CloudAWS:
		plan = cloud.PlanEKS(ctx, runner, input)
	}
	spinner.Stop()
	return plan
}

func printProvisioning(out *ui.Printer, cloudName string, found kube.Autoprovisioner) {
	switch {
	case found.AutoModeServed:
		out.Success("EKS Auto Mode provisions this cluster's nodes")
	case found.NodePoolsServed && found.AKSClassesServed:
		out.Success("Node Auto Provisioning (AKS's managed Karpenter) is on")
	case found.NodePoolsServed && found.EC2ClassesServed:
		if found.KarpenterRunning() {
			out.Success("Karpenter is installed and running")
			if !found.KarpenterStaticCapacity() {
				out.Warn("Karpenter's static capacity is off, so Coral's rebalancing cannot create its new nodes here")
			}
		} else {
			out.Warn("Karpenter's resources are served, but no Karpenter controller pod is ready")
		}
	case found.NodePoolsServed:
		out.Info("karpenter.sh NodePools are served, with no AKS or EC2 node class")
	default:
		switch cloudName {
		case kube.CloudAzure:
			out.Info("Node Auto Provisioning is not on")
		case kube.CloudAWS:
			out.Info("Karpenter is not installed")
		default:
			out.Info("No Karpenter-style node provisioner is served")
		}
		if found.ClusterAutoscaler {
			out.Success("A cluster autoscaler reports its status in " + found.ClusterAutoscalerNamespace + "/" +
				kube.ClusterAutoscalerStatus)
		}
		return
	}
	for _, pool := range found.Pools {
		line := "NodePool " + pool.Name
		if pool.NodeClass != "" {
			line += " (node class " + pool.NodeClass + ")"
		}
		switch {
		case pool.ZoorikOwned:
			line += ": Zoorik's own"
		case pool.CreatesNoNode:
			line += ": creates no node (cpu limit 0)"
		}
		out.Item(line)
	}
	for _, class := range found.Classes {
		line := class.Kind + " " + class.Name
		if class.ImageFamily != "" {
			line += ": image family " + class.ImageFamily
		}
		if class.ZoorikOwned {
			line += " (Zoorik's own)"
		}
		out.Item(line)
	}
}

func printReadiness(out *ui.Printer, cloudName string, pools []kube.PoolCRIUReadiness, provisioned bool) {
	total, ready, sourceOnly := 0, 0, 0
	for _, pool := range pools {
		total += len(pool.Nodes)
		ready += pool.ReadyNodes()
		sourceOnly += pool.SourceOnlyNodes()
		headline := fmt.Sprintf("%s (%s", pool.Name, pool.Kind)
		if pool.ZoorikOwned {
			headline += ", Zoorik's own"
		}
		counted := pool.ReadyNodes()
		if pool.Verdict() == "SOURCE ONLY" {
			counted = pool.SourceOnlyNodes()
		}
		headline += fmt.Sprintf("): %s on %d of %d node(s)", pool.Verdict(), counted, len(pool.Nodes))
		if pool.Verdict() == "PARTLY READY" && pool.SourceOnlyNodes() > 0 {
			headline += fmt.Sprintf(", source only on %d", pool.SourceOnlyNodes())
		}
		switch pool.Verdict() {
		case "READY":
			out.Success(headline)
		case "NOT READY":
			out.Fail(headline)
		default:
			out.Warn(headline)
		}
		out.Dim("OS image:   " + strings.Join(pool.Distinct(func(n kube.NodeCRIUFacts) string { return n.OSImage }), ", "))
		out.Dim("Kernel:     " + strings.Join(pool.Distinct(func(n kube.NodeCRIUFacts) string { return n.Kernel }), ", "))
		out.Dim("Runtime:    " + strings.Join(pool.Distinct(func(n kube.NodeCRIUFacts) string {
			return n.ContainerRuntime
		}), ", "))
		out.Dim("Kubelet:    " + strings.Join(pool.Distinct(func(n kube.NodeCRIUFacts) string { return n.Kubelet }), ", "))
		series := pool.Distinct(func(n kube.NodeCRIUFacts) string { return n.CPUSeries })
		out.Dim("CPU:        " + strings.Join(series, ", ") + " on " +
			strings.Join(pool.Distinct(func(n kube.NodeCRIUFacts) string { return n.Architecture }), ", "))
		out.Dim("Daemon:     " + pool.DaemonState())
		if builds := pool.DaemonCRIU(); len(builds) > 0 {
			out.Dim("CRIU:       " + strings.Join(builds, ", "))
		}
		for _, reason := range pool.Reasons() {
			out.Dim("Not ready:  " + reason)
		}
		for _, note := range pool.RestoreNotes() {
			out.Dim("Restore:    " + note)
		}
		if len(series) > 1 {
			out.Dim("Note:       more than one CPU series; a pod moves only to a node whose CPU offers every " +
				"feature of the one it runs on")
		}
		if gpu := pool.GPUNodes(); gpu > 0 {
			out.Dim(fmt.Sprintf("Note:       %d node(s) have GPUs; pods that use a GPU or another device are never "+
				"live-migrated", gpu))
		}
		if advice := moveAdvice(cloudName, pool); advice != "" && !provisioned {
			out.Dim("Move:       " + advice)
		}
	}
	out.Blank()
	switch {
	case total == 0:
		out.Warn("No node to report on")
	case ready == total:
		out.Success(fmt.Sprintf("All %d node(s) in %d pool(s) are ready for live migration", total, len(pools)))
	default:
		summary := fmt.Sprintf("%d of %d node(s) in %d pool(s) are ready for live migration.", ready, total,
			len(pools))
		if sourceOnly > 0 {
			summary += fmt.Sprintf(" %d node(s) can only be checkpointed: no pod moves onto them, and their own pods "+
				"move only to a node that restores.", sourceOnly)
		}
		if ready+sourceOnly < total {
			summary += " Pods on the others are handed over or evicted, as today."
		}
		out.Info(summary)
	}
	out.Detail("Ready means the node runs Linux on " + strings.Join(kube.SupportedArchitectures, " or ") +
		" with containerd, whatever its OS image, kernel or containerd release, until the node's own proof " +
		"says otherwise. A node on containerd " + fmt.Sprintf("%d.%d", kube.CRIUContainerdNoRestoreMajor,
		kube.CRIUContainerdNoRestoreMinor) + " or later can only be checkpointed: no pod moves onto it. " +
		"Once the live-migration daemon runs there, the node's own proof decides, source-only included (the " +
		kube.LiveMigrationAnnotation + " node annotation), on any OS image: the CRIU placed at " +
		"/usr/local/bin/criu hashes to the digest the daemon's image carries, and a short-lived Zoorik canary " +
		"process in the " + kube.NodeDaemonNamespace + " namespace is checkpointed from one pod and restored " +
		"into a second through containerd (only checkpointed where containerd does not restore) at daemon " +
		"start, and again when the node's CRIU, " +
		"containerd or its restore setting, or OS image changes. Source and target of a move share the " +
		"architecture and OS family, the target's CPU offers every feature of the source's, and its kernel is at " +
		"least the source's. " +
		"Zoorik installs no packages and never changes your node pools or their containerd.")
}

func moveAdvice(cloudName string, pool kube.PoolCRIUReadiness) string {
	if pool.ZoorikOwned || pool.Verdict() == "READY" {
		return ""
	}
	platform := false
	for _, node := range pool.Nodes {
		if node.PlatformReason == kube.ReadinessRuntimeUnsupported {
			platform = true
		}
	}
	if !platform {
		return ""
	}
	switch cloudName {
	case kube.CloudAzure:
		return "by hand, to an Azure Linux 3.0 or Ubuntu 24.04 node pool (for example `az aks nodepool add " +
			"--os-sku AzureLinux`), then move its workloads there"
	case kube.CloudAWS:
		return "by hand, to an Amazon Linux 2023 node group (AMI type AL2023_x86_64_STANDARD or " +
			"AL2023_ARM_64_STANDARD) or to a Karpenter NodePool on an AL2023 EC2NodeClass"
	}
	return "by hand, to Linux nodes whose container runtime is containerd"
}

func checkSteps(ctx context.Context, out *ui.Printer, plan *cloud.Plan) error {
	spinner := out.Spin("Checking what is already set up...")
	for i := range plan.Steps {
		done, err := plan.Steps[i].Check(ctx)
		if err != nil {
			spinner.Fail("What is already set up could not be read")
			return refusal.Wrap(refusal.CodePermission, err,
				"Checking \""+plan.Steps[i].Title+"\" failed: "+text.OneLine(err.Error()),
				"Check that "+plan.Credential+" may read this cluster's resources, and run this again. Nothing "+
					"has been changed.")
		}
		plan.Steps[i].Done = done
	}
	spinner.Success(fmt.Sprintf("%d of %d step(s) already set up", len(plan.Settled()), len(plan.Steps)))
	return nil
}

func applySteps(ctx context.Context, out *ui.Printer, plan cloud.Plan, pending []cloud.Step) error {
	for index, step := range pending {
		label := fmt.Sprintf("[%d/%d] %s", index+1, len(pending), step.Title)
		spinner := out.Spin(label + "...")
		done, err := step.Check(ctx)
		if err == nil && done {
			spinner.Success(label + ": already set up")
			continue
		}
		if err := step.Apply(ctx, spinner.Note); err != nil {
			spinner.Fail(label + ": stopped")
			return stepFailed(plan, pending, index, err)
		}
		spinner.Tick(label + ": checking that it is in place...")
		if err := requireInPlace(ctx, step); err != nil {
			spinner.Fail(label + ": not in place")
			return stepFailed(plan, pending, index, err)
		}
		spinner.Success(label)
	}
	return nil
}

func requireInPlace(ctx context.Context, step cloud.Step) error {
	inPlace, err := step.InPlace(ctx)
	switch {
	case err != nil:
		return refusal.Wrap(refusal.CodeApply, err, "\""+step.Title+"\" was applied, but whether it is in place "+
			"could not be read: "+text.OneLine(err.Error()), "").WithKind(errStepNotInPlace)
	case !inPlace:
		return refusal.New(refusal.CodeApply, step.NotInPlaceMessage(), "").WithKind(errStepNotInPlace)
	}
	return nil
}

func printSetupPlan(out *ui.Printer, plan cloud.Plan) {
	printNotes(out, plan.Notes)
	for _, step := range plan.Settled() {
		out.Success("Already set up: " + step.Title)
	}
	pending := plan.Pending()
	if len(pending) == 0 {
		return
	}
	out.Blank()
	out.Plain(fmt.Sprintf("  %d change(s) to make, with %s:", len(pending), plan.Credential))
	for index, step := range pending {
		out.Blank()
		out.Info(fmt.Sprintf("%d. %s", index+1, step.Title))
		out.Detail(step.Why)
		for _, command := range step.Commands {
			if strings.HasPrefix(command, "{") || command == "EOF" {
				out.Raw(indentBlock(command, "      "))
				continue
			}
			out.Dim("$ " + command)
		}
	}
}

func printNotes(out *ui.Printer, notes []string) {
	for _, note := range notes {
		out.Detail(note)
	}
}

func indentBlock(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

func planBlocked(plan cloud.Plan) error {
	first := plan.Blockers[0]
	failed := refusal.New(refusal.CodeApply, first.What, first.Remedy)
	for _, other := range plan.Blockers[1:] {
		failed = failed.WithDetail("Also: " + other.What + " " + other.Remedy)
	}
	return failed
}

func stepFailed(plan cloud.Plan, pending []cloud.Step, index int, err error) error {
	lines := make([]string, 0, index+1)
	for _, step := range plan.Settled() {
		lines = append(lines, "Already set up before this run: "+step.Title)
	}
	for _, step := range pending[:index] {
		lines = append(lines, "Done in this run: "+step.Title)
	}
	code := refusal.CodeApply
	if strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "AuthorizationFailed") ||
		strings.Contains(err.Error(), "Forbidden") {
		code = refusal.CodePermission
	}
	next := "Every step already done stays done, and running the same command again checks each step and " +
		"resumes where this one stopped."
	if errors.Is(err, errStepNotInPlace) {
		next = "Every step already done stays done. This step ran without an error, but its check does not find " +
			"what it sets up in place, for the reason reported below. Once that is fixed, running the same command " +
			"again checks each step and resumes here."
	}
	return refusal.Wrap(code, err,
		"Setting up "+plan.Provisioner+" stopped at \""+pending[index].Title+"\".", next).WithDetail(lines...)
}

func confirmCloudChanges(ctx context.Context, s *session, plan cloud.Plan, changes int, yes bool,
	canceled string) error {
	return askCloudChanges(ctx, s, plan, changes, yes, canceled, false, fmt.Sprintf(
		"Make these %d change(s) to %s with %s? [y/N]", changes, text.OrText(plan.Cluster, "this cluster"),
		plan.Credential))
}

func confirmNodeDeletion(ctx context.Context, s *session, plan cloud.Plan, changes int, yes bool,
	canceled string) error {
	return askCloudChanges(ctx, s, plan, changes, yes, canceled, true, fmt.Sprintf(
		"Let Zoorik remove spare machines and adjust node pool sizes in %s, with %s? [Y/n]",
		text.OrText(plan.Cluster, "this cluster"), plan.Credential))
}

func askCloudChanges(ctx context.Context, s *session, plan cloud.Plan, changes int, yes bool, canceled string,
	agreedByDefault bool, question string) error {
	if yes {
		s.errs.Success(fmt.Sprintf("Making %d change(s) (--yes)", changes))
		return nil
	}
	if !s.streams.Interactive {
		return refusal.New(refusal.CodeNotConfirmed,
			"These changes use "+plan.Credential+", and nobody is at a terminal to agree to them.",
			"Read the plan above, then run the same command with --yes to make the changes, or with --dry-run "+
				"to print the plan only. Nothing was changed.")
	}
	s.errs.Prompt(question)
	asked := time.Now()
	answer, err := readAnswer(ctx, s.streams.In)
	for agreedByDefault && err == nil && answer == "" && time.Since(asked) < strayEnterWindow {
		asked = time.Now()
		answer, err = readAnswer(ctx, s.streams.In)
	}
	if err == nil && (strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") ||
		(agreedByDefault && answer == "")) {
		s.errs.Success("Confirmed: making the changes")
		return nil
	}
	return &declined{
		title:       canceled,
		interrupted: err != nil,
		next: "Nothing was changed. Answer y to make the changes, add --yes to skip the question, or run with " +
			"--dry-run to print the plan only.",
	}
}
