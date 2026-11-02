package gitpushtest

import (
	"context"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/identity"
)

// Identifiers of the fixture. Two tenants with identically named repositories
// prove that tenant ownership, not an ID's spelling, decides every answer.
const (
	TenantA  domain.TenantID  = "t130_acme"
	TenantB  domain.TenantID  = "t130_globex"
	ProjectA domain.ProjectID = "p130_inventory"
	ProjectX domain.ProjectID = "p130_ledger" // another project of TenantA
	ProjectB domain.ProjectID = "p130_inventory_b"

	Engineer identity.UserID = "u130_engineer"
	Viewer   identity.UserID = "u130_viewer"

	// Repositories of ProjectA (RepoMain also exists, identically named, in TenantB).
	RepoMain     = "r130_backend"  // internal, ready, with refs
	RepoEmpty    = "r130_empty"    // internal, ready, no refs
	RepoDirect   = "r130_direct"   // policy.directPushToDefault: true
	RepoArchived = "r130_archived" // archived
	RepoBooting  = "r130_booting"  // provisioning
	RepoExternal = "r130_external" // hosted elsewhere
	// RepoOther belongs to ProjectX.
	RepoOther = "r130_other"
)

// Scopes of the fixture.
func ScopeA() domain.Scope { return domain.Scope{TenantID: TenantA, ProjectID: ProjectA} }
func ScopeX() domain.Scope { return domain.Scope{TenantID: TenantA, ProjectID: ProjectX} }
func ScopeB() domain.Scope { return domain.Scope{TenantID: TenantB, ProjectID: ProjectB} }

// OID is a valid object ID made of one repeated hex digit (the zero ID for "0").
func OID(c string) string { return strings.Repeat(c, 40) }

// Zero is the all-zero object ID.
var Zero = OID("0")

// Fixture is one environment with the stand-ins and the services wired over
// them: the real gitauth service (so claims, current authority and the seed
// consumption are the real ones) and the push service under test.
type Fixture struct {
	T            testing.TB
	Env          Env
	Clock        *Clock
	Repos        *Repositories
	Worker       *Worker
	Reservations *Reservations
	Reads        *ReadAuth
	Admins       *AdminAuth
	Roles        *gitauthtest.Roles
	Attempts     *gitauthtest.Attempts
	Gates        *gitauthtest.Gates
	Auth         *gitauth.Service
	Svc          *gitpush.Service

	// AfterLiveness runs after the push service read an attempt's liveness and
	// before its transaction: tests cancel the attempt in that window.
	AfterLiveness func()

	deps gitpush.Dependencies
	ptr  **gitpush.Service
}

// pushesFwd lets gitauth ask the push service, which is constructed after it.
type pushesFwd struct{ svc **gitpush.Service }

func (p pushesFwd) UnsettledPush(ctx context.Context, scope domain.Scope, repositoryID string) (bool, error) {
	return (*p.svc).UnsettledPush(ctx, scope, repositoryID)
}

// New builds the fixture with the standard data.
func New(t testing.TB, open Open, opts ...func(*gitpush.Options)) *Fixture {
	t.Helper()
	env := open(t)
	f := &Fixture{T: t, Env: env, Clock: NewClock(), Repos: &Repositories{}, Worker: &Worker{}, Reservations: &Reservations{}, Reads: &ReadAuth{}, Admins: &AdminAuth{},
		Roles: &gitauthtest.Roles{}, Attempts: &gitauthtest.Attempts{}, Gates: &gitauthtest.Gates{}}
	put := func(scope domain.Scope, id string, mutate func(*Repository)) {
		r := Repository{Facts: gitpush.RepositoryFacts{ID: id, Scope: scope, DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady}}
		if mutate != nil {
			mutate(&r)
		}
		env.SeedRepository(t, scope, id)
		env.SyncRepository(t, r.Facts)
		f.Repos.Put(r)
	}
	put(ScopeA(), RepoMain, nil)
	put(ScopeA(), RepoEmpty, func(r *Repository) { r.Empty = true })
	put(ScopeA(), RepoDirect, func(r *Repository) { r.Facts.DirectPushToDefault = true })
	put(ScopeA(), RepoArchived, nil)
	put(ScopeA(), RepoBooting, nil)
	put(ScopeA(), RepoExternal, nil)
	put(ScopeX(), RepoOther, nil)
	put(ScopeB(), RepoMain, nil)
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleEngineer)
	f.Roles.Set(TenantA, ProjectA, Viewer, identity.RoleViewer)
	f.Roles.Set(TenantB, ProjectB, Engineer, identity.RoleEngineer) // the same user ID in another tenant
	f.Reads.Allow("usr_reader", ScopeA())
	f.Admins.Allow("usr_admin", ScopeA())
	f.Reads.Allow("usr_admin", ScopeA())
	authority, err := identity.NewStandaloneAuthority(f.Roles)
	if err != nil {
		t.Fatal(err)
	}
	env.UseRepositories(f.Repos.GuardView())
	var svc *gitpush.Service
	f.Auth, err = gitauth.NewService(gitauth.Config{
		Store: env.GrantStore(), Repositories: f.Repos.GitauthView(), Pushes: pushesFwd{svc: &svc}, Attempts: f.Attempts, Gates: f.Gates,
		Authority: authority, Clock: f.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.deps = gitpush.Dependencies{
		Store: env.Store(), Repositories: f.Repos, Grants: f.Auth, Attempts: AttemptsView{Leases: f.Attempts, After: func() {
			if hook := f.AfterLiveness; hook != nil {
				hook()
			}
		}}, Reservations: f.Reservations,
		Worker: f.Worker, Reads: f.Reads, Admin: f.Admins,
	}
	f.ptr = &svc
	f.Reconfigure(opts...)
	return f
}

// UseWorker replaces the scripted worker the reconciler reads evidence from
// (a real worker's RPC, for the integration tests) and rebuilds the service.
func (f *Fixture) UseWorker(w gitpush.Worker, opts ...func(*gitpush.Options)) {
	f.deps.Worker = w
	f.Reconfigure(opts...)
}

// Reconfigure replaces the push service with one built with the options (the
// stores and stand-ins stay): the bound of updates, the deadlines.
func (f *Fixture) Reconfigure(opts ...func(*gitpush.Options)) {
	f.T.Helper()
	o := gitpush.Options{Clock: f.Clock.Now}
	for _, fn := range opts {
		fn(&o)
	}
	svc, err := gitpush.NewService(f.deps, o)
	if err != nil {
		f.T.Fatal(err)
	}
	*f.ptr, f.Svc = svc, svc
}

// ChangeRepository changes a repository in the repository stand-in and in the
// backing store, the way the repository module's transitions would.
func (f *Fixture) ChangeRepository(tenant domain.TenantID, id string, mutate func(*gitpush.RepositoryFacts)) {
	f.Repos.Update(tenant, id, func(r *Repository) { mutate(&r.Facts) })
	facts, err := f.Repos.GuardView().Repository(context.Background(), tenant, id)
	if err != nil {
		f.T.Fatal(err)
	}
	f.Env.SyncRepository(f.T, facts)
}

// SetStatus changes a repository's status.
func (f *Fixture) SetStatus(tenant domain.TenantID, id string, status gitv1.RepositoryStatus) {
	f.ChangeRepository(tenant, id, func(r *gitpush.RepositoryFacts) { r.Status = status })
}

// Human issues an engineer's or viewer's human credential for the repository
// (scope ScopeA) and returns the grant ID.
func (f *Fixture) Human(user identity.UserID, repo string) string {
	f.T.Helper()
	resp, err := f.Auth.Exchange(context.Background(), TenantA, user, gitv1.CredentialRequest{ProjectID: string(ProjectA), RepositoryID: repo})
	if err != nil {
		f.T.Fatalf("human credential for %s: %v", repo, err)
	}
	return resp.GrantID
}

// Seed issues the single-use seed grant of an empty repository and returns its
// grant ID.
func (f *Fixture) Seed(user identity.UserID, repo string) (string, error) {
	resp, err := f.Auth.Exchange(context.Background(), TenantA, user, gitv1.CredentialRequest{ProjectID: string(ProjectA), RepositoryID: repo, Seed: true})
	return resp.GrantID, err
}

// Live sets an attempt's lease.
func (f *Fixture) Live(scope domain.Scope, attempt string, live bool) {
	f.Attempts.Set(scope, domain.AttemptID(attempt), gitauth.AttemptLease{Live: live})
}

// Attempt issues an attempt grant of a run with the given update patterns on the
// repository (scope ScopeA) and returns the grant ID. The attempt is live.
func (f *Fixture) Attempt(attempt, run, repo string, patterns ...string) string {
	f.T.Helper()
	f.Live(ScopeA(), attempt, true)
	if patterns == nil {
		patterns = []string{}
	}
	h, err := f.Auth.IssueAttempt(context.Background(), ScopeA(), gitauth.AttemptGrantRequest{
		Kind:         gitv1.GrantAttempt,
		Actor:        gitauthtest.Workload(attempt, run),
		Repositories: []gitv1.GrantRepository{{ID: repo, Read: true, Update: patterns}},
		ExpiresAt:    f.Clock.Now().Add(time.Hour),
	})
	if err != nil {
		f.T.Fatalf("attempt grant: %v", err)
	}
	return h.GrantID
}

// Integration issues the integration grant of an approved candidate for the
// default branch of repo and returns the grant ID.
func (f *Fixture) Integration(attempt, repo string, c gitv1.IntegrationConstraint) string {
	f.T.Helper()
	f.Live(ScopeA(), attempt, true)
	f.Gates.Set(ScopeA(), c.GateID, gitauth.GateDecision{Approved: true, Current: true, RunID: "run_i", AttemptID: domain.AttemptID(attempt),
		RepositoryID: repo, Base: c.From, Candidate: c.To})
	h, err := f.Auth.IssueIntegration(context.Background(), ScopeA(), domain.AttemptID(attempt), repo, c)
	if err != nil {
		f.T.Fatalf("integration grant: %v", err)
	}
	return h.GrantID
}

// U is one update of a request: FastForward is the worker's fact for an update
// of an existing ref (nil for a create or delete).
type U struct {
	Ref, Old, New string
	FastForward   *bool
}

// FF and NFF are the worker's fast-forward facts.
func FF() *bool  { v := true; return &v }
func NFF() *bool { v := false; return &v }

// Create, Advance, Rewrite and Delete build updates of the four kinds.
func Create(ref, new string) U       { return U{Ref: ref, Old: Zero, New: new} }
func Advance(ref, old, new string) U { return U{Ref: ref, Old: old, New: new, FastForward: FF()} }
func Rewrite(ref, old, new string) U { return U{Ref: ref, Old: old, New: new, FastForward: NFF()} }
func Delete(ref, old string) U       { return U{Ref: ref, Old: old, New: Zero} }
func Branch(name string) string      { return gitv1.HeadsPrefix + name }
func Task(workItem string) string    { return gitv1.TaskBranchPrefix + workItem }
func Agent(wi, run, label string) string {
	return gitv1.AgentBranchPrefix + wi + "/" + run + "/" + label
}

// Request builds the authorization request of a receive.
func Request(repo, grant, receive string, empty bool, updates ...U) gitv1.PushAuthorizeRequest {
	req := gitv1.PushAuthorizeRequest{RepositoryID: repo, GrantID: grant, ReceiveID: receive, PushOptions: []string{}, Updates: []gitv1.RefUpdate{}, GitFacts: []gitv1.GitFact{}}
	for _, u := range updates {
		req.Updates = append(req.Updates, gitv1.RefUpdate{Ref: u.Ref, Old: u.Old, New: u.New})
		req.GitFacts = append(req.GitFacts, gitv1.GitFact{Ref: u.Ref, FastForward: u.FastForward, RepositoryEmpty: empty})
	}
	return req
}

// Authorize calls the service in TenantA.
func (f *Fixture) Authorize(req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	return f.Svc.Authorize(context.Background(), TenantA, req)
}

// MustAuthorize fails the test unless the call succeeds and returns the response.
func (f *Fixture) MustAuthorize(req gitv1.PushAuthorizeRequest) gitv1.PushAuthorizeResponse {
	f.T.Helper()
	resp, err := f.Authorize(req)
	if err != nil {
		f.T.Fatalf("authorize: %v", err)
	}
	if err := gitv1.CheckAuthorizeResponse(req, resp); err != nil {
		f.T.Fatalf("response: %v", err)
	}
	return resp
}

// Updates are the request's updates (the commit-intent and outcome bodies).
func Updates(req gitv1.PushAuthorizeRequest, refs ...string) []gitv1.RefUpdate {
	var out []gitv1.RefUpdate
	for _, u := range req.Updates {
		if len(refs) == 0 || contains(refs, u.Ref) {
			out = append(out, u)
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Intent runs commit intent for the listed refs (all when none).
func (f *Fixture) Intent(pushID string, req gitv1.PushAuthorizeRequest, refs ...string) error {
	_, err := f.Svc.CommitIntent(context.Background(), TenantA, pushID, gitv1.CommitIntentRequest{ReceiveID: req.ReceiveID, Updates: Updates(req, refs...)})
	return err
}

// Outcome reports the listed refs applied (none when none).
func (f *Fixture) Outcome(pushID string, req gitv1.PushAuthorizeRequest, refs ...string) (gitv1.OutcomeResponse, error) {
	applied := []gitv1.RefUpdate{}
	if len(refs) > 0 {
		applied = Updates(req, refs...)
	}
	return f.Svc.Outcome(context.Background(), TenantA, pushID, gitv1.OutcomeRequest{ReceiveID: req.ReceiveID, Applied: applied})
}

// Record reloads the push record through the public read.
func (f *Fixture) Record(repo, pushID string) gitv1.PushRecord {
	f.T.Helper()
	rec, err := f.Svc.Get(context.Background(), "usr_reader", ScopeA(), repo, pushID)
	if err != nil {
		f.T.Fatalf("get %s: %v", pushID, err)
	}
	return rec
}

// Reconcile runs one reconciliation pass of the repository in TenantA.
func (f *Fixture) Reconcile(repo string) error {
	return f.Svc.Reconcile(context.Background(), TenantA, repo)
}

// States are the entry states of a record keyed by ref.
func States(rec gitv1.PushRecord) map[string]gitv1.EntryState {
	out := map[string]gitv1.EntryState{}
	for _, e := range rec.Entries {
		out[e.Ref] = e.State
	}
	return out
}

// Reasons are the entry reasons of a record keyed by ref ("" when none).
func Reasons(rec gitv1.PushRecord) map[string]string {
	out := map[string]string{}
	for _, e := range rec.Entries {
		if e.Reason != nil {
			out[e.Ref] = *e.Reason
		} else {
			out[e.Ref] = ""
		}
	}
	return out
}
