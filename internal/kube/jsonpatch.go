package kube

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type writeOutcome int

const (
	patchApplied writeOutcome = iota
	patchStale
	patchGone
)

var (
	pointerEscape    = strings.NewReplacer("~", "~0", "/", "~1")
	jsonPatchOptions = metav1.PatchOptions{FieldManager: FieldManager}
	patchNotApplied  = apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{},
		"", "", 0, false).ErrStatus
)

type jsonPatch []map[string]any

func (p *jsonPatch) test(path string, value any) {
	*p = append(*p, map[string]any{"op": "test", "path": path, "value": value})
}

func (p *jsonPatch) remove(path string) {
	*p = append(*p, map[string]any{"op": "remove", "path": path})
}

func (p *jsonPatch) replace(path string, value any) {
	*p = append(*p, map[string]any{"op": "replace", "path": path, "value": value})
}

func (p *jsonPatch) add(path string, value any) {
	*p = append(*p, map[string]any{"op": "add", "path": path, "value": value})
}

func (p *jsonPatch) drop(path string, value any) {
	p.test(path, value)
	p.remove(path)
}

func (p jsonPatch) body() []byte {
	raw, _ := json.Marshal(p)
	return raw
}

func pointer(segments ...string) string {
	var path strings.Builder
	for _, segment := range segments {
		path.WriteString("/" + pointerEscape.Replace(segment))
	}
	return path.String()
}

func annotationPath(key string) string {
	return pointer("metadata", "annotations", key)
}

func labelPath(key string) string {
	return pointer("metadata", "labels", key)
}

func outcomeOf(err error) (writeOutcome, error) {
	var refused *apierrors.StatusError
	switch {
	case err == nil:
		return patchApplied, nil
	case apierrors.IsNotFound(err):
		return patchGone, nil
	case errors.As(err, &refused) && refused.ErrStatus.Reason == patchNotApplied.Reason &&
		refused.ErrStatus.Message == patchNotApplied.Message:
		return patchStale, nil
	}
	return patchApplied, err
}
