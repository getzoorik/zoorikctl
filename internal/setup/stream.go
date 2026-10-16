package setup

import (
	"fmt"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const DocumentSeparator = "---"

type Document = *unstructured.Unstructured

func Split(stream string) ([]Document, error) {
	var docs []Document
	for index, text := range texts(stream) {
		if strings.TrimSpace(text) == "" {
			continue
		}
		doc, err := decode(index, text)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func texts(stream string) []string {
	var texts []string
	offset := 0

	for offset <= len(stream) {
		start, end, found := nextSeparator(stream, offset)
		if !found {
			texts = append(texts, stream[offset:])
			break
		}
		texts = append(texts, stream[offset:start])
		offset = end
	}
	return texts
}

func nextSeparator(stream string, from int) (start int, end int, found bool) {
	for offset := from; offset < len(stream); {
		lineEnd := strings.IndexByte(stream[offset:], '\n')
		var line string
		var next int
		if lineEnd < 0 {
			line = stream[offset:]
			next = len(stream)
		} else {
			line = stream[offset : offset+lineEnd]
			next = offset + lineEnd + 1
		}
		if isSeparator(line) {
			return offset, next, true
		}
		offset = next
	}
	return 0, 0, false
}

func isSeparator(line string) bool {
	trimmed := strings.TrimRight(line, " \t\r")
	return trimmed == DocumentSeparator
}

func decode(index int, text string) (Document, error) {
	asJSON, err := yaml.YAMLToJSON([]byte(text))
	if err != nil {
		return nil, refusal.Wrap(refusal.CodeApply, err,
			fmt.Sprintf("Document %d of the install is not valid YAML.", index+1),
			"Nothing has been applied. Report it: the control plane rendered this, so it is our defect and "+
				"not your cluster's.")
	}
	object := &unstructured.Unstructured{}
	if err := object.UnmarshalJSON(asJSON); err != nil {
		return nil, refusal.Wrap(refusal.CodeApply, err,
			fmt.Sprintf("Document %d of the install is not a Kubernetes object.", index+1),
			"Nothing has been applied. Report it: the control plane rendered this.")
	}
	if object.GetKind() == "" || object.GetAPIVersion() == "" {
		return nil, refusal.New(refusal.CodeApply,
			fmt.Sprintf("Document %d of the install declares no kind or no apiVersion.", index+1),
			"Nothing has been applied. Report it: the control plane rendered this.")
	}
	return object, nil
}

func Join(docs []Document) (string, error) {
	var builder strings.Builder
	for index, doc := range docs {
		asYAML, err := yaml.Marshal(doc.Object)
		if err != nil {
			return "", refusal.Wrap(refusal.CodeApply, err,
				fmt.Sprintf("Document %d of the install could not be printed.", index+1),
				"Nothing has been applied. Report this: it is a defect in zoorikctl.")
		}
		if index > 0 {
			builder.WriteString(DocumentSeparator)
			builder.WriteString("\n")
		}
		builder.Write(asYAML)
	}
	return builder.String(), nil
}
