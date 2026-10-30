package repositorytest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/repository"
)

// Fixed identities of the fixture. The two projects of tenant A and the project
// of tenant B exist in every Store under contract. Tenant and project names are
// deliberately alike, as in GR-A01: two tenants create identically named
// projects and repositories.
var (
	ScopeA  = domain.Scope{TenantID: "t127_acme", ProjectID: "p127_inventory"}
	ScopeA2 = domain.Scope{TenantID: "t127_acme", ProjectID: "p127_other"}
	ScopeB  = domain.Scope{TenantID: "t127_globex", ProjectID: "p127_inventory"}
)

// Actors of the fixture.
var (
	Engineer = repository.Actor{Type: repository.ActorUser, ID: "usr_engineer"}
	Admin    = repository.Actor{Type: repository.ActorUser, ID: "usr_admin"}
	Viewer   = repository.Actor{Type: repository.ActorUser, ID: "usr_viewer"}
	Outsider = repository.Actor{Type: repository.ActorUser, ID: "usr_outsider"}
	System   = repository.Actor{Type: repository.ActorService, ID: "installer"}
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)} }

// Now returns the current instant and advances it by a millisecond, so every
// reading differs and rows created in sequence order by (created_at, id).
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// Authz is a role table: what each actor may do in which scope, by project role
// (docs/specs/identity-tenancy-and-rbac.md). Anything not granted is denied;
// Fail simulates an authority outage, which must refuse (fail closed).
type Authz struct {
	mu     sync.Mutex
	grants map[string]map[repository.Action]bool
	err    error
	Calls  int
}

// NewAuthz grants the fixture's roles: the engineer manages repositories in
// every fixture scope but tenant B; the admin sets policy only; the viewer
// reads; the outsider nothing; the system actor registers external records.
func NewAuthz() *Authz {
	a := &Authz{grants: map[string]map[repository.Action]bool{}}
	for _, s := range []domain.Scope{ScopeA, ScopeA2} {
		a.Grant(Engineer, s, repository.ActionCreate, repository.ActionRead, repository.ActionList, repository.ActionUpdate,
			repository.ActionSetPolicy, repository.ActionArchive, repository.ActionRestore)
		a.Grant(Admin, s, repository.ActionRead, repository.ActionList, repository.ActionSetPolicy)
		a.Grant(Viewer, s, repository.ActionRead, repository.ActionList)
		a.Grant(System, s, repository.ActionRegisterRemote)
	}
	a.Grant(Engineer, ScopeB, repository.ActionCreate, repository.ActionRead, repository.ActionList, repository.ActionUpdate,
		repository.ActionSetPolicy, repository.ActionArchive, repository.ActionRestore)
	a.Grant(System, ScopeB, repository.ActionRegisterRemote)
	return a
}

func key(actor repository.Actor, scope domain.Scope) string {
	return string(actor.Type) + "/" + actor.ID + "@" + scope.String()
}

// Grant allows the actions of the actor in the scope.
func (a *Authz) Grant(actor repository.Actor, scope domain.Scope, actions ...repository.Action) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.grants[key(actor, scope)]
	if g == nil {
		g = map[repository.Action]bool{}
		a.grants[key(actor, scope)] = g
	}
	for _, x := range actions {
		g[x] = true
	}
}

// Fail makes every decision fail with err; nil restores the table.
func (a *Authz) Fail(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

// Authorize implements repository.Authorizer.
func (a *Authz) Authorize(_ context.Context, actor repository.Actor, action repository.Action, scope domain.Scope, _ repository.Resource) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls++
	if a.err != nil {
		return a.err
	}
	if a.grants[key(actor, scope)][action] {
		return nil
	}
	return repository.ErrDenied
}

// ScriptedFailure is the next Initialize outcome: Err is returned; Apply says
// whether the worker created the repository before failing (the response was
// lost).
type ScriptedFailure struct {
	Err   error
	Apply bool
}

// Unavailable, Unknown and RefusedBy build the worker failures of Hosts.
func Unavailable() error { return &repository.CallError{Outcome: repository.Unavailable} }

// Unknown is a failure after the request may have been applied.
func Unknown() error { return &repository.CallError{Outcome: repository.Unknown} }

// RefusedBy is a contract refusal of the worker.
func RefusedBy(code domain.ErrorCode) error {
	return &repository.CallError{Outcome: repository.Refused, Err: &gitv1.ContractError{Code: code, Message: "refused"}}
}

// Hosts is a fake tenant worker: initialization is idempotent on the repository
// ID per tenant, failures are scripted, and every call is recorded. The methods
// the repository module does not use answer unavailable.
type Hosts struct {
	mu       sync.Mutex
	repos    map[string]string // tenant/id -> default branch
	script   []ScriptedFailure
	Calls    []string // tenant/repository of every Initialize, in order
	Refs     gitv1.RefsResponse
	RefsErr  error
	RefsReqs []gitv1.ReadRefsRequest
	// NotEmpty makes Initialize report a repository that already has refs.
	NotEmpty bool
}

// NewHosts returns a worker with no repositories.
func NewHosts() *Hosts {
	return &Hosts{repos: map[string]string{}, Refs: gitv1.RefsResponse{Refs: []gitv1.RefTip{}, Empty: true}}
}

// Script queues the outcomes of the next Initialize calls.
func (h *Hosts) Script(f ...ScriptedFailure) {
	h.mu.Lock()
	h.script = append(h.script, f...)
	h.mu.Unlock()
}

// Created reports whether the worker holds the repository (the path exists).
func (h *Hosts) Created(tenant domain.TenantID, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.repos[string(tenant)+"/"+id]
	return ok
}

// Initialized counts the Initialize calls made for the repository.
func (h *Hosts) Initialized(tenant domain.TenantID, id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.Calls {
		if c == string(tenant)+"/"+id {
			n++
		}
	}
	return n
}

// Initialize implements repository.Hosts.
func (h *Hosts) Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.InitializeRepositoryResponse{}, RefusedBy(domain.CodeInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := string(tenant) + "/" + req.RepositoryID
	h.Calls = append(h.Calls, k)
	apply := func() bool {
		_, existed := h.repos[k]
		if !existed {
			h.repos[k] = req.DefaultBranch
		}
		return !existed
	}
	if len(h.script) > 0 {
		f := h.script[0]
		h.script = h.script[1:]
		if f.Err != nil {
			if f.Apply {
				apply()
			}
			return gitv1.InitializeRepositoryResponse{}, f.Err
		}
	}
	created := apply()
	return gitv1.InitializeRepositoryResponse{RepositoryID: req.RepositoryID, DefaultBranch: h.repos[k], Empty: !h.NotEmpty, Created: created}, nil
}

// ReadRefs implements repository.Hosts.
func (h *Hosts) ReadRefs(_ context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.RefsReqs = append(h.RefsReqs, req)
	if h.RefsErr != nil {
		return gitv1.RefsResponse{}, h.RefsErr
	}
	if _, ok := h.repos[string(tenant)+"/"+req.RepositoryID]; !ok {
		return gitv1.RefsResponse{}, RefusedBy(domain.CodeNotFound)
	}
	return h.Refs, nil
}

var errNotUsed = Unavailable()

// ReadRefValues implements repository.Hosts (unused by this module).
func (*Hosts) ReadRefValues(context.Context, domain.TenantID, gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	return gitv1.ReadRefValuesResponse{}, errNotUsed
}

// ReadJournal implements repository.Hosts (unused by this module).
func (*Hosts) ReadJournal(context.Context, domain.TenantID, gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	return gitv1.ReadJournalResponse{}, errNotUsed
}

// ReceiveLiveness implements repository.Hosts (unused by this module).
func (*Hosts) ReceiveLiveness(context.Context, domain.TenantID, string, string) (gitv1.ReceiveLivenessResponse, error) {
	return gitv1.ReceiveLivenessResponse{}, errNotUsed
}

// SetRetainedRef implements repository.Hosts (unused by this module).
func (*Hosts) SetRetainedRef(context.Context, domain.TenantID, gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error) {
	return gitv1.SetRetainedRefResponse{}, errNotUsed
}

// DeleteRetainedRef implements repository.Hosts (unused by this module).
func (*Hosts) DeleteRetainedRef(context.Context, domain.TenantID, gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error) {
	return gitv1.DeleteRetainedRefResponse{}, errNotUsed
}

// CreateBranch implements repository.Hosts (unused by this module).
func (*Hosts) CreateBranch(context.Context, domain.TenantID, gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error) {
	return gitv1.CreateBranchResponse{}, errNotUsed
}

// DeleteBranch implements repository.Hosts (unused by this module).
func (*Hosts) DeleteBranch(context.Context, domain.TenantID, gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error) {
	return gitv1.DeleteBranchResponse{}, errNotUsed
}

// Maintain implements repository.Hosts (unused by this module).
func (*Hosts) Maintain(context.Context, domain.TenantID, gitv1.MaintainRequest) (gitv1.MaintainResponse, error) {
	return gitv1.MaintainResponse{}, errNotUsed
}

var _ repository.Hosts = (*Hosts)(nil)

// URLs builds the fixture clone URLs; the keys are the identifiers themselves.
type URLs struct{}

// CloneURL implements repository.CloneURLs.
func (URLs) CloneURL(_ context.Context, scope domain.Scope, name string) (string, error) {
	return fmt.Sprintf("https://git.example.invalid/git/%s/%s/%s.git", scope.TenantID, scope.ProjectID, name), nil
}

// Tenants lists fixed tenants.
type Tenants struct{ IDs []domain.TenantID }

// TenantIDs implements repository.TenantSource.
func (t *Tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t.IDs, nil }

// External is a project's configured repository by scope.
type External struct {
	mu   sync.Mutex
	cfgs map[domain.Scope]repository.ExternalConfig
	Err  error
}

// NewExternal returns a source with nothing configured.
func NewExternal() *External { return &External{cfgs: map[domain.Scope]repository.ExternalConfig{}} }

// Set configures the project's repository.
func (e *External) Set(scope domain.Scope, cfg repository.ExternalConfig) {
	e.mu.Lock()
	e.cfgs[scope] = cfg
	e.mu.Unlock()
}

// Configured implements repository.ExternalSource.
func (e *External) Configured(_ context.Context, scope domain.Scope) (repository.ExternalConfig, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Err != nil {
		return repository.ExternalConfig{}, false, e.Err
	}
	cfg, ok := e.cfgs[scope]
	return cfg, ok, nil
}

var errOutage = errors.New("authority outage")

// ErrOutage is the error the fake authority returns when told to fail.
func ErrOutage() error { return errOutage }
