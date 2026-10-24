package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

// Planner resolves everything run admission pins from a verified push, outside
// any transaction, into the plan the ledger admits. A permanent refusal wraps
// gitwebhook.ErrRunRefused; any other error is an outage.
type Planner interface {
	Plan(ctx context.Context, req gw.RunRequest) (*execution.AdmissionPlan, error)
}

// Runs implements gitwebhook.Runs over the core's run admission and one
// admission Ledger.
//
//   - StartRun is idempotent by key. A key already decided answers from the
//     ledger without planning again: its run (Created=false) or
//     ErrAdmissionClosed after a refusal, so a replay never re-evaluates
//     authority and a stalled holder can never create a run after a refusal.
//     An undecided key is planned outside the transaction and admitted by the
//     ledger, which re-checks the key under its lock.
//   - FindRun reads the decision without any authority check.
//   - DecideRefusal makes the refusal durable unless a run exists, in the same
//     ledger, so the two outcomes exclude each other.
type Runs struct {
	planner Planner
	ledger  Ledger
}

var _ gw.Runs = (*Runs)(nil)

// NewRuns requires both collaborators.
func NewRuns(planner Planner, ledger Ledger) (*Runs, error) {
	if planner == nil || ledger == nil {
		return nil, errors.New("gitwebhook/adapters: a planner and a ledger are required")
	}
	return &Runs{planner: planner, ledger: ledger}, nil
}

func outage(op string, err error) error {
	if errors.Is(err, gw.ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", gw.ErrUnavailable, op, err)
}

func validateKey(scope domain.Scope, key string) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	if key == "" || len(key) > 512 {
		return fmt.Errorf("%w: the idempotency key must be 1..512 bytes", gw.ErrInvalid)
	}
	return nil
}

// StartRun implements gitwebhook.Runs.
func (r *Runs) StartRun(ctx context.Context, req gw.RunRequest) (gw.RunRef, error) {
	if err := validateKey(req.Scope, req.IdempotencyKey); err != nil {
		return gw.RunRef{}, err
	}
	entry, found, err := r.ledger.Lookup(ctx, req.Scope, req.IdempotencyKey)
	if err != nil {
		return gw.RunRef{}, outage("admission lookup", err)
	}
	if found {
		if entry.RunID == "" {
			return gw.RunRef{}, gw.ErrAdmissionClosed
		}
		return gw.RunRef{ID: entry.RunID}, nil
	}
	plan, err := r.planner.Plan(ctx, req)
	if err != nil {
		if errors.Is(err, gw.ErrRunRefused) {
			return gw.RunRef{}, err
		}
		return gw.RunRef{}, outage("admission plan", err)
	}
	adm, err := r.ledger.Admit(ctx, req.Scope, req.IdempotencyKey, plan)
	switch {
	case err == nil:
		return gw.RunRef{ID: adm.RunID, Created: adm.Created}, nil
	case errors.Is(err, gw.ErrAdmissionClosed):
		return gw.RunRef{}, gw.ErrAdmissionClosed
	}
	return gw.RunRef{}, ClassifyAdmission(err)
}

// FindRun implements gitwebhook.Runs.
func (r *Runs) FindRun(ctx context.Context, scope domain.Scope, key string) (gw.RunRef, bool, error) {
	if err := validateKey(scope, key); err != nil {
		return gw.RunRef{}, false, err
	}
	entry, found, err := r.ledger.Lookup(ctx, scope, key)
	if err != nil {
		return gw.RunRef{}, false, outage("admission lookup", err)
	}
	if !found || entry.RunID == "" {
		return gw.RunRef{}, false, nil
	}
	return gw.RunRef{ID: entry.RunID}, true, nil
}

// DecideRefusal implements gitwebhook.Runs.
func (r *Runs) DecideRefusal(ctx context.Context, scope domain.Scope, key, reason string) (gw.Decision, error) {
	if err := validateKey(scope, key); err != nil {
		return gw.Decision{}, err
	}
	if reason == "" || len(reason) > 128 {
		return gw.Decision{}, fmt.Errorf("%w: a refusal reason must be 1..128 bytes", gw.ErrInvalid)
	}
	entry, err := r.ledger.Decide(ctx, scope, key, reason)
	if err != nil {
		return gw.Decision{}, outage("admission decision", err)
	}
	if entry.RunID != "" {
		return gw.Decision{Found: true, Run: gw.RunRef{ID: entry.RunID}}, nil
	}
	return gw.Decision{Reason: entry.Refusal}, nil
}

// ClassifyAdmission maps an error of run admission to the trigger's classes: a
// definition, configuration, source, authority or work-item refusal is a
// permanent gitwebhook.ErrRunRefused; a conflict, quota, busy, not-configured or
// infrastructure failure is an outage that leaves the delivery retryable.
func ClassifyAdmission(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gw.ErrRunRefused) || errors.Is(err, gw.ErrUnavailable) {
		return err
	}
	for _, permanent := range []error{
		execution.ErrAdmissionRefused, execution.ErrDenied, execution.ErrHidden, execution.ErrNotActionable,
		execution.ErrWorkCancelled, execution.ErrPurposeDenied, execution.ErrInvalid, execution.ErrNotFound,
	} {
		if errors.Is(err, permanent) {
			// Keep the content-free cause for diagnostics, without wrapping it
			// into the permanent class twice.
			return fmt.Errorf("%w: %w", gw.ErrRunRefused, err)
		}
	}
	return fmt.Errorf("%w: run admission: %w", gw.ErrUnavailable, err)
}
