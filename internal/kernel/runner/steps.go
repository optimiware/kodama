package runner

import (
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// pendingStart is what a drive keeps of a launch intent it has committed and not
// yet executed.
type pendingStart struct {
	spec runtime.WorkloadSpec
}

func launchCommandID(run domain.RunID, step domain.StepID, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:%d", runtime.CommandLaunch, run, step, attempt)
}

// ---------------------------------------------------------------------------
// Stages
// ---------------------------------------------------------------------------

// stageFailFast ends the run when a step failed: nothing further starts. The
// failed step's reason becomes the run's.
func (d *drive) stageFailFast() (flow, error) {
	for _, s := range d.rec.Run.Steps {
		if s.State == runtime.StepFailed {
			return d.windDown(modeFail, s.Reason)
		}
	}
	return flowPass, nil
}

// stageReconcile settles every effect whose outcome is not final and that this
// drive did not just issue: an intent or uncertain command left by a crash, a
// lost response or an expired lease. It observes by stable identity; it never
// issues the effect again, with the one exception of an idempotent stop or
// removal, which is issued only when the observation shows it did not take hold.
func (d *drive) stageReconcile() (flow, error) {
	for _, c := range d.rec.Unresolved() {
		if _, ok := d.fresh[c.ID]; ok {
			continue
		}
		switch c.Kind {
		case runtime.CommandLaunch:
			return d.reconcileLaunch(c)
		case runtime.CommandStop, runtime.CommandRemove:
			if _, ok, err := d.workloadEffect(c.Kind, c.Binding); err != nil {
				return flowPass, err
			} else if !ok {
				return d.stop(DriveRetry)
			}
			return flowAgain, nil
		case runtime.CommandEnsureResource:
			return d.reconcileEnsureCommand(c)
		}
	}
	return flowPass, nil
}

func (d *drive) reconcileEnsureCommand(c runtime.Command) (flow, error) {
	h := d.workspaceHandle()
	if c.Target != h.Identity {
		for _, s := range d.rec.Plan.Services {
			if sh := d.serviceHandle(s); sh.Identity == c.Target {
				h = sh
			}
		}
	}
	res, err := d.reconcileEnsure(c, h)
	switch {
	case err != nil:
		return flowPass, err
	case res == ensureInconclusive:
		return d.stop(DriveRetry)
	case res == ensureFailed && h.Kind == runtime.ResourceWorkspace && d.started():
		return d.windDown(modeFail, runtime.ReasonWorkspaceLost)
	case res == ensureFailed && d.started():
		return d.windDown(modeFail, runtime.ReasonServiceLost)
	case res == ensureFailed:
		return d.windDown(modeFail, runtime.ReasonPreparationFailed)
	}
	return flowAgain, nil
}

// reconcileLaunch settles a launch whose outcome is unknown by observing the
// workload by its stable identity and the binding the intent recorded. A
// workload that exists is adopted. One that is absent, or exists with another
// specification, is not launched again: the step fails with launch_uncertain and
// cleanup removes whatever the identity names. A backend that cannot answer
// leaves the launch for a later claim.
func (d *drive) reconcileLaunch(c runtime.Command) (flow, error) {
	b := d.rebind(c.Binding)
	obs, err := d.e.cfg.Runtime.Observe(d.ctx, b)
	if err != nil {
		return d.inconclusive()
	}
	o, aerr := accept(obs, b)
	if aerr != nil {
		return d.failLaunch(c)
	}
	switch o.Phase {
	case runtime.PhaseRunning, runtime.PhaseExited:
		cc := c
		if err := cc.Settle(runtime.CommandConfirmed, runtime.ReasonNone); err != nil {
			return flowPass, err
		}
		bt := d.batch()
		bt.commands = []runtime.Command{cc}
		bt.workloads = []runtime.WorkloadObservation{o}
		d.obsFails = 0
		return flowAgain, d.commit(bt)
	case runtime.PhaseAbsent:
		return d.failLaunch(c)
	}
	return d.inconclusive()
}

// inconclusive counts an observation that told nothing. After the configured
// number the drive stops and a later claim reconciles from the durable record.
func (d *drive) inconclusive() (flow, error) {
	d.obsFails++
	if d.obsFails >= d.e.cfg.ObserveFailures {
		d.obsFails = 0
		return d.stop(DriveRetry)
	}
	if err := d.pause(); err != nil {
		return flowPass, err
	}
	return flowAgain, nil
}

// failLaunch records that an uncertain launch was not found: the command is
// rejected for good and the step fails; it is never retried.
func (d *drive) failLaunch(c runtime.Command) (flow, error) {
	cc := c
	if err := cc.Settle(runtime.CommandRejected, runtime.ReasonLaunchUncertain); err != nil {
		return flowPass, err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{cc}
	if s := d.step(c.StepKey); s.State == runtime.StepRunning && s.Attempt == c.Binding.Attempt {
		if err := bt.run.AdvanceStep(c.StepKey, runtime.StepFailed, runtime.ReasonLaunchUncertain); err != nil {
			return flowPass, err
		}
		bt.moved(c.StepKey)
	}
	return flowAgain, d.commit(bt)
}

// removed reports whether the workload of a step attempt is confirmed removed.
func (d *drive) removed(step domain.StepID, attempt int) bool {
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandRemove && c.StepKey == step && c.Binding.Attempt == attempt && c.State == runtime.CommandConfirmed {
			return true
		}
	}
	return false
}

// stageRelease removes the workload of every settled step before the next step
// starts, so a prior step has settled and released the workspace first. A
// removal that cannot be confirmed fails the run: cleanup keeps working on it.
func (d *drive) stageRelease() (flow, error) {
	for _, s := range d.rec.Run.Steps {
		if s.Kind != runtime.StepContainer || !s.State.Terminal() {
			continue
		}
		for a := 1; a <= s.Attempt; a++ {
			c, ok := d.launchCommand(s.Key, a)
			if !ok || !maybeLaunched(c) || d.removed(s.Key, a) {
				continue
			}
			// A removal is idempotent on the workload identity, so one that
			// provably did not reach the backend, or whose outcome the
			// observation could not settle, is issued again a bounded number
			// of times before it stops the run.
			confirmed := false
			for try := 0; try <= max(d.e.cfg.LaunchRetries, 0) && !confirmed; try++ {
				var err error
				if _, confirmed, err = d.workloadEffect(runtime.CommandRemove, c.Binding); err != nil {
					return flowPass, err
				}
			}
			if !confirmed {
				return d.windDown(modeFail, runtime.ReasonStopUncertain)
			}
			return flowAgain, nil
		}
	}
	return flowPass, nil
}

// stageRunning drives the one container step that holds the workspace.
func (d *drive) stageRunning() (flow, error) {
	var s runtime.Step
	found := false
	for _, x := range d.rec.Run.Steps {
		if x.Kind == runtime.StepContainer && x.State == runtime.StepRunning {
			s, found = x, true
			break
		}
	}
	if !found {
		return flowPass, nil
	}
	cmd, ok := d.launchCommand(s.Key, s.Attempt)
	if !ok {
		return flowPass, fmt.Errorf("runner: running step %s has no launch intent", s.Key)
	}
	if p, fresh := d.fresh[cmd.ID]; fresh {
		return d.start(s, cmd, p)
	}
	switch cmd.State {
	case runtime.CommandConfirmed:
		return d.observeRunning(s, cmd)
	case runtime.CommandRejected:
		return d.failLaunch(cmd)
	}
	return d.stop(DriveRetry)
}

// start issues the launch whose intent this drive committed. Secrets are
// resolved only now, only for the declared references, and released when the
// step settles.
func (d *drive) start(s runtime.Step, cmd runtime.Command, p pendingStart) (flow, error) {
	delete(d.fresh, cmd.ID)
	req := runtime.StartRequest{Binding: cmd.Binding, Spec: p.spec, Workspace: d.workspaceHandle(), Services: d.serviceHandles()}
	if refs := p.spec.Secrets; len(refs) > 0 {
		if d.e.cfg.Secrets == nil {
			return d.rejectLaunch(s, cmd, runtime.ReasonSecretUnavailable)
		}
		m, err := d.e.cfg.Secrets.Resolve(d.ctx, runtime.ResolveRequest{Scope: d.rec.Run.Scope, RunID: d.rec.Run.ID, StepKey: s.Key, Refs: refs})
		if err != nil {
			return d.rejectLaunch(s, cmd, runtime.ReasonSecretUnavailable)
		}
		if d.mats == nil {
			d.mats = map[domain.StepID]runtime.Materialization{}
		}
		d.mats[s.Key] = m
		req.Secrets = m
	}
	obs, err := d.e.cfg.Runtime.Start(d.ctx, req)
	state := runtime.ClassifyEffect(err)
	var o runtime.WorkloadObservation
	if state == runtime.CommandConfirmed {
		var aerr error
		if o, aerr = accept(obs, cmd.Binding); aerr != nil {
			state = runtime.CommandUncertain
		}
	}
	c := cmd
	bt := d.batch()
	switch state {
	case runtime.CommandConfirmed:
		if err := c.Settle(runtime.CommandConfirmed, runtime.ReasonNone); err != nil {
			return flowPass, err
		}
		bt.commands = []runtime.Command{c}
		bt.workloads = []runtime.WorkloadObservation{o}
	case runtime.CommandRejected:
		return d.retryLaunch(s, cmd, p)
	default:
		if err := c.Settle(runtime.CommandUncertain, runtime.ReasonLaunchUncertain); err != nil {
			return flowPass, err
		}
		bt.commands = []runtime.Command{c}
	}
	return flowAgain, d.commit(bt)
}

// rejectLaunch settles a launch that never reached the backend for a reason of
// ours, and fails the step: the failure is not a launch to retry.
func (d *drive) rejectLaunch(s runtime.Step, cmd runtime.Command, reason runtime.Reason) (flow, error) {
	c := cmd
	if err := c.Settle(runtime.CommandRejected, reason); err != nil {
		return flowPass, err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	if err := bt.run.AdvanceStep(s.Key, runtime.StepFailed, reason); err != nil {
		return flowPass, err
	}
	bt.moved(s.Key)
	return flowAgain, d.commit(bt)
}

// retryLaunch handles a launch that provably did not reach the backend: the only
// case in which one is issued again, as the next attempt with its own workload
// identity. Past the retry bound the step fails.
func (d *drive) retryLaunch(s runtime.Step, cmd runtime.Command, p pendingStart) (flow, error) {
	c := cmd
	if err := c.Settle(runtime.CommandRejected, runtime.ReasonLaunchNotReached); err != nil {
		return flowPass, err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	if d.e.cfg.LaunchRetries < 0 || s.Attempt > d.e.cfg.LaunchRetries {
		if err := bt.run.AdvanceStep(s.Key, runtime.StepFailed, runtime.ReasonLaunchNotReached); err != nil {
			return flowPass, err
		}
		bt.moved(s.Key)
		return flowAgain, d.commit(bt)
	}
	if err := bt.run.RetryStep(s.Key, c); err != nil {
		return flowPass, err
	}
	nb := d.binding(s.Key, s.Attempt+1, c.SpecDigest)
	nc, err := runtime.NewWorkloadCommand(launchCommandID(d.rec.Run.ID, s.Key, nb.Attempt), nb, runtime.CommandLaunch, d.now())
	if err != nil {
		return flowPass, err
	}
	bt.commands = append(bt.commands, nc)
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	d.fresh[nc.ID] = p
	return flowAgain, nil
}

// serviceHandles are the ready services a workload may reach.
func (d *drive) serviceHandles() []runtime.ResourceHandle {
	var out []runtime.ResourceHandle
	for _, s := range d.rec.Plan.Services {
		h := d.serviceHandle(s)
		if o, ok := d.resourceObservation(h); ok && o.State == runtime.ResourceReady {
			out = append(out, h)
		}
	}
	return out
}
