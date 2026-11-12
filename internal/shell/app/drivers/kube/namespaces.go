package kube

import (
	"context"
	"encoding/json"
	"net/http"

	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// namespaceWire is the tenant namespace response subset.
type namespaceWire struct {
	Metadata objectMeta `json:"metadata"`
}

// GetNamespace returns one observed namespace and its ownership labels. A
// missing namespace is ErrNotFound.
func (c *Cluster) GetNamespace(ctx context.Context, name string) (runtimekube.Namespace, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, namespacePath(name), nil, nil, "")
	if err != nil {
		return runtimekube.Namespace{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Namespace{}, statusError("get namespace", status, false)
	}
	var wire namespaceWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return runtimekube.Namespace{}, undecodable("get namespace")
	}
	return runtimekube.Namespace{Name: wire.Metadata.Name, Labels: wire.Metadata.Labels}, nil
}
