// Retained-workspace port for the Kubernetes adapter.
//
// The port is deliberately separate from Runtime because workspace ownership
// outlives one workload. Status only observes; Retain persists a durable marker;
// Release deletes the claim only after every Job/Pod reference is gone, and a
// conditional update/delete prevents a stale Release from dropping a workspace
// that another controller retained meanwhile. No operation ever creates a
// missing claim, adopts a foreign claim, or reports an uncertain deletion as
// success.
package kube

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

const retainedValue = "true"

// lockWorkspace serializes one workspace's lifecycle inside this adapter and
// shares the lock with Prepare, Start and Remove.
func (a *Adapter) lockWorkspace(workspaceID id.WorkspaceID) func() {
	a.locksMu.Lock()
	if a.locks == nil {
		a.locks = map[id.WorkspaceID]*workspaceLock{}
	}
	lock := a.locks[workspaceID]
	if lock == nil {
		lock = &workspaceLock{}
		a.locks[workspaceID] = lock
	}
	lock.refs++
	a.locksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		a.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(a.locks, workspaceID)
		}
		a.locksMu.Unlock()
	}
}

// checkTenantNamespace proves the adapter's bound namespace still belongs to
// this tenant before any workspace effect.
func (a *Adapter) checkTenantNamespace(ctx context.Context) error {
	namespace, err := a.cluster.GetNamespace(ctx, a.namespace)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "tenant namespace is unavailable", err)
	}
	if namespace.Labels[LabelTenant] != string(a.tenant) {
		return fault.New(fault.Forbidden, "namespace is not owned by this tenant")
	}
	return nil
}

// unownedClaimReason explains why a claim at the deterministic name is not this
// installation's workspace. An empty reason means the claim is owned.
func (a *Adapter) unownedClaimReason(claim Claim, workspaceID id.WorkspaceID) string {
	switch {
	case claim.Namespace != a.namespace:
		return "claim is in another namespace"
	case claim.Labels[LabelTenant] != string(a.tenant):
		return "claim is not owned by this tenant"
	case claim.Labels[LabelWorkspace] != string(workspaceID):
		return "claim is not owned by this workspace"
	default:
		return ""
	}
}

// Status reports availability evidence without changing anything. A missing,
// foreign, unlabelled or terminating claim is unavailable and is never
// recreated or adopted.
func (a *Adapter) Status(ctx context.Context, workspaceID id.WorkspaceID) (engine.WorkspaceObservation, error) {
	observation := engine.WorkspaceObservation{WorkspaceID: workspaceID}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return observation, err
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}

	unlock := a.lockWorkspace(workspaceID)
	defer unlock()

	if err := a.checkTenantNamespace(ctx); err != nil {
		return observation, err
	}
	claimName, err := WorkspaceClaimName(workspaceID)
	if err != nil {
		return observation, err
	}
	claim, err := a.cluster.GetClaim(ctx, a.namespace, claimName)
	switch {
	case errors.Is(err, ErrNotFound):
		observation.Detail = fmt.Sprintf("retained workspace claim %q is missing; it is never recreated", claimName)
		return observation, nil
	case err != nil:
		return observation, fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable", err)
	}
	if reason := a.unownedClaimReason(claim, workspaceID); reason != "" {
		observation.Detail = fmt.Sprintf("workspace claim %q is not available: %s", claimName, reason)
		return observation, nil
	}
	if claim.DeletionTimestamp != nil {
		observation.Detail = fmt.Sprintf("workspace claim %q is terminating and is not available", claimName)
		return observation, nil
	}
	observation.Available = true
	observation.Detail = fmt.Sprintf("workspace claim %q is present and owned by this installation", claimName)
	return observation, nil
}

// Retain persists a durable retention marker. It is idempotent and never
// recreates a missing claim or adopts a foreign one.
func (a *Adapter) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	unlock := a.lockWorkspace(workspaceID)
	defer unlock()

	if err := a.checkTenantNamespace(ctx); err != nil {
		return err
	}
	claimName, err := WorkspaceClaimName(workspaceID)
	if err != nil {
		return err
	}
	claim, err := a.cluster.GetClaim(ctx, a.namespace, claimName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fault.New(fault.NotFound, fmt.Sprintf("retained workspace claim %q is missing; retain never recreates it", claimName))
		}
		return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable", err)
	}
	if reason := a.unownedClaimReason(claim, workspaceID); reason != "" {
		return fault.New(fault.Forbidden, "workspace claim is not owned by this installation: "+reason)
	}
	if claim.DeletionTimestamp != nil {
		return fault.New(fault.DependencyUnavailable, fmt.Sprintf("workspace claim %q is terminating and cannot be retained", claimName))
	}
	if claimRetained(claim) {
		return nil
	}

	retained := copyClaim(claim)
	if retained.Annotations == nil {
		retained.Annotations = map[string]string{}
	}
	if retained.Labels == nil {
		retained.Labels = map[string]string{}
	}
	retained.Annotations[AnnotationRetained] = retainedValue
	retained.Labels[LabelRetained] = retainedValue
	if _, err := a.cluster.UpdateClaim(ctx, retained); err != nil {
		if errors.Is(err, ErrConflict) {
			// A concurrent Retain may already have won. Re-read before
			// reporting a conflict; never overwrite the newer marker.
			current, getErr := a.cluster.GetClaim(ctx, a.namespace, claimName)
			switch {
			case getErr == nil && claimRetained(current):
				return nil
			case errors.Is(getErr, ErrNotFound):
				return fault.New(fault.NotFound, fmt.Sprintf("retained workspace claim %q disappeared during retain", claimName))
			case getErr != nil:
				return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable after a retention conflict", getErr)
			default:
				return fault.Wrap(fault.VersionConflict, "workspace claim changed during retain", err)
			}
		}
		if errors.Is(err, ErrNotFound) {
			return fault.New(fault.NotFound, fmt.Sprintf("retained workspace claim %q disappeared during retain", claimName))
		}
		return fault.Wrap(fault.UncertainExternalEffect, "workspace retention outcome is unknown", err)
	}
	return nil
}

// Release deletes the retained workspace claim only when no Job or live or
// uncertain Pod references it and its durable retention marker can be cleared.
// A missing claim is already released. An owned, unreferenced, unretained claim
// that is already terminating, or a claim that is still present with a deletion
// timestamp right after our own accepted versioned delete, is an accepted
// deletion in progress: Kubernetes completes it asynchronously (a PVC keeps the
// kubernetes.io/pvc-protection finalizer for that window), so Release returns
// nil and the Kernel cleanup loop completes instead of spending one of its
// bounded attempts. A claim that is still referenced is never reported
// released, a retained claim is never reported released until its durable
// marker is cleared, and an uncertain deletion is still an error. When an
// external deletion has already started a retained claim (a lost workspace),
// the in-progress path clears the marker with the same conditional metadata
// update the live path uses and then accepts the deletion, so the release can
// settle instead of exhausting cleanup retries on a workspace Kubernetes is
// already removing.
func (a *Adapter) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	unlock := a.lockWorkspace(workspaceID)
	defer unlock()

	if err := a.checkTenantNamespace(ctx); err != nil {
		return err
	}
	claimName, err := WorkspaceClaimName(workspaceID)
	if err != nil {
		return err
	}
	claim, err := a.cluster.GetClaim(ctx, a.namespace, claimName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable", err)
	}
	if reason := a.unownedClaimReason(claim, workspaceID); reason != "" {
		return fault.New(fault.Forbidden, "workspace claim is not owned by this installation: "+reason)
	}
	// The reference guard applies to every release path, including a claim that
	// is already terminating: an accepted deletion is not evidence that no
	// workload still writes the volume.
	if err := a.ensureNoWorkspaceReference(ctx, workspaceID); err != nil {
		return err
	}
	if claim.DeletionTimestamp != nil {
		return a.acceptDeletionInProgress(ctx, claim, claimName)
	}

	if claimRetained(claim) {
		cleared := copyClaim(claim)
		delete(cleared.Annotations, AnnotationRetained)
		delete(cleared.Labels, LabelRetained)
		updated, err := a.cluster.UpdateClaim(ctx, cleared)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				current, getErr := a.cluster.GetClaim(ctx, a.namespace, claimName)
				switch {
				case getErr == nil && claimRetained(current):
					return fault.Wrap(fault.VersionConflict, "workspace was retained concurrently; release refused", err)
				case errors.Is(getErr, ErrNotFound):
					return nil
				case getErr != nil:
					return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable after a release conflict", getErr)
				default:
					return fault.Wrap(fault.VersionConflict, "workspace claim changed during release", err)
				}
			}
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return fault.Wrap(fault.UncertainExternalEffect, "workspace retention clear outcome is unknown", err)
		}
		claim = updated
	}

	if err := a.cluster.DeleteClaimVersioned(ctx, a.namespace, claimName, claim.ResourceVersion); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if errors.Is(err, ErrConflict) {
			current, getErr := a.cluster.GetClaim(ctx, a.namespace, claimName)
			switch {
			case getErr == nil && claimRetained(current):
				return fault.Wrap(fault.VersionConflict, "workspace was retained concurrently; release refused", err)
			case errors.Is(getErr, ErrNotFound):
				return nil
			case getErr != nil:
				return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable after a deletion conflict", getErr)
			default:
				return fault.Wrap(fault.VersionConflict, "workspace claim changed during release", err)
			}
		}
		return fault.Wrap(fault.UncertainExternalEffect, "workspace deletion outcome is unknown", err)
	}

	// The API server accepted the delete, but a finalizer-protected claim stays
	// present with a deletion timestamp until its teardown completes. Read it
	// back once: absence confirms the release, and an owned, unretained,
	// terminating claim is the same accepted deletion in progress. A claim that
	// is still live is not a confirmed release.
	current, err := a.cluster.GetClaim(ctx, a.namespace, claimName)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return fault.Wrap(fault.UncertainExternalEffect, "workspace release outcome is unknown", err)
	}
	if reason := a.unownedClaimReason(current, workspaceID); reason != "" {
		return fault.New(fault.UncertainExternalEffect, "workspace claim changed ownership during release: "+reason)
	}
	if current.DeletionTimestamp == nil {
		return fault.New(fault.UncertainExternalEffect, "workspace claim is still present after deletion")
	}
	return a.acceptDeletionInProgress(ctx, current, claimName)
}

// acceptDeletionInProgress accepts an owned, unreferenced claim whose deletion
// is already running as released-in-progress. The retention guard still holds:
// a release never reports a workspace released while its durable retention
// marker is present. A retained terminating claim therefore has its marker
// cleared first, with the same conditional metadata update the live release
// path uses; an external deletion cannot be reversed, so refusing to clear the
// marker would leave a lost workspace owing cleanup forever. A conflicting
// concurrent retention, a disappearing claim and an unknown clear outcome are
// classified exactly like the live path: only a currently retained claim is
// refused, and an unretained or absent claim is released.
func (a *Adapter) acceptDeletionInProgress(ctx context.Context, claim Claim, claimName string) error {
	if !claimRetained(claim) {
		return nil
	}
	cleared := copyClaim(claim)
	delete(cleared.Annotations, AnnotationRetained)
	delete(cleared.Labels, LabelRetained)
	if _, err := a.cluster.UpdateClaim(ctx, cleared); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case errors.Is(err, ErrConflict):
			current, getErr := a.cluster.GetClaim(ctx, a.namespace, claimName)
			switch {
			case errors.Is(getErr, ErrNotFound):
				return nil
			case getErr != nil:
				return fault.Wrap(fault.DependencyUnavailable, "workspace claim is unreachable after an in-progress release conflict", getErr)
			case claimRetained(current):
				return fault.Wrap(fault.VersionConflict, "workspace was retained concurrently; release refused", err)
			default:
				return nil
			}
		default:
			return fault.Wrap(fault.UncertainExternalEffect, "workspace retention clear outcome is unknown", err)
		}
	}
	return nil
}

// ensureNoWorkspaceReference refuses release while any Job or a live or
// uncertain Pod still references the workspace.
func (a *Adapter) ensureNoWorkspaceReference(ctx context.Context, workspaceID id.WorkspaceID) error {
	selector := map[string]string{LabelWorkspace: string(workspaceID)}
	jobs, err := a.cluster.ListJobs(ctx, a.namespace, selector)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace Job references are unreachable", err)
	}
	if len(jobs) > 0 {
		return fault.New(fault.Forbidden, fmt.Sprintf("workspace is still referenced by %d Job(s)", len(jobs)))
	}
	pods, err := a.cluster.ListPods(ctx, a.namespace, selector)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace Pod references are unreachable", err)
	}
	if !workspaceReleasable(pods) {
		return fault.New(fault.Forbidden, fmt.Sprintf("workspace is still referenced by %d Pod(s)", len(pods)))
	}
	return nil
}

// claimRetained reports the durable marker in either of its representations.
func claimRetained(claim Claim) bool {
	return claim.Annotations[AnnotationRetained] == retainedValue || claim.Labels[LabelRetained] == retainedValue
}

// copyClaim deep-copies the mutable claim maps so a conditional update cannot
// mutate the caller's snapshot.
func copyClaim(claim Claim) Claim {
	copied := claim
	copied.Labels = copyStringMap(claim.Labels)
	copied.Annotations = copyStringMap(claim.Annotations)
	copied.Finalizers = append([]string(nil), claim.Finalizers...)
	if claim.DeletionTimestamp != nil {
		deleted := *claim.DeletionTimestamp
		copied.DeletionTimestamp = &deleted
	}
	return copied
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}
