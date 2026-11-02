package kernelcontracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func admissionCases() []Case {
	return []Case{
		{"admit/idempotent_and_conflict", GroupAdmission, func(t TB, e *Env) {
			first := e.admit(scopeA, run1, reviewPlan())
			e.expect(first.Run.Version == 1 && first.Claim == (runtime.Claim{}), "a new run is version 1 and unclaimed: %+v", first.Run)
			e.expect(len(first.Waits) == 1 && first.Waits[0].State == runtime.WaitPending, "admission records one pending wait")
			again, err := e.S.Admit(e.Ctx, e.admission(scopeA, run1, reviewPlan(), 2))
			e.ok(err, "repeat the admission")
			e.expect(reflect.DeepEqual(first, again), "a repeat returns the existing record")
			e.expect(countKind(e.outbox(scopeA, run1), runtime.EventRunCreated) == 1, "run.created is recorded once")

			other := reviewPlan()
			other.Digest = "sha256:other"
			_, err = e.S.Admit(e.Ctx, e.admission(scopeA, run1, other, 2))
			e.isErr(err, domain.ErrConflict, "a different plan digest under the same run id")
			e.unchanged(first, 1, "the conflicting admission")
		}},
		{"admit/invalid_admission_is_refused", GroupAdmission, func(t TB, e *Env) {
			a := e.admission(scopeA, run1, reviewPlan(), 2)
			a.Waits = nil
			_, err := e.S.Admit(e.Ctx, a)
			e.isErr(err, domain.ErrInvalidValue, "an admission without its wait")
			_, err = e.S.Load(e.Ctx, scopeA, run1)
			e.notFound(err, "the refused run")
			started := e.admission(scopeA, run2, reviewPlan(), 2)
			started.Run.Version = 2
			_, err = e.S.Admit(e.Ctx, started)
			e.isErr(err, domain.ErrInvalidValue, "an admitted run that is not new")
		}},
		{"admit/rolls_back_with_its_outbox", GroupAdmission, func(t TB, e *Env) {
			boom := errors.New("injected commit failure")
			e.S.FailNextCommit(boom)
			t.Cleanup(func() { e.S.FailNextCommit(nil) })
			_, err := e.S.Admit(e.Ctx, e.admission(scopeA, run1, reviewPlan(), 2))
			e.isErr(err, boom, "an admission failing at commit")
			_, err = e.S.Load(e.Ctx, scopeA, run1)
			e.notFound(err, "the rolled back run")
			_, err = e.S.Outbox(e.Ctx, scopeA, run1)
			e.notFound(err, "the outbox of the rolled back run")
			e.admit(scopeA, run1, reviewPlan())
			e.expect(len(e.outbox(scopeA, run1)) == 1, "the retry commits the event once")
		}},
		{"admit/returns_copies", GroupAdmission, func(t TB, e *Env) {
			rec := e.admit(scopeA, run1, reviewPlan())
			// An independent snapshot: a copy that shares memory with the store would change with it.
			want, err := json.Marshal(e.load(scopeA, run1))
			e.ok(err, "snapshot")
			rec.Run.Steps[0].State = runtime.StepSucceeded
			rec.Plan.Steps[0].Workload.Image = "evil"
			rec.Waits[0].Spec.Outcomes[0] = "evil"
			rec.Waits[0].Spec.Correlate["candidate"] = "evil"
			loaded := e.load(scopeA, run1)
			loaded.Run.Steps[1].State = runtime.StepCancelled
			loaded.Plan.Steps[1].Wait.Outcomes[1] = "evil"
			got, err := json.Marshal(e.load(scopeA, run1))
			e.ok(err, "snapshot")
			e.expect(string(want) == string(got), "mutating a returned record must not change the store")
		}},
	}
}

func claimCases() []Case {
	return []Case{
		{"claim/race_has_one_winner", GroupClaims, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			race := func(round int, wantFence runtime.Fence) {
				const n = 8
				var wg sync.WaitGroup
				errs := make([]error, n)
				won := make([]runtime.RunRecord, n)
				now := e.Clock.Now()
				for i := range n {
					wg.Add(1)
					go func() {
						defer wg.Done()
						won[i], errs[i] = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, fmt.Sprintf("worker-%d-%d", round, i), runtime.ClaimExecute, ttl), now)
					}()
				}
				wg.Wait()
				wins := 0
				for i, err := range errs {
					switch {
					case err == nil:
						wins++
						e.expect(won[i].Claim.Fence == wantFence, "the winner's fence is %d, got %d", wantFence, won[i].Claim.Fence)
					case !errors.Is(err, runtime.ErrClaimHeld):
						t.Fatalf("a losing claimant must get ErrClaimHeld, got %v", err)
					}
				}
				e.expect(wins == 1, "exactly one of %d concurrent claims wins, %d did", n, wins)
			}
			race(0, 1)
			e.Clock.Advance(ttl) // the lease expires exactly at its expiry instant
			race(1, 2)
		}},
		{"claim/unexpired_lease_is_held_even_for_its_owner", GroupClaims, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			held := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			e.expect(held.Claim.Fence == 1 && held.Claim.Owner == "w1" && held.Claim.Expires.Equal(t0.Add(ttl)), "first claim: %+v", held.Claim)
			_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "the owner claiming again (it must renew)")
			e.Clock.Advance(ttl - time.Nanosecond)
			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w2", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "another claimant one instant before expiry")
			e.Clock.Advance(time.Nanosecond)
			got := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			e.expect(got.Claim.Fence == 2 && got.Claim.Owner == "w2", "takeover at expiry: %+v", got.Claim)
		}},
		{"claim/renewal_keeps_the_fence_and_is_persisted", GroupClaims, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			e.Clock.Advance(30 * time.Second)
			renewed, err := e.S.Renew(e.Ctx, rec.Claim, ttl, e.Clock.Now())
			e.ok(err, "renew")
			e.expect(renewed.Fence == 1 && renewed.Owner == "w1" && renewed.Purpose == runtime.ClaimExecute, "renewal keeps owner, purpose and fence: %+v", renewed)
			e.expect(renewed.Expires.Equal(t0.Add(90*time.Second)), "the lease moves to now+ttl, got %s", renewed.Expires)
			e.expect(reflect.DeepEqual(e.load(scopeA, run1).Claim, renewed), "the renewed lease is the stored lease")

			short, err := e.S.Renew(e.Ctx, renewed, time.Second, e.Clock.Now())
			e.ok(err, "renew with a shorter ttl")
			e.expect(short.Expires.Equal(renewed.Expires), "a renewal never shortens a lease: %s -> %s", renewed.Expires, short.Expires)

			e.Clock.Advance(31 * time.Second) // past the original expiry, inside the renewed one
			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w2", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "a claim inside the renewed lease")

			impostor := renewed
			impostor.Owner = "w2"
			_, err = e.S.Renew(e.Ctx, impostor, ttl, e.Clock.Now())
			e.isErr(err, runtime.ErrStaleFence, "a renewal by another owner at the same fence")
			e.expect(e.load(scopeA, run1).Claim.Owner == "w1", "the rejected renewal changed nothing")
		}},
		{"claim/takeover_rejects_the_stale_writer", GroupClaims, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			w1 := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			w1 = e.startBuild(w1)
			n := len(e.outbox(scopeA, run1))
			e.Clock.Advance(ttl)
			w2 := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			e.expect(w2.Claim.Fence == 2 && w2.Run.Version == w1.Run.Version, "takeover increments the fence and leaves the run: %+v", w2.Claim)

			stale := e.transition(w1, func(r *runtime.Run) error {
				return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
			}, e.event(scopeA, run1, "build", runtime.EventStepStateChanged, w1.Run.Version+1))
			_, err := e.S.Commit(e.Ctx, stale)
			e.isErr(err, runtime.ErrStaleFence, "the superseded writer's commit")
			_, err = e.S.Renew(e.Ctx, w1.Claim, ttl, e.Clock.Now())
			e.isErr(err, runtime.ErrStaleFence, "the superseded writer's renewal")
			e.unchanged(w2, n, "the stale writer")

			fresh := e.transition(w2, func(r *runtime.Run) error {
				return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
			})
			got := e.commit(fresh)
			e.expect(got.Run.Steps[0].State == runtime.StepSucceeded && got.Claim.Fence == 2, "the new owner commits")
		}},
		{"claim/expired_lease_cannot_commit_or_renew", GroupClaims, func(t TB, e *Env) {
			e.admit(scopeA, run1, reviewPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			n := len(e.outbox(scopeA, run1))
			e.Clock.Advance(ttl)
			late := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) },
				e.event(scopeA, run1, "build", runtime.EventStepStateChanged, 2))
			_, err := e.S.Commit(e.Ctx, late)
			e.isErr(err, runtime.ErrClaimExpired, "a commit after the lease expired")
			_, err = e.S.Renew(e.Ctx, rec.Claim, ttl, e.Clock.Now())
			e.isErr(err, runtime.ErrClaimExpired, "a renewal after the lease expired")
			e.unchanged(rec, n, "the expired writer")
		}},
		{"claim/never_claimed_run_has_no_writer", GroupClaims, func(t TB, e *Env) {
			rec := e.admit(scopeA, run1, reviewPlan())
			forged := runtime.Claim{Scope: scopeA, RunID: run1, Owner: "w1", Purpose: runtime.ClaimExecute, Fence: 1, Expires: t0.Add(time.Hour)}
			tr := e.bump(rec)
			tr.Claim = forged
			_, err := e.S.Commit(e.Ctx, tr)
			e.expect(err != nil, "a commit under a claim nobody granted must be refused")
			_, err = e.S.Renew(e.Ctx, forged, ttl, e.Clock.Now())
			e.expect(err != nil, "a renewal of a claim nobody granted must be refused")
			e.unchanged(rec, 1, "the forged claim")
		}},
		{"claim/purpose_must_be_admitted", GroupClaims, func(t TB, e *Env) {
			rec := e.admit(scopeA, run1, reviewPlan())
			_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", runtime.ClaimCleanup, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrIllegalTransition, "a cleanup claim on an executing run")
			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", "bogus", ttl), e.Clock.Now())
			e.isErr(err, domain.ErrInvalidValue, "an unknown purpose")
			_, err = e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", runtime.ClaimExecute, 0), e.Clock.Now())
			e.isErr(err, domain.ErrInvalidValue, "a claim without a ttl")
			e.unchanged(rec, 1, "the refused claims")
		}},
		{"claim/runnable_lists_unclaimed_and_expired_work", GroupClaims, func(t TB, e *Env) {
			for _, id := range []domain.RunID{"run1", "run2", "run3"} {
				e.admit(scopeA, id, reviewPlan())
			}
			list := func(limit int) []runtime.RunRef {
				refs, err := e.S.Runnable(e.Ctx, runtime.RunnableQuery{Now: e.Clock.Now(), Limit: limit})
				e.ok(err, "list runnable runs")
				return refs
			}
			refs := list(10)
			e.expect(len(refs) == 3, "three unclaimed runs are runnable, got %v", refs)
			for _, r := range refs {
				e.expect(r.Scope == scopeA && r.Purpose == runtime.ClaimExecute, "a fresh run is executable work: %+v", r)
			}
			e.expect(len(list(2)) == 2, "the limit bounds the result")
			_, err := e.S.Runnable(e.Ctx, runtime.RunnableQuery{Now: e.Clock.Now(), Limit: 0})
			e.expect(err != nil, "an unbounded query is refused")

			e.claim(scopeA, "run1", "w1", runtime.ClaimExecute, ttl)
			e.expect(len(list(10)) == 2, "a run under an active lease is not runnable")
			e.Clock.Advance(ttl)
			e.expect(len(list(10)) == 3, "a run whose lease expired is runnable again")

			_, err = e.S.RequestCancel(e.Ctx, scopeA, "run2")
			e.ok(err, "cancel")
			for _, r := range list(10) {
				if r.RunID == "run2" {
					e.expect(r.Purpose != runtime.ClaimExecute, "a cancelling run is not executable work")
				}
				// Whatever purpose is listed must be one the run admits.
				_, err := e.S.Claim(e.Ctx, e.claimReq(r.Scope, r.RunID, "sweeper", r.Purpose, ttl), e.Clock.Now())
				e.ok(err, "claim %s for the listed purpose %s", r.RunID, r.Purpose)
			}
			e.expect(len(list(10)) == 0, "every listed run was claimed")
		}},
	}
}
