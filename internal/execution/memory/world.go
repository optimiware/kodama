package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
)

// World is the set of in-memory peers an execution Store joins: a work catalog,
// workspaces with a single writer slot and retained-source pins, a secret and
// variable catalog with exact version pins, and an event outbox, all over the
// same memdb kernel as the Store so the peer joins pass the same Tx. It also
// implements the outside-the-transaction seams of the use cases that read
// these peers (execution.WorkContent, execution.Sources,
// execution.CancelledWork, execution.Configuration).
//
// World is a fake of the consumer-owned seams, not an implementation of the
// work, workspace or secrets modules.
type World struct {
	db *memdb.Store

	mu         sync.Mutex
	history    map[domain.WorkItemID]map[string]bool
	remoteTip  map[string]string
	retainErr  error
	verifyErr  error
	retains    int
	pendingPub map[domain.AttemptID]bool
	eventFault map[string]error

	catalog catalog
}

var (
	_ execution.WorkContent   = (*World)(nil)
	_ execution.Sources       = (*World)(nil)
	_ execution.CancelledWork = (*World)(nil)
	_ execution.Configuration = (*World)(nil)
	_ Workspaces              = (*World)(nil)
	_ SecretPins              = (*World)(nil)
	_ EventAppender           = (*World)(nil)
)

// NewWorld returns empty peers over db.
func NewWorld(db *memdb.Store) *World {
	return &World{
		db: db, history: map[domain.WorkItemID]map[string]bool{}, remoteTip: map[string]string{}, pendingPub: map[domain.AttemptID]bool{},
		catalog: newCatalog(),
	}
}

// Peers returns the joins of a Store wired to these peers and the authority.
func (w *World) Peers(a *Authority) Peers {
	return Peers{Authority: a.Bind, Work: w.LockWork, Workspaces: w, Secrets: w, Events: w}
}

// ---- work ----

// WorkItem is a fake work item: the content the run pins and the cancellation
// state the admission lock reads.
type WorkItem struct {
	Scope     domain.Scope
	ID        domain.WorkItemID
	Epic      bool
	Cancelled bool
	Revision  domain.ContentRevision
	Spec      string
}

var workT = memdb.NewTable("world_work", func(w WorkItem) string { return key(string(w.Scope.ProjectID), string(w.ID)) })

// AddWork creates an actionable item at content revision 1.
func (w *World) AddWork(ctx context.Context, scope domain.Scope, id domain.WorkItemID, spec string) error {
	return w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return workT.Put(tx, WorkItem{Scope: scope, ID: id, Revision: 1, Spec: spec})
	})
}

func (w *World) updateWork(ctx context.Context, scope domain.Scope, id domain.WorkItemID, fn func(*WorkItem)) error {
	return w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		item, ok := workT.Get(tx, key(string(scope.ProjectID), string(id)))
		if !ok {
			return execution.ErrNotFound
		}
		fn(&item)
		return workT.Put(tx, item)
	})
}

// EditSpec changes the item's specification and advances its content revision.
func (w *World) EditSpec(ctx context.Context, scope domain.Scope, id domain.WorkItemID, spec string) error {
	return w.updateWork(ctx, scope, id, func(i *WorkItem) { i.Spec, i.Revision = spec, i.Revision+1 })
}

// MakeEpic turns the item into a non-actionable epic.
func (w *World) MakeEpic(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	return w.updateWork(ctx, scope, id, func(i *WorkItem) { i.Epic = true })
}

// CancelWork cancels the item, as the work module's cancellation transaction
// does: the state is the durable cancellation intent. It does not call
// execution; the cancellation consumer or the periodic scan does.
func (w *World) CancelWork(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	return w.updateWork(ctx, scope, id, func(i *WorkItem) { i.Cancelled = true })
}

// LockWork is the WorkLock of the Store: the item's admission through the tx.
func (w *World) LockWork(tx memdb.Tx, scope domain.Scope, id domain.WorkItemID) (execution.WorkAdmission, error) {
	item, ok := workT.Get(tx, key(string(scope.ProjectID), string(id)))
	switch {
	case !ok:
		return execution.WorkAdmission{}, execution.ErrNotFound
	case item.Cancelled:
		return execution.WorkAdmission{}, execution.ErrWorkCancelled
	case item.Epic:
		return execution.WorkAdmission{}, execution.ErrNotActionable
	}
	return execution.WorkAdmission{ContentRevision: item.Revision}, nil
}

// Snapshot implements execution.WorkContent.
func (w *World) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	var out execution.WorkSnapshot
	err := w.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		item, ok := workT.Get(tx, key(string(scope.ProjectID), string(id)))
		if !ok {
			return execution.ErrNotFound
		}
		out = execution.WorkSnapshot{
			ID: id, Actionable: !item.Epic, Cancelled: item.Cancelled, ContentRevision: item.Revision, Spec: item.Spec,
			Criteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}},
		}
		return nil
	})
	return out, err
}

// CancelledItems implements execution.CancelledWork.
func (w *World) CancelledItems(ctx context.Context, tenant domain.TenantID, after execution.CancelCursor, limit int) ([]execution.WorkCancellation, error) {
	var out []execution.WorkCancellation
	err := w.db.View(ctx, tenant, func(tx memdb.Tx) error {
		items := collect(tx, workT, func(i WorkItem) bool {
			if !i.Cancelled {
				return false
			}
			return i.Scope.ProjectID > after.ProjectID || (i.Scope.ProjectID == after.ProjectID && i.ID > after.ID)
		})
		sort.Slice(items, func(a, b int) bool {
			if items[a].Scope.ProjectID != items[b].Scope.ProjectID {
				return items[a].Scope.ProjectID < items[b].Scope.ProjectID
			}
			return items[a].ID < items[b].ID
		})
		for _, i := range items {
			if len(out) == limit {
				break
			}
			out = append(out, execution.WorkCancellation{Scope: i.Scope, WorkItemID: i.ID, Version: domain.Version(i.Revision)})
		}
		return nil
	})
	return out, err
}

// ---- workspaces and sources ----

// Workspace is a fake workspace: its accepted tip and single writer slot.
type Workspace struct {
	Scope  domain.Scope
	ID     string
	Item   domain.WorkItemID
	URL    string
	Branch string
	Tip    string
	Writer domain.AttemptID
	Fenced bool
}

// Pin is a run-bound retained-source reference: an intent until confirmed.
type Pin struct {
	Scope     domain.Scope
	ID        string
	Workspace string
	Run       domain.RunID
	Commit    string
	Confirmed bool
}

var (
	workspacesT = memdb.NewTable("world_workspaces", func(w Workspace) string { return key(string(w.Scope.ProjectID), w.ID) })
	pinsT       = memdb.NewTable("world_pins", func(p Pin) string { return key(string(p.Scope.ProjectID), p.ID) })
)

// AddWorkspace creates an available workspace of the item whose accepted tip is
// tip; tip is also accepted history.
func (w *World) AddWorkspace(ctx context.Context, scope domain.Scope, id string, item domain.WorkItemID, tip string) error {
	w.mu.Lock()
	if w.history[item] == nil {
		w.history[item] = map[string]bool{}
	}
	w.history[item][tip] = true
	w.mu.Unlock()
	return w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return workspacesT.Put(tx, Workspace{
			Scope: scope, ID: id, Item: item, URL: "https://git.example.invalid/" + id + ".git", Branch: workspace.WorkBranch(item), Tip: tip,
		})
	})
}

// AcceptCommit adds a commit to the item's accepted workspace history.
func (w *World) AcceptCommit(item domain.WorkItemID, commit string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.history[item] == nil {
		w.history[item] = map[string]bool{}
	}
	w.history[item][commit] = true
}

// SetRemoteTip makes the next refreshing Describe report tip for the workspace,
// as a remote branch that moved.
func (w *World) SetRemoteTip(workspace, tip string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.remoteTip[workspace] = tip
}

// SetRetainErr makes Retain fail (a Git outage); nil recovers.
func (w *World) SetRetainErr(err error) { w.mu.Lock(); w.retainErr = err; w.mu.Unlock() }

// SetVerifyErr makes Verify fail (a Git outage); nil recovers.
func (w *World) SetVerifyErr(err error) { w.mu.Lock(); w.verifyErr = err; w.mu.Unlock() }

// Retains reports how many times Retain was called.
func (w *World) Retains() int { w.mu.Lock(); defer w.mu.Unlock(); return w.retains }

// SetPublicationPending makes settling the attempt's writer slot ErrBusy until
// cleared, as an unsettled publication does.
func (w *World) SetPublicationPending(attempt domain.AttemptID, pending bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingPub[attempt] = pending
}

// Publish moves the workspace's accepted tip to commit, as an accepted
// publication of the workspace module does.
func (w *World) Publish(ctx context.Context, scope domain.Scope, id, commit string) error {
	return w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		ws, ok := workspacesT.Get(tx, key(string(scope.ProjectID), id))
		if !ok {
			return execution.ErrNotFound
		}
		ws.Tip = commit
		return workspacesT.Put(tx, ws)
	})
}

// Fence blocks writer admission of the workspace (a fenced publication).
func (w *World) Fence(ctx context.Context, scope domain.Scope, id string, fenced bool) error {
	return w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		ws, ok := workspacesT.Get(tx, key(string(scope.ProjectID), id))
		if !ok {
			return execution.ErrNotFound
		}
		ws.Fenced = fenced
		return workspacesT.Put(tx, ws)
	})
}

// WorkspaceOf reads a workspace.
func (w *World) WorkspaceOf(ctx context.Context, scope domain.Scope, id string) (Workspace, error) {
	var out Workspace
	err := w.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		ws, ok := workspacesT.Get(tx, key(string(scope.ProjectID), id))
		if !ok {
			return execution.ErrNotFound
		}
		out = ws
		return nil
	})
	return out, err
}

// PinsOf lists the workspace's retained-source pins.
func (w *World) PinsOf(ctx context.Context, scope domain.Scope, workspace string) ([]Pin, error) {
	var out []Pin
	err := w.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = collect(tx, pinsT, func(p Pin) bool { return p.Scope == scope && p.Workspace == workspace })
		return nil
	})
	return out, err
}

func sourceInfo(ws Workspace) execution.SourceInfo {
	return execution.SourceInfo{WorkspaceID: ws.ID, RepositoryURL: ws.URL, Branch: ws.Branch, AcceptedTip: ws.Tip}
}

// Verify implements execution.Sources: the commit must belong to the item's
// accepted workspace history.
func (w *World) Verify(ctx context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (execution.SourceInfo, error) {
	w.mu.Lock()
	verr, ok := w.verifyErr, w.history[item][commit]
	w.mu.Unlock()
	if verr != nil {
		return execution.SourceInfo{}, verr
	}
	var out execution.SourceInfo
	err := w.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		for _, ws := range collect(tx, workspacesT, func(ws Workspace) bool { return ws.Scope == scope && ws.Item == item }) {
			out = sourceInfo(ws)
			return nil
		}
		return execution.ErrNotFound
	})
	if err != nil {
		return execution.SourceInfo{}, err
	}
	if !ok {
		return execution.SourceInfo{}, execution.Refusedf("the source commit is not in the workspace history")
	}
	return out, nil
}

// Describe implements execution.Sources. A refresh reports the remote tip set
// with SetRemoteTip, or the accepted tip.
func (w *World) Describe(ctx context.Context, scope domain.Scope, workspace string, refresh bool) (execution.SourceInfo, error) {
	ws, err := w.WorkspaceOf(ctx, scope, workspace)
	if err != nil {
		return execution.SourceInfo{}, err
	}
	info := sourceInfo(ws)
	if refresh {
		w.mu.Lock()
		if tip, ok := w.remoteTip[workspace]; ok {
			info.AcceptedTip = tip
		}
		w.mu.Unlock()
	}
	return info, nil
}

// Retain implements execution.Sources: it confirms the run's pin, idempotently,
// as the workspace service's confirmation does.
func (w *World) Retain(ctx context.Context, scope domain.Scope, workspace string, run domain.RunID, commit string) (string, error) {
	w.mu.Lock()
	w.retains++
	err := w.retainErr
	w.mu.Unlock()
	if err != nil {
		return "", err
	}
	var id string
	uerr := w.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		for _, p := range collect(tx, pinsT, func(p Pin) bool {
			return p.Scope == scope && p.Workspace == workspace && p.Run == run && p.Commit == commit
		}) {
			id = p.ID
			p.Confirmed = true
			return pinsT.Put(tx, p)
		}
		return execution.ErrNotFound
	})
	if uerr != nil {
		return "", uerr
	}
	return id, nil
}

// ReserveWriter implements Workspaces.
func (w *World) ReserveWriter(tx memdb.Tx, at *execution.Attempt) error {
	ws, ok := workspacesT.Get(tx, key(string(at.Scope.ProjectID), at.WorkspaceID))
	switch {
	case !ok:
		return execution.ErrNotFound
	case ws.Fenced, ws.Writer != "" && ws.Writer != at.ID:
		return fmt.Errorf("%w: the workspace writer slot is not available", execution.ErrBusy)
	case ws.Tip != at.ExpectedTip:
		return fmt.Errorf("%w: the workspace tip moved", execution.ErrStale)
	}
	ws.Writer = at.ID
	return workspacesT.Put(tx, ws)
}

// SettleWriter implements Workspaces.
func (w *World) SettleWriter(tx memdb.Tx, at *execution.Attempt, outcome execution.WriterOutcome, source string) error {
	ws, ok := workspacesT.Get(tx, key(string(at.Scope.ProjectID), at.WorkspaceID))
	if !ok || ws.Writer != at.ID {
		return nil // a slot already released is success
	}
	w.mu.Lock()
	pending := w.pendingPub[at.ID]
	w.mu.Unlock()
	if pending {
		return fmt.Errorf("%w: the writer's publication is unsettled", execution.ErrBusy)
	}
	if outcome == execution.WriterSucceeded && source != "" && source != ws.Tip {
		// Only the accepted tip is a published output; anything else was never
		// published to the workspace.
		return fmt.Errorf("%w: the writer's output is not published", execution.ErrStale)
	}
	ws.Writer = ""
	return workspacesT.Put(tx, ws)
}

// RetainSource implements Workspaces: the run-bound pin, in intent status.
func (w *World) RetainSource(tx memdb.Tx, run *execution.Run, pinID string) error {
	ws, ok := workspacesT.Get(tx, key(string(run.Scope.ProjectID), run.WorkspaceID))
	if !ok {
		return fmt.Errorf("%w: the workspace was not found", execution.ErrNotFound)
	}
	for _, p := range collect(tx, pinsT, func(p Pin) bool { return p.Scope == run.Scope && p.Run == run.ID }) {
		_ = p // the run's intent exists already
		return nil
	}
	return pinsT.Put(tx, Pin{Scope: run.Scope, ID: pinID, Workspace: ws.ID, Run: run.ID, Commit: run.SourceCommit})
}

// ---- event outbox ----

type eventRow struct {
	Seq string
	Env eventsv1.Envelope
}

var eventsT = memdb.NewTable("world_events",
	func(e eventRow) string { return e.Seq },
	func(e eventRow) string { return e.Env.ID })

// FailEventType makes the append of one event type fail (nil clears it), to
// prove an outbox refusal rolls the whole transition back.
func (w *World) FailEventType(typ string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.eventFault == nil {
		w.eventFault = map[string]error{}
	}
	w.eventFault[typ] = err
}

// Append implements EventAppender. It deduplicates on the envelope ID: the same
// event appended again, in this or a later transaction, stays one row.
func (w *World) Append(tx memdb.Tx, env eventsv1.Envelope) error {
	if env.TenantID != tx.Tenant() {
		return execution.ErrNotFound
	}
	// The PostgreSQL appender validates the envelope against the event contract;
	// the fake does too, so a producer's output is proven valid in the unit layer.
	if err := env.Validate(); err != nil {
		return fmt.Errorf("execution: invalid event %s: %w", env.Type, err)
	}
	w.mu.Lock()
	fault := w.eventFault[env.Type]
	w.mu.Unlock()
	if fault != nil {
		return fault
	}
	rows := collect(tx, eventsT, nil)
	for _, r := range rows {
		if r.Env.ID == env.ID {
			return nil
		}
	}
	env.Data = slices.Clone(env.Data)
	return eventsT.Put(tx, eventRow{Seq: fmt.Sprintf("%016d", len(rows)+1), Env: env})
}

// Events returns the tenant's published envelopes in append order.
func (w *World) Events(ctx context.Context, tenant domain.TenantID) ([]eventsv1.Envelope, error) {
	var out []eventsv1.Envelope
	err := w.db.View(ctx, tenant, func(tx memdb.Tx) error {
		for _, r := range collect(tx, eventsT, nil) {
			env := r.Env
			env.Data = slices.Clone(env.Data)
			out = append(out, env)
		}
		return nil
	})
	return out, err
}

// EventTypes returns the types of the tenant's envelopes of a resource, in
// append order.
func (w *World) EventTypes(ctx context.Context, tenant domain.TenantID, resourceType string) ([]string, error) {
	evs, err := w.Events(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range evs {
		if resourceType == "" || e.Resource.Type == resourceType {
			out = append(out, e.Type)
		}
	}
	return out, nil
}

// ---- secret pins ----

type secretPin struct {
	Scope    domain.Scope
	Run      domain.RunID
	Ref      string
	Version  int64
	Released bool
}

var secretPinsT = memdb.NewTable("world_secret_pins", func(p secretPin) string {
	return key(string(p.Scope.ProjectID), string(p.Run), p.Ref, fmt.Sprint(p.Version))
})

// RegisterPins implements SecretPins: each pinned version must still be usable.
func (w *World) RegisterPins(tx memdb.Tx, scope domain.Scope, run domain.RunID, bindings []runtimev1.SecretBinding) error {
	for _, b := range bindings {
		if !w.catalog.usable(scope, b.Ref, b.Version) {
			return execution.Refusedf("a pinned secret is no longer usable")
		}
		if err := secretPinsT.Put(tx, secretPin{Scope: scope, Run: run, Ref: b.Ref, Version: b.Version}); err != nil {
			return err
		}
	}
	return nil
}

// ReleasePins implements SecretPins.
func (w *World) ReleasePins(tx memdb.Tx, scope domain.Scope, run domain.RunID) error {
	for _, p := range collect(tx, secretPinsT, func(p secretPin) bool { return p.Scope == scope && p.Run == run && !p.Released }) {
		p.Released = true
		if err := secretPinsT.Put(tx, p); err != nil {
			return err
		}
	}
	return nil
}

// LivePins lists the secret versions pinned by the run and not yet released,
// as "ref@version".
func (w *World) LivePins(ctx context.Context, scope domain.Scope, run domain.RunID) ([]string, error) {
	var out []string
	err := w.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		for _, p := range collect(tx, secretPinsT, func(p secretPin) bool { return p.Scope == scope && p.Run == run && !p.Released }) {
			out = append(out, fmt.Sprintf("%s@%d", p.Ref, p.Version))
		}
		return nil
	})
	return out, err
}

// secretName returns the secret name of a "secret://project/<name>" reference.
func secretName(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "secret://project/")
	return name, ok && name != ""
}
