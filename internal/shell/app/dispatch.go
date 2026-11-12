package app

import (
	"context"
	"sync/atomic"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
)

// dispatchGate is the composition's installation.DispatchGate. It gates only
// new dispatch (Kernel reconciliation and push selection); publication of
// already committed intent is not dispatch and continues to drain.
type dispatchGate struct {
	enabled atomic.Bool
}

var _ installation.DispatchGate = (*dispatchGate)(nil)

func newDispatchGate(enabled bool) *dispatchGate {
	gate := &dispatchGate{}
	gate.enabled.Store(enabled)
	return gate
}

// EnableDispatch enables dispatch after startup preflight or a fenced restore.
func (gate *dispatchGate) EnableDispatch(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "dispatch gate context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	gate.enabled.Store(true)
	return nil
}

// Enabled reports whether new dispatch is currently allowed.
func (gate *dispatchGate) Enabled() bool {
	return gate != nil && gate.enabled.Load()
}

// DispatchGate returns the composition dispatch gate. A restore path uses it
// only after fencing and reconciliation.
func (app *App) DispatchGate() installation.DispatchGate {
	return app.gate
}
