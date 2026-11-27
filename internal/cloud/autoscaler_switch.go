package cloud

import (
	"context"

	"github.com/getzoorik/zoorikctl/internal/kube"
)

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
