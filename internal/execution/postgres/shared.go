package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// The run_shared rows (migration 003_run_resources.sql): run resources, workspace holders,
// the command-begin journal and the run-level capacity reservation. They are
// loaded and saved with the run aggregate under the same run row lock; the
// reservation is taken under the quota scope locks. No statement here calls the
// runtime.

var (
	_ execution.SharedEnv   = (*txEnv)(nil)
	_ execution.SharedStore = (*Store)(nil)
)

const resourceColumns = `key, kind, name, ordinal, backend, spec, fingerprint, ensure_requested_at, observe_gen, applied_gen, observe_requested_at, uid, bound_at, phase, reason, observed_at,
	ready_at, probe_kind, not_ready_since, unknown_since, released, released_at, receipt, receipt_digest, receipt_at, stop_requested_at,
	stop_reason, stopped_at, stop_launch, removed_at, failures, next_action_at, version, created_at, updated_at`

func scanResource(row pgx.Row, scope domain.Scope, run domain.RunID) (*execution.RunResource, error) {
	var (
		key, kind, name, backend, fp, phase                                          string
		ordinal, failures                                                            int
		spec, receipt                                                                []byte
		uid, reason, probe, receiptDigest, stopReason, stopLaunch                    *string
		ensureAt, boundAt, observed, ready, notReady, unknown, releasedAt, receiptAt *time.Time
		stopReq, stopped, removed, next                                              *time.Time
		released                                                                     bool
		version, observeGen, appliedGen                                              int64
		observeReq                                                                   *time.Time
		created, updated                                                             time.Time
	)
	if err := row.Scan(&key, &kind, &name, &ordinal, &backend, &spec, &fp, &ensureAt, &observeGen, &appliedGen, &observeReq, &uid, &boundAt, &phase, &reason, &observed, &ready,
		&probe, &notReady, &unknown, &released, &releasedAt, &receipt, &receiptDigest, &receiptAt, &stopReq, &stopReason, &stopped,
		&stopLaunch, &removed, &failures, &next, &version, &created, &updated); err != nil {
		return nil, err
	}
	r := &execution.RunResource{
		Scope: scope, RunID: run, Kind: execution.ResourceKind(kind), Key: key, Name: name, Ordinal: ordinal, Backend: backend, Fingerprint: fp,
		EnsureRequestedAt: tmp(ensureAt), ObserveGen: observeGen, AppliedGen: appliedGen, ObserveRequestedAt: tmp(observeReq), UID: str(uid), BoundAt: tmp(boundAt), Phase: execution.ResourcePhase(phase),
		Reason: execution.ResourceReason(str(reason)), ObservedAt: tmp(observed), ReadyAt: tmp(ready), ProbeKind: str(probe),
		NotReadySince: tmp(notReady), UnknownSince: tmp(unknown), Released: released, ReleasedAt: tmp(releasedAt),
		ReceiptDigest: str(receiptDigest), ReceiptAt: tmp(receiptAt), StopRequestedAt: tmp(stopReq), StopReason: execution.ResourceReason(str(stopReason)),
		StoppedAt: tmp(stopped), StopLaunch: execution.LaunchEvidence(str(stopLaunch)), RemovedAt: tmp(removed), Failures: failures,
		NextActionAt: tmp(next), Version: domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
	}
	if err := json.Unmarshal(spec, &r.Spec); err != nil {
		return nil, fmt.Errorf("execution: stored resource spec: %w", err)
	}
	if receipt != nil {
		// A source-set receipt (runtime v3) carries one entry per source; the
		// earlier receipt a single commit. The stored shape tells them apart.
		var probe struct {
			Sources json.RawMessage `json:"sources"`
		}
		if err := json.Unmarshal(receipt, &probe); err != nil {
			return nil, fmt.Errorf("execution: stored receipt: %w", err)
		}
		if probe.Sources != nil {
			var rc runtimev3.PreparationReceiptRequestV3
			if err := json.Unmarshal(receipt, &rc); err != nil {
				return nil, fmt.Errorf("execution: stored receipt: %w", err)
			}
			r.ReceiptV3 = &rc
		} else {
			var rc runtimev2.PreparationReceiptRequest
			if err := json.Unmarshal(receipt, &rc); err != nil {
				return nil, fmt.Errorf("execution: stored receipt: %w", err)
			}
			r.Receipt = &rc
		}
	}
	return r, nil
}

func (s *Store) loadShared(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate) error {
	scope, id := agg.Run.Scope, agg.Run.ID
	rows, err := tx.Query(ctx, `SELECT `+resourceColumns+` FROM run_resources WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return err
	}
	for rows.Next() {
		r, err := scanResource(rows, scope, id)
		if err != nil {
			rows.Close()
			return err
		}
		agg.Resources = append(agg.Resources, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	hrows, err := tx.Query(ctx, `SELECT attempt_id, step_id, held_at, released_at FROM run_holders WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY held_at, attempt_id`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return err
	}
	for hrows.Next() {
		var attempt, step string
		var held time.Time
		var released *time.Time
		if err := hrows.Scan(&attempt, &step, &held, &released); err != nil {
			hrows.Close()
			return err
		}
		agg.Holders = append(agg.Holders, &execution.RunHolder{Scope: scope, RunID: id, AttemptID: domain.AttemptID(attempt), StepID: step, HeldAt: held.UTC(), ReleasedAt: tmp(released)})
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return err
	}
	brows, err := tx.Query(ctx, `SELECT attempt_id, step_id, access, began_at, workspace_state, ignored_present, body_digest FROM attempt_begins
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY began_at, attempt_id`, string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return err
	}
	defer brows.Close()
	for brows.Next() {
		var attempt, step, access, state, digest string
		var began time.Time
		var ignored bool
		if err := brows.Scan(&attempt, &step, &access, &began, &state, &ignored, &digest); err != nil {
			return err
		}
		agg.Begins = append(agg.Begins, &execution.BeginRecord{
			Scope: scope, RunID: id, AttemptID: domain.AttemptID(attempt), StepID: step, Access: execution.Access(access), BeganAt: began.UTC(),
			WorkspaceState: runtimev2.WorkspaceState(state), IgnoredPresent: ignored, BodyDigest: digest,
		})
	}
	return brows.Err()
}

func (s *Store) persistShared(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate) error {
	scope := agg.Run.Scope
	created := map[string]bool{}
	for _, r := range agg.NewResources {
		created[r.Key] = true
		spec, err := jsonb(r.Spec)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_resources (tenant_id, project_id, run_id, key, kind, name, ordinal, backend, spec, fingerprint, phase, version, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			string(scope.TenantID), string(scope.ProjectID), string(r.RunID), r.Key, string(r.Kind), r.Name, r.Ordinal, r.Backend, spec, r.Fingerprint,
			string(r.Phase), int64(r.Version), micros(r.CreatedAt), micros(r.UpdatedAt)); err != nil {
			return err
		}
	}
	for _, r := range agg.Resources {
		if created[r.Key] || !r.Dirty() {
			continue
		}
		specJSON, err := jsonb(r.Spec)
		if err != nil {
			return err
		}
		var receipt any
		switch {
		case r.ReceiptV3 != nil:
			b, err := jsonb(r.ReceiptV3)
			if err != nil {
				return err
			}
			receipt = b
		case r.Receipt != nil:
			b, err := jsonb(r.Receipt)
			if err != nil {
				return err
			}
			receipt = b
		}
		tag, err := tx.Exec(ctx, `UPDATE run_resources SET ensure_requested_at=$5, uid=$6, bound_at=$7, phase=$8, reason=$9, observed_at=$10, ready_at=$11,
			probe_kind=$12, not_ready_since=$13, unknown_since=$14, released=$15, released_at=$16, receipt=$17, receipt_digest=$18, receipt_at=$19,
			stop_requested_at=$20, stop_reason=$21, stopped_at=$22, stop_launch=$23, removed_at=$24, failures=$25, next_action_at=$26, version=$27, updated_at=$28,
			observe_gen=$29, applied_gen=$30, observe_requested_at=$31
			WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND key=$4 AND version=$27-1
			  AND fingerprint=$32 AND name=$33 AND backend=$34 AND kind=$35 AND spec=$36::jsonb`,
			string(scope.TenantID), string(scope.ProjectID), string(r.RunID), r.Key, ntm(r.EnsureRequestedAt), nstr(r.UID), ntm(r.BoundAt), string(r.Phase),
			nstr(string(r.Reason)), ntm(r.ObservedAt), ntm(r.ReadyAt), nstr(r.ProbeKind), ntm(r.NotReadySince), ntm(r.UnknownSince), r.Released,
			ntm(r.ReleasedAt), receipt, nstr(r.ReceiptDigest), ntm(r.ReceiptAt), ntm(r.StopRequestedAt), nstr(string(r.StopReason)), ntm(r.StoppedAt),
			nstr(string(r.StopLaunch)), ntm(r.RemovedAt), r.Failures, ntm(r.NextActionAt), int64(r.Version), micros(r.UpdatedAt),
			r.ObserveGen, r.AppliedGen, ntm(r.ObserveRequestedAt), r.Fingerprint, r.Name, r.Backend, string(r.Kind), specJSON)
		if err != nil {
			return uniqueUID(err)
		}
		if tag.RowsAffected() != 1 {
			// The identity, spec and fingerprint are columns the runtime role cannot
			// update; a caller that changed them is refused like the memory store does,
			// distinguished from a lost version race.
			var same bool
			e := tx.QueryRow(ctx, `SELECT version = $5-1 FROM run_resources WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND key=$4`,
				string(scope.TenantID), string(scope.ProjectID), string(r.RunID), r.Key, int64(r.Version)).Scan(&same)
			if e == nil && same {
				return fmt.Errorf("%w: the identity, spec and fingerprint of a run resource are immutable", execution.ErrInvalidTransition)
			}
			return execution.ErrStale
		}
	}
	newHolder := map[domain.AttemptID]bool{}
	for _, h := range agg.NewHolders {
		newHolder[h.AttemptID] = true
		if _, err := tx.Exec(ctx, `INSERT INTO run_holders (tenant_id, project_id, run_id, attempt_id, step_id, held_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			string(scope.TenantID), string(scope.ProjectID), string(h.RunID), string(h.AttemptID), h.StepID, micros(h.HeldAt)); err != nil {
			return err
		}
	}
	for _, h := range agg.Holders {
		if newHolder[h.AttemptID] || !h.Dirty() {
			continue
		}
		tag, err := tx.Exec(ctx, `UPDATE run_holders SET released_at=$5 WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND attempt_id=$4`,
			string(scope.TenantID), string(scope.ProjectID), string(h.RunID), string(h.AttemptID), ntm(h.ReleasedAt))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return execution.ErrStale
		}
	}
	for _, b := range agg.NewBegins {
		if _, err := tx.Exec(ctx, `INSERT INTO attempt_begins (tenant_id, project_id, attempt_id, run_id, step_id, access, began_at, workspace_state, ignored_present, body_digest)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, string(scope.TenantID), string(scope.ProjectID), string(b.AttemptID), string(b.RunID), b.StepID,
			string(b.Access), micros(b.BeganAt), string(b.WorkspaceState), b.IgnoredPresent, b.BodyDigest); err != nil {
			return err
		}
	}
	return nil
}

// uniqueUID maps the unique (backend, UID) index violation to the conflict class:
// one provider instance is bound to at most one run resource of a backend.
func uniqueUID(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_resources_uid_unique_idx" {
		return fmt.Errorf("%w: the provider instance is bound to another run resource", execution.ErrConflict)
	}
	return err
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

// ReserveRunCapacity implements execution.SharedEnv: it sums the held run
// reservations of the tenant and of the project under the quota scope locks and
// inserts the run's reservation only if every quantity still fits, so
// concurrent starts cannot exceed a limit and a refusal commits nothing.
func (e *txEnv) ReserveRunCapacity(ctx context.Context, run *execution.Run, units execution.CapacityUnits, lim execution.Limit) error {
	if !e.quotaLocked || run.Scope != e.scope {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	var done int
	err := e.tx.QueryRow(ctx, `SELECT 1 FROM execution_run_reservations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3`,
		string(run.Scope.TenantID), string(run.Scope.ProjectID), string(run.ID)).Scan(&done)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	held := func(project bool) (execution.CapacityUnits, error) {
		var u execution.CapacityUnits
		q := `SELECT COALESCE(sum(services),0), COALESCE(sum(cpu_millis),0), COALESCE(sum(memory_mib),0), COALESCE(sum(ephemeral_mib),0), COALESCE(sum(storage_mib),0)
			FROM execution_run_reservations WHERE tenant_id = $1 AND released_at IS NULL`
		args := []any{string(run.Scope.TenantID)}
		if project {
			q += ` AND project_id = $2`
			args = append(args, string(run.Scope.ProjectID))
		}
		err := e.tx.QueryRow(ctx, q, args...).Scan(&u.Services, &u.CPUMillis, &u.MemoryMiB, &u.EphemeralMiB, &u.StorageMiB)
		return u, err
	}
	th, err := held(false)
	if err != nil {
		return err
	}
	ph, err := held(true)
	if err != nil {
		return err
	}
	if !units.Fits(th, lim.SharedTenant) || !units.Fits(ph, lim.SharedProject) {
		return fmt.Errorf("%w: the run's service, preparation, attempt and scratch capacity does not fit", execution.ErrQuotaExceeded)
	}
	// The run also takes one tenant/project active-attempt slot, consumed by every
	// successive attempt of the run without a second charge. Slots held by
	// attempts of other runs and by other shared runs count (CapacityAvailable).
	if err := e.CapacityAvailable(ctx, run.Scope, lim); err != nil {
		return err
	}
	_, err = e.tx.Exec(ctx, `INSERT INTO execution_run_reservations (tenant_id, project_id, run_id, services, cpu_millis, memory_mib, ephemeral_mib, storage_mib, held_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, string(run.Scope.TenantID), string(run.Scope.ProjectID), string(run.ID), units.Services, units.CPUMillis,
		units.MemoryMiB, units.EphemeralMiB, units.StorageMiB, e.s.now())
	return err
}

// ReleaseRunCapacity releases the run reservation once; a repeat is a no-op.
func (e *txEnv) ReleaseRunCapacity(ctx context.Context, run *execution.Run) error {
	_, err := e.tx.Exec(ctx, `UPDATE execution_run_reservations SET released_at = $4
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND released_at IS NULL`,
		string(run.Scope.TenantID), string(run.Scope.ProjectID), string(run.ID), e.s.now())
	return err
}

// UnfinishedResources implements execution.SharedStore: terminal runs that still
// have a resource not verified removed, oldest first.
func (s *Store) UnfinishedResources(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
	if limit < 1 || limit > 1000 {
		return nil, execution.Invalidf("limit must be between 1 and 1000")
	}
	var out []execution.RunRef
	err := s.inTenantRead(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var createdAfter time.Time
		if after.ID != "" {
			if e := tx.QueryRow(ctx, `SELECT created_at FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
				string(tenant), string(after.Scope.ProjectID), string(after.ID)).Scan(&createdAfter); e != nil {
				return e
			}
		}
		rows, err := tx.Query(ctx, `SELECT r.project_id, r.id FROM runs r
			WHERE r.tenant_id = $1 AND r.status IN ('succeeded', 'failed', 'cancelled')
			  AND ($2 = '' OR (r.created_at, r.project_id, r.id) > ($3, $4, $2))
			  AND EXISTS (SELECT 1 FROM run_resources x WHERE x.tenant_id = r.tenant_id AND x.project_id = r.project_id AND x.run_id = r.id AND x.removed_at IS NULL)
			ORDER BY r.created_at, r.project_id, r.id LIMIT $5`,
			string(tenant), string(after.ID), micros(createdAfter), string(after.Scope.ProjectID), limit)
		if err != nil {
			return err
		}
		out, err = scanRefs(rows, tenant)
		return err
	})
	return out, err
}
