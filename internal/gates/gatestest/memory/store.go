package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
)

// AuthorityBinder binds current-authorization checks to one open transaction.
type AuthorityBinder func(tx memdb.Tx) gates.TxAuthority

// DeploymentBinder binds the deployment reader to one open transaction.
type DeploymentBinder func(tx memdb.Tx) gates.Deployments

// StoreConfig wires a Store. DB and Authority are required: a gate transition
// without its authority check must not be constructible.
type StoreConfig struct {
	DB        *memdb.Store
	Authority AuthorityBinder
	// Deployments is optional; without it a deployment-bearing gate cannot be
	// decided or consumed.
	Deployments DeploymentBinder
	Clock       func() time.Time
	// Env supplies the counters of forbidden execution-peer calls.
	Env *Env
}

// Store implements gates.Store over memdb. One method call is one memdb
// update: the gate row, the bound execution step and the recorded events land
// together or not at all.
type Store struct {
	db   *memdb.Store
	auth AuthorityBinder
	dep  DeploymentBinder
	now  func() time.Time
	env  *Env
}

var _ gates.Store = (*Store)(nil)

// NewStore validates the configuration.
func NewStore(c StoreConfig) *Store {
	if c.DB == nil || c.Authority == nil {
		panic("gates/memory: a database and an authority binder are required")
	}
	s := &Store{db: c.DB, auth: c.Authority, dep: c.Deployments, now: c.Clock, env: c.Env}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s
}

func (s *Store) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// classify maps memdb failures to the module's classes.
func classify(err error) error {
	switch {
	case err == nil, gates.IsBusinessError(err):
		return err
	case errors.Is(err, memdb.ErrConflict):
		return fmt.Errorf("%w: the record already exists", gates.ErrConflict)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return gates.Unavailablef(err, "the request ended before the outcome was known")
	}
	return gates.Unavailablef(err, "the commit failed")
}

func (s *Store) update(ctx context.Context, tenant domain.TenantID, fn func(memdb.Tx) error) error {
	return classify(s.db.Update(ctx, tenant, fn))
}

func (s *Store) view(ctx context.Context, tenant domain.TenantID, fn func(memdb.Tx) error) error {
	return classify(s.db.View(ctx, tenant, fn))
}

// ---- gate rows ------------------------------------------------------------------

// cloneGate detaches every mutable member of a gate (memdb stores values as
// given): the decision, the invalidation, the evidence snapshot and the
// conformance snapshot with its required checks. The subject is immutable by
// construction (private fields, copying accessors) and is shared.
func cloneGate(g gates.Gate) gates.Gate {
	if g.Decision != nil {
		d := *g.Decision
		g.Decision = &d
	}
	if g.Invalidation != nil {
		i := *g.Invalidation
		g.Invalidation = &i
	}
	g.Required.Evidence = append([]gates.RequiredEvidence{}, g.Required.Evidence...)
	if c := g.Required.Conformance; c != nil {
		cc := *c
		if c.RequiredChecks != nil {
			cc.RequiredChecks = append([]string{}, c.RequiredChecks...)
		}
		g.Required.Conformance = &cc
	}
	return g
}

// loadGate returns a private copy: mutating it changes nothing stored.
func loadGate(tx memdb.Tx, scope domain.Scope, id gates.ID) (*gates.Gate, error) {
	g, ok := gateTable.Get(tx, projectKey(scope, string(id)))
	if !ok {
		return nil, gates.ErrNotFound
	}
	cp := cloneGate(g)
	return &cp, nil
}

func byStep(tx memdb.Tx, scope domain.Scope, run domain.RunID, step string) (*gates.Gate, bool) {
	var out *gates.Gate
	gateTable.Scan(tx, func(g gates.Gate) bool {
		if g.Scope == scope && g.RunID == run && g.StepID == step {
			cp := cloneGate(g)
			out = &cp
			return false
		}
		return true
	})
	return out, out != nil
}

// saveGate stores a detached copy: the caller keeps (and may mutate) g.
func saveGate(tx memdb.Tx, g *gates.Gate) error {
	cp := cloneGate(*g)
	cp.MarkClean()
	if err := gateTable.Put(tx, cp); err != nil {
		return err
	}
	g.MarkClean()
	return nil
}

// Get implements gates.Store.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id gates.ID) (*gates.Gate, error) {
	if err := scope.Validate(); err != nil || id.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	var g *gates.Gate
	err := s.view(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		g, err = loadGate(tx, scope, id)
		return err
	})
	return g, err
}

// ByStep implements gates.Store.
func (s *Store) ByStep(ctx context.Context, scope domain.Scope, run domain.RunID, step string) (*gates.Gate, error) {
	if err := scope.Validate(); err != nil || run.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	var g *gates.Gate
	err := s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var ok bool
		if g, ok = byStep(tx, scope, run, step); !ok {
			return gates.ErrNotFound
		}
		return nil
	})
	return g, err
}

// List implements gates.Store: gates of the project ordered by (created_at, id).
func (s *Store) List(ctx context.Context, scope domain.Scope, q gates.ListQuery) ([]*gates.Gate, error) {
	if err := scope.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	if q.Limit < 1 || q.Limit > 1000 {
		return nil, gates.Invalidf("limit is out of range")
	}
	var all []*gates.Gate
	err := s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		gateTable.Scan(tx, func(g gates.Gate) bool {
			if g.Scope != scope || (q.RunID != "" && g.RunID != q.RunID) {
				return true
			}
			if len(q.Statuses) > 0 {
				found := false
				for _, st := range q.Statuses {
					found = found || st == g.Status
				}
				if !found {
					return true
				}
			}
			cp := cloneGate(g)
			all = append(all, &cp)
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	var out []*gates.Gate
	for _, g := range all {
		if q.After != nil && !(g.CreatedAt.After(q.After.CreatedAt) || (g.CreatedAt.Equal(q.After.CreatedAt) && g.ID > q.After.ID)) {
			continue
		}
		out = append(out, g)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// ---- events ---------------------------------------------------------------------

func eventID(g *gates.Gate, kind string, version domain.Version) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "gates/%s/%s/%s/%s/%d", g.Scope.TenantID, g.Scope.ProjectID, g.ID, kind, version))
	return "evt_gt_" + hex.EncodeToString(sum[:20])
}

// record writes the event of one gate transition in the transaction that saved
// the gate. Its ID derives from (gate, kind, version), so a repeat replaces
// itself and never duplicates.
func record(tx memdb.Tx, g *gates.Gate, kind string, actor gates.Actor, reason string) error {
	return eventTable.Put(tx, EventRow{ID: eventID(g, kind, g.Version), Type: "gate." + kind, Scope: g.Scope, Gate: g.ID, Version: g.Version, Actor: actor, Reason: reason})
}

// ---- the execution peer ----------------------------------------------------------

// Runs is the in-memory execution peer's run reader (gates.RunReader).
type Runs struct{ db *memdb.Store }

var _ gates.RunReader = (*Runs)(nil)

// Get implements gates.RunReader and returns a private copy.
func (r *Runs) Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	var agg *execution.Aggregate
	err := r.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := runTable.Get(tx, projectKey(scope, string(id)))
		if !ok {
			return execution.ErrNotFound
		}
		agg = cloneAggregate(row.agg)
		return nil
	})
	return agg, err
}

// cloneAggregate copies the entities a gate transition mutates so a stored
// snapshot is never changed in place (memdb stores values as given).
func cloneAggregate(a *execution.Aggregate) *execution.Aggregate {
	run := *a.Run
	c := &execution.Aggregate{Run: &run}
	for _, st := range a.Steps {
		cp := *st
		if st.Outputs != nil {
			cp.Outputs = make(map[string]runtimev1.Value, len(st.Outputs))
			for k, v := range st.Outputs {
				cp.Outputs[k] = v
			}
		}
		c.Steps = append(c.Steps, &cp)
	}
	for _, at := range a.Attempts {
		cp := *at
		c.Attempts = append(c.Attempts, &cp)
	}
	if a.Controls != nil {
		c.Controls = map[domain.AttemptID]*execution.Control{}
		for id, ctl := range a.Controls {
			cp := *ctl
			c.Controls[id] = &cp
		}
	}
	return c
}

// execEnv is the execution.Env a gate mutation sees. Gates never reserve
// capacity or a writer slot and never decide authority through execution, so
// every such call is refused and counted.
type execEnv struct {
	s     *Store
	tx    memdb.Tx
	now   time.Time
	scope domain.Scope
	item  domain.WorkItemID
}

var _ execution.Env = execEnv{}

func (v execEnv) Now() time.Time { return v.now }

func (v execEnv) NewID(prefix string) string { return v.s.env.next(prefix) }

func (v execEnv) forbidden() error {
	v.s.env.reservations.Add(1)
	return fmt.Errorf("%w: a gate step must not use executor capacity", execution.ErrNotConfigured)
}

func (v execEnv) Authorize(context.Context, execution.Actor, domain.Scope, execution.Permission) error {
	return v.forbidden()
}

func (v execEnv) Work() (execution.WorkAdmission, error) {
	w, ok := workTable.Get(v.tx, projectKey(v.scope, string(v.item)))
	if !ok {
		return execution.WorkAdmission{}, execution.ErrNotFound
	}
	if w.Cancelled {
		return execution.WorkAdmission{}, execution.ErrWorkCancelled
	}
	return execution.WorkAdmission{ContentRevision: w.Revision}, nil
}

func (v execEnv) CapacityAvailable(context.Context, domain.Scope, execution.Limit) error {
	return v.forbidden()
}
func (v execEnv) ReserveCapacity(context.Context, *execution.Attempt) error { return v.forbidden() }
func (v execEnv) ReleaseCapacity(context.Context, *execution.Attempt) error { return v.forbidden() }
func (v execEnv) ReserveWriter(context.Context, *execution.Attempt) error   { return v.forbidden() }
func (v execEnv) SettleWriter(context.Context, *execution.Attempt, execution.WriterOutcome, string) error {
	return v.forbidden()
}
func (v execEnv) ReleaseSecrets(context.Context, *execution.Run) error {
	v.s.env.secrets.Add(1)
	return nil
}

// mutate applies an execution mutation to a copy of the stored run inside tx,
// runs its queued peer effects in order and stores the result; any error leaves
// the stored run untouched and the caller's memdb update rolls back.
func (s *Store) mutate(ctx context.Context, tx memdb.Tx, scope domain.Scope, id domain.RunID, m execution.Mutation) (any, error) {
	row, ok := runTable.Get(tx, projectKey(scope, string(id)))
	if !ok {
		return nil, gates.ErrNotFound
	}
	agg := cloneAggregate(row.agg)
	env := execEnv{s: s, tx: tx, now: s.clock(), scope: scope, item: agg.Run.WorkItemID}
	fx, err := m(ctx, env, agg)
	if err != nil {
		return nil, mapExecError(err)
	}
	for _, after := range fx.After {
		if err := after(ctx, env); err != nil {
			return nil, mapExecError(err)
		}
	}
	agg.Run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}
	if err := runTable.Put(tx, runRow{agg: agg}); err != nil {
		return nil, err
	}
	return fx.Result, nil
}

// mapExecError translates execution-module failures that surface through the
// joined execution mutation.
func mapExecError(err error) error {
	switch {
	case err == nil, gates.IsBusinessError(err):
		return err
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return gates.ErrNotFound
	case errors.Is(err, execution.ErrStale):
		return fmt.Errorf("%w: %v", gates.ErrInvalidTransition, err)
	case errors.Is(err, execution.ErrInvalid), errors.Is(err, execution.ErrInvalidTransition):
		return fmt.Errorf("%w: %v", gates.ErrInvalidTransition, err)
	case errors.Is(err, execution.ErrUnavailable), errors.Is(err, execution.ErrNotConfigured), errors.Is(err, execution.ErrBusy):
		return gates.Unavailablef(err, "the execution module is unavailable")
	}
	return err
}

// ---- mutations --------------------------------------------------------------------

// Request implements gates.Store: the gate row, its gate.requested event and
// the binding of the gate to its pending step commit together. The execution
// inputs the subject was built from are recomputed from the stored run and a
// mismatch refuses the request, so a gate is never bound on stale inputs.
func (s *Store) Request(ctx context.Context, p *gates.RequestPlan) (*gates.Gate, bool, error) {
	if p == nil || p.Gate == nil {
		return nil, false, gates.Invalidf("a gate plan is required")
	}
	g := p.Gate
	var out *gates.Gate
	created := false
	err := s.update(ctx, g.Scope.TenantID, func(tx memdb.Tx) error {
		if existing, ok := byStep(tx, g.Scope, g.RunID, g.StepID); ok {
			out = existing
			return nil
		}
		verify := func(agg *execution.Aggregate) error {
			in, set, err := gates.ResolveInputs(agg, g.StepID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(in, p.Inputs) || !reflect.DeepEqual(set, p.SourceSet) {
				return fmt.Errorf("%w: the gate inputs changed while it was prepared", execution.ErrStale)
			}
			return nil
		}
		after := func(context.Context, execution.Env) error {
			if err := saveGate(tx, g); err != nil {
				return err
			}
			return record(tx, g, "requested", p.Actor, "")
		}
		if _, err := s.mutate(ctx, tx, g.Scope, g.RunID, execution.BindGateMutation(g.StepID, string(g.ID), verify, after)); err != nil {
			return err
		}
		stored := cloneGate(*g)
		stored.MarkClean()
		out, created = &stored, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		g.MarkClean()
	}
	return out, created, nil
}

// runView is the unlocked aggregate a decision resolves the inputs from.
func (s *Store) runView(tx memdb.Tx, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	row, ok := runTable.Get(tx, projectKey(scope, string(id)))
	if !ok {
		return nil, gates.ErrNotFound
	}
	return cloneAggregate(row.agg), nil
}

// loadFacts reads the current facts a gate's subject is compared with, all
// through tx. forceWaiting treats the step as waiting (currency checks of a
// consumed approval).
func (s *Store) loadFacts(ctx context.Context, tx memdb.Tx, g *gates.Gate, agg *execution.Aggregate, forceWaiting bool) (gates.Facts, error) {
	var f gates.Facts
	step := agg.Step(g.StepID)
	if step == nil {
		return f, gates.ErrNotFound
	}
	in, set, err := gates.ResolveInputs(agg, g.StepID)
	if err != nil {
		return f, fmt.Errorf("%w: the gate inputs cannot be resolved: %v", gates.ErrStaleSubject, err)
	}
	f.Inputs, f.SourceSet = in, set
	f.StepWaiting = forceWaiting || (step.Status == execution.StepPending && agg.Run.AcceptsDispatch())
	w, ok := workTable.Get(tx, projectKey(g.Scope, string(in.WorkItemID)))
	if !ok {
		return f, gates.ErrNotFound
	}
	f.ContentRevision, f.Cancelled = w.Revision, w.Cancelled

	f.Evidence = map[domain.ArtifactID]gates.EvidenceFacts{}
	for _, ref := range g.Subject.Evidence() {
		a, ok := artTable.Get(tx, projectKey(g.Scope, string(ref.ID)))
		if !ok {
			f.Evidence[ref.ID] = gates.EvidenceFacts{}
			continue
		}
		f.Evidence[ref.ID] = gates.EvidenceFacts{Found: true, Usable: a.Meta.Usable && !a.Removed, Digest: a.Meta.Digest, Scope: g.Scope}
	}

	if id, _ := g.Subject.Deployment(); id != "" || in.DeploymentID != "" {
		want := in.DeploymentID
		if want == "" {
			want = id
		}
		if s.dep == nil {
			return f, fmt.Errorf("%w: a deployment adapter is required for a deployment gate", gates.ErrNotConfigured)
		}
		snap, err := s.dep(tx).Snapshot(ctx, g.Scope, want)
		switch {
		case err == nil:
			f.Deployment = &snap
		case errors.Is(err, gates.ErrNotFound):
			f.Deployment = nil
		case gates.IsBusinessError(err):
			return f, err
		default:
			return f, gates.Unavailablef(err, "the deployment configuration cannot be read")
		}
	}
	return f, nil
}

// authorizeReviewer checks, through the transaction, that actor still holds
// the reviewer authority.
func (s *Store) authorizeReviewer(ctx context.Context, tx memdb.Tx, actor gates.Actor, scope domain.Scope) error {
	err := s.auth(tx).Authorize(ctx, actor, scope, gates.PermDecide)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gates.ErrHidden):
		return gates.ErrHidden
	case errors.Is(err, gates.ErrDenied):
		return gates.ErrDenied
	}
	return gates.Unavailablef(err, "the authority is unavailable")
}

// Decide implements gates.Store. Under one memdb update it rechecks the
// reviewer's current authority, the expected version, the subject digest, the
// deadline and the current subject facts, applies the one immutable decision
// and records the gate.decided event.
func (s *Store) Decide(ctx context.Context, p *gates.DecisionPlan) (*gates.Gate, error) {
	if p == nil {
		return nil, gates.Invalidf("a decision plan is required")
	}
	if err := p.Scope.Validate(); err != nil || p.GateID.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	var out *gates.Gate
	err := s.update(ctx, p.Scope.TenantID, func(tx memdb.Tx) error {
		g, err := loadGate(tx, p.Scope, p.GateID)
		if err != nil {
			return err
		}
		if err := s.authorizeReviewer(ctx, tx, p.Command.Actor, p.Scope); err != nil {
			return err
		}
		view, err := s.runView(tx, p.Scope, g.RunID)
		if err != nil {
			return err
		}
		facts, err := s.loadFacts(ctx, tx, g, view, false)
		if err != nil {
			return err
		}
		if err := g.Decide(p.Command, facts, s.clock()); err != nil {
			return err
		}
		if err := saveGate(tx, g); err != nil {
			return err
		}
		if err := record(tx, g, "decided", g.Decision.By, g.Decision.Reason); err != nil {
			return err
		}
		out = g
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Invalidate implements gates.Store.
func (s *Store) Invalidate(ctx context.Context, scope domain.Scope, id gates.ID, e *gates.StaleError, _ string) (*gates.Gate, bool, error) {
	if e == nil || !e.Definitive {
		return nil, false, gates.Invalidf("only a definitive staleness is recorded")
	}
	var out *gates.Gate
	created := false
	err := s.update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		g, err := loadGate(tx, scope, id)
		if err != nil {
			return err
		}
		if !g.Invalidate(e.Reason, e.SupersedingCommit, e.SupersedingRevision, s.clock()) {
			out = g
			return nil
		}
		if err := saveGate(tx, g); err != nil {
			return err
		}
		if err := record(tx, g, "invalidated", gates.System, e.Reason); err != nil {
			return err
		}
		out, created = g, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Reevaluate implements gates.Store: a read-only currency check.
func (s *Store) Reevaluate(ctx context.Context, scope domain.Scope, id gates.ID) error {
	return s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		g, err := loadGate(tx, scope, id)
		if err != nil {
			return err
		}
		view, err := s.runView(tx, scope, g.RunID)
		if err != nil {
			return err
		}
		f, err := s.loadFacts(ctx, tx, g, view, true)
		if err != nil {
			return err
		}
		return g.CheckCurrent(f)
	})
}

// CompleteStep implements gates.Store: it rechecks the gate record, the current
// facts and the original reviewer's authority in the same update that performs
// the explicit pending-to-succeeded step transition with no attempt and no
// reservation. The clock is sampled again after the last read, so an approval
// that reached its deadline never authorizes progression.
func (s *Store) CompleteStep(ctx context.Context, scope domain.Scope, id gates.ID) (gates.StepOutcome, error) {
	g, err := s.Get(ctx, scope, id)
	if err != nil {
		return gates.StepOutcome{}, err
	}
	if g.Status != gates.StatusApproved {
		return gates.StepOutcome{}, fmt.Errorf("%w: the gate is %s", gates.ErrInvalidTransition, g.Status)
	}
	var out gates.StepOutcome
	err = s.update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		check := func(ctx context.Context, _ execution.Env, agg *execution.Aggregate, step *execution.StepRun) error {
			if step.GateRef != string(g.ID) {
				return fmt.Errorf("%w: the step is bound to another gate", gates.ErrInvalidTransition)
			}
			cur, err := s.checkConsumable(ctx, tx, g.ID, scope, agg)
			if err != nil {
				return err
			}
			return cur.Applicable(s.clock())
		}
		res, err := s.mutate(ctx, tx, scope, g.RunID, execution.CompleteGateMutation(g.StepID, check))
		if err != nil {
			return err
		}
		if r, ok := res.(execution.GateStepOutcome); ok {
			out = gates.StepOutcome{Completed: r.Completed, Cancelled: r.Cancelled, Waiting: r.Status == execution.StepPending}
		}
		return nil
	})
	if err != nil {
		return gates.StepOutcome{}, err
	}
	return out, nil
}

// checkConsumable is the progression check of one gate inside the update that
// holds the run: the approval must be applicable against the current facts and
// the original reviewer must still hold the reviewer authority.
func (s *Store) checkConsumable(ctx context.Context, tx memdb.Tx, id gates.ID, scope domain.Scope, agg *execution.Aggregate) (*gates.Gate, error) {
	g, err := loadGate(tx, scope, id)
	if err != nil {
		return nil, err
	}
	f, err := s.loadFacts(ctx, tx, g, agg, true)
	if err != nil {
		return nil, err
	}
	if err := g.Revalidate(f, s.clock()); err != nil {
		return nil, err
	}
	if g.Decision == nil {
		return nil, fmt.Errorf("%w: the gate has no decision", gates.ErrInvalidTransition)
	}
	switch err := s.authorizeReviewer(ctx, tx, g.Decision.By, scope); {
	case err == nil:
		return g, nil
	case errors.Is(err, gates.ErrDenied), errors.Is(err, gates.ErrHidden):
		return nil, fmt.Errorf("%w: the deciding reviewer no longer holds the reviewer authority", gates.ErrReviewerAuthority)
	default:
		return nil, err
	}
}

// RejectStep implements gates.Store.
func (s *Store) RejectStep(ctx context.Context, scope domain.Scope, id gates.ID) (gates.StepOutcome, error) {
	g, err := s.Get(ctx, scope, id)
	if err != nil {
		return gates.StepOutcome{}, err
	}
	if g.Status != gates.StatusRejected {
		return gates.StepOutcome{}, fmt.Errorf("%w: the gate is %s", gates.ErrInvalidTransition, g.Status)
	}
	var out gates.StepOutcome
	err = s.update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		res, err := s.mutate(ctx, tx, scope, g.RunID, execution.RejectGateMutation(g.StepID))
		if err != nil {
			return err
		}
		if r, ok := res.(execution.GateStepOutcome); ok {
			out = gates.StepOutcome{Rejected: r.Rejected, Waiting: r.Status == execution.StepPending}
		}
		return nil
	})
	if err != nil {
		return gates.StepOutcome{}, err
	}
	return out, nil
}

// ---- scans ------------------------------------------------------------------------

func nonterminal(a *execution.Aggregate) bool { return !a.Run.Status.Terminal() }

// ReadyGateSteps implements gates.Store: pending, unbound gate steps of
// nonterminal runs without cancellation or failure intent, ordered by
// (project, run, step) after the given position.
func (s *Store) ReadyGateSteps(ctx context.Context, tenant domain.TenantID, after gates.StepRef, limit int) ([]gates.StepRef, error) {
	if err := tenant.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	var all []gates.StepRef
	err := s.view(ctx, tenant, func(tx memdb.Tx) error {
		runTable.Scan(tx, func(r runRow) bool {
			a := r.agg
			if !nonterminal(a) || a.Run.CancelRequestedAt != nil || a.Run.FailureIntentAt != nil {
				return true
			}
			for _, st := range a.Steps {
				if st.Spec.Kind == pipeline.KindGate && st.Status == execution.StepPending && st.GateRef == "" {
					all = append(all, gates.StepRef{Scope: a.Run.Scope, RunID: a.Run.ID, StepID: st.ID})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	less := func(a, b gates.StepRef) bool {
		if a.Scope.ProjectID != b.Scope.ProjectID {
			return a.Scope.ProjectID < b.Scope.ProjectID
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.StepID < b.StepID
	}
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	var out []gates.StepRef
	for _, r := range all {
		if !less(after, r) {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// OpenGates implements gates.Store: gates of nonterminal runs that were not
// invalidated, ordered by (project, id).
func (s *Store) OpenGates(ctx context.Context, tenant domain.TenantID, after gates.GateRef, limit int) ([]gates.GateRef, error) {
	if err := tenant.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	var all []gates.GateRef
	err := s.view(ctx, tenant, func(tx memdb.Tx) error {
		gateTable.Scan(tx, func(g gates.Gate) bool {
			if g.Invalidation != nil {
				return true
			}
			row, ok := runTable.Get(tx, projectKey(g.Scope, string(g.RunID)))
			if !ok || !nonterminal(row.agg) {
				return true
			}
			all = append(all, gates.GateRef{Scope: g.Scope, ID: g.ID})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	less := func(a, b gates.GateRef) bool {
		if a.Scope.ProjectID != b.Scope.ProjectID {
			return a.Scope.ProjectID < b.Scope.ProjectID
		}
		return a.ID < b.ID
	}
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	var out []gates.GateRef
	for _, r := range all {
		if !less(after, r) {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
