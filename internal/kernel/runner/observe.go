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
	return flowAgain, nil
}

// Reasons a run is held rather than advanced when the engine cannot revoke a
// settled step's secrets faithfully. They are stable codes in Report.Reason; the
// run's own state is not changed, because nothing here is a verdict on the run.
const (
	// ReasonRevocationNoResolver: a settled step's attempt may have materialized
	// secrets and this engine has no SecretResolver to revoke them with.
	ReasonRevocationNoResolver runtime.Reason = "secret_revocation_no_resolver"
	// ReasonRevocationUnrecoverable: a settled step's materialization was created
	// by an engine that is gone. The frozen SecretResolver contract does not
	// guarantee that Release works from the step's identity alone, and the record
	// does not hold the materialization (its directory and revision), so the
	// engine cannot revoke it faithfully. It holds, never advances and never
	// skips. The durable materialization identity that lifts this is owned by the
	// contract amendment (T-186).
	ReasonRevocationUnrecoverable runtime.Reason = "secret_revocation_unrecoverable"
)

// needsRevocation reports whether a settled container step may hold secret
// material: it declares secrets and at least one of its launch attempts got past
// resolution. An attempt rejected because resolution failed, or because there was
// no resolver, never materialized anything.
func (d *drive) needsRevocation(s runtime.Step) bool {
	ps := d.planStep(s.Key)
	if s.Kind != runtime.StepContainer || !s.State.Terminal() || s.Attempt < 1 || ps.Workload == nil || len(ps.Workload.Secrets) == 0 {
		return false
	}
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandLaunch && c.StepKey == s.Key && c.Reason != runtime.ReasonSecretUnavailable {
			return true
		}
	}
	return false
}

// revoke releases the materialization this drive created for a settled step:
// authority ends when the workload settles, not when the run does. Only the
// engine instance that resolved the secrets holds the materialization, so only it
// can revoke faithfully. A failed release is retried in place, boundedly, and then
// returned; it is never discarded.
func (d *drive) revoke(step domain.StepID) error {
	m, ok := d.mats[step]
	if !ok {
		return nil
	}
	var err error
	for try := 0; try < d.e.cfg.ObserveFailures; try++ {
		if try > 0 {
			if perr := d.pause(); perr != nil {
				return perr
			}
		}
		if err = d.e.cfg.Secrets.Release(d.ctx, m); err == nil {
			delete(d.mats, step)
			if d.released == nil {
				d.released = map[domain.StepID]bool{}
			}
			d.released[step] = true
			return nil
		}
	}
	return err
}

// revokeSettled revokes the secrets of every settled step that needs it, or says
// why it cannot. A step whose materialization this drive does not hold (it was
// created before a restart or takeover) or that has no resolver to revoke it with
// is a hold, not a skip.
func (d *drive) revokeSettled() (hold runtime.Reason, err error) {
	var steps []domain.StepID
	for _, s := range d.rec.Run.Steps {
		if d.needsRevocation(s) && !d.released[s.Key] {
			steps = append(steps, s.Key)
		}
	}
	for _, k := range steps {
		switch _, held := d.mats[k]; {
		case d.e.cfg.Secrets == nil:
			return ReasonRevocationNoResolver, nil
		case !held:
			return ReasonRevocationUnrecoverable, nil
		}
		if err := d.revoke(k); err != nil {
			return runtime.ReasonNone, err
		}
	}
	return runtime.ReasonNone, nil
}

// hold ends the drive without advancing, with a stable reason.
func (d *drive) hold(reason runtime.Reason, status DriveStatus) (flow, error) {
	d.report = Report{Status: status, Reason: reason}
	return flowStop, nil
}

// stageRevoke gates every pass after the failure check: no step is evaluated,
// started or consumed until the secrets of the steps already settled are
// revoked. It fails closed: a settled step that may hold material this engine
// cannot revoke holds the run (DriveHeld). A release that keeps failing leaves
// the run for a later claim.
func (d *drive) stageRevoke() (flow, error) {
	hold, err := d.revokeSettled()
	switch {
	case err != nil:
		return d.stop(DriveRetry)
	case hold != runtime.ReasonNone:
		return d.hold(hold, DriveHeld)
	}
	return flowPass, nil
}
