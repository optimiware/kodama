package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// ClaimSpec is the driver-owned provisioning request for one retained workspace
// claim.
//
// The runtimekube.Cluster seam deliberately has no claim-create method:
// the adapter only observes, retains, updates and deletes a claim that must
// already exist. This concrete, seam-external primitive is the one place the
// Shell provisions that claim, so a composed server never has to synthesize a
// PVC outside the driver's pinned request shape.
//
// The object name is derived from WorkspaceID with the same deterministic
// WorkspaceClaimName the adapter uses, so a retry after a lost response
// addresses the identical claim. Only the two ownership labels the adapter's
// unownedClaimReason validates are stamped; the retention marker belongs to
// Retain and is never pre-set here.
type ClaimSpec struct {
	// Namespace is the tenant namespace that owns the claim. Required.
	Namespace string
	// TenantID is the tenant ownership label value. Required and validated.
	TenantID id.TenantID
	// WorkspaceID is the stable workspace identity the claim name and workspace
	// ownership label are derived from. Required and validated.
	WorkspaceID id.WorkspaceID
	// StorageClass is the requested StorageClass. Required.
	StorageClass string
	// Size is the requested storage quantity, for example "64Mi". Required.
	Size string
	// AccessMode is one of the four Kubernetes PVC access modes. Required.
	AccessMode string
}

// claimCreateBody is the exact persistentvolumeclaims request body. An empty
// annotations map is omitted so a create never pre-sets the retention marker.
type claimCreateBody struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetaBody `json:"metadata"`
	Spec       claimSpecBody  `json:"spec"`
}

// claimSpecBody is the requested storage specification.
type claimSpecBody struct {
	AccessModes      []string           `json:"accessModes"`
	Resources        claimResourcesBody `json:"resources"`
	StorageClassName string             `json:"storageClassName"`
	VolumeMode       string             `json:"volumeMode"`
}

// claimResourcesBody carries the requested storage quantity.
type claimResourcesBody struct {
	Requests map[string]string `json:"requests"`
}

// claimAccessModes is the closed set of Kubernetes PVC access modes.
var claimAccessModes = map[string]bool{
	"ReadWriteOnce":    true,
	"ReadOnlyMany":     true,
	"ReadWriteMany":    true,
	"ReadWriteOncePod": true,
}

// CreateClaim provisions the deterministic workspace claim for one retained
// workspace.
//
// The driver creates first and never pre-reads: a lost create response is
// ErrAmbiguous, and the retry converges on the identical claim. A 409 is
// reconciled with a GET and the claim is adopted only when both ownership labels
// match exactly and it is not terminating. A foreign, differently-owned or
// unlabeled claim is Forbidden and is never overwritten; a terminating claim is
// a VersionConflict. A 2xx whose identity or ownership labels do not describe
// the requested claim is an untrustworthy success and stays ErrAmbiguous, like
// any other malformed 2xx.
func (c *Cluster) CreateClaim(ctx context.Context, spec ClaimSpec) (runtimekube.Claim, error) {
	manifest, claimName, err := buildClaimManifest(spec)
	if err != nil {
		return runtimekube.Claim{}, err
	}
	if err := ctx.Err(); err != nil {
		return runtimekube.Claim{}, ambiguous(
			"kubernetes create workspace claim was cancelled before any request", err)
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return runtimekube.Claim{}, fault.New(fault.InternalFailure,
			"kubernetes workspace claim manifest is not encodable")
	}
	data, status, err := c.exchange(ctx, http.MethodPost, claimsPath(spec.Namespace), nil, payload, jsonContentType)
	if err != nil {
		return runtimekube.Claim{}, err
	}
	switch {
	case status == http.StatusConflict:
		// The object exists. Never let statusError classify this as
		// ErrAlreadyExists: the retry must reconcile the existing claim.
		return c.adoptWorkspaceClaim(ctx, claimName, spec)
	case status < http.StatusOK || status >= http.StatusMultipleChoices:
		return runtimekube.Claim{}, statusError("create workspace claim", status, false)
	}
	claim, err := decodeClaim(data, "create workspace claim")
	if err != nil {
		return runtimekube.Claim{}, err
	}
	if reason := claimOwnershipReason(claim, claimName, spec); reason != "" {
		return runtimekube.Claim{}, ambiguous(
			"kubernetes create workspace claim returned a different claim: " + reason)
	}
	return claim, nil
}

// adoptWorkspaceClaim reconciles one create conflict. The existing claim is
// adopted only when it is exactly the requested workspace claim and is not
// terminating; anything else is refused without a write.
func (c *Cluster) adoptWorkspaceClaim(ctx context.Context, claimName string, spec ClaimSpec) (runtimekube.Claim, error) {
	observed, err := c.GetClaim(ctx, spec.Namespace, claimName)
	if err != nil {
		if errors.Is(err, ErrAmbiguous) {
			return runtimekube.Claim{}, err
		}
		// The create conflicted, so the object exists, but its ownership cannot
		// be read. That is an uncertain outcome, never a fabricated absence and
		// never another classification (a NotFound here must not leak out of a
		// create that conflicted), so the cause is deliberately not joined.
		return runtimekube.Claim{}, ambiguous(
			"kubernetes create workspace claim conflicted and the existing claim is unreadable")
	}
	if reason := claimOwnershipReason(observed, claimName, spec); reason != "" {
		return runtimekube.Claim{}, fault.New(fault.Forbidden,
			"kubernetes workspace claim is already held by another owner: "+reason)
	}
	if observed.DeletionTimestamp != nil {
		return runtimekube.Claim{}, fault.Wrap(fault.VersionConflict,
			"kubernetes workspace claim is terminating and cannot be adopted", runtimekube.ErrConflict)
	}
	return observed, nil
}

// buildClaimManifest validates one provisioning request and renders the exact
// REST body. Every value is checked before any request, so an incomplete or
// unknown request never reaches the API server.
func buildClaimManifest(spec ClaimSpec) (claimCreateBody, string, error) {
	if strings.TrimSpace(spec.Namespace) == "" {
		return claimCreateBody{}, "", fault.New(fault.InvalidInput,
			"kubernetes workspace claim requires a namespace")
	}
	if _, err := id.ParseTenantID(string(spec.TenantID)); err != nil {
		return claimCreateBody{}, "", fault.Wrap(fault.InvalidInput,
			"kubernetes workspace claim tenant id is invalid", err)
	}
	claimName, err := runtimekube.WorkspaceClaimName(spec.WorkspaceID)
	if err != nil {
		return claimCreateBody{}, "", err
	}
	if strings.TrimSpace(spec.StorageClass) == "" {
		return claimCreateBody{}, "", fault.New(fault.InvalidInput,
			"kubernetes workspace claim requires a storage class")
	}
	if strings.TrimSpace(spec.Size) == "" {
		return claimCreateBody{}, "", fault.New(fault.InvalidInput,
			"kubernetes workspace claim requires a storage size")
	}
	if !claimAccessModes[spec.AccessMode] {
		return claimCreateBody{}, "", fault.New(fault.InvalidInput,
			"kubernetes workspace claim access mode is not supported")
	}
	manifest := claimCreateBody{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata: objectMetaBody{
			Name:      claimName,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				runtimekube.LabelTenant:    string(spec.TenantID),
				runtimekube.LabelWorkspace: string(spec.WorkspaceID),
			},
		},
		Spec: claimSpecBody{
			AccessModes: []string{spec.AccessMode},
			Resources: claimResourcesBody{
				Requests: map[string]string{"storage": spec.Size},
			},
			StorageClassName: spec.StorageClass,
			VolumeMode:       "Filesystem",
		},
	}
	return manifest, claimName, nil
}

// claimOwnershipReason explains why a returned claim does not describe the
// requested workspace claim. An empty reason means it matches.
func claimOwnershipReason(claim runtimekube.Claim, claimName string, spec ClaimSpec) string {
	switch {
	case claim.Name != claimName:
		return "claim name does not match the requested claim"
	case claim.Namespace != spec.Namespace:
		return "claim namespace does not match the requested namespace"
	case claim.Labels[runtimekube.LabelTenant] != string(spec.TenantID):
		return "claim is not owned by this tenant"
	case claim.Labels[runtimekube.LabelWorkspace] != string(spec.WorkspaceID):
		return "claim is not owned by this workspace"
	default:
		return ""
	}
}
