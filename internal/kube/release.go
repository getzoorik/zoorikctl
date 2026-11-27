package kube

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/api"

	appsv1 "k8s.io/api/apps/v1"
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

	AgentCredentialSecret   = "zoorik-agent-credential"
	AgentControlPlaneURLKey = "controlPlaneUrl"

	ReleaseAgentStopTimeout = 90 * time.Second
	ReleaseNamespaceWait    = 60 * time.Second

	agentCredentialKey           = "credential"
	releaseStepAgent             = "agent"
	releaseStepZoorik            = "zoorik"
	releaseStepWebhook           = "webhook"
	releaseStepWebhookWorkload   = "webhook-workload"
	releaseStepGrant             = "grant"
	releaseStepAdmissionPolicy   = "admission-policy"
	releaseStepNodeDaemon        = "node-daemon"
	releaseStepCRD               = "crd"
	releaseStepHelm              = "helm"
	releaseStepNamespace         = "namespace"
	admissionRegistrationGroup   = "admissionregistration.k8s.io"
	validatingPolicyKind         = "ValidatingAdmissionPolicy"
	validatingPolicyBindingKind  = "ValidatingAdmissionPolicyBinding"
	recommendationResourcePlural = "recommendations"
	actionRequestResourcePlural  = "actionrequests"
)

var (
	crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1",
		Resource: "customresourcedefinitions"}
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
	Left      bool
}

func (i ReleaseItem) Describe() string {
	subject := i.Name
	if i.Namespace != "" {
		subject = i.Namespace + "/" + i.Name
	}
	if i.Kind != "" {
		subject = i.Kind + " " + subject
	}
	if i.Detail == "" {
		return subject
	}
	return subject + ": " + i.Detail
}

func (i ReleaseItem) key() string {
	return i.Step + "|" + i.Kind + "|" + i.Namespace + "|" + i.Name
}

type releaseGrant struct {
	kind      string
	namespace string
	name      string
}

func (g releaseGrant) item(detail string) ReleaseItem {
	step := releaseStepGrant
	if _, policy := policyResources[g.kind]; policy {
		step = releaseStepAdmissionPolicy
	}
	return ReleaseItem{Step: step, Kind: g.kind, Namespace: g.namespace, Name: g.name, Detail: detail}
}

type ReleasePlan struct {
	Items           []ReleaseItem
	ControlPlaneURL string

	agentKey        string
	agentPresent    bool
	webhookPresent  bool
	webhookWorkload bool
	grants          []releaseGrant
	nodeDaemon      bool
	floors          []autoscalerFloor
	hpas            []autoscalingv2.HorizontalPodAutoscaler
	vpas            []unstructured.Unstructured
	vpaRecords      map[string]string
	markers         []rolloutMarker
	surges          []handoverSurge
	nodes           []corev1.Node
	claims          []restoreClaim
	pools           []coralPool
	rolls           []appRoll
	baseClass       *baseNodeClass
	crds            []string
	helmName        string
	helmNamespace   string
	namespace       bool

	autoscalerPolicy   bool
	stoppedAutoscalers []appsv1.Deployment
}

func PlanRelease(ctx context.Context, target *Target) (*ReleasePlan, error) {
	plan := &ReleasePlan{}
	for _, read := range []func(context.Context, *Target) error{plan.readAgent, plan.readActors,
		plan.readAutoscalerSwitch, plan.readAutoscalers, plan.readMarks, plan.readPools, plan.readDefinitions} {
		if err := read(ctx, target); err != nil {
			return nil, err
		}
	}
	plan.Items = plan.items()
	return plan, nil
}

func (p *ReleasePlan) readAgent(ctx context.Context, target *Target) error {
	core := target.Clientset.CoreV1()
	deployments := target.Clientset.AppsV1().Deployments(AgentNamespace)
	_, err := deployments.Get(ctx, AgentWorkload, metav1.GetOptions{})
	if p.agentPresent, err = exists(err); err != nil {
		return err
	}
	_, err = deployments.Get(ctx, WebhookWorkload, metav1.GetOptions{})
	if p.webhookWorkload, err = exists(err); err != nil {
		return err
	}
	config, err := core.ConfigMaps(AgentNamespace).Get(ctx, AgentConfigMap, metav1.GetOptions{})
	switch {
	case err == nil:
		p.ControlPlaneURL = strings.TrimSpace(config.Data[AgentControlPlaneURLKey])
	case !apierrors.IsNotFound(err):
		return err
	}
	secret, err := core.Secrets(AgentNamespace).Get(ctx, AgentCredentialSecret, metav1.GetOptions{})
	switch {
	case err == nil:
		p.agentKey = strings.TrimSpace(string(secret.Data[agentCredentialKey]))
	case !apierrors.IsNotFound(err):
		return err
	}
	namespace, err := core.Namespaces().Get(ctx, AgentNamespace, metav1.GetOptions{})
	switch {
	case err == nil:
		p.namespace = true
		p.helmName = namespace.Annotations[HelmReleaseNameAnnotation]
		p.helmNamespace = namespace.Annotations[HelmReleaseNamespaceAnnotation]
	case !apierrors.IsNotFound(err):
		return err
	}
	return nil
}

func (p *ReleasePlan) readActors(ctx context.Context, target *Target) error {
	_, err := target.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx,
		WebhookConfiguration, metav1.GetOptions{})
	if p.webhookPresent, err = exists(err); err != nil {
		return err
	}
	if p.grants, err = labelledGrants(ctx, target); err != nil {
		return err
	}
	p.nodeDaemon, err = NodeDaemonInstalled(ctx, target)
	return err
}

func (p *ReleasePlan) readDefinitions(ctx context.Context, target *Target) error {
	for _, name := range []string{RecommendationCRD, ActionRequestCRD} {
		_, err := target.Dynamic.Resource(crdResource).Get(ctx, name, metav1.GetOptions{})
		found, err := exists(err)
		if err != nil {
			return err
		}
		if found {
			p.crds = append(p.crds, name)
		}
	}
	return nil
}

func (p *ReleasePlan) items() []ReleaseItem {
	var items []ReleaseItem
	add := func(step, kind, namespace, name, detail string) {
		items = append(items, ReleaseItem{Step: step, Kind: kind, Namespace: namespace, Name: name,
			Detail: detail})
	}
	if p.agentPresent {
		add(releaseStepAgent, kindDeployment, AgentNamespace, AgentWorkload, "scaled to 0, so it stops making changes")
	}
	told := len(items)
	if p.webhookPresent {
		add(releaseStepWebhook, kindMutatingWebhookConfiguration, "", WebhookConfiguration,
			"deleted, so new pods start at their own sizes")
	}
	if p.webhookWorkload {
		add(releaseStepWebhookWorkload, kindDeployment, AgentNamespace, WebhookWorkload, "scaled to 0")
	}
	for _, grant := range p.grants {
		items = append(items, grant.item("deleted"))
	}
	if p.nodeDaemon {
		add(releaseStepNodeDaemon, kindNamespace, "", NodeDaemonNamespace, "the live-migration daemon: deleted")
	}
	items = append(items, p.handBackItems()...)
	items = append(items, p.poolItems()...)
	for _, name := range p.crds {
		add(releaseStepCRD, kindCustomResourceDefinition, "", name, "deleted with its records")
	}
	if p.helmName != "" && p.helmNamespace != "" {
		items = append(items, p.helmItem())
	}
	if p.namespace {
		add(releaseStepNamespace, kindNamespace, "", AgentNamespace, "deleted")
	}
	if len(items) == 0 {
		return nil
	}
	zoorik := ReleaseItem{Step: releaseStepZoorik, Name: "Zoorik", Detail: "cannot be told: " + p.Untold()}
	if p.Untold() == "" {
		zoorik = ReleaseItem{Step: releaseStepZoorik, Name: "Zoorik at " + p.ControlPlaneURL,
			Detail: "told that this cluster leaves its console"}
	}
	return slices.Insert(items, told, zoorik)
}

func (p *ReleasePlan) helmItem() ReleaseItem {
	return ReleaseItem{Step: releaseStepHelm, Kind: "Helm release", Name: p.helmName + " in " + p.helmNamespace,
		Detail: "uninstalled"}
}

func (p *ReleasePlan) Empty() bool {
	return len(p.Items) == 0
}

func (p *ReleasePlan) HasNamespace() bool {
	return p.namespace
}

func (p *ReleasePlan) Untold() string {
	switch {
	case p.agentKey == "":
		return "the agent's key is not on this cluster"
	case p.ControlPlaneURL == "":
		return "the agent's settings do not name Zoorik's address"
	}
	return ""
}

func (p *ReleasePlan) StopAgent(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	if !p.agentPresent {
		return nil
	}
	forced, err := stopAgent(ctx, target)
	if err != nil {
		return err
	}
	detail := "scaled to 0; no agent pod is left running"
	if len(forced) > 0 {
		detail += "; force-deleted " + strings.Join(forced, ", ") + ", whose node is gone or not Ready " +
			"(if that node comes back, its kubelet stops the pod)"
	}
	report(ReleaseItem{Step: releaseStepAgent, Kind: kindDeployment, Namespace: AgentNamespace,
		Name: AgentWorkload, Detail: detail})
	return nil
}

func (p *ReleasePlan) TellZoorik(ctx context.Context) api.DisconnectAnswer {
	if reason := p.Untold(); reason != "" {
		return api.DisconnectAnswer{Reason: reason}
	}
	return api.New(p.ControlPlaneURL, p.agentKey).Disconnect(ctx)
}

func (p *ReleasePlan) RemoveActors(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	if p.webhookPresent {
		err := target.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx,
			WebhookConfiguration, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		report(ReleaseItem{Step: releaseStepWebhook, Kind: kindMutatingWebhookConfiguration,
			Name: WebhookConfiguration, Detail: "deleted"})
	}
	if p.webhookWorkload {
		_, err := target.Clientset.AppsV1().Deployments(AgentNamespace).Patch(ctx, WebhookWorkload,
			types.MergePatchType, []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{FieldManager: FieldManager})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		report(ReleaseItem{Step: releaseStepWebhookWorkload, Kind: kindDeployment, Namespace: AgentNamespace,
			Name: WebhookWorkload, Detail: "scaled to 0"})
	}
	for _, grant := range p.grants {
		if err := deleteGrant(ctx, target, grant); err != nil {
			return err
		}
		report(grant.item("deleted"))
	}
	if p.nodeDaemon {
		detail, err := deleteNamespaceNamed(ctx, target, NodeDaemonNamespace)
		if err != nil {
			return err
		}
		report(ReleaseItem{Step: releaseStepNodeDaemon, Kind: kindNamespace, Name: NodeDaemonNamespace,
			Detail: "the live-migration daemon: " + detail})
	}
	return nil
}

func (p *ReleasePlan) Refresh(ctx context.Context, target *Target, found func(ReleaseItem)) error {
	if !p.agentPresent && !p.nodeDaemon {
		return nil
	}
	fresh, err := PlanRelease(ctx, target)
	if err != nil {
		return err
	}
	planned := make(map[string]bool, len(p.Items))
	for _, item := range p.Items {
		planned[item.key()] = true
	}
	for _, item := range fresh.Items {
		if item.Step != releaseStepAgent && item.Step != releaseStepZoorik && !planned[item.key()] {
			found(item)
		}
	}
	*p = *fresh
	return nil
}

func (p *ReleasePlan) DeleteDefinitions(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	for _, name := range p.crds {
		err := target.Dynamic.Resource(crdResource).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		report(ReleaseItem{Step: releaseStepCRD, Kind: kindCustomResourceDefinition, Name: name,
			Detail: "deleted with its records"})
	}
	return nil
}

func (p *ReleasePlan) UninstallHelm(report func(ReleaseItem), uninstall func(name, namespace string) error) error {
	if p.helmName == "" || p.helmNamespace == "" {
		return nil
	}
	if err := uninstall(p.helmName, p.helmNamespace); err != nil {
		return err
	}
	report(p.helmItem())
	return nil
}

func (p *ReleasePlan) DeleteNamespace(ctx context.Context, target *Target, report func(ReleaseItem)) error {
	if !p.namespace {
		return nil
	}
	detail, err := deleteNamespaceNamed(ctx, target, AgentNamespace)
	if err != nil {
		return err
	}
	report(ReleaseItem{Step: releaseStepNamespace, Kind: kindNamespace, Name: AgentNamespace, Detail: detail})
	return nil
}

func exists(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	}
	return false, err
}

func deleteNamespaceNamed(ctx context.Context, target *Target, name string) (string, error) {
	namespaces := target.Clientset.CoreV1().Namespaces()
	err := namespaces.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
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
		if err := pause(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

func pause(ctx context.Context, wait time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func stopAgent(ctx context.Context, target *Target) ([]string, error) {
	deployments := target.Clientset.AppsV1().Deployments(AgentNamespace)
	_, err := deployments.Patch(ctx, AgentWorkload, types.MergePatchType,
		[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{FieldManager: FieldManager})
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
				"nothing else was changed yet, so run the disconnect again once `%s` shows none",
				AgentWorkload, running, ReleaseAgentStopTimeout,
				target.Kubectl("-n "+AgentNamespace+" get pods -l app.kubernetes.io/name="+AgentWorkload))
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return nil, err
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

func customResources(ctx context.Context, target *Target, crd, plural, namespace string) (
	schema.GroupVersionResource, []unstructured.Unstructured, error) {
	resource, ok, err := servedResource(ctx, target, crd, plural)
	if err != nil || !ok {
		return resource, nil, err
	}
	list, err := target.Dynamic.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if absentAPI(err) {
			return resource, nil, nil
		}
		return resource, nil, err
	}
	return resource, list.Items, nil
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
		err = target.Dynamic.Resource(policyResources[grant.kind]).Delete(ctx, grant.name, options)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func absentAPI(err error) bool {
	return apierrors.IsNotFound(err) || strings.Contains(err.Error(), "the server could not find the requested resource")
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
