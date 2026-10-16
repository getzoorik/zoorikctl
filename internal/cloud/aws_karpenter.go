package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	awsCLI = "aws"

	helmCLI = "helm"

	DefaultKarpenterVersion = "1.8.1"

	karpenterChart = "oci://public.ecr.aws/karpenter/karpenter"

	karpenterCRDChart = "oci://public.ecr.aws/karpenter/karpenter-crd"

	karpenterCRDRelease = "karpenter-crd"

	karpenterRelease = "karpenter"

	staticCapacityValue = "settings.featureGates.staticCapacity=true"

	helmTimeout = 10 * time.Minute

	installedVersionPlaceholder = "<the installed version>"

	autoscalerDiscoveryTag = "k8s.io/cluster-autoscaler/enabled"

	discoveryTag = "karpenter.sh/discovery"

	nodeRolePrefix = "KarpenterNodeRole-"

	controllerRolePrefix = "KarpenterControllerRole-"

	controllerPolicyName = "KarpenterControllerPolicy"

	queuePrefix = "Karpenter-"

	podIdentityAddon = "eks-pod-identity-agent"

	queueTargetID = "KarpenterInterruptionQueueTarget"

	iamNameLimit = 64

	queueNameLimit = 80

	ruleNameLimit = 64

	eksAMIAlias = "al2023@latest"

	accessModeAPI = "API"

	accessModeAPIAndConfigMap = "API_AND_CONFIG_MAP"

	StaticCapacityNotInPlace = "Karpenter does not serve static capacity yet (its StaticCapacity feature gate on, " +
		"and NodePools that hold spec.replicas), so Coral's rebalancing cannot create its new nodes"

	staticCapacityMinimum = "1.8"

	staticCapacityMinimumMajor = 1

	staticCapacityMinimumMinor = 8

	helmLibrary = "zoorikctl does this with Helm's library, so no helm binary is needed; the command is the helm " +
		"equivalent."
)

var autoModeBuiltInPools = []string{"general-purpose", "system"}

var nodeRolePolicies = []string{
	"AmazonEKSWorkerNodePolicy",
	"AmazonEKS_CNI_Policy",
	"AmazonEC2ContainerRegistryPullOnly",
	"AmazonSSMManagedInstanceCore",
}

var interruptionRules = []struct {
	suffix  string
	pattern map[string]any
}{
	{"-ScheduledChange", map[string]any{"source": []string{"aws.health"}, "detail-type": []string{"AWS Health Event"}}},
	{"-SpotInterruption", map[string]any{"source": []string{"aws.ec2"},
		"detail-type": []string{"EC2 Spot Instance Interruption Warning"}}},
	{"-Rebalance", map[string]any{"source": []string{"aws.ec2"},
		"detail-type": []string{"EC2 Instance Rebalance Recommendation"}}},
	{"-InstanceStateChange", map[string]any{"source": []string{"aws.ec2"},
		"detail-type": []string{"EC2 Instance State-change Notification"}}},
}

type EKSCluster struct {
	Partition string
	Region    string
	Account   string
	Name      string
}

func (c EKSCluster) ARN() string {
	return "arn:" + c.Partition + ":eks:" + c.Region + ":" + c.Account + ":cluster/" + c.Name
}

type eksNames struct {
	nodeRole       string
	controllerRole string
	queue          string
	rulePrefix     string
}

func namesFor(cluster EKSCluster) eksNames {
	return eksNames{
		nodeRole:       boundedName(nodeRolePrefix, cluster.Name, iamNameLimit),
		controllerRole: boundedName(controllerRolePrefix, cluster.Name, iamNameLimit),
		queue:          boundedName(queuePrefix, cluster.Name, queueNameLimit),
		rulePrefix:     boundedName(queuePrefix, cluster.Name, ruleNameLimit-len("-InstanceStateChange")),
	}
}

type eksDescription struct {
	Cluster struct {
		Arn                string `json:"arn"`
		Version            string `json:"version"`
		Endpoint           string `json:"endpoint"`
		Status             string `json:"status"`
		ResourcesVpcConfig struct {
			SubnetIDs              []string `json:"subnetIds"`
			ClusterSecurityGroupID string   `json:"clusterSecurityGroupId"`
		} `json:"resourcesVpcConfig"`
		AccessConfig struct {
			AuthenticationMode string `json:"authenticationMode"`
		} `json:"accessConfig"`
		ComputeConfig *struct {
			Enabled     bool     `json:"enabled"`
			NodePools   []string `json:"nodePools"`
			NodeRoleArn string   `json:"nodeRoleArn"`
		} `json:"computeConfig"`
		Identity struct {
			OIDC struct {
				Issuer string `json:"issuer"`
			} `json:"oidc"`
		} `json:"identity"`
	} `json:"cluster"`
}

func (d eksDescription) autoModeOn() bool {
	return d.Cluster.ComputeConfig != nil && d.Cluster.ComputeConfig.Enabled
}

func (d eksDescription) autoModeRole() string {
	if !d.autoModeOn() {
		return ""
	}
	return d.Cluster.ComputeConfig.NodeRoleArn
}

func (d eksDescription) builtInPoolsOn() bool {
	if !d.autoModeOn() {
		return false
	}
	for _, pool := range autoModeBuiltInPools {
		if !slices.Contains(d.Cluster.ComputeConfig.NodePools, pool) {
			return false
		}
	}
	return true
}

func (d eksDescription) autoModeReport() string {
	if !d.autoModeOn() {
		return "AWS reports EKS Auto Mode's compute off"
	}
	return "AWS reports EKS Auto Mode's compute on, built-in NodePools " +
		text.OrText(strings.Join(d.Cluster.ComputeConfig.NodePools, ", "), "none") + ", node role " +
		text.OrText(d.autoModeRole(), "none")
}

func PlanEKS(ctx context.Context, runner *Runner, input Input) Plan {
	plan := Plan{Cloud: kube.CloudAWS, Provisioner: ProvisionerKarpenter, Credential: "your AWS CLI credentials (aws)"}
	found := input.Found
	switch {
	case found.AutoModeServed:
		plan.Present = true
		plan.Provisioner = ProvisionerAutoMode
		plan.Found = describeFound(found)
		if found.HasCustomerNodePool() {
			plan.note("EKS Auto Mode runs Karpenter for you, so it is left as it is.")
			return plan
		}
		planAutoModeBuiltInPools(ctx, runner, &plan, input)
		return plan
	case found.NodePoolsServed && found.EC2ClassesServed:
		plan.Present = true
		plan.Credential = kubeCredential(input.Target)
		plan.Found = describeFound(found)
		if !found.KarpenterRunning() {
			plan.note("Karpenter's resources are served, but no Karpenter controller pod is ready (zoorikctl finds " +
				"the controller by the label app.kubernetes.io/name=karpenter, or by an image whose repository " +
				"contains " + kube.KarpenterControllerImageMark + "). It is left as it is; check the controller, as " +
				"it is what adds nodes for Pending pods.")
			return plan
		}
		poolClass, ok := eksDefaultPoolClass(&plan, found)
		if !ok {
			return plan
		}
		planStaticCapacity(ctx, &plan, runner, input.Target, found)
		if poolClass != "" {
			plan.Steps = append(plan.Steps, defaultNodePoolStep(input.Target, poolClass, "The cluster has no "+
				"NodePool of its own (Zoorik's zoorik-* pools do not count), so nothing adds nodes for Pending pods. "+
				"It gets the NodePool zoorikctl adds when it installs Karpenter, on the cluster's own "+
				kube.KindEC2NodeClass+" "+poolClass+", which is not touched. "))
		}
		planEKSBaseNodeClass(ctx, &plan, input.Target, found, poolClass)
		return plan
	case found.NodePoolsServed:
		plan.Present = true
		plan.Found = describeFound(found)
		plan.note("This cluster serves karpenter.sh NodePools without EC2NodeClass, so another provisioner owns " +
			"its nodes. It is left as it is.")
		return plan
	case found.ClusterAutoscaler:
		groups, err := kube.AutoscaledNodeGroups(ctx, input.Target)
		if err != nil || len(groups) > 0 {
			plan.Present = true
			plan.Provisioner = ProvisionerClusterAutoscaler
			plan.AutoscaledPools = groups
			noteAutoscaledGroups(&plan, groups, err, autoscalerStatus(found))
			return plan
		}
		plan.note("A cluster autoscaler reports its status in " + autoscalerStatus(found) + " but manages no node " +
			"group, so Karpenter can be installed beside it. The autoscaler is left as it is; Zoorik never changes it.")
	}

	cluster, described, ok := signInEKS(ctx, runner, &plan, input, eksProvisioner)
	if !ok {
		return plan
	}
	info := described.Cluster
	plan.Found = append(plan.Found, "EKS "+text.OrText(info.Version, "version unknown")+", status "+
		text.OrText(info.Status, "unknown")+", authentication mode "+
		text.OrText(info.AccessConfig.AuthenticationMode, "CONFIG_MAP"))
	if described.autoModeOn() {
		plan.Present = true
		plan.Provisioner = ProvisionerAutoMode
		plan.note("AWS reports EKS Auto Mode on, so it is left as it is.")
		return plan
	}
	if info.ResourcesVpcConfig.ClusterSecurityGroupID == "" {
		plan.block("The cluster has no cluster security group, so Karpenter's nodes have no security group to "+
			"discover.", "Tag the node security group with "+discoveryTag+"="+cluster.Name+" by hand, or report "+
			"this. Nothing has been changed.")
	}
	chartCheck := Cmd(helmCLI, "show", "chart", karpenterChart, "--version", DefaultKarpenterVersion)
	if err := runner.HelmChart(chartCheck, karpenterChart, DefaultKarpenterVersion); err != nil {
		plan.block("The Karpenter chart "+DefaultKarpenterVersion+" could not be read: "+err.Error(),
			"Check that this machine can reach public.ecr.aws. A stale login there needs its public.ecr.aws entry "+
				"removed from "+helmRegistryConfig()+" (or `docker logout public.ecr.aws`, where Docker holds the "+
				"login). Nothing has been changed.")
	}
	if len(plan.Blockers) > 0 {
		return plan
	}
	plan.Found = append(plan.Found, "Karpenter chart "+DefaultKarpenterVersion+" resolves at "+karpenterChart)

	subnets, subnetSource := nodeSubnets(ctx, runner, cluster, info.ResourcesVpcConfig.SubnetIDs)
	plan.note("Discovery tags go on the " + subnetSource + " and the cluster security group " +
		info.ResourcesVpcConfig.ClusterSecurityGroupID + ". Karpenter launches nodes only into subnets and " +
		"security groups tagged " + discoveryTag + "=" + cluster.Name + ".")
	plan.note("Karpenter's controller runs on your existing nodes, which Karpenter does not manage.")

	names := namesFor(cluster)
	nodeClass := map[string]any{
		"role":             names.nodeRole,
		"amiSelectorTerms": []any{map[string]any{"alias": eksAMIAlias}},
		"subnetSelectorTerms": []any{map[string]any{"tags": map[string]any{
			discoveryTag: cluster.Name}}},
		"securityGroupSelectorTerms": []any{map[string]any{"tags": map[string]any{
			discoveryTag: cluster.Name}}},
	}
	plan.Steps = append(plan.Steps,
		nodeRoleStep(runner, cluster, names),
		controllerRoleStep(runner, cluster, names),
		queueStep(runner, cluster, names),
		rulesStep(runner, cluster, names),
		tagsStep(runner, cluster, append(subnets, info.ResourcesVpcConfig.ClusterSecurityGroupID)))
	if !acceptsAccessEntries(info.AccessConfig.AuthenticationMode) {
		plan.Steps = append(plan.Steps, authenticationModeStep(runner, cluster))
	}
	plan.Steps = append(plan.Steps,
		accessEntryStep(runner, cluster, names),
		podIdentityAddonStep(runner, cluster, "It hands Karpenter's controller its role. It is an AWS-managed "+
			"DaemonSet in kube-system."),
		podIdentityAssociationStep(runner, cluster, names),
		helmStep(runner, input.Target, cluster, names),
		eksBaseNodeClassStep(input.Target, nodeClass, "the node role and the discovery tags above"),
		karpenterObjectStep(input.Target, kube.EC2NodeClassGroup, kube.KindEC2NodeClass, kube.DefaultProvisionerName,
			nodeClass, "The default NodePool below launches its nodes with this class: Amazon Linux 2023, the node "+
				"role and the discovery tags above. It is the cluster's own, not Zoorik's."),
		defaultNodePoolStep(input.Target, kube.DefaultProvisionerName, ""),
	)
	return plan
}

func planStaticCapacity(ctx context.Context, plan *Plan, runner *Runner, target *kube.Target,
	found kube.Autoprovisioner) {
	controller, gateOff := gateOffController(found)
	version := found.KarpenterVersion()
	if gateOff {
		version = kube.Autoprovisioner{Controllers: []kube.ControllerState{controller}}.KarpenterVersion()
	}
	if gateOff && version != "" && !servesStaticCapacity(version) {
		plan.note("Karpenter " + version + " is installed and running, so it is left as it is. Static capacity, " +
			"which Coral's rebalancing creates its new nodes with, needs Karpenter " + staticCapacityMinimum +
			" or later, so rebalancing cannot create its new nodes on this cluster yet. " +
			karpenterUpgradeRemedy(target, controller, version) + " Node optimization and workload optimization " +
			"work as before.")
		return
	}
	served, err := kube.NodePoolServesReplicas(ctx, target)
	if err != nil {
		plan.note("Karpenter is installed and running, so it is left as it is. Whether its NodePool resource " +
			"serves spec.replicas, which Coral's rebalancing creates its new nodes with, could not be read (" +
			text.OneLine(innerCause(err).Error()) + "). Check that the context's credential may run `" +
			target.Kubectl("get --raw "+kube.KarpenterOpenAPIPath) + "`, then run " + prepareCommand + " again.")
		return
	}
	if !served {
		then := ""
		if gateOff {
			then = " Then turn its static capacity on: " + staticCapacityRemedy(target, controller, version) + "."
		}
		plan.note("Karpenter is installed and running, so it is left as it is. Its NodePool resource definition " +
			"does not serve spec.replicas, so Coral's rebalancing cannot create its new nodes on this cluster yet: " +
			"helm never upgrades a chart's own resource definitions, so they can be older than the controller. " +
			replicasSchemaRemedy(target, version) + then + " Node optimization and workload optimization work " +
			"as before.")
		return
	}
	if !gateOff || found.KarpenterStaticCapacity() {
		plan.note("Karpenter is installed and running with static capacity on, so it is left as it is.")
		return
	}
	own := installedByZoorikctl(controller)
	upgradable := own && version != ""
	if upgradable {
		plan.note("Karpenter " + version + " is installed and running without static capacity, so Coral's " +
			"rebalancing cannot create its new nodes on this cluster yet. Its Helm release " +
			controller.ReleaseName + " in " + controller.ReleaseNamespace + " has the name, namespace and " +
			"interruption queue (" + controller.InterruptionQueue + ") zoorikctl installs Karpenter with, so the " +
			"step below turns static capacity on and leaves the rest of it as it is.")
		plan.Steps = append(plan.Steps, staticCapacityStep(runner, target, controller, version))
		return
	}
	left := "It is the cluster's own, and zoorikctl never changes it."
	if own {
		left = "Its version cannot be read from its image (" + text.OrText(controller.Image, "none") + "), so " +
			"zoorikctl does not upgrade it."
	}
	plan.note("Karpenter is installed and running, so it is left as it is. " + left + " Its static capacity is " +
		"off, so Coral's rebalancing cannot create its new nodes on this cluster. " +
		staticCapacityNeed(target, controller, version) + " Node optimization and workload optimization work as " +
		"before.")
}

func StaticCapacityRemedy(target *kube.Target, found kube.Autoprovisioner,
	state kube.KarpenterStaticCapacity) (string, bool) {
	if state.Ready() {
		return "", false
	}
	if state.GateOn {
		return "Its NodePool resource definition does not serve spec.replicas: helm never upgrades a chart's " +
			"own resource definitions, so they can be older than the controller. " +
			replicasSchemaRemedy(target, found.KarpenterVersion()), true
	}
	controller, gateOff := gateOffController(found)
	if !gateOff {
		return "", false
	}
	version := kube.Autoprovisioner{Controllers: []kube.ControllerState{controller}}.KarpenterVersion()
	return staticCapacityNeed(target, controller, version), true
}

func staticCapacityNeed(target *kube.Target, controller kube.ControllerState, version string) string {
	if version != "" && !servesStaticCapacity(version) {
		return "It needs Karpenter " + staticCapacityMinimum + " or later with static capacity on, and it runs " +
			version + ". " + karpenterUpgradeRemedy(target, controller, version)
	}
	return "It needs Karpenter " + staticCapacityMinimum + " or later with static capacity on: " +
		staticCapacityRemedy(target, controller, version) + "."
}

func servesStaticCapacity(version string) bool {
	return kube.VersionAtLeast(version, staticCapacityMinimumMajor, staticCapacityMinimumMinor)
}

func karpenterUpgradeRemedy(target *kube.Target, controller kube.ControllerState, version string) string {
	controllerStep := "then the controller " + controller.Namespace + "/" + controller.Name + " to " +
		DefaultKarpenterVersion + ", with " + kube.KarpenterStaticCapacityGate + "=true added to its " +
		kube.KarpenterFeatureGatesEnv + " environment variable"
	if controller.ReleaseName != "" {
		controllerStep = "then its release (`" + staticCapacityUpgrade(target, controller.ReleaseName,
			controller.ReleaseNamespace, DefaultKarpenterVersion).String() + "`)"
	}
	return "Upgrade it from " + version + " to " + DefaultKarpenterVersion + " by Karpenter's upgrade guide: its " +
		"resource definitions first (`" + crdUpgrade(target, DefaultKarpenterVersion).String() + "`), " +
		controllerStep + ", then run " + prepareCommand + " again. zoorikctl never moves a running Karpenter to " +
		"another minor version itself."
}

func replicasSchemaRemedy(target *kube.Target, version string) string {
	return "Upgrade its resource definitions to the version it runs (`" +
		crdUpgrade(target, text.OrText(version, installedVersionPlaceholder)).String() + "`), then run " +
		prepareCommand + " again."
}

func crdUpgrade(target *kube.Target, version string) Command {
	args := []string{"upgrade", "--install", karpenterCRDRelease, karpenterCRDChart, "--version", version,
		"--namespace", kube.KarpenterNamespace}
	return Cmd(helmCLI, append(args, target.HelmArgs()...)...)
}

func gateOffController(found kube.Autoprovisioner) (kube.ControllerState, bool) {
	for _, controller := range found.Controllers {
		if controller.Ready > 0 && !controller.StaticCapacity {
			return controller, true
		}
	}
	return kube.ControllerState{}, false
}

func installedByZoorikctl(controller kube.ControllerState) bool {
	return controller.ReleaseName == karpenterRelease && controller.ReleaseNamespace == kube.KarpenterNamespace &&
		strings.HasPrefix(controller.InterruptionQueue, queuePrefix)
}

func staticCapacityRemedy(target *kube.Target, controller kube.ControllerState, version string) string {
	if controller.ReleaseName == "" {
		return "add " + kube.KarpenterStaticCapacityGate + "=true to the " + kube.KarpenterFeatureGatesEnv +
			" environment variable of the controller " + controller.Namespace + "/" + controller.Name
	}
	return "`" + staticCapacityUpgrade(target, controller.ReleaseName, controller.ReleaseNamespace,
		text.OrText(version, installedVersionPlaceholder)).String() + "`"
}

func staticCapacityUpgrade(target *kube.Target, release, namespace, version string) Command {
	args := []string{"upgrade", release, karpenterChart, "--namespace", namespace, "--version", version}
	args = append(args, target.HelmArgs()...)
	return Cmd(helmCLI, append(args, "--reuse-values", "--set", staticCapacityValue, "--wait", "--timeout",
		helmTimeout.String())...)
}

func staticCapacityStep(runner *Runner, target *kube.Target, controller kube.ControllerState, version string) Step {
	upgrade := staticCapacityUpgrade(target, controller.ReleaseName, controller.ReleaseNamespace, version)
	release := helmRelease{target: target, chart: karpenterChart, version: version, release: controller.ReleaseName,
		namespace: controller.ReleaseNamespace, values: []string{staticCapacityValue}, reuse: true, wait: true,
		timeout: helmTimeout}
	return Step{
		Title: "Turn on Karpenter's static capacity",
		Why: "Coral's rebalancing creates its new nodes as NodePools with a fixed count, which Karpenter serves only " +
			"with its " + kube.KarpenterStaticCapacityGate + " feature gate on. helm upgrades the release " +
			controller.ReleaseName + " with the version it runs (" + version + "), keeps every value it has and " +
			"adds only " + staticCapacityValue + ". The controller restarts with the gate on; its NodePools, node " +
			"classes and nodes are not touched. " + helmLibrary,
		Commands:   []string{upgrade.String()},
		NotInPlace: StaticCapacityNotInPlace,
		Check: func(ctx context.Context) (bool, error) {
			return kube.KarpenterReadyWithStaticCapacity(ctx, target)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			note("helm is upgrading Karpenter and waiting for it to be ready")
			return runner.Helm(ctx, upgrade, release)
		},
	}
}

func eksDefaultPoolClass(plan *Plan, found kube.Autoprovisioner) (string, bool) {
	if found.HasCustomerNodePool() {
		return "", true
	}
	plan.Poolless = true
	if class, ok := found.CustomerClass(kube.KindEC2NodeClass); ok {
		return class, true
	}
	plan.block("Karpenter runs, and the cluster has no NodePool of its own (Zoorik's zoorik-* pools do not count), "+
		"so nothing adds nodes for Pending pods. zoorikctl gives it Karpenter's default NodePool on one of the "+
		"cluster's own "+kube.KindEC2NodeClass+" objects, and the cluster has none.", "An "+kube.KindEC2NodeClass+
		" names the node role and the subnet and security group selectors of the nodes Karpenter launches, and "+
		"zoorikctl copies those from the cluster rather than invent them. Create "+kube.KindEC2NodeClass+" "+
		kube.DefaultProvisionerName+" (Amazon Linux 2023, amiSelectorTerms alias "+eksAMIAlias+", with your node "+
		"role and your subnet and security group selectors), then run "+prepareCommand+" again: it adds NodePool "+
		kube.DefaultProvisionerName+" on it. Nothing has been changed.")
	return "", false
}

func planAutoModeBuiltInPools(ctx context.Context, runner *Runner, plan *Plan, input Input) {
	plan.Poolless = true
	plan.note("The cluster has no NodePool of its own (Zoorik's zoorik-* pools do not count), so nothing adds " +
		"nodes for Pending pods until it has one. Apart from its built-in NodePools, EKS Auto Mode's settings and " +
		"the cluster's node classes stay as they are.")
	cluster, described, ok := signInEKS(ctx, runner, plan, input, eksBuiltInPools)
	if !ok {
		return
	}
	plan.Found = append(plan.Found, described.autoModeReport())
	switch {
	case !described.autoModeOn():
		plan.block("AWS reports EKS Auto Mode's compute off, so it has no built-in NodePools to turn on, though "+
			"this cluster serves its NodeClass resource and has no NodePool of its own.", "EKS takes Auto Mode's "+
			"compute, load balancing and block storage together, so turning its NodePools "+
			strings.Join(autoModeBuiltInPools, " and ")+" on turns EKS Auto Mode on. "+autoModeRoleRemedy(cluster))
	case described.autoModeRole() == "":
		plan.block("EKS Auto Mode has no node role, so its built-in NodePools cannot be turned on: they launch "+
			"their nodes with it, and zoorikctl does not create one.", autoModeRoleRemedy(cluster))
	default:
		plan.Steps = append(plan.Steps, autoModeBuiltInPoolsStep(runner, cluster, described.autoModeRole()))
	}
}

func autoModeRoleRemedy(cluster EKSCluster) string {
	return "Run `" + autoModeBuiltInPoolsCommand(cluster, "<node role ARN>").String() + "` with the ARN of an IAM " +
		"role for EKS Auto Mode nodes (trusted by ec2.amazonaws.com, with AmazonEKSWorkerNodeMinimalPolicy and " +
		"AmazonEC2ContainerRegistryPullOnly attached), then run " + prepareCommand + " again. Nothing has been " +
		"changed."
}

func autoModeBuiltInPoolsCommand(cluster EKSCluster, role string) Command {
	enabled := map[string]any{"enabled": true}
	return Cmd(awsCLI, awsArgs(cluster, "eks", "update-cluster-config", "--name", cluster.Name,
		"--compute-config", compactJSON(map[string]any{"enabled": true, "nodePools": autoModeBuiltInPools,
			"nodeRoleArn": role}),
		"--kubernetes-network-config", compactJSON(map[string]any{"elasticLoadBalancing": enabled}),
		"--storage-config", compactJSON(map[string]any{"blockStorage": enabled}))...)
}

func autoModeBuiltInPoolsStep(runner *Runner, cluster EKSCluster, role string) Step {
	update := autoModeBuiltInPoolsCommand(cluster, role)
	describe := Cmd(awsCLI, awsArgs(cluster, "eks", "describe-cluster", "--name", cluster.Name)...)
	on := func(ctx context.Context) (bool, error) {
		var described eksDescription
		if err := runner.JSON(ctx, describe, &described); err != nil {
			return false, err
		}
		return described.builtInPoolsOn(), nil
	}
	pools := strings.Join(autoModeBuiltInPools, " and ")
	return Step{
		Title: "Turn on EKS Auto Mode's built-in NodePools for " + cluster.Name,
		Why: "The cluster has no NodePool of its own (Zoorik's zoorik-* pools do not count), so nothing adds nodes " +
			"for Pending pods. EKS Auto Mode's built-in NodePools " + pools + " add them, as on a cluster created " +
			"with EKS Auto Mode, and launch their nodes with its node role " + role + ": they are the cluster's own " +
			"provisioner, and Zoorik still creates no machine. EKS takes Auto Mode's compute, load balancing and " +
			"block storage together, so the last two are stated on, as they are. EKS Auto Mode's other settings, " +
			"its node classes, the existing node groups, their nodes and their pods are not touched.",
		Commands: []string{update.String()},
		Check:    on,
		Apply: func(ctx context.Context, note func(string)) error {
			if err := runner.Run(ctx, update); err != nil {
				return err
			}
			note("EKS is updating the cluster; it usually takes a few minutes")
			done, err := waitFor(ctx, func() (bool, error) { return on(ctx) })
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("EKS did not report the built-in NodePools %s on %s within %s; run prepare again",
					pools, cluster.Name, crdWait)
			}
			return nil
		},
	}
}

func defaultNodePoolStep(target *kube.Target, nodeClass, lead string) Step {
	return karpenterObjectStep(target, kube.KarpenterGroup, kube.KindNodePool, kube.DefaultProvisionerName,
		defaultNodePool(nodeClass), lead+"Karpenter's getting-started NodePool: on-demand amd64 Linux instances of "+
			"the c, m and r categories, generation 3 or later, at most 1000 CPUs in all, each replaced after 720 "+
			"hours and consolidated a minute after it is empty or underused. It adds nodes for Pending pods: it is "+
			"the cluster's own provisioner, and Zoorik still creates no machine.")
}

func planEKSBaseNodeClass(ctx context.Context, plan *Plan, target *kube.Target, found kube.Autoprovisioner,
	poolClass string) {
	if found.Has(kube.KindEC2NodeClass, kube.BaseNodeClassName) {
		return
	}
	source, err := kube.BaseNodeClassSource(ctx, target)
	if err == nil && source == nil && poolClass != "" {
		source, err = kube.NodeClassSourceFor(ctx, target, kube.DefaultProvisionerName, poolClass)
	}
	if err == nil && copyable(source) {
		plan.Steps = append(plan.Steps, eksBaseNodeClassStep(target, copiedNodeClass(source), "the "+
			nodeIdentityOf(source)+", subnet selectors and security group selectors of the cluster's own "+
			kube.KindEC2NodeClass+" "+source.NodeClass+" (used by NodePool "+source.NodePool+")"))
		return
	}
	lead := "Zoorik's own zoorik-* pools clone Zoorik's base node class " + kube.BaseNodeClassName + ", and " +
		"rebalancing creates its new nodes with it when the cluster has no NodePool of its own to copy. zoorikctl " +
		"copies its node role and selectors from the cluster's own " + kube.KindEC2NodeClass + ", and "
	if err != nil {
		plan.note(lead + "the cluster's NodePools and " + kube.KindEC2NodeClass + " objects could not be read (" +
			text.OneLine(innerCause(err).Error()) + "). Check that the context's credential may run `" +
			target.Kubectl("get "+found.NodePools.GroupResource().String()+","+
				found.EC2NodeClasses.GroupResource().String()) + "`, then run " + prepareCommand + " again.")
		return
	}
	plan.note(lead + "none of the cluster's own NodePools uses an " + kube.KindEC2NodeClass + " it can copy. " +
		"Add a NodePool of your own on an " + kube.KindEC2NodeClass + " and run " + prepareCommand + " again, or " +
		"create " + kube.KindEC2NodeClass + " " + kube.BaseNodeClassName + " yourself (Amazon Linux 2023, " +
		"amiSelectorTerms alias " + eksAMIAlias + ", with your node role and your subnet and security group " +
		"selectors).")
}

func copyable(source *kube.EC2NodeClassSource) bool {
	return source != nil && (source.Role != "" || source.InstanceProfile != "") &&
		len(source.SubnetSelectorTerms) > 0 && len(source.SecurityGroupSelectorTerms) > 0
}

func nodeIdentityOf(source *kube.EC2NodeClassSource) string {
	if source.Role != "" {
		return "node role " + source.Role
	}
	return "instance profile " + source.InstanceProfile
}

func copiedNodeClass(source *kube.EC2NodeClassSource) map[string]any {
	spec := map[string]any{
		"amiSelectorTerms":           []any{map[string]any{"alias": eksAMIAlias}},
		"subnetSelectorTerms":        source.SubnetSelectorTerms,
		"securityGroupSelectorTerms": source.SecurityGroupSelectorTerms,
	}
	if source.Role != "" {
		spec["role"] = source.Role
	} else {
		spec["instanceProfile"] = source.InstanceProfile
	}
	return spec
}

func eksBaseNodeClassStep(target *kube.Target, spec map[string]any, copied string) Step {
	return karpenterObjectStep(target, kube.EC2NodeClassGroup, kube.KindEC2NodeClass, kube.BaseNodeClassName, spec,
		"Zoorik's own zoorik-* pools clone this node class, and rebalancing creates its new nodes with it when the "+
			"cluster has no NodePool of its own to copy. It uses Amazon Linux 2023, the image live migration "+
			"supports on EKS, with "+copied+". It is not a pool and creates no node.")
}

func autoscalerStatus(found kube.Autoprovisioner) string {
	return found.ClusterAutoscalerNamespace + "/" + kube.ClusterAutoscalerStatus
}

func noteAutoscaledGroups(plan *Plan, groups []string, unread error, status string) {
	managed := "its node groups"
	if len(groups) > 0 {
		managed = "node group(s) " + strings.Join(groups, ", ")
	}
	plan.note("A cluster autoscaler manages " + managed + " (its status is in " + status + ") and " +
		"adds nodes for Pending pods, so Karpenter is not installed. The cluster keeps its autoscaler; Zoorik never " +
		"changes it.")
	if unread != nil {
		plan.note("The node groups it manages could not be read from " + status + " (" +
			text.OneLine(innerCause(unread).Error()) + ").")
	}
	plan.note("Coral's rebalancing needs Karpenter, and Zoorik installs it only where the cluster autoscaler does " +
		"not manage node groups. Take " + managed + " out of the cluster autoscaler (remove the " +
		autoscalerDiscoveryTag + " tag from their Auto Scaling groups, or remove the autoscaler), then run " +
		prepareCommand + " again once its status no longer lists them; each group keeps the nodes it has. Until " +
		"then rebalancing is not available on this cluster; node optimization and workload optimization work as " +
		"before.")
}

func innerCause(err error) error {
	if inner := errors.Unwrap(err); inner != nil {
		return inner
	}
	return err
}

func defaultNodePool(nodeClass string) map[string]any {
	requirement := func(key, operator string, values ...any) map[string]any {
		return map[string]any{"key": key, "operator": operator, "values": values}
	}
	return map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"requirements": []any{
					requirement(kube.ArchLabel, "In", "amd64"),
					requirement(kube.OSLabel, "In", "linux"),
					requirement("karpenter.sh/capacity-type", "In", "on-demand"),
					requirement("karpenter.k8s.aws/instance-category", "In", "c", "m", "r"),
					requirement("karpenter.k8s.aws/instance-generation", "Gt", "2"),
				},
				"nodeClassRef": map[string]any{"group": kube.EC2NodeClassGroup, "kind": kube.KindEC2NodeClass,
					"name": nodeClass},
				"expireAfter": "720h",
			},
		},
		"limits":     map[string]any{"cpu": int64(1000)},
		"disruption": map[string]any{"consolidationPolicy": "WhenEmptyOrUnderutilized", "consolidateAfter": "1m"},
	}
}

func describeEKSCluster(ctx context.Context, runner *Runner, plan *Plan, cluster EKSCluster, p purpose) (
	eksDescription, bool) {
	plan.Cluster = cluster.ARN()
	region := []string{"--region", cluster.Region}

	var caller struct {
		Account string `json:"Account"`
		Arn     string `json:"Arn"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, append([]string{"sts", "get-caller-identity", "--output", "json"},
		region...)...), &caller); err != nil {
		plan.blockTooling("The AWS CLI has no usable credentials: "+err.Error(),
			"Sign in (`aws sso login`, or set AWS_PROFILE) to account "+cluster.Account+" as someone allowed to "+
				p.allowed+", and "+again(p.removal)+". Nothing has been changed.")
		return eksDescription{}, false
	}
	plan.Credential = "your AWS CLI credentials (" + caller.Arn + ")"
	if caller.Account != cluster.Account {
		plan.block("Your AWS credentials are for account "+caller.Account+", and the cluster is in account "+
			cluster.Account+".", "Switch to a profile in account "+cluster.Account+" and "+again(p.removal)+
			". Nothing has been changed.")
		return eksDescription{}, false
	}

	var described eksDescription
	describe := Cmd(awsCLI, append([]string{"eks", "describe-cluster", "--name", cluster.Name, "--output", "json"},
		region...)...)
	if err := runner.JSON(ctx, describe, &described); err != nil {
		plan.block("AWS would not describe the cluster: "+err.Error(),
			"Check that your credentials may read "+cluster.ARN()+" (`"+describe.String()+"`), and "+
				again(p.removal)+". Nothing has been changed.")
		return eksDescription{}, false
	}
	return described, true
}

func resolveEKSCluster(ctx context.Context, runner *Runner, input Input) (EKSCluster, error) {
	if input.Resource != nil {
		return eksClusterOf(*input.Resource)
	}
	if resource, ok := kube.ParseClusterResource(input.Target.ClusterName); ok && resource.Cloud == kube.CloudAWS {
		return eksClusterOf(resource)
	}
	if input.Region == "" {
		return EKSCluster{}, fmt.Errorf("no node names its AWS region, so the EKS cluster cannot be found")
	}
	var listed struct {
		Clusters []string `json:"clusters"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, "eks", "list-clusters", "--region", input.Region, "--output", "json"),
		&listed); err != nil {
		return EKSCluster{}, fmt.Errorf("the EKS clusters in %s could not be listed: %w", input.Region, err)
	}
	server := strings.TrimRight(strings.ToLower(input.Target.ServerURL), "/")
	for _, name := range listed.Clusters {
		var described eksDescription
		if err := runner.JSON(ctx, Cmd(awsCLI, "eks", "describe-cluster", "--name", name, "--region", input.Region,
			"--output", "json"), &described); err != nil {
			continue
		}
		if strings.TrimRight(strings.ToLower(described.Cluster.Endpoint), "/") == server {
			if resource, ok := kube.ParseClusterResource(described.Cluster.Arn); ok {
				return eksClusterOf(resource)
			}
		}
	}
	return EKSCluster{}, fmt.Errorf("no EKS cluster in %s has the API server %s", input.Region, input.Target.ServerURL)
}

func eksClusterOf(resource kube.ClusterResource) (EKSCluster, error) {
	if resource.Cloud != kube.CloudAWS {
		return EKSCluster{}, fmt.Errorf("--cluster-resource-id names an AKS cluster, and this cluster runs on AWS")
	}
	partition, _, _ := strings.Cut(strings.TrimPrefix(strings.ToLower(resource.Raw), "arn:"), ":")
	return EKSCluster{Partition: partition, Region: resource.Region, Account: resource.Account, Name: resource.Name},
		nil
}

func nodeSubnets(ctx context.Context, runner *Runner, cluster EKSCluster, clusterSubnets []string) ([]string, string) {
	region := []string{"--region", cluster.Region, "--output", "json"}
	var groups struct {
		Nodegroups []string `json:"nodegroups"`
	}
	seen := map[string]bool{}
	if runner.JSON(ctx, Cmd(awsCLI, append([]string{"eks", "list-nodegroups", "--cluster-name", cluster.Name},
		region...)...), &groups) == nil {
		for _, group := range groups.Nodegroups {
			var described struct {
				Nodegroup struct {
					Subnets []string `json:"subnets"`
				} `json:"nodegroup"`
			}
			if runner.JSON(ctx, Cmd(awsCLI, append([]string{"eks", "describe-nodegroup", "--cluster-name",
				cluster.Name, "--nodegroup-name", group}, region...)...), &described) != nil {
				continue
			}
			for _, subnet := range described.Nodegroup.Subnets {
				seen[subnet] = true
			}
		}
	}
	source := "subnets of the cluster's managed node groups"
	if len(seen) == 0 {
		for _, subnet := range clusterSubnets {
			seen[subnet] = true
		}
		source = "cluster's own subnets (it has no managed node group to copy subnets from)"
	}
	subnets := make([]string, 0, len(seen))
	for subnet := range seen {
		subnets = append(subnets, subnet)
	}
	sort.Strings(subnets)
	return subnets, source
}

func awsArgs(cluster EKSCluster, args ...string) []string {
	return append(args, "--region", cluster.Region, "--output", "json")
}

func nodeRoleStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	trust := compactJSON(map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]any{"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}}})
	create := Cmd(awsCLI, awsArgs(cluster, "iam", "create-role", "--role-name", names.nodeRole,
		"--assume-role-policy-document", trust, "--tags", "Key="+discoveryTag+",Value="+cluster.Name)...)
	commands := []string{create.String()}
	attach := make([]Command, 0, len(nodeRolePolicies))
	for _, policy := range nodeRolePolicies {
		command := Cmd(awsCLI, awsArgs(cluster, "iam", "attach-role-policy", "--role-name", names.nodeRole,
			"--policy-arn", "arn:"+cluster.Partition+":iam::aws:policy/"+policy)...)
		attach = append(attach, command)
		commands = append(commands, command.String())
	}
	roleExists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role", "--role-name", names.nodeRole)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	attached := func(ctx context.Context) (map[string]bool, error) {
		var listed struct {
			AttachedPolicies []struct {
				PolicyName string `json:"PolicyName"`
			} `json:"AttachedPolicies"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "list-attached-role-policies", "--role-name",
			names.nodeRole)...), &listed)
		if err != nil {
			return nil, err
		}
		present := map[string]bool{}
		for _, policy := range listed.AttachedPolicies {
			present[policy.PolicyName] = true
		}
		return present, nil
	}
	return Step{
		Title: "Create the node role " + names.nodeRole,
		Why: "Karpenter's nodes join the cluster with this role. It carries the four AWS-managed policies an EKS " +
			"node needs, and nothing else.",
		Commands: commands,
		Check: func(ctx context.Context) (bool, error) {
			exists, err := roleExists(ctx)
			if err != nil || !exists {
				return false, err
			}
			present, err := attached(ctx)
			if err != nil {
				return false, err
			}
			for _, policy := range nodeRolePolicies {
				if !present[policy] {
					return false, nil
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context, note func(string)) error {
			exists, err := roleExists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				if err := runner.Run(ctx, create); err != nil {
					return err
				}
			}
			present, err := attached(ctx)
			if err != nil {
				return err
			}
			for i, policy := range nodeRolePolicies {
				if present[policy] {
					continue
				}
				note("attaching " + policy)
				if err := runner.Run(ctx, attach[i]); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func controllerPolicy(cluster EKSCluster, names eksNames) string {
	partition, region, account, name := cluster.Partition, cluster.Region, cluster.Account, cluster.Name
	arn := func(service, resource string) string {
		return "arn:" + partition + ":" + service + ":" + region + ":" + account + ":" + resource
	}
	ec2 := func(resource string) string { return "arn:" + partition + ":ec2:" + region + ":*:" + resource }
	clusterTag := "aws:ResourceTag/kubernetes.io/cluster/" + name
	requestClusterTag := "aws:RequestTag/kubernetes.io/cluster/" + name
	instanceProfiles := "arn:" + partition + ":iam::" + account + ":instance-profile/*"
	statements := []any{
		map[string]any{"Sid": "AllowScopedEC2InstanceAccessActions", "Effect": "Allow",
			"Action": []string{"ec2:RunInstances", "ec2:CreateFleet"},
			"Resource": []string{ec2("image/*"), ec2("snapshot/*"), ec2("security-group/*"), ec2("subnet/*"),
				ec2("capacity-reservation/*")}},
		map[string]any{"Sid": "AllowScopedEC2LaunchTemplateAccessActions", "Effect": "Allow",
			"Action": []string{"ec2:RunInstances", "ec2:CreateFleet"}, "Resource": ec2("launch-template/*"),
			"Condition": map[string]any{"StringEquals": map[string]any{clusterTag: "owned"},
				"StringLike": map[string]any{"aws:ResourceTag/karpenter.sh/nodepool": "*"}}},
		map[string]any{"Sid": "AllowScopedEC2InstanceActionsWithTags", "Effect": "Allow",
			"Action": []string{"ec2:RunInstances", "ec2:CreateFleet", "ec2:CreateLaunchTemplate"},
			"Resource": []string{ec2("fleet/*"), ec2("instance/*"), ec2("volume/*"), ec2("network-interface/*"),
				ec2("launch-template/*"), ec2("spot-instances-request/*")},
			"Condition": map[string]any{"StringEquals": map[string]any{requestClusterTag: "owned",
				"aws:RequestTag/eks:eks-cluster-name": name},
				"StringLike": map[string]any{"aws:RequestTag/karpenter.sh/nodepool": "*"}}},
		map[string]any{"Sid": "AllowScopedResourceCreationTagging", "Effect": "Allow", "Action": "ec2:CreateTags",
			"Resource": []string{ec2("fleet/*"), ec2("instance/*"), ec2("volume/*"), ec2("network-interface/*"),
				ec2("launch-template/*"), ec2("spot-instances-request/*")},
			"Condition": map[string]any{"StringEquals": map[string]any{requestClusterTag: "owned",
				"aws:RequestTag/eks:eks-cluster-name": name,
				"ec2:CreateAction":                    []string{"RunInstances", "CreateFleet", "CreateLaunchTemplate"}},
				"StringLike": map[string]any{"aws:RequestTag/karpenter.sh/nodepool": "*"}}},
		map[string]any{"Sid": "AllowScopedResourceTagging", "Effect": "Allow", "Action": "ec2:CreateTags",
			"Resource": ec2("instance/*"),
			"Condition": map[string]any{"StringEquals": map[string]any{clusterTag: "owned"},
				"StringLike":           map[string]any{"aws:ResourceTag/karpenter.sh/nodepool": "*"},
				"StringEqualsIfExists": map[string]any{"aws:RequestTag/eks:eks-cluster-name": name},
				"ForAllValues:StringEquals": map[string]any{"aws:TagKeys": []string{"eks:eks-cluster-name",
					"karpenter.sh/nodeclaim", "Name"}}}},
		map[string]any{"Sid": "AllowScopedDeletion", "Effect": "Allow",
			"Action":   []string{"ec2:TerminateInstances", "ec2:DeleteLaunchTemplate"},
			"Resource": []string{ec2("instance/*"), ec2("launch-template/*")},
			"Condition": map[string]any{"StringEquals": map[string]any{clusterTag: "owned"},
				"StringLike": map[string]any{"aws:ResourceTag/karpenter.sh/nodepool": "*"}}},
		map[string]any{"Sid": "AllowRegionalReadActions", "Effect": "Allow",
			"Action": []string{"ec2:DescribeAvailabilityZones", "ec2:DescribeCapacityReservations",
				"ec2:DescribeImages", "ec2:DescribeInstances", "ec2:DescribeInstanceTypeOfferings",
				"ec2:DescribeInstanceTypes", "ec2:DescribeLaunchTemplates", "ec2:DescribeSecurityGroups",
				"ec2:DescribeSpotPriceHistory", "ec2:DescribeSubnets"},
			"Resource":  "*",
			"Condition": map[string]any{"StringEquals": map[string]any{"aws:RequestedRegion": region}}},
		map[string]any{"Sid": "AllowSSMReadActions", "Effect": "Allow", "Action": "ssm:GetParameter",
			"Resource": "arn:" + partition + ":ssm:" + region + "::parameter/aws/service/*"},
		map[string]any{"Sid": "AllowPricingReadActions", "Effect": "Allow", "Action": "pricing:GetProducts",
			"Resource": "*"},
		map[string]any{"Sid": "AllowInterruptionQueueActions", "Effect": "Allow",
			"Action":   []string{"sqs:DeleteMessage", "sqs:GetQueueUrl", "sqs:ReceiveMessage"},
			"Resource": arn("sqs", names.queue)},
		map[string]any{"Sid": "AllowPassingInstanceRole", "Effect": "Allow", "Action": "iam:PassRole",
			"Resource": "arn:" + partition + ":iam::" + account + ":role/" + names.nodeRole,
			"Condition": map[string]any{"StringEquals": map[string]any{"iam:PassedToService": []string{
				"ec2.amazonaws.com", "ec2.amazonaws.com.cn"}}}},
		map[string]any{"Sid": "AllowScopedInstanceProfileCreationActions", "Effect": "Allow",
			"Action": "iam:CreateInstanceProfile", "Resource": instanceProfiles,
			"Condition": map[string]any{"StringEquals": map[string]any{requestClusterTag: "owned",
				"aws:RequestTag/eks:eks-cluster-name": name, "aws:RequestTag/topology.kubernetes.io/region": region},
				"StringLike": map[string]any{"aws:RequestTag/karpenter.k8s.aws/ec2nodeclass": "*"}}},
		map[string]any{"Sid": "AllowScopedInstanceProfileTagActions", "Effect": "Allow",
			"Action": "iam:TagInstanceProfile", "Resource": instanceProfiles,
			"Condition": map[string]any{"StringEquals": map[string]any{clusterTag: "owned",
				"aws:ResourceTag/topology.kubernetes.io/region": region, requestClusterTag: "owned",
				"aws:RequestTag/eks:eks-cluster-name": name, "aws:RequestTag/topology.kubernetes.io/region": region},
				"StringLike": map[string]any{"aws:ResourceTag/karpenter.k8s.aws/ec2nodeclass": "*",
					"aws:RequestTag/karpenter.k8s.aws/ec2nodeclass": "*"}}},
		map[string]any{"Sid": "AllowScopedInstanceProfileActions", "Effect": "Allow",
			"Action": []string{"iam:AddRoleToInstanceProfile", "iam:RemoveRoleFromInstanceProfile",
				"iam:DeleteInstanceProfile"},
			"Resource": instanceProfiles,
			"Condition": map[string]any{"StringEquals": map[string]any{clusterTag: "owned",
				"aws:ResourceTag/topology.kubernetes.io/region": region},
				"StringLike": map[string]any{"aws:ResourceTag/karpenter.k8s.aws/ec2nodeclass": "*"}}},
		map[string]any{"Sid": "AllowInstanceProfileReadActions", "Effect": "Allow",
			"Action": []string{"iam:GetInstanceProfile", "iam:ListInstanceProfiles"}, "Resource": "*"},
		map[string]any{"Sid": "AllowAPIServerEndpointDiscovery", "Effect": "Allow", "Action": "eks:DescribeCluster",
			"Resource": arn("eks", "cluster/"+name)},
	}
	return compactJSON(map[string]any{"Version": "2012-10-17", "Statement": statements})
}

func podIdentityTrust() string {
	return compactJSON(map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]any{"Service": "pods.eks.amazonaws.com"},
		"Action": []string{"sts:AssumeRole", "sts:TagSession"}}}})
}

func controllerRoleStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	create := Cmd(awsCLI, awsArgs(cluster, "iam", "create-role", "--role-name", names.controllerRole,
		"--assume-role-policy-document", podIdentityTrust(), "--tags", "Key="+discoveryTag+",Value="+cluster.Name)...)
	put := Cmd(awsCLI, awsArgs(cluster, "iam", "put-role-policy", "--role-name", names.controllerRole,
		"--policy-name", controllerPolicyName, "--policy-document", controllerPolicy(cluster, names))...)
	roleExists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role", "--role-name", names.controllerRole)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	policyExists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role-policy", "--role-name",
			names.controllerRole, "--policy-name", controllerPolicyName)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return Step{
		Title: "Create the controller role " + names.controllerRole,
		Why: "Karpenter's controller assumes this role through EKS Pod Identity. Its policy is Karpenter's own, " +
			"scoped to this cluster: it launches and terminates only instances tagged for " + cluster.Name +
			", passes only " + names.nodeRole + ", and reads only the interruption queue " + names.queue + ".",
		Commands: []string{create.String(), put.String()},
		Check: func(ctx context.Context) (bool, error) {
			exists, err := roleExists(ctx)
			if err != nil || !exists {
				return false, err
			}
			return policyExists(ctx)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			exists, err := roleExists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				if err := runner.Run(ctx, create); err != nil {
					return err
				}
			}
			present, err := policyExists(ctx)
			if err != nil || present {
				return err
			}
			note("adding " + controllerPolicyName)
			return runner.Run(ctx, put)
		},
	}
}

func queueARN(cluster EKSCluster, names eksNames) string {
	return "arn:" + cluster.Partition + ":sqs:" + cluster.Region + ":" + cluster.Account + ":" + names.queue
}

func queueStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	policy := compactJSON(map[string]any{"Version": "2012-10-17", "Id": "EC2InterruptionPolicy",
		"Statement": []any{
			map[string]any{"Effect": "Allow", "Principal": map[string]any{"Service": []string{
				"events.amazonaws.com", "sqs.amazonaws.com"}}, "Action": "sqs:SendMessage",
				"Resource": queueARN(cluster, names)},
			map[string]any{"Sid": "DenyHTTP", "Effect": "Deny", "Action": "sqs:*", "Resource": queueARN(cluster, names),
				"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": false}},
				"Principal": "*"},
		}})
	attributes := compactJSON(map[string]string{"MessageRetentionPeriod": "300", "SqsManagedSseEnabled": "true",
		"Policy": policy})
	create := Cmd(awsCLI, awsArgs(cluster, "sqs", "create-queue", "--queue-name", names.queue, "--attributes",
		attributes, "--tags", discoveryTag+"="+cluster.Name)...)
	exists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "sqs", "get-queue-url", "--queue-name", names.queue)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return Step{
		Title: "Create the interruption queue " + names.queue,
		Why: "AWS sends spot interruptions, rebalance recommendations, scheduled maintenance and instance state " +
			"changes here, so Karpenter drains a node before AWS takes it away. Messages expire after 5 minutes.",
		Commands: []string{create.String()},
		Check:    exists,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := exists(ctx)
			if err != nil || present {
				return err
			}
			return runner.Run(ctx, create)
		},
	}
}

func rulesStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	type rule struct {
		name    string
		put     Command
		targets Command
	}
	rules := make([]rule, 0, len(interruptionRules))
	var commands []string
	for _, entry := range interruptionRules {
		name := names.rulePrefix + entry.suffix
		put := Cmd(awsCLI, awsArgs(cluster, "events", "put-rule", "--name", name, "--event-pattern",
			compactJSON(entry.pattern), "--tags", "Key="+discoveryTag+",Value="+cluster.Name)...)
		targets := Cmd(awsCLI, awsArgs(cluster, "events", "put-targets", "--rule", name, "--targets",
			compactJSON([]map[string]string{{"Id": queueTargetID, "Arn": queueARN(cluster, names)}}))...)
		rules = append(rules, rule{name: name, put: put, targets: targets})
		commands = append(commands, put.String(), targets.String())
	}
	targeted := func(ctx context.Context, name string) (bool, error) {
		var listed struct {
			Targets []struct {
				Arn string `json:"Arn"`
			} `json:"Targets"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "events", "list-targets-by-rule", "--rule", name)...),
			&listed)
		if NotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, target := range listed.Targets {
			if target.Arn == queueARN(cluster, names) {
				return true, nil
			}
		}
		return false, nil
	}
	return Step{
		Title:    "Route interruption events to " + names.queue,
		Why:      "Four EventBridge rules send the EC2 and AWS Health events Karpenter acts on to the queue.",
		Commands: commands,
		Check: func(ctx context.Context) (bool, error) {
			for _, entry := range rules {
				done, err := targeted(ctx, entry.name)
				if err != nil || !done {
					return false, err
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context, note func(string)) error {
			for _, entry := range rules {
				done, err := targeted(ctx, entry.name)
				if err != nil {
					return err
				}
				if done {
					continue
				}
				note("rule " + entry.name)
				if err := runner.Run(ctx, entry.put); err != nil {
					return err
				}
				if err := runner.Run(ctx, entry.targets); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func tagsStep(runner *Runner, cluster EKSCluster, resources []string) Step {
	var kept []string
	for _, resource := range resources {
		if resource != "" {
			kept = append(kept, resource)
		}
	}
	untagged := func(ctx context.Context) ([]string, []string, error) {
		var described struct {
			Tags []struct {
				ResourceID string `json:"ResourceId"`
				Value      string `json:"Value"`
			} `json:"Tags"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "ec2", "describe-tags", "--filters",
			"Name=resource-id,Values="+strings.Join(kept, ","), "Name=key,Values="+discoveryTag)...), &described)
		if err != nil {
			return nil, nil, err
		}
		tagged := map[string]string{}
		for _, tag := range described.Tags {
			tagged[tag.ResourceID] = tag.Value
		}
		var missing, foreign []string
		for _, resource := range kept {
			value, present := tagged[resource]
			switch {
			case !present:
				missing = append(missing, resource)
			case value != cluster.Name:
				foreign = append(foreign, resource+" ("+value+")")
			}
		}
		return missing, foreign, nil
	}
	create := Cmd(awsCLI, awsArgs(cluster, append([]string{"ec2", "create-tags", "--resources"},
		append(append([]string{}, kept...), "--tags", "Key="+discoveryTag+",Value="+cluster.Name)...)...)...)
	return Step{
		Title: fmt.Sprintf("Tag %d subnet(s) and the cluster security group for discovery", len(kept)-1),
		Why: "Karpenter picks subnets and security groups by the tag " + discoveryTag + "=" + cluster.Name +
			". A tag is only a label; the subnets and the security group are otherwise unchanged.",
		Commands: []string{create.String()},
		Check: func(ctx context.Context) (bool, error) {
			missing, _, err := untagged(ctx)
			return err == nil && len(missing) == 0, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			missing, foreign, err := untagged(ctx)
			if err != nil {
				return err
			}
			if len(foreign) > 0 {
				note("left as they are, tagged for another cluster: " + strings.Join(foreign, ", "))
			}
			if len(missing) == 0 {
				return nil
			}
			return runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, append([]string{"ec2", "create-tags", "--resources"},
				append(missing, "--tags", "Key="+discoveryTag+",Value="+cluster.Name)...)...)...))
		},
	}
}

func acceptsAccessEntries(mode string) bool {
	switch strings.ToUpper(mode) {
	case accessModeAPI, accessModeAPIAndConfigMap:
		return true
	}
	return false
}

func authenticationModeStep(runner *Runner, cluster EKSCluster) Step {
	update := Cmd(awsCLI, awsArgs(cluster, "eks", "update-cluster-config", "--name", cluster.Name,
		"--access-config", "authenticationMode="+accessModeAPIAndConfigMap)...)
	describe := Cmd(awsCLI, awsArgs(cluster, "eks", "describe-cluster", "--name", cluster.Name)...)
	switched := func(ctx context.Context) (bool, error) {
		var described eksDescription
		if err := runner.JSON(ctx, describe, &described); err != nil {
			return false, err
		}
		return acceptsAccessEntries(described.Cluster.AccessConfig.AuthenticationMode), nil
	}
	return Step{
		Title: "Let " + cluster.Name + " grant node access with access entries",
		Why: "The cluster authenticates nodes only through the aws-auth ConfigMap, and Karpenter's node role is " +
			"granted access with an access entry. " + accessModeAPIAndConfigMap + " adds access entries and keeps " +
			"every aws-auth mapping; EKS does not switch a cluster back to CONFIG_MAP.",
		Commands: []string{update.String()},
		Check:    switched,
		Apply: func(ctx context.Context, note func(string)) error {
			done, err := switched(ctx)
			if err != nil || done {
				return err
			}
			if err := runner.Run(ctx, update); err != nil {
				return err
			}
			note("EKS is switching the authentication mode; it usually takes a few minutes")
			done, err = waitFor(ctx, func() (bool, error) { return switched(ctx) })
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("EKS did not finish switching %s to %s within %s; run prepare again",
					cluster.Name, accessModeAPIAndConfigMap, crdWait)
			}
			return nil
		},
	}
}

func accessEntryStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	role := "arn:" + cluster.Partition + ":iam::" + cluster.Account + ":role/" + names.nodeRole
	create := Cmd(awsCLI, awsArgs(cluster, "eks", "create-access-entry", "--cluster-name", cluster.Name,
		"--principal-arn", role, "--type", "EC2_LINUX")...)
	exists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-access-entry", "--cluster-name",
			cluster.Name, "--principal-arn", role)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return Step{
		Title:    "Let nodes with " + names.nodeRole + " join the cluster",
		Why:      "An EC2_LINUX access entry lets a node that runs with the node role register itself; it grants nothing else.",
		Commands: []string{create.String()},
		Check:    exists,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := exists(ctx)
			if err != nil || present {
				return err
			}
			return runner.Run(ctx, create)
		},
	}
}

func podIdentityAddonStep(runner *Runner, cluster EKSCluster, why string) Step {
	create := Cmd(awsCLI, awsArgs(cluster, "eks", "create-addon", "--cluster-name", cluster.Name, "--addon-name",
		podIdentityAddon)...)
	exists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-addon", "--cluster-name", cluster.Name,
			"--addon-name", podIdentityAddon)...))
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return Step{
		Title:    "Add the EKS Pod Identity Agent add-on",
		Why:      why,
		Commands: []string{create.String()},
		Check:    exists,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := exists(ctx)
			if err != nil || present {
				return err
			}
			return runner.Run(ctx, create)
		},
	}
}

func podIdentityAssociationStep(runner *Runner, cluster EKSCluster, names eksNames) Step {
	role := "arn:" + cluster.Partition + ":iam::" + cluster.Account + ":role/" + names.controllerRole
	create := Cmd(awsCLI, awsArgs(cluster, "eks", "create-pod-identity-association", "--cluster-name", cluster.Name,
		"--namespace", kube.KarpenterNamespace, "--service-account", kube.KarpenterServiceAccount, "--role-arn", role)...)
	exists := func(ctx context.Context) (bool, error) {
		var listed struct {
			Associations []struct{} `json:"associations"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "list-pod-identity-associations",
			"--cluster-name", cluster.Name, "--namespace", kube.KarpenterNamespace, "--service-account",
			kube.KarpenterServiceAccount)...), &listed)
		if err != nil {
			return false, err
		}
		return len(listed.Associations) > 0, nil
	}
	return Step{
		Title:    "Give Karpenter's service account the controller role",
		Why:      "Only " + kube.KarpenterNamespace + "/" + kube.KarpenterServiceAccount + " may assume " + names.controllerRole + ".",
		Commands: []string{create.String()},
		Check:    exists,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := exists(ctx)
			if err != nil || present {
				return err
			}
			return runner.Run(ctx, create)
		},
	}
}

func helmStep(runner *Runner, target *kube.Target, cluster EKSCluster, names eksNames) Step {
	release := helmRelease{target: target, chart: karpenterChart, version: DefaultKarpenterVersion,
		release: karpenterRelease, namespace: kube.KarpenterNamespace, values: []string{
			"settings.clusterName=" + cluster.Name,
			"settings.interruptionQueue=" + names.queue,
			staticCapacityValue,
			"controller.resources.requests.cpu=1",
			"controller.resources.requests.memory=1Gi",
			"controller.resources.limits.cpu=1",
			"controller.resources.limits.memory=1Gi",
		}, install: true, wait: true, timeout: helmTimeout}
	args := []string{"upgrade", "--install", karpenterRelease, karpenterChart, "--version", DefaultKarpenterVersion,
		"--namespace", kube.KarpenterNamespace}
	args = append(args, target.HelmArgs()...)
	for _, value := range release.values {
		args = append(args, "--set", value)
	}
	install := Cmd(helmCLI, append(args, "--wait", "--timeout", helmTimeout.String())...)
	return Step{
		Title: "Install Karpenter " + DefaultKarpenterVersion + " into " + kube.KarpenterNamespace,
		Why: "The chart installs Karpenter's resource definitions and its controller (two replicas), with" +
			" static capacity on: Coral's rebalancing creates its new nodes as NodePools with a fixed count. " +
			helmLibrary,
		Commands:   []string{install.String()},
		NotInPlace: StaticCapacityNotInPlace,
		Check: func(ctx context.Context) (bool, error) {
			return kube.KarpenterReadyWithStaticCapacity(ctx, target)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			note("helm is installing Karpenter and waiting for it to be ready")
			return runner.Helm(ctx, install, release)
		},
	}
}
