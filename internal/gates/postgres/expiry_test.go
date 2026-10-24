package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// holdAuthorization takes the reviewer's authorization-revision lock exclusively,
// as an in-flight revocation or grant does. The authority check of a gate
// consumption waits for it.
func holdAuthorization(t *testing.T, e *gatestest.Env, scope domain.Scope, user string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	hold, err := e.X.Migration.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if _, err := hold.Exec(ctx, `SELECT 1 FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`,
		string(scope.TenantID), user); err != nil {
		t.Fatal(err)
	}
	return hold
}

// holdGate takes the gate row lock exclusively, as an in-flight decision does.
func holdGate(t *testing.T, e *gatestest.Env, scope domain.Scope, id gates.ID) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	hold, err := e.X.Migration.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if _, err := hold.Exec(ctx, `SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
		string(scope.TenantID), string(scope.ProjectID), string(id)); err != nil {
		t.Fatal(err)
	}
	return hold
}

// gateEffects is the durable state an expired approval must leave untouched.
type gateEffects struct {
	status                string
	version               int64
	invalidated           bool
	audits, steps         int
	invalidatedEvents     int
	decidedEvents, decAud int
}

func effectsOf(e *gatestest.Env, run *execution.Run, g *gates.Gate, step string) gateEffects {
	var f gateEffects
	e.X.Scalar(&f.status, `SELECT status FROM gates WHERE id = $1`, string(g.ID))
	e.X.Scalar(&f.version, `SELECT version FROM gates WHERE id = $1`, string(g.ID))
	f.invalidated = count(e, `SELECT count(*) FROM gates WHERE id = $1 AND invalidated_at IS NOT NULL`, string(g.ID)) == 1
	f.audits = count(e, `SELECT count(*) FROM audit WHERE resource_id = $1`, string(g.ID))
	f.decAud = audits(e, "gate.decided", g.ID)
	f.invalidatedEvents = outbox(e, "gate.invalidated", g.ID)
	f.decidedEvents = outbox(e, "gate.decided", g.ID)
	f.steps = stepTransitions(e, run, step)
	return f
}

func expectDenied(t *testing.T, err error) {
	t.Helper()
	var se *gates.StaleError
	if !errors.Is(err, gates.ErrStaleSubject) || !errors.As(err, &se) || se.Reason != gates.ReasonExpired || se.Definitive {
		t.Fatalf("expected a non-definitive expiry denial, got %v", err)
	}
}

type completion struct {
	out gates.StepOutcome
	err error
}

func completeAsync(e *gatestest.Env, scope domain.Scope, id gates.ID) <-chan completion {
	ch := make(chan completion, 1)
	go func() {
		out, err := e.Store.CompleteStep(context.Background(), scope, id)
		ch <- completion{out, err}
	}()
	return ch
}

func awaitCompletion(t *testing.T, ch <-chan completion) completion {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(30 * time.Second):
		t.Fatal("the completion never finished")
	}
	return completion{}
}

// approvedGate runs test -> review(gate) -> deploy up to an approved gate that
// is not yet consumed.
func approvedGate(t *testing.T, e *gatestest.Env, scope domain.Scope) (executiontest.Project, *execution.Run, *gates.Gate) {
	t.Helper()
	p, run, g := pendingGate(t, e, scope)
	if _, err := e.Gates.Decide(context.Background(), gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
		t.Fatal(err)
	}
	return p, run, g
}

// twoProtectingGates is test -> review(gate) -> audit(gate) -> deploy(writer):
// review is approved by the reviewer, audit twenty minutes later by the author
// (a different reviewer), and both are consumed. The deploy claim is then
// protected by both gates, in that order, and review expires first. The clock
// is restored when the test ends.
func twoProtectingGates(t *testing.T, e *gatestest.Env, scope domain.Scope) (executiontest.Project, *execution.Run, *gates.Gate, *gates.Gate) {
	t.Helper()
	ctx := context.Background()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		gatestest.Gate("audit", []string{"test", "review"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [audit]", writeRun))
	run := e.MustAdmit(p, def, nil)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	step := func(id string, who string) *gates.Gate {
		t.Helper()
		if _, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil {
			t.Fatal(err)
		}
		g, err := e.Store.ByStep(ctx, scope, run.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(who), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatal(err)
		}
		if rep, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Completed != 1 {
			t.Fatalf("consume %s: %+v %v", id, rep, err)
		}
		g, err = e.Store.Get(ctx, scope, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	review := step("review", gatestest.Reviewer)
	e.X.Clock.Advance(20 * time.Minute)
	t.Cleanup(func() { e.X.Clock.Advance(-20 * time.Minute) })
	audit := step("audit", gatestest.Author)
	if !audit.Deadline.After(review.Deadline) {
		t.Fatalf("audit must expire after review: %v %v", audit.Deadline, review.Deadline)
	}
	return p, run, review, audit
}

// TestGateExpiryAfterLockWait (DQ-04, PX-13, SD-04, SD-05): an approval that
// reaches its deadline while the consuming transaction waits for the gate lock
// or for the reviewer's authorization lock never authorizes progression: the
// expiry is judged after the waits, not at the time the operation began. The
// refusal leaves the approval historically approved (no invalidation, no new
// decision, audit or event), no step transition, attempt ordinal, capacity
// reservation or writer slot, and a retry or redelivered wakeup cannot renew it.
func TestGateExpiryAfterLockWait(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA
	pastTTL := time.Hour + time.Minute

	// advance moves the clock and restores it when the subtest ends, so one
	// subtest's expiry does not age the gates of the next.
	advance := func(t *testing.T, d time.Duration) {
		t.Helper()
		e.X.Clock.Advance(d)
		t.Cleanup(func() { e.X.Clock.Advance(-d) })
	}

	t.Run("completion expires while waiting for the gate lock", func(t *testing.T) {
		_, run, g := approvedGate(t, e, scope)
		before := effectsOf(e, run, g, "review")
		hold := holdGate(t, e, scope, g.ID)
		result := completeAsync(e, scope, g.ID)
		waitBlocked(t, e, hold)
		advance(t, pastTTL)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		expectDenied(t, awaitCompletion(t, result).err)
		if after := effectsOf(e, run, g, "review"); after != before {
			t.Fatalf("an expired approval changed durable state: %+v -> %+v", before, after)
		}
		if before.status != "approved" || before.invalidated {
			t.Fatalf("unexpected starting state %+v", before)
		}
		// Retries and reconciliation never renew it.
		for i := 0; i < 2; i++ {
			if _, err := e.Store.CompleteStep(ctx, scope, g.ID); err == nil {
				t.Fatal("a retry consumed an expired approval")
			}
			if rep, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Completed != 0 || rep.Invalidated != 0 {
				t.Fatalf("reconcile of an expired approval: %+v %v", rep, err)
			}
		}
		if after := effectsOf(e, run, g, "review"); after != before {
			t.Fatalf("retries changed durable state: %+v -> %+v", before, after)
		}
	})

	t.Run("completion expires while waiting for the reviewer authorization lock", func(t *testing.T) {
		_, run, g := approvedGate(t, e, scope)
		before := effectsOf(e, run, g, "review")
		hold := holdAuthorization(t, e, scope, gatestest.Reviewer)
		result := completeAsync(e, scope, g.ID)
		waitBlocked(t, e, hold)
		advance(t, pastTTL)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		expectDenied(t, awaitCompletion(t, result).err)
		if after := effectsOf(e, run, g, "review"); after != before {
			t.Fatalf("an expired approval changed durable state: %+v -> %+v", before, after)
		}
	})

	t.Run("reconciliation and decision wakeups are denied the same way", func(t *testing.T) {
		_, run, g := approvedGate(t, e, scope)
		before := effectsOf(e, run, g, "review")
		hold := holdAuthorization(t, e, scope, gatestest.Reviewer)
		type processed struct {
			rep gates.Report
			err error
		}
		ch := make(chan processed, 1)
		go func() {
			rep, err := e.Gates.ProcessGate(ctx, scope, g.ID)
			ch <- processed{rep, err}
		}()
		waitBlocked(t, e, hold)
		advance(t, pastTTL)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-ch:
			if r.rep.Completed != 0 {
				t.Fatalf("an expired approval progressed: %+v %v", r.rep, r.err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the wakeup never finished")
		}
		if after := effectsOf(e, run, g, "review"); after != before {
			t.Fatalf("an expired approval changed durable state: %+v -> %+v", before, after)
		}
	})

	t.Run("the control: a wait that ends before the deadline still completes", func(t *testing.T) {
		_, run, g := approvedGate(t, e, scope)
		hold := holdAuthorization(t, e, scope, gatestest.Reviewer)
		result := completeAsync(e, scope, g.ID)
		waitBlocked(t, e, hold)
		advance(t, 10*time.Minute)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		c := awaitCompletion(t, result)
		if c.err != nil || !c.out.Completed {
			t.Fatalf("an unexpired approval was refused: %+v", c)
		}
		if n := stepTransitions(e, run, "review"); n != 1 {
			t.Fatalf("step transitions = %d", n)
		}
	})

	t.Run("a protected claim expires while waiting for the reviewer authorization lock", func(t *testing.T) {
		p, run, review, _ := gatedWriter(t, e, scope)
		before := claimLeaks(e, p, run)
		effects := effectsOf(e, run, review, "review")
		hold := holdAuthorization(t, e, scope, gatestest.Reviewer)
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)
		advance(t, pastTTL)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-result:
			if !errors.Is(r.err, execution.ErrNothingRunnable) {
				t.Fatalf("claim behind an expired approval: %+v", r)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the claim never finished")
		}
		if after := claimLeaks(e, p, run); after != before {
			t.Fatalf("a refused claim leaked: %+v -> %+v", before, after)
		}
		if after := effectsOf(e, run, review, "review"); after != effects {
			t.Fatalf("an expired approval changed durable state: %+v -> %+v", effects, after)
		}
		// A repeated claim neither renews the approval nor allocates compute.
		if _, err := e.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("retried claim: %v", err)
		}
		if after := claimLeaks(e, p, run); after != before {
			t.Fatalf("a retried claim leaked: %+v -> %+v", before, after)
		}
	})

	t.Run("the protected control: a wait that ends before the deadline still claims", func(t *testing.T) {
		p, run, _, _ := gatedWriter(t, e, scope)
		hold := holdAuthorization(t, e, scope, gatestest.Reviewer)
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)
		advance(t, 10*time.Minute)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-result:
			if r.err != nil || r.in == nil {
				t.Fatalf("an unexpired approval refused the claim: %+v", r)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the claim never finished")
		}
		if l := claimLeaks(e, p, run); l.attempts != 1 || l.ordinal != 1 || l.writer != 1 {
			t.Fatalf("claim effects %+v", l)
		}
	})

	t.Run("an earlier protecting gate expires while a later gate's authority is awaited", func(t *testing.T) {
		p, run, review, audit := twoProtectingGates(t, e, scope)
		before := claimLeaks(e, p, run)
		reviewFx := effectsOf(e, run, review, "review")
		auditFx := effectsOf(e, run, audit, "audit")
		// Only the author's authority (audit) is awaited: the reviewer's check
		// of the earlier gate has already passed when the claim blocks.
		hold := holdAuthorization(t, e, scope, gatestest.Author)
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)
		// Past review's deadline, before audit's.
		advance(t, review.Deadline.Sub(e.X.Clock.Now())+time.Minute)
		if e.X.Clock.Now().After(audit.Deadline) {
			t.Fatal("the clock passed the later deadline as well")
		}
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-result:
			if !errors.Is(r.err, execution.ErrNothingRunnable) {
				t.Fatalf("claim behind a stale earlier gate: %+v", r)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the claim never finished")
		}
		if after := claimLeaks(e, p, run); after != before {
			t.Fatalf("a refused claim leaked: %+v -> %+v", before, after)
		}
		if after := effectsOf(e, run, review, "review"); after != reviewFx {
			t.Fatalf("review changed: %+v -> %+v", reviewFx, after)
		}
		if after := effectsOf(e, run, audit, "audit"); after != auditFx {
			t.Fatalf("audit changed: %+v -> %+v", auditFx, after)
		}
	})

	t.Run("the multi-gate control: both unexpired still claims", func(t *testing.T) {
		p, run, _, _ := twoProtectingGates(t, e, scope)
		hold := holdAuthorization(t, e, scope, gatestest.Author)
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)
		advance(t, 10*time.Minute)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-result:
			if r.err != nil || r.in == nil {
				t.Fatalf("unexpired approvals refused the claim: %+v", r)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the claim never finished")
		}
		if l := claimLeaks(e, p, run); l.attempts != 1 || l.ordinal != 1 || l.writer != 1 {
			t.Fatalf("claim effects %+v", l)
		}
	})
}
