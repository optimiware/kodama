package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

const runColumns = `id, purpose, work_item_id, workspace_id, deployment_id, definition_id, definition_digest, content_revision,
	specification, source_commit, inputs, snapshot_digest, source_retention, retained_ref_id, requested_by_type, requested_by_id,
	status, status_reason, cancel_requested_at, cancel_reason, failure_intent_at, failure_reason, version, created_at, updated_at,
	started_at, finished_at`

func scanRun(row pgx.Row, scope domain.Scope) (*execution.Run, error) {
	var (
		id, purpose, definition, digest, retention, byType, byID, status, reason string
		workItem, workspaceID, deployment, commit, retained                      *string
		revision                                                                 *int64
		spec, inputs                                                             []byte
		snapDigest                                                               string
		cancelAt, failAt, started, finished                                      *time.Time
		cancelReason, failReason                                                 *string
		version                                                                  int64
		created, updated                                                         time.Time
	)
	if err := row.Scan(&id, &purpose, &workItem, &workspaceID, &deployment, &definition, &digest, &revision, &spec, &commit, &inputs,
		&snapDigest, &retention, &retained, &byType, &byID, &status, &reason, &cancelAt, &cancelReason, &failAt, &failReason,
		&version, &created, &updated, &started, &finished); err != nil {
		return nil, err
	}
	r := &execution.Run{
		Scope: scope, ID: domain.RunID(id), Purpose: execution.Purpose(purpose), WorkItemID: domain.WorkItemID(str(workItem)),
		WorkspaceID: str(workspaceID), DeploymentID: str(deployment), DefinitionID: domain.DefinitionID(definition), DefinitionDigest: digest,
		SourceCommit: str(commit), SnapshotDigest: snapDigest, SourceRetention: execution.SourceRetention(retention), RetainedRefID: str(retained),
		RequestedBy: execution.Actor{Type: execution.ActorType(byType), ID: byID},
		Status:      execution.RunStatus(status), Reason: execution.Reason(reason),
		CancelRequestedAt: tmp(cancelAt), CancelReason: execution.Reason(str(cancelReason)),
		FailureIntentAt: tmp(failAt), FailureReason: execution.Reason(str(failReason)),
		Version: domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(), StartedAt: tmp(started), FinishedAt: tmp(finished),
	}
	if revision != nil {
		r.ContentRevision = domain.ContentRevision(*revision)
	}
	if spec != nil {
		var sp execution.Specification
		if err := json.Unmarshal(spec, &sp); err != nil {
			return nil, fmt.Errorf("execution: stored specification: %w", err)
		}
		r.Specification = &sp
	}
	if err := json.Unmarshal(inputs, &r.Snapshot); err != nil {
		return nil, fmt.Errorf("execution: stored inputs: %w", err)
	}
	return r, nil
}

const stepColumns = `step_id, ordinal, spec, action_key, status, reason, next_attempt_at, gate_ref, attempts_started, max_attempts,
	backoff_seconds, retry_on, output_commit, outputs, version, created_at, updated_at, finished_at`

func scanStep(row pgx.Row, run domain.RunID) (*execution.StepRun, error) {
	var (
		id, action, status, reason string
		ordinal, attempts, max     int
		spec, outputs              []byte
		next, finished             *time.Time
		gate, commit               *string
		backoff, version           int64
		retryOn                    []string
		created, updated           time.Time
	)
	if err := row.Scan(&id, &ordinal, &spec, &action, &status, &reason, &next, &gate, &attempts, &max, &backoff, &retryOn,
		&commit, &outputs, &version, &created, &updated, &finished); err != nil {
		return nil, err
	}
	s := &execution.StepRun{
		RunID: run, ID: id, Ordinal: ordinal, ActionKey: action, Status: execution.StepStatus(status), Reason: execution.Reason(reason),
		NextAttemptAt: tmp(next), GateRef: str(gate), AttemptsStarted: attempts, MaxAttempts: max, BackoffSeconds: backoff,
		OutputCommit: str(commit), Version: domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(), FinishedAt: tmp(finished),
	}
	for _, c := range retryOn {
		s.RetryOn = append(s.RetryOn, pipeline.RetryClass(c))
	}
	var derr error
	if s.Spec, derr = execution.DecodeStep(spec); derr != nil {
		return nil, fmt.Errorf("execution: stored step: %w", derr)
	}
	if len(outputs) > 0 && string(outputs) != "{}" {
		if err := json.Unmarshal(outputs, &s.Outputs); err != nil {
			return nil, fmt.Errorf("execution: stored outputs: %w", err)
		}
	}
	return s, nil
}

const attemptColumns = `id, step_id, ordinal, action_key, status, reason, failure_class, job_name, spec_fingerprint, observed_job_uid,
	workspace_id, access, input_commit, expected_tip, timeout_seconds, startup_deadline_at, execution_deadline_at, context_digest, context,
	started_at, finished_at, result_hash, result, authority_revision, version, created_at, updated_at`

func scanAttempt(row pgx.Row, scope domain.Scope, run domain.RunID) (*execution.Attempt, error) {
	var (
		id, step, action, status, reason, job, fp, access, ctxDigest, ctxDoc string
		ordinal                                                              int
		class, uid, workspaceID, input, tip, resultHash                      *string
		timeout, revision, version                                           int64
		startup, created, updated                                            time.Time
		execDeadline, started, finished                                      *time.Time
		result                                                               []byte
	)
	if err := row.Scan(&id, &step, &ordinal, &action, &status, &reason, &class, &job, &fp, &uid, &workspaceID, &access, &input, &tip,
		&timeout, &startup, &execDeadline, &ctxDigest, &ctxDoc, &started, &finished, &resultHash, &result, &revision, &version, &created, &updated); err != nil {
		return nil, err
	}
	a := &execution.Attempt{
		Scope: scope, ID: domain.AttemptID(id), RunID: run, StepID: step, Ordinal: ordinal, ActionKey: action,
		Status: execution.AttemptStatus(status), Reason: execution.Reason(reason), Class: execution.FailureClass(str(class)),
		WorkloadName: job, SpecFingerprint: fp, WorkloadUID: str(uid), WorkspaceID: str(workspaceID), Access: execution.Access(access),
		InputCommit: str(input), ExpectedTip: str(tip), TimeoutSeconds: timeout, StartupDeadline: startup.UTC(), ExecutionDeadline: tmp(execDeadline),
		ContextDigest: ctxDigest, Context: []byte(ctxDoc), StartedAt: tmp(started), FinishedAt: tmp(finished), ResultHash: str(resultHash),
		AuthorityRevision: revision, Version: domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
	}
	if result != nil {
		var r runtimev1.Result
		if err := json.Unmarshal(result, &r); err != nil {
			return nil, fmt.Errorf("execution: stored result: %w", err)
		}
		a.Result = &r
	}
	return a, nil
}

const controlColumns = `attempt_id, dispatch_confirmed_at, claim_token, claim_until, revoked_at, termination_requested_at,
	termination_confirmed_at, launch, effect, reconciled_at, replay_safe, settled_at, step_resolved_at`

func scanControl(row pgx.Row) (*execution.Control, error) {
	var (
		id, launch, effect                                                          string
		token                                                                       *string
		confirmed, until, revoked, termReq, termConf, reconciled, settled, resolved *time.Time
		replay                                                                      bool
	)
	if err := row.Scan(&id, &confirmed, &token, &until, &revoked, &termReq, &termConf, &launch, &effect, &reconciled, &replay, &settled, &resolved); err != nil {
		return nil, err
	}
	return &execution.Control{
		AttemptID: domain.AttemptID(id), DispatchConfirmedAt: tmp(confirmed), ClaimToken: str(token), ClaimUntil: tmp(until),
		RevokedAt: tmp(revoked), TerminationRequestedAt: tmp(termReq), TerminationConfirmedAt: tmp(termConf),
		Launch: execution.LaunchEvidence(launch), Effect: execution.EffectState(effect), ReconciledAt: tmp(reconciled), ReplaySafe: replay,
		SettledAt: tmp(settled), StepResolvedAt: tmp(resolved),
	}, nil
}

type lockMode int

const (
	lockNone lockMode = iota
	lockUpdate
	lockSkip
)

func (m lockMode) sql() string {
	switch m {
	case lockUpdate:
		return " FOR UPDATE"
	case lockSkip:
		return " FOR UPDATE SKIP LOCKED"
	}
	return ""
}

// loadRun reads the run, locking its row as requested. With lockSkip a locked
// row is ErrBusy and a missing row ErrNotFound.
func (s *Store) loadRun(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, mode lockMode) (*execution.Run, error) {
	row := tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`+mode.sql(),
		string(scope.TenantID), string(scope.ProjectID), string(id))
	r, err := scanRun(row, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		if mode == lockSkip {
			var one int
			e := tx.QueryRow(ctx, `SELECT 1 FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
				string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&one)
			if e == nil {
				return nil, execution.ErrBusy
			}
		}
		return nil, execution.ErrNotFound
	}
	return r, err
}

// loadAggregate reads a run with its steps, attempts and bookkeeping. The run
// row lock (when taken) protects the children: every writer goes through it.
func (s *Store) loadAggregate(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, mode lockMode) (*execution.Aggregate, error) {
	run, err := s.loadRun(ctx, tx, scope, id, mode)
	if err != nil {
		return nil, err
	}
	agg := &execution.Aggregate{Run: run, Controls: map[domain.AttemptID]*execution.Control{}}
	rows, err := tx.Query(ctx, `SELECT `+stepColumns+` FROM step_runs WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		st, err := scanStep(rows, id)
		if err != nil {
			rows.Close()
			return nil, err
		}
		agg.Steps = append(agg.Steps, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT `+attemptColumns+` FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY step_id, ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		a, err := scanAttempt(rows, scope, id)
		if err != nil {
			rows.Close()
			return nil, err
		}
		agg.Attempts = append(agg.Attempts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT `+controlColumns+` FROM attempt_controls WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		c, err := scanControl(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		agg.Controls[c.AttemptID] = c
	}
	rows.Close()
	return agg, rows.Err()
}

// Get implements execution.Store (a consistent read, no locks).
func (s *Store) Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	var out *execution.Aggregate
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = s.loadAggregate(ctx, tx, scope, id, lockNone)
		return err
	})
	return out, err
}

// List implements execution.Store.
func (s *Store) List(ctx context.Context, scope domain.Scope, q execution.ListQuery) ([]*execution.Run, error) {
	var out []*execution.Run
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sql := `SELECT ` + runColumns + ` FROM runs WHERE tenant_id = $1 AND project_id = $2`
		args := []any{string(scope.TenantID), string(scope.ProjectID)}
		if len(q.Statuses) > 0 {
			var st []string
			for _, x := range q.Statuses {
				st = append(st, string(x))
			}
			args = append(args, st)
			sql += fmt.Sprintf(" AND status = ANY($%d)", len(args))
		}
		if q.WorkItemID != "" {
			args = append(args, string(q.WorkItemID))
			sql += fmt.Sprintf(" AND work_item_id = $%d", len(args))
		}
		if q.After != nil {
			args = append(args, micros(q.After.CreatedAt), string(q.After.ID))
			sql += fmt.Sprintf(" AND (created_at, id) > ($%d, $%d)", len(args)-1, len(args))
		}
		args = append(args, q.Limit)
		sql += fmt.Sprintf(" ORDER BY created_at, id LIMIT $%d", len(args))
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// RunOf implements execution.Store.
func (s *Store) RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error) {
	var id string
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT run_id FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			return execution.ErrNotFound
		}
		return e
	})
	return domain.RunID(id), err
}

func scanRefs(rows pgx.Rows, tenant domain.TenantID) ([]execution.RunRef, error) {
	defer rows.Close()
	var out []execution.RunRef
	for rows.Next() {
		var project, id string
		if err := rows.Scan(&project, &id); err != nil {
			return nil, err
		}
		out = append(out, execution.RunRef{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, ID: domain.RunID(id)})
	}
	return out, rows.Err()
}

// Dispatchable implements execution.Store: nonterminal runs that accept
// dispatch and have a pending, due step, oldest first. The scan is bounded and
// tenant-scoped; the claim rechecks everything under locks.
func (s *Store) Dispatchable(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
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
			WHERE r.tenant_id = $1 AND r.status IN ('queued', 'running')
			  AND r.cancel_requested_at IS NULL AND r.failure_intent_at IS NULL
			  AND r.source_retention IN ('confirmed', 'not_required')
			  AND ($2 = '' OR (r.created_at, r.project_id, r.id) > ($3, $4, $2))
			  AND EXISTS (SELECT 1 FROM step_runs s WHERE s.tenant_id = r.tenant_id AND s.project_id = r.project_id AND s.run_id = r.id
			              AND s.status = 'pending' AND (s.next_attempt_at IS NULL OR s.next_attempt_at <= $6))
			ORDER BY r.created_at, r.project_id, r.id LIMIT $5`,
			string(tenant), string(after.ID), micros(createdAfter), string(after.Scope.ProjectID), limit, s.now())
		if err != nil {
			return err
		}
		out, err = scanRefs(rows, tenant)
		return err
	})
	return out, err
}

// Active implements execution.Store.
func (s *Store) Active(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
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
		rows, err := tx.Query(ctx, `SELECT project_id, id FROM runs
			WHERE tenant_id = $1 AND status IN ('queued', 'running') AND ($2 = '' OR (created_at, project_id, id) > ($3, $4, $2))
			ORDER BY created_at, project_id, id LIMIT $5`,
			string(tenant), string(after.ID), micros(createdAfter), string(after.Scope.ProjectID), limit)
		if err != nil {
			return err
		}
		out, err = scanRefs(rows, tenant)
		return err
	})
	return out, err
}
