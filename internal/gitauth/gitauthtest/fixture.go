// Package gitauthtest is the shared fixture of the gitauth tests: controlled
// stand-ins for the modules gitauth consumes (repositories, push records,
// execution's attempts, gates and the identity roles), the memory and PostgreSQL
// environments, and the shared Contract suite both store adapters must pass
// (SD-13/SD-14). It is imported only by tests. The stand-ins prove gitauth's own
// behavior against the ports it owns, not the behavior of the modules that
// will implement them (T-127, T-130, T-148/T-150, the gates module).
package gitauthtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/identity"
)

// Identifiers of the fixture. Two tenants with identically shaped data prove
// that tenant ownership, not an ID's spelling, decides every answer.
const (
	TenantA  domain.TenantID  = "t136_acme"
	TenantB  domain.TenantID  = "t136_globex"
	ProjectA domain.ProjectID = "p136_inventory" // in TenantA
	ProjectX domain.ProjectID = "p136_ledger"    // another project of TenantA
	ProjectB domain.ProjectID = "p136_inventory_b"

	Engineer identity.UserID = "u136_engineer"
	Viewer   identity.UserID = "u136_viewer"
	Admin    identity.UserID = "u136_admin"
	Outsider identity.UserID = "u136_outsider"

	// Repositories of ProjectA.
	RepoReady    = "r136_backend"  // internal, ready, with refs
	RepoEmpty    = "r136_empty"    // internal, ready, no refs
	RepoExternal = "r136_external" // hosted elsewhere
	RepoArchived = "r136_archived"
	RepoBooting  = "r136_booting" // provisioning
	// RepoOther belongs to ProjectX, RepoB to TenantB's project.
	RepoOther = "r136_other"
	RepoB     = "r136_backend_b"
)

// Scope helpers.
func ScopeA() domain.Scope { return domain.Scope{TenantID: TenantA, ProjectID: ProjectA} }
func ScopeX() domain.Scope { return domain.Scope{TenantID: TenantA, ProjectID: ProjectX} }
func ScopeB() domain.Scope { return domain.Scope{TenantID: TenantB, ProjectID: ProjectB} }

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)} }

// Now returns the current fixture time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ---- repositories, pushes, attempts, gates -----------------------------------------

// Repositories is the repository module stand-in.
type Repositories struct {
	mu    sync.Mutex
	repos map[string]gitv1.Repository
	Fail  error
}

var _ gitauth.Repositories = (*Repositories)(nil)

func repoKey(scope domain.Scope, id string) string {
	return string(scope.TenantID) + "|" + string(scope.ProjectID) + "|" + id
}

// Put stores a repository of the scope.
func (r *Repositories) Put(scope domain.Scope, repo gitv1.Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repos == nil {
		r.repos = map[string]gitv1.Repository{}
	}
	r.repos[repoKey(scope, repo.ID)] = repo
}

// Repository implements gitauth.Repositories: a repository of another scope is
// unknown.
func (r *Repositories) Repository(_ context.Context, scope domain.Scope, id string) (gitv1.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return gitv1.Repository{}, r.Fail
	}
	repo, ok := r.repos[repoKey(scope, id)]
	if !ok {
		return gitv1.Repository{}, gitauth.ErrNotFound
	}
	return repo, nil
}

// NewRepository builds a valid repository record.
func NewRepository(id string, mutate func(*gitv1.Repository)) gitv1.Repository {
	repo := gitv1.Repository{
		ID: id, Name: "backend", DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady,
		CloneURL: "https://git.example.invalid/git/acme/inventory/backend.git", Version: 1, CreatedAt: "2026-10-30T09:00:00Z",
	}
	if mutate != nil {
		mutate(&repo)
	}
	return repo
}

// Pushes is the push-record stand-in: which repositories have an unsettled push.
type Pushes struct {
	mu        sync.Mutex
	unsettled map[string]bool
	Fail      error
	// OnCheck, when set, runs inside UnsettledPush, that is inside the seed
	// issuance transaction after it took the seed lock; tests use it to hold the
	// issuance open while a receive tries to consume.
	OnCheck func()
}

var _ gitauth.Pushes = (*Pushes)(nil)

// SetUnsettled marks a repository as having (or not) an unsettled push record.
func (p *Pushes) SetUnsettled(scope domain.Scope, repositoryID string, v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unsettled == nil {
		p.unsettled = map[string]bool{}
	}
	p.unsettled[repoKey(scope, repositoryID)] = v
}

// UnsettledPush implements gitauth.Pushes.
func (p *Pushes) UnsettledPush(_ context.Context, scope domain.Scope, repositoryID string) (bool, error) {
	p.mu.Lock()
	hook := p.OnCheck
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Fail != nil {
		return false, p.Fail
	}
	return p.unsettled[repoKey(scope, repositoryID)], nil
}

// Attempts is the execution stand-in.
type Attempts struct {
	mu     sync.Mutex
	leases map[string]gitauth.AttemptLease
	Fail   error
}

var _ gitauth.Attempts = (*Attempts)(nil)

func attemptKey(scope domain.Scope, a domain.AttemptID) string {
	return string(scope.TenantID) + "|" + string(scope.ProjectID) + "|" + string(a)
}

// Set stores the attempt's lease.
func (a *Attempts) Set(scope domain.Scope, attempt domain.AttemptID, lease gitauth.AttemptLease) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leases == nil {
		a.leases = map[string]gitauth.AttemptLease{}
	}
	a.leases[attemptKey(scope, attempt)] = lease
}

// Lease implements gitauth.Attempts.
func (a *Attempts) Lease(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (gitauth.AttemptLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Fail != nil {
		return gitauth.AttemptLease{}, a.Fail
	}
	l, ok := a.leases[attemptKey(scope, attempt)]
	if !ok {
		return gitauth.AttemptLease{}, gitauth.ErrNotFound
	}
	return l, nil
}

// Gates is the gates-module stand-in.
type Gates struct {
	mu        sync.Mutex
	decisions map[string]gitauth.GateDecision
	Fail      error
}

var _ gitauth.Gates = (*Gates)(nil)

// Set stores the gate's decision.
func (g *Gates) Set(scope domain.Scope, gateID string, d gitauth.GateDecision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.decisions == nil {
		g.decisions = map[string]gitauth.GateDecision{}
	}
	g.decisions[repoKey(scope, gateID)] = d
}

// Decision implements gitauth.Gates.
func (g *Gates) Decision(_ context.Context, scope domain.Scope, gateID string) (gitauth.GateDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Fail != nil {
		return gitauth.GateDecision{}, g.Fail
	}
	d, ok := g.decisions[repoKey(scope, gateID)]
	if !ok {
		return gitauth.GateDecision{}, gitauth.ErrNotFound
	}
	return d, nil
}

// Roles is the identity stand-in: current role bindings read by the real
// standalone authority (the real role matrix decides what each role may do).
type Roles struct {
	mu       sync.Mutex
	bindings map[string][]identity.Binding
	n        int
	Fail     error
}

var _ identity.AccessReader = (*Roles)(nil)

// Set binds the user to the role at the project (replacing any earlier binding
// of that project), the way a role change does.
func (r *Roles) Set(tenant domain.TenantID, project domain.ProjectID, user identity.UserID, role identity.Role) {
	r.Remove(tenant, project, user)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bindings == nil {
		r.bindings = map[string][]identity.Binding{}
	}
	r.n++
	b, err := identity.Grant(identity.BindingID(fmt.Sprintf("b136_%d", r.n)), tenant, user, project, role, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		panic(err)
	}
	k := string(tenant) + "|" + string(user)
	r.bindings[k] = append(r.bindings[k], b)
}

// Remove deletes the user's binding at the project.
func (r *Roles) Remove(tenant domain.TenantID, project domain.ProjectID, user identity.UserID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := string(tenant) + "|" + string(user)
	var keep []identity.Binding
	for _, b := range r.bindings[k] {
		if b.Project != project {
			keep = append(keep, b)
		}
	}
	if r.bindings == nil {
		r.bindings = map[string][]identity.Binding{}
	}
	r.bindings[k] = keep
}

// CurrentBindings implements identity.AccessReader.
func (r *Roles) CurrentBindings(_ context.Context, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return nil, r.Fail
	}
	return append([]identity.Binding(nil), r.bindings[string(tenant)+"|"+string(user)]...), nil
}

// ---- environment and fixture ------------------------------------------------------------

// Env is one store adapter's environment.
type Env interface {
	// Store is the adapter under test.
	Store() gitauth.Store
	// SeedProject makes the tenant and project known to the store (the
	// composite project reference of the SQL schema).
	SeedProject(t testing.TB, scope domain.Scope)
	// GrantCount reports how many grants the tenant holds, read outside the
	// grant API.
	GrantCount(t testing.TB, tenant domain.TenantID) int
	// RevocationAudits lists the tenant's tenant-wide revocation audit records,
	// read outside the grant API.
	RevocationAudits(t testing.TB, tenant domain.TenantID) []AuditRow
}

// AuditRow is what the fixture reads back of one revocation audit record.
type AuditRow struct {
	Action    string
	ActorType string
	ActorID   string
	Reason    string
	Revoked   int
}

// Open creates a fresh environment; it skips the test when the adapter's
// service is not available.
type Open func(testing.TB) Env

// Fixture is one environment with the stand-ins and a service wired over them.
type Fixture struct {
	T        testing.TB
	Env      Env
	Clock    *Clock
	Repos    *Repositories
	Pushes   *Pushes
	Attempts *Attempts
	Gates    *Gates
	Roles    *Roles
	Svc      *gitauth.Service
}

// New builds the fixture with the standard data: both tenants' projects and
// repositories and the users' roles in ProjectA (engineer, viewer and admin; the
// outsider has none).
func New(t testing.TB, open Open) *Fixture {
	t.Helper()
	env := open(t)
	f := &Fixture{T: t, Env: env, Clock: NewClock(), Repos: &Repositories{}, Pushes: &Pushes{}, Attempts: &Attempts{}, Gates: &Gates{}, Roles: &Roles{}}
	for _, scope := range []domain.Scope{ScopeA(), ScopeX(), ScopeB()} {
		env.SeedProject(t, scope)
	}
	f.Repos.Put(ScopeA(), NewRepository(RepoReady, nil))
	f.Repos.Put(ScopeA(), NewRepository(RepoEmpty, func(r *gitv1.Repository) { r.Empty = true }))
	f.Repos.Put(ScopeA(), NewRepository(RepoExternal, func(r *gitv1.Repository) { r.Hosting = gitv1.HostingExternal }))
	f.Repos.Put(ScopeA(), NewRepository(RepoArchived, func(r *gitv1.Repository) { r.Status = gitv1.StatusArchived }))
	f.Repos.Put(ScopeA(), NewRepository(RepoBooting, func(r *gitv1.Repository) { r.Status = gitv1.StatusProvisioning }))
	f.Repos.Put(ScopeX(), NewRepository(RepoOther, nil))
	f.Repos.Put(ScopeB(), NewRepository(RepoB, nil))
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleEngineer)
	f.Roles.Set(TenantA, ProjectA, Viewer, identity.RoleViewer)
	f.Roles.Set(TenantA, ProjectA, Admin, identity.RoleAdmin)
	f.Roles.Set(TenantB, ProjectB, Engineer, identity.RoleEngineer) // the same user ID in another tenant
	authority, err := identity.NewStandaloneAuthority(f.Roles)
	if err != nil {
		t.Fatal(err)
	}
	f.Svc, err = gitauth.NewService(gitauth.Config{
		Store: env.Store(), Repositories: f.Repos, Pushes: f.Pushes, Attempts: f.Attempts, Gates: f.Gates,
		Authority: authority, Clock: f.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Verify is the service's Verify, failing the test on an unexpected refusal.
func (f *Fixture) MustVerify(tenant domain.TenantID, credential string) gitv1.GrantClaims {
	f.T.Helper()
	claims, err := f.Svc.Verify(context.Background(), tenant, credential)
	if err != nil {
		f.T.Fatalf("verify: %v", err)
	}
	return claims
}

// WantCode fails the test unless err is a contract error with the code.
func WantCode(t testing.TB, err error, code domain.ErrorCode) {
	t.Helper()
	if !gitauth.IsCode(err, code) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

// WantInvalid fails the test unless err is the one invalid_credential answer.
func WantInvalid(t testing.TB, err error) {
	t.Helper()
	var ce *gitv1.ContractError
	if !errors.As(err, &ce) || ce.Code != domain.CodeInvalidCredential || ce.Message != "the credential is not valid" {
		t.Fatalf("error = %v, want the invalid_credential answer", err)
	}
}

// Workload builds the actor of an attempt of a run.
func Workload(attempt, run string) gitv1.Actor {
	return gitv1.Actor{Type: gitv1.ActorWorkload, ID: attempt, RunID: &run, AttemptID: &attempt}
}
