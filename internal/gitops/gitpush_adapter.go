package gitops

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
)

type pushesOf struct{ s *gitpush.Service }

// PushesOf adapts the push service (gitpush.Service) to the Pushes port.
func PushesOf(s *gitpush.Service) Pushes { return pushesOf{s: s} }

func (p pushesOf) Reconcile(ctx context.Context, tenant domain.TenantID, repositoryID string) error {
	return p.s.Reconcile(ctx, tenant, repositoryID)
}

func (p pushesOf) Settlement(ctx context.Context, tenant domain.TenantID, repositoryID string) (PushSettlement, error) {
	st, err := p.s.Settlement(ctx, tenant, repositoryID)
	return PushSettlement{InFlight: st.InFlight, Unknown: st.Unknown, KnownHead: st.KnownHead}, err
}

func (p pushesOf) AcknowledgeRestore(ctx context.Context, scope domain.Scope, repositoryID string, head int64, by, reason string) (bool, error) {
	return p.s.AcknowledgeRestore(ctx, scope, repositoryID, head, by, reason)
}
