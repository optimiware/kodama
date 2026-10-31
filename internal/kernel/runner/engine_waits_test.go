package runner_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// reviewPlan is build -> review (wait) -> {ship, rework, integrate}, plus late
// when the wait continues on expiry. ship runs on an approval, rework on a
// rejection, late after an expiry, and integrate is the protected step that only
// an approving decision satisfies. The frozen format allows the literal expired in
// a when only for a wait with on_expiry: continue, so late exists only then.
func reviewPlan(onExpiry runtime.ExpiryAction) runtime.Plan {
	build := ctr("build")
	build.Outputs = []string{"commit", "question"}
	review := runtime.PlanStep{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &runtime.WaitSpec{
		Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"}, Input: "approval",
		Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour, OnExpiry: onExpiry}}
	ship := ctr("ship", "review")
	ship.When = `review.outcome == "approved"`
	rework := ctr("rework", "review")
	rework.When = `review.outcome == "rejected"`
	integrate := ctr("integrate", "review")
	integrate.Authority = runtime.AuthorityIntegrate
	integrate.Requires = []runtime.Requirement{{Wait: "review", Outcome: "approved"}}
	steps := []runtime.PlanStep{build, review, ship, rework}
	if onExpiry == runtime.ExpiryContinue {
		late := ctr("late", "review")
		late.When = `review.outcome == "expired"`
		steps = append(steps, late)
	}
	steps = append(steps, integrate)
	return plan("review-"+string(onExpiry), steps...)
}

func signal(run domain.RunID, key, outcome string) runtime.SignalRequest {
	return runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope, Caller: "alice", Key: key},
		WaitID: waitID(run, "review"), Outcome: outcome, Answer: "because", Actor: "user:alice"}
}

func (e *env) decide(req runtime.SignalRequest) runtime.SignalResult {
	e.t.Helper()
	res, err := e.state.DecideSignal(context.Background(), req, e.clock.Now())
	if err != nil {
		e.t.Fatalf("decide %q: %v", req.Identity.Key, err)
	}
	return res
}

// park admits the review plan, produces a candidate and runs until the run waits.
func (e *env) park(id domain.RunID, onExpiry runtime.ExpiryAction) runner.Report {
	e.t.Helper()
	e.admit(id, reviewPlan(onExpiry), 3)
	e.outputs.produce("build", map[string]string{"commit": "c1", "question": "why?"})
	rep := e.drive(e.engine("w1"), id)
	e.expect(rep.Status == runner.DriveWaiting, "the run parks on the wait: %s", rep.Status)
	return rep
}

func reviewWaitOf(rec runtime.RunRecord) runtime.Wait {
	for _, w := range rec.Waits {
		if w.StepKey == "review" {
			return w
		}
	}
	return runtime.Wait{}
}

// Test_unit_kernel_engine_waits proves the wait and condition behaviour: the wait
// is persisted before it is announced, one decision is consumed once, resolution,
// expiry and cancellation race on one version-checked decision, and the deadline's
// declared branch is taken through when only.
func Test_unit_kernel_engine_waits(t *testing.T) {
	t.Run("a wait is persisted before it is announced and one signal resumes the run once", func(t *testing.T) {
		e := newEnv(t)
		rep := e.park("run1", "")
		rec := e.load("run1")
		w := reviewWaitOf(rec)
		e.expect(w.State == runtime.WaitOpen && w.Correlation["candidate"] == "c1" && w.Request == "", "the wait is open with what it read: %+v", w)
		e.expect(w.Deadline.Equal(w.OpenedAt.Add(time.Hour)) && rep.Next.Equal(w.Deadline), "the report names the deadline: %v vs %v", rep.Next, w.Deadline)
		e.expect(stepOf(rec, "review").State == runtime.StepWaiting && rec.Run.Status() == runtime.StatusRunning, "a run with an open wait stays running: %v", rec.Run.Status())
		var opened []runtime.Event
		for _, ev := range e.outbox("run1") {
			if ev.Kind == runtime.EventWaitOpened {
				opened = append(opened, ev)
			}
		}
		e.expect(len(opened) == 1 && opened[0].Version == w.Version && opened[0].StepKey == "review",
			"wait.opened commits with the open wait, so the wait exists before anything announces it: %+v", opened)
		e.expect(e.rt.liveCount() == 0 && e.rt.count(e.rt.starts) == 1, "a wait holds no workload")

		res := e.decide(signal("run1", "k1", "approved"))
		e.expect(res.Applied && res.Decision.Outcome == "approved", "the signal decided the wait: %+v", res)
		e.expire()
		e.drive(e.engine("w2"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded, "run: %+v", got.Run)
		e.expect(fmt.Sprint(states(got)) == "map[build:succeeded integrate:succeeded review:succeeded rework:skipped ship:succeeded]", "states: %v", states(got))
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build ship integrate]", "the next declared steps ran once, in order: %v", e.rt.startOrder)
		e.expect(stepOf(got, "rework").Reason == runtime.ReasonConditionFalse, "a false predicate skips")
		ev := stepOf(got, "ship").Predicate
		e.expect(ev.Evaluated && ev.Result && ev.InputDigest != "" && !ev.At.IsZero(), "the predicate result is recorded with the step: %+v", ev)
		e.expect(!stepOf(got, "rework").Predicate.Result && stepOf(got, "rework").Predicate.Evaluated, "so is a false one")

		// Replays and late signals change nothing and create no second continuation.
		starts := e.rt.count(e.rt.starts)
		again := e.decide(signal("run1", "k1", "approved"))
		e.expect(again.Replayed && again.Applied, "a replay returns the recorded result: %+v", again)
		late := e.decide(signal("run1", "k2", "rejected"))
		e.expect(!late.Applied && late.Refusal == runtime.RefusalDecided && late.Decision.Outcome == "approved", "a late signal is refused with the decision it lost to: %+v", late)
		e.expire()
		e.drive(e.engine("w3"), "run1")
		e.expect(e.rt.count(e.rt.starts) == starts && fmt.Sprint(e.rt.startOrder) == "[build ship integrate]", "nothing ran again")
		e.expect(countKind(e.outbox("run1"), runtime.EventWaitResolved) == 1, "one wait.resolved event")
	})

	t.Run("a restart between the decision and the next step continues the same run once", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", "")
		e.rt.scripts["ship"] = script{polls: 5}
		e.decide(signal("run1", "k1", "approved"))
		e.expire()

		sleeps := 0
		e.onSleep = func() error {
			if sleeps++; sleeps == 2 {
				return errCrash
			}
			return nil
		}
		_, err := e.engine("w2").Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
		e.expect(errors.Is(err, errCrash), "the engine crashed mid-continuation: %v", err)
		mid := e.load("run1")
		e.expect(stepOf(mid, "review").State == runtime.StepSucceeded && stepOf(mid, "ship").State == runtime.StepRunning, "states: %v", states(mid))

		e.onSleep = nil
		e.expire()
		e.drive(e.engine("w3"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded, "run: %+v", got.Run)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build ship integrate]", "no settled step was repeated: %v", e.rt.startOrder)
		changes := 0
		for _, ev := range e.outbox("run1") {
			if ev.Kind == runtime.EventStepStateChanged && ev.StepKey == "review" && ev.Reason == runtime.ReasonNone && ev.Version == stepOf(got, "review").Version {
				changes++
			}
		}
		e.expect(changes == 1, "the decision was consumed in one committed step change, saw %d", changes)
	})

	t.Run("a rejection is a recorded outcome that takes its branch and never fails the run", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", "")
		e.decide(signal("run1", "k1", "rejected"))
		e.expire()
		e.drive(e.engine("w2"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && got.Run.Reason == runtime.ReasonNone, "run: %+v", got.Run)
		e.expect(stepOf(got, "review").State == runtime.StepSucceeded, "a decided wait settles succeeded")
		e.expect(stepOf(got, "rework").State == runtime.StepSucceeded && stepOf(got, "ship").Reason == runtime.ReasonConditionFalse, "the rejected branch ran: %v", states(got))
		i := stepOf(got, "integrate")
		e.expect(i.State == runtime.StepSkipped && i.Reason == runtime.ReasonRequiresUnmet, "the protected step is skipped after a rejection: %+v", i)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build rework]", "the protected step never started: %v", e.rt.startOrder)
	})

	t.Run("a deadline continues through when only and never authorizes a protected step", func(t *testing.T) {
		e := newEnv(t)
		rep := e.park("run1", runtime.ExpiryContinue)
		e.clock.Advance(time.Hour)
		e.expect(!e.clock.Now().Before(rep.Next), "the deadline passed")
		e.drive(e.engine("w2"), "run1")

		got := e.load("run1")
		w := reviewWaitOf(got)
		e.expect(w.State == runtime.WaitExpired && w.Decision.Kind == runtime.DecisionExpired && w.Decision.Outcome == runtime.WaitOutcomeExpired, "the wait: %+v", w)
		e.expect(stepOf(got, "review").State == runtime.StepSucceeded, "on_expiry continue settles the step succeeded")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded, "run: %+v", got.Run)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build late]", "only the step that tests the expiry ran: %v", e.rt.startOrder)
		e.expect(stepOf(got, "ship").Reason == runtime.ReasonConditionFalse && stepOf(got, "integrate").Reason == runtime.ReasonRequiresUnmet,
			"approval is not implied by expiry: %v", states(got))
		late := e.decide(signal("run1", "k1", "approved"))
		e.expect(!late.Applied && late.Refusal == runtime.RefusalDecided && late.Decision.Kind == runtime.DecisionExpired, "a signal after expiry is refused: %+v", late)
		e.expect(countKind(e.outbox("run1"), runtime.EventWaitExpired) == 1, "one wait.expired event")
	})

	t.Run("a deadline with the default branch fails the step and the run", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", "")
		e.clock.Advance(2 * time.Hour)
		e.drive(e.engine("w2"), "run1")

		got := e.load("run1")
		r := stepOf(got, "review")
		e.expect(r.State == runtime.StepFailed && r.Reason == runtime.ReasonWaitExpired, "review: %+v", r)
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonWaitExpired, "run: %+v", got.Run)
		e.expect(reviewWaitOf(got).Decision.Outcome == "" && stepOf(got, "ship").Reason == runtime.ReasonRunFailed, "no outcome was recorded; unstarted steps are skipped")
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build]", "nothing ran after the failure: %v", e.rt.startOrder)
	})

	t.Run("resolution at the deadline loses to expiry and a resolution before it wins the race", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", runtime.ExpiryContinue)
		deadline := reviewWaitOf(e.load("run1")).Deadline
		e.clock.Set(deadline)
		res := e.decide(signal("run1", "k1", "approved"))
		e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionExpired,
			"a resolution at the deadline loses: %+v", res)
		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(fmt.Sprint(e.rt.startOrder) == "[build late]" && got.Run.Outcome == runtime.OutcomeSucceeded, "one continuation, the expiry branch: %v", e.rt.startOrder)

		// The engine's expiry and a signal race on the same version: the signal
		// committed first, so the engine's expiry is refused and it consumes the
		// resolution instead.
		e2 := newEnv(t)
		e2.park("run1", runtime.ExpiryContinue)
		deadline = reviewWaitOf(e2.load("run1")).Deadline
		e2.clock.Set(deadline)
		raced := false
		store := &raceStore{StateStore: e2.state, before: func(tr runtime.Transition) {
			if raced || len(tr.Waits) == 0 || tr.Waits[0].State != runtime.WaitExpired {
				return
			}
			raced = true
			res, err := e2.state.DecideSignal(context.Background(), signal("run1", "k1", "approved"), deadline.Add(-time.Second))
			e2.expect(err == nil && res.Applied, "the signal won the race: %+v %v", res, err)
		}}
		e2.expire()
		eng := e2.engine("w2", func(c *runner.EngineConfig) { c.State = store })
		e2.drive(eng, "run1")
		got = e2.load("run1")
		w := reviewWaitOf(got)
		e2.expect(raced && w.State == runtime.WaitResolved && w.Decision.Outcome == "approved", "the first commit won: %+v", w)
		e2.expect(fmt.Sprint(e2.rt.startOrder) == "[build ship integrate]" && got.Run.Outcome == runtime.OutcomeSucceeded,
			"exactly one continuation, the resolved one: %v", e2.rt.startOrder)
		e2.expect(countKind(e2.outbox("run1"), runtime.EventWaitExpired) == 0, "no expiry event for a wait that was resolved")
	})

	t.Run("a missing correlation value fails the step and opens no wait", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", reviewPlan(""), 3)
		e.outputs.produce("build", map[string]string{"question": "why?"})
		e.drive(e.engine("w1"), "run1")
		got := e.load("run1")
		r := stepOf(got, "review")
		e.expect(r.State == runtime.StepFailed && r.Reason == runtime.ReasonCorrelationMissing, "review: %+v", r)
		e.expect(reviewWaitOf(got).State == runtime.WaitPending && got.Run.Reason == runtime.ReasonCorrelationMissing, "no wait opened: %+v", reviewWaitOf(got))
		e.expect(countKind(e.outbox("run1"), runtime.EventWaitOpened) == 0, "nothing was announced")
	})

	t.Run("a recorded question opens the wait with a request, none skips it, a failed step opens nothing", func(t *testing.T) {
		mk := func() runtime.Plan {
			build := ctr("build")
			build.Outputs = []string{"question"}
			clarify := runtime.PlanStep{Key: "clarify", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"},
				When: `build.outputs.question != ""`, Wait: &runtime.WaitSpec{Assign: "engineer", Request: "build.outputs.question",
					Input: "answer", Outcomes: []string{"answered"}, Timeout: time.Hour}}
			return plan("clarify", build, clarify, ctr("next", "clarify"))
		}
		e := newEnv(t)
		e.admit("asks", mk(), 3)
		e.outputs.produce("build", map[string]string{"question": "which branch?"})
		rep := e.drive(e.engine("w1"), "asks")
		got := e.load("asks")
		var w runtime.Wait
		for _, x := range got.Waits {
			w = x
		}
		e.expect(rep.Status == runner.DriveWaiting && w.State == runtime.WaitOpen && w.Request == "which branch?", "the wait carries the request: %+v", w)

		e = newEnv(t)
		e.admit("quiet", mk(), 3)
		e.drive(e.engine("w1"), "quiet")
		got = e.load("quiet")
		c := stepOf(got, "clarify")
		e.expect(c.State == runtime.StepSkipped && c.Reason == runtime.ReasonConditionFalse && c.Predicate.Evaluated && !c.Predicate.Result, "a missing output is false: %+v", c)
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && stepOf(got, "next").Reason == runtime.ReasonPrerequisiteSkipped,
			"the dependent of a skipped wait skips under join all: %v", states(got))

		e = newEnv(t)
		e.admit("fails", mk(), 3)
		e.outputs.produce("build", map[string]string{"question": "ignored"})
		e.rt.scripts["build"] = script{result: runtime.Result{Code: runtime.ResultFailed, ExitCode: 1}}
		e.drive(e.engine("w1"), "fails")
		got = e.load("fails")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && len(e.outputs.collect) == 0 && reviewlessWaits(got) == 0, "a failed step's outputs are never recorded and open nothing")
	})

	t.Run("a cancellation and a signal race on one decision", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", "")
		if _, err := e.state.RequestCancel(context.Background(), scope, "run1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		res := e.decide(signal("run1", "k1", "approved"))
		e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionCancelled, "the signal lost to the cancellation: %+v", res)
		e.expire()
		rep, err := e.engine("w2").Drive(context.Background(), e.ref("run1", runtime.ClaimReconcile))
		e.expect(err == nil && rep.Run.Outcome == runtime.OutcomeCancelled, "the run settled cancelled: %v %+v", err, rep.Run)
		got := e.load("run1")
		e.expect(stepOf(got, "review").State == runtime.StepCancelled && fmt.Sprint(e.rt.startOrder) == "[build]", "nothing continued: %v", states(got))

		// A decision that was committed before the cancellation stands: the
		// cancellation does not rewrite it and no continuation starts.
		e = newEnv(t)
		e.park("run1", "")
		e.decide(signal("run1", "k1", "approved"))
		if _, err := e.state.RequestCancel(context.Background(), scope, "run1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		e.expire()
		rep, err = e.engine("w2").Drive(context.Background(), e.ref("run1", runtime.ClaimReconcile))
		got = e.load("run1")
		e.expect(err == nil && got.Run.Outcome == runtime.OutcomeCancelled && reviewWaitOf(got).State == runtime.WaitResolved, "the resolution stands: %+v", reviewWaitOf(got))
		e.expect(stepOf(got, "review").State == runtime.StepSucceeded && fmt.Sprint(e.rt.startOrder) == "[build]", "but nothing continued: %v", e.rt.startOrder)
	})

	t.Run("losing the workspace during a wait fails the run and never replaces it", func(t *testing.T) {
		e := newEnv(t)
		e.park("run1", "")
		e.res.lose("workspace")
		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonWorkspaceLost, "run: %+v", got.Run)
		w := reviewWaitOf(got)
		e.expect(w.State == runtime.WaitCancelled && w.Decision.Reason == runtime.ReasonRunFailed && stepOf(got, "review").State == runtime.StepCancelled,
			"the open wait was decided cancelled: %+v", w)
		e.expect(len(e.res.ensures) == 1, "no replacement workspace: %v", e.res.ensures)
	})

	t.Run("joins and when over a skipped prerequisite follow the ordered eligibility rules", func(t *testing.T) {
		e := newEnv(t)
		a := ctr("a")
		a.When = "inputs.go"
		a.Outputs = []string{"x"}
		c := ctr("c", "a", "b")
		c.Join = runtime.JoinAny
		c.When = `a.status == "skipped" && b.status == "succeeded" && !(a.outputs.x == "v") && inputs.limit < 5`
		p := plan("joins", a, ctr("b"), c, ctr("d", "a", "b"))
		p.Inputs = map[string]runtime.InputValue{"go": runtime.BooleanInput(false), "limit": runtime.IntegerInput(3)}
		e.admit("run1", p, 3)
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(fmt.Sprint(states(got)) == "map[a:skipped b:succeeded c:succeeded d:skipped]", "states: %v", states(got))
		e.expect(stepOf(got, "a").Reason == runtime.ReasonConditionFalse && stepOf(got, "d").Reason == runtime.ReasonPrerequisiteSkipped,
			"join all skips on a skipped prerequisite, without evaluating when: %+v", stepOf(got, "d"))
		e.expect(!stepOf(got, "d").Predicate.Evaluated && stepOf(got, "c").Predicate.Evaluated && stepOf(got, "c").Predicate.Result, "join any ran c and recorded its predicate")
		e.expect(fmt.Sprint(e.rt.startOrder) == "[b c]" && got.Run.Outcome == runtime.OutcomeSucceeded, "a run with skipped branches succeeds: %v", e.rt.startOrder)
	})

	t.Run("a recorded predicate is reused after a restart even if the value it read changed", func(t *testing.T) {
		e := newEnv(t)
		build := ctr("build")
		build.Outputs = []string{"mode"}
		deploy := ctr("deploy", "build")
		deploy.When = `build.outputs.mode == "fast"`
		e.admit("run1", plan("reuse", build, deploy), 3)
		e.outputs.produce("build", map[string]string{"mode": "fast"})
		e.rt.scripts["deploy"] = script{polls: 4}
		sleeps := 0
		e.onSleep = func() error {
			if sleeps++; sleeps == 2 {
				return errCrash
			}
			return nil
		}
		_, err := e.engine("w1").Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
		e.expect(errors.Is(err, errCrash), "crash: %v", err)
		before := stepOf(e.load("run1"), "deploy")
		e.expect(before.State == runtime.StepRunning && before.Predicate.Evaluated && before.Predicate.Result, "deploy started on a true predicate: %+v", before)

		e.outputs.produce("build", map[string]string{"mode": "slow"})
		e.onSleep = nil
		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		after := stepOf(got, "deploy")
		e.expect(after.State == runtime.StepSucceeded && after.Predicate == before.Predicate, "the evaluation was not repeated: %+v vs %+v", after.Predicate, before.Predicate)
		digest, _ := runtime.DigestPredicateReads([]runtime.PredicateRead{{Ref: "build.outputs.mode", Type: runtime.InputString, Present: true, Value: "fast"}})
		e.expect(after.Predicate.InputDigest == digest, "the digest names what was read: %s vs %s", after.Predicate.InputDigest, digest)
		e.expect(e.rt.count(e.rt.starts) == 2, "deploy was launched once: %v", e.rt.starts)
	})

	t.Run("an outputs read that fails leaves the run for a later claim instead of failing it", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", reviewPlan(""), 3)
		e.outputs.produce("build", map[string]string{"commit": "c1"})
		bad := &flakyOutputs{fakeOutputs: e.outputs, failReads: 1}
		eng := e.engine("w1", func(c *runner.EngineConfig) { c.Outputs = bad })
		rep := e.drive(eng, "run1")
		e.expect(rep.Status == runner.DriveRetry && stepOf(e.load("run1"), "review").State == runtime.StepPending, "status %s", rep.Status)
		e.expire()
		rep = e.drive(eng, "run1")
		e.expect(rep.Status == runner.DriveWaiting, "the next claim opens the wait: %s", rep.Status)
	})

	t.Run("a decided wait does not report success over lost state", lostStateBlocksResume)
	t.Run("a failing outputs port is not a verdict on a condition or binding", transientReadsLeaveStepsPending)

	t.Run("process backend", processWaits)
}

func reviewlessWaits(rec runtime.RunRecord) int {
	n := 0
	for _, w := range rec.Waits {
		if w.State == runtime.WaitOpen {
			n++
		}
	}
	return n
}

// raceStore lets a test run another writer immediately before the engine's commit.
type raceStore struct {
	runtime.StateStore
	before func(runtime.Transition)
}

func (r *raceStore) Commit(ctx context.Context, t runtime.Transition) (runtime.RunRecord, error) {
	r.before(t)
	return r.StateStore.Commit(ctx, t)
}

type flakyOutputs struct {
	*fakeOutputs
	failReads int
}

func (f *flakyOutputs) Read(ctx context.Context, ref runner.StepRef) (map[string]string, error) {
	if f.failReads > 0 {
		f.failReads--
		return nil, errors.New("store unavailable")
	}
	return f.fakeOutputs.Read(ctx, ref)
}

// Test_unit_kernel_engine_conditions exercises the frozen when grammar directly.
func Test_unit_kernel_engine_conditions(t *testing.T) {
	read := func(vals map[string]runtime.InputValue) runner.ReferenceReader {
		return func(ref string) (runtime.InputValue, bool, error) {
			if v, ok := vals[ref]; ok {
				return v, true, nil
			}
			switch {
			case ref == "inputs.nope":
				return runtime.BooleanInput(false), false, nil
			case strings.HasPrefix(ref, "inputs.n"):
				return runtime.IntegerInput(0), false, nil
			}
			return runtime.StringInput(""), false, nil
		}
	}
	vals := map[string]runtime.InputValue{
		"inputs.n": runtime.IntegerInput(3), "inputs.s": runtime.StringInput("1"), "inputs.flag": runtime.BooleanInput(true),
		"a.status": runtime.StringInput("succeeded"), "r.outcome": runtime.StringInput("approved"), "b.status": runtime.StringInput("skipped"),
	}
	eval := func(src string) (bool, error) {
		c, err := runner.ParseCondition(src)
		if err != nil {
			return false, err
		}
		got, _, err := c.Evaluate(read(vals))
		return got, err
	}
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{`r.outcome == "approved"`, true},
		{`r.outcome != "approved"`, false},
		{`r.outcome in ["rejected", "approved"]`, true},
		{`inputs.n >= 3 && inputs.n < 4`, true},
		{`inputs.n < -1 || a.status == "succeeded"`, true},
		{`inputs.flag`, true},
		{`!inputs.flag`, false},
		{`inputs.s == "1"`, true},
		{`b.status == "skipped"`, true},
		{`(a.status == "succeeded" || a.status == "failed") && !(inputs.n == 4)`, true},
		// A missing value makes every comparison false, != and in included; ! then negates.
		{`b.outputs.x == "v"`, false},
		{`b.outputs.x != "v"`, false},
		{`b.outputs.x in ["v"]`, false},
		{`!(b.outputs.x == "v")`, true},
		{`b.outputs.x != ""`, false},
		{`inputs.nope`, false},
	} {
		got, err := eval(tc.src)
		if err != nil || got != tc.want {
			t.Errorf("%s = %v, %v; want %v", tc.src, got, err, tc.want)
		}
	}

	// No coercion: operands have the same type; ordering is for integers only; a
	// bare operand is boolean.
	for _, src := range []string{
		`inputs.s == 1`, `inputs.n == "3"`, `inputs.flag == "true"`, `r.outcome < "b"`, `inputs.flag < true`,
		`r.outcome`, `inputs.n`, `inputs.n in ["3"]`, `1 == true`,
	} {
		if _, err := eval(src); !errors.Is(err, runner.ErrCondition) {
			t.Errorf("%s: got %v, want ErrCondition", src, err)
		}
	}

	// Syntax and bounds.
	long := strings.Repeat("a", 65)
	ok := []string{
		`inputs.n == 1 && inputs.n == 2 && inputs.n == 3 && inputs.n == 4 && inputs.n == 5 && inputs.n == 6 && inputs.n == 7 && inputs.n == 8`,
		`!(!(inputs.flag))`,
		`r.outcome == "` + strings.Repeat("a", 64) + `"`,
		`r.outcome in ["1","2","3","4","5","6","7","8"]`,
		`inputs.n == 9223372036854775807 || inputs.n == -9223372036854775808`,
	}
	for _, src := range ok {
		if _, err := runner.ParseCondition(src); err != nil {
			t.Errorf("%q should parse at the bound: %v", src, err)
		}
	}
	bad := []string{
		``, `inputs.n ==`, `(inputs.flag`, `inputs.flag)`, `r.outcome = "a"`, `r.outcome == 'a'`, `inputs.n == 1 &`, `foo`, `a.b.c.d == "x"`,
		`inputs.n == 1 && inputs.n == 2 && inputs.n == 3 && inputs.n == 4 && inputs.n == 5 && inputs.n == 6 && inputs.n == 7 && inputs.n == 8 && inputs.n == 9`,
		`!(!(!(!(!inputs.flag))))`,
		`((((( inputs.flag )))))`,
		`r.outcome == "` + long + `"`,
		`r.outcome in ["1","2","3","4","5","6","7","8","9"]`,
		`inputs.n == 9223372036854775808`,
		`r.outcome in []`, `r.outcome in ["a" "b"]`, `a.nope == "x"`, `inputs.Bad == 1`,
	}
	for _, src := range bad {
		if _, err := runner.ParseCondition(src); !errors.Is(err, runner.ErrCondition) {
			t.Errorf("%q: got %v, want ErrCondition", src, err)
		}
	}

	// The digest depends on what was read, not on the order, and on the type.
	c, _ := runner.ParseCondition(`inputs.s == "1" && inputs.n == 1`)
	_, reads, err := c.Evaluate(read(vals))
	if err != nil || len(reads) != 2 || reads[0].Ref != "inputs.n" {
		t.Fatalf("reads: %+v %v", reads, err)
	}
	if got := c.References(); fmt.Sprint(got) != "[inputs.n inputs.s]" {
		t.Errorf("references: %v", got)
	}
}
