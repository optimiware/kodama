package idtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/memdb"
)

// EventAppender is the transaction-local event append of the in-memory event bus
// (eventstest.Mem.Append), the counterpart of the PostgreSQL adapter's
// pgx-based appender.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// Mem is the in-memory identity.Repository, identity.Directory,
// identity.PrincipalMapper and identity.AccessReader over memdb.
//
// Every tenant-owned record (projects, bindings, authorization revisions, audit
// rows, recorded changes) lives in the kernel, so Transact is one memdb.Update:
// the identity state, the revisions, the audit rows and the membership.changed
// and project.settings_changed events appended through the event bus commit
// together or not at all, as PostgreSQL's Record writes them in its tenant
// transaction. With NewMemOn the kernel and the event bus are shared with the
// other modules, and TransactTx joins an identity mutation to a caller's own Tx.
// Users and the tenant list are global in PostgreSQL (definer functions), so they
// stay in a small registry outside the kernel; they are created outside a
// tenant unit and are never rolled back.
//
// It reproduces the observable semantics of the PostgreSQL adapter
// (ErrNotFound/ErrConflict/ErrVersionConflict, ordered pages) but none of its
// locking. It is development-only.
type Mem struct {
	kernel *memdb.Store
	events EventAppender

	mu      sync.Mutex
	users   map[identity.UserID]identity.User
	tenants map[domain.TenantID]identity.TenantSummary

	// Changes lists the changes committed through Transact, in commit order. It
	// is a convenience for tests: the tenant tables are authoritative (ChangesOf).
	Changes []identity.Change
	// FailRecord makes the next Record fail, as a failed audit or event append would.
	FailRecord error
	// Down makes every operation fail like a database outage.
	Down error
}

var (
	_ identity.Repository      = (*Mem)(nil)
	_ identity.Directory       = (*Mem)(nil)
	_ identity.PrincipalMapper = (*Mem)(nil)
	_ identity.AccessReader    = (*Mem)(nil)
)

// NewMem returns an empty store over a private kernel, with no event bus: audit
// rows are written, events are not.
func NewMem() *Mem {
	m, err := NewMemOn(memdb.New(), nil)
	if err != nil {
		panic(err) // a fresh kernel cannot be invalid
	}
	return m
}

// NewMemOn returns an empty store over a shared kernel. events may be nil; when
// set, Record appends the typed events to the same Update.
func NewMemOn(store *memdb.Store, events EventAppender) (*Mem, error) {
	if store == nil {
		return nil, errors.New("idtest: a memdb store is required")
	}
	return &Mem{kernel: store, events: events, users: map[identity.UserID]identity.User{}, tenants: map[domain.TenantID]identity.TenantSummary{}}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*Mem) DevelopmentOnly() bool { return true }

// Kernel returns the memdb kernel the tenant records live in.
func (m *Mem) Kernel() *memdb.Store { return m.kernel }

type projectRec struct{ P identity.Project }

type bindingRec struct{ B identity.Binding }

type revisionRec struct {
	User identity.UserID
	Rev  int64
}

type auditRec struct {
	ID         string
	Action     string
	Resource   string
	ResourceID string
	Version    int64
	ActorType  string
	ActorID    string
	Project    domain.ProjectID
	Metadata   map[string]any
}

type changeRec struct{ C identity.Change }

var (
	projectsT = memdb.NewTable("identity_projects",
		func(r projectRec) string { return string(r.P.Scope.ProjectID) },
		func(r projectRec) string { return r.P.Key })
	bindingsT = memdb.NewTable("identity_memberships",
		func(r bindingRec) string { return string(r.B.ID) },
		func(r bindingRec) string {
			return string(r.B.User) + "|" + string(r.B.Project) + "|" + string(r.B.Role)
		})
	revisionsT = memdb.NewTable("identity_authz_revisions", func(r revisionRec) string { return string(r.User) })
	auditT     = memdb.NewTable("identity_audit", func(r auditRec) string { return r.ID })
	changesT   = memdb.NewTable("identity_changes", func(r changeRec) string { return r.C.ID })
)

func cloneProject(p identity.Project) identity.Project {
	p.Settings = bytes.Clone(p.Settings)
	return p
}

// ---- seeding and the global registry ------------------------------------------------

// MapPrincipal implements identity.PrincipalMapper: the stable user of the exact
// verified (issuer, subject) pair, created on first login without any
// membership.
func (m *Mem) MapPrincipal(_ context.Context, p identity.Principal) (identity.UserID, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Down != nil {
		return "", identity.Unavailablef(m.Down, "memory store down")
	}
	for id, u := range m.users {
		if u.Issuer == p.Issuer && u.Subject == p.Subject {
			return id, nil
		}
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", identity.Unavailablef(err, "identifier generation failed")
	}
	id := identity.UserID("usr_" + hex.EncodeToString(raw[:]))
	m.users[id] = identity.User{ID: id, Issuer: p.Issuer, Subject: p.Subject, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	return id, nil
}

// Revision is the user's authorization revision in the tenant (0 when none).
func (m *Mem) Revision(tenant domain.TenantID, user identity.UserID) int64 {
	var rev int64
	_ = m.kernel.View(context.Background(), tenant, func(tx memdb.Tx) error {
		r, _ := revisionsT.Get(tx, string(user))
		rev = r.Rev
		return nil
	})
	return rev
}

// AddTenant, AddProject, AddUser and AddBinding seed state directly.
func (m *Mem) AddTenant(id domain.TenantID, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[id] = identity.TenantSummary{ID: id, Key: key, DisplayName: key}
}

func (m *Mem) seed(tenant domain.TenantID, fn func(tx memdb.Tx) error) {
	if err := m.kernel.Update(context.Background(), tenant, fn); err != nil {
		panic(err)
	}
}

func (m *Mem) AddProject(t domain.TenantID, id domain.ProjectID, key string) identity.Project {
	p, err := identity.NewProject(domain.Scope{TenantID: t, ProjectID: id}, key, key, nil, 0, time.Now())
	if err != nil {
		panic(err)
	}
	m.seed(t, func(tx memdb.Tx) error { return projectsT.Put(tx, projectRec{P: p}) })
	return p
}

func (m *Mem) AddUser(id identity.UserID, issuer, subject string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[id] = identity.User{ID: id, Issuer: issuer, Subject: subject, CreatedAt: time.Now().UTC()}
}

func (m *Mem) AddBinding(t domain.TenantID, id identity.BindingID, user identity.UserID, project domain.ProjectID, role identity.Role) {
	b, err := identity.Grant(id, t, user, project, role, time.Now())
	if err != nil {
		panic(err)
	}
	m.seed(t, func(tx memdb.Tx) error {
		if err := bindingsT.Put(tx, bindingRec{B: b}); err != nil {
			return err
		}
		r, _ := revisionsT.Get(tx, string(user))
		return revisionsT.Put(tx, revisionRec{User: user, Rev: r.Rev + 1})
	})
}

func (m *Mem) down() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Down != nil {
		return identity.Unavailablef(m.Down, "memory store down")
	}
	return nil
}

func (m *Mem) knownTenant(t domain.TenantID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tenants[t]
	return ok
}

func (m *Mem) knownUser(u identity.UserID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.users[u]
	return ok
}

// ---- transactions -------------------------------------------------------------------

// classify maps infrastructure failures to the module's classes, as the
// PostgreSQL adapter does: business errors pass through, anything else is an
// unavailable dependency (an outage is never access).
func classify(err error) error {
	if err == nil || identity.IsBusinessError(err) {
		return err
	}
	if errors.Is(err, events.ErrBacklogFull) {
		return identity.Unavailablef(err, "the event backlog is full")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return identity.Unavailablef(err, "the request ended before the outcome was known")
	}
	return identity.Unavailablef(err, "the store could not complete the request")
}

// Transact implements identity.Repository: one memdb.Update.
func (m *Mem) Transact(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx identity.Tx) error) error {
	if err := m.down(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return identity.Unavailablef(err, "cancelled")
	}
	if !m.knownTenant(tenant) {
		return identity.ErrHidden
	}
	var pending []identity.Change
	ran := false
	err := m.kernel.Update(ctx, tenant, func(kt memdb.Tx) error {
		pending = pending[:0]
		if err := fn(ctx, &memTx{m: m, tx: kt, tenant: tenant, pending: &pending}); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		// The function and every write succeeded, so the failure is the commit's.
		return identity.Unavailablef(err, "the outcome of the write is unknown; reread before retrying")
	}
	if err != nil {
		return classify(err)
	}
	m.mu.Lock()
	m.Changes = append(m.Changes, pending...)
	m.mu.Unlock()
	return nil
}

// TransactTx runs an identity mutation inside the caller's Tx, the counterpart of
// the PostgreSQL adapter's transaction-bound seams: the identity state, the
// revisions, the audit rows and the events commit or roll back with the caller's
// own writes. It never opens an Update. Changes (the convenience list) is not
// updated; read ChangesOf after the caller committed.
func (m *Mem) TransactTx(ctx context.Context, tx memdb.Tx, fn func(ctx context.Context, tx identity.Tx) error) error {
	if err := m.down(); err != nil {
		return err
	}
	if !m.knownTenant(tx.Tenant()) {
		return identity.ErrHidden
	}
	var pending []identity.Change
	return classify(fn(ctx, &memTx{m: m, tx: tx, tenant: tx.Tenant(), pending: &pending}))
}

// ChangesOf lists the changes committed in the tenant, in ID order.
func (m *Mem) ChangesOf(ctx context.Context, tenant domain.TenantID) ([]identity.Change, error) {
	var out []identity.Change
	err := m.kernel.View(ctx, tenant, func(tx memdb.Tx) error {
		changesT.Scan(tx, func(r changeRec) bool { out = append(out, r.C); return true })
		return nil
	})
	return out, err
}

// AuditActions lists the tenant's audit actions, sorted.
func (m *Mem) AuditActions(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	var out []string
	err := m.kernel.View(ctx, tenant, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool { out = append(out, r.Action); return true })
		return nil
	})
	sort.Strings(out)
	return out, err
}

// AuthorityIn is the memory twin of postgres.NewTxAuthority: it decides access
// from the bindings visible in the caller's Tx, so a revocation committed
// earlier is seen and one made in the same Tx counts at once.
func (m *Mem) AuthorityIn(tx memdb.Tx) identity.Authority { return txAuthority{tx: tx} }

type txAuthority struct{ tx memdb.Tx }

func (a txAuthority) Authorize(_ context.Context, actor identity.Actor, target identity.Target, perm identity.Permission) error {
	if err := target.TenantID.Validate(); err != nil || target.TenantID != a.tx.Tenant() {
		return identity.ErrHidden
	}
	if actor.Type != identity.ActorUser || actor.Validate() != nil {
		return identity.ErrDenied
	}
	return identity.Decide(userBindings(a.tx, identity.UserID(actor.ID)), actor, target, perm)
}

func userBindings(tx memdb.Tx, user identity.UserID) []identity.Binding {
	var out []identity.Binding
	bindingsT.Scan(tx, func(r bindingRec) bool {
		if r.B.User == user {
			out = append(out, r.B)
		}
		return true
	})
	return out
}

// ---- directory and access reads ---------------------------------------------------------

// User implements identity.Directory.
func (m *Mem) User(_ context.Context, id identity.UserID) (identity.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Down != nil {
		return identity.User{}, identity.Unavailablef(m.Down, "memory store down")
	}
	u, ok := m.users[id]
	if !ok {
		return identity.User{}, identity.ErrNotFound
	}
	return u, nil
}

// Tenants implements identity.Directory.
func (m *Mem) Tenants(ctx context.Context, id identity.UserID, after string, limit int) ([]identity.TenantSummary, error) {
	if err := m.down(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.tenants))
	for t := range m.tenants {
		ids = append(ids, string(t))
	}
	m.mu.Unlock()
	sort.Strings(ids)
	out := []identity.TenantSummary{}
	for _, t := range ids {
		if t <= after || len(out) >= limit {
			continue
		}
		var mine []identity.Binding
		if err := m.kernel.View(ctx, domain.TenantID(t), func(tx memdb.Tx) error { mine = userBindings(tx, id); return nil }); err != nil {
			return nil, identity.Unavailablef(err, "reading the tenant")
		}
		if len(mine) == 0 {
			continue
		}
		m.mu.Lock()
		sum := m.tenants[domain.TenantID(t)]
		m.mu.Unlock()
		sum.Roles = nil
		seen := map[identity.Role]bool{}
		for _, b := range mine {
			if b.TenantWide() && !seen[b.Role] {
				seen[b.Role] = true
				sum.Roles = append(sum.Roles, b.Role)
			}
		}
		slices.Sort(sum.Roles)
		out = append(out, sum)
	}
	return out, nil
}

// CurrentBindings implements identity.AccessReader.
func (m *Mem) CurrentBindings(ctx context.Context, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	m.mu.Lock()
	down := m.Down
	m.mu.Unlock()
	if down != nil {
		return nil, down
	}
	var out []identity.Binding
	err := m.kernel.View(ctx, tenant, func(tx memdb.Tx) error { out = userBindings(tx, user); return nil })
	return out, err
}

// ---- the tenant transaction ----------------------------------------------------------------

type memTx struct {
	m       *Mem
	tx      memdb.Tx
	tenant  domain.TenantID
	pending *[]identity.Change
}

var _ identity.Tx = (*memTx)(nil)

func (t *memTx) LockProject(ctx context.Context, id domain.ProjectID) (identity.Project, error) {
	return t.Project(ctx, id)
}

func (t *memTx) LockAuthorization(_ context.Context, changed, _ []identity.UserID) error {
	for _, u := range changed {
		if !t.m.knownUser(u) {
			return identity.ErrNotFound
		}
		if _, ok := revisionsT.Get(t.tx, string(u)); !ok {
			if err := revisionsT.Put(t.tx, revisionRec{User: u}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *memTx) BumpAuthorization(_ context.Context, u identity.UserID) (int64, error) {
	r, _ := revisionsT.Get(t.tx, string(u))
	r.User = u
	r.Rev++
	return r.Rev, revisionsT.Put(t.tx, r)
}

func (t *memTx) Bindings(_ context.Context, u identity.UserID) ([]identity.Binding, error) {
	return userBindings(t.tx, u), nil
}

func (t *memTx) Binding(_ context.Context, id identity.BindingID) (identity.Binding, error) {
	r, ok := bindingsT.Get(t.tx, string(id))
	if !ok {
		return identity.Binding{}, identity.ErrNotFound
	}
	return r.B, nil
}

func (t *memTx) ListBindings(_ context.Context, q identity.BindingQuery) ([]identity.Binding, error) {
	var out []identity.Binding
	bindingsT.Scan(t.tx, func(r bindingRec) bool {
		b := r.B
		switch {
		case !q.AllScopes && (b.Project == "" || !slices.Contains(q.Projects, b.Project)):
		case q.Project != "" && b.Project != q.Project:
		case q.User != "" && b.User != q.User:
		case q.After != "" && string(b.ID) <= string(q.After):
		default:
			out = append(out, b)
		}
		return true
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (t *memTx) InsertBinding(_ context.Context, b identity.Binding) error {
	if b.Tenant != t.tenant {
		return fmt.Errorf("%w: the binding belongs to another tenant", identity.ErrInvalid)
	}
	if _, ok := bindingsT.Get(t.tx, string(b.ID)); ok {
		return identity.ErrConflict
	}
	if b.Project != "" {
		if _, ok := projectsT.Get(t.tx, string(b.Project)); !ok {
			return identity.ErrNotFound
		}
	}
	if err := bindingsT.Put(t.tx, bindingRec{B: b}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return identity.ErrConflict
		}
		return err
	}
	return nil
}

func (t *memTx) UpdateBinding(_ context.Context, next identity.Binding, expected domain.Version) error {
	cur, ok := bindingsT.Get(t.tx, string(next.ID))
	if !ok || cur.B.Version != expected {
		return identity.ErrVersionConflict
	}
	// Only the role and the version change; the identity, the owner, the scope and
	// the creation time are immutable, as in the PostgreSQL UPDATE.
	cur.B.Role, cur.B.Version = next.Role, next.Version
	if err := bindingsT.Put(t.tx, bindingRec{B: cur.B}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return identity.ErrConflict
		}
		return err
	}
	return nil
}

func (t *memTx) DeleteBinding(_ context.Context, b identity.Binding) error {
	cur, ok := bindingsT.Get(t.tx, string(b.ID))
	if !ok || cur.B.Version != b.Version {
		return identity.ErrVersionConflict
	}
	bindingsT.Delete(t.tx, string(b.ID))
	return nil
}

func (t *memTx) Project(_ context.Context, id domain.ProjectID) (identity.Project, error) {
	r, ok := projectsT.Get(t.tx, string(id))
	if !ok {
		return identity.Project{}, identity.ErrNotFound
	}
	return cloneProject(r.P), nil
}

func (t *memTx) ListProjects(_ context.Context, q identity.ProjectQuery) ([]identity.Project, error) {
	var out []identity.Project
	projectsT.Scan(t.tx, func(r projectRec) bool {
		id := r.P.Scope.ProjectID
		if (!q.All && !slices.Contains(q.Only, id)) || (q.After != "" && id <= q.After) {
			return true
		}
		out = append(out, cloneProject(r.P))
		return true
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (t *memTx) InsertProject(_ context.Context, p identity.Project) error {
	if p.Scope.TenantID != t.tenant {
		return fmt.Errorf("%w: the project belongs to another tenant", identity.ErrInvalid)
	}
	if _, ok := projectsT.Get(t.tx, string(p.Scope.ProjectID)); ok {
		return identity.ErrConflict
	}
	if err := projectsT.Put(t.tx, projectRec{P: cloneProject(p)}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return identity.ErrConflict
		}
		return err
	}
	return nil
}

func (t *memTx) UpdateProject(_ context.Context, next identity.Project, expected domain.Version) error {
	cur, ok := projectsT.Get(t.tx, string(next.Scope.ProjectID))
	if !ok || cur.P.Version != expected {
		return identity.ErrVersionConflict
	}
	// The scope, the key and the creation time are immutable.
	cur.P.DisplayName, cur.P.Settings, cur.P.ActiveWIPLimit = next.DisplayName, bytes.Clone(next.Settings), next.ActiveWIPLimit
	cur.P.UpdatedAt, cur.P.Version = next.UpdatedAt, next.Version
	return projectsT.Put(t.tx, projectRec{P: cur.P})
}

// Record writes the audit rows, the typed events and the change record of the
// mutation in the same Tx as its state: one audit row per binding operation and
// per project effect, then a membership.changed event per user and a
// project.settings_changed event for the project, as postgres.Record does. A
// failure here fails the whole unit. Audit metadata carries identifiers, roles
// and versions only.
func (t *memTx) Record(_ context.Context, c identity.Change) error {
	if t.m.FailRecord != nil {
		err := t.m.FailRecord
		t.m.FailRecord = nil
		return err
	}
	if c.Tenant != t.tenant {
		return fmt.Errorf("%w: the change belongs to another tenant", identity.ErrInvalid)
	}
	if err := c.Actor.Validate(); err != nil {
		return err
	}
	actorType := string(c.Actor.Type)
	if c.Actor.Type == identity.ActorService {
		actorType = "system"
	}
	n := 0
	audit := func(project domain.ProjectID, action, resource, resourceID string, version int64, meta map[string]any) error {
		n++
		return auditT.Put(t.tx, auditRec{ID: fmt.Sprintf("aud_%s_%d", c.ID, n), Action: action, Resource: resource, ResourceID: resourceID, Version: version,
			ActorType: actorType, ActorID: c.Actor.ID, Project: project, Metadata: meta})
	}
	correlation := c.RequestID
	if correlation == "" {
		correlation = c.ID
	}
	actor := eventsv1.Actor{Type: eventsv1.ActorType(c.Actor.Type), ID: c.Actor.ID}
	appendEvent := func(env eventsv1.Envelope) error {
		if t.m.events == nil {
			return nil
		}
		_, err := t.m.events.Append(t.tx, env)
		return err
	}
	for _, bc := range c.Bindings {
		ids := make([]string, 0, len(bc.Ops))
		for _, op := range bc.Ops {
			meta := map[string]any{"userId": string(bc.User), "role": string(op.Role), "effectiveVersion": bc.EffectiveVersion}
			if op.PreviousRole != "" {
				meta["previousRole"] = string(op.PreviousRole)
			}
			if err := audit(bc.Project, string(op.Kind), "membership", string(op.ID), int64(op.Version), meta); err != nil {
				return err
			}
			ids = append(ids, string(op.ID))
		}
		data, err := json.Marshal(eventsv1.MembershipChanged{UserID: string(bc.User), BindingIDs: ids, EffectiveVersion: bc.EffectiveVersion})
		if err != nil {
			return err
		}
		if err := appendEvent(eventsv1.Envelope{
			ID: fmt.Sprintf("evt_%s_m%d", c.ID, n), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeMembershipChanged, OccurredAt: c.OccurredAt.UTC(),
			TenantID: c.Tenant, ProjectID: bc.Project,
			Resource: eventsv1.Resource{Type: "membership", ID: string(bc.User), Version: bc.EffectiveVersion},
			Actor:    actor, CorrelationID: correlation, Data: data,
		}); err != nil {
			return err
		}
	}
	if pc := c.Project; pc != nil {
		action := string(identity.ChangeProjectUpdated)
		if c.Kind == identity.ChangeProjectCreated {
			action = string(identity.ChangeProjectCreated)
		}
		if err := audit(pc.Scope.ProjectID, action, "project", string(pc.Scope.ProjectID), int64(pc.Version), map[string]any{"changed": pc.Changed}); err != nil {
			return err
		}
		data, err := json.Marshal(eventsv1.ProjectSettingsChanged{ChangedSettings: pc.Changed, Version: int64(pc.Version)})
		if err != nil {
			return err
		}
		if err := appendEvent(eventsv1.Envelope{
			ID: fmt.Sprintf("evt_%s_p", c.ID), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeProjectSettingsChanged, OccurredAt: c.OccurredAt.UTC(),
			TenantID: c.Tenant, ProjectID: pc.Scope.ProjectID,
			Resource: eventsv1.Resource{Type: "project", ID: string(pc.Scope.ProjectID), Version: int64(pc.Version)},
			Actor:    actor, CorrelationID: correlation, Data: data,
		}); err != nil {
			return err
		}
	}
	if err := changesT.Put(t.tx, changeRec{C: c}); err != nil {
		return err
	}
	*t.pending = append(*t.pending, c)
	return nil
}

// Settings returns a project's settings as a decoded map (test convenience).
func Settings(p identity.Project) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(p.Settings, &m)
	return m
}
