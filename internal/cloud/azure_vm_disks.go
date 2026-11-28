package cloud

import (
	"context"
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	vmDisksProvisioner = "Zoorik's access to this VM's data disks"

	vmGrantLaterAzure = "The VM connects without Zoorik's access to its data disks, and the commands that grant it are " +
		"printed at the end: run them in Azure Cloud Shell, or give them to someone who may make these changes."

	managedIdentityUser = "servicePrincipal"
)

type AzureVM struct {
	Subscription  string
	ResourceGroup string
	Name          string
	ID            string
	DataDisks     []string
}

func (vm AzureVM) subscriptionScope() string {
	return "/subscriptions/" + vm.Subscription
}

func (vm AzureVM) group() string {
	return vm.subscriptionScope() + "/resourceGroups/" + vm.ResourceGroup
}

func (vm AzureVM) inGroup(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), strings.ToLower(vm.group())+"/")
}

func AzureVMRoleName(role api.AzureRole, subscription string) string {
	return role.BaseName + " " + subscription
}

func PlanAzureVMDisks(ctx context.Context, runner *Runner, vm AzureVM, grant api.AzureVMGrant, org string) VMDisks {
	plan := VMDisks{Plan: Plan{Cloud: kube.CloudAzure, Provisioner: vmDisksProvisioner, Credential: azureCredential}}
	if !runner.Available(azureCLI) {
		plan.blockTooling("The Azure CLI (az) is not installed on this VM, so zoorikctl cannot grant "+
			vmDisksProvisioner+" from here.", vmGrantLaterAzure)
		return plan
	}
	var account struct {
		User struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"user"`
	}
	if err := runner.JSON(ctx, Cmd(azureCLI, "account", "show", "--subscription", vm.Subscription, "-o", "json"),
		&account); err != nil {
		plan.blockTooling("The Azure CLI is not signed in, or its sign-in cannot see subscription "+vm.Subscription+
			": "+text.OneLine(err.Error()), vmGrantLaterAzure)
		return plan
	}
	plan.Credential = "your Azure CLI sign-in (" + azureSignIn(account.User.Name, account.User.Type) + ")"

	diskRole := AzureVMRoleName(grant.DiskRole, vm.Subscription)
	catalogRole := AzureVMRoleName(grant.CatalogRole, vm.Subscription)
	for _, role := range []struct {
		name    string
		actions []string
	}{{diskRole, grant.DiskRole.Actions}, {catalogRole, grant.CatalogRole.Actions}} {
		definitions, err := customRoles(ctx, runner, vm.Subscription, role.name)
		if err != nil {
			plan.unread("The role \""+role.name+"\"", err, vmGrantLaterAzure)
			return plan
		}
		for _, definition := range definitions {
			if blockCustomRole(&plan.Plan, role.name, role.actions, definition) {
				return plan
			}
		}
	}

	tagged := func(ctx context.Context) (string, error) {
		var group struct {
			Tags map[string]string `json:"tags"`
		}
		err := runner.JSON(ctx, Cmd(azureCLI, "group", "show", "--subscription", vm.Subscription, "--name",
			vm.ResourceGroup, "-o", "json"), &group)
		return tagValue(group.Tags, grant.OrgTagKey), err
	}
	owner, err := tagged(ctx)
	if err != nil {
		plan.unread("The resource group "+vm.ResourceGroup, err, vmGrantLaterAzure)
		return plan
	}
	if owner != "" && owner != org {
		plan.TaggedFor = owner
		return plan
	}

	var sets []string
	for _, disk := range vm.DataDisks {
		if !vm.inGroup(disk) {
			plan.note(armName(disk) + " is in resource group " + armSegment(disk, "resourceGroups") +
				", outside this grant; Amoeba leaves it alone.")
		}
		output, err := runner.Output(ctx, Cmd(azureCLI, "disk", "show", "--ids", disk, "--query",
			"encryption.diskEncryptionSetId", "-o", "tsv"))
		if err != nil {
			plan.unread("The data disk "+armName(disk), err, vmGrantLaterAzure)
			return plan
		}
		set := strings.TrimSpace(string(output))
		if set != "" && !vm.inGroup(set) && !containsFold(sets, set) {
			sets = append(sets, set)
		}
	}

	servicePrincipal := func(ctx context.Context) (string, error) {
		var shown struct {
			ID string `json:"id"`
		}
		err := runner.JSON(ctx, Cmd(azureCLI, "ad", "sp", "show", "--id", grant.AppID, "-o", "json"), &shown)
		if NotFound(err) {
			return "", nil
		}
		return shown.ID, err
	}
	principal, err := servicePrincipal(ctx)
	if err != nil {
		plan.unread("Zoorik's app in your Microsoft Entra directory", err, vmGrantLaterAzure)
		return plan
	}
	principalShown := text.OrText(principal, "<object id of Zoorik's app>")

	tag := Cmd(azureCLI, "tag", "update", "--resource-id", vm.group(), "--operation", "Merge", "--tags",
		grant.OrgTagKey+"="+org, "-o", "none")
	plan.Steps = append(plan.Steps,
		appStep(runner, grant.AppID, servicePrincipal),
		vmRoleStep(runner, vm.Subscription, diskRole, grant.DiskRole),
		vmRoleStep(runner, vm.Subscription, catalogRole, grant.CatalogRole),
		Step{
			Title: "Tag the resource group " + vm.ResourceGroup + " with " + grant.OrgTagKey + "=" + org,
			Why: "The tag names your Zoorik organisation as the one whose VMs in this group Zoorik may manage. " +
				"Zoorik checks it before it acts, so the roles below work for your organisation only.",
			Commands: []string{tag.String()},
			Check: func(ctx context.Context) (bool, error) {
				owner, err := tagged(ctx)
				return owner == org, err
			},
			Apply: func(ctx context.Context, note func(string)) error {
				owner, err := tagged(ctx)
				if err != nil || owner == org {
					return err
				}
				return runner.Run(ctx, tag)
			},
		},
		vmAssignmentStep(runner, vm.Subscription, diskRole, grant.DiskRole.Actions, vm.group(), principalShown,
			servicePrincipal, "Assign \""+diskRole+"\" to Zoorik's app on the resource group "+vm.ResourceGroup,
			"One assignment covers this VM and every other VM in this group you connect. Zoorik creates new "+
				"disks and safety snapshots in this group."))
	for _, set := range sets {
		plan.Steps = append(plan.Steps, vmAssignmentStep(runner, vm.Subscription, diskRole, grant.DiskRole.Actions,
			set, principalShown, servicePrincipal,
			"Assign \""+diskRole+"\" to Zoorik's app on the disk encryption set "+armName(set),
			"A data disk of this VM is encrypted with this set, which is in resource group "+
				armSegment(set, "resourceGroups")+". Zoorik reads it to encrypt the disks it creates the same way."))
	}
	plan.Steps = append(plan.Steps, vmAssignmentStep(runner, vm.Subscription, catalogRole,
		grant.CatalogRole.Actions, vm.subscriptionScope(), principalShown, servicePrincipal,
		"Assign \""+catalogRole+"\" to Zoorik's app on subscription "+vm.Subscription,
		"Zoorik reads the subscription's VM sizes, disk SKUs and region names, and finds this VM's disks with "+
			"Resource Graph, which returns only what the disk role lets Zoorik read."))
	return plan
}

func appStep(runner *Runner, appID string, servicePrincipal func(context.Context) (string, error)) Step {
	create := Cmd(azureCLI, "ad", "sp", "create", "--id", appID, "-o", "none")
	return Step{
		Title: "Add Zoorik's app " + appID + " to your Microsoft Entra directory",
		Why: "Zoorik reaches this VM's disks as its own app. Adding it gives it a service principal in your " +
			"directory, which holds no rights until the roles below are assigned to it.",
		Commands: []string{create.String()},
		Check: func(ctx context.Context) (bool, error) {
			principal, err := servicePrincipal(ctx)
			return principal != "", err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			principal, err := servicePrincipal(ctx)
			if err != nil || principal != "" {
				return err
			}
			return runner.Run(ctx, create)
		},
	}
}

func vmRoleStep(runner *Runner, subscription, name string, role api.AzureRole) Step {
	create := Cmd(azureCLI, "role", "definition", "create", "--subscription", subscription, "--role-definition",
		vmRoleDefinition(name, subscription, role), "-o", "none")
	ready := func(ctx context.Context) (bool, error) {
		return customRoleReady(ctx, runner, subscription, name, role.Actions)
	}
	return Step{
		Title:    "Define the role \"" + name + "\"",
		Why:      role.Description + " It is defined once for this subscription.",
		Commands: []string{create.String()},
		Check:    ready,
		Apply: func(ctx context.Context, note func(string)) error {
			present, err := ready(ctx)
			if err != nil || present {
				return err
			}
			err = runner.Run(ctx, create)
			exists := stderrHas(err, "RoleDefinitionWithSameNameExists")
			if err != nil && !exists {
				return err
			}
			listed, err := roleListed(ctx, note, subscription, ready)
			switch {
			case err != nil:
				return err
			case listed:
				return nil
			case exists:
				return fmt.Errorf("a custom role named %q already exists in this Azure tenant, in a subscription "+
					"your sign-in cannot list; add \"/subscriptions/%s\" to its AssignableScopes with az role "+
					"definition update, and run this again", name, subscription)
			}
			return fmt.Errorf("Azure did not list the role %q in subscription %s within %s", name, subscription,
				crdWait)
		},
	}
}

func vmAssignmentStep(runner *Runner, subscription, role string, actions []string, scope, principalShown string,
	servicePrincipal func(context.Context) (string, error), title, why string) Step {
	assign := func(principal, roleID string) Command {
		return Cmd(azureCLI, "role", "assignment", "create", "--subscription", subscription, "--assignee-object-id",
			principal, "--assignee-principal-type", managedIdentityPrincipal, "--role", roleID, "--scope", scope,
			"-o", "none")
	}
	return Step{
		Title:    title,
		Why:      why,
		Commands: []string{assign(principalShown, role).String()},
		Check: func(ctx context.Context) (bool, error) {
			ready, err := customRoleReady(ctx, runner, subscription, role, actions)
			if err != nil || !ready {
				return false, err
			}
			principal, err := servicePrincipal(ctx)
			if err != nil || principal == "" {
				return false, err
			}
			ids, err := roleAssignmentsAt(ctx, runner, subscription, scope, role, principal)
			return len(ids) > 0, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			principal, err := servicePrincipal(ctx)
			if err != nil {
				return err
			}
			if principal == "" {
				return fmt.Errorf("Zoorik's app has no service principal in your Microsoft Entra directory")
			}
			note("assigning; Azure can take a few minutes to see a new app or role")
			done, err := waitFor(ctx, func() (bool, error) {
				roleID, err := customRoleID(ctx, runner, subscription, role, actions)
				if err != nil || roleID == "" {
					return false, err
				}
				err = runner.Run(ctx, assign(principal, roleID))
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
				return fmt.Errorf("Azure did not see Zoorik's app or the role %q within %s", role, crdWait)
			}
			return nil
		},
	}
}

func azureSignIn(name, kind string) string {
	if kind == managedIdentityUser && (name == "systemAssignedIdentity" || name == "userAssignedIdentity") {
		return "this VM's own managed identity"
	}
	return text.OrText(name, "az")
}

func (p *VMDisks) unread(what string, err error, later string) {
	p.blockTooling(what+" could not be read with "+p.Credential+": "+text.OneLine(err.Error()), later)
}

func armName(id string) string {
	return id[strings.LastIndex(id, "/")+1:]
}

func armSegment(id, key string) string {
	parts := strings.Split(id, "/")
	for index := 0; index+1 < len(parts); index++ {
		if strings.EqualFold(parts[index], key) {
			return parts[index+1]
		}
	}
	return ""
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
