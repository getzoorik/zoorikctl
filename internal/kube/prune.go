package kube

import (
	"context"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/setup"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	PartOfLabelKey   = "app.kubernetes.io/part-of"
	PartOfLabelValue = "zoorik-agent"

	rbacGroup = "rbac.authorization.k8s.io"

	kindClusterRole        = "ClusterRole"
	kindClusterRoleBinding = "ClusterRoleBinding"
	kindRole               = "Role"
	kindRoleBinding        = "RoleBinding"

	kindConfigMap                = "ConfigMap"
	kindDaemonSet                = "DaemonSet"
	kindCustomResourceDefinition = "CustomResourceDefinition"
)

var prunableKinds = map[string]bool{
	kindClusterRole:        true,
	kindClusterRoleBinding: true,
	kindRole:               true,
	kindRoleBinding:        true,
}

type prunableObject struct {
	kind     schema.GroupKind
	resource schema.GroupVersionResource
}

var prunableObjects = []prunableObject{
	{schema.GroupKind{Group: "apps", Kind: kindDaemonSet}, schema.GroupVersionResource{Group: "apps", Version: "v1",
		Resource: "daemonsets"}},
	{schema.GroupKind{Kind: kindService}, schema.GroupVersionResource{Version: "v1", Resource: "services"}},
	{schema.GroupKind{Kind: kindConfigMap}, schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}},
	{schema.GroupKind{Group: crdResource.Group, Kind: kindCustomResourceDefinition}, crdResource},
}

func PartOfSelector() string {
	return PartOfLabelKey + "=" + PartOfLabelValue
}

func ListGrantsCommand(target *Target) string {
	return target.Kubectl("get clusterrole,clusterrolebinding,role,rolebinding -A -l " + PartOfSelector())
}

func ListObjectsCommand(target *Target) string {
	return target.Kubectl("get service,configmap,daemonset,customresourcedefinition -A -l " + PartOfSelector())
}

func ListPoliciesCommand(target *Target) string {
	return target.Kubectl("get validatingadmissionpolicy,validatingadmissionpolicybinding -l " + PartOfSelector())
}

func DeleteGrantCommand(target *Target, grant setup.Object) string {
	arguments := "delete " + strings.ToLower(grant.Kind) + " " + ShellWord(grant.Name)
	if grant.Namespace != "" {
		arguments = "-n " + ShellWord(grant.Namespace) + " " + arguments
	}
	return target.Kubectl(arguments)
}

func StaleGrants(ctx context.Context, target *Target, docs []setup.Document) ([]setup.Object, error) {
	kept := keptGrants(docs)
	if len(kept) == 0 {
		return nil, nil
	}
	selector := metav1.ListOptions{LabelSelector: PartOfSelector()}
	rbac := target.Clientset.RbacV1()
	var found []setup.Object

	roleBindings, err := rbac.RoleBindings(metav1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range roleBindings.Items {
		found = append(found, setup.Object{Kind: kindRoleBinding, Name: item.Name, Namespace: item.Namespace})
	}
	clusterRoleBindings, err := rbac.ClusterRoleBindings().List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range clusterRoleBindings.Items {
		found = append(found, setup.Object{Kind: kindClusterRoleBinding, Name: item.Name})
	}
	roles, err := rbac.Roles(metav1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range roles.Items {
		found = append(found, setup.Object{Kind: kindRole, Name: item.Name, Namespace: item.Namespace})
	}
	clusterRoles, err := rbac.ClusterRoles().List(ctx, selector)
	if err != nil {
		return nil, err
	}
	for _, item := range clusterRoles.Items {
		found = append(found, setup.Object{Kind: kindClusterRole, Name: item.Name})
	}

	stale := make([]setup.Object, 0, len(found))
	for _, grant := range found {
		if !kept[grant] {
			stale = append(stale, grant)
		}
	}
	return stale, nil
}

func StaleAdmissionPolicies(ctx context.Context, target *Target, docs []setup.Document) ([]setup.Object, error) {
	kept := keptGrants(docs)
	policies := installedPolicies(docs)
	if len(kept) == 0 || len(policies) == 0 {
		return nil, nil
	}
	selector := metav1.ListOptions{LabelSelector: PartOfSelector()}
	var stale []setup.Object
	for _, kind := range []string{validatingPolicyBindingKind, validatingPolicyKind} {
		list, err := target.Dynamic.Resource(policyResources[kind]).List(ctx, selector)
		if err != nil {
			if absentAPI(err) {
				continue
			}
			return nil, err
		}
		for _, item := range list.Items {
			if policy := (setup.Object{Kind: kind, Name: item.GetName()}); !policies[policy] {
				stale = append(stale, policy)
			}
		}
	}
	return stale, nil
}

func StaleObjects(ctx context.Context, target *Target, docs []setup.Document) ([]setup.Object, error) {
	kept := map[setup.Object]bool{}
	for _, object := range docs {
		for _, prunable := range prunableObjects {
			if object.GroupVersionKind().GroupKind() == prunable.kind {
				kept[setup.Object{Kind: object.GetKind(), Name: object.GetName(),
					Namespace: object.GetNamespace()}] = true
			}
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	selector := metav1.ListOptions{LabelSelector: PartOfSelector()}
	var stale []setup.Object
	for _, prunable := range prunableObjects {
		list, err := target.Dynamic.Resource(prunable.resource).List(ctx, selector)
		if err != nil {
			if absentAPI(err) {
				continue
			}
			return nil, err
		}
		for _, item := range list.Items {
			if found := (setup.Object{Kind: prunable.kind.Kind, Name: item.GetName(),
				Namespace: item.GetNamespace()}); !kept[found] {
				stale = append(stale, found)
			}
		}
	}
	return stale, nil
}

func installedPolicies(docs []setup.Document) map[setup.Object]bool {
	installed := map[setup.Object]bool{}
	for _, object := range docs {
		gvk := object.GroupVersionKind()
		if _, policy := policyResources[gvk.Kind]; policy && gvk.Group == admissionRegistrationGroup {
			installed[setup.Object{Kind: gvk.Kind, Name: object.GetName()}] = true
		}
	}
	return installed
}

func DeleteStale(ctx context.Context, target *Target, stale []setup.Object,
	deleted func(setup.Object)) ([]setup.Object, error) {
	removed := make([]setup.Object, 0, len(stale))
	rbac := target.Clientset.RbacV1()
	options := metav1.DeleteOptions{}
	for _, grant := range stale {
		var err error
		switch grant.Kind {
		case kindRoleBinding:
			err = rbac.RoleBindings(grant.Namespace).Delete(ctx, grant.Name, options)
		case kindClusterRoleBinding:
			err = rbac.ClusterRoleBindings().Delete(ctx, grant.Name, options)
		case kindRole:
			err = rbac.Roles(grant.Namespace).Delete(ctx, grant.Name, options)
		case kindClusterRole:
			err = rbac.ClusterRoles().Delete(ctx, grant.Name, options)
		default:
			resource, known := staleResource(grant.Kind)
			if !known {
				continue
			}
			var client dynamic.ResourceInterface = target.Dynamic.Resource(resource)
			if grant.Namespace != "" {
				client = target.Dynamic.Resource(resource).Namespace(grant.Namespace)
			}
			err = client.Delete(ctx, grant.Name, options)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return removed, err
		}
		removed = append(removed, grant)
		if deleted != nil {
			deleted(grant)
		}
	}
	return removed, nil
}

func staleResource(kind string) (schema.GroupVersionResource, bool) {
	if resource, policy := policyResources[kind]; policy {
		return resource, true
	}
	for _, prunable := range prunableObjects {
		if prunable.kind.Kind == kind {
			return prunable.resource, true
		}
	}
	return schema.GroupVersionResource{}, false
}

func keptGrants(docs []setup.Document) map[setup.Object]bool {
	kept := map[setup.Object]bool{}
	for _, object := range docs {
		gvk := object.GroupVersionKind()
		if gvk.Group != rbacGroup || !prunableKinds[gvk.Kind] {
			continue
		}
		grant := setup.Object{Kind: gvk.Kind, Name: object.GetName()}
		if gvk.Kind == kindRole || gvk.Kind == kindRoleBinding {
			grant.Namespace = object.GetNamespace()
		}
		kept[grant] = true
	}
	return kept
}
