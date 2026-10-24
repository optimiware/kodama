// Package memory is the development-only, infrastructure-free adapter set of
// the MCP proxy (SD-12, SD-13): an mcpproxy.Store over the memdb kernel, with
// fake current runtime authority, fake MCP-kind secrets and run pins, a
// scripted deterministic remote server and the small policy, human and
// destination ports. Together they run the real mcpproxy.Service and the shared
// mcpcontract.Contract with no service, no network and no key provider.
//
// The secrets, authority and registration tables live in one memdb store, so
// the store's authority recheck and pin registration join the caller's
// transaction exactly as the PostgreSQL store's bridges join its pgx.Tx.
//
// Not modelled, deliberately: SQL, row locks and the module lock order, RLS and
// isolation levels (they stay with PostgreSQL), the Streamable HTTP protocol
// and SDK, destination DNS/TLS checks, real key unwrapping and execution/
// runtime-authority state. Nothing here may be selected under `production`.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/memdb"
)

// Clock is a controllable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)} }

// Now returns the current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- rows -------------------------------------------------------------------------

func pkey(scope domain.Scope, parts ...string) string {
	return string(scope.ProjectID) + "/" + strings.Join(parts, "/")
}

type bindingRow struct {
	Scope         domain.Scope
	Run           domain.RunID
	Server        mcpproxy.RegistrationID
	ServerVersion domain.Version
	Secret        mcpproxy.SecretRef
}

type callRow struct {
	Scope domain.Scope
	Run   domain.RunID
	Calls int64
}

// AuditRow is one recorded audit entry: identifiers and classified outcomes
// only, never arguments, results or credentials.
type AuditRow struct {
	ID        string
	Scope     domain.Scope
	ActorType string
	ActorID   string
	Action    string
	Resource  string
	RequestID string
	Meta      map[string]any
}

type attemptRow struct {
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	Session string
	Token   string
	// Ended: the attempt's grant ended (cancelled, finished or revoked).
	Ended bool
}

type secretRow struct {
	Scope   domain.Scope
	Name    string
	Kind    string
	Deleted bool
	Current int64
}

type versionRow struct {
	Scope     domain.Scope
	Name      string
	Version   int64
	Recipient string
	Token     string
	Revoked   bool
}

type pinRow struct {
	Scope    domain.Scope
	Run      domain.RunID
	Name     string
	Version  int64
	Released bool
}

var (
	serverTable  = memdb.NewTable("mcp_servers", func(r mcpproxy.Registration) string { return string(r.ID) })
	bindingTable = memdb.NewTable("mcp_run_bindings", func(b bindingRow) string { return string(b.Run) + "/" + string(b.Server) })
	sessionTable = memdb.NewTable("mcp_sessions", func(s mcpproxy.RemoteSession) string { return string(s.ID) })
	invTable     = memdb.NewTable("mcp_invocations", func(i mcpproxy.Invocation) string { return string(i.Session) + "/" + string(i.ID) })
	callTable    = memdb.NewTable("mcp_run_calls", func(c callRow) string { return string(c.Run) })
	auditTable   = memdb.NewTable("audit", func(a AuditRow) string { return a.ID })
	attemptTable = memdb.NewTable("attempts", func(a attemptRow) string { return string(a.Attempt) })
	secretTable  = memdb.NewTable("secrets", func(s secretRow) string { return pkey(s.Scope, s.Name) })
	versionTable = memdb.NewTable("secret_versions", func(v versionRow) string { return pkey(v.Scope, v.Name, fmt.Sprint(v.Version)) })
	pinTable     = memdb.NewTable("secret_pins", func(p pinRow) string { return pkey(p.Scope, string(p.Run), p.Name, fmt.Sprint(p.Version)) })
)

// ---- the world --------------------------------------------------------------------

// Options tune a World.
type Options struct {
	// Tune adjusts the proxy options before the service is built.
	Tune func(*mcpproxy.Options)
	// Incarnation names this process (default "inc-memory-a").
	Incarnation string
	// Approved lists the endpoints the destination policy approves. Empty
	// approves only DefaultEndpoint.
	Approved []string
}

// DefaultEndpoint is the approved remote server of the fixtures.
const DefaultEndpoint = "https://mcp.example.test/mcp"

// DefaultOrigin is the recipient a credential for DefaultEndpoint carries.
const DefaultOrigin = "https://mcp.example.test"

// Admin is the verified management principal of the fixtures.
const Admin = "u_mcp_admin"

// World is one fully wired in-memory MCP proxy.
type World struct {
	DB      *memdb.Store
	Clock   *Clock
	Store   *Store
	Service *mcpproxy.Service

	Authority *RuntimeAuthority
	Secrets   *Secrets
	Policies  *Policies
	Humans    *Humans
	Dest      *Destinations
	Remote    *Remote
	Opts      mcpproxy.Options

	cfg   mcpproxy.Config
	opt   Options
	seq   atomic.Int64
	fault atomic.Pointer[func() error]
}

// DefaultOptions are the proxy options of the fixtures.
func DefaultOptions(incarnation string) mcpproxy.Options {
	return mcpproxy.Options{
		Protocol: "2025-11-25", Incarnation: incarnation, SessionTTL: time.Hour, CallTimeout: 5 * time.Second,
		RecheckInterval: time.Hour, StaleAfter: time.Minute, MaxArgumentBytes: 4096, MaxResultBytes: 4096, MaxDiscoveryPages: 4,
	}
}

// New builds a World. The registration of the management caller is granted.
func New(o ...Options) *World {
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	if opt.Incarnation == "" {
		opt.Incarnation = "inc-memory-a"
	}
	if len(opt.Approved) == 0 {
		opt.Approved = []string{DefaultEndpoint}
	}
	w := &World{Clock: NewClock(), opt: opt}
	w.DB = memdb.New(memdb.WithCommitFault(func() error {
		if p := w.fault.Load(); p != nil {
			return (*p)()
		}
		return nil
	}))
	w.Authority = &RuntimeAuthority{w: w}
	w.Secrets = &Secrets{w: w}
	w.Policies = &Policies{}
	w.Humans = &Humans{}
	w.Dest = &Destinations{approved: opt.Approved}
	w.Remote = NewRemote()
	w.Store = NewStore(StoreConfig{DB: w.DB, Recheck: w.Authority.recheck, Pin: w.Secrets.pin, Clock: w.Clock.Now, Seq: &w.seq})
	w.Opts = DefaultOptions(opt.Incarnation)
	if opt.Tune != nil {
		opt.Tune(&w.Opts)
	}
	w.cfg = mcpproxy.Config{
		Store: w.Store, Human: w.Humans, Runtime: w.Authority, Policy: w.Policies, Credentials: &Credentials{w: w}, Remote: w.Remote,
		Destinations: w.Dest, Secrets: w.Store, Options: w.Opts, Clock: w.Clock.Now,
		NewID: func(prefix string) (string, error) { return fmt.Sprintf("%s%016d", prefix, w.seq.Add(1)), nil },
	}
	svc, err := mcpproxy.NewService(w.cfg)
	if err != nil {
		panic(err)
	}
	w.Service = svc
	return w
}

// Config returns the service configuration, so a test can compose a variant.
func (w *World) Config() mcpproxy.Config { return w.cfg }

// Restart builds another service over the same durable records, remote and
// clock, as a restarted process with another incarnation would. Live handles of
// the old service are not carried over.
func (w *World) Restart(incarnation string) *mcpproxy.Service {
	cfg := w.cfg
	cfg.Options.Incarnation = incarnation
	svc, err := mcpproxy.NewService(cfg)
	if err != nil {
		panic(err)
	}
	return svc
}

// SetCommitFault consults fn at every later memdb commit; a non-nil result
// discards that update. nil restores commits.
func (w *World) SetCommitFault(fn func() error) {
	if fn == nil {
		w.fault.Store(nil)
		return
	}
	w.fault.Store(&fn)
}

// Audit lists the audit rows of the tenant in order.
func (w *World) Audit(ctx context.Context, tenant domain.TenantID) ([]AuditRow, error) {
	var out []AuditRow
	err := w.DB.View(ctx, tenant, func(tx memdb.Tx) error {
		auditTable.Scan(tx, func(a AuditRow) bool { out = append(out, a); return true })
		return nil
	})
	return out, err
}

// Calls reports the run-wide admitted call count.
func (w *World) Calls(ctx context.Context, scope domain.Scope, run domain.RunID) (int64, error) {
	var n int64
	err := w.DB.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if c, ok := callTable.Get(tx, string(run)); ok && c.Scope == scope {
			n = c.Calls
		}
		return nil
	})
	return n, err
}

// ---- runtime authority --------------------------------------------------------------

// RuntimeAuthority is the fake current runtime authority: attempts with a
// bearer token and a grant that can end, read live at every check and through
// the caller's transaction. It satisfies mcpproxy.RuntimeAuthority.
type RuntimeAuthority struct {
	w   *World
	mu  sync.Mutex
	err error
	m   map[string]mcpproxy.Caller
}

var _ mcpproxy.RuntimeAuthority = (*RuntimeAuthority)(nil)

// Workload is one admitted attempt with its bearer and verified caller.
type Workload struct {
	Token  string
	Caller mcpproxy.Caller
}

// NewWorkload admits an attempt of the run in the scope with a fresh grant
// session and returns its bearer and verified caller.
func (a *RuntimeAuthority) NewWorkload(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID) (Workload, error) {
	n := a.w.seq.Add(1)
	row := attemptRow{Scope: scope, Run: run, Attempt: attempt, Session: fmt.Sprintf("grant-%d", n), Token: fmt.Sprintf("tok-%d-%s", n, attempt)}
	err := a.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error { return attemptTable.Put(tx, row) })
	if err != nil {
		return Workload{}, err
	}
	caller := mcpproxy.Caller{Scope: scope, RunID: run, AttemptID: attempt, Session: row.Session}
	a.mu.Lock()
	if a.m == nil {
		a.m = map[string]mcpproxy.Caller{}
	}
	a.m[row.Token] = caller
	a.mu.Unlock()
	return Workload{Token: row.Token, Caller: caller}, nil
}

// End ends the attempt's grant (a cancelled, finished or revoked attempt).
func (a *RuntimeAuthority) End(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return a.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptTable.Get(tx, string(attempt))
		if !ok || row.Scope != scope {
			return mcpproxy.ErrNotFound
		}
		row.Ended = true
		return attemptTable.Put(tx, row)
	})
}

// Restore reinstates an ended grant.
func (a *RuntimeAuthority) Restore(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return a.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptTable.Get(tx, string(attempt))
		if !ok || row.Scope != scope {
			return mcpproxy.ErrNotFound
		}
		row.Ended = false
		return attemptTable.Put(tx, row)
	})
}

// Fail makes every authority check an outage with err; nil restores service.
func (a *RuntimeAuthority) Fail(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

func (a *RuntimeAuthority) outage() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func tupleOf(r attemptRow) mcpproxy.Caller {
	return mcpproxy.Caller{Scope: r.Scope, RunID: r.Run, AttemptID: r.Attempt, Session: r.Session}
}

// recheck is the in-transaction authority check of the store: an outage is an
// ordinary error, an ended or unknown grant is a denial, and the whole caller
// tuple must be the one the grant was issued to.
func (a *RuntimeAuthority) recheck(_ context.Context, tx memdb.Tx, c mcpproxy.Caller, _ time.Time) error {
	if err := a.outage(); err != nil {
		return err
	}
	row, ok := attemptTable.Get(tx, string(c.AttemptID))
	if !ok || tupleOf(row) != c {
		return mcpproxy.ErrUnauthenticated
	}
	if row.Ended {
		return mcpproxy.ErrDenied
	}
	return nil
}

// Authorize implements mcpproxy.RuntimeAuthority for the tool.invoke action: an
// unknown bearer, or one issued for another attempt, is unauthenticated; an
// ended grant is a denial. Identifiers come from the grant, never the request.
func (a *RuntimeAuthority) Authorize(ctx context.Context, bearer string, attempt domain.AttemptID) (mcpproxy.Caller, error) {
	if err := a.outage(); err != nil {
		return mcpproxy.Caller{}, err
	}
	a.mu.Lock()
	c, ok := a.m[bearer]
	a.mu.Unlock()
	if !ok || c.AttemptID != attempt {
		return mcpproxy.Caller{}, mcpproxy.ErrUnauthenticated
	}
	err := a.w.DB.View(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptTable.Get(tx, string(attempt))
		switch {
		case !ok || tupleOf(row) != c:
			return mcpproxy.ErrUnauthenticated
		case row.Ended:
			return mcpproxy.ErrDenied
		}
		return nil
	})
	if err != nil {
		return mcpproxy.Caller{}, err
	}
	return c, nil
}

// ---- secrets ------------------------------------------------------------------------

// Secrets is the fake secrets module: MCP-kind secrets with exact versions that
// can be rotated, revoked and deleted, and run pins that can be released.
type Secrets struct{ w *World }

// Create stores a new MCP-kind secret with version 1: a bearer addressed to the
// recipient.
func (s *Secrets) Create(ctx context.Context, scope domain.Scope, name, recipient, token string) (mcpproxy.SecretRef, error) {
	err := s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, ok := secretTable.Get(tx, pkey(scope, name)); ok {
			return mcpproxy.ErrConflict
		}
		if err := secretTable.Put(tx, secretRow{Scope: scope, Name: name, Kind: "mcp", Current: 1}); err != nil {
			return err
		}
		return versionTable.Put(tx, versionRow{Scope: scope, Name: name, Version: 1, Recipient: recipient, Token: token})
	})
	return mcpproxy.SecretRef{Name: name, Version: 1}, err
}

// CreateKind stores a secret of another kind (never usable by the proxy).
func (s *Secrets) CreateKind(ctx context.Context, scope domain.Scope, name, kind string) (mcpproxy.SecretRef, error) {
	err := s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if err := secretTable.Put(tx, secretRow{Scope: scope, Name: name, Kind: kind, Current: 1}); err != nil {
			return err
		}
		return versionTable.Put(tx, versionRow{Scope: scope, Name: name, Version: 1, Recipient: DefaultOrigin, Token: "x"})
	})
	return mcpproxy.SecretRef{Name: name, Version: 1}, err
}

// Rotate adds a version and returns its exact reference.
func (s *Secrets) Rotate(ctx context.Context, scope domain.Scope, name, recipient, token string) (mcpproxy.SecretRef, error) {
	var ref mcpproxy.SecretRef
	err := s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := secretTable.Get(tx, pkey(scope, name))
		if !ok {
			return mcpproxy.ErrNotFound
		}
		row.Current++
		if err := secretTable.Put(tx, row); err != nil {
			return err
		}
		ref = mcpproxy.SecretRef{Name: name, Version: row.Current}
		return versionTable.Put(tx, versionRow{Scope: scope, Name: name, Version: row.Current, Recipient: recipient, Token: token})
	})
	return ref, err
}

// Revoke revokes one exact version.
func (s *Secrets) Revoke(ctx context.Context, scope domain.Scope, ref mcpproxy.SecretRef) error {
	return s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		v, ok := versionTable.Get(tx, pkey(scope, ref.Name, fmt.Sprint(ref.Version)))
		if !ok {
			return mcpproxy.ErrNotFound
		}
		v.Revoked = true
		return versionTable.Put(tx, v)
	})
}

// Delete tombstones the secret: new pins are closed, existing ones keep their
// version available to their consumers.
func (s *Secrets) Delete(ctx context.Context, scope domain.Scope, name string) error {
	return s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := secretTable.Get(tx, pkey(scope, name))
		if !ok {
			return mcpproxy.ErrNotFound
		}
		row.Deleted = true
		return secretTable.Put(tx, row)
	})
}

// ReleasePins releases every pin of the run (the run ended).
func (s *Secrets) ReleasePins(ctx context.Context, scope domain.Scope, run domain.RunID) error {
	return s.w.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var rows []pinRow
		pinTable.Scan(tx, func(p pinRow) bool {
			if p.Scope == scope && p.Run == run {
				rows = append(rows, p)
			}
			return true
		})
		for _, p := range rows {
			p.Released = true
			if err := pinTable.Put(tx, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// Pins counts the unreleased pins of the run.
func (s *Secrets) Pins(ctx context.Context, scope domain.Scope, run domain.RunID) (int, error) {
	n := 0
	err := s.w.DB.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		pinTable.Scan(tx, func(p pinRow) bool {
			if p.Scope == scope && p.Run == run && !p.Released {
				n++
			}
			return true
		})
		return nil
	})
	return n, err
}

// pin registers, in the caller's transaction, that the run retains the exact
// version. A deleted, revoked, foreign or non-MCP secret cannot be newly pinned.
func (s *Secrets) pin(_ context.Context, tx memdb.Tx, scope domain.Scope, run domain.RunID, name string, version int64) error {
	sec, ok := secretTable.Get(tx, pkey(scope, name))
	if !ok || sec.Kind != "mcp" || sec.Deleted {
		return mcpproxy.ErrCredential
	}
	v, ok := versionTable.Get(tx, pkey(scope, name, fmt.Sprint(version)))
	if !ok || v.Revoked {
		return mcpproxy.ErrCredential
	}
	return pinTable.Put(tx, pinRow{Scope: scope, Run: run, Name: name, Version: version})
}

// check is the registration-time secret check (metadata only).
func checkSecret(tx memdb.Tx, scope domain.Scope, ref mcpproxy.SecretRef) error {
	sec, ok := secretTable.Get(tx, pkey(scope, ref.Name))
	if !ok || sec.Kind != "mcp" || sec.Deleted {
		return mcpproxy.ErrCredential
	}
	v, ok := versionTable.Get(tx, pkey(scope, ref.Name, fmt.Sprint(ref.Version)))
	if !ok || v.Revoked {
		return mcpproxy.ErrCredential
	}
	return nil
}

// Credentials opens the exact frozen credential of a run binding. It satisfies
// mcpproxy.CredentialSource.
type Credentials struct{ w *World }

type credential struct {
	bearer    []byte
	recipient string
}

func (c *credential) Bearer() []byte    { return c.bearer }
func (c *credential) Recipient() string { return c.recipient }
func (c *credential) Discard() {
	for i := range c.bearer {
		c.bearer[i] = 0
	}
}

// Open implements mcpproxy.CredentialSource: ErrCredential for a missing,
// revoked, foreign or unpinned version; ErrBindingInvalid when the registration
// moved on from what the run froze.
func (c *Credentials) Open(ctx context.Context, req mcpproxy.CredentialRequest) (mcpproxy.Credential, error) {
	ref, scope, run := req.Registration.Secret, req.Caller.Scope, req.Caller.RunID
	var out *credential
	err := c.w.DB.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		b, ok := bindingTable.Get(tx, string(run)+"/"+string(req.Registration.ID))
		if !ok || b.Scope != scope || b.ServerVersion != req.Registration.Version || b.Secret != ref {
			return mcpproxy.ErrBindingInvalid
		}
		if err := checkVersionUse(tx, scope, run, ref); err != nil {
			return err
		}
		v, _ := versionTable.Get(tx, pkey(scope, ref.Name, fmt.Sprint(ref.Version)))
		out = &credential{bearer: []byte(v.Token), recipient: v.Recipient}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkVersionUse: the exact version exists, is not revoked and is still pinned
// (unreleased) by the run. Deleting the secret alone does not end use.
func checkVersionUse(tx memdb.Tx, scope domain.Scope, run domain.RunID, ref mcpproxy.SecretRef) error {
	if _, ok := secretTable.Get(tx, pkey(scope, ref.Name)); !ok {
		return mcpproxy.ErrCredential
	}
	v, ok := versionTable.Get(tx, pkey(scope, ref.Name, fmt.Sprint(ref.Version)))
	if !ok || v.Revoked {
		return mcpproxy.ErrCredential
	}
	p, ok := pinTable.Get(tx, pkey(scope, string(run), ref.Name, fmt.Sprint(ref.Version)))
	if !ok || p.Released {
		return mcpproxy.ErrCredential
	}
	return nil
}

// ---- policy, humans, destinations ---------------------------------------------------

// Policies is a settable stand-in for the pinned-profile policy reader.
type Policies struct {
	mu sync.Mutex
	m  map[domain.AttemptID]mcpproxy.ToolPolicy
}

// Set pins the policy of an attempt.
func (p *Policies) Set(a domain.AttemptID, tools []string, max *int64, record bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[domain.AttemptID]mcpproxy.ToolPolicy{}
	}
	p.m[a] = mcpproxy.NewToolPolicy(tools, max, record)
}

// PolicyOf implements mcpproxy.AttemptPolicy: no pinned policy is no tool.
func (p *Policies) PolicyOf(_ context.Context, c mcpproxy.Caller) (mcpproxy.ToolPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pol, ok := p.m[c.AttemptID]
	if !ok {
		return mcpproxy.ToolPolicy{}, mcpproxy.ErrNotFound
	}
	return pol, nil
}

// Humans is a settable stand-in for the identity authority.
type Humans struct {
	mu     sync.Mutex
	grants map[string]bool
}

// Grant lets a principal use a permission in a scope.
func (h *Humans) Grant(principal string, scope domain.Scope, perms ...mcpproxy.Permission) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants == nil {
		h.grants = map[string]bool{}
	}
	for _, p := range perms {
		h.grants[principal+"|"+scope.String()+"|"+string(p)] = true
	}
}

// Revoke removes the principal's permissions in the scope.
func (h *Humans) Revoke(principal string, scope domain.Scope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.grants {
		if strings.HasPrefix(k, principal+"|"+scope.String()+"|") {
			delete(h.grants, k)
		}
	}
}

// Authorize implements mcpproxy.HumanAuthority: an unbound principal is not
// found, as are principals of other scopes.
func (h *Humans) Authorize(_ context.Context, principal string, scope domain.Scope, perm mcpproxy.Permission) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[principal+"|"+scope.String()+"|"+string(perm)] {
		return nil
	}
	return mcpproxy.ErrNotFound
}

// Destinations is the installation's approved-destination policy: exact
// normalized endpoints.
type Destinations struct{ approved []string }

// Approve implements mcpproxy.Destinations.
func (d *Destinations) Approve(endpoint string) error {
	for _, a := range d.approved {
		if a == endpoint {
			return nil
		}
	}
	return mcpproxy.ErrDestination
}

var errOutage = errors.New("memory: injected outage")

// ErrOutage is the error the fixtures inject for a dependency outage.
func ErrOutage() error { return errOutage }
