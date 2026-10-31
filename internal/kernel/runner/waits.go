package runner

import (
	"fmt"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// stageWaits consumes the decisions of waiting steps. A wait is decided by a
// signal (resolved, committed by the store), by its deadline, or by
// cancellation; the engine's part is to take the one recorded decision into the
// step in a single commit, so the continuation happens once however many times a
// restarted engine looks at it. An open wait past its deadline is expired here,
// by the same version-checked write a signal races against. A wait still open is
// left for a signal and remembered for the report's next wake.
func (d *drive) stageWaits() (flow, error) {
	for _, s := range d.rec.Run.Steps {
		if s.Kind != runtime.StepWait || s.State != runtime.StepWaiting {
			continue
		}
		w, ok := d.wait(s.Key)
		if !ok {
			return flowPass, fmt.Errorf("runner: wait step %s has no wait", s.Key)
		}
		if w.State != runtime.WaitOpen {
			if continues(w) {
				if f, stop, err := d.resume(); stop {
					return f, err
				}
			}
			return d.consume(s, w)
		}
		if !d.now().Before(w.Deadline) {
			if w.Spec.OnExpiry.Effective() == runtime.ExpiryContinue {
				if f, stop, err := d.resume(); stop {
					return f, err
				}
			}
			return d.expire(s, w)
		}
		if d.nextWake.IsZero() || w.Deadline.Before(d.nextWake) {
			d.nextWake = w.Deadline
		}
		// A workspace or service lost during a wait fails the run; it is never
		// replaced.
		if lost, transient := d.verify(); lost != runtime.ReasonNone {
			return d.windDown(modeFail, lost)
		} else if transient {
			return d.stop(DriveRetry)
		}
	}
	return flowPass, nil
}

// continues reports whether a decided wait lets its step succeed and the run go
// on: a resolution, or an expiry that continues.
func continues(w runtime.Wait) bool {
	return w.State == runtime.WaitResolved ||
		(w.State == runtime.WaitExpired && w.Spec.OnExpiry.Effective() == runtime.ExpiryContinue)
}

// resume verifies the retained workspace and services before a waiting step is
// consumed as a success, whether or not another step follows it: a resolution or
// an expiry that continues must not report success over state that was lost
// while the run was parked. A loss fails the run; an inconclusive check leaves it
// for a later claim. stop reports that the caller must return f and err.
func (d *drive) resume() (f flow, stop bool, err error) {
	lost, transient := d.verify()
	switch {
	case lost != runtime.ReasonNone:
		f, err = d.windDown(modeFail, lost)
		return f, true, err
	case transient:
		f, err = d.stop(DriveRetry)
		return f, true, err
	}
	return flowPass, false, nil
}

// openWait opens the wait of a step that became eligible. The correlation values
// and request are read from persisted values at this moment and recorded with the
// wait; the wait, the step's move to waiting and the wait.opened outbox event
// commit together, so the wait exists durably before anything announces it. A
// correlation value that is missing fails the step with correlation_missing and
// no wait opens; a missing request opens the wait without one.
func (d *drive) openWait(ps runtime.PlanStep, ev *runtime.PredicateEvaluation) (flow, error) {
	if lost, transient := d.verify(); lost != runtime.ReasonNone {
		return d.windDown(modeFail, lost)
	} else if transient {
		return d.stop(DriveRetry)
	}
	w, ok := d.wait(ps.Key)
	if !ok {
		return flowPass, fmt.Errorf("runner: wait step %s has no wait", ps.Key)
	}
	read := d.reader(ps)
	correlation := map[string]string{}
	for key, ref := range ps.Wait.Correlate {
		v, present, err := read(ref)
		if err != nil {
			return d.refused(ps)
		}
		if !present || v.Text() == "" || len(v.Text()) > runtime.MaxRequestBytes {
			return d.failBeforeStart(ps, ev, runtime.ReasonCorrelationMissing)
		}
		correlation[key] = v.Text()
	}
	request := ""
	if ref := ps.Wait.Request; ref != "" {
		v, present, err := read(ref)
		if err != nil {
			return d.refused(ps)
		}
		if present && len(v.Text()) <= runtime.MaxRequestBytes {
			request = v.Text()
		}
	}
	if err := w.Open(d.now(), correlation, request); err != nil {
		return d.failBeforeStart(ps, ev, runtime.ReasonPreparationFailed)
	}
	bt := d.batch()
	if err := d.move(bt, ps.Key, ev, runtime.StepWaiting, runtime.ReasonNone); err != nil {
		return flowPass, err
	}
	bt.waits = []runtime.Wait{w}
	bt.events = append(bt.events, pendingEvent{kind: runtime.EventWaitOpened, step: ps.Key, version: w.Version})
	return flowAgain, d.commit(bt)
}

// refused fails a step whose condition, binding or reference the grammar
// refuses: it cannot be prepared. Every reference is read from the run record,
// so a read never fails for a reason that a later claim would not share.
func (d *drive) refused(ps runtime.PlanStep) (flow, error) {
	return d.failBeforeStart(ps, nil, runtime.ReasonPreparationFailed)
}

// expire decides an open wait whose deadline was reached and consumes the
// decision in the same commit: the step follows the wait's on_expiry, and with
// fail the run then fails through fail-fast. Another writer that decided the wait
// first wins the version check; the engine then reloads and consumes that
// decision instead.
func (d *drive) expire(s runtime.Step, w runtime.Wait) (flow, error) {
	if _, err := w.Expire(d.now()); err != nil {
		return flowPass, err
	}
	bt := d.batch()
	if err := consumeDecision(bt, s, w, runtime.ReasonCancelled); err != nil {
		return flowPass, err
	}
	bt.moved(s.Key)
	bt.waits = []runtime.Wait{w}
	bt.events = append(bt.events, pendingEvent{kind: runtime.EventWaitExpired, step: s.Key, version: w.Version})
	return flowAgain, d.commit(bt)
}

// consume takes the recorded decision of a closed wait into its waiting step.
func (d *drive) consume(s runtime.Step, w runtime.Wait) (flow, error) {
	bt := d.batch()
	if err := consumeDecision(bt, s, w, runtime.ReasonCancelled); err != nil {
		return flowPass, err
	}
	bt.moved(s.Key)
	return flowAgain, d.commit(bt)
}

// consumeDecision moves a waiting step according to its wait's decision: a
// resolution settles the step succeeded whatever the outcome, a rejection
// included; an expiry follows on_expiry (continue settles succeeded with the
// reserved outcome expired, fail fails with wait_expired); a cancellation
// cancels the step.
func consumeDecision(bt *batch, s runtime.Step, w runtime.Wait, cancelReason runtime.Reason) error {
	switch w.State {
	case runtime.WaitResolved:
		return bt.run.AdvanceStep(s.Key, runtime.StepSucceeded, runtime.ReasonNone)
	case runtime.WaitExpired:
		if w.Spec.OnExpiry.Effective() == runtime.ExpiryContinue {
			return bt.run.AdvanceStep(s.Key, runtime.StepSucceeded, runtime.ReasonNone)
		}
		return bt.run.AdvanceStep(s.Key, runtime.StepFailed, runtime.ReasonWaitExpired)
	case runtime.WaitCancelled:
		return bt.run.AdvanceStep(s.Key, runtime.StepCancelled, cancelReason)
	}
	return fmt.Errorf("runner: wait %s is %s, not decided", w.ID, w.State)
}
