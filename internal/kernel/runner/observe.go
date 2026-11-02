package runner

import (
	"errors"
	"time"
	"unicode/utf8"

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
			runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}, &o, nil)
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
		runtime.Result{Code: runtime.ResultTimedOut, Reason: runtime.ReasonStepTimeout}, nil, nil)
}

// settleStep records the result of an exited workload on its step. A succeeded
// step records its declared outputs in the same commit, so they are visible
// exactly when the step is; the outputs of any other outcome are never recorded.
func (d *drive) settleStep(s runtime.Step, o runtime.WorkloadObservation) (flow, error) {
	res := o.Result
	switch res.Code {
	case runtime.ResultSucceeded:
		outs, err := d.collectOutputs(s)
		if err != nil {
			return d.finishStep(s, runtime.StepFailed, runtime.ReasonCommandFailed,
				runtime.Result{Code: runtime.ResultFailed, Reason: runtime.ReasonCommandFailed}, &o, nil)
		}
		return d.finishStep(s, runtime.StepSucceeded, runtime.ReasonNone, res, &o, outs)
	case runtime.ResultFailed:
		return d.finishStep(s, runtime.StepFailed, runtime.ReasonCommandFailed, res, &o, nil)
	case runtime.ResultTimedOut:
		return d.finishStep(s, runtime.StepFailed, runtime.ReasonStepTimeout, res, &o, nil)
	}
	// Lost, or cancelled by someone other than this run's own request.
	return d.finishStep(s, runtime.StepFailed, runtime.ReasonWorkloadLost,
		runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}, &o, nil)
}

// collectOutputs reads what a succeeded step produced through the collector and
// keeps what the record accepts: names the plan declared, with bounded text.
// Nothing recorded is returned as nil.
func (d *drive) collectOutputs(s runtime.Step) (*runtime.StepOutputs, error) {
	ps := d.planStep(s.Key)
	out := d.e.cfg.Outputs
	if out == nil || len(ps.Outputs) == 0 {
		return nil, nil
	}
	got, err := out.Collect(d.ctx, StepRef{Scope: d.rec.Run.Scope, RunID: d.rec.Run.ID, StepKey: s.Key, Attempt: s.Attempt}, ps.Outputs)
	if err != nil {
		return nil, err
	}
	rec := runtime.StepOutputs{StepKey: s.Key, Attempt: s.Attempt, Values: map[string]string{}}
	for _, name := range ps.Outputs {
		if v, ok := got[name]; ok && len(v) <= runtime.MaxOutputValueBytes && utf8.ValidString(v) {
			rec.Values[name] = v
		}
	}
	if len(rec.Values) == 0 {
		return nil, nil
	}
	return &rec, nil
}

// finishStep commits the settlement of a running container step with the
// observation it rests on and the outputs it produced. Its secret access is
// revoked next, before anything else is evaluated: authority ends when the
// workload settles, not when the run does.
func (d *drive) finishStep(s runtime.Step, to runtime.StepState, reason runtime.Reason, res runtime.Result, o *runtime.WorkloadObservation, outs *runtime.StepOutputs) (flow, error) {
	bt := d.batch()
	if err := bt.run.FinishStep(s.Key, to, reason, res); err != nil {
		return flowPass, err
	}
	bt.moved(s.Key)
	if o != nil {
		bt.workloads = []runtime.WorkloadObservation{*o}
	}
	if outs != nil {
		bt.outputs = []runtime.StepOutputs{*outs}
	}
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	return flowAgain, nil
}

// Reasons a run is held rather than advanced when the engine cannot revoke a
// settled step's secrets. They are stable codes in Report.Reason; the run's own
// state is not changed, because nothing here is a verdict on the run.
const (
	// ReasonRevocationNoResolver: the record holds secret material of a settled
	// step that is not yet revoked and this engine has no SecretResolver to revoke
	// it with. It is a real condition of the engine's configuration, not of the
	// record: any engine with a resolver revokes the material from its recorded
	// identity and resumes the run.
	ReasonRevocationNoResolver runtime.Reason = "secret_revocation_no_resolver"
)

// revokeError marks a failure of the resolver's Revoke, which the engine retries
// and then leaves for a later claim, as opposed to a failure of the store.
type revokeError struct{ err error }

func (e revokeError) Error() string { return e.err.Error() }
func (e revokeError) Unwrap() error { return e.err }

// issue records the identity of the material of one attempt before the resolver
// is asked to create it. A repeat of a recorded identity writes nothing.
func (d *drive) issue(ref runtime.MaterializationRef) error {
	for _, m := range d.rec.Materializations {
		if m.Ref.ID == ref.ID {
			return nil
		}
	}
	bt := d.batch()
	bt.mats = []runtime.MaterializationRecord{{Ref: ref, State: runtime.MaterializationIssued, IssuedAt: d.now()}}
	return d.commit(bt)
}

// revoke revokes one recorded materialization by its identity, with no
// directory and no knowledge of who created it, and records that it is revoked.
// A failed revocation is retried in place, boundedly, and then returned as a
// revokeError; it is never discarded. A crash between the resolver's success and
// the record leaves the identity issued, and the next claim revokes it again,
// which the resolver contract makes idempotent.
func (d *drive) revoke(m runtime.MaterializationRecord) error {
	var err error
	for try := 0; try < d.e.cfg.ObserveFailures; try++ {
		if try > 0 {
			if perr := d.pause(); perr != nil {
				return perr
			}
		}
		if err = d.e.cfg.Secrets.Revoke(d.ctx, m.Ref); err == nil {
			break
		}
	}
	if err != nil {
		return revokeError{err}
	}
	m.State = runtime.MaterializationRevoked
	bt := d.batch()
	bt.mats = []runtime.MaterializationRecord{m}
	return d.commit(bt)
}

// revokeSettled revokes every recorded materialization whose step settled (or
// whose attempt was superseded), or says why it cannot: with no resolver it is a
// hold, not a skip. Everything else is the record's, so an engine that took the
// run over resumes it.
func (d *drive) revokeSettled() (hold runtime.Reason, err error) {
	pending := d.rec.PendingRevocations()
	switch {
	case len(pending) == 0:
		return runtime.ReasonNone, nil
	case d.e.cfg.Secrets == nil:
		return ReasonRevocationNoResolver, nil
	}
	for _, m := range pending {
		if err := d.revoke(m); err != nil {
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

// retryRevocation turns a failed revocation into the drive's retry status and
// passes any other error (a lost version race, a takeover, a cancelled context)
// to the drive loop, which reloads or stops.
func (d *drive) retryRevocation(err error, status DriveStatus) (flow, error) {
	var re revokeError
	if errors.As(err, &re) {
		return d.stop(status)
	}
	return flowPass, err
}

// stageRevoke gates every pass after the failure check: no step is evaluated,
// started or consumed until the secrets of the steps already settled are
// revoked. It fails closed: with material to revoke and no resolver it holds the
// run (DriveHeld); a revocation that keeps failing leaves the run for a later
// claim.
func (d *drive) stageRevoke() (flow, error) {
	hold, err := d.revokeSettled()
	switch {
	case err != nil:
		return d.retryRevocation(err, DriveRetry)
	case hold != runtime.ReasonNone:
		return d.hold(hold, DriveHeld)
	}
	return flowPass, nil
}
