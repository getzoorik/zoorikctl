package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/text"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
)

const (
	LabelNodePool            = "karpenter.sh/nodepool"
	OwnedByRebalancing       = "rebalancing"
	AnnotationDisconnectApps = ZoorikKeyPrefix + "disconnect-apps"
	AnnotationDisposable     = ZoorikKeyPrefix + "disposable"
	AnnotationSafeToEvict    = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	KindNodeClaim            = "NodeClaim"

	PoolMoveStall = 20 * time.Minute

	poolPoll             = 10 * time.Second
	cordonLiftWait       = 30 * time.Second
	releaseStepRoll      = "roll"
	releaseStepPool      = "pool"
	releaseStepBaseClass = "base-class"
	kindReplicaSet       = "ReplicaSet"
	kindRollout          = "Rollout"
	kindJob              = "Job"
	kindPod              = "Pod"
	nodeNameField        = "metadata.name"
	flagTrue             = "true"
)

var (
	coralNamespaces  = map[string]bool{AgentNamespace: true, NodeDaemonNamespace: true}
	volumeZoneLabels = map[string]bool{"topology.kubernetes.io/zone": true,
		"failure-domain.beta.kubernetes.io/zone": true, "topology.disk.csi.azure.com/zone": true,
		"topology.ebs.csi.aws.com/zone": true}
	nodeOperators = map[corev1.NodeSelectorOperator]selection.Operator{
		corev1.NodeSelectorOpIn:           selection.In,
		corev1.NodeSelectorOpNotIn:        selection.NotIn,
		corev1.NodeSelectorOpExists:       selection.Exists,
		corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
		corev1.NodeSelectorOpGt:           selection.GreaterThan,
		corev1.NodeSelectorOpLt:           selection.LessThan,
	}
	replicaKinds = map[schema.GroupKind]bool{
		{Group: "apps", Kind: kindDeployment}:      true,
		{Group: "apps", Kind: kindReplicaSet}:      true,
		{Group: "apps", Kind: kindStatefulSet}:     true,
		{Group: "", Kind: "ReplicationController"}: true,
		{Group: "argoproj.io", Kind: kindRollout}:  true,
	}
	baseClassKinds = []schema.GroupKind{{Group: AKSNodeClassGroup, Kind: KindAKSNodeClass},
		{Group: EC2NodeClassGroup, Kind: KindEC2NodeClass}}
)

type Stuck struct {
	What     string
	Serving  bool
	Draining bool
}

type poolApp struct {
	Kind       string    `json:"kind"`
	APIVersion string    `json:"apiVersion"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
}

type podHold struct {
	namespace string
	name      string
	reason    string
}

type coralPool struct {
	name    string
	uid     types.UID
	emptied bool
	apps    []poolApp
	found   []poolApp
	nodes   []corev1.Node
	holds   []podHold
}

func (c coralPool) stuck() []Stuck {
	stuck := make([]Stuck, 0, len(c.holds))
	for _, hold := range c.holds {
		stuck = append(stuck, Stuck{What: "pod " + hold.name + " in " + hold.namespace + " cannot move off NodePool " +
			c.name + ": " + hold.reason})
	}
	return stuck
}

type appRoll struct {
	namespace string
	name      string
}

type baseNodeClass struct {
	kind     string
	resource schema.GroupVersionResource
	uid      types.UID
	users    []string
}

type poolScene struct {
	coral       map[string]string
	customer    []corev1.Node
	pods        []corev1.Pod
	deployments []appsv1.Deployment
	sets        map[string]appsv1.ReplicaSet
	claims      map[string]corev1.PersistentVolumeClaim
	volumes     map[string]corev1.PersistentVolume
}

type appState struct {
	app     poolApp
	ready   int
	want    int
	running bool
}

type stallClock struct {
	since  time.Time
	seen   bool
	pods   int
	ready  int
	claims int
}

func (p *ReleasePlan) readPools(ctx context.Context, target *Target) error {
	p.nodes, p.pools, p.rolls, p.baseClass = nil, nil, nil, nil
	nodes, err := target.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pools, err := listCoralPools(ctx, target)
	if err != nil {
		return err
	}
	at := make(map[string]int, len(pools))
	for index, pool := range pools {
		at[pool.name] = index
	}
	for _, node := range nodes.Items {
		if index, owned := at[node.Labels[LabelNodePool]]; owned {
			pools[index].nodes = append(pools[index].nodes, node)
		} else if nodeMarked(node) {
			p.nodes = append(p.nodes, node)
		}
	}
	if len(pools) > 0 {
		scene, err := readScene(ctx, target, nodes.Items, pools)
		if err != nil {
			return err
		}
		p.rolls = scene.rolls()
		for index := range pools {
			if !pools[index].emptied {
				pools[index].holds = scene.holds(pools[index].name)
				pools[index].found = scene.appsOn(pools[index].name)
			}
		}
	}
	p.pools = pools
	p.baseClass, err = readBaseClass(ctx, target)
	return err
}

func listCoralPools(ctx context.Context, target *Target) ([]coralPool, error) {
	resource, served := mappedResource(target, KarpenterGroup, KindNodePool)
	if !served {
		return nil, nil
	}
	list, err := target.Dynamic.Resource(resource).List(ctx, metav1.ListOptions{
		LabelSelector: LabelOwnedBy + "=" + OwnedByRebalancing})
	if err != nil {
		if absentAPI(err) {
			return nil, nil
		}
		return nil, err
	}
	pools := make([]coralPool, 0, len(list.Items))
	for _, item := range list.Items {
		replicas, static, _ := unstructured.NestedInt64(item.Object, "spec", "replicas")
		pool := coralPool{name: item.GetName(), uid: item.GetUID(),
			emptied: item.GetDeletionTimestamp() != nil || (static && replicas == 0)}
		if recorded := item.GetAnnotations()[AnnotationDisconnectApps]; recorded != "" {
			_ = json.Unmarshal([]byte(recorded), &pool.apps)
		}
		pools = append(pools, pool)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].name < pools[j].name })
	return pools, nil
}

func readScene(ctx context.Context, target *Target, nodes []corev1.Node, pools []coralPool) (*poolScene, error) {
	scene := &poolScene{coral: map[string]string{}, sets: map[string]appsv1.ReplicaSet{},
		claims: map[string]corev1.PersistentVolumeClaim{}, volumes: map[string]corev1.PersistentVolume{}}
	for _, pool := range pools {
		for _, node := range pool.nodes {
			scene.coral[node.Name] = pool.name
		}
	}
	for _, node := range nodes {
		if _, owned := scene.coral[node.Name]; !owned {
			scene.customer = append(scene.customer, node)
		}
	}
	core, apps := target.Clientset.CoreV1(), target.Clientset.AppsV1()
	all := metav1.ListOptions{}
	pods, err := core.Pods(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, err
	}
	scene.pods = pods.Items
	deployments, err := apps.Deployments(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, err
	}
	scene.deployments = deployments.Items
	sets, err := apps.ReplicaSets(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, err
	}
	for _, set := range sets.Items {
		scene.sets[set.Namespace+"/"+set.Name] = set
	}
	claims, err := core.PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, err
	}
	for _, claim := range claims.Items {
		scene.claims[claim.Namespace+"/"+claim.Name] = claim
	}
	volumes, err := core.PersistentVolumes().List(ctx, all)
	if err != nil {
		return nil, err
	}
	for _, volume := range volumes.Items {
		scene.volumes[volume.Name] = volume
	}
	return scene, nil
}

func (s *poolScene) topController(pod corev1.Pod) (poolApp, bool) {
	owner := metav1.GetControllerOf(&pod)
	if owner == nil {
		return poolApp{}, false
	}
	app := poolApp{Kind: owner.Kind, APIVersion: owner.APIVersion, Namespace: pod.Namespace, Name: owner.Name,
		UID: owner.UID}
	set, found := s.sets[pod.Namespace+"/"+owner.Name]
	if owner.Kind != kindReplicaSet || !found {
		return app, true
	}
	if parent := metav1.GetControllerOf(&set); parent != nil && (parent.Kind == kindDeployment ||
		parent.Kind == kindRollout) {
		return poolApp{Kind: parent.Kind, APIVersion: parent.APIVersion, Namespace: pod.Namespace, Name: parent.Name,
			UID: parent.UID}, true
	}
	return app, true
}

func (s *poolScene) rolls() []appRoll {
	type copies struct{ ready, coral int }
	counted := map[types.UID]*copies{}
	for _, pod := range s.pods {
		if pod.DeletionTimestamp != nil || !isRunningAndReady(pod) {
			continue
		}
		app, found := s.topController(pod)
		if !found || app.Kind != kindDeployment {
			continue
		}
		count := counted[app.UID]
		if count == nil {
			count = &copies{}
			counted[app.UID] = count
		}
		count.ready++
		if _, owned := s.coral[pod.Spec.NodeName]; owned {
			count.coral++
		}
	}
	var rolls []appRoll
	for _, deployment := range s.deployments {
		count := counted[deployment.UID]
		if count == nil || count.coral != count.ready || coralNamespaces[deployment.Namespace] ||
			deployment.Spec.Paused || deployment.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType ||
			s.singleWriter(deployment) {
			continue
		}
		rolls = append(rolls, appRoll{namespace: deployment.Namespace, name: deployment.Name})
	}
	return rolls
}

func (s *poolScene) singleWriter(deployment appsv1.Deployment) bool {
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		claim := s.claims[deployment.Namespace+"/"+volume.PersistentVolumeClaim.ClaimName]
		if slices.Contains(claim.Spec.AccessModes, corev1.ReadWriteOnce) ||
			slices.Contains(claim.Spec.AccessModes, corev1.ReadWriteOncePod) {
			return true
		}
	}
	return false
}

func (s *poolScene) holds(pool string) []podHold {
	var holds []podHold
	for _, pod := range s.pods {
		if s.coral[pod.Spec.NodeName] != pool || podExempt(pod) || podDisposable(pod) {
			continue
		}
		if reason := s.holdReason(pod); reason != "" {
			holds = append(holds, podHold{namespace: pod.Namespace, name: pod.Name, reason: reason})
		}
	}
	return holds
}

func (s *poolScene) holdReason(pod corev1.Pod) string {
	if metav1.GetControllerOf(&pod) == nil {
		return "nothing would start it again, as no controller owns it"
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			return "it keeps data on its machine's own disk (" + volume.HostPath.Path + "), which would be lost"
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && s.pinned(pod, volume.PersistentVolumeClaim.ClaimName) {
			return "its volume " + volume.PersistentVolumeClaim.ClaimName + " can attach only to that machine"
		}
	}
	return ""
}

func (s *poolScene) pinned(pod corev1.Pod, claimName string) bool {
	claim, found := s.claims[pod.Namespace+"/"+claimName]
	if !found {
		return false
	}
	volume, found := s.volumes[claim.Spec.VolumeName]
	if !found {
		return false
	}
	if volume.Spec.Local != nil || volume.Spec.HostPath != nil {
		return true
	}
	if volume.Spec.NodeAffinity == nil || volume.Spec.NodeAffinity.Required == nil ||
		zonal(volume.Spec.NodeAffinity.Required) {
		return false
	}
	for _, node := range s.customer {
		if selectorMatches(volume.Spec.NodeAffinity.Required, node) {
			return false
		}
	}
	return true
}

func (s *poolScene) appsOn(pool string) []poolApp {
	seen := map[types.UID]bool{}
	var apps []poolApp
	for _, pod := range s.pods {
		if s.coral[pod.Spec.NodeName] != pool || podExempt(pod) {
			continue
		}
		if app, found := s.topController(pod); found && !seen[app.UID] {
			seen[app.UID] = true
			apps = append(apps, app)
		}
	}
	return apps
}

func zonal(required *corev1.NodeSelector) bool {
	zones := 0
	for _, term := range required.NodeSelectorTerms {
		if len(term.MatchFields) > 0 || len(term.MatchExpressions) == 0 {
			return false
		}
		for _, expression := range term.MatchExpressions {
			if !volumeZoneLabels[expression.Key] || expression.Operator != corev1.NodeSelectorOpIn {
				return false
			}
			zones += len(expression.Values)
		}
	}
	return zones > 0
}

func selectorMatches(selector *corev1.NodeSelector, node corev1.Node) bool {
	for _, term := range selector.NodeSelectorTerms {
		if termMatches(term, node) {
			return true
		}
	}
	return false
}

func termMatches(term corev1.NodeSelectorTerm, node corev1.Node) bool {
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	set := labels.Set(node.Labels)
	for _, expression := range term.MatchExpressions {
		operator, known := nodeOperators[expression.Operator]
		if !known {
			return false
		}
		requirement, err := labels.NewRequirement(expression.Key, operator, expression.Values)
		if err != nil || !requirement.Matches(set) {
			return false
		}
	}
	for _, field := range term.MatchFields {
		listed := slices.Contains(field.Values, node.Name)
		if field.Key != nodeNameField || (field.Operator == corev1.NodeSelectorOpIn) != listed ||
			(field.Operator != corev1.NodeSelectorOpIn && field.Operator != corev1.NodeSelectorOpNotIn) {
			return false
		}
	}
	return true
}

func podExempt(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed ||
		pod.DeletionTimestamp != nil || coralNamespaces[pod.Namespace] {
		return true
	}
	if _, mirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return true
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == kindDaemonSet {
			return true
		}
	}
	controller := metav1.GetControllerOf(&pod)
	return controller != nil && controller.Kind == kindNode
}

func podDisposable(pod corev1.Pod) bool {
	return pod.Annotations[AnnotationDisposable] == flagTrue || pod.Labels[AnnotationDisposable] == flagTrue ||
		pod.Annotations[AnnotationSafeToEvict] == flagTrue
}

func toleratesEverything(pod corev1.Pod) bool {
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == "" && toleration.Operator == corev1.TolerationOpExists &&
			(toleration.Effect == "" || toleration.Effect == corev1.TaintEffectNoSchedule) {
			return true
		}
	}
	return false
}

func readBaseClass(ctx context.Context, target *Target) (*baseNodeClass, error) {
	for _, kind := range baseClassKinds {
		resource, served := mappedResource(target, kind.Group, kind.Kind)
		if !served {
			continue
		}
		object, err := target.Dynamic.Resource(resource).Get(ctx, BaseNodeClassName, metav1.GetOptions{})
		if err != nil {
			if absentAPI(err) {
				continue
			}
			return nil, err
		}
		if object.GetLabels()[LabelOwnedBy] != OwnedByOnboarding {
			continue
		}
		users, err := baseClassUsers(ctx, target, kind.Kind)
		if err != nil {
			return nil, err
		}
		return &baseNodeClass{kind: kind.Kind, resource: resource, uid: object.GetUID(), users: users}, nil
	}
	return nil, nil
}

func baseClassUsers(ctx context.Context, target *Target, kind string) ([]string, error) {
	resource, served := mappedResource(target, KarpenterGroup, KindNodePool)
	if !served {
		return nil, nil
	}
	list, err := target.Dynamic.Resource(resource).List(ctx, metav1.ListOptions{})
	if err != nil {
		if absentAPI(err) {
			return nil, nil
		}
		return nil, err
	}
	var users []string
	for _, item := range list.Items {
		reference, _, _ := unstructured.NestedStringMap(item.Object, "spec", "template", "spec", "nodeClassRef")
		if reference["name"] == BaseNodeClassName && reference["kind"] == kind {
			users = append(users, item.GetName())
		}
	}
	return users, nil
}

func (p *ReleasePlan) poolItems() []ReleaseItem {
	var items []ReleaseItem
	for _, roll := range p.rolls {
		items = append(items, ReleaseItem{Step: releaseStepRoll, Kind: kindDeployment, Namespace: roll.namespace,
			Name: roll.name, Detail: "restarted, so its copies start on your own machines"})
	}
	coral := map[string]bool{}
	for _, pool := range p.pools {
		coral[pool.name] = true
		detail := countOf(len(pool.nodes), "machine", "machines") + "; cordoned, emptied once its apps run on " +
			"your own machines, then deleted"
		switch {
		case pool.emptied:
			detail = "already set to 0 machines; deleted once they are gone and its apps run again"
		case len(pool.holds) > 0:
			hold := pool.holds[0]
			detail = countOf(len(pool.nodes), "machine", "machines") + "; not emptied, as pod " + hold.name + " in " +
				hold.namespace + " cannot move: " + hold.reason
			if others := len(pool.holds) - 1; others > 0 {
				detail += " (and " + countOf(others, "other pod", "other pods") + ")"
			}
		}
		items = append(items, ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool.name, Detail: detail})
	}
	if base := p.baseClass; base != nil && !slices.ContainsFunc(base.users, func(user string) bool {
		return !coral[user]
	}) {
		items = append(items, ReleaseItem{Step: releaseStepBaseClass, Kind: base.kind, Name: BaseNodeClassName,
			Detail: "deleted once no NodePool uses it"})
	}
	return items
}

func (p *ReleasePlan) MovePools(ctx context.Context, target *Target, report func(ReleaseItem),
	waiting func(string)) ([]Stuck, error) {
	if err := p.readPools(ctx, target); err != nil {
		return nil, err
	}
	cordoned := map[string][]string{}
	lift := func(pool string) (bool, error) {
		names := cordoned[pool]
		delete(cordoned, pool)
		return len(names) > 0, liftCordons(ctx, target, names)
	}
	liftAll := func(stuck []Stuck, err error) ([]Stuck, error) {
		pools := make([]string, 0, len(cordoned))
		for pool := range cordoned {
			pools = append(pools, pool)
		}
		sort.Strings(pools)
		for _, pool := range pools {
			lifted, failed := lift(pool)
			if failed != nil {
				err = cmp.Or(err, failed)
				continue
			}
			if lifted {
				report(ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool,
					Detail: "cordon lifted, so its machines take pods again"})
			}
		}
		return stuck, err
	}

	var coralNames []string
	for _, pool := range p.pools {
		coralNames = append(coralNames, pool.name)
		if pool.emptied {
			continue
		}
		own, err := cordonNodes(ctx, target, pool.nodes)
		cordoned[pool.name] = own
		if err != nil {
			return liftAll(nil, err)
		}
		report(ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool.name,
			Detail: countOf(len(pool.nodes), "machine", "machines") + " cordoned, so no new pod starts there"})
	}
	if len(p.rolls) > 0 {
		at := time.Now()
		for _, roll := range p.rolls {
			if err := restart(ctx, target, roll, at); err != nil {
				return liftAll(nil, err)
			}
			report(ReleaseItem{Step: releaseStepRoll, Kind: kindDeployment, Namespace: roll.namespace, Name: roll.name,
				Detail: "restarted, so its copies start on your own machines"})
		}
		if stuck, err := waitRolls(ctx, target, p.rolls, coralNames, waiting); err != nil || len(stuck) > 0 {
			return liftAll(stuck, err)
		}
		if err := p.readPools(ctx, target); err != nil {
			return liftAll(nil, err)
		}
	}

	var stuck []Stuck
	var draining []coralPool
	for _, pool := range p.pools {
		switch {
		case pool.emptied:
			draining = append(draining, pool)
		case len(pool.holds) > 0:
			lifted, err := lift(pool.name)
			if err != nil {
				return liftAll(stuck, err)
			}
			detail := "not emptied, as a pod on it cannot move"
			if lifted {
				detail += "; its cordon is lifted, so its machines take pods again"
			}
			report(ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool.name, Detail: detail, Left: true})
			stuck = append(stuck, pool.stuck()...)
		default:
			pool.apps = unionApps(pool.apps, pool.found)
			gone, err := emptyPool(ctx, target, pool)
			if err != nil {
				return liftAll(stuck, err)
			}
			delete(cordoned, pool.name)
			if gone {
				continue
			}
			report(ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool.name, Detail: "set to 0 " +
				"machines; they are emptied under your PodDisruptionBudgets and removed"})
			draining = append(draining, pool)
		}
	}
	waited, err := waitPools(ctx, target, draining, report, waiting)
	if err != nil {
		return stuck, err
	}
	stuck = append(stuck, waited...)
	return stuck, settleBaseClass(ctx, target, report)
}

func cordonNodes(ctx context.Context, target *Target, nodes []corev1.Node) ([]string, error) {
	api := target.Clientset.CoreV1().Nodes()
	var cordon jsonPatch
	cordon.add(pointer("spec", "unschedulable"), true)
	var own []string
	for _, node := range nodes {
		switch {
		case !node.Spec.Unschedulable:
			_, err := api.Patch(ctx, node.Name, types.JSONPatchType, cordon.body(), jsonPatchOptions)
			outcome, err := outcomeOf(err)
			if err != nil {
				return own, err
			}
			if outcome == patchGone {
				continue
			}
			own = append(own, node.Name)
		case ownCordon(node):
			own = append(own, node.Name)
		}
		if pins := pinDrop(node); len(pins) > 0 {
			_, err := api.Patch(ctx, node.Name, types.JSONPatchType, pins.body(), jsonPatchOptions)
			if _, err := outcomeOf(err); err != nil {
				return own, err
			}
		}
	}
	return own, nil
}

func ownCordon(node corev1.Node) bool {
	for _, entry := range node.ManagedFields {
		if entry.Manager == FieldManager && fieldsOwn(entry.FieldsV1, "f:spec", "f:unschedulable") {
			return true
		}
	}
	return false
}

func liftCordons(ctx context.Context, target *Target, names []string) error {
	if len(names) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cordonLiftWait)
	defer cancel()
	var uncordon jsonPatch
	uncordon.test(pointer("spec", "unschedulable"), true)
	uncordon.remove(pointer("spec", "unschedulable"))
	for _, name := range names {
		_, err := target.Clientset.CoreV1().Nodes().Patch(ctx, name, types.JSONPatchType, uncordon.body(),
			jsonPatchOptions)
		if _, err := outcomeOf(err); err != nil {
			return err
		}
	}
	return nil
}

func restart(ctx context.Context, target *Target, roll appRoll, at time.Time) error {
	body, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{RestartedAtAnnotation: at.Format(time.RFC3339)}}}}})
	_, err := target.Clientset.AppsV1().Deployments(roll.namespace).Patch(ctx, roll.name, types.MergePatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func rolledOut(deployment appsv1.Deployment) bool {
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return false
	}
	want := int32(1)
	if deployment.Spec.Replicas != nil {
		want = *deployment.Spec.Replicas
	}
	status := deployment.Status
	return status.UpdatedReplicas >= want && status.Replicas <= status.UpdatedReplicas &&
		status.AvailableReplicas >= status.UpdatedReplicas
}

func waitRolls(ctx context.Context, target *Target, rolls []appRoll, pools []string, waiting func(string)) (
	[]Stuck, error) {
	clock := &stallClock{since: time.Now()}
	for {
		var pending []appsv1.Deployment
		started := 0
		for _, roll := range rolls {
			deployment, err := target.Clientset.AppsV1().Deployments(roll.namespace).Get(ctx, roll.name,
				metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			started += int(min(deployment.Status.UpdatedReplicas, deployment.Status.AvailableReplicas))
			if !rolledOut(*deployment) {
				pending = append(pending, *deployment)
			}
		}
		if len(pending) == 0 {
			return nil, nil
		}
		pods, err := podsOnPools(ctx, target, pools)
		if err != nil {
			return nil, err
		}
		if clock.stalled(len(pods), started, 0) {
			stuck := make([]Stuck, 0, len(pending))
			for _, deployment := range pending {
				stuck = append(stuck, Stuck{What: "Deployment " + deployment.Name + " in " + deployment.Namespace +
					" could not start a new copy on your machines: " + rollReason(ctx, target, deployment),
					Serving: deployment.Status.AvailableReplicas > 0})
			}
			return stuck, nil
		}
		waiting("Waiting for " + countOf(len(pending), "restarted Deployment", "restarted Deployments") +
			" to start their copies on your own machines...")
		if err := pause(ctx, poolPoll); err != nil {
			return nil, err
		}
	}
}

func emptyPool(ctx context.Context, target *Target, pool coralPool) (bool, error) {
	resource, served := mappedResource(target, KarpenterGroup, KindNodePool)
	if !served {
		return true, nil
	}
	record, _ := json.Marshal(pool.apps)
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{AnnotationDisconnectApps: string(record)}},
		"spec":     map[string]any{"replicas": 0}})
	_, err := target.Dynamic.Resource(resource).Patch(ctx, pool.name, types.MergePatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

func waitPools(ctx context.Context, target *Target, pools []coralPool, report func(ReleaseItem),
	waiting func(string)) ([]Stuck, error) {
	clock := &stallClock{since: time.Now()}
	for len(pools) > 0 {
		var pending []coralPool
		var names []string
		ready, claims := 0, 0
		for _, pool := range pools {
			left, err := poolClaims(ctx, target, pool.name)
			if err != nil {
				return nil, err
			}
			states, err := appStates(ctx, target, pool.apps)
			if err != nil {
				return nil, err
			}
			running, started := true, 0
			for _, state := range states {
				started += state.ready
				running = running && state.running
			}
			if left == 0 && running {
				if err := deletePool(ctx, target, pool); err != nil {
					return nil, err
				}
				report(ReleaseItem{Step: releaseStepPool, Kind: KindNodePool, Name: pool.name,
					Detail: "deleted, with the node classes it owns"})
				continue
			}
			ready += started
			claims += left
			pending = append(pending, pool)
			names = append(names, pool.name)
		}
		if len(pending) == 0 {
			break
		}
		if len(pending) < len(pools) {
			clock = &stallClock{since: time.Now()}
		}
		pools = pending
		pods, err := podsOnPools(ctx, target, names)
		if err != nil {
			return nil, err
		}
		if clock.stalled(len(pods), ready, claims) {
			return poolStuck(ctx, target, pools, pods)
		}
		waiting(fmt.Sprintf("Waiting for %s to empty: %s and %s left...", countOf(len(pools), "NodePool",
			"NodePools"), countOf(claims, "machine", "machines"), countOf(len(pods), "pod", "pods")))
		if err := pause(ctx, poolPoll); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (c *stallClock) stalled(pods, ready, claims int) bool {
	now := time.Now()
	if c.seen && (pods < c.pods || ready > c.ready || claims < c.claims) {
		c.since = now
	}
	if !c.seen || pods < c.pods {
		c.pods = pods
	}
	if !c.seen || ready > c.ready {
		c.ready = ready
	}
	if !c.seen || claims < c.claims {
		c.claims = claims
	}
	c.seen = true
	return now.Sub(c.since) >= PoolMoveStall
}

func poolClaims(ctx context.Context, target *Target, pool string) (int, error) {
	resource, served := mappedResource(target, KarpenterGroup, KindNodeClaim)
	if !served {
		return 0, nil
	}
	list, err := target.Dynamic.Resource(resource).List(ctx, metav1.ListOptions{LabelSelector: LabelNodePool + "=" +
		pool})
	if err != nil {
		if absentAPI(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(list.Items), nil
}

func podsOnPools(ctx context.Context, target *Target, pools []string) ([]corev1.Pod, error) {
	if len(pools) == 0 {
		return nil, nil
	}
	core := target.Clientset.CoreV1()
	nodes, err := core.Nodes().List(ctx, metav1.ListOptions{LabelSelector: LabelNodePool + " in (" +
		strings.Join(pools, ",") + ")"})
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	for _, node := range nodes.Items {
		listed, err := core.Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" +
			node.Name})
		if err != nil {
			return nil, err
		}
		for _, pod := range listed.Items {
			if !podExempt(pod) {
				pods = append(pods, pod)
			}
		}
	}
	return pods, nil
}

func appStates(ctx context.Context, target *Target, apps []poolApp) ([]appState, error) {
	states := make([]appState, 0, len(apps))
	for _, app := range apps {
		state, err := appStateOf(ctx, target, app)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func appStateOf(ctx context.Context, target *Target, app poolApp) (appState, error) {
	state := appState{app: app, running: true}
	version, err := schema.ParseGroupVersion(app.APIVersion)
	if err != nil {
		return state, nil
	}
	kind := schema.GroupKind{Group: version.Group, Kind: app.Kind}
	mapping, err := target.Mapper.RESTMapping(kind, version.Version)
	if meta.IsNoMatchError(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	object, err := target.Dynamic.Resource(mapping.Resource).Namespace(app.Namespace).Get(ctx, app.Name,
		metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && object.GetUID() != app.UID) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	switch {
	case replicaKinds[kind]:
		want, found, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
		if !found {
			want = 1
		}
		ready, _, _ := unstructured.NestedInt64(object.Object, "status", "readyReplicas")
		state.ready, state.want, state.running = int(ready), int(want), ready >= want
	case kind == schema.GroupKind{Group: "batch", Kind: kindJob}:
		ready, _, _ := unstructured.NestedInt64(object.Object, "status", "ready")
		state.ready, state.want, state.running = int(ready), 1, ready > 0 || jobFinished(*object)
	default:
		pods, err := target.Clientset.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return state, err
		}
		for _, pod := range pods.Items {
			owner := metav1.GetControllerOf(&pod)
			if owner != nil && owner.UID == app.UID && pod.DeletionTimestamp == nil {
				state.want++
				if isRunningAndReady(pod) {
					state.ready++
				}
			}
		}
		state.running = state.want > 0 && state.ready == state.want
	}
	return state, nil
}

func jobFinished(job unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if (condition["type"] == "Complete" || condition["type"] == "Failed") && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func deletePool(ctx context.Context, target *Target, pool coralPool) error {
	resource, served := mappedResource(target, KarpenterGroup, KindNodePool)
	if !served {
		return nil
	}
	err := target.Dynamic.Resource(resource).Delete(ctx, pool.name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pool.uid}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

func unionApps(recorded, found []poolApp) []poolApp {
	seen := map[types.UID]bool{}
	var all []poolApp
	for _, app := range slices.Concat(recorded, found) {
		if !seen[app.UID] {
			seen[app.UID] = true
			all = append(all, app)
		}
	}
	return all
}

func poolStuck(ctx context.Context, target *Target, pools []coralPool, pods []corev1.Pod) ([]Stuck, error) {
	var stuck []Stuck
	for _, pod := range pods {
		if !toleratesEverything(pod) {
			stuck = append(stuck, Stuck{What: "pod " + pod.Name + " in " + pod.Namespace + " cannot leave machine " +
				pod.Spec.NodeName + ": " + leaveReason(ctx, target, pod), Draining: true})
		}
	}
	seen := map[types.UID]bool{}
	for _, pool := range pools {
		for _, app := range pool.apps {
			if seen[app.UID] {
				continue
			}
			seen[app.UID] = true
			state, err := appStateOf(ctx, target, app)
			if err != nil {
				return nil, err
			}
			if !state.running {
				stuck = append(stuck, Stuck{What: app.Kind + " " + app.Name + " in " + app.Namespace +
					" could not start again on your machines: " + startReason(ctx, target, state), Draining: true})
			}
		}
	}
	if len(stuck) > 0 {
		return stuck, nil
	}
	for _, pool := range pools {
		left, err := poolClaims(ctx, target, pool.name)
		if err != nil {
			return nil, err
		}
		if left > 0 {
			stuck = append(stuck, Stuck{What: "NodePool " + pool.name + " still has " +
				countOf(left, "machine", "machines") + ": " + text.OrText(poolWarning(ctx, target, pool.name),
				"they have not been removed yet"), Draining: true})
		}
	}
	return stuck, nil
}

func poolWarning(ctx context.Context, target *Target, pool string) string {
	newest, newestAt := "", time.Time{}
	consider := func(kind, name string) {
		if warning, at := warningOf(ctx, target, metav1.NamespaceAll, kind, name); warning != "" && at.After(newestAt) {
			newest, newestAt = warning, at
		}
	}
	selector := metav1.ListOptions{LabelSelector: LabelNodePool + "=" + pool}
	if nodes, err := target.Clientset.CoreV1().Nodes().List(ctx, selector); err == nil {
		for _, node := range nodes.Items {
			consider(kindNode, node.Name)
		}
	}
	if resource, served := mappedResource(target, KarpenterGroup, KindNodeClaim); served {
		if claims, err := target.Dynamic.Resource(resource).List(ctx, selector); err == nil {
			for _, claim := range claims.Items {
				consider(KindNodeClaim, claim.GetName())
			}
		}
	}
	return newest
}

func leaveReason(ctx context.Context, target *Target, pod corev1.Pod) string {
	budgets, err := target.Clientset.PolicyV1().PodDisruptionBudgets(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, budget := range budgets.Items {
			selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
			if err == nil && !selector.Empty() && selector.Matches(labels.Set(pod.Labels)) &&
				budget.Status.DisruptionsAllowed == 0 {
				return "PodDisruptionBudget " + budget.Name + " allows no disruption now"
			}
		}
	}
	if pod.Annotations[AnnotationDoNotDisrupt] == flagTrue {
		return "it is marked " + AnnotationDoNotDisrupt
	}
	warning, _ := warningOf(ctx, target, pod.Namespace, kindPod, pod.Name)
	return text.OrText(warning, "it has not left yet")
}

func startReason(ctx context.Context, target *Target, state appState) string {
	namespace := state.app.Namespace
	owners := map[types.UID]bool{state.app.UID: true}
	if sets, err := target.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, set := range sets.Items {
			if owner := metav1.GetControllerOf(&set); owner != nil && owner.UID == state.app.UID {
				owners[set.UID] = true
			}
		}
	}
	if pods, err := target.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pod := range newestFirst(pods.Items) {
			owner := metav1.GetControllerOf(&pod)
			if owner != nil && owners[owner.UID] && pod.DeletionTimestamp == nil && !isRunningAndReady(pod) {
				return podReason(ctx, target, pod)
			}
		}
	}
	if state.want > 0 {
		return "it runs " + strconv.Itoa(state.ready) + " of its " + countOf(state.want, "copy", "copies")
	}
	return "it has not started again yet"
}

func rollReason(ctx context.Context, target *Target, deployment appsv1.Deployment) string {
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err == nil {
		pods, err := target.Clientset.CoreV1().Pods(deployment.Namespace).List(ctx,
			metav1.ListOptions{LabelSelector: selector.String()})
		if err == nil {
			for _, pod := range newestFirst(pods.Items) {
				if pod.DeletionTimestamp == nil && !isRunningAndReady(pod) {
					return podReason(ctx, target, pod)
				}
			}
		}
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse {
			return text.OneLine(condition.Message)
		}
	}
	return "its new copies are not ready yet"
}

func podReason(ctx context.Context, target *Target, pod corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
			condition.Message != "" {
			return "no machine can take pod " + pod.Name + ": " + text.OneLine(condition.Message)
		}
	}
	if warning, _ := warningOf(ctx, target, pod.Namespace, kindPod, pod.Name); warning != "" {
		return "pod " + pod.Name + ": " + warning
	}
	waiting, _ := waitingReason(pod)
	return "pod " + pod.Name + " is not ready: " + waiting
}

func newestFirst(pods []corev1.Pod) []corev1.Pod {
	sorted := slices.Clone(pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[j].CreationTimestamp.Before(&sorted[i].CreationTimestamp)
	})
	return sorted
}

func warningOf(ctx context.Context, target *Target, namespace, kind, name string) (string, time.Time) {
	events, err := target.Clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=" +
		corev1.EventTypeWarning + ",involvedObject.kind=" + kind + ",involvedObject.name=" + name})
	if err != nil {
		return "", time.Time{}
	}
	newest, newestAt := "", time.Time{}
	for _, event := range events.Items {
		at := event.LastTimestamp.Time
		if event.EventTime.After(at) {
			at = event.EventTime.Time
		}
		if newest == "" || at.After(newestAt) {
			newest, newestAt = event.Reason+": "+text.OneLine(event.Message), at
		}
	}
	return newest, newestAt
}

func settleBaseClass(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	base, err := readBaseClass(ctx, target)
	if err != nil || base == nil || len(base.users) > 0 {
		return err
	}
	err = target.Dynamic.Resource(base.resource).Delete(ctx, BaseNodeClassName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &base.uid}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return err
	}
	report(ReleaseItem{Step: releaseStepBaseClass, Kind: base.kind, Name: BaseNodeClassName, Detail: "deleted"})
	return nil
}
