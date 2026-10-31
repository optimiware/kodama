package runner

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func (d *drive) timeoutOf(ps runtime.PlanStep) time.Duration {
	if ps.Workload != nil && ps.Workload.Timeout > 0 {
		return ps.Workload.Timeout
	}
	return d.e.cfg.StepTimeout
}

// observeRunning observes the workload of the running step by its stable
// identity and acts on what it finds: settle an exited workload, fail one that
// vanished, stop one past its deadline, and otherwise keep the lease and poll.
func (d *drive) observeRunning(s runtime.Step, cmd runtime.Command) (flow, error) {
	b := d.rebind(cmd.Binding)
	obs, err := d.e.cfg.Runtime.Observe(d.ctx, b)
	if err != nil {
		return d.inconclusive()
	}
	o, err := accept(obs, b)
	if err != nil {
		return d.inconclusive()
	}
	switch o.Phase {
	case runtime.PhaseExited:
		d.obsFails = 0
		return d.settleStep(s, o)
	case runtime.PhaseAbsent:
		d.obsFails = 0
		return d.finishStep(s, runtime.StepFailed, runtime.ReasonWorkloadLost,
			runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}, &o)
	case runtime.PhaseRunning:
		d.obsFails = 0
		if prev, ok := d.workloadObservation(s.Key, s.Attempt); !ok || prev.Phase != o.Phase {
			bt := d.batch()
			bt.workloads = []runtime.WorkloadObservation{o}
			return flowAgain, d.commit(bt)
		}
		if d.now().Sub(cmd.RecordedAt) >= d.timeoutOf(d.planStep(s.Key)) {
			return d.timeout(s, cmd)
		}
		if err := d.pause(); err != nil {
			return flowPass, err
		}
		return flowAgain, nil
	}
	return d.inconclusive()
}

// timeout stops a workload that outlived its step's deadline and fails the step.
func (d *drive) timeout(s runtime.Step, cmd runtime.Command) (flow, error) {
	if _, _, err := d.workloadEffect(runtime.CommandStop, cmd.Binding); err != nil {
		return flowPass, err
	}
	return d.finishStep(s, runtime.StepFailed, runtime.ReasonStepTimeout,
		runtime.Result{Code: runtime.ResultTimedOut, Reason: runtime.ReasonStepTimeout}, nil)
}

// settleStep records the result of an exited workload on its step. A succeeded
// step records its declared outputs first, so they are visible exactly when the
// step is; the outputs of any other outcome are never recorded.
func (d *drive) settleStep(s runtime.Step, o runtime.WorkloadObservation) (flow, error) {
	res := o.Result
	switch res.Code {
	case runtime.ResultSucceeded:
		ps := d.planStep(s.Key)
		if out := d.e.cfg.Outputs; out != nil && len(ps.Outputs) > 0 {
			ref := StepRef{Scope: d.rec.Run.Scope, RunID: d.rec.Run.ID, StepKey: s.Key, Attempt: s.Attempt}
			if _, err := out.Collect(d.ctx, ref, ps.Outputs); err != nil {
				return d.finishStep(s, runtime.StepFailed, runtime.ReasonCommandFailed,
					runtime.Result{Code: runtime.ResultFailed, Reason: runtime.ReasonCommandFailed}, &o)
			}
		}
		return d.finishStep(s, runtime.StepSucceeded, runtime.ReasonNone, res, &o)
	case runtime.ResultFailed:
		return d.finishStep(s, runtime.StepFailed, runtime.ReasonCommandFailed, res, &o)
	case runtime.ResultTimedOut:
		return d.finishStep(s, runtime.StepFailed, runtime.ReasonStepTimeout, res, &o)
	}
	// Lost, or cancelled by someone other than this run's own request.
	return d.finishStep(s, runtime.StepFailed, runtime.ReasonWorkloadLost,
		runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}, &o)
}

// finishStep commits the settlement of a running container step with the
// observation it rests on, then revokes the step's secret access: authority ends
// when its workload settles, not when the run does.
func (d *drive) finishStep(s runtime.Step, to runtime.StepState, reason runtime.Reason, res runtime.Result, o *runtime.WorkloadObservation) (flow, error) {
	bt := d.batch()
	if err := bt.run.FinishStep(s.Key, to, reason, res); err != nil {
		return flowPass, err
	}
	bt.moved(s.Key)
	if o != nil {
		bt.workloads = []runtime.WorkloadObservation{*o}
	}
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	d.releaseSecrets(s.Key)
	return flowAgain, nil
}

// releaseSecrets removes the step's materialization. After a restart the engine
// no longer holds the one it created, so it releases by the step's identity and
// declared references; Release is idempotent.
func (d *drive) releaseSecrets(step domain.StepID) {
	ps := d.planStep(step)
	if d.e.cfg.Secrets == nil || ps.Workload == nil || len(ps.Workload.Secrets) == 0 {
		return
	}
	m, ok := d.mats[step]
	if !ok {
		m = runtime.Materialization{Scope: d.rec.Run.Scope, RunID: d.rec.Run.ID, StepKey: step, Refs: ps.Workload.Secrets}
	}
	_ = d.e.cfg.Secrets.Release(d.ctx, m)
	delete(d.mats, step)
}
