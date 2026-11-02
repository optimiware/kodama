package kernelcontracts

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// outputsPlan is build (declaring the outputs commit and branch) -> deploy.
func outputsPlan() runtime.Plan {
	return runtime.Plan{Digest: "sha256:outputs", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Outputs: []string{"commit", "branch"},
			Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "deploy", Kind: runtime.StepContainer, DependsOn: []domain.StepID{"build"},
			Workload: &runtime.WorkloadSpec{Image: "alpine:3", Commands: []string{"deploy"}}},
	}}
}

// runningBuild admits an outputs-plan run, claims it for w1 and starts build.
func (e *Env) runningBuild() runtime.RunRecord {
	e.T.Helper()
	e.admit(scopeA, run1, outputsPlan())
	rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
	return e.startBuild(rec)
}

// settleBuild is the transition that settles build as succeeded, with outs.
func (e *Env) settleBuild(rec runtime.RunRecord, outs ...runtime.StepOutputs) runtime.Transition {
	e.T.Helper()
	tr := e.transition(rec, func(r *runtime.Run) error {
		return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	})
	tr.Outputs = outs
	return tr
}

func buildOutputs(kv map[string]string) runtime.StepOutputs {
	return runtime.StepOutputs{StepKey: "build", Attempt: 1, Values: kv}
}

func outputsCases() []Case {
	return []Case{
		{"outputs/are_recorded_with_the_settlement_and_survive_takeover", GroupOutputs, func(t TB, e *Env) {
			rec := e.runningBuild()
			want := buildOutputs(map[string]string{"commit": "c1", "branch": ""})
			got := e.commit(e.settleBuild(rec, want))
			e.expect(got.Run.Steps[0].State == runtime.StepSucceeded && len(got.Outputs) == 1 && reflect.DeepEqual(got.Outputs[0], want),
				"the outputs are in the record the commit returns: %+v", got.Outputs)
			e.expect(reflect.DeepEqual(got, e.load(scopeA, run1)), "Commit returns the stored record")
			v, ok := got.Output("build", "commit")
			e.expect(ok && v == "c1", "the record answers for the step: %q %v", v, ok)
			_, ok = got.Output("build", "other")
			e.expect(!ok, "an unrecorded name is missing")
			e.ok(got.Validate(), "the stored record is coherent")

			// A returned record is a copy: changing its values changes nothing stored.
			got.Outputs[0].Values["commit"] = "evil"
			loaded := e.load(scopeA, run1)
			loaded.Outputs[0].Values["branch"] = "evil"
			again := e.load(scopeA, run1)
			e.expect(reflect.DeepEqual(again.Outputs, []runtime.StepOutputs{want}), "mutating a returned record changed the store: %+v", again.Outputs)

			// A replacement engine, after a takeover, reads them from the record alone.
			e.Clock.Advance(ttl)
			w2 := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			e.expect(reflect.DeepEqual(w2.Outputs, []runtime.StepOutputs{want}) && w2.Claim.Fence == 2, "outputs survive the takeover: %+v", w2.Outputs)
			// And they are not rewritten by the writers that follow.
			next := e.step(w2, func(r *runtime.Run) error { return r.AdvanceStep("deploy", runtime.StepRunning, runtime.ReasonNone) })
			e.expect(reflect.DeepEqual(next.Outputs, []runtime.StepOutputs{want}), "later commits keep them: %+v", next.Outputs)
		}},
		{"outputs/refuse_undeclared_oversized_late_and_unsettled", GroupOutputs, func(t TB, e *Env) {
			rec := e.runningBuild()
			n := len(e.outbox(scopeA, run1))

			_, err := e.S.Commit(e.Ctx, e.settleBuild(rec, buildOutputs(map[string]string{"secret": "x"})))
			e.isErr(err, domain.ErrInvalidValue, "an undeclared output name")
			e.unchanged(rec, n, "the undeclared output")
			_, err = e.S.Commit(e.Ctx, e.settleBuild(rec, buildOutputs(map[string]string{"commit": strings.Repeat("a", runtime.MaxOutputValueBytes+1)})))
			e.isErr(err, domain.ErrInvalidValue, "an oversized output value")
			e.unchanged(rec, n, "the oversized output")
			_, err = e.S.Commit(e.Ctx, e.settleBuild(rec, runtime.StepOutputs{StepKey: "build", Attempt: 2, Values: map[string]string{"commit": "x"}}))
			e.isErr(err, runtime.ErrIllegalTransition, "outputs of another attempt")
			e.unchanged(rec, n, "the wrong attempt")

			// Outputs belong to the step's settlement: not to a step that is still running,
			// not to a step that has not started.
			running := e.bump(rec)
			running.Outputs = []runtime.StepOutputs{buildOutputs(map[string]string{"commit": "c1"})}
			_, err = e.S.Commit(e.Ctx, running)
			e.isErr(err, runtime.ErrIllegalTransition, "outputs of a step that is still running")
			e.unchanged(rec, n, "the unsettled step")
			other := e.settleBuild(rec, runtime.StepOutputs{StepKey: "deploy", Attempt: 1, Values: map[string]string{"commit": "x"}})
			_, err = e.S.Commit(e.Ctx, other)
			e.isErr(err, runtime.ErrIllegalTransition, "outputs of a step that did not settle in this commit")
			e.unchanged(rec, n, "the other step")

			// Never later: a step settled without outputs does not gain any.
			settled := e.finishStep(rec, "build")
			n = len(e.outbox(scopeA, run1))
			late := e.bump(settled)
			late.Outputs = []runtime.StepOutputs{buildOutputs(map[string]string{"commit": "c1"})}
			_, err = e.S.Commit(e.Ctx, late)
			e.isErr(err, runtime.ErrIllegalTransition, "outputs added after the step settled")
			e.unchanged(settled, n, "the late outputs")
			e.expect(len(e.load(scopeA, run1).Outputs) == 0, "nothing was recorded")
		}},
		{"outputs/are_all_or_nothing_with_the_commit", GroupOutputs, func(t TB, e *Env) {
			rec := e.runningBuild()
			n := len(e.outbox(scopeA, run1))
			tr := e.settleBuild(rec, buildOutputs(map[string]string{"commit": "c1"}))
			boom := errors.New("injected commit failure")
			e.S.FailNextCommit(boom)
			t.Cleanup(func() { e.S.FailNextCommit(nil) })
			_, err := e.S.Commit(e.Ctx, tr)
			e.isErr(err, boom, "a commit failing at its commit point")
			e.unchanged(rec, n, "the failed settlement")
			got := e.commit(tr)
			e.expect(got.Run.Steps[0].State == runtime.StepSucceeded && len(got.Outputs) == 1, "the retry records the step and its outputs together")
		}},
	}
}

func releaseCases() []Case {
	return []Case{
		{"release/frees_the_run_without_ending_it", GroupRelease, func(t TB, e *Env) {
			e.admit(scopeA, run1, outputsPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			rec = e.startBuild(rec)
			n := len(e.outbox(scopeA, run1))
			e.Clock.Advance(10 * time.Second)
			now := e.Clock.Now()

			e.ok(e.S.Release(e.Ctx, rec.Claim, now), "release the claim")
			after := e.load(scopeA, run1)
			e.expect(after.Claim.Fence == rec.Claim.Fence && after.Claim.Owner == "w1" && after.Claim.Purpose == runtime.ClaimExecute,
				"the fence and owner are kept: %+v", after.Claim)
			e.expect(!after.Claim.Active(now) && after.Claim.Expires.Equal(now), "the lease ends now: %s", after.Claim.Expires)
			e.expect(reflect.DeepEqual(after.Run, rec.Run), "the run is not changed: version %d", after.Run.Version)
			e.expect(len(e.outbox(scopeA, run1)) == n, "a release writes no event")

			// The holder is done: it can neither write nor renew.
			_, err := e.S.Commit(e.Ctx, e.bump(rec))
			e.isErr(err, runtime.ErrClaimExpired, "a commit after the release")
			_, err = e.S.Renew(e.Ctx, rec.Claim, ttl, now)
			e.isErr(err, runtime.ErrClaimExpired, "a renewal after the release")

			// A repeat changes nothing.
			e.ok(e.S.Release(e.Ctx, rec.Claim, now), "release again")
			e.expect(reflect.DeepEqual(e.load(scopeA, run1), after), "a repeat is a no-op")

			// The run is claimable at once, with no wait for the original expiry.
			refs, err := e.S.Runnable(e.Ctx, runtime.RunnableQuery{Now: now, Limit: 10})
			e.ok(err, "list runnable runs")
			e.expect(len(refs) == 1 && refs[0].RunID == run1 && refs[0].Purpose == runtime.ClaimExecute, "the released run is runnable: %v", refs)
			w2 := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			e.expect(w2.Claim.Fence == 2 && w2.Claim.Owner == "w2", "the next claim is a takeover that increments the fence: %+v", w2.Claim)
		}},
		{"release/never_takes_anothers_claim", GroupRelease, func(t TB, e *Env) {
			e.admit(scopeA, run1, outputsPlan())
			w1 := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, ttl)
			e.Clock.Advance(ttl)
			w2 := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			n := len(e.outbox(scopeA, run1))

			e.ok(e.S.Release(e.Ctx, w1.Claim, e.Clock.Now()), "the superseded holder releases")
			e.unchanged(w2, n, "the superseded holder's release")
			_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w3", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "a claim while the new holder's lease stands")

			impostor := w2.Claim
			impostor.Owner = "w3"
			e.ok(e.S.Release(e.Ctx, impostor, e.Clock.Now()), "another owner at the current fence")
			future := w2.Claim
			future.Fence++
			e.ok(e.S.Release(e.Ctx, future, e.Clock.Now()), "a fence the store never granted")
			e.unchanged(w2, n, "the foreign releases")

			// An expired lease is already free: releasing it changes nothing.
			e.Clock.Advance(2 * ttl)
			e.ok(e.S.Release(e.Ctx, w2.Claim, e.Clock.Now()), "release after expiry")
			e.unchanged(w2, n, "the release of an expired lease")

			// A run nobody claimed has no holder to release.
			e.admit(scopeA, run2, outputsPlan())
			never := e.load(scopeA, run2)
			forged := runtime.Claim{Scope: scopeA, RunID: run2, Owner: "w1", Purpose: runtime.ClaimExecute, Fence: 1, Expires: e.Clock.Now().Add(time.Hour)}
			e.ok(e.S.Release(e.Ctx, forged, e.Clock.Now()), "a claim nobody granted")
			e.unchanged(never, 1, "the forged release")
			e.expect(e.claim(scopeA, run2, "w1", runtime.ClaimExecute, ttl).Claim.Fence == 1, "the run is still unclaimed")

			bad := w2.Claim
			bad.Owner = ""
			e.isErr(e.S.Release(e.Ctx, bad, e.Clock.Now()), domain.ErrInvalidValue, "an invalid claim")
		}},
		{"release/is_scoped_by_tenant_and_project", GroupRelease, func(t TB, e *Env) {
			e.admit(scopeA, run1, outputsPlan())
			rec := e.claim(scopeA, run1, "w1", runtime.ClaimExecute, longTTL)
			n := len(e.outbox(scopeA, run1))
			for name, scope := range map[string]domain.Scope{"another tenant": scopeB, "another project of the tenant": scopeC} {
				held := rec.Claim
				held.Scope = scope
				e.notFound(e.S.Release(e.Ctx, held, e.Clock.Now()), "a release in "+name)
			}
			held := rec.Claim
			held.RunID = "absent"
			e.notFound(e.S.Release(e.Ctx, held, e.Clock.Now()), "a release of a run that does not exist")
			e.unchanged(rec, n, "the foreign releases")
			_, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w2", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.isErr(err, runtime.ErrClaimHeld, "the holder still holds the run")
		}},
	}
}

func materializationCases() []Case {
	return []Case{
		{"materialization/identity_is_durable_scoped_and_revoked_once", GroupMaterialization, func(t TB, e *Env) {
			rec := e.runningBuild()
			ref, err := runtime.NewMaterializationRef(scopeA, run1, "build", 1)
			e.ok(err, "name the material of the running attempt")
			issued := runtime.MaterializationRecord{Ref: ref, State: runtime.MaterializationIssued, IssuedAt: e.Clock.Now()}
			tr := e.bump(rec)
			tr.Materializations = []runtime.MaterializationRecord{issued}
			rec = e.commit(tr)
			e.expect(reflect.DeepEqual(rec.Materializations, []runtime.MaterializationRecord{issued}) && len(rec.PendingRevocations()) == 0,
				"the identity is recorded and the running attempt's material is live: %+v", rec.Materializations)
			e.ok(rec.Validate(), "the stored record is coherent")

			// Another tenant's identity is never recorded in this run.
			foreign, _ := runtime.NewMaterializationRef(scopeB, run1, "build", 1)
			tr = e.bump(rec)
			tr.Materializations = []runtime.MaterializationRecord{{Ref: foreign, State: runtime.MaterializationIssued, IssuedAt: e.Clock.Now()}}
			_, err = e.S.Commit(e.Ctx, tr)
			e.isErr(err, domain.ErrInvalidValue, "another tenant's identity")
			e.unchanged(rec, len(e.outbox(scopeA, run1)), "the foreign identity")

			// After a crash and takeover the replacement finds it in the record.
			e.Clock.Advance(ttl)
			w2 := e.claim(scopeA, run1, "w2", runtime.ClaimExecute, ttl)
			e.expect(reflect.DeepEqual(w2.Materializations, []runtime.MaterializationRecord{issued}), "the identity survives the takeover: %+v", w2.Materializations)
			settled := e.step(w2, func(r *runtime.Run) error {
				return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
			})
			e.expect(len(settled.PendingRevocations()) == 1, "once the step settled the material is owed: %+v", settled.Materializations)
			n := len(e.outbox(scopeA, run1))

			// What is refused: an unknown step, an attempt that was
			// never launched, a rewritten identity, reopening, and issuing for a settled step.
			unknown, _ := runtime.NewMaterializationRef(scopeA, run1, "nope", 1)
			future, _ := runtime.NewMaterializationRef(scopeA, run1, "build", 2)
			for what, bad := range map[string]runtime.MaterializationRecord{
				"an unknown step":                {Ref: unknown, State: runtime.MaterializationIssued, IssuedAt: e.Clock.Now()},
				"an attempt that never launched": {Ref: future, State: runtime.MaterializationIssued, IssuedAt: e.Clock.Now()},
				"a rewritten issue time":         {Ref: ref, State: runtime.MaterializationRevoked, IssuedAt: e.Clock.Now().Add(time.Second)},
			} {
				tr := e.bump(settled)
				tr.Materializations = []runtime.MaterializationRecord{bad}
				_, err := e.S.Commit(e.Ctx, tr)
				e.isErr(err, domain.ErrInvalidValue, what)
				e.unchanged(settled, n, what)
			}
			fresh, _ := runtime.NewMaterializationRef(scopeA, run1, "deploy", 1)
			tr = e.bump(settled)
			tr.Materializations = []runtime.MaterializationRecord{{Ref: fresh, State: runtime.MaterializationIssued, IssuedAt: e.Clock.Now()}}
			_, err = e.S.Commit(e.Ctx, tr)
			e.expect(err != nil, "material is issued only for a running attempt")
			e.unchanged(settled, n, "the material for a step that is not running")

			// A stale writer cannot record anything.
			stale := e.bump(settled)
			stale.Claim = w2.Claim
			stale.Claim.Fence = 1
			stale.Claim.Owner = "w1"
			stale.Materializations = []runtime.MaterializationRecord{{Ref: ref, State: runtime.MaterializationRevoked, IssuedAt: issued.IssuedAt}}
			_, err = e.S.Commit(e.Ctx, stale)
			e.isErr(err, runtime.ErrStaleFence, "the superseded writer's revocation record")
			e.unchanged(settled, n, "the stale writer")

			// Revocation is recorded once and is final.
			revoked := issued
			revoked.State = runtime.MaterializationRevoked
			tr = e.bump(settled)
			tr.Materializations = []runtime.MaterializationRecord{revoked}
			done := e.commit(tr)
			e.expect(len(done.PendingRevocations()) == 0 && done.Materializations[0].State == runtime.MaterializationRevoked, "revoked: %+v", done.Materializations)
			n = len(e.outbox(scopeA, run1))
			tr = e.bump(done)
			tr.Materializations = []runtime.MaterializationRecord{issued}
			_, err = e.S.Commit(e.Ctx, tr)
			e.isErr(err, runtime.ErrIllegalTransition, "reopening a revoked materialization")
			e.unchanged(done, n, "the reopening")
			tr = e.bump(done)
			tr.Materializations = []runtime.MaterializationRecord{revoked}
			again := e.commit(tr)
			e.expect(reflect.DeepEqual(again.Materializations, done.Materializations), "a repeat of the stored record changes nothing")
		}},
	}
}
