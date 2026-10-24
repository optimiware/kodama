package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Reconciliation is one module-owned repair scan over durable desired state
// (for example "runs whose last observed event is stale"). It must be bounded,
// idempotent and independent of broker messages. T-005 does not implement the
// run or workspace reconcilers; their modules register them.
type Reconciliation struct {
	Name string
	Run  func(ctx context.Context, tenant domain.TenantID) error
}

// Reconciler runs registered reconciliations for every tenant at startup and
// periodically, so missed, duplicated or reordered wakeups cannot stall
// progress. Optional wakeups only bring the next pass forward and coalesce.
type Reconciler struct {
	tenants  TenantSource
	tasks    []Reconciliation
	interval time.Duration
	logger   *slog.Logger
	wake     chan struct{}
}

// NewReconciler returns a reconciler. interval must be positive.
func NewReconciler(tenants TenantSource, interval time.Duration, logger *slog.Logger, tasks ...Reconciliation) (*Reconciler, error) {
	if tenants == nil || interval <= 0 {
		return nil, fmt.Errorf("%w: reconciler needs tenants and a positive interval", ErrInvalidConfiguration)
	}
	for _, t := range tasks {
		if t.Name == "" || t.Run == nil {
			return nil, fmt.Errorf("%w: reconciliation needs a name and a function", ErrInvalidConfiguration)
		}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{tenants: tenants, tasks: tasks, interval: interval, logger: logger, wake: make(chan struct{}, 1)}, nil
}

// Wake requests a prompt pass; concurrent wakeups coalesce into one.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// RunOnce runs every reconciliation for every tenant. Failures are collected so
// one broken scan or tenant does not starve the rest.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	ids, err := r.tenants.TenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("events: list tenants: %w", err)
	}
	var errs []error
	for _, tenant := range ids {
		for _, task := range r.tasks {
			if ctx.Err() != nil {
				return errors.Join(append(errs, ctx.Err())...)
			}
			if err := task.Run(ctx, tenant); err != nil {
				errs = append(errs, fmt.Errorf("reconcile %s for tenant %s: %w", task.Name, tenant, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Run scans at startup, then every interval or wakeup, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) error {
	return runLoop(ctx, r.interval, r.wake, func(ctx context.Context) {
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("reconciliation pass failed", "error", err)
		}
	})
}
