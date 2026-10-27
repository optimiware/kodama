package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var t012Tables = []string{
	"variables", "variable_versions", "secret_scope_keys", "secret_scope_key_wraps",
	"secrets", "secret_versions", "secret_pins", "secret_consumer_handling",
}

// Test_migration_reapply: clean install in the documented dependency order,
// repeat application, grants, constraints and immutability guards on a real
// PostgreSQL.
func Test_migration_reapply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgtest.New(t)
	mig := db.MigrationPool(ctx, 4)

	// The squashed baseline: schema, then the runtime role's grants.
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("combined manifest: %v", err)
	}
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Applied) != 2 {
		t.Fatalf("the baseline is two migrations, applied %v", first.Applied)
	}
	// Pins name the consuming run; this module's schema test has no execution.
	pgtest.DropExecutionLinks(t, mig, "secret_pins", "secret_pins_run_fk")
	second, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply applied %v skipped %d", second.Applied, second.Skipped)
	}

	one := func(sql string, args ...any) (v bool) {
		t.Helper()
		if err := mig.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
		return v
	}

	t.Run("row level security is enabled and forced", func(t *testing.T) {
		for _, tbl := range t012Tables {
			if !one(`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tbl) {
				t.Errorf("%s: RLS not enabled and forced", tbl)
			}
		}
	})

	t.Run("runtime grants are minimal", func(t *testing.T) {
		for _, tbl := range t012Tables {
			if one(`SELECT has_table_privilege($1, $2, 'DELETE')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role may DELETE", tbl)
			}
			if one(`SELECT has_table_privilege($1, $2, 'TRUNCATE')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role may TRUNCATE", tbl)
			}
			if !one(`SELECT has_table_privilege($1, $2, 'SELECT')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role cannot SELECT", tbl)
			}
		}
		// Immutable columns are not updatable; the one-way columns are.
		for tbl, col := range map[string]string{
			"secret_versions": "ciphertext", "secret_versions ": "nonce", "secrets": "name", "variables": "name", "variables ": "type",
			"secret_scope_key_wraps": "wrapped_key", "secret_scope_keys": "generation", "variable_versions": "value_text",
			"secret_pins": "secret_version", "secret_consumer_handling": "effect",
		} {
			tbl = strings.TrimSpace(tbl)
			if one(`SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, tbl, col) {
				t.Errorf("%s.%s is updatable by the runtime role", tbl, col)
			}
		}
		for tbl, col := range map[string]string{
			"secret_versions": "revoked_at", "secrets": "deleted_at", "secret_scope_keys": "current_revision",
			"secret_pins": "released_at", "secret_consumer_handling": "state", "variables": "value_text",
		} {
			if !one(`SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, tbl, col) {
				t.Errorf("%s.%s is not updatable by the runtime role", tbl, col)
			}
		}
		if one(`SELECT has_table_privilege($1, 'audit', 'UPDATE') OR has_table_privilege($1, 'audit', 'DELETE')`, db.RuntimeRole) {
			t.Error("audit became mutable")
		}
	})

	seed := func() {
		t.Helper()
		// One multi-statement Exec is one implicit transaction, so the deferred
		// foreign keys of a scope key and its first wrap are checked together.
		if _, err := mig.Exec(ctx, strings.Join([]string{
			`INSERT INTO tenants (id, key) VALUES ('t_a', 'a'), ('t_b', 'b')`,
			`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t_a', 'p_1', 'p1', 3), ('t_a', 'p_2', 'p2', 3), ('t_b', 'p_9', 'p9', 3)`,
			`INSERT INTO secret_scope_keys (tenant_id, id, generation, current_revision, created_at) VALUES ('t_a', 'sk_a', 1, 1, now()), ('t_b', 'sk_b', 1, 1, now())`,
			`INSERT INTO secret_scope_key_wraps (tenant_id, key_id, revision, wrapped_key, wrap_algorithm, kek_provider, kek_id, kek_version, wrapped_at)
			 VALUES ('t_a', 'sk_a', 1, '\x0102', 'a', 'p', 'k', 'v1', now()), ('t_b', 'sk_b', 1, '\x0102', 'a', 'p', 'k', 'v1', now())`,
		}, ";\n")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed()
	ct := `'\x` + strings.Repeat("ab", 20) + `'`
	nonce := `'\x` + strings.Repeat("01", 12) + `'`
	insertSecret := `
INSERT INTO secrets (tenant_id, project_id, id, name, kind, current_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 's_1', 'erp-token', 'generic', 1, 1, now(), now());
INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ('t_a', 'p_1', 's_1', 1, 'sk_a', 1, 1, 'aes-256-gcm/hkdf-sha256', ` + nonce + `, ` + ct + `, now(), 'alice');`
	if _, err := mig.Exec(ctx, insertSecret); err != nil {
		t.Fatalf("a valid secret with its first version: %v", err)
	}

	expectFail := func(name, sql string) {
		t.Helper()
		if _, err := mig.Exec(ctx, sql); err == nil {
			t.Errorf("%s: statement succeeded:\n%s", name, sql)
		}
	}

	t.Run("constraints reject invalid rows", func(t *testing.T) {
		expectFail("secret without a version row (deferred FK)", `INSERT INTO secrets (tenant_id, project_id, id, name, kind, current_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 's_2', 'no-version', 'generic', 1, 1, now(), now())`)
		expectFail("duplicate name in the project", `INSERT INTO secrets (tenant_id, project_id, id, name, kind, current_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 's_3', 'erp-token', 'generic', 1, 1, now(), now())`)
		expectFail("bad name", `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 'v_1', 'Bad_Name', 'string', 'x', 1, 1, now(), now())`)
		expectFail("unknown kind", `UPDATE secrets SET kind = 'oauth' WHERE id = 's_1'`)
		expectFail("project of another tenant", `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_a', 'p_9', 'v_2', 'name', 'string', 'x', 1, 1, now(), now())`)
		expectFail("short nonce", strings.Replace(insertSecret, nonce, `'\x0102'`, 1))
		expectFail("tiny ciphertext", strings.Replace(insertSecret, ct, `'\x01'`, 1))
		expectFail("scope key of another tenant", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ('t_a', 'p_1', 's_1', 2, 'sk_b', 1, 1, 'aes-256-gcm/hkdf-sha256', `+nonce+`, `+ct+`, now(), 'alice')`)
		expectFail("wrong key generation", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ('t_a', 'p_1', 's_1', 2, 'sk_a', 7, 1, 'aes-256-gcm/hkdf-sha256', `+nonce+`, `+ct+`, now(), 'alice')`)
		expectFail("unknown algorithm", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ('t_a', 'p_1', 's_1', 2, 'sk_a', 1, 1, 'rot13', `+nonce+`, `+ct+`, now(), 'alice')`)
		expectFail("version in the wrong project", `INSERT INTO secret_versions (tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ('t_a', 'p_2', 's_1', 2, 'sk_a', 1, 1, 'aes-256-gcm/hkdf-sha256', `+nonce+`, `+ct+`, now(), 'alice')`)
		expectFail("second scope key for a generation", `INSERT INTO secret_scope_keys (tenant_id, id, generation, current_revision, created_at) VALUES ('t_a', 'sk_a2', 1, 1, now())`)
		expectFail("half-set revocation", `UPDATE secret_versions SET revoked_at = now() WHERE secret_id = 's_1'`)
		expectFail("variable value version above the resource version", `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 'v_3', 'name', 'string', 'x', 2, 1, now(), now())`)
	})

	t.Run("immutability guards hold even for the owner", func(t *testing.T) {
		expectFail("rename secret", `UPDATE secrets SET name = 'other', version = version + 1 WHERE id = 's_1'`)
		expectFail("change kind", `UPDATE secrets SET kind = 'git', version = version + 1 WHERE id = 's_1'`)
		expectFail("skip a value version", `UPDATE secrets SET current_version = 3, version = version + 1 WHERE id = 's_1'`)
		expectFail("version does not increase", `UPDATE secrets SET updated_at = now() WHERE id = 's_1'`)
		expectFail("rewrite ciphertext", `UPDATE secret_versions SET ciphertext = '\x`+strings.Repeat("cd", 20)+`' WHERE secret_id = 's_1'`)
		expectFail("rewrite nonce", `UPDATE secret_versions SET nonce = `+`'\x`+strings.Repeat("02", 12)+`' WHERE secret_id = 's_1'`)
		expectFail("move scope key", `UPDATE secret_versions SET scope_key_id = 'sk_b' WHERE secret_id = 's_1'`)
		expectFail("delete a version", `DELETE FROM secret_versions WHERE secret_id = 's_1'`)
		expectFail("scope key generation", `UPDATE secret_scope_keys SET generation = 2 WHERE id = 'sk_a'`)
		expectFail("skip a wrap revision", `UPDATE secret_scope_keys SET current_revision = 3 WHERE id = 'sk_a'`)

		// Revocation is allowed once and is irreversible.
		if _, err := mig.Exec(ctx, `UPDATE secret_versions SET revoked_at = now(), revoked_by = 'alice' WHERE secret_id = 's_1'`); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		expectFail("un-revoke", `UPDATE secret_versions SET revoked_at = NULL, revoked_by = NULL WHERE secret_id = 's_1'`)
		expectFail("re-revoke by another actor", `UPDATE secret_versions SET revoked_by = 'mallory' WHERE secret_id = 's_1'`)

		// Deletion is irreversible and takes no new values.
		if _, err := mig.Exec(ctx, `UPDATE secrets SET deleted_at = now(), version = version + 1 WHERE id = 's_1'`); err != nil {
			t.Fatalf("delete: %v", err)
		}
		expectFail("undelete", `UPDATE secrets SET deleted_at = NULL, version = version + 1 WHERE id = 's_1'`)
		expectFail("new version after deletion", `UPDATE secrets SET current_version = 2, version = version + 1 WHERE id = 's_1'`)

		// Variables: name/type immutable, value version in step with the value.
		if _, err := mig.Exec(ctx, `INSERT INTO variables (tenant_id, project_id, id, name, type, value_text, value_version, version, created_at, updated_at)
VALUES ('t_a', 'p_1', 'v_ok', 'region', 'string', 'eu', 1, 1, now(), now())`); err != nil {
			t.Fatal(err)
		}
		expectFail("rename variable", `UPDATE variables SET name = 'zone', version = 2 WHERE id = 'v_ok'`)
		expectFail("retype variable", `UPDATE variables SET type = 'integer', version = 2 WHERE id = 'v_ok'`)
		expectFail("change a value without a version", `UPDATE variables SET value_text = 'us', version = 2 WHERE id = 'v_ok'`)
		expectFail("bump a value version without a change", `UPDATE variables SET value_version = 2, version = 2 WHERE id = 'v_ok'`)
		if _, err := mig.Exec(ctx, `UPDATE variables SET value_text = 'us', value_version = 2, version = 2 WHERE id = 'v_ok'`); err != nil {
			t.Fatalf("a proper value edit: %v", err)
		}
	})

	t.Run("pins and handling are retained", func(t *testing.T) {
		// A pin names an existing version; the handling record names an existing pin.
		if _, err := mig.Exec(ctx, `INSERT INTO secret_pins (tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ('t_a', 'p_1', 'run', 'run_1', 's_1', 1, now())`); err != nil {
			t.Fatal(err)
		}
		expectFail("pin of an unknown version", `INSERT INTO secret_pins (tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ('t_a', 'p_1', 'run', 'run_1', 's_1', 9, now())`)
		expectFail("pin in the wrong project", `INSERT INTO secret_pins (tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ('t_a', 'p_2', 'run', 'run_2', 's_1', 1, now())`)
		expectFail("unknown consumer kind", `INSERT INTO secret_pins (tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ('t_a', 'p_1', 'job', 'run_3', 's_1', 1, now())`)
		expectFail("handling for an unpinned consumer", `INSERT INTO secret_consumer_handling (tenant_id, id, project_id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at)
VALUES ('t_a', 'ch_x', 'p_1', 's_1', 1, 'run', 'run_nope', 'terminate_consumer', 'pending', false, now())`)
		if _, err := mig.Exec(ctx, `INSERT INTO secret_consumer_handling (tenant_id, id, project_id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at)
VALUES ('t_a', 'ch_1', 'p_1', 's_1', 1, 'run', 'run_1', 'terminate_consumer', 'pending', false, now())`); err != nil {
			t.Fatal(err)
		}
		expectFail("duplicate effect", `INSERT INTO secret_consumer_handling (tenant_id, id, project_id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at)
VALUES ('t_a', 'ch_2', 'p_1', 's_1', 1, 'run', 'run_1', 'terminate_consumer', 'pending', false, now())`)
		expectFail("completed without an actor", `UPDATE secret_consumer_handling SET state = 'completed', resolved_at = now() WHERE id = 'ch_1'`)
		expectFail("delete a pin", `DELETE FROM secret_pins WHERE consumer_id = 'run_1'`)
		if _, err := mig.Exec(ctx, `UPDATE secret_pins SET released_at = now() WHERE consumer_id = 'run_1'`); err != nil {
			t.Fatal(err)
		}
		expectFail("unrelease a pin", `UPDATE secret_pins SET released_at = NULL WHERE consumer_id = 'run_1'`)
		if _, err := mig.Exec(ctx, `UPDATE secret_consumer_handling SET state = 'completed', resolved_at = now(), resolved_by = 'ops' WHERE id = 'ch_1'`); err != nil {
			t.Fatal(err)
		}
		expectFail("rewrite an acknowledgement", `UPDATE secret_consumer_handling SET state = 'not_applicable', resolved_by = 'x' WHERE id = 'ch_1'`)
	})

	t.Run("owner can read and repeat does not change data", func(t *testing.T) {
		if _, err := storage.Migrate(ctx, mig, m, opts); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := mig.QueryRow(ctx, `SELECT count(*) FROM secrets`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("secrets after reapply: %d %v", n, err)
		}
	})
}
