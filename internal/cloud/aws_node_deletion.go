package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	podIdentityService = "pods.eks.amazonaws.com"

	webIdentityAction = "sts:AssumeRoleWithWebIdentity"

	inlinePolicyNameLimit = 128

	clusterARNSessionTag = "aws:RequestTag/eks-cluster-arn"

	namespaceSessionTag = "aws:RequestTag/kubernetes-namespace"

	serviceAccountSessionTag = "aws:RequestTag/kubernetes-service-account"

	sourceARNKey = "aws:SourceArn"

	sourceAccountKey = "aws:SourceAccount"
)

var clusterNameTags = []string{"eks:cluster-name", "aws:eks:cluster-name", "eks:eks-cluster-name"}

type iamRole struct {
	Role struct {
		Arn                      string          `json:"Arn"`
		AssumeRolePolicyDocument json.RawMessage `json:"AssumeRolePolicyDocument"`
		Tags                     []struct {
			Key   string `json:"Key"`
			Value string `json:"Value"`
		} `json:"Tags"`
	} `json:"Role"`
}

func (r iamRole) tag(key string) string {
	for _, tag := range r.Role.Tags {
		if strings.EqualFold(tag.Key, key) {
			return tag.Value
		}
	}
	return ""
}

type ownedRole struct {
	name   string
	role   iamRole
	exists bool
}

type podAssociation struct {
	ID                 string `json:"associationId"`
	RoleArn            string `json:"roleArn"`
	TargetRoleArn      string `json:"targetRoleArn"`
	DisableSessionTags bool   `json:"disableSessionTags"`
}

type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*l = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

func (l stringList) has(value string) bool {
	for _, item := range l {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

type trustStatement struct {
	Effect       string                           `json:"Effect"`
	Principal    json.RawMessage                  `json:"Principal"`
	NotPrincipal json.RawMessage                  `json:"NotPrincipal"`
	Action       stringList                       `json:"Action"`
	NotAction    json.RawMessage                  `json:"NotAction"`
	Condition    map[string]map[string]stringList `json:"Condition"`
}

func (s trustStatement) onlyPrincipal(kind, want string) bool {
	var named map[string]stringList
	if json.Unmarshal(s.Principal, &named) != nil || len(named) != 1 || len(named[kind]) == 0 {
		return false
	}
	for _, value := range named[kind] {
		if !strings.EqualFold(value, want) {
			return false
		}
	}
	return true
}

func (s trustStatement) onlyActions(allowed ...string) bool {
	if len(s.Action) == 0 || len(s.NotAction) > 0 || len(s.NotPrincipal) > 0 {
		return false
	}
	for _, action := range s.Action {
		if !stringList(allowed).has(action) {
			return false
		}
	}
	return true
}

func (s trustStatement) requires(operator, key, want string) bool {
	for name, conditions := range s.Condition {
		if name != operator {
			continue
		}
		for condition, values := range conditions {
			if strings.EqualFold(condition, key) && len(values) == 1 && values[0] == want {
				return true
			}
		}
	}
	return false
}

func nodeRemovalPolicy(cluster EKSCluster, roleName string) string {
	groups := "arn:" + cluster.Partition + ":autoscaling:" + cluster.Region + ":" + cluster.Account +
		":autoScalingGroup:*:autoScalingGroupName/*"
	nodegroups := "arn:" + cluster.Partition + ":eks:" + cluster.Region + ":" + cluster.Account + ":nodegroup/" +
		cluster.Name + "/*/*"
	role := "arn:" + cluster.Partition + ":iam::" + cluster.Account + ":role/" + roleName
	ownedGroup := map[string]any{"autoscaling:ResourceTag/kubernetes.io/cluster/" + cluster.Name: "owned"}
	namedGroup := map[string]any{"autoscaling:ResourceTag/eks:cluster-name": cluster.Name}
	minimumOnly := map[string]any{"autoscaling:MinSize": "false", "autoscaling:MaxSize": "true",
		"autoscaling:LaunchConfigurationName": "true", "autoscaling:LaunchTemplateVersionSpecified": "true",
		"autoscaling:VPCZoneIdentifiers": "true", "autoscaling:InstanceTypes": "true", "autoscaling:ImageId": "true",
		"autoscaling:TargetCapacityTypes": "true", "autoscaling:CapacityReservationIds": "true",
		"autoscaling:CapacityReservationResourceGroupArns": "true"}
	minimumTags := map[string]any{"aws:TagKeys": []string{originalMinimumTag, loweredMinimumTag}}
	tagKeysGiven := map[string]any{"aws:TagKeys": "false"}
	return compactJSON(map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Sid": "DescribeGroupsAndInstances", "Effect": "Allow",
			"Action": []string{"autoscaling:DescribeAutoScalingInstances", "autoscaling:DescribeAutoScalingGroups",
				"ec2:DescribeInstances"},
			"Resource": "*"},
		map[string]any{"Sid": "ConfirmOwnGrant", "Effect": "Allow",
			"Action": "iam:SimulatePrincipalPolicy", "Resource": role},
		map[string]any{"Sid": "TerminateInThisClustersGroups", "Effect": "Allow",
			"Action": "autoscaling:TerminateInstanceInAutoScalingGroup", "Resource": groups,
			"Condition": map[string]any{"StringEquals": ownedGroup}},
		map[string]any{"Sid": "TerminateInThisClustersNodeGroups", "Effect": "Allow",
			"Action": "autoscaling:TerminateInstanceInAutoScalingGroup", "Resource": groups,
			"Condition": map[string]any{"StringEquals": namedGroup}},
		map[string]any{"Sid": "LowerMinimumInThisClustersGroups", "Effect": "Allow",
			"Action": "autoscaling:UpdateAutoScalingGroup", "Resource": groups,
			"Condition": map[string]any{"StringEquals": ownedGroup, "Null": minimumOnly}},
		map[string]any{"Sid": "LowerMinimumInThisClustersNodeGroups", "Effect": "Allow",
			"Action": "autoscaling:UpdateAutoScalingGroup", "Resource": groups,
			"Condition": map[string]any{"StringEquals": namedGroup, "Null": minimumOnly}},
		map[string]any{"Sid": "RecordMinimumOnThisClustersGroups", "Effect": "Allow",
			"Action": "autoscaling:CreateOrUpdateTags", "Resource": groups,
			"Condition": map[string]any{"StringEquals": ownedGroup, "ForAllValues:StringEquals": minimumTags,
				"Null": tagKeysGiven}},
		map[string]any{"Sid": "RecordMinimumOnThisClustersNodeGroupsByName", "Effect": "Allow",
			"Action": "autoscaling:CreateOrUpdateTags", "Resource": groups,
			"Condition": map[string]any{"StringEquals": namedGroup, "ForAllValues:StringEquals": minimumTags,
				"Null": tagKeysGiven}},
		map[string]any{"Sid": "LowerMinimumOfThisClustersManagedNodeGroups", "Effect": "Allow",
			"Action": []string{"eks:DescribeNodegroup", "eks:UpdateNodegroupConfig"}, "Resource": nodegroups},
		map[string]any{"Sid": "RecordMinimumOnThisClustersManagedNodeGroups", "Effect": "Allow",
			"Action": "eks:TagResource", "Resource": nodegroups,
			"Condition": map[string]any{"ForAllValues:StringEquals": minimumTags, "Null": tagKeysGiven}},
	}})
}

func nodeRemovalPolicyOf(cluster EKSCluster) string {
	return boundedName(nodeRemovalPolicyName+"-", cluster.Name+"-"+shortHash(cluster.ARN()), inlinePolicyNameLimit)
}

func agentPodIdentityTrust(cluster EKSCluster) string {
	return compactJSON(map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect":    "Allow",
		"Principal": map[string]any{"Service": podIdentityService},
		"Action":    []string{"sts:AssumeRole", "sts:TagSession"},
		"Condition": map[string]any{"StringEquals": map[string]any{
			sourceAccountKey:         cluster.Account,
			clusterARNSessionTag:     cluster.ARN(),
			namespaceSessionTag:      kube.AgentNamespace,
			serviceAccountSessionTag: kube.AgentServiceAccount}}}}})
}

func oidcProviderARN(cluster EKSCluster, issuerHostPath string) string {
	return "arn:" + cluster.Partition + ":iam::" + cluster.Account + ":oidc-provider/" + issuerHostPath
}

func agentRoleNames(cluster EKSCluster) []string {
	return []string{
		boundedName(agentIdentityPrefix, cluster.Name, iamNameLimit),
		boundedName(agentIdentityPrefix, cluster.Name+"-"+cluster.Region, iamNameLimit),
	}
}

func PlanNodeDeletionEKS(ctx context.Context, runner *Runner, input Input) NodeDeletion {
	plan := Plan{Cloud: kube.CloudAWS, Provisioner: NodeDeletionTitle, Credential: "your AWS CLI credentials (aws)"}
	cluster, described, ok := signInEKS(ctx, runner, &plan, input, eksNodeDeletion)
	if !ok {
		return NodeDeletion{Plan: plan}
	}
	info := described.Cluster
	autoMode := described.autoModeOn()
	hostPath := strings.TrimPrefix(info.Identity.OIDC.Issuer, "https://")
	provider := oidcProviderARN(cluster, hostPath)
	plan.Found = append(plan.Found, "EKS "+text.OrText(info.Version, "version unknown")+", status "+
		text.OrText(info.Status, "unknown")+", OIDC issuer "+text.OrText(info.Identity.OIDC.Issuer, "none"))
	if autoMode {
		plan.Found = append(plan.Found, "EKS Auto Mode is on; it runs the Pod Identity Agent itself")
	}
	agent := kube.AgentNamespace + "/" + kube.AgentServiceAccount
	policy := nodeRemovalPolicyOf(cluster)

	via, roleARN, reused := "", "", false
	if name, ok := roleOfAccount(input.Occupant, cluster.Account); ok && hostPath != "" {
		if role, exists, err := getRole(ctx, runner, cluster, name); err == nil && exists {
			irsa, pods := agentTrust(role, cluster, provider, hostPath)
			switch {
			case irsa:
				via, roleARN, reused = ViaIRSA, role.Role.Arn, true
				plan.Found = append(plan.Found, "Role "+roleARN+" already trusts "+agent+" through this cluster's IAM "+
					"OIDC provider, and nothing else")
			case !pods:
				plan.Found = append(plan.Found, "The agent's role "+role.Role.Arn+" may be assumed by more than "+agent+
					" on this cluster, so node removal is not added to it")
			}
		}
	}
	var association *podAssociation
	if via == "" {
		found, err := agentAssociation(ctx, runner, cluster)
		if err != nil {
			plan.block("AWS would not read the EKS Pod Identity association of "+agent+": "+err.Error(),
				"Check that your credentials may list and describe the pod identity associations of "+cluster.ARN()+
					" (eks:ListPodIdentityAssociations, eks:DescribePodIdentityAssociation), and run this again. "+
					"Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
		association = found
	}
	if association != nil {
		usable, why, err := associationUsable(ctx, runner, cluster, *association, provider, hostPath)
		if err != nil {
			plan.block("AWS would not read the role "+association.RoleArn+": "+err.Error(),
				"Check that your credentials may read IAM roles in account "+cluster.Account+", and run this again. "+
					"Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
		if usable {
			via, roleARN, reused = ViaPodIdentity, association.RoleArn, true
			plan.Found = append(plan.Found, "EKS Pod Identity already gives "+agent+" the role "+roleARN+", which "+
				"only "+agent+" on this cluster may assume")
		} else {
			plan.Found = append(plan.Found, "EKS Pod Identity gives "+agent+" the role "+association.RoleArn+", but "+
				why+"; the agent gets a role of its own, and the association is pointed at it")
		}
	}
	ownExists := false
	if via == "" {
		own, passed, err := agentOwnRole(ctx, runner, cluster)
		plan.Found = append(plan.Found, passed...)
		if err != nil {
			plan.block("AWS would not read the role "+strings.Join(agentRoleNames(cluster), " or ")+": "+err.Error(),
				"Check that your credentials may read IAM roles in account "+cluster.Account+", and run this again. "+
					"Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
		if own.name == "" {
			names := strings.Join(agentRoleNames(cluster), " and ")
			plan.block("The roles "+names+" both exist and neither is tagged "+clusterTag+"="+
				clusterTagValue(cluster.ARN())+", so zoorikctl will not use either for this cluster.",
				"Check what they are for, then delete or rename one of them, and run this again. Nothing has been "+
					"changed.")
			return NodeDeletion{Plan: plan}
		}
		ownExists = own.exists
		roleARN = "arn:" + cluster.Partition + ":iam::" + cluster.Account + ":role/" + own.name
		if own.exists && own.role.Role.Arn != "" {
			roleARN = own.role.Role.Arn
		}
		switch {
		case association != nil:
			via = ViaPodIdentity
		case own.exists:
			if _, pods := agentTrust(own.role, cluster, provider, hostPath); pods {
				via = ViaPodIdentity
			}
		}
		if via == "" {
			if !autoMode && !addonPresent(ctx, runner, cluster) && !addonAvailable(ctx, runner, cluster, info.Version) {
				plan.block("The Pod Identity Agent add-on is unavailable for this cluster (Kubernetes "+
					text.OrText(info.Version, "unknown")+"), so the agent has no way to receive a role.",
					"Upgrade the cluster to a Kubernetes version EKS offers the eks-pod-identity-agent add-on for "+
						"and run this again, or create an IAM role for IRSA yourself and connect with "+
						"--cloud-identity-client-id naming it. Nothing has been changed.")
				return NodeDeletion{Plan: plan}
			}
			via = ViaPodIdentity
		}
	}
	boundName := roleNameOf(roleARN)

	plan.note("Zoorik never launches a machine itself. The role below may describe Auto Scaling groups and this " +
		"cluster's node groups, terminate an instance in a group tagged for this cluster (always lowering its " +
		"desired capacity), and update such a group or one of this cluster's managed node groups. AWS has no " +
		"narrower permission for setting a minimum, so the role may also set an Auto Scaling group's desired " +
		"capacity and health check, termination, instance protection and lifetime settings, and a managed node " +
		"group's maximum, desired size, labels and taints, which could add machines. It can never set an Auto " +
		"Scaling group's maximum, launch template or configuration, image, subnets, instance types or capacity " +
		"reservations. The agent only ever lowers a minimum, only for a node it cordoned and emptied itself, and " +
		"disconnect puts every minimum it lowered back.")

	if via == ViaPodIdentity && !autoMode {
		plan.Steps = append(plan.Steps, podIdentityAddonStep(runner, cluster,
			"It hands the agent its role. It is an AWS-managed DaemonSet in kube-system."))
	}
	if !reused {
		plan.Steps = append(plan.Steps, agentRoleStep(runner, cluster, boundName, ownExists))
	}
	policyStep := nodeRemovalPolicyStep(runner, cluster, boundName, policy)
	var earlier string
	if reused || ownExists {
		held, err := rolePolicyPresent(ctx, runner, cluster, boundName, policy)
		if err != nil {
			plan.block("AWS would not read the inline policy "+policy+" of "+boundName+": "+err.Error(),
				"Check that your credentials may read IAM role policies in account "+cluster.Account+", and run "+
					"this again. Nothing has been changed.")
			return NodeDeletion{Plan: plan}
		}
		if held {
			policyStep.Upgrade = true
			earlier = "an earlier version of the inline policy " + policy + " on " + boundName
		}
	}
	plan.Steps = append(plan.Steps, policyStep)
	if via == ViaPodIdentity {
		plan.Steps = append(plan.Steps, agentAssociationStep(runner, cluster, roleARN, boundName, association))
	}

	return NodeDeletion{Plan: plan, Earlier: earlier, Bind: func(ctx context.Context) (Binding, error) {
		role, exists, err := getRole(ctx, runner, cluster, boundName)
		if err != nil {
			return Binding{}, err
		}
		if !exists || role.Role.Arn == "" {
			return Binding{}, fmt.Errorf("the role %s was not found", boundName)
		}
		return Binding{Identity: role.Role.Arn, Via: via, Annotate: via == ViaIRSA}, nil
	}}
}

func PlanNodeDeletionCleanupEKS(ctx context.Context, runner *Runner, input Input) Plan {
	plan := Plan{Cloud: kube.CloudAWS, Provisioner: NodeDeletionTitle, Credential: "your AWS CLI credentials (aws)"}
	cluster, _, ok := signInEKS(ctx, runner, &plan, input, eksRemoval)
	if !ok {
		return plan
	}
	planMinimumRestoreEKS(ctx, runner, &plan, cluster)
	policy := nodeRemovalPolicyOf(cluster)
	own, _, err := agentOwnRole(ctx, runner, cluster)
	if err != nil {
		plan.block("AWS would not read the role "+strings.Join(agentRoleNames(cluster), " or ")+": "+err.Error(),
			"Check that your credentials may read IAM roles in account "+cluster.Account+", and run this disconnect "+
				"again. Nothing has been changed.")
		return plan
	}
	association, err := agentAssociation(ctx, runner, cluster)
	if err != nil {
		plan.block("AWS would not read the EKS Pod Identity association of "+kube.AgentNamespace+"/"+
			kube.AgentServiceAccount+": "+err.Error(),
			"Check that your credentials may list and describe the pod identity associations of "+cluster.ARN()+
				", and run this disconnect again. Nothing has been changed.")
		return plan
	}

	var holders []string
	if own.exists {
		holders = append(holders, own.name)
		plan.Found = append(plan.Found, "Role "+own.role.Role.Arn+", tagged "+clusterTag+"="+
			clusterTagValue(cluster.ARN()))
	} else {
		plan.Found = append(plan.Found, "No role "+strings.Join(agentRoleNames(cluster), " or ")+" tagged for this "+
			"cluster")
	}
	var others []string
	if name, ok := roleOfAccount(input.Occupant, cluster.Account); ok {
		others = append(others, name)
	}
	if association != nil {
		if name, ok := roleOfAccount(association.RoleArn, cluster.Account); ok {
			others = append(others, name)
		}
	}
	for _, name := range others {
		if stringList(holders).has(name) {
			continue
		}
		if present, err := hasInlinePolicy(ctx, runner, cluster, name, policy); err == nil && !present {
			continue
		}
		holders = append(holders, name)
		plan.Found = append(plan.Found, "Role "+name+", which the agent used, may hold the inline policy "+policy)
	}
	for _, name := range holders {
		plan.Steps = append(plan.Steps, policyRemovalStep(runner, cluster, name, policy))
	}
	if own.exists {
		plan.Steps = append(plan.Steps, ownRoleRemoval(ctx, runner, cluster, own)...)
	}
	plan.note("The EKS Pod Identity Agent add-on and the cluster's IAM OIDC provider stay; other workloads may use " +
		"them. A role zoorikctl did not create keeps everything else it holds.")
	return plan
}

func policyRemovalStep(runner *Runner, cluster EKSCluster, roleName, policy string) Step {
	deletePolicy := Cmd(awsCLI, awsArgs(cluster, "iam", "delete-role-policy", "--role-name", roleName,
		"--policy-name", policy)...)
	policyGone := func(ctx context.Context) (bool, error) {
		present, err := hasInlinePolicy(ctx, runner, cluster, roleName, policy)
		return err == nil && !present, err
	}
	return Step{
		Title:    "Remove the inline policy " + policy + " from " + roleName,
		Why:      "It let the role terminate instances in this cluster's Auto Scaling groups and lower their minimums.",
		Commands: []string{deletePolicy.String()},
		Check:    policyGone,
		Apply: func(ctx context.Context, note func(string)) error {
			gone, err := policyGone(ctx)
			if err != nil || gone {
				return err
			}
			return runner.Run(ctx, deletePolicy)
		},
	}
}

func ownRoleRemoval(ctx context.Context, runner *Runner, cluster EKSCluster, own ownedRole) []Step {
	associations := func(ctx context.Context) ([]string, error) {
		return associationsOf(ctx, runner, cluster, own.role.Role.Arn)
	}
	dissociate := func(id string) Command {
		return Cmd(awsCLI, awsArgs(cluster, "eks", "delete-pod-identity-association", "--cluster-name",
			cluster.Name, "--association-id", id)...)
	}
	idsAtPlan, err := associations(ctx)
	if err != nil || len(idsAtPlan) == 0 {
		idsAtPlan = []string{"<association id>"}
	}
	dissociations := make([]string, 0, len(idsAtPlan))
	for _, id := range idsAtPlan {
		dissociations = append(dissociations, dissociate(id).String())
	}
	deleteRole := Cmd(awsCLI, awsArgs(cluster, "iam", "delete-role", "--role-name", own.name)...)
	roleGone := func(ctx context.Context) (bool, error) {
		_, exists, err := getRole(ctx, runner, cluster, own.name)
		return err == nil && !exists, err
	}
	return []Step{
		{
			Title:    "Remove the pod identity association of " + kube.AgentNamespace + "/" + kube.AgentServiceAccount,
			Why:      "It handed " + own.name + " to the agent's service account on this cluster.",
			Commands: dissociations,
			Check: func(ctx context.Context) (bool, error) {
				ids, err := associations(ctx)
				return err == nil && len(ids) == 0, err
			},
			Apply: func(ctx context.Context, note func(string)) error {
				ids, err := associations(ctx)
				if err != nil {
					return err
				}
				for _, id := range ids {
					if err := runner.Run(ctx, dissociate(id)); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			Title: "Delete the role " + own.name,
			Why: "zoorikctl created it for this cluster, tagged " + clusterTag + "=" + clusterTagValue(cluster.ARN()) +
				".",
			Commands: []string{deleteRole.String()},
			Check:    roleGone,
			Apply: func(ctx context.Context, note func(string)) error {
				gone, err := roleGone(ctx)
				if err != nil || gone {
					return err
				}
				return runner.Run(ctx, deleteRole)
			},
		},
	}
}

func signInEKS(ctx context.Context, runner *Runner, plan *Plan, input Input, p purpose) (EKSCluster,
	eksDescription, bool) {
	if !runner.Available(awsCLI) {
		plan.blockTooling("`aws` is not installed on this machine, so "+p.aim+" from here.",
			"Install the AWS CLI v2, sign in as someone allowed to "+p.allowed+", and "+again(p.removal)+
				". Nothing has been changed.")
		return EKSCluster{}, eksDescription{}, false
	}
	cluster, err := resolveEKSCluster(ctx, runner, input)
	if err != nil && input.Resource == nil && input.Kept != nil {
		if kept, keptErr := eksClusterOf(*input.Kept); keptErr == nil {
			plan.Found = append(plan.Found, "The nodes did not lead to the EKS cluster ("+text.OneLine(err.Error())+
				"), so the cluster resource the agent holds is used")
			cluster, err = kept, nil
		}
	}
	if err != nil {
		plan.block(err.Error(), unresolvedEKS(err, p.removal))
		return EKSCluster{}, eksDescription{}, false
	}
	described, ok := describeEKSCluster(ctx, runner, plan, cluster, p)
	if !ok {
		return EKSCluster{}, eksDescription{}, false
	}
	if resource, parsed := kube.ParseClusterResource(described.Cluster.Arn); parsed {
		if exact, err := eksClusterOf(resource); err == nil {
			cluster = exact
			plan.Cluster = cluster.ARN()
		}
	}
	if mismatch := eksTargetMismatch(ctx, runner, input.Target, cluster, described); mismatch != "" {
		plan.block(mismatch, wrongCluster(p.removal))
		return EKSCluster{}, eksDescription{}, false
	}
	return cluster, described, true
}

func unresolvedEKS(err error, removal bool) string {
	var failed *CommandError
	switch {
	case errors.As(err, &failed):
		return "Check that your AWS CLI credentials are current (`aws sso login`, or AWS_PROFILE) and may list the " +
			"EKS clusters of the region, and " + again(removal) + ". Nothing has been changed."
	case removal:
		return "zoorikctl could not tell from the nodes and the API server which EKS cluster this is. Nothing has " +
			"been changed."
	}
	return "Pass --cluster-resource-id with the EKS cluster's ARN (arn:aws:eks:<region>:<account>:cluster/<name>). " +
		"Nothing has been changed."
}

func eksTargetMismatch(ctx context.Context, runner *Runner, target *kube.Target, cluster EKSCluster,
	described eksDescription) string {
	endpoint := described.Cluster.Endpoint
	if endpoint != "" && serverHost(endpoint) == serverHost(target.ServerURL) {
		return ""
	}
	instance, err := kube.AWSInstanceID(ctx, target)
	if err == nil && instance != "" && instanceOfCluster(ctx, runner, cluster, instance) {
		return ""
	}
	return fmt.Sprintf("The EKS cluster %s is not the cluster %s reaches: it answers at %s, %s answers at %s, and "+
		"no node of %s was found to be an instance of %s.", cluster.ARN(), target.ContextName,
		text.OrText(serverHost(endpoint), "an unknown address"), target.ContextName, serverHost(target.ServerURL),
		target.ContextName, cluster.Name)
}

func instanceOfCluster(ctx context.Context, runner *Runner, cluster EKSCluster, instance string) bool {
	var described struct {
		Reservations []struct {
			Instances []struct {
				Tags []struct {
					Key   string `json:"Key"`
					Value string `json:"Value"`
				} `json:"Tags"`
			} `json:"Instances"`
		} `json:"Reservations"`
	}
	if runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "ec2", "describe-instances", "--instance-ids", instance)...),
		&described) != nil {
		return false
	}
	for _, reservation := range described.Reservations {
		for _, found := range reservation.Instances {
			for _, tag := range found.Tags {
				if tag.Key == "kubernetes.io/cluster/"+cluster.Name {
					return true
				}
				if stringList(clusterNameTags).has(tag.Key) && tag.Value == cluster.Name {
					return true
				}
			}
		}
	}
	return false
}

func agentOwnRole(ctx context.Context, runner *Runner, cluster EKSCluster) (ownedRole, []string, error) {
	var passed []string
	free := ""
	for _, name := range agentRoleNames(cluster) {
		role, exists, err := getRole(ctx, runner, cluster, name)
		if err != nil {
			return ownedRole{}, passed, err
		}
		switch {
		case !exists:
			if free == "" {
				free = name
			}
		case taggedFor(role.tag(clusterTag), cluster.ARN()):
			return ownedRole{name: name, role: role, exists: true}, passed, nil
		default:
			owner := "without the tag " + clusterTag
			if tag := role.tag(clusterTag); tag != "" {
				owner = "for another cluster (" + clusterTag + "=" + tag + ")"
			}
			passed = append(passed, "The role "+name+" exists "+owner+", so zoorikctl leaves it alone")
		}
	}
	return ownedRole{name: free}, passed, nil
}

func associationUsable(ctx context.Context, runner *Runner, cluster EKSCluster, association podAssociation,
	provider, hostPath string) (bool, string, error) {
	if association.TargetRoleArn != "" {
		return false, "it hands the agent the role " + association.TargetRoleArn + " in another account", nil
	}
	name, ok := roleOfAccount(association.RoleArn, cluster.Account)
	if !ok {
		return false, "that role is not in account " + cluster.Account, nil
	}
	role, exists, err := getRole(ctx, runner, cluster, name)
	if err != nil {
		return false, "", err
	}
	if !exists {
		return false, "that role no longer exists", nil
	}
	if _, pods := agentTrust(role, cluster, provider, hostPath); !pods {
		return false, "its trust lets more than " + kube.AgentNamespace + "/" + kube.AgentServiceAccount +
			" on this cluster assume it", nil
	}
	return true, "", nil
}

func agentRoleStep(runner *Runner, cluster EKSCluster, roleName string, exists bool) Step {
	agent := kube.AgentNamespace + "/" + kube.AgentServiceAccount
	trust := agentPodIdentityTrust(cluster)
	why := "The role the agent removes drained nodes with. Only EKS Pod Identity may assume it, and only for " +
		agent + " on this cluster: its trust checks the session tags EKS sets for the cluster, namespace and " +
		"service account."
	tag := clusterTagValue(cluster.ARN())
	create := Cmd(awsCLI, awsArgs(cluster, "iam", "create-role", "--role-name", roleName,
		"--assume-role-policy-document", trust, "--tags", "Key="+clusterTag+",Value="+tag)...)
	update := Cmd(awsCLI, awsArgs(cluster, "iam", "update-assume-role-policy", "--role-name", roleName,
		"--policy-document", trust)...)
	state := func(ctx context.Context) (bool, bool, error) {
		role, present, err := getRole(ctx, runner, cluster, roleName)
		if err != nil || !present {
			return present, false, err
		}
		return true, sameDocument(role.Role.AssumeRolePolicyDocument, trust), nil
	}
	title, command := "Create the role "+roleName, create
	if exists {
		title, command = "Let only "+agent+" on "+cluster.Name+" assume the role "+roleName, update
	}
	return Step{
		Title:    title,
		Why:      why + " The tag " + clusterTag + "=" + tag + " lets disconnect find it.",
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
			note("replacing the trust policy of " + roleName)
			return runner.Run(ctx, update)
		},
	}
}

func rolePolicyPresent(ctx context.Context, runner *Runner, cluster EKSCluster, roleName, policyName string) (
	bool, error) {
	var got struct {
		PolicyDocument json.RawMessage `json:"PolicyDocument"`
	}
	err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role-policy", "--role-name", roleName,
		"--policy-name", policyName)...), &got)
	if NotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func nodeRemovalPolicyStep(runner *Runner, cluster EKSCluster, roleName, policyName string) Step {
	policy := nodeRemovalPolicy(cluster, roleName)
	put := Cmd(awsCLI, awsArgs(cluster, "iam", "put-role-policy", "--role-name", roleName, "--policy-name",
		policyName, "--policy-document", policy)...)
	current := func(ctx context.Context) (bool, error) {
		var got struct {
			PolicyDocument json.RawMessage `json:"PolicyDocument"`
		}
		err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role-policy", "--role-name", roleName,
			"--policy-name", policyName)...), &got)
		if NotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return sameDocument(got.PolicyDocument, policy), nil
	}
	return Step{
		Title: "Allow " + roleName + " to remove drained nodes and lower node group minimums of " + cluster.Name +
			" only",
		Why: "The inline policy " + policyName + " lets it describe Auto Scaling groups and EC2 instances, " +
			"confirm its own grant with a policy simulation, and terminate an instance " +
			"only in a group of this region tagged kubernetes.io/cluster/" + cluster.Name + "=owned or " +
			"eks:cluster-name=" + cluster.Name + ". On such a group it may also update the minimum size; IAM has no " +
			"narrower key, so that request may also carry the desired capacity and other group settings, but IAM " +
			"refuses one that carries a maximum, a launch configuration or template, an image, subnets, instance " +
			"types or capacity reservations. On this cluster's managed node groups it may describe them and update " +
			"their configuration (minimum, maximum, desired size, labels and taints), which AWS cannot narrow " +
			"further; the agent sends only a lower minSize. It may write " +
			"only the tags " + originalMinimumTag + " and " + loweredMinimumTag + ", which record the minimum " +
			"before Zoorik lowered it so disconnect can put it back. It cannot launch or delete a group, and " +
			"the agent never raises a size. IAM cannot require ShouldDecrementDesiredCapacity; the agent always " +
			"sends it as true. The policy is named for this cluster, so it never replaces another cluster's.",
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

func agentAssociationStep(runner *Runner, cluster EKSCluster, roleARN, roleName string,
	existing *podAssociation) Step {
	agent := kube.AgentNamespace + "/" + kube.AgentServiceAccount
	create := Cmd(awsCLI, awsArgs(cluster, "eks", "create-pod-identity-association", "--cluster-name", cluster.Name,
		"--namespace", kube.AgentNamespace, "--service-account", kube.AgentServiceAccount, "--role-arn", roleARN)...)
	update := func(association podAssociation) Command {
		args := []string{"eks", "update-pod-identity-association", "--cluster-name", cluster.Name,
			"--association-id", association.ID, "--role-arn", roleARN}
		if association.DisableSessionTags {
			args = append(args, "--no-disable-session-tags")
		}
		return Cmd(awsCLI, awsArgs(cluster, args...)...)
	}
	title, command := "Give "+agent+" the role "+roleName, create
	if existing != nil {
		title, command = "Point the pod identity association of "+agent+" at "+roleName, update(*existing)
	}
	bound := func(association *podAssociation) bool {
		return association != nil && strings.EqualFold(association.RoleArn, roleARN) &&
			!association.DisableSessionTags && association.TargetRoleArn == ""
	}
	return Step{
		Title: title,
		Why: "An EKS Pod Identity association: only that service account on this cluster receives the role's " +
			"credentials, with the session tags the role's trust checks.",
		Commands: []string{command.String()},
		Check: func(ctx context.Context) (bool, error) {
			association, err := agentAssociation(ctx, runner, cluster)
			return bound(association), err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			association, err := agentAssociation(ctx, runner, cluster)
			switch {
			case err != nil:
				return err
			case association == nil:
				return runner.Run(ctx, create)
			case bound(association):
				return nil
			case association.TargetRoleArn != "":
				return fmt.Errorf("the pod identity association %s also hands the agent the role %s in another "+
					"account; remove its target role and run this again", association.ID, association.TargetRoleArn)
			}
			note("pointing the association " + association.ID + " at " + roleName)
			return runner.Run(ctx, update(*association))
		},
	}
}

func getRole(ctx context.Context, runner *Runner, cluster EKSCluster, name string) (iamRole, bool, error) {
	var role iamRole
	err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role", "--role-name", name)...), &role)
	if NotFound(err) {
		return iamRole{}, false, nil
	}
	if err != nil {
		return iamRole{}, false, err
	}
	return role, true, nil
}

func hasInlinePolicy(ctx context.Context, runner *Runner, cluster EKSCluster, roleName, policy string) (bool,
	error) {
	err := runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "iam", "get-role-policy", "--role-name", roleName,
		"--policy-name", policy)...))
	if NotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func agentAssociations(ctx context.Context, runner *Runner, cluster EKSCluster) ([]string, error) {
	var listed struct {
		Associations []struct {
			AssociationID string `json:"associationId"`
		} `json:"associations"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "list-pod-identity-associations",
		"--cluster-name", cluster.Name, "--namespace", kube.AgentNamespace, "--service-account",
		kube.AgentServiceAccount)...), &listed); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listed.Associations))
	for _, association := range listed.Associations {
		ids = append(ids, association.AssociationID)
	}
	return ids, nil
}

func describeAssociation(ctx context.Context, runner *Runner, cluster EKSCluster, id string) (*podAssociation,
	error) {
	var described struct {
		Association podAssociation `json:"association"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-pod-identity-association",
		"--cluster-name", cluster.Name, "--association-id", id)...), &described); err != nil {
		return nil, err
	}
	if described.Association.ID == "" {
		described.Association.ID = id
	}
	return &described.Association, nil
}

func agentAssociation(ctx context.Context, runner *Runner, cluster EKSCluster) (*podAssociation, error) {
	ids, err := agentAssociations(ctx, runner, cluster)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return describeAssociation(ctx, runner, cluster, ids[0])
}

func associationsOf(ctx context.Context, runner *Runner, cluster EKSCluster, roleARN string) ([]string, error) {
	ids, err := agentAssociations(ctx, runner, cluster)
	if err != nil {
		return nil, err
	}
	var matched []string
	for _, id := range ids {
		association, err := describeAssociation(ctx, runner, cluster, id)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(association.RoleArn, roleARN) {
			matched = append(matched, id)
		}
	}
	return matched, nil
}

func addonPresent(ctx context.Context, runner *Runner, cluster EKSCluster) bool {
	return runner.Run(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-addon", "--cluster-name", cluster.Name,
		"--addon-name", podIdentityAddon)...)) == nil
}

func addonAvailable(ctx context.Context, runner *Runner, cluster EKSCluster, version string) bool {
	if version == "" {
		return false
	}
	var listed struct {
		Addons []struct {
			AddonVersions []json.RawMessage `json:"addonVersions"`
		} `json:"addons"`
	}
	if runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-addon-versions", "--addon-name",
		podIdentityAddon, "--kubernetes-version", version)...), &listed) != nil {
		return false
	}
	for _, addon := range listed.Addons {
		if len(addon.AddonVersions) > 0 {
			return true
		}
	}
	return false
}

func roleOfAccount(arn, account string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(arn), ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "iam" || parts[4] != account ||
		!strings.HasPrefix(parts[5], "role/") {
		return "", false
	}
	return roleNameOf(arn), true
}

func roleNameOf(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

func trustStatements(document json.RawMessage) []trustStatement {
	var parsed struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if json.Unmarshal(decodedDocument(document), &parsed) != nil {
		return nil
	}
	var many []trustStatement
	if json.Unmarshal(parsed.Statement, &many) == nil {
		return many
	}
	var one trustStatement
	if json.Unmarshal(parsed.Statement, &one) == nil {
		return []trustStatement{one}
	}
	return nil
}

func agentTrust(role iamRole, cluster EKSCluster, provider, hostPath string) (bool, bool) {
	irsa, pods := false, false
	for _, statement := range trustStatements(role.Role.AssumeRolePolicyDocument) {
		if !strings.EqualFold(statement.Effect, "Allow") {
			continue
		}
		switch {
		case irsaStatement(statement, provider, hostPath):
			irsa = true
		case podIdentityStatement(statement, cluster):
			pods = true
		default:
			return false, false
		}
	}
	return irsa, pods
}

func irsaStatement(statement trustStatement, provider, hostPath string) bool {
	return hostPath != "" && statement.onlyPrincipal("Federated", provider) &&
		statement.onlyActions(webIdentityAction) &&
		statement.requires("StringEquals", hostPath+":sub", AgentSubject()) &&
		statement.requires("StringEquals", hostPath+":aud", stsAudience)
}

func podIdentityStatement(statement trustStatement, cluster EKSCluster) bool {
	boundToCluster := statement.requires("StringEquals", clusterARNSessionTag, cluster.ARN()) ||
		statement.requires("ArnEquals", sourceARNKey, cluster.ARN()) ||
		statement.requires("StringEquals", sourceARNKey, cluster.ARN())
	return boundToCluster && statement.onlyPrincipal("Service", podIdentityService) &&
		statement.onlyActions("sts:AssumeRole", "sts:TagSession") && statement.Action.has("sts:AssumeRole") &&
		statement.requires("StringEquals", namespaceSessionTag, kube.AgentNamespace) &&
		statement.requires("StringEquals", serviceAccountSessionTag, kube.AgentServiceAccount)
}

func decodedDocument(document json.RawMessage) []byte {
	var encoded string
	if json.Unmarshal(document, &encoded) != nil {
		return document
	}
	if decoded, err := url.QueryUnescape(encoded); err == nil {
		return []byte(decoded)
	}
	return []byte(encoded)
}

func sameDocument(document json.RawMessage, want string) bool {
	var got, expected any
	if json.Unmarshal(decodedDocument(document), &got) != nil || json.Unmarshal([]byte(want), &expected) != nil {
		return false
	}
	return reflect.DeepEqual(got, expected)
}
