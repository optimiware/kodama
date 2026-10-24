package stack

import (
	"context"
	"sync"

	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/execution"
)

// EffectSource names the delivery adapter in the reconciliation evidence it
// records.
const EffectSource = deliveryadapters.EffectSource

// DeploymentEffects is the fault-injection decorator around the delivery module's
// own effect reconciler (delivery/adapters.NewEffects, built over the real
// deployment store and the real cleanup bindings of the stack). It records every
// query the control plane makes and can withhold affirmative evidence — the
// record store or observation being unreachable — answering "still unknown"
// instead. Every affirmative verdict is the adapter's: this decorator decides
// nothing.
type DeploymentEffects struct {
	mu       sync.Mutex
	inner    execution.EffectReconciler
	calls    []execution.EffectQuery
	withheld bool
}

var _ execution.EffectReconciler = (*DeploymentEffects)(nil)

// bind replaces the adapter after a restart; the recorded queries and the
// withholding switch belong to the test and survive it.
func (e *DeploymentEffects) bind(inner execution.EffectReconciler) {
	e.mu.Lock()
	e.inner = inner
	e.mu.Unlock()
}

// Withhold makes the decorator answer "still unknown" to every query until
// released, without consulting the adapter.
func (e *DeploymentEffects) Withhold(on bool) {
	e.mu.Lock()
	e.withheld = on
	e.mu.Unlock()
}

// Calls lists the attempts the control plane asked about.
func (e *DeploymentEffects) Calls() []execution.EffectQuery {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]execution.EffectQuery(nil), e.calls...)
}

// Reconcile implements execution.EffectReconciler.
func (e *DeploymentEffects) Reconcile(ctx context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	e.mu.Lock()
	e.calls = append(e.calls, q)
	withheld, inner := e.withheld, e.inner
	e.mu.Unlock()
	if withheld {
		return execution.ReconciliationEvidence{Verdict: execution.VerdictStillUnknown, Source: EffectSource}, nil
	}
	return inner.Reconcile(ctx, q)
}
