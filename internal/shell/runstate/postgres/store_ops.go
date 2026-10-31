package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// Admit records the run, its plan, its pending waits and its events in one
// transaction. It is idempotent on (scope, run id): a repeat with the same plan
// digest returns the stored record and writes nothing; another digest is
// domain.ErrConflict. A pipeline that does not exist in the scope refuses the
// admission (the runs row references it).
func (s *Store) Admit(ctx context.Context, a runtime.Admission) (runtime.RunRecord, error) {
	if err := a.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	r := a.Run
	if r.Version != 1 || r.CancelRequested || r.Outcome != runtime.OutcomeNone || r.Cleanup.State != runtime.CleanupNotStarted ||
		r.Cleanup.Attempts != 0 || r.Cleanup.Retained != nil || r.Cleanup.RetainedWorkloads != nil || r.Cleanup.Reason != runtime.ReasonNone {
		return runtime.RunRecord{}, invalid("an admitted run is new: version 1, not cancelled, not settled")
	}
	for _, st := range r.Steps {
		if st.Version != 1 || st.Attempt != 0 || st.Reason != runtime.ReasonNone || st.Result != (runtime.Result{}) || st.Predicate.Evaluated {
			return runtime.RunRecord{}, invalid("step %s of an admitted run is new", st.Key)
		}
	}
	waits := map[domain.StepID]runtime.Wait{}
	for _, w := range a.Waits {
		waits[w.StepKey] = w
	}
	events := make([]runtime.Event, len(a.Events))
	for i, e := range a.Events {
		e.At = micro(e.At)
		events[i] = e
	}

	var out runtime.RunRecord
	err := s.update(ctx, r.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, stmtInsertRun, r.Scope.TenantID, r.Scope.ProjectID, r.ID, r.PipelineID, jsonOf(a.Plan),
			int64(r.Version), r.Cleanup.MaxAttempts)
		if err != nil {
			return fmt.Errorf("runstate/postgres: insert run: %w", err)
		}
		if tag.RowsAffected() == 0 {
			prev, err := loadRecord(ctx, tx, r.Scope, r.ID, false)
			if err != nil {
				return err
			}
			if prev.Plan.Digest != a.Plan.Digest {
				return fmt.Errorf("%w: run %s is admitted with plan %s, not %s", domain.ErrConflict, r.ID, prev.Plan.Digest, a.Plan.Digest)
			}
			out = prev
			return nil
		}
		for i, st := range r.Steps {
			var image, waitID, assignment, waitVersion any
			if st.Kind == runtime.StepContainer {
				image = a.Plan.Steps[i].Workload.Image
			} else {
				w := waits[st.Key]
				waitID, assignment, waitVersion = string(w.ID), nz(w.Assignment), int64(w.Version)
			}
			if _, err := tx.Exec(ctx, stmtInsertStep, r.Scope.TenantID, r.Scope.ProjectID, r.ID, string(st.Key), string(st.Key), i,
				image, string(st.Kind), int64(st.Version), waitID, assignment, waitVersion); err != nil {
				return fmt.Errorf("runstate/postgres: insert step %s: %w", st.Key, err)
			}
		}
		if err := emit(ctx, tx, events); err != nil {
			return err
		}
		out, err = loadRecord(ctx, tx, r.Scope, r.ID, false)
		return err
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return out, nil
}

// Load returns the run record, or a not-found error for a run that is not in
// the scope.
func (s *Store) Load(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunRecord, error) {
	if err := checkRef(scope, id); err != nil {
		return runtime.RunRecord{}, err
	}
	var out runtime.RunRecord
	err := s.view(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = loadRecord(ctx, tx, scope, id, false)
		return err
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return out, nil
}

// View rebuilds the read model from the retained run, step and wait rows.
func (s *Store) View(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunView, error) {
	rec, err := s.Load(ctx, scope, id)
	if err != nil {
		return runtime.RunView{}, err
	}
	return runtime.RunView{Run: rec.Run, Waits: rec.Waits, Status: rec.Run.Status()}, nil
}

// Claim grants a lease through runtime.AcquireClaim for a purpose the run
// admits. The run row lock makes concurrent claimants take turns: exactly one
// of them takes a free or expired lease, the others see it held. The fence is
// the stored fence plus one, so it only grows (the run guard trigger refuses
// anything else).
func (s *Store) Claim(ctx context.Context, req runtime.ClaimRequest, now time.Time) (runtime.RunRecord, error) {
	if err := req.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	if now.IsZero() {
		return runtime.RunRecord{}, invalid("a claim needs the time")
	}
	now = micro(now)
	var out runtime.RunRecord
	err := s.update(ctx, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := loadRecord(ctx, tx, req.Scope, req.RunID, true)
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
		if _, err := tx.Exec(ctx, stmtSetLease, req.Scope.TenantID, req.Scope.ProjectID, req.RunID, c.Owner, string(c.Purpose),
			int64(c.Fence), c.Expires); err != nil {
			return fmt.Errorf("runstate/postgres: take the lease: %w", err)
		}
		out, err = loadRecord(ctx, tx, req.Scope, req.RunID, false)
		return err
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return out, nil
}

// Renew extends the holder's lease through runtime.RenewClaim, checked against
// the stored claim under the row lock; the fence never changes.
func (s *Store) Renew(ctx context.Context, held runtime.Claim, ttl time.Duration, now time.Time) (runtime.Claim, error) {
	if err := checkRef(held.Scope, held.RunID); err != nil {
		return runtime.Claim{}, err
	}
	now = micro(now)
	var out runtime.Claim
	err := s.update(ctx, held.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := loadRecord(ctx, tx, held.Scope, held.RunID, true)
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
		if _, err := tx.Exec(ctx, stmtSetLeaseExpiry, held.Scope.TenantID, held.Scope.ProjectID, held.RunID, c.Expires); err != nil {
			return fmt.Errorf("runstate/postgres: renew the lease: %w", err)
		}
		out = c
		out.Expires = micro(c.Expires)
		return nil
	})
	if err != nil {
		return runtime.Claim{}, err
	}
	return out, nil
}

// Release ends the holder's lease now without ending the run, through
// runtime.ReleaseClaim: the owner, purpose and fence stay, so the next claim is
// a normal takeover. It is a no-op for a claim the caller no longer holds.
func (s *Store) Release(ctx context.Context, held runtime.Claim, now time.Time) error {
	if err := checkRef(held.Scope, held.RunID); err != nil {
		return err
	}
	now = micro(now)
	return s.update(ctx, held.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := loadRecord(ctx, tx, held.Scope, held.RunID, true)
		if err != nil {
			return err
		}
		c, released, err := runtime.ReleaseClaim(rec.Claim, held, now)
		if err != nil || !released {
			return err
		}
		if _, err := tx.Exec(ctx, stmtSetLeaseExpiry, held.Scope.TenantID, held.Scope.ProjectID, held.RunID, c.Expires); err != nil {
			return fmt.Errorf("runstate/postgres: release the lease: %w", err)
		}
		return nil
	})
}

// Runnable lists runs with claimable work, bounded by q.Limit: runs with no
// unexpired lease at q.Now that are not terminal. The purpose is the one the run
// admits: execute for a running run, reconcile once cancellation is requested,
// cleanup once execution settled. A run whose cleanup is exhausted is not
// listed. Each tenant is read in its own tenant transaction; results are ordered
// by tenant, project and run.
func (s *Store) Runnable(ctx context.Context, q runtime.RunnableQuery) ([]runtime.RunRef, error) {
	if q.Limit < 1 {
		return nil, invalid("a runnable query is bounded by a positive limit")
	}
	if q.Now.IsZero() {
		return nil, invalid("a runnable query needs the time")
	}
	now := micro(q.Now)
	tenants, err := storage.ListTenantIDs(ctx, s.db)
	if err != nil {
		return nil, err
	}
	var out []runtime.RunRef
	for _, tenant := range tenants {
		err := s.view(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, stmtRunnable, tenant, now, q.Limit-len(out))
			if err != nil {
				return fmt.Errorf("runstate/postgres: list runnable runs: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var (
					project, id, status string
					cancelled           bool
				)
				if err := rows.Scan(&project, &id, &status, &cancelled); err != nil {
					return fmt.Errorf("runstate/postgres: scan runnable run: %w", err)
				}
				purpose := runtime.ClaimExecute
				switch {
				case runtime.Outcome(status).Decided():
					purpose = runtime.ClaimCleanup
				case cancelled:
					purpose = runtime.ClaimReconcile
				}
				out = append(out, runtime.RunRef{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)},
					RunID: domain.RunID(id), Purpose: purpose})
			}
			return rows.Err()
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

// Commit applies one claim-fenced, version-checked transition together with its
// outbox events in a single transaction. The stored record is read under the
// run's row lock, the kernel's rules (fence and lease, version, run and wait
// updates, outputs, materializations) are applied to it, and the difference is
// written; the durable guards then refuse anything the rules missed. Nothing is
// persisted unless all of it is.
func (s *Store) Commit(ctx context.Context, t runtime.Transition) (runtime.RunRecord, error) {
	if err := t.Validate(); err != nil {
		return runtime.RunRecord{}, err
	}
	t = normalize(t)
	var out runtime.RunRecord
	err := s.update(ctx, t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := loadRecord(ctx, tx, t.Scope, t.RunID, true)
		if err != nil {
			return err
		}
		next, err := applyTransition(stored, t)
		if err != nil {
			return err
		}
		if err := persist(ctx, tx, stored, next, t); err != nil {
			return err
		}
		if err := emit(ctx, tx, t.Events); err != nil {
			return err
		}
		out, err = loadRecord(ctx, tx, t.Scope, t.RunID, false)
		return err
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return out, nil
}
