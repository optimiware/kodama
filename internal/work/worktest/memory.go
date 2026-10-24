package worktest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// EventAppender is the transaction-local event append of the in-memory event
// bus (eventstest.Mem.Append), the counterpart of the PostgreSQL adapter's
// pgx-based appender.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// GuardBinder binds the cross-module guards (authority, writer, dependency and
// review-subject checks) to one memdb transaction, the counterpart of the
// PostgreSQL adapter's GuardBinder. A zero TxGuards member fails closed.
type GuardBinder func(tx memdb.Tx) work.TxGuards

type itemRec struct {
	Item work.Item
}

type historyRec struct {
	Entry work.HistoryEntry
}

type auditRec struct {
	ID        string
	ItemID    domain.WorkItemID
	Action    string
	ActorType string
	ActorID   string
	Version   domain.Version
}

type projectRec struct {
	ID       domain.ProjectID
	WIPLimit int
}

type memberRec struct {
	User    string
	Project domain.ProjectID // empty: tenant-wide
}

var (
	itemsT   = memdb.NewTable("work_items", func(r itemRec) string { return string(r.Item.Scope.ProjectID) + "/" + string(r.Item.ID) })
	historyT = memdb.NewTable("work_item_history", func(r historyRec) string {
		return historyKey(r.Entry.Scope.ProjectID, r.Entry.WorkItemID, r.Entry.Version)
	})
	auditT    = memdb.NewTable("work_audit", func(r auditRec) string { return r.ID })
	projectsT = memdb.NewTable("work_projects", func(r projectRec) string { return string(r.ID) })
	membersT  = memdb.NewTable("work_members", func(r memberRec) string { return r.User + "/" + string(r.Project) })
)

func historyKey(project domain.ProjectID, id domain.WorkItemID, v domain.Version) string {
	return fmt.Sprintf("%s/%s/%020d", project, id, int64(v))
}

func itemKey(scope domain.Scope, id domain.WorkItemID) string {
	return string(scope.ProjectID) + "/" + string(id)
}

// MemStore implements work.Repository, work.CancellationSource and the
// admission seam over memdb. One Insert or Update is one memdb.Update: the
// item, its history entry, the audit row and the outbox event commit together
// or not at all, and the guards read their own tables through the same Tx.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store  *memdb.Store
	events EventAppender
	bind   GuardBinder
}

var (
	_ work.Repository         = (*MemStore)(nil)
	_ work.CancellationSource = (*MemStore)(nil)
)

// NewMemStore requires every collaborator, like the PostgreSQL store: a store
// without an event appender or guard binder could not honor atomic outbox
// writes or fail-closed guards.
func NewMemStore(store *memdb.Store, appender EventAppender, bind GuardBinder) (*MemStore, error) {
	if store == nil || appender == nil || bind == nil {
		return nil, errors.New("worktest: a memdb store, an event appender and a guard binder are required")
	}
	return &MemStore{store: store, events: appender, bind: bind}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

// SeedProject creates (or replaces) the project row with its active WIP limit,
// the stand-in for the identity module's project.
func (s *MemStore) SeedProject(ctx context.Context, scope domain.Scope, wip int) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return projectsT.Put(tx, projectRec{ID: scope.ProjectID, WIPLimit: wip})
	})
}

// AddMember records a current membership of user in the tenant (project empty)
// or in one project, the stand-in for the identity module's membership.
func (s *MemStore) AddMember(ctx context.Context, tenant domain.TenantID, user string, project domain.ProjectID) error {
	return s.store.Update(ctx, tenant, func(tx memdb.Tx) error { return membersT.Put(tx, memberRec{User: user, Project: project}) })
}

func notFound() error { return fmt.Errorf("%w: the work item was not found", work.ErrNotFound) }

// classify maps infrastructure failures to the module's classes, as the
// PostgreSQL adapter does for its driver errors.
func classify(err error) error {
	if err == nil || work.IsBusinessError(err) {
		return err
	}
	if errors.Is(err, events.ErrBacklogFull) {
		return work.Unavailablef(err, "the event backlog is full")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return work.Unavailablef(err, "the request ended before the outcome was known")
	}
	return err
}

// uncertain converts a failure after the mutation function completed into an
// unknown-outcome error: the commit may or may not have happened.
func uncertain(err error, ranToCommit bool) error {
	if err != nil && ranToCommit {
		return work.Unavailablef(err, "the outcome of the write is unknown; reread the work item before retrying")
	}
	return err
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// cloneItem copies an item through its stored (JSON) form, as the PostgreSQL
// adapter stores it, so no caller shares slices or pointers with the rows.
func cloneItem(it work.Item) (work.Item, error) {
	raw, err := json.Marshal(it)
	if err != nil {
		return work.Item{}, err
	}
	var out work.Item
	if err := json.Unmarshal(raw, &out); err != nil {
		return work.Item{}, err
	}
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	if out.Criteria == nil {
		out.Criteria = []work.Criterion{}
	}
	if out.EvidenceLinks == nil {
		out.EvidenceLinks = []work.EvidenceRef{}
	}
	return out, nil
}

func cloneHistory(h work.HistoryEntry) (work.HistoryEntry, error) {
	raw, err := json.Marshal(h)
	if err != nil {
		return work.HistoryEntry{}, err
	}
	var out work.HistoryEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return work.HistoryEntry{}, err
	}
	out.OccurredAt = out.OccurredAt.UTC()
	if out.ChangedFields == nil {
		out.ChangedFields = []string{}
	}
	if out.Before == nil {
		out.Before = map[string]any{}
	}
	if out.After == nil {
		out.After = map[string]any{}
	}
	return out, nil
}

func selectItem(tx memdb.Tx, scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	r, ok := itemsT.Get(tx, itemKey(scope, id))
	if !ok {
		return work.Item{}, notFound()
	}
	return cloneItem(r.Item)
}

func project(tx memdb.Tx, id domain.ProjectID) (projectRec, bool) {
	return projectsT.Get(tx, string(id))
}

// Insert implements work.Repository.
func (s *MemStore) Insert(ctx context.Context, scope domain.Scope, fn work.InsertFunc) (work.Item, error) {
	var out work.Item
	ran := false
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		env := s.newTxEnv(tx, scope, false)
		it, h, err := fn(ctx, env)
		if err != nil {
			return err
		}
		if it.Scope != scope || h.Scope != scope {
			return fmt.Errorf("%w: the mutation does not belong to the transaction's scope", work.ErrInvalid)
		}
		if _, ok := project(tx, scope.ProjectID); !ok {
			return fmt.Errorf("%w: a referenced resource was not found", work.ErrNotFound)
		}
		if _, exists := itemsT.Get(tx, itemKey(scope, it.ID)); exists {
			return fmt.Errorf("%w: the work item already exists", work.ErrVersionConflict)
		}
		stored, err := cloneItem(it)
		if err != nil {
			return err
		}
		if err := itemsT.Put(tx, itemRec{Item: stored}); err != nil {
			return err
		}
		if err := s.recordMutation(tx, it, h); err != nil {
			return err
		}
		// The caller gets its own deep copy: nothing it does to the result can
		// reach the committed row.
		returned, err := cloneItem(stored)
		if err != nil {
			return err
		}
		out, ran = returned, true
		return nil
	})
	return out, uncertain(classify(err), ran)
}

// Update implements work.Repository: project check (when requested), item,
// expected-version comparison, the update function over guards bound to this
// transaction, then item, history, audit and event, all in one memdb.Update.
func (s *MemStore) Update(ctx context.Context, scope domain.Scope, id domain.WorkItemID, expected domain.Version, opts work.UpdateOptions, fn work.UpdateFunc) (work.Item, error) {
	var out work.Item
	ran := false
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if opts.LockProject {
			if _, ok := project(tx, scope.ProjectID); !ok {
				return fmt.Errorf("%w: the project was not found", work.ErrNotFound)
			}
		}
		cur, err := selectItem(tx, scope, id)
		if err != nil {
			return err
		}
		if cur.Version != expected {
			return fmt.Errorf("%w: the work item is at version %d", work.ErrVersionConflict, cur.Version)
		}
		next, h, err := fn(ctx, s.newTxEnv(tx, scope, opts.LockProject), cur)
		if errors.Is(err, work.ErrNoChange) {
			out = cur
			return nil
		}
		if err != nil {
			return err
		}
		if next.Scope != scope || next.ID != id || next.Kind != cur.Kind || next.Version != cur.Version+1 || h.Version != next.Version {
			return fmt.Errorf("%w: the mutation does not continue the locked item", work.ErrInvalid)
		}
		stored, err := cloneItem(next)
		if err != nil {
			return err
		}
		if err := itemsT.Put(tx, itemRec{Item: stored}); err != nil {
			return err
		}
		if err := s.recordMutation(tx, next, h); err != nil {
			return err
		}
		// The caller gets its own deep copy: nothing it does to the result can
		// reach the committed row.
		returned, err := cloneItem(stored)
		if err != nil {
			return err
		}
		out, ran = returned, true
		return nil
	})
	return out, uncertain(classify(err), ran)
}

// recordMutation writes the history entry, the audit row and the outbox event.
func (s *MemStore) recordMutation(tx memdb.Tx, it work.Item, h work.HistoryEntry) error {
	if h.ID == "" || len(h.ID) > 120 {
		return fmt.Errorf("%w: history identifier is unusable", work.ErrInvalid)
	}
	key := historyKey(h.Scope.ProjectID, h.WorkItemID, h.Version)
	if _, dup := historyT.Get(tx, key); dup {
		return fmt.Errorf("%w: the resource changed concurrently", work.ErrVersionConflict)
	}
	stored, err := cloneHistory(h)
	if err != nil {
		return err
	}
	if err := historyT.Put(tx, historyRec{Entry: stored}); err != nil {
		return err
	}
	action, evType, data, err := eventFor(h)
	if err != nil {
		return err
	}
	auditActor := string(h.Actor.Type)
	if h.Actor.Type == work.ActorService {
		auditActor = "system"
	}
	if err := auditT.Put(tx, auditRec{ID: "aud_" + h.ID, ItemID: h.WorkItemID, Action: action, ActorType: auditActor, ActorID: h.Actor.ID, Version: h.Version}); err != nil {
		return err
	}
	correlation := h.RequestID
	if correlation == "" {
		correlation = h.ID
	}
	_, err = s.events.Append(tx, eventsv1.Envelope{
		ID: "evt_" + h.ID, SchemaVersion: eventsv1.SchemaVersion, Type: evType, OccurredAt: h.OccurredAt,
		TenantID: h.Scope.TenantID, ProjectID: h.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "work_item", ID: string(h.WorkItemID), Version: int64(h.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(h.Actor.Type), ID: h.Actor.ID},
		CorrelationID: correlation, Data: data,
	})
	return err
}

// eventFor builds the catalog event of a history entry. Payloads carry
// references and field names only, never specification content.
func eventFor(h work.HistoryEntry) (action, eventType string, data json.RawMessage, err error) {
	switch h.Kind {
	case work.ChangeCreated:
		kind, _ := h.After[work.FieldKind].(string)
		data, err = json.Marshal(eventsv1.WorkItemCreated{Kind: kind, State: string(h.To)})
		return "work_item.created", eventsv1.TypeWorkItemCreated, data, err
	case work.ChangeUpdated:
		p := eventsv1.WorkItemUpdated{ChangedFields: h.ChangedFields}
		if h.ContentChanged() {
			rev := int64(h.ContentRevision)
			p.ContentRevision = &rev
		}
		data, err = json.Marshal(p)
		return "work_item.updated", eventsv1.TypeWorkItemUpdated, data, err
	case work.ChangeTransitioned:
		data, err = json.Marshal(eventsv1.WorkItemTransitioned{From: string(h.From), To: string(h.To), TransitionID: h.ID})
		return "work_item.transitioned", eventsv1.TypeWorkItemTransitioned, data, err
	}
	return "", "", nil, fmt.Errorf("%w: unknown change kind %q", work.ErrInvalid, h.Kind)
}

// Get implements work.Repository.
func (s *MemStore) Get(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	var out work.Item
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = selectItem(tx, scope, id)
		return err
	})
	return out, classify(err)
}

// List implements work.Repository. Ordering is (created_at, id).
func (s *MemStore) List(ctx context.Context, scope domain.Scope, q work.ListQuery) ([]work.Item, error) {
	var out []work.Item
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var rows []work.Item
		itemsT.Scan(tx, func(r itemRec) bool {
			if r.Item.Scope.ProjectID == scope.ProjectID && listMatches(r.Item, q) {
				rows = append(rows, r.Item)
			}
			return true
		})
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].CreatedAt.Before(rows[j].CreatedAt)
			}
			return rows[i].ID < rows[j].ID
		})
		for _, it := range rows {
			if q.Limit > 0 && len(out) >= q.Limit {
				break
			}
			c, err := cloneItem(it)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	return out, classify(err)
}

func listMatches(it work.Item, q work.ListQuery) bool {
	if len(q.States) > 0 && !containsState(q.States, it.State) {
		return false
	}
	if len(q.Kinds) > 0 {
		found := false
		for _, k := range q.Kinds {
			found = found || k == it.Kind
		}
		if !found {
			return false
		}
	}
	if q.AssigneeID != "" && it.AssigneeID != q.AssigneeID {
		return false
	}
	if q.ParentID != "" && it.ParentID != q.ParentID {
		return false
	}
	if q.Blocked != nil && (it.BlockedReason != "") != *q.Blocked {
		return false
	}
	if q.After != nil {
		at := micros(q.After.CreatedAt)
		if it.CreatedAt.Before(at) || (it.CreatedAt.Equal(at) && it.ID <= q.After.ID) {
			return false
		}
	}
	return true
}

func containsState(ss []work.State, s work.State) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// History implements work.Repository.
func (s *MemStore) History(ctx context.Context, scope domain.Scope, id domain.WorkItemID, q work.HistoryQuery) ([]work.HistoryEntry, error) {
	var out []work.HistoryEntry
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, ok := itemsT.Get(tx, itemKey(scope, id)); !ok {
			return notFound()
		}
		historyT.Scan(tx, func(r historyRec) bool {
			e := r.Entry
			if e.Scope.ProjectID != scope.ProjectID || e.WorkItemID != id || e.Version <= q.AfterVersion {
				return true
			}
			if q.Limit > 0 && len(out) >= q.Limit {
				return false
			}
			c, err := cloneHistory(e)
			if err == nil {
				out = append(out, c)
			}
			return true
		})
		return nil
	})
	return out, classify(err)
}

// CancelledItems implements work.CancellationSource: a bounded, tenant-scoped
// scan of cancelled items ordered by (project, id), after the cursor.
func (s *MemStore) CancelledItems(ctx context.Context, tenant domain.TenantID, after work.CancelledCursor, limit int) ([]work.CancelledItem, error) {
	if limit < 1 || limit > 1000 {
		return nil, work.Invalidf("limit must be between 1 and 1000")
	}
	var out []work.CancelledItem
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		itemsT.Scan(tx, func(r itemRec) bool { // key order is (project, id)
			it := r.Item
			if it.State != work.StateCancelled || len(out) >= limit {
				return len(out) < limit
			}
			if it.Scope.ProjectID < after.ProjectID || (it.Scope.ProjectID == after.ProjectID && it.ID <= after.ID) {
				return true
			}
			out = append(out, work.CancelledItem{CancelRequest: work.CancelRequest{
				Scope: domain.Scope{TenantID: tenant, ProjectID: it.Scope.ProjectID}, WorkItemID: it.ID, Version: it.Version,
			}})
			return true
		})
		return nil
	})
	return out, classify(err)
}

// LockActionable is the transaction-local admission seam for the workspace and
// execution modules, over the caller's memdb.Tx: it returns the item's current
// kind, state, version and content revision and refuses an item outside scope
// (work.ErrNotFound), an epic (work.ErrNotActionable) and a cancelled item
// (work.ErrCancelled). Because the caller's reservation shares the Tx, it
// commits or rolls back with this check; no lock is taken (serialized writers
// make a cancellation either precede or follow the whole Update).
func LockActionable(tx memdb.Tx, scope domain.Scope, id domain.WorkItemID) (workpg.Admission, error) {
	if err := scope.Validate(); err != nil {
		return workpg.Admission{}, work.Invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return workpg.Admission{}, work.Invalidf("%v", err)
	}
	if tx.Tenant() != scope.TenantID {
		return workpg.Admission{}, notFound()
	}
	r, ok := itemsT.Get(tx, itemKey(scope, id))
	if !ok {
		return workpg.Admission{}, notFound()
	}
	it := r.Item
	a := workpg.Admission{Scope: scope, ID: id, Kind: it.Kind, State: it.State, Version: it.Version, ContentRevision: it.ContentRevision}
	if !a.Kind.Actionable() {
		return a, fmt.Errorf("%w: an epic has no workspace", work.ErrNotActionable)
	}
	if a.State == work.StateCancelled {
		return a, fmt.Errorf("%w: a cancelled work item admits no new work", work.ErrCancelled)
	}
	return a, nil
}

// ---- the transaction-bound environment ----------------------------------------

// txEnv is the work.Tx bound to one memdb transaction. Cross-module guards come
// from the injected GuardBinder, which reads its own tables through the same
// Tx; checks of work-owned rows are answered here.
type txEnv struct {
	tx            memdb.Tx
	guards        work.TxGuards
	scope         domain.Scope
	projectLocked bool
}

var _ work.Tx = (*txEnv)(nil)

func (s *MemStore) newTxEnv(tx memdb.Tx, scope domain.Scope, projectLocked bool) *txEnv {
	return &txEnv{tx: tx, guards: s.bind(tx), scope: scope, projectLocked: projectLocked}
}

func (e *txEnv) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	return e.guards.Authorize(ctx, actor, scope, perm)
}

func (e *txEnv) WriterSettled(ctx context.Context, item work.Item) (bool, error) {
	return e.guards.WriterSettled(ctx, item)
}

func (e *txEnv) DependenciesSatisfied(ctx context.Context, item work.Item) error {
	return e.guards.DependenciesSatisfied(ctx, item)
}

func (e *txEnv) VerifySubject(ctx context.Context, item work.Item, claim work.ReviewClaim) (work.VerifiedSubject, error) {
	return e.guards.VerifySubject(ctx, item, claim)
}

func (e *txEnv) VerifyEvidence(ctx context.Context, scope domain.Scope, refs []work.EvidenceRef) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	return e.guards.VerifyEvidence(ctx, scope, refs)
}

// AdmitActive counts active items of the project against its limit. Blocked
// items count: a blocked reason does not release the slot.
func (e *txEnv) AdmitActive(_ context.Context, item work.Item) error {
	if !e.projectLocked || item.Scope != e.scope {
		return work.Unavailablef(nil, "active admission requires the project lock")
	}
	p, ok := project(e.tx, e.scope.ProjectID)
	if !ok {
		return fmt.Errorf("%w: the project was not found", work.ErrNotFound)
	}
	active := 0
	itemsT.Scan(e.tx, func(r itemRec) bool {
		if r.Item.Scope.ProjectID == e.scope.ProjectID && r.Item.State == work.StateActive {
			active++
		}
		return true
	})
	if active >= p.WIPLimit {
		return fmt.Errorf("%w: %d of %d active slots are in use", work.ErrWIPLimit, active, p.WIPLimit)
	}
	return nil
}

// ChildSummary counts an epic's children by terminal state.
func (e *txEnv) ChildSummary(_ context.Context, item work.Item) (work.ChildSummary, error) {
	if item.Scope != e.scope || item.Kind != work.KindEpic {
		return work.ChildSummary{}, work.Invalidf("only an epic has a child summary")
	}
	var sum work.ChildSummary
	itemsT.Scan(e.tx, func(r itemRec) bool {
		if r.Item.Scope.ProjectID == e.scope.ProjectID && r.Item.ParentID == item.ID {
			sum.Total++
			switch r.Item.State {
			case work.StateDone:
				sum.Done++
			case work.StateCancelled:
				sum.Cancelled++
			}
		}
		return true
	})
	return sum, nil
}

// AssigneeEligible requires a current tenant-wide or project membership.
func (e *txEnv) AssigneeEligible(_ context.Context, scope domain.Scope, userID string) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	for _, p := range []domain.ProjectID{"", scope.ProjectID} {
		if _, ok := membersT.Get(e.tx, userID+"/"+string(p)); ok {
			return nil
		}
	}
	return fmt.Errorf("%w: the assignee was not found", work.ErrNotFound)
}

// ParentEligible requires an epic of the same project and no cycle.
func (e *txEnv) ParentEligible(_ context.Context, scope domain.Scope, id, parent domain.WorkItemID) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	if id == parent {
		return work.Invalidf("a work item cannot be its own parent")
	}
	p, ok := itemsT.Get(e.tx, itemKey(scope, parent))
	if !ok {
		return fmt.Errorf("%w: the parent was not found", work.ErrNotFound)
	}
	if p.Item.Kind != work.KindEpic {
		return work.Invalidf("the parent must be an epic")
	}
	// An existing item must not become its own ancestor.
	seen := map[domain.WorkItemID]bool{}
	for cur := parent; cur != "" && !seen[cur]; {
		if cur == id {
			return work.Invalidf("the parent would create a cycle")
		}
		seen[cur] = true
		r, ok := itemsT.Get(e.tx, itemKey(scope, cur))
		if !ok {
			break
		}
		cur = r.Item.ParentID
	}
	return nil
}

// ---- peer stand-ins over memdb ---------------------------------------------------

type principalRec struct {
	ID   string
	Type work.ActorType
}

type grantRec struct {
	Principal string
	Perm      work.Permission
}

var (
	principalsT = memdb.NewTable("work_principals", func(r principalRec) string { return r.ID })
	grantsT     = memdb.NewTable("work_grants", func(r grantRec) string { return r.Principal + "/" + string(r.Perm) })
)

// MemAuthority is the in-memory fixture authority: principals with an actor
// type and permission grants stand in for the identity module. Its guards read
// the grants through the transaction that mutates, so a revocation committed
// before the write is seen by it.
type MemAuthority struct{ store *memdb.Store }

var _ work.Authority = (*MemAuthority)(nil)

// NewMemAuthority returns an authority over store.
func NewMemAuthority(store *memdb.Store) *MemAuthority { return &MemAuthority{store: store} }

// Principal registers a principal with its actor type and grants.
func (a *MemAuthority) Principal(ctx context.Context, tenant domain.TenantID, id string, typ work.ActorType, perms ...work.Permission) error {
	return a.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := principalsT.Put(tx, principalRec{ID: id, Type: typ}); err != nil {
			return err
		}
		for _, p := range perms {
			if err := grantsT.Put(tx, grantRec{Principal: id, Perm: p}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Revoke removes one grant.
func (a *MemAuthority) Revoke(ctx context.Context, tenant domain.TenantID, id string, perm work.Permission) error {
	return a.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		grantsT.Delete(tx, id+"/"+string(perm))
		return nil
	})
}

// ResolveActor implements work.ActorResolver.
func (a *MemAuthority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (work.Actor, error) {
	var actor work.Actor
	err := a.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		p, ok := principalsT.Get(tx, principalID)
		if !ok {
			return work.ErrHidden
		}
		actor = work.Actor{ID: principalID, Type: p.Type}
		return nil
	})
	return actor, err
}

// Authorize implements work.PermissionChecker.
func (a *MemAuthority) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	return a.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return memGuards{tx}.Authorize(ctx, actor, scope, perm)
	})
}

// Guards binds the fixture guards to tx (a GuardBinder).
func (a *MemAuthority) Guards(tx memdb.Tx) work.TxGuards {
	g := memGuards{tx}
	return work.TxGuards{Permissions: g, Writers: g, Dependencies: work.NoDependencyGraph{}, Subjects: g}
}

type memGuards struct{ tx memdb.Tx }

func (g memGuards) Authorize(_ context.Context, actor work.Actor, _ domain.Scope, perm work.Permission) error {
	if _, ok := grantsT.Get(g.tx, actor.ID+"/"+string(perm)); !ok {
		return work.ErrDenied
	}
	return nil
}

// WriterSettled: no workspace module here, so no writer is ever unsettled.
func (memGuards) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, nil
}

// VerifySubject accepts the claim as the current subject.
func (memGuards) VerifySubject(_ context.Context, _ domain.Scope, _ domain.WorkItemID, c work.ReviewClaim) (work.VerifiedSubject, error) {
	return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
}

// VerifyEvidence accepts every reference.
func (memGuards) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }
