package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	azureCLI = "az"

	napModeAuto = "Auto"

	napModeFlag = "--node-provisioning-mode"

	napDefaultPoolsAuto = "Auto"

	napDefaultPoolsFlag = "--node-provisioning-default-pools"

	azureCredential = "your Azure CLI sign-in (az)"

	azureCLIUpgradeRemedy = "Run `az upgrade` (or `az extension add --name aks-preview`) and run this again. " +
		"Nothing has been changed."

	aksBaseImageFamily = "AzureLinux"

	disableAutoscalerFlag = "--disable-cluster-autoscaler"

	prepareCommand = "zoorikctl cluster prepare"
)

type napStaticPools struct {
	line   string
	served bool
}

type AzureCluster struct {
	Subscription  string
	ResourceGroup string
	Name          string
}

func (c AzureCluster) ID() string {
	return "/subscriptions/" + c.Subscription + "/resourceGroups/" + c.ResourceGroup +
		"/providers/Microsoft.ContainerService/managedClusters/" + c.Name
}

func (c AzureCluster) selector() []string {
	return []string{"--subscription", c.Subscription, "--resource-group", c.ResourceGroup, "--name", c.Name}
}

func (c AzureCluster) known() bool {
	return c.Name != ""
}

func (c AzureCluster) upgradesCommand() string {
	if !c.known() {
		return azureCLI + " aks get-upgrades --resource-group <resource group> --name <cluster> -o table"
	}
	return Cmd(azureCLI, append([]string{"aks", "get-upgrades"}, append(c.selector(), "-o", "table")...)...).String()
}

func (c AzureCluster) disableAutoscalerCommand(pool string) string {
	return Cmd(azureCLI, "aks", "nodepool", "update", "--subscription", c.Subscription, "--resource-group",
		c.ResourceGroup, "--cluster-name", c.Name, "--name", pool, disableAutoscalerFlag).String()
}

type aksCluster struct {
	Location          string `json:"location"`
	FQDN              string `json:"fqdn"`
	PrivateFQDN       string `json:"privateFqdn"`
	NodeResourceGroup string `json:"nodeResourceGroup"`
	OIDCIssuerProfile *struct {
		Enabled   bool   `json:"enabled"`
		IssuerURL string `json:"issuerUrl"`
	} `json:"oidcIssuerProfile"`
	SecurityProfile struct {
		WorkloadIdentity *struct {
			Enabled bool `json:"enabled"`
		} `json:"workloadIdentity"`
	} `json:"securityProfile"`
	NodeProvisioningProfile *struct {
		Mode             string `json:"mode"`
		DefaultNodePools string `json:"defaultNodePools"`
	} `json:"nodeProvisioningProfile"`
	NetworkProfile struct {
		NetworkPlugin     string `json:"networkPlugin"`
		NetworkPluginMode string `json:"networkPluginMode"`
		NetworkDataplane  string `json:"networkDataplane"`
	} `json:"networkProfile"`
	AgentPoolProfiles []struct {
		Name              string `json:"name"`
		OSType            string `json:"osType"`
		EnableAutoScaling bool   `json:"enableAutoScaling"`
	} `json:"agentPoolProfiles"`
}

func (c aksCluster) napMode() string {
	if c.NodeProvisioningProfile == nil || c.NodeProvisioningProfile.Mode == "" {
		return "Manual"
	}
	return c.NodeProvisioningProfile.Mode
}

func (c aksCluster) napDefaultPools() string {
	if c.NodeProvisioningProfile == nil || c.NodeProvisioningProfile.DefaultNodePools == "" {
		return "Auto"
	}
	return c.NodeProvisioningProfile.DefaultNodePools
}

func (c aksCluster) napOn() bool {
	return strings.EqualFold(c.napMode(), napModeAuto)
}

func (c aksCluster) defaultPoolsOn() bool {
	return strings.EqualFold(c.napDefaultPools(), napDefaultPoolsAuto)
}

func (c aksCluster) napReport() string {
	return "Azure reports node provisioning mode " + c.napMode() + ", default NodePools " + c.napDefaultPools()
}

func (c aksCluster) issuer() string {
	if c.OIDCIssuerProfile == nil || !c.OIDCIssuerProfile.Enabled {
		return ""
	}
	return c.OIDCIssuerProfile.IssuerURL
}

func (c aksCluster) workloadIdentity() bool {
	return c.SecurityProfile.WorkloadIdentity != nil && c.SecurityProfile.WorkloadIdentity.Enabled
}

func (c aksCluster) autoscaledPools() []string {
	var autoscaled []string
	for _, pool := range c.AgentPoolProfiles {
		if pool.EnableAutoScaling {
			autoscaled = append(autoscaled, pool.Name)
		}
	}
	sort.Strings(autoscaled)
	return autoscaled
}

func PlanAKS(ctx context.Context, runner *Runner, input Input) Plan {
	plan := Plan{Cloud: kube.CloudAzure, Provisioner: ProvisionerNAP, Credential: azureCredential}
	found := input.Found
	if found.NodePoolsServed && found.AKSClassesServed {
		plan.Present = true
		plan.Credential = kubeCredential(input.Target)
		plan.Found = describeFound(found)
		var cluster AzureCluster
		if found.HasCustomerNodePool() {
			plan.note("Node Auto Provisioning is on, so it is left as it is. Its NodePools and node classes are the " +
				"cluster's own and stay as they are; Zoorik adds only its own zoorik-* node class and pools when " +
				"live migration or rebalancing needs them.")
			cluster = readNAPCluster(ctx, runner, &plan, input)
		} else {
			resolved, ok := planNAPDefaultPools(ctx, runner, &plan, input)
			if !ok {
				return plan
			}
			cluster = resolved
		}
		if pools := readNAPStaticPools(ctx, input.Target, cluster); pools.served {
			plan.Found = append(plan.Found, pools.line)
		} else {
			plan.note(pools.line)
		}
		if !found.Has(kube.KindAKSNodeClass, kube.BaseNodeClassName) {
			plan.Steps = append(plan.Steps, aksBaseNodeClassStep(input.Target))
		}
		return plan
	}
	if found.NodePoolsServed && !found.AKSClassesServed {
		plan.Present = true
		plan.Found = describeFound(found)
		plan.note("This cluster serves karpenter.sh NodePools without AKSNodeClass, so something other than " +
			"Node Auto Provisioning provisions its nodes. It is left as it is.")
		return plan
	}

	cluster, shown, showCommand, ok := signInAKS(ctx, runner, &plan, input, aksProvisioner)
	if !ok {
		return plan
	}
	plan.Found = append(plan.Found, shown.napReport(),
		"Network: plugin "+text.OrText(shown.NetworkProfile.NetworkPlugin, "none")+
			modeSuffix(shown.NetworkProfile.NetworkPluginMode)+", dataplane "+
			text.OrText(shown.NetworkProfile.NetworkDataplane, "azure"))
	if shown.napOn() {
		plan.Present = true
		plan.note("Azure reports Node Auto Provisioning on, but this cluster does not serve its NodePool and " +
			"AKSNodeClass resources yet. It is left as it is; run prepare again once they appear.")
		return plan
	}
	if autoscaled := shown.autoscaledPools(); len(autoscaled) > 0 {
		plan.Present = true
		plan.Provisioner = ProvisionerClusterAutoscaler
		plan.AutoscaledPools = autoscaled
		pools := "node pool(s) " + strings.Join(autoscaled, ", ")
		plan.note("The cluster autoscaler runs on " + pools + " and adds nodes for Pending pods, so Node Auto " +
			"Provisioning is not turned on. The cluster keeps its autoscaler; Zoorik never changes it.")
		var prerequisites Plan
		checkAKSPrerequisites(&prerequisites, shown)
		plan.Notes = append(plan.Notes, prerequisites.Notes...)
		if len(prerequisites.Blockers) > 0 {
			plan.note("Coral's rebalancing needs Node Auto Provisioning, and this cluster cannot turn it on: " +
				prerequisites.Blockers[0].What + " So keep the cluster autoscaler on " + pools + ": rebalancing is " +
				"not available on this cluster, and node optimization and workload optimization work as before.")
			return plan
		}
		commands := make([]string, 0, len(autoscaled))
		for _, pool := range autoscaled {
			commands = append(commands, "`"+cluster.disableAutoscalerCommand(pool)+"`")
		}
		tooling := ""
		if missing := missingUpdateFlags(ctx, runner, napModeFlag, napDefaultPoolsFlag); len(missing) > 0 {
			tooling = " First update the Azure CLI on this machine (`az upgrade`, or `az extension add --name " +
				"aks-preview`): its `az aks update` has no " + strings.Join(missing, " or ") + " yet, so prepare " +
				"could not turn Node Auto Provisioning on afterwards."
		}
		plan.note("Coral's rebalancing needs Node Auto Provisioning, and AKS turns it on only when no node pool " +
			"uses the cluster autoscaler." + tooling + " Turn autoscaling off on " + pools + " (" +
			strings.Join(commands, "; ") + "), then run " + prepareCommand + " again; each pool keeps the nodes it " +
			"has, and nothing adds nodes for Pending pods until Node Auto Provisioning is on. Until then " +
			"rebalancing is not available on this cluster; node optimization and workload optimization work as " +
			"before.")
		return plan
	}

	checkAKSPrerequisites(&plan, shown)
	if len(plan.Blockers) > 0 {
		return plan
	}

	if missing := missingUpdateFlags(ctx, runner, napModeFlag, napDefaultPoolsFlag); len(missing) > 0 {
		plan.block("This Azure CLI cannot turn on Node Auto Provisioning: `az aks update` has no "+
			strings.Join(missing, " or ")+".", azureCLIUpgradeRemedy)
		return plan
	}

	enable := append([]string{"aks", "update"}, cluster.selector()...)
	enableCommand := Cmd(azureCLI, append(enable, napModeFlag, napModeAuto, napDefaultPoolsFlag, napDefaultPoolsAuto,
		"-o", "none")...)
	napOn := azureReports(runner, showCommand, func(shown aksCluster) bool {
		return shown.napOn() && shown.defaultPoolsOn()
	})
	plan.Steps = append(plan.Steps, Step{
		Title: "Turn on Node Auto Provisioning for " + cluster.Name,
		Why: "Node Auto Provisioning is AKS's managed Karpenter. Its default NodePools add nodes for Pending pods: " +
			"it is the cluster's own provisioner, and Zoorik still creates no machine. Existing node pools, their " +
			"nodes and their pods are not touched.",
		Commands: []string{enableCommand.String()},
		Check:    napOn,
		Apply: func(ctx context.Context, note func(string)) error {
			note("az aks update is running; Azure usually takes several minutes")
			if err := runner.Run(ctx, enableCommand); err != nil {
				return err
			}
			on, err := napOn(ctx)
			if err != nil {
				return err
			}
			if !on {
				return fmt.Errorf("az aks update finished, but Azure does not report node provisioning mode %s "+
					"with default NodePools %s; run prepare again", napModeAuto, napDefaultPoolsAuto)
			}
			note("checking whether Node Auto Provisioning serves static NodePools")
			note(awaitNAPStaticPools(ctx, input.Target, cluster).line)
			return nil
		},
	})
	plan.Steps = append(plan.Steps, aksBaseNodeClassStep(input.Target))
	return plan
}

func readNAPCluster(ctx context.Context, runner *Runner, plan *Plan, input Input) AzureCluster {
	var cluster AzureCluster
	if input.Resource != nil {
		if resolved, err := azureClusterOf(*input.Resource); err == nil {
			cluster = resolved
		}
	}
	if cluster.known() && runner.Available(azureCLI) {
		var shown aksCluster
		if runner.JSON(ctx, Cmd(azureCLI, append([]string{"aks", "show"},
			append(cluster.selector(), "-o", "json")...)...), &shown) == nil {
			plan.Cluster = cluster.ID()
			plan.Found = append(plan.Found, shown.napReport())
		}
	}
	return cluster
}

func planNAPDefaultPools(ctx context.Context, runner *Runner, plan *Plan, input Input) (AzureCluster, bool) {
	plan.Poolless = true
	plan.Credential = azureCredential
	plan.note("The cluster has no NodePool of its own (Zoorik's zoorik-* pools do not count), so nothing adds " +
		"nodes for Pending pods until it has one. Apart from its default NodePools, Node Auto Provisioning's " +
		"settings and the cluster's node classes stay as they are; Zoorik adds only its own zoorik-* node class " +
		"and pools when live migration or rebalancing needs them.")
	cluster, shown, showCommand, ok := signInAKS(ctx, runner, plan, input, aksDefaultPools)
	if !ok {
		return AzureCluster{}, false
	}
	plan.Found = append(plan.Found, shown.napReport())
	if missing := missingUpdateFlags(ctx, runner, napDefaultPoolsFlag); len(missing) > 0 {
		plan.block("This Azure CLI cannot turn on Node Auto Provisioning's default NodePools: `az aks update` has "+
			"no "+strings.Join(missing, " or ")+".", azureCLIUpgradeRemedy)
		return AzureCluster{}, false
	}
	if !shown.napOn() {
		plan.block("Azure reports Node Auto Provisioning off (node provisioning mode "+shown.napMode()+"), so it "+
			"has no default NodePools to turn on, though this cluster serves NodePool and AKSNodeClass resources "+
			"and has no NodePool of its own.", "Add a NodePool of your own on one of the cluster's AKSNodeClasses, "+
			"then run "+prepareCommand+" again. Nothing has been changed.")
		return AzureCluster{}, false
	}
	plan.Steps = append(plan.Steps, napDefaultPoolsStep(runner, cluster, showCommand))
	return cluster, true
}

func napDefaultPoolsStep(runner *Runner, cluster AzureCluster, showCommand Command) Step {
	update := Cmd(azureCLI, append(append([]string{"aks", "update"}, cluster.selector()...), napDefaultPoolsFlag,
		napDefaultPoolsAuto, "-o", "none")...)
	on := azureReports(runner, showCommand, aksCluster.defaultPoolsOn)
	return Step{
		Title: "Turn on Node Auto Provisioning's default NodePools for " + cluster.Name,
		Why: "The cluster has no NodePool of its own (Zoorik's zoorik-* pools do not count), so nothing adds nodes " +
			"for Pending pods. Node Auto Provisioning's default NodePools add them, as on a cluster where zoorikctl " +
			"turns Node Auto Provisioning on: they are the cluster's own provisioner, and Zoorik still creates no " +
			"machine. Node Auto Provisioning's other settings, its node classes, the existing node pools, their " +
			"nodes and their pods are not touched.",
		Commands: []string{update.String()},
		Check:    on,
		Apply: func(ctx context.Context, note func(string)) error {
			note("az aks update is running; Azure usually takes several minutes")
			if err := runner.Run(ctx, update); err != nil {
				return err
			}
			done, err := on(ctx)
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("az aks update finished, but Azure does not report default NodePools %s; run "+
					"prepare again", napDefaultPoolsAuto)
			}
			return nil
		},
	}
}

func azureReports(runner *Runner, showCommand Command, state func(aksCluster) bool) func(context.Context) (bool,
	error) {
	return func(ctx context.Context) (bool, error) {
		var shown aksCluster
		if err := runner.JSON(ctx, showCommand, &shown); err != nil {
			return false, err
		}
		return state(shown), nil
	}
}

func missingUpdateFlags(ctx context.Context, runner *Runner, flags ...string) []string {
	help, _ := runner.Output(ctx, Cmd(azureCLI, "aks", "update", "--help"))
	var missing []string
	for _, flag := range flags {
		if !strings.Contains(string(help), flag) {
			missing = append(missing, flag)
		}
	}
	return missing
}

func aksBaseNodeClassStep(target *kube.Target) Step {
	return karpenterObjectStep(target, kube.AKSNodeClassGroup, kube.KindAKSNodeClass, kube.BaseNodeClassName,
		map[string]any{"imageFamily": aksBaseImageFamily},
		"Zoorik's own zoorik-* pools clone this node class, and rebalancing creates its new nodes with it when "+
			"the cluster has no NodePool of its own to copy. It uses Azure Linux, which ships containerd 2 and a "+
			"6.6 kernel. It is not a pool and creates no node.")
}

func readNAPStaticPools(ctx context.Context, target *kube.Target, cluster AzureCluster) napStaticPools {
	served, err := kube.NodePoolServesReplicas(ctx, target)
	switch {
	case err != nil:
		return staticPoolsUnread(err, "Check that the context's credential may run `"+
			target.Kubectl("get --raw "+kube.KarpenterOpenAPIPath)+"`, then run "+prepareCommand+" again.")
	case served:
		return napStaticPools{line: "Node Auto Provisioning serves static NodePools (spec.replicas), which " +
			"rebalancing creates its new nodes with", served: true}
	}
	return napStaticPools{line: "Node Auto Provisioning on this cluster does not serve static NodePools " +
		"(spec.replicas on NodePool), so Coral's rebalancing cannot create its new nodes here. zoorikctl changes " +
		"nothing for it: upgrade the cluster to an AKS version whose Node Auto Provisioning serves spec.replicas " +
		"(`" + cluster.upgradesCommand() + "` lists the versions it can move to), then run " + prepareCommand +
		" again. Node optimization and workload optimization work as before."}
}

func awaitNAPStaticPools(ctx context.Context, target *kube.Target, cluster AzureCluster) napStaticPools {
	served, err := waitFor(ctx, func() (bool, error) {
		if err := target.Rediscover(); err != nil {
			return false, nil
		}
		found, err := kube.DetectAutoprovisioner(ctx, target)
		if err != nil {
			return false, err
		}
		return found.NodePoolsServed, nil
	})
	switch {
	case err != nil:
		return staticPoolsUnread(err, "Run "+prepareCommand+" again; it reads this again.")
	case !served:
		return staticPoolsUnread(fmt.Errorf("the cluster did not serve %s within %s", kube.KindNodePool, crdWait),
			"Run "+prepareCommand+" again once it does; it reads this again.")
	}
	return readNAPStaticPools(ctx, target, cluster)
}

func staticPoolsUnread(err error, remedy string) napStaticPools {
	cause := err
	if inner := errors.Unwrap(err); inner != nil {
		cause = inner
	}
	return napStaticPools{line: "Whether Node Auto Provisioning serves static NodePools (spec.replicas), which " +
		"rebalancing creates its new nodes with, could not be read (" + text.OneLine(cause.Error()) + "). " + remedy}
}

func checkAKSPrerequisites(plan *Plan, shown aksCluster) {
	plugin := strings.ToLower(shown.NetworkProfile.NetworkPlugin)
	switch plugin {
	case "azure":
	case "kubenet":
		plan.block("The cluster uses kubenet networking, which Node Auto Provisioning does not support.",
			"NAP needs Azure CNI (overlay or pod subnet). Moving a cluster off kubenet is an Azure migration "+
				"(`az aks update --network-plugin azure --network-plugin-mode overlay`) that the cluster's owner "+
				"plans and runs; zoorikctl never does it. Nothing has been changed.")
	default:
		plan.note("The cluster's network plugin is " + text.OrText(plugin, "none (bring your own CNI)") + ". Node " +
			"Auto Provisioning is documented for Azure CNI; if `az aks update` refuses, that is why.")
	}
	var windows []string
	for _, pool := range shown.AgentPoolProfiles {
		if strings.EqualFold(pool.OSType, "Windows") {
			windows = append(windows, pool.Name)
		}
	}
	if len(windows) > 0 {
		plan.note("Node pool(s) " + strings.Join(windows, ", ") + " run Windows. Node Auto Provisioning " +
			"provisions only Linux nodes, and live migration never runs on Windows.")
	}
}

func resolveAzureCluster(ctx context.Context, runner *Runner, input Input) (AzureCluster, error) {
	if input.Resource != nil {
		return azureClusterOf(*input.Resource)
	}
	subscription, nodeGroup, err := kube.AzureNodeResourceGroup(ctx, input.Target)
	if err != nil {
		return AzureCluster{}, fmt.Errorf("the nodes could not be listed to find the AKS cluster: %w", err)
	}
	if subscription == "" || nodeGroup == "" {
		return AzureCluster{}, fmt.Errorf("no node names its Azure subscription and resource group, so the AKS " +
			"cluster cannot be found")
	}
	var clusters []struct {
		Name              string `json:"name"`
		ResourceGroup     string `json:"resourceGroup"`
		NodeResourceGroup string `json:"nodeResourceGroup"`
	}
	if err := runner.JSON(ctx, Cmd(azureCLI, "aks", "list", "--subscription", subscription, "-o", "json"),
		&clusters); err != nil {
		return AzureCluster{}, fmt.Errorf("the AKS clusters of subscription %s could not be listed: %w",
			subscription, err)
	}
	var matched []AzureCluster
	for _, cluster := range clusters {
		if strings.EqualFold(cluster.NodeResourceGroup, nodeGroup) {
			matched = append(matched, AzureCluster{Subscription: subscription, ResourceGroup: cluster.ResourceGroup,
				Name: cluster.Name})
		}
	}
	if len(matched) != 1 {
		return AzureCluster{}, fmt.Errorf("%d AKS clusters in subscription %s use node resource group %s, so "+
			"this cluster cannot be told apart", len(matched), subscription, nodeGroup)
	}
	return matched[0], nil
}

func azureClusterOf(resource kube.ClusterResource) (AzureCluster, error) {
	if resource.Cloud != kube.CloudAzure {
		return AzureCluster{}, fmt.Errorf("--cluster-resource-id names an EKS cluster, and this cluster runs on Azure")
	}
	parts := strings.Split(strings.Trim(resource.Raw, "/"), "/")
	if len(parts) != 8 {
		return AzureCluster{}, fmt.Errorf("--cluster-resource-id %q is not an AKS cluster's ARM id", resource.Raw)
	}
	return AzureCluster{Subscription: resource.Account, ResourceGroup: parts[3], Name: parts[7]}, nil
}

func karpenterObjectStep(target *kube.Target, group, kind, name string, spec map[string]any, why string) Step {
	title := "Add the node class " + name + " (" + kind + ")"
	if kind == kube.KindNodePool {
		title = "Add the NodePool " + name
	}
	manifest := kube.KarpenterObjectManifest(kube.PreferredVersion(target, group, kind), kind, name, spec)
	return Step{
		Title:    title,
		Why:      why,
		Commands: []string{target.Kubectl("apply -f - <<'EOF'"), manifest, "EOF"},
		Check: func(ctx context.Context) (bool, error) {
			if err := target.Rediscover(); err != nil {
				return false, err
			}
			found, err := kube.DetectAutoprovisioner(ctx, target)
			if err != nil {
				return false, err
			}
			return found.Has(kind, name), nil
		},
		Apply: func(ctx context.Context, note func(string)) error {
			note("waiting for the cluster to serve " + kind)
			done, err := waitFor(ctx, func() (bool, error) {
				if err := target.Rediscover(); err != nil {
					return false, nil
				}
				found, err := kube.DetectAutoprovisioner(ctx, target)
				if err != nil {
					return false, err
				}
				gvr, served := found.Resource(kind)
				if !served {
					return false, nil
				}
				if found.Has(kind, name) {
					return true, nil
				}
				err = kube.CreateKarpenterObject(ctx, target, gvr, kind, name, spec)
				return err == nil, err
			})
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("the cluster did not serve %s within %s; run prepare again", kind, crdWait)
			}
			return nil
		},
	}
}

func describeFound(found kube.Autoprovisioner) []string {
	var lines []string
	if found.AutoModeServed {
		lines = append(lines, "EKS Auto Mode node classes are served")
	}
	lines = append(lines, fmt.Sprintf("%d NodePool(s) served as %s", len(found.Pools),
		found.NodePools.GroupResource().String()+"/"+found.NodePools.Version))
	for _, pool := range found.Pools {
		line := "NodePool " + pool.Name
		if pool.NodeClass != "" {
			line += ", node class " + pool.NodeClass
		}
		if pool.Weight != "" {
			line += ", weight " + pool.Weight
		}
		if pool.LimitsCPU != "" {
			line += ", cpu limit " + pool.LimitsCPU
		}
		if pool.ZoorikOwned {
			line += " (Zoorik's own)"
		}
		lines = append(lines, line)
	}
	for _, class := range found.Classes {
		line := class.Kind + " " + class.Name
		if class.ImageFamily != "" {
			line += ", image family " + class.ImageFamily
		}
		if class.ZoorikOwned {
			line += " (Zoorik's own)"
		}
		lines = append(lines, line)
	}
	for _, controller := range found.Controllers {
		lines = append(lines, fmt.Sprintf("Controller %s/%s: %d of %d ready (%s)", controller.Namespace,
			controller.Name, controller.Ready, controller.Desired, text.OrText(controller.Image, "image unknown")))
	}
	return lines
}

func modeSuffix(mode string) string {
	if mode == "" {
		return ""
	}
	return " (" + mode + ")"
}
