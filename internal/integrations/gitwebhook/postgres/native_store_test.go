package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var nativeAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// nativeEnv is a migrated database with the native trigger tables, three scopes
// (two tenants, two projects in the first) and their repositories and
// definitions seeded as the owner.
type nativeEnv struct {
	t       testing.TB
	owner   *pgxpool.Pool
	runtime *pgxpool.Pool
	db      *pgtest.Database
	store   *gwpg.NativeStore

	a, b, a2 domain.Scope
}

func nativeManifest(t testing.TB, c ...storage.Contribution) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(append([]storage.Contribution{storage.BaseContribution(), repopg.Migrations()}, c...)...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newNativeEnv(t testing.TB) *nativeEnv {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &nativeEnv{
		t: t, db: db, owner: db.MigrationPool(ctx, 4), runtime: db.RuntimePool(ctx, 20),
		a: domain.Scope{TenantID: "tenant-a", ProjectID: "project-a"}, b: domain.Scope{TenantID: "tenant-b", ProjectID: "project-b"},
		a2: domain.Scope{TenantID: "tenant-a", ProjectID: "project-a2"},
	}
	if _, err := storage.Migrate(ctx, e.owner, nativeManifest(t, gwpg.NativeMigrations()), storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var err error
	if e.store, err = gwpg.NewNativeStore(e.runtime, gwpg.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []domain.Scope{e.a, e.b, e.a2} {
		e.seed(s)
	}
	return e
}

func (e *nativeEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *nativeEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// seed creates the scope's tenant, project, repository repo_1 and the
// definitions pd_ci and pd_task.
func (e *nativeEnv) seed(s domain.Scope) {
	e.t.Helper()
	e.exec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(s.TenantID))
	e.exec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5) ON CONFLICT DO NOTHING`, string(s.TenantID), string(s.ProjectID))
	e.exec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1, $2, 'repo_1', 'main-repo', 'main', 'internal', 'ready', 'user', 'usr_fixture', now(), now()) ON CONFLICT DO NOTHING`, string(s.TenantID), string(s.ProjectID))
	for i, id := range []string{"pd_ci", "pd_task"} {
		body := `{"definition":"` + id + `"}`
		sum := sha256.Sum256([]byte(body))
		e.exec(`INSERT INTO pipeline_definitions (tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
			VALUES ($1, $2, $3, $4, 1, $5, $6, 'user', 'usr_fixture', now()) ON CONFLICT DO NOTHING`,
			string(s.TenantID), string(s.ProjectID), id, []string{"ci", "task"}[i], "sha256:"+hex.EncodeToString(sum[:]), body)
	}
}

func (e *nativeEnv) trigger(scope domain.Scope, id string) gw.PushTrigger {
	v := true
	return gw.PushTrigger{
		Scope: scope, ConfiguredBy: "usr_1", Version: 1, EnabledAt: nativeAt, CreatedAt: nativeAt, UpdatedAt: nativeAt,
		Config: gitv1.TriggerConfig{TriggerID: id, RepositoryID: "repo_1", RefPatterns: []string{"refs/heads/main"}, ExcludeOriginRuns: &v, DefinitionID: "pd_ci", Enabled: true},
	}
}

func (e *nativeEnv) audit(action string, scope domain.Scope, id string) gw.AuditRecord {
	return gw.AuditRecord{Action: action, Scope: scope, TriggerID: gw.TriggerID(id), Principal: "usr_1", At: nativeAt, Metadata: map[string]string{"k": "v"}}
}

// TestPostgresNativeStoreContract runs the shared contract on real PostgreSQL
// (GR-A07, PX-A20): scoped, versioned configuration and the deduplicating inbox.
func TestPostgresNativeStoreContract(t *testing.T) {
	gitwebhooktest.NativeStoreContract(t, func(t testing.TB) gitwebhooktest.NativeSubject {
		e := newNativeEnv(t)
		return gitwebhooktest.NativeSubject{
			Store: e.store,
			Restart: func() gw.NativeStore {
				s, err := gwpg.NewNativeStore(e.db.RuntimePool(context.Background(), 4), gwpg.Options{})
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
			ScopeA: e.a, ScopeB: e.b, ScopeA2: e.a2,
			NewTrigger: func(t testing.TB, scope domain.Scope, id string) gw.PushTrigger { return e.trigger(scope, id) },
			NewRoute: func(t testing.TB, scope domain.Scope) gw.TaskRoute {
				return gw.TaskRoute{Scope: scope, DefinitionID: "pd_task", Enabled: true, ConfiguredBy: "usr_1", Version: 1, EnabledAt: nativeAt, CreatedAt: nativeAt, UpdatedAt: nativeAt}
			},
			Audits: func(t testing.TB, scope domain.Scope, action, resource string) int {
				return e.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND project_id = $2 AND action = $3 AND resource_id = $4`,
					string(scope.TenantID), string(scope.ProjectID), action, resource)
			},
		}
	})
}

// TestPostgresNativeMigration: the contribution is the reserved slot 012, applies
// after 007, reapplies as a no-op, refuses an edit, and its tables carry the
// constraints, row level security and least-privilege grants the store relies on.
func TestPostgresNativeMigration(t *testing.T) {
	ctx := context.Background()
	if got := gwpg.NativeMigrations().Migrations; len(got) != 1 || got[0].ID != "012_git_triggers.sql" || gwpg.NativeMigrationID != "012_git_triggers.sql" {
		t.Fatalf("the contribution is one migration, 012_git_triggers.sql: %+v", got)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	m := nativeManifest(t, gwpg.NativeMigrations())
	first, err := storage.Migrate(ctx, owner, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Migrate(ctx, owner, m, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := gwpg.NativeMigrations()
	changed.Migrations[0].SQL += "\n-- edited after merge"
	if _, err := storage.Migrate(ctx, owner, nativeManifest(t, changed), opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}
	for table, names := range map[string][]string{
		"git_push_triggers": {"git_push_triggers_pkey", "git_push_triggers_patterns_count", "git_push_triggers_version_range",
			"git_push_triggers_tenant_id_project_id_repository_id_fkey", "git_push_triggers_tenant_id_project_id_definition_id_fkey"},
		"git_task_routes":   {"git_task_routes_pkey", "git_task_routes_tenant_id_project_id_definition_id_fkey"},
		"git_trigger_inbox": {"git_trigger_inbox_pkey", "git_trigger_inbox_done_shape", "git_trigger_inbox_ref_shape", "git_trigger_inbox_lease_pair"},
	} {
		for _, name := range names {
			var n int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`, name, table).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: constraint %s missing (%d, %v)", table, name, n, err)
			}
		}
		var rls, force bool
		var policies int
		if err := owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity,
			(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid AND pg_get_expr(p.polqual, p.polrelid) LIKE '%kodama_tenant_id()%')
			FROM pg_class c WHERE c.relname = $1`, table).Scan(&rls, &force, &policies); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || policies < 1 {
			t.Errorf("%s: rls=%v force=%v tenant policies=%d", table, rls, force, policies)
		}
	}
	// Every grant names the runtime role and none deletes or grants everything.
	sql := gwpg.NativeMigrations().Migrations[0].SQL
	for _, g := range regexp.MustCompile(`(?m)^GRANT\b[^;]*;`).FindAllString(sql, -1) {
		if !strings.Contains(g, storage.RuntimeRolePlaceholder) {
			t.Errorf("a grant that does not name the runtime role: %s", g)
		}
		for _, bad := range []string{"DELETE", "ALL", "TRUNCATE", "PUBLIC", "WITH GRANT OPTION"} {
			if regexp.MustCompile(`(?i)\b` + bad + `\b`).MatchString(g) {
				t.Errorf("grant mentions %s: %s", bad, g)
			}
		}
	}
	for _, forbidden := range []string{"ALTER TABLE git_triggers", "ALTER TABLE git_trigger_deliveries", "ALTER TABLE git_trigger_admissions", "ALTER TABLE projects", "DROP "} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("the migration touches the external trigger or another contribution: %s", forbidden)
		}
	}
}

// TestPostgresNativeIsolation: tenant isolation, ownership by composite foreign
// keys, the guards that hold for every role, and the absence of any delete.
func TestPostgresNativeIsolation(t *testing.T) {
	ctx := context.Background()
	e := newNativeEnv(t)
	for _, id := range []string{"trg_1"} {
		if err := e.store.InsertPushTrigger(ctx, e.trigger(e.a, id), e.audit(gw.AuditPushTriggerCreated, e.a, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.PutTaskRoute(ctx, gw.TaskRoute{Scope: e.a, DefinitionID: "pd_task", Enabled: true, ConfiguredBy: "usr_1", Version: 1, EnabledAt: nativeAt, CreatedAt: nativeAt, UpdatedAt: nativeAt}, 0, e.audit(gw.AuditTaskRouteConfigured, e.a, "project-a")); err != nil {
		t.Fatal(err)
	}
	key := gw.InboxKey{Scope: e.a, Kind: gw.InboxPush, Source: "trg_1", Subject: "push_1", Ref: "refs/heads/main"}
	if _, _, err := e.store.ClaimInbox(ctx, key, "w1", nativeAt, time.Minute); err != nil {
		t.Fatal(err)
	}

	t.Run("a tenant context sees only its own rows, whatever the predicates", func(t *testing.T) {
		for _, table := range []string{"git_push_triggers", "git_task_routes", "git_trigger_inbox"} {
			var seen int
			err := storage.WithTenant(ctx, e.runtime, e.b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&seen)
			})
			if err != nil || seen != 0 {
				t.Errorf("%s: tenant B sees %d rows (%v)", table, seen, err)
			}
		}
		// Without a tenant context nothing is visible (fails closed).
		var n int
		if err := e.runtime.QueryRow(ctx, `SELECT count(*) FROM git_trigger_inbox`).Scan(&n); err == nil && n != 0 {
			t.Errorf("rows visible without a tenant context: %d", n)
		}
	})

	t.Run("a foreign tenant cannot insert for another tenant's scope", func(t *testing.T) {
		err := storage.WithTenant(ctx, e.runtime, e.b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO git_trigger_inbox (tenant_id, project_id, kind, source, subject, ref, received_at) VALUES ('tenant-a', 'project-a', 'push', 's', 'p', 'refs/heads/x', now())`)
			return err
		})
		if err == nil {
			t.Fatal("the tenant policy let a row of another tenant in")
		}
	})

	t.Run("ownership is composite: foreign repository, definition and project are refused", func(t *testing.T) {
		// A repository that exists only in tenant B.
		e.exec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ('tenant-b', 'project-b', 'repo_b_only', 'b-only', 'main', 'internal', 'ready', 'user', 'u', now(), now())`)
		foreign := e.trigger(e.a, "trg_f")
		foreign.Config.RepositoryID = "repo_b_only"
		if err := e.store.InsertPushTrigger(ctx, foreign, e.audit(gw.AuditPushTriggerCreated, e.a, "trg_f")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a trigger on another tenant's repository: %v", err)
		}
		def := e.trigger(e.a, "trg_d")
		def.Config.DefinitionID = "pd_unknown"
		if err := e.store.InsertPushTrigger(ctx, def, e.audit(gw.AuditPushTriggerCreated, e.a, "trg_d")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unregistered definition: %v", err)
		}
		if _, err := e.store.PutTaskRoute(ctx, gw.TaskRoute{Scope: e.a2, DefinitionID: "pd_missing", Enabled: true, ConfiguredBy: "u", Version: 1, EnabledAt: nativeAt, CreatedAt: nativeAt, UpdatedAt: nativeAt}, 0, e.audit(gw.AuditTaskRouteConfigured, e.a2, "project-a2")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a route to an unregistered definition: %v", err)
		}
		if n := e.count(`SELECT count(*) FROM audit WHERE action = $1 AND resource_id IN ('trg_f', 'trg_d')`, gw.AuditPushTriggerCreated); n != 0 {
			t.Fatalf("a refused creation leaves no audit, got %d", n)
		}
		unknown := e.trigger(domain.Scope{TenantID: "tenant-a", ProjectID: "project-unknown"}, "trg_u")
		if err := e.store.InsertPushTrigger(ctx, unknown, e.audit(gw.AuditPushTriggerCreated, unknown.Scope, "trg_u")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unknown project: %v", err)
		}
	})

	t.Run("guards hold for every role and the runtime role never deletes", func(t *testing.T) {
		// Owner: the guards still refuse.
		for name, sql := range map[string]string{
			"repository rebinding": `UPDATE git_push_triggers SET repository_id = 'repo_x', version = version + 1`,
			"version skip":         `UPDATE git_push_triggers SET enabled = false, version = version + 5`,
			"no version bump":      `UPDATE git_push_triggers SET enabled = false`,
			"inbox key change":     `UPDATE git_trigger_inbox SET subject = 'other'`,
			"route project change": `UPDATE git_task_routes SET project_id = 'project-a2', version = version + 1`,
		} {
			if _, err := e.owner.Exec(ctx, sql); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		// Runtime role: no delete on any table, no truncate.
		for _, table := range []string{"git_push_triggers", "git_task_routes", "git_trigger_inbox"} {
			err := storage.WithTenant(ctx, e.runtime, e.a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM `+table)
				return err
			})
			if err == nil {
				t.Errorf("the runtime role deleted from %s", table)
			}
		}
		// A decided inbox key is final even for the owner.
		if _, err := e.store.FinishInbox(ctx, key, gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_1"}, nativeAt, e.audit(gw.AuditPushRunStarted, e.a, "trg_1")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.owner.Exec(ctx, `UPDATE git_trigger_inbox SET outcome = 'rejected', reason = 'x', run_id = NULL`); err == nil {
			t.Error("a decided key was changed")
		}
		// And the CHECK shapes: a started row without a run cannot exist.
		if _, err := e.owner.Exec(ctx, `INSERT INTO git_trigger_inbox (tenant_id, project_id, kind, source, subject, ref, done, outcome, run_id, received_at, completed_at)
			VALUES ('tenant-a', 'project-a', 'push', 's', 'p', 'refs/heads/x', true, 'started', NULL, now(), now())`); err == nil {
			t.Error("a started outcome without a run was accepted")
		}
		if _, err := e.owner.Exec(ctx, `INSERT INTO git_trigger_inbox (tenant_id, project_id, kind, source, subject, ref, received_at)
			VALUES ('tenant-a', 'project-a', 'task_ready', 's', 'p', 'refs/heads/x', now())`); err == nil {
			t.Error("a task key with a ref was accepted")
		}
	})

	t.Run("outcome and audit commit together and the audit names the safe fields", func(t *testing.T) {
		var action, resource, actor string
		var meta string
		if err := e.owner.QueryRow(ctx, `SELECT action, resource_type, actor_id, metadata::text FROM audit WHERE action = $1 AND resource_id = 'trg_1'`, gw.AuditPushRunStarted).Scan(&action, &resource, &actor, &meta); err != nil {
			t.Fatal(err)
		}
		if resource != "git_push_trigger" || actor != "usr_1" || !strings.Contains(meta, `"k"`) {
			t.Fatalf("audit: %s %s %s %s", action, resource, actor, meta)
		}
		if n := e.count(`SELECT count(*) FROM audit WHERE action = $1`, gw.AuditTaskRouteConfigured); n != 1 {
			t.Fatalf("route audit: %d", n)
		}
		var rt string
		if err := e.owner.QueryRow(ctx, `SELECT resource_type FROM audit WHERE action = $1`, gw.AuditTaskRouteConfigured).Scan(&rt); err != nil || rt != "git_task_route" {
			t.Fatalf("route audit resource type %q %v", rt, err)
		}
	})

	t.Run("a lost acknowledgement leaves the decided key intact across a restart", func(t *testing.T) {
		restarted, err := gwpg.NewNativeStore(e.db.RuntimePool(ctx, 4), gwpg.Options{})
		if err != nil {
			t.Fatal(err)
		}
		rec, acquired, err := restarted.ClaimInbox(ctx, key, "w9", nativeAt.Add(time.Hour), time.Minute)
		if err != nil || acquired || !rec.Done || rec.RunID != "run_1" {
			t.Fatalf("%+v %v %v", rec, acquired, err)
		}
	})
}
