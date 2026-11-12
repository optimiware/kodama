package engine

// Recovery scanning is the authoritative reschedule mechanism. Notifications may
// accelerate it, but a restart reconstructs work only from persisted due times
// and outstanding intents.
//
// A pass asks for every due and every unfinished execution. Unfinished work
// stays unfinished after it was reconciled (a running container is observed
// again on the next pass), so a bounded selection in a stable order would return
// the same executions every time and never reach the rest. The request therefore
// carries no limit: a zero Limit asks the state port for the complete set.

import (
	"context"
	"errors"
)

// Scan reconciles every due and unfinished execution reported by the state
// port. The authoritative time is read once per pass so both scans agree.
//
// An execution that cannot be reconciled does not stop the pass: the remaining
// executions are still reconciled, and the collected failures are returned
// joined once the pass is done. A cancelled context or a failed state scan ends
// the pass immediately.
func (e *Engine) Scan(ctx context.Context) error {
	now := e.now()
	var failures []error
	for _, kind := range ScanKinds() {
		if err := ctx.Err(); err != nil {
			return err
		}
		executionIDs, err := e.state.Scan(ctx, ScanRequest{Kind: kind, Now: now})
		if err != nil {
			return err
		}
		for _, executionID := range executionIDs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := e.Reconcile(ctx, executionID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
