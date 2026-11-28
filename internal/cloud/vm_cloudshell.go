package cloud

import (
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/kube"
)

func AzureCloudShell(vm AzureVM, grant api.AzureVMGrant, org string) string {
	word := kube.ShellWord
	diskRole := AzureVMRoleName(grant.DiskRole, vm.Subscription)
	catalogRole := AzureVMRoleName(grant.CatalogRole, vm.Subscription)
	return strings.Join([]string{
		"# Zoorik Amoeba: let Zoorik manage the data disks of VM " + vm.Name,
		"# (resource group " + vm.ResourceGroup + ", subscription " + vm.Subscription + "). Safe to run more than once.",
		"SUB=" + word(vm.Subscription) + "; RG=" + word(vm.ResourceGroup) + "; VM=" + word(vm.ID) + "; APP=" +
			word(grant.AppID) + "; ORG=" + word(org),
		`RGL=$(printf %s "$RG" | tr '[:upper:]' '[:lower:]')`,
		"DISKS=" + word(diskRole) + "; CATALOG=" + word(catalogRole),
		`role_id() { az role definition list --custom-role-only true --name "$1" --scope "/subscriptions/$SUB" ` +
			`--query '[0].id' -o tsv; }`,
		`assigned() { [ -n "$(az role assignment list --assignee "$SP" --role "$1" --scope "$2" --query '[0].id' ` +
			`-o tsv)" ]; }`,
		`az account set --subscription "$SUB"`,
		`az ad sp show --id "$APP" -o none 2>/dev/null || az ad sp create --id "$APP" -o none`,
		`SP=$(az ad sp show --id "$APP" --query id -o tsv)`,
		`[ -n "$(role_id "$DISKS")" ]   || az role definition create --role-definition ` +
			word(vmRoleDefinition(diskRole, vm.Subscription, grant.DiskRole)) + ` -o none`,
		`[ -n "$(role_id "$CATALOG")" ] || az role definition create --role-definition ` +
			word(vmRoleDefinition(catalogRole, vm.Subscription, grant.CatalogRole)) + ` -o none`,
		`until DISKS_ID=$(role_id "$DISKS"); CATALOG_ID=$(role_id "$CATALOG"); [ -n "$DISKS_ID" ] && ` +
			`[ -n "$CATALOG_ID" ]; do sleep 10; done`,
		`az tag update --resource-id "/subscriptions/$SUB/resourceGroups/$RG" --operation Merge --tags ` +
			word(grant.OrgTagKey+"=") + `"$ORG" -o none`,
		`assigned "$DISKS_ID" "/subscriptions/$SUB/resourceGroups/$RG" || az role assignment create ` +
			`--assignee-object-id "$SP" --assignee-principal-type ServicePrincipal --role "$DISKS_ID" --scope ` +
			`"/subscriptions/$SUB/resourceGroups/$RG" -o none`,
		`for DISK in $(az vm show --ids "$VM" --query 'storageProfile.dataDisks[].managedDisk.id' -o tsv); do`,
		`  DES=$(az disk show --ids "$DISK" --query encryption.diskEncryptionSetId -o tsv)`,
		`  case "$(printf %s "$DES" | tr '[:upper:]' '[:lower:]')" in ""|*/resourcegroups/"$RGL"/*) ;; *) assigned ` +
			`"$DISKS_ID" "$DES" || az role assignment create --assignee-object-id "$SP" --assignee-principal-type ` +
			`ServicePrincipal --role "$DISKS_ID" --scope "$DES" -o none ;; esac`,
		`done`,
		`assigned "$CATALOG_ID" "/subscriptions/$SUB" || az role assignment create --assignee-object-id "$SP" ` +
			`--assignee-principal-type ServicePrincipal --role "$CATALOG_ID" --scope "/subscriptions/$SUB" -o none`,
	}, "\n")
}

func AWSCloudShell(vm AWSVM, grant api.AWSVMGrant) string {
	word := kube.ShellWord
	tag := word("Key=" + grant.VMTagKey + ",Value=")
	return strings.Join([]string{
		"# Zoorik Amoeba: let Zoorik manage the data disks of instance " + vm.InstanceID + " (" + vm.Name + "), " +
			vm.Region + ", account " + vm.Account + ".",
		"# Safe to run more than once.",
		"I=" + word(vm.InstanceID) + "; R=" + word(vm.Region) + "; ROLE=" + word(grant.RoleNamePrefix+vm.InstanceID),
		"TRUST=" + word(grant.TrustPolicy),
		"POLICY=" + word(AWSVMPolicy(vm, grant)),
		`aws iam get-role --role-name "$ROLE" >/dev/null 2>&1 || aws iam create-role --role-name "$ROLE" ` +
			`--assume-role-policy-document "$TRUST" --description "Zoorik Amoeba: the data disks of $I" --tags ` +
			tag + `"$I" >/dev/null`,
		`aws iam update-assume-role-policy --role-name "$ROLE" --policy-document "$TRUST"`,
		`aws iam put-role-policy --role-name "$ROLE" --policy-name ` + word(grant.PolicyName) +
			` --policy-document "$POLICY"`,
		`ROOT=$(aws ec2 describe-instances --region "$R" --instance-ids "$I" --query ` +
			`'Reservations[0].Instances[0].RootDeviceName' --output text)`,
		`VOLS=$(aws ec2 describe-instances --region "$R" --instance-ids "$I" --query ` +
			`"Reservations[0].Instances[0].BlockDeviceMappings[?DeviceName!='$ROOT'].Ebs.VolumeId" --output text)`,
		`UNTAGGED=$([ -z "$VOLS" ] || aws ec2 describe-volumes --region "$R" --volume-ids $VOLS --query ` +
			`"Volumes[?!(Tags[?Key=='` + grant.VMTagKey + `'])].VolumeId" --output text)`,
		`[ -z "$UNTAGGED" ] || aws ec2 create-tags --region "$R" --resources $UNTAGGED --tags ` + tag + `"$I"`,
	}, "\n")
}

func vmRoleDefinition(name, subscription string, role api.AzureRole) string {
	return compactJSON(map[string]any{
		"Name":             name,
		"IsCustom":         true,
		"Description":      role.Description,
		"Actions":          role.Actions,
		"NotActions":       []string{},
		"DataActions":      []string{},
		"NotDataActions":   []string{},
		"AssignableScopes": []string{"/subscriptions/" + subscription},
	})
}
