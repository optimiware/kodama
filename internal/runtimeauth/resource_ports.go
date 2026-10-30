package runtimeauth

import (
	"context"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- consumer-owned execution ports (resources) --------------------------

// ResourcePhase is the execution module's authoritative view of whether a
// preparation or service may hold runtime authority.
type ResourcePhase string

const (
	// ResourceActive may be admitted and may use its operations.
	ResourceActive ResourcePhase = "active"
	// ResourceEnded: stop intent, a stopped or removed resource, a cancelled,
	// failed or terminal run, or the resource's deadline. All authority is gone
	// and nothing revives it.
	ResourceEnded ResourcePhase = "ended"
)

// ResourceState is what the execution owner tells this module about one
// resource: the committed record, never anything from a request. It carries the
// pinned ResourceContext the workload receives, built from the committed spec.
type ResourceState struct {
	Scope domain.Scope
	RunID domain.RunID
	Key   string
	Kind  runtimev2.ResourceKind
	Phase ResourcePhase
	// Fingerprint is the committed spec fingerprint ("sha256:...").
	Fingerprint string
	// WorkloadName and WorkloadUID are the committed companion-port name and the
	// bound instance UID. An empty UID is a resource execution has not bound.
	WorkloadName string
	WorkloadUID  string
	// DeadlineAt is the resource's deadline.
	DeadlineAt time.Time
	// Context is the immutable assignment of the workload on the run_shared wire
	// (runtime v2). It is the zero value for a resource of a source-set run, whose
	// assignment is ContextV3.
	Context runtimev2.ResourceContext
	// ContextV3 is the immutable assignment of the workload on the source-set wire
	// (runtime v3). It is non-nil exactly for a resource of a source-set run, which
	// is served only on /runtime/v3 (a v2 route answers context_version_mismatch).
	ContextV3 *runtimev3.ResourceContextV3
}

// Wire is the runtime wire the resource belongs to: WireV3 when it carries a
// ContextV3, WireV2 otherwise.
func (s ResourceState) Wire() int64 {
	if s.ContextV3 != nil {
		return WireV3
	}
	return WireV2
}

// Validate checks the structure and that the context belongs to the resource.
func (s ResourceState) Validate() error {
	bad := fail(ErrUnavailable, "the execution state is malformed")
	ref := ResourceRef{Scope: s.Scope, RunID: s.RunID, Key: s.Key}
	if ref.Validate() != nil {
		return bad
	}
	kind, _ := ResourceKindOf(s.Key)
	switch {
	case s.Kind != kind,
		s.Phase != ResourceActive && s.Phase != ResourceEnded,
		!fingerprintPattern.MatchString(s.Fingerprint),
		!workloadNamePattern.MatchString(s.WorkloadName),
		s.WorkloadUID != "" && !workloadUIDPattern.MatchString(s.WorkloadUID),
		s.DeadlineAt.IsZero():
		return bad
	}
	var deadline time.Time
	var err error
	if c3 := s.ContextV3; c3 != nil {
		if c3.Validate() != nil || c3.ResourceKind != kind || c3.ResourceKey != s.Key || c3.RunID != s.RunID ||
			(domain.Scope{TenantID: c3.TenantID, ProjectID: c3.ProjectID}) != s.Scope {
			return bad
		}
		deadline, err = contextDeadlineV3(*c3)
	} else {
		c := s.Context
		if c.Validate() != nil || c.ResourceKind != kind || c.ResourceKey != s.Key || c.RunID != s.RunID ||
			(domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}) != s.Scope {
			return bad
		}
		deadline, err = contextDeadline(c)
	}
	if err != nil || !micros(deadline).Equal(micros(s.DeadlineAt)) {
		return bad
	}
	return nil
}

func contextDeadlineV3(c runtimev3.ResourceContextV3) (time.Time, error) {
	switch {
	case c.Preparation != nil:
		return c.Preparation.DeadlineAt.Time()
	case c.Service != nil:
		return c.Service.DeadlineAt.Time()
	}
	return time.Time{}, fail(ErrUnavailable, "the context has no deadline")
}

func contextDeadline(c runtimev2.ResourceContext) (time.Time, error) {
	switch {
	case c.Preparation != nil:
		return c.Preparation.DeadlineAt.Time()
	case c.Service != nil:
		return c.Service.DeadlineAt.Time()
	}
	return time.Time{}, fail(ErrUnavailable, "the context has no deadline")
}

// Bound reports whether execution has bound the workload instance.
func (s ResourceState) Bound() bool { return s.WorkloadUID != "" }

// ResourceObserver reads resource state without taking locks. Its answer is
// advisory (used before a workload observation); every mutation re-reads and
// locks through ResourceTx.LockResource. ErrNotFound means no such resource.
type ResourceObserver interface {
	ObserveResource(ctx context.Context, ref ResourceRef) (ResourceState, error)
}

// ResourceWorkload is the committed workload a claimed Pod must belong to.
type ResourceWorkload struct {
	Ref  ResourceRef
	Name string
	UID  string
}

// ResourceWorkloads confirms that a Pod claimed by a resource process belongs to
// the committed workload UID (S6). The Kubernetes adapter exports
// VerifyResourcePod; T-116 bridges it. ErrNotObserved means the Pod or workload
// definitely does not exist or is not controlled by that UID (the claim is
// forbidden); any other error is inconclusive (503). It is called outside any
// SQL transaction.
type ResourceWorkloads interface {
	VerifyPod(ctx context.Context, w ResourceWorkload, podName, podUID string) error
}

// ---- persistence ports ---------------------------------------------------

// ResourceLocator is the ownership of the grant matching a credential digest.
type ResourceLocator struct {
	Ref ResourceRef
}

// ResourceStore is the persistence port of resource grants.
type ResourceStore interface {
	// LocateResource resolves a credential digest to its owner through a
	// restricted, digest-filtered lookup. An unknown digest is
	// ErrInvalidCredential.
	LocateResource(ctx context.Context, digest Digest) (ResourceLocator, error)
	// DoResource runs fn in one transaction scoped to tenant. fn returning nil
	// commits; any error rolls back. fn must not make network calls.
	DoResource(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx ResourceTx) error) error
}

// ResourceTx is the transaction-bound view of resource grants and execution
// state.
//
// Lock order, for every transaction in the system: the resource's execution
// state first (LockResource, which conflicts with the owner's stop and the run's
// cancellation), then grant rows.
type ResourceTx interface {
	// LockResource re-reads the authoritative state and locks it against a
	// concurrent stop or cancellation. ErrNotFound if the resource does not exist
	// in the scope.
	LockResource(ctx context.Context, ref ResourceRef) (ResourceState, error)
	// ResourceGrantByCredential and ResourceGrantBySession return the grant
	// (ErrNotFound when absent or invisible to the tenant) under the lock.
	ResourceGrantByCredential(ctx context.Context, digest Digest, mode LockMode) (ResourceGrant, error)
	ResourceGrantBySession(ctx context.Context, session SessionID, mode LockMode) (ResourceGrant, error)
	// InsertResourceGrant stores a new grant. When a grant for the resource exists
	// it returns that grant and inserted == false; the caller compares.
	InsertResourceGrant(ctx context.Context, g ResourceGrant) (stored ResourceGrant, inserted bool, err error)
	// SaveResourceConsumption persists the first consumption of a locked grant.
	SaveResourceConsumption(ctx context.Context, g ResourceGrant) error
	// SaveResourceRevocation persists the revocation of a locked grant.
	SaveResourceRevocation(ctx context.Context, g ResourceGrant) error
	// RevokeResource revokes the resource's grant and reports how many were newly
	// revoked; RevokeRun revokes every resource grant of the run.
	RevokeResource(ctx context.Context, ref ResourceRef, now time.Time) (int, error)
	RevokeRun(ctx context.Context, scope domain.Scope, run domain.RunID, now time.Time) (int, error)
}

// ---- tokens --------------------------------------------------------------

// ResourceTokenIssuer signs resource tokens (audience kodama-runtime-resource).
// It shares the attempt issuer's signing generation.
type ResourceTokenIssuer interface {
	Generation() int64
	IssueResource(req IssueRequest) (string, error)
}

// ResourceTokenVerifier verifies a bearer of the resource audience. A token that
// is genuine but of another audience is ErrForbidden; everything else invalid is
// ErrInvalidCredential. An unusable verifier is ErrUnavailable.
type ResourceTokenVerifier interface {
	VerifyResource(ctx context.Context, raw string) (TokenClaims, error)
}

// ResourceConfig is the optional resource-subject half of Config. Without it
// every resource operation is unavailable (there is no allow-all fallback).
type ResourceConfig struct {
	Store     ResourceStore
	State     ResourceObserver
	Workloads ResourceWorkloads
	Tokens    ResourceTokenIssuer
	Verifier  ResourceTokenVerifier
	// Wrapper defaults to the attempt policy: the same compatibility handshake
	// applies to a client version.
	Wrapper WrapperPolicy
}
