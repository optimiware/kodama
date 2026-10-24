package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/storage"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Test_runtimeauth_tenant_isolation (IT-02, IT-04, IT-05, IT-A02, IT-A03): row
// level security, composite ownership, pooled context, least-privilege grants
// and the restricted digest lookup.
func Test_runtimeauth_tenant_isolation(t *testing.T) {
	s, a, cred, _ := seed(t)
	scopeB := domain.Scope{TenantID: "t019_globex", ProjectID: "p019_ledger"}
	s.SeedProject(scopeB)
	cB := rtest.ContextFor(a.Context, "att_b")
	cB.TenantID, cB.ProjectID = scopeB.TenantID, scopeB.ProjectID
	aB := s.SeedAttempt(cB, rtest.AllActions())
	credB, _, err := s.Register(aB, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}

	count := func(tx pgx.Tx) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(bg, `SELECT count(*) FROM runtime_grants`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	inTenant := func(tenant domain.TenantID, fn func(tx pgx.Tx) error) error {
		return storage.WithTenant(bg, s.Runtime, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
	}

	t.Run("RLS hides other tenants and fails closed", func(t *testing.T) {
		for _, tenant := range []domain.TenantID{a.Scope.TenantID, scopeB.TenantID} {
			if err := inTenant(tenant, func(tx pgx.Tx) error {
				if n := count(tx); n != 1 {
					t.Errorf("%s sees %d grants", tenant, n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if n := func() int {
			var n int
			if err := s.Runtime.QueryRow(bg, `SELECT count(*) FROM runtime_grants`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}(); n != 0 {
			t.Fatalf("no tenant context sees %d grants", n)
		}
	})

	t.Run("pooled connections keep no tenant context", func(t *testing.T) {
		one := s.DB.RuntimePool(bg, 1)
		if err := storage.WithTenant(bg, one, a.Scope.TenantID, func(_ context.Context, tx pgx.Tx) error {
			if count(tx) != 1 {
				t.Error("tenant context not applied")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := one.QueryRow(bg, `SELECT count(*) FROM runtime_grants`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the reused connection kept the tenant context: %d %v", n, err)
		}
	})

	t.Run("composite ownership and policy checks", func(t *testing.T) {
		insert := `INSERT INTO runtime_grants (tenant_id, project_id, run_id, attempt_id, job_namespace, job_name, credential_digest,
			signing_generation, actions, authority_revision, context_digest, context_doc, created_at, expires_at)
			VALUES ($1, $2, 'run_x', $3, 'ns', 'job', $4, 1, ARRAY['context.read'], 1, $5, '{}', now(), now() + interval '1 minute')`
		digest := func(s string) []byte { d := runtimeauth.NonceDigest(s); return d[:] }
		// A project of another tenant under this tenant's context: the composite foreign key rejects it.
		err := inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, string(a.Scope.TenantID), string(scopeB.ProjectID), "att_x1", digest("1"), digest("c"))
			return err
		})
		if pgCode(err) != "23503" {
			t.Errorf("cross-owner project: %v", err)
		}
		// Another tenant's row under this tenant's context: the policy rejects it.
		err = inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, string(scopeB.TenantID), string(scopeB.ProjectID), "att_x2", digest("2"), digest("c"))
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("foreign tenant write: %v", err)
		}
		// Without any tenant context nothing can be written.
		if _, err := s.Runtime.Exec(bg, insert, string(a.Scope.TenantID), string(a.Scope.ProjectID), "att_x3", digest("3"), digest("c")); pgCode(err) != "42501" {
			t.Errorf("write without tenant context: %v", err)
		}
		// Unknown action and bad digest length are rejected by constraints.
		err = inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, strings.Replace(insert, "ARRAY['context.read']", "ARRAY['gate.approve']", 1), string(a.Scope.TenantID), string(a.Scope.ProjectID), "att_x4", digest("4"), digest("c"))
			return err
		})
		if pgCode(err) != "23514" {
			t.Errorf("unknown action: %v", err)
		}
		err = inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, insert, string(a.Scope.TenantID), string(a.Scope.ProjectID), "att_x5", []byte{1, 2}, digest("c"))
			return err
		})
		if pgCode(err) != "23514" {
			t.Errorf("short digest: %v", err)
		}
		// A duplicate credential digest cannot be registered for another attempt.
		err = inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			d := cred.Digest()
			_, err := tx.Exec(bg, insert, string(a.Scope.TenantID), string(a.Scope.ProjectID), "att_x6", d[:], digest("c"))
			return err
		})
		if pgCode(err) != "23505" {
			t.Errorf("duplicate credential digest: %v", err)
		}
	})

	t.Run("least privilege", func(t *testing.T) {
		var del, trunc, upd, ins, sel bool
		if err := s.Runtime.QueryRow(bg, `SELECT has_table_privilege(current_user, 'runtime_grants', 'DELETE'),
			has_table_privilege(current_user, 'runtime_grants', 'TRUNCATE'),
			has_table_privilege(current_user, 'runtime_grants', 'UPDATE'),
			has_table_privilege(current_user, 'runtime_grants', 'INSERT'),
			has_table_privilege(current_user, 'runtime_grants', 'SELECT')`).Scan(&del, &trunc, &upd, &ins, &sel); err != nil {
			t.Fatal(err)
		}
		// Table-level UPDATE is false: only specific columns are granted.
		if del || trunc || upd || !ins || !sel {
			t.Fatalf("privileges delete=%v truncate=%v update=%v insert=%v select=%v", del, trunc, upd, ins, sel)
		}
		for _, col := range []string{"credential_digest", "actions", "context_doc", "context_digest", "authority_revision", "signing_generation", "attempt_id", "tenant_id", "expires_at", "job_name"} {
			var ok bool
			if err := s.Runtime.QueryRow(bg, `SELECT has_column_privilege(current_user, 'runtime_grants', $1, 'UPDATE')`, col).Scan(&ok); err != nil || ok {
				t.Errorf("the runtime role can update %s (%v)", col, err)
			}
		}
		for _, col := range []string{"session_id", "nonce_digest", "bound_pod_uid", "revoked_at", "max_expires_at"} {
			var ok bool
			if err := s.Runtime.QueryRow(bg, `SELECT has_column_privilege(current_user, 'runtime_grants', $1, 'UPDATE')`, col).Scan(&ok); err != nil || !ok {
				t.Errorf("the runtime role cannot update %s (%v)", col, err)
			}
		}
		err := inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `DELETE FROM runtime_grants`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("delete: %v", err)
		}
		err = inTenant(a.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `UPDATE runtime_grants SET actions = ARRAY['result.submit','secret.resolve','model.invoke']`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("widening the ceiling: %v", err)
		}
	})

	t.Run("transitions are enforced by the database", func(t *testing.T) {
		s, a, _, req := seed(t)
		if _, err := s.Service.Admit(bg, "att_01", req); err != nil {
			t.Fatal(err)
		}
		mutate := func(sql string) error {
			return storage.WithTenant(bg, s.Runtime, a.Scope.TenantID, func(_ context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(bg, sql)
				return err
			})
		}
		for name, sql := range map[string]string{
			"re-pointing the session":  `UPDATE runtime_grants SET session_id = 'ses_aaaaaaaaaaaaaaaaaaaaaa'`,
			"changing the nonce":       `UPDATE runtime_grants SET nonce_digest = decode(repeat('00', 32), 'hex')`,
			"moving the bound Pod":     `UPDATE runtime_grants SET bound_pod_uid = '9c4f4f51-6d7e-4f80-9bc2-3d4e5f607182'`,
			"extending the maximum":    `UPDATE runtime_grants SET max_expires_at = max_expires_at + interval '1 day'`,
			"clearing the consumption": `UPDATE runtime_grants SET session_id = NULL, nonce_digest = NULL, bound_job_uid = NULL, bound_pod_name = NULL, bound_pod_uid = NULL, consumed_at = NULL, max_expires_at = NULL`,
		} {
			if err := mutate(sql); pgCode(err) != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := mutate(`UPDATE runtime_grants SET revoked_at = now()`); err != nil {
			t.Fatalf("revocation: %v", err)
		}
		if err := mutate(`UPDATE runtime_grants SET revoked_at = NULL`); pgCode(err) != "23514" {
			t.Errorf("un-revoking: %v", err)
		}
		if err := mutate(`UPDATE runtime_grants SET revoked_at = revoked_at + interval '1 hour'`); pgCode(err) != "23514" {
			t.Errorf("rewriting the revocation time: %v", err)
		}
		// Even the owner cannot alter the registration (the trigger applies to everyone).
		if _, err := s.Owner.Exec(bg, `UPDATE runtime_grants SET actions = ARRAY['model.invoke'] WHERE attempt_id = 'att_01'`); pgCode(err) != "23514" {
			t.Errorf("owner altering the ceiling: %v", err)
		}
	})

	t.Run("restricted digest lookup", func(t *testing.T) {
		var tenant, project, attempt string
		d := cred.Digest()
		rows, err := s.Runtime.Query(bg, `SELECT * FROM kodama_runtime_locate_grant($1)`, d[:])
		if err != nil {
			t.Fatal(err)
		}
		cols := rows.FieldDescriptions()
		if len(cols) != 3 || cols[0].Name != "tenant_id" || cols[1].Name != "project_id" || cols[2].Name != "attempt_id" {
			t.Fatalf("the lookup exposes %d columns", len(cols))
		}
		if !rows.Next() {
			t.Fatal("no row for a known digest")
		}
		if err := rows.Scan(&tenant, &project, &attempt); err != nil {
			t.Fatal(err)
		}
		if rows.Next() {
			t.Fatal("more than one row")
		}
		rows.Close()
		if tenant != string(a.Scope.TenantID) || project != string(a.Scope.ProjectID) || attempt != "att_01" {
			t.Fatalf("locator = %s/%s/%s", tenant, project, attempt)
		}
		dB := credB.Digest()
		if loc, err := s.Store.Locate(bg, dB); err != nil || loc.Scope != scopeB || loc.AttemptID != "att_b" {
			t.Fatalf("locate B: %+v %v", loc, err)
		}
		unknown := runtimeauth.NonceDigest("unknown")
		if _, err := s.Store.Locate(bg, unknown); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("unknown digest: %v", err)
		}
		// No digest filter, no data: a malformed argument yields nothing.
		var n int
		if err := s.Runtime.QueryRow(bg, `SELECT count(*) FROM kodama_runtime_locate_grant(decode('00', 'hex'))`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a non-matching digest returned %d rows (%v)", n, err)
		}
		var secdef, public bool
		var config []string
		if err := s.Runtime.QueryRow(bg, `SELECT p.prosecdef, COALESCE(p.proconfig, '{}'),
			EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a WHERE a.grantee = 0)
			FROM pg_proc p WHERE p.proname = 'kodama_runtime_locate_grant'`).Scan(&secdef, &config, &public); err != nil {
			t.Fatal(err)
		}
		if !secdef || public || len(config) == 0 || !strings.HasPrefix(config[0], "search_path=pg_catalog") {
			t.Fatalf("definer=%v public=%v config=%v", secdef, public, config)
		}
	})

	t.Run("RLS is enabled and forced", func(t *testing.T) {
		var on, forced bool
		if err := s.Owner.QueryRow(bg, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'runtime_grants'`).Scan(&on, &forced); err != nil || !on || !forced {
			t.Fatalf("rls=%v force=%v %v", on, forced, err)
		}
	})

	t.Run("no credential, nonce or token is stored", func(t *testing.T) {
		resp, err := s.Service.Admit(bg, "att_01", s.AdmitRequest(a, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
		if err != nil {
			t.Fatal(err)
		}
		var dump string
		if err := s.Owner.QueryRow(bg, `SELECT string_agg(g::text, ' ') FROM runtime_grants g`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		for name, secret := range map[string]string{"credential": cred.Reveal(), "token": resp.Token.Token, "nonce": "nnnnnnnnnnnnnnnnnnnnnn"} {
			if strings.Contains(dump, secret) {
				t.Errorf("the %s is stored in clear", name)
			}
		}
	})
}
