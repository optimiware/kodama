package deliverycleanup

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// DeliveryService is the part of the deployment-record module (T-035) the
// scheduler consumes. *delivery.Service implements it.
type DeliveryService interface {
	ListDeployments(ctx context.Context, c delivery.Caller, scope domain.Scope, f delivery.DeploymentFilter, afterID delivery.DeploymentID, limit int) ([]delivery.Deployment, error)
	GetDeployment(ctx context.Context, c delivery.Caller, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
	RequestCleanup(ctx context.Context, c delivery.Caller, scope domain.Scope, id delivery.DeploymentID) (delivery.Receipt, error)
}

var _ DeliveryService = (*delivery.Service)(nil)

// systemCaller is the scheduler's own attribution. The delivery service treats a
// system actor as trusted, so only a composition root builds this adapter.
var systemCaller = delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorSystem, ID: "deployment-cleanup-scheduler"}, RequestID: "deployment-cleanup"}

// DeliveryRecords adapts the delivery service to Records.
type DeliveryRecords struct{ svc DeliveryService }

var _ Records = (*DeliveryRecords)(nil)

// NewRecords requires the delivery service.
func NewRecords(svc DeliveryService) (*DeliveryRecords, error) {
	if svc == nil {
		return nil, errors.New("deliverycleanup: a delivery service is required")
	}
	return &DeliveryRecords{svc: svc}, nil
}

func candidate(d delivery.Deployment) Candidate {
	r := d.Record()
	return Candidate{Scope: r.Scope, Deployment: string(r.ID), ActionKey: r.ActionKey, ExpiresAt: r.ExpiresAt,
		RequestedAt: r.CleanupRequestedAt, State: RecordState(r.CleanupState), Deleted: r.DeletedAt != nil}
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrDenied):
		return ErrNotFound
	case errors.Is(err, delivery.ErrInvalidTransition):
		return ErrNotSettled
	}
	return err
}

// Scan implements Records by paging the scope's deployments in identity order.
// Deleted records are not cleanup work and are dropped; the cursor still
// advances past them.
func (r *DeliveryRecords) Scan(ctx context.Context, scope domain.Scope, after string, limit int) ([]Candidate, string, error) {
	ds, err := r.svc.ListDeployments(ctx, systemCaller, scope, delivery.DeploymentFilter{}, delivery.DeploymentID(after), limit)
	if err != nil {
		return nil, "", err
	}
	var out []Candidate
	for _, d := range ds {
		if c := candidate(d); !c.Deleted {
			out = append(out, c)
		}
	}
	next := ""
	if len(ds) == limit {
		next = string(ds[len(ds)-1].ID())
	}
	return out, next, nil
}

// Get implements Records.
func (r *DeliveryRecords) Get(ctx context.Context, scope domain.Scope, deployment string) (Candidate, error) {
	d, err := r.svc.GetDeployment(ctx, systemCaller, scope, delivery.DeploymentID(deployment))
	if err != nil {
		return Candidate{}, mapErr(err)
	}
	return candidate(d), nil
}

// RequestCleanup implements Records.
func (r *DeliveryRecords) RequestCleanup(ctx context.Context, scope domain.Scope, deployment string) error {
	_, err := r.svc.RequestCleanup(ctx, systemCaller, scope, delivery.DeploymentID(deployment))
	return mapErr(err)
}

// RunBindings is the execution-side reader of a cleanup run's deployment.
// *execution.CleanupBindings implements it.
type RunBindings interface {
	Binding(ctx context.Context, scope domain.Scope, run domain.RunID) (string, bool, error)
}

var _ RunBindings = (*execution.CleanupBindings)(nil)

// DeliveryBindings implements delivery.CleanupBindings over a run reader: the
// binding of a cleanup run is the deployment execution persisted with it.
type DeliveryBindings struct{ runs RunBindings }

var _ delivery.CleanupBindings = (*DeliveryBindings)(nil)

// NewBindings requires the run reader.
func NewBindings(runs RunBindings) (*DeliveryBindings, error) {
	if runs == nil {
		return nil, errors.New("deliverycleanup: a cleanup run reader is required")
	}
	return &DeliveryBindings{runs: runs}, nil
}

// Binding implements delivery.CleanupBindings.
func (b *DeliveryBindings) Binding(ctx context.Context, scope domain.Scope, run domain.RunID) (delivery.DeploymentID, bool, error) {
	id, found, err := b.runs.Binding(ctx, scope, run)
	if err != nil || !found {
		return "", false, err
	}
	return delivery.DeploymentID(id), true, nil
}
