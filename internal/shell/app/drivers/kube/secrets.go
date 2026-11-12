package kube

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// Secret request and response bodies. Secret bytes travel as base64 data and the
// type is always the explicit Opaque type.

type secretBody struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMetaBody    `json:"metadata"`
	Type       string            `json:"type"`
	Data       map[string]string `json:"data,omitempty"`
}

type secretWire struct {
	Metadata  objectMeta        `json:"metadata"`
	Type      string            `json:"type"`
	Immutable bool              `json:"immutable"`
	Data      map[string]string `json:"data"`
}

// PutSecret upserts the mutable credential Secret. An empty ResourceVersion
// creates and falls back to GET + PUT with the observed version on a create
// conflict; a set ResourceVersion is one conditional PUT whose 409 is
// ErrConflict.
func (c *Cluster) PutSecret(ctx context.Context, secret runtimekube.Secret) (runtimekube.Secret, error) {
	if secret.Name == "" || secret.Namespace == "" {
		return runtimekube.Secret{}, fault.New(fault.InvalidInput,
			"kubernetes Secret requires a name and namespace")
	}
	if secret.ResourceVersion != "" {
		return c.updateSecret(ctx, secret, secret.ResourceVersion)
	}
	payload, err := secretPayload(secret, "")
	if err != nil {
		return runtimekube.Secret{}, err
	}
	data, status, err := c.exchange(ctx, http.MethodPost, secretsPath(secret.Namespace), nil, payload, jsonContentType)
	if err != nil {
		return runtimekube.Secret{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return decodeSecret(data, "put Secret")
	case http.StatusConflict:
		observed, err := c.GetSecret(ctx, secret.Namespace, secret.Name)
		if err != nil {
			return runtimekube.Secret{}, err
		}
		if observed.ResourceVersion == "" {
			return runtimekube.Secret{}, ambiguous("kubernetes Secret upsert could not observe a resource version")
		}
		return c.updateSecret(ctx, secret, observed.ResourceVersion)
	default:
		return runtimekube.Secret{}, statusError("put Secret", status, true)
	}
}

// updateSecret sends one conditional update guarded by expectedVersion.
func (c *Cluster) updateSecret(ctx context.Context, secret runtimekube.Secret, expectedVersion string) (runtimekube.Secret, error) {
	payload, err := secretPayload(secret, expectedVersion)
	if err != nil {
		return runtimekube.Secret{}, err
	}
	data, status, err := c.exchange(ctx, http.MethodPut, secretPath(secret.Namespace, secret.Name), nil, payload, jsonContentType)
	if err != nil {
		return runtimekube.Secret{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return runtimekube.Secret{}, statusError("update Secret", status, false)
	}
	return decodeSecret(data, "update Secret")
}

// secretPayload renders one Secret body. An immutable marker is never sent: the
// Secret is the mutable credential surface.
func secretPayload(secret runtimekube.Secret, resourceVersion string) ([]byte, error) {
	body := secretBody{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: objectMetaBody{
			Name:            secret.Name,
			Namespace:       secret.Namespace,
			ResourceVersion: resourceVersion,
			Labels:          copyStringMap(secret.Labels),
		},
		Type: "Opaque",
		Data: encodeBase64Data(secret.Data),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fault.New(fault.InternalFailure, "kubernetes Secret body is not encodable")
	}
	return payload, nil
}

// GetSecret returns one observed Secret with decoded data. A missing object is
// ErrNotFound.
func (c *Cluster) GetSecret(ctx context.Context, namespace, name string) (runtimekube.Secret, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, secretPath(namespace, name), nil, nil, "")
	if err != nil {
		return runtimekube.Secret{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Secret{}, statusError("get Secret", status, false)
	}
	return decodeSecret(data, "get Secret")
}

// DeleteSecret deletes one Secret. A missing object is ErrNotFound.
func (c *Cluster) DeleteSecret(ctx context.Context, namespace, name string) error {
	_, status, err := c.exchange(ctx, http.MethodDelete, secretPath(namespace, name), nil, nil, jsonContentType)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return statusError("delete Secret", status, false)
	}
	return nil
}

// decodeSecret parses one Secret response and decodes its data.
func decodeSecret(data []byte, operation string) (runtimekube.Secret, error) {
	var wire secretWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return runtimekube.Secret{}, undecodable(operation)
	}
	decoded, err := decodeBase64Data(wire.Data)
	if err != nil {
		return runtimekube.Secret{}, undecodable(operation)
	}
	return runtimekube.Secret{
		Name:            wire.Metadata.Name,
		Namespace:       wire.Metadata.Namespace,
		Labels:          wire.Metadata.Labels,
		Data:            decoded,
		Immutable:       wire.Immutable,
		ResourceVersion: wire.Metadata.ResourceVersion,
	}, nil
}
