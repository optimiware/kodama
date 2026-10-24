package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// attemptsTable stands in for the dispatch module's attempt table: the writer
// reservation must commit or roll back together with the attempt row, the
// audit rows and the outbox events of the enclosing transaction.
func attemptsTable(t *testing.T, e *wt.Env) {
	t.Helper()
	ownerExec(t, e, fmt.Sprintf(`CREATE TABLE t013_attempts (tenant_id kodama_id NOT NULL, id text NOT NULL, PRIMARY KEY (tenant_id, id));
		ALTER TABLE t013_attempts ENABLE ROW LEVEL SECURITY; ALTER TABLE t013_attempts FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_all ON t013_attempts USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
		GRANT SELECT, INSERT ON t013_attempts TO %s`, pgx.Identifier{e.DB.RuntimeRole}.Sanitize()))
}

func ownerExec(t *testing.T, e *wt.Env, sql string, args ...any) {
	t.Helper()
	if _, err := e.Migration.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// Test_writer_reservation_atomicity: WS-03 and WS-A02 at the transaction
// boundary the dispatch module joins. Competing reservations yield one winner;
// a rollback also removes the enclosing attempt, audit and outbox changes.
func Test_writer_reservation_atomicity(t *testing.T) {
	ctx := context.Background()
	e := wt.New(t, wt.Config{})
	attemptsTable(t, e)
	w := e.Ready(wt.ScopeA, "wi_1")
	tenant := w.Scope.TenantID

	reserve := func(attempt string) error {
		return storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t013_attempts (tenant_id, id) VALUES ($1, $2)`, string(tenant), attempt); err != nil {
				return err
			}
			// Another module's event in the same transaction.
			if _, err := e.Events.Append(ctx, tx, eventstest.WorkItemTransition(string(tenant), string(w.Scope.ProjectID), "evt_"+attempt, "wi_1", 1)); err != nil {
				return err
			}
			_, err := e.PG.UpdateInTx(ctx, tx, w.Scope, w.ID, workspace.Load{}, workspace.ReserveWriterMutation(workspace.ReserveWriterInput{
				Scope: w.Scope, WorkspaceID: w.ID, AttemptID: domain.AttemptID(attempt), ExpectedTip: w.AcceptedCommit, Actor: workspace.Actor{Type: workspace.ActorService, ID: "dispatcher"},
			}, e.Clock.Now))
			return err
		})
	}

	const n = 12
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = reserve(fmt.Sprintf("att_%d", i)) }()
	}
	wg.Wait()
	winners := 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, workspace.ErrWriterBusy):
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d reservations won", winners)
	}
	if got := e.CountTenant(tenant, `SELECT count(*) FROM t013_attempts`); got != 1 {
		t.Fatalf("%d attempt rows survived; losers must roll back with their reservation", got)
	}
	if got := e.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE event_type = 'work_item.transitioned'`); got != 1 {
		t.Fatalf("%d outbox rows survived", got)
	}
	if got := e.CountSQL(`SELECT count(*) FROM audit WHERE action = 'workspace.writer_reserved' AND resource_id = $1`, string(w.ID)); got != 1 {
		t.Fatalf("%d reservation audit rows", got)
	}
	var holder string
	if err := e.Migration.QueryRow(ctx, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(w.ID)).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	var attempt string
	if err := storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM t013_attempts`).Scan(&attempt)
	}); err != nil || attempt != holder {
		t.Fatalf("the surviving attempt %q is not the writer %q (%v)", attempt, holder, err)
	}

	t.Run("a failure after the reservation rolls back everything", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		attemptsTable(t, e)
		w := e.Ready(wt.ScopeA, "wi_1")
		boom := errors.New("later step failed")
		err := storage.WithTenant(ctx, e.Runtime, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t013_attempts (tenant_id, id) VALUES ($1, 'att_x')`, string(w.Scope.TenantID)); err != nil {
				return err
			}
			if _, err := e.PG.UpdateInTx(ctx, tx, w.Scope, w.ID, workspace.Load{}, workspace.ReserveWriterMutation(workspace.ReserveWriterInput{
				Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_x", ExpectedTip: w.AcceptedCommit, Actor: workspace.Actor{Type: workspace.ActorService, ID: "dispatcher"},
			}, e.Clock.Now)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Writer != nil || cur.Version != w.Version {
			t.Fatalf("a rolled-back reservation is visible: %+v", cur)
		}
		if got := e.CountSQL(`SELECT count(*) FROM audit WHERE action = 'workspace.writer_reserved'`); got != 0 {
			t.Fatalf("%d audit rows survived the rollback", got)
		}
		if got := e.CountTenant(w.Scope.TenantID, `SELECT count(*) FROM t013_attempts`); got != 0 {
			t.Fatalf("%d attempt rows survived", got)
		}
	})

	t.Run("a reservation by a stale observation is refused atomically", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		err := storage.WithTenant(ctx, e.Runtime, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := e.PG.UpdateInTx(ctx, tx, w.Scope, w.ID, workspace.Load{}, workspace.ReserveWriterMutation(workspace.ReserveWriterInput{
				Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_x", ExpectedTip: workspace.Commit("0000000000000000000000000000000000000001"),
				Actor: workspace.Actor{Type: workspace.ActorService, ID: "dispatcher"},
			}, e.Clock.Now))
			return err
		})
		if !errors.Is(err, workspace.ErrStale) {
			t.Fatalf("got %v", err)
		}
	})
}
