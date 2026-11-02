package repositorytest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/repository"
)

// Env is the persistence under contract. The fixture scopes ScopeA, ScopeA2
// (another project of tenant A) and ScopeB exist in every implementation.
type Env interface {
	Store() repository.Store
	// EventTypes lists the types of the repository's outbox events, sorted.
	EventTypes(t testing.TB, scope domain.Scope, id repository.ID) []string
	// AuditActions lists the repository's audit actions, sorted.
	AuditActions(t testing.TB, scope domain.Scope, id repository.ID) []string
	// FailEvents makes every event append fail with err after the event was
	// written in the mutation's transaction; nil restores it.
	FailEvents(err error)
}

// OpenEnv builds an empty Env.
type OpenEnv func(testing.TB) Env

// Kit is one wired Service over an Env's Store and the fakes: the worker, the
// authority, the configured-repository source and the clock.
type Kit struct {
	Svc   *repository.Service
	Hosts *Hosts
	Authz *Authz
	Ext   *External
	Clock *Clock
	// Ctx carries a request ID, as the HTTP adapter does.
	Ctx context.Context

	mu  sync.Mutex
	seq int
}

// NewKit wires a Service over the store of env with sequential identifiers
// (sweeps of two repositories per page, so paging is exercised).
func NewKit(t testing.TB, env Env) *Kit {
	t.Helper()
	k := &Kit{Hosts: NewHosts(), Authz: NewAuthz(), Ext: NewExternal(), Clock: NewClock(),
		Ctx: repository.WithRequestID(context.Background(), "req_t127")}
	var err error
	k.Svc, err = repository.NewService(repository.Dependencies{
		Store: env.Store(), Authorizer: k.Authz, Hosts: k.Hosts, CloneURLs: URLs{}, External: k.Ext,
		Tenants: &Tenants{IDs: []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID}}, Clock: k.Clock.Now, NewID: k.newID,
	}, repository.Options{HostsDeadline: 5 * time.Second, ReconcileBatch: 2})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *Kit) newID(prefix string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seq++
	return fmt.Sprintf("%s_t127_%04d", prefix, k.seq)
}

// fixture is a Kit with an Env, for the contract's scenarios.
type fixture struct {
	t     testing.TB
	env   Env
	svc   *repository.Service
	hosts *Hosts
	authz *Authz
	ext   *External
	clock *Clock
	ctx   context.Context
}

func newFixture(t *testing.T, open OpenEnv) *fixture {
	t.Helper()
	env := open(t)
	k := NewKit(t, env)
	return &fixture{t: t, env: env, svc: k.Svc, hosts: k.Hosts, authz: k.Authz, ext: k.Ext, clock: k.Clock, ctx: k.Ctx}
}

func (f *fixture) create(scope domain.Scope, name string, opts ...func(*gitv1.CreateRepositoryRequest)) repository.Created {
	f.t.Helper()
	req := gitv1.CreateRepositoryRequest{Name: name}
	for _, o := range opts {
		o(&req)
	}
	got, err := f.svc.Create(f.ctx, Engineer, scope, req)
	if err != nil {
		f.t.Fatalf("create %s in %s: %v", name, scope, err)
	}
	return got
}

func branch(b string) func(*gitv1.CreateRepositoryRequest) {
	return func(r *gitv1.CreateRepositoryRequest) { r.DefaultBranch = &b }
}

func direct(v bool) func(*gitv1.CreateRepositoryRequest) {
	return func(r *gitv1.CreateRepositoryRequest) {
		r.Policy = &gitv1.RepositoryPolicyInput{DirectPushToDefault: &v}
	}
}

func (f *fixture) events(r *repository.Repository) []string {
	f.t.Helper()
	return f.env.EventTypes(f.t, r.Scope, r.ID)
}

func (f *fixture) audit(r *repository.Repository) []string {
	f.t.Helper()
	return f.env.AuditActions(f.t, r.Scope, r.ID)
}

func (f *fixture) get(scope domain.Scope, id repository.ID) *repository.Repository {
	f.t.Helper()
	r, err := f.svc.Get(f.ctx, Engineer, scope, id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return r
}

var (
	readyEvents = []string{"repository.created", "repository.state_changed"}
	errFault    = errors.New("injected event failure")
)

func wantEq[T comparable](t testing.TB, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func wantSlice(t testing.TB, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func wantErr(t testing.TB, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s: error = %v, want %v", what, got, want)
	}
}

// Contract runs every shared behavior on one adapter: the store's persistence
// rules and the use cases over it. It asserts outcomes, not lock order or
// isolation levels.
func Contract(t *testing.T, open OpenEnv) {
	t.Helper()
	for _, c := range []struct {
		name string
		run  func(*testing.T, OpenEnv)
	}{
		{"create is idempotent", CreateIdempotent},
		{"names are unique per project", NameUniquePerProject},
		{"policy defaults deny", PolicyDefaultsDeny},
		{"a patch of both settings is one step", PatchBothSettings},
		{"provisioning reconciles", ProvisioningReconciles},
		{"archive and restore", ArchiveRestore},
		{"other tenants and projects cannot see a repository", CrossTenantHidden},
		{"external mapping is read-only", ExternalMapping},
		{"emptiness reports", EmptyReports},
		{"listing pages", ListPaging},
		{"a failed effect rolls the change back", AtomicEffects},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, open) })
	}
}

// CreateIdempotent: retried and concurrent creation of one name converges on
// one repository, one provisioning and one repository.created event (GR-02,
// GR-A02).
func CreateIdempotent(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	first := f.create(ScopeA, "backend")
	r := first.Repository
	if !first.Created || r.Status != gitv1.StatusReady || r.Reason != "" || !r.Empty || r.Version != 2 {
		t.Fatalf("created = %+v", first)
	}
	wantEq(t, "hosting", r.Hosting, gitv1.HostingInternal)
	wantEq(t, "default branch", r.DefaultBranch, "main")
	wantEq(t, "directPushToDefault", r.Policy.DirectPushToDefault, false)
	wantEq(t, "worker initializations", f.hosts.Initialized(ScopeA.TenantID, string(r.ID)), 1)
	wantSlice(t, "events", f.events(r), readyEvents)
	wantSlice(t, "audit", f.audit(r), []string{"repository.provisioning_requested", "repository.ready"})

	t.Run("a retry returns the same repository and writes nothing", func(t *testing.T) {
		again := f.create(ScopeA, "backend")
		if again.Created || again.Repository.ID != r.ID || again.Repository.Version != r.Version {
			t.Fatalf("retry = %+v", again)
		}
		wantEq(t, "worker initializations", f.hosts.Initialized(ScopeA.TenantID, string(r.ID)), 1)
		wantSlice(t, "events", f.events(r), readyEvents)
		wantSlice(t, "audit", f.audit(r), []string{"repository.provisioning_requested", "repository.ready"})
	})

	t.Run("concurrent creators converge", func(t *testing.T) {
		const n = 8
		var wg sync.WaitGroup
		results := make([]repository.Created, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = f.svc.Create(f.ctx, Engineer, ScopeA2, gitv1.CreateRepositoryRequest{Name: "racing"})
			}()
		}
		wg.Wait()
		created := 0
		var id repository.ID
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("creator %d: %v", i, errs[i])
			}
			if results[i].Created {
				created++
			}
			if id == "" {
				id = results[i].Repository.ID
			}
			wantEq(t, "repository id", results[i].Repository.ID, id)
		}
		wantEq(t, "creators that created", created, 1)
		got := f.get(ScopeA2, id)
		wantEq(t, "status", got.Status, gitv1.StatusReady)
		wantSlice(t, "events", f.events(got), readyEvents)
		wantEq(t, "worker created it once", f.hosts.Created(ScopeA2.TenantID, string(id)), true)
		list, err := f.svc.List(f.ctx, Engineer, ScopeA2, repository.ListQuery{Limit: 10})
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %v %v", list, err)
		}
	})

	t.Run("another request for the name is a conflict, never a silent change", func(t *testing.T) {
		_, err := f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "backend", DefaultBranch: ptr("trunk")})
		wantErr(t, "another default branch", err, repository.ErrNameTaken)
		on := true
		_, err = f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "backend", Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}})
		wantErr(t, "another policy", err, repository.ErrNameTaken)
		got := f.get(ScopeA, r.ID)
		wantEq(t, "default branch", got.DefaultBranch, "main")
		wantEq(t, "policy", got.Policy.DirectPushToDefault, false)
	})

	t.Run("invalid, unauthorized and unavailable authority create nothing", func(t *testing.T) {
		for _, name := range []string{"", "Backend", "-x", "a_b", "has space", "x/y"} {
			_, err := f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: name})
			wantErr(t, "name "+name, err, repository.ErrInvalidValue)
		}
		for _, b := range []string{"refs/heads/main", "kodama", "kodama/x", "a..b", "x.lock", "HEAD"} {
			_, err := f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "valid", DefaultBranch: ptr(b)})
			wantErr(t, "branch "+b, err, repository.ErrInvalidValue)
		}
		_, err := f.svc.Create(f.ctx, Viewer, ScopeA, gitv1.CreateRepositoryRequest{Name: "valid"})
		wantErr(t, "viewer", err, repository.ErrDenied)
		_, err = f.svc.Create(f.ctx, Engineer, domain.Scope{TenantID: ScopeA.TenantID, ProjectID: "p127_unknown"}, gitv1.CreateRepositoryRequest{Name: "valid"})
		wantErr(t, "unknown project", err, repository.ErrDenied)
		f.authz.Fail(ErrOutage())
		_, err = f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "valid"})
		wantErr(t, "authority outage", err, repository.ErrUnavailable)
		f.authz.Fail(nil)
		if _, err := f.env.Store().GetByName(f.ctx, ScopeA, "valid"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a refused create left a repository: %v", err)
		}
	})
}

// NameUniquePerProject: names are unique within a project and free across
// projects and tenants (GR-01, GR-A01); an identifier is never reused.
func NameUniquePerProject(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	a := f.create(ScopeA, "backend").Repository
	a2 := f.create(ScopeA2, "backend").Repository
	b := f.create(ScopeB, "backend").Repository
	if a.ID == a2.ID || a.ID == b.ID || a2.ID == b.ID {
		t.Fatalf("identifiers collide: %s %s %s", a.ID, a2.ID, b.ID)
	}
	for _, r := range []*repository.Repository{a, a2, b} {
		got, err := f.env.Store().GetByName(f.ctx, r.Scope, "backend")
		if err != nil || got.ID != r.ID {
			t.Fatalf("by name in %s: %v %v", r.Scope, got, err)
		}
		wantEq(t, "scope", got.Scope, r.Scope)
	}

	store := f.env.Store()
	t.Run("the store returns the holder of a name and writes nothing", func(t *testing.T) {
		clone, err := repository.New(repository.NewParams{ID: "repo_t127_other", Scope: ScopeA, Name: "backend", CreatedBy: Engineer}, f.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		got, created, err := store.Insert(f.ctx, clone, repository.Effects{Audit: []repository.Audit{{Action: "repository.provisioning_requested", Actor: Engineer}}})
		if err != nil || created || got.ID != a.ID {
			t.Fatalf("insert of a held name: %v %v %v", got, created, err)
		}
		if _, err := store.Get(f.ctx, ScopeA, "repo_t127_other"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the refused identifier was stored: %v", err)
		}
		wantSlice(t, "audit of the holder", f.audit(a), []string{"repository.provisioning_requested", "repository.ready"})
	})

	t.Run("an identifier collision never replaces a repository", func(t *testing.T) {
		clash, err := repository.New(repository.NewParams{ID: a.ID, Scope: ScopeA2, Name: "another", CreatedBy: Engineer}, f.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got, created, err := store.Insert(f.ctx, clash, repository.Effects{}); err == nil || created {
			t.Fatalf("a reused identifier was accepted: %v %v %v", got, created, err)
		}
		still := f.get(ScopeA, a.ID)
		wantEq(t, "name", still.Name, "backend")
	})
}

// PolicyDefaultsDeny: an omitted policy is false (O-33, GR-19), a patch states
// it, the admin sets policy without any other authority and only a ready
// repository is patched.
func PolicyDefaultsDeny(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	r := f.create(ScopeA, "svc").Repository
	wantEq(t, "omitted policy", r.Policy.DirectPushToDefault, false)
	explicit := f.create(ScopeA, "svc-explicit", direct(false)).Repository
	wantEq(t, "explicit false", explicit.Policy.DirectPushToDefault, false)
	opted := f.create(ScopeA, "svc-open", direct(true)).Repository
	wantEq(t, "explicit true", opted.Policy.DirectPushToDefault, true)

	res, err := f.svc.Describe(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	wantEq(t, "described policy", res.Policy, gitv1.RepositoryPolicy{})
	wantEq(t, "clone URL", res.CloneURL, "https://git.example.invalid/git/t127_acme/p127_inventory/svc.git")

	on, off := true, false
	patch := func(actor repository.Actor, id repository.ID, expected domain.Version, p gitv1.PatchRepositoryRequest) (*repository.Repository, error) {
		return f.svc.Update(f.ctx, repository.UpdateCommand{Actor: actor, Scope: ScopeA, ID: id, Expected: expected, Patch: p})
	}
	policyPatch := func(v *bool) gitv1.PatchRepositoryRequest {
		return gitv1.PatchRepositoryRequest{Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: v}}
	}

	_, err = patch(Engineer, r.ID, 0, gitv1.PatchRepositoryRequest{})
	wantErr(t, "empty patch", err, repository.ErrInvalidValue)
	_, err = patch(Engineer, r.ID, 0, policyPatch(nil))
	wantErr(t, "a policy that does not state the flag", err, repository.ErrInvalidValue)
	_, err = patch(Viewer, r.ID, 0, policyPatch(&on))
	wantErr(t, "viewer", err, repository.ErrDenied)
	_, err = patch(Admin, r.ID, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk")})
	wantErr(t, "admin changing the branch", err, repository.ErrDenied)
	_, err = patch(Admin, r.ID, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}})
	wantErr(t, "a patch needs every authority it names", err, repository.ErrDenied)
	wantEq(t, "nothing changed", f.get(ScopeA, r.ID).Version, r.Version)

	got, err := patch(Admin, r.ID, r.Version, policyPatch(&on))
	if err != nil || !got.Policy.DirectPushToDefault || got.Version != r.Version+1 {
		t.Fatalf("admin sets policy: %+v %v", got, err)
	}
	wantSlice(t, "audit", f.audit(got), []string{"repository.policy_changed", "repository.provisioning_requested", "repository.ready"})
	_, err = patch(Admin, r.ID, r.Version, policyPatch(&off))
	wantErr(t, "stale version", err, repository.ErrVersionConflict)

	same, err := patch(Engineer, r.ID, got.Version, policyPatch(&on))
	if err != nil || same.Version != got.Version {
		t.Fatalf("an unchanged policy bumps the version: %+v %v", same, err)
	}
	off2, err := patch(Engineer, r.ID, got.Version, policyPatch(&off))
	if err != nil || off2.Policy.DirectPushToDefault || off2.Version != got.Version+1 {
		t.Fatalf("policy back to false: %+v %v", off2, err)
	}

	t.Run("the default branch changes at any time while ready", func(t *testing.T) {
		moved, err := patch(Engineer, r.ID, off2.Version, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk")})
		if err != nil || moved.DefaultBranch != "trunk" || moved.Version != off2.Version+1 {
			t.Fatalf("branch change: %+v %v", moved, err)
		}
		again, err := patch(Engineer, r.ID, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk")})
		if err != nil || again.Version != moved.Version {
			t.Fatalf("an unchanged branch bumps the version: %+v %v", again, err)
		}
		for _, b := range []string{"kodama/work", "refs/heads/x", "a..b"} {
			_, err = patch(Engineer, r.ID, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr(b)})
			wantErr(t, "branch "+b, err, repository.ErrInvalidValue)
		}
		wantEq(t, "stored branch", f.get(ScopeA, r.ID).DefaultBranch, "trunk")
	})

	t.Run("a repository that is not ready is not patched", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		p := f.create(ScopeA, "pending").Repository
		wantEq(t, "status", p.Status, gitv1.StatusProvisioning)
		_, err := patch(Engineer, p.ID, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk")})
		wantErr(t, "provisioning", err, repository.ErrNotReady)
		_, err = patch(Engineer, p.ID, 0, policyPatch(&on))
		wantErr(t, "provisioning policy", err, repository.ErrNotReady)
		if _, err := f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, 0); err != nil {
			t.Fatal(err)
		}
		_, err = patch(Engineer, r.ID, 0, policyPatch(&on))
		wantErr(t, "archived", err, repository.ErrArchived)
	})
}

// ProvisioningReconciles: an interrupted provisioning is finished by the
// reconciler, the status change and repository.created commit together, and
// every worker failure is a visible reason, never another status (GR-02,
// GR-A02).
func ProvisioningReconciles(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)

	t.Run("a worker that was not reached leaves a reason; the sweep completes it", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		r := f.create(ScopeA, "wait").Repository
		wantEq(t, "status", r.Status, gitv1.StatusProvisioning)
		wantEq(t, "reason", r.Reason, repository.ReasonWorkerUnavailable)
		wantEq(t, "version", r.Version, domain.Version(2))
		wantSlice(t, "no event before ready", f.events(r), nil)
		again := f.get(ScopeA, r.ID)
		wantEq(t, "stored reason", again.Reason, repository.ReasonWorkerUnavailable)

		rep, err := f.svc.Reconcile(f.ctx, ScopeA.TenantID)
		if err != nil || rep.Ready != 1 || rep.Pending != 0 {
			t.Fatalf("sweep: %+v %v", rep, err)
		}
		ready := f.get(ScopeA, r.ID)
		wantEq(t, "status after the sweep", ready.Status, gitv1.StatusReady)
		wantEq(t, "reason after the sweep", ready.Reason, "")
		wantSlice(t, "events", f.events(ready), readyEvents)
		wantEq(t, "worker initializations", f.hosts.Initialized(ScopeA.TenantID, string(r.ID)), 2)
		rep, err = f.svc.Reconcile(f.ctx, ScopeA.TenantID)
		if err != nil || rep.Examined != 0 {
			t.Fatalf("a second sweep: %+v %v", rep, err)
		}
		wantSlice(t, "events after a second sweep", f.events(ready), readyEvents)
	})

	t.Run("a lost response after the worker applied converges on one repository and one event", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unknown(), Apply: true})
		r := f.create(ScopeA, "lost").Repository
		wantEq(t, "reason", r.Reason, repository.ReasonInitializationUnknown)
		wantEq(t, "the worker holds it", f.hosts.Created(ScopeA.TenantID, string(r.ID)), true)
		if rep, err := f.svc.Reconcile(f.ctx, ScopeA.TenantID); err != nil || rep.Ready != 1 {
			t.Fatalf("sweep: %+v %v", rep, err)
		}
		ready := f.get(ScopeA, r.ID)
		wantEq(t, "status", ready.Status, gitv1.StatusReady)
		wantSlice(t, "events", f.events(ready), readyEvents)
	})

	t.Run("a refusal stays visible and is retried", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: RefusedBy(domain.CodeUnavailable)}, ScriptedFailure{Err: RefusedBy(domain.CodeUnavailable)})
		r := f.create(ScopeA, "refused").Repository
		wantEq(t, "reason", r.Reason, repository.ReasonInitializationRefused)
		if rep, err := f.svc.Reconcile(f.ctx, ScopeA.TenantID); err != nil || rep.Pending != 1 {
			t.Fatalf("a refused sweep: %+v %v", rep, err)
		}
		wantEq(t, "still provisioning", f.get(ScopeA, r.ID).Status, gitv1.StatusProvisioning)
		if rep, err := f.svc.Reconcile(f.ctx, ScopeA.TenantID); err != nil || rep.Ready != 1 {
			t.Fatalf("the retry: %+v %v", rep, err)
		}
	})

	t.Run("a crash between the worker's answer and the commit changes nothing and is repaired", func(t *testing.T) {
		f.env.FailEvents(errFault)
		_, err := f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "crash"})
		if err == nil {
			t.Fatal("the failed completion was reported as success")
		}
		f.env.FailEvents(nil)
		r, gerr := f.env.Store().GetByName(f.ctx, ScopeA, "crash")
		if gerr != nil {
			t.Fatal(gerr)
		}
		wantEq(t, "status", r.Status, gitv1.StatusProvisioning)
		wantEq(t, "version", r.Version, domain.Version(1))
		wantSlice(t, "no event survived the rollback", f.events(r), nil)
		wantEq(t, "the worker holds it", f.hosts.Created(ScopeA.TenantID, string(r.ID)), true)
		if rep, err := f.svc.Reconcile(f.ctx, ScopeA.TenantID); err != nil || rep.Ready != 1 {
			t.Fatalf("repair: %+v %v", rep, err)
		}
		ready := f.get(ScopeA, r.ID)
		wantEq(t, "status", ready.Status, gitv1.StatusReady)
		wantSlice(t, "events", f.events(ready), readyEvents)
		wantEq(t, "worker creations", f.hosts.Initialized(ScopeA.TenantID, string(r.ID)), 2)
	})

	t.Run("the worker's confirmation completes provisioning idempotently", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		r := f.create(ScopeA, "confirm", branch("trunk")).Repository
		wantEq(t, "status", r.Status, gitv1.StatusProvisioning)
		_, err := f.svc.ConfirmInitialized(f.ctx, ScopeA.TenantID, gitv1.InitializedRequest{RepositoryID: string(r.ID), DefaultBranch: "other", Empty: true})
		wantErr(t, "another default branch", err, repository.ErrInvalidValue)
		wantEq(t, "the mismatch is visible", f.get(ScopeA, r.ID).Reason, repository.ReasonInitializationMismatch)
		resp, err := f.svc.ConfirmInitialized(f.ctx, ScopeA.TenantID, gitv1.InitializedRequest{RepositoryID: string(r.ID), DefaultBranch: "trunk", Empty: true})
		if err != nil || resp.Status != gitv1.StatusReady || resp.RepositoryID != string(r.ID) {
			t.Fatalf("confirm: %+v %v", resp, err)
		}
		again, err := f.svc.ConfirmInitialized(f.ctx, ScopeA.TenantID, gitv1.InitializedRequest{RepositoryID: string(r.ID), DefaultBranch: "trunk", Empty: true})
		if err != nil || again != resp {
			t.Fatalf("a repeated confirmation: %+v %v", again, err)
		}
		wantSlice(t, "events", f.events(f.get(ScopeA, r.ID)), readyEvents)
		_, err = f.svc.ConfirmInitialized(f.ctx, ScopeB.TenantID, gitv1.InitializedRequest{RepositoryID: string(r.ID), DefaultBranch: "trunk", Empty: true})
		wantErr(t, "another tenant's worker", err, repository.ErrNotFound)
	})

	t.Run("a cancelled caller records no outcome", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		r := f.create(ScopeA, "cancel").Repository
		before := f.get(ScopeA, r.ID)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := f.svc.Provision(ctx, ScopeA, r.ID); err == nil {
			t.Fatal("a cancelled provisioning succeeded")
		}
		after := f.get(ScopeA, r.ID)
		if *after != *before {
			t.Fatalf("a cancelled provisioning changed the repository: %+v -> %+v", before, after)
		}
	})

	t.Run("the sweep covers every tenant", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()}, ScriptedFailure{Err: Unavailable()})
		a := f.create(ScopeA2, "sweep-a").Repository
		b := f.create(ScopeB, "sweep-b").Repository
		rep, err := f.svc.ReconcileAll(f.ctx)
		if err != nil || rep.Ready < 2 {
			t.Fatalf("sweep all: %+v %v", rep, err)
		}
		wantEq(t, "tenant A", f.get(ScopeA2, a.ID).Status, gitv1.StatusReady)
		wantEq(t, "tenant B", f.get(ScopeB, b.ID).Status, gitv1.StatusReady)
		wantEq(t, "the worker of tenant A never saw B's repository", f.hosts.Created(ScopeA.TenantID, string(b.ID)), false)
	})
}

// ArchiveRestore: archive refuses pushes and runs by status and never deletes
// history; restore reopens it; both are explicit, versioned, audited
// transitions (GR-14).
func ArchiveRestore(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	r := f.create(ScopeA, "keep").Repository

	_, err := f.svc.Archive(f.ctx, Viewer, ScopeA, r.ID, 0)
	wantErr(t, "viewer archive", err, repository.ErrDenied)
	_, err = f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, r.Version+5)
	wantErr(t, "stale archive", err, repository.ErrVersionConflict)

	arch, err := f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, r.Version)
	if err != nil || arch.Status != gitv1.StatusArchived || arch.Reason != repository.ReasonArchived || arch.Version != r.Version+1 {
		t.Fatalf("archive: %+v %v", arch, err)
	}
	wantSlice(t, "events", f.events(arch), []string{"repository.created", "repository.state_changed", "repository.state_changed"})
	wantSlice(t, "audit", f.audit(arch), []string{"repository.archived", "repository.provisioning_requested", "repository.ready"})

	again, err := f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, 0)
	if err != nil || again.Version != arch.Version {
		t.Fatalf("archiving twice: %+v %v", again, err)
	}
	wantEq(t, "events after a repeated archive", len(f.events(arch)), 3)
	if _, err := f.svc.ReadRefs(f.ctx, Viewer, ScopeA, r.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix}); err != nil {
		t.Fatalf("an archived repository keeps its history readable: %v", err)
	}

	_, err = f.svc.Restore(f.ctx, Viewer, ScopeA, r.ID, 0)
	wantErr(t, "viewer restore", err, repository.ErrDenied)
	back, err := f.svc.Restore(f.ctx, Engineer, ScopeA, r.ID, arch.Version)
	if err != nil || back.Status != gitv1.StatusReady || back.Reason != "" || back.Version != arch.Version+1 {
		t.Fatalf("restore: %+v %v", back, err)
	}
	wantEq(t, "events", len(f.events(back)), 4)
	noop, err := f.svc.Restore(f.ctx, Engineer, ScopeA, r.ID, 0)
	if err != nil || noop.Version != back.Version {
		t.Fatalf("restoring a ready repository: %+v %v", noop, err)
	}

	t.Run("archive and restore never make a non-empty repository seedable", func(t *testing.T) {
		n := f.create(ScopeA, "seeded").Repository
		wantEq(t, "empty at the start", n.Empty, true)
		if _, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, n.ID, false, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Archive(f.ctx, Engineer, ScopeA, n.ID, 0); err != nil {
			t.Fatal(err)
		}
		got, err := f.svc.Restore(f.ctx, Engineer, ScopeA, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		wantEq(t, "empty after restore", got.Empty, false)
	})

	t.Run("a provisioning repository is neither archived nor restored", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		p := f.create(ScopeA, "young").Repository
		_, err := f.svc.Archive(f.ctx, Engineer, ScopeA, p.ID, 0)
		wantErr(t, "archive", err, repository.ErrNotReady)
		_, err = f.svc.Restore(f.ctx, Engineer, ScopeA, p.ID, 0)
		wantErr(t, "restore", err, repository.ErrNotReady)
		wantEq(t, "still provisioning", f.get(ScopeA, p.ID).Status, gitv1.StatusProvisioning)
		_, err = f.svc.ReadRefs(f.ctx, Engineer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix})
		wantErr(t, "refs of a provisioning repository", err, repository.ErrNotReady)
	})
}

// CrossTenantHidden: a repository of another tenant or project is
// indistinguishable from an absent one on every path (GR-05, GR-A01).
func CrossTenantHidden(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	a := f.create(ScopeA, "secret").Repository
	b := f.create(ScopeB, "secret").Repository
	wantEq(t, "each tenant has its own", a.ID != b.ID, true)

	foreign := []domain.Scope{ScopeB, ScopeA2}
	for _, scope := range foreign {
		if _, err := f.svc.Get(f.ctx, Engineer, scope, a.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("get from %s: %v", scope, err)
		}
		if _, err := f.env.Store().Get(f.ctx, scope, a.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("store get from %s: %v", scope, err)
		}
		_, err := f.svc.Update(f.ctx, repository.UpdateCommand{Actor: Engineer, Scope: scope, ID: a.ID,
			Patch: gitv1.PatchRepositoryRequest{DefaultBranch: ptr("x")}})
		wantErr(t, "update from "+scope.String(), err, repository.ErrNotFound)
		_, err = f.svc.Archive(f.ctx, Engineer, scope, a.ID, 0)
		wantErr(t, "archive from "+scope.String(), err, repository.ErrNotFound)
		_, err = f.svc.Restore(f.ctx, Engineer, scope, a.ID, 0)
		wantErr(t, "restore from "+scope.String(), err, repository.ErrNotFound)
		_, err = f.svc.ReadRefs(f.ctx, Engineer, scope, a.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix})
		wantErr(t, "refs from "+scope.String(), err, repository.ErrNotFound)
		_, err = f.svc.Provision(f.ctx, scope, a.ID)
		wantErr(t, "provision from "+scope.String(), err, repository.ErrNotFound)
	}
	if _, err := f.env.Store().GetInTenant(f.ctx, ScopeB.TenantID, a.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("worker-side read of another tenant: %v", err)
	}
	if _, err := f.svc.ReportEmpty(f.ctx, ScopeB.TenantID, a.ID, false, time.Time{}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("report from another tenant's worker: %v", err)
	}
	if got, err := f.env.Store().GetInTenant(f.ctx, ScopeA.TenantID, a.ID); err != nil || got.Scope != ScopeA {
		t.Fatalf("worker-side read: %v %v", got, err)
	}
	for _, id := range []repository.ID{"repo_t127_guess", "x", "../etc"} {
		if _, err := f.svc.Get(f.ctx, Engineer, ScopeA, id); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("guessed id %q: %v", id, err)
		}
	}

	list, err := f.svc.List(f.ctx, Engineer, ScopeB, repository.ListQuery{Limit: 10})
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("tenant B's list: %v %v", list, err)
	}
	if list, err := f.svc.List(f.ctx, Engineer, ScopeA2, repository.ListQuery{Limit: 10}); err != nil || len(list) != 0 {
		t.Fatalf("another project's list: %v %v", list, err)
	}

	_, err = f.svc.Get(f.ctx, Outsider, ScopeA, a.ID)
	wantErr(t, "an actor with no role", err, repository.ErrDenied)
	_, err = f.svc.List(f.ctx, Outsider, ScopeA, repository.ListQuery{Limit: 10})
	wantErr(t, "an actor with no role lists", err, repository.ErrDenied)
	wantEq(t, "tenant A's repository is untouched", f.get(ScopeA, a.ID).Version, a.Version)
	wantEq(t, "the worker of tenant B never initialized A's", f.hosts.Initialized(ScopeB.TenantID, string(a.ID)), 0)
}

// ExternalMapping: the project's configured repository maps to a read-only
// hosting: external record, idempotently, without a worker and without a
// transition (docs/implementation/internal-git-service.md, "Migration and
// cutover").
func ExternalMapping(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	cfg := repository.ExternalConfig{ID: "repo_cfg", URL: "https://git.example.test/acme/Backend.git", BaseRef: "refs/heads/trunk"}

	_, _, err := f.svc.RegisterExternal(f.ctx, Engineer, ScopeA, cfg)
	wantErr(t, "an engineer is not the installer", err, repository.ErrDenied)

	r, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeA, cfg)
	if err != nil || !created {
		t.Fatalf("register: %+v %v %v", r, created, err)
	}
	wantEq(t, "hosting", r.Hosting, gitv1.HostingExternal)
	wantEq(t, "status", r.Status, gitv1.StatusReady)
	wantEq(t, "name", r.Name, "backend")
	wantEq(t, "default branch", r.DefaultBranch, "trunk")
	wantEq(t, "empty", r.Empty, false)
	wantEq(t, "policy", r.Policy.DirectPushToDefault, false)
	wantEq(t, "id", r.ID, repository.ExternalID(ScopeA, "repo_cfg"))
	wantSlice(t, "events", f.events(r), []string{"repository.created"})
	res, err := f.svc.Describe(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	wantEq(t, "clone URL", res.CloneURL, cfg.URL)
	wantEq(t, "described hosting", res.Hosting, gitv1.HostingExternal)
	if res.Reason != nil {
		t.Fatalf("a ready record has a reason: %v", *res.Reason)
	}

	t.Run("the mapping is idempotent", func(t *testing.T) {
		again, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeA, cfg)
		if err != nil || created || again.ID != r.ID || again.Version != r.Version {
			t.Fatalf("repeat: %+v %v %v", again, created, err)
		}
		wantEq(t, "events", len(f.events(r)), 1)
		other, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeA2, cfg)
		if err != nil || !created || other.ID == r.ID {
			t.Fatalf("the same configuration in another project is another record: %+v %v %v", other, created, err)
		}
	})

	t.Run("the record is read-only", func(t *testing.T) {
		_, err := f.svc.Update(f.ctx, repository.UpdateCommand{Actor: Engineer, Scope: ScopeA, ID: r.ID, Patch: gitv1.PatchRepositoryRequest{DefaultBranch: ptr("x")}})
		wantErr(t, "patch", err, repository.ErrHostingMismatch)
		_, err = f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, 0)
		wantErr(t, "archive", err, repository.ErrHostingMismatch)
		_, err = f.svc.Restore(f.ctx, Engineer, ScopeA, r.ID, 0)
		wantErr(t, "restore", err, repository.ErrHostingMismatch)
		_, err = f.svc.ReadRefs(f.ctx, Engineer, ScopeA, r.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix})
		wantErr(t, "refs", err, repository.ErrHostingMismatch)
		_, err = f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, true, time.Time{})
		wantErr(t, "an emptiness report", err, repository.ErrHostingMismatch)
		_, err = f.svc.ConfirmInitialized(f.ctx, ScopeA.TenantID, gitv1.InitializedRequest{RepositoryID: string(r.ID), DefaultBranch: "trunk", Empty: true})
		wantErr(t, "a worker confirmation", err, repository.ErrNotFound)
		_, err = f.svc.Provision(f.ctx, ScopeA, r.ID)
		wantErr(t, "provisioning", err, repository.ErrHostingMismatch)
		wantEq(t, "no worker call", len(f.hosts.Calls), 0)
		wantEq(t, "unchanged", f.get(ScopeA, r.ID).Version, r.Version)
		_, err = f.svc.Create(f.ctx, Engineer, ScopeA, gitv1.CreateRepositoryRequest{Name: "backend", DefaultBranch: ptr("trunk")})
		wantErr(t, "an internal repository cannot take the name", err, repository.ErrNameTaken)
	})

	t.Run("a held name is avoided and the mapping is still stable", func(t *testing.T) {
		held := f.create(ScopeB, "backend").Repository
		wantEq(t, "hosting", held.Hosting, gitv1.HostingInternal)
		first, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeB, cfg)
		if err != nil || !created || first.Name == "backend" || first.Hosting != gitv1.HostingExternal {
			t.Fatalf("register beside a held name: %+v %v %v", first, created, err)
		}
		again, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeB, cfg)
		if err != nil || created || again.ID != first.ID {
			t.Fatalf("repeat: %+v %v %v", again, created, err)
		}
		wantEq(t, "the internal repository is untouched", f.get(ScopeB, held.ID).Hosting, gitv1.HostingInternal)
	})

	t.Run("an unmappable configuration records nothing", func(t *testing.T) {
		before, err := f.svc.List(f.ctx, Engineer, ScopeA2, repository.ListQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []repository.ExternalConfig{
			{ID: "repo_x", URL: "git@example.test:acme/app.git", BaseRef: "main"},
			{ID: "repo_x", URL: "ssh://example.test/acme/app.git", BaseRef: "main"},
			{ID: "repo_x", URL: "https://user:secret@example.test/acme/app.git", BaseRef: "main"},
			{ID: "repo_x", URL: "http://example.test/acme/app.git", BaseRef: "main"},
			{ID: "repo_x", URL: "https://example.test/acme/app.git", BaseRef: "refs/heads/kodama/work"},
			{ID: "", URL: "https://example.test/acme/app.git", BaseRef: "main"},
		} {
			if _, _, err := f.svc.RegisterExternal(f.ctx, System, ScopeA2, bad); !errors.Is(err, repository.ErrInvalidValue) {
				t.Fatalf("%+v: %v", bad, err)
			}
		}
		after, err := f.svc.List(f.ctx, Engineer, ScopeA2, repository.ListQuery{Limit: 100})
		if err != nil || len(after) != len(before) {
			t.Fatalf("a refused mapping recorded something: %d -> %d %v", len(before), len(after), err)
		}
	})

	t.Run("the configured repository is read through the port", func(t *testing.T) {
		got, mapped, err := f.svc.MapConfigured(f.ctx, System, ScopeB)
		if err != nil || mapped || got != nil {
			// ScopeB has no configured repository yet.
			t.Fatalf("nothing configured: %+v %v %v", got, mapped, err)
		}
		f.ext.Set(ScopeB, repository.ExternalConfig{ID: "repo_b", URL: "https://git.example.test/globex/ledger", BaseRef: ""})
		got, created, err := f.svc.MapConfigured(f.ctx, System, ScopeB)
		if err != nil || !created || got.Name != "ledger" || got.DefaultBranch != "main" {
			t.Fatalf("map: %+v %v %v", got, created, err)
		}
		again, created, err := f.svc.MapConfigured(f.ctx, System, ScopeB)
		if err != nil || created || again.ID != got.ID {
			t.Fatalf("map again: %+v %v %v", again, created, err)
		}
		f.ext.Err = ErrOutage()
		_, _, err = f.svc.MapConfigured(f.ctx, System, ScopeB)
		wantErr(t, "a configuration outage", err, repository.ErrUnavailable)
		f.ext.Err = nil
	})
}

// EmptyReports: the stored emptiness follows the worker's reports; a report
// older than the stored one never overwrites it (GR-18).
func EmptyReports(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	r := f.create(ScopeA, "reports").Repository
	t0 := f.clock.Now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	_, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, false, at(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got := f.get(ScopeA, r.ID)
	wantEq(t, "refs reported", got.Empty, false)
	wantEq(t, "version", got.Version, r.Version+1)

	stale, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, true, at(time.Second))
	if err != nil || stale.Empty || stale.Version != got.Version {
		t.Fatalf("a stale report: %+v %v", stale, err)
	}
	wantEq(t, "stored after a stale report", f.get(ScopeA, r.ID).Empty, false)

	// The same value observed later moves only the observation: no new version,
	// but a report between the two observations is now stale.
	same, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, false, at(4*time.Second))
	if err != nil || same.Version != got.Version {
		t.Fatalf("a repeated report: %+v %v", same, err)
	}
	between, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, true, at(3*time.Second))
	if err != nil || between.Empty || between.Version != got.Version {
		t.Fatalf("a report older than the last observation: %+v %v", between, err)
	}
	wantEq(t, "stored", f.get(ScopeA, r.ID).Empty, false)

	newer, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, r.ID, true, at(5*time.Second))
	if err != nil || !newer.Empty || newer.Version != got.Version+1 {
		t.Fatalf("a newer report: %+v %v", newer, err)
	}

	t.Run("a refs read refreshes the stored emptiness", func(t *testing.T) {
		p := f.create(ScopeA, "refresh").Repository
		f.hosts.Refs = gitv1.RefsResponse{Refs: []gitv1.RefTip{{Ref: "refs/heads/main", Commit: "1111111111111111111111111111111111111111"}}, Empty: false}
		resp, err := f.svc.ReadRefs(f.ctx, Viewer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix, Limit: 10})
		if err != nil || len(resp.Refs) != 1 {
			t.Fatalf("refs: %+v %v", resp, err)
		}
		wantEq(t, "emptiness", f.get(ScopeA, p.ID).Empty, false)
		_, err = f.svc.ReadRefs(f.ctx, Viewer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.ServerRefPrefix})
		wantErr(t, "the server namespace is not public", err, repository.ErrInvalidValue)
		f.hosts.RefsErr = Unavailable()
		_, err = f.svc.ReadRefs(f.ctx, Viewer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.TagsPrefix})
		wantErr(t, "a worker outage", err, repository.ErrUnavailable)
		f.hosts.RefsErr = nil
	})

	t.Run("an unchanged confirmation advances the watermark, so a delayed older report is ignored", func(t *testing.T) {
		p := f.create(ScopeA, "watermark").Repository
		t1 := f.clock.Now()
		if _, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, p.ID, false, t1); err != nil {
			t.Fatal(err)
		}
		nonEmpty := f.get(ScopeA, p.ID)
		wantEq(t, "refs reported", nonEmpty.Empty, false)
		// The worker confirms the same value later through a refs read (t2 > t1).
		f.hosts.Refs = gitv1.RefsResponse{Refs: []gitv1.RefTip{{Ref: "refs/heads/main", Commit: "1111111111111111111111111111111111111111"}}, Empty: false}
		if _, err := f.svc.ReadRefs(f.ctx, Viewer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix, Limit: 10}); err != nil {
			t.Fatal(err)
		}
		wantEq(t, "an unchanged confirmation bumps no version", f.get(ScopeA, p.ID).Version, nonEmpty.Version)
		// A delayed report observed between t1 and the confirmation says "empty":
		// it is older than the newest evidence and must not make the repository
		// seedable.
		late, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, p.ID, true, t1.Add(time.Microsecond))
		if err != nil || late.Empty || late.Version != nonEmpty.Version {
			t.Fatalf("a delayed contradictory report: %+v %v", late, err)
		}
		wantEq(t, "stored", f.get(ScopeA, p.ID).Empty, false)
	})

	t.Run("overlapping reports and refs reads leave the newest evidence", func(t *testing.T) {
		p := f.create(ScopeA, "overlap").Repository
		base := f.clock.Now().Add(time.Hour)
		f.hosts.Refs = gitv1.RefsResponse{Refs: []gitv1.RefTip{}, Empty: true} // an older, contradicting observation
		const n = 20
		var wg sync.WaitGroup
		for i := 1; i <= n; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				// The value at the greatest time (i == n) is "has refs".
				if _, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, p.ID, i%2 == 1, base.Add(time.Duration(i)*time.Second)); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := f.svc.ReadRefs(f.ctx, Viewer, ScopeA, p.ID, repository.RefsQuery{Prefix: gitv1.HeadsPrefix, Limit: 10}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		got := f.get(ScopeA, p.ID)
		wantEq(t, "the newest report decided", got.Empty, false)
		wantEq(t, "its observation", got.EmptyObservedAt.Equal(base.Add(n*time.Second)), true)
	})

	t.Run("a provisioning repository takes no report", func(t *testing.T) {
		f.hosts.Script(ScriptedFailure{Err: Unavailable()})
		p := f.create(ScopeA, "young-empty").Repository
		_, err := f.svc.ReportEmpty(f.ctx, ScopeA.TenantID, p.ID, true, time.Time{})
		wantErr(t, "report", err, repository.ErrNotReady)
	})
}

// ListPaging: a project's repositories are listed in (created_at, id) order
// and a position continues exactly after the last row.
func ListPaging(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	var want []repository.ID
	for _, name := range []string{"a-one", "b-two", "c-three", "d-four", "e-five"} {
		want = append(want, f.create(ScopeA, name).Repository.ID)
	}
	f.create(ScopeB, "other-tenant")
	f.create(ScopeA2, "other-project")

	var got []repository.ID
	var after *repository.Position
	for pages := 0; pages < 10; pages++ {
		page, err := f.svc.List(f.ctx, Viewer, ScopeA, repository.ListQuery{After: after, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got = append(got, r.ID)
		}
		if len(page) < 3 {
			break
		}
		last := page[len(page)-1]
		after = &repository.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged = %v, want %v", got, want)
	}
	if _, err := f.svc.List(f.ctx, Viewer, ScopeA, repository.ListQuery{Limit: 0}); !errors.Is(err, repository.ErrInvalidValue) {
		t.Fatalf("a zero page: %v", err)
	}
}

// AtomicEffects: audit and outbox records commit with the change or not at all.
func AtomicEffects(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)

	t.Run("a registration whose event fails is not recorded", func(t *testing.T) {
		f.env.FailEvents(errFault)
		_, _, err := f.svc.RegisterExternal(f.ctx, System, ScopeA, repository.ExternalConfig{ID: "repo_cfg", URL: "https://git.example.test/acme/app", BaseRef: "main"})
		f.env.FailEvents(nil)
		if err == nil {
			t.Fatal("the failed event was reported as success")
		}
		if _, err := f.env.Store().GetByName(f.ctx, ScopeA, "app"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the rolled-back record exists: %v", err)
		}
		if _, created, err := f.svc.RegisterExternal(f.ctx, System, ScopeA, repository.ExternalConfig{ID: "repo_cfg", URL: "https://git.example.test/acme/app", BaseRef: "main"}); err != nil || !created {
			t.Fatalf("the retry: %v %v", created, err)
		}
	})

	t.Run("an archive whose event fails changes nothing", func(t *testing.T) {
		r := f.create(ScopeA, "atomic").Repository
		f.env.FailEvents(errFault)
		_, err := f.svc.Archive(f.ctx, Engineer, ScopeA, r.ID, 0)
		f.env.FailEvents(nil)
		if err == nil {
			t.Fatal("the failed event was reported as success")
		}
		got := f.get(ScopeA, r.ID)
		wantEq(t, "status", got.Status, gitv1.StatusReady)
		wantEq(t, "version", got.Version, r.Version)
		wantSlice(t, "events", f.events(got), readyEvents)
		wantSlice(t, "audit", f.audit(got), []string{"repository.provisioning_requested", "repository.ready"})
	})
}

func ptr[T any](v T) *T { return &v }

// PatchBothSettings: a patch naming the default branch and the policy is one
// transition with one version step (the storage guard refuses any other), both
// settings and their audit records commit together, and a patch that fails
// changes neither.
func PatchBothSettings(t *testing.T, open OpenEnv) {
	f := newFixture(t, open)
	r := f.create(ScopeA, "both").Repository
	on := true
	both := gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}}
	patch := func(actor repository.Actor, expected domain.Version, p gitv1.PatchRepositoryRequest) (*repository.Repository, error) {
		return f.svc.Update(f.ctx, repository.UpdateCommand{Actor: actor, Scope: ScopeA, ID: r.ID, Expected: expected, Patch: p})
	}

	got, err := patch(Engineer, r.Version, both)
	if err != nil {
		t.Fatalf("a two-setting patch: %v", err)
	}
	wantEq(t, "default branch", got.DefaultBranch, "trunk")
	wantEq(t, "policy", got.Policy.DirectPushToDefault, true)
	wantEq(t, "one version step", got.Version, r.Version+1)
	stored := f.get(ScopeA, r.ID)
	wantEq(t, "stored branch", stored.DefaultBranch, "trunk")
	wantEq(t, "stored policy", stored.Policy.DirectPushToDefault, true)
	wantEq(t, "stored version", stored.Version, r.Version+1)
	wantSlice(t, "audit", f.audit(stored), []string{"repository.default_branch_changed", "repository.policy_changed", "repository.provisioning_requested", "repository.ready"})

	t.Run("one setting of two changing is still one step", func(t *testing.T) {
		off := false
		again, err := patch(Engineer, stored.Version, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &off}})
		if err != nil || again.Version != stored.Version+1 || again.Policy.DirectPushToDefault || again.DefaultBranch != "trunk" {
			t.Fatalf("%+v %v", again, err)
		}
		same, err := patch(Engineer, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("trunk"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &off}})
		if err != nil || same.Version != again.Version {
			t.Fatalf("an unchanged two-setting patch bumps the version: %+v %v", same, err)
		}
		wantEq(t, "audit rows", len(f.audit(same)), 5)
	})

	t.Run("a failing patch changes neither setting", func(t *testing.T) {
		before := f.get(ScopeA, r.ID)
		auditBefore := f.audit(before)
		off := false
		_, err := patch(Engineer, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("kodama/x"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}})
		wantErr(t, "an invalid branch", err, repository.ErrInvalidValue)
		_, err = patch(Admin, 0, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("other"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}})
		wantErr(t, "an admin naming the branch", err, repository.ErrDenied)
		_, err = patch(Engineer, before.Version+3, gitv1.PatchRepositoryRequest{DefaultBranch: ptr("other"), Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &off}})
		wantErr(t, "a stale version", err, repository.ErrVersionConflict)
		after := f.get(ScopeA, r.ID)
		if *after != *before {
			t.Fatalf("a failed patch changed the repository: %+v -> %+v", before, after)
		}
		wantSlice(t, "audit", f.audit(after), auditBefore)
	})

	t.Run("a mutation that steps the version twice is refused by the store", func(t *testing.T) {
		before := f.get(ScopeA, r.ID)
		_, err := f.env.Store().Update(f.ctx, ScopeA, r.ID, func(x *repository.Repository) (repository.Effects, error) {
			x.DefaultBranch = "double-step"
			x.Version += 2
			return repository.Effects{}, nil
		})
		if err == nil {
			t.Fatal("a two-step version change was accepted")
		}
		wantEq(t, "unchanged", *f.get(ScopeA, r.ID), *before)
	})
}
