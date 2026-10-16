package setup

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Summary struct {
	Objects       []Object
	ClusterScoped []Object
	Namespaces    []string
	Images        []string
	Replicas      []string
}

type Object struct {
	Kind      string
	Name      string
	Namespace string
}

func (o Object) String() string {
	if o.Namespace == "" {
		return fmt.Sprintf("%s/%s", o.Kind, o.Name)
	}
	return fmt.Sprintf("%s/%s in %s", o.Kind, o.Name, o.Namespace)
}

var clusterScopedKinds = map[string]bool{
	"Namespace":                        true,
	"ClusterRole":                      true,
	"ClusterRoleBinding":               true,
	"CustomResourceDefinition":         true,
	"MutatingWebhookConfiguration":     true,
	"ValidatingAdmissionPolicy":        true,
	"ValidatingAdmissionPolicyBinding": true,
}

func Summarize(docs []Document) Summary {
	var summary Summary
	seenImage := map[string]bool{}
	seenNamespace := map[string]bool{}

	for _, doc := range docs {
		object := Object{Kind: doc.GetKind(), Name: doc.GetName(), Namespace: doc.GetNamespace()}
		if clusterScopedKinds[object.Kind] {
			summary.ClusterScoped = append(summary.ClusterScoped, object)
		}
		summary.Objects = append(summary.Objects, object)

		if object.Kind == "Namespace" && !seenNamespace[object.Name] {
			seenNamespace[object.Name] = true
			summary.Namespaces = append(summary.Namespaces, object.Name)
		}
		for _, image := range images(doc) {
			if !seenImage[image] {
				seenImage[image] = true
				summary.Images = append(summary.Images, image)
			}
		}
		if replicas, found, _ := unstructured.NestedInt64(doc.Object, "spec", "replicas"); found {
			summary.Replicas = append(summary.Replicas,
				fmt.Sprintf("%s/%s runs %d replica(s)", object.Kind, object.Name, replicas))
		}
	}
	sort.Strings(summary.Images)
	return summary
}

func images(doc Document) []string {
	var found []string
	for _, field := range []string{"initContainers", "containers"} {
		containers, _, _ := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", field)
		for _, entry := range containers {
			container, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if image, ok := container["image"].(string); ok && image != "" {
				found = append(found, image)
			}
		}
	}
	return found
}
