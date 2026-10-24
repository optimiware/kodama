package agents

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. The composition root
// wires adapters; a nil or missing port fails closed, and nothing here has an
// allow-all default.

// Authority resolves a verified principal to an attributable actor in a scope
// and answers whether the actor currently holds a permission. Authorize returns
// nil to allow, ErrDenied or ErrHidden to refuse, and any other error for an
// outage (treated as unavailable, never as access).
type Authority interface {
	ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (Actor, error)
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// Tuple is the exact combination the compatibility evidence covers.
type Tuple struct {
	Image          string
	WrapperID      string
	WrapperVersion string
	Command        []string
	HarnessID      string
	HarnessVersion string
}

// Qualification is the evidence that a tuple was integration-tested and which
// controls the installation enforces for it.
type Qualification struct {
	// Evidence names the smoke/compatibility result that qualified the tuple.
	Evidence string
	Controls []Control
}

// Supports reports whether the qualification covers the control.
func (q Qualification) Supports(c Control) bool {
	for _, k := range q.Controls {
		if k == c {
			return true
		}
	}
	return false
}

// Compatibility answers whether a wrapper/harness/image tuple is a tested,
// supported combination (AR-02). It returns ErrUnsupported for an unknown or
// unqualified tuple and any other error for an outage. The catalogue is
// injected: an empty one qualifies nothing, and test fixtures never establish
// production support.
type Compatibility interface {
	Qualify(ctx context.Context, t Tuple) (Qualification, error)
}

// Approvals is the approved-reference policy: whether the scope may use the
// model configuration and the tools. Both return nil to allow, ErrUnsupported
// is not used here; a refusal is ErrDenied, and anything else is an outage.
// Approval never grants authority; it only narrows what a profile may name.
type Approvals interface {
	ApproveModel(ctx context.Context, scope domain.Scope, m Model) error
	ApproveTools(ctx context.Context, scope domain.Scope, tools []string) error
}

// Position is the continuation key of a list: the creation time and ID of the
// last returned row.
type Position struct {
	CreatedAt time.Time
	ID        ProfileID
}

// ListQuery filters and pages a list. Ordering is always (created_at, id).
type ListQuery struct {
	// Name optionally restricts the list to one profile name.
	Name  string
	After *Position
	// Limit is the number of rows to read (callers ask for page size + 1).
	Limit int
}

// Repository is the persistence port. RegisterOrGet is one atomic operation:
// the version and its audit record commit together or not at all. Identical
// content (same scoped digest) returns the existing version with created=false
// and writes nothing. An unknown commit outcome is reported as ErrUnavailable
// wrapping ErrOutcomeUnknown and is never retried internally.
type Repository interface {
	RegisterOrGet(ctx context.Context, c Candidate) (p ProfileVersion, created bool, err error)
	// Get reads one exact version of a name in the scope.
	Get(ctx context.Context, scope domain.Scope, name string, version domain.Version) (ProfileVersion, error)
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]ProfileVersion, error)
}
