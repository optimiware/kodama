package runner

import (
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// windMode says why a run is being brought to an end.
type windMode int

const (
	// modeFail ends a run after a failure: steps that never started are skipped
	// with run_failed and open waits are decided cancelled.
	modeFail windMode = iota
	// modeCancel ends a cancelled run: steps that never started are cancelled.
	modeCancel
)

// cancel ends a run whose cancellation was requested. The request itself already
// decided every pending and open wait cancelled and blocks execution claims; this
// stops the running workload, settles the steps and records the outcome.
func (d *drive) cancel() (flow, error) {
	return d.windDown(modeCancel, runtime.ReasonRunCancelled)
}

// windDown brings every live step to a terminal state and settles the run in one
// atomic commit: the run's outcome is recorded once, after the running workload
// has been stopped. Stopping is an effect and goes first, with its intent
// committed and an uncertain outcome reconciled; if a stop cannot be confirmed
// the step is still ended and cleanup keeps working on the workload's identity.
// Execution outcome and cleanup are separate: cleanup starts when this commits.
func (d *drive) windDown(mode windMode, reason runtime.Reason) (flow, error) {
	for _, s := range d.rec.Run.Steps {
		if s.Kind != runtime.StepContainer || s.State != runtime.StepRunning {
			continue
		}
		if c, ok := d.launchCommand(s.Key, s.Attempt); ok && c.State != runtime.CommandRejected {
			if _, _, err := d.workloadEffect(runtime.CommandStop, c.Binding); err != nil {
				return flowPass, err
			}
		}
	}

	now := d.now()
	stepReason, waitReason := runtime.ReasonRunFailed, runtime.ReasonRunFailed
	if mode == modeCancel {
		stepReason, waitReason = runtime.ReasonCancelled, runtime.ReasonRunCancelled
	}
	bt := d.batch()
	var released []domain.StepID
	for _, s := range d.rec.Run.Steps {
		switch {
		case s.State.Terminal():
		case s.Kind == runtime.StepContainer && s.State == runtime.StepRunning:
			if err := bt.run.FinishStep(s.Key, runtime.StepCancelled, stepReason,
				runtime.Result{Code: runtime.ResultCancelled, Reason: stepReason}); err != nil {
				return flowPass, err
			}
			bt.moved(s.Key)
			released = append(released, s.Key)
		case s.Kind == runtime.StepWait && s.State == runtime.StepWaiting:
			w, ok := d.wait(s.Key)
			if !ok {
				return flowPass, fmt.Errorf("runner: wait step %s has no wait", s.Key)
			}
			if w.State == runtime.WaitOpen {
				if _, err := w.Cancel(now, waitReason); err != nil {
					return flowPass, err
				}
				bt.waits = append(bt.waits, w)
				bt.events = append(bt.events, pendingEvent{kind: runtime.EventWaitCancelled, step: s.Key, version: w.Version, reason: waitReason})
			}
			if err := consumeDecision(bt, s, w, stepReason); err != nil {
				return flowPass, err
			}
			bt.moved(s.Key)
		default: // pending
			to, why := runtime.StepSkipped, runtime.ReasonRunFailed
			if mode == modeCancel {
				to, why = runtime.StepCancelled, runtime.ReasonCancelled
			}
			if err := bt.run.AdvanceStep(s.Key, to, why); err != nil {
				return flowPass, err
			}
			bt.moved(s.Key)
		}
	}
	outcome := runtime.OutcomeFailed
	if mode == modeCancel {
		outcome = runtime.OutcomeCancelled
	}
	if err := bt.run.Settle(outcome, reason); err != nil {
		return flowPass, err
	}
	bt.runChanged(reason)
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	for _, k := range released {
		d.releaseSecrets(k)
	}
	return flowAgain, nil
}
