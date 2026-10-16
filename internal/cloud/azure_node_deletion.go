package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	enableOIDCIssuerFlag = "--enable-oidc-issuer"

	enableWorkloadIdentityFlag = "--enable-workload-identity"

	managedIdentityPrincipal = "ServicePrincipal"

	nodeRemovalRoleDescription = "Lets the zoorik-agent of one AKS cluster read its agent pools, delete " +
		"specific machines it has already drained, and lower an agent pool's minimum count. agentPools/write is " +
		"the narrowest action for that and also allows creating or changing agent pools; the agent never creates " +
		"a pool or a machine and never raises a count, minimum or maximum."
)

var assignmentRetryMarkers = []string{"PrincipalNotFound", "RoleDefinitionDoesNotExist", "doesn't exist"}

var nodeRemovalActions = []string{
	"Microsoft.ContainerService/managedClusters/read",
	"Microsoft.ContainerService/managedClusters/agentPools/read",
	"Microsoft.ContainerService/managedClusters/agentPools/machines/read",
	"Microsoft.ContainerService/managedClusters/agentPools/deleteMachines/action",
	"Microsoft.Authorization/permissions/read",
	"Microsoft.ContainerService/managedClusters/agentPools/write",
}

type azureIdentity struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	ResourceGroup string            `json:"resourceGroup"`
	ClientID      string            `json:"clientId"`
	PrincipalID   string            `json:"principalId"`
	Tags          map[string]string `json:"tags"`
}

type federatedCredential struct {
	Name    string `json:"name"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type roleAssignment struct {
	ID            string `json:"id"`
	PrincipalID   string `json:"principalId"`
	PrincipalType string `json:"principalType"`
	Scope         string `json:"scope"`
}

type rolePermission struct {
	Actions        []string `json:"actions"`
	NotActions     []string `json:"notActions"`
	DataActions    []string `json:"dataActions"`
	NotDataActions []string `json:"notDataActions"`
}

type roleDefinition struct {
	ID               string           `json:"id"`
	AssignableScopes []string         `json:"assignableScopes"`
	Permissions      []rolePermission `json:"permissions"`
}

func (d roleDefinition) removesNodesOnly() bool {
	var actions, others []string
	for _, permission := range d.Permissions {
		actions = append(actions, permission.Actions...)
		others = append(others, permission.NotActions...)
		others = append(others, permission.DataActions...)
		others = append(others, permission.NotDataActions...)
	}
	return len(others) == 0 && sameSet(actions, nodeRemovalActions)
}

func (d roleDefinition) allows() string {
	var actions, notActions, dataActions, notDataActions []string
	for _, permission := range d.Permissions {
		actions = append(actions, permission.Actions...)
		notActions = append(notActions, permission.NotActions...)
		dataActions = append(dataActions, permission.DataActions...)
		notDataActions = append(notDataActions, permission.NotDataActions...)
	}
	parts := []string{"actions [" + strings.Join(actions, ", ") + "]"}
	for _, extra := range []struct {
		label  string
		listed []string
	}{{"not-actions", notActions}, {"data actions", dataActions}, {"not-data-actions", notDataActions}} {
		if len(extra.listed) > 0 {
			parts = append(parts, extra.label+" ["+strings.Join(extra.listed, ", ")+"]")
		}
	}
	return strings.Join(parts, ", ")
}

func nodeRemovalRoleDefinition(subscription string) string {
	return compactJSON(map[string]any{
		"Name":             nodeRemovalRole,
		"IsCustom":         true,
		"Description":      nodeRemovalRoleDescription,
		"Actions":          nodeRemovalActions,
		"NotActions":       []string{},
		"DataActions":      []string{},
		"NotDataActions":   []string{},
		"AssignableScopes": []string{"/subscriptions/" + subscription},
	})
}

func nodeRemovalRoleExtension(existing roleDefinition, subscription string) string {
	scopes := append(append([]string{}, existing.AssignableScopes...), "/subscriptions/"+subscription)
	return compactJSON(map[string]any{
		"Id":               existing.ID,
		"Name":             nodeRemovalRole,
		"RoleName":         nodeRemovalRole,
		"IsCustom":         true,
		"Description":      nodeRemovalRoleDescription,
		"Actions":          nodeRemovalActions,
		"NotActions":       []string{},
		"DataActions":      []string{},
		"NotDataActions":   []string{},
		"AssignableScopes": scopes,
	})
}

func PlanNodeDeletionAKS(ctx context.Context, runner *Runner, input Input) NodeDeletion {
	plan := Plan{Cloud: kube.CloudAzure, Provisioner: NodeDeletionTitle, Credential: "your Azure CLI sign-in (az)"}
	cluster, shown, showCommand, ok := signInAKS(ctx, runner, &plan, input, aksNodeDeletion)
	if !ok {
		return NodeDeletion{Plan: plan}
	}
	subscription := cluster.Subscription
	issuer := shown.issuer()
	plan.Found = append(plan.Found, "Workload identity "+onOff(shown.workloadIdentity())+", OIDC issuer "+
		text.OrText(issuer, "off"))
	trusting := issuer != "" && shown.workloadIdentity()
	if !trusting {
		help, _ := runner.Output(ctx, Cmd(azureCLI, "aks", "update", "--help"))
		if !strings.Contains(string(help), enableWorkloadIdentityFlag) {
			plan.block("This Azure CLI cannot turn on workload identity: `az aks update` has no "+
				enableWorkloadIdentityFlag+".", "Run `az upgrade` and run this again. Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
	}

	var identities []azureIdentity
	if err := runner.JSON(ctx, Cmd(azureCLI, "identity", "list", "--subscription", subscription, "-o", "json"),
		&identities); err != nil {
		plan.block("Azure would not list the managed identities of subscription "+subscription+": "+err.Error(),
			"Check that your sign-in may read them, and run this again. Nothing has been changed.")
		return NodeDeletion{Plan: plan}
	}
	name := boundedName(agentIdentityPrefix, cluster.Name, azureIdentityNameLimit)
	chosen := azureIdentity{Name: name, ResourceGroup: cluster.ResourceGroup}
	own, ownFound := findIdentity(identities, name, cluster.ResourceGroup)
	reused := false
	if issuer != "" {
		trusted, found, passed := trustedIdentity(ctx, runner, cluster, issuer, identities, input.Occupant)
		plan.Found = append(plan.Found, passed...)
		if found {
			chosen, reused = trusted, true
			plan.Found = append(plan.Found, "Managed identity "+trusted.Name+" in "+trusted.ResourceGroup+
				" already trusts "+kube.AgentNamespace+"/"+kube.AgentServiceAccount+" on this cluster, and nothing else")
		}
	}
	if !reused && ownFound {
		if !ownIdentityUsable(ctx, runner, &plan, cluster, own, issuer) {
			return NodeDeletion{Plan: plan}
		}
		chosen = own
	}

	definitions, err := nodeRemovalDefinitions(ctx, runner, subscription)
	if err != nil {
		plan.block("Azure would not list the custom roles of subscription "+subscription+": "+err.Error(),
			"Check that your sign-in may read role definitions there, and run this again. Nothing has been changed.")
		return NodeDeletion{Plan: plan}
	}
	var extended *roleDefinition
	if len(definitions) > 0 {
		if blockForeignRole(&plan, definitions[0]) {
			return NodeDeletion{Plan: plan}
		}
	} else if existing, found := nodeRemovalDefinedElsewhere(ctx, runner, subscription); found {
		if blockForeignRole(&plan, existing) {
			return NodeDeletion{Plan: plan}
		}
		extended = &existing
		plan.Found = append(plan.Found, "The role \""+nodeRemovalRole+"\" is already defined in this Azure tenant, "+
			"assignable in "+strings.Join(existing.AssignableScopes, ", "))
	}
	var legacy []string
	if chosen.PrincipalID != "" {
		legacy, err = roleAssignmentsOn(ctx, runner, cluster, legacyNodeRemovalRole, chosen.PrincipalID)
		if err != nil {
			plan.block("Azure would not list the role assignments on "+cluster.ID()+": "+err.Error(),
				"Check that your sign-in may read them, and run this again. Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
	}
	if len(legacy) > 0 {
		plan.Found = append(plan.Found, chosen.Name+" holds \""+legacyNodeRemovalRole+"\" on "+cluster.Name+
			", which only deletes machines; \""+nodeRemovalRole+"\" replaces it")
	}

	plan.note("Zoorik never creates a machine itself. The identity below may read this cluster, delete a named " +
		"machine of one of its agent pools, and create or change its agent pools, sizes and VM sizes included, " +
		"because Azure has no narrower permission than agentPools/write. The agent only ever lowers an autoscaled " +
		"pool's minimum count, never below 1 on a " +
		"System pool or 0 on a User pool, and only when that minimum alone keeps a node it has cordoned and " +
		"emptied. It never raises a count, minimum or maximum, and disconnect puts every minimum it lowered back.")

	identityShow := Cmd(azureCLI, "identity", "show", "--subscription", subscription, "--resource-group",
		chosen.ResourceGroup, "--name", chosen.Name, "-o", "json")
	if !trusting {
		plan.Steps = append(plan.Steps, workloadIdentityStep(runner, cluster, shown, showCommand))
	}
	if !reused {
		plan.Steps = append(plan.Steps,
			identityStep(runner, cluster, chosen, shown.Location, identityShow),
			federatedCredentialStep(runner, cluster, chosen, issuer, showCommand))
	}
	upgrade := []Step{roleDefinitionStep(runner, subscription, extended),
		roleAssignmentStep(runner, cluster, chosen, identityShow)}
	var earlier string
	if len(legacy) > 0 {
		upgrade = append(upgrade, legacyAssignmentRemovalStep(runner, cluster, chosen, identityShow, legacy))
		for i := range upgrade {
			upgrade[i].Upgrade = true
		}
		earlier = "\"" + legacyNodeRemovalRole + "\" held by " + chosen.Name
	}
	plan.Steps = append(plan.Steps, upgrade...)

	return NodeDeletion{Plan: plan, Earlier: earlier, Bind: func(ctx context.Context) (Binding, error) {
		var bound azureIdentity
		if err := runner.JSON(ctx, identityShow, &bound); err != nil {
			return Binding{}, err
		}
		if bound.ClientID == "" {
			return Binding{}, fmt.Errorf("Azure reports no client id for the managed identity %s", chosen.Name)
		}
		return Binding{Identity: bound.ClientID, Via: ViaAzureWorkloadIdentity, Annotate: true}, nil
	}}
}

func PlanNodeDeletionCleanupAKS(ctx context.Context, runner *Runner, input Input) Plan {
	plan := Plan{Cloud: kube.CloudAzure, Provisioner: NodeDeletionTitle, Credential: "your Azure CLI sign-in (az)"}
	cluster, _, _, ok := signInAKS(ctx, runner, &plan, input, aksRemoval)
	if !ok {
		return plan
	}
	subscription, group := cluster.Subscription, cluster.ResourceGroup
	planMinimumRestoreAKS(ctx, runner, &plan, cluster)

	for _, role := range []string{nodeRemovalRole, legacyNodeRemovalRole} {
		ids, err := roleAssignmentsOn(ctx, runner, cluster, role, "")
		if err != nil {
			plan.block("Azure would not list the role assignments on "+cluster.ID()+": "+err.Error(),
				"Check that your sign-in may read them, and run this disconnect again. Nothing has been changed.")
			return plan
		}
		if len(ids) > 0 {
			plan.Found = append(plan.Found, fmt.Sprintf("%d assignment(s) of \"%s\" on %s", len(ids), role,
				cluster.Name))
		} else if role == legacyNodeRemovalRole {
			continue
		}
		plan.Steps = append(plan.Steps, assignmentRemovalStep(runner, cluster, role, ids))
	}

	name := boundedName(agentIdentityPrefix, cluster.Name, azureIdentityNameLimit)
	identityShow := Cmd(azureCLI, "identity", "show", "--subscription", subscription, "--resource-group", group,
		"--name", name, "-o", "json")
	var found azureIdentity
	err := runner.JSON(ctx, identityShow, &found)
	switch {
	case NotFound(err):
		plan.Found = append(plan.Found, "No managed identity "+name+" in "+group)
	case err != nil:
		plan.block("Azure would not read the managed identity "+name+": "+err.Error(),
			"Check that your sign-in may read managed identities in "+group+", and run this disconnect again. "+
				"Nothing has been changed.")
		return plan
	case !taggedFor(tagValue(found.Tags, clusterTag), cluster.ID()):
		plan.note("Managed identity " + name + " is not tagged " + clusterTag + "=" + clusterTagValue(cluster.ID()) +
			", so zoorikctl did not create it for this cluster; it is left alone.")
	default:
		plan.Found = append(plan.Found, "Managed identity "+name+" in "+group+", tagged "+clusterTag+"="+
			clusterTagValue(cluster.ID()))
		plan.Steps = append(plan.Steps, ownIdentityRemoval(runner, cluster, name, identityShow)...)
	}
	plan.note("The roles \"" + nodeRemovalRole + "\" and \"" + legacyNodeRemovalRole + "\", where defined, stay " +
		"defined in subscription " + subscription + " for other clusters, and workload identity and the OIDC issuer " +
		"stay on.")
	return plan
}

func assignmentRemovalStep(runner *Runner, cluster AzureCluster, role string, idsAtPlan []string) Step {
	assigned := func(ctx context.Context) ([]string, error) {
		return roleAssignmentsOn(ctx, runner, cluster, role, "")
	}
	shown := idsAtPlan
	if len(shown) == 0 {
		shown = []string{"<assignment ids>"}
	}
	allows := "delete machines of this cluster's agent pools and change those pools"
	if role == legacyNodeRemovalRole {
		allows = "delete machines of this cluster's agent pools"
	}
	return Step{
		Title: "Remove the \"" + role + "\" assignments on " + cluster.Name,
		Why: "Each lets an identity " + allows + "; they were made for the agent. An identity zoorikctl reused " +
			"keeps everything else it holds.",
		Commands: []string{unassignCommand(cluster.Subscription, shown).String()},
		Check: func(ctx context.Context) (bool, error) {
			ids, err := assigned(ctx)
			return err == nil && len(ids) == 0, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			ids, err := assigned(ctx)
			if err != nil || len(ids) == 0 {
				return err
			}
			return runner.Run(ctx, unassignCommand(cluster.Subscription, ids))
		},
	}
}

func unassignCommand(subscription string, ids []string) Command {
	args := append([]string{"role", "assignment", "delete", "--subscription", subscription, "--ids"}, ids...)
	return Cmd(azureCLI, append(args, "-o", "none")...)
}

func ownIdentityRemoval(runner *Runner, cluster AzureCluster, name string, identityShow Command) []Step {
	subscription, group := cluster.Subscription, cluster.ResourceGroup
	credentialDelete := Cmd(azureCLI, "identity", "federated-credential", "delete", "--subscription", subscription,
		"--resource-group", group, "--identity-name", name, "--name", agentCredentialName, "--yes")
	credentialGone := func(ctx context.Context) (bool, error) {
		credentials, err := federatedCredentials(ctx, runner, subscription, group, name)
		if NotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		for _, credential := range credentials {
			if credential.Name == agentCredentialName {
				return false, nil
			}
		}
		return true, nil
	}
	identityDelete := Cmd(azureCLI, "identity", "delete", "--subscription", subscription, "--resource-group", group,
		"--name", name)
	identityGone := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, identityShow)
		if NotFound(err) {
			return true, nil
		}
		return false, err
	}
	return []Step{
		{
			Title:    "Remove the federated credential " + agentCredentialName + " from " + name,
			Why:      "It let " + kube.AgentNamespace + "/" + kube.AgentServiceAccount + " sign in as " + name + ".",
			Commands: []string{credentialDelete.String()},
			Check:    credentialGone,
			Apply: func(ctx context.Context, note func(string)) error {
				gone, err := credentialGone(ctx)
				if err != nil || gone {
					return err
				}
				return runner.Run(ctx, credentialDelete)
			},
		},
		{
			Title: "Delete the managed identity " + name,
			Why: "zoorikctl created it in " + group + " for this cluster, tagged " + clusterTag + "=" +
				clusterTagValue(cluster.ID()) + ".",
			Commands: []string{identityDelete.String()},
			Check:    identityGone,
			Apply: func(ctx context.Context, note func(string)) error {
				gone, err := identityGone(ctx)
				if err != nil || gone {
					return err
				}
				return runner.Run(ctx, identityDelete)
			},
		},
	}
}

func signInAKS(ctx context.Context, runner *Runner, plan *Plan, input Input, p purpose) (AzureCluster, aksCluster,
	Command, bool) {
	if !runner.Available(azureCLI) {
		plan.blockTooling("The Azure CLI (az) is not installed on this machine, so "+p.aim+" from here.",
			"Install it (https://aka.ms/installazurecli), sign in with `az login` as someone allowed to "+p.allowed+
				", and "+again(p.removal)+". Nothing has been changed.")
		return AzureCluster{}, aksCluster{}, Command{}, false
	}
	var account struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := runner.JSON(ctx, Cmd(azureCLI, "account", "show", "-o", "json"), &account); err != nil {
		plan.blockTooling("The Azure CLI is not signed in: "+err.Error(),
			"Run `az login` as someone allowed to "+p.allowed+", and "+again(p.removal)+". Nothing has been changed.")
		return AzureCluster{}, aksCluster{}, Command{}, false
	}
	if account.User.Name != "" {
		plan.Credential = "your Azure CLI sign-in (" + account.User.Name + ")"
	}
	cluster, err := resolveAzureCluster(ctx, runner, input)
	if err != nil && input.Resource == nil && input.Kept != nil {
		if kept, keptErr := azureClusterOf(*input.Kept); keptErr == nil {
			plan.Found = append(plan.Found, "The nodes did not lead to the AKS cluster ("+text.OneLine(err.Error())+
				"), so the cluster resource the agent holds is used")
			cluster, err = kept, nil
		}
	}
	if err != nil {
		plan.block(err.Error(), unresolvedAKS(err, p.removal))
		return AzureCluster{}, aksCluster{}, Command{}, false
	}
	plan.Cluster = cluster.ID()

	var shown aksCluster
	showCommand := Cmd(azureCLI, append([]string{"aks", "show"}, append(cluster.selector(), "-o", "json")...)...)
	if err := runner.JSON(ctx, showCommand, &shown); err != nil {
		plan.block("Azure would not describe the cluster: "+err.Error(),
			"Check that your `az login` is current (sign in again if it has expired) and may read "+cluster.ID()+
				" (`"+showCommand.String()+"`), and "+again(p.removal)+". Nothing has been changed.")
		return AzureCluster{}, aksCluster{}, Command{}, false
	}
	if mismatch := aksTargetMismatch(ctx, input.Target, cluster, shown); mismatch != "" {
		plan.block(mismatch, wrongCluster(p.removal))
		return AzureCluster{}, aksCluster{}, Command{}, false
	}
	return cluster, shown, showCommand, true
}

func unresolvedAKS(err error, removal bool) string {
	var failed *CommandError
	switch {
	case errors.As(err, &failed):
		return "Check that your `az login` is current (sign in again if it has expired) and may read the AKS " +
			"clusters of the cluster's subscription, and " + again(removal) + ". Nothing has been changed."
	case removal:
		return "zoorikctl could not tell from the nodes which AKS cluster this is. Nothing has been changed."
	}
	return "Pass --cluster-resource-id with the AKS cluster's ARM id " +
		"(/subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerService/" +
		"managedClusters/<name>). Nothing has been changed."
}

func aksTargetMismatch(ctx context.Context, target *kube.Target, cluster AzureCluster, shown aksCluster) string {
	host := serverHost(target.ServerURL)
	for _, fqdn := range []string{shown.FQDN, shown.PrivateFQDN} {
		if fqdn != "" && strings.EqualFold(fqdn, host) {
			return ""
		}
	}
	subscription, group, err := kube.AzureNodeResourceGroup(ctx, target)
	if err == nil && group != "" && strings.EqualFold(group, shown.NodeResourceGroup) &&
		strings.EqualFold(subscription, cluster.Subscription) {
		return ""
	}
	return fmt.Sprintf("The AKS cluster %s is not the cluster %s reaches: it answers at %s with nodes in %s, and "+
		"%s answers at %s with nodes in %s.", cluster.ID(), target.ContextName,
		text.OrText(shown.FQDN, text.OrText(shown.PrivateFQDN, "an unknown address")),
		text.OrText(shown.NodeResourceGroup, "an unknown resource group"), target.ContextName, host,
		text.OrText(group, "an unknown resource group"))
}

func ownIdentityUsable(ctx context.Context, runner *Runner, plan *Plan, cluster AzureCluster, own azureIdentity,
	issuer string) bool {
	tag := clusterTagValue(cluster.ID())
	if !taggedFor(tagValue(own.Tags, clusterTag), cluster.ID()) {
		plan.block("A managed identity named "+own.Name+" already exists in "+own.ResourceGroup+" without the tag "+
			clusterTag+"="+tag+", so zoorikctl did not create it and will not give it node removal.",
			"Check what it is for. If the agent should use it, remove its other role assignments and federated "+
				"credentials and tag it "+clusterTag+"="+tag+"; otherwise delete or rename it. Then run this again. "+
				"Nothing has been changed.")
		return false
	}
	credentials, err := federatedCredentials(ctx, runner, cluster.Subscription, own.ResourceGroup, own.Name)
	if err != nil && !NotFound(err) {
		plan.block("Azure would not list the federated credentials of "+own.Name+": "+err.Error(),
			"Check that your sign-in may read them, and run this again. Nothing has been changed.")
		return false
	}
	var others []string
	for _, credential := range credentials {
		if credential.Subject == AgentSubject() &&
			(credential.Name == agentCredentialName || sameIssuer(credential.Issuer, issuer)) {
			continue
		}
		others = append(others, trusteeOf(credential))
	}
	if len(others) > 0 {
		plan.block("The managed identity "+own.Name+" also trusts "+strings.Join(others, "; ")+", so node removal "+
			"given to it would reach them too.",
			"Remove those federated credentials from "+own.Name+", or delete the identity, and run this again. "+
				"Nothing has been changed.")
		return false
	}
	return true
}

func workloadIdentityStep(runner *Runner, cluster AzureCluster, shown aksCluster, showCommand Command) Step {
	args := append([]string{"aks", "update"}, cluster.selector()...)
	if shown.issuer() == "" {
		args = append(args, enableOIDCIssuerFlag)
	}
	if !shown.workloadIdentity() {
		args = append(args, enableWorkloadIdentityFlag)
	}
	enable := Cmd(azureCLI, append(args, "-o", "none")...)
	return Step{
		Title: "Turn on workload identity and the OIDC issuer for " + cluster.Name,
		Why: "The agent signs in to Azure as its own managed identity with a token the cluster issues to its " +
			"service account, so no secret is stored anywhere. AKS restarts its API server to start issuing " +
			"those tokens, and the OIDC issuer cannot be turned off again. No node pool, node or pod is changed; " +
			"a pod that reads a service account token only at start may need a restart afterwards, as Azure " +
			"documents.",
		Commands: []string{enable.String()},
		Check: func(ctx context.Context) (bool, error) {
			var now aksCluster
			if err := runner.JSON(ctx, showCommand, &now); err != nil {
				return false, err
			}
			return now.issuer() != "" && now.workloadIdentity(), nil
		},
		Apply: func(ctx context.Context, note func(string)) error {
			note("az aks update is running; Azure usually takes several minutes")
			return runner.Run(ctx, enable)
		},
	}
}

func identityStep(runner *Runner, cluster AzureCluster, chosen azureIdentity, location string,
	identityShow Command) Step {
	tag := clusterTagValue(cluster.ID())
	create := Cmd(azureCLI, "identity", "create", "--subscription", cluster.Subscription, "--resource-group",
		chosen.ResourceGroup, "--name", chosen.Name, "--location", location, "--tags", clusterTag+"="+tag,
		"-o", "none")
	exists := func(ctx context.Context) (bool, error) {
		err := runner.Run(ctx, identityShow)
		if NotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return Step{
		Title: "Create the managed identity " + chosen.Name + " in " + chosen.ResourceGroup,
		Why: "The identity the agent removes drained nodes with. It holds no role until the steps below, and the " +
			"tag " + clusterTag + "=" + tag + " lets disconnect find it.",
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

func federatedCredentialStep(runner *Runner, cluster AzureCluster, chosen azureIdentity, issuer string,
	showCommand Command) Step {
	credentialArgs := func(verb, issuer string) Command {
		return Cmd(azureCLI, "identity", "federated-credential", verb, "--subscription", cluster.Subscription,
			"--resource-group", chosen.ResourceGroup, "--identity-name", chosen.Name, "--name", agentCredentialName,
			"--issuer", issuer, "--subject", AgentSubject(), "--audiences", tokenExchangeAudience, "-o", "none")
	}
	currentIssuer := func(ctx context.Context) (string, error) {
		var now aksCluster
		if err := runner.JSON(ctx, showCommand, &now); err != nil {
			return "", err
		}
		return now.issuer(), nil
	}
	listed := func(ctx context.Context) ([]federatedCredential, error) {
		credentials, err := federatedCredentials(ctx, runner, cluster.Subscription, chosen.ResourceGroup, chosen.Name)
		if NotFound(err) {
			return nil, nil
		}
		return credentials, err
	}
	return Step{
		Title: "Let " + kube.AgentNamespace + "/" + kube.AgentServiceAccount + " sign in as " + chosen.Name,
		Why: "The federated credential " + agentCredentialName + " trusts only tokens this cluster's OIDC issuer " +
			"signs for that one service account.",
		Commands: []string{credentialArgs("create", text.OrText(issuer, "<issuer from step 1>")).String()},
		Check: func(ctx context.Context) (bool, error) {
			now := issuer
			if now == "" {
				read, err := currentIssuer(ctx)
				if err != nil || read == "" {
					return false, err
				}
				now = read
			}
			credentials, err := listed(ctx)
			if err != nil {
				return false, err
			}
			return trusts(credentials, now), nil
		},
		Apply: func(ctx context.Context, note func(string)) error {
			now, err := currentIssuer(ctx)
			if err != nil {
				return err
			}
			if now == "" {
				return fmt.Errorf("the cluster %s has no OIDC issuer yet; run this again once it has one", cluster.Name)
			}
			credentials, err := listed(ctx)
			if err != nil {
				return err
			}
			if trusts(credentials, now) {
				return nil
			}
			for _, credential := range credentials {
				if credential.Name != agentCredentialName {
					continue
				}
				if credential.Subject != AgentSubject() {
					return fmt.Errorf("the federated credential %s of %s trusts %s, not the agent; zoorikctl will "+
						"not change it", agentCredentialName, chosen.Name, trusteeOf(credential))
				}
				note("updating the federated credential " + agentCredentialName + " to this cluster's issuer")
				return runner.Run(ctx, credentialArgs("update", now))
			}
			return runner.Run(ctx, credentialArgs("create", now))
		},
	}
}

func roleDefinitionStep(runner *Runner, subscription string, extended *roleDefinition) Step {
	ready := func(ctx context.Context) (bool, error) {
		return nodeRemovalRoleReady(ctx, runner, subscription)
	}
	awaitListed := func(ctx context.Context, note func(string)) error {
		note("waiting for Azure to list the role in subscription " + subscription + "; it can take a few minutes")
		listed, err := waitFor(ctx, func() (bool, error) { return ready(ctx) })
		if err != nil {
			return err
		}
		if !listed {
			return fmt.Errorf("Azure did not list the role %q in subscription %s within %s; run this again",
				nodeRemovalRole, subscription, crdWait)
		}
		return nil
	}
	if extended != nil {
		update := Cmd(azureCLI, "role", "definition", "update", "--role-definition",
			nodeRemovalRoleExtension(*extended, subscription), "-o", "none")
		return Step{
			Title: "Make the role \"" + nodeRemovalRole + "\" assignable in subscription " + subscription,
			Why: "Custom role names are unique in an Azure tenant, and this role is already defined there for " +
				strings.Join(extended.AssignableScopes, ", ") + ". Adding this subscription to its assignable scopes " +
				"lets it be assigned on this cluster; what it allows stays the same: read the cluster, its agent " +
				"pools and their machines, delete named machines, and write agent pools, which the agent uses only " +
				"to lower an autoscaled pool's minimum count.",
			Commands: []string{update.String()},
			Check:    ready,
			Apply: func(ctx context.Context, note func(string)) error {
				present, err := ready(ctx)
				if err != nil || present {
					return err
				}
				if err := runner.Run(ctx, update); err != nil {
					return err
				}
				return awaitListed(ctx, note)
			},
		}
	}
	create := Cmd(azureCLI, "role", "definition", "create", "--subscription", subscription, "--role-definition",
		nodeRemovalRoleDefinition(subscription), "-o", "none")
	return Step{
		Title: "Define the role \"" + nodeRemovalRole + "\" in subscription " + subscription,
		Why: "It reads the cluster, its agent pools and their machines, deletes named machines, and writes agent " +
			"pools: Azure has no narrower action than agentPools/write for lowering a pool's minimum count, which " +
			"is all the agent uses it for. It is defined once per subscription and assigned only on the clusters " +
			"that use it.",
		Commands: []string{create.String()},
		Check:    ready,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := ready(ctx)
			if err != nil || present {
				return err
			}
			err = runner.Run(ctx, create)
			if stderrHas(err, "RoleDefinitionWithSameNameExists") {
				return fmt.Errorf("a custom role named %q already exists in this Azure tenant, in a subscription your "+
					"sign-in cannot list; check that it allows exactly %s and nothing else, add \"/subscriptions/%s\" "+
					"to its AssignableScopes with az role definition update, and run this again: %w", nodeRemovalRole,
					strings.Join(nodeRemovalActions, ", "), subscription, err)
			}
			if err != nil {
				return err
			}
			return awaitListed(ctx, note)
		},
	}
}

func roleAssignmentStep(runner *Runner, cluster AzureCluster, chosen azureIdentity, identityShow Command) Step {
	assign := func(principal string) Command {
		return Cmd(azureCLI, "role", "assignment", "create", "--subscription", cluster.Subscription,
			"--assignee-object-id", principal, "--assignee-principal-type", managedIdentityPrincipal, "--role",
			nodeRemovalRole, "--scope", cluster.ID(), "-o", "none")
	}
	principal := principalOf(runner, identityShow)
	return Step{
		Title: "Assign \"" + nodeRemovalRole + "\" to " + chosen.Name + " on " + cluster.Name + " only",
		Why: "The assignment's scope is this one managed cluster, so the identity can delete machines of this " +
			"cluster's agent pools and create or change those pools, and nothing in any other cluster.",
		Commands: []string{assign(text.OrText(chosen.PrincipalID, "<principal id of "+chosen.Name+">")).String()},
		Check: func(ctx context.Context) (bool, error) {
			ready, err := nodeRemovalRoleReady(ctx, runner, cluster.Subscription)
			if err != nil || !ready {
				return false, err
			}
			id, err := principal(ctx)
			if err != nil || id == "" {
				return false, err
			}
			ids, err := roleAssignmentsOn(ctx, runner, cluster, nodeRemovalRole, id)
			return len(ids) > 0, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			id, err := principal(ctx)
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("the managed identity %s was not found in %s", chosen.Name, chosen.ResourceGroup)
			}
			note("assigning; Azure can take a few minutes to see a new identity or role")
			done, err := waitFor(ctx, func() (bool, error) {
				ready, err := nodeRemovalRoleReady(ctx, runner, cluster.Subscription)
				if err != nil || !ready {
					return false, err
				}
				err = runner.Run(ctx, assign(id))
				switch {
				case err == nil, stderrHas(err, "RoleAssignmentExists"):
					return true, nil
				case stderrHas(err, assignmentRetryMarkers...):
					return false, nil
				}
				return false, err
			})
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("Azure did not see the identity or the role within %s; run this again", crdWait)
			}
			return nil
		},
	}
}

func legacyAssignmentRemovalStep(runner *Runner, cluster AzureCluster, chosen azureIdentity, identityShow Command,
	idsAtPlan []string) Step {
	principal := principalOf(runner, identityShow)
	assigned := func(ctx context.Context) ([]string, error) {
		id, err := principal(ctx)
		if err != nil || id == "" {
			return nil, err
		}
		return roleAssignmentsOn(ctx, runner, cluster, legacyNodeRemovalRole, id)
	}
	return Step{
		Title: "Remove the \"" + legacyNodeRemovalRole + "\" assignment of " + chosen.Name + " on " + cluster.Name,
		Why: "\"" + nodeRemovalRole + "\", assigned above, allows everything it did. The role \"" +
			legacyNodeRemovalRole + "\" itself stays defined for the clusters that still use it.",
		Commands: []string{unassignCommand(cluster.Subscription, idsAtPlan).String()},
		Check: func(ctx context.Context) (bool, error) {
			ids, err := assigned(ctx)
			return err == nil && len(ids) == 0, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			ids, err := assigned(ctx)
			if err != nil || len(ids) == 0 {
				return err
			}
			return runner.Run(ctx, unassignCommand(cluster.Subscription, ids))
		},
	}
}

func principalOf(runner *Runner, identityShow Command) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var shown azureIdentity
		err := runner.JSON(ctx, identityShow, &shown)
		if NotFound(err) {
			return "", nil
		}
		return shown.PrincipalID, err
	}
}

func trustedIdentity(ctx context.Context, runner *Runner, cluster AzureCluster, issuer string,
	identities []azureIdentity, occupant string) (azureIdentity, bool, []string) {
	var candidates []azureIdentity
	seen := map[string]bool{}
	add := func(identity azureIdentity) {
		key := strings.ToLower(identity.ID)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, identity)
	}
	if occupant != "" {
		for _, identity := range identities {
			if strings.EqualFold(identity.ClientID, occupant) {
				add(identity)
			}
		}
	}
	var assignments []roleAssignment
	if runner.JSON(ctx, Cmd(azureCLI, "role", "assignment", "list", "--subscription", cluster.Subscription,
		"--scope", cluster.ID(), "--fill-principal-name", "false", "-o", "json"), &assignments) == nil {
		for _, assignment := range assignments {
			if !strings.EqualFold(assignment.PrincipalType, managedIdentityPrincipal) {
				continue
			}
			for _, identity := range identities {
				if strings.EqualFold(identity.PrincipalID, assignment.PrincipalID) {
					add(identity)
				}
			}
		}
	}
	var passed []string
	for _, candidate := range candidates {
		credentials, err := federatedCredentials(ctx, runner, cluster.Subscription, candidate.ResourceGroup,
			candidate.Name)
		if err != nil || !trusts(credentials, issuer) {
			continue
		}
		var others []string
		for _, credential := range credentials {
			if credential.Subject != AgentSubject() || !sameIssuer(credential.Issuer, issuer) {
				others = append(others, trusteeOf(credential))
			}
		}
		if len(others) > 0 {
			passed = append(passed, "Managed identity "+candidate.Name+" in "+candidate.ResourceGroup+" trusts "+
				kube.AgentNamespace+"/"+kube.AgentServiceAccount+" on this cluster but also "+strings.Join(others, "; ")+
				", so node removal is not added to it")
			continue
		}
		return candidate, true, passed
	}
	return azureIdentity{}, false, passed
}

func federatedCredentials(ctx context.Context, runner *Runner, subscription, group, identity string) (
	[]federatedCredential, error) {
	var credentials []federatedCredential
	err := runner.JSON(ctx, Cmd(azureCLI, "identity", "federated-credential", "list", "--subscription", subscription,
		"--resource-group", group, "--identity-name", identity, "-o", "json"), &credentials)
	return credentials, err
}

func trusts(credentials []federatedCredential, issuer string) bool {
	for _, credential := range credentials {
		if sameIssuer(credential.Issuer, issuer) && credential.Subject == AgentSubject() {
			return true
		}
	}
	return false
}

func sameIssuer(got, want string) bool {
	return want != "" && strings.EqualFold(strings.TrimRight(got, "/"), strings.TrimRight(want, "/"))
}

func trusteeOf(credential federatedCredential) string {
	return credential.Subject + " from " + credential.Issuer + " (" + credential.Name + ")"
}

func nodeRemovalDefinitions(ctx context.Context, runner *Runner, subscription string) ([]roleDefinition, error) {
	return customRoles(ctx, runner, subscription, nodeRemovalRole)
}

func customRoles(ctx context.Context, runner *Runner, subscription, role string) ([]roleDefinition, error) {
	var definitions []roleDefinition
	err := runner.JSON(ctx, Cmd(azureCLI, "role", "definition", "list", "--subscription", subscription,
		"--custom-role-only", "true", "--name", role, "--scope", "/subscriptions/"+subscription, "-o", "json"),
		&definitions)
	return definitions, err
}

func nodeRemovalRoleReady(ctx context.Context, runner *Runner, subscription string) (bool, error) {
	definitions, err := nodeRemovalDefinitions(ctx, runner, subscription)
	if err != nil || len(definitions) == 0 {
		return false, err
	}
	for _, definition := range definitions {
		if !definition.removesNodesOnly() {
			return false, nil
		}
	}
	return true, nil
}

func nodeRemovalDefinedElsewhere(ctx context.Context, runner *Runner, subscription string) (roleDefinition, bool) {
	var accounts []struct {
		ID       string `json:"id"`
		TenantID string `json:"tenantId"`
		State    string `json:"state"`
	}
	if runner.JSON(ctx, Cmd(azureCLI, "account", "list", "-o", "json"), &accounts) != nil {
		return roleDefinition{}, false
	}
	tenant := ""
	for _, account := range accounts {
		if strings.EqualFold(account.ID, subscription) {
			tenant = account.TenantID
		}
	}
	if tenant == "" {
		return roleDefinition{}, false
	}
	for _, account := range accounts {
		if strings.EqualFold(account.ID, subscription) || !strings.EqualFold(account.TenantID, tenant) ||
			!strings.EqualFold(account.State, "Enabled") {
			continue
		}
		definitions, err := nodeRemovalDefinitions(ctx, runner, account.ID)
		if err != nil {
			continue
		}
		for _, definition := range definitions {
			if len(definition.AssignableScopes) > 0 {
				return definition, true
			}
		}
	}
	return roleDefinition{}, false
}

func blockForeignRole(plan *Plan, definition roleDefinition) bool {
	if definition.removesNodesOnly() {
		return false
	}
	scope := "/subscriptions/<subscription>"
	if len(definition.AssignableScopes) > 0 {
		scope = definition.AssignableScopes[0]
	}
	plan.block("A custom role named \""+nodeRemovalRole+"\" already exists ("+text.OrText(definition.ID, "id unknown")+
		") and allows "+definition.allows()+", not exactly what zoorikctl defines, so zoorikctl will not assign it.",
		"Check what it allows and where it is assigned (`az role definition list --custom-role-only true --name "+
			kube.ShellWord(nodeRemovalRole)+" --scope "+scope+" -o json`). If it is an older or hand-made copy of "+
			"Zoorik's role, set its actions to exactly "+strings.Join(nodeRemovalActions, ", ")+" with `az role "+
			"definition update`, or delete it; then run this again. Nothing has been changed.")
	return true
}

func roleAssignmentsOn(ctx context.Context, runner *Runner, cluster AzureCluster, role, principal string) (
	[]string, error) {
	definitions, err := customRoles(ctx, runner, cluster.Subscription, role)
	if err != nil || len(definitions) == 0 {
		return nil, err
	}
	var assignments []roleAssignment
	if err := runner.JSON(ctx, Cmd(azureCLI, "role", "assignment", "list", "--subscription", cluster.Subscription,
		"--scope", cluster.ID(), "--role", role, "--fill-principal-name", "false", "-o", "json"),
		&assignments); err != nil {
		return nil, err
	}
	var ids []string
	for _, assignment := range assignments {
		if !strings.EqualFold(assignment.Scope, cluster.ID()) {
			continue
		}
		if principal == "" || strings.EqualFold(assignment.PrincipalID, principal) {
			ids = append(ids, assignment.ID)
		}
	}
	return ids, nil
}

func findIdentity(identities []azureIdentity, name, group string) (azureIdentity, bool) {
	for _, identity := range identities {
		if strings.EqualFold(identity.Name, name) && strings.EqualFold(identity.ResourceGroup, group) {
			return identity, true
		}
	}
	return azureIdentity{}, false
}

func tagValue(tags map[string]string, key string) string {
	for name, value := range tags {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func sameSet(got, want []string) bool {
	folded := func(values []string) map[string]bool {
		set := map[string]bool{}
		for _, value := range values {
			set[strings.ToLower(value)] = true
		}
		return set
	}
	gotSet, wantSet := folded(got), folded(want)
	if len(gotSet) != len(wantSet) {
		return false
	}
	for value := range wantSet {
		if !gotSet[value] {
			return false
		}
	}
	return true
}

func stderrHas(err error, markers ...string) bool {
	var failed *CommandError
	if !errors.As(err, &failed) {
		return false
	}
	for _, marker := range markers {
		if strings.Contains(failed.Stderr, marker) {
			return true
		}
	}
	return false
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
