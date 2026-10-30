package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
	"github.com/optimiware/kodama/internal/storage"
)

// Test_postgres_operator_actions_guarded: migration 015 relaxes the terminal
// unknown state and the monotone journal head only for the operator actions,
// each recorded in the same transaction; a plain update, an earlier
// transaction's row and a later one are refused (real SQL).
func Test_postgres_operator_actions_guarded(t *testing.T) {
	ctx := context.Background()
	e := gitpushtest.NewPGEnv(t)
	f := gitpushtest.New(t, func(testing.TB) gitpushtest.Env { return e })
	A := gitpushtest.TenantA
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	unknown := func(receive, branch string) gitv1.PushAuthorizeResponse {
		req := gitpushtest.Request(gitpushtest.RepoMain, grant, receive, false, gitpushtest.Create(gitpushtest.Branch(branch), gitpushtest.OID("1")))
		resp := f.MustAuthorize(req)
		if err := f.Intent(resp.PushID, req); err != nil {
			t.Fatal(err)
		}
		if err := f.Reconcile(gitpushtest.RepoMain); err != nil { // no evidence: unknown
			t.Fatal(err)
		}
		if rec := f.Record(gitpushtest.RepoMain, resp.PushID); rec.Status != gitv1.PushUnknown {
			t.Fatalf("status %s", rec.Status)
		}
		return resp
	}
	resp := unknown("rcv_op1", "op")
	stale := unknown("rcv_op2", "op2")
	forged := unknown("rcv_op3", "op3")
	run := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return storage.WithTenant(ctx, e.Runtime, A, fn)
	}
	exec := func(tx pgx.Tx, ctx context.Context, sql string, args ...any) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}
	insertResolution := func(tx pgx.Tx, ctx context.Context, pushID, ref, state string) error {
		return exec(tx, ctx, `INSERT INTO git_push_resolutions (tenant_id, push_id, ordinal, project_id, repository_id, ref, resolution, reason, resolved_by, resolved_at)
			VALUES ($1, $2, 0, $3, $4, $5, $6, 'inspected', 'usr_admin', now())`,
			string(A), pushID, string(gitpushtest.ProjectA), gitpushtest.RepoMain, ref, state)
	}
	move := func(state string) string {
		if state == "applied" {
			return `UPDATE git_push_entries SET state = 'applied', reason = NULL WHERE tenant_id = $1 AND push_id = $2`
		}
		return `UPDATE git_push_entries SET state = 'refused', reason = 'ref_unchanged' WHERE tenant_id = $1 AND push_id = $2`
	}

	t.Run("a plain update leaves unknown for nothing", func(t *testing.T) {
		for _, s := range []string{"applied", "refused"} {
			err := run(func(ctx context.Context, tx pgx.Tx) error { return exec(tx, ctx, move(s), string(A), resp.PushID) })
			if sqlState(err) != "23514" {
				t.Fatalf("unknown -> %s without a resolution: %v", s, err)
			}
		}
		if err := run(func(ctx context.Context, tx pgx.Tx) error {
			return exec(tx, ctx, `UPDATE git_push_entries SET state = 'authorized', reason = NULL WHERE tenant_id = $1 AND push_id = $2`, string(A), resp.PushID)
		}); sqlState(err) != "23514" {
			t.Fatalf("unknown -> authorized: %v", err)
		}
	})

	t.Run("a resolution of another state or an earlier transaction does not count", func(t *testing.T) {
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			if err := insertResolution(tx, ctx, resp.PushID, gitpushtest.Branch("op"), "refused"); err != nil {
				return err
			}
			return exec(tx, ctx, move("applied"), string(A), resp.PushID)
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a refused resolution moves the entry to applied: %v", err)
		}
		// A committed resolution row with no change cannot be spent by a later transaction.
		if err := run(func(ctx context.Context, tx pgx.Tx) error {
			return insertResolution(tx, ctx, stale.PushID, gitpushtest.Branch("op2"), "applied")
		}); err != nil {
			t.Fatal(err)
		}
		err = run(func(ctx context.Context, tx pgx.Tx) error {
			return exec(tx, ctx, move("applied"), string(A), stale.PushID)
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a resolution from an earlier transaction: %v", err)
		}
	})

	t.Run("a row cannot carry another transaction's identity", func(t *testing.T) {
		resolutionSQL := `INSERT INTO git_push_resolutions (tenant_id, push_id, ordinal, project_id, repository_id, ref, resolution, reason, resolved_by, resolved_at, txid)
			VALUES ($1, $2, 0, $3, $4, $5, 'applied', 'forged', 'usr_x', now(), 1)`
		resetSQL := `INSERT INTO git_journal_resets (tenant_id, repository_id, from_head, to_head, reset_by, reason, reset_at, txid)
			VALUES ($1, $2, 9, 1, 'usr_x', 'forged', now(), 1)`
		resolutionArgs := []any{string(A), forged.PushID, string(gitpushtest.ProjectA), gitpushtest.RepoMain, gitpushtest.Branch("op3")}
		resetArgs := []any{string(A), "r130_other"}
		// The runtime role cannot even name the column.
		for _, c := range []struct {
			sql  string
			args []any
		}{{resolutionSQL, resolutionArgs}, {resetSQL, resetArgs}} {
			err := run(func(ctx context.Context, tx pgx.Tx) error { return exec(tx, ctx, c.sql, c.args...) })
			if sqlState(err) != "42501" {
				t.Errorf("a runtime insert naming txid: %v", err)
			}
		}
		// Even the owner (no column grants) cannot choose it: the trigger assigns it.
		for _, c := range []struct {
			sql  string
			args []any
		}{{resolutionSQL, resolutionArgs}, {resetSQL, resetArgs}} {
			var stored, current int64
			tx, err := e.Owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = tx.QueryRow(ctx, c.sql+` RETURNING txid, txid_current()`, c.args...).Scan(&stored, &current)
			_ = tx.Rollback(ctx)
			if err != nil || stored == 1 || stored != current {
				t.Errorf("the stored identity is %d, the transaction's %d (%v)", stored, current, err)
			}
		}
		// Two connections: another transaction's uncommitted row grants nothing, and
		// neither does a committed one forged by the owner for another transaction.
		release := make(chan struct{})
		inserted := make(chan error, 1)
		done := make(chan error, 1)
		go func() {
			done <- run(func(ctx context.Context, tx pgx.Tx) error {
				err := insertResolution(tx, ctx, forged.PushID, gitpushtest.Branch("op3"), "applied")
				inserted <- err
				<-release
				return err
			})
		}()
		if err := <-inserted; err != nil {
			t.Fatal(err)
		}
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			return exec(tx, ctx, move("applied"), string(A), forged.PushID)
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a concurrent transaction used another transaction's resolution: %v", err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		err = run(func(ctx context.Context, tx pgx.Tx) error {
			return exec(tx, ctx, move("applied"), string(A), forged.PushID)
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a committed resolution of an earlier transaction was reused: %v", err)
		}
	})

	t.Run("resolution rows are immutable and the runtime role cannot rewrite them", func(t *testing.T) {
		for _, sql := range []string{
			`UPDATE git_push_resolutions SET reason = 'other' WHERE tenant_id = $1`,
			`DELETE FROM git_push_resolutions WHERE tenant_id = $1`,
			`UPDATE git_journal_resets SET reason = 'other' WHERE tenant_id = $1`,
			`DELETE FROM git_journal_resets WHERE tenant_id = $1`,
		} {
			err := run(func(ctx context.Context, tx pgx.Tx) error { return exec(tx, ctx, sql, string(A)) })
			if sqlState(err) != "42501" {
				t.Errorf("%s: %v", sql, err)
			}
		}
	})

	t.Run("the use case resolves through the same transaction", func(t *testing.T) {
		got, err := f.Svc.ResolveUnknown(ctx, gitpushResolve(f, resp.PushID))
		if err != nil || got.Record.Status != gitv1.PushRefused {
			t.Fatalf("%+v %v", got, err)
		}
		if n := countTenant(t, e, A, `SELECT count(*) FROM git_push_resolutions WHERE push_id = $1`, resp.PushID); n != 1 {
			t.Fatalf("%d resolution rows", n)
		}
	})

	t.Run("the journal head decreases only with a recorded reset", func(t *testing.T) {
		repo := gitpushtest.RepoDirect
		if err := run(func(ctx context.Context, tx pgx.Tx) error {
			return exec(tx, ctx, `INSERT INTO git_journal_heads (tenant_id, repository_id, head, updated_at) VALUES ($1, $2, 9, now())`, string(A), repo)
		}); err != nil {
			t.Fatal(err)
		}
		lower := func(tx pgx.Tx, ctx context.Context, to int) error {
			return exec(tx, ctx, `UPDATE git_journal_heads SET head = $3, updated_at = now() WHERE tenant_id = $1 AND repository_id = $2`, string(A), repo, to)
		}
		if err := run(func(ctx context.Context, tx pgx.Tx) error { return lower(tx, ctx, 4) }); sqlState(err) != "23514" {
			t.Fatalf("a plain decrease: %v", err)
		}
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			if err := exec(tx, ctx, `INSERT INTO git_journal_resets (tenant_id, repository_id, from_head, to_head, reset_by, reason, reset_at)
				VALUES ($1, $2, 9, 4, 'usr_admin', 'restored', now())`, string(A), repo); err != nil {
				return err
			}
			return lower(tx, ctx, 3) // another value than the recorded reset
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a decrease to a head the reset does not name: %v", err)
		}
		changed, err := f.Svc.AcknowledgeRestore(ctx, gitpushtest.ScopeA(), repo, 4, "usr_admin", "restored from snapshot")
		if err != nil || !changed {
			t.Fatalf("acknowledge: %v %v", changed, err)
		}
		if n := countTenant(t, e, A, `SELECT count(*) FROM git_journal_resets WHERE repository_id = $1 AND to_head = 4`, repo); n != 1 {
			t.Fatalf("%d reset rows", n)
		}
		if n := countTenant(t, e, A, `SELECT count(*) FROM audit WHERE action = 'git.journal.head_reset' AND resource_id = $1`, repo); n != 1 {
			t.Fatalf("%d audit rows", n)
		}
	})
}

func gitpushResolve(f *gitpushtest.Fixture, pushID string) gitpush.ResolveCommand {
	v, err := f.Svc.GetVersioned(context.Background(), "usr_reader", gitpushtest.ScopeA(), gitpushtest.RepoMain, pushID)
	if err != nil {
		panic(err)
	}
	return gitpush.ResolveCommand{Principal: "usr_admin", Scope: gitpushtest.ScopeA(), RepositoryID: gitpushtest.RepoMain, PushID: pushID,
		Ref: gitpushtest.Branch("op"), Resolution: gitv1.EntryRefused, Reason: "inspected: the ref does not exist", Expected: v.Version}
}
