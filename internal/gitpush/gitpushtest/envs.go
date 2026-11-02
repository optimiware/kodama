package gitpushtest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/gitauth"
	gitauthmem "github.com/optimiware/kodama/internal/gitauth/memory"
	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/gitpush"
	gitpushpg "github.com/optimiware/kodama/internal/gitpush/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Env is one store adapter's environment: the push store and the grant store
// over the same database or kernel (the seed consumption joins the push
// transaction), and the observations the contract reads outside the use cases.
type Env interface {
	Store() gitpush.Store
	GrantStore() gitauth.Store
	// SeedRepository makes the tenant, project and repository known to the store
	// (the composite foreign keys of the SQL schema).
	SeedRepository(t testing.TB, scope domain.Scope, repositoryID string)
	// SyncRepository makes the backing store's repository row agree with the
	// stand-in's facts (status ready or archived, default branch, push policy),
	// as the repository module's own transitions would: the push store re-reads
	// the row inside the authorization transaction. Other states are not synced.
	SyncRepository(t testing.TB, facts gitpush.RepositoryFacts)
	// UseRepositories tells an adapter that has no repositories table where to
	// read the repository state of Tx.RepositoryState from.
	UseRepositories(r gitpush.Repositories)
	// HoldGrantLock locks the grant's row in a transaction of its own, as a
	// cancelling transaction would, and holds it until the returned release
	// function ends that transaction unchanged. A commit intent of the grant's
	// attempt waits for it.
	HoldGrantLock(t testing.TB, scope domain.Scope, grantID string) (release func())
	// Events are the git.refs_updated events committed for the repository, in
	// commit order.
	Events(t testing.TB, scope domain.Scope, repositoryID string) []eventsv1.Envelope
	// AuditActions are the audit actions recorded for the push record, sorted.
	AuditActions(t testing.TB, scope domain.Scope, pushID string) []string
	// FailEvents makes every later event append fail after the real append (nil
	// clears it), to prove settlement and event commit together.
	FailEvents(err error)
}

// Open creates a fresh environment; it skips the test when the adapter's
// service is not available.
type Open func(testing.TB) Env

// MemEnv is the push persistence in memory: one memdb kernel shared by the push
// store, the grant store and the event bus.
type MemEnv struct {
	DB     *memdb.Store
	Bus    *eventstest.Mem
	Mem    *MemStore
	Grants *gitauthmem.Store

	mu    sync.Mutex
	fault error
}

var _ Env = (*MemEnv)(nil)

type memFaultAppender struct{ env *MemEnv }

func (f memFaultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := f.env.Bus.Append(tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewMemEnv wires the memory stores over a fresh kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	e := &MemEnv{DB: memdb.New(opts...)}
	var err error
	if e.Bus, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	if e.Mem, err = NewMemStore(e.DB, memFaultAppender{env: e}, MemOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.Grants, err = gitauthmem.New(e.DB); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenMem is the Open of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

func (e *MemEnv) Store() gitpush.Store      { return e.Mem }
func (e *MemEnv) GrantStore() gitauth.Store { return e.Grants }

func (e *MemEnv) SeedRepository(testing.TB, domain.Scope, string) {}

func (e *MemEnv) SyncRepository(testing.TB, gitpush.RepositoryFacts) {}

func (e *MemEnv) HoldGrantLock(t testing.TB, scope domain.Scope, _ string) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		// memdb serializes writers: an open Update is the lock every other writer waits for.
		done <- e.DB.Update(context.Background(), scope.TenantID, func(memdb.Tx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	return func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func (e *MemEnv) UseRepositories(r gitpush.Repositories) { e.Mem.UseRepositories(r) }

func (e *MemEnv) Events(t testing.TB, scope domain.Scope, repositoryID string) []eventsv1.Envelope {
	t.Helper()
	var out []eventsv1.Envelope
	err := e.DB.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		obs, err := e.Bus.ReadObservations(tx, events.Scope{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, 0, 1000)
		if err != nil {
			return err
		}
		for _, o := range obs {
			if o.Envelope.Type != eventsv1.TypeGitRefsUpdated {
				continue
			}
			data, err := eventsv1.DecodeData(o.Envelope)
			if err != nil {
				return err
			}
			if data.(*eventsv1.GitRefsUpdated).RepositoryID == repositoryID {
				out = append(out, o.Envelope)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *MemEnv) AuditActions(t testing.TB, scope domain.Scope, pushID string) []string {
	t.Helper()
	out, err := e.Mem.AuditActions(context.Background(), scope.TenantID, pushID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *MemEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

// PGEnv is the PostgreSQL adapter over an isolated migrated database: the
// baseline plus the repositories (007), Git grants (008), push (009) and
// operations (015) contributions.
type PGEnv struct {
	T testing.TB
	// DB, Owner and Runtime are the isolated database, its owner pool and the
	// non-owner runtime pool the stores use.
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	Bus     *eventspg.Store
	PG      *gitpushpg.Store
	Grants  *gitauthpg.Store

	mu    sync.Mutex
	fault error
}

var _ Env = (*PGEnv)(nil)

type pgFaultAppender struct{ env *PGEnv }

func (f pgFaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	res, err := f.env.Bus.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// OpenPostgres is the Open of the PostgreSQL adapter. It skips without
// PostgreSQL, and always in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
	return NewPGEnv(t)
}

// NewPGEnv provisions the database and applies the migrations.
func NewPGEnv(t testing.TB) *PGEnv {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &PGEnv{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 30)}
	manifest, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations(), gitauthpg.Migrations(), gitpushpg.Migrations(), gitpushpg.OperationsMigrations())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if e.Bus, err = eventspg.NewStore(e.Runtime, eventstest.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	if e.PG, err = gitpushpg.NewStore(e.Runtime, pgFaultAppender{env: e}, gitpushpg.Options{}); err != nil {
		t.Fatal(err)
	}
	if e.Grants, err = gitauthpg.NewStore(e.Runtime); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *PGEnv) Store() gitpush.Store      { return e.PG }
func (e *PGEnv) GrantStore() gitauth.Store { return e.Grants }

// OwnerExec runs a statement as the owner (outside row-level security).
func (e *PGEnv) OwnerExec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.Owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *PGEnv) SeedRepository(t testing.TB, scope domain.Scope, repositoryID string) {
	t.Helper()
	e.OwnerExec(t, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	e.OwnerExec(t, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5) ON CONFLICT DO NOTHING`,
		string(scope.TenantID), string(scope.ProjectID))
	e.OwnerExec(t, `INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'main', 'internal', 'ready', 'user', 'usr_fixture', now(), now()) ON CONFLICT DO NOTHING`,
		string(scope.TenantID), string(scope.ProjectID), repositoryID, strings.ReplaceAll(repositoryID, "_", "-"))
}

func (e *PGEnv) HoldGrantLock(t testing.TB, scope domain.Scope, grantID string) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- storage.WithTenant(context.Background(), e.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := gitauthpg.Bind(tx, scope.TenantID).GrantByID(ctx, grantID, true)
			close(locked)
			if err != nil {
				return err
			}
			<-release
			return nil
		})
	}()
	<-locked
	return func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func (e *PGEnv) UseRepositories(gitpush.Repositories) {}

func (e *PGEnv) SyncRepository(t testing.TB, f gitpush.RepositoryFacts) {
	t.Helper()
	if f.Status != gitv1.StatusReady && f.Status != gitv1.StatusArchived {
		return // a provisioning repository is never pushed to: the check precedes the transaction
	}
	e.OwnerExec(t, `UPDATE repositories SET default_branch = $3, status = $4, reason = CASE WHEN $4 = 'ready' THEN NULL ELSE 'archived' END,
		direct_push_to_default = $5, version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND (default_branch, status, direct_push_to_default) IS DISTINCT FROM ($3, $4, $5)`,
		string(f.Scope.TenantID), f.ID, f.DefaultBranch, string(f.Status), f.DirectPushToDefault)
}

func (e *PGEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

func (e *PGEnv) column(t testing.TB, tenant domain.TenantID, sql string, args ...any) [][]byte {
	t.Helper()
	var out [][]byte
	err := storage.WithTenant(context.Background(), e.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *PGEnv) Events(t testing.TB, scope domain.Scope, repositoryID string) []eventsv1.Envelope {
	t.Helper()
	var out []eventsv1.Envelope
	for _, raw := range e.column(t, scope.TenantID, `SELECT payload FROM outbox WHERE tenant_id = $1 AND event_type = 'git.refs_updated'
		AND convert_from(payload, 'UTF8')::jsonb -> 'data' ->> 'repositoryId' = $2 ORDER BY created_at, event_id`, string(scope.TenantID), repositoryID) {
		env, err := eventsv1.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	return out
}

func (e *PGEnv) AuditActions(t testing.TB, scope domain.Scope, pushID string) []string {
	t.Helper()
	var out []string
	for _, raw := range e.column(t, scope.TenantID, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type = 'git_push' AND resource_id = $2`,
		string(scope.TenantID), pushID) {
		out = append(out, string(raw))
	}
	slices.Sort(out)
	return out
}
