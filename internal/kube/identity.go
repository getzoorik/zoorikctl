package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	CloudAzure = "AZURE"

	CloudAWS = "AWS"

	CloudLocal = "LOCAL"

	RegionLabel = "topology.kubernetes.io/region"

	NodeTypeLabel = "type"

	VirtualKubeletNodeType = "virtual-kubelet"

	identityNodeSample = 200
)

var cloudProviderIDSchemes = []struct {
	scheme string
	cloud  string
}{
	{"azure", CloudAzure},
	{"aws", CloudAWS},
}

type Identity struct {
	Cloud     string
	Region    string
	Evidence  string
	Nodes     int
	MoreNodes bool

	Subscriptions []string
}

func Identify(ctx context.Context, target *Target) (Identity, error) {
	nodes, err := target.Clientset.CoreV1().Nodes().
		List(ctx, metav1.ListOptions{Limit: identityNodeSample})
	if err != nil {
		return Identity{}, refusal.Wrap(refusal.CodePermission, err,
			"This cluster would not say which cloud it runs on.",
			"The context's credential needs to list nodes, which is what names the cloud: "+
				"`"+target.Kubectl("get nodes -o wide")+"`. Nothing has been applied.")
	}

	served := servedNodes(nodes.Items)
	cloud, scheme, reporting := cloudOf(served)
	return Identity{
		Cloud:         cloud,
		Region:        regionOf(nodes.Items),
		Evidence:      evidenceFor(scheme, reporting, len(nodes.Items)),
		Nodes:         len(nodes.Items),
		MoreNodes:     nodes.Continue != "",
		Subscriptions: subscriptionsOf(served),
	}, nil
}

func subscriptionsOf(nodes []corev1.Node) []string {
	seen := map[string]bool{}
	for _, node := range nodes {
		if subscription := azureSubscriptionOf(node.Spec.ProviderID); subscription != "" {
			seen[subscription] = true
		}
	}
	return sortedSet(seen)
}

func cloudOf(nodes []corev1.Node) (string, string, int) {
	counts := map[string]int{}
	for _, node := range nodes {
		counts[providerIDScheme(node.Spec.ProviderID)]++
	}
	for _, served := range cloudProviderIDSchemes {
		if reporting := counts[served.scheme]; reporting > 0 {
			return served.cloud, served.scheme, reporting
		}
	}
	return CloudLocal, "", 0
}

func servedNodes(nodes []corev1.Node) []corev1.Node {
	served := make([]corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Labels[NodeTypeLabel] == VirtualKubeletNodeType {
			continue
		}
		served = append(served, node)
	}
	return served
}

func providerIDScheme(providerID string) string {
	trimmed := strings.TrimSpace(providerID)
	separator := strings.Index(trimmed, "://")
	if separator < 0 {
		return ""
	}
	return strings.ToLower(trimmed[:separator])
}

func regionOf(nodes []corev1.Node) string {
	for _, node := range nodes {
		if region := strings.TrimSpace(node.Labels[RegionLabel]); region != "" {
			return region
		}
	}
	return ""
}

func evidenceFor(scheme string, reporting, total int) string {
	switch {
	case scheme != "":
		return fmt.Sprintf("%d of %d node(s) carry %s:// provider ids", reporting, total, scheme)
	case total == 0:
		return "the cluster has no node yet, so it is connected as a local cluster"
	}
	return fmt.Sprintf("none of its %d node(s) carry an azure:// or aws:// provider id, so it is connected as a "+
		"local cluster", total)
}

func sorted(counted map[string]int) []string {
	names := make([]string, 0, len(counted))
	for name := range counted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedSet(present map[string]bool) []string {
	names := make([]string, 0, len(present))
	for name := range present {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
