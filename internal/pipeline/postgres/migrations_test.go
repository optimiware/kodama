package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

func Test_pipeline_migrations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := e.mustRegister(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a")))

	t.Run("reapplication is a no-op that preserves rows", func(t *testing.T) {
		manifest, err := storage.NewManifest(storage.BaseContribution())
		if err != nil {
			t.Fatal(err)
		}
		out, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Applied) != 0 || out.Skipped != 2 {
			t.Fatalf("reapply applied %v skipped %d", out.Applied, out.Skipped)
		}
		got, err := e.store.Get(ctx, scopeA, res.Definition.ID)
		if err != nil || got.Digest != res.Definition.Digest {
			t.Fatalf("row after reapply: %+v %v", got, err)
		}
	})

	t.Run("an edited migration is detected", func(t *testing.T) {
		m := storage.BaseContribution()
		m.Migrations[0].SQL += "\n-- edited"
		manifest, err := storage.NewManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole}); err == nil {
			t.Fatal("a modified applied migration must be rejected")
		}
	})

	t.Run("row level security is enabled and forced", func(t *testing.T) {
		var rls, forced bool
		if err := e.owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'pipeline_definitions'`).Scan(&rls, &forced); err != nil || !rls || !forced {
			t.Fatalf("row level security: %v %v %v", rls, forced, err)
		}
		var policies []string
		rows, err := e.owner.Query(ctx, `SELECT policyname || ':' || cmd FROM pg_policies WHERE tablename = 'pipeline_definitions' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			policies = append(policies, p)
		}
		want := "owner_insert:INSERT,owner_select:SELECT,tenant_insert:INSERT,tenant_select:SELECT"
		if got := strings.Join(policies, ","); got != want {
			t.Fatalf("policies = %s, want %s (no UPDATE or DELETE policy may exist)", got, want)
		}
	})

	t.Run("runtime grants are minimal", func(t *testing.T) {
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false, "REFERENCES": false, "TRIGGER": false} {
			var has bool
			if err := e.owner.QueryRow(ctx, `SELECT has_table_privilege($1, 'pipeline_definitions', $2)`, e.db.RuntimeRole, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s for the runtime role = %v, want %v", priv, has, want)
			}
		}
	})

	t.Run("constraints and indexes are installed", func(t *testing.T) {
		for _, name := range []string{
			"pipeline_definitions_pkey", "pipeline_definitions_scope_key", "pipeline_definitions_pin_key",
			"pipeline_definitions_name_version_key", "pipeline_definitions_digest_key", "pipeline_definitions_name_syntax",
			"pipeline_definitions_version_range", "pipeline_definitions_digest_syntax", "pipeline_definitions_digest_matches_body",
			"pipeline_definitions_name_matches_body", "pipeline_definitions_body_object",
		} {
			if n := e.count(`SELECT count(*) FROM pg_constraint WHERE conname = $1`, name); n != 1 {
				t.Errorf("constraint %s: %d", name, n)
			}
		}
		var composite int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			WHERE t.relname = 'pipeline_definitions' AND c.contype = 'f' AND array_length(c.conkey, 1) = 2`).Scan(&composite); err != nil || composite != 1 {
			t.Fatalf("composite project foreign key: %d %v", composite, err)
		}
		for _, idx := range []string{"pipeline_definitions_list_idx", "pipeline_definitions_name_list_idx"} {
			if n := e.count(`SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx); n != 1 {
				t.Errorf("index %s: %d", idx, n)
			}
		}
		// The unique keys scope uniqueness to the project, never globally.
		for _, key := range []string{"pipeline_definitions_digest_key", "pipeline_definitions_name_version_key"} {
			var def string
			if err := e.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, key).Scan(&def); err != nil || !strings.Contains(def, "tenant_id, project_id") {
				t.Errorf("%s = %q (%v)", key, def, err)
			}
		}
	})

	t.Run("a future run can reference the exact pinned definition", func(t *testing.T) {
		e.ownerExec(`CREATE TABLE t014_runs (tenant_id kodama_id NOT NULL, project_id kodama_id NOT NULL, id kodama_id NOT NULL,
			definition_id kodama_id NOT NULL, definition_digest text NOT NULL, PRIMARY KEY (tenant_id, id),
			FOREIGN KEY (tenant_id, project_id, definition_id, definition_digest) REFERENCES pipeline_definitions (tenant_id, project_id, id, digest))`)
		insert := func(tenant, project, id, digest string) error {
			_, err := e.owner.Exec(ctx, `INSERT INTO t014_runs VALUES ($1, $2, $3, $4, $5)`, tenant, project, "run_"+id, id, digest)
			return err
		}
		if err := insert(tenantA, projectA, string(res.Definition.ID), string(res.Definition.Digest)); err != nil {
			t.Fatalf("a run in the owning project must reference the definition: %v", err)
		}
		if err := insert(tenantA, projectA2, string(res.Definition.ID), string(res.Definition.Digest)); err == nil {
			t.Fatal("a run in another project must not reference the definition")
		}
		if err := insert(tenantA, projectA, string(res.Definition.ID), string(pipeline.DigestOf([]byte("other")))); err == nil {
			t.Fatal("a run must not pin a digest the definition does not have")
		}
	})
}
