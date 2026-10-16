package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LiveMigrationAnnotation = ZoorikKeyPrefix + "live-migration"

	LiveMigrationKeyAnnotation = ZoorikKeyPrefix + "live-migration-key"

	NodeDaemonNamespace = "zoorik-node"

	NodeDaemonWorkload = "zoorik-node"

	LiveMigrationGrant = "live-migration"

	ReadinessReady = "READY"

	ReadinessRuntimeUnsupported = "RUNTIME_UNSUPPORTED"

	ReadinessArchitectureUnsupported = "ARCHITECTURE_UNSUPPORTED"

	ReadinessWindows = "WINDOWS"

	ReadinessFargate = "FARGATE"

	ReadinessDaemonNotReady = "DAEMON_NOT_READY"

	ReadinessAnnotationUnreadable = "ANNOTATION_UNREADABLE"

	ReadinessCanaryUnproven = "CANARY_UNPROVEN"

	CRIUContainerdNoRestoreMajor = 2

	CRIUContainerdNoRestoreMinor = 4

	unlabelledPool = "(no node pool label)"

	karpenterNodePoolLabel = "karpenter.sh/nodepool"

	eksComputeTypeLabel = "eks.amazonaws.com/compute-type"

	karpenterInstanceGenerationLabel = "karpenter.k8s.aws/instance-generation"

	karpenterInstanceFamilyLabel = "karpenter.k8s.aws/instance-family"

	napSKUFamilyLabel = "karpenter.azure.com/sku-family"

	napSKUVersionLabel = "karpenter.azure.com/sku-version"

	gpuResource = "nvidia.com/gpu"

	readinessNodePage = 500
)

var nodePoolLabelPrecedence = []struct {
	label string
	kind  string
}{
	{"kubernetes.azure.com/agentpool", "AKS node pool"},
	{"eks.amazonaws.com/nodegroup", "EKS node group"},
	{"alpha.eksctl.io/nodegroup-name", "eksctl node group"},
	{karpenterNodePoolLabel, "Karpenter NodePool"},
	{ZoorikKeyPrefix + "node-pool", "node pool"},
}

var (
	leadingVersion = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)`)
	azureSeries    = regexp.MustCompile(`^Standard_([A-Za-z]+)[0-9]+(?:-[0-9]+)?([A-Za-z]*)(_v[0-9]+)?`)
	awsFamily      = regexp.MustCompile(`^([a-z]+)([0-9]+)([a-z0-9-]*)\.`)
)

type NodeReadiness struct {
	Ready      bool        `json:"ready"`
	Reason     string      `json:"reason"`
	Source     bool        `json:"source"`
	Target     bool        `json:"target"`
	Code       string      `json:"code"`
	CPU        string      `json:"cpu"`
	CRIU       string      `json:"criu"`
	CRIUSha256 string      `json:"criuSha256"`
	Canary     *NodeCanary `json:"canary"`
}

type NodeCanary struct {
	Dump    bool `json:"dump"`
	Restore bool `json:"restore"`
}

func (r NodeReadiness) Proven() bool {
	return r.Ready && r.Canary != nil && r.Canary.Dump && r.Canary.Restore
}

type NodeCRIUFacts struct {
	Name             string
	Pool             string
	PoolKind         string
	OSImage          string
	Kernel           string
	ContainerRuntime string
	Kubelet          string
	Architecture     string
	InstanceType     string
	CPUSeries        string
	GPU              bool
	PlatformReason   string
	PlatformDetail   string
	Daemon           *NodeReadiness
	DaemonUnreadable bool
	ZoorikOwnedPool  bool
	containerd       string
	noRestore        bool
}

func (f NodeCRIUFacts) Ready() bool {
	switch {
	case f.Daemon != nil:
		return f.Daemon.Proven()
	case f.DaemonUnreadable:
		return false
	}
	return f.PlatformReason == ReadinessReady && !f.noRestore
}

func (f NodeCRIUFacts) SourceOnly() bool {
	if f.Daemon != nil {
		return f.Daemon.Source && !f.Daemon.Target
	}
	return f.unreported() && f.noRestore
}

func (f NodeCRIUFacts) RestoreNote() string {
	switch {
	case !f.SourceOnly():
		return ""
	case f.Daemon != nil:
		return f.Daemon.Code + ": " + f.Daemon.Reason
	}
	return fmt.Sprintf("source only: containerd %d.%d and later no longer restore a checkpoint when a container "+
		"is created, so no pod moves onto these nodes, and their own pods move only to a node that restores",
		CRIUContainerdNoRestoreMajor, CRIUContainerdNoRestoreMinor)
}

func (f NodeCRIUFacts) unreported() bool {
	return f.PlatformReason == ReadinessReady && !f.DaemonUnreadable && f.Daemon == nil
}

func (f NodeCRIUFacts) Reason() string {
	switch {
	case f.Daemon != nil && f.Daemon.Proven():
		return ReadinessReady
	case f.PlatformReason != ReadinessReady:
		return f.PlatformReason
	case f.DaemonUnreadable:
		return ReadinessAnnotationUnreadable
	case f.Daemon != nil && !f.Daemon.Ready:
		return ReadinessDaemonNotReady
	case f.Daemon != nil && !f.Daemon.Proven():
		return ReadinessCanaryUnproven
	}
	return ReadinessReady
}

type PoolCRIUReadiness struct {
	Name        string
	Kind        string
	Nodes       []NodeCRIUFacts
	ZoorikOwned bool
}

func (p PoolCRIUReadiness) ReadyNodes() int {
	ready := 0
	for _, node := range p.Nodes {
		if node.Ready() {
			ready++
		}
	}
	return ready
}

func (p PoolCRIUReadiness) SourceOnlyNodes() int {
	sourceOnly := 0
	for _, node := range p.Nodes {
		if node.SourceOnly() {
			sourceOnly++
		}
	}
	return sourceOnly
}

func (p PoolCRIUReadiness) Verdict() string {
	ready := p.ReadyNodes()
	switch {
	case ready == len(p.Nodes):
		return "READY"
	case ready == 0 && p.SourceOnlyNodes() > 0:
		return "SOURCE ONLY"
	case ready == 0:
		return "NOT READY"
	}
	return "PARTLY READY"
}

func (p PoolCRIUReadiness) Distinct(value func(NodeCRIUFacts) string) []string {
	counts := map[string]int{}
	for _, node := range p.Nodes {
		counts[text.OrNone(value(node))]++
	}
	values := make([]string, 0, len(counts))
	for key, count := range counts {
		if len(counts) > 1 {
			key = fmt.Sprintf("%s (%d)", key, count)
		}
		values = append(values, key)
	}
	sort.Strings(values)
	return values
}

func (p PoolCRIUReadiness) Reasons() []string {
	counts := map[string]int{}
	details := map[string]string{}
	for _, node := range p.Nodes {
		if node.Ready() || node.SourceOnly() {
			continue
		}
		reason := node.Reason()
		counts[reason]++
		if _, seen := details[reason]; !seen {
			details[reason] = reasonDetail(node, reason)
		}
	}
	reasons := make([]string, 0, len(counts))
	for reason, count := range counts {
		line := fmt.Sprintf("%s on %d node(s)", reason, count)
		if detail := details[reason]; detail != "" {
			line += ": " + detail
		}
		reasons = append(reasons, line)
	}
	sort.Strings(reasons)
	return reasons
}

func (p PoolCRIUReadiness) RestoreNotes() []string {
	counts := map[string]int{}
	notes := map[string]string{}
	for _, node := range p.Nodes {
		note := node.RestoreNote()
		if note == "" {
			continue
		}
		counts[node.containerd]++
		notes[node.containerd] = note
	}
	lines := make([]string, 0, len(counts))
	for version, count := range counts {
		lines = append(lines, fmt.Sprintf("containerd %s on %d node(s): %s", version, count, notes[version]))
	}
	sort.Strings(lines)
	return lines
}

func (p PoolCRIUReadiness) DaemonState() string {
	reporting, ready := 0, 0
	for _, node := range p.Nodes {
		if node.Daemon != nil {
			reporting++
			if node.Daemon.Proven() {
				ready++
			}
		}
	}
	switch {
	case reporting == 0:
		return "not reporting (the live-migration daemon is not installed, or has not started on these nodes)"
	case reporting == len(p.Nodes) && ready == reporting:
		return fmt.Sprintf("reporting ready on all %d node(s)", reporting)
	}
	return fmt.Sprintf("reporting on %d of %d node(s), ready on %d", reporting, len(p.Nodes), ready)
}

func (p PoolCRIUReadiness) DaemonCRIU() []string {
	seen := map[string]bool{}
	var builds []string
	for _, node := range p.Nodes {
		if node.Daemon == nil || node.Daemon.CRIU == "" {
			continue
		}
		build := node.Daemon.CRIU
		if node.Daemon.CRIUSha256 != "" {
			build += " (sha256 " + shortFingerprint(node.Daemon.CRIUSha256) + ")"
		}
		if !seen[build] {
			seen[build] = true
			builds = append(builds, build)
		}
	}
	sort.Strings(builds)
	return builds
}

func (p PoolCRIUReadiness) GPUNodes() int {
	count := 0
	for _, node := range p.Nodes {
		if node.GPU {
			count++
		}
	}
	return count
}

func ReadCRIUReadiness(ctx context.Context, target *Target) ([]PoolCRIUReadiness, error) {
	var nodes []corev1.Node
	options := metav1.ListOptions{Limit: readinessNodePage}
	for {
		page, err := target.Clientset.CoreV1().Nodes().List(ctx, options)
		if err != nil {
			return nil, refusal.Wrap(refusal.CodePermission, err,
				"This cluster would not list its nodes, so no node pool's live-migration readiness can be read.",
				"The context's credential needs to list nodes: `"+target.Kubectl("get nodes -o wide")+
					"`. Nothing has been changed.")
		}
		nodes = append(nodes, page.Items...)
		if page.Continue == "" {
			break
		}
		options.Continue = page.Continue
	}
	byPool := map[string]*PoolCRIUReadiness{}
	for _, node := range nodes {
		if node.Labels[NodeTypeLabel] == VirtualKubeletNodeType {
			continue
		}
		facts := NodeFactsOf(node)
		pool := byPool[facts.Pool]
		if pool == nil {
			pool = &PoolCRIUReadiness{Name: facts.Pool, Kind: facts.PoolKind, ZoorikOwned: facts.ZoorikOwnedPool}
			byPool[facts.Pool] = pool
		}
		pool.Nodes = append(pool.Nodes, facts)
	}
	pools := make([]PoolCRIUReadiness, 0, len(byPool))
	for _, pool := range byPool {
		sort.Slice(pool.Nodes, func(i, j int) bool { return pool.Nodes[i].Name < pool.Nodes[j].Name })
		pools = append(pools, *pool)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	return pools, nil
}

func NodeFactsOf(node corev1.Node) NodeCRIUFacts {
	info := node.Status.NodeInfo
	labels := node.Labels
	facts := NodeCRIUFacts{
		Name:             node.Name,
		OSImage:          strings.TrimSpace(info.OSImage),
		Kernel:           strings.TrimSpace(info.KernelVersion),
		ContainerRuntime: strings.TrimSpace(info.ContainerRuntimeVersion),
		Kubelet:          strings.TrimSpace(info.KubeletVersion),
		Architecture:     firstNonEmpty(info.Architecture, labels[ArchLabel]),
		InstanceType:     firstNonEmpty(labels["node.kubernetes.io/instance-type"], labels["beta.kubernetes.io/instance-type"]),
		Pool:             unlabelledPool,
		PoolKind:         "nodes without a pool label",
	}
	for _, candidate := range nodePoolLabelPrecedence {
		if value := strings.TrimSpace(labels[candidate.label]); value != "" {
			facts.Pool = value
			facts.PoolKind = candidate.kind
			facts.ZoorikOwnedPool = candidate.label == karpenterNodePoolLabel &&
				strings.HasPrefix(value, ZoorikPoolPrefix)
			break
		}
	}
	facts.CPUSeries = cpuSeriesOf(labels, facts.InstanceType)
	if quantity, ok := node.Status.Allocatable[gpuResource]; ok && !quantity.IsZero() {
		facts.GPU = true
	}
	facts.PlatformReason, facts.PlatformDetail = platformVerdict(labels, facts)
	if runtime, version, _ := strings.Cut(facts.ContainerRuntime, "://"); strings.EqualFold(runtime, "containerd") {
		facts.containerd = version
		facts.noRestore = versionAtLeast(version, CRIUContainerdNoRestoreMajor, CRIUContainerdNoRestoreMinor)
	}
	if raw, ok := node.Annotations[LiveMigrationAnnotation]; ok {
		var published NodeReadiness
		if err := json.Unmarshal([]byte(raw), &published); err != nil {
			facts.DaemonUnreadable = true
		} else {
			facts.Daemon = &published
			if published.CPU != "" {
				sum := sha256.Sum256([]byte(published.CPU))
				facts.CPUSeries = facts.CPUSeries + " fingerprint " + shortFingerprint(hex.EncodeToString(sum[:]))
			}
		}
	}
	return facts
}

func platformVerdict(labels map[string]string, facts NodeCRIUFacts) (string, string) {
	if strings.EqualFold(labels[OSLabel], "windows") || strings.Contains(strings.ToLower(facts.OSImage), "windows") {
		return ReadinessWindows, "Windows nodes cannot run CRIU"
	}
	if strings.EqualFold(labels[eksComputeTypeLabel], "fargate") {
		return ReadinessFargate, "Fargate runs each pod in its own micro-VM, with no node to migrate between"
	}
	if facts.Architecture != "" && !isSupportedArchitecture(facts.Architecture) {
		return ReadinessArchitectureUnsupported, "the node runs " + facts.Architecture + "; live migration is built for " +
			strings.Join(SupportedArchitectures, " and ")
	}
	runtime, _, _ := strings.Cut(facts.ContainerRuntime, "://")
	if !strings.EqualFold(runtime, "containerd") {
		return ReadinessRuntimeUnsupported, "the container runtime is " + text.OrNone(facts.ContainerRuntime) +
			"; live migration restores through containerd"
	}
	return ReadinessReady, ""
}

func cpuSeriesOf(labels map[string]string, instanceType string) string {
	if family := labels[napSKUFamilyLabel]; family != "" {
		if version := labels[napSKUVersionLabel]; version != "" {
			if match := azureSeries.FindStringSubmatch(instanceType); match != nil {
				return match[1] + match[2] + "_v" + strings.TrimPrefix(version, "v")
			}
			return family + " v" + strings.TrimPrefix(version, "v")
		}
	}
	if match := azureSeries.FindStringSubmatch(instanceType); match != nil {
		return match[1] + match[2] + match[3]
	}
	if family := labels[karpenterInstanceFamilyLabel]; family != "" {
		if generation := labels[karpenterInstanceGenerationLabel]; generation != "" {
			return family + " (generation " + generation + ")"
		}
		return family
	}
	if match := awsFamily.FindStringSubmatch(instanceType); match != nil {
		return match[1] + match[2] + match[3] + " (generation " + match[2] + ")"
	}
	if instanceType == "" {
		return "unknown"
	}
	return instanceType
}

func reasonDetail(node NodeCRIUFacts, reason string) string {
	switch reason {
	case ReadinessDaemonNotReady:
		if node.Daemon != nil && node.Daemon.Reason != "" {
			return "the daemon reports " + node.Daemon.Reason
		}
		return "the daemon reports not ready"
	case ReadinessCanaryUnproven:
		return "the daemon reports ready, but no dump and restore canary passed on this node's current CRIU, " +
			"containerd, kernel and OS image"
	case ReadinessAnnotationUnreadable:
		return "the " + LiveMigrationAnnotation + " annotation is not the JSON the daemon writes"
	}
	return node.PlatformDetail
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) > 12 {
		return fingerprint[:12]
	}
	return fingerprint
}

func versionAtLeast(version string, major, minor int) bool {
	gotMajor, gotMinor, ok := majorMinor(version)
	if !ok {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

func majorMinor(version string) (int, int, bool) {
	match := leadingVersion.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(match[1])
	minor, errMinor := strconv.Atoi(match[2])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
