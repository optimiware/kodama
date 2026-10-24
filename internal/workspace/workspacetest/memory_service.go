package workspacetest

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
)

// MemServiceEnv is the whole workspace module in `development`: the real
// workspace.Service over the memory store, the local bare Git (reader, bundle
// preparer and publisher) and the explicit peer fakes. It needs no database, no
// Git server and no container.
type MemServiceEnv struct {
	T      testing.TB
	DB     *memdb.Store
	Events *eventstest.Mem
	Store  *MemStore
	Git    *LocalGit
	Repo   *LocalRepo
	Svc    *workspace.Service

	Authz    *Authz
	Items    *Items
	Repos    *Repos
	Attempts *Attempts
	Bundles  *Bundles
	Secrets  *Secrets
	Clock    *Clock
	Tenants  *Tenants
	seq      int
}

// NewMemServiceEnv wires the service. The Git root is removed when the test ends.
func NewMemServiceEnv(t testing.TB, opts ...memdb.Option) *MemServiceEnv {
	t.Helper()
	e := &MemServiceEnv{T: t, DB: memdb.New(opts...)}
	var err error
	if e.Events, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	e.Clock = NewClock()
	if e.Store, err = NewMemStore(e.DB, e.Events, MemOptions{Clock: e.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	e.Git = LocalGitFor(t)
	e.Repo = e.Git.NewRepo(t, "t088-repo")
	e.Authz, e.Items, e.Repos, e.Attempts = NewAuthz(), NewItems(), NewRepos(), NewAttempts()
	e.Bundles, e.Secrets = NewBundles(), NewSecrets()
	e.Tenants = &Tenants{IDs: []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID}}
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		e.Authz.Grant(Engineer.ID, s)
		e.Repos.Set(s, workspace.RepositoryConfig{ID: "repo_t088", URL: e.Repo.Path, CheckoutSecretRef: CheckoutRef, PublisherSecretRef: PublisherRef, BaseRef: e.Repo.BaseRef})
		e.Events.AddTenant(s.TenantID)
	}
	e.Svc, err = workspace.NewService(workspace.Dependencies{
		Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos, Attempts: e.Attempts,
		Bundles: e.Bundles, Secrets: e.Secrets, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(), Clock: e.Clock.Now,
	}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ID returns a fresh, valid identifier.
func (e *MemServiceEnv) ID(prefix string) string {
	e.seq++
	return prefix + "_t088_" + itoa(e.seq)
}

// Ready provisions an available workspace for a fresh story in scope.
func (e *MemServiceEnv) Ready(scope domain.Scope, item domain.WorkItemID) *workspace.Workspace {
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
func (e *MemServiceEnv) Writer(w *workspace.Workspace, attempt domain.AttemptID) *workspace.Workspace {
	e.T.Helper()
	e.Attempts.Register(w.Scope, attempt, w.ID, workspace.AccessWrite)
	out, err := e.Svc.ReserveWriter(context.Background(), Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: attempt})
	if err != nil {
		e.T.Fatalf("reserve writer: %v", err)
	}
	return out
}

// Proposal builds the bundle the entrypoint would upload for attempt on top of
// base, registers it as the attempt's bundle artifact and returns the proposal.
func (e *MemServiceEnv) Proposal(w *workspace.Workspace, attempt domain.AttemptID, base workspace.Commit, msg string, files ...File) workspace.ProposeInput {
	e.T.Helper()
	commit, bundle := e.Repo.BundleT(e.T, base, msg, files...)
	art := domain.ArtifactID(e.ID("art"))
	e.Bundles.Add(w.Scope, art, attempt, bundle)
	return workspace.ProposeInput{Scope: w.Scope, AttemptID: attempt, Commit: commit, ExpectedTip: base, BundleArtifactID: art}
}
