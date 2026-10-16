package kube

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	AgentNamespace = "zoorik-agent"
	AgentConfigMap = "zoorik-agent-config"

	AgentClusterNameKey       = "clusterName"
	AgentClusterResourceIDKey = "clusterResourceId"

	AgentServiceAccount = "zoorik-agent"

	AzureWorkloadIdentityClientIDAnnotation = "azure.workload.identity/client-id"
	EKSRoleARNAnnotation                    = "eks.amazonaws.com/role-arn"

	AgentWorkload = "zoorik-agent"

	AgentImageTagSetting = "CORAL_CONSOLE_INSTALLER_AGENT_IMAGE_TAG"

	KindContextPrefix = "kind-"

	RestartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
)

type Occupant struct {
	Present               bool
	ClusterName           string
	ClusterResourceID     string
	CloudIdentityClientID string
}

func InspectOccupant(ctx context.Context, target *Target) (Occupant, error) {
	configMap, err := target.Clientset.CoreV1().ConfigMaps(AgentNamespace).
		Get(ctx, AgentConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Occupant{}, nil
		}
		return Occupant{}, err
	}
	occupant := Occupant{
		Present:           true,
		ClusterName:       configMap.Data[AgentClusterNameKey],
		ClusterResourceID: strings.TrimSpace(configMap.Data[AgentClusterResourceIDKey]),
	}
	account, err := target.Clientset.CoreV1().ServiceAccounts(AgentNamespace).
		Get(ctx, AgentServiceAccount, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return occupant, err
	}
	if err == nil {
		occupant.CloudIdentityClientID = firstNonEmpty(account.Annotations[AzureWorkloadIdentityClientIDAnnotation],
			account.Annotations[EKSRoleARNAnnotation])
	}
	return occupant, nil
}

func RemoveCloudIdentityAnnotations(ctx context.Context, target *Target) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		AzureWorkloadIdentityClientIDAnnotation: nil,
		EKSRoleARNAnnotation:                    nil,
	}}})
	if err != nil {
		return err
	}
	_, err = target.Clientset.CoreV1().ServiceAccounts(AgentNamespace).Patch(ctx,
		AgentServiceAccount, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func RemoveAgentConfigKey(ctx context.Context, target *Target, key string) error {
	body, err := json.Marshal(map[string]any{"data": map[string]any{key: nil}})
	if err != nil {
		return err
	}
	_, err = target.Clientset.CoreV1().ConfigMaps(AgentNamespace).Patch(ctx, AgentConfigMap,
		types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func SetAgentConfigKey(ctx context.Context, target *Target, key, value string) error {
	body, err := json.Marshal(map[string]any{"data": map[string]any{key: value}})
	if err != nil {
		return err
	}
	_, err = target.Clientset.CoreV1().ConfigMaps(AgentNamespace).Patch(ctx, AgentConfigMap,
		types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func SetCloudIdentityAnnotation(ctx context.Context, target *Target, key, value string) error {
	other := EKSRoleARNAnnotation
	if key == EKSRoleARNAnnotation {
		other = AzureWorkloadIdentityClientIDAnnotation
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		key:   value,
		other: nil,
	}}})
	if err != nil {
		return err
	}
	_, err = target.Clientset.CoreV1().ServiceAccounts(AgentNamespace).Patch(ctx,
		AgentServiceAccount, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func RestartAgent(ctx context.Context, target *Target, at time.Time) (string, error) {
	stamp := at.UTC().Format(time.RFC3339)
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			RestartedAtAnnotation: stamp,
		}},
	}}})
	if err != nil {
		return "", err
	}
	_, err = target.Clientset.AppsV1().Deployments(AgentNamespace).Patch(ctx, AgentWorkload,
		types.StrategicMergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	return stamp, err
}

func AgentCredentialsInjected(ctx context.Context, target *Target, name, value, restartedAt string) (bool,
	error) {
	pods, err := target.Clientset.CoreV1().Pods(AgentNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + AgentWorkload,
	})
	if err != nil {
		return false, err
	}
	live := 0
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if restartedAt != "" && pod.Annotations[RestartedAtAnnotation] != restartedAt {
			return false, nil
		}
		live++
		if !carriesEnv(pod, name, value) {
			return false, nil
		}
	}
	return live > 0, nil
}

func WaitForAgentCredentials(ctx context.Context, target *Target, name, value, restartedAt string,
	timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		injected, err := AgentCredentialsInjected(ctx, target, name, value, restartedAt)
		if err != nil || injected {
			return injected, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func carriesEnv(pod corev1.Pod, name, value string) bool {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == name && (value == "" || env.Value == value) {
				return true
			}
		}
	}
	return false
}
