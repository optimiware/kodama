package runstate

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/memdb"
)

// runRow is one run's whole retained record. Run, plan, claim, waits, commands,
// workload and resource observations, step outputs and materializations live in one row so every Commit is one
// atomic replacement, exactly as it is one transaction in a database.
type runRow struct{ Rec runtime.RunRecord }

type signalRow struct{ Lookup runtime.SignalLookup }

// outboxRow is one event of the outbox in commit order. Seq is its position.
type outboxRow struct {
	Seq   int64
	Event runtime.Event
}

type eventIndexRow struct{ Event runtime.Event }

type counterRow struct {
	Name string
	N    int64
}

const outboxCounter = "outbox"

var (
	runsT = memdb.NewClonedTable("kernel_runs",
		func(r runRow) runRow { return runRow{Rec: cloneRecord(r.Rec)} },
		func(r runRow) string { return runKey(r.Rec.Run.Scope.ProjectID, r.Rec.Run.ID) })
	signalsT = memdb.NewClonedTable("kernel_signals",
		func(r signalRow) signalRow { return signalRow{Lookup: cloneLookup(r.Lookup)} },
		func(r signalRow) string { return signalKeyOf(r.Lookup) })
	outboxT   = memdb.NewTable("kernel_outbox", func(r outboxRow) string { return fmt.Sprintf("%020d", r.Seq) })
	eventIdxT = memdb.NewTable("kernel_event_index",
		func(r eventIndexRow) string { return eventKey(r.Event.Scope.ProjectID, r.Event.ID) })
	countersT = memdb.NewTable("kernel_counters", func(r counterRow) string { return r.Name })
)

func runKey(p domain.ProjectID, id domain.RunID) string { return string(p) + "/" + string(id) }

func eventKey(p domain.ProjectID, id string) string { return string(p) + "/" + id }

func signalKeyOf(l runtime.SignalLookup) string {
	if l.Record != nil {
		return string(l.Record.ID)
	}
	return string(l.Tombstone.ID)
}

// Memory is a runtime.StateStore over internal/memdb for tests and development.
// It is safe for concurrent use. The zero value is not usable; use NewMemory.
//
// Beyond the port it offers Outbox, which reads the events committed with
// state, and FailNextCommit, which makes the next write transaction fail at
// commit time. Both exist for the shared contract suite.
type Memory struct {
	clock runtime.Clock
	db    *memdb.Store

	mu      sync.Mutex
	tenants map[domain.TenantID]struct{}
	fault   error
}

var _ runtime.StateStore = (*Memory)(nil)

// NewMemory returns an empty store. clock is the store's one clock: it times a
// cancellation, which carries no time of its own.
func NewMemory(clock runtime.Clock) *Memory {
	if clock == nil {
		panic("runstate: NewMemory needs a clock")
	}
	m := &Memory{clock: clock, tenants: map[domain.TenantID]struct{}{}}
	m.db = memdb.New(memdb.WithCommitFault(m.takeFault))
	return m
}

func (m *Memory) takeFault() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.fault
	m.fault = nil
	return err
}

// FailNextCommit makes the next write transaction that reaches commit return
// err (wrapped) and persist nothing. A nil err clears a pending fault.
func (m *Memory) FailNextCommit(err error) {
	m.mu.Lock()
	m.fault = err
	m.mu.Unlock()
}

func (m *Memory) register(t domain.TenantID) {
	m.mu.Lock()
	m.tenants[t] = struct{}{}
	m.mu.Unlock()
}

func (m *Memory) tenantList() []domain.TenantID {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.TenantID, 0, len(m.tenants))
	for t := range m.tenants {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func notFound(what string) error {
	return &domain.Error{Code: domain.CodeNotFound, Message: what + " not found"}
}

func versionConflict(format string, args ...any) error {
	return &domain.Error{Code: domain.CodeVersionConflict, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidValue, fmt.Sprintf(format, args...))
}

func checkRef(scope domain.Scope, id domain.RunID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return id.Validate()
}

// get returns the run record in scope, or not-found. A run of another project
// of the same tenant has another key; a run of another tenant is in another
// tenant's rows: both are absent here.
func get(tx memdb.Tx, scope domain.Scope, id domain.RunID) (runtime.RunRecord, error) {
	row, ok := runsT.Get(tx, runKey(scope.ProjectID, id))
	if !ok {
		return runtime.RunRecord{}, notFound("run")
	}
	return row.Rec, nil
}

func put(tx memdb.Tx, rec runtime.RunRecord) error { return runsT.Put(tx, runRow{Rec: rec}) }

// emit appends events to the outbox in the caller's transaction. An event
// already recorded with the same identity and content is not added again; the
// same identity with other content is a conflict.
func emit(tx memdb.Tx, events []runtime.Event) error {
	for _, e := range events {
		k := eventKey(e.Scope.ProjectID, e.ID)
		if prev, ok := eventIdxT.Get(tx, k); ok {
			if prev.Event != e {
				return fmt.Errorf("%w: event %s was recorded with other content", domain.ErrConflict, e.ID)
			}
			continue
		}
		c, _ := countersT.Get(tx, outboxCounter)
		c.Name, c.N = outboxCounter, c.N+1
		if err := countersT.Put(tx, c); err != nil {
			return err
		}
		if err := outboxT.Put(tx, outboxRow{Seq: c.N, Event: e}); err != nil {
			return err
		}
		if err := eventIdxT.Put(tx, eventIndexRow{Event: e}); err != nil {
			return err
		}
	}
	return nil
}

// Admit records the run, plan, pending waits and events atomically.
func (m *Memory) Admit(ctx context.Context, a runtime.Admission) (runtime.RunRecord, error) {
	if err := a.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	r := a.Run
	if r.Version != 1 || r.CancelRequested || r.Outcome != runtime.OutcomeNone || r.Cleanup.State != runtime.CleanupNotStarted {
		return runtime.RunRecord{}, invalid("an admitted run is new: version 1, not cancelled, not settled")
	}
	m.register(r.Scope.TenantID)
	var out runtime.RunRecord
	err := m.db.Update(ctx, r.Scope.TenantID, func(tx memdb.Tx) error {
		if prev, ok := runsT.Get(tx, runKey(r.Scope.ProjectID, r.ID)); ok {
			if prev.Rec.Plan.Digest != a.Plan.Digest {
				return fmt.Errorf("%w: run %s is admitted with plan %s, not %s",
					domain.ErrConflict, r.ID, prev.Rec.Plan.Digest, a.Plan.Digest)
			}
			if !sameTask(prev.Rec.Task, a.Task) {
				return fmt.Errorf("%w: run %s is admitted for %s, not for %s",
					domain.ErrConflict, r.ID, taskName(prev.Rec.Task), taskName(a.Task))
			}
			out = prev.Rec
			return nil
		}
		rec := cloneRecord(runtime.RunRecord{Run: r, Plan: a.Plan, Task: a.Task, Waits: a.Waits})
		if err := put(tx, rec); err != nil {
			return err
		}
		out = rec
		return emit(tx, a.Events)
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return cloneRecord(out), nil
}

// Load returns the run record or a not-found error.
func (m *Memory) Load(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunRecord, error) {
	if err := checkRef(scope, id); err != nil {
		return runtime.RunRecord{}, err
	}
	var out runtime.RunRecord
	err := m.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		rec, err := get(tx, scope, id)
		out = rec
		return err
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return cloneRecord(out), nil
}

// Claim grants a lease through runtime.AcquireClaim for a purpose the run admits.
func (m *Memory) Claim(ctx context.Context, req runtime.ClaimRequest, now time.Time) (runtime.RunRecord, error) {
	if err := req.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	if now.IsZero() {
		return runtime.RunRecord{}, invalid("a claim needs the time")
	}
	var out runtime.RunRecord
	err := m.db.Update(ctx, req.Scope.TenantID, func(tx memdb.Tx) error {
		rec, err := get(tx, req.Scope, req.RunID)
		if err != nil {
			return err
		}
		if err := rec.Run.Admits(req.Purpose); err != nil {
			return err
		}
		c, err := runtime.AcquireClaim(rec.Claim, req, now)
		if err != nil {
			return err
		}
		rec.Claim = c
		out = rec
		return put(tx, rec)
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return cloneRecord(out), nil
}

// Renew extends the holder's lease atomically; the fence never changes.
func (m *Memory) Renew(ctx context.Context, held runtime.Claim, ttl time.Duration, now time.Time) (runtime.Claim, error) {
	if err := checkRef(held.Scope, held.RunID); err != nil {
		return runtime.Claim{}, err
	}
	var out runtime.Claim
	err := m.db.Update(ctx, held.Scope.TenantID, func(tx memdb.Tx) error {
		rec, err := get(tx, held.Scope, held.RunID)
		if err != nil {
			return err
		}
		if rec.Claim == (runtime.Claim{}) {
			return fmt.Errorf("%w: run %s was never claimed", runtime.ErrStaleFence, held.RunID)
		}
		c, err := runtime.RenewClaim(rec.Claim, held, now, ttl)
		if err != nil {
			return err
		}
		rec.Claim = c
		out = c
		return put(tx, rec)
	})
	if err != nil {
		return runtime.Claim{}, err
	}
	return out, nil
}

// Release ends the holder's lease without ending the run, through
// runtime.ReleaseClaim. It is a no-op for a claim the caller no longer holds.
func (m *Memory) Release(ctx context.Context, held runtime.Claim, now time.Time) error {
	if err := checkRef(held.Scope, held.RunID); err != nil {
		return err
	}
	return m.db.Update(ctx, held.Scope.TenantID, func(tx memdb.Tx) error {
		rec, err := get(tx, held.Scope, held.RunID)
		if err != nil {
			return err
		}
		c, released, err := runtime.ReleaseClaim(rec.Claim, held, now)
		if err != nil || !released {
			return err
		}
		rec.Claim = c
		return put(tx, rec)
	})
}

// Commit applies one claim-fenced, version-checked transition with its outbox
// intent in a single transaction.
func (m *Memory) Commit(ctx context.Context, t runtime.Transition) (runtime.RunRecord, error) {
	if err := t.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	var out runtime.RunRecord
	err := m.db.Update(ctx, t.Scope.TenantID, func(tx memdb.Tx) error {
		stored, err := get(tx, t.Scope, t.RunID)
		if err != nil {
			return err
		}
		next, err := applyTransition(stored, t)
		if err != nil {
			return err
		}
		if err := put(tx, next); err != nil {
			return err
		}
		out = next
		return emit(tx, t.Events)
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return cloneRecord(out), nil
}

// Runnable lists runs with claimable work: not terminal, with no active lease at
// q.Now. The purpose is the one the run admits: execute for a running run,
// reconcile once cancellation is requested, cleanup once execution settled. A
// run whose cleanup is exhausted is not listed: late reconciliation of its
// retained resources is an explicit cleanup claim, not scheduled work.
// Results are ordered by tenant, project and run.
func (m *Memory) Runnable(ctx context.Context, q runtime.RunnableQuery) ([]runtime.RunRef, error) {
	if q.Limit < 1 {
		return nil, invalid("a runnable query is bounded by a positive limit")
	}
	if q.Now.IsZero() {
		return nil, invalid("a runnable query needs the time")
	}
	var out []runtime.RunRef
	for _, tenant := range m.tenantList() {
		err := m.db.View(ctx, tenant, func(tx memdb.Tx) error {
			runsT.Scan(tx, func(row runRow) bool {
				if len(out) >= q.Limit {
					return false
				}
				if p, ok := runnablePurpose(row.Rec, q.Now); ok {
					out = append(out, runtime.RunRef{Scope: row.Rec.Run.Scope, RunID: row.Rec.Run.ID, Purpose: p})
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func runnablePurpose(rec runtime.RunRecord, now time.Time) (runtime.ClaimPurpose, bool) {
	r := rec.Run
	if r.Terminal() || rec.Claim.Active(now) {
		return "", false
	}
	switch {
	case r.Settled():
		return runtime.ClaimCleanup, true
	case r.CancelRequested:
		return runtime.ClaimReconcile, true
	}
	return runtime.ClaimExecute, true
}

// RequestCancel records the cancellation and decides every pending and open
// wait as cancelled, in one transaction and with no claim. It is idempotent.
func (m *Memory) RequestCancel(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.Run, error) {
	if err := checkRef(scope, id); err != nil {
		return runtime.Run{}, err
	}
	now := m.clock.Now()
	var out runtime.Run
	err := m.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		rec, err := get(tx, scope, id)
		if err != nil {
			return err
		}
		if rec.Run.Settled() {
			// Cancelling a settled run changes nothing, including a wait a skipped
			// step left pending: its decision, if any, was taken when the run settled.
			out = rec.Run
			return nil
		}
		rec.Run.RequestCancel()
		var events []runtime.Event
		for i := range rec.Waits {
			w := &rec.Waits[i]
			if w.State != runtime.WaitPending && w.State != runtime.WaitOpen {
				continue
			}
			if _, err := w.Cancel(now, runtime.ReasonRunCancelled); err != nil {
				return err
			}
			events = append(events, runtime.Event{ID: fmt.Sprintf("wait.cancelled:%s:%d", w.ID, w.Version), Scope: scope,
				RunID: id, StepKey: w.StepKey, Kind: runtime.EventWaitCancelled, Version: w.Version,
				Reason: runtime.ReasonRunCancelled, At: now})
		}
		out = rec.Run
		if err := put(tx, rec); err != nil {
			return err
		}
		return emit(tx, events)
	})
	if err != nil {
		return runtime.Run{}, err
	}
	return out.Clone(), nil
}

// LookupSignal returns the record or tombstone of an identity, or the zero lookup.
func (m *Memory) LookupSignal(ctx context.Context, id runtime.SignalIdentity) (runtime.SignalLookup, error) {
	if err := id.Validate(); err != nil {
		return runtime.SignalLookup{}, err
	}
	var out runtime.SignalLookup
	err := m.db.View(ctx, id.Scope.TenantID, func(tx memdb.Tx) error {
		if row, ok := signalsT.Get(tx, string(id.ID())); ok {
			out = row.Lookup
		}
		return nil
	})
	return cloneLookup(out), err
}

// DecideSignal routes and decides one signal in a single transaction: the wait,
// the decision record or tombstone, and the outbox events commit together or
// not at all.
func (m *Memory) DecideSignal(ctx context.Context, req runtime.SignalRequest, now time.Time) (runtime.SignalResult, error) {
	if err := req.Validate(); err != nil {
		return runtime.SignalResult{}, err
	}
	scope := req.Identity.Scope
	var out runtime.SignalResult
	err := m.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		sigKey := string(req.Identity.ID())
		var prior runtime.SignalLookup
		if row, ok := signalsT.Get(tx, sigKey); ok {
			prior = row.Lookup
		}
		// Candidates are the waits of the caller's project only.
		var waits []runtime.Wait
		runOf := map[runtime.SignalTarget]runtime.RunRecord{}
		runsT.Scan(tx, func(row runRow) bool {
			if row.Rec.Run.Scope != scope {
				return true
			}
			for _, w := range row.Rec.Waits {
				waits = append(waits, w)
				runOf[w.Target()] = row.Rec
			}
			return true
		})
		// The shell supplies authority before routing; the store passes none.
		o, err := runtime.RouteSignal(req, prior, waits, nil, now)
		if err != nil {
			return err
		}
		out = o.Result
		if o.Record == nil && o.Tombstone == nil && o.Wait == nil {
			return nil // a replay of a recorded identity writes nothing
		}
		var events []runtime.Event
		if o.Wait != nil {
			w := *o.Wait
			rec := runOf[w.Target()]
			i := slices.IndexFunc(rec.Waits, func(x runtime.Wait) bool { return x.ID == w.ID && x.StepKey == w.StepKey })
			if i < 0 || rec.Waits[i].Version+1 != w.Version {
				return versionConflict("wait %s changed during routing", w.ID)
			}
			rec.Waits[i] = w
			if err := put(tx, rec); err != nil {
				return err
			}
			events = append(events, decisionEvent(w, now))
		}
		switch {
		case o.Record != nil:
			if err := signalsT.Put(tx, signalRow{Lookup: runtime.SignalLookup{Record: o.Record}}); err != nil {
				return err
			}
			if !o.Record.Applied {
				t := o.Record.Target
				version := o.Record.Decision.Version
				if version == 0 {
					if rec, ok := runOf[t]; ok {
						if i := slices.IndexFunc(rec.Waits, func(x runtime.Wait) bool { return x.ID == t.WaitID }); i >= 0 {
							version = rec.Waits[i].Version
						}
					}
				}
				events = append(events, runtime.Event{ID: "signal.refused:" + string(o.Record.ID), Scope: scope,
					RunID: t.RunID, StepKey: t.StepKey, Kind: runtime.EventSignalRefused, Version: version, At: now})
			}
		case o.Tombstone != nil:
			// A targetless refusal has no run to attach an event to; its durable
			// tombstone is the record.
			if err := signalsT.Put(tx, signalRow{Lookup: runtime.SignalLookup{Tombstone: o.Tombstone}}); err != nil {
				return err
			}
		}
		return emit(tx, events)
	})
	if err != nil {
		return runtime.SignalResult{}, err
	}
	return out, nil
}

func decisionEvent(w runtime.Wait, now time.Time) runtime.Event {
	kind := runtime.EventWaitResolved
	if w.State == runtime.WaitExpired {
		kind = runtime.EventWaitExpired
	}
	return runtime.Event{ID: fmt.Sprintf("%s:%s:%d", kind, w.ID, w.Version), Scope: w.Scope, RunID: w.RunID,
		StepKey: w.StepKey, Kind: kind, Version: w.Version, At: now}
}

// View rebuilds the read model from the retained run, step and wait records.
func (m *Memory) View(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunView, error) {
	rec, err := m.Load(ctx, scope, id)
	if err != nil {
		return runtime.RunView{}, err
	}
	return runtime.RunView{Run: rec.Run, Task: rec.Task, Waits: rec.Waits, Status: rec.Run.Status()}, nil
}

// Outbox returns the events committed for one run, in commit order. A run in
// another scope is not found.
func (m *Memory) Outbox(ctx context.Context, scope domain.Scope, id domain.RunID) ([]runtime.Event, error) {
	if err := checkRef(scope, id); err != nil {
		return nil, err
	}
	var out []runtime.Event
	err := m.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, err := get(tx, scope, id); err != nil {
			return err
		}
		outboxT.Scan(tx, func(r outboxRow) bool {
			if r.Event.Scope == scope && r.Event.RunID == id {
				out = append(out, r.Event)
			}
			return true
		})
		return nil
	})
	return out, err
}
