// Fresh-only workspace claim provisioning for the Kubernetes runtime.
//
// The Kubernetes adapter never creates a PersistentVolumeClaim: Prepare and
// Retain refuse a missing claim, so a lost workspace of an execution that
// already ran fails visibly instead of silently starting empty. The Kernel
// calls engine.Workspace.Retain before it commits an activation
// (internal/kernel/engine/reconcile.go activate), so the activating action is
// still pending when Retain runs. The composition therefore creates the
// deterministic workspace claim before Retain, but only for a never-activated
// workspace.
//
// The seam-external driver primitive
// (*driverkube.Cluster).CreateClaim is idempotent, adopts only an
// exactly-owned existing claim, refuses a foreign claim as forbidden and
// reports an uncertain outcome as driverkube.ErrAmbiguous. This file consumes
// it through the app-owned ClaimProvisioner port so the app package never
// depends on a concrete cluster.
package app

import (
	"context"
	"errors"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// ClaimProvisioner is the one external effect the Kubernetes decorator adds:
// the idempotent creation of the deterministic owned workspace claim. It is
// satisfied by the real *drivers/kube.Cluster.
type ClaimProvisioner interface {
	CreateClaim(ctx context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error)
}

var _ ClaimProvisioner = (*driverkube.Cluster)(nil)

// WorkspacePVC is the trusted workspace claim template.
//
// Every value comes from composition configuration and is never derived from a
// request, a principal or a rendered workload. The template namespace must
// equal the runtime adapter's configured tenant namespace; the
// engine.Workspace seam exposes no namespace, so that pairing is an operator
// configuration invariant, while the durable owning tenant is checked by the
// decorator for every fresh workspace.
type WorkspacePVC struct {
	// Namespace is the tenant namespace that owns the claim. Required.
	Namespace string
	// TenantID is the tenant ownership label value. Required and validated.
	TenantID id.TenantID
	// StorageClass is the requested StorageClass. Required.
	StorageClass string
	// Size is the requested storage quantity, for example "64Mi". Required.
	Size string
	// AccessMode is one of the four Kubernetes PVC access modes. Required.
	AccessMode string
}

// workspaceClaimAccessModes is the closed set of PVC access modes accepted by
// the driver's CreateClaim. It mirrors the driver-owned set; a template value
// outside it would be refused by the driver after the tenant fact was read, so
// it is refused at construction and validation instead.
var workspaceClaimAccessModes = map[string]bool{
	"ReadWriteOnce":    true,
	"ReadOnlyMany":     true,
	"ReadWriteMany":    true,
	"ReadWriteOncePod": true,
}

// validateWorkspacePVC refuses an incomplete or unusable trusted template
// before any adapter or external effect exists. It never renders a configured
// value.
func validateWorkspacePVC(template WorkspacePVC) error {
	if strings.TrimSpace(template.Namespace) == "" {
		return fault.New(fault.InvalidInput, "workspace claim template requires a namespace")
	}
	if _, err := id.ParseTenantID(string(template.TenantID)); err != nil {
		return fault.New(fault.InvalidInput, "workspace claim template tenant id is invalid")
	}
	if strings.TrimSpace(template.StorageClass) == "" {
		return fault.New(fault.InvalidInput, "workspace claim template requires a storage class")
	}
	if strings.TrimSpace(template.Size) == "" {
		return fault.New(fault.InvalidInput, "workspace claim template requires a storage size")
	}
	if strings.TrimSpace(template.AccessMode) == "" {
		return fault.New(fault.InvalidInput, "workspace claim template requires an access mode")
	}
	if !workspaceClaimAccessModes[template.AccessMode] {
		return fault.New(fault.InvalidInput, "workspace claim template access mode is not supported")
	}
	return nil
}

// kubeProvisioningWorkspace decorates the Kernel workspace port with fresh-only
// workspace claim provisioning. Status and Release delegate unchanged.
type kubeProvisioningWorkspace struct {
	inner       kernelengine.Workspace
	provisioner ClaimProvisioner
	activation  WorkspaceActivation
	claim       WorkspacePVC
}

var _ kernelengine.Workspace = (*kubeProvisioningWorkspace)(nil)

// newKubeProvisioningWorkspace validates every dependency and the trusted
// template and returns the decorator. An incomplete decorator is refused at
// construction rather than failing open later.
func newKubeProvisioningWorkspace(inner kernelengine.Workspace, provisioner ClaimProvisioner, activation WorkspaceActivation, claim WorkspacePVC) (kernelengine.Workspace, error) {
	if inner == nil {
		return nil, fault.New(fault.InvalidInput, "claim provisioning workspace requires a workspace adapter")
	}
	if provisioner == nil {
		return nil, fault.New(fault.InvalidInput, "claim provisioning workspace requires a claim provisioner")
	}
	if activation == nil {
		return nil, fault.New(fault.InvalidInput, "claim provisioning workspace requires an activation fact source")
	}
	if err := validateWorkspacePVC(claim); err != nil {
		return nil, err
	}
	return &kubeProvisioningWorkspace{
		inner:       inner,
		provisioner: provisioner,
		activation:  activation,
		claim:       claim,
	}, nil
}

// Status delegates unchanged and never provisions.
func (workspace *kubeProvisioningWorkspace) Status(ctx context.Context, workspaceID id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	return workspace.inner.Status(ctx, workspaceID)
}

// Release delegates unchanged and never provisions.
func (workspace *kubeProvisioningWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	return workspace.inner.Release(ctx, workspaceID)
}

// Retain provisions the deterministic workspace claim exactly once for a fresh
// workspace, then delegates retention. An already-activated workspace is never
// provisioned: a missing claim surfaces from the adapter's Retain as not_found,
// which is the intended workspace-loss evidence.
//
// The trusted template tenant must equal the durable owning tenant of the
// workspace's execution. A mismatch is forbidden before any external effect.
// A cancelled context performs no external effect and reads no fact. A failed
// or uncertain provision is returned without delegating Retain: the driver's
// ambiguous outcome stays reachable as uncertain_external_effect, and no
// namespace, tenant, storage class, size, access mode or derived claim name is
// rendered into an error.
//
// The decorator never waits for the claim to become Bound. A
// WaitForFirstConsumer StorageClass only binds the claim when the workload Job
// runs, so waiting here would deadlock the activation that triggered Retain.
func (workspace *kubeProvisioningWorkspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	previouslyActivated, err := workspace.activation.PreviouslyActivated(ctx, workspaceID)
	if err != nil {
		// An unknown activation fact fails closed: never provision, never
		// retain, and never guess that the workspace is fresh.
		return err
	}
	if previouslyActivated {
		return workspace.inner.Retain(ctx, workspaceID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tenant, err := workspace.activation.OwningTenant(ctx, workspaceID)
	if err != nil {
		// An unknown tenant fact fails closed exactly like an unknown
		// activation fact.
		return err
	}
	if tenant != workspace.claim.TenantID {
		// Fixed text: neither tenant value is rendered.
		return fault.New(fault.Forbidden,
			"workspace claim template tenant does not own the workspace execution")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	spec := driverkube.ClaimSpec{
		Namespace:    workspace.claim.Namespace,
		TenantID:     workspace.claim.TenantID,
		WorkspaceID:  workspaceID,
		StorageClass: workspace.claim.StorageClass,
		Size:         workspace.claim.Size,
		AccessMode:   workspace.claim.AccessMode,
	}
	if _, err := workspace.provisioner.CreateClaim(ctx, spec); err != nil {
		if errors.Is(err, driverkube.ErrAmbiguous) {
			return fault.Wrap(fault.UncertainExternalEffect,
				"workspace claim provisioning outcome is unknown", err)
		}
		return err
	}
	return workspace.inner.Retain(ctx, workspaceID)
}
