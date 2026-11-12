package kube

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// PersistentVolumeClaim request and response bodies.

type claimWire struct {
	Metadata objectMeta `json:"metadata"`
}

type deleteOptionsBody struct {
	APIVersion        string                   `json:"apiVersion"`
	Kind              string                   `json:"kind"`
	PropagationPolicy string                   `json:"propagationPolicy,omitempty"`
	Preconditions     *deletePreconditionsBody `json:"preconditions,omitempty"`
}

type deletePreconditionsBody struct {
	ResourceVersion string `json:"resourceVersion"`
}

// GetClaim returns one observed workspace claim, including its finalizers and a
// pending deletion timestamp.
func (c *Cluster) GetClaim(ctx context.Context, namespace, name string) (runtimekube.Claim, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, claimPath(namespace, name), nil, nil, "")
	if err != nil {
		return runtimekube.Claim{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Claim{}, statusError("get workspace claim", status, false)
	}
	return decodeClaim(data, "get workspace claim")
}

// UpdateClaim applies a metadata-only merge patch guarded by the caller's
// resourceVersion. The patch never sends spec, finalizers or the deletion
// timestamp, and it removes a server-side key the caller dropped with an
// explicit null.
func (c *Cluster) UpdateClaim(ctx context.Context, claim runtimekube.Claim) (runtimekube.Claim, error) {
	if claim.ResourceVersion == "" {
		return runtimekube.Claim{}, fault.New(fault.InvalidInput,
			"kubernetes workspace claim update requires a resource version")
	}
	if claim.Name == "" || claim.Namespace == "" {
		return runtimekube.Claim{}, fault.New(fault.InvalidInput,
			"kubernetes workspace claim requires a name and namespace")
	}
	observed, err := c.GetClaim(ctx, claim.Namespace, claim.Name)
	if err != nil {
		return runtimekube.Claim{}, err
	}
	if observed.ResourceVersion != claim.ResourceVersion {
		return runtimekube.Claim{}, fault.Wrap(fault.VersionConflict,
			"kubernetes workspace claim changed before update", runtimekube.ErrConflict)
	}
	payload, err := json.Marshal(buildClaimPatch(claim, observed))
	if err != nil {
		return runtimekube.Claim{}, fault.New(fault.InternalFailure, "kubernetes claim patch is not encodable")
	}
	data, status, err := c.exchange(ctx, http.MethodPatch, claimPath(claim.Namespace, claim.Name), nil, payload, mergePatchContentType)
	if err != nil {
		return runtimekube.Claim{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Claim{}, statusError("update workspace claim", status, false)
	}
	return decodeClaim(data, "update workspace claim")
}

// buildClaimPatch renders the metadata-only merge patch. Only labels and
// annotations are mutable here.
func buildClaimPatch(claim, observed runtimekube.Claim) map[string]any {
	metadata := map[string]any{"resourceVersion": claim.ResourceVersion}
	if labels, ok := mergeMetadataMap(observed.Labels, claim.Labels); ok {
		metadata["labels"] = labels
	}
	if annotations, ok := mergeMetadataMap(observed.Annotations, claim.Annotations); ok {
		metadata["annotations"] = annotations
	}
	return map[string]any{"metadata": metadata}
}

// mergeMetadataMap builds a merge-patch map that sets every desired key and
// removes every observed key the caller dropped with an explicit null.
func mergeMetadataMap(observed, desired map[string]string) (map[string]any, bool) {
	merged := make(map[string]any, len(observed)+len(desired))
	for key, value := range desired {
		merged[key] = value
	}
	for key := range observed {
		if _, ok := desired[key]; !ok {
			merged[key] = nil
		}
	}
	if len(merged) == 0 {
		return nil, false
	}
	return merged, true
}

// DeleteClaim deletes one workspace claim without a precondition.
func (c *Cluster) DeleteClaim(ctx context.Context, namespace, name string) error {
	_, status, err := c.exchange(ctx, http.MethodDelete, claimPath(namespace, name), nil, nil, jsonContentType)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return statusError("delete workspace claim", status, false)
	}
	return nil
}

// DeleteClaimVersioned deletes a claim only when it still carries
// expectedVersion, so a stale release never drops a retained workspace.
func (c *Cluster) DeleteClaimVersioned(ctx context.Context, namespace, name, expectedVersion string) error {
	if expectedVersion == "" {
		return fault.New(fault.InvalidInput,
			"kubernetes versioned claim deletion requires a resource version")
	}
	payload, err := json.Marshal(deleteOptionsBody{
		APIVersion:    "v1",
		Kind:          "DeleteOptions",
		Preconditions: &deletePreconditionsBody{ResourceVersion: expectedVersion},
	})
	if err != nil {
		return fault.New(fault.InternalFailure, "kubernetes delete options are not encodable")
	}
	_, status, err := c.exchange(ctx, http.MethodDelete, claimPath(namespace, name), nil, payload, jsonContentType)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return statusError("delete workspace claim", status, false)
	}
	return nil
}

// decodeClaim parses one claim response.
func decodeClaim(data []byte, operation string) (runtimekube.Claim, error) {
	var wire claimWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return runtimekube.Claim{}, undecodable(operation)
	}
	deleted, err := wire.Metadata.parseDeletionTimestamp()
	if err != nil {
		return runtimekube.Claim{}, undecodable(operation)
	}
	return runtimekube.Claim{
		Name:              wire.Metadata.Name,
		Namespace:         wire.Metadata.Namespace,
		Labels:            wire.Metadata.Labels,
		Annotations:       wire.Metadata.Annotations,
		Finalizers:        wire.Metadata.Finalizers,
		DeletionTimestamp: deleted,
		ResourceVersion:   wire.Metadata.ResourceVersion,
	}, nil
}
