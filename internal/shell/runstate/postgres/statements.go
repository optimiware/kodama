package postgres

// Every SQL statement of the adapter is a constant in this file, so the
// statements can be reviewed (and tested, see static_test.go) in one place.
// Parameters are positional; $1 is always the tenant and, where a statement is
// project-scoped, $2 the project. A statement never builds SQL from values.
//
// Row level security (forced, tenant_isolation) is the second fence behind the
// explicit tenant_id predicate of each statement: the runtime role cannot see or
// write another tenant's row even if a predicate were missing.

// ---------------------------------------------------------------------------
// Run record: reads.
// ---------------------------------------------------------------------------

const stmtGetRun = `SELECT pipeline_id, definition_snapshot, status, reason, cancel_requested_at, version,
  lease_owner, lease_purpose, lease_fence, lease_expires_at, cleanup_state, cleanup_attempts, cleanup_max_attempts,
  cleanup_reason, retained_resources
FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`

// stmtLockRun serializes every writer of one run on its row. Claim, renewal,
// release, commit, cancellation and signal decisions all start here.
const stmtLockRun = stmtGetRun + ` FOR UPDATE`

const stmtGetSteps = `SELECT step_key, step_kind, status, reason, version, attempt, exit_code, result_code, result_reason,
  when_evaluated, when_result, when_input_digest, when_evaluated_at, outputs,
  wait_id, wait_assignment, wait_correlation, wait_request, wait_state, wait_version, wait_deadline_at, wait_outcome,
  wait_answer, wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_reason
FROM run_steps WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`

const stmtGetCommands = `SELECT command_id, step_key, kind, target, spec_digest, fence, state, reason, result_code,
  result_exit_code, result_reason, binding_attempt, binding_backend, recorded_at
FROM run_commands WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY recorded_at, command_id`

const stmtGetWorkloads = `SELECT step_key, attempt, workload_id, spec_digest, backend, fence, phase, result_code,
  result_exit_code, result_reason, observed_at
FROM run_workload_observations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY step_key, attempt`

const stmtGetResources = `SELECT kind, name, identity, owner_project_id, lifetime, handle_run_id, fence, state, reason, observed_at
FROM run_resource_observations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY kind, name, identity`

const stmtGetMaterializations = `SELECT step_key, attempt, state, issued_at
FROM run_materializations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY issued_at, step_key, attempt`

// ---------------------------------------------------------------------------
// Admission.
// ---------------------------------------------------------------------------

const stmtInsertRun = `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot, status, version,
  cleanup_state, cleanup_attempts, cleanup_max_attempts)
VALUES ($1, $2, $3, $4, $5::jsonb, 'queued', $6, 'none', 0, $7)
ON CONFLICT (tenant_id, project_id, id) DO NOTHING`

const stmtInsertStep = `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind, status,
  version, wait_id, wait_assignment, wait_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10, $11, $12)`

// ---------------------------------------------------------------------------
// Claims.
// ---------------------------------------------------------------------------

const stmtSetLease = `UPDATE runs SET lease_owner = $4, lease_purpose = $5, lease_fence = $6, lease_expires_at = $7
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`

const stmtSetLeaseExpiry = `UPDATE runs SET lease_expires_at = $4 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`

// stmtRunnable lists runs with claimable work in one tenant: not terminal (a
// settled run whose cleanup is final) and with no unexpired lease at $2.
const stmtRunnable = `SELECT project_id, id, status, cancel_requested_at IS NOT NULL
FROM runs
WHERE tenant_id = $1
  AND NOT (status IN ('succeeded', 'failed', 'cancelled') AND cleanup_state IN ('done', 'retained'))
  AND NOT (lease_owner IS NOT NULL AND lease_fence <> 0 AND lease_expires_at > $2)
ORDER BY project_id, id
LIMIT $3`

// ---------------------------------------------------------------------------
// Transitions.
// ---------------------------------------------------------------------------

// stmtUpdateRun is guarded by the version the writer read: a row at another
// version is not updated.
const stmtUpdateRun = `UPDATE runs SET status = $4, reason = $5,
  cancel_requested_at = CASE WHEN $6::boolean THEN COALESCE(cancel_requested_at, $7::timestamptz) ELSE NULL END,
  version = $8,
  finished_at = CASE WHEN $9::boolean THEN COALESCE(finished_at, GREATEST(created_at, $7::timestamptz)) ELSE NULL END,
  cleanup_state = $10, cleanup_attempts = $11, cleanup_reason = $12, retained_resources = $13::jsonb
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $14`

// stmtUpdateStep writes one step with its wait columns together, so the step
// status and the wait state always satisfy the table's checks as one row.
const stmtUpdateStep = `UPDATE run_steps SET status = $5, reason = $6, version = $7, attempt = $8,
  started_at = COALESCE(started_at, $9::timestamptz),
  finished_at = CASE WHEN $10::boolean THEN COALESCE(finished_at, $11::timestamptz) ELSE NULL END,
  exit_code = $12, result_code = $13, result_reason = $14,
  when_evaluated = $15, when_result = $16, when_input_digest = $17, when_evaluated_at = $18,
  outputs = COALESCE($19::jsonb, outputs),
  wait_assignment = $20, wait_correlation = $21::jsonb, wait_request = $22, wait_input = $23, wait_outcomes = $24::jsonb,
  wait_approving = $25, wait_on_expiry = $26, wait_deadline_at = $27, wait_state = $28, wait_outcome = $29,
  wait_answer = $30, wait_actor_ref = $31, wait_decided_at = $32, wait_consumed_signal_id = $33, wait_version = $34,
  wait_reason = $35
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND step_key = $4`

const stmtInsertCommand = `INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, step_key, kind, target, spec_digest,
  fence, state, reason, result_code, result_exit_code, result_reason, binding_attempt, binding_backend, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

const stmtUpdateCommand = `UPDATE run_commands SET state = $5, reason = $6, result_code = $7, result_exit_code = $8, result_reason = $9
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND command_id = $4`

const stmtInsertWorkload = `INSERT INTO run_workload_observations (tenant_id, project_id, run_id, step_key, attempt, workload_id,
  spec_digest, backend, fence, phase, result_code, result_exit_code, result_reason, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

const stmtUpdateWorkload = `UPDATE run_workload_observations SET fence = $6, phase = $7, result_code = $8, result_exit_code = $9,
  result_reason = $10, observed_at = $11
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND step_key = $4 AND attempt = $5`

const stmtInsertResource = `INSERT INTO run_resource_observations (tenant_id, project_id, run_id, kind, name, identity,
  owner_project_id, lifetime, handle_run_id, fence, state, reason, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

const stmtUpdateResource = `UPDATE run_resource_observations SET fence = $7, state = $8, reason = $9, observed_at = $10
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND kind = $4 AND name = $5 AND identity = $6`

const stmtInsertMaterialization = `INSERT INTO run_materializations (tenant_id, project_id, run_id, step_key, attempt,
  materialization_id, state, issued_at)
VALUES ($1, $2, $3, $4, $5, $6, 'issued', $7)`

const stmtRevokeMaterialization = `UPDATE run_materializations SET state = 'revoked', revoked_at = GREATEST(issued_at, $6::timestamptz)
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND step_key = $4 AND attempt = $5`

// ---------------------------------------------------------------------------
// Signals.
// ---------------------------------------------------------------------------

// stmtLockSignalIdentity serializes every decision of one signal identity, so a
// duplicate sees the record its twin committed.
const stmtLockSignalIdentity = `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`

const stmtGetSignal = `SELECT shape, request_digest, applied, refusal_reason, target_run_id, target_step_id, target_wait_id,
  decision_kind, decision_outcome, decision_answer, decision_actor_ref, decision_version, decided_at, recorded_at
FROM signal_records WHERE tenant_id = $1 AND project_id = $2 AND caller_principal = $3 AND caller_key = $4`

const stmtInsertSignal = `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape,
  request_digest, applied, refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome,
  decision_answer, decision_actor_ref, decision_version, decided_at, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

// stmtWaitReason recovers the reason of a cancelled decision a refused signal
// lost to, from the wait the signal targeted: signal_records has no column for it.
const stmtWaitReason = `SELECT wait_reason FROM run_steps
WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND step_key = $4 AND wait_id = $5 AND wait_state = 'cancelled'`

// stmtWinningSignal finds the one applied signal of a wait: the deciding signal
// a refusal that lost to a resolution reports. It reads signal_records only, so
// it works after the wait, step and run are gone.
const stmtWinningSignal = `SELECT signal_id FROM signal_records
WHERE tenant_id = $1 AND project_id = $2 AND target_wait_id = $3 AND applied`

// stmtSignalRunsByWait finds the run of a wait addressed by identity.
const stmtSignalRunsByWait = `SELECT DISTINCT run_id FROM run_steps
WHERE tenant_id = $1 AND project_id = $2 AND wait_id = $3`

// stmtSignalRunsByCorrelation finds the runs with an open wait whose complete
// correlation equals the request's.
const stmtSignalRunsByCorrelation = `SELECT DISTINCT run_id FROM run_steps
WHERE tenant_id = $1 AND project_id = $2 AND wait_state = 'waiting' AND wait_correlation = $3::jsonb`

// ---------------------------------------------------------------------------
// Outbox.
// ---------------------------------------------------------------------------

// stmtInsertEvent records one event of the outbox. created_at is the statement
// clock, so the events of one transaction keep their order. A repeat of an event
// already recorded does nothing.
const stmtInsertEvent = `INSERT INTO audit_events (tenant_id, id, project_id, actor_type, actor_id, event_type, resource_type,
  resource_id, payload, created_at)
VALUES ($1, $2, $3, 'system', 'kernel', $4, 'run', $5, $6::jsonb, clock_timestamp())
ON CONFLICT (tenant_id, id) DO NOTHING`

const stmtGetEvent = `SELECT payload FROM audit_events WHERE tenant_id = $1 AND id = $2`

const stmtRunEvents = `SELECT payload FROM audit_events
WHERE tenant_id = $1 AND project_id = $2 AND resource_type = 'run' AND resource_id = $3
ORDER BY created_at, id`

// allStatements lists every statement above; static_test.go checks that no
// statement is missing from it and that each one is tenant-scoped.
func allStatements() []string {
	return []string{
		stmtGetRun, stmtLockRun, stmtGetSteps, stmtGetCommands, stmtGetWorkloads, stmtGetResources, stmtGetMaterializations,
		stmtInsertRun, stmtInsertStep,
		stmtSetLease, stmtSetLeaseExpiry, stmtRunnable,
		stmtUpdateRun, stmtUpdateStep, stmtInsertCommand, stmtUpdateCommand, stmtInsertWorkload, stmtUpdateWorkload,
		stmtInsertResource, stmtUpdateResource, stmtInsertMaterialization, stmtRevokeMaterialization,
		stmtLockSignalIdentity, stmtGetSignal, stmtInsertSignal, stmtWaitReason, stmtWinningSignal, stmtSignalRunsByWait, stmtSignalRunsByCorrelation,
		stmtInsertEvent, stmtGetEvent, stmtRunEvents,
	}
}
