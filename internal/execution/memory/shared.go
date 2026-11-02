package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// The run_shared rows: resources, workspace holders, command-begin records and
// the run-level capacity reservation. They follow the same discipline as the
// rest of the store: copied on every crossing, guarded in Go as the PostgreSQL
// triggers guard them (UID, stop, removal and receipt are written once;
// removed resources and spec identity never change), committed or rolled back
// with the rest of the update.

type resourceRow struct {
	Project  domain.ProjectID
	Run      domain.RunID
	Resource execution.RunResource
}

type holderRow struct {
	Project domain.ProjectID
	Holder  execution.RunHolder
}

type beginRow struct {
	Project domain.ProjectID
	Begin   execution.BeginRecord
}

// RunReservation is one run-level capacity reservation (all services,
// preparation, the largest active attempt and the scratch volume).
type RunReservation struct {
	Scope      domain.Scope
	Run        domain.RunID
	Units      execution.CapacityUnits
	HeldAt     time.Time
	ReleasedAt *time.Time
}

// Held reports whether the reservation still counts against capacity.
func (r RunReservation) Held() bool { return r.ReleasedAt == nil }

var (
	resourcesT = memdb.NewTable("execution_run_resources", func(r resourceRow) string {
		return key(string(r.Project), string(r.Run), r.Resource.Key)
	})
	holdersT = memdb.NewTable("execution_run_holders", func(h holderRow) string {
		return key(string(h.Project), string(h.Holder.RunID), string(h.Holder.AttemptID))
	})
	beginsT = memdb.NewTable("execution_attempt_begins", func(b beginRow) string {
		return key(string(b.Project), string(b.Begin.AttemptID))
	})
	runReservationsT = memdb.NewTable("execution_run_reservations", func(r RunReservation) string {
		return key(string(r.Scope.ProjectID), string(r.Run))
	})
)

// cloneResource deep-copies a resource through its stored (JSON) form, the same
// boundary the PostgreSQL adapter stores the spec across.
func cloneResource(r execution.RunResource) (execution.RunResource, error) {
	b, err := json.Marshal(r.Spec)
	if err != nil {
		return r, err
	}
	var spec execution.ResourceSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return r, err
	}
	r.Spec = spec
	if r.ReceiptV3 != nil {
		c := *r.ReceiptV3
		c.Sources = append([]runtimev3.PreparedSource(nil), c.Sources...)
		r.ReceiptV3 = &c
	}
	if r.Receipt != nil {
		c := *r.Receipt
		r.Receipt = &c
	}
	for _, p := range []**time.Time{&r.EnsureRequestedAt, &r.BoundAt, &r.ObservedAt, &r.ReadyAt, &r.NotReadySince, &r.UnknownSince, &r.ReleasedAt,
		&r.ReceiptAt, &r.StopRequestedAt, &r.StoppedAt, &r.RemovedAt, &r.NextActionAt, &r.ObserveRequestedAt} {
		*p = cloneTime(*p)
	}
	return r, nil
}

func cloneShared(s *execution.RunShared) (*execution.RunShared, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out execution.RunShared
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) loadShared(tx memdb.Tx, agg *execution.Aggregate) error {
	scope, id := agg.Run.Scope, agg.Run.ID
	for _, row := range collect(tx, resourcesT, func(r resourceRow) bool { return r.Project == scope.ProjectID && r.Run == id }) {
		r, err := cloneResource(row.Resource)
		if err != nil {
			return err
		}
		agg.Resources = append(agg.Resources, &r)
	}
	sort.Slice(agg.Resources, func(i, j int) bool { return agg.Resources[i].Ordinal < agg.Resources[j].Ordinal })
	for _, row := range collect(tx, holdersT, func(h holderRow) bool { return h.Project == scope.ProjectID && h.Holder.RunID == id }) {
		h := row.Holder
		h.ReleasedAt = cloneTime(h.ReleasedAt)
		agg.Holders = append(agg.Holders, &h)
	}
	sort.Slice(agg.Holders, func(i, j int) bool { return agg.Holders[i].HeldAt.Before(agg.Holders[j].HeldAt) })
	for _, row := range collect(tx, beginsT, func(b beginRow) bool { return b.Project == scope.ProjectID && b.Begin.RunID == id }) {
		b := row.Begin
		agg.Begins = append(agg.Begins, &b)
	}
	sort.Slice(agg.Begins, func(i, j int) bool { return agg.Begins[i].BeganAt.Before(agg.Begins[j].BeganAt) })
	return nil
}

func (s *Store) persistShared(tx memdb.Tx, agg *execution.Aggregate) error {
	scope := agg.Run.Scope
	created := map[string]bool{}
	for _, r := range agg.NewResources {
		created[r.Key] = true
		if _, dup := resourcesT.Get(tx, key(string(scope.ProjectID), string(agg.Run.ID), r.Key)); dup {
			return fmt.Errorf("%w: the run resource exists", execution.ErrConflict)
		}
		for range collect(tx, resourcesT, func(o resourceRow) bool { return o.Resource.Name == r.Name }) {
			return fmt.Errorf("%w: the deterministic resource name is taken", execution.ErrConflict)
		}
		cp, err := cloneResource(*r)
		if err != nil {
			return err
		}
		cp.MarkClean()
		if err := resourcesT.Put(tx, resourceRow{Project: scope.ProjectID, Run: agg.Run.ID, Resource: cp}); err != nil {
			return err
		}
	}
	for _, r := range agg.Resources {
		if created[r.Key] || !r.Dirty() {
			continue
		}
		if err := s.updateResource(tx, scope, r); err != nil {
			return err
		}
	}
	for _, h := range agg.NewHolders {
		// One unreleased holder per run, atomically with the attempt row.
		for _, other := range collect(tx, holdersT, func(o holderRow) bool {
			return o.Project == scope.ProjectID && o.Holder.RunID == agg.Run.ID && o.Holder.ReleasedAt == nil
		}) {
			return immutablef("the run workspace already has holder %s", other.Holder.AttemptID)
		}
		cp := *h
		cp.MarkClean()
		if err := holdersT.Put(tx, holderRow{Project: scope.ProjectID, Holder: cp}); err != nil {
			return err
		}
	}
	newHolder := map[domain.AttemptID]bool{}
	for _, h := range agg.NewHolders {
		newHolder[h.AttemptID] = true
	}
	for _, h := range agg.Holders {
		if newHolder[h.AttemptID] || !h.Dirty() {
			continue
		}
		row, ok := holdersT.Get(tx, key(string(scope.ProjectID), string(agg.Run.ID), string(h.AttemptID)))
		if !ok {
			return execution.ErrNotFound
		}
		if row.Holder.ReleasedAt != nil && (h.ReleasedAt == nil || !h.ReleasedAt.Equal(*row.Holder.ReleasedAt)) {
			return immutablef("a holder release is recorded once")
		}
		row.Holder.ReleasedAt = cloneTime(h.ReleasedAt)
		if err := holdersT.Put(tx, row); err != nil {
			return err
		}
	}
	for _, b := range agg.NewBegins {
		if _, dup := beginsT.Get(tx, key(string(scope.ProjectID), string(b.AttemptID))); dup {
			return fmt.Errorf("%w: the begin record exists", execution.ErrConflict)
		}
		if err := beginsT.Put(tx, beginRow{Project: scope.ProjectID, Begin: *b}); err != nil {
			return err
		}
	}
	return nil
}

// updateResource applies the same guards the PostgreSQL trigger and constraints
// apply (t112_run_resources_guard and the table checks): a removed resource, the
// identity, spec and fingerprint, the bound UID and its time, the stop intent
// and its reason, the verified stop and its launch evidence, the preparation
// receipt body, digest and time, the first ready time and an ensure intent are
// all written once; generations only grow; a removal needs a verified stop; a
// stopped resource is never ready; (backend, UID) is unique. A violation is
// rejected, never silently coerced.
func (s *Store) updateResource(tx memdb.Tx, scope domain.Scope, r *execution.RunResource) error {
	row, ok := resourcesT.Get(tx, key(string(scope.ProjectID), string(r.RunID), r.Key))
	if !ok {
		return execution.ErrNotFound
	}
	cur := row.Resource
	if cur.RemovedAt != nil {
		return immutablef("a removed run resource is immutable")
	}
	if r.Version != cur.Version+1 {
		return execution.ErrStale
	}
	if r.Scope != cur.Scope || r.RunID != cur.RunID || r.Key != cur.Key || r.Kind != cur.Kind || r.Name != cur.Name || r.Ordinal != cur.Ordinal ||
		r.Backend != cur.Backend || r.Fingerprint != cur.Fingerprint || !r.CreatedAt.Equal(cur.CreatedAt) || !sameSpec(r.Spec, cur.Spec) {
		return immutablef("the identity, spec and fingerprint of a run resource are immutable")
	}
	if cur.UID != "" && (r.UID != cur.UID || !sameTime(cur.BoundAt, r.BoundAt)) {
		return immutablef("the bound resource UID is immutable")
	}
	if (r.UID == "") != (r.BoundAt == nil) {
		return immutablef("a bound UID carries its binding time")
	}
	if cur.StopRequestedAt != nil && (!sameTime(cur.StopRequestedAt, r.StopRequestedAt) || cur.StopReason != r.StopReason) {
		return immutablef("the stop intent is recorded once")
	}
	if cur.StoppedAt != nil && (!sameTime(cur.StoppedAt, r.StoppedAt) || cur.StopLaunch != r.StopLaunch) {
		return immutablef("a verified stop is immutable")
	}
	if cur.Receipt != nil && (r.Receipt == nil || *r.Receipt != *cur.Receipt || r.ReceiptDigest != cur.ReceiptDigest || !sameTime(cur.ReceiptAt, r.ReceiptAt)) {
		return immutablef("the preparation receipt is recorded once")
	}
	if cur.ReceiptV3 != nil && (r.ReceiptV3 == nil || r.ReceiptDigest != cur.ReceiptDigest || !sameTime(cur.ReceiptAt, r.ReceiptAt) || !sameReceiptV3(*cur.ReceiptV3, *r.ReceiptV3)) {
		return immutablef("the preparation receipt is recorded once")
	}
	if cur.ReadyAt != nil && !sameTime(cur.ReadyAt, r.ReadyAt) {
		return immutablef("the first ready time is immutable")
	}
	if cur.EnsureRequestedAt != nil && r.EnsureRequestedAt == nil {
		return immutablef("an ensure intent is never forgotten")
	}
	if r.ObserveGen < cur.ObserveGen || r.AppliedGen < cur.AppliedGen || r.AppliedGen > r.ObserveGen {
		return immutablef("observation generations only grow and never pass the requested one")
	}
	if r.StoppedAt != nil && r.Phase == execution.PhaseReady {
		return immutablef("a stopped resource is never started again")
	}
	if r.RemovedAt != nil && r.StoppedAt == nil {
		return immutablef("a resource is removed only after a verified stop")
	}
	if r.StoppedAt != nil && (r.StopRequestedAt == nil || r.StopLaunch == "") {
		return immutablef("a verified stop carries its intent and launch evidence")
	}
	if r.HasReceipt() && (r.Kind != execution.KindPreparation || r.UID == "" || r.ReceiptDigest == "" || r.ReceiptAt == nil) {
		return immutablef("a receipt belongs to a bound preparation with its digest and time")
	}
	if r.UID != "" {
		for _, other := range collect(tx, resourcesT, func(o resourceRow) bool {
			return o.Resource.Backend == r.Backend && o.Resource.UID == r.UID && !(o.Project == scope.ProjectID && o.Run == r.RunID && o.Resource.Key == r.Key)
		}) {
			_ = other
			return fmt.Errorf("%w: the provider instance is bound to another run resource", execution.ErrConflict)
		}
	}
	cp, err := cloneResource(*r)
	if err != nil {
		return err
	}
	cp.MarkClean()
	return resourcesT.Put(tx, resourceRow{Project: scope.ProjectID, Run: r.RunID, Resource: cp})
}

func sameSpec(a, b execution.ResourceSpec) bool {
	x, err1 := a.Canonical()
	y, err2 := b.Canonical()
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func markCleanShared(agg *execution.Aggregate) {
	for _, r := range agg.Resources {
		r.MarkClean()
	}
	for _, h := range agg.Holders {
		h.MarkClean()
	}
	agg.NewResources, agg.NewHolders, agg.NewBegins = nil, nil, nil
}

// ReserveRunCapacity implements execution.SharedEnv. It counts the held run
// reservations of the tenant and of the project under the quota locks.
func (e *txEnv) ReserveRunCapacity(_ context.Context, run *execution.Run, units execution.CapacityUnits, lim execution.Limit) error {
	if !e.quotaLocked || run.Scope != e.scope {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	k := key(string(run.Scope.ProjectID), string(run.ID))
	if _, ok := runReservationsT.Get(e.tx, k); ok {
		return nil
	}
	var tenantHeld, projectHeld execution.CapacityUnits
	for _, r := range collect(e.tx, runReservationsT, RunReservation.Held) {
		tenantHeld = tenantHeld.Add(r.Units)
		if r.Scope.ProjectID == run.Scope.ProjectID {
			projectHeld = projectHeld.Add(r.Units)
		}
	}
	if !units.Fits(tenantHeld, lim.SharedTenant) || !units.Fits(projectHeld, lim.SharedProject) {
		return fmt.Errorf("%w: the run's service, preparation, attempt and scratch capacity does not fit", execution.ErrQuotaExceeded)
	}
	// The run also takes one tenant/project active-attempt slot, consumed by every
	// successive attempt of the run without a second charge; slots held by
	// attempts of other runs and by other shared runs count.
	if err := e.CapacityAvailable(context.Background(), run.Scope, lim); err != nil {
		return err
	}
	return runReservationsT.Put(e.tx, RunReservation{Scope: run.Scope, Run: run.ID, Units: units, HeldAt: e.s.now()})
}

// ReleaseRunCapacity releases the run reservation once; a repeat is a no-op.
func (e *txEnv) ReleaseRunCapacity(_ context.Context, run *execution.Run) error {
	r, ok := runReservationsT.Get(e.tx, key(string(run.Scope.ProjectID), string(run.ID)))
	if !ok || r.ReleasedAt != nil {
		return nil
	}
	t := e.s.now()
	r.ReleasedAt = &t
	return runReservationsT.Put(e.tx, r)
}

var _ execution.SharedEnv = (*txEnv)(nil)
var _ execution.SharedStore = (*Store)(nil)

// UnfinishedResources implements execution.SharedStore: terminal runs that
// still have a resource not verified removed.
func (s *Store) UnfinishedResources(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
	if limit < 1 || limit > 1000 {
		return nil, execution.Invalidf("limit must be between 1 and 1000")
	}
	var out []execution.RunRef
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) (err error) {
		pending := map[string]bool{}
		for _, row := range collect(tx, resourcesT, func(r resourceRow) bool { return r.Resource.RemovedAt == nil }) {
			pending[key(string(row.Project), string(row.Run))] = true
		}
		out, err = s.scanRuns(tx, after, limit, func(r execution.Run) bool {
			return r.Status.Terminal() && pending[runKey(r.Scope, r.ID)]
		})
		return err
	})
	return out, err
}

// RunReservations returns the tenant's run-level reservations (inspection).
func (s *Store) RunReservations(ctx context.Context, tenant domain.TenantID) ([]RunReservation, error) {
	var out []RunReservation
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) error {
		out = collect(tx, runReservationsT, nil)
		sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
		return nil
	})
	return out, err
}

// ForceTerminalForTest models a run left terminal (failed) by an interrupted
// older controller while its resources were never verified removed. It exists
// for fixtures only: the in-memory store must never be selected in production,
// and the entities never allow a shared run to finish with unremoved resources.
func (s *Store) ForceTerminalForTest(ctx context.Context, scope domain.Scope, run domain.RunID) error {
	return s.inTenant(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := runsT.Get(tx, runKey(scope, run))
		if !ok {
			return execution.ErrNotFound
		}
		t := s.now()
		r.Status, r.Reason, r.FinishedAt = execution.RunFailed, execution.ReasonRunFailed, &t
		r.FailureIntentAt, r.FailureReason = &t, execution.ReasonRunFailed
		r.Version++
		return runsT.Put(tx, r)
	})
}

// CorruptPolicyForTest makes the run's retained policy body stop verifying
// against its pinned digest, modeling a lost or damaged retained policy. Fixture
// only: the PostgreSQL guard forbids any rewrite of the pins.
func (s *Store) CorruptPolicyForTest(ctx context.Context, scope domain.Scope, run domain.RunID) error {
	return s.inTenant(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := runsT.Get(tx, runKey(scope, run))
		if !ok || r.Shared == nil {
			return execution.ErrNotFound
		}
		c := cloneRun(r)
		c.Shared.PolicyBody.Limits.StopGraceSeconds = 99
		return runsT.Put(tx, c)
	})
}

// CorruptModelForTest makes the run's pinned run model one this controller does
// not implement. Fixture only (the PostgreSQL guard forbids rewriting pins).
func (s *Store) CorruptModelForTest(ctx context.Context, scope domain.Scope, run domain.RunID) error {
	return s.inTenant(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := runsT.Get(tx, runKey(scope, run))
		if !ok || r.Shared == nil {
			return execution.ErrNotFound
		}
		c := cloneRun(r)
		c.Shared.RunModel = "run_shared_v9"
		return runsT.Put(tx, c)
	})
}

// sameReceiptV3 compares two source-set receipts field by field.
func sameReceiptV3(a, b runtimev3.PreparationReceiptRequestV3) bool {
	if a.HooksDisabled != b.HooksDisabled || a.Submodules != b.Submodules || a.CredentialsRemoved != b.CredentialsRemoved ||
		a.Attempts != b.Attempts || len(a.Sources) != len(b.Sources) {
		return false
	}
	for i := range a.Sources {
		if a.Sources[i] != b.Sources[i] {
			return false
		}
	}
	return true
}
