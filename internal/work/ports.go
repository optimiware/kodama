package work

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. Persistence adapters and
// the identity, workspace, evidence and execution modules implement them; the
// composition root wires them. A missing implementation fails closed: nothing
// here has an allow-all default.

// ActorResolver resolves a verified principal to an attributable actor and its
// type in a scope. The type comes from the identity authority, never from the
// principal's spelling or from request JSON.
type ActorResolver interface {
	ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (Actor, error)
}

// PermissionChecker decides whether the actor currently holds a permission in
// the scope. It returns nil to allow, ErrDenied or ErrHidden to refuse, and any
// other error for an outage (treated as unavailable, never as access).
type PermissionChecker interface {
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// Authority combines both identity questions. It is called before persistence;
// the same PermissionChecker is bound to the mutating transaction (TxGuards) so
// revoked access cannot complete work after a lock wait.
type Authority interface {
	ActorResolver
	PermissionChecker
}

// WriterGuard reports whether any writer attempt on the item's workspace is
// unsettled. T-013/T-020 provide the transaction-bound implementation.
type WriterGuard interface {
	WriterSettled(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (bool, error)
}

// DependencyGuard evaluates M3 dependency edges at admission. An M1
// installation selects NoDependencyGraph explicitly.
type DependencyGuard interface {
	DependenciesSatisfied(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error
}

// SubjectGuard verifies review evidence against current durable state: that the
// commit is a retained candidate of the item's workspace and that artifacts
// exist in the same tenant and project. It returns ErrStaleSubject when a
// previously recorded subject is no longer current, and ErrNotFound for a
// reference that does not exist in scope.
type SubjectGuard interface {
	VerifySubject(ctx context.Context, scope domain.Scope, id domain.WorkItemID, claim ReviewClaim) (VerifiedSubject, error)
	// VerifyEvidence checks evidence links set on an item.
	VerifyEvidence(ctx context.Context, scope domain.Scope, refs []EvidenceRef) error
}

// TxGuards are the transaction-bound guards for one mutation. Any nil member
// fails closed with ErrUnavailable.
type TxGuards struct {
	Permissions  PermissionChecker
	Writers      WriterGuard
	Dependencies DependencyGuard
	Subjects     SubjectGuard
}

func guardMissing(name string) error {
	return Unavailablef(nil, "%s guard is not configured", name)
}

// Authorize implements the permission part of TransitionEnv.
func (g TxGuards) Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error {
	if g.Permissions == nil {
		return guardMissing("permission")
	}
	return normalizeGuardError(g.Permissions.Authorize(ctx, actor, scope, perm), "permission")
}

// WriterSettled implements the writer part of TransitionEnv.
func (g TxGuards) WriterSettled(ctx context.Context, item Item) (bool, error) {
	if g.Writers == nil {
		return false, guardMissing("writer")
	}
	ok, err := g.Writers.WriterSettled(ctx, item.Scope, item.ID)
	if err != nil {
		return false, normalizeGuardError(err, "writer")
	}
	return ok, nil
}

// DependenciesSatisfied implements the dependency part of TransitionEnv.
func (g TxGuards) DependenciesSatisfied(ctx context.Context, item Item) error {
	if g.Dependencies == nil {
		return guardMissing("dependency")
	}
	return normalizeGuardError(g.Dependencies.DependenciesSatisfied(ctx, item.Scope, item.ID), "dependency")
}

// VerifySubject implements the subject part of TransitionEnv.
func (g TxGuards) VerifySubject(ctx context.Context, item Item, claim ReviewClaim) (VerifiedSubject, error) {
	if g.Subjects == nil {
		return VerifiedSubject{}, guardMissing("review subject")
	}
	v, err := g.Subjects.VerifySubject(ctx, item.Scope, item.ID, claim)
	if err != nil {
		return VerifiedSubject{}, normalizeGuardError(err, "review subject")
	}
	return v, nil
}

// VerifyEvidence checks evidence links.
func (g TxGuards) VerifyEvidence(ctx context.Context, scope domain.Scope, refs []EvidenceRef) error {
	if g.Subjects == nil {
		return guardMissing("evidence")
	}
	return normalizeGuardError(g.Subjects.VerifyEvidence(ctx, scope, refs), "evidence")
}

// normalizeGuardError keeps the module's classified failures and converts any
// other guard error into ErrUnavailable: an unknown guard failure is never a
// pass and never a 500.
func normalizeGuardError(err error, name string) error {
	if err == nil || IsBusinessError(err) {
		return err
	}
	return Unavailablef(err, "%s guard failed", name)
}

// NoDependencyGraph is the explicit M1 dependency checker: there is no
// dependency graph, so nothing blocks admission. Selecting it is a deliberate
// wiring decision; it is never a default.
type NoDependencyGraph struct{}

// DependenciesSatisfied implements DependencyGuard.
func (NoDependencyGraph) DependenciesSatisfied(context.Context, domain.Scope, domain.WorkItemID) error {
	return nil
}

// WIPSnapshot is the project's active admission state under the project lock.
type WIPSnapshot struct {
	Limit  int
	Active int
}

// Tx is what an update function sees inside the repository's transaction: the
// transition environment plus the reference checks of edits. Every method
// evaluates current state inside the transaction.
type Tx interface {
	TransitionEnv
	// AssigneeEligible checks the user holds a current membership in scope.
	AssigneeEligible(ctx context.Context, scope domain.Scope, userID string) error
	// ParentEligible checks the parent is an epic of the same project and
	// that making it the parent of id creates no cycle. It requires the
	// project lock.
	ParentEligible(ctx context.Context, scope domain.Scope, id, parent domain.WorkItemID) error
	// VerifyEvidence checks evidence links.
	VerifyEvidence(ctx context.Context, scope domain.Scope, refs []EvidenceRef) error
}

// Position is the continuation key of a work item list: the creation time and
// ID of the last returned row.
type Position struct {
	CreatedAt time.Time
	ID        domain.WorkItemID
}

// ListQuery filters and pages a list. Filters are conjunctive; repeated states
// or kinds are disjunctive within the field. Ordering is always
// (created_at, id).
type ListQuery struct {
	States     []State
	Kinds      []Kind
	AssigneeID string
	ParentID   domain.WorkItemID
	Blocked    *bool
	After      *Position
	// Limit is the number of rows to read (callers ask for page size + 1).
	Limit int
}

// HistoryQuery pages an item's history by version.
type HistoryQuery struct {
	AfterVersion domain.Version
	Limit        int
}

// UpdateFunc computes a mutation of the locked current item. It must be free of
// network calls and must not retain tx. It returns ErrNoChange when nothing
// needs to be written.
type UpdateFunc func(ctx context.Context, tx Tx, current Item) (Item, HistoryEntry, error)

// InsertFunc computes a new item inside the repository's transaction.
type InsertFunc func(ctx context.Context, tx Tx) (Item, HistoryEntry, error)

// UpdateOptions control how Update serializes.
type UpdateOptions struct {
	// LockProject takes the project row lock before the item lock. Required
	// for transitions (active WIP admission) and parent changes (cycle checks).
	LockProject bool
}

// Repository is the persistence port of the work module. One method call is one
// transaction: state, history, audit and outbox event commit together or not at
// all. Version comparison is part of the write. An unknown commit outcome is
// reported as ErrUnavailable and never replayed automatically.
type Repository interface {
	Insert(ctx context.Context, scope domain.Scope, fn InsertFunc) (Item, error)
	Update(ctx context.Context, scope domain.Scope, id domain.WorkItemID, expected domain.Version, opts UpdateOptions, fn UpdateFunc) (Item, error)
	Get(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (Item, error)
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]Item, error)
	History(ctx context.Context, scope domain.Scope, id domain.WorkItemID, q HistoryQuery) ([]HistoryEntry, error)
}

// SubjectRefresher synchronizes external state (for example a human Git push)
// before a review transition. It runs outside any SQL transaction; the
// transaction then verifies the resulting durable subject. Optional.
type SubjectRefresher interface {
	RefreshSubject(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error
}
