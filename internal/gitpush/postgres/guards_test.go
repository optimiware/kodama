package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
	"github.com/optimiware/kodama/internal/storage"
)

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func countTenant(t *testing.T, e *gitpushtest.PGEnv, tenant domain.TenantID, sql string, args ...any) int {
	t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Test_tenant_isolation_and_guards: row level security, the composite ownership
// key, the runtime role's least privilege and the database-enforced entry and
// record invariants, against rows the use cases wrote.
func Test_tenant_isolation_and_guards(t *testing.T) {
	ctx := context.Background()
	e := gitpushtest.NewPGEnv(t)
	f := gitpushtest.New(t, func(testing.TB) gitpushtest.Env { return e })
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	// An applied record and an open one.
	appliedReq := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_g1", false, gitpushtest.Create(gitpushtest.Branch("done"), gitpushtest.OID("1")))
	applied := f.MustAuthorize(appliedReq)
	if err := f.Intent(applied.PushID, appliedReq); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Outcome(applied.PushID, appliedReq, gitpushtest.Branch("done")); err != nil {
		t.Fatal(err)
	}
	openReq := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_g2", false, gitpushtest.Create(gitpushtest.Branch("open"), gitpushtest.OID("1")))
	open := f.MustAuthorize(openReq)
	A, B := gitpushtest.TenantA, gitpushtest.TenantB

	exec := func(tenant domain.TenantID, sql string, args ...any) error {
		return storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}

	t.Run("RLS hides every row from other tenants and from no tenant", func(t *testing.T) {
		for _, table := range []string{"git_push_records", "git_push_entries"} {
			var n int
			if err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s without tenant context: %d rows, %v", table, n, err)
			}
			if got := countTenant(t, e, B, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, string(A)); got != 0 {
				t.Fatalf("tenant B sees %d %s rows of tenant A", got, table)
			}
			if got := countTenant(t, e, A, `SELECT count(*) FROM `+table); got == 0 {
				t.Fatalf("tenant A sees no %s rows", table)
			}
		}
		err := exec(B, `INSERT INTO git_journal_heads (tenant_id, repository_id, head, updated_at) VALUES ($1, $2, 1, now())`, string(A), gitpushtest.RepoMain)
		if sqlState(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v", err)
		}
		// The store addresses by tenant: tenant B cannot reload tenant A's record.
		if _, err := f.Svc.Reload(ctx, gitpushtest.ScopeB(), gitpushtest.RepoMain, applied.PushID); err == nil {
			t.Fatalf("tenant B reloaded tenant A's record: %v", err)
		}
	})

	t.Run("the runtime role has least privilege", func(t *testing.T) {
		for _, sql := range []string{
			`DELETE FROM git_push_records WHERE tenant_id = $1`,
			`DELETE FROM git_push_entries WHERE tenant_id = $1`,
			`DELETE FROM git_journal_heads WHERE tenant_id = $1`,
			`UPDATE git_push_records SET grant_id = 'other' WHERE tenant_id = $1`,
			`UPDATE git_push_records SET receive_id = 'other' WHERE tenant_id = $1`,
			`UPDATE git_push_entries SET ref = 'refs/heads/other' WHERE tenant_id = $1`,
			`UPDATE git_push_entries SET new_value = old_value WHERE tenant_id = $1`,
			`UPDATE git_push_entries SET kind = 'rewrite' WHERE tenant_id = $1`,
			`UPDATE git_push_entries SET applied_seq = 1 WHERE tenant_id = $1`, // the apply order is the database's
		} {
			if got := sqlState(exec(A, sql, string(A))); got != "42501" {
				t.Errorf("%s: sqlstate %q", sql, got)
			}
		}
	})

	t.Run("the apply order is numbered by the database, once", func(t *testing.T) {
		if got := countTenant(t, e, A, `SELECT count(*) FROM git_push_entries WHERE state = 'applied' AND applied_seq IS NULL`); got != 0 {
			t.Fatalf("%d applied entries without an order", got)
		}
		if got := countTenant(t, e, A, `SELECT count(*) FROM git_push_entries WHERE state <> 'applied' AND applied_seq IS NOT NULL`); got != 0 {
			t.Fatalf("%d entries that are not applied have an order", got)
		}
		if got := countTenant(t, e, A, `SELECT count(*) FROM git_push_entries WHERE state = 'applied'`); got == 0 {
			t.Fatal("the fixture has no applied entry")
		}
		// Not even the owner can write an order: the trigger keeps the database's.
		if _, err := e.Owner.Exec(ctx, `UPDATE git_push_entries SET applied_seq = 1`); err != nil {
			t.Fatal(err)
		}
		if got := countTenant(t, e, A, `SELECT count(*) FROM git_push_entries WHERE applied_seq = 1`); got > 1 {
			t.Fatalf("%d entries took an order from a plain update", got)
		}
		if got := countTenant(t, e, A, `SELECT count(*) FROM git_push_entries WHERE state <> 'applied' AND applied_seq IS NOT NULL`); got != 0 {
			t.Fatalf("%d entries that are not applied have an order", got)
		}
	})

	t.Run("the database refuses an illegal entry transition", func(t *testing.T) {
		for name, sql := range map[string]string{
			"applied entries are final":     `UPDATE git_push_entries SET state = 'authorized', reason = NULL WHERE tenant_id = $1 AND push_id = $2`,
			"applied cannot be refused":     `UPDATE git_push_entries SET state = 'refused', reason = 'no_commit_intent' WHERE tenant_id = $1 AND push_id = $2`,
			"applied cannot become unknown": `UPDATE git_push_entries SET state = 'unknown', reason = 'evidence_missing' WHERE tenant_id = $1 AND push_id = $2`,
			"a reason on an applied entry":  `UPDATE git_push_entries SET reason = 'ref_unchanged' WHERE tenant_id = $1 AND push_id = $2`,
		} {
			if got := sqlState(exec(A, sql, string(A), applied.PushID)); got != "23514" {
				t.Errorf("%s: sqlstate %q", name, got)
			}
		}
		// authorized cannot skip committing to applied, and a refusal states its reason.
		if got := sqlState(exec(A, `UPDATE git_push_entries SET state = 'applied' WHERE tenant_id = $1 AND push_id = $2`, string(A), open.PushID)); got != "23514" {
			t.Errorf("authorized -> applied: sqlstate %q", got)
		}
		if got := sqlState(exec(A, `UPDATE git_push_entries SET state = 'refused' WHERE tenant_id = $1 AND push_id = $2`, string(A), open.PushID)); got != "23514" {
			t.Errorf("a refusal without a reason: sqlstate %q", got)
		}
		if got := sqlState(exec(A, `UPDATE git_push_entries SET state = 'committing' WHERE tenant_id = $1 AND push_id = $2`, string(A), open.PushID)); got != "" {
			t.Errorf("authorized -> committing is legal: sqlstate %q", got)
		}
	})

	t.Run("the database refuses an illegal record change", func(t *testing.T) {
		for name, sql := range map[string]string{
			"a status without a version": `UPDATE git_push_records SET status = 'refused', updated_at = now() WHERE tenant_id = $1 AND id = $2`,
			"a version that skips":       `UPDATE git_push_records SET version = version + 2, updated_at = now() WHERE tenant_id = $1 AND id = $2`,
			"a set report obligation":    `UPDATE git_push_records SET empty_report_pending = true WHERE tenant_id = $1 AND id = $2`,
		} {
			if got := sqlState(exec(A, sql, string(A), open.PushID)); got != "23514" {
				t.Errorf("%s: sqlstate %q", name, got)
			}
		}
	})

	t.Run("the known journal head never decreases", func(t *testing.T) {
		if err := exec(A, `INSERT INTO git_journal_heads (tenant_id, repository_id, head, updated_at) VALUES ($1, $2, 7, now())`, string(A), gitpushtest.RepoDirect); err != nil {
			t.Fatal(err)
		}
		if got := sqlState(exec(A, `UPDATE git_journal_heads SET head = 3, updated_at = now() WHERE tenant_id = $1 AND repository_id = $2`, string(A), gitpushtest.RepoDirect)); got != "23514" {
			t.Fatalf("lowering the head: sqlstate %q", got)
		}
		var head int64
		err := storage.WithTenant(ctx, e.Runtime, A, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT head FROM git_journal_heads WHERE tenant_id = $1 AND repository_id = $2`, string(A), gitpushtest.RepoDirect).Scan(&head)
		})
		if err != nil || head != 7 {
			t.Fatalf("head = %d, %v", head, err)
		}
	})

	t.Run("a record belongs to an existing repository of its project", func(t *testing.T) {
		insert := func(project, repo string) error {
			return exec(A, `INSERT INTO git_push_records (tenant_id, project_id, id, repository_id, receive_id, grant_id, grant_kind, actor_type, actor_id,
				repository_empty, request_digest, deadline, status, created_at, updated_at)
				VALUES ($1, $2, 'push_fk', $3, 'rcv_fk', 'grant_fk', 'human', 'user', 'usr_fk', false, repeat('a', 64), now(), 'authorized', now(), now())`,
				string(A), project, repo)
		}
		if got := sqlState(insert(string(gitpushtest.ProjectA), "r130_missing")); got != "23503" {
			t.Errorf("an unknown repository: sqlstate %q", got)
		}
		if got := sqlState(insert(string(gitpushtest.ProjectX), gitpushtest.RepoMain)); got != "23503" {
			t.Errorf("a repository of another project: sqlstate %q", got)
		}
		if got := sqlState(insert(string(gitpushtest.ProjectA), gitpushtest.RepoMain)); got != "" {
			t.Errorf("a valid record: sqlstate %q", got)
		}
	})

	t.Run("a receive is unique per repository", func(t *testing.T) {
		// The same receive of the same repository is one record (replay), of
		// another repository another record: the unique key is (repository, receive).
		again := f.MustAuthorize(openReq)
		if again.PushID != open.PushID {
			t.Fatalf("replay created another record: %s != %s", again.PushID, open.PushID)
		}
		if n := countTenant(t, e, A, `SELECT count(*) FROM git_push_records WHERE receive_id = 'rcv_g2'`); n != 1 {
			t.Fatalf("%d records of one receive", n)
		}
	})

	t.Run("a refused receive is final in the database too", func(t *testing.T) {
		denied := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_g3", false, gitpushtest.Advance(gitpushtest.Branch("main"), gitpushtest.OID("1"), gitpushtest.OID("2")))
		resp := f.MustAuthorize(denied)
		if resp.Allowed() {
			t.Fatal("the default branch was not protected")
		}
		if got := sqlState(exec(A, `UPDATE git_push_entries SET state = 'committing', reason = NULL WHERE tenant_id = $1 AND push_id = $2`, string(A), resp.PushID)); got != "23514" {
			t.Fatalf("refused -> committing: sqlstate %q", got)
		}
		rec := f.Record(gitpushtest.RepoMain, resp.PushID)
		if rec.Status != gitv1.PushRefused {
			t.Fatalf("status %s", rec.Status)
		}
	})
}

func waitsFor(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s did not wait for the lock held by the other transaction (returned %v)", what, err)
	case <-time.After(500 * time.Millisecond):
	}
}

// Test_postgres_commit_intent_serializes_with_cancellation holds the cancelling
// transaction open (its revocation of the attempt's grants uncommitted): the
// commit intent blocks on the grant row inside its own transaction and, once the
// cancellation commits, is refused. Without the row lock it would succeed.
func Test_postgres_commit_intent_serializes_with_cancellation(t *testing.T) {
	ctx := context.Background()
	e := gitpushtest.NewPGEnv(t)
	f := gitpushtest.New(t, func(testing.TB) gitpushtest.Env { return e })
	att := f.Attempt("att_c", "run_c", gitpushtest.RepoMain)
	agent := gitpushtest.Agent("wi_1", "run_c", "work")
	req := gitpushtest.Request(gitpushtest.RepoMain, att, "rcv_c1", false, gitpushtest.Create(agent, gitpushtest.OID("1")))
	resp := f.MustAuthorize(req)

	locked, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		cancelled <- storage.WithTenant(ctx, e.Runtime, gitpushtest.TenantA, func(ctx context.Context, tx pgx.Tx) error {
			n, err := gitauthpg.RevokeAttempt(ctx, tx, gitpushtest.ScopeA(), "att_c", time.Now())
			if err != nil || n != 1 {
				return errors.Join(err, errors.New("the grant was not revoked"))
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-cancelled:
		t.Fatalf("cancellation: %v", err)
	}
	intent := make(chan error, 1)
	go func() { intent <- f.Intent(resp.PushID, req) }()
	waitsFor(t, "the commit intent", intent)
	close(release)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	var ce *gitv1.ContractError
	if err := <-intent; !errors.As(err, &ce) || ce.Code != gitv1.CodeAttemptInactive {
		t.Fatalf("intent after the cancellation committed: %v", err)
	}
	rec := f.Record(gitpushtest.RepoMain, resp.PushID)
	if rec.Entries[0].State != gitv1.EntryAuthorized {
		t.Fatalf("entry %+v", rec.Entries[0])
	}
}

// Test_postgres_authorization_serializes_with_repository_change holds an
// archive of the repository uncommitted: the authorization, which already read
// the repository as ready, blocks on the row inside its transaction and, once the
// archive commits, is refused as retryable and records nothing.
func Test_postgres_authorization_serializes_with_repository_change(t *testing.T) {
	ctx := context.Background()
	e := gitpushtest.NewPGEnv(t)
	f := gitpushtest.New(t, func(testing.TB) gitpushtest.Env { return e })
	eng := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	req := gitpushtest.Request(gitpushtest.RepoMain, eng, "rcv_a1", false, gitpushtest.Create(gitpushtest.Branch("x"), gitpushtest.OID("1")))

	tx, err := e.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE repositories SET status = 'archived', reason = 'archived', version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, string(gitpushtest.TenantA), gitpushtest.RepoMain); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.Authorize(req); done <- err }()
	waitsFor(t, "the authorization", done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, gitpush.ErrUnavailable) {
		t.Fatalf("authorization after the archive committed: %v", err)
	}
	page, err := f.Svc.List(ctx, "usr_reader", gitpushtest.ScopeA(), gitpushtest.RepoMain, gitpush.ListQuery{Limit: 10})
	if err != nil || len(page) != 0 {
		t.Fatalf("records after the refused authorization: %d %v", len(page), err)
	}
}
