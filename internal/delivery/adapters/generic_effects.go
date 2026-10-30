package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// GenericEffectSource names the ADR-0012 generic deployment reconciler in
// execution reconciliation evidence.
const GenericEffectSource = "generic-delivery-record"

// GenericDeploymentRecords reads tool-neutral deployment records by scope and action key.
type GenericDeploymentRecords interface {
	FindGenericByAction(ctx context.Context, scope domain.Scope, actionKey string) (delivery.GenericDeployment, error)
}

// GenericEffects implements execution.EffectReconciler for ADR-0012 generic
// delivery steps (DQ-21). Unlike the legacy ToolQualification-gated adapter:
//   - An absent deployment record is NEVER treated as proof that an arbitrary
//     command had no external effect (it returns VerdictStillUnknown).
//   - A pending or running deployment with unknown outcome stays VerdictStillUnknown.
//   - Only a deployment that reached a terminal outcome or was explicitly
//     settled by an authorized observation returns VerdictSettled (ReplaySafe=false),
//     allowing the run to settle without automatically re-running the command.
type GenericEffects struct {
	records GenericDeploymentRecords
}

var _ execution.EffectReconciler = (*GenericEffects)(nil)

// NewGenericEffects constructs a tool-neutral effect reconciler.
func NewGenericEffects(records GenericDeploymentRecords) (*GenericEffects, error) {
	if records == nil {
		return nil, errors.New("delivery/adapters: generic deployment records are required")
	}
	return &GenericEffects{records: records}, nil
}

// Reconcile implements execution.EffectReconciler.
func (e *GenericEffects) Reconcile(ctx context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	if err := q.Scope.Validate(); err != nil || q.RunID.Validate() != nil || q.ActionKey == "" {
		return execution.ReconciliationEvidence{}, execution.Invalidf("an effect query needs a valid scope, run and action key")
	}
	d, err := e.records.FindGenericByAction(ctx, q.Scope, q.ActionKey)
	switch {
	case errors.Is(err, delivery.ErrNotFound):
		// Missing receipt never proves an arbitrary container command made no
		// external change: stay unknown!
		return genericVerdict(execution.VerdictStillUnknown), nil
	case err != nil:
		if errors.Is(err, delivery.ErrInvalid) {
			return genericVerdict(execution.VerdictStillUnknown), nil
		}
		return execution.ReconciliationEvidence{}, execution.Unavailablef(err, "the generic deployment record is unavailable")
	}
	intent := d.Intent()
	if intent.Scope != q.Scope || intent.ActionKey != q.ActionKey || intent.RunID != q.RunID || intent.StepID != q.StepID {
		return genericVerdict(execution.VerdictStillUnknown), nil
	}
	if d.Status().Terminal() {
		// Settled (never replay_safe: tool-neutral commands are not automatically re-run).
		return genericVerdict(execution.VerdictSettled), nil
	}
	return genericVerdict(execution.VerdictStillUnknown), nil
}

func genericVerdict(v execution.Verdict) execution.ReconciliationEvidence {
	return execution.ReconciliationEvidence{Verdict: v, Source: GenericEffectSource}
}
