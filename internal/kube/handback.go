package kube

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/text"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	AnnotationEvictedAt         = ZoorikKeyPrefix + "evicted-at"
	AnnotationEvictionStatus    = ZoorikKeyPrefix + "eviction-status"
	LabelHpaManaged             = ZoorikKeyPrefix + "hpa-managed"
	AnnotationHpaCreated        = ZoorikKeyPrefix + "hpa-created"
	AnnotationReplicasBeforeHpa = ZoorikKeyPrefix + "replicas-before-hpa"
	AnnotationHpaRevert         = ZoorikKeyPrefix + "hpa-revert-configuration"
	AnnotationHpaConversion     = ZoorikKeyPrefix + "hpa-conversion-original"
	AnnotationHpaReleaseStarted = ZoorikKeyPrefix + "hpa-release-started-at"
	AnnotationHpaAppliedSpec    = ZoorikKeyPrefix + "hpa-applied-spec"
	AnnotationVpaOriginalPolicy = ZoorikKeyPrefix + "vpa-original-update-policy"
	AnnotationVpaTakenOverAt    = ZoorikKeyPrefix + "vpa-taken-over-at"
	AnnotationCordonAction      = ZoorikKeyPrefix + "cordon-action-id"
	AnnotationRebalancingStatus = ZoorikKeyPrefix + "rebalancing-status"
	AnnotationUncordonAfter     = ZoorikKeyPrefix + "uncordon-after"
	AnnotationRebalancingFloor  = ZoorikKeyPrefix + "rebalancing-floor"
	AnnotationDoNotDisruptBy    = ZoorikKeyPrefix + "do-not-disrupt-by"
	AnnotationRestoreClaim      = ZoorikKeyPrefix + "restore-claim"
	AnnotationDoNotDisrupt      = "karpenter.sh/do-not-disrupt"
	AnnotationDeletionCost      = "controller.kubernetes.io/pod-deletion-cost"

	ReleaseScaleDownWait = 30 * time.Second

	zoorikDeletionCost       = "-1000"
	zoorikFieldManagerPrefix = "zoorik-"
	zoorikHpaWriter          = "zoorik-hpa"
	zoorikVpaWriter          = "zoorik-vpa"
	vpaModeOff               = "Off"
	vpaUnrecorded            = "null"
	scaledObjectLabel        = "scaledobject.keda.sh/name"
	kedaGroup                = "keda.sh"
	kedaDefaultMax           = 100
	handoverScaleRequested   = "SCALE_REQUESTED"
	handoverScaledUp         = "SCALED_UP"
	changedSince             = "left as it is: it changed after Zoorik set it"
	kindHPA                  = "HorizontalPodAutoscaler"
	kindScaledObject         = "ScaledObject"
	kindVPA                  = "VerticalPodAutoscaler"
	kindStatefulSet          = "StatefulSet"
	kindNode                 = "Node"
	releaseStepFloor         = "floor"
	releaseStepHpaDelete     = "hpa-delete"
	releaseStepHpaRestore    = "hpa-restore"
	releaseStepHpaConversion = "hpa-conversion"
	releaseStepVpa           = "vpa"
	releaseStepVpaHeld       = "vpa-held"
	releaseStepMarker        = "rollout-marker"
	releaseStepSurge         = "surge"
	releaseStepNode          = "node"
	releaseStepRestoreClaim  = "restore-claim"
)

var (
	vpaResource = schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1",
		Resource: "verticalpodautoscalers"}
	hpaResource = schema.GroupVersionResource{Group: "autoscaling", Version: "v2",
		Resource: "horizontalpodautoscalers"}
	scaledObjectResource = schema.GroupVersionResource{Group: kedaGroup, Version: "v1alpha1",
		Resource: "scaledobjects"}
	workloadResources = map[string]schema.GroupVersionResource{
		kindDeployment:  {Group: "apps", Version: "v1", Resource: "deployments"},
		kindStatefulSet: {Group: "apps", Version: "v1", Resource: "statefulsets"},
	}
	markerKinds = map[string]schema.GroupVersionKind{
		kindDeployment:  {Group: "apps", Version: "v1", Kind: kindDeployment},
		kindStatefulSet: {Group: "apps", Version: "v1", Kind: kindStatefulSet},
		kindRollout:     {Group: "argoproj.io", Version: "v1alpha1", Kind: kindRollout},
	}
	workloadScaleTargets = map[string]bool{"DEPLOYMENT_SCALE": true, "ROLLOUT_SCALE": true}
	hpaAnnotations       = []string{AnnotationHpaCreated, AnnotationReplicasBeforeHpa, AnnotationHpaRevert,
		AnnotationHpaConversion, AnnotationHpaReleaseStarted, AnnotationHpaAppliedSpec}
	nodeAnnotations = []string{AnnotationEvictedAt, AnnotationEvictionStatus, AnnotationCordonAction,
		AnnotationRebalancingStatus, AnnotationUncordonAfter, LiveMigrationAnnotation,
		LiveMigrationKeyAnnotation}
)

type autoscalerFloor struct {
	kind   string
	object unstructured.Unstructured
}

type floorRecord struct {
	MinBefore *int64 `json:"minBefore"`
	MaxBefore *int64 `json:"maxBefore"`
	Floor     *int64 `json:"floor"`
}

type rolloutMarker struct {
	Namespace    string `json:"namespace"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	BaseReplicas int    `json:"baseReplicas"`
	key          string
	raw          string
}

type handoverSurge struct {
	namespace string
	kind      string
	name      string
	base      int
}

type restoreClaim struct {
	kind      string
	namespace string
	name      string
	value     string
}

type workloadScale struct {
	resource schema.GroupVersionResource
	name     string
	replicas int
	selector string
}

func (p *ReleasePlan) readAutoscalers(ctx context.Context, target *Target) error {
	hpas, err := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, hpa := range hpas.Items {
		if _, floored := hpa.Annotations[AnnotationRebalancingFloor]; floored && !kedaOwned(hpa) {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&hpa)
			if err != nil {
				return err
			}
			p.floors = append(p.floors, autoscalerFloor{kind: kindHPA,
				object: unstructured.Unstructured{Object: object}})
		}
		if hpaMarked(hpa) {
			p.hpas = append(p.hpas, hpa)
		}
	}
	scaledObjects, err := target.Dynamic.Resource(scaledObjectResource).Namespace(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{})
	switch {
	case err == nil:
		for _, object := range scaledObjects.Items {
			if _, floored := object.GetAnnotations()[AnnotationRebalancingFloor]; floored {
				p.floors = append(p.floors, autoscalerFloor{kind: kindScaledObject, object: object})
			}
		}
	case !absentAPI(err):
		return err
	}

	recommendations, err := customResources(ctx, target, RecommendationCRD, recommendationResourcePlural,
		metav1.NamespaceAll)
	if err != nil {
		return err
	}
	p.vpaRecords = vpaStatusRecords(recommendations)
	vpas, err := target.Dynamic.Resource(vpaResource).Namespace(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{})
	switch {
	case err == nil:
		for _, vpa := range vpas.Items {
			if _, held := vpaHeldRecord(vpa, p.vpaRecords); held {
				p.vpas = append(p.vpas, vpa)
			}
		}
	case !absentAPI(err):
		return err
	}
	return nil
}

func (p *ReleasePlan) readMarks(ctx context.Context, target *Target) error {
	markers, err := target.Clientset.CoreV1().ConfigMaps(AgentNamespace).Get(ctx, RolloutMarkersMap,
		metav1.GetOptions{})
	switch {
	case err == nil:
		keys := make([]string, 0, len(markers.Data))
		for key := range markers.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			marker := rolloutMarker{key: key, raw: markers.Data[key]}
			if json.Unmarshal([]byte(marker.raw), &marker) != nil || marker.Name == "" {
				continue
			}
			if _, scaled := markerKinds[marker.Kind]; scaled {
				p.markers = append(p.markers, marker)
			}
		}
	case !apierrors.IsNotFound(err):
		return err
	}

	requests, err := customResources(ctx, target, ActionRequestCRD, actionRequestResourcePlural,
		metav1.NamespaceAll)
	if err != nil {
		return err
	}
	p.surges = handoverSurges(requests, p.markers)

	for _, kind := range []string{kindDeployment, kindStatefulSet} {
		list, err := target.Dynamic.Resource(workloadResources[kind]).Namespace(metav1.NamespaceAll).List(ctx,
			metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, item := range list.Items {
			if value, claimed := item.GetAnnotations()[AnnotationRestoreClaim]; claimed {
				p.claims = append(p.claims, restoreClaim{kind: kind, namespace: item.GetNamespace(),
					name: item.GetName(), value: value})
			}
		}
	}
	return nil
}

func handoverSurges(requests []unstructured.Unstructured, markers []rolloutMarker) []handoverSurge {
	seen := map[handoverSurge]bool{}
	for _, marker := range markers {
		seen[handoverSurge{namespace: marker.Namespace, kind: marker.Kind, name: marker.Name,
			base: marker.BaseReplicas}] = true
	}
	var surges []handoverSurge
	for _, request := range requests {
		rows, _, _ := unstructured.NestedSlice(request.Object, "status", "handover", "pods")
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			surge, held := surgeOf(row)
			if held && !seen[surge] {
				seen[surge] = true
				surges = append(surges, surge)
			}
		}
	}
	return surges
}

func surgeOf(row map[string]any) (handoverSurge, bool) {
	var surge handoverSurge
	surge.namespace, _, _ = unstructured.NestedString(row, "namespace")
	surge.kind, _, _ = unstructured.NestedString(row, "workloadKind")
	surge.name, _, _ = unstructured.NestedString(row, "workloadName")
	scaleTarget, _, _ := unstructured.NestedString(row, "scaleTarget")
	if _, scaled := markerKinds[surge.kind]; !scaled || surge.name == "" || !workloadScaleTargets[scaleTarget] {
		return surge, false
	}
	if state, _, _ := unstructured.NestedString(row, "state"); state == handoverScaleRequested ||
		state == handoverScaledUp {
		base, found, _ := unstructured.NestedInt64(row, "surgeBase")
		if !found {
			base, found, _ = unstructured.NestedInt64(row, "replicasBefore")
		}
		surge.base = int(base)
		return surge, found
	}
	owed, found, _ := unstructured.NestedInt64(row, "surgeOwed")
	surge.base = int(owed) - 1
	return surge, found && owed >= 1
}

func (p *ReleasePlan) handBackItems() []ReleaseItem {
	var items []ReleaseItem
	add := func(step, kind, namespace, name, detail string) {
		items = append(items, ReleaseItem{Step: step, Kind: kind, Namespace: namespace, Name: name,
			Detail: detail})
	}
	for _, floor := range p.floors {
		_, detail := floor.handBack()
		add(releaseStepFloor, floor.kind, floor.object.GetNamespace(), floor.object.GetName(), detail)
	}
	for _, hpa := range p.hpas {
		if hpa.Annotations[AnnotationHpaCreated] != "" {
			add(releaseStepHpaDelete, kindHPA, hpa.Namespace, hpa.Name,
				"created by Zoorik; held at its current replicas, then deleted")
			continue
		}
		step, _, detail := hpaHandBack(hpa)
		add(step, kindHPA, hpa.Namespace, hpa.Name, detail)
	}
	for _, vpa := range p.vpas {
		original, _ := vpaHeldRecord(vpa, p.vpaRecords)
		step, _, detail := vpaHandBack(vpa, original)
		add(step, kindVPA, vpa.GetNamespace(), vpa.GetName(), detail)
	}
	for _, marker := range p.markers {
		add(releaseStepMarker, marker.Kind, marker.Namespace, marker.Name, "scaled back to its "+
			strconv.Itoa(marker.BaseReplicas)+" replicas if it still runs the one extra copy Zoorik added")
	}
	for _, surge := range p.surges {
		add(releaseStepSurge, surge.kind, surge.namespace, surge.name, "scaled back to "+strconv.Itoa(surge.base)+
			" replicas if it still runs the one extra copy Zoorik added for a move")
	}
	for _, node := range p.nodes {
		_, detail := nodeHandBack(node)
		add(releaseStepNode, kindNode, "", node.Name, detail)
	}
	for _, claim := range p.claims {
		add(releaseStepRestoreClaim, claim.kind, claim.namespace, claim.name, "Zoorik's restore-claim note removed")
	}
	return items
}

func (p *ReleasePlan) HandBack(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	for _, floor := range p.floors {
		patch, detail := floor.handBack()
		resource := hpaResource
		if floor.kind == kindScaledObject {
			resource = scaledObjectResource
		}
		_, err := target.Dynamic.Resource(resource).Namespace(floor.object.GetNamespace()).Patch(ctx,
			floor.object.GetName(), types.JSONPatchType, patch.body(), jsonPatchOptions)
		outcome, err := outcomeOf(err)
		if err != nil {
			return err
		}
		report(settled(ReleaseItem{Step: releaseStepFloor, Kind: floor.kind, Namespace: floor.object.GetNamespace(),
			Name: floor.object.GetName()}, outcome, detail))
	}
	for _, hpa := range p.hpas {
		item, err := releaseHpa(ctx, target, hpa)
		if err != nil {
			return err
		}
		report(item)
	}
	for _, vpa := range p.vpas {
		original, _ := vpaHeldRecord(vpa, p.vpaRecords)
		step, patch, detail := vpaHandBack(vpa, original)
		outcome := patchApplied
		if len(patch) > 0 {
			_, err := target.Dynamic.Resource(vpaResource).Namespace(vpa.GetNamespace()).Patch(ctx, vpa.GetName(),
				types.JSONPatchType, patch.body(), jsonPatchOptions)
			if outcome, err = outcomeOf(err); err != nil {
				return err
			}
		}
		report(settled(ReleaseItem{Step: step, Kind: kindVPA, Namespace: vpa.GetNamespace(), Name: vpa.GetName()},
			outcome, detail))
	}
	for _, marker := range p.markers {
		detail, left, err := scaleBack(ctx, target, marker.Namespace, marker.Kind, marker.Name, marker.BaseReplicas)
		if err != nil {
			return err
		}
		var entry jsonPatch
		entry.drop(pointer("data", marker.key), marker.raw)
		_, err = target.Clientset.CoreV1().ConfigMaps(AgentNamespace).Patch(ctx, RolloutMarkersMap,
			types.JSONPatchType, entry.body(), jsonPatchOptions)
		if _, err := outcomeOf(err); err != nil {
			return err
		}
		report(ReleaseItem{Step: releaseStepMarker, Kind: marker.Kind, Namespace: marker.Namespace,
			Name: marker.Name, Detail: detail, Left: left})
	}
	for _, surge := range p.surges {
		detail, left, err := scaleBack(ctx, target, surge.namespace, surge.kind, surge.name, surge.base)
		if err != nil {
			return err
		}
		report(ReleaseItem{Step: releaseStepSurge, Kind: surge.kind, Namespace: surge.namespace, Name: surge.name,
			Detail: detail, Left: left})
	}
	for _, node := range p.nodes {
		patch, detail := nodeHandBack(node)
		_, err := target.Clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, patch.body(),
			jsonPatchOptions)
		outcome, err := outcomeOf(err)
		if err != nil {
			return err
		}
		report(settled(ReleaseItem{Step: releaseStepNode, Kind: kindNode, Name: node.Name}, outcome, detail))
	}
	for _, claim := range p.claims {
		var patch jsonPatch
		patch.drop(annotationPath(AnnotationRestoreClaim), claim.value)
		_, err := target.Dynamic.Resource(workloadResources[claim.kind]).Namespace(claim.namespace).Patch(ctx,
			claim.name, types.JSONPatchType, patch.body(), jsonPatchOptions)
		outcome, err := outcomeOf(err)
		if err != nil {
			return err
		}
		report(settled(ReleaseItem{Step: releaseStepRestoreClaim, Kind: claim.kind, Namespace: claim.namespace,
			Name: claim.name}, outcome, "Zoorik's restore-claim note removed"))
	}
	return nil
}

func settled(item ReleaseItem, outcome writeOutcome, detail string) ReleaseItem {
	switch outcome {
	case patchStale:
		item.Detail, item.Left = changedSince, true
	case patchGone:
		item.Detail = "gone"
	default:
		item.Detail = detail
	}
	return item
}

func (f autoscalerFloor) handBack() (jsonPatch, string) {
	key := annotationPath(AnnotationRebalancingFloor)
	raw := f.object.GetAnnotations()[AnnotationRebalancingFloor]
	var patch jsonPatch
	patch.test(key, raw)
	var record floorRecord
	if json.Unmarshal([]byte(raw), &record) != nil || record.MaxBefore == nil || record.Floor == nil {
		patch.remove(key)
		return patch, "its floor record cannot be read, so its bounds stay; the record is removed"
	}
	minField, maxField, liveMaxDefault := "minReplicas", "maxReplicas", int64(-1)
	if f.kind == kindScaledObject {
		minField, maxField, liveMaxDefault = "minReplicaCount", "maxReplicaCount", kedaDefaultMax
	}
	floor, maxBefore := *record.Floor, *record.MaxBefore
	maxDuring := max(maxBefore, floor)
	liveMin, minSet, _ := unstructured.NestedInt64(f.object.Object, "spec", minField)
	liveMax, maxSet, _ := unstructured.NestedInt64(f.object.Object, "spec", maxField)
	if !maxSet {
		liveMax = liveMaxDefault
	}
	if !minSet || liveMin != floor || liveMax != maxDuring {
		patch.remove(key)
		return patch, "its bounds changed after Zoorik raised its floor, so they stay; Zoorik's floor record is removed"
	}
	minPath, maxPath := pointer("spec", minField), pointer("spec", maxField)
	patch.test(minPath, floor)
	if maxSet {
		patch.test(maxPath, maxDuring)
	}
	minimum := "unset"
	if record.MinBefore == nil {
		patch.remove(minPath)
	} else {
		patch.replace(minPath, *record.MinBefore)
		minimum = strconv.FormatInt(*record.MinBefore, 10)
	}
	if liveMax != maxBefore {
		patch.add(maxPath, maxBefore)
	}
	patch.remove(key)
	return patch, "its minimum goes back to " + minimum + " and its maximum to " + strconv.FormatInt(maxBefore, 10) +
		", as before Zoorik raised its floor for a move"
}

func kedaOwned(hpa autoscalingv2.HorizontalPodAutoscaler) bool {
	if hpa.Labels[scaledObjectLabel] != "" {
		return true
	}
	for _, owner := range hpa.OwnerReferences {
		if owner.Kind == kindScaledObject && owner.Name != "" && strings.HasPrefix(owner.APIVersion, kedaGroup+"/") {
			return true
		}
	}
	return false
}

func hpaMarked(hpa autoscalingv2.HorizontalPodAutoscaler) bool {
	if _, labelled := hpa.Labels[LabelHpaManaged]; labelled {
		return true
	}
	for _, key := range hpaAnnotations {
		if _, ok := hpa.Annotations[key]; ok {
			return true
		}
	}
	return false
}

func hpaHandBack(hpa autoscalingv2.HorizontalPodAutoscaler) (string, jsonPatch, string) {
	var patch jsonPatch
	var parts []string
	step := releaseStepHpaRestore
	if revert := hpa.Annotations[AnnotationHpaRevert]; revert != "" {
		restore, bounds, readable := hpaRecordedBounds(hpa, revert)
		if readable {
			patch = append(patch, restore...)
			parts = append(parts, bounds)
		} else {
			parts = append(parts, "its takeover record cannot be read, so its bounds stay")
		}
	}
	if conversion := hpa.Annotations[AnnotationHpaConversion]; conversion != "" {
		if len(parts) == 0 {
			step = releaseStepHpaConversion
		}
		var original []any
		writer := hpaMetricsWriter(hpa)
		switch {
		case json.Unmarshal([]byte(conversion), &original) != nil:
			parts = append(parts, "its conversion record cannot be read, so its metrics stay")
		case writer != "" && writer != zoorikHpaWriter:
			parts = append(parts, "its metrics stay, as "+writer+" changed them after Zoorik converted them")
		default:
			patch.add(pointer("spec", "metrics"), original)
			parts = append(parts, "its original metrics are restored")
		}
	}
	if value, labelled := hpa.Labels[LabelHpaManaged]; labelled {
		patch.drop(labelPath(LabelHpaManaged), value)
	}
	for _, key := range hpaAnnotations {
		if value, ok := hpa.Annotations[key]; ok {
			patch.drop(annotationPath(key), value)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "Zoorik's label and notes are removed")
	}
	return step, patch, strings.Join(parts, "; ")
}

func hpaRecordedBounds(hpa autoscalingv2.HorizontalPodAutoscaler, revert string) (jsonPatch, string, bool) {
	var record struct {
		MinReplicas *int64 `json:"minReplicas"`
		MaxReplicas *int64 `json:"maxReplicas"`
	}
	if json.Unmarshal([]byte(revert), &record) != nil || record.MaxReplicas == nil {
		return nil, "", false
	}
	var patch jsonPatch
	patch.add(pointer("spec", "maxReplicas"), *record.MaxReplicas)
	minimum := "unset"
	switch {
	case record.MinReplicas != nil:
		patch.add(pointer("spec", "minReplicas"), *record.MinReplicas)
		minimum = strconv.FormatInt(*record.MinReplicas, 10)
	case hpa.Spec.MinReplicas != nil:
		patch.remove(pointer("spec", "minReplicas"))
	}
	return patch, "its minimum goes back to " + minimum + " and its maximum to " +
		strconv.FormatInt(*record.MaxReplicas, 10) + ", as before Zoorik took it over", true
}

func hpaMetricsWriter(hpa autoscalingv2.HorizontalPodAutoscaler) string {
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

func releaseHpa(ctx context.Context, target *Target, hpa autoscalingv2.HorizontalPodAutoscaler) (ReleaseItem, error) {
	item := ReleaseItem{Kind: kindHPA, Namespace: hpa.Namespace, Name: hpa.Name}
	hpas := target.Clientset.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace)
	if hpa.Annotations[AnnotationHpaCreated] == "" {
		step, patch, detail := hpaHandBack(hpa)
		item.Step = step
		_, err := hpas.Patch(ctx, hpa.Name, types.JSONPatchType, patch.body(), jsonPatchOptions)
		outcome, err := outcomeOf(err)
		return settled(item, outcome, detail), err
	}

	item.Step = releaseStepHpaDelete
	held := 1
	if before, err := strconv.Atoi(strings.TrimSpace(hpa.Annotations[AnnotationReplicasBeforeHpa])); err == nil &&
		before > held {
		held = before
	}
	held = max(held, int(hpa.Status.CurrentReplicas))
	scale, scaleErr := targetScale(ctx, target, hpa.Namespace, hpa.Spec.ScaleTargetRef)
	if scaleErr == nil {
		held = max(held, scale.replicas)
	}
	var hold jsonPatch
	hold.test(annotationPath(AnnotationHpaCreated), hpa.Annotations[AnnotationHpaCreated])
	hold.add(pointer("spec", "minReplicas"), held)
	hold.add(pointer("spec", "maxReplicas"), held)
	_, err := hpas.Patch(ctx, hpa.Name, types.JSONPatchType, hold.body(), jsonPatchOptions)
	outcome, err := outcomeOf(err)
	if err != nil || outcome != patchApplied {
		return settled(item, outcome, ""), err
	}
	detail := "held at " + strconv.Itoa(held) + " replicas and deleted; the workload keeps them"
	switch {
	case scaleErr != nil:
		detail = "held at " + strconv.Itoa(held) + " replicas and deleted; its workload's scale could not be read (" +
			text.OneLine(scaleErr.Error()) + ")"
	case scale.replicas > 0 && scale.replicas < held:
		outcome, err := setScale(ctx, target, hpa.Namespace, scale, held)
		if err != nil {
			return item, err
		}
		if outcome != patchApplied {
			detail = "deleted; its workload's replicas changed meanwhile, so they stay as they are"
		}
	}
	err = hpas.Delete(ctx, hpa.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &hpa.UID}})
	switch {
	case apierrors.IsConflict(err):
		item.Detail, item.Left = changedSince, true
		return item, nil
	case err != nil && !apierrors.IsNotFound(err):
		return item, err
	}
	item.Detail = detail
	return item, nil
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

func vpaMarked(vpa unstructured.Unstructured) bool {
	annotations := vpa.GetAnnotations()
	_, original := annotations[AnnotationVpaOriginalPolicy]
	_, takenAt := annotations[AnnotationVpaTakenOverAt]
	return original || takenAt
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
		if entry.Manager == zoorikVpaWriter && fieldsOwn(entry.FieldsV1, "f:spec", "f:updatePolicy", "f:updateMode") {
			return true
		}
	}
	return false
}

func vpaHandBack(vpa unstructured.Unstructured, original string) (string, jsonPatch, string) {
	var patch jsonPatch
	for _, key := range []string{AnnotationVpaOriginalPolicy, AnnotationVpaTakenOverAt} {
		if value, ok := vpa.GetAnnotations()[key]; ok {
			patch.drop(annotationPath(key), value)
		}
	}
	if mode := vpaMode(vpa); mode != vpaModeOff {
		return releaseStepVpaHeld, patch, "left at update mode " + mode + ", which someone else set after Zoorik " +
			"took it over; Zoorik's record is removed"
	}
	restore, detail, err := vpaRestore(vpa, original)
	if err != nil {
		return releaseStepVpaHeld, patch, "its recorded update policy cannot be read, so it stays Off; Zoorik's " +
			"record is removed"
	}
	patch.test(pointer("spec", "updatePolicy", "updateMode"), vpaModeOff)
	return releaseStepVpa, append(patch, restore...), detail
}

func vpaRestore(vpa unstructured.Unstructured, original string) (jsonPatch, string, error) {
	var patch jsonPatch
	mode := ""
	if strings.TrimSpace(original) != vpaUnrecorded {
		var recorded map[string]any
		if err := json.Unmarshal([]byte(original), &recorded); err != nil {
			return nil, "", err
		}
		mode, _ = recorded["updateMode"].(string)
	}
	if mode != "" {
		patch.replace(pointer("spec", "updatePolicy", "updateMode"), mode)
		return patch, "update mode restored to " + mode, nil
	}
	policy, _, _ := unstructured.NestedMap(vpa.Object, "spec", "updatePolicy")
	if strings.TrimSpace(original) == vpaUnrecorded && len(policy) == 1 {
		patch.remove(pointer("spec", "updatePolicy"))
		return patch, "update policy removed, as it was before Zoorik", nil
	}
	patch.remove(pointer("spec", "updatePolicy", "updateMode"))
	return patch, "update mode removed, as it was before Zoorik", nil
}

func vpaMode(vpa unstructured.Unstructured) string {
	mode, found, _ := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	if !found || mode == "" {
		return "Auto"
	}
	return mode
}

func scaleBack(ctx context.Context, target *Target, namespace, kind, name string, base int) (string, bool, error) {
	gvk := markerKinds[kind]
	scale, err := targetScale(ctx, target, namespace, autoscalingv2.CrossVersionObjectReference{
		APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: name})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return "gone", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if scale.replicas != base+1 {
		cleared, err := clearDeletionCost(ctx, target, namespace, scale.selector, 0)
		return "left as it is: it runs " + strconv.Itoa(scale.replicas) + " replicas, not the " +
			strconv.Itoa(base+1) + " Zoorik left" + clearedSuffix(cleared), true, err
	}
	outcome, err := setScale(ctx, target, namespace, scale, base)
	if err != nil {
		return "", false, err
	}
	if outcome != patchApplied {
		return settled(ReleaseItem{}, outcome, "").Detail, outcome == patchStale, nil
	}
	cleared, err := clearDeletionCost(ctx, target, namespace, scale.selector, base)
	return "scaled back to " + strconv.Itoa(base) + " replicas" + clearedSuffix(cleared), false, err
}

func clearedSuffix(cleared int) string {
	if cleared == 0 {
		return ""
	}
	return "; Zoorik's deletion-cost mark removed from " + countOf(cleared, "pod", "pods")
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
			if err := pause(ctx, 2*time.Second); err != nil {
				return 0, err
			}
			continue
		}
		var patch jsonPatch
		patch.drop(annotationPath(AnnotationDeletionCost), zoorikDeletionCost)
		cleared := 0
		for _, pod := range marked {
			_, err := pods.Patch(ctx, pod.Name, types.JSONPatchType, patch.body(), jsonPatchOptions)
			outcome, err := outcomeOf(err)
			if err != nil {
				return cleared, err
			}
			if outcome == patchApplied {
				cleared++
			}
		}
		return cleared, nil
	}
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
	return workloadScale{resource: mapping.Resource, name: ref.Name, replicas: int(replicas), selector: selector}, nil
}

func setScale(ctx context.Context, target *Target, namespace string, scale workloadScale, replicas int) (
	writeOutcome, error) {
	var patch jsonPatch
	patch.test(pointer("spec", "replicas"), scale.replicas)
	patch.replace(pointer("spec", "replicas"), replicas)
	_, err := target.Dynamic.Resource(scale.resource).Namespace(namespace).Patch(ctx, scale.name,
		types.JSONPatchType, patch.body(), jsonPatchOptions, "scale")
	return outcomeOf(err)
}

func nodeHandBack(node corev1.Node) (jsonPatch, string) {
	var patch jsonPatch
	var parts, taints []string
	for index := len(node.Spec.Taints) - 1; index >= 0; index-- {
		key := node.Spec.Taints[index].Key
		if strings.HasPrefix(key, ZoorikKeyPrefix) {
			patch.test(pointer("spec", "taints", strconv.Itoa(index), "key"), key)
			patch.remove(pointer("spec", "taints", strconv.Itoa(index)))
			taints = append([]string{key}, taints...)
		}
	}
	if len(taints) > 0 {
		parts = append(parts, "taints "+strings.Join(taints, ", ")+" removed")
	}
	if zoorikCordon(node) {
		patch.test(pointer("spec", "unschedulable"), true)
		patch.remove(pointer("spec", "unschedulable"))
		parts = append(parts, "uncordoned, as Zoorik cordoned it")
	}
	if pin := pinDrop(node); len(pin) > 0 {
		patch = append(patch, pin...)
		parts = append(parts, "Zoorik's pin removed")
	}
	noted := false
	for _, key := range nodeAnnotations {
		if value, ok := node.Annotations[key]; ok {
			patch.drop(annotationPath(key), value)
			noted = true
		}
	}
	if noted {
		parts = append(parts, "Zoorik's notes removed")
	}
	return patch, strings.Join(parts, "; ")
}

func pinDrop(node corev1.Node) jsonPatch {
	var patch jsonPatch
	holder, pinned := node.Annotations[AnnotationDoNotDisruptBy]
	if !pinned {
		return nil
	}
	patch.drop(annotationPath(AnnotationDoNotDisruptBy), holder)
	if value, ok := node.Annotations[AnnotationDoNotDisrupt]; ok {
		patch.drop(annotationPath(AnnotationDoNotDisrupt), value)
	}
	return patch
}

func zoorikCordon(node corev1.Node) bool {
	if !node.Spec.Unschedulable {
		return false
	}
	for _, entry := range node.ManagedFields {
		if (strings.HasPrefix(entry.Manager, zoorikFieldManagerPrefix) || entry.Manager == FieldManager) &&
			fieldsOwn(entry.FieldsV1, "f:spec", "f:unschedulable") {
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

func nodeMarked(node corev1.Node) bool {
	_, detail := nodeHandBack(node)
	return detail != ""
}
