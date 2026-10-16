package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

const (
	KarpenterGroup = "karpenter.sh"

	KindNodePool = "NodePool"

	AKSNodeClassGroup = "karpenter.azure.com"

	KindAKSNodeClass = "AKSNodeClass"

	EC2NodeClassGroup = "karpenter.k8s.aws"

	KindEC2NodeClass = "EC2NodeClass"

	AutoModeNodeClassGroup = "eks.amazonaws.com"

	KindAutoModeNodeClass = "NodeClass"

	KarpenterNamespace = "kube-system"

	KarpenterFeatureGatesEnv = "FEATURE_GATES"

	KarpenterFeatureGatesFlag = "--feature-gates="

	KarpenterStaticCapacityGate = "StaticCapacity"

	KarpenterInterruptionQueueEnv = "INTERRUPTION_QUEUE"

	KarpenterControllerImageMark = "/karpenter/controller"

	KarpenterServiceAccount = "karpenter"

	karpenterSelector = "app.kubernetes.io/name=karpenter"

	controllerDeploymentPage = 500

	KarpenterOpenAPIPath = "/openapi/v3/apis/" + KarpenterGroup + "/v1"

	NodePoolReplicasPointer = "/components/schemas/sh.karpenter.v1.NodePool/properties/spec/properties/replicas"

	HelmReleaseNameAnnotation = "meta.helm.sh/release-name"

	HelmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"

	InstanceLabelKey = "app.kubernetes.io/instance"

	ClusterAutoscalerStatus = "cluster-autoscaler-status"

	ClusterAutoscalerStatusKey = "status"

	legacyNodeGroupsHeader = "NodeGroups:"

	legacyNodeGroupName = "Name:"

	autoscalerInitializing = "Initializing"

	LabelOwnedBy = ZoorikKeyPrefix + "owned-by"

	OwnedByOnboarding = "onboarding"

	BaseNodeClassName = "zoorik-base"

	DefaultProvisionerName = "default"

	ZoorikPoolPrefix = "zoorik-"

	PrepareFieldManager = "zoorik-prepare"
)

type Autoprovisioner struct {
	NodePools                  schema.GroupVersionResource
	NodePoolsServed            bool
	AKSNodeClasses             schema.GroupVersionResource
	AKSClassesServed           bool
	EC2NodeClasses             schema.GroupVersionResource
	EC2ClassesServed           bool
	AutoModeServed             bool
	ClusterAutoscaler          bool
	ClusterAutoscalerNamespace string
	Controllers                []ControllerState
	Pools                      []AutoprovisionerPool
	Classes                    []AutoprovisionerClass
}

type ControllerState struct {
	Namespace         string
	Name              string
	Ready             int32
	Desired           int32
	Image             string
	StaticCapacity    bool
	ReleaseName       string
	ReleaseNamespace  string
	InterruptionQueue string
}

type EC2NodeClassSource struct {
	NodePool                   string
	NodeClass                  string
	Role                       string
	InstanceProfile            string
	SubnetSelectorTerms        []any
	SecurityGroupSelectorTerms []any
}

type autoscalerStatusDocument struct {
	AutoscalerStatus string `json:"autoscalerStatus"`
	NodeGroups       []struct {
		Name string `json:"name"`
	} `json:"nodeGroups"`
}

type AutoprovisionerPool struct {
	Name          string
	NodeClass     string
	Weight        string
	LimitsCPU     string
	ZoorikOwned   bool
	CreatesNoNode bool
}

type AutoprovisionerClass struct {
	Kind        string
	Name        string
	ImageFamily string
	ZoorikOwned bool
}

func (a Autoprovisioner) KarpenterRunning() bool {
	for _, controller := range a.Controllers {
		if controller.Ready > 0 {
			return true
		}
	}
	return false
}

func (a Autoprovisioner) KarpenterStaticCapacity() bool {
	for _, controller := range a.Controllers {
		if controller.Ready > 0 && controller.StaticCapacity {
			return true
		}
	}
	return false
}

func (a Autoprovisioner) KarpenterVersion() string {
	for _, controller := range a.Controllers {
		if controller.Ready > 0 {
			if version := imageTag(controller.Image); version != "" {
				return version
			}
		}
	}
	return ""
}

func (a Autoprovisioner) Resource(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case KindNodePool:
		return a.NodePools, a.NodePoolsServed
	case KindAKSNodeClass:
		return a.AKSNodeClasses, a.AKSClassesServed
	case KindEC2NodeClass:
		return a.EC2NodeClasses, a.EC2ClassesServed
	}
	return schema.GroupVersionResource{}, false
}

func (a Autoprovisioner) HasCustomerNodePool() bool {
	for _, pool := range a.Pools {
		if !pool.ZoorikOwned {
			return true
		}
	}
	return false
}

func (a Autoprovisioner) CustomerClass(kind string) (string, bool) {
	chosen := ""
	for _, class := range a.Classes {
		if class.Kind != kind || class.ZoorikOwned {
			continue
		}
		if class.Name == DefaultProvisionerName {
			return class.Name, true
		}
		if chosen == "" {
			chosen = class.Name
		}
	}
	return chosen, chosen != ""
}

func (a Autoprovisioner) Has(kind, name string) bool {
	if kind == KindNodePool {
		for _, pool := range a.Pools {
			if pool.Name == name {
				return true
			}
		}
		return false
	}
	for _, class := range a.Classes {
		if class.Kind == kind && class.Name == name {
			return true
		}
	}
	return false
}

func (t *Target) Rediscover() error {
	discoveryClient, err := t.discoveryClient()
	if err != nil {
		return err
	}
	groups, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return err
	}
	t.Mapper = restmapper.NewDiscoveryRESTMapper(groups)
	return nil
}

func (t *Target) discoveryClient() (*discovery.DiscoveryClient, error) {
	if t.restConfig == nil {
		return nil, fmt.Errorf("the cluster was never connected")
	}
	return discovery.NewDiscoveryClientForConfig(t.restConfig)
}

func mappedResource(target *Target, group, kind string) (schema.GroupVersionResource, bool) {
	mapping, err := target.Mapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind})
	if err != nil || mapping.Scope.Name() != meta.RESTScopeNameRoot {
		return schema.GroupVersionResource{}, false
	}
	return mapping.Resource, true
}

func DetectAutoprovisioner(ctx context.Context, target *Target) (Autoprovisioner, error) {
	var found Autoprovisioner
	found.NodePools, found.NodePoolsServed = mappedResource(target, KarpenterGroup, KindNodePool)
	found.AKSNodeClasses, found.AKSClassesServed = mappedResource(target, AKSNodeClassGroup, KindAKSNodeClass)
	found.EC2NodeClasses, found.EC2ClassesServed = mappedResource(target, EC2NodeClassGroup, KindEC2NodeClass)
	_, found.AutoModeServed = mappedResource(target, AutoModeNodeClassGroup, KindAutoModeNodeClass)

	if found.NodePoolsServed {
		list, err := target.Dynamic.Resource(found.NodePools).List(ctx, metav1.ListOptions{})
		if err != nil {
			return found, readRefused(target, err, "NodePools", "get nodepools.karpenter.sh")
		}
		for _, item := range list.Items {
			found.Pools = append(found.Pools, poolOf(item))
		}
	}
	for _, class := range []struct {
		served bool
		gvr    schema.GroupVersionResource
		kind   string
		field  string
	}{
		{found.AKSClassesServed, found.AKSNodeClasses, KindAKSNodeClass, "imageFamily"},
		{found.EC2ClassesServed, found.EC2NodeClasses, KindEC2NodeClass, "amiFamily"},
	} {
		if !class.served {
			continue
		}
		list, err := target.Dynamic.Resource(class.gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return found, readRefused(target, err, class.kind+" objects",
				"get "+class.gvr.Resource+"."+class.gvr.Group)
		}
		for _, item := range list.Items {
			family, _, _ := unstructured.NestedString(item.Object, "spec", class.field)
			if family == "" && class.kind == KindEC2NodeClass {
				family = amiAliasFamily(item)
			}
			found.Classes = append(found.Classes, AutoprovisionerClass{Kind: class.kind, Name: item.GetName(),
				ImageFamily: family, ZoorikOwned: zoorikOwned(item)})
		}
	}
	sort.Slice(found.Pools, func(i, j int) bool { return found.Pools[i].Name < found.Pools[j].Name })
	sort.Slice(found.Classes, func(i, j int) bool {
		return found.Classes[i].Kind+found.Classes[i].Name < found.Classes[j].Kind+found.Classes[j].Name
	})

	status, err := clusterAutoscalerStatus(ctx, target)
	if err != nil {
		return found, err
	}
	if status != nil {
		found.ClusterAutoscaler = true
		found.ClusterAutoscalerNamespace = status.Namespace
	}

	controllers, err := karpenterControllers(ctx, target)
	switch {
	case err == nil:
		found.Controllers = controllers
	case !apierrors.IsForbidden(err):
		return found, readRefused(target, err, "Deployments", "get deployments -A")
	}
	return found, nil
}

func karpenterControllers(ctx context.Context, target *Target) ([]ControllerState, error) {
	deployments := target.Clientset.AppsV1().Deployments(metav1.NamespaceAll)
	labelled, err := deployments.List(ctx, metav1.ListOptions{LabelSelector: karpenterSelector})
	if err != nil {
		return nil, err
	}
	var controllers []ControllerState
	for _, deployment := range labelled.Items {
		controllers = append(controllers, controllerOf(deployment))
	}
	if len(controllers) > 0 {
		return controllers, nil
	}
	options := metav1.ListOptions{Limit: controllerDeploymentPage}
	for {
		page, err := deployments.List(ctx, options)
		if err != nil {
			return nil, err
		}
		for _, deployment := range page.Items {
			if _, marked := karpenterContainer(deployment.Spec.Template.Spec.Containers); marked {
				controllers = append(controllers, controllerOf(deployment))
			}
		}
		if page.Continue == "" {
			return controllers, nil
		}
		options.Continue = page.Continue
	}
}

func controllerOf(deployment appsv1.Deployment) ControllerState {
	state := ControllerState{
		Namespace:        deployment.Namespace,
		Name:             deployment.Name,
		Ready:            deployment.Status.ReadyReplicas,
		ReleaseName:      deployment.Annotations[HelmReleaseNameAnnotation],
		ReleaseNamespace: deployment.Annotations[HelmReleaseNamespaceAnnotation],
	}
	if state.ReleaseName == "" {
		state.ReleaseName = deployment.Labels[InstanceLabelKey]
	}
	if state.ReleaseNamespace == "" {
		state.ReleaseNamespace = deployment.Namespace
	}
	if deployment.Spec.Replicas != nil {
		state.Desired = *deployment.Spec.Replicas
	}
	containers := deployment.Spec.Template.Spec.Containers
	controller, marked := karpenterContainer(containers)
	if !marked && len(containers) > 0 {
		controller = containers[0]
	}
	state.Image = controller.Image
	state.InterruptionQueue = envValue(controller.Env, KarpenterInterruptionQueueEnv)
	state.StaticCapacity = staticCapacityOn(containers)
	return state
}

func karpenterContainer(containers []corev1.Container) (corev1.Container, bool) {
	for _, container := range containers {
		if strings.Contains(imageRepository(container.Image), KarpenterControllerImageMark) {
			return container, true
		}
	}
	return corev1.Container{}, false
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, variable := range env {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
}

func poolOf(item unstructured.Unstructured) AutoprovisionerPool {
	pool := AutoprovisionerPool{Name: item.GetName(), ZoorikOwned: zoorikOwned(item)}
	pool.NodeClass, _, _ = unstructured.NestedString(item.Object, "spec", "template", "spec", "nodeClassRef", "name")
	if weight, found, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "weight"); found {
		pool.Weight = fmt.Sprint(weight)
	}
	cpu, cpuFound, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "limits", "cpu")
	if cpuFound {
		pool.LimitsCPU = fmt.Sprint(cpu)
	}
	pool.CreatesNoNode = cpuFound && zeroQuantity(cpu)
	return pool
}

func zeroQuantity(value any) bool {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return false
		}
		number := strings.TrimRight(trimmed, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
		parsed, err := strconv.ParseFloat(number, 64)
		return err == nil && parsed == 0
	case int64:
		return typed == 0
	case float64:
		return typed == 0
	}
	return false
}

func zoorikOwned(item unstructured.Unstructured) bool {
	_, owned := item.GetLabels()[LabelOwnedBy]
	return owned || strings.HasPrefix(item.GetName(), ZoorikPoolPrefix)
}

func amiAliasFamily(item unstructured.Unstructured) string {
	terms, _, _ := unstructured.NestedSlice(item.Object, "spec", "amiSelectorTerms")
	for _, term := range terms {
		entry, ok := term.(map[string]any)
		if !ok {
			continue
		}
		if alias, ok := entry["alias"].(string); ok && alias != "" {
			family, _, _ := strings.Cut(alias, "@")
			return family
		}
	}
	return ""
}

func readRefused(target *Target, err error, what, kubectl string) error {
	return refusal.Wrap(refusal.CodePermission, err,
		"This cluster would not list its "+what+", so what provisions its nodes cannot be read.",
		"The context's credential needs to read them: `"+target.Kubectl(kubectl)+"`. Nothing has been changed.")
}

func karpenterObject(gvr schema.GroupVersionResource, kind, name string, spec map[string]any) map[string]any {
	metadata := map[string]any{"name": name}
	if name == BaseNodeClassName {
		metadata["labels"] = map[string]any{
			LabelOwnedBy:     OwnedByOnboarding,
			PartOfLabelKey:   PartOfLabelValue,
			InstanceLabelKey: BaseNodeClassName,
		}
	}
	return map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   metadata,
		"spec":       spec,
	}
}

func CreateKarpenterObject(ctx context.Context, target *Target, gvr schema.GroupVersionResource, kind, name string,
	spec map[string]any) error {
	object := &unstructured.Unstructured{Object: karpenterObject(gvr, kind, name, spec)}
	_, err := target.Dynamic.Resource(gvr).Create(ctx, object, metav1.CreateOptions{FieldManager: PrepareFieldManager})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func KarpenterObjectManifest(gvr schema.GroupVersionResource, kind, name string, spec map[string]any) string {
	encoded, err := json.MarshalIndent(karpenterObject(gvr, kind, name, spec), "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}

type KarpenterStaticCapacity struct {
	GateOn         bool
	ReplicasServed bool
}

func (s KarpenterStaticCapacity) Ready() bool {
	return s.GateOn && s.ReplicasServed
}

func ReadKarpenterStaticCapacity(ctx context.Context, target *Target) (KarpenterStaticCapacity, error) {
	controllers, err := karpenterControllers(ctx, target)
	if err != nil {
		return KarpenterStaticCapacity{}, err
	}
	state := KarpenterStaticCapacity{GateOn: Autoprovisioner{Controllers: controllers}.KarpenterStaticCapacity()}
	if !state.GateOn {
		return state, nil
	}
	served, err := NodePoolServesReplicas(ctx, target)
	if err != nil {
		return state, err
	}
	state.ReplicasServed = served
	return state, nil
}

func KarpenterReadyWithStaticCapacity(ctx context.Context, target *Target) (bool, error) {
	state, err := ReadKarpenterStaticCapacity(ctx, target)
	if err != nil {
		return false, err
	}
	return state.Ready(), nil
}

func VersionAtLeast(version string, major, minor int) bool {
	return versionAtLeast(version, major, minor)
}

func NodePoolServesReplicas(ctx context.Context, target *Target) (bool, error) {
	discoveryClient, err := target.discoveryClient()
	if err != nil {
		return false, err
	}
	document, err := discoveryClient.RESTClient().Get().AbsPath(KarpenterOpenAPIPath).
		SetHeader("Accept", runtime.ContentTypeJSON).Do(ctx).Raw()
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, schemaRefused(target, err)
	}
	var parsed any
	if err := json.Unmarshal(document, &parsed); err != nil {
		return false, schemaRefused(target, err)
	}
	return pointerResolves(parsed, NodePoolReplicasPointer), nil
}

func pointerResolves(document any, pointer string) bool {
	unescape := strings.NewReplacer("~1", "/", "~0", "~")
	current := document
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		object, isObject := current.(map[string]any)
		if !isObject {
			return false
		}
		next, present := object[unescape.Replace(token)]
		if !present {
			return false
		}
		current = next
	}
	return true
}

func schemaRefused(target *Target, err error) error {
	return refusal.Wrap(refusal.CodePermission, err,
		"This cluster would not serve its "+KarpenterGroup+"/v1 OpenAPI schema, so whether its NodePools can hold "+
			"a fixed number of nodes cannot be read.",
		"The context's credential needs to read it: `"+target.Kubectl("get --raw "+KarpenterOpenAPIPath)+
			"`. Nothing has been changed.")
}

func AutoscaledNodeGroups(ctx context.Context, target *Target) ([]string, error) {
	status, err := clusterAutoscalerStatus(ctx, target)
	if err != nil || status == nil {
		return nil, err
	}
	return nodeGroupNames(status.Data[ClusterAutoscalerStatusKey])
}

func clusterAutoscalerStatus(ctx context.Context, target *Target) (*corev1.ConfigMap, error) {
	list, err := target.Clientset.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + ClusterAutoscalerStatus})
	if err != nil {
		return nil, readRefused(target, err, "ConfigMaps", "get configmap -A --field-selector metadata.name="+
			ClusterAutoscalerStatus)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		if (list.Items[i].Namespace == metav1.NamespaceSystem) != (list.Items[j].Namespace == metav1.NamespaceSystem) {
			return list.Items[i].Namespace == metav1.NamespaceSystem
		}
		return list.Items[i].Namespace < list.Items[j].Namespace
	})
	return &list.Items[0], nil
}

func nodeGroupNames(status string) ([]string, error) {
	var names []string
	var document autoscalerStatusDocument
	parsed := yaml.Unmarshal([]byte(status), &document) == nil &&
		(document.AutoscalerStatus != "" || document.NodeGroups != nil)
	switch {
	case parsed && document.AutoscalerStatus == autoscalerInitializing:
		return nil, fmt.Errorf("the cluster autoscaler reports %s and has not listed its node groups yet",
			autoscalerInitializing)
	case parsed:
		for _, group := range document.NodeGroups {
			names = append(names, strings.TrimSpace(group.Name))
		}
	default:
		legacy, listed := legacyNodeGroupNames(status)
		if !listed {
			return nil, fmt.Errorf("its %q key is in a form zoorikctl does not read", ClusterAutoscalerStatusKey)
		}
		names = legacy
	}
	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	slices.Sort(names)
	return slices.Compact(names), nil
}

func legacyNodeGroupNames(status string) ([]string, bool) {
	var names []string
	inNodeGroups := false
	for _, line := range strings.Split(status, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == legacyNodeGroupsHeader {
			inNodeGroups = true
			continue
		}
		if !inNodeGroups {
			continue
		}
		if name, named := strings.CutPrefix(trimmed, legacyNodeGroupName); named {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names, inNodeGroups
}

func BaseNodeClassSource(ctx context.Context, target *Target) (*EC2NodeClassSource, error) {
	pools, poolsServed := mappedResource(target, KarpenterGroup, KindNodePool)
	classes, classesServed := mappedResource(target, EC2NodeClassGroup, KindEC2NodeClass)
	if !poolsServed || !classesServed {
		return nil, nil
	}
	list, err := target.Dynamic.Resource(pools).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, readRefused(target, err, "NodePools", "get nodepools."+KarpenterGroup)
	}
	for _, reference := range customerClassReferences(list.Items) {
		source, err := ec2NodeClassSource(ctx, target, classes, reference)
		if source != nil || err != nil {
			return source, err
		}
	}
	return nil, nil
}

func NodeClassSourceFor(ctx context.Context, target *Target, nodePool, nodeClass string) (*EC2NodeClassSource,
	error) {
	classes, served := mappedResource(target, EC2NodeClassGroup, KindEC2NodeClass)
	if !served {
		return nil, nil
	}
	return ec2NodeClassSource(ctx, target, classes, classReference{nodePool: nodePool, nodeClass: nodeClass})
}

func ec2NodeClassSource(ctx context.Context, target *Target, classes schema.GroupVersionResource,
	reference classReference) (*EC2NodeClassSource, error) {
	class, err := target.Dynamic.Resource(classes).Get(ctx, reference.nodeClass, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, readRefused(target, err, KindEC2NodeClass+" objects",
			"get "+classes.Resource+"."+classes.Group+" "+reference.nodeClass)
	}
	if zoorikOwned(*class) {
		return nil, nil
	}
	source := &EC2NodeClassSource{NodePool: reference.nodePool, NodeClass: class.GetName()}
	source.Role, _, _ = unstructured.NestedString(class.Object, "spec", "role")
	source.InstanceProfile, _, _ = unstructured.NestedString(class.Object, "spec", "instanceProfile")
	source.SubnetSelectorTerms, _, _ = unstructured.NestedSlice(class.Object, "spec", "subnetSelectorTerms")
	source.SecurityGroupSelectorTerms, _, _ = unstructured.NestedSlice(class.Object, "spec",
		"securityGroupSelectorTerms")
	return source, nil
}

type classReference struct {
	nodePool  string
	nodeClass string
}

func customerClassReferences(items []unstructured.Unstructured) []classReference {
	var references []classReference
	for _, item := range items {
		if poolOf(item).ZoorikOwned {
			continue
		}
		if nodeClass, isEC2 := ec2NodeClassRef(item); isEC2 {
			references = append(references, classReference{nodePool: item.GetName(), nodeClass: nodeClass})
		}
	}
	sort.SliceStable(references, func(i, j int) bool {
		iDefault := references[i].nodePool == DefaultProvisionerName
		jDefault := references[j].nodePool == DefaultProvisionerName
		if iDefault != jDefault {
			return iDefault
		}
		return references[i].nodePool < references[j].nodePool
	})
	return references
}

func ec2NodeClassRef(item unstructured.Unstructured) (string, bool) {
	reference, found, err := unstructured.NestedStringMap(item.Object, "spec", "template", "spec", "nodeClassRef")
	if err != nil || !found {
		return "", false
	}
	group := reference["group"]
	if group == "" {
		group, _, _ = strings.Cut(reference["apiVersion"], "/")
	}
	name := reference["name"]
	return name, name != "" && reference["kind"] == KindEC2NodeClass && (group == "" || group == EC2NodeClassGroup)
}

func staticCapacityOn(containers []corev1.Container) bool {
	for _, container := range containers {
		for _, env := range container.Env {
			if env.Name == KarpenterFeatureGatesEnv && gateOn(env.Value, KarpenterStaticCapacityGate) {
				return true
			}
		}
		for _, arg := range container.Args {
			if value, found := strings.CutPrefix(arg, KarpenterFeatureGatesFlag); found &&
				gateOn(value, KarpenterStaticCapacityGate) {
				return true
			}
		}
	}
	return false
}

func gateOn(gates, gate string) bool {
	for _, entry := range strings.Split(gates, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(entry), "=")
		if found && name == gate {
			on, err := strconv.ParseBool(strings.TrimSpace(value))
			return err == nil && on
		}
	}
	return false
}

func imageTag(image string) string {
	_, tag := splitImage(image)
	return tag
}

func imageRepository(image string) string {
	repository, _ := splitImage(image)
	return repository
}

func splitImage(image string) (string, string) {
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

var azureProviderGroup = regexp.MustCompile(`(?i)^azure:///subscriptions/([^/]+)/resourceGroups/([^/]+)/`)

var awsProviderInstance = regexp.MustCompile(`^aws:///[^/]+/(i-[0-9a-f]+)$`)

func (t *Target) HelmArgs() []string {
	args := []string{"--kube-context", t.ContextName}
	if t.kubeconfigPath != "" {
		args = append(args, "--kubeconfig", t.kubeconfigPath)
	}
	return args
}

func AzureNodeResourceGroup(ctx context.Context, target *Target) (string, string, error) {
	nodes, err := target.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: identityNodeSample})
	if err != nil {
		return "", "", err
	}
	for _, node := range nodes.Items {
		if match := azureProviderGroup.FindStringSubmatch(strings.TrimSpace(node.Spec.ProviderID)); match != nil {
			return strings.ToLower(match[1]), match[2], nil
		}
	}
	return "", "", nil
}

func AWSInstanceID(ctx context.Context, target *Target) (string, error) {
	nodes, err := target.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: identityNodeSample})
	if err != nil {
		return "", err
	}
	for _, node := range nodes.Items {
		if match := awsProviderInstance.FindStringSubmatch(strings.TrimSpace(node.Spec.ProviderID)); match != nil {
			return match[1], nil
		}
	}
	return "", nil
}

func PreferredVersion(target *Target, group, kind string) schema.GroupVersionResource {
	if gvr, served := mappedResource(target, group, kind); served {
		return gvr
	}
	switch kind {
	case KindAKSNodeClass:
		return schema.GroupVersionResource{Group: group, Version: "v1beta1", Resource: "aksnodeclasses"}
	case KindEC2NodeClass:
		return schema.GroupVersionResource{Group: group, Version: "v1", Resource: "ec2nodeclasses"}
	}
	return schema.GroupVersionResource{Group: group, Version: "v1", Resource: strings.ToLower(kind) + "s"}
}
