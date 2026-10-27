package workspacetest

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	gitfx "github.com/optimiware/kodama/test/support/git"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Fixed identifiers of the fixture tenants; the t013 prefix marks the task's
// test resources.
var (
	ScopeA = domain.Scope{TenantID: "t013_acme", ProjectID: "p013_inventory"}
	ScopeB = domain.Scope{TenantID: "t013_globex", ProjectID: "p013_billing"}
	// ScopeA2 is a second project of tenant A.
	ScopeA2 = domain.Scope{TenantID: "t013_acme", ProjectID: "p013_other"}
)

// Engineer is the authorized human actor of the fixture.
var Engineer = workspace.Actor{Type: workspace.ActorUser, ID: "usr_engineer"}

const (
	CheckoutRef  = "secret://t013/checkout"
	PublisherRef = "secret://t013/publisher"
)

// Config tunes the fixture.
type Config struct {
	Service workspace.Options
	Git     func(*gitad.Options)
	// WrapStore and WrapPublisher insert fault injection around the real adapters.
	WrapStore     func(workspace.Store) workspace.Store
	WrapPublisher func(workspace.RefPublisher) workspace.RefPublisher
}

// Env is one fully wired workspace service over real PostgreSQL and Git.
type Env struct {
	T testing.TB

	DB                 *pgtest.Database
	Migration, Runtime *pgxpool.Pool
	Events             *eventspg.Store
	PG                 *workspacepg.Store
	Store              workspace.Store

	Git  *gitfx.Server
	Repo *gitfx.Repo
	// ReadToken and WriteToken are the fixture's two credentials.
	Reader    *gitad.Reader
	Publisher *gitad.Publisher
	Svc       *workspace.Service

	Authz    *Authz
	Items    *Items
	Repos    *Repos
	Attempts *Attempts
	Bundles  *Bundles
	Secrets  *Secrets
	Policy   *Policy
	Rotation *Rotation
	Clock    *Clock
	Tenants  *Tenants

	deps workspace.Dependencies
	opts workspace.Options
	mu   sync.Mutex
	seq  int
}

// Tenants lists the seeded tenants.
type Tenants struct{ IDs []domain.TenantID }

func (t *Tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t.IDs, nil }

// DefaultOptions are short explicit test policies, not production values.
func DefaultOptions() workspace.Options {
	return workspace.Options{
		GitDeadline: 30 * time.Second, ArchiveWait: 3 * time.Second, ArchivePoll: 20 * time.Millisecond,
		DriverStaleAfter: time.Minute, RedriveAfter: time.Hour, ReconcileBatch: 10,
	}
}

// credentials resolves checkout references to the read token and publisher
// references to the write token, inside the fixture scopes only.
type credentials struct {
	byRef map[string]string
	mu    sync.Mutex
	Log   []string
}

func (c *credentials) Resolve(_ context.Context, s domain.Scope, ref string) (gitad.Credential, error) {
	if s != ScopeA && s != ScopeA2 && s != ScopeB {
		return gitad.Credential{}, errors.New("foreign scope")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Log = append(c.Log, ref)
	tok, ok := c.byRef[ref]
	if !ok {
		return gitad.Credential{}, errors.New("unknown secret reference")
	}
	return gitad.Credential{Username: gitfx.Username, Password: tok}, nil
}

// New builds the environment. PostgreSQL follows test/support/postgres: the
// test skips when no administrator URL is configured and fails instead when
// KODAMA_REQUIRE_POSTGRES=1.
func New(t testing.TB, cfg Config) *Env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &Env{T: t, DB: db, Migration: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 30)}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.Migration, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The work-item, writer, publication and retained-ref links name rows other
	// modules own (work items, attempts, runs, artifacts). This fixture exercises
	// the workspace module alone, so it drops those links; the execution
	// composition tests cover them against real rows.
	pgtest.DropExecutionLinks(t, e.Migration, "workspaces",
		"workspaces_work_item_fk", "workspaces_writer_attempt_fk", "workspaces_publish_attempt_fk", "workspaces_publish_artifact_fk")
	pgtest.DropExecutionLinks(t, e.Migration, "workspace_publications",
		"workspace_publications_attempt_fk", "workspace_publications_artifact_fk")
	pgtest.DropExecutionLinks(t, e.Migration, "workspace_retained_refs",
		"workspace_retained_refs_run_fk", "workspace_retained_refs_attempt_fk")
	for _, s := range []domain.Scope{ScopeA, ScopeB} {
		if _, err := e.Migration.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, string(s.TenantID)); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		if _, err := e.Migration.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(s.TenantID), string(s.ProjectID)); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}
	if _, err := e.Migration.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(ScopeA2.TenantID), string(ScopeA2.ProjectID)); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	e.Tenants = &Tenants{IDs: []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID}}

	e.Events, err = eventspg.NewStore(e.Runtime, eventstest.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	e.PG, err = workspacepg.NewStore(e.Runtime, e.Events, workspacepg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e.Store = e.PG
	if cfg.WrapStore != nil {
		e.Store = cfg.WrapStore(e.PG)
	}

	e.Git = gitfx.NewServer(t)
	e.Repo = e.Git.NewRepo(t, "t013-repo")
	gopts := gitad.Options{
		CAFile: e.Git.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
		ScratchRoot: filepath.Join(t.TempDir(), "scratch"),
	}
	if cfg.Git != nil {
		cfg.Git(&gopts)
	}
	creds := &credentials{byRef: map[string]string{CheckoutRef: e.Git.ReadToken, PublisherRef: e.Git.WriteToken}}
	e.Reader, err = gitad.NewReader(gopts, creds)
	if err != nil {
		t.Fatal(err)
	}
	e.Publisher, err = gitad.NewPublisher(gopts, creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Reader.Close)
	t.Cleanup(e.Publisher.Close)

	e.Authz, e.Items, e.Repos, e.Attempts = NewAuthz(), NewItems(), NewRepos(), NewAttempts()
	e.Bundles, e.Secrets, e.Policy, e.Rotation, e.Clock = NewBundles(), NewSecrets(), &Policy{}, &Rotation{}, NewClock()
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		e.Authz.Grant(Engineer.ID, s)
		e.Repos.Set(s, e.RepositoryConfig())
	}
	// A principal with no grants at all, and one granted only in tenant B.
	e.Authz.Grant("usr_other_tenant", ScopeB)

	var pub workspace.RefPublisher = e.Publisher
	if cfg.WrapPublisher != nil {
		pub = cfg.WrapPublisher(pub)
	}
	e.opts = DefaultOptions()
	if cfg.Service != (workspace.Options{}) {
		e.opts = cfg.Service
	}
	e.deps = workspace.Dependencies{
		Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos,
		Attempts: e.Attempts, Bundles: e.Bundles, Secrets: e.Secrets, Refs: e.Reader, Source: e.Reader, Publisher: pub,
		Deletion: e.Policy, Rotation: e.Rotation, Clock: e.Clock.Now,
	}
	e.Svc, err = workspace.NewService(e.deps, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// RepositoryConfig is the fixture repository as the project configures it.
func (e *Env) RepositoryConfig() workspace.RepositoryConfig {
	return workspace.RepositoryConfig{
		ID: "repo_t013", URL: e.Repo.URL, CheckoutSecretRef: CheckoutRef, PublisherSecretRef: PublisherRef, BaseRef: e.Repo.BaseRef,
	}
}

// NewService builds another Service over the same ports, as a second control-
// plane process (or an old one after restore) would be.
func (e *Env) NewService(mutate func(*workspace.Dependencies, *workspace.Options)) *workspace.Service {
	e.T.Helper()
	d, o := e.deps, e.opts
	if mutate != nil {
		mutate(&d, &o)
	}
	s, err := workspace.NewService(d, o)
	if err != nil {
		e.T.Fatal(err)
	}
	return s
}

// ID returns a fresh, valid, readable identifier.
func (e *Env) ID(prefix string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	return prefix + "_t013_" + string(rune('a'+e.seq%26)) + itoa(e.seq)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// Ready provisions an available workspace for a fresh story in scope.
func (e *Env) Ready(scope domain.Scope, item domain.WorkItemID) *workspace.Workspace {
	e.T.Helper()
	e.Items.Add(scope, item, "story")
	w, _, err := e.Svc.Provision(context.Background(), Engineer, scope, item)
	if err != nil {
		e.T.Fatalf("provision: %v", err)
	}
	if w.State != workspace.StateAvailable {
		e.T.Fatalf("workspace state %s (%s)", w.State, w.LastError)
	}
	return w
}

// Writer registers a live write attempt and reserves the slot for it.
func (e *Env) Writer(w *workspace.Workspace, attempt domain.AttemptID) *workspace.Workspace {
	e.T.Helper()
	e.Attempts.Register(w.Scope, attempt, w.ID, workspace.AccessWrite)
	out, err := e.Svc.ReserveWriter(context.Background(), Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: attempt})
	if err != nil {
		e.T.Fatalf("reserve writer: %v", err)
	}
	return out
}

// Proposal builds what the entrypoint would upload for attempt on top of base
// and registers the bundle artifact. It returns the proposal.
func (e *Env) Proposal(w *workspace.Workspace, attempt domain.AttemptID, base string, msg string, files ...gitfx.File) workspace.ProposeInput {
	e.T.Helper()
	commit, bundle, _ := e.Repo.Bundle(e.T, base, msg, files...)
	art := domain.ArtifactID(e.ID("art"))
	e.Bundles.Add(w.Scope, art, attempt, bundle)
	return workspace.ProposeInput{
		Scope: w.Scope, AttemptID: attempt, Commit: workspace.Commit(commit), ExpectedTip: workspace.Commit(base), BundleArtifactID: art,
	}
}

// CountSQL runs a count query as the owner (bypassing nothing: owner policies
// are explicit), scoped by the caller's SQL.
func (e *Env) CountSQL(sql string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Migration.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// CountTenant runs a count query through the runtime role inside a tenant
// transaction, as the application sees the data (forced RLS applies).
func (e *Env) CountTenant(tenant domain.TenantID, sql string, args ...any) int {
	e.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		e.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}
