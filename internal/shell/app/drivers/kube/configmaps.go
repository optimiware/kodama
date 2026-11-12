package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// ConfigMap request and response bodies. Activation bytes always travel as
// binaryData so arbitrary content round-trips exactly.

type configMapBody struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMetaBody    `json:"metadata"`
	Immutable  bool              `json:"immutable"`
	BinaryData map[string]string `json:"binaryData,omitempty"`
}

type configMapWire struct {
	Metadata   objectMeta        `json:"metadata"`
	Immutable  bool              `json:"immutable"`
	BinaryData map[string]string `json:"binaryData"`
}

// PutConfigMap creates the immutable activation snapshot once. A conflicting
// create is resolved by observing the existing object: identical content reuses
// it, changed content is ErrConflict. A non-empty ResourceVersion is refused
// because the snapshot is create-only.
func (c *Cluster) PutConfigMap(ctx context.Context, configMap runtimekube.ConfigMap) (runtimekube.ConfigMap, error) {
	if configMap.ResourceVersion != "" {
		return runtimekube.ConfigMap{}, fault.New(fault.InvalidInput,
			"kubernetes ConfigMap activation snapshot is create-only")
	}
	if configMap.Name == "" || configMap.Namespace == "" {
		return runtimekube.ConfigMap{}, fault.New(fault.InvalidInput,
			"kubernetes ConfigMap requires a name and namespace")
	}
	body := configMapBody{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: objectMetaBody{
			Name:      configMap.Name,
			Namespace: configMap.Namespace,
			Labels:    copyStringMap(configMap.Labels),
		},
		Immutable:  configMap.Immutable,
		BinaryData: encodeConfigMapData(configMap.Data),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return runtimekube.ConfigMap{}, fault.New(fault.InternalFailure, "kubernetes ConfigMap body is not encodable")
	}
	data, status, err := c.exchange(ctx, http.MethodPost, configMapsPath(configMap.Namespace), nil, payload, jsonContentType)
	if err != nil {
		return runtimekube.ConfigMap{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return decodeConfigMap(data, "put ConfigMap")
	case http.StatusConflict:
		existing, err := c.GetConfigMap(ctx, configMap.Namespace, configMap.Name)
		if err != nil {
			return runtimekube.ConfigMap{}, err
		}
		if !sameBytes(existing.Data, configMap.Data) {
			return runtimekube.ConfigMap{}, fault.Wrap(fault.ChangedIdempotencyContent,
				"kubernetes ConfigMap activation snapshot content changed", runtimekube.ErrConflict)
		}
		return existing, nil
	default:
		return runtimekube.ConfigMap{}, statusError("put ConfigMap", status, true)
	}
}

// GetConfigMap returns one observed ConfigMap. A missing object is ErrNotFound.
func (c *Cluster) GetConfigMap(ctx context.Context, namespace, name string) (runtimekube.ConfigMap, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, configMapPath(namespace, name), nil, nil, "")
	if err != nil {
		return runtimekube.ConfigMap{}, err
	}
	if status != http.StatusOK {
		return runtimekube.ConfigMap{}, statusError("get ConfigMap", status, false)
	}
	return decodeConfigMap(data, "get ConfigMap")
}

// DeleteConfigMap deletes one ConfigMap. A missing object is ErrNotFound.
func (c *Cluster) DeleteConfigMap(ctx context.Context, namespace, name string) error {
	_, status, err := c.exchange(ctx, http.MethodDelete, configMapPath(namespace, name), nil, nil, jsonContentType)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return statusError("delete ConfigMap", status, false)
	}
	return nil
}

// encodeConfigMapData encodes every activation path as a valid ConfigMap key.
func encodeConfigMapData(values map[string][]byte) map[string]string {
	if len(values) == 0 {
		return nil
	}
	encoded := make(map[string]string, len(values))
	for key, value := range values {
		encoded[encodeConfigMapKey(key)] = base64.StdEncoding.EncodeToString(value)
	}
	return encoded
}

// decodeConfigMap parses one ConfigMap response and restores the activation
// paths from their encoded keys.
func decodeConfigMap(data []byte, operation string) (runtimekube.ConfigMap, error) {
	var wire configMapWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return runtimekube.ConfigMap{}, undecodable(operation)
	}
	decoded, err := decodeConfigMapData(wire.BinaryData)
	if err != nil {
		return runtimekube.ConfigMap{}, undecodable(operation)
	}
	return runtimekube.ConfigMap{
		Name:            wire.Metadata.Name,
		Namespace:       wire.Metadata.Namespace,
		Labels:          wire.Metadata.Labels,
		Data:            decoded,
		Immutable:       wire.Immutable,
		ResourceVersion: wire.Metadata.ResourceVersion,
	}, nil
}
