package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Update implements execution.Store: load, mutate, persist, run peer effects
// and write history, audit and events in one update.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	var agg *execution.Aggregate
	var res any
	err := s.inTenant(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var err error
		agg, res, err = s.UpdateInTx(ctx, tx, scope, id, opts, fn)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return agg, res, nil
}

// UpdateInTx is Update inside a caller-owned transaction of scope's tenant,
// for a peer that joins execution to its own update. It never commits.
func (s *Store) UpdateInTx(ctx context.Context, tx memdb.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	p, err := s.mutate(ctx, tx, scope, id, opts, fn)
	if err != nil {
		return nil, nil, err
	}
	if err := p.settleWriters(); err != nil {
		return nil, nil, err
	}
	if err := s.record(ctx, tx, p); err != nil {
		return nil, nil, err
	}
	return p.agg, p.res, nil
}

// pendingUpdate is a run mutation applied and persisted up to the peer
// effects, with workspace settlement and event writing still to come, so a
// batch can finish every run's earlier effects first.
type pendingUpdate struct {
	agg *execution.Aggregate
	res any
	env *txEnv
}

func (s *Store) mutate(ctx context.Context, tx memdb.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*pendingUpdate, error) {
	if scope.Validate() != nil || id.Validate() != nil || tx.Tenant() != scope.TenantID {
		return nil, execution.ErrNotFound
	}
	env := s.newEnv(tx, scope)
	env.deferWriter = true
	if opts.Quota {
		env.quotaLocked = true
	}
	if opts.LockWork {
		run, ok := runsT.Get(tx, runKey(scope, id))
		if !ok {
			return nil, execution.ErrNotFound
		}
		if run.WorkItemID != "" {
			env.takeWork(run.WorkItemID)
		}
	}
	agg, err := s.loadAggregate(tx, scope, id)
	if err != nil {
		return nil, err
	}
	fx, err := fn(ctx, env, agg)
	if err != nil {
		return nil, err
	}
	if err := s.persist(tx, agg); err != nil {
		return nil, err
	}
	if err := s.revokeGrants(tx, scope, agg, env.Now()); err != nil {
		return nil, err
	}
	for _, after := range fx.After {
		if err := after(ctx, env); err != nil {
			return nil, err
		}
	}
	return &pendingUpdate{agg: agg, res: fx.Result, env: env}, nil
}

func (p *pendingUpdate) settleWriters() error { return p.env.runSettles() }

func (s *Store) record(ctx context.Context, tx memdb.Tx, p *pendingUpdate) error {
	if err := s.writeTransitions(ctx, tx, p.agg); err != nil {
		return err
	}
	markClean(p.agg)
	return nil
}

// revokeGrants ends the runtime grants of every attempt whose authority ended
// without an accepted result, in attempt ID order. An attempt whose result was
// accepted keeps its grant (the receipt exception of runtime admission).
func (s *Store) revokeGrants(tx memdb.Tx, scope domain.Scope, agg *execution.Aggregate, now time.Time) error {
	if s.peers.Grants == nil {
		return nil
	}
	seen := map[domain.AttemptID]bool{}
	var ids []domain.AttemptID
	for _, at := range agg.Revoked {
		if at.ResultHash != "" || seen[at.ID] {
			continue
		}
		if at.Scope != scope {
			return fmt.Errorf("%w: an attempt of another scope was revoked", execution.ErrNotFound)
		}
		seen[at.ID] = true
		ids = append(ids, at.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if err := s.peers.Grants(tx, scope, id, now); err != nil {
			return execution.Unavailablef(err, "the runtime grants of attempt %s could not be revoked", id)
		}
	}
	return nil
}

func markClean(agg *execution.Aggregate) {
	agg.Run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}
	for _, a := range agg.Attempts {
		a.MarkClean()
	}
	for _, c := range agg.Controls {
		c.MarkClean()
	}
	agg.Created = nil
	markCleanShared(agg)
}

// ---- Env ----

// txEnv is the execution.Env bound to one open transaction.
type txEnv struct {
	s     *Store
	tx    memdb.Tx
	scope domain.Scope

	quotaLocked bool
	work        execution.WorkAdmission
	workErr     error
	workTaken   bool
	authority   execution.TxAuthority

	// deferWriter queues SettleWriter calls so the store runs them after the
	// other queued effects, whatever order the entities queued them in.
	deferWriter bool
	settles     []func() error
}

var _ execution.Env = (*txEnv)(nil)

func (s *Store) newEnv(tx memdb.Tx, scope domain.Scope) *txEnv {
	return &txEnv{s: s, tx: tx, scope: scope}
}

func (e *txEnv) Now() time.Time             { return e.s.now() }
func (e *txEnv) NewID(prefix string) string { return e.s.opts.NewID(prefix) }

func (e *txEnv) Work() (execution.WorkAdmission, error) {
	if !e.workTaken {
		return execution.WorkAdmission{}, execution.Unavailablef(nil, "the work item admission lock was not taken")
	}
	return e.work, e.workErr
}

// takeWork reads the work item's admission through the transaction.
func (e *txEnv) takeWork(id domain.WorkItemID) {
	adm, err := e.s.peers.Work(e.tx, e.scope, id)
	e.workTaken = true
	e.work, e.workErr = adm, err
}

// Authorize checks the actor's permission through the transaction. A missing
// binding fails closed.
func (e *txEnv) Authorize(ctx context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	if scope != e.scope {
		return execution.ErrDenied
	}
	if e.authority == nil {
		e.authority = e.s.peers.Authority(e.tx)
	}
	if e.authority == nil {
		return execution.Unavailablef(nil, "authority is not bound")
	}
	return e.authority.Authorize(ctx, actor, scope, perm)
}

// CapacityAvailable counts the held reservations of the tenant and of the
// project and refuses when either limit is reached.
func (e *txEnv) CapacityAvailable(_ context.Context, scope domain.Scope, lim execution.Limit) error {
	if !e.quotaLocked || scope != e.scope {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	if err := lim.Validate(); err != nil {
		return execution.Unavailablef(err, "invalid limits")
	}
	var tenantHeld, projectHeld int
	for _, r := range collect(e.tx, reservationsT, Reservation.Held) {
		tenantHeld++
		if r.Scope.ProjectID == scope.ProjectID {
			projectHeld++
		}
	}
	// A run_shared run's active-attempt slot is reserved for its whole life.
	for _, r := range collect(e.tx, runReservationsT, RunReservation.Held) {
		tenantHeld++
		if r.Scope.ProjectID == scope.ProjectID {
			projectHeld++
		}
	}
	if tenantHeld >= lim.TenantConcurrency || projectHeld >= lim.ProjectConcurrency {
		return fmt.Errorf("%w: %d of %d tenant and %d of %d project slots are in use", execution.ErrQuotaExceeded,
			tenantHeld, lim.TenantConcurrency, projectHeld, lim.ProjectConcurrency)
	}
	return nil
}

// ReserveCapacity records the reservation exactly once.
func (e *txEnv) ReserveCapacity(_ context.Context, at *execution.Attempt) error {
	if !e.quotaLocked {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	k := key(string(at.Scope.ProjectID), string(at.ID))
	if _, ok := reservationsT.Get(e.tx, k); ok {
		return nil
	}
	return reservationsT.Put(e.tx, Reservation{Scope: at.Scope, Attempt: at.ID, Run: at.RunID, HeldAt: e.s.now()})
}

// ReleaseCapacity releases the reservation once; a repeat is a no-op.
func (e *txEnv) ReleaseCapacity(_ context.Context, at *execution.Attempt) error {
	r, ok := reservationsT.Get(e.tx, key(string(at.Scope.ProjectID), string(at.ID)))
	if !ok || r.ReleasedAt != nil {
		return nil
	}
	t := e.s.now()
	r.ReleasedAt = &t
	return reservationsT.Put(e.tx, r)
}

// ReserveWriter joins the workspace's writer reservation to this transaction.
func (e *txEnv) ReserveWriter(_ context.Context, at *execution.Attempt) error {
	return e.s.peers.Workspaces.ReserveWriter(e.tx, at)
}

// SettleWriter ends the writer reservation with the attempt's outcome. The
// settlement runs after the other queued effects.
func (e *txEnv) SettleWriter(_ context.Context, at *execution.Attempt, outcome execution.WriterOutcome, source string) error {
	settle := func() error { return e.s.peers.Workspaces.SettleWriter(e.tx, at, outcome, source) }
	if e.deferWriter {
		e.settles = append(e.settles, settle)
		return nil
	}
	return settle()
}

func (e *txEnv) runSettles() error {
	e.deferWriter = false
	settles := e.settles
	e.settles = nil
	for _, settle := range settles {
		if err := settle(); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseSecrets releases the run's secret-consumer pins.
func (e *txEnv) ReleaseSecrets(_ context.Context, run *execution.Run) error {
	return e.s.peers.Secrets.ReleasePins(e.tx, run.Scope, run.ID)
}
