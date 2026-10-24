// Package memory is the development-only, infrastructure-free adapter set of
// the gate module (SD-12, SD-13): a gates.Store over the memdb kernel, with
// fake current authority, evidence, candidate and deployment ports and a small
// in-memory execution peer. Together they let the real gates.Service and the
// real gate entity run the shared gatestest.Contract with no service.
//
// The execution peer holds run aggregates in a memdb table and applies the real
// execution gate mutations (BindGateMutation, CompleteGateMutation,
// RejectGateMutation) to a copy of the stored aggregate inside the same memdb
// transaction as the gate rows, the counterpart of the PostgreSQL store passing
// its pgx.Tx to execution.UpdateInTx. Its Env refuses every capacity, writer
// and authority call, so a test that reaches one proves a gate consumed
// executor capacity.
//
// Not modelled, deliberately: SQL, row locks and lock order, RLS and
// isolation levels (they stay with PostgreSQL and the T-073 checkpoint),
// execution's claim-time guard behind gates, and any production wiring. Nothing
// here may be selected under `production`.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/memdb"
)

// ReportOutput is the declared output name of the conformance report.
const ReportOutput = "conformance"

// Repository is the repository every fixture artifact and candidate is
// verified in.
const Repository = "https://git.example.test/acme/inventory.git"

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

func projectKey(scope domain.Scope, id string) string { return string(scope.ProjectID) + "/" + id }

// ---- tables -----------------------------------------------------------------

type runRow struct{ agg *execution.Aggregate }

type workRow struct {
	Scope     domain.Scope
	ID        domain.WorkItemID
	Revision  domain.ContentRevision
	Cancelled bool
}

type artifactRow struct {
	Meta    gates.EvidenceMeta
	Data    []byte
	Removed bool
}

type deploymentRow struct{ Snap gates.DeploymentSnapshot }

type grantRow struct {
	Scope     domain.Scope
	Principal string
	Type      gates.ActorType
	Roles     map[string]bool
}

// EventRow is one recorded gate event (the in-memory counterpart of the audit
// row and outbox event the PostgreSQL store writes in the same transaction).
type EventRow struct {
	ID      string
	Type    string
	Scope   domain.Scope
	Gate    gates.ID
	Version domain.Version
	Actor   gates.Actor
	Reason  string
}

type pinRow struct {
	Scope    domain.Scope
	Artifact domain.ArtifactID
	Gate     gates.ID
}

var (
	gateTable = memdb.NewTable("gates",
		func(g gates.Gate) string { return projectKey(g.Scope, string(g.ID)) },
		func(g gates.Gate) string { return projectKey(g.Scope, string(g.RunID)+"/"+g.StepID) })
	runTable   = memdb.NewTable("runs", func(r runRow) string { return projectKey(r.agg.Run.Scope, string(r.agg.Run.ID)) })
	workTable  = memdb.NewTable("work_items", func(w workRow) string { return projectKey(w.Scope, string(w.ID)) })
	artTable   = memdb.NewTable("artifacts", func(a artifactRow) string { return projectKey(a.Meta.Scope, string(a.Meta.ID)) })
	depTable   = memdb.NewTable("deployments", func(d deploymentRow) string { return d.Snap.ID })
	grantTable = memdb.NewTable("grants", func(g grantRow) string { return projectKey(g.Scope, g.Principal) })
	eventTable = memdb.NewTable("events", func(e EventRow) string { return e.ID })
	pinTable   = memdb.NewTable("pins", func(p pinRow) string {
		return projectKey(p.Scope, string(p.Gate)+"/"+string(p.Artifact))
	})
)

// ---- environment ------------------------------------------------------------

// Options tune an Env.
type Options struct {
	// GateTTL is the gate deadline duration (default one hour).
	GateTTL time.Duration
	// Batch bounds each reconciliation page (default 10).
	Batch int
	// CommitFault, when non-nil, is consulted at every memdb commit: a non-nil
	// result discards the whole update.
	CommitFault func() error
}

// Env is one fully wired in-memory gate module.
type Env struct {
	DB    *memdb.Store
	Clock *Clock
	Store *Store
	Svc   *gates.Service
	// Authority is the fake current authority (ports and seeding).
	Authority *Authority
	// Deployments is the fake deployment peer.
	Deployments *Deployments
	Evidence    *Evidence
	Candidates  *Candidates
	// Runs is the in-memory execution peer's run reader.
	Runs *Runs

	opts Options
	ttl  time.Duration
	seq  atomic.Int64
	// Reservations counts every capacity, writer or authority call the gate
	// path made into the execution peer. It must stay zero: a gate waits in
	// durable records and consumes no executor capacity (PX-13).
	reservations atomic.Int64
	secrets      atomic.Int64
	fault        atomic.Pointer[func() error]
}

// New builds the environment.
func New(o ...Options) *Env {
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	if opt.GateTTL == 0 {
		opt.GateTTL = time.Hour
	}
	if opt.Batch == 0 {
		opt.Batch = 10
	}
	e := &Env{Clock: NewClock(), opts: opt, ttl: opt.GateTTL}
	e.DB = memdb.New(memdb.WithCommitFault(func() error {
		if p := e.fault.Load(); p != nil {
			return (*p)()
		}
		if opt.CommitFault != nil {
			return opt.CommitFault()
		}
		return nil
	}))
	e.Authority = &Authority{db: e.DB}
	e.Deployments = &Deployments{db: e.DB}
	e.Evidence = &Evidence{db: e.DB}
	e.Candidates = &Candidates{commits: map[string]bool{}}
	e.Runs = &Runs{db: e.DB}
	e.Store = NewStore(StoreConfig{
		DB: e.DB, Authority: e.Authority.Bind, Deployments: e.Deployments.Bind, Clock: e.Clock.Now, Env: e,
	})
	e.build()
	return e
}

// Dependencies returns the ports every service of the Env is built from, so a
// test can compose a variant (a missing optional port, a faulty peer).
func (e *Env) Dependencies() gates.Dependencies {
	return gates.Dependencies{
		Store: e.Store, Authority: e.Authority, Runs: e.Runs, Evidence: e.Evidence, Pins: &Pins{db: e.DB},
		Candidates: e.Candidates, Tenants: tenantList{}, Deployments: e.Deployments, Clock: e.Clock.Now,
		// The real mapping over the immutable run data: baseline and required
		// checks come from the producing attempt's pinned context.
		Conformance: adapters.ConformanceMapping{ReportOutput: ReportOutput},
	}
}

// ServiceOptions returns the gate service options of the Env.
func (e *Env) ServiceOptions() gates.Options {
	return gates.Options{GateTTL: e.ttl, Batch: e.opts.Batch}
}

func (e *Env) build() {
	svc, err := gates.NewService(e.Dependencies(), e.ServiceOptions())
	if err != nil {
		panic(err)
	}
	e.Svc = svc
}

// SetTenants lists the tenants ReconcileAll visits.
func (e *Env) SetTenants(ts ...domain.TenantID) {
	d := e.Dependencies()
	d.Tenants = tenantList(ts)
	svc, err := gates.NewService(d, e.ServiceOptions())
	if err != nil {
		panic(err)
	}
	e.Svc = svc
}

// FailCommits makes every later memdb commit fail with err until it is called
// with nil (a failed commit discards the whole update).
func (e *Env) FailCommits(err error) {
	if err == nil {
		e.fault.Store(nil)
		return
	}
	fn := func() error { return err }
	e.fault.Store(&fn)
}

// SetCommitFault consults fn at every later memdb commit; a non-nil result
// discards that update. nil restores commits. It lets a test fail one chosen
// commit of a multi-step use case.
func (e *Env) SetCommitFault(fn func() error) {
	if fn == nil {
		e.fault.Store(nil)
		return
	}
	e.fault.Store(&fn)
}

// Reservations reports the capacity/writer/authority calls the gate path made
// into execution; always zero for a correct gate.
func (e *Env) Reservations() int { return int(e.reservations.Load()) }

// SecretReleases reports how many times a run's secrets were released.
func (e *Env) SecretReleases() int { return int(e.secrets.Load()) }

func (e *Env) next(prefix string) string { return fmt.Sprintf("%s%d", prefix, e.seq.Add(1)) }

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// Events lists the gate events recorded for the tenant, in event-ID order.
func (e *Env) Events(ctx context.Context, tenant domain.TenantID) ([]EventRow, error) {
	var out []EventRow
	err := e.DB.View(ctx, tenant, func(tx memdb.Tx) error {
		eventTable.Scan(tx, func(r EventRow) bool { out = append(out, r); return true })
		return nil
	})
	return out, err
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---- fake current authority ---------------------------------------------------

// Authority is the fake current authority: who holds which role in a project,
// read live at every check. It is a test double and never wired in production.
type Authority struct{ db *memdb.Store }

var _ gates.Authority = (*Authority)(nil)

// Roles of the fake authority. Only the reviewer decides; any role reads.
const (
	RoleReviewer = "reviewer"
	RoleEngineer = "engineer"
	RoleAdmin    = "admin"
	RoleViewer   = "viewer"
)

// Grant gives a user the roles in the project (replacing earlier roles).
func (a *Authority) Grant(ctx context.Context, scope domain.Scope, principal string, roles ...string) error {
	return a.GrantActor(ctx, scope, principal, gates.ActorUser, roles...)
}

// GrantActor grants roles to a principal of an explicit actor type.
func (a *Authority) GrantActor(ctx context.Context, scope domain.Scope, principal string, typ gates.ActorType, roles ...string) error {
	set := map[string]bool{}
	for _, r := range roles {
		set[r] = true
	}
	return a.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return grantTable.Put(tx, grantRow{Scope: scope, Principal: principal, Type: typ, Roles: set})
	})
}

// Revoke removes every binding of the principal in the project.
func (a *Authority) Revoke(ctx context.Context, scope domain.Scope, principal string) error {
	return a.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		grantTable.Delete(tx, projectKey(scope, principal))
		return nil
	})
}

// RemoveRole drops one role and keeps the others: the member remains but may no
// longer decide.
func (a *Authority) RemoveRole(ctx context.Context, scope domain.Scope, principal, role string) error {
	return a.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		g, ok := grantTable.Get(tx, projectKey(scope, principal))
		if !ok {
			return nil
		}
		roles := map[string]bool{}
		for r := range g.Roles {
			if r != role {
				roles[r] = true
			}
		}
		g.Roles = roles
		return grantTable.Put(tx, g)
	})
}

func check(g grantRow, perm gates.Permission) error {
	switch perm {
	case gates.PermDecide:
		if g.Roles[RoleReviewer] {
			return nil
		}
		return gates.ErrDenied
	case gates.PermRead:
		if len(g.Roles) > 0 {
			return nil
		}
		return gates.ErrDenied
	}
	return gates.ErrDenied
}

// ResolveActor implements gates.Authority. A principal with no binding in the
// project is hidden, as is one bound only in another tenant.
func (a *Authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (gates.Actor, error) {
	if err := scope.Validate(); err != nil {
		return gates.Actor{}, gates.ErrHidden
	}
	var out gates.Actor
	err := a.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		g, ok := grantTable.Get(tx, projectKey(scope, principalID))
		if !ok {
			return gates.ErrHidden
		}
		out = gates.Actor{Type: g.Type, ID: principalID}
		return nil
	})
	return out, err
}

// Authorize implements gates.Authority.
func (a *Authority) Authorize(ctx context.Context, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	return a.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error { return authorizeIn(tx, actor, scope, perm) })
}

func authorizeIn(tx memdb.Tx, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	// The decision is human-only: a service, workload or operator identity is
	// refused before any binding is looked at.
	if perm == gates.PermDecide && actor.Type != gates.ActorUser {
		return gates.ErrDenied
	}
	g, ok := grantTable.Get(tx, projectKey(scope, actor.ID))
	if !ok {
		return gates.ErrHidden
	}
	return check(g, perm)
}

type txAuthority struct{ tx memdb.Tx }

func (t txAuthority) Authorize(_ context.Context, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	return authorizeIn(t.tx, actor, scope, perm)
}

// Bind is the transaction-bound check the Store takes: it reads the grant
// table through the same memdb transaction that decides or consumes.
func (a *Authority) Bind(tx memdb.Tx) gates.TxAuthority { return txAuthority{tx: tx} }

// ---- deployments --------------------------------------------------------------

// Deployments is the fake deployment peer: a controlled snapshot per
// deployment id that, like the PostgreSQL fixture, answers whatever scope it
// recorded without filtering by the requested one, so the gate's own ownership
// and candidate checks are exercised.
type Deployments struct {
	db  *memdb.Store
	mu  sync.Mutex
	err error
}

var _ gates.Deployments = (*Deployments)(nil)

// Set records a deployment of scope that records candidate and has the digest.
func (d *Deployments) Set(ctx context.Context, scope domain.Scope, id, candidate, configDigest string) error {
	return d.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return depTable.Put(tx, deploymentRow{Snap: gates.DeploymentSnapshot{ID: id, ConfigDigest: configDigest, Scope: scope, Candidate: candidate}})
	})
}

// Remove makes the deployment unknown.
func (d *Deployments) Remove(ctx context.Context, tenant domain.TenantID, id string) error {
	return d.db.Update(ctx, tenant, func(tx memdb.Tx) error { depTable.Delete(tx, id); return nil })
}

// Fail makes every read an outage with err; nil restores service.
func (d *Deployments) Fail(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

func (d *Deployments) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// Snapshot implements gates.Deployments.
func (d *Deployments) Snapshot(ctx context.Context, scope domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	if err := d.failure(); err != nil {
		return gates.DeploymentSnapshot{}, err
	}
	var out gates.DeploymentSnapshot
	err := d.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error { return snapshotIn(tx, id, &out) })
	return out, err
}

func snapshotIn(tx memdb.Tx, id string, out *gates.DeploymentSnapshot) error {
	row, ok := depTable.Get(tx, id)
	if !ok {
		return gates.ErrNotFound
	}
	*out = row.Snap
	return nil
}

type txDeployments struct {
	d  *Deployments
	tx memdb.Tx
}

func (t txDeployments) Snapshot(_ context.Context, _ domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	if err := t.d.failure(); err != nil {
		return gates.DeploymentSnapshot{}, err
	}
	var out gates.DeploymentSnapshot
	err := snapshotIn(t.tx, id, &out)
	return out, err
}

// Bind reads the deployment through the caller's transaction.
func (d *Deployments) Bind(tx memdb.Tx) gates.Deployments { return txDeployments{d: d, tx: tx} }

// ---- evidence, candidates, pins ------------------------------------------------

// Evidence serves artifact metadata and bytes from the in-memory table.
type Evidence struct{ db *memdb.Store }

var _ gates.Evidence = (*Evidence)(nil)

// Describe implements gates.Evidence.
func (e *Evidence) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (gates.EvidenceMeta, error) {
	var out gates.EvidenceMeta
	err := e.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := artTable.Get(tx, projectKey(scope, string(id)))
		if !ok {
			return gates.ErrNotFound
		}
		out = row.Meta
		out.Usable = row.Meta.Usable && !row.Removed
		return nil
	})
	return out, err
}

// Read implements gates.Evidence.
func (e *Evidence) Read(ctx context.Context, scope domain.Scope, meta gates.EvidenceMeta, max int64) ([]byte, error) {
	var out []byte
	err := e.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := artTable.Get(tx, projectKey(scope, string(meta.ID)))
		if !ok || row.Removed {
			return gates.ErrEvidence
		}
		if int64(len(row.Data)) > max || digestOf(row.Data) != meta.Digest {
			return gates.ErrEvidence
		}
		out = append([]byte(nil), row.Data...)
		return nil
	})
	return out, err
}

// Candidates verifies commits against a fixed history.
type Candidates struct {
	mu      sync.Mutex
	commits map[string]bool
	err     error
}

var _ gates.Candidates = (*Candidates)(nil)

// Allow records commits as part of the work items' retained history.
func (c *Candidates) Allow(commits ...string) {
	c.mu.Lock()
	for _, x := range commits {
		c.commits[x] = true
	}
	c.mu.Unlock()
}

// Fail makes verification an outage with err; nil restores service.
func (c *Candidates) Fail(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// Verify implements gates.Candidates.
func (c *Candidates) Verify(_ context.Context, _ domain.Scope, _ domain.WorkItemID, commit string) (gates.VerifiedSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return gates.VerifiedSource{}, c.err
	}
	if !c.commits[commit] {
		return gates.VerifiedSource{}, gates.ErrEvidence
	}
	return gates.VerifiedSource{Repository: Repository}, nil
}

// Pins records evidence pins idempotently.
type Pins struct{ db *memdb.Store }

var _ gates.Pins = (*Pins)(nil)

// Pin implements gates.Pins.
func (p *Pins) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, gate gates.ID) error {
	return p.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return pinTable.Put(tx, pinRow{Scope: scope, Artifact: id, Gate: gate})
	})
}

// PinCount reports how many artifacts the gate pinned.
func (e *Env) PinCount(ctx context.Context, scope domain.Scope, gate gates.ID) (int, error) {
	n := 0
	err := e.DB.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		pinTable.Scan(tx, func(p pinRow) bool {
			if p.Gate == gate && p.Scope == scope {
				n++
			}
			return true
		})
		return nil
	})
	return n, err
}
