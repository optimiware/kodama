package kernelcontracts

import (
	"errors"
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func cancelCases() []Case {
	return []Case{
		{"cancel/blocks_execution_but_allows_reconcile_and_cleanup", GroupCancel, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			run, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "request cancellation without a claim")
			e.expect(run.CancelRequested && run.Version == 2 && !run.Settled(), "cancellation is recorded and does not settle: %+v", run)

			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrExecutionClosed, "an execution claim on a cancelling run")
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimReconcile, ttl)
			e.expect(rec.Claim.Purpose == runtime.ClaimReconcile && rec.Claim.Fence == 1, "a reconciliation claim is admitted: %+v", rec.Claim)
			e.Clock.Advance(ttl)
			rec = e.claim(scopeA, run1, "w2", runtime.ClaimCleanup, ttl)
			e.expect(rec.Claim.Purpose == runtime.ClaimCleanup && rec.Claim.Fence == 2, "a cleanup claim is admitted: %+v", rec.Claim)
			e.expect(e.load(scopeA, run1).Run.CancelRequested, "the cancellation stands")
		}},
		{"cancel/is_idempotent", GroupCancel, func(t TB, e *Env) {
			waiting := e.waitingRun(scopeA, run1)
			first, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel")
			n := len(e.outbox(scopeA, run1))
			again, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel again")
			e.expect(reflect.DeepEqual(first, again) && first.Version == waiting.Run.Version+1, "a repeat returns the same run: %d vs %d", first.Version, again.Version)
			e.expect(len(e.outbox(scopeA, run1)) == n, "a repeat writes no event")
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventWaitCancelled) == 1, "the wait is cancelled once")
		}},
		{"cancel/decides_pending_and_open_waits_with_the_request", GroupCancel, func(t TB, e *Env) {
			open := e.waitingRun(scopeA, run1)
			before := e.review(open)
			e.admit(scopeA, run2, reviewPlan())
			e.Clock.Advance(time.Minute)
			cancelAt := e.Clock.Now()

			_, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel the run with an open wait")
			w := e.review(e.load(scopeA, run1))
			e.expect(w.State == runtime.WaitCancelled && w.Version == before.Version+1, "the open wait is cancelled: %+v", w)
			e.expect(w.Decision.Kind == runtime.DecisionCancelled && w.Decision.Reason == runtime.ReasonRunCancelled &&
				w.Decision.Version == w.Version && w.Decision.At.Equal(cancelAt), "the decision records the cancellation: %+v", w.Decision)
			e.expect(w.Correlation["candidate"] == "c1", "an opened wait keeps what it read")

			_, err = e.S.RequestCancel(e.Ctx, scopeA, run2)
			e.ok(err, "cancel the run with a pending wait")
			p := e.review(e.load(scopeA, run2))
			e.expect(p.State == runtime.WaitCancelled && len(p.Correlation) == 0 && p.OpenedAt.IsZero(), "a wait cancelled before it opened has no values: %+v", p)
			for _, id := range []domain.RunID{run1, run2} {
				events := e.outbox(scopeA, id)
				e.expect(countKind(events, runtime.EventWaitCancelled) == 1, "run %s has one wait.cancelled event", id)
			}
			// The wait stays decided: a signal for it is refused with the recorded cancellation.
			res := e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionCancelled,
				"a signal for a cancelled wait is refused as decided with the cancellation: %+v", res)
		}},
		{"cancel/needs_no_claim_and_does_not_wait_for_the_lease", GroupCancel, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			held := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, longTTL)
			run, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel under another owner's active lease")
			e.expect(run.CancelRequested, "the cancellation is recorded")
			after := e.load(scopeA, run1)
			e.expect(after.Claim == held.Claim, "the lease is untouched by the cancellation")

			// The holder wrote from the version it read: it must reload and see the cancellation.
			stale := e.transition(held, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
			_, err = e.S.Commit(e.Ctx, stale)
			e.versionConflict(err, "a writer that read before the cancellation")
		}},
		{"cancel/refuses_new_steps_even_to_a_current_writer", GroupCancel, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			held := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, longTTL)
			_, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel")
			rec := e.load(scopeA, run1)
			// A buggy writer that read the cancellation anyway starts a step by hand.
			r := rec.Run.Clone()
			r.Steps[0].State, r.Steps[0].Attempt = runtime.StepRunning, 1
			r.Steps[0].Version++
			r.Version++
			tr := e.bump(rec)
			tr.Run, tr.Claim = r, held.Claim
			n := len(e.outbox(scopeA, run1))
			_, err = e.S.Commit(e.Ctx, tr)
			e.isErr(err, runtime.ErrExecutionClosed, "starting a step after cancellation")
			e.unchanged(rec, n, "the refused start")
		}},
		{"cancel/rolls_back_with_its_wait_decisions", GroupCancel, func(t TB, e *Env) {
			rec := e.waitingRun(scopeA, run1)
			n := len(e.outbox(scopeA, run1))
			boom := errors.New("injected commit failure")
			e.S.FailNextCommit(boom)
			t.Cleanup(func() { e.S.FailNextCommit(nil) })
			_, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.isErr(err, boom, "a cancellation failing at commit")
			e.unchanged(rec, n, "the failed cancellation")
			_, err = e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "the retry")
			e.expect(e.review(e.load(scopeA, run1)).State == runtime.WaitCancelled, "the retry cancels the wait")
		}},
		{"cancel/after_settlement_changes_nothing", GroupCancel, func(t TB, e *Env) {
			e.admit(scopeA, run1, solePlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
			n := len(e.outbox(scopeA, run1))
			run, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel a settled run")
			e.expect(!run.CancelRequested && run.Outcome == runtime.OutcomeSucceeded && run.Version == rec.Run.Version, "a settled run is not cancelled: %+v", run)
			e.unchanged(rec, n, "cancelling a settled run")

			// A settled run may retain a pending wait for a step that was skipped: it stays as it was.
			e.admit(scopeA, run2, reviewPlan())
			rec = e.claim(scopeA, run2, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.step(rec, func(r *runtime.Run) error {
				return r.FinishStep("build", runtime.StepFailed, runtime.ReasonCommandFailed, runtime.Result{Code: runtime.ResultFailed, ExitCode: 1})
			})
			rec = e.step(rec, func(r *runtime.Run) error {
				for _, k := range []domain.StepID{"review", "ship"} {
					if err := r.AdvanceStep(k, runtime.StepSkipped, runtime.ReasonDependencyFailed); err != nil {
						return err
					}
				}
				return nil
			})
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeFailed, runtime.ReasonCommandFailed) })
			e.expect(e.review(rec).State == runtime.WaitPending && rec.Run.Settled(), "the settled run retains a pending wait: %+v", e.review(rec))
			n = len(e.outbox(scopeA, run2))
			run, err = e.S.RequestCancel(e.Ctx, scopeA, run2)
			e.ok(err, "cancel a settled run with a pending wait")
			e.expect(!run.CancelRequested && run.Version == rec.Run.Version, "a settled run is not cancelled: %+v", run)
			e.unchanged(rec, n, "cancelling a settled run that retains a pending wait")
			e.expect(countKind(e.outbox(scopeA, run2), runtime.EventWaitCancelled) == 0, "no wait.cancelled event on a settled run")
		}},
	}
}

func cleanupCases() []Case {
	return []Case{
		{"cleanup/exhaustion_then_takeover_confirms_what_remains", GroupCleanup, func(t TB, e *Env) {
			e.admit(scopeA, run1, solePlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
			e.expect(rec.Run.Status() == runtime.StatusCleaning && !rec.Run.Terminal(), "a settled run is cleaning, not terminal")

			// A cleanup claim takes over from the executor and makes two unfinished attempts.
			e.Clock.Advance(ttl)
			rec = e.claim(scopeA, run1, "w2", runtime.ClaimCleanup, ttl)
			db, cache := handle(scopeA, run1, "db"), handle(scopeA, run1, "cache")
			attempt := func(rec runtime.RunRecord) runtime.RunRecord {
				return e.step(rec, func(r *runtime.Run) error {
					return r.RecordCleanupRemaining(runtime.Remaining{Resources: []runtime.ResourceHandle{db, cache}}, runtime.ReasonCleanupFailed)
				})
			}
			rec = attempt(rec)
			e.expect(rec.Run.Cleanup.State == runtime.CleanupPending && rec.Run.Cleanup.Attempts == 1, "one attempt is not exhaustion: %+v", rec.Run.Cleanup)
			rec = attempt(rec)
			c := rec.Run.Cleanup
			e.expect(c.State == runtime.CleanupExhausted && c.Attempts == 2 && len(c.Retained) == 2 && rec.Run.Terminal(), "the bound ends cleanup with what it retained: %+v", c)
			e.expect(e.load(scopeA, run1).Run.Status() == runtime.StatusSucceeded, "execution outcome and cleanup are separate: the run is terminal")

			// Execution and general reconciliation stay closed; only cleanup is admitted.
			for _, p := range []runtime.ClaimPurpose{runtime.ClaimExecute, runtime.ClaimReconcile} {
				_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w9", p, ttl), e.Clock.Now())
				e.isErr(err, runtime.ErrExecutionClosed, "a "+string(p)+" claim on a terminal run")
			}
			_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w3", runtime.ClaimCleanup, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "a cleanup claim under w2's active lease")

			// Takeover after the lease: the previous cleanup owner is fenced out.
			e.Clock.Advance(ttl)
			w3 := e.claim(scopeA, run1, "w3", runtime.ClaimCleanup, ttl)
			e.expect(w3.Claim.Fence == 3 && w3.Claim.Purpose == runtime.ClaimCleanup, "takeover: %+v", w3.Claim)
			n := len(e.outbox(scopeA, run1))
			stale := e.transition(rec, func(r *runtime.Run) error {
				return r.ReconcileExhausted(runtime.Remaining{Resources: []runtime.ResourceHandle{cache}})
			})
			_, err = e.S.Commit(e.Ctx, stale)
			e.isErr(err, runtime.ErrStaleFence, "the previous cleanup owner")
			e.unchanged(w3, n, "the fenced-out cleanup writer")

			// The new owner confirms part of what remained, then the rest.
			rec = e.step(w3, func(r *runtime.Run) error {
				return r.ReconcileExhausted(runtime.Remaining{Resources: []runtime.ResourceHandle{cache}})
			})
			c = rec.Run.Cleanup
			e.expect(c.State == runtime.CleanupExhausted && len(c.Retained) == 1 && c.Retained[0] == cache && rec.Run.Terminal(),
				"a partial confirmation narrows what is retained: %+v", c)
			rec = e.step(rec, func(r *runtime.Run) error { return r.ReconcileExhausted(runtime.Remaining{}) })
			c = rec.Run.Cleanup
			e.expect(c.State == runtime.CleanupComplete && len(c.Retained) == 0 && c.Reason == runtime.ReasonNone, "the late reconciliation completes cleanup: %+v", c)
			e.expect(reflect.DeepEqual(rec, e.load(scopeA, run1)) && rec.Run.Outcome == runtime.OutcomeSucceeded, "the stored run is complete and keeps its outcome")

			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w4", runtime.ClaimCleanup, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrExecutionClosed, "a cleanup claim once cleanup is complete")
		}},
	}
}
