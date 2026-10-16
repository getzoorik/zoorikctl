package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/setup"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const FieldManager = "zoorikctl"

type Result struct {
	Object  setup.Object
	Created bool
}

var checkedBeforeApply = map[schema.GroupKind]bool{
	{Group: admissionRegistrationGroup, Kind: kindValidatingAdmissionPolicy}: true,
	{Group: rbacGroup, Kind: kindClusterRole}:                                true,
}

func Apply(ctx context.Context, target *Target, docs []setup.Document,
	progress func(Result, int)) ([]Result, error) {
	if err := preflight(ctx, target, docs); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(docs))
	for _, object := range inDependencyOrder(docs) {
		result, err := applyOne(ctx, target, object)
		if err != nil {
			return results, annotate(err, object, results)
		}
		results = append(results, result)
		if progress != nil {
			progress(result, len(results))
		}
	}
	return results, nil
}

func preflight(ctx context.Context, target *Target, objects []*unstructured.Unstructured) error {
	for _, object := range objects {
		if !checkedBeforeApply[object.GroupVersionKind().GroupKind()] && !policyInCluster(ctx, target, object) {
			continue
		}
		resource, err := resourceFor(target, object)
		if err != nil {
			return err
		}
		if _, err := serverSideApply(ctx, resource, object, true); err != nil {
			return refusedBeforeApply(err, object)
		}
	}
	return nil
}

func policyInCluster(ctx context.Context, target *Target, binding *unstructured.Unstructured) bool {
	if binding.GroupVersionKind().GroupKind() != (schema.GroupKind{Group: admissionRegistrationGroup,
		Kind: kindValidatingAdmissionPolicyBinding}) {
		return false
	}
	policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName")
	if policyName == "" {
		return false
	}
	_, err := target.Dynamic.Resource(policyResources[validatingPolicyKind]).
		Get(ctx, policyName, metav1.GetOptions{})
	return err == nil
}

func inDependencyOrder(objects []*unstructured.Unstructured) []*unstructured.Unstructured {
	byKey := make(map[string]*unstructured.Unstructured, len(objects))
	paramKinds := map[string]schema.GroupKind{}
	for _, object := range objects {
		byKey[keyOf(object)] = object
		if object.GetKind() != kindValidatingAdmissionPolicy {
			continue
		}
		if apiVersion, kind, declared := paramKindOf(object); declared {
			if version, err := schema.ParseGroupVersion(apiVersion); err == nil {
				paramKinds[object.GetName()] = schema.GroupKind{Group: version.Group, Kind: kind}
			}
		}
	}
	ordered := make([]*unstructured.Unstructured, 0, len(objects))
	placed := make(map[*unstructured.Unstructured]bool, len(objects))
	place := func(object *unstructured.Unstructured) {
		if !placed[object] {
			ordered = append(ordered, object)
			placed[object] = true
		}
	}
	for _, object := range objects {
		if param := byKey[paramOf(object, paramKinds)]; param != nil && !placed[param] {
			namespace := byKey[objectKey(schema.GroupKind{Kind: kindNamespace}, "", param.GetNamespace())]
			if param.GetNamespace() == "" || namespace == nil || placed[namespace] {
				place(param)
			}
		}
		place(object)
	}
	return ordered
}

func paramOf(binding *unstructured.Unstructured, paramKinds map[string]schema.GroupKind) string {
	if binding.GetKind() != kindValidatingAdmissionPolicyBinding {
		return ""
	}
	policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName")
	name, _, _ := unstructured.NestedString(binding.Object, "spec", "paramRef", "name")
	kind, declared := paramKinds[policyName]
	if !declared || name == "" {
		return ""
	}
	namespace, _, _ := unstructured.NestedString(binding.Object, "spec", "paramRef", "namespace")
	return objectKey(kind, namespace, name)
}

func paramKindOf(policy *unstructured.Unstructured) (string, string, bool) {
	apiVersion, _, _ := unstructured.NestedString(policy.Object, "spec", "paramKind", "apiVersion")
	kind, _, _ := unstructured.NestedString(policy.Object, "spec", "paramKind", "kind")
	return apiVersion, kind, kind != ""
}

func keyOf(object *unstructured.Unstructured) string {
	return objectKey(object.GroupVersionKind().GroupKind(), object.GetNamespace(), object.GetName())
}

func objectKey(kind schema.GroupKind, namespace, name string) string {
	return strings.Join([]string{kind.Group, kind.Kind, namespace, name}, "/")
}

func applyOne(ctx context.Context, target *Target,
	object *unstructured.Unstructured) (Result, error) {
	resource, err := resourceFor(target, object)
	if err != nil {
		return Result{}, err
	}
	_, err = resource.Get(ctx, object.GetName(), metav1.GetOptions{})
	existed := err == nil

	applied, err := serverSideApply(ctx, resource, object, false)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Object: setup.Object{
			Kind:      applied.GetKind(),
			Name:      applied.GetName(),
			Namespace: applied.GetNamespace(),
		},
		Created: !existed,
	}, nil
}

func resourceFor(target *Target, object *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := object.GroupVersionKind()
	mapping, err := target.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, refusal.Wrap(refusal.CodeApply, err,
			fmt.Sprintf("This cluster does not serve %s.", gvk.String()),
			"The agent bundle uses only core Kubernetes APIs, so this normally means the cluster "+
				"is older than the bundle expects. Check the cluster's Kubernetes version.")
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return target.Dynamic.Resource(mapping.Resource).Namespace(object.GetNamespace()), nil
	}
	return target.Dynamic.Resource(mapping.Resource), nil
}

func serverSideApply(ctx context.Context, resource dynamic.ResourceInterface,
	object *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	body, err := object.MarshalJSON()
	if err != nil {
		return nil, refusal.Wrap(refusal.CodeApply, err,
			"An object could not be re-encoded for the API server.", "Report this.")
	}
	options := metav1.PatchOptions{FieldManager: FieldManager, Force: boolPtr(true)}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	return resource.Patch(ctx, object.GetName(), types.ApplyPatchType, body, options)
}

func refusedBeforeApply(err error, object *unstructured.Unstructured) error {
	if apierrors.IsForbidden(err) {
		return annotate(err, object, nil)
	}
	var typed *refusal.Refusal
	if asRefusal(err, &typed) {
		return typed
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return refusal.Wrap(refusal.CodeApply, err,
			fmt.Sprintf("%s/%s could not be checked with the cluster before the install.",
				object.GetKind(), object.GetName()),
			"No object of the install has been applied. Run this again.")
	}
	return refusal.Wrap(refusal.CodeApply, err,
		fmt.Sprintf("The cluster would refuse %s/%s.", object.GetKind(), object.GetName()),
		"No object of the install has been applied: each admission policy and cluster role is "+
			"checked with a server-side dry run before the first object is. Share the log named "+
			"below with Zoorik support.")
}

func annotate(err error, object *unstructured.Unstructured, applied []Result) error {
	var typed *refusal.Refusal
	if asRefusal(err, &typed) {
		return typed
	}

	identity := fmt.Sprintf("%s/%s", object.GetKind(), object.GetName())

	if apierrors.IsForbidden(err) {
		return refusal.Wrap(refusal.CodePermission, err,
			fmt.Sprintf("This context is not permitted to create %s.", identity),
			"Re-run the same command as a user with cluster-admin on this cluster. The command "+
				"is safe to run again: everything below is applied by name and converges rather "+
				"than duplicating.").WithDetail(
			append([]string{
				"The install needs cluster-admin because it creates ClusterRoles, and Kubernetes",
				"refuses to let anyone grant permissions they do not themselves hold.",
				"",
				partialInstallLine(applied),
			}, appliedLines(applied)...)...)
	}

	return refusal.Wrap(refusal.CodeApply, err,
		fmt.Sprintf("The cluster refused %s.", identity),
		"Nothing after this object was applied.").WithDetail(appliedLines(applied)...)
}

func partialInstallLine(applied []Result) string {
	if len(applied) == 0 {
		return "Nothing was created before this failed, so the cluster is unchanged."
	}
	return fmt.Sprintf("%d object(s) were already applied before this failed:", len(applied))
}

func appliedLines(applied []Result) []string {
	lines := make([]string, 0, len(applied))
	for _, result := range applied {
		lines = append(lines, "  · "+result.Object.String())
	}
	return lines
}

func asRefusal(err error, out **refusal.Refusal) bool {
	for current := err; current != nil; {
		if typed, ok := current.(*refusal.Refusal); ok {
			*out = typed
			return true
		}
		unwrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = unwrapper.Unwrap()
	}
	return false
}

func boolPtr(v bool) *bool { return &v }
