package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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

func (c AzureCluster) autoscalerOffCommand(pool string) Command {
	return Cmd(azureCLI, "aks", "nodepool", "update", "--subscription", c.Subscription, "--resource-group",
		c.ResourceGroup, "--cluster-name", c.Name, "--name", pool, disableAutoscalerFlag, "-o", "none")
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
		NetworkPolicy     string `json:"networkPolicy"`
	} `json:"networkProfile"`
	ServicePrincipalProfile *struct {
		ClientID string `json:"clientId"`
	} `json:"servicePrincipalProfile"`
	AgentPoolProfiles []aksPoolProfile `json:"agentPoolProfiles"`
}

type aksPoolProfile struct {
	Name              string `json:"name"`
	OSType            string `json:"osType"`
	EnableAutoScaling bool   `json:"enableAutoScaling"`
	Count             *int   `json:"count"`
	MinCount          *int   `json:"minCount"`
	MaxCount          *int   `json:"maxCount"`
}

func (p aksPoolProfile) windows() bool {
	return strings.EqualFold(p.OSType, "Windows")
}

func (p aksPoolProfile) nodes() string {
	if p.Count == nil {
		return ""
	}
	return strconv.Itoa(*p.Count) + " node(s)"
}

func (p aksPoolProfile) autoscalerRange() string {
	if p.MinCount == nil || p.MaxCount == nil {
		return ""
	}
	return strconv.Itoa(*p.MinCount) + " to " + strconv.Itoa(*p.MaxCount)
}

func (p aksPoolProfile) autoscalerOnCommand(cluster AzureCluster) string {
	if p.MinCount == nil || p.MaxCount == nil {
		return ""
	}
	return Cmd(azureCLI, "aks", "nodepool", "update", "--subscription", cluster.Subscription, "--resource-group",
		cluster.ResourceGroup, "--cluster-name", cluster.Name, "--name", p.Name, "--enable-cluster-autoscaler",
		"--min-count", strconv.Itoa(*p.MinCount), "--max-count", strconv.Itoa(*p.MaxCount), "-o", "none").String()
}

func (p aksPoolProfile) found() string {
	line := "Node pool " + p.Name + ": cluster autoscaler on"
	if span := p.autoscalerRange(); span != "" {
		line += ", " + span + " nodes"
	}
	if p.Count != nil {
		line += ", " + strconv.Itoa(*p.Count) + " now"
	}
	return line
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

func (c aksCluster) servicePrincipal() bool {
	return c.ServicePrincipalProfile != nil && c.ServicePrincipalProfile.ClientID != "" &&
		!strings.EqualFold(c.ServicePrincipalProfile.ClientID, "msi")
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
	for _, pool := range c.autoscaledProfiles() {
		autoscaled = append(autoscaled, pool.Name)
	}
	return autoscaled
}

func (c aksCluster) autoscaledProfiles() []aksPoolProfile {
	var autoscaled []aksPoolProfile
	for _, pool := range c.AgentPoolProfiles {
		if pool.EnableAutoScaling {
			autoscaled = append(autoscaled, pool)
		}
	}
	sort.Slice(autoscaled, func(i, j int) bool { return autoscaled[i].Name < autoscaled[j].Name })
	return autoscaled
}

func (c aksCluster) poolAutoscaled(name string) bool {
	for _, pool := range c.AgentPoolProfiles {
		if pool.Name == name {
			return pool.EnableAutoScaling
		}
	}
	return false
}

func napCannotReplace(shown aksCluster, autoscaled []aksPoolProfile) string {
	switch plugin := strings.ToLower(shown.NetworkProfile.NetworkPlugin); plugin {
	case "azure":
	case "kubenet":
		return "The cluster uses kubenet networking, which Node Auto Provisioning does not support."
	default:
		return "Its network plugin is " + text.OrText(plugin, "none (bring your own CNI)") + ", and Node Auto " +
			"Provisioning runs only on Azure CNI."
	}
	if strings.EqualFold(shown.NetworkProfile.NetworkPolicy, "calico") {
		return "The cluster uses Calico network policy, which Node Auto Provisioning does not support."
	}
	if shown.servicePrincipal() {
		return "The cluster signs in to Azure with a service principal, and Node Auto Provisioning needs a managed " +
			"identity."
	}
	var windows []string
	for _, pool := range autoscaled {
		if pool.windows() {
			windows = append(windows, pool.Name)
		}
	}
	if len(windows) > 0 {
		return "Node pool(s) " + strings.Join(windows, ", ") + " run Windows, and Node Auto Provisioning adds only " +
			"Linux nodes."
	}
	return ""
}

func autoscalerOffStep(runner *Runner, cluster AzureCluster, showCommand Command, pool aksPoolProfile,
	first bool) Step {
	off := cluster.autoscalerOffCommand(pool.Name)
	why := "The pool keeps the nodes it has, at that fixed count"
	if nodes := pool.nodes(); nodes != "" {
		why = "The pool keeps its " + nodes + " at that fixed count"
	}
	if span := pool.autoscalerRange(); span != "" {
		why += " (its autoscaler range was " + span + ")"
	}
	why += "; nothing adds nodes to it until Node Auto Provisioning is on."
	if first {
		why = "AKS turns Node Auto Provisioning on only when no node pool uses the cluster autoscaler, and from " +
			"then on refuses the autoscaler on any pool while Node Auto Provisioning is on. " + why
	}
	return Step{
		Title:    "Turn off the cluster autoscaler on node pool " + pool.Name,
		Why:      why,
		Commands: []string{off.String()},
		Check: azureReports(runner, showCommand, func(shown aksCluster) bool {
			return !shown.poolAutoscaled(pool.Name)
		}),
		Apply: func(ctx context.Context, note func(string)) error {
			note("az aks nodepool update is running; Azure usually takes a minute or two")
			return runner.Run(ctx, off)
		},
	}
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
		plan.Steps = append(plan.Steps, staleStatusSteps(ctx, runner, input, nil)...)
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
		plan.Steps = append(plan.Steps, staleStatusSteps(ctx, runner, input, &shown)...)
		return plan
	}
	var switched []aksPoolProfile
	if autoscaled := shown.autoscaledProfiles(); len(autoscaled) > 0 {
		if reason := napCannotReplace(shown, autoscaled); reason != "" {
			plan.Present = true
			plan.Provisioner = ProvisionerClusterAutoscaler
			plan.AutoscaledPools = shown.autoscaledPools()
			plan.note("The cluster autoscaler stays on node pool(s) " + strings.Join(plan.AutoscaledPools, ", ") +
				". " + reason + " Node optimization and workload optimization work as before.")
			return plan
		}
		switched = autoscaled
		plan.Switch = true
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

	var putBack []string
	for i, pool := range switched {
		plan.Found = append(plan.Found, pool.found())
		plan.Steps = append(plan.Steps, autoscalerOffStep(runner, cluster, showCommand, pool, i == 0))
		if command := pool.autoscalerOnCommand(cluster); command != "" {
			putBack = append(putBack, "`"+command+"`")
		}
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
				if len(putBack) > 0 {
					return fmt.Errorf("%w. To put the cluster autoscaler back on the node pools turned off above, "+
						"run %s", err, strings.Join(putBack, " and "))
				}
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
	if len(switched) > 0 && found.ClusterAutoscaler {
		plan.Steps = append(plan.Steps, autoscalerStatusStep(input.Target, found.ClusterAutoscalerNamespace))
	} else {
		plan.Steps = append(plan.Steps, staleStatusSteps(ctx, runner, input, &shown)...)
	}
	plan.Steps = append(plan.Steps, aksBaseNodeClassStep(input.Target))
	return plan
}

// staleStatusSteps deletes a cluster-autoscaler-status that still lists node groups while no node pool uses the
// cluster autoscaler and no Deployment runs it, as after a switch that stopped once the pools were turned off.
// shown is read from Azure when nil.
func staleStatusSteps(ctx context.Context, runner *Runner, input Input, shown *aksCluster) []Step {
	found := input.Found
	if !found.ClusterAutoscaler {
		return nil
	}
	if groups, err := kube.AutoscaledNodeGroups(ctx, input.Target); err != nil || len(groups) == 0 {
		return nil
	}
	deployments, err := kube.ClusterAutoscalerDeployments(ctx, input.Target)
	if err != nil {
		return nil
	}
	for _, deployment := range deployments {
		if deployment.Replicas > 0 {
			return nil
		}
	}
	if shown == nil {
		if !runner.Available(azureCLI) {
			return nil
		}
		cluster, err := resolveAzureCluster(ctx, runner, input)
		if err != nil {
			return nil
		}
		var read aksCluster
		if runner.JSON(ctx, Cmd(azureCLI, append([]string{"aks", "show"}, append(cluster.selector(), "-o",
			"json")...)...), &read) != nil {
			return nil
		}
		shown = &read
	}
	if len(shown.autoscaledProfiles()) > 0 {
		return nil
	}
	return []Step{autoscalerStatusStep(input.Target, found.ClusterAutoscalerNamespace)}
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
		if pool.windows() {
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
