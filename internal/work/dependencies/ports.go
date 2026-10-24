package dependencies

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// MaxPageRows bounds one list read (callers ask for page size + 1).
const MaxPageRows = work.MaxPageRows

// ListQuery reads one page of a dependent's prerequisites.
type ListQuery struct {
	Dependent domain.WorkItemID
	After     domain.WorkItemID
	Limit     int
}

// MutationRequest carries everything a link mutation needs explicitly: the
// scope, the verified actor, the expected version of the dependent and the
// permission to re-evaluate inside the transaction after every lock wait.
type MutationRequest struct {
	Scope      domain.Scope
	Dependent  domain.WorkItemID
	Expected   domain.Version
	Actor      work.Actor
	Permission work.Permission
}

// Tx is what a mutation function sees inside the repository's transaction. All
// answers reflect state after the project and dependent locks were acquired.
type Tx interface {
	// PrerequisiteExists returns work.ErrNotFound unless the work item exists
	// in the scope. Foreign-tenant, foreign-project and missing items are the
	// same answer.
	PrerequisiteExists(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error
	// LinkExists reports whether the link is stored.
	LinkExists(ctx context.Context, l Link) (bool, error)
	// PrerequisiteCount counts the direct prerequisites of a work item.
	PrerequisiteCount(ctx context.Context, scope domain.Scope, dependent domain.WorkItemID) (int, error)
	// DependsOn reports whether from depends, directly or transitively, on
	// target through prerequisite links.
	DependsOn(ctx context.Context, scope domain.Scope, from, target domain.WorkItemID) (bool, error)
}

// MutateFunc computes a link change for the locked dependent. It must be free
// of network calls and must not retain tx. It returns work.ErrNoChange when
// nothing needs to be written.
type MutateFunc func(ctx context.Context, tx Tx, owner Owner) (Change, error)

// MutationResult is the owner after the call: the changed owner, or the
// unchanged one for a no-op.
type MutationResult struct {
	Owner   Owner
	Changed bool
}

// Repository is the persistence port of the dependency graph. One Mutate call
// is one transaction: the edge, the dependent's version, history, audit and
// outbox event commit together or not at all. The adapter serializes graph
// changes with work transitions (the project lock) and compares the expected
// version under the dependent's row lock. An unknown commit outcome is reported
// as work.ErrUnavailable and never replayed automatically.
type Repository interface {
	List(ctx context.Context, scope domain.Scope, q ListQuery) (Page, error)
	Mutate(ctx context.Context, req MutationRequest, fn MutateFunc) (MutationResult, error)
}
