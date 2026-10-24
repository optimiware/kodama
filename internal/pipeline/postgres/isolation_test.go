package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func Test_pipeline_tenant_isolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := pipelinetest.Doc(pipelinetest.Container("a"))
	inA := e.mustRegister(scopeA, engineer, src)
	inA2 := e.mustRegister(scopeA2, engineer, src)
	inB := e.mustRegister(scopeB, engineer, src)

	t.Run("deduplication is scoped to the project", func(t *testing.T) {
		if inA.Definition.ID == inA2.Definition.ID || inA.Definition.ID == inB.Definition.ID || inA2.Definition.ID == inB.Definition.ID {
			t.Fatal("identical content in different projects must be separate definitions")
		}
		if inA.Definition.Digest != inB.Definition.Digest {
			t.Fatal("the digest is content-only")
		}
	})

	t.Run("deduplication is not authorization", func(t *testing.T) {
		e.auth.Grant(scopeA, "u_viewer", pipeline.ActorUser, pipeline.PermRead)
		before := e.definitions(scopeA)
		_, err := e.register(scopeA, "u_viewer", src)
		if !errors.Is(err, pipeline.ErrDenied) {
			t.Fatalf("a caller without register permission must be denied even for existing content: %v", err)
		}
		_, err = e.register(scopeB, "u_unknown", src)
		if !errors.Is(err, pipeline.ErrHidden) {
			t.Fatalf("an unresolvable principal must be hidden: %v", err)
		}
		if e.definitions(scopeA) != before {
			t.Fatal("a denied request must not write")
		}
	})

	t.Run("foreign-project and foreign-tenant reads are not found", func(t *testing.T) {
		caller := pipeline.Caller{PrincipalID: engineer}
		for name, c := range map[string]struct {
			scope domain.Scope
			id    domain.DefinitionID
		}{
			"other project, same tenant": {scopeA2, inA.Definition.ID},
			"other tenant":               {scopeB, inA.Definition.ID},
			"reverse":                    {scopeA, inB.Definition.ID},
		} {
			if _, err := e.svc.Get(ctx, c.scope, caller, c.id); !errors.Is(err, pipeline.ErrNotFound) {
				t.Errorf("%s: expected not found, got %v", name, err)
			}
		}
		for _, c := range []struct {
			scope domain.Scope
			want  domain.DefinitionID
		}{{scopeA, inA.Definition.ID}, {scopeA2, inA2.Definition.ID}, {scopeB, inB.Definition.ID}} {
			page, err := e.svc.List(ctx, c.scope, caller, pipeline.ListQuery{Limit: 10})
			if err != nil || len(page) != 1 || page[0].ID != c.want {
				t.Errorf("list in %s: %+v %v", c.scope, page, err)
			}
		}
	})

	t.Run("a store call in the wrong tenant context sees nothing", func(t *testing.T) {
		// Ask the store about tenant A's definition with tenant B's project: the
		// explicit predicates and RLS both exclude it.
		wrong := domain.Scope{TenantID: tenantB, ProjectID: projectA}
		if _, err := e.store.Get(ctx, wrong, inA.Definition.ID); !errors.Is(err, pipeline.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
		page, err := e.store.List(ctx, wrong, pipeline.ListQuery{Limit: 10})
		if err != nil || len(page) != 0 {
			t.Fatalf("%+v %v", page, err)
		}
	})

	t.Run("row level security confines the runtime role", func(t *testing.T) {
		var n int
		if err := e.rt.QueryRow(ctx, `SELECT count(*) FROM pipeline_definitions`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("without a tenant context the runtime role sees %d rows (%v)", n, err)
		}
		err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pipeline_definitions`).Scan(&n); err != nil {
				return err
			}
			if n != 2 {
				t.Errorf("tenant A sees %d rows, want its 2", n)
			}
			var foreign int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pipeline_definitions WHERE tenant_id = $1`, tenantB).Scan(&foreign); err != nil || foreign != 0 {
				t.Errorf("tenant A sees %d of tenant B's rows (%v)", foreign, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// A foreign tenant's row cannot be inserted from another tenant context.
		err = e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, insertSQL, tenantB, projectB, "pd_forged", "sample", 9, string(inB.Definition.Digest), string(inB.Definition.Content.Canonical()))
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("forged cross-tenant insert: %v", err)
		}
	})

	t.Run("composite ownership rejects a project of another tenant", func(t *testing.T) {
		err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, insertSQL, tenantA, projectB, "pd_misowned", "sample", 9, string(inA.Definition.Digest), string(inA.Definition.Content.Canonical()))
			return err
		})
		if sqlState(err) != "23503" {
			t.Fatalf("a definition owned by tenant A in tenant B's project must violate the composite key: %v", err)
		}
		err = e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, insertSQL, tenantA, "p_ghost", "pd_orphan", "sample", 9, string(inA.Definition.Digest), string(inA.Definition.Content.Canonical()))
			return err
		})
		if sqlState(err) != "23503" {
			t.Fatalf("a definition in a missing project must violate the composite key: %v", err)
		}
	})

	t.Run("the runtime role cannot update or delete versions", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE pipeline_definitions SET name = 'renamed'`,
			`UPDATE pipeline_definitions SET body = body`,
			`DELETE FROM pipeline_definitions`,
			`TRUNCATE pipeline_definitions`,
		} {
			err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if sqlState(err) != "42501" {
				t.Errorf("%s: want insufficient_privilege, got %v", stmt, err)
			}
		}
		// Even the table owner is held by forced row level security: no policy
		// permits UPDATE or DELETE, so they affect nothing.
		for _, stmt := range []string{`UPDATE pipeline_definitions SET created_by_id = 'x'`, `DELETE FROM pipeline_definitions`} {
			tag, err := e.owner.Exec(ctx, stmt)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("owner %s: %d rows, %v", stmt, tag.RowsAffected(), err)
			}
		}
		if e.definitions(scopeA) != 1 || e.definitions(scopeB) != 1 {
			t.Fatal("rows were lost")
		}
	})

	t.Run("pooled connections never leak a tenant context", func(t *testing.T) {
		single := newEnvWith(t, envOptions{conns: 1})
		single.mustRegister(scopeA, engineer, src)
		for i := 0; i < 3; i++ {
			var n int
			if err := single.rt.QueryRow(ctx, `SELECT count(*) FROM pipeline_definitions`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("iteration %d: the reused connection sees %d rows (%v)", i, n, err)
			}
			if _, err := single.svc.List(ctx, scopeA, pipeline.Caller{PrincipalID: engineer}, pipeline.ListQuery{Limit: 5}); err != nil {
				t.Fatal(err)
			}
			if _, err := single.svc.List(ctx, scopeB, pipeline.Caller{PrincipalID: engineer}, pipeline.ListQuery{Limit: 5}); err != nil {
				t.Fatal(err)
			}
			var setting string
			if err := single.rt.QueryRow(ctx, `SELECT COALESCE(current_setting('kodama.tenant_id', true), '')`).Scan(&setting); err != nil || setting != "" {
				t.Fatalf("tenant setting leaked: %q %v", setting, err)
			}
		}
	})

	t.Run("a failed registration releases its tenant context too", func(t *testing.T) {
		single := newEnvWith(t, envOptions{conns: 1})
		single.ownerExec(`CREATE FUNCTION t014_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't014 audit failure'; END $$`)
		single.ownerExec(`CREATE TRIGGER t014_fail_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t014_fail_audit()`)
		if _, err := single.register(scopeA, engineer, src); err == nil {
			t.Fatal("expected a failure")
		}
		var n int
		if err := single.rt.QueryRow(ctx, `SELECT count(*) FROM pipeline_definitions`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("after a failed transaction the connection sees %d rows (%v)", n, err)
		}
	})

	t.Run("the database enforces what the entity guarantees", func(t *testing.T) {
		body, digest := string(inA.Definition.Content.Canonical()), string(inA.Definition.Digest)
		for name, args := range map[string][]any{
			"digest does not match the body": {tenantA, projectA, "pd_bad1", "sample", 9, "sha256:0000000000000000000000000000000000000000000000000000000000000000", body},
			"name does not match the body":   {tenantA, projectA, "pd_bad2", "other", 9, digest, body},
			"duplicate version":              {tenantA, projectA, "pd_bad3", "sample", int(inA.Definition.Version), pipeline.DigestOf([]byte(`{"x":1}`)), `{"x":1}`},
			"duplicate digest in a project":  {tenantA, projectA, "pd_bad4", "sample", 9, digest, body},
			"version zero":                   {tenantA, projectA, "pd_bad5", "sample", 0, digest, body},
			"not JSON":                       {tenantA, projectA, "pd_bad6", "sample", 9, pipeline.DigestOf([]byte("nope")), "nope"},
		} {
			err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, insertSQL, args...)
				return err
			})
			if code := sqlState(err); code != "23514" && code != "23505" && code != "22P02" {
				t.Errorf("%s: want a check, unique or syntax violation, got %v", name, err)
			}
		}
		if e.definitions(scopeA) != 1 {
			t.Fatal("a rejected insert left a row")
		}
	})
}

const insertSQL = `INSERT INTO pipeline_definitions (tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, 'user', 'u_forged', now())`
