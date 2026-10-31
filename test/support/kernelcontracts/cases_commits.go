package kernelcontracts

import (
	"errors"
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func commitCases() []Case {
	return []Case{
		{"commit/applies_state_with_its_outbox_in_order", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			a := e.event(scopeA, run1, "build", runtime.EventRunStateChanged, 2)
			b := e.event(scopeA, run1, "build", runtime.EventStepStateChanged, 2)
			got := e.commit(e.transition(rec, func(r *runtime.Run) error {
				return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone)
			}, a, b))
			e.expect(got.Run.Version == 2 && got.Run.Steps[0].State == runtime.StepRunning && got.Run.Steps[0].Attempt == 1, "the step started: %+v", got.Run.Steps[0])
			e.expect(reflect.DeepEqual(got, e.load(scopeA, run1)), "Commit returns the stored record")
			events := e.outbox(scopeA, run1)
			e.expect(len(events) == 3 && events[0].Kind == runtime.EventRunCreated && events[1].ID == a.ID && events[2].ID == b.ID,
				"the outbox holds the admission event then the transition's events in order: %+v", events)
		}},
		{"commit/optimistic_version_check", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			v1 := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			v2 := e.startBuild(v1)
			n := len(e.outbox(scopeA, run1))
			// A writer that read version 1 and decides after another writer moved on.
			stale := e.transition(v1, func(r *runtime.Run) error {
				return r.AdvanceStep("build", runtime.StepCancelled, runtime.ReasonCancelled)
			}, e.event(scopeA, run1, "build", runtime.EventStepStateChanged, 2))
			_, err := e.S.Commit(e.Ctx, stale)
			e.versionConflict(err, "a transition built on version 1 against version 2")
			e.unchanged(v2, n, "the stale-version transition")

			skip := e.bump(v2)
			skip.Run.Version = v2.Run.Version + 2
			_, err = e.S.Commit(e.Ctx, skip)
			e.isErr(err, domain.ErrInvalidValue, "a run that skips a version")
			e.unchanged(v2, n, "the skipping transition")
		}},
		{"commit/duplicate_transition_applies_once", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			tr := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) },
				e.event(scopeA, run1, "build", runtime.EventStepStateChanged, 2))
			done := e.commit(tr)
			_, err := e.S.Commit(e.Ctx, tr)
			e.versionConflict(err, "the same transition committed twice")
			e.unchanged(done, 2, "the duplicate transition")
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventStepStateChanged) == 1, "the event is in the outbox once")
		}},
		{"commit/rollback_discards_state_and_outbox_together", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			ev := e.event(scopeA, run1, "build", runtime.EventStepStateChanged, 2)
			tr := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) }, ev)
			launch := e.launchCommand(rec, 1)
			tr.Commands = []runtime.Command{launch}

			boom := errors.New("injected commit failure")
			e.S.FailNextCommit(boom)
			t.Cleanup(func() { e.S.FailNextCommit(nil) })
			_, err := e.S.Commit(e.Ctx, tr)
			e.isErr(err, boom, "a commit failing at its commit point")
			e.unchanged(rec, 1, "the failed commit")

			// The failure consumed nothing: the same transition now commits, once.
			got := e.commit(tr)
			e.expect(got.Run.Version == 2 && len(got.Commands) == 1, "the retried transition commits")
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventStepStateChanged) == 1, "the event reaches the outbox once")
		}},
		{"commit/renewal_rides_with_the_commit", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			e.Clock.Advance(30 * time.Second)
			tr := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
			tr.RenewTTL = ttl
			got := e.commit(tr)
			e.expect(got.Claim.Fence == 1 && got.Claim.Owner == "w1" && got.Claim.Expires.Equal(t0.Add(90*time.Second)), "the lease is extended with the commit: %+v", got.Claim)
			e.expect(reflect.DeepEqual(got.Claim, e.load(scopeA, run1).Claim), "the extended lease is stored")
			tr2 := e.bump(got)
			tr2.RenewTTL = time.Second
			got2 := e.commit(tr2)
			e.expect(got2.Claim.Expires.Equal(got.Claim.Expires), "a commit never shortens the lease")
		}},
		{"commit/recorded_history_is_immutable", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, solePlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			n := len(e.outbox(scopeA, run1))
			// A terminal step is final: rewrite it by hand, as a buggy writer would.
			r := rec.Run.Clone()
			r.Steps[0].State, r.Steps[0].Reason, r.Steps[0].Result = runtime.StepFailed, runtime.ReasonCommandFailed, runtime.Result{Code: runtime.ResultFailed, ExitCode: 1}
			r.Steps[0].Version++
			r.Version++
			tr := e.bump(rec)
			tr.Run = r
			_, err := e.S.Commit(e.Ctx, tr)
			e.isErr(err, runtime.ErrIllegalTransition, "rewriting a settled step")
			e.unchanged(rec, n, "the rewrite")
		}},
		{"commit/wait_updates_are_version_checked", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			pending := rec.Waits[e.waitIndex(rec)]
			opened := e.openReview(rec)
			n := len(e.outbox(scopeA, run1))
			w := e.review(opened)
			e.expect(w.State == runtime.WaitOpen && w.Version == pending.Version+1 && w.Correlation["candidate"] == "c1", "the wait opened: %+v", w)

			// A writer carrying the wait it read before it opened.
			stale := e.bump(opened)
			stale.Waits = []runtime.Wait{pending}
			_, err := e.S.Commit(e.Ctx, stale)
			e.versionConflict(err, "a stale wait")
			// A wait that changes without a new version.
			quiet := w
			quiet.Request = "rewritten"
			tr := e.bump(opened)
			tr.Waits = []runtime.Wait{quiet}
			_, err = e.S.Commit(e.Ctx, tr)
			e.versionConflict(err, "a wait changed without a new version")
			e.unchanged(opened, n, "the refused wait updates")
		}},
		{"commit/wait_opens_only_with_its_step", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			n := len(e.outbox(scopeA, run1))
			// Open the wait while the run merely advances its version: the step stays pending.
			w := rec.Waits[e.waitIndex(rec)]
			e.ok(w.Open(e.Clock.Now(), map[string]string{"candidate": "c1"}, "please review"), "open the wait")
			tr := e.bump(rec)
			tr.Waits = []runtime.Wait{w}
			_, err := e.S.Commit(e.Ctx, tr)
			e.isErr(err, runtime.ErrIllegalTransition, "a wait opened without its step entering waiting")
			e.unchanged(rec, n, "the early open")
			res := e.decide(byWait(scopeA, "alice", "k1", w.ID, "approved"))
			e.expect(!res.Applied && res.Refusal == runtime.RefusalUnknownWait, "a wait that never opened cannot be resolved: %+v", res)

			// Opened with its step, the wait is decided asynchronously: the decision may precede
			// the engine's consumption of it in the step.
			opened := e.openReview(rec)
			won := e.decide(byWait(scopeA, "alice", "k2", w.ID, "approved"))
			e.expect(won.Applied, "the wait opened with its step is decidable: %+v", won)
			after := e.load(scopeA, run1)
			e.expect(after.Run.Steps[e.stepIndex(after, "review")].State == runtime.StepWaiting && after.Run.Version == opened.Run.Version,
				"the decision leaves the step for the engine")
			e.step(after, func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone) })
		}},
		{"commit/step_and_wait_must_agree", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			n := len(e.outbox(scopeA, run1))
			// Parking the step without opening its wait is incoherent.
			tr := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) })
			_, err := e.S.Commit(e.Ctx, tr)
			e.expect(err != nil, "a waiting step whose wait is still pending must be refused")
			e.unchanged(rec, n, "the incoherent move")
		}},
		{"commit/command_intent_and_observations_are_retained", GroupCommits, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			launch := e.launchCommand(rec, 1)
			tr := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
			tr.Commands = []runtime.Command{launch}
			rec = e.commit(tr)
			e.expect(len(rec.Unresolved()) == 1 && rec.Unresolved()[0].ID == launch.ID, "an intent with no outcome is unresolved")

			// A command carries its writer's fence: another fence is not this writer's.
			foreign := launch
			foreign.ID, foreign.Fence, foreign.Binding.Fence = "cmd-other", rec.Claim.Fence+1, rec.Claim.Fence+1
			bad := e.bump(rec)
			bad.Commands = []runtime.Command{foreign}
			_, err := e.S.Commit(e.Ctx, bad)
			e.isErr(err, runtime.ErrStaleFence, "a command stamped with a fence the writer does not hold")

			settled := launch
			e.ok(settled.Settle(runtime.CommandConfirmed, runtime.ReasonNone), "confirm the launch")
			obs := runtime.WorkloadObservation{Binding: launch.Binding, Phase: runtime.PhaseRunning, ObservedAt: e.Clock.Now()}
			ok := e.bump(rec)
			ok.Commands = []runtime.Command{settled}
			ok.Workloads = []runtime.WorkloadObservation{obs}
			rec = e.commit(ok)
			e.expect(len(rec.Unresolved()) == 0 && len(rec.Workloads) == 1 && rec.Commands[0].State == runtime.CommandConfirmed,
				"the confirmed launch and the observation are retained: %+v", rec.Commands)

			// confirmed is final.
			back := settled
			back.State, back.Reason = runtime.CommandRejected, runtime.ReasonLaunchNotReached
			final := e.bump(rec)
			final.Commands = []runtime.Command{back}
			_, err = e.S.Commit(e.Ctx, final)
			e.isErr(err, runtime.ErrIllegalTransition, "reopening a confirmed command")
			e.unchanged(rec, len(e.outbox(scopeA, run1)), "the refused command rewrite")
		}},
	}
}

// launchCommand is the intent to launch attempt n of build under rec's claim.
func (e *Env) launchCommand(rec runtime.RunRecord, attempt int) runtime.Command {
	e.T.Helper()
	b := runtime.RuntimeBinding{Scope: rec.Run.Scope, RunID: rec.Run.ID, StepKey: "build", Attempt: attempt,
		WorkloadID: "wl-" + string(rec.Run.ID) + "-build", SpecDigest: "sha256:spec", Backend: "process", Fence: rec.Claim.Fence}
	c, err := runtime.NewWorkloadCommand("cmd-launch-"+string(rec.Run.ID), b, runtime.CommandLaunch, e.Clock.Now())
	e.ok(err, "build the launch command")
	return c
}
