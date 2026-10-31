package runner

import (
	"maps"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// prerequisitesTerminal reports whether every depends_on step is terminal, which
// is when a step is evaluated.
func (d *drive) prerequisitesTerminal(ps runtime.PlanStep) bool {
	for _, k := range ps.DependsOn {
		if !d.step(k).State.Terminal() {
			return false
		}
	}
	return true
}

// stageNext evaluates the first pending step, in declaration order, whose
// prerequisites are all terminal. A step is evaluated once and the evaluation is
// recorded with its move.
func (d *drive) stageNext() (flow, error) {
	for _, ps := range d.rec.Plan.Steps {
		if d.step(ps.Key).State != runtime.StepPending || !d.prerequisitesTerminal(ps) {
			continue
		}
		return d.evaluate(ps)
	}
	return flowPass, nil
}

// stageSettle settles the run once every step is terminal, or parks it on the
// waits that are still open.
func (d *drive) stageSettle() (flow, error) {
	waiting, live := false, false
	for _, s := range d.rec.Run.Steps {
		switch {
		case s.State == runtime.StepWaiting:
			waiting = true
		case !s.State.Terminal():
			live = true
		}
	}
	switch {
	case waiting:
		// Nothing else is eligible while a wait is open: park until a signal or
		// the deadline. The lease is left to expire; a later claim resumes the run.
		d.report = Report{Status: DriveWaiting, Next: d.nextWake}
		return flowStop, nil
	case live:
		return flowPass, nil
	}
	bt := d.batch()
	if err := bt.run.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone); err != nil {
		return flowPass, err
	}
	bt.runChanged(runtime.ReasonNone)
	return flowAgain, d.commit(bt)
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

// joinSatisfied applies a step's join to its prerequisites, every one of which
// is succeeded or skipped: a failed prerequisite ends the run before this step
// is evaluated.
func (d *drive) joinSatisfied(ps runtime.PlanStep) bool {
	if len(ps.DependsOn) == 0 {
		return true
	}
	ok := 0
	for _, k := range ps.DependsOn {
		if d.step(k).State == runtime.StepSucceeded {
			ok++
		}
	}
	if ps.Join.Effective() == runtime.JoinAny {
		return ok > 0
	}
	return ok == len(ps.DependsOn)
}

// move applies a step's first move. A step with a when predicate commits the
// evaluation with the move in one run-version increment (EvaluateStep); one whose
// evaluation was already recorded, or that has none, just moves.
func (d *drive) move(bt *batch, key domain.StepID, ev *runtime.PredicateEvaluation, to runtime.StepState, reason runtime.Reason) error {
	var err error
	if ev != nil && !d.step(key).Predicate.Evaluated {
		err = bt.run.EvaluateStep(key, *ev, to, reason)
	} else {
		err = bt.run.AdvanceStep(key, to, reason)
	}
	if err != nil {
		return err
	}
	bt.moved(key)
	return nil
}

func (d *drive) skip(ps runtime.PlanStep, ev *runtime.PredicateEvaluation, reason runtime.Reason) (flow, error) {
	bt := d.batch()
	if err := d.move(bt, ps.Key, ev, runtime.StepSkipped, reason); err != nil {
		return flowPass, err
	}
	return flowAgain, d.commit(bt)
}

// failBeforeStart fails a step that never started: no workload and no wait.
func (d *drive) failBeforeStart(ps runtime.PlanStep, ev *runtime.PredicateEvaluation, reason runtime.Reason) (flow, error) {
	bt := d.batch()
	if err := d.move(bt, ps.Key, ev, runtime.StepFailed, reason); err != nil {
		return flowPass, err
	}
	return flowAgain, d.commit(bt)
}

// evaluate decides, once, what happens to an evaluable step. The first matching
// rule applies: the join, then the when predicate, then (for a protected step)
// the requirements; otherwise the step starts.
func (d *drive) evaluate(ps runtime.PlanStep) (flow, error) {
	if !d.joinSatisfied(ps) {
		return d.skip(ps, nil, runtime.ReasonPrerequisiteSkipped)
	}
	var ev *runtime.PredicateEvaluation
	if ps.When != "" {
		if s := d.step(ps.Key); s.Predicate.Evaluated {
			// Recovery reuses the record and never evaluates again.
			e := s.Predicate
			ev = &e
		} else {
			e, err := d.evaluateWhen(ps)
			if err != nil {
				// A port that failed is not a verdict on the condition: the step
				// stays pending for a later claim. Only a condition the grammar
				// refuses fails it.
				return d.readFailed(ps, err)
			}
			ev = &e
		}
		if !ev.Result {
			return d.skip(ps, ev, runtime.ReasonConditionFalse)
		}
	}
	// The engine checks the recorded decisions again, whatever the format said.
	if ps.Protected() {
		if err := d.rec.RequirementsMet(ps.Key); err != nil {
			return d.skip(ps, ev, runtime.ReasonRequiresUnmet)
		}
	}
	if ps.Kind == runtime.StepWait {
		return d.openWait(ps, ev)
	}
	return d.startStep(ps, ev)
}

// startStep moves a container step to running together with its launch intent:
// the intent is durable before any effect. The workload runs only against a
// workspace and services that are still there.
func (d *drive) startStep(ps runtime.PlanStep, ev *runtime.PredicateEvaluation) (flow, error) {
	if lost, transient := d.verify(); lost != runtime.ReasonNone {
		return d.windDown(modeFail, lost)
	} else if transient {
		return d.stop(DriveRetry)
	}
	spec, err := d.workloadSpec(ps)
	if err != nil {
		return d.readFailed(ps, err)
	}
	digest, err := workloadDigest(spec, d.workspaceHandle().Identity)
	if err != nil {
		return flowPass, err
	}
	bt := d.batch()
	if err := d.move(bt, ps.Key, ev, runtime.StepRunning, runtime.ReasonNone); err != nil {
		return flowPass, err
	}
	b := d.binding(ps.Key, 1, digest)
	cmd, err := runtime.NewWorkloadCommand(launchCommandID(d.rec.Run.ID, ps.Key, 1), b, runtime.CommandLaunch, d.now())
	if err != nil {
		return flowPass, err
	}
	bt.commands = []runtime.Command{cmd}
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	d.fresh[cmd.ID] = pendingStart{spec: spec}
	return flowAgain, nil
}

// workloadSpec resolves a container step into the specification the backend
// receives: the declared environment, the built-in KODAMA_ values, the bindings'
// current values (a missing value leaves its variable unset) and the timeout.
func (d *drive) workloadSpec(ps runtime.PlanStep) (runtime.WorkloadSpec, error) {
	w := *ps.Workload
	w.Commands = slices.Clone(w.Commands)
	w.Secrets = slices.Clone(w.Secrets)
	w.Requires = slices.Clone(w.Requires)
	env := maps.Clone(w.Env)
	if env == nil {
		env = map[string]string{}
	}
	r := d.rec.Run
	env["KODAMA_TENANT"] = string(r.Scope.TenantID)
	env["KODAMA_PROJECT"] = string(r.Scope.ProjectID)
	env["KODAMA_RUN_ID"] = string(r.ID)
	env["KODAMA_STEP_KEY"] = string(ps.Key)
	for name, ref := range ps.Bindings {
		v, present, err := d.reader(ps)(ref)
		if err != nil {
			return runtime.WorkloadSpec{}, err
		}
		if present && len(v.Text()) <= runtime.MaxRequestBytes {
			env[name] = v.Text()
		}
	}
	w.Env = env
	w.Timeout = d.timeoutOf(ps)
	return w, nil
}
