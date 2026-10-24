package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// EffectSource names this adapter in reconciliation evidence. It identifies the
// deployment record as the origin of the verdict; it is never workload text.
const EffectSource = "delivery-record"

// DeploymentRecords is the scoped deployment-record read the reconciler needs.
// delivery.Store implements it; every method treats a record of another tenant
// or project as delivery.ErrNotFound.
type DeploymentRecords interface {
	FindByAction(ctx context.Context, scope domain.Scope, actionKey string) (delivery.Deployment, error)
	GetDeployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
}

// Effects implements execution.EffectReconciler for deployment steps. T-065's
// apply script registers the intent before its first change, replays a settled
// action without effects, observes the recorded objects before acting on an
// unknown outcome and resumes an applying deployment idempotently by
// annotation. So a replay is safe whenever the record shows the script can
// observe what a lost attempt may have done, and unknown whenever it cannot.
//
// The verdict is derived only from the scoped deployment record, never from the
// attempt's own output, and a record that cannot be read never produces an
// affirmative verdict.
type Effects struct {
	records  DeploymentRecords
	bindings delivery.CleanupBindings
}

var _ execution.EffectReconciler = (*Effects)(nil)

// NewEffects requires the deployment records and the cleanup bindings: without
// the bindings a cleanup run could not be told from an apply and would be
// judged by the wrong record, so a missing one is a configuration error rather
// than a default.
func NewEffects(records DeploymentRecords, bindings delivery.CleanupBindings) (*Effects, error) {
	if records == nil {
		return nil, errors.New("delivery/adapters: deployment records are required")
	}
	if bindings == nil {
		return nil, errors.New("delivery/adapters: cleanup bindings are required")
	}
	return &Effects{records: records, bindings: bindings}, nil
}

// Reconcile implements execution.EffectReconciler.
//
// Apply (the run is not a bound cleanup run), looked up by action key within
// the query's scope:
//
//	no record                           replay_safe  (nothing was applied)
//	pending (registered, not started)   replay_safe
//	applying, no objects                still_unknown (nothing to observe)
//	applying, with objects              replay_safe  (the script observes first)
//	unknown outcome (any objects)       replay_safe  (the script never applies again)
//	succeeded or failed                 replay_safe  (a settled action replays)
//
// Cleanup (the run is bound to a deployment): replay_safe once cleanup was
// requested on that deployment, still_unknown otherwise. An unreadable record
// is still_unknown and an outage is an error; neither is affirmative.
func (e *Effects) Reconcile(ctx context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	if err := q.Scope.Validate(); err != nil {
		return execution.ReconciliationEvidence{}, execution.Invalidf("an effect query needs a valid scope")
	}
	if q.RunID.Validate() != nil || q.ActionKey == "" {
		return execution.ReconciliationEvidence{}, execution.Invalidf("an effect query names its run and action key")
	}
	bound, cleanup, err := e.bindings.Binding(ctx, q.Scope, q.RunID)
	if err != nil {
		return execution.ReconciliationEvidence{}, execution.Unavailablef(err, "the cleanup binding is unavailable")
	}
	if cleanup {
		return e.cleanup(ctx, q, bound)
	}
	return e.apply(ctx, q)
}

func (e *Effects) apply(ctx context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	d, err := e.records.FindByAction(ctx, q.Scope, q.ActionKey)
	switch {
	case errors.Is(err, delivery.ErrNotFound):
		return evidence(execution.VerdictReplaySafe), nil
	case err != nil:
		return unreadable(err)
	}
	rec := d.Record()
	if rec.Scope != q.Scope || rec.ActionKey != q.ActionKey || rec.Run != q.RunID || rec.Step != q.StepID {
		// A record that is not this step's own proves nothing about it.
		return evidence(execution.VerdictStillUnknown), nil
	}
	switch rec.Status {
	case delivery.StatusPending:
		return evidence(execution.VerdictReplaySafe), nil
	case delivery.StatusRunning:
		// An unknown outcome is settled by apply.sh without applying again, even
		// over an empty object list, so it is replay safe. An applying record
		// with no recorded identities cannot be observed: still unknown.
		if rec.Reason != delivery.ReasonOutcomeUnknown && len(rec.Objects) == 0 {
			return evidence(execution.VerdictStillUnknown), nil
		}
		return evidence(execution.VerdictReplaySafe), nil
	case delivery.StatusSucceeded, delivery.StatusFailed:
		return evidence(execution.VerdictReplaySafe), nil
	}
	return evidence(execution.VerdictStillUnknown), nil
}

func (e *Effects) cleanup(ctx context.Context, q execution.EffectQuery, id delivery.DeploymentID) (execution.ReconciliationEvidence, error) {
	d, err := e.records.GetDeployment(ctx, q.Scope, id)
	switch {
	case errors.Is(err, delivery.ErrNotFound):
		// A binding without a readable deployment of this scope is no proof.
		return evidence(execution.VerdictStillUnknown), nil
	case err != nil:
		return unreadable(err)
	}
	rec := d.Record()
	if rec.Scope != q.Scope || rec.ID != id || rec.CleanupRequestedAt == nil {
		return evidence(execution.VerdictStillUnknown), nil
	}
	return evidence(execution.VerdictReplaySafe), nil
}

func evidence(v execution.Verdict) execution.ReconciliationEvidence {
	return execution.ReconciliationEvidence{Verdict: v, Source: EffectSource}
}

// unreadable maps a failed read: a record that cannot be interpreted is
// inconclusive, anything else (an outage, a lost answer, an unclassified
// failure) is an error. Neither reaches an affirmative verdict.
func unreadable(err error) (execution.ReconciliationEvidence, error) {
	if errors.Is(err, delivery.ErrInvalid) {
		return evidence(execution.VerdictStillUnknown), nil
	}
	return execution.ReconciliationEvidence{}, execution.Unavailablef(err, "the deployment record is unavailable")
}
