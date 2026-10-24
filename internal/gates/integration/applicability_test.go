package integration_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const writeRun = "workspace: {access: write, from: run}"

func writeFrom(step string) string { return "workspace: {access: write, from: " + step + "}" }
func readFrom(step string) string  { return "workspace: {access: read, from: " + step + "}" }

// approveAndConsume requests the step's gate, approves it as the reviewer and
// lets reconciliation complete the gate step.
func approveAndConsume(t *testing.T, e *gatestest.Env, scope domain.Scope, run *execution.Run, step string) *gates.Gate {
	t.Helper()
	reconcile(t, e, scope.TenantID)
	g := gateOf(t, e, run, step)
	if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
		t.Fatalf("approve %s: %v", step, err)
	}
	if rep := reconcile(t, e, scope.TenantID); rep.Completed < 1 {
		t.Fatalf("consume %s: %+v", step, rep)
	}
	if got := stepStatus(t, e, run, step); got != execution.StepSucceeded {
		t.Fatalf("gate step %s is %s", step, got)
	}
	return gateByID(t, e, g)
}

func attemptsOf(e *gatestest.Env, run *execution.Run, step string) int {
	return count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = $2`, string(run.ID), step)
}

func reservations(e *gatestest.Env, run *execution.Run) int {
	return count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
}

// TestChangedSpecSourceConfig (DQ-04, DQ-05, DQ-A03, PX-13): a changed
// specification, accepted candidate, evidence or deployment configuration makes
// the old approval inapplicable for new actions, before the decision, before
// consumption and after the gate step already succeeded; the historical
// decision and results stay intact, and an outage denies progression without
// fabricating an invalidation.
func TestChangedSpecSourceConfig(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA
	tenant := scope.TenantID

	t.Run("content changed before the decision", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		e.X.EditSpec(scope, p.Item, "# Plan\nA changed requirement.")
		_, err := approve(t, e, g, gatestest.Reviewer)
		var se *gates.StaleError
		if !errors.As(err, &se) || se.Reason != gates.ReasonContentChanged || !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("approving a stale gate: %v", err)
		}
		cur := gateByID(t, e, g)
		if cur.Status != gates.StatusPending || cur.Decision != nil {
			t.Fatalf("a stale decision was recorded: %+v", cur)
		}
		if cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonContentChanged || cur.Invalidation.SupersedingRevision != 2 {
			t.Fatalf("the definitive staleness was not recorded: %+v", cur.Invalidation)
		}
		// Recorded once: a repeat and a reconciliation add nothing.
		if _, err := approve(t, e, cur, gatestest.Reviewer); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("approving an invalidated gate: %v", err)
		}
		reconcile(t, e, tenant)
		if n := outboxCount(e, "gate.invalidated", g.ID); n != 1 {
			t.Fatalf("%d gate.invalidated events", n)
		}
		if n := auditCount(e, "gate.invalidated", g.ID); n != 1 {
			t.Fatalf("%d invalidation audit rows", n)
		}
		if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a successor of a stale gate ran: %v", err)
		}
	})

	t.Run("content changed between approval and consumption", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		e.X.EditSpec(scope, p.Item, "# Plan\nChanged after the approval.")
		rep := reconcile(t, e, tenant)
		if rep.Completed != 0 || rep.Invalidated != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
			t.Fatalf("the stale approval completed the gate step: %s", got)
		}
		cur := gateByID(t, e, g)
		if cur.Status != gates.StatusApproved || cur.Decision == nil || cur.Invalidation == nil {
			t.Fatalf("the decision must stay with a separate invalidation: %+v", cur)
		}
		if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a successor ran on a stale approval: %v", err)
		}
	})

	t.Run("content changed after the gate step succeeded", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		g := approveAndConsume(t, e, scope, run, "review")
		decision := *g.Decision
		attempts := attemptsOf(e, run, "deploy")
		e.X.EditSpec(scope, p.Item, "# Plan\nChanged after the gate step succeeded.")
		if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a protected successor was claimed after the spec changed: %v", err)
		}
		if n := attemptsOf(e, run, "deploy"); n != attempts {
			t.Fatalf("the refused claim leaked an attempt ordinal: %d -> %d", attempts, n)
		}
		if n := reservations(e, run); n != 1 {
			t.Fatalf("the refused claim leaked a reservation: %d", n)
		}
		rep := reconcile(t, e, tenant)
		if rep.Invalidated != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		cur := gateByID(t, e, g)
		if cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonContentChanged || *cur.Decision != decision || cur.Status != gates.StatusApproved {
			t.Fatalf("history must stay intact with the invalidation beside it: %+v", cur)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepSucceeded {
			t.Fatalf("the gate step's historical result changed: %s", got)
		}
		// Cancellation stays possible with a stale approval.
		if _, err := e.Svc.Cancel(ctx, execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})

	t.Run("a write after the approval needs a new gate", func(t *testing.T) {
		p := e.X.NewProject(scope)
		def := e.Define(scope,
			pipelinetest.Container("implement", writeRun),
			gatestest.Gate("review", []string{"implement"}),
			pipelinetest.Container("polish", "needs: [review]", writeFrom("implement")),
			pipelinetest.Container("ship", "needs: [polish]", readFrom("polish")))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		e.X.Sources.History[gatestest.CommitC] = true

		e.Finish(e.MustClaim(tenant), gatestest.CommitB)
		g := approveAndConsume(t, e, scope, run, "review")
		if g.Subject.CandidateCommit() != gatestest.CommitB {
			t.Fatalf("candidate %s: the writer's published commit is the candidate", g.Subject.CandidateCommit())
		}
		// The writer itself acts on the approved candidate.
		e.X.Sources.Tip = gatestest.CommitB
		in := e.MustClaim(tenant)
		if in.StepID != "polish" {
			t.Fatalf("claimed %s", in.StepID)
		}
		e.Finish(in, gatestest.CommitC)
		// The commit it produced was never approved: no gate covers it.
		if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a successor ran on a commit nobody approved: %v", err)
		}
		if n := attemptsOf(e, run, "ship"); n != 0 {
			t.Fatalf("%d attempts for the uncovered step", n)
		}
		if cur := gateByID(t, e, g); cur.Invalidation != nil || cur.Status != gates.StatusApproved {
			t.Fatalf("the earlier approval is history and stays untouched: %+v", cur)
		}
	})

	t.Run("a new gate after the write covers the new candidate", func(t *testing.T) {
		p := e.X.NewProject(scope)
		def := e.Define(scope,
			pipelinetest.Container("implement", writeRun),
			gatestest.Gate("review", []string{"implement"}),
			pipelinetest.Container("polish", "needs: [review]", writeFrom("implement")),
			gatestest.Gate("review2", []string{"polish"}),
			pipelinetest.Container("ship", "needs: [review2]", readFrom("polish")))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		e.X.Sources.History[gatestest.CommitC] = true
		e.X.Sources.Tip = gatestest.CommitA

		e.Finish(e.MustClaim(tenant), gatestest.CommitB)
		approveAndConsume(t, e, scope, run, "review")
		e.X.Sources.Tip = gatestest.CommitB
		e.Finish(e.MustClaim(tenant), gatestest.CommitC)
		g2 := approveAndConsume(t, e, scope, run, "review2")
		if g2.Subject.CandidateCommit() != gatestest.CommitC {
			t.Fatalf("second candidate %s", g2.Subject.CandidateCommit())
		}
		in := e.MustClaim(tenant)
		if in.StepID != "ship" {
			t.Fatalf("claimed %s", in.StepID)
		}
	})

	t.Run("evidence removed after approval", func(t *testing.T) {
		p, run := reviewed(t, e)
		ids := finishTest(t, e, p)
		approveAndConsume(t, e, scope, run, "review")
		e.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(ids["report"]))
		if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a successor ran after its evidence was removed: %v", err)
		}
		if rep := reconcile(t, e, tenant); rep.Invalidated != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
	})

	t.Run("deployment configuration", func(t *testing.T) {
		p := e.X.NewProject(scope)
		def := e.DefineDoc(scope, pipelinetest.DocNamed("deploying", "{}",
			pipelinetest.Container("build", readRun, "outputs: {deployment: {type: string}}"),
			gatestest.DeploymentGate("review", []string{"build"}, nil, "${{ steps.build.outputs.deployment }}"),
			pipelinetest.Container("promote", "needs: [review]", readRun)))
		e.Deployments.Set(scope, "dep_t034_1", gatestest.CommitA, digestOf("config-1"))

		newRun := func() *execution.Run {
			run := e.MustAdmit(p, def, nil)
			cancelAtEnd(t, e, run)
			in := e.MustClaim(tenant)
			e.StartAndSucceed(in, map[string]runtimev1.Value{"deployment": runtimev1.StringValue("dep_t034_1")})
			return run
		}

		// Without the deployment adapter a deployment gate cannot be created,
		// while it is still enforced for source-only gates.
		run := newRun()
		e.Deployments.Remove("dep_t034_1")
		if rep := reconcile(t, e, tenant); rep.Requested != 0 {
			t.Fatalf("a gate was created for an unknown deployment: %+v", rep)
		}
		if _, err := e.Store.ByStep(ctx, scope, run.ID, "review"); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("gate for an unknown deployment: %v", err)
		}
		e.Deployments.Set(scope, "dep_t034_1", gatestest.CommitA, digestOf("config-1"))

		// The configuration is bound into the subject.
		if rep := reconcile(t, e, tenant); rep.Requested != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		g := gateOf(t, e, run, "review")
		if id, cfg := g.Subject.Deployment(); id != "dep_t034_1" || cfg != digestOf("config-1") {
			t.Fatalf("subject deployment %q %q", id, cfg)
		}

		// A changed configuration refuses the decision and is recorded once.
		e.Deployments.Set(scope, "dep_t034_1", gatestest.CommitA, digestOf("config-2"))
		_, err := approve(t, e, g, gatestest.Reviewer)
		var se *gates.StaleError
		if !errors.As(err, &se) || se.Reason != gates.ReasonDeploymentChanged {
			t.Fatalf("approving over a changed configuration: %v", err)
		}
		if cur := gateByID(t, e, g); cur.Invalidation == nil || cur.Status != gates.StatusPending {
			t.Fatalf("gate %+v", cur)
		}
	})

	// Ownership and candidate are part of the deployment facts: a snapshot with
	// the same identity and configuration that belongs to another project or
	// records another candidate no longer describes the subject.
	foreign := domain.Scope{TenantID: scope.TenantID, ProjectID: "prj_t034_foreign"}
	for i, tc := range []struct {
		name  string
		scope domain.Scope
		cand  string
	}{
		{"foreign scope", foreign, gatestest.CommitA},
		{"wrong candidate", scope, gatestest.CommitB},
	} {
		suffix := string(rune('a' + i))
		depID := "dep_t034_own_" + suffix
		setup := func(t *testing.T, name string) (*execution.Run, *gates.Gate) {
			t.Helper()
			p := e.X.NewProject(scope)
			def := e.DefineDoc(scope, pipelinetest.DocNamed(name, "{}",
				pipelinetest.Container("build", readRun, "outputs: {deployment: {type: string}}"),
				gatestest.DeploymentGate("review", []string{"build"}, nil, "${{ steps.build.outputs.deployment }}"),
				pipelinetest.Container("promote", "needs: [review]", readRun)))
			e.Deployments.Set(scope, depID, gatestest.CommitA, digestOf("config-own"))
			run := e.MustAdmit(p, def, nil)
			cancelAtEnd(t, e, run)
			e.StartAndSucceed(e.MustClaim(tenant), map[string]runtimev1.Value{"deployment": runtimev1.StringValue(depID)})
			reconcile(t, e, tenant)
			return run, gateOf(t, e, run, "review")
		}
		t.Run("deployment ownership at the decision: "+tc.name, func(t *testing.T) {
			_, g := setup(t, "own-decide-"+suffix)
			e.Deployments.Set(tc.scope, depID, tc.cand, digestOf("config-own"))
			_, err := approve(t, e, g, gatestest.Reviewer)
			var se *gates.StaleError
			if !errors.As(err, &se) || se.Reason != gates.ReasonDeploymentChanged {
				t.Fatalf("approving over a %s deployment: %v", tc.name, err)
			}
			if cur := gateByID(t, e, g); cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil {
				t.Fatalf("gate %+v", cur)
			}
		})
		t.Run("deployment ownership at consumption: "+tc.name, func(t *testing.T) {
			run, g := setup(t, "own-consume-"+suffix)
			if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
				t.Fatal(err)
			}
			e.Deployments.Set(tc.scope, depID, tc.cand, digestOf("config-own"))
			rep := reconcile(t, e, tenant)
			if rep.Completed != 0 || rep.Invalidated != 1 {
				t.Fatalf("reconcile: %+v", rep)
			}
			if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
				t.Fatalf("the approval of a %s deployment completed the gate step: %s", tc.name, got)
			}
			cur := gateByID(t, e, g)
			if cur.Status != gates.StatusApproved || cur.Decision == nil || cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonDeploymentChanged {
				t.Fatalf("the decision must stay with a separate invalidation: %+v", cur)
			}
		})
		t.Run("deployment ownership at a protected claim: "+tc.name, func(t *testing.T) {
			run, g := setup(t, "own-claim-"+suffix)
			if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
				t.Fatal(err)
			}
			if rep := reconcile(t, e, tenant); rep.Completed != 1 {
				t.Fatalf("consume: %+v", rep)
			}
			decision := *gateByID(t, e, g).Decision
			attempts := attemptsOf(e, run, "promote")
			e.Deployments.Set(tc.scope, depID, tc.cand, digestOf("config-own"))
			if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("a protected successor was claimed over a %s deployment: %v", tc.name, err)
			}
			if n := attemptsOf(e, run, "promote"); n != attempts {
				t.Fatalf("the refused claim leaked an attempt: %d -> %d", attempts, n)
			}
			if rep := reconcile(t, e, tenant); rep.Invalidated != 1 {
				t.Fatalf("reconcile: %+v", rep)
			}
			cur := gateByID(t, e, g)
			if cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonDeploymentChanged || *cur.Decision != decision {
				t.Fatalf("history must stay intact with the invalidation beside it: %+v", cur)
			}
			if got := stepStatus(t, e, run, "review"); got != execution.StepSucceeded {
				t.Fatalf("the gate step's historical result changed: %s", got)
			}
		})
	}

	t.Run("a deployment outage denies without invalidating", func(t *testing.T) {
		p := e.X.NewProject(scope)
		def := e.DefineDoc(scope, pipelinetest.DocNamed("deploying2", "{}",
			pipelinetest.Container("build", readRun, "outputs: {deployment: {type: string}}"),
			gatestest.DeploymentGate("review", []string{"build"}, nil, "${{ steps.build.outputs.deployment }}"),
			pipelinetest.Container("promote", "needs: [review]", readRun)))
		e.Deployments.Set(scope, "dep_t034_2", gatestest.CommitA, digestOf("config-a"))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		e.StartAndSucceed(e.MustClaim(tenant), map[string]runtimev1.Value{"deployment": runtimev1.StringValue("dep_t034_2")})
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		e.Deployments.Fail(errors.New("deployment service down"))
		defer e.Deployments.Fail(nil)
		rep := reconcile(t, e, tenant)
		if rep.Completed != 0 || rep.Invalidated != 0 || rep.Failed == 0 {
			t.Fatalf("an outage must deny progression and record nothing: %+v", rep)
		}
		if _, err := approve(t, e, gateByID(t, e, g), gatestest.Reviewer); err == nil {
			t.Fatalf("a decision during an outage")
		}
		if cur := gateByID(t, e, g); cur.Invalidation != nil || stepStatus(t, e, run, "review") != execution.StepPending {
			t.Fatalf("an outage fabricated an invalidation or moved the step: %+v", cur)
		}
		e.Deployments.Fail(nil)
		if rep := reconcile(t, e, tenant); rep.Completed != 1 {
			t.Fatalf("after recovery: %+v", rep)
		}
	})
}
