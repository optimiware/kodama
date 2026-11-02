package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	gitpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var bg = context.Background()

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// TestPostgresGitauthContract runs the shared grant-authority contract on the
// PostgreSQL adapter: real SQL, row-level security and row locks. It is recorded
// in the integration layer and skips in the unit layer and without PostgreSQL.
func TestPostgresGitauthContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	gitauthtest.Contract(t, gitauthtest.OpenPostgres)
}

// TestSeedIssuanceVersusConsumptionPostgres: the advisory seed lock taken by both
// issuance and ConsumeSeedIn serializes them across connections: issuance never
// runs between a receive's consumption and its push record, and a receive never
// consumes a seed a newer issuance revoked.
func TestSeedIssuanceVersusConsumptionPostgres(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	gitauthtest.Run(t, gitauthtest.OpenPostgres, "seed_issuance_vs_consumption")
}

// TestMigrationReapply: the migration applies after the baseline, reapplies as
// a no-op, rejects a modified applied migration and creates the constraints, the
// guard trigger, the forced row-level security and the least-privilege grants
// the module relies on.
func TestMigrationReapply(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	manifest, err := storage.NewManifest(storage.BaseContribution(), gitpg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(bg, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Applied[len(first.Applied)-1]; got != gitpg.GrantsMigrationID {
		t.Fatalf("applied %v, the last must be %s", first.Applied, gitpg.GrantsMigrationID)
	}
	second, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := gitpg.Migrations()
	changed.Migrations[0].SQL += "\n-- edited"
	bad, err := storage.NewManifest(storage.BaseContribution(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(bg, owner, bad, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}

	for _, name := range []string{
		"git_grants_pkey", "git_grants_tenant_id_project_id_fkey", "git_grants_credential_unique", "git_grants_issue_key_unique",
		"git_grants_kind_known", "git_grants_actor_type_known", "git_grants_repositories_bounded", "git_grants_claims_object",
		"git_grants_credential_digest_length", "git_grants_issue_key_length", "git_grants_claims_match", "git_grants_kind_shape",
		"git_grants_expiry_order", "git_grants_lifetime_bounded", "git_grants_consumption_complete", "git_grants_consumption_seed_only",
		"git_grants_consumed_after_issue", "git_grants_revoked_after_issue",
	} {
		var n int
		if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'git_grants'::regclass`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("constraint %s missing (%d, %v)", name, n, err)
		}
	}
	var fk string
	if err := owner.QueryRow(bg, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'git_grants_tenant_id_project_id_fkey'`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != "FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id)" {
		t.Errorf("foreign key = %s", fk)
	}
	var triggers int
	if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'git_grants'::regclass AND tgname = 'git_grants_guard' AND NOT tgisinternal`).Scan(&triggers); err != nil || triggers != 1 {
		t.Fatalf("guard trigger: %d %v", triggers, err)
	}
	var rls, force bool
	if err := owner.QueryRow(bg, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'git_grants'::regclass`).Scan(&rls, &force); err != nil || !rls || !force {
		t.Fatalf("row level security: %v %v %v", rls, force, err)
	}
	priv := func(q string, args ...any) bool {
		var ok bool
		if err := owner.QueryRow(bg, q, args...).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	role := db.RuntimeRole
	for _, p := range []string{"SELECT", "INSERT"} {
		if !priv(`SELECT has_table_privilege($1, 'git_grants', $2)`, role, p) {
			t.Errorf("the runtime role lacks %s", p)
		}
	}
	for _, p := range []string{"DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		if priv(`SELECT has_table_privilege($1, 'git_grants', $2)`, role, p) {
			t.Errorf("the runtime role holds %s", p)
		}
	}
	for _, c := range []string{"credential_digest", "consumed_receive_id", "consumed_at", "revoked_at"} {
		if !priv(`SELECT has_column_privilege($1, 'git_grants', $2, 'UPDATE')`, role, c) {
			t.Errorf("the runtime role cannot update %s", c)
		}
	}
	for _, c := range []string{"tenant_id", "id", "project_id", "kind", "claims", "attempt_id", "repository_ids", "issue_key", "issued_at", "expires_at"} {
		if priv(`SELECT has_column_privilege($1, 'git_grants', $2, 'UPDATE')`, role, c) {
			t.Errorf("the runtime role can update %s", c)
		}
	}
}

// TestGrantsAreTenantIsolatedAndStoredAsDigests (IT-02, IT-04, IT-05, GR-05):
// row-level security hides other tenants and fails closed, pooled connections
// keep no tenant context, a project of another tenant cannot be named, the
// credential is stored only as a digest and the transition guards hold.
func TestGrantsAreTenantIsolatedAndStoredAsDigests(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	f := gitauthtest.New(t, gitauthtest.OpenPostgres)
	env := f.Env.(*gitauthtest.PGEnv)
	a := f.HumanCredential(gitauthtest.Engineer, gitauthtest.RepoReady)
	resp, err := f.Svc.Exchange(bg, gitauthtest.TenantB, gitauthtest.Engineer, gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectB), RepositoryID: gitauthtest.RepoB})
	if err != nil {
		t.Fatal(err)
	}
	b := resp.Password

	count := func(tx pgx.Tx) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(bg, `SELECT count(*) FROM git_grants`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	inTenant := func(tenant domain.TenantID, fn func(tx pgx.Tx) error) error {
		return storage.WithTenant(bg, env.Runtime, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
	}

	t.Run("RLS hides other tenants and fails closed", func(t *testing.T) {
		for _, tenant := range []domain.TenantID{gitauthtest.TenantA, gitauthtest.TenantB} {
			if err := inTenant(tenant, func(tx pgx.Tx) error {
				if n := count(tx); n != 1 {
					t.Errorf("%s sees %d grants", tenant, n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := env.Runtime.QueryRow(bg, `SELECT count(*) FROM git_grants`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("no tenant context sees %d grants (%v)", n, err)
		}
	})

	t.Run("pooled connections keep no tenant context", func(t *testing.T) {
		one := env.DB.RuntimePool(bg, 1)
		if err := storage.WithTenant(bg, one, gitauthtest.TenantA, func(_ context.Context, tx pgx.Tx) error {
			if count(tx) != 1 {
				t.Error("tenant context not applied")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := one.QueryRow(bg, `SELECT count(*) FROM git_grants`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the reused connection kept the tenant context: %d %v", n, err)
		}
	})

	t.Run("a credential is found only in its own tenant", func(t *testing.T) {
		if _, err := f.Svc.Verify(bg, gitauthtest.TenantA, b); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Fatalf("tenant B's credential in tenant A: %v", err)
		}
		if _, err := f.Svc.Verify(bg, gitauthtest.TenantB, a); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Fatalf("tenant A's credential in tenant B: %v", err)
		}
	})

	insert := `INSERT INTO git_grants (tenant_id, id, project_id, kind, actor_type, actor_id, run_id, attempt_id, repository_ids, claims,
		credential_digest, issue_key, issued_at, expires_at)
		VALUES ($1, $2, $3, 'human', 'user', 'u1', NULL, NULL, ARRAY['r1'], $4::jsonb, $5, NULL, now(), now() + interval '10 minutes')`
	claims := func(tenant, project, id string) string {
		return `{"grantId":"` + id + `","audience":"kodama-git","tenantId":"` + tenant + `","projectId":"` + project +
			`","actor":{"type":"user","id":"u1","runId":null,"attemptId":null},"kind":"human","repositories":[],"issuedAt":"2026-10-30T10:00:00Z","expiresAt":"2026-10-30T10:10:00Z"}`
	}
	digest := func(b byte) []byte {
		d := make([]byte, 32)
		d[0] = b
		return d
	}
	tA, pA, pB := string(gitauthtest.TenantA), string(gitauthtest.ProjectA), string(gitauthtest.ProjectB)

	t.Run("composite ownership and policy checks", func(t *testing.T) {
		// A project of another tenant under this tenant's context: the composite foreign key rejects it.
		err := inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, tA, "gg_x1", pB, claims(tA, pB, "gg_x1"), digest(1))
			return err
		})
		if pgCode(err) != "23503" {
			t.Errorf("cross-owner project: %v", err)
		}
		// Another tenant's row under this tenant's context: the policy rejects it.
		err = inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, string(gitauthtest.TenantB), "gg_x2", pB, claims(string(gitauthtest.TenantB), pB, "gg_x2"), digest(2))
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("foreign tenant write: %v", err)
		}
		// Without any tenant context nothing can be written.
		if _, err := env.Runtime.Exec(bg, insert, tA, "gg_x3", pA, claims(tA, pA, "gg_x3"), digest(3)); pgCode(err) != "42501" {
			t.Errorf("write without tenant context: %v", err)
		}
		// Control: a consistent row is accepted.
		err = inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, tA, "gg_ok", pA, claims(tA, pA, "gg_ok"), digest(4))
			return err
		})
		if err != nil {
			t.Fatalf("control row: %v", err)
		}
		// The columns must equal the stored claims; the digest has a length; the lifetime is bounded by kind.
		for name, c := range map[string]struct {
			sql  string
			args []any
		}{
			"claims of another grant":  {insert, []any{tA, "gg_y1", pA, claims(tA, pA, "gg_other"), digest(5)}},
			"claims of another tenant": {insert, []any{tA, "gg_y2", pA, claims("t136_globex", pA, "gg_y2"), digest(6)}},
			"digest length":            {insert, []any{tA, "gg_y3", pA, claims(tA, pA, "gg_y3"), []byte("short")}},
			"duplicate digest":         {insert, []any{tA, "gg_y4", pA, claims(tA, pA, "gg_y4"), digest(4)}},
			"duplicate id":             {insert, []any{tA, "gg_ok", pA, claims(tA, pA, "gg_ok"), digest(7)}},
			"human beyond an hour": {strings.Replace(insert, "interval '10 minutes'", "interval '61 minutes'", 1),
				[]any{tA, "gg_y5", pA, claims(tA, pA, "gg_y5"), digest(8)}},
			"seed beyond 15 minutes": {strings.Replace(strings.Replace(insert, "'human'", "'seed'", 1), "interval '10 minutes'", "interval '16 minutes'", 1),
				[]any{tA, "gg_y6", pA, strings.Replace(claims(tA, pA, "gg_y6"), `"kind":"human"`, `"kind":"seed"`, 1), digest(9)}},
			"unknown kind": {strings.Replace(insert, "'human'", "'root'", 1),
				[]any{tA, "gg_y7", pA, strings.Replace(claims(tA, pA, "gg_y7"), `"kind":"human"`, `"kind":"root"`, 1), digest(10)}},
			"human with an issue key": {strings.Replace(insert, "NULL, now()", "'k', now()", 1),
				[]any{tA, "gg_y8", pA, claims(tA, pA, "gg_y8"), digest(11)}},
			"expiry before issue": {strings.Replace(insert, "now() + interval '10 minutes'", "now() - interval '1 minute'", 1),
				[]any{tA, "gg_y9", pA, claims(tA, pA, "gg_y9"), digest(12)}},
		} {
			err := inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
				_, err := tx.Exec(bg, c.sql, c.args...)
				return err
			})
			if code := pgCode(err); code != "23514" && code != "23505" {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("only a digest is stored", func(t *testing.T) {
		for _, secret := range []string{a, b} {
			var leaked int
			if err := env.Owner.QueryRow(bg, `SELECT count(*) FROM git_grants g WHERE g::text LIKE '%' || $1 || '%'`, secret).Scan(&leaked); err != nil || leaked != 0 {
				t.Fatalf("the secret appears in %d rows (%v)", leaked, err)
			}
		}
		g, err := gitauth.ParseSecret(a)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		d := g.Digest()
		if err := env.Owner.QueryRow(bg, `SELECT count(*) FROM git_grants WHERE credential_digest = $1`, d[:]).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the digest identifies the grant: %d %v", n, err)
		}
	})

	t.Run("transition guards", func(t *testing.T) {
		seed := f.Env.(*gitauthtest.PGEnv)
		resp, err := f.Svc.Exchange(bg, gitauthtest.TenantA, gitauthtest.Engineer, gitv1.CredentialRequest{ProjectID: pA, RepositoryID: gitauthtest.RepoEmpty, Seed: true})
		if err != nil {
			t.Fatal(err)
		}
		// The runtime role cannot rewrite registration, delete, or reach another column.
		err = inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `UPDATE git_grants SET expires_at = expires_at + interval '1 hour' WHERE id = $1`, resp.GrantID)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("registration update: %v", err)
		}
		err = inTenant(gitauthtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `DELETE FROM git_grants WHERE id = $1`, resp.GrantID)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("delete: %v", err)
		}
		// Even the owner cannot rewrite the registration, replay a consumption or revive a grant.
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET expires_at = expires_at + interval '1 hour' WHERE id = $1`, resp.GrantID); pgCode(err) != "23514" {
			t.Errorf("owner registration update: %v", err)
		}
		if err := f.Svc.ConsumeSeed(bg, gitauthtest.TenantA, resp.GrantID, "rcv_guard"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET consumed_receive_id = 'rcv_other' WHERE id = $1`, resp.GrantID); pgCode(err) != "23514" {
			t.Errorf("consumption is write-once: %v", err)
		}
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET consumed_receive_id = NULL, consumed_at = NULL WHERE id = $1`, resp.GrantID); pgCode(err) != "23514" {
			t.Errorf("consumption cannot be erased: %v", err)
		}
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET credential_digest = $2 WHERE id = $1`, resp.GrantID, digest(99)); pgCode(err) != "23514" {
			t.Errorf("the credential of a consumed grant cannot be replaced: %v", err)
		}
		if err := f.Svc.Revoke(bg, gitauthtest.TenantA, resp.GrantID); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET revoked_at = NULL WHERE id = $1`, resp.GrantID); pgCode(err) != "23514" {
			t.Errorf("revocation is irreversible: %v", err)
		}
		// A consumption on a human grant violates the seed-only rule.
		var human string
		if err := seed.Owner.QueryRow(bg, `SELECT id FROM git_grants WHERE kind = 'human' AND tenant_id = $1 LIMIT 1`, tA).Scan(&human); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Owner.Exec(bg, `UPDATE git_grants SET consumed_receive_id = 'rcv_1', consumed_at = now() WHERE id = $1`, human); pgCode(err) != "23514" {
			t.Errorf("only a seed is consumed: %v", err)
		}
	})
}

// TestTransactionSeam: the execution owner and the push authorizer join their
// own pgx transaction through Bind, RevokeAttempt and ConsumeSeed, so the grant
// change commits or rolls back with their writes (GR-06, S4/S5).
func TestTransactionSeam(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	f := gitauthtest.New(t, gitauthtest.OpenPostgres)
	env := f.Env.(*gitauthtest.PGEnv)
	scope := gitauthtest.ScopeA()
	f.Attempts.Set(scope, "att_tx", gitauth.AttemptLease{Live: true})
	h, err := f.Svc.IssueAttempt(bg, scope, gitauth.AttemptGrantRequest{Kind: gitv1.GrantAttempt, Actor: gitauthtest.Workload("att_tx", "run_tx"),
		Repositories: []gitv1.GrantRepository{{ID: gitauthtest.RepoReady, Read: true, Update: []string{"refs/heads/kodama/work/wi_1"}}}, ExpiresAt: f.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("cancellation rolled back")
	err = storage.WithTenant(bg, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if n, err := gitpg.RevokeAttempt(ctx, tx, scope, "att_tx", f.Clock.Now()); err != nil || n != 1 {
			t.Fatalf("revoked %d: %v", n, err)
		}
		// The caller's own statements see the revocation inside the transaction.
		var revoked bool
		if err := tx.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM git_grants WHERE attempt_id = 'att_tx'`).Scan(&revoked); err != nil || !revoked {
			t.Fatalf("revocation not visible in the transaction: %v %v", revoked, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	f.MustVerify(scope.TenantID, h.Credential.Reveal())
	// A transaction that carries another tenant's context reaches none of this
	// tenant's rows, whatever scope it is told: row-level security answers 0.
	if err := storage.WithTenant(bg, env.Runtime, gitauthtest.TenantB, func(ctx context.Context, tx pgx.Tx) error {
		if n, err := gitpg.RevokeAttempt(ctx, tx, scope, "att_tx", f.Clock.Now()); err != nil || n != 0 {
			t.Errorf("another tenant's context revoked %d grants: %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.MustVerify(scope.TenantID, h.Credential.Reveal())
	if err := storage.WithTenant(bg, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := gitpg.RevokeAttempt(ctx, tx, scope, "att_tx", f.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Svc.Verify(bg, scope.TenantID, h.Credential.Reveal()); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Fatalf("after commit: %v", err)
	}

	// Seed consumption in the authorization transaction.
	seed, err := f.Svc.Exchange(bg, scope.TenantID, gitauthtest.Engineer, gitv1.CredentialRequest{ProjectID: string(scope.ProjectID), RepositoryID: gitauthtest.RepoEmpty, Seed: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WithTenant(bg, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := gitpg.ConsumeSeed(ctx, tx, scope.TenantID, seed.GrantID, "rcv_tx", f.Clock.Now()); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// Two authorizations race on two connections: the row lock admits exactly one.
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = storage.WithTenant(bg, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return gitpg.ConsumeSeed(ctx, tx, scope.TenantID, seed.GrantID, "rcv_race_"+string(rune('a'+i)), f.Clock.Now())
			})
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, gitauth.ErrSeedConsumed):
		default:
			t.Fatalf("racing consumption: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d racing receives consumed one seed", won)
	}
}

// TestRevokeAllSeam: the offboarding owner joins its own pgx transaction through
// RevokeAll, so the bulk revocation and its audit row commit or roll back with
// its writes; the runtime role's column grant and the transition guards allow
// the bulk update (grants of every kind, an already consumed seed included), a
// transaction that carries another tenant's context reaches none of the rows,
// and a replay neither changes the first revocation time nor appends audit.
func TestRevokeAllSeam(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	f := gitauthtest.New(t, gitauthtest.OpenPostgres)
	env := f.Env.(*gitauthtest.PGEnv)
	tenant := gitauthtest.TenantA
	human := f.HumanCredential(gitauthtest.Engineer, gitauthtest.RepoReady)
	seed, err := f.Svc.Exchange(bg, tenant, gitauthtest.Engineer, gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectA), RepositoryID: gitauthtest.RepoEmpty, Seed: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.ConsumeSeed(bg, tenant, seed.GrantID, "rcv_all"); err != nil {
		t.Fatal(err)
	}
	other, err := f.Svc.Exchange(bg, gitauthtest.TenantB, gitauthtest.Engineer, gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectB), RepositoryID: gitauthtest.RepoB})
	if err != nil {
		t.Fatal(err)
	}
	actor := gitauth.AuditActor{Type: gitauth.AuditActorSystem, ID: "gitops-offboarding"}
	boom := errors.New("offboarding rolled back")

	// Rolled back with the caller's transaction: no revocation, no audit row.
	err = storage.WithTenant(bg, env.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if n, err := gitpg.RevokeAll(ctx, tx, tenant, "offboarding", actor, f.Clock.Now()); err != nil || n != 2 {
			t.Fatalf("revoked %d: %v", n, err)
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM git_grants WHERE revoked_at IS NULL`).Scan(&open); err != nil || open != 0 {
			t.Fatalf("the transaction sees %d unrevoked grants: %v", open, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	f.MustVerify(tenant, human)
	if got := f.Env.RevocationAudits(t, tenant); len(got) != 0 {
		t.Fatalf("a rolled back revocation was audited: %+v", got)
	}

	// Another tenant's context revokes nothing of this tenant (row-level security).
	if err := storage.WithTenant(bg, env.Runtime, gitauthtest.TenantB, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM git_grants WHERE tenant_id = $1`, string(tenant)).Scan(&n); err != nil || n != 0 {
			t.Errorf("tenant B's context reads %d of tenant A's grants: %v", n, err)
		}
		// The adapter binds the transaction to the tenant it names; a mismatch with the
		// context's tenant revokes nothing and records its audit nowhere visible.
		if n, err := gitpg.RevokeAll(ctx, tx, tenant, "wrong tenant", actor, f.Clock.Now()); err != nil || n != 0 {
			t.Errorf("revoked %d through another tenant's context: %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.MustVerify(tenant, human)

	// Committed.
	var first time.Time
	if err := storage.WithTenant(bg, env.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if n, err := gitpg.RevokeAll(ctx, tx, tenant, "offboarding", actor, f.Clock.Now()); err != nil || n != 2 {
			t.Fatalf("revoked %d: %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.Owner.QueryRow(bg, `SELECT min(revoked_at) FROM git_grants WHERE tenant_id = $1 AND revoked_at IS NOT NULL`, string(tenant)).Scan(&first); err != nil {
		t.Fatal(err)
	}
	_, err = f.Svc.Verify(bg, tenant, human)
	if !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Fatalf("after commit: %v", err)
	}
	f.MustVerify(gitauthtest.TenantB, other.Password)
	if got := f.Env.RevocationAudits(t, tenant); len(got) != 1 || got[0].Revoked != 2 {
		t.Fatalf("audit = %+v", got)
	}

	// Replay later: nothing revoked, nothing recorded, the first time stands.
	f.Clock.Advance(time.Minute)
	if n, err := f.Svc.RevokeAll(bg, tenant, "offboarding", actor); err != nil || n != 0 {
		t.Fatalf("replay = %d, %v", n, err)
	}
	var again time.Time
	if err := env.Owner.QueryRow(bg, `SELECT min(revoked_at) FROM git_grants WHERE tenant_id = $1`, string(tenant)).Scan(&again); err != nil || !again.Equal(first) {
		t.Fatalf("revocation time %s, want %s: %v", again, first, err)
	}
	if got := f.Env.RevocationAudits(t, tenant); len(got) != 1 {
		t.Fatalf("a replay was audited: %+v", got)
	}
	if n, err := f.Svc.Active(bg, tenant); err != nil || n != 0 {
		t.Fatalf("active = %d, %v", n, err)
	}
	if n, err := f.Svc.Active(bg, gitauthtest.TenantB); err != nil || n != 1 {
		t.Fatalf("tenant B active = %d, %v", n, err)
	}
}
