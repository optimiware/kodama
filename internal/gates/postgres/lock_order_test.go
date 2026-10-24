package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const writeRun = "workspace: {access: write, from: run}"

// gatedWriter is test -> review(gate) -> deploy(writer), with a sibling gate
// "audit" over the same evidence that protects nothing. Both gates are
// requested; review is approved and consumed, so deploy is claimable.
func gatedWriter(t *testing.T, e *gatestest.Env, scope domain.Scope) (executiontest.Project, *execution.Run, *gates.Gate, *gates.Gate) {
	t.Helper()
	ctx := context.Background()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		gatestest.Gate("audit", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", writeRun))
	run := e.MustAdmit(p, def, nil)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	if rep, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Requested != 2 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	review, err := e.Store.ByStep(ctx, scope, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	audit, err := e.Store.ByStep(ctx, scope, run.ID, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, review.ID, approveCmd(review)); err != nil {
		t.Fatal(err)
	}
	if rep, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Completed != 1 {
		t.Fatalf("consume: %+v %v", rep, err)
	}
	return p, run, review, audit
}

// leaks reports the effects a refused or rolled-back claim must not leave: an
// attempt, a consumed attempt ordinal, a capacity reservation or a writer slot.
type leaks struct{ attempts, ordinal, reservations, writer int }

// regrantReviewer restores the reviewer role, whether or not it was revoked.
func regrantReviewer(e *gatestest.Env) {
	// Revocation is per tenant and removes every binding of the user, so clear
	// once before re-granting in each scope (the bindings may or may not exist).
	e.X.Revoke(gatestest.ScopeA, gatestest.Reviewer)
	for _, s := range []domain.Scope{gatestest.ScopeA, gatestest.ScopeA2} {
		e.X.Grant(s, gatestest.Reviewer, identity.RoleReviewer)
	}
}

func claimLeaks(e *gatestest.Env, p executiontest.Project, run *execution.Run) leaks {
	var l leaks
	l.attempts = count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'deploy'`, string(run.ID))
	e.X.Scalar(&l.ordinal, `SELECT attempts_started FROM step_runs WHERE run_id = $1 AND step_id = 'deploy'`, string(run.ID))
	l.reservations = count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
	l.writer = count(e, `SELECT count(*) FROM workspaces WHERE id = $1 AND writer_attempt_id IS NOT NULL`, string(p.Workspace))
	return l
}

// waitBlocked waits until some other backend is waiting for the transaction
// hold, which holds the row lock under test. Other roles' activity rows are not
// readable by the test connection, so the wait is observed through pg_locks.
func waitBlocked(t *testing.T, e *gatestest.Env, hold pgx.Tx) {
	t.Helper()
	var xid string
	if err := hold.QueryRow(context.Background(), `SELECT pg_current_xact_id()::xid::text`).Scan(&xid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if count(e, `SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype = 'transactionid' AND transactionid::text = $1`, xid) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend ever waited for the held gate lock (transaction %s)", xid)
}

type claimResult struct {
	in  *execution.AttemptSpec
	err error
}

func claimAsync(e *gatestest.Env, tenant domain.TenantID) <-chan claimResult {
	ch := make(chan claimResult, 1)
	go func() {
		in, err := e.Claim(tenant)
		ch <- claimResult{in, err}
	}()
	return ch
}

func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	return (errors.As(err, &pg) && pg.Code == "40P01") || (err != nil && strings.Contains(strings.ToLower(err.Error()), "deadlock"))
}

// TestGateClaimLockOrder (PX-05, PX-13, DQ-04, SD-04): a dispatch transaction
// locks the run's bound gates before the claim mutation can read any
// authorization revision, so decisions, invalidations and revocations contend
// with claims in one acyclic order. A claim refused after waiting leaves no
// attempt, ordinal, capacity reservation or writer slot, and racing claims,
// decisions and revocations converge without a deadlock.
func TestGateClaimLockOrder(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA

	t.Run("the claim waits for a gate lock before it holds an authorization lock", func(t *testing.T) {
		p, run, review, _ := gatedWriter(t, e, scope)
		before := claimLeaks(e, p, run)

		// An in-flight decision or invalidation holds the gate FOR UPDATE.
		hold, err := e.X.Migration.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(review.ID)); err != nil {
			t.Fatal(err)
		}
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)

		// The blocked claim holds no authorization revision lock: a revocation
		// of the run's requester and of the reviewer proceeds at once. Were the
		// authorization read before the gate lock, this would wait for the claim
		// and the claim for the gate: an inversion against a decision.
		rev, err := e.X.Migration.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rev.Rollback(ctx)
		if _, err := rev.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
			t.Fatal(err)
		}
		for _, user := range []string{gatestest.Engineer, gatestest.Reviewer} {
			if _, err := rev.Exec(ctx, `UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`, string(scope.TenantID), user); err != nil {
				t.Fatalf("the blocked claim holds the authorization lock of %s: %v", user, err)
			}
		}
		if _, err := rev.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, string(scope.TenantID), gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		if err := rev.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		// The claim re-reads current authority after the wait: the reviewer was
		// revoked, so the claim is refused and nothing leaks.
		select {
		case r := <-result:
			if !errors.Is(r.err, execution.ErrNothingRunnable) {
				t.Fatalf("claim after the reviewer was revoked: %+v", r)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the claim never finished")
		}
		if after := claimLeaks(e, p, run); after != before {
			t.Fatalf("a refused claim leaked: %+v -> %+v", before, after)
		}
		regrantReviewer(e)
	})

	t.Run("an applicability failure after the wait rolls the claim back", func(t *testing.T) {
		p, run, review, _ := gatedWriter(t, e, scope)
		before := claimLeaks(e, p, run)
		hold, err := e.X.Migration.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(review.ID)); err != nil {
			t.Fatal(err)
		}
		result := claimAsync(e, scope.TenantID)
		waitBlocked(t, e, hold)
		// The evidence is removed while the claim waits. (A specification edit
		// would itself wait: the claim already holds the work item lock, which
		// precedes the gate lock.)
		e.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE run_id = $1 AND name = 'report'`, string(run.ID))
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if r := <-result; !errors.Is(r.err, execution.ErrNothingRunnable) {
			t.Fatalf("claim over removed evidence: %+v", r)
		}
		if after := claimLeaks(e, p, run); after != before {
			t.Fatalf("a refused claim leaked: %+v -> %+v", before, after)
		}
	})

	t.Run("racing claims, a decision and a revocation converge without deadlock", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			i := i
			t.Run(fmt.Sprintf("iteration %d", i), func(t *testing.T) {
				regrantReviewer(e)
				p, run, _, audit := gatedWriter(t, e, scope)
				before := claimLeaks(e, p, run)
				var wg sync.WaitGroup
				errs := make(chan error, 8)
				start := make(chan struct{})
				launch := func(f func() error) {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						if err := f(); err != nil {
							errs <- err
						}
					}()
				}
				for c := 0; c < 3; c++ {
					launch(func() error {
						_, err := e.Claim(scope.TenantID)
						if errors.Is(err, execution.ErrNothingRunnable) {
							return nil
						}
						return err
					})
				}
				launch(func() error {
					_, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, audit.ID, approveCmd(audit))
					switch {
					case err == nil, errors.Is(err, gates.ErrDenied), errors.Is(err, gates.ErrHidden):
						return nil
					}
					return err
				})
				launch(func() error { e.X.Revoke(scope, gatestest.Reviewer); return nil })
				done := make(chan struct{})
				go func() { wg.Wait(); close(done) }()
				close(start)
				select {
				case <-done:
				case <-time.After(60 * time.Second):
					t.Fatal("racing transactions did not finish: lock inversion")
				}
				close(errs)
				for err := range errs {
					if isDeadlock(err) {
						t.Fatalf("a lock inversion deadlocked: %v", err)
					}
					t.Fatalf("racing operation: %v", err)
				}
				// Convergence: the claim either won (one attempt, one ordinal, one
				// reservation, the writer slot held) or none of those exist.
				l := claimLeaks(e, p, run)
				l.reservations -= before.reservations
				switch {
				case l.attempts == 0 && l.ordinal == 0 && l.reservations == 0 && l.writer == 0:
				case l.attempts == 1 && l.ordinal == 1 && l.reservations == 1 && l.writer == 1:
				default:
					t.Fatalf("iteration %d: inconsistent claim effects %+v", i, l)
				}
			})
		}
	})

}
