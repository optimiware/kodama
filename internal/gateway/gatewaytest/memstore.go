package gatewaytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/memdb"
)

// MemStore is the in-memory gateway.Store over the memdb transaction kernel
// (SD-13, SD-A11, ADR 0009). It applies the same entity transitions as the
// PostgreSQL store (ModelSettings.Apply, RunBudget, ModelRequest, Usage) inside
// one tenant transaction, so a refused or failed call writes nothing: state,
// counters, usage, audit entry and event commit together or not at all.
//
// It is development-only evidence of policy. It does not model SQL, row level
// security, locking or lock order, and it has no restart persistence: the
// obligations the PostgreSQL store and its integration tests carry stay there.
//
// Workload authority is a consumer port of the store (WorkloadCheck), exactly
// as in the PostgreSQL adapter; MemAuthority is the table fixture for it.
type MemStore struct {
	db         *memdb.Store
	workload   WorkloadCheck
	management gateway.Authority

	mu       sync.Mutex
	skipLose int
	lose     int
	skipFail int
	fail     int
	eventErr error
	seq      atomic.Int64
}

// WorkloadCheck rechecks, inside the store's transaction, that the workload
// still holds model.invoke authority. It returns gateway.ErrUnauthenticated,
// ErrForbidden or ErrUnavailable; there is no allow-all default.
type WorkloadCheck func(w gateway.Workload, now time.Time) error

// ErrFaultInjected is the cause of an injected commit fault.
var ErrFaultInjected = errors.New("injected commit fault CAUSE-SENTINEL-t083")

// NewMemStore builds an empty store. Both authorities are required.
func NewMemStore(workload WorkloadCheck, management gateway.Authority) (*MemStore, error) {
	if workload == nil || management == nil {
		return nil, errors.New("gatewaytest: a workload check and a management authority are required")
	}
	s := &MemStore{workload: workload, management: management}
	s.db = memdb.New(memdb.WithCommitFault(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case s.skipFail > 0:
			s.skipFail--
		case s.fail > 0:
			s.fail--
			return ErrFaultInjected
		}
		return nil
	}))
	return s, nil
}

var _ gateway.Store = (*MemStore)(nil)

// FailNextCommits makes the next n write transactions roll back at commit. The
// caller sees an unknown commit outcome, as it would from the network.
func (s *MemStore) FailNextCommits(n int) { s.FailCommits(0, n) }

// FailCommits lets the next skip write transactions commit and then rolls back
// the following n.
func (s *MemStore) FailCommits(skip, n int) {
	s.mu.Lock()
	s.skipFail, s.fail = skip, n
	s.mu.Unlock()
}

// LoseNextCommits lets the next skip write transactions commit normally and
// then makes n of them commit but report an error: an ambiguous commit.
func (s *MemStore) LoseNextCommits(skip, n int) {
	s.mu.Lock()
	s.skipLose, s.lose = skip, n
	s.mu.Unlock()
}

// FailEvents makes every event append fail with err (nil restores it). An
// error wrapping gateway.ErrBackpressure models a full outbox: the whole
// transaction rolls back.
func (s *MemStore) FailEvents(err error) {
	s.mu.Lock()
	s.eventErr = err
	s.mu.Unlock()
}

// ---- tables ---------------------------------------------------------------------

// AuditEntry is one audit row of the store: identifiers, numbers and codes only.
type AuditEntry struct {
	ID           string
	Scope        domain.Scope
	At           time.Time
	ActorType    string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	RequestID    string
	Metadata     map[string]any
}

// Event is one outbox row.
type Event struct {
	Seq      int64
	Envelope eventsv1.Envelope
}

type revisionRow struct {
	Project string
	Rev     int64
	Row     gateway.ModelSettings
}

type budgetRow struct {
	Project  string
	Run      string
	Admitted int64
	Charged  int64
	Binding  gateway.RunBinding
}

type pinRow struct {
	Project, Run string
	Ref          gateway.CredentialRef
}

var (
	settingsTable = newTable("model_settings", cloneSettings, func(m gateway.ModelSettings) string { return string(m.Scope.ProjectID) })
	revisionTable = newTable("model_settings_revisions", cloneRevision, func(r revisionRow) string { return fmt.Sprintf("%s/%020d", r.Project, r.Rev) })
	budgetTable   = newTable("model_run_bindings", cloneBudget, func(b budgetRow) string { return b.Project + "/" + b.Run })
	requestTable  = newTable("model_requests", cloneRequest,
		func(m gateway.ModelRequest) string { return string(m.Scope.ProjectID) + "/" + string(m.ID) },
		// The one-unresolved-request-per-run index: only a slot holder has a value.
		func(m gateway.ModelRequest) string {
			if m.State.HoldsSlot() {
				return string(m.Scope.ProjectID) + "/" + string(m.RunID)
			}
			return ""
		})
	usageTable = newTable("model_usage", cloneUsage, func(u gateway.Usage) string { return string(u.Scope.ProjectID) + "/" + string(u.RequestID) })
	pinTable   = newTable("model_run_pins", func(p pinRow) pinRow { return p }, func(p pinRow) string { return p.Project + "/" + p.Run })
	auditTable = newTable("audit", cloneAudit, func(a AuditEntry) string { return a.ID })
	eventTable = newTable("outbox", cloneEvent, func(e Event) string { return fmt.Sprintf("%020d", e.Seq) },
		// An event identity is unique: a retried settlement can never append a second.
		func(e Event) string { return e.Envelope.ID })
)

// cloned is a memdb table that copies every row on its way in and out. The
// kernel stores and returns values as given, but these rows hold slices,
// pointers and maps; copying at the persistence boundary keeps a caller's
// mutation of an input or of a returned value from changing stored policy,
// usage or history, exactly as a database row cannot be edited through a
// previously read value.
type cloned[V any] struct {
	t     *memdb.Table[V]
	clone func(V) V
}

func newTable[V any](name string, clone func(V) V, key func(V) string, uniques ...func(V) string) *cloned[V] {
	return &cloned[V]{t: memdb.NewTable(name, key, uniques...), clone: clone}
}

func (c *cloned[V]) Get(tx memdb.Tx, key string) (V, bool) {
	v, ok := c.t.Get(tx, key)
	if !ok {
		return v, false
	}
	return c.clone(v), true
}

func (c *cloned[V]) Put(tx memdb.Tx, v V) error { return c.t.Put(tx, c.clone(v)) }

func (c *cloned[V]) Scan(tx memdb.Tx, yield func(V) bool) {
	c.t.Scan(tx, func(v V) bool { return yield(c.clone(v)) })
}

func cloneTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	t := *p
	return &t
}

func cloneInt(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneSettings(s gateway.ModelSettings) gateway.ModelSettings {
	s.Models = slices.Clone(s.Models)
	return s
}

func cloneBinding(b gateway.RunBinding) gateway.RunBinding {
	b.Models = slices.Clone(b.Models)
	return b
}

func cloneRevision(r revisionRow) revisionRow {
	r.Row = cloneSettings(r.Row)
	return r
}

func cloneBudget(b budgetRow) budgetRow {
	b.Binding = cloneBinding(b.Binding)
	return b
}

func cloneRequest(m gateway.ModelRequest) gateway.ModelRequest {
	m.ForwardingAt, m.FinishedAt = cloneTime(m.ForwardingAt), cloneTime(m.FinishedAt)
	return m
}

func cloneUsage(u gateway.Usage) gateway.Usage {
	u.InputTokens, u.OutputTokens = cloneInt(u.InputTokens), cloneInt(u.OutputTokens)
	return u
}

func cloneAudit(a AuditEntry) AuditEntry {
	a.Metadata = cloneMeta(a.Metadata)
	return a
}

func cloneEvent(e Event) Event {
	e.Envelope.Data = slices.Clone(e.Envelope.Data)
	if c := e.Envelope.CausationID; c != nil {
		v := *c
		e.Envelope.CausationID = &v
	}
	return e
}

// cloneMeta copies an audit metadata map. Its values are scalars, string lists
// and nested maps of the same shapes.
func cloneMeta(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case []string:
			out[k] = slices.Clone(t)
		case map[string]any:
			out[k] = cloneMeta(t)
		default:
			out[k] = v
		}
	}
	return out
}

func rowKey(scope domain.Scope, id string) string { return string(scope.ProjectID) + "/" + id }

// ---- transactions ---------------------------------------------------------------

// atomically runs fn in one tenant transaction. A failure of the commit itself,
// after fn succeeded, has an unknown outcome and is reported as
// gateway.ErrUnknownCommit wrapped in ErrUnavailable, as the PostgreSQL store
// does.
func (s *MemStore) atomically(ctx context.Context, tenant domain.TenantID, fn func(tx memdb.Tx) error) error {
	done := false
	err := s.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err == nil {
		s.mu.Lock()
		lost := false
		switch {
		case s.skipLose > 0:
			s.skipLose--
		case s.lose > 0:
			s.lose--
			lost = true
		}
		s.mu.Unlock()
		if lost {
			return gateway.FailWith(gateway.ErrUnavailable, errors.Join(gateway.ErrUnknownCommit, ErrFaultInjected), "the commit outcome is unknown")
		}
		return nil
	}
	if done {
		return gateway.FailWith(gateway.ErrUnavailable, errors.Join(gateway.ErrUnknownCommit, err), "the commit outcome is unknown")
	}
	return classify(err)
}

func (s *MemStore) read(ctx context.Context, tenant domain.TenantID, fn func(tx memdb.Tx) error) error {
	return classify(s.db.View(ctx, tenant, fn))
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	return gateway.Classify(err)
}

func (s *MemStore) appendEvent(tx memdb.Tx, env eventsv1.Envelope) error {
	s.mu.Lock()
	injected := s.eventErr
	s.mu.Unlock()
	if injected != nil {
		if errors.Is(injected, gateway.ErrBackpressure) {
			return injected
		}
		return fmt.Errorf("gatewaytest: append event: %w", injected)
	}
	return eventTable.Put(tx, Event{Seq: s.seq.Add(1), Envelope: env})
}

func (s *MemStore) audit(tx memdb.Tx, a AuditEntry) error {
	a.ID = fmt.Sprintf("aud_%020d", s.seq.Add(1))
	return auditTable.Put(tx, a)
}

// ---- inspection -----------------------------------------------------------------

// Events returns the tenant's outbox in append order.
func (s *MemStore) Events(tenant domain.TenantID) []eventsv1.Envelope {
	var out []Event
	_ = s.db.View(context.Background(), tenant, func(tx memdb.Tx) error {
		eventTable.Scan(tx, func(e Event) bool { out = append(out, e); return true })
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	envs := make([]eventsv1.Envelope, len(out))
	for i, e := range out {
		envs[i] = e.Envelope
	}
	return envs
}

// EventsOfType counts the tenant's events of one type.
func (s *MemStore) EventsOfType(tenant domain.TenantID, typ string) int {
	n := 0
	for _, e := range s.Events(tenant) {
		if string(e.Type) == typ {
			n++
		}
	}
	return n
}

// Audits returns the tenant's audit rows in append order.
func (s *MemStore) Audits(tenant domain.TenantID) []AuditEntry {
	var out []AuditEntry
	_ = s.db.View(context.Background(), tenant, func(tx memdb.Tx) error {
		auditTable.Scan(tx, func(a AuditEntry) bool { out = append(out, a); return true })
		return nil
	})
	return out
}

// AuditsOf counts the tenant's audit rows with one action.
func (s *MemStore) AuditsOf(tenant domain.TenantID, action string) int {
	n := 0
	for _, a := range s.Audits(tenant) {
		if a.Action == action {
			n++
		}
	}
	return n
}

// Budget returns a run's admitted request count and charged tokens (zero when
// the run has no binding).
func (s *MemStore) Budget(scope domain.Scope, run domain.RunID) (admitted, charged int64) {
	_ = s.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		if b, ok := budgetTable.Get(tx, rowKey(scope, string(run))); ok {
			admitted, charged = b.Admitted, b.Charged
		}
		return nil
	})
	return admitted, charged
}

// Binding returns a run's immutable binding.
func (s *MemStore) Binding(scope domain.Scope, run domain.RunID) (gateway.RunBinding, bool) {
	var out gateway.RunBinding
	var ok bool
	_ = s.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		var b budgetRow
		if b, ok = budgetTable.Get(tx, rowKey(scope, string(run))); ok {
			out = b.Binding
		}
		return nil
	})
	return out, ok
}

// Pins returns the credential versions pinned for a run, which is at most one
// (registered once, at the run's first admission).
func (s *MemStore) Pins(scope domain.Scope, run domain.RunID) []gateway.CredentialRef {
	var out []gateway.CredentialRef
	_ = s.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		if p, ok := pinTable.Get(tx, rowKey(scope, string(run))); ok {
			out = append(out, p.Ref)
		}
		return nil
	})
	return out
}

// Revisions returns every stored settings revision of a project, oldest first.
func (s *MemStore) Revisions(scope domain.Scope) []gateway.ModelSettings {
	var out []gateway.ModelSettings
	_ = s.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		revisionTable.Scan(tx, func(r revisionRow) bool {
			if r.Project == string(scope.ProjectID) {
				out = append(out, r.Row)
			}
			return true
		})
		return nil
	})
	return out
}

// Requests lists a run's requests in admission order.
func (s *MemStore) Requests(scope domain.Scope, run domain.RunID) []gateway.ModelRequest {
	var out []gateway.ModelRequest
	_ = s.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		out = runRequests(tx, scope, run)
		return nil
	})
	return out
}

func runRequests(tx memdb.Tx, scope domain.Scope, run domain.RunID) []gateway.ModelRequest {
	var out []gateway.ModelRequest
	requestTable.Scan(tx, func(m gateway.ModelRequest) bool {
		if m.Scope == scope && m.RunID == run {
			out = append(out, m)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// ---- settings -------------------------------------------------------------------

func loadSettings(tx memdb.Tx, scope domain.Scope) gateway.ModelSettings {
	if s, ok := settingsTable.Get(tx, string(scope.ProjectID)); ok {
		return s
	}
	return gateway.Disabled(scope)
}

// Settings implements gateway.Store.
func (s *MemStore) Settings(ctx context.Context, scope domain.Scope) (gateway.ModelSettings, error) {
	if err := scope.Validate(); err != nil {
		return gateway.ModelSettings{}, gateway.Failf(gateway.ErrInvalid, "the scope is malformed")
	}
	var out gateway.ModelSettings
	err := s.read(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = loadSettings(tx, scope)
		return nil
	})
	return out, err
}

// UpdateSettings implements gateway.Store.
func (s *MemStore) UpdateSettings(ctx context.Context, cmd gateway.UpdateSettings) (gateway.SettingsResult, error) {
	if err := cmd.Scope.Validate(); err != nil {
		return gateway.SettingsResult{}, gateway.Failf(gateway.ErrInvalid, "the scope is malformed")
	}
	var res gateway.SettingsResult
	err := s.atomically(ctx, cmd.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.management.Authorize(ctx, cmd.Actor, cmd.Scope, gateway.PermSettings); err != nil {
			return err
		}
		cur := loadSettings(tx, cmd.Scope)
		next, changed, err := cur.Apply(cmd.Expected, cmd.Edit, cmd.Actor.ID, cmd.Now)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			res = gateway.SettingsResult{Settings: cur}
			return nil
		}
		if err := revisionTable.Put(tx, revisionRow{Project: string(next.Scope.ProjectID), Rev: next.Revision, Row: next}); err != nil {
			return versionConflict(err)
		}
		if err := settingsTable.Put(tx, next); err != nil {
			return versionConflict(err)
		}
		meta := map[string]any{
			"changed": changed, "enabled": next.Enabled, "origin": next.Endpoint.Origin(), "credentialRef": next.Credential.Ref,
			"credentialVersion": next.Credential.Version, "models": next.Models, "recording": next.Recording,
		}
		if err := s.audit(tx, AuditEntry{
			Scope: next.Scope, At: next.UpdatedAt, ActorType: "user", ActorID: cmd.Actor.ID, Action: "model_settings.update",
			ResourceType: "model_settings", ResourceID: string(next.Scope.ProjectID), RequestID: cmd.RequestID, Metadata: meta,
		}); err != nil {
			return err
		}
		data, err := json.Marshal(eventsv1.ProjectSettingsChanged{ChangedSettings: changed, Version: next.Revision})
		if err != nil {
			return err
		}
		id := fmt.Sprintf("evt_%020d", s.seq.Add(1))
		corr := cmd.RequestID
		if corr == "" {
			corr = id
		}
		if err := s.appendEvent(tx, eventsv1.Envelope{
			ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeProjectSettingsChanged, OccurredAt: next.UpdatedAt,
			TenantID: next.Scope.TenantID, ProjectID: next.Scope.ProjectID,
			Resource: eventsv1.Resource{Type: "project", ID: string(next.Scope.ProjectID), Version: next.Revision},
			Actor:    eventsv1.Actor{Type: eventsv1.ActorUser, ID: cmd.Actor.ID}, CorrelationID: corr, Data: data,
		}); err != nil {
			return err
		}
		res = gateway.SettingsResult{Settings: next, Changed: true}
		return nil
	})
	if err != nil {
		return gateway.SettingsResult{}, err
	}
	return res, nil
}

func versionConflict(err error) error {
	if errors.Is(err, domain.ErrConflict) {
		return gateway.Failf(gateway.ErrVersionConflict, "the settings changed; reload and retry")
	}
	return err
}

// ---- binding and budget ---------------------------------------------------------

func loadBudget(tx memdb.Tx, scope domain.Scope, run domain.RunID) (gateway.RunBudget, bool) {
	row, ok := budgetTable.Get(tx, rowKey(scope, string(run)))
	if !ok {
		return gateway.RunBudget{}, false
	}
	b := gateway.RunBudget{Binding: row.Binding, Admitted: row.Admitted, Charged: row.Charged}
	for _, m := range runRequests(tx, scope, run) {
		if m.State.HoldsSlot() {
			b.Unresolved = m.ID
		}
	}
	return b, true
}

func saveBudget(tx memdb.Tx, b gateway.RunBudget) error {
	sc, run := b.Binding.Scope, string(b.Binding.RunID)
	return budgetTable.Put(tx, budgetRow{Project: string(sc.ProjectID), Run: run, Admitted: b.Admitted, Charged: b.Charged, Binding: b.Binding})
}

// bindRun returns the run's budget, creating the immutable binding from the
// project's current enabled settings at the run's first admission and
// registering the exact credential version pin in the same transaction.
func bindRun(tx memdb.Tx, scope domain.Scope, run domain.RunID, now time.Time) (gateway.RunBudget, error) {
	if b, ok := loadBudget(tx, scope, run); ok {
		return b, nil
	}
	binding, err := gateway.BindRun(loadSettings(tx, scope), run, now)
	if err != nil {
		return gateway.RunBudget{}, err
	}
	b := gateway.RunBudget{Binding: binding}
	if err := saveBudget(tx, b); err != nil {
		return gateway.RunBudget{}, err
	}
	if err := pinTable.Put(tx, pinRow{Project: string(scope.ProjectID), Run: string(run), Ref: binding.Credential}); err != nil {
		return gateway.RunBudget{}, err
	}
	return b, nil
}

// ---- requests -------------------------------------------------------------------

func loadRequest(tx memdb.Tx, scope domain.Scope, id gateway.RequestID) (gateway.ModelRequest, error) {
	m, ok := requestTable.Get(tx, rowKey(scope, string(id)))
	if !ok || m.Scope != scope {
		return gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the request does not exist in this scope")
	}
	return m, nil
}

func saveRequest(tx memdb.Tx, m gateway.ModelRequest) error {
	if err := requestTable.Put(tx, m); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return gateway.FailWith(gateway.ErrBusy, err, "another model request of this run is unresolved")
		}
		return err
	}
	return nil
}

func owned(m gateway.ModelRequest, w gateway.Workload) error {
	if m.Scope != w.Scope || m.RunID != w.RunID || m.AttemptID != w.AttemptID {
		return gateway.Failf(gateway.ErrNotFound, "the request does not belong to this attempt")
	}
	return nil
}

// Admit implements gateway.Store.
func (s *MemStore) Admit(ctx context.Context, cmd gateway.AdmitCommand) (gateway.Admission, error) {
	w := cmd.Workload
	if err := w.Validate(); err != nil {
		return gateway.Admission{}, err
	}
	if err := cmd.RequestID.Validate(); err != nil {
		return gateway.Admission{}, err
	}
	var out gateway.Admission
	err := s.atomically(ctx, w.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.workload(w, cmd.Now); err != nil {
			return err
		}
		// Admission only inserts: a request identity that exists in the project,
		// whatever its run or state, is refused before anything changes, so it can
		// neither replace another run's request (and its slot) nor be reused after
		// its usage was recorded. This is the primary key of the PostgreSQL table.
		if _, taken := requestTable.Get(tx, rowKey(w.Scope, string(cmd.RequestID))); taken {
			return gateway.Failf(gateway.ErrInvalid, "the request identifier is already in use")
		}
		budget, err := bindRun(tx, w.Scope, w.RunID, cmd.Now)
		if err != nil {
			return err
		}
		current := loadSettings(tx, w.Scope)
		if !current.Enabled {
			return gateway.Failf(gateway.ErrNotConfigured, "model access is not enabled for this project")
		}
		if !budget.Binding.Allows(cmd.Model) {
			return gateway.Failf(gateway.ErrModelNotAllowed, "the model is not on the run's allow-list")
		}
		outputMax := cmd.OutputMax
		if outputMax == 0 {
			outputMax = budget.Binding.Limits.MaxOutputTokens
		}
		if _, err := budget.Admit(cmd.RequestID, cmd.RequestBytes, cmd.InputBound, outputMax); err != nil {
			return err
		}
		recording := cmd.RecordingAvailable && budget.Binding.Recording && current.Recording
		req, err := gateway.NewModelRequest(cmd.RequestID, w, budget.Admitted, cmd.Model, cmd.InputBound, outputMax, recording, cmd.Now)
		if err != nil {
			return err
		}
		if err := saveRequest(tx, req); err != nil {
			return err
		}
		if err := saveBudget(tx, budget); err != nil {
			return err
		}
		out = gateway.Admission{Request: req, Binding: budget.Binding, Recording: recording}
		return nil
	})
	if err != nil {
		return gateway.Admission{}, err
	}
	return out, nil
}

// BeginForwarding implements gateway.Store.
func (s *MemStore) BeginForwarding(ctx context.Context, w gateway.Workload, id gateway.RequestID, now time.Time) error {
	return s.atomically(ctx, w.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.workload(w, now); err != nil {
			return err
		}
		m, err := loadRequest(tx, w.Scope, id)
		if err != nil {
			return err
		}
		if err := owned(m, w); err != nil {
			return err
		}
		if err := m.BeginForwarding(now); err != nil {
			return err
		}
		return saveRequest(tx, m)
	})
}

// Heartbeat implements gateway.Store.
func (s *MemStore) Heartbeat(ctx context.Context, w gateway.Workload, id gateway.RequestID, now time.Time) error {
	return s.atomically(ctx, w.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.workload(w, now); err != nil {
			return err
		}
		m, err := loadRequest(tx, w.Scope, id)
		if err != nil {
			return err
		}
		if err := owned(m, w); err != nil {
			return err
		}
		if err := m.Beat(now); err != nil {
			return err
		}
		return saveRequest(tx, m)
	})
}

// Request implements gateway.Store.
func (s *MemStore) Request(ctx context.Context, scope domain.Scope, id gateway.RequestID) (gateway.ModelRequest, error) {
	var out gateway.ModelRequest
	err := s.read(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = loadRequest(tx, scope, id)
		return err
	})
	return out, err
}

// Usage implements gateway.Store.
func (s *MemStore) Usage(ctx context.Context, scope domain.Scope, id gateway.RequestID) (gateway.Usage, error) {
	var out gateway.Usage
	err := s.read(ctx, scope.TenantID, func(tx memdb.Tx) error {
		u, ok := usageTable.Get(tx, rowKey(scope, string(id)))
		if !ok {
			return gateway.Failf(gateway.ErrNotFound, "no usage is recorded for this request")
		}
		out = u
		return nil
	})
	return out, err
}

// RecordingOutcome implements gateway.Store.
func (s *MemStore) RecordingOutcome(ctx context.Context, scope domain.Scope, id gateway.RequestID, recorded bool) error {
	outcome := "failed"
	if recorded {
		outcome = "recorded"
	}
	return s.atomically(ctx, scope.TenantID, func(tx memdb.Tx) error {
		m, err := loadRequest(tx, scope, id)
		if err != nil || !m.Recording || m.RecordingOutcome != "" {
			return gateway.Failf(gateway.ErrSuperseded, "the recording outcome was not stored")
		}
		m.RecordingOutcome = outcome
		return saveRequest(tx, m)
	})
}

// Abandoned implements gateway.Store.
func (s *MemStore) Abandoned(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) ([]gateway.ModelRequest, error) {
	if limit < 1 || limit > 1000 {
		return nil, gateway.Failf(gateway.ErrInvalid, "the limit is out of range")
	}
	var out []gateway.ModelRequest
	err := s.read(ctx, tenant, func(tx memdb.Tx) error {
		requestTable.Scan(tx, func(m gateway.ModelRequest) bool {
			if m.InFlight() && m.HeartbeatAt.Before(before) {
				out = append(out, m)
			}
			return true
		})
		return nil
	})
	slices.SortFunc(out, func(a, b gateway.ModelRequest) int {
		if c := a.HeartbeatAt.Compare(b.HeartbeatAt); c != 0 {
			return c
		}
		return compareString(string(a.ID), string(b.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---- settlement -----------------------------------------------------------------

type settler struct {
	reason    string
	eventType eventsv1.Actor
	auditType string
}

func workloadSettler(attempt domain.AttemptID, reason string) settler {
	return settler{reason: reason, eventType: eventsv1.Actor{Type: eventsv1.ActorWorkload, ID: string(attempt)}, auditType: "workload"}
}

func reconcilerSettler(reason string) settler {
	return settler{reason: reason, eventType: eventsv1.Actor{Type: eventsv1.ActorService, ID: "model-gateway"}, auditType: "system"}
}

func (s *MemStore) auditUsage(tx memdb.Tx, m gateway.ModelRequest, u gateway.Usage, who settler, b gateway.RunBudget) error {
	meta := map[string]any{
		"runId": string(u.RunID), "attemptId": string(u.AttemptID), "requestedModel": u.RequestedModel, "modelConfirmed": u.ModelConfirmed,
		"origin": u.Destination.Origin, "basis": string(u.Basis), "uncertainty": u.Uncertainty, "outcome": string(u.Result), "code": u.Code,
		"charged": u.Charged, "runCharged": b.Charged, "runRequests": b.Admitted, "recording": m.Recording,
	}
	if u.ModelConfirmed {
		meta["actualModel"] = u.ActualModel
	}
	return s.audit(tx, AuditEntry{
		Scope: u.Scope, At: u.RecordedAt, ActorType: who.auditType, ActorID: who.eventType.ID, Action: "model.request." + who.reason,
		ResourceType: "model_request", ResourceID: string(u.RequestID), Metadata: meta,
	})
}

// settle persists one request's settlement in the caller's transaction: the
// request, the budget counters, the usage record, the audit entry and the
// usage.recorded event, whose identity derives from the request.
func (s *MemStore) settle(tx memdb.Tx, b gateway.RunBudget, m gateway.ModelRequest, u gateway.Usage, release bool, who settler) error {
	if err := b.Settle(m.ID, u.Charged, release); err != nil {
		return err
	}
	if err := saveRequest(tx, m); err != nil {
		return err
	}
	if err := saveBudget(tx, b); err != nil {
		return err
	}
	if _, exists := usageTable.Get(tx, rowKey(u.Scope, string(u.RequestID))); exists {
		return gateway.Failf(gateway.ErrSuperseded, "the request is already settled")
	}
	if err := usageTable.Put(tx, u); err != nil {
		return err
	}
	if err := s.auditUsage(tx, m, u, who, b); err != nil {
		return err
	}
	data, err := json.Marshal(eventsv1.UsageRecorded{
		RunID: string(u.RunID), AttemptID: string(u.AttemptID), Model: u.EventModel(), Basis: string(u.Basis), Uncertainty: u.Uncertainty,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
	})
	if err != nil {
		return err
	}
	return s.appendEvent(tx, eventsv1.Envelope{
		ID: "evt_" + string(u.RequestID), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeUsageRecorded, OccurredAt: u.RecordedAt,
		TenantID: u.Scope.TenantID, ProjectID: u.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "usage_record", ID: string(u.RequestID), Version: 1},
		Actor:    who.eventType, CorrelationID: string(u.RequestID), Data: data,
	})
}

// lockSettlement loads the run's budget and the request and checks they belong
// together.
func lockSettlement(tx memdb.Tx, scope domain.Scope, id gateway.RequestID, run domain.RunID, attempt domain.AttemptID) (gateway.RunBudget, gateway.ModelRequest, error) {
	m, err := loadRequest(tx, scope, id)
	if err != nil {
		return gateway.RunBudget{}, gateway.ModelRequest{}, err
	}
	if m.RunID != run || (attempt != "" && m.AttemptID != attempt) {
		return gateway.RunBudget{}, gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the request does not belong to this attempt")
	}
	b, ok := loadBudget(tx, scope, m.RunID)
	if !ok {
		return gateway.RunBudget{}, gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the run binding does not exist")
	}
	return b, m, nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Finalize implements gateway.Store. It needs no workload authority: a
// cancelled attempt must still be able to record what its request cost.
func (s *MemStore) Finalize(ctx context.Context, cmd gateway.FinalizeCommand) (gateway.Usage, error) {
	if err := firstError(cmd.Scope.Validate(), cmd.RunID.Validate(), cmd.AttemptID.Validate(), cmd.RequestID.Validate()); err != nil {
		return gateway.Usage{}, gateway.Failf(gateway.ErrInvalid, "the finalization is malformed")
	}
	var out gateway.Usage
	err := s.atomically(ctx, cmd.Scope.TenantID, func(tx memdb.Tx) error {
		b, m, err := lockSettlement(tx, cmd.Scope, cmd.RequestID, cmd.RunID, cmd.AttemptID)
		if err != nil {
			return err
		}
		u, err := m.Finalize(cmd.Outcome, cmd.Now)
		if err != nil {
			return err
		}
		if err := s.settle(tx, b, m, u, !m.State.HoldsSlot(), workloadSettler(cmd.AttemptID, "finalized")); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// Abandon implements gateway.Store.
func (s *MemStore) Abandon(ctx context.Context, scope domain.Scope, id gateway.RequestID, before, now time.Time) (gateway.Usage, error) {
	var out gateway.Usage
	err := s.atomically(ctx, scope.TenantID, func(tx memdb.Tx) error {
		first, err := loadRequest(tx, scope, id)
		if err != nil {
			return err
		}
		b, m, err := lockSettlement(tx, scope, id, first.RunID, "")
		if err != nil {
			return err
		}
		if !m.HeartbeatAt.Before(before) {
			return gateway.Failf(gateway.ErrSuperseded, "the request is not abandoned")
		}
		var u gateway.Usage
		release := true
		switch m.State {
		case gateway.StateAdmitted:
			u, err = m.AbandonUnsent(now)
		case gateway.StateForwarding:
			u, err = m.MarkUnknown(now)
			release = false
		default:
			return gateway.Failf(gateway.ErrSuperseded, "the request is already settled")
		}
		if err != nil {
			return err
		}
		if err := s.settle(tx, b, m, u, release, reconcilerSettler("reconciled")); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// Settle implements gateway.Store.
func (s *MemStore) Settle(ctx context.Context, cmd gateway.SettleCommand) error {
	scope, id, now := cmd.Scope, cmd.RequestID, cmd.Now
	if err := firstError(scope.Validate(), id.Validate()); err != nil {
		return gateway.Failf(gateway.ErrInvalid, "the settlement is malformed")
	}
	return s.atomically(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if err := s.management.Authorize(ctx, cmd.Actor, scope, gateway.PermSettings); err != nil {
			return err
		}
		first, err := loadRequest(tx, scope, id)
		if err != nil {
			return err
		}
		b, m, err := lockSettlement(tx, scope, id, first.RunID, "")
		if err != nil {
			return err
		}
		if err := m.Settle(now); err != nil {
			return err
		}
		if err := b.Release(m.ID); err != nil {
			return err
		}
		if err := saveRequest(tx, m); err != nil {
			return err
		}
		if err := saveBudget(tx, b); err != nil {
			return err
		}
		u := gateway.Usage{RequestID: m.ID, Scope: m.Scope, RunID: m.RunID, AttemptID: m.AttemptID, RequestedModel: m.Model,
			Basis: gateway.BasisEstimated, Uncertainty: gateway.UncertaintyWorkerLost, Result: gateway.StateUnknown, Code: "settled", RecordedAt: now.UTC()}
		return s.auditUsage(tx, m, u, settler{reason: "settled", eventType: eventsv1.Actor{Type: eventsv1.ActorUser, ID: cmd.Actor.ID}, auditType: "user"}, b)
	})
}
