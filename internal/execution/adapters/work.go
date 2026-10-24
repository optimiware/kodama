package adapters

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// WorkContent adapts the work module's repository: the item's current content
// for pinning. A missing or foreign item is not found.
type WorkContent struct{ repo work.Repository }

var _ execution.WorkContent = WorkContent{}

// NewWorkContent requires the repository.
func NewWorkContent(r work.Repository) (WorkContent, error) {
	if r == nil {
		return WorkContent{}, errors.New("execution/adapters: a work repository is required")
	}
	return WorkContent{repo: r}, nil
}

// Snapshot implements execution.WorkContent.
func (w WorkContent) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	it, err := w.repo.Get(ctx, scope, id)
	switch {
	case err == nil:
	case errors.Is(err, work.ErrNotFound), errors.Is(err, work.ErrHidden):
		return execution.WorkSnapshot{}, execution.ErrNotFound
	default:
		return execution.WorkSnapshot{}, execution.Unavailablef(err, "the work repository is unavailable")
	}
	crit := make([]runtimev1.AcceptanceCriterion, 0, len(it.Criteria))
	for _, c := range it.Criteria {
		crit = append(crit, runtimev1.AcceptanceCriterion{ID: c.ID, Text: c.Text})
	}
	return execution.WorkSnapshot{
		ID: it.ID, Actionable: it.Kind.Actionable(), Cancelled: it.State == work.StateCancelled,
		ContentRevision: it.ContentRevision, Spec: it.Spec, Criteria: crit,
	}, nil
}

// CancellationRequester implements work.CancellationRequester over the
// execution use case: the periodic cancelled-item scan (work.ReconcileCancellations)
// re-requests cancellation idempotently.
type CancellationRequester struct{ svc *execution.Service }

var _ work.CancellationRequester = CancellationRequester{}

// NewCancellationRequester requires the service.
func NewCancellationRequester(s *execution.Service) (CancellationRequester, error) {
	if s == nil {
		return CancellationRequester{}, errors.New("execution/adapters: a service is required")
	}
	return CancellationRequester{svc: s}, nil
}

// RequestCancellation implements work.CancellationRequester.
func (c CancellationRequester) RequestCancellation(ctx context.Context, req work.CancelRequest) error {
	return c.svc.RequestCancellation(ctx, execution.WorkCancellation{Scope: req.Scope, WorkItemID: req.WorkItemID, Version: req.Version})
}

// TxCancellationRequester returns the transaction-bound requester that
// workpg.CancellationBinding consumes: it records the item-scoped intent and
// cancels the item's runs inside the consumer's inbox transaction.
func TxCancellationRequester(s *execpg.Store) workpg.TxCancellationRequester {
	return func(ctx context.Context, tx pgx.Tx, req work.CancelRequest) error {
		return s.RequestCancellationInTx(ctx, tx, execution.WorkCancellation{Scope: req.Scope, WorkItemID: req.WorkItemID, Version: req.Version})
	}
}

// CancelledWork adapts the work module's cancelled-item scan.
type CancelledWork struct{ src work.CancellationSource }

var _ execution.CancelledWork = CancelledWork{}

// NewCancelledWork requires the source.
func NewCancelledWork(s work.CancellationSource) (CancelledWork, error) {
	if s == nil {
		return CancelledWork{}, errors.New("execution/adapters: a cancellation source is required")
	}
	return CancelledWork{src: s}, nil
}

// CancelledItems implements execution.CancelledWork.
func (c CancelledWork) CancelledItems(ctx context.Context, tenant domain.TenantID, after execution.CancelCursor, limit int) ([]execution.WorkCancellation, error) {
	items, err := c.src.CancelledItems(ctx, tenant, work.CancelledCursor{ProjectID: after.ProjectID, ID: after.ID}, limit)
	if err != nil {
		return nil, execution.Unavailablef(err, "the cancelled-item scan is unavailable")
	}
	out := make([]execution.WorkCancellation, 0, len(items))
	for _, it := range items {
		out = append(out, execution.WorkCancellation{Scope: it.Scope, WorkItemID: it.WorkItemID, Version: it.Version})
	}
	return out, nil
}

// BindWriterGuard completes a work guard binder with the execution-backed
// writer guard: the transaction-bound check that no writer attempt of the item
// is unsettled. The remaining guards come from next.
func BindWriterGuard(next func(tx pgx.Tx) work.TxGuards) func(tx pgx.Tx) work.TxGuards {
	return func(tx pgx.Tx) work.TxGuards {
		g := work.TxGuards{}
		if next != nil {
			g = next(tx)
		}
		g.Writers = execpg.NewWriterGuard(tx)
		return g
	}
}
