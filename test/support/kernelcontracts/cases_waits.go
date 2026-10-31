package kernelcontracts

import (
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// sameResult compares two signal results ignoring Replayed.
func sameResult(a, b runtime.SignalResult) bool {
	a.Replayed, b.Replayed = false, false
	return reflect.DeepEqual(a, b)
}

func waitCases() []Case {
	return []Case{
		{"wait/resolution_by_identity_is_one_decision", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			res := e.decide(byWait(scopeA, "alice", "k1", w.ID, "approved"))
			e.expect(res.Applied && !res.Replayed && res.Refusal == "" && res.SignalID == identity(scopeA, "alice", "k1").ID(), "the signal decided the wait: %+v", res)
			e.expect(res.Target == w.Target() && res.Decision.Kind == runtime.DecisionResolved && res.Decision.Outcome == "approved" &&
				res.Decision.Actor == "user:alice" && res.Decision.Answer == "because", "its decision: %+v", res)

			rec := e.load(scopeA, run1)
			got := e.review(rec)
			e.expect(got.State == runtime.WaitResolved && got.Version == w.Version+1 && reflect.DeepEqual(got.Decision, res.Decision), "the wait holds the decision: %+v", got)
			e.expect(rec.Run.Version == opened.Run.Version && rec.Run.Steps[1].State == runtime.StepWaiting, "the signal decides the wait and leaves the step to the engine")
			events := e.outbox(scopeA, run1)
			e.expect(countKind(events, runtime.EventWaitResolved) == 1 && countKind(events, runtime.EventSignalRefused) == 0, "one wait.resolved event: %+v", events)

			look, err := e.S.LookupSignal(e.Ctx, identity(scopeA, "alice", "k1"))
			e.ok(err, "look the signal up")
			e.expect(look.Record != nil && look.Tombstone == nil && look.Record.Applied && sameResult(look.Record.Result(), res) &&
				look.Record.Target == w.Target(), "the durable record holds the decision and its target: %+v", look)

			// The engine consumes the decision: the step settles with the wait as decided.
			rec = e.step(rec, func(r *runtime.Run) error {
				return r.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone)
			})
			out, ok := rec.WaitOutcome("review")
			e.expect(ok && out == "approved", "the recorded outcome is readable for conditions: %q", out)
		}},
		{"wait/resolution_by_complete_correlation", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			// Wait identity and correlation must agree.
			res := e.decide(runtime.SignalRequest{Identity: identity(scopeA, "svc", "k0"), WaitID: w.ID,
				Correlation: map[string]string{"candidate": "c2"}, Outcome: "approved", Actor: "svc"})
			e.expect(!res.Applied && res.Refusal == runtime.RefusalUnknownWait, "a disagreeing address reaches no wait: %+v", res)
			res = e.decide(byCorrelation(scopeA, "svc", "k1", "c1", "rejected"))
			e.expect(res.Applied && res.Target == w.Target() && res.Decision.Outcome == "rejected", "an external signal decides by correlation: %+v", res)
			e.expect(e.review(e.load(scopeA, run1)).State == runtime.WaitResolved, "the wait is resolved")
		}},
		{"wait/duplicate_signal_converges", GroupWaits, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			req := byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved")
			first := e.decide(req)
			before := e.load(scopeA, run1)
			n := len(e.outbox(scopeA, run1))

			again := e.decide(req)
			e.expect(again.Replayed && again.Applied && sameResult(first, again), "a replay returns the recorded result: %+v vs %+v", first, again)
			e.unchanged(before, n, "the replay")

			changed := req
			changed.Outcome = "rejected"
			_, err := e.S.DecideSignal(e.Ctx, changed, e.Clock.Now())
			e.isErr(err, runtime.ErrKeyReuse, "the same key with another outcome")
			changed = req
			changed.Answer = "other"
			_, err = e.S.DecideSignal(e.Ctx, changed, e.Clock.Now())
			e.isErr(err, runtime.ErrKeyReuse, "the same key with another answer")
			e.unchanged(before, n, "key reuse")
			look, _ := e.S.LookupSignal(e.Ctx, req.Identity)
			e.expect(look.Record != nil && sameResult(look.Record.Result(), first), "key reuse never modifies the recorded result")
		}},
		{"wait/concurrent_duplicates_decide_once", GroupWaits, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			req := byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved")
			const n = 8
			var wg sync.WaitGroup
			res := make([]runtime.SignalResult, n)
			errs := make([]error, n)
			now := e.Clock.Now()
			for i := range n {
				wg.Add(1)
				go func() { defer wg.Done(); res[i], errs[i] = e.S.DecideSignal(e.Ctx, req, now) }()
			}
			wg.Wait()
			fresh := 0
			for i := range n {
				e.ok(errs[i], "concurrent duplicate %d", i)
				e.expect(res[i].Applied && sameResult(res[i], res[0]), "every duplicate converges on the winner's result: %+v", res[i])
				if !res[i].Replayed {
					fresh++
				}
			}
			e.expect(fresh == 1, "exactly one duplicate decided the wait, %d did", fresh)
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventWaitResolved) == 1, "one continuation intent")
		}},
		{"wait/new_signal_for_a_decided_wait_is_refused_with_its_decision", GroupWaits, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			won := e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			before := e.load(scopeA, run1)

			late := byWait(scopeA, "bob", "k2", waitIDOf(run1), "rejected")
			res := e.decide(late)
			e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && !res.Replayed && reflect.DeepEqual(res.Decision, won.Decision),
				"a later signal loses and sees the recorded decision: %+v", res)
			after := e.load(scopeA, run1)
			e.expect(reflect.DeepEqual(e.review(before), e.review(after)), "the wait did not change")
			events := e.outbox(scopeA, run1)
			e.expect(countKind(events, runtime.EventWaitResolved) == 1 && countKind(events, runtime.EventSignalRefused) == 1, "one decision and one refusal event: %+v", events)

			look, _ := e.S.LookupSignal(e.Ctx, late.Identity)
			e.expect(look.Record != nil && !look.Record.Applied && look.Record.Refusal == runtime.RefusalDecided &&
				reflect.DeepEqual(look.Record.Decision, won.Decision), "the refusal is recorded under the loser's identity with the decision it lost to: %+v", look.Record)
			again := e.decide(late)
			e.expect(again.Replayed && sameResult(again, res), "retrying the refusal returns it")
			e.expect(len(e.outbox(scopeA, run1)) == len(events), "a refusal is recorded once")
		}},
		{"wait/lookup_returns_the_decision_after_the_wait_closes", GroupWaits, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			won := e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			rec := e.load(scopeA, run1)
			rec = e.step(rec, func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone) })
			rec = e.step(rec, func(r *runtime.Run) error { return r.AdvanceStep("ship", runtime.StepRunning, runtime.ReasonNone) })
			rec = e.finishStep(rec, "ship")
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
			rec = e.step(rec, func(r *runtime.Run) error { return r.CompleteCleanup() })
			e.expect(rec.Run.Terminal() && e.review(rec).State == runtime.WaitResolved, "the run is terminal and its wait closed")
			look, err := e.S.LookupSignal(e.Ctx, identity(scopeA, "alice", "k1"))
			e.ok(err, "lookup")
			e.expect(look.Record != nil && reflect.DeepEqual(look.Record.Decision, won.Decision), "the closed wait's decision is still served from the record: %+v", look)
			again := e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			e.expect(again.Replayed && again.Applied && sameResult(again, won), "a replay long after closure is served from the record")
		}},
		{"wait/expiry_wins_at_the_deadline", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			e.Clock.Set(w.Deadline)
			req := byWait(scopeA, "alice", "k1", w.ID, "approved")
			res := e.decide(req)
			e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionExpired,
				"a resolution at the deadline loses to expiry: %+v", res)
			got := e.review(e.load(scopeA, run1))
			e.expect(got.State == runtime.WaitExpired && got.Version == w.Version+1 && reflect.DeepEqual(got.Decision, res.Decision), "the wait is expired: %+v", got)
			events := e.outbox(scopeA, run1)
			e.expect(countKind(events, runtime.EventWaitExpired) == 1 && countKind(events, runtime.EventWaitResolved) == 0, "one expiry event: %+v", events)

			again := e.decide(req)
			e.expect(again.Replayed && sameResult(again, res), "the refusal replays")
			other := e.decide(byWait(scopeA, "bob", "k2", w.ID, "approved"))
			e.expect(!other.Applied && other.Decision.Kind == runtime.DecisionExpired, "another signal sees the expiry")
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventWaitExpired) == 1, "no second expiry")

			// One instant earlier the same signal would have won.
			e.Clock.Set(t0)
			e.waitingRun(scopeA, run2)
			w2 := e.review(e.load(scopeA, run2))
			e.Clock.Set(w2.Deadline.Add(-time.Nanosecond))
			win := e.decide(byWait(scopeA, "alice", "k-early", w2.ID, "approved"))
			e.expect(win.Applied, "a resolution before the deadline wins: %+v", win)
		}},
		{"wait/expiry_by_the_engine_is_the_recorded_decision", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			e.Clock.Set(w.Deadline)
			expired := w
			_, err := expired.Expire(e.Clock.Now())
			e.ok(err, "expire the wait")
			tr := e.transition(opened, func(r *runtime.Run) error {
				return r.AdvanceStep("review", runtime.StepFailed, runtime.ReasonWaitExpired)
			}, e.event(scopeA, run1, "review", runtime.EventWaitExpired, expired.Version))
			tr.Waits = []runtime.Wait{expired}
			rec := e.commit(tr)
			e.expect(e.review(rec).State == runtime.WaitExpired, "the engine recorded the expiry with the step")
			res := e.decide(byWait(scopeA, "alice", "k1", w.ID, "approved"))
			e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionExpired, "a late signal sees the expiry: %+v", res)
		}},
		{"wait/stale_expected_version_is_a_recorded_refusal", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			req := byWait(scopeA, "alice", "k1", w.ID, "approved")
			req.ExpectedVersion = w.Version - 1
			res := e.decide(req)
			e.expect(!res.Applied && res.Refusal == runtime.RefusalVersionConflict && res.Decision == (runtime.Decision{}) && res.Target == w.Target(),
				"an outdated version applies nothing: %+v", res)
			e.expect(e.review(e.load(scopeA, run1)).State == runtime.WaitOpen, "the wait is still open")
			look, _ := e.S.LookupSignal(e.Ctx, req.Identity)
			e.expect(look.Record != nil && look.Record.Refusal == runtime.RefusalVersionConflict && look.Record.Decision == (runtime.Decision{}), "the refusal is recorded without a decision")

			// The same key stays bound to its refusal; a new key with the right version wins.
			req.ExpectedVersion = w.Version
			again := e.decide(req)
			e.expect(again.Replayed && sameResult(again, res), "the identity replays its recorded refusal")
			req.Identity.Key = "k2"
			win := e.decide(req)
			e.expect(win.Applied, "a fresh key with the current version resolves: %+v", win)
		}},
		{"wait/undeclared_outcome_is_a_recorded_refusal_and_bad_syntax_is_not", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			w := e.review(opened)
			res := e.decide(byWait(scopeA, "alice", "k1", w.ID, "maybe"))
			e.expect(!res.Applied && res.Refusal == runtime.RefusalInvalidOutcome && res.Decision == (runtime.Decision{}), "an undeclared outcome: %+v", res)
			e.expect(e.review(e.load(scopeA, run1)).State == runtime.WaitOpen, "the wait is still open")

			bad := byWait(scopeA, "alice", "k2", w.ID, "Not An Outcome!")
			_, err := e.S.DecideSignal(e.Ctx, bad, e.Clock.Now())
			e.isErr(err, domain.ErrInvalidValue, "a syntactically invalid request")
			look, err := e.S.LookupSignal(e.Ctx, bad.Identity)
			e.ok(err, "lookup")
			e.expect(!look.Found(), "a caller error is never recorded and does not use its key")
		}},
		{"wait/targetless_refusal_never_reroutes_to_a_later_wait", GroupWaits, func(t TB, e *Env) {
			// Run 1 has not opened its wait yet: a signal for it, by identity or by correlation, reaches nothing.
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "engine", runtime.ClaimExecute, longTTL)
			rec = e.startBuild(rec)
			rec = e.finishStep(rec, "build")

			byID := byWait(scopeA, "alice", "k-id", waitIDOf(run1), "approved")
			byCorr := byCorrelation(scopeA, "svc", "k-corr", "c1", "approved")
			for _, req := range []runtime.SignalRequest{byID, byCorr} {
				res := e.decide(req)
				e.expect(!res.Applied && res.Refusal == runtime.RefusalUnknownWait && res.Target == (runtime.SignalTarget{}) && res.Decision == (runtime.Decision{}),
					"a signal that resolves no target is refused with no target: %+v", res)
				look, err := e.S.LookupSignal(e.Ctx, req.Identity)
				e.ok(err, "lookup")
				e.expect(look.Tombstone != nil && look.Record == nil && look.Tombstone.Refusal == runtime.RefusalUnknownWait, "the refusal is a tombstone: %+v", look)
			}
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventSignalRefused) == 0, "a targetless refusal has no run to attach an event to")

			// The wait opens with exactly the correlation the refused signals named.
			rec = e.openReview(rec)
			n := len(e.outbox(scopeA, run1))
			for _, req := range []runtime.SignalRequest{byID, byCorr} {
				again := e.decide(req)
				e.expect(again.Replayed && !again.Applied && again.Refusal == runtime.RefusalUnknownWait, "the retry returns the tombstone's refusal, it is not rerouted: %+v", again)
			}
			e.unchanged(rec, n, "the retries")
			e.expect(e.review(rec).State == runtime.WaitOpen, "the later wait is untouched")

			// A new identity is a new request and does reach the wait.
			won := e.decide(byCorrelation(scopeA, "svc", "k-corr-2", "c1", "approved"))
			e.expect(won.Applied && won.Target == e.review(rec).Target(), "a fresh key resolves the wait: %+v", won)
		}},
		{"wait/ambiguous_correlation_is_a_tombstone", GroupWaits, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			e.waitingRun(scopeA, run2)
			req := byCorrelation(scopeA, "svc", "k1", "c1", "approved")
			res := e.decide(req)
			e.expect(!res.Applied && res.Refusal == runtime.RefusalAmbiguousCorrelation && res.Target == (runtime.SignalTarget{}), "two open waits match: %+v", res)
			for _, id := range []domain.RunID{run1, run2} {
				e.expect(e.review(e.load(scopeA, id)).State == runtime.WaitOpen, "neither wait is decided")
			}
			look, _ := e.S.LookupSignal(e.Ctx, req.Identity)
			e.expect(look.Tombstone != nil && look.Tombstone.Refusal == runtime.RefusalAmbiguousCorrelation, "the ambiguity is recorded")

			// Cancel one run: only one open wait would match now, but the identity stays refused.
			_, err := e.S.RequestCancel(e.Ctx, scopeA, run2)
			e.ok(err, "cancel run2")
			again := e.decide(req)
			e.expect(again.Replayed && again.Refusal == runtime.RefusalAmbiguousCorrelation, "the retry is not rerouted to the remaining wait: %+v", again)
			e.expect(e.review(e.load(scopeA, run1)).State == runtime.WaitOpen, "run1's wait is untouched")
			e.expect(e.decide(byCorrelation(scopeA, "svc", "k2", "c1", "approved")).Applied, "a new key reaches the one open wait")
		}},
		{"wait/resolution_cancellation_race_has_one_winner", GroupWaits, func(t TB, e *Env) {
			// Signal first, then cancel: the decision stands.
			e.waitingRun(scopeA, run1)
			won := e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			_, err := e.S.RequestCancel(e.Ctx, scopeA, run1)
			e.ok(err, "cancel after the decision")
			w := e.review(e.load(scopeA, run1))
			e.expect(w.State == runtime.WaitResolved && reflect.DeepEqual(w.Decision, won.Decision), "cancellation does not undo a decision: %+v", w)
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventWaitCancelled) == 0, "no cancellation event for a decided wait")

			// Concurrent signal and cancellation of fresh runs: exactly one decision each.
			for i, id := range []domain.RunID{"race1", "race2", "race3", "race4", "race5", "race6"} {
				e.waitingRun(scopeA, id)
				var wg sync.WaitGroup
				var res runtime.SignalResult
				var sigErr, cancelErr error
				now := e.Clock.Now()
				wg.Add(2)
				go func() {
					defer wg.Done()
					res, sigErr = e.S.DecideSignal(e.Ctx, byWait(scopeA, "alice", "race-"+string(id), waitIDOf(id), "approved"), now)
				}()
				go func() { defer wg.Done(); _, cancelErr = e.S.RequestCancel(e.Ctx, scopeA, id) }()
				wg.Wait()
				e.ok(sigErr, "round %d signal", i)
				e.ok(cancelErr, "round %d cancel", i)
				got := e.review(e.load(scopeA, id))
				events := e.outbox(scopeA, id)
				resolved, cancelled := countKind(events, runtime.EventWaitResolved), countKind(events, runtime.EventWaitCancelled)
				e.expect(resolved+cancelled == 1, "round %d: exactly one decision event, got %d resolved and %d cancelled", i, resolved, cancelled)
				switch got.State {
				case runtime.WaitResolved:
					e.expect(res.Applied && resolved == 1, "round %d: the signal won: %+v", i, res)
				case runtime.WaitCancelled:
					e.expect(!res.Applied && res.Refusal == runtime.RefusalDecided && res.Decision.Kind == runtime.DecisionCancelled && cancelled == 1,
						"round %d: the cancellation won and the signal saw it: %+v", i, res)
				default:
					t.Fatalf("round %d: the wait is %s", i, got.State)
				}
			}
		}},
		{"wait/decision_rolls_back_as_one", GroupWaits, func(t TB, e *Env) {
			rec := e.waitingRun(scopeA, run1)
			n := len(e.outbox(scopeA, run1))
			req := byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved")
			boom := errors.New("injected commit failure")
			e.S.FailNextCommit(boom)
			t.Cleanup(func() { e.S.FailNextCommit(nil) })
			_, err := e.S.DecideSignal(e.Ctx, req, e.Clock.Now())
			e.isErr(err, boom, "a decision failing at commit")
			e.unchanged(rec, n, "the failed decision")
			look, _ := e.S.LookupSignal(e.Ctx, req.Identity)
			e.expect(!look.Found(), "no signal record survives a rolled back decision")
			res := e.decide(req)
			e.expect(res.Applied && !res.Replayed, "the retry decides the wait: %+v", res)
		}},
		{"wait/engine_cannot_overwrite_a_decision", GroupWaits, func(t TB, e *Env) {
			opened := e.waitingRun(scopeA, run1)
			e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			decided := e.load(scopeA, run1)
			n := len(e.outbox(scopeA, run1))

			// An engine still holding the open wait it read before the signal.
			stale := e.bump(decided)
			stale.Waits = []runtime.Wait{e.review(opened)}
			_, err := e.S.Commit(e.Ctx, stale)
			e.versionConflict(err, "the open wait the engine read before the decision")
			// Or one that tries to change the decision under its version.
			forged := e.review(decided)
			forged.Decision.Outcome = "rejected"
			tr := e.bump(decided)
			tr.Waits = []runtime.Wait{forged}
			_, err = e.S.Commit(e.Ctx, tr)
			e.versionConflict(err, "a rewritten decision")
			// Or one that cancels the decided wait with the next version.
			cancelled := e.review(decided)
			cancelled.State, cancelled.Version = runtime.WaitCancelled, cancelled.Version+1
			cancelled.Decision = runtime.Decision{Kind: runtime.DecisionCancelled, Reason: runtime.ReasonRunCancelled, Version: cancelled.Version, At: e.Clock.Now()}
			tr = e.bump(decided)
			tr.Waits = []runtime.Wait{cancelled}
			_, err = e.S.Commit(e.Ctx, tr)
			e.isErr(err, runtime.ErrWaitClosed, "cancelling a decided wait")
			e.unchanged(decided, n, "the refused wait writes")
		}},
	}
}
