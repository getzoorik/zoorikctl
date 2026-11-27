package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
)

func switchableAutoscalers(ctx context.Context, target *kube.Target, status string) ([]kube.AutoscalerDeployment,
	string, error) {
	if !kube.AdmissionPoliciesServed(target) {
		return nil, "This cluster does not serve ValidatingAdmissionPolicy (admissionregistration.k8s.io/v1, " +
			"Kubernetes 1.30 or later), which keeps the cluster autoscaler from coming back beside Karpenter.", nil
	}
	deployments, err := kube.ClusterAutoscalerDeployments(ctx, target)
	if err != nil {
		return nil, "", err
	}
	if len(deployments) == 0 {
		namespace, _, _ := strings.Cut(status, "/")
		return nil, "No Deployment in the cluster runs the cluster autoscaler's image (" + kube.ClusterAutoscalerImage +
			", from any registry), so zoorikctl cannot stop it. If it is no longer installed, delete the ConfigMap " +
			status + " it left (`" + target.Kubectl("-n "+namespace+" delete configmap "+
			kube.ClusterAutoscalerStatus) + "`) and run " + prepareCommand + " again.", nil
	}
	return deployments, "", nil
}

func blockRefused(plan *Plan, err error) {
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		plan.block(refused.What, refused.Next)
		return
	}
	plan.block(err.Error(), "Run "+prepareCommand+" again. Nothing has been changed.")
}

func autoscalerFound(deployments []kube.AutoscalerDeployment, groups []string) []string {
	managed := ""
	if len(groups) > 0 {
		managed = ", manages node group(s) " + strings.Join(groups, ", ")
	}
	lines := make([]string, 0, len(deployments))
	for _, deployment := range deployments {
		lines = append(lines, fmt.Sprintf("Cluster autoscaler %s/%s: %d replica(s)%s", deployment.Namespace,
			deployment.Name, deployment.Replicas, managed))
	}
	return lines
}

func autoscalerSwitchSteps(target *kube.Target, deployments []kube.AutoscalerDeployment,
	statusNamespace string) []Step {
	steps := []Step{autoscalerPolicyStep(target)}
	for _, deployment := range deployments {
		if deployment.Replicas > 0 || deployment.Recorded != "" {
			steps = append(steps, autoscalerStopStep(target, deployment))
		}
	}
	if statusNamespace != "" {
		steps = append(steps, autoscalerStatusStep(target, statusNamespace))
	}
	return steps
}

func autoscalerPolicyStep(target *kube.Target) Step {
	return Step{
		Title: "Keep the cluster autoscaler from coming back",
		Why: "The ValidatingAdmissionPolicy " + kube.AutoscalerPolicyName + " and its binding refuse a Deployment " +
			"that runs the cluster autoscaler's image (any registry and tag) with more than 0 replicas, in any " +
			"namespace. A Terraform, Helm or GitOps apply that brings it back fails with a message that says why, " +
			"instead of running two autoscalers. Scaling it to 0 or deleting it stays allowed, and zoorikctl " +
			"cluster disconnect removes the policy.",
		Commands: []string{target.Kubectl("apply -f - <<'EOF'"), kube.AutoscalerPolicyManifest(), "EOF"},
		Check: func(ctx context.Context) (bool, error) {
			return kube.AutoscalerPolicyInPlace(ctx, target)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			return kube.ApplyAutoscalerPolicy(ctx, target)
		},
	}
}

func autoscalerStopStep(target *kube.Target, deployment kube.AutoscalerDeployment) Step {
	subject := deployment.Namespace + "/" + deployment.Name
	replicas := fmt.Sprint(deployment.Replicas)
	if deployment.Replicas == 0 {
		replicas = deployment.Recorded
	}
	return Step{
		Title: "Stop the cluster autoscaler " + subject,
		Why: "Karpenter now adds nodes for Pending pods; two autoscalers would both add nodes for the same pods. It " +
			"goes from " + replicas + " replica(s) to 0, and the " + replicas + " is kept in its annotation " +
			kube.AnnotationReplicasBeforeKarpenter + " so zoorikctl cluster disconnect can put it back. Its node " +
			"groups keep the nodes they have.",
		Commands: []string{target.Kubectl("-n " + deployment.Namespace + " patch deployment " + deployment.Name +
			" --type=merge -p '" + kube.StopAutoscalerPatch(deployment.Replicas) + "'")},
		NotInPlace: "The cluster autoscaler " + subject + " still runs pods a minute after it was scaled to 0. " +
			"Check `" + target.Kubectl("-n "+deployment.Namespace+" get deployment "+deployment.Name) + "`, then " +
			"run " + prepareCommand + " again.",
		Check: func(ctx context.Context) (bool, error) {
			return kube.ClusterAutoscalerStopped(ctx, target, deployment.Namespace, deployment.Name)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			return kube.StopClusterAutoscaler(ctx, target, deployment.Namespace, deployment.Name)
		},
	}
}

func autoscalerStatusStep(target *kube.Target, namespace string) Step {
	status := namespace + "/" + kube.ClusterAutoscalerStatus
	return Step{
		Title: "Delete the cluster autoscaler's last status " + status,
		Why: "The stopped autoscaler leaves this ConfigMap behind, and Zoorik reads it to tell which node pools an " +
			"autoscaler manages; while it stays, Zoorik keeps treating those pools as the autoscaler's. A running " +
			"cluster autoscaler writes it again.",
		Commands: []string{target.Kubectl("-n " + namespace + " delete configmap " + kube.ClusterAutoscalerStatus)},
		NotInPlace: status + " was written again after it was deleted, so a cluster autoscaler still runs and " +
			"manages node groups. Stop it, then run " + prepareCommand + " again.",
		Check: func(ctx context.Context) (bool, error) {
			return kube.AutoscalerStatusSettled(ctx, target, namespace)
		},
		Apply: func(ctx context.Context, note func(string)) error {
			return kube.DeleteAutoscalerStatus(ctx, target, namespace)
		},
	}
}
