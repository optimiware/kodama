package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/identity"
)

// TestReviewerRevoked (DQ-03, DQ-04, IT-10, IT-A07, DQ-A03): the current human
// reviewer authority is checked at decision, at replay and at consumption,
// including when a revocation commits while the transaction waits on a lock.
// Admin, engineer, viewer and nonhuman identities cannot substitute; an author
// who also holds the reviewer role can decide.
func TestReviewerRevoked(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA

	t.Run("only a human reviewer decides", func(t *testing.T) {
		g := requestedGate(t, e)
		for _, who := range []string{gatestest.Admin, gatestest.Engineer, gatestest.Viewer} {
			if _, err := approve(t, e, g, who); !errors.Is(err, gates.ErrDenied) {
				t.Errorf("%s: %v", who, err)
			}
		}
		if _, err := approve(t, e, g, "u034_stranger"); !errors.Is(err, gates.ErrHidden) {
			t.Errorf("a stranger: %v", err)
		}
		// A service identity cannot decide even with the right version and digest.
		_, err := e.Store.Decide(ctx, &gates.DecisionPlan{Scope: scope, GateID: g.ID, Command: gates.DecideCommand{
			Decision: gates.DecisionApprove, Reason: "automatic", Actor: gates.System, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
		}})
		if !errors.Is(err, gates.ErrDenied) {
			t.Errorf("a service identity: %v", err)
		}
		if cur := gateByID(t, e, g); cur.Status != gates.StatusPending || cur.Version != g.Version {
			t.Fatalf("a refused decision changed the gate: %+v", cur)
		}
		// An author who also holds the reviewer role decides.
		if got, err := approve(t, e, g, gatestest.Author); err != nil || got.Decision.By.ID != gatestest.Author {
			t.Fatalf("author-plus-reviewer: %v", err)
		}
	})

	t.Run("revocation blocks the decision", func(t *testing.T) {
		g := requestedGate(t, e)
		e.X.Grant(scope, "u034_temp", identity.RoleReviewer)
		e.X.Revoke(scope, "u034_temp")
		if _, err := approve(t, e, g, "u034_temp"); !errors.Is(err, gates.ErrHidden) {
			t.Fatalf("a revoked reviewer: %v", err)
		}
		// Losing only the reviewer role leaves a member who may not decide.
		e.X.Grant(scope, "u034_demoted", identity.RoleReviewer)
		e.X.Grant(scope, "u034_demoted", identity.RoleViewer)
		e.X.OwnerExec(`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = 'u034_demoted' AND role = 'reviewer'`, string(scope.TenantID))
		e.X.OwnerExec(`UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = 'u034_demoted'`, string(scope.TenantID))
		if _, err := approve(t, e, g, "u034_demoted"); !errors.Is(err, gates.ErrDenied) {
			t.Fatalf("a demoted reviewer: %v", err)
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 0 {
			t.Fatalf("%d audit rows for refused decisions", n)
		}
	})

	t.Run("revocation blocks a keyed replay", func(t *testing.T) {
		g := requestedGate(t, e)
		e.X.Grant(scope, "u034_replayer", identity.RoleReviewer)
		cmd := cmdFor(g, gates.DecisionApprove, "Reviewed the identified source and test evidence.")
		if r, err := decideKeyed(ctx, e, g, "u034_replayer", "key-r", `{"x":1}`, cmd); err != nil || r.Replayed {
			t.Fatalf("decide: %+v %v", r, err)
		}
		e.X.Revoke(scope, "u034_replayer")
		if r, err := decideKeyed(ctx, e, g, "u034_replayer", "key-r", `{"x":1}`, cmd); err == nil || len(r.Body) != 0 {
			t.Fatalf("the stored response was released to a revoked reviewer: %+v %v", r, err)
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d decision audit rows", n)
		}
	})

	t.Run("revocation blocks consumption until restored", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, p.Scope.TenantID)
		g := gateOf(t, e, run, "review")
		e.X.Grant(scope, "u034_leaving", identity.RoleReviewer)
		if _, err := approve(t, e, g, "u034_leaving"); err != nil {
			t.Fatal(err)
		}
		e.X.Revoke(scope, "u034_leaving")
		rep := reconcile(t, e, p.Scope.TenantID)
		if rep.Completed != 0 || rep.Invalidated != 0 {
			t.Fatalf("a revoked reviewer's approval was consumed or invalidated: %+v", rep)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
			t.Fatalf("gate step %s", got)
		}
		if _, err := e.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a successor ran on a revoked approval: %v", err)
		}
		if cur := gateByID(t, e, g); cur.Invalidation != nil || cur.Status != gates.StatusApproved {
			t.Fatalf("an authority check must not invalidate or rewrite the decision: %+v", cur)
		}
		// Current authority is the rule: restoring the reviewer restores it.
		e.X.Grant(scope, "u034_leaving", identity.RoleReviewer)
		if rep := reconcile(t, e, p.Scope.TenantID); rep.Completed != 1 {
			t.Fatalf("after the reviewer was restored: %+v", rep)
		}
		if in := e.MustClaim(scope.TenantID); in.StepID != "deploy" {
			t.Fatalf("claimed %s", in.StepID)
		}
	})

	t.Run("a revocation that commits while a decision waits on the gate lock", func(t *testing.T) {
		g := requestedGate(t, e)
		e.X.Grant(scope, "u034_racer", identity.RoleReviewer)
		release, held := holdGateLock(t, e, g)
		done := make(chan error, 1)
		go func() {
			_, err := approve(t, e, g, "u034_racer")
			done <- err
		}()
		<-held
		time.Sleep(300 * time.Millisecond) // the decision is now waiting inside its transaction
		e.X.Revoke(scope, "u034_racer")
		release()
		if err := <-done; !errors.Is(err, gates.ErrHidden) && !errors.Is(err, gates.ErrDenied) {
			t.Fatalf("the decision used the authority it had before the lock wait: %v", err)
		}
		if cur := gateByID(t, e, g); cur.Status != gates.StatusPending || cur.Decision != nil {
			t.Fatalf("a revoked reviewer decided after waiting: %+v", cur)
		}
	})

	t.Run("a revocation that commits while consumption waits on a lock", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, p.Scope.TenantID)
		g := gateOf(t, e, run, "review")
		e.X.Grant(scope, "u034_waiter", identity.RoleReviewer)
		if _, err := approve(t, e, g, "u034_waiter"); err != nil {
			t.Fatal(err)
		}
		release, held := holdGateLock(t, e, g)
		done := make(chan error, 1)
		go func() {
			_, err := e.Store.CompleteStep(ctx, scope, g.ID)
			done <- err
		}()
		<-held
		time.Sleep(300 * time.Millisecond)
		e.X.Revoke(scope, "u034_waiter")
		release()
		if err := <-done; !errors.Is(err, gates.ErrReviewerAuthority) {
			t.Fatalf("consumption used the authority it had before the lock wait: %v", err)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
			t.Fatalf("gate step %s", got)
		}
	})

	t.Run("a revoked author-plus-reviewer cannot decide", func(t *testing.T) {
		g := requestedGate(t, e)
		e.X.Revoke(scope, gatestest.Author)
		if _, err := approve(t, e, g, gatestest.Author); err == nil {
			t.Fatalf("a revoked author decided")
		}
	})
}

// holdGateLock locks the gate row FOR UPDATE in its own transaction and returns
// a release function and a channel closed once the lock is held.
func holdGateLock(t *testing.T, e *gatestest.Env, g *gates.Gate) (release func(), held <-chan struct{}) {
	t.Helper()
	ready := make(chan struct{})
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		err := e.X.RuntimeTx(g.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var one int
			if err := tx.QueryRow(ctx, `SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
				string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.ID)).Scan(&one); err != nil {
				return err
			}
			close(ready)
			<-stop
			return nil
		})
		if err != nil {
			t.Errorf("hold gate lock: %v", err)
		}
	}()
	<-ready
	return func() { close(stop); <-finished }, ready
}
