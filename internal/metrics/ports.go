package metrics

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are owned by this module. Adapters implement them and the
// composition root wires them; nothing here has an allow-all default.

// ReadAuthorizer decides whether the principal may read work items in the scope.
// It returns nil to allow, ErrDenied or ErrHidden to refuse, and any other error
// for an outage (treated as unavailable, never as access).
type ReadAuthorizer interface {
	AuthorizeRead(ctx context.Context, scope domain.Scope, principalID string) error
}

// SummaryReader derives summaries for a bounded set of items of one project.
// Items that do not exist in the scope, including items of other tenants or
// projects, are omitted without distinction. A failure returns no summaries.
type SummaryReader interface {
	ReadSummaries(ctx context.Context, scope domain.Scope, ids []domain.WorkItemID, w Window) ([]WorkSummary, error)
}
