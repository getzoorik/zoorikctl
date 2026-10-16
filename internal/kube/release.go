package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/text"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ZoorikKeyPrefix = "coral.zoorik.com/"

	RecommendationCRD = "recommendations.coral.zoorik.com"
	ActionRequestCRD  = "actionrequests.coral.zoorik.com"
	RolloutMarkersMap = "zoorik-rollout-markers"

	AnnotationEvictedAt          = ZoorikKeyPrefix + "evicted-at"
	AnnotationEvictionStatus     = ZoorikKeyPrefix + "eviction-status"
	LabelHpaManaged              = ZoorikKeyPrefix + "hpa-managed"
	AnnotationHpaCreated         = ZoorikKeyPrefix + "hpa-created"
	AnnotationReplicasBeforeHpa  = ZoorikKeyPrefix + "replicas-before-hpa"
	AnnotationHpaRevert          = ZoorikKeyPrefix + "hpa-revert-configuration"
	AnnotationHpaConversion      = ZoorikKeyPrefix + "hpa-conversion-original"
	AnnotationHpaReleaseStarted  = ZoorikKeyPrefix + "hpa-release-started-at"
	AnnotationVpaOriginalPolicy  = ZoorikKeyPrefix + "vpa-original-update-policy"
	AnnotationVpaTakenOverAt     = ZoorikKeyPrefix + "vpa-taken-over-at"
	AnnotationHpaAppliedSpec     = ZoorikKeyPrefix + "hpa-applied-spec"
	AnnotationCordonAction       = ZoorikKeyPrefix + "cordon-action-id"
	AnnotationRebalancingStatus  = ZoorikKeyPrefix + "rebalancing-status"
	AnnotationUncordonAfter      = ZoorikKeyPrefix + "uncordon-after"
	AnnotationDeletionCost       = "controller.kubernetes.io/pod-deletion-cost"
	ReleaseAgentStopTimeout      = 90 * time.Second
	ReleaseNamespaceWait         = 60 * time.Second
	ReleaseScaleDownWait         = 30 * time.Second
	ReleaseAgentSilence          = 15 * time.Minute
	releaseHpaPasses             = 5
	zoorikDeletionCost           = "-1000"
	zoorikFieldManagerPrefix     = "zoorik-"
	zoorikHpaWriter              = "zoorik-hpa"
	zoorikVpaWriter              = "zoorik-vpa"
	vpaModeOff                   = "Off"
	vpaUnrecorded                = "null"
	releaseStepAgent             = "agent"
	releaseStepWebhook           = "webhook"
	releaseStepTaint             = "taint"
	releaseStepHpaDelete         = "hpa-delete"
	releaseStepHpaRestore        = "hpa-restore"
	releaseStepHpaConversion     = "hpa-conversion"
	releaseStepVpa               = "vpa"
	releaseStepVpaHeld           = "vpa-held"
	releaseStepMarker            = "rollout-marker"
	releaseStepRecommendations   = "recommendations"
	releaseStepActionRequests    = "action-requests"
	releaseStepCRD               = "crd"
	releaseStepGrant             = "grant"
	releaseStepAdmissionPolicy   = "admission-policy"
	releaseStepNamespace         = "namespace"
	releaseStepNodeDaemon        = "node-daemon"
	admissionRegistrationGroup   = "admissionregistration.k8s.io"
	validatingPolicyKind         = "ValidatingAdmissionPolicy"
	validatingPolicyBindingKind  = "ValidatingAdmissionPolicyBinding"
	recommendationResourcePlural = "recommendations"
	actionRequestResourcePlural  = "actionrequests"
)

var (
	vpaResource = schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1",
		Resource: "verticalpodautoscalers"}
	crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1",
		Resource: "customresourcedefinitions"}
	hpaAnnotations = []string{AnnotationHpaCreated, AnnotationReplicasBeforeHpa, AnnotationHpaRevert,
		AnnotationHpaConversion, AnnotationHpaReleaseStarted, AnnotationHpaAppliedSpec}
	nodeAnnotations = []string{AnnotationEvictedAt, AnnotationEvictionStatus, AnnotationCordonAction,
		AnnotationRebalancingStatus, AnnotationUncordonAfter, LiveMigrationAnnotation,
		LiveMigrationKeyAnnotation}
	policyResources = map[string]schema.GroupVersionResource{
		validatingPolicyKind: {Group: admissionRegistrationGroup, Version: "v1",
			Resource: "validatingadmissionpolicies"},
		validatingPolicyBindingKind: {Group: admissionRegistrationGroup, Version: "v1",
			Resource: "validatingadmissionpolicybindings"},
	}
)

type ReleaseItem struct {
	Step      string
	Kind      string
	Namespace string
	Name      string
	Detail    string
}

func (i ReleaseItem) Describe() string {
	name := i.Name
	if i.Namespace != "" {
		name = i.Namespace + "/" + i.Name
	}
	if i.Detail == "" {
		return i.Kind + " " + name
	}
	return i.Kind + " " + name + ": " + i.Detail
}

type rolloutMarker struct {
	Namespace    string `json:"namespace"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	BaseReplicas int    `json:"baseReplicas"`
}

type ReleasePlan struct {
	Items []ReleaseItem

	agentPresent    bool
	webhookPresent  bool
	nodeDaemon      bool
	nodes           []corev1.Node
	hpas            []autoscalingv2.HorizontalPodAutoscaler
	vpas            []unstructured.Unstructured
	vpaRecords      map[string]string
	markers         []rolloutMarker
	recommendations []unstructured.Unstructured
	actionRequests  []unstructured.Unstructured
	crds            []string
	grants          []releaseGrant
	namespace       bool
}

type releaseGrant struct {
	kind      string
	namespace string
	name      string
}

func PlanRelease(ctx context.Context, target *Target) (*ReleasePlan, error) {
	plan := &ReleasePlan{}
	core := target.Clientset.CoreV1()

	if _, err := target.Clientset.AppsV1().Deployments(AgentNamespace).Get(ctx, AgentWorkload,
		metav1.GetOptions{}); err == nil {
		plan.agentPresent = true
		plan.add(releaseStepAgent, "Deployment", AgentNamespace, AgentWorkload, "scaled to 0 replicas first")
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if _, err := target.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx,
		WebhookConfiguration, metav1.GetOptions{}); err == nil {
		plan.webhookPresent = true
		plan.add(releaseStepWebhook, "MutatingWebhookConfiguration", "", WebhookConfiguration,
			"deleted before any release step, so pod creation is never mutated or blocked")
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	grants, err := labelledGrants(ctx, target)
	if err != nil {
		return nil, err
	}
	for _, grant := range grants {
		step := releaseStepGrant
		if _, policy := policyResources[grant.kind]; policy {
			step = releaseStepAdmissionPolicy
		}
		plan.grants = append(plan.grants, grant)
		plan.add(step, grant.kind, grant.namespace, grant.name, "deleted")
	}

	daemon, err := NodeDaemonInstalled(ctx, target)
	if err != nil {
		return nil, err
	}
	if daemon {
		plan.nodeDaemon = true
		plan.add(releaseStepNodeDaemon, "Namespace", "", NodeDaemonNamespace, "the live-migration daemon; "+
			"deleted before the nodes are released, so it stops writing its node annotation")
	}

	nodes, err := core.Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, node := range nodes.Items {
		detail := nodeReleaseDetail(node)
		if detail == "" {
			continue
		}
		plan.nodes = append(plan.nodes, node)
		plan.add(releaseStepTaint, "Node", "", node.Name, detail)
	}

	hpas, err := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if hpas != nil {
		for _, hpa := range hpas.Items {
			if !hpaMarked(hpa) {
				continue
			}
			plan.hpas = append(plan.hpas, hpa)
			step, detail := hpaReleasePlan(hpa)
			plan.add(step, "HorizontalPodAutoscaler", hpa.Namespace, hpa.Name, detail)
		}
	}

	recommendations, err := customResources(ctx, target, RecommendationCRD, recommendationResourcePlural,
		metav1.NamespaceAll)
	if err != nil {
		return nil, err
	}
	plan.vpaRecords = vpaStatusRecords(recommendations)

	vpas, err := target.Dynamic.Resource(vpaResource).Namespace(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{})
	if err == nil {
		for _, vpa := range vpas.Items {
			original, held := vpaHeldRecord(vpa, plan.vpaRecords)
			if !held {
				continue
			}
			plan.vpas = append(plan.vpas, vpa)
			_, detail, readErr := vpaRestoreSpec(original)
			switch {
			case vpaMode(vpa) != vpaModeOff:
				plan.add(releaseStepVpaHeld, "VerticalPodAutoscaler", vpa.GetNamespace(), vpa.GetName(),
					"left at update mode "+vpaMode(vpa)+", which someone else set after Zoorik took it over; "+
						"Zoorik's record is removed")
			case readErr != nil:
				plan.add(releaseStepVpaHeld, "VerticalPodAutoscaler", vpa.GetNamespace(), vpa.GetName(),
					"its recorded update policy "+original+" cannot be read, so it stays Off and Zoorik's "+
						"record is removed")
			default:
				plan.add(releaseStepVpa, "VerticalPodAutoscaler", vpa.GetNamespace(), vpa.GetName(), detail)
			}
		}
	} else if !absentAPI(err) {
		return nil, err
	}

	markers, err := core.ConfigMaps(AgentNamespace).Get(ctx, RolloutMarkersMap, metav1.GetOptions{})
	if err == nil {
		keys := make([]string, 0, len(markers.Data))
		for key := range markers.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var marker rolloutMarker
			if json.Unmarshal([]byte(markers.Data[key]), &marker) != nil || marker.Name == "" {
				continue
			}
			plan.markers = append(plan.markers, marker)
			plan.add(releaseStepMarker, marker.Kind, marker.Namespace, marker.Name,
				"scaled back to its "+strconv.Itoa(marker.BaseReplicas)+" base replicas if it still runs one extra")
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	plan.recommendations = recommendations
	if len(recommendations) > 0 {
		plan.add(releaseStepRecommendations, "Recommendation", "", "(all)",
			strconv.Itoa(len(recommendations))+" deleted")
	}
	actionRequests, err := customResources(ctx, target, ActionRequestCRD, actionRequestResourcePlural,
		metav1.NamespaceAll)
	if err != nil {
		return nil, err
	}
	plan.actionRequests = actionRequests
	if len(actionRequests) > 0 {
		plan.add(releaseStepActionRequests, "ActionRequest", "", "(all)",
			strconv.Itoa(len(actionRequests))+" deleted")
	}
	for _, name := range []string{RecommendationCRD, ActionRequestCRD} {
		if _, err := target.Dynamic.Resource(crdResource).Get(ctx, name, metav1.GetOptions{}); err == nil {
			plan.crds = append(plan.crds, name)
			plan.add(releaseStepCRD, "CustomResourceDefinition", "", name, "deleted")
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}

	if _, err := core.Namespaces().Get(ctx, AgentNamespace, metav1.GetOptions{}); err == nil {
		plan.namespace = true
		plan.add(releaseStepNamespace, "Namespace", "", AgentNamespace, "deleted")
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	return plan, nil
}

func (p *ReleasePlan) add(step, kind, namespace, name, detail string) {
	p.Items = append(p.Items, ReleaseItem{Step: step, Kind: kind, Namespace: namespace, Name: name,
		Detail: detail})
}

func (p *ReleasePlan) Empty() bool {
	return len(p.Items) == 0
}

func (p *ReleasePlan) Execute(ctx context.Context, target *Target, done func(ReleaseItem),
	skipped func(ReleaseItem, string), appeared func(ReleaseItem)) error {
	if p.agentPresent {
		forced, err := stopAgent(ctx, target)
		if err != nil {
			return err
		}
		detail := "scaled to 0; no agent pod is left running"
		if len(forced) > 0 {
			detail += "; force-deleted " + strings.Join(forced, ", ") + ", whose node is gone or not Ready " +
				"(if that node comes back, its kubelet stops the pod)"
		}
		done(ReleaseItem{Step: releaseStepAgent, Kind: "Deployment", Namespace: AgentNamespace,
			Name: AgentWorkload, Detail: detail})
	}
	if p.webhookPresent {
		err := target.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx,
			WebhookConfiguration, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		done(ReleaseItem{Step: releaseStepWebhook, Kind: "MutatingWebhookConfiguration",
			Name: WebhookConfiguration, Detail: "deleted"})
	}
	if err := p.releaseGrants(ctx, target, done); err != nil {
		return err
	}
	if p.nodeDaemon {
		detail, err := deleteNamespaceNamed(ctx, target, NodeDaemonNamespace)
		if err != nil {
			return err
		}
		p.nodeDaemon = false
		done(ReleaseItem{Step: releaseStepNodeDaemon, Kind: "Namespace", Name: NodeDaemonNamespace,
			Detail: "the live-migration daemon: " + detail})
	}
	if p.agentPresent || p.webhookPresent {
		fresh, err := PlanRelease(ctx, target)
		if err != nil {
			return err
		}
		planned := make(map[string]bool, len(p.Items))
		for _, item := range p.Items {
			planned[item.key()] = true
		}
		for _, item := range fresh.Items {
			if item.Step == releaseStepAgent || item.Step == releaseStepWebhook ||
				item.Step == releaseStepNodeDaemon || planned[item.key()] {
				continue
			}
			appeared(item)
		}
		p.adopt(fresh)
	}
	for _, node := range p.nodes {
		item, err := releaseNode(ctx, target, node.Name)
		if err != nil {
			return err
		}
		done(item)
	}
	for _, hpa := range p.hpas {
		item, reason, err := releaseHpa(ctx, target, hpa.Namespace, hpa.Name)
		if err != nil {
			return err
		}
		if reason != "" {
			skipped(item, reason)
			continue
		}
		done(item)
	}
	for _, vpa := range p.vpas {
		item, reason, err := releaseVpa(ctx, target, vpa.GetNamespace(), vpa.GetName(), p.vpaRecords)
		if err != nil {
			return err
		}
		if reason != "" {
			skipped(item, reason)
			continue
		}
		done(item)
	}
	for _, marker := range p.markers {
		item, reason, err := releaseMarker(ctx, target, marker)
		if err != nil {
			return err
		}
		if reason != "" {
			skipped(item, reason)
			continue
		}
		done(item)
	}
	if err := deleteCustomResources(ctx, target, RecommendationCRD, recommendationResourcePlural,
		p.recommendations); err != nil {
		return err
	}
	if len(p.recommendations) > 0 {
		done(ReleaseItem{Step: releaseStepRecommendations, Kind: "Recommendation", Name: "(all)",
			Detail: strconv.Itoa(len(p.recommendations)) + " deleted"})
	}
	if err := deleteCustomResources(ctx, target, ActionRequestCRD, actionRequestResourcePlural,
		p.actionRequests); err != nil {
		return err
	}
	if len(p.actionRequests) > 0 {
		done(ReleaseItem{Step: releaseStepActionRequests, Kind: "ActionRequest", Name: "(all)",
			Detail: strconv.Itoa(len(p.actionRequests)) + " deleted"})
	}
	for _, name := range p.crds {
		err := target.Dynamic.Resource(crdResource).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		done(ReleaseItem{Step: releaseStepCRD, Kind: "CustomResourceDefinition", Name: name,
			Detail: "deleted"})
	}
	if err := p.releaseGrants(ctx, target, done); err != nil {
		return err
	}
	if p.namespace {
		detail, err := deleteNamespace(ctx, target)
		if err != nil {
			return err
		}
		done(ReleaseItem{Step: releaseStepNamespace, Kind: "Namespace", Name: AgentNamespace, Detail: detail})
	}
	return nil
}

func (p *ReleasePlan) releaseGrants(ctx context.Context, target *Target, done func(ReleaseItem)) error {
	for len(p.grants) > 0 {
		grant := p.grants[0]
		if err := deleteGrant(ctx, target, grant); err != nil {
			return err
		}
		p.grants = p.grants[1:]
		step := releaseStepGrant
		if _, policy := policyResources[grant.kind]; policy {
			step = releaseStepAdmissionPolicy
		}
		done(ReleaseItem{Step: step, Kind: grant.kind, Namespace: grant.namespace, Name: grant.name,
			Detail: "deleted"})
	}
	return nil
}

func (i ReleaseItem) key() string {
	return i.Step + "|" + i.Kind + "|" + i.Namespace + "|" + i.Name
}

func (p *ReleasePlan) adopt(fresh *ReleasePlan) {
	p.nodes = fresh.nodes
	p.hpas = fresh.hpas
	p.vpas = fresh.vpas
	p.vpaRecords = fresh.vpaRecords
	p.markers = fresh.markers
	p.recommendations = fresh.recommendations
	p.actionRequests = fresh.actionRequests
	p.crds = fresh.crds
	p.grants = fresh.grants
	p.namespace = fresh.namespace
}

func deleteNamespace(ctx context.Context, target *Target) (string, error) {
	return deleteNamespaceNamed(ctx, target, AgentNamespace)
}

func deleteNamespaceNamed(ctx context.Context, target *Target, name string) (string, error) {
	namespaces := target.Clientset.CoreV1().Namespaces()
	err := namespaces.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	deadline := time.Now().Add(ReleaseNamespaceWait)
	for {
		namespace, err := namespaces.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return "deleted", nil
		}
		if err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "deletion started; it is still " + strings.ToLower(string(namespace.Status.Phase)) +
				" after " + ReleaseNamespaceWait.String() + ", and `" + target.Kubectl("get namespace "+
				name) + "` shows when it is gone", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func stopAgent(ctx context.Context, target *Target) ([]string, error) {
	deployments := target.Clientset.AppsV1().Deployments(AgentNamespace)
	_, err := deployments.Patch(ctx, AgentWorkload, types.MergePatchType,
		[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	deadline := time.Now().Add(ReleaseAgentStopTimeout)
	selector := metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + AgentWorkload}
	pods := target.Clientset.CoreV1().Pods(AgentNamespace)
	var forced []string
	seen := map[string]bool{}
	for {
		listed, err := pods.List(ctx, selector)
		if err != nil {
			return nil, err
		}
		running := 0
		for _, pod := range listed.Items {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			if pod.DeletionTimestamp != nil {
				reachable, err := nodeReady(ctx, target, pod.Spec.NodeName)
				if err != nil {
					return nil, err
				}
				if !reachable {
					zero := int64(0)
					err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
					if err != nil && !apierrors.IsNotFound(err) {
						return nil, err
					}
					if !seen[pod.Name] {
						seen[pod.Name] = true
						forced = append(forced, "pod "+pod.Name+nodeSuffix(pod.Spec.NodeName))
					}
					continue
				}
			}
			running++
		}
		if running == 0 {
			return forced, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the %s Deployment was scaled to 0, but %d of its pods still run after %s; "+
				"nothing was released yet, so run the disconnect again once `%s` shows none",
				AgentWorkload, running, ReleaseAgentStopTimeout,
				target.Kubectl("-n "+AgentNamespace+" get pods -l app.kubernetes.io/name="+AgentWorkload))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func nodeReady(ctx context.Context, target *Target, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	node, err := target.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

func nodeSuffix(name string) string {
	if name == "" {
		return " (never scheduled)"
	}
	return " on node " + name
}

func releaseNode(ctx context.Context, target *Target, name string) (ReleaseItem, error) {
	item := ReleaseItem{Step: releaseStepTaint, Kind: "Node", Name: name}
	nodes := target.Clientset.CoreV1().Nodes()
	for attempt := 0; attempt < 3; attempt++ {
		node, err := nodes.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			item.Detail = "gone"
			return item, nil
		}
		if err != nil {
			return item, err
		}
		detail := nodeReleaseDetail(*node)
		if detail == "" {
			item.Detail = "already released"
			return item, nil
		}
		kept := make([]corev1.Taint, 0, len(node.Spec.Taints))
		for _, taint := range node.Spec.Taints {
			if !strings.HasPrefix(taint.Key, ZoorikKeyPrefix) {
				kept = append(kept, taint)
			}
		}
		if zoorikCordon(*node) {
			node.Spec.Unschedulable = false
		}
		node.Spec.Taints = kept
		for _, key := range nodeAnnotations {
			delete(node.Annotations, key)
		}
		_, err = nodes.Update(ctx, node, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return item, err
		}
		item.Detail = detail
		return item, nil
	}
	return item, fmt.Errorf("node %s kept changing while its Zoorik taints were removed; run the "+
		"disconnect again", name)
}

func nodeReleaseDetail(node corev1.Node) string {
	var parts []string
	if keys := zoorikTaintKeys(node); len(keys) > 0 {
		parts = append(parts, "taints "+strings.Join(keys, ", ")+" removed")
	}
	if zoorikCordon(node) {
		parts = append(parts, "uncordoned, as Zoorik cordoned it")
	}
	for _, key := range nodeAnnotations {
		if _, ok := node.Annotations[key]; ok {
			parts = append(parts, "Zoorik's annotations cleared")
			break
		}
	}
	return strings.Join(parts, "; ")
}

func zoorikCordon(node corev1.Node) bool {
	if !node.Spec.Unschedulable {
		return false
	}
	for _, entry := range node.ManagedFields {
		if strings.HasPrefix(entry.Manager, zoorikFieldManagerPrefix) && fieldsOwn(entry.FieldsV1, "f:spec",
			"f:unschedulable") {
			return true
		}
	}
	return false
}

func fieldsOwn(fields *metav1.FieldsV1, path ...string) bool {
	if fields == nil || len(fields.Raw) == 0 {
		return false
	}
	var node any
	if json.Unmarshal(fields.Raw, &node) != nil {
		return false
	}
	for _, segment := range path {
		owned, ok := node.(map[string]any)
		if !ok {
			return false
		}
		if node, ok = owned[segment]; !ok {
			return false
		}
	}
	return true
}

func hpaMarked(hpa autoscalingv2.HorizontalPodAutoscaler) bool {
	if hpa.Labels[LabelHpaManaged] != "" {
		return true
	}
	for _, key := range hpaAnnotations {
		if _, ok := hpa.Annotations[key]; ok {
			return true
		}
	}
	return false
}

func hpaReleasePlan(hpa autoscalingv2.HorizontalPodAutoscaler) (string, string) {
	if hpa.Annotations[AnnotationHpaCreated] != "" {
		return releaseStepHpaDelete, "created by Zoorik; its replicas are held and then it is deleted"
	}
	var parts []string
	step := releaseStepHpaRestore
	if hpa.Annotations[AnnotationHpaRevert] != "" {
		parts = append(parts, "restored to "+hpa.Annotations[AnnotationHpaRevert])
	}
	if hpa.Annotations[AnnotationHpaConversion] != "" {
		if len(parts) == 0 {
			step = releaseStepHpaConversion
		}
		parts = append(parts, "its original metrics are restored")
	}
	if len(parts) == 0 {
		parts = append(parts, "Zoorik's label and marks are removed")
	}
	return step, strings.Join(parts, ", then ")
}

func releaseHpa(ctx context.Context, target *Target, namespace, name string) (ReleaseItem, string, error) {
	item := ReleaseItem{Step: releaseStepHpaRestore, Kind: "HorizontalPodAutoscaler", Namespace: namespace,
		Name: name}
	hpas := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace)
	var details, warnings []string
	for pass := 0; ; pass++ {
		hpa, err := hpas.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if len(details) == 0 {
				item.Step = releaseStepHpaDelete
				details = append(details, "gone")
			}
			break
		}
		if err != nil {
			return item, "", err
		}
		if !hpaMarked(*hpa) {
			if len(details) == 0 {
				details = append(details, "already released")
			}
			break
		}
		if pass == releaseHpaPasses {
			warnings = append(warnings, "Zoorik's marks are still on it after "+strconv.Itoa(pass)+
				" passes; run the disconnect again")
			break
		}
		detail, warning, err := releaseHpaOnce(ctx, target, hpa, &item, pass == 0)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return item, "", err
		}
		details = append(details, detail)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	item.Detail = strings.Join(details, "; ")
	return item, strings.Join(warnings, "; "), nil
}

func releaseHpaOnce(ctx context.Context, target *Target, hpa *autoscalingv2.HorizontalPodAutoscaler,
	item *ReleaseItem, first bool) (string, string, error) {
	hpas := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace)
	patch := func(body any) error {
		raw, _ := json.Marshal(body)
		_, err := hpas.Patch(ctx, hpa.Name, types.MergePatchType, raw, metav1.PatchOptions{})
		return err
	}
	switch {
	case hpa.Annotations[AnnotationHpaCreated] != "":
		if first {
			item.Step = releaseStepHpaDelete
		}
		detail, err := deleteCreatedHpa(ctx, target, hpa)
		return detail, "", err
	case hpa.Annotations[AnnotationHpaRevert] != "":
		body, err := hpaRestorePatch(hpa)
		if err != nil {
			return "its takeover record " + hpa.Annotations[AnnotationHpaRevert] + " is removed",
				"its takeover record could not be read, so its current bounds are kept",
				patch(hpaDropPatch(hpa, AnnotationHpaRevert))
		}
		if _, err := hpas.Patch(ctx, hpa.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
			return "", "", err
		}
		return "restored to " + hpa.Annotations[AnnotationHpaRevert], "", nil
	case hpa.Annotations[AnnotationHpaConversion] != "":
		if first {
			item.Step = releaseStepHpaConversion
		}
		var original []any
		if json.Unmarshal([]byte(hpa.Annotations[AnnotationHpaConversion]), &original) != nil {
			return "its unreadable conversion record is removed",
				"its conversion record could not be read, so its current metrics are kept",
				patch(hpaDropPatch(hpa, AnnotationHpaConversion))
		}
		if manager := hpaMetricsWriter(hpa); manager != "" && manager != zoorikHpaWriter {
			return "its metrics are kept, as " + manager + " changed them after Zoorik converted them; " +
				"Zoorik's conversion record is removed", "", patch(hpaDropPatch(hpa, AnnotationHpaConversion))
		}
		return "its original metrics are restored", "", patch(map[string]any{
			"metadata": map[string]any{
				"resourceVersion": hpa.ResourceVersion,
				"annotations":     map[string]any{AnnotationHpaConversion: nil}},
			"spec": map[string]any{"metrics": original}})
	default:
		return "Zoorik's label and marks are removed", "", patch(hpaDropPatch(hpa, ""))
	}
}

func hpaDropPatch(hpa *autoscalingv2.HorizontalPodAutoscaler, unreadable string) map[string]any {
	annotations := map[string]any{}
	for _, key := range hpaAnnotations {
		if _, ok := hpa.Annotations[key]; !ok {
			continue
		}
		if key == unreadable || (key != AnnotationHpaCreated && key != AnnotationHpaRevert &&
			key != AnnotationHpaConversion) {
			annotations[key] = nil
		}
	}
	return map[string]any{"metadata": map[string]any{
		"resourceVersion": hpa.ResourceVersion,
		"labels":          map[string]any{LabelHpaManaged: nil},
		"annotations":     annotations}}
}

func hpaMetricsWriter(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	var newest *metav1.Time
	manager := ""
	for _, entry := range hpa.ManagedFields {
		if entry.Time == nil || !fieldsOwn(entry.FieldsV1, "f:spec", "f:metrics") {
			continue
		}
		if newest == nil || entry.Time.After(newest.Time) {
			newest = entry.Time
			manager = entry.Manager
		}
	}
	return manager
}

func deleteCreatedHpa(ctx context.Context, target *Target, hpa *autoscalingv2.HorizontalPodAutoscaler) (
	string, error) {
	hpas := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace)
	held := 1
	if before, err := strconv.Atoi(strings.TrimSpace(hpa.Annotations[AnnotationReplicasBeforeHpa])); err == nil &&
		before > held {
		held = before
	}
	if hpa.Status.CurrentReplicas > int32(held) {
		held = int(hpa.Status.CurrentReplicas)
	}
	scale, scaleErr := targetScale(ctx, target, hpa.Namespace, hpa.Spec.ScaleTargetRef)
	if scaleErr == nil && scale.replicas > held {
		held = scale.replicas
	}
	hold, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": hpa.ResourceVersion},
		"spec":     map[string]any{"minReplicas": held, "maxReplicas": held}})
	if _, err := hpas.Patch(ctx, hpa.Name, types.MergePatchType, hold, metav1.PatchOptions{}); err != nil &&
		!apierrors.IsNotFound(err) {
		return "", err
	}
	if scaleErr == nil && scale.replicas < held {
		if err := setScale(ctx, target, hpa.Namespace, scale, held); err != nil {
			return "", err
		}
	}
	if err := hpas.Delete(ctx, hpa.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	if scaleErr != nil {
		return "held at " + strconv.Itoa(held) + " replicas and deleted; its workload's scale " +
			"could not be read (" + text.OneLine(scaleErr.Error()) + ")", nil
	}
	return "held at " + strconv.Itoa(held) + " replicas and deleted; the workload keeps them", nil
}

func hpaRestorePatch(hpa *autoscalingv2.HorizontalPodAutoscaler) ([]byte, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal([]byte(hpa.Annotations[AnnotationHpaRevert]), &record); err != nil {
		return nil, err
	}
	maxReplicas, ok := record["maxReplicas"]
	if !ok {
		return nil, fmt.Errorf("the record has no maxReplicas")
	}
	spec := map[string]any{"maxReplicas": maxReplicas}
	if minReplicas, ok := record["minReplicas"]; ok {
		spec["minReplicas"] = minReplicas
	}
	return json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": hpa.ResourceVersion,
			"labels":          map[string]any{LabelHpaManaged: nil},
			"annotations":     map[string]any{AnnotationHpaRevert: nil}},
		"spec": spec})
}

func vpaMarked(vpa unstructured.Unstructured) bool {
	annotations := vpa.GetAnnotations()
	_, original := annotations[AnnotationVpaOriginalPolicy]
	_, takenAt := annotations[AnnotationVpaTakenOverAt]
	return original || takenAt
}

func vpaStatusRecords(recommendations []unstructured.Unstructured) map[string]string {
	records := map[string]string{}
	for _, recommendation := range recommendations {
		uid, _, _ := unstructured.NestedString(recommendation.Object, "status", "nativeVpa", "uid")
		original, found, _ := unstructured.NestedString(recommendation.Object, "status", "nativeVpa",
			"originalUpdatePolicy")
		if uid != "" && found {
			records[uid] = original
		}
	}
	return records
}

func vpaHeldRecord(vpa unstructured.Unstructured, records map[string]string) (string, bool) {
	if vpaMarked(vpa) {
		return vpa.GetAnnotations()[AnnotationVpaOriginalPolicy], true
	}
	original, recorded := records[string(vpa.GetUID())]
	if !recorded || vpaMode(vpa) != vpaModeOff || !vpaUpdateModeWrittenByZoorik(vpa) {
		return "", false
	}
	return original, true
}

func vpaUpdateModeWrittenByZoorik(vpa unstructured.Unstructured) bool {
	for _, entry := range vpa.GetManagedFields() {
		if entry.Manager != zoorikVpaWriter || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if json.Unmarshal(entry.FieldsV1.Raw, &fields) != nil {
			continue
		}
		spec, _ := fields["f:spec"].(map[string]any)
		policy, _ := spec["f:updatePolicy"].(map[string]any)
		if _, owned := policy["f:updateMode"]; owned {
			return true
		}
	}
	return false
}

func releaseVpa(ctx context.Context, target *Target, namespace, name string,
	records map[string]string) (ReleaseItem, string, error) {
	item := ReleaseItem{Step: releaseStepVpa, Kind: "VerticalPodAutoscaler", Namespace: namespace, Name: name}
	vpas := target.Dynamic.Resource(vpaResource).Namespace(namespace)
	vpa, err := vpas.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		item.Detail = "gone"
		return item, "", nil
	}
	if err != nil {
		return item, "", err
	}
	original, held := vpaHeldRecord(*vpa, records)
	if !held {
		item.Detail = "already restored"
		return item, "", nil
	}
	dropRecord := map[string]any{
		"resourceVersion": vpa.GetResourceVersion(),
		"annotations":     map[string]any{AnnotationVpaOriginalPolicy: nil, AnnotationVpaTakenOverAt: nil}}
	spec, detail, readErr := vpaRestoreSpec(original)
	body := map[string]any{"metadata": dropRecord}
	warning := ""
	switch {
	case vpaMode(*vpa) != vpaModeOff:
		item.Step = releaseStepVpaHeld
		item.Detail = "left at update mode " + vpaMode(*vpa) + ", which someone else set after Zoorik took it " +
			"over; Zoorik's record is removed"
	case readErr != nil:
		item.Step = releaseStepVpaHeld
		item.Detail = "Zoorik's record is removed"
		warning = "its recorded update policy " + original + " could not be read, so it stays at update mode Off"
	default:
		item.Detail = detail
		body["spec"] = spec
	}
	raw, _ := json.Marshal(body)
	if _, err := vpas.Patch(ctx, name, types.MergePatchType, raw, metav1.PatchOptions{}); err != nil {
		return item, "", err
	}
	return item, warning, nil
}

func vpaRestoreSpec(original string) (map[string]any, string, error) {
	if strings.TrimSpace(original) == vpaUnrecorded {
		return map[string]any{"updatePolicy": nil}, "update policy removed, as it was before Zoorik", nil
	}
	var recorded map[string]any
	if err := json.Unmarshal([]byte(original), &recorded); err != nil {
		return nil, "", err
	}
	mode, ok := recorded["updateMode"].(string)
	if !ok || mode == "" {
		return map[string]any{"updatePolicy": map[string]any{"updateMode": nil}},
			"update mode removed, as it was before Zoorik", nil
	}
	return map[string]any{"updatePolicy": map[string]any{"updateMode": mode}},
		"update mode restored to " + mode, nil
}

func releaseMarker(ctx context.Context, target *Target, marker rolloutMarker) (ReleaseItem, string, error) {
	item := ReleaseItem{Step: releaseStepMarker, Kind: marker.Kind, Namespace: marker.Namespace,
		Name: marker.Name}
	gvk, ok := markerKinds[marker.Kind]
	if !ok {
		return item, "Zoorik does not scale this kind", nil
	}
	scale, err := targetScale(ctx, target, marker.Namespace, autoscalingv2.CrossVersionObjectReference{
		APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: marker.Name})
	if apierrors.IsNotFound(err) {
		item.Detail = "gone"
		return item, "", nil
	}
	if err != nil {
		return item, "", err
	}
	if scale.replicas != marker.BaseReplicas+1 {
		cleared, err := clearDeletionCost(ctx, target, marker.Namespace, scale.selector, 0)
		if err != nil {
			return item, "", err
		}
		item.Detail = "at " + strconv.Itoa(scale.replicas) + " replicas, not its base plus one, so left as it is" +
			clearedSuffix(cleared)
		return item, "", nil
	}
	if err := setScale(ctx, target, marker.Namespace, scale, marker.BaseReplicas); err != nil {
		return item, "", err
	}
	cleared, err := clearDeletionCost(ctx, target, marker.Namespace, scale.selector, marker.BaseReplicas)
	if err != nil {
		return item, "", err
	}
	item.Detail = "scaled back to " + strconv.Itoa(marker.BaseReplicas) + " replicas" + clearedSuffix(cleared)
	return item, "", nil
}

func clearedSuffix(cleared int) string {
	if cleared == 0 {
		return ""
	}
	return "; Zoorik's deletion-cost mark removed from " + strconv.Itoa(cleared) + " pod(s)"
}

func clearDeletionCost(ctx context.Context, target *Target, namespace, selector string, settleAt int) (int, error) {
	if selector == "" {
		return 0, nil
	}
	pods := target.Clientset.CoreV1().Pods(namespace)
	deadline := time.Now().Add(ReleaseScaleDownWait)
	for {
		listed, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return 0, err
		}
		var marked []corev1.Pod
		live := 0
		for _, pod := range listed.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			live++
			if pod.Annotations[AnnotationDeletionCost] == zoorikDeletionCost {
				marked = append(marked, pod)
			}
		}
		if len(marked) == 0 {
			return 0, nil
		}
		if settleAt > 0 && live > settleAt && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		body, _ := json.Marshal(map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{AnnotationDeletionCost: nil}}})
		cleared := 0
		for _, pod := range marked {
			_, err := pods.Patch(ctx, pod.Name, types.MergePatchType, body, metav1.PatchOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return cleared, err
			}
			cleared++
		}
		return cleared, nil
	}
}

var markerKinds = map[string]schema.GroupVersionKind{
	"Deployment":  {Group: "apps", Version: "v1", Kind: "Deployment"},
	"StatefulSet": {Group: "apps", Version: "v1", Kind: "StatefulSet"},
	"Rollout":     {Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout"},
}

type workloadScale struct {
	resource        schema.GroupVersionResource
	name            string
	replicas        int
	resourceVersion string
	selector        string
}

func targetScale(ctx context.Context, target *Target, namespace string,
	ref autoscalingv2.CrossVersionObjectReference) (workloadScale, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return workloadScale{}, err
	}
	mapping, err := target.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	if err != nil {
		return workloadScale{}, err
	}
	scale, err := target.Dynamic.Resource(mapping.Resource).Namespace(namespace).Get(ctx, ref.Name,
		metav1.GetOptions{}, "scale")
	if err != nil {
		return workloadScale{}, err
	}
	replicas, _, _ := unstructured.NestedInt64(scale.Object, "spec", "replicas")
	selector, _, _ := unstructured.NestedString(scale.Object, "status", "selector")
	return workloadScale{resource: mapping.Resource, name: ref.Name, replicas: int(replicas),
		resourceVersion: scale.GetResourceVersion(), selector: selector}, nil
}

func setScale(ctx context.Context, target *Target, namespace string, scale workloadScale, replicas int) error {
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": scale.resourceVersion},
		"spec":     map[string]any{"replicas": replicas}})
	_, err := target.Dynamic.Resource(scale.resource).Namespace(namespace).Patch(ctx, scale.name,
		types.MergePatchType, body, metav1.PatchOptions{}, "scale")
	return err
}

func customResources(ctx context.Context, target *Target, crd, plural, namespace string) (
	[]unstructured.Unstructured, error) {
	resource, ok, err := servedResource(ctx, target, crd, plural)
	if err != nil || !ok {
		return nil, err
	}
	list, err := target.Dynamic.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if absentAPI(err) {
			return nil, nil
		}
		return nil, err
	}
	return list.Items, nil
}

func deleteCustomResources(ctx context.Context, target *Target, crd, plural string,
	items []unstructured.Unstructured) error {
	if len(items) == 0 {
		return nil
	}
	resource, ok, err := servedResource(ctx, target, crd, plural)
	if err != nil || !ok {
		return err
	}
	for _, item := range items {
		client := target.Dynamic.Resource(resource).Namespace(item.GetNamespace())
		if err := client.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil &&
			!apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func servedResource(ctx context.Context, target *Target, crd, plural string) (
	schema.GroupVersionResource, bool, error) {
	definition, err := target.Dynamic.Resource(crdResource).Get(ctx, crd, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return schema.GroupVersionResource{}, false, nil
	}
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	group, _, _ := unstructured.NestedString(definition.Object, "spec", "group")
	versions, _, _ := unstructured.NestedSlice(definition.Object, "spec", "versions")
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := version["name"].(string)
		if served, _ := version["served"].(bool); served && name != "" {
			if storage, _ := version["storage"].(bool); storage {
				return schema.GroupVersionResource{Group: group, Version: name, Resource: plural}, true, nil
			}
		}
	}
	return schema.GroupVersionResource{}, false, nil
}

func labelledGrants(ctx context.Context, target *Target) ([]releaseGrant, error) {
	selector := metav1.ListOptions{LabelSelector: PartOfSelector()}
	rbac := target.Clientset.RbacV1()
	var found []releaseGrant
	clusterRoleBindings, err := rbac.ClusterRoleBindings().List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range clusterRoleBindings.Items {
		found = append(found, releaseGrant{kind: kindClusterRoleBinding, name: item.Name})
	}
	clusterRoles, err := rbac.ClusterRoles().List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range clusterRoles.Items {
		found = append(found, releaseGrant{kind: kindClusterRole, name: item.Name})
	}
	roleBindings, err := rbac.RoleBindings(metav1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range roleBindings.Items {
		if item.Namespace != AgentNamespace {
			found = append(found, releaseGrant{kind: kindRoleBinding, namespace: item.Namespace, name: item.Name})
		}
	}
	roles, err := rbac.Roles(metav1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range roles.Items {
		if item.Namespace != AgentNamespace {
			found = append(found, releaseGrant{kind: kindRole, namespace: item.Namespace, name: item.Name})
		}
	}
	for _, kind := range []string{validatingPolicyBindingKind, validatingPolicyKind} {
		list, err := target.Dynamic.Resource(policyResources[kind]).List(ctx, selector)
		if err != nil {
			if absentAPI(err) {
				continue
			}
			return nil, err
		}
		for _, item := range list.Items {
			found = append(found, releaseGrant{kind: kind, name: item.GetName()})
		}
	}
	return found, nil
}

func deleteGrant(ctx context.Context, target *Target, grant releaseGrant) error {
	rbac := target.Clientset.RbacV1()
	options := metav1.DeleteOptions{}
	var err error
	switch grant.kind {
	case kindClusterRoleBinding:
		err = rbac.ClusterRoleBindings().Delete(ctx, grant.name, options)
	case kindClusterRole:
		err = rbac.ClusterRoles().Delete(ctx, grant.name, options)
	case kindRoleBinding:
		err = rbac.RoleBindings(grant.namespace).Delete(ctx, grant.name, options)
	case kindRole:
		err = rbac.Roles(grant.namespace).Delete(ctx, grant.name, options)
	default:
		resource, ok := policyResources[grant.kind]
		if !ok {
			return nil
		}
		err = target.Dynamic.Resource(resource).Delete(ctx, grant.name, options)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func zoorikTaintKeys(node corev1.Node) []string {
	var keys []string
	for _, taint := range node.Spec.Taints {
		if strings.HasPrefix(taint.Key, ZoorikKeyPrefix) {
			keys = append(keys, taint.Key)
		}
	}
	return keys
}

func vpaMode(vpa unstructured.Unstructured) string {
	mode, found, _ := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	if !found || mode == "" {
		return "Auto"
	}
	return mode
}

func absentAPI(err error) bool {
	return apierrors.IsNotFound(err) || strings.Contains(err.Error(), "the server could not find the requested resource")
}
