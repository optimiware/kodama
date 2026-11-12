package kube

import (
	"time"
)

// objectMeta is the response metadata subset the driver maps onto the seam
// types.
type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	Finalizers        []string          `json:"finalizers"`
	DeletionTimestamp *string           `json:"deletionTimestamp"`
}

// objectMetaBody is the request metadata subset. An empty field is omitted so a
// create never sends a meaningless empty resourceVersion.
type objectMetaBody struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// parseTimestamp decodes one RFC3339 Kubernetes timestamp.
func parseTimestamp(value *string) (*time.Time, error) {
	if value == nil || *value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// parseDeletionTimestamp decodes the optional metadata.deletionTimestamp.
func (m objectMeta) parseDeletionTimestamp() (*time.Time, error) {
	return parseTimestamp(m.DeletionTimestamp)
}
