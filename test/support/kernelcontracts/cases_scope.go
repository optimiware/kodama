package kernelcontracts

import (
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// inScope returns rec's claim addressed to another scope: the same run id and
// lease, presented to a tenant or project that does not own the run.
func inScope(rec runtime.RunRecord, scope domain.Scope) runtime.Claim {
	c := rec.Claim
	c.Scope = scope
	return c
}

func scopeCases() []Case {
	return []Case{
		{"scope/foreign_or_mismatched_scope_is_not_found", GroupScope, func(t TB, e *Env) {
			rec := e.waitingRun(scopeA, run1)
			n := len(e.outbox(scopeA, run1))
			for _, foreign := range []domain.Scope{scopeB, scopeC} {
				name := foreign.String()
				_, err := e.S.Load(e.Ctx, foreign, run1)
				e.notFound(err, "Load from "+name)
				_, err = e.S.View(e.Ctx, foreign, run1)
				e.notFound(err, "View from "+name)
				_, err = e.S.Outbox(e.Ctx, foreign, run1)
				e.notFound(err, "Outbox from "+name)
				_, err = e.S.RequestCancel(e.Ctx, foreign, run1)
				e.notFound(err, "RequestCancel from "+name)
				_, err = e.S.Claim(e.Ctx, e.claimReq(foreign, run1, "intruder", runtime.ClaimReconcile, ttl), e.Clock.Now())
				e.notFound(err, "Claim from "+name)
				_, err = e.S.Renew(e.Ctx, inScope(rec, foreign), ttl, e.Clock.Now())
				e.notFound(err, "Renew from "+name)

				// A transition for the same run id in the foreign scope.
				r := rec.Run.Clone()
				r.Scope = foreign
				r.Version++
				tr := runtime.Transition{Scope: foreign, RunID: run1, Claim: inScope(rec, foreign), Now: e.Clock.Now(), ExpectedVersion: rec.Run.Version, Run: r}
				_, err = e.S.Commit(e.Ctx, tr)
				e.notFound(err, "Commit from "+name)
			}
			e.unchanged(rec, n, "every foreign operation")
		}},
		{"scope/the_same_run_id_in_another_scope_is_another_run", GroupScope, func(t TB, e *Env) {
			a := e.admit(scopeA, run1, reviewPlan())
			other := reviewPlan()
			other.Digest = "sha256:another-project"
			b, err := e.S.Admit(e.Ctx, e.admission(scopeB, run1, other, 2))
			e.ok(err, "admit the same run id for another tenant")
			c, err := e.S.Admit(e.Ctx, e.admission(scopeC, run1, other, 2))
			e.ok(err, "admit the same run id for another project")
			e.expect(a.Run.Scope == scopeA && b.Run.Scope == scopeB && c.Run.Scope == scopeC && b.Plan.Digest == other.Digest, "three runs, three scopes")

			e.claim(scopeB, run1, "wb", runtime.ClaimExecute, ttl)
			e.expect(e.load(scopeA, run1).Claim == (runtime.Claim{}) && e.load(scopeC, run1).Claim == (runtime.Claim{}), "a claim in one scope claims nothing elsewhere")
			_, err = e.S.RequestCancel(e.Ctx, scopeC, run1)
			e.ok(err, "cancel in scope C")
			e.expect(!e.load(scopeA, run1).Run.CancelRequested && !e.load(scopeB, run1).Run.CancelRequested && e.load(scopeC, run1).Run.CancelRequested,
				"cancelling in one scope cancels nothing elsewhere")
			e.expect(len(e.outbox(scopeA, run1)) == 1 && len(e.outbox(scopeB, run1)) == 1, "the outboxes are separate")
		}},
		{"scope/signal_identity_is_scoped_by_tenant_project_and_caller", GroupScope, func(t TB, e *Env) {
			e.waitingRun(scopeA, run1)
			e.waitingRun(scopeA, run2)
			won := e.decide(byWait(scopeA, "alice", "shared-key", waitIDOf(run1), "approved"))
			e.expect(won.Applied, "alice resolves run1's wait")

			// The same key from another caller, project or tenant is another identity.
			for _, id := range []runtime.SignalIdentity{identity(scopeA, "bob", "shared-key"), identity(scopeB, "alice", "shared-key"), identity(scopeC, "alice", "shared-key")} {
				look, err := e.S.LookupSignal(e.Ctx, id)
				e.ok(err, "lookup")
				e.expect(!look.Found(), "%+v must not see alice's record", id)
			}
			// Bob's request with the same key decides his own request, not alice's.
			bob := e.decide(byWait(scopeA, "bob", "shared-key", waitIDOf(run2), "rejected"))
			e.expect(bob.Applied && bob.Target.RunID == run2 && bob.SignalID != won.SignalID, "the key is bound to its caller: %+v", bob)

			// A signal from another tenant or project reaches no wait of scope A, and decides nothing.
			for _, scope := range []domain.Scope{scopeB, scopeC} {
				for _, req := range []runtime.SignalRequest{
					byWait(scope, "mallory", "x1", waitIDOf(run2), "approved"),
					byCorrelation(scope, "mallory", "x2", "c1", "approved"),
				} {
					res := e.decide(req)
					e.expect(!res.Applied && res.Refusal == runtime.RefusalUnknownWait && res.Target == (runtime.SignalTarget{}),
						"a foreign wait is indistinguishable from a missing one: %+v", res)
				}
			}
			e.expect(e.review(e.load(scopeA, run2)).Decision.Actor == "user:bob", "run2's wait holds only bob's decision")
			// The foreign refusals are recorded in the foreign scope, not scope A.
			look, _ := e.S.LookupSignal(e.Ctx, identity(scopeB, "mallory", "x1"))
			e.expect(look.Tombstone != nil && look.Tombstone.Identity.Scope == scopeB, "the refusal belongs to the caller's own scope")
		}},
		{"scope/runnable_reports_each_scope_once", GroupScope, func(t TB, e *Env) {
			for _, s := range []domain.Scope{scopeA, scopeB, scopeC} {
				e.admit(s, run1, reviewPlan())
			}
			refs, err := e.S.Runnable(e.Ctx, runtime.RunnableQuery{Now: e.Clock.Now().Add(time.Second), Limit: 10})
			e.ok(err, "runnable")
			got := map[domain.Scope]int{}
			for _, r := range refs {
				got[r.Scope]++
				e.expect(r.RunID == run1 && r.Purpose == runtime.ClaimExecute, "unexpected ref %+v", r)
			}
			e.expect(len(refs) == 3 && got[scopeA] == 1 && got[scopeB] == 1 && got[scopeC] == 1, "each scope's run is listed once with its own scope: %+v", refs)
			// Each ref is claimable only under its own scope.
			for _, r := range refs {
				_, err := e.S.Claim(e.Ctx, e.claimReq(r.Scope, r.RunID, "sweeper", r.Purpose, ttl), e.Clock.Now())
				e.ok(err, "claim %+v", r)
			}
		}},
	}
}

func viewCases() []Case {
	return []Case{
		{"view/is_rebuilt_from_retained_records_at_every_stage", GroupViews, func(t TB, e *Env) {
			check := func(stage string, want runtime.RunStatus) runtime.RunRecord {
				t.Helper()
				rec := e.load(scopeA, run1)
				view, err := e.S.View(e.Ctx, scopeA, run1)
				e.ok(err, "view at %s", stage)
				e.expect(reflect.DeepEqual(view.Run, rec.Run) && reflect.DeepEqual(view.Waits, rec.Waits), "%s: the view carries the retained run and waits", stage)
				e.expect(view.Status == want && view.Status == rec.Run.Status(), "%s: status %s, want %s", stage, view.Status, want)
				return rec
			}
			e.admit(scopeA, run1, reviewPlan())
			check("admitted", runtime.StatusQueued)
			rec := e.claim(scopeA, run1, "engine", runtime.ClaimExecute, longTTL)
			check("claimed", runtime.StatusQueued)
			rec = e.startBuild(rec)
			check("build running", runtime.StatusRunning)
			rec = e.finishStep(rec, "build")
			rec = e.openReview(rec)
			v := check("waiting", runtime.StatusRunning)
			e.expect(len(v.Waits) == 1 && v.Waits[0].State == runtime.WaitOpen, "the open wait is in the view")
			e.decide(byWait(scopeA, "alice", "k1", waitIDOf(run1), "approved"))
			v = check("decided", runtime.StatusRunning)
			e.expect(v.Waits[0].State == runtime.WaitResolved, "the decision is in the view")
			rec = e.load(scopeA, run1)
			rec = e.step(rec, func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone) })
			rec = e.step(rec, func(r *runtime.Run) error { return r.AdvanceStep("ship", runtime.StepRunning, runtime.ReasonNone) })
			rec = e.finishStep(rec, "ship")
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
			check("settled", runtime.StatusCleaning)
			e.step(rec, func(r *runtime.Run) error { return r.CompleteCleanup() })
			check("cleaned", runtime.StatusSucceeded)

			// A takeover or a claim changes no view: it is rebuilt from run, step and wait records alone.
			_, err := e.S.View(e.Ctx, scopeB, run1)
			e.notFound(err, "a view from another tenant")
			_, err = e.S.View(e.Ctx, scopeA, "absent")
			e.notFound(err, "a view of a run that does not exist")
		}},
		{"view/cancelled_and_failed_runs_derive_their_status", GroupViews, func(t TB, e *Env) {
			e.admit(scopeA, run1, solePlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			rec = e.step(rec, func(r *runtime.Run) error {
				return r.FinishStep("build", runtime.StepFailed, runtime.ReasonCommandFailed, runtime.Result{Code: runtime.ResultFailed, ExitCode: 2})
			})
			rec = e.step(rec, func(r *runtime.Run) error { return r.Settle(runtime.OutcomeFailed, runtime.ReasonCommandFailed) })
			view, err := e.S.View(e.Ctx, scopeA, run1)
			e.ok(err, "view")
			e.expect(view.Status == runtime.StatusCleaning, "a failed run is cleaning until cleanup is final: %s", view.Status)
			e.step(rec, func(r *runtime.Run) error { return r.CompleteCleanup() })
			view, _ = e.S.View(e.Ctx, scopeA, run1)
			e.expect(view.Status == runtime.StatusFailed, "then failed: %s", view.Status)
		}},
	}
}
