package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const vmGrantLaterAWS = "The VM connects without Zoorik's access to its data disks, and the commands that grant it are " +
	"printed at the end: run them in AWS CloudShell, or give them to someone who may make these changes."

type AWSVM struct {
	InstanceID string
	Name       string
	Region     string
	Account    string
}

func (vm AWSVM) args(args ...string) []string {
	return append(args, "--region", vm.Region, "--output", "json")
}

func AWSVMPolicy(vm AWSVM, grant api.AWSVMGrant) string {
	return strings.NewReplacer("{{region}}", vm.Region, "{{account}}", vm.Account, "{{instanceId}}", vm.InstanceID).
		Replace(grant.PolicyTemplate)
}

func PlanAWSVMDisks(ctx context.Context, runner *Runner, vm AWSVM, grant api.AWSVMGrant) VMDisks {
	plan := VMDisks{Plan: Plan{Cloud: kube.CloudAWS, Provisioner: vmDisksProvisioner,
		Credential: "your AWS CLI credentials"}}
	if !runner.Available(awsCLI) {
		plan.blockTooling("The AWS CLI (aws) is not installed on this VM, so zoorikctl cannot grant "+
			vmDisksProvisioner+" from here.", vmGrantLaterAWS)
		return plan
	}
	var caller struct {
		Account string `json:"Account"`
		Arn     string `json:"Arn"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, vm.args("sts", "get-caller-identity")...), &caller); err != nil {
		plan.blockTooling("The AWS CLI has no usable credentials: "+text.OneLine(err.Error()), vmGrantLaterAWS)
		return plan
	}
	plan.Credential = "your AWS CLI credentials (" + awsSignIn(caller.Arn, vm.InstanceID) + ")"
	if caller.Account != vm.Account {
		plan.blockTooling("Your AWS CLI credentials are for account "+caller.Account+", and this instance is in "+
			"account "+vm.Account+".", vmGrantLaterAWS)
		return plan
	}

	role := grant.RoleNamePrefix + vm.InstanceID
	_, present, err := vmRole(ctx, runner, vm, role)
	if err != nil {
		plan.unread("The role "+role, err, vmGrantLaterAWS)
		return plan
	}
	volumes := func(ctx context.Context) ([]string, []string, error) {
		return dataVolumes(ctx, runner, vm, grant.VMTagKey)
	}
	untagged, foreign, err := volumes(ctx)
	if err != nil {
		plan.unread("The volumes of "+vm.InstanceID, err, vmGrantLaterAWS)
		return plan
	}
	for _, volume := range foreign {
		plan.note(volume + ", for another instance, so Amoeba leaves it alone.")
	}

	plan.Steps = append(plan.Steps, vmRoleTrustStep(runner, vm, grant, role, present),
		vmPolicyStep(runner, vm, grant, role))
	if len(untagged) > 0 {
		tagCommand := func(resources []string) Command {
			return Cmd(awsCLI, vm.args(append(append([]string{"ec2", "create-tags", "--resources"}, resources...),
				"--tags", "Key="+grant.VMTagKey+",Value="+vm.InstanceID)...)...)
		}
		plan.Steps = append(plan.Steps, Step{
			Title: fmt.Sprintf("Tag %d data volume(s) of %s with %s=%s", len(untagged), vm.InstanceID, grant.VMTagKey,
				vm.InstanceID),
			Why: "The role's policy lets Zoorik change, snapshot and delete only the volumes that carry this tag. " +
				"The root volume is never tagged.",
			Commands: []string{tagCommand(untagged).String()},
			Check: func(ctx context.Context) (bool, error) {
				missing, _, err := volumes(ctx)
				return err == nil && len(missing) == 0, err
			},
			Apply: func(ctx context.Context, note func(string)) error {
				missing, _, err := volumes(ctx)
				if err != nil || len(missing) == 0 {
					return err
				}
				return runner.Run(ctx, tagCommand(missing))
			},
		})
	}
	return plan
}

func vmRoleTrustStep(runner *Runner, vm AWSVM, grant api.AWSVMGrant, role string, present bool) Step {
	create := Cmd(awsCLI, vm.args("iam", "create-role", "--role-name", role, "--assume-role-policy-document",
		grant.TrustPolicy, "--description", "Zoorik Amoeba: the data disks of "+vm.InstanceID, "--tags",
		"Key="+grant.VMTagKey+",Value="+vm.InstanceID)...)
	update := Cmd(awsCLI, vm.args("iam", "update-assume-role-policy", "--role-name", role, "--policy-document",
		grant.TrustPolicy)...)
	state := func(ctx context.Context) (bool, bool, error) {
		found, present, err := vmRole(ctx, runner, vm, role)
		if err != nil || !present {
			return present, false, err
		}
		return true, sameDocument(found.Role.AssumeRolePolicyDocument, grant.TrustPolicy), nil
	}
	title, command := "Create the role "+role, create
	if present {
		title, command = "Let only Zoorik, with your organisation's ExternalId, assume the role "+role, update
	}
	return Step{
		Title: title,
		Why: "Zoorik assumes this role to manage this instance's data volumes. Its trust admits only Zoorik's AWS " +
			"account, and only with your organisation's ExternalId, so no other Zoorik organisation can use it.",
		Commands: []string{command.String()},
		Check: func(ctx context.Context) (bool, error) {
			present, trusted, err := state(ctx)
			return present && trusted, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			present, trusted, err := state(ctx)
			switch {
			case err != nil:
				return err
			case !present:
				return runner.Run(ctx, create)
			case trusted:
				return nil
			}
			note("replacing the trust policy of " + role)
			return runner.Run(ctx, update)
		},
	}
}

func vmPolicyStep(runner *Runner, vm AWSVM, grant api.AWSVMGrant, role string) Step {
	policy := AWSVMPolicy(vm, grant)
	put := Cmd(awsCLI, vm.args("iam", "put-role-policy", "--role-name", role, "--policy-name", grant.PolicyName,
		"--policy-document", policy)...)
	current := func(ctx context.Context) (bool, error) {
		var got struct {
			PolicyDocument json.RawMessage `json:"PolicyDocument"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, vm.args("iam", "get-role-policy", "--role-name", role, "--policy-name",
			grant.PolicyName)...), &got)
		if NotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return sameDocument(got.PolicyDocument, policy), nil
	}
	return Step{
		Title: "Allow " + role + " to manage the data volumes of " + vm.InstanceID + " only",
		Why: "The inline policy " + grant.PolicyName + " lets Zoorik list the account's instances, volumes and " +
			"snapshots (IAM cannot narrow EC2's describe calls), attach and detach volumes on this instance, create " +
			"volumes and snapshots only with the tag " + grant.VMTagKey + "=" + vm.InstanceID + ", change, snapshot " +
			"and delete only the volumes and snapshots that carry it, and use the KMS keys of encrypted volumes " +
			"through EC2.",
		Commands: []string{put.String()},
		Check:    current,
		Apply: func(ctx context.Context, note func(string)) error {
			done, err := current(ctx)
			if err != nil || done {
				return err
			}
			return runner.Run(ctx, put)
		},
	}
}

func vmRole(ctx context.Context, runner *Runner, vm AWSVM, name string) (iamRole, bool, error) {
	var role iamRole
	err := runner.JSON(ctx, Cmd(awsCLI, vm.args("iam", "get-role", "--role-name", name)...), &role)
	if NotFound(err) {
		return iamRole{}, false, nil
	}
	if err != nil {
		return iamRole{}, false, err
	}
	return role, true, nil
}

func dataVolumes(ctx context.Context, runner *Runner, vm AWSVM, key string) ([]string, []string, error) {
	var described struct {
		Reservations []struct {
			Instances []struct {
				RootDeviceName      string `json:"RootDeviceName"`
				BlockDeviceMappings []struct {
					DeviceName string `json:"DeviceName"`
					Ebs        struct {
						VolumeID string `json:"VolumeId"`
					} `json:"Ebs"`
				} `json:"BlockDeviceMappings"`
			} `json:"Instances"`
		} `json:"Reservations"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, vm.args("ec2", "describe-instances", "--instance-ids",
		vm.InstanceID)...), &described); err != nil {
		return nil, nil, err
	}
	var ids []string
	for _, reservation := range described.Reservations {
		for _, instance := range reservation.Instances {
			for _, mapping := range instance.BlockDeviceMappings {
				if mapping.DeviceName != instance.RootDeviceName && mapping.Ebs.VolumeID != "" {
					ids = append(ids, mapping.Ebs.VolumeID)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	var listed struct {
		Volumes []struct {
			VolumeID string `json:"VolumeId"`
			Tags     []struct {
				Key   string `json:"Key"`
				Value string `json:"Value"`
			} `json:"Tags"`
		} `json:"Volumes"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, vm.args(append([]string{"ec2", "describe-volumes", "--volume-ids"},
		ids...)...)...), &listed); err != nil {
		return nil, nil, err
	}
	var untagged, foreign []string
	for _, volume := range listed.Volumes {
		tagged, value := false, ""
		for _, tag := range volume.Tags {
			if tag.Key == key {
				tagged, value = true, tag.Value
			}
		}
		switch {
		case !tagged:
			untagged = append(untagged, volume.VolumeID)
		case value != vm.InstanceID:
			foreign = append(foreign, volume.VolumeID+" is tagged "+key+"="+value)
		}
	}
	return untagged, foreign, nil
}

func awsSignIn(arn, instance string) string {
	parts := strings.Split(arn, "/")
	if len(parts) == 3 && strings.HasSuffix(parts[0], ":assumed-role") && parts[2] == instance {
		return "this VM's instance role " + parts[1]
	}
	return arn
}
