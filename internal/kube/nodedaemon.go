package kube

import (
	"context"

	"github.com/getzoorik/zoorikctl/internal/setup"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const LiveMigrationGrantLabel = ZoorikKeyPrefix + "grant"

func NodeDaemonInstalled(ctx context.Context, target *Target) (bool, error) {
	namespace, err := target.Clientset.CoreV1().Namespaces().Get(ctx, NodeDaemonNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return namespace.Labels[PartOfLabelKey] == PartOfLabelValue, nil
}

func RemoveNodeDaemon(ctx context.Context, target *Target) (string, error) {
	return deleteNamespaceNamed(ctx, target, NodeDaemonNamespace)
}

func SetupHasLiveMigration(docs []setup.Document) bool {
	for _, doc := range docs {
		if doc.GetLabels()[LiveMigrationGrantLabel] == LiveMigrationGrant ||
			doc.GetNamespace() == NodeDaemonNamespace ||
			(doc.GetKind() == kindNamespace && doc.GetName() == NodeDaemonNamespace) {
			return true
		}
	}
	return false
}
