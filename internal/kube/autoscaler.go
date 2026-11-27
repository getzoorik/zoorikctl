package kube

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ClusterAutoscalerImagePattern = `(^|/)autoscaling/cluster-autoscaler([:@].*)?$`

	ClusterAutoscalerImage = "registry.k8s.io/autoscaling/cluster-autoscaler"

	AnnotationReplicasBeforeKarpenter = ZoorikKeyPrefix + "replicas-before-karpenter"

	AutoscalerPolicyName = "zoorik-no-cluster-autoscaler"

	ManagedByLabelKey = "app.kubernetes.io/managed-by"

	autoscalerPolicyMessage = "Zoorik switched this cluster to Karpenter. The cluster autoscaler can't run beside " +
		"it. Remove it from your Terraform or Helm, or run zoorikctl cluster disconnect first."
)

var clusterAutoscalerImage = regexp.MustCompile(ClusterAutoscalerImagePattern)

type AutoscalerDeployment struct {
	Namespace string
	Name      string
	Replicas  int32
	Recorded  string
}

func ClusterAutoscalerDeployments(ctx context.Context, target *Target) ([]AutoscalerDeployment, error) {
	list, err := target.Clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, readRefused(target, err, "Deployments", "get deployments -A")
	}
	var found []AutoscalerDeployment
	for _, deployment := range list.Items {
		if !runsClusterAutoscaler(deployment) {
			continue
		}
		found = append(found, AutoscalerDeployment{Namespace: deployment.Namespace, Name: deployment.Name,
			Replicas: replicasOf(deployment), Recorded: deployment.Annotations[AnnotationReplicasBeforeKarpenter]})
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].Namespace+"/"+found[i].Name < found[j].Namespace+"/"+found[j].Name
	})
	return found, nil
}

func runsClusterAutoscaler(deployment appsv1.Deployment) bool {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if clusterAutoscalerImage.MatchString(container.Image) {
			return true
		}
	}
	return false
}

func replicasOf(deployment appsv1.Deployment) int32 {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return *deployment.Spec.Replicas
}

func AdmissionPoliciesServed(target *Target) bool {
	_, err := target.Mapper.RESTMapping(schema.GroupKind{Group: admissionRegistrationGroup,
		Kind: validatingPolicyKind}, "v1")
	return err == nil
}

func autoscalerPolicyObjects() []*unstructured.Unstructured {
	metadata := func() map[string]any {
		return map[string]any{"name": AutoscalerPolicyName, "labels": map[string]any{ManagedByLabelKey: FieldManager}}
	}
	policy := map[string]any{
		"apiVersion": admissionRegistrationGroup + "/v1",
		"kind":       validatingPolicyKind,
		"metadata":   metadata(),
		"spec": map[string]any{
			"failurePolicy": "Fail",
			"matchConstraints": map[string]any{
				"resourceRules": []any{map[string]any{
					"apiGroups":   []any{"apps"},
					"apiVersions": []any{"*"},
					"operations":  []any{"CREATE", "UPDATE"},
					"resources":   []any{"deployments"},
				}},
			},
			"matchConditions": []any{map[string]any{
				"name": "runs-the-cluster-autoscaler",
				"expression": "object.spec.template.spec.containers.exists(c, has(c.image) && c.image.matches('" +
					ClusterAutoscalerImagePattern + "'))",
			}},
			"validations": []any{map[string]any{
				"expression": "has(object.spec.replicas) && object.spec.replicas == 0",
				"message":    autoscalerPolicyMessage,
				"reason":     "Forbidden",
			}},
		},
	}
	binding := map[string]any{
		"apiVersion": admissionRegistrationGroup + "/v1",
		"kind":       validatingPolicyBindingKind,
		"metadata":   metadata(),
		"spec": map[string]any{
			"policyName":        AutoscalerPolicyName,
			"validationActions": []any{"Deny"},
		},
	}
	return []*unstructured.Unstructured{{Object: policy}, {Object: binding}}
}

func AutoscalerPolicyManifest() string {
	var items []any
	for _, object := range autoscalerPolicyObjects() {
		items = append(items, object.Object)
	}
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": items}); err != nil {
		return ""
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

func ApplyAutoscalerPolicy(ctx context.Context, target *Target) error {
	for _, object := range autoscalerPolicyObjects() {
		if _, err := serverSideApply(ctx, target.Dynamic.Resource(policyResources[object.GetKind()]), object,
			false); err != nil {
			return err
		}
	}
	return nil
}

func AutoscalerPolicyInPlace(ctx context.Context, target *Target) (bool, error) {
	for _, kind := range []string{validatingPolicyKind, validatingPolicyBindingKind} {
		_, err := target.Dynamic.Resource(policyResources[kind]).Get(ctx, AutoscalerPolicyName, metav1.GetOptions{})
		found, err := exists(err)
		if err != nil || !found {
			return false, err
		}
	}
	return true, nil
}

func StopAutoscalerPatch(replicas int32) string {
	encoded, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			AnnotationReplicasBeforeKarpenter: strconv.Itoa(int(replicas))}},
		"spec": map[string]any{"replicas": 0},
	})
	return string(encoded)
}

func StopClusterAutoscaler(ctx context.Context, target *Target, namespace, name string) error {
	deployments := target.Clientset.AppsV1().Deployments(namespace)
	deployment, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	replicas := replicasOf(*deployment)
	if replicas == 0 {
		return nil
	}
	_, err = deployments.Patch(ctx, name, types.MergePatchType, []byte(StopAutoscalerPatch(replicas)),
		metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func ClusterAutoscalerStopped(ctx context.Context, target *Target, namespace, name string) (bool, error) {
	deployment, err := target.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if replicasOf(*deployment) > 0 {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return false, err
	}
	pods, err := target.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String()})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return false, nil
		}
	}
	return true, nil
}

func AutoscalerStatusSettled(ctx context.Context, target *Target, namespace string) (bool, error) {
	status, err := target.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, ClusterAutoscalerStatus,
		metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	groups, err := nodeGroupNames(status.Data[ClusterAutoscalerStatusKey])
	return err == nil && len(groups) == 0, nil
}

func DeleteAutoscalerStatus(ctx context.Context, target *Target, namespace string) error {
	err := target.Clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, ClusterAutoscalerStatus,
		metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
