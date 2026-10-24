package postgres_test

import (
	"context"
	"strings"
	"testing"

	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

func TestAgentMigrations(t *testing.T) {
	t.Run("the contribution is ordered and valid after the base", func(t *testing.T) {
		c := agentspg.Migrations()
		if c.Module != "agents" || len(c.Migrations) != 1 || c.Migrations[0].ID != "T030_001_agent_profiles.sql" {
			t.Fatalf("contribution = %+v", c)
		}
		if !strings.Contains(c.Migrations[0].SQL, storage.RuntimeRolePlaceholder) {
			t.Fatal("grants must follow the deployment's runtime role")
		}
		if _, err := storage.NewManifest(storage.BaseContribution(), c); err != nil {
			t.Fatal(err)
		}
	})

	e := newEnv(t)
	ctx := context.Background()
	res := e.mustRegister(scopeA, engineer, e.draft)

	t.Run("reapplication is a no-op that preserves rows", func(t *testing.T) {
		manifest, err := storage.NewManifest(storage.BaseContribution(), agentspg.Migrations())
		if err != nil {
			t.Fatal(err)
		}
		out, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Applied) != 0 || out.Skipped < 4 {
			t.Fatalf("reapply applied %v skipped %d", out.Applied, out.Skipped)
		}
		got, err := e.store.Get(ctx, scopeA, "coding", 1)
		if err != nil || got.Digest != res.Profile.Digest {
			t.Fatalf("row after reapply: %+v %v", got, err)
		}
	})

	t.Run("an edited migration is detected", func(t *testing.T) {
		m := agentspg.Migrations()
		m.Migrations[0].SQL += "\n-- edited"
		manifest, err := storage.NewManifest(storage.BaseContribution(), m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole}); err == nil {
			t.Fatal("a modified applied migration must be rejected")
		}
	})

	t.Run("row level security is enabled and forced with no update or delete policy", func(t *testing.T) {
		var rls, forced bool
		if err := e.owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'agent_profiles'`).Scan(&rls, &forced); err != nil || !rls || !forced {
			t.Fatalf("row level security: %v %v %v", rls, forced, err)
		}
		rows, err := e.owner.Query(ctx, `SELECT policyname || ':' || cmd FROM pg_policies WHERE tablename = 'agent_profiles' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var policies []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			policies = append(policies, p)
		}
		want := "owner_insert:INSERT,owner_select:SELECT,tenant_insert:INSERT,tenant_select:SELECT"
		if got := strings.Join(policies, ","); got != want {
			t.Fatalf("policies = %s, want %s", got, want)
		}
	})

	t.Run("runtime grants are minimal", func(t *testing.T) {
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false, "REFERENCES": false, "TRIGGER": false} {
			var has bool
			if err := e.owner.QueryRow(ctx, `SELECT has_table_privilege($1, 'agent_profiles', $2)`, e.db.RuntimeRole, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s for the runtime role = %v, want %v", priv, has, want)
			}
		}
	})

	t.Run("constraints and indexes are installed and scoped to the project", func(t *testing.T) {
		for _, name := range []string{
			"agent_profiles_pkey", "agent_profiles_scope_key", "agent_profiles_name_version_key", "agent_profiles_digest_key",
			"agent_profiles_name_syntax", "agent_profiles_version_range", "agent_profiles_digest_syntax", "agent_profiles_digest_matches_body",
			"agent_profiles_name_matches_body", "agent_profiles_body_object", "agent_profiles_body_size",
		} {
			if n := e.count(`SELECT count(*) FROM pg_constraint WHERE conname = $1`, name); n != 1 {
				t.Errorf("constraint %s: %d", name, n)
			}
		}
		var composite int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			WHERE t.relname = 'agent_profiles' AND c.contype = 'f' AND array_length(c.conkey, 1) = 2`).Scan(&composite); err != nil || composite != 1 {
			t.Fatalf("composite project foreign key: %d %v", composite, err)
		}
		for _, idx := range []string{"agent_profiles_list_idx", "agent_profiles_name_list_idx"} {
			if n := e.count(`SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx); n != 1 {
				t.Errorf("index %s: %d", idx, n)
			}
		}
		for _, key := range []string{"agent_profiles_digest_key", "agent_profiles_name_version_key"} {
			var def string
			if err := e.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, key).Scan(&def); err != nil || !strings.Contains(def, "tenant_id, project_id") {
				t.Errorf("%s = %q (%v)", key, def, err)
			}
		}
	})

	t.Run("the table is not referenced by runs: a run pins the full content itself", func(t *testing.T) {
		// Migration prerequisites only: the agents table references tenants and projects.
		var refs int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid WHERE t.relname = 'agent_profiles' AND c.contype = 'f'`).Scan(&refs); err != nil || refs != 2 {
			t.Fatalf("foreign keys: %d %v", refs, err)
		}
	})
}
