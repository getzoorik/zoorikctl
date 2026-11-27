package kube

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
