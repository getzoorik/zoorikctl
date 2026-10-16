package kube

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/setup"
	"github.com/getzoorik/zoorikctl/internal/text"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

const (
	SupportedMajorVersion = 1

	MinimumMinorVersion = 30

	ReasonUnsupportedKubernetesVersion = "UNSUPPORTED_KUBERNETES_VERSION"

	ReasonUnsupportedArchitecture = "UNSUPPORTED_ARCHITECTURE"

	OSLabel = "kubernetes.io/os"

	ArchLabel = "kubernetes.io/arch"

	kindDeployment = "Deployment"

	kindMutatingWebhookConfiguration = "MutatingWebhookConfiguration"

	kindValidatingAdmissionPolicy = "ValidatingAdmissionPolicy"

	kindValidatingAdmissionPolicyBinding = "ValidatingAdmissionPolicyBinding"

	platformPage = 500
)

var SupportedArchitectures = []string{"amd64", "arm64"}

type SizeTier struct {
	MaxPods       int
	AgentMemory   string
	WebhookMemory string
}

var sizeTiers = []SizeTier{
	{MaxPods: 5000, AgentMemory: "512Mi", WebhookMemory: "384Mi"},
	{MaxPods: 20000, AgentMemory: "1Gi", WebhookMemory: "512Mi"},
	{MaxPods: 50000, AgentMemory: "2Gi", WebhookMemory: "1Gi"},
}

type Platform struct {
	Version       string
	Major         int
	Minor         int
	VersionKnown  bool
	OpenShift     bool
	Architectures map[string]int
	Unsupported   map[string]int
	Pods          int
	PodsCounted   bool
	Tier          SizeTier
	BeyondTiers   bool
}

type Adaptation struct {
	OpenShiftIdentity []string
	Resized           []string
}

func DetectPlatform(ctx context.Context, target *Target) (Platform, error) {
	platform := Platform{Architectures: map[string]int{}, Unsupported: map[string]int{}}

	version, err := target.Clientset.Discovery().ServerVersion()
	if err != nil {
		return platform, refusal.Wrap(refusal.CodePermission, err,
			"This cluster would not say which Kubernetes version it runs.",
			"The context's credential needs `"+target.Kubectl("version")+"` to answer. Nothing "+
				"has been applied.")
	}
	platform.Version = strings.TrimSpace(version.GitVersion)
	platform.Major, platform.Minor, platform.VersionKnown = versionOf(version.Major, version.Minor,
		version.GitVersion)
	if platform.VersionKnown && (platform.Major != SupportedMajorVersion ||
		platform.Minor < MinimumMinorVersion) {
		return platform, refusal.New(refusal.CodeWrongCluster,
			fmt.Sprintf("This cluster runs Kubernetes %s; Zoorik needs %d.%d or later.",
				text.OrNone(platform.Version), SupportedMajorVersion, MinimumMinorVersion),
			"Upgrade the cluster and run this again. Nothing has been applied.").
			WithDetail("Reason: " + ReasonUnsupportedKubernetesVersion)
	}

	platform.OpenShift = serves(target, "security.openshift.io", "SecurityContextConstraints", "v1")

	client, err := metadata.NewForConfig(target.restConfig)
	if err != nil {
		return platform, refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("Could not build a client for %s.", target.ServerURL),
			"Check the context's credentials.")
	}
	if err := countArchitectures(ctx, client, &platform); err != nil {
		return platform, refusal.Wrap(refusal.CodePermission, err,
			"This cluster would not list its nodes, so the architectures the agent must run on "+
				"are unknown.",
			"The context's credential needs to list nodes: `"+
				target.Kubectl("get nodes -L "+ArchLabel)+"`. Nothing has been applied.")
	}
	if len(platform.Architectures) > 0 && supported(platform.Architectures) == 0 {
		return platform, refusal.New(refusal.CodeWrongCluster,
			"No node on this cluster runs on "+strings.Join(SupportedArchitectures, " or ")+
				", the architectures the agent image is built for.",
			"Add an "+strings.Join(SupportedArchitectures, " or an ")+" node and run this again. "+
				"Nothing has been applied.").
			WithDetail("Nodes: "+tallyText(platform.Architectures), "Reason: "+ReasonUnsupportedArchitecture)
	}

	platform.Pods, platform.PodsCounted = countPods(ctx, client)
	platform.Tier = sizeTiers[0]
	if platform.PodsCounted {
		platform.Tier, platform.BeyondTiers = tierFor(platform.Pods)
	}
	return platform, nil
}

func (p Platform) VersionText() string {
	if p.Version == "" {
		return "unknown"
	}
	return p.Version
}

func (p Platform) ArchitectureText() string {
	return tallyText(p.Architectures)
}

func (p Platform) UnsupportedText() string {
	return tallyText(p.Unsupported)
}

func (p Platform) Adapt(docs []setup.Document) (Adaptation, error) {
	var adaptation Adaptation
	for _, object := range docs {
		if object.GetKind() != kindDeployment || !isZoorikWorkload(object.GetName()) {
			continue
		}
		if p.OpenShift && dropFixedIdentity(object) {
			adaptation.OpenShiftIdentity = append(adaptation.OpenShiftIdentity, object.GetName())
		}
		resized, err := p.resize(object)
		if err != nil {
			return adaptation, err
		}
		if resized != "" {
			adaptation.Resized = append(adaptation.Resized, resized)
		}
	}
	return adaptation, nil
}

func (p Platform) resize(object *unstructured.Unstructured) (string, error) {
	memory := p.Tier.AgentMemory
	if object.GetName() == WebhookWorkload {
		memory = p.Tier.WebhookMemory
	}
	wanted, err := resource.ParseQuantity(memory)
	if err != nil {
		return "", err
	}
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec",
		"containers")
	if err != nil || !found {
		return "", err
	}
	var resized []string
	for index, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		limit, hasLimit, _ := unstructured.NestedString(container, "resources", "limits", "memory")
		if !hasLimit {
			continue
		}
		current, err := resource.ParseQuantity(limit)
		if err != nil || current.Cmp(wanted) >= 0 {
			continue
		}
		if request, hasRequest, _ := unstructured.NestedString(container, "resources", "requests",
			"memory"); hasRequest {
			if requested, err := resource.ParseQuantity(request); err == nil {
				scaled := resource.NewQuantity(requested.Value()*wanted.Value()/current.Value(),
					resource.BinarySI)
				if err := unstructured.SetNestedField(container, scaled.String(), "resources",
					"requests", "memory"); err != nil {
					return "", err
				}
			}
		}
		if err := unstructured.SetNestedField(container, wanted.String(), "resources", "limits",
			"memory"); err != nil {
			return "", err
		}
		containers[index] = container
		resized = append(resized, fmt.Sprintf("%s/%s memory limit %s -> %s", object.GetName(),
			stringOf(container["name"]), limit, wanted.String()))
	}
	if len(resized) == 0 {
		return "", nil
	}
	if err := unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec",
		"containers"); err != nil {
		return "", err
	}
	return strings.Join(resized, "; "), nil
}

func dropFixedIdentity(object *unstructured.Unstructured) bool {
	dropped := false
	for _, field := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if _, found, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "template", "spec",
			"securityContext", field); found {
			unstructured.RemoveNestedField(object.Object, "spec", "template", "spec", "securityContext",
				field)
			dropped = true
		}
	}
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec",
		"containers")
	if err != nil || !found {
		return dropped
	}
	for index, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"runAsUser", "runAsGroup"} {
			if _, found, _ := unstructured.NestedFieldNoCopy(container, "securityContext", field); found {
				unstructured.RemoveNestedField(container, "securityContext", field)
				dropped = true
			}
		}
		containers[index] = container
	}
	_ = unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers")
	return dropped
}

func isZoorikWorkload(name string) bool {
	return name == AgentWorkload || name == WebhookWorkload
}

func serves(target *Target, group, kind, version string) bool {
	if target == nil || target.Mapper == nil {
		return false
	}
	_, err := target.Mapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind}, version)
	return err == nil
}

func versionOf(major, minor, gitVersion string) (int, int, bool) {
	majorNumber, majorOK := leadingNumber(major)
	minorNumber, minorOK := leadingNumber(minor)
	if majorOK && minorOK {
		return majorNumber, minorNumber, true
	}
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(gitVersion), "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	majorNumber, majorOK = leadingNumber(parts[0])
	minorNumber, minorOK = leadingNumber(parts[1])
	return majorNumber, minorNumber, majorOK && minorOK
}

func leadingNumber(text string) (int, bool) {
	text = strings.TrimSpace(text)
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	number, err := strconv.Atoi(text[:end])
	return number, err == nil
}

var nodeResource = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

var podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

func countArchitectures(ctx context.Context, client metadata.Interface, platform *Platform) error {
	options := metav1.ListOptions{Limit: platformPage}
	for {
		page, err := client.Resource(nodeResource).List(ctx, options)
		if err != nil {
			return err
		}
		for _, node := range page.Items {
			architecture := strings.TrimSpace(node.Labels[ArchLabel])
			if architecture == "" {
				architecture = "unknown"
			}
			platform.Architectures[architecture]++
			if !isSupportedArchitecture(architecture) && architecture != "unknown" {
				platform.Unsupported[architecture]++
			}
		}
		if page.Continue == "" {
			return nil
		}
		options.Continue = page.Continue
	}
}

func countPods(ctx context.Context, client metadata.Interface) (int, bool) {
	first, err := client.Resource(podResource).Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return 0, false
	}
	if first.Continue == "" {
		return len(first.Items), true
	}
	if remaining := first.RemainingItemCount; remaining != nil {
		return len(first.Items) + int(*remaining), true
	}
	counted := 0
	options := metav1.ListOptions{Limit: platformPage}
	for {
		page, err := client.Resource(podResource).Namespace(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return 0, false
		}
		counted += len(page.Items)
		if page.Continue == "" {
			return counted, true
		}
		options.Continue = page.Continue
	}
}

func tierFor(pods int) (SizeTier, bool) {
	for _, tier := range sizeTiers {
		if pods <= tier.MaxPods {
			return tier, false
		}
	}
	return sizeTiers[len(sizeTiers)-1], true
}

func isSupportedArchitecture(architecture string) bool {
	for _, supported := range SupportedArchitectures {
		if architecture == supported {
			return true
		}
	}
	return false
}

func supported(architectures map[string]int) int {
	total := 0
	for architecture, count := range architectures {
		if isSupportedArchitecture(architecture) {
			total += count
		}
	}
	return total
}

func tallyText(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, key := range sorted(counts) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
	}
	return strings.Join(parts, ", ")
}

func stringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
