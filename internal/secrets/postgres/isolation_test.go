package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// Test_tenant_isolation covers IT-02..IT-06/IT-11 for the module's tables:
// non-owner RLS, explicit predicates, composite ownership foreign keys, absent
// tenant context and pooled-connection cleanup.
func Test_tenant_isolation(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()

	secA := e.createSecret(scopeA1, "erp-token", "a-SENTINEL")
	secB := e.createSecret(scopeB1, "erp-token", "b-SENTINEL") // the same name in another tenant
	secA2 := e.createSecret(scopeA2, "erp-token", "a2-SENTINEL")
	varA := e.createVariable(scopeA1, "region", "eu")
	e.createVariable(scopeB1, "region", "us")
	if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	e.revoke(scopeA1, secA.ID(), 1)

	allTables := []string{
		"variables", "variable_versions", "secret_scope_keys", "secret_scope_key_wraps",
		"secrets", "secret_versions", "secret_pins", "secret_consumer_handling",
	}
	count := func(tenant domain.TenantID, tbl string, explicit bool) int {
		t.Helper()
		var n int
		err := storage.WithTenant(ctx, e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if explicit {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, string(tenant)).Scan(&n)
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		})
		if err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		return n
	}

	t.Run("a tenant sees only its rows, even without a predicate", func(t *testing.T) {
		for _, tbl := range allTables {
			a, b := count(tenantA, tbl, false), count(tenantB, tbl, false)
			if tbl == "secret_pins" || tbl == "secret_consumer_handling" {
				if b != 0 || a == 0 {
					t.Errorf("%s: tenant A %d, tenant B %d", tbl, a, b)
				}
				continue
			}
			if a == 0 || b == 0 {
				t.Errorf("%s: tenant A %d, tenant B %d", tbl, a, b)
			}
			// What each tenant sees equals what the owner counts for it.
			ownerA := e.count(`SELECT count(*) FROM ` + tbl + ` WHERE tenant_id = 't_acme'`)
			if a != ownerA || count(tenantA, tbl, true) != ownerA {
				t.Errorf("%s: tenant A sees %d of %d", tbl, a, ownerA)
			}
			if x := count(tenantA, tbl, true) + count(tenantB, tbl, true); x != e.count(`SELECT count(*) FROM `+tbl) {
				t.Errorf("%s: tenants do not partition the table", tbl)
			}
		}
		// A predicate naming the other tenant returns nothing.
		var n int
		err := storage.WithTenant(ctx, e.base.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE tenant_id = 't_globex'`).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Fatalf("cross-tenant predicate: %d %v", n, err)
		}
	})

	t.Run("absent context and pooled connections", func(t *testing.T) {
		one := e.base.DB.RuntimePool(ctx, 1) // a single connection that every call reuses
		for _, tbl := range allTables {
			var n int
			if err := one.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s without a tenant context: %d %v", tbl, n, err)
			}
		}
		if _, err := one.Exec(ctx, `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_acme', 'p_inventory', 'v_x', 'x', 'string', 'x', 1, 1, now(), now())`); err == nil {
			t.Fatal("insert without a tenant context succeeded")
		}
		// Enter tenant A on the only connection, leave it, and check nothing leaked.
		if err := storage.WithTenant(ctx, one, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM secrets`).Scan(&n); err != nil || n == 0 {
				return errors.New("tenant context not active")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var current *string
		if err := one.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&current); err != nil || current != nil {
			t.Fatalf("tenant context leaked to the pooled connection: %v %v", current, err)
		}
		var n int
		if err := one.QueryRow(ctx, `SELECT count(*) FROM secret_versions`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rows visible after the transaction ended: %d %v", n, err)
		}
		// A failed transaction does not leak either.
		_ = storage.WithTenant(ctx, one, tenantA, func(ctx context.Context, tx pgx.Tx) error { return errors.New("boom") })
		if err := one.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&current); err != nil || current != nil {
			t.Fatalf("tenant context leaked after a failed transaction: %v %v", current, err)
		}
	})

	t.Run("writes cannot cross tenants", func(t *testing.T) {
		err := storage.WithTenant(ctx, e.base.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_globex', 'p_ops', 'v_x', 'x', 'string', 'x', 1, 1, now(), now())`)
			return err
		})
		if err == nil {
			t.Fatal("tenant A wrote a tenant B row")
		}
		// Updates of another tenant's rows affect nothing.
		var affected int64
		err = storage.WithTenant(ctx, e.base.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE secrets SET deleted_at = now(), version = version + 1 WHERE id = $1`, secB.ID())
			affected = tag.RowsAffected()
			return err
		})
		if err != nil || affected != 0 {
			t.Fatalf("cross-tenant update: %d %v", affected, err)
		}
		if cur, err := e.store.GetSecret(ctx, scopeB1, secB.ID()); err != nil || cur.Deleted() {
			t.Fatalf("tenant B's secret changed: %v", err)
		}
	})

	t.Run("the store never returns another tenant's or project's resources", func(t *testing.T) {
		for name, scope := range map[string]domain.Scope{"other tenant": scopeB1, "other project": scopeA2} {
			if _, err := e.store.GetSecret(ctx, scope, secA.ID()); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: GetSecret: %v", name, err)
			}
			if _, err := e.store.GetVariable(ctx, scope, varA.ID()); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: GetVariable: %v", name, err)
			}
			if _, err := e.store.GetSecretVersion(ctx, scope, secA.ID(), 1); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: GetSecretVersion: %v", name, err)
			}
			if vs, err := e.store.ListSecretVersions(ctx, scope, secA.ID(), 0, 10); err != nil || len(vs) != 0 {
				t.Errorf("%s: ListSecretVersions: %d %v", name, len(vs), err)
			}
		}
		// Each scope lists only its own secrets even when names collide.
		for scope, id := range map[domain.Scope]string{scopeA1: secA.ID(), scopeA2: secA2.ID(), scopeB1: secB.ID()} {
			list, err := e.store.ListSecrets(ctx, scope, "", 10)
			if err != nil || len(list) != 1 || list[0].ID() != id {
				t.Errorf("list %v: %v %v", scope, err, len(list))
			}
		}
		// The Writer is bound to its tenant: another tenant's scope is not found.
		err := e.store.Atomically(ctx, tenantB, func(ctx context.Context, w secrets.Writer) error {
			if _, err := w.GetSecret(ctx, scopeA1, secA.ID()); !errors.Is(err, secrets.ErrNotFound) {
				return errors.New("writer read another tenant's secret")
			}
			_, err := w.RevokeSecretVersion(ctx, e.mutation("mallory"), scopeA1, secA.ID(), 1)
			return err
		})
		if !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("writer used another tenant's scope: %v", err)
		}
		// Scope key identities are per tenant.
		skA, _ := e.store.ActiveScopeKey(ctx, tenantA)
		if _, err := e.store.GetScopeKey(ctx, tenantB, skA.ID()); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("tenant B read tenant A's scope key: %v", err)
		}
	})

	t.Run("pins respect ownership", func(t *testing.T) {
		// A pin through another project or tenant finds no such secret.
		err := storage.WithTenant(ctx, e.base.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return e.store.RegisterPins(ctx, tx, secretspg.PinRequest{
				Scope: scopeA2, Consumer: secrets.ConsumerRef{Kind: "run", ID: "run_x"},
				Bindings: []runtimeBinding{binding("missing", 1)},
			})
		})
		if !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("pin of a missing secret: %v", err)
		}
		// The transaction's tenant context must equal the request's tenant.
		err = storage.WithTenant(ctx, e.base.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return e.store.RegisterPins(ctx, tx, secretspg.PinRequest{
				Scope: scopeB1, Consumer: secrets.ConsumerRef{Kind: "run", ID: "run_x"},
				Bindings: []runtimeBinding{binding("erp-token", 1)},
			})
		})
		if !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("pin under another tenant's context: %v", err)
		}
		if found, _, err := e.store.HasPin(ctx, scopeB1, secrets.ConsumerRef{Kind: "run", ID: "run_1"}, secA.ID(), 1); err != nil || found {
			t.Fatalf("tenant B sees tenant A's pin: %v %v", found, err)
		}
		if found, _, err := e.store.HasPin(ctx, scopeA1, secrets.ConsumerRef{Kind: "run", ID: "run_1"}, secA.ID(), 1); err != nil || !found {
			t.Fatalf("tenant A lost its pin: %v %v", found, err)
		}
		// A run of tenant A cannot resolve through tenant B's scope, and vice versa.
		auth := newAuthority()
		auth.set(secrets.AttemptGrant{Scope: scopeB1, RunID: "run_1", AttemptID: "att_b", Bindings: []runtimeBinding{binding("erp-token", 1)}})
		r := e.resolver(auth)
		if _, err := r.Resolve(ctx, "att_b", ref("erp-token", 1), "r"); !errors.Is(err, secrets.ErrNotPinned) {
			t.Fatalf("tenant B's run used a pin of the same run ID in tenant A: %v", err)
		}
	})

	t.Run("composite foreign keys reject mismatched ownership", func(t *testing.T) {
		fail := func(name, sql string) {
			t.Helper()
			if _, err := e.base.Migration.Exec(ctx, sql); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		// secret_versions: project does not own the secret; scope key from another tenant.
		fail("version in a sibling project", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
SELECT tenant_id, 'p_billing', secret_id, 9, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by FROM secret_versions WHERE secret_id = '`+secA.ID()+`' AND version = 1`)
		fail("scope key of another tenant", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
SELECT sv.tenant_id, sv.project_id, sv.secret_id, 9, k.id, k.generation, sv.format_version, sv.algorithm, sv.nonce, sv.ciphertext, sv.created_at, sv.created_by
FROM secret_versions sv, secret_scope_keys k WHERE sv.secret_id = '`+secA.ID()+`' AND sv.version = 1 AND k.tenant_id = 't_globex'`)
		fail("variable version in a sibling project", `INSERT INTO variable_versions (tenant_id, project_id, variable_id, version, type, value_text, created_at)
VALUES ('t_acme', 'p_billing', '`+varA.ID()+`', 9, 'string', 'x', now())`)
		fail("pin in a sibling project", `INSERT INTO secret_pins (tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ('t_acme', 'p_billing', 'run', 'run_77', '`+secA.ID()+`', 1, now())`)
		fail("handling for another tenant's pin", `INSERT INTO secret_consumer_handling (tenant_id, id, project_id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at)
VALUES ('t_globex', 'ch_x', 'p_ops', '`+secB.ID()+`', 1, 'run', 'run_1', 'terminate_consumer', 'pending', false, now())`)
		fail("wrap of another tenant's key", `INSERT INTO secret_scope_key_wraps (tenant_id, key_id, revision, wrapped_key, wrap_algorithm, kek_provider, kek_id, kek_version, wrapped_at)
SELECT 't_globex', id, 2, '\x0102', 'a', 'p', 'k', 'v', now() FROM secret_scope_keys WHERE tenant_id = 't_acme'`)
	})
}
