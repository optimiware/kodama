package postgres

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// RequestCancel records the cancellation and decides every pending and open wait
// of the run as cancelled, in one transaction and without a claim. It is
// idempotent, never waits for a lease (it only takes the run's row lock for the
// length of the transaction), and changes nothing for a settled run. A signal
// that decides a wait concurrently takes the same row lock, so exactly one of
// them decides it.
func (s *Store) RequestCancel(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.Run, error) {
	if err := checkRef(scope, id); err != nil {
		return runtime.Run{}, err
	}
	now := micro(s.clock.Now())
	var out runtime.Run
	err := s.update(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := loadRecord(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if rec.Run.Settled() {
			// Cancelling a settled run changes nothing, including a wait a skipped
			// step left pending: its decision, if any, was taken when the run settled.
			out = rec.Run
			return nil
		}
		next := rec.Run.Clone()
		next.RequestCancel()
		waits := slices.Clone(rec.Waits)
		var events []runtime.Event
		var decided []int
		for i := range waits {
			w := &waits[i]
			if w.State != runtime.WaitPending && w.State != runtime.WaitOpen {
				continue
			}
			if _, err := w.Cancel(now, runtime.ReasonRunCancelled); err != nil {
				return err
			}
			decided = append(decided, i)
			events = append(events, runtime.Event{ID: fmt.Sprintf("wait.cancelled:%s:%d", w.ID, w.Version), Scope: scope,
				RunID: id, StepKey: w.StepKey, Kind: runtime.EventWaitCancelled, Version: w.Version,
				Reason: runtime.ReasonRunCancelled, At: now})
		}
		if next.Version != rec.Run.Version {
			if err := writeRun(ctx, tx, rec.Run, next, now); err != nil {
				return err
			}
		}
		for _, i := range decided {
			si := slices.IndexFunc(rec.Run.Steps, func(st runtime.Step) bool { return st.Key == waits[i].StepKey })
			if si < 0 {
				return invalid("wait %s names an unknown step", waits[i].ID)
			}
			if err := writeStep(ctx, tx, scope, id, rec.Run.Steps[si], &waits[i], nil, now); err != nil {
				return err
			}
		}
		if err := emit(ctx, tx, events); err != nil {
			return err
		}
		cur, err := loadRecord(ctx, tx, scope, id, false)
		out = cur.Run
		return err
	})
	if err != nil {
		return runtime.Run{}, err
	}
	return out, nil
}
