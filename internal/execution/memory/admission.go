package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Admit implements execution.Store in its own update.
func (s *Store) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	if p == nil || p.Aggregate == nil || p.Aggregate.Run == nil {
		return nil, false, execution.Invalidf("an admission plan is required")
	}
	var run *execution.Run
	var created bool
	err := s.inTenant(ctx, p.Aggregate.Run.Scope.TenantID, func(tx memdb.Tx) (err error) {
		run, created, err = s.AdmitInTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return run, created, nil
}

// AdmitInTx creates the pinned run inside a caller-owned transaction of the
// run's tenant. Under the work item's admission it verifies the pinned content
// revision, rechecks the requester's authority, inserts the run and its steps,
// registers the secret pins and records the run-bound retained-source intent
// through the workspace peer; the run's history, audit and events commit with
// them. A repeat of the same run ID returns the stored run and changes nothing.
func (s *Store) AdmitInTx(ctx context.Context, tx memdb.Tx, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	agg := p.Aggregate
	run := agg.Run
	scope := run.Scope
	if err := scope.Validate(); err != nil {
		return nil, false, execution.Invalidf("%v", err)
	}
	if tx.Tenant() != scope.TenantID {
		return nil, false, execution.ErrNotFound
	}
	if existing, err := s.loadRun(tx, scope, run.ID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, execution.ErrNotFound) {
		return nil, false, err
	}
	env := s.newEnv(tx, scope)
	ctx = execution.WithActor(ctx, p.Actor)
	if p.RequestID != "" {
		ctx = execution.WithRequestID(ctx, p.RequestID)
	}
	var pinID string
	if run.Purpose == execution.PurposeWork {
		env.takeWork(run.WorkItemID)
		adm, err := env.Work()
		if err != nil {
			return nil, false, err
		}
		if adm.ContentRevision != p.WorkRevision {
			return nil, false, fmt.Errorf("%w: the work item's specification changed during admission", execution.ErrConflict)
		}
		if err := env.Authorize(ctx, p.Actor, scope, execution.PermRunManage); err != nil {
			switch {
			case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrHidden):
				return nil, false, execution.ErrDenied
			}
			return nil, false, execution.Unavailablef(err, "the authority is unavailable")
		}
		pinID = s.opts.NewID("pin")
		run.RetainedRefID = pinID
	}
	if err := s.insertRun(tx, run); err != nil {
		return nil, false, err
	}
	for _, st := range agg.Steps {
		if err := s.insertStep(tx, scope, st); err != nil {
			return nil, false, err
		}
	}
	if run.Purpose == execution.PurposeWork {
		if len(p.Secrets) > 0 {
			if err := s.peers.Secrets.RegisterPins(tx, scope, run.ID, p.Secrets); err != nil {
				return nil, false, err
			}
		}
		if err := s.peers.Workspaces.RetainSource(tx, run, pinID); err != nil {
			return nil, false, err
		}
	}
	first := []execution.Transition{{Subject: execution.SubjectRun, ID: string(run.ID), To: string(run.Status), Reason: execution.ReasonCreated, At: run.CreatedAt}}
	for _, st := range agg.Steps {
		first = append(first, execution.Transition{Subject: execution.SubjectStep, ID: st.ID, StepID: st.ID, To: string(execution.StepPending), Reason: execution.ReasonCreated, At: run.CreatedAt})
	}
	// The creation transitions are staged on a copy: the plan is reusable, so a
	// repeat after a lost commit writes the same history and event identities
	// instead of creation rows stacked on the failed attempt's.
	staged := *agg
	staged.Transitions = append(first, agg.Transitions...)
	if err := s.writeTransitions(ctx, tx, &staged); err != nil {
		return nil, false, err
	}
	markClean(agg)
	return run, true, nil
}

// RequestCancellation implements execution.Store in its own update.
func (s *Store) RequestCancellation(ctx context.Context, c execution.WorkCancellation) error {
	if err := c.Scope.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	if err := c.WorkItemID.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	return s.inTenant(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		return s.RequestCancellationInTx(ctx, tx, c)
	})
}

// RequestCancellationInTx records the item-scoped intent idempotently and
// cancels the item's nonterminal runs in the caller's transaction: dispatch is
// refused from the commit on, live attempts lose authority and the runs settle
// before completing. A duplicate or reordered delivery repeats an idempotent
// request, and a run created later is refused by the admission check. The
// request is intent and revocation only: a writer slot or capacity reservation
// is released by the later settlement, never by this call.
func (s *Store) RequestCancellationInTx(ctx context.Context, tx memdb.Tx, c execution.WorkCancellation) error {
	if err := c.Scope.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	if err := c.WorkItemID.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	if tx.Tenant() != c.Scope.TenantID {
		return execution.ErrNotFound
	}
	k := key(string(c.Scope.ProjectID), string(c.WorkItemID))
	if cur, ok := cancelsT.Get(tx, k); !ok || cur.Version < c.Version {
		if err := cancelsT.Put(tx, cancelRequest{Scope: c.Scope, Item: c.WorkItemID, Version: c.Version, At: s.now()}); err != nil {
			return err
		}
	}
	runs := collect(tx, runsT, func(r execution.Run) bool {
		return r.Scope == c.Scope && r.WorkItemID == c.WorkItemID && (r.Status == execution.RunQueued || r.Status == execution.RunRunning)
	})
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	pending := make([]*pendingUpdate, 0, len(runs))
	for _, r := range runs {
		p, err := s.mutate(ctx, tx, c.Scope, r.ID, execution.UpdateOptions{}, execution.CancelMutation(execution.ReasonWorkCancelled, nil))
		if err != nil {
			return err
		}
		pending = append(pending, p)
	}
	for _, p := range pending {
		if err := p.settleWriters(); err != nil {
			return err
		}
	}
	for _, p := range pending {
		if err := s.record(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}

// Inspection reads for tests and fixtures: the append-only history, audit,
// evidence and reservation rows. They are not part of execution.Store.

// History returns the run's history rows, oldest first.
func (s *Store) History(ctx context.Context, scope domain.Scope, run domain.RunID) ([]History, error) {
	var out []History
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = collect(tx, historyT, func(h History) bool { return h.Scope == scope && h.Run == run })
		sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
		return nil
	})
	return out, err
}

// AuditRows returns the project's audit rows, oldest first.
func (s *Store) AuditRows(ctx context.Context, scope domain.Scope) ([]Audit, error) {
	var out []Audit
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = collect(tx, auditT, func(a Audit) bool { return a.Scope == scope })
		for i := range out {
			var err error
			if out[i].Metadata, err = cloneDetail(out[i].Metadata); err != nil {
				return err
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
		return nil
	})
	return out, err
}

// EvidenceOf returns an attempt's evidence rows, oldest first.
func (s *Store) EvidenceOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]Evidence, error) {
	var out []Evidence
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = collect(tx, evidenceT, func(e Evidence) bool { return e.Scope == scope && e.Attempt == attempt })
		for i := range out {
			var err error
			if out[i].Detail, err = cloneDetail(out[i].Detail); err != nil {
				return err
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
		return nil
	})
	return out, err
}

// Reservations returns the tenant's capacity reservations, held or released.
func (s *Store) Reservations(ctx context.Context, tenant domain.TenantID) ([]Reservation, error) {
	var out []Reservation
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) error {
		out = collect(tx, reservationsT, nil)
		for i := range out {
			out[i].ReleasedAt = cloneTime(out[i].ReleasedAt)
		}
		return nil
	})
	return out, err
}
