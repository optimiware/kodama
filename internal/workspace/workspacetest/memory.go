package workspacetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
)

// EventAppender is the transaction-local event append of the in-memory event bus
// (eventstest.Mem.Append), the counterpart of the PostgreSQL adapter's
// pgx-based appender.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// MemOptions configure a MemStore; the zero value uses the wall clock and
// random identifiers.
type MemOptions struct {
	Clock func() time.Time
	NewID func(prefix string) string
}

type wsRec struct{ W workspace.Workspace }

type receiptRec struct {
	WorkspaceID workspace.ID
	P           workspace.Publication
}

type pinRec struct{ P workspace.RetainedRef }

type auditRec struct {
	ID          string
	WorkspaceID workspace.ID
	Action      string
	ActorType   string
	ActorID     string
	Reason      string
	RequestID   string
	Version     domain.Version
}

var (
	workspacesT = memdb.NewTable("workspaces",
		func(r wsRec) string { return string(r.W.ID) },
		func(r wsRec) string { return string(r.W.Scope.ProjectID) + "/" + string(r.W.WorkItemID) },
		func(r wsRec) string {
			return string(r.W.Scope.ProjectID) + "/" + string(r.W.RepositoryID) + "/" + r.W.WorkBranch
		})
	receiptsT = memdb.NewTable("workspace_publications", func(r receiptRec) string { return string(r.WorkspaceID) + "/" + string(r.P.AttemptID) })
	pinsT     = memdb.NewTable("workspace_retained_refs",
		func(r pinRec) string { return string(r.P.WorkspaceID) + "/" + string(r.P.ID) },
		func(r pinRec) string { return string(r.P.WorkspaceID) + "|" + r.P.RefName })
	auditT = memdb.NewTable("workspace_audit", func(r auditRec) string { return r.ID })
)

// MemStore implements workspace.Store over memdb. Insert and Update are one
// memdb.Update: the workspace, its receipt and retained references, the audit
// entries and the outbox events commit together or not at all, and the mutation
// runs on a private copy, so an error leaves every record unchanged. Callers
// that join work admission or secret pins pass their own Tx to UpdateInTx.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store  *memdb.Store
	events EventAppender
	opts   MemOptions
}

var _ workspace.Store = (*MemStore)(nil)

// NewMemStore requires a kernel and an event appender: a workspace transition
// without its audit and outbox records would be unobservable.
func NewMemStore(store *memdb.Store, events EventAppender, opts MemOptions) (*MemStore, error) {
	if store == nil || events == nil {
		return nil, fmt.Errorf("%w: a memdb store and an event appender are required", workspace.ErrNotConfigured)
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &MemStore{store: store, events: events, opts: opts}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

func (s *MemStore) now() time.Time { return s.opts.Clock().UTC() }

func cloneTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	t := p.UTC()
	return &t
}

// cloneWorkspace copies a workspace and its pointer members, so no caller shares
// them with a stored row.
func cloneWorkspace(w *workspace.Workspace) *workspace.Workspace {
	c := *w
	if w.Writer != nil {
		x := *w.Writer
		c.Writer = &x
	}
	if w.Pending != nil {
		x := *w.Pending
		c.Pending = &x
	}
	c.ArchiveRequestedAt = cloneTime(w.ArchiveRequestedAt)
	return &c
}

func clonePublication(p *workspace.Publication) *workspace.Publication {
	if p == nil {
		return nil
	}
	c := *p
	c.SettledAt = cloneTime(p.SettledAt)
	return &c
}

func clonePin(p *workspace.RetainedRef) *workspace.RetainedRef {
	c := *p
	c.ConfirmedAt, c.ReleasedAt, c.RemovalRequestedAt, c.RemovedAt = cloneTime(p.ConfirmedAt), cloneTime(p.ReleasedAt), cloneTime(p.RemovalRequestedAt), cloneTime(p.RemovedAt)
	return &c
}

func loadWorkspace(tx memdb.Tx, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error) {
	r, ok := workspacesT.Get(tx, string(id))
	if !ok || r.W.Scope != scope {
		return nil, workspace.ErrNotFound
	}
	return cloneWorkspace(&r.W), nil
}

func (s *MemStore) loadAggregate(tx memdb.Tx, w *workspace.Workspace, load workspace.Load) *workspace.Aggregate {
	agg := &workspace.Aggregate{Workspace: w}
	if load.Receipt != "" {
		if r, ok := receiptsT.Get(tx, string(w.ID)+"/"+string(load.Receipt)); ok {
			agg.Receipt = clonePublication(&r.P)
		}
	}
	if load.Pins {
		var pins []*workspace.RetainedRef
		pinsT.Scan(tx, func(r pinRec) bool {
			if r.P.WorkspaceID == w.ID {
				pins = append(pins, clonePin(&r.P))
			}
			return true
		})
		sort.Slice(pins, func(i, j int) bool {
			if !pins[i].CreatedAt.Equal(pins[j].CreatedAt) {
				return pins[i].CreatedAt.Before(pins[j].CreatedAt)
			}
			return pins[i].ID < pins[j].ID
		})
		agg.Pins = pins
	}
	return agg
}

// Insert implements workspace.Store: the workspace and its effects are written
// unless one exists for the work item, which is returned instead.
func (s *MemStore) Insert(ctx context.Context, w *workspace.Workspace, fx workspace.Effects) (*workspace.Workspace, bool, error) {
	var out *workspace.Workspace
	created := false
	err := s.store.Update(ctx, w.Scope.TenantID, func(tx memdb.Tx) error {
		created, out = false, nil
		var existing *workspace.Workspace
		workspacesT.Scan(tx, func(r wsRec) bool {
			if r.W.Scope == w.Scope && r.W.WorkItemID == w.WorkItemID {
				existing = cloneWorkspace(&r.W)
				return false
			}
			return true
		})
		if existing != nil {
			out = existing
			return nil
		}
		if _, taken := workspacesT.Get(tx, string(w.ID)); taken {
			// Put replaces a row with the same key: refuse before it can.
			return fmt.Errorf("%w: workspace identity %s is already in use", domain.ErrConflict, w.ID)
		}
		stored := cloneWorkspace(w)
		if err := workspacesT.Put(tx, wsRec{W: *stored}); err != nil {
			return err
		}
		if err := s.writeEffects(ctx, tx, stored, fx); err != nil {
			return err
		}
		out, created = cloneWorkspace(stored), true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Get implements workspace.Store.
func (s *MemStore) Get(ctx context.Context, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error) {
	var out *workspace.Workspace
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = loadWorkspace(tx, scope, id)
		return err
	})
	return out, err
}

// GetByWorkItem implements workspace.Store.
func (s *MemStore) GetByWorkItem(ctx context.Context, scope domain.Scope, item domain.WorkItemID) (*workspace.Workspace, error) {
	var out *workspace.Workspace
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		workspacesT.Scan(tx, func(r wsRec) bool {
			if r.W.Scope == scope && r.W.WorkItemID == item {
				out = cloneWorkspace(&r.W)
				return false
			}
			return true
		})
		return nil
	})
	if err == nil && out == nil {
		err = workspace.ErrNotFound
	}
	return out, err
}

// View implements workspace.Store.
func (s *MemStore) View(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load) (*workspace.Aggregate, error) {
	var out *workspace.Aggregate
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		w, err := loadWorkspace(tx, scope, id)
		if err != nil {
			return err
		}
		out = s.loadAggregate(tx, w, load)
		return nil
	})
	return out, err
}

// Update implements workspace.Store: load the workspace, run the pure mutation
// on a private copy, persist the workspace under its version, upsert the receipt
// and retained references and write audit and event records, all in one Update.
func (s *MemStore) Update(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	var out *workspace.Aggregate
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = s.UpdateInTx(ctx, tx, scope, id, load, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateInTx is Update inside a caller-owned memdb.Tx: the entry point through
// which the dispatch and result modules join writer reservation or settlement to
// their own attempt changes atomically, as postgres.UpdateInTx takes a pgx.Tx.
// It never opens an Update of its own.
func (s *MemStore) UpdateInTx(ctx context.Context, tx memdb.Tx, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	if tx.Tenant() != scope.TenantID {
		return nil, workspace.ErrNotFound
	}
	w, err := loadWorkspace(tx, scope, id)
	if err != nil {
		return nil, err
	}
	agg := s.loadAggregate(tx, w, load)
	before := w.Version
	fx, err := fn(agg)
	if err != nil {
		return nil, err
	}
	if w.Version != before {
		cur, _ := workspacesT.Get(tx, string(id))
		if cur.W.Version != before {
			return nil, workspace.ErrStale // cannot happen with serialized writers; fail safe
		}
		if err := workspacesT.Put(tx, wsRec{W: *cloneWorkspace(w)}); err != nil {
			return nil, err
		}
	}
	if err := s.writeEffects(ctx, tx, w, fx); err != nil {
		return nil, err
	}
	return agg, nil
}

// writeEffects persists receipts, pins, audit records and events for a change
// whose workspace row is already current at w.Version.
func (s *MemStore) writeEffects(ctx context.Context, tx memdb.Tx, w *workspace.Workspace, fx workspace.Effects) error {
	if r := fx.Receipt; r != nil {
		key := string(w.ID) + "/" + string(r.AttemptID)
		rec := receiptRec{WorkspaceID: w.ID, P: *clonePublication(r)}
		if cur, ok := receiptsT.Get(tx, key); ok {
			// Only the settlement fields move; the proposal's identity is immutable.
			cur.P.Status, cur.P.ObservedTip, cur.P.ConflictReason, cur.P.Outcome, cur.P.SettledAt = r.Status, r.ObservedTip, r.ConflictReason, r.Outcome, cloneTime(r.SettledAt)
			rec = cur
		}
		if err := receiptsT.Put(tx, rec); err != nil {
			return fmt.Errorf("workspace: save publication: %w", err)
		}
	}
	for _, p := range fx.Pins {
		key := string(w.ID) + "/" + string(p.ID)
		rec := pinRec{P: *clonePin(p)}
		if cur, ok := pinsT.Get(tx, key); ok {
			cur.P.Status, cur.P.ConfirmedAt, cur.P.ReleasedAt = p.Status, cloneTime(p.ConfirmedAt), cloneTime(p.ReleasedAt)
			cur.P.RemovalRequestedAt, cur.P.RemovedAt = cloneTime(p.RemovalRequestedAt), cloneTime(p.RemovedAt)
			rec = cur
		}
		if err := pinsT.Put(tx, rec); err != nil {
			return fmt.Errorf("workspace: save retained reference: %w", err)
		}
	}
	for _, a := range fx.Audit {
		if err := s.writeAudit(ctx, tx, w, a); err != nil {
			return err
		}
	}
	for i, e := range fx.Events {
		if err := s.writeEvent(ctx, tx, w, i, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemStore) writeAudit(ctx context.Context, tx memdb.Tx, w *workspace.Workspace, a workspace.Audit) error {
	reason := a.Reason
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	reqID := workspace.RequestID(ctx)
	if len(reqID) > 256 {
		reqID = reqID[:256]
	}
	actor := string(a.Actor.Type)
	if a.Actor.Type == workspace.ActorService {
		actor = "system" // the audit vocabulary's name for the service actor
	}
	if err := auditT.Put(tx, auditRec{ID: s.opts.NewID("aud"), WorkspaceID: w.ID, Action: a.Action, ActorType: actor, ActorID: a.Actor.ID,
		Reason: reason, RequestID: reqID, Version: w.Version}); err != nil {
		return fmt.Errorf("workspace: write audit: %w", err)
	}
	return nil
}

// eventID is deterministic per (workspace, version, position): a retried
// transaction can never append a second event for the same committed change.
func eventID(w *workspace.Workspace, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "workspace/%s/%s/%s/%d/%d", w.Scope.TenantID, w.Scope.ProjectID, w.ID, w.Version, index))
	return "evt_ws_" + hex.EncodeToString(sum[:20])
}

func (s *MemStore) writeEvent(ctx context.Context, tx memdb.Tx, w *workspace.Workspace, index int, e workspace.Event) error {
	var data any
	switch e.Kind {
	case workspace.EventStateChanged:
		data = eventsv1.WorkspaceStateChanged{From: e.From, To: e.To, Reason: e.Reason}
	case workspace.EventCheckpointCreated:
		p := eventsv1.WorkspaceCheckpointCreated{Repository: string(w.RepositoryID), NewCommit: string(e.NewCommit), HumanSynchronization: e.HumanSynchronization}
		if e.PreviousCommit != "" {
			prev := string(e.PreviousCommit)
			p.PreviousCommit = &prev
		}
		if e.PublishingAttempt != "" {
			att := string(e.PublishingAttempt)
			p.PublishingAttemptID = &att
		}
		data = p
	default:
		return fmt.Errorf("workspace: unknown event kind %q", e.Kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr := workspace.RequestID(ctx)
	if corr == "" {
		corr = s.opts.NewID("corr")
	}
	env := eventsv1.Envelope{
		ID: eventID(w, index), SchemaVersion: eventsv1.SchemaVersion, Type: string(e.Kind), OccurredAt: s.now(),
		TenantID: w.Scope.TenantID, ProjectID: w.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "workspace", ID: string(w.ID), Version: int64(w.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(e.Actor.Type), ID: e.Actor.ID},
		CorrelationID: corr, Data: raw,
	}
	if _, err := s.events.Append(tx, env); err != nil {
		return fmt.Errorf("workspace: append event %s: %w", e.Kind, err)
	}
	return nil
}

// Reconcilable implements workspace.Store: workspaces of the tenant with
// unfinished durable work, ordered by ID after the given ID.
func (s *MemStore) Reconcilable(ctx context.Context, tenant domain.TenantID, after workspace.ID, limit int) ([]workspace.Candidate, error) {
	var out []workspace.Candidate
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		busy := map[workspace.ID]bool{}
		pinsT.Scan(tx, func(r pinRec) bool {
			if r.P.Status == workspace.PinIntent || r.P.Status == workspace.PinRemoving {
				busy[r.P.WorkspaceID] = true
			}
			return true
		})
		workspacesT.Scan(tx, func(r wsRec) bool {
			w := r.W
			if len(out) >= limit {
				return false
			}
			if w.ID <= after {
				return true
			}
			if w.State == workspace.StateProvisioning || (w.ArchiveRequestedAt != nil && w.State == workspace.StateAvailable) || w.Pending != nil || busy[w.ID] {
				out = append(out, workspace.Candidate{Scope: w.Scope, ID: w.ID})
			}
			return true
		})
		return nil
	})
	return out, err
}

// ListProject implements workspace.Store.
func (s *MemStore) ListProject(ctx context.Context, scope domain.Scope, after workspace.ID, limit int) ([]workspace.ID, error) {
	var out []workspace.ID
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		workspacesT.Scan(tx, func(r wsRec) bool {
			if len(out) >= limit {
				return false
			}
			if r.W.Scope == scope && r.W.ID > after {
				out = append(out, r.W.ID)
			}
			return true
		})
		return nil
	})
	return out, err
}

// AuditActions lists the recorded audit actions of one workspace, sorted.
func (s *MemStore) AuditActions(ctx context.Context, tenant domain.TenantID, id workspace.ID) ([]string, error) {
	var out []string
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool {
			if r.WorkspaceID == id {
				out = append(out, r.Action)
			}
			return true
		})
		return nil
	})
	sort.Strings(out)
	return out, err
}
