package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// TestTenantPersistence: RLS, grants, pooled context reset, composite
// ownership, immutable columns, nondisclosing lookups and rollback of state and
// audit changes, on a real PostgreSQL under the non-owner runtime role.
func TestTenantPersistence(t *testing.T) {
	env := artifactstest.NewDB(t, artifactstest.Options{RuntimeConns: 1})
	ctx := env.Ctx()
	store := env.Store

	a := fileArtifact(t, scopeA, "att_a", "report", []byte("alpha"), artifacts.RoleOutput)
	b := fileArtifact(t, scopeB, "att_b", "report", []byte("beta"), artifacts.RoleOutput)
	for _, x := range []artifacts.Artifact{a, b} {
		res, err := store.Reserve(ctx, reserveCmd(x, bigOK))
		if err != nil || !res.Created {
			t.Fatalf("reserve: %+v %v", res, err)
		}
	}

	t.Run("lookups never cross tenant, project or attempt", func(t *testing.T) {
		if got, err := store.Get(ctx, scopeA, a.ID()); err != nil || got.ID() != a.ID() {
			t.Fatalf("own artifact: %v", err)
		}
		// The same ID under another tenant, another project of the same tenant
		// and the other tenant's scope are all just "not found".
		for name, sc := range map[string]domain.Scope{
			"other tenant":  scopeB,
			"other project": scopeA2,
			"mixed":         {TenantID: scopeA.TenantID, ProjectID: scopeB.ProjectID},
		} {
			if _, err := store.Get(ctx, sc, a.ID()); !errors.Is(err, artifacts.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
			if _, err := store.GetForAttempt(ctx, sc, "att_a", a.ID()); !errors.Is(err, artifacts.ErrNotFound) {
				t.Errorf("%s (attempt): %v", name, err)
			}
			if _, err := store.FindByName(ctx, sc, "att_a", "report"); !errors.Is(err, artifacts.ErrNotFound) {
				t.Errorf("%s (name): %v", name, err)
			}
			refs, err := store.ReferencesForAttempt(ctx, sc, "att_a", []domain.ArtifactID{a.ID()})
			if err != nil || len(refs) != 0 {
				t.Errorf("%s (refs): %v %v", name, refs, err)
			}
		}
		if _, err := store.GetForAttempt(ctx, scopeA, "att_other", a.ID()); !errors.Is(err, artifacts.ErrNotFound) {
			t.Errorf("another attempt of the same scope must not see the artifact: %v", err)
		}
		refs, err := store.ReferencesForAttempt(ctx, scopeA, "att_a", []domain.ArtifactID{a.ID(), b.ID(), "art_nope"})
		if err != nil || len(refs) != 1 || refs[a.ID()].Usable || refs[a.ID()].State != artifacts.StateReserved {
			t.Errorf("only the attempt's own, unfinished artifact: %+v %v", refs, err)
		}
	})

	t.Run("row level security confines every table", func(t *testing.T) {
		for _, tbl := range []string{"artifacts", "artifact_objects", "artifact_pins", "audit"} {
			var n int
			err := storage.WithTenant(ctx, env.Runtime, scopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, string(scopeA.TenantID)).Scan(&n)
			})
			if err != nil || n != 0 {
				t.Errorf("%s: tenant B sees %d of tenant A's rows (%v)", tbl, n, err)
			}
		}
		// No tenant context: nothing visible, nothing writable.
		var n int
		if err := env.Runtime.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&n); err != nil || n != 0 {
			t.Errorf("without a tenant context %d rows are visible (%v)", n, err)
		}
		_, err := env.Runtime.Exec(ctx, `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, staging_key, transfer_expires_at, state, created_at, updated_at, version)
VALUES ('t_acme', 'p_inventory', 'art_x', 'r', 'att_x', 's', 'n', 'file', 'output', $1, 'text/plain', 1, 'artifacts/x', now(), 'reserved', now(), now(), 1)`, string(artifacts.DigestOf([]byte("x"))))
		if err == nil {
			t.Error("an insert without tenant context succeeded")
		}
		// A write for tenant A from tenant B's context is refused by the policy.
		err = storage.WithTenant(ctx, env.Runtime, scopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, staging_key, transfer_expires_at, state, created_at, updated_at, version)
VALUES ('t_acme', 'p_inventory', 'art_x', 'r', 'att_x', 's', 'n', 'file', 'output', $1, 'text/plain', 1, 'artifacts/x', now(), 'reserved', now(), now(), 1)`, string(artifacts.DigestOf([]byte("x"))))
			return err
		})
		if err == nil {
			t.Error("tenant B wrote a row owned by tenant A")
		}
	})

	t.Run("pooled connections do not carry tenant context", func(t *testing.T) {
		// The pool has one connection: a tenant transaction, then a plain query on
		// the very same connection.
		if _, err := store.Get(ctx, scopeA, a.ID()); err != nil {
			t.Fatal(err)
		}
		var setting string
		if err := env.Runtime.QueryRow(ctx, `SELECT coalesce(current_setting('kodama.tenant_id', true), '')`).Scan(&setting); err != nil || setting != "" {
			t.Fatalf("tenant context leaked across the pool: %q %v", setting, err)
		}
		var n int
		if err := env.Runtime.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a reused connection saw %d rows", n)
		}
	})

	t.Run("composite ownership rejects a project of another tenant", func(t *testing.T) {
		_, err := env.Owner.Exec(ctx, `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, staging_key, transfer_expires_at, state, created_at, updated_at, version)
VALUES ('t_acme', 'p_ops', 'art_y', 'r', 'att_y', 's', 'n', 'file', 'output', $1, 'text/plain', 1, 'artifacts/y', now(), 'reserved', now(), now(), 1)`, string(artifacts.DigestOf([]byte("x"))))
		if err == nil {
			t.Fatal("an artifact in another tenant's project was accepted")
		}
		bad := fileArtifact(t, domain.Scope{TenantID: scopeA.TenantID, ProjectID: scopeB.ProjectID}, "att_y", "n", []byte("x"), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(bad, bigOK)); err == nil {
			t.Fatal("the store reserved into a project the tenant does not own")
		}
	})

	t.Run("runtime role cannot rewrite identity or delete", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE artifacts SET digest = 'sha256:` + strings.Repeat("0", 64) + `'`,
			`UPDATE artifacts SET attempt_id = 'att_z'`,
			`UPDATE artifacts SET staging_key = 'artifacts/z'`,
			`UPDATE artifacts SET source_commit = '` + strings.Repeat("c", 40) + `', source_repository_url = 'https://x.example/y'`,
			`DELETE FROM artifacts`,
			`DELETE FROM artifact_objects`,
			`UPDATE artifact_objects SET key = 'artifacts/q'`,
		} {
			err := storage.WithTenant(ctx, env.Runtime, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if err == nil {
				t.Errorf("statement succeeded: %s", stmt)
			}
		}
	})

	t.Run("audit rows commit with the transition and carry no secrets", func(t *testing.T) {
		var action, actor, meta string
		err := env.Owner.QueryRow(ctx, `SELECT action, actor_type || ':' || actor_id, metadata::text FROM audit WHERE resource_id = $1`, string(a.ID())).Scan(&action, &actor, &meta)
		if err != nil {
			t.Fatal(err)
		}
		if action != "artifact.reserved" || actor != "workload:att_a" {
			t.Fatalf("audit: %s %s", action, actor)
		}
		for _, leak := range []string{"artifacts/", "staging", string(a.Digest()), "http"} {
			if strings.Contains(meta, leak) {
				t.Errorf("audit metadata discloses %q: %s", leak, meta)
			}
		}
	})

	t.Run("a failure rolls state and audit back together", func(t *testing.T) {
		// Fail the audit insert of the completion: the owner installs a trigger
		// that rejects completion audit rows.
		env.Exec(`CREATE FUNCTION t015_block_completed() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.action = 'artifact.completed' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
		env.Exec(`CREATE TRIGGER t015_block BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t015_block_completed()`)
		defer env.Exec(`DROP TRIGGER t015_block ON audit`)

		now := at()
		cr, err := store.BeginCompletion(ctx, artifacts.BeginCompletion{Scope: scopeA, Attempt: "att_a", ID: a.ID(), Token: "tok1",
			Destination: artifacts.ObjectKey(scopeA, a.ID(), artifacts.SegmentSealed, "d1"), Lease: 30e9, Now: now})
		if err != nil || cr.Claim == nil {
			t.Fatalf("begin: %v", err)
		}
		_, err = store.CommitUsable(ctx, artifacts.CommitCommand{Scope: scopeA, Attempt: "att_a", ID: a.ID(), Claim: *cr.Claim,
			Destination:  artifacts.ObjectKey(scopeA, a.ID(), artifacts.SegmentSealed, "d1"),
			Verification: artifacts.Verification{Digest: a.Digest(), Size: 5}, Actor: artifacts.Actor{Type: "workload", ID: "att_a"}, Now: now})
		if err == nil {
			t.Fatal("the commit succeeded although its audit record could not be written")
		}
		got, err := store.Get(ctx, scopeA, a.ID())
		if err != nil || got.State() != artifacts.StateReserved || got.Version() != 1 || got.SealedKey() != "" {
			t.Fatalf("state changed without its audit record: %v %v", got, err)
		}
		if n := env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(a.ID())); n != 0 {
			t.Fatalf("winner flag survived the rollback: %d", n)
		}
	})

	t.Run("a refused reservation leaves nothing behind", func(t *testing.T) {
		tiny := artifacts.Limits{OutputBytes: 1, OutputFiles: 5}
		x := fileArtifact(t, scopeA, "att_lim", "big", []byte("12345"), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(x, tiny)); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("limit: %v", err)
		}
		if n := env.Count(`SELECT count(*) FROM artifacts WHERE attempt_id = 'att_lim'`) + env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1`, string(x.ID())) +
			env.Count(`SELECT count(*) FROM audit WHERE resource_id = $1`, string(x.ID())); n != 0 {
			t.Fatalf("a refused reservation left %d rows", n)
		}
	})

	t.Run("concurrent reads in different tenants stay isolated", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := store.Get(ctx, scopeA, a.ID()); err != nil {
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := store.Get(ctx, scopeB, a.ID()); !errors.Is(err, artifacts.ErrNotFound) {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}
