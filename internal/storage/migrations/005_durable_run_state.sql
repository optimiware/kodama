-- Durable run state for the kernel record (T-185): the columns and tables a
-- StateStore needs to round-trip a complete runtime.RunRecord without a second
-- source of truth in Go. Appended after 004_pipeline_runtime_expand.sql, which
-- is left untouched. Expand-only: no table, column or data is dropped, and the
-- legacy composition keeps reading and writing the columns it uses today. Four
-- check constraints and the run-step guard function of 004 are replaced because
-- their old form forbade the new shape (a wait that has an identity but has not
-- opened); no other earlier object changes except by adding columns, constraints,
-- indexes, three tables, triggers and grants.
--
-- The whole file is one transaction (the runner applies it atomically with its
-- ledger row): a refusal or a failure leaves the database exactly as it was.
--
-- Where each part of a RunRecord lives (the plan is runs.definition_snapshot):
--   Run         runs (status, reason, cancel_requested_at, version; cleanup_*)
--   Claim       runs (lease_owner, lease_purpose, lease_fence, lease_expires_at)
--   Step        run_steps (state, reason, version, attempt, result_*, when_*)
--   Wait        run_steps (wait_*), recorded from admission, opened later
--   Command     run_commands
--   Workload    run_workload_observations
--   Resource    run_resource_observations

-- ---------------------------------------------------------------------------
-- runs: claim purpose and the cleanup bound and reason.
-- ---------------------------------------------------------------------------
ALTER TABLE runs
    -- Why the run is claimed. Null exactly when there is no holder.
    ADD COLUMN lease_purpose text,
    -- Bound on cleanup retries, fixed at admission (it is not in the runtime
    -- update grant). Cleanup attempts never exceed it.
    ADD COLUMN cleanup_max_attempts integer NOT NULL DEFAULT 1,
    -- Why cleanup left something retained (the kernel's Cleanup.Reason).
    ADD COLUMN cleanup_reason text;

-- A claim before purposes existed was an execution claim. A row that already
-- records more cleanup attempts than the default bound keeps that count within
-- its bound. A run already retained gets the reason the kernel gives exhaustion.
-- Each statement touches only rows that would otherwise violate the constraints
-- below; on a database written only by the 004 schema none exists.
UPDATE runs SET lease_purpose = 'execute' WHERE lease_owner IS NOT NULL;
UPDATE runs SET cleanup_max_attempts = cleanup_attempts WHERE cleanup_attempts > cleanup_max_attempts;
UPDATE runs SET cleanup_reason = 'cleanup_exhausted' WHERE cleanup_state = 'retained';

ALTER TABLE runs
    ADD CONSTRAINT runs_lease_purpose_known CHECK (lease_purpose IN ('execute', 'reconcile', 'cleanup')),
    ADD CONSTRAINT runs_lease_purpose_pair CHECK ((lease_owner IS NULL) = (lease_purpose IS NULL)),
    ADD CONSTRAINT runs_cleanup_max_attempts_range CHECK (cleanup_max_attempts >= 1 AND cleanup_attempts <= cleanup_max_attempts),
    -- A reason is a stable code, kept only while cleanup is unfinished or
    -- exhausted; retained resources are never left without one.
    ADD CONSTRAINT runs_cleanup_reason_shape CHECK (
        (cleanup_reason IS NULL OR (cleanup_reason ~ '^[a-z][a-z0-9_]{0,62}$' AND cleanup_state IN ('pending', 'retained')))
        AND (cleanup_state <> 'retained' OR cleanup_reason IS NOT NULL));

-- ---------------------------------------------------------------------------
-- run_commands: the durable intent to perform one external effect.
-- ---------------------------------------------------------------------------
-- Keyed by the engine's stable command id, so a replayed commit is idempotent. A
-- workload command carries the binding the engine needs to observe, stop or
-- remove the workload after a restart: the attempt and the backend are stored
-- here; the binding's scope, run and step are this row's, its workload identity
-- is `target`, its specification digest is `spec_digest` and its fence is
-- `fence` (the kernel requires them equal). A resource command belongs to no
-- step by default, so commands cannot live on run_steps.
CREATE TABLE run_commands (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    command_id text NOT NULL CHECK (length(command_id) BETWEEN 1 AND 256),
    step_key text CHECK (step_key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    kind text NOT NULL CHECK (kind IN ('launch_workload', 'stop_workload', 'remove_workload',
                                       'ensure_resource', 'stop_resource', 'remove_resource')),
    -- Stable workload or resource identity.
    target text NOT NULL CHECK (length(target) BETWEEN 1 AND 256),
    -- Opaque specification identity; its form belongs to the engine.
    spec_digest text CHECK (length(spec_digest) BETWEEN 1 AND 256),
    -- The writing claim's fence; positive.
    fence bigint NOT NULL CHECK (fence BETWEEN 1 AND 9007199254740991),
    state text NOT NULL DEFAULT 'intent' CHECK (state IN ('intent', 'confirmed', 'rejected', 'uncertain')),
    reason text CHECK (reason ~ '^[a-z][a-z0-9_]{0,62}$'),
    -- The workload result a confirmed workload command ended, retained once.
    result_code text CHECK (result_code IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'lost')),
    result_exit_code integer,
    result_reason text CHECK (result_reason ~ '^[a-z][a-z0-9_]{0,62}$'),
    binding_attempt integer CHECK (binding_attempt >= 1),
    binding_backend text CHECK (length(binding_backend) BETWEEN 1 AND 256),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id, command_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_key) REFERENCES run_steps (tenant_id, project_id, run_id, step_key),
    -- A workload command names its step and carries the complete binding; a
    -- resource command carries none.
    CONSTRAINT run_commands_binding_shape CHECK (
        (kind IN ('launch_workload', 'stop_workload', 'remove_workload')
            AND step_key IS NOT NULL AND spec_digest IS NOT NULL
            AND binding_attempt IS NOT NULL AND binding_backend IS NOT NULL)
        OR (kind IN ('ensure_resource', 'stop_resource', 'remove_resource')
            AND num_nonnulls(binding_attempt, binding_backend) = 0)),
    -- intent and confirmed have no reason; rejected and uncertain require one.
    CONSTRAINT run_commands_state_reason CHECK (
        (state IN ('intent', 'confirmed') AND reason IS NULL)
        OR (state IN ('rejected', 'uncertain') AND reason IS NOT NULL)),
    -- Only a confirmed workload command records a result; a success has exit
    -- code 0 and no reason.
    CONSTRAINT run_commands_result_shape CHECK (
        (num_nonnulls(result_code, result_exit_code, result_reason) = 0)
        OR (result_code IS NOT NULL AND state = 'confirmed'
            AND kind IN ('launch_workload', 'stop_workload', 'remove_workload')
            AND (result_code <> 'succeeded' OR (coalesce(result_exit_code, 0) = 0 AND result_reason IS NULL))))
);
-- Commands whose outcome is not final: reconciled by identity after a restart.
CREATE INDEX run_commands_unresolved ON run_commands (tenant_id, project_id, run_id) WHERE state IN ('intent', 'uncertain');

-- A command keeps its identity, target, specification, fence and binding. Its
-- state follows the kernel's settlement: intent to any outcome, uncertain to
-- confirmed or rejected, confirmed and rejected final. A reason changes only
-- with a settlement, and a recorded result is kept once written.
CREATE FUNCTION kodama_run_command_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.command_id, NEW.step_key, NEW.kind, NEW.target,
        NEW.spec_digest, NEW.fence, NEW.binding_attempt, NEW.binding_backend, NEW.recorded_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.command_id, OLD.step_key, OLD.kind, OLD.target,
        OLD.spec_digest, OLD.fence, OLD.binding_attempt, OLD.binding_backend, OLD.recorded_at) THEN
        RAISE EXCEPTION 'a command keeps its identity, target, specification, fence and binding' USING ERRCODE = '23514';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF NOT ((OLD.state = 'intent' AND NEW.state IN ('confirmed', 'rejected', 'uncertain'))
                OR (OLD.state = 'uncertain' AND NEW.state IN ('confirmed', 'rejected'))) THEN
            RAISE EXCEPTION 'command cannot move from % to %', OLD.state, NEW.state USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.reason IS DISTINCT FROM OLD.reason THEN
        RAISE EXCEPTION 'a command changes its reason only by settling' USING ERRCODE = '23514';
    END IF;
    IF OLD.result_code IS NOT NULL
       AND (NEW.result_code, NEW.result_exit_code, NEW.result_reason)
           IS DISTINCT FROM (OLD.result_code, OLD.result_exit_code, OLD.result_reason) THEN
        RAISE EXCEPTION 'a command keeps its recorded result' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_commands_guard BEFORE UPDATE ON run_commands
    FOR EACH ROW EXECUTE FUNCTION kodama_run_command_guard();

-- ---------------------------------------------------------------------------
-- run_workload_observations: the latest observation of each workload attempt.
-- ---------------------------------------------------------------------------
CREATE TABLE run_workload_observations (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    step_key text NOT NULL CHECK (step_key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    attempt integer NOT NULL CHECK (attempt >= 1),
    -- The binding: identity, specification, backend and the observing fence.
    workload_id text NOT NULL CHECK (length(workload_id) BETWEEN 1 AND 256),
    spec_digest text NOT NULL CHECK (length(spec_digest) BETWEEN 1 AND 256),
    backend text NOT NULL CHECK (length(backend) BETWEEN 1 AND 256),
    fence bigint NOT NULL CHECK (fence BETWEEN 1 AND 9007199254740991),
    phase text NOT NULL CHECK (phase IN ('running', 'exited', 'absent', 'unknown')),
    result_code text CHECK (result_code IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'lost')),
    result_exit_code integer,
    result_reason text CHECK (result_reason ~ '^[a-z][a-z0-9_]{0,62}$'),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id, step_key, attempt),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_key) REFERENCES run_steps (tenant_id, project_id, run_id, step_key),
    -- Only an exited workload has a result; a success has exit code 0 and no reason.
    CONSTRAINT run_workload_observations_result_shape CHECK (
        (phase = 'exited' AND result_code IS NOT NULL
            AND (result_code <> 'succeeded' OR (coalesce(result_exit_code, 0) = 0 AND result_reason IS NULL)))
        OR (phase <> 'exited' AND num_nonnulls(result_code, result_exit_code, result_reason) = 0))
);

CREATE FUNCTION kodama_run_workload_observation_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.step_key, NEW.attempt, NEW.workload_id, NEW.backend)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.step_key, OLD.attempt, OLD.workload_id, OLD.backend) THEN
        RAISE EXCEPTION 'an observation names one workload attempt and cannot change it' USING ERRCODE = '23514';
    END IF;
    IF NEW.spec_digest IS DISTINCT FROM OLD.spec_digest THEN
        RAISE EXCEPTION 'the workload specification identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.fence < OLD.fence THEN
        RAISE EXCEPTION 'an observation fence cannot decrease' USING ERRCODE = '23514';
    END IF;
    IF NEW.observed_at < OLD.observed_at THEN
        RAISE EXCEPTION 'an observation cannot be older than the retained one' USING ERRCODE = '23514';
    END IF;
    -- An exited workload keeps its result: it may be repeated or confirmed absent.
    IF OLD.phase = 'exited'
       AND NOT (NEW.phase = 'absent'
                OR (NEW.phase = 'exited'
                    AND (NEW.result_code, NEW.result_exit_code, NEW.result_reason)
                        IS NOT DISTINCT FROM (OLD.result_code, OLD.result_exit_code, OLD.result_reason))) THEN
        RAISE EXCEPTION 'an exited workload keeps its result' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_workload_observations_guard BEFORE UPDATE ON run_workload_observations
    FOR EACH ROW EXECUTE FUNCTION kodama_run_workload_observation_guard();

-- ---------------------------------------------------------------------------
-- run_resource_observations: the latest observation of each workspace or service.
-- ---------------------------------------------------------------------------
-- One row per (observing run, kind, name, identity). The handle's scope is this
-- row's tenant and project; its owner is the tenant, with `owner_project_id`
-- null for a tenant-owned resource and the project for a project- or
-- pipeline-owned one (a tenant-lifetime service is owned by the tenant alone and
-- shared by its projects). `handle_run_id` is the handle's run, which is the
-- observing run for a pipeline-lifetime resource and may be null otherwise.
CREATE TABLE run_resource_observations (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    kind text NOT NULL CHECK (kind IN ('workspace', 'service')),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 256),
    identity text NOT NULL CHECK (length(identity) BETWEEN 1 AND 256),
    owner_project_id kodama_id,
    lifetime text NOT NULL CHECK (lifetime IN ('tenant', 'project', 'pipeline')),
    handle_run_id kodama_id,
    -- The handle's fence: the last writer's, never older than the retained one.
    fence bigint NOT NULL CHECK (fence BETWEEN 1 AND 9007199254740991),
    state text NOT NULL CHECK (state IN ('starting', 'ready', 'failed', 'stopped', 'removed', 'unknown')),
    reason text CHECK (reason ~ '^[a-z][a-z0-9_]{0,62}$'),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id, kind, name, identity),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    -- The owner follows from the lifetime and the requesting scope: the tenant
    -- alone for tenant lifetime, otherwise this project; never another project.
    CONSTRAINT run_resource_observations_owner CHECK (
        (lifetime = 'tenant') = (owner_project_id IS NULL)
        AND (owner_project_id IS NULL OR owner_project_id = project_id)),
    -- A pipeline-lifetime resource belongs to the observing run; a shared one
    -- names that run or none.
    CONSTRAINT run_resource_observations_run CHECK (
        (lifetime = 'pipeline' AND handle_run_id IS NOT NULL AND handle_run_id = run_id)
        OR (lifetime <> 'pipeline' AND (handle_run_id IS NULL OR handle_run_id = run_id))),
    -- The workspace is the run's own.
    CONSTRAINT run_resource_observations_workspace CHECK (kind <> 'workspace' OR lifetime = 'pipeline')
);

CREATE FUNCTION kodama_run_resource_observation_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.kind, NEW.name, NEW.identity, NEW.owner_project_id, NEW.lifetime, NEW.handle_run_id)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.kind, OLD.name, OLD.identity, OLD.owner_project_id, OLD.lifetime, OLD.handle_run_id) THEN
        RAISE EXCEPTION 'a resource observation names one resource, owner and lifetime and cannot change them' USING ERRCODE = '23514';
    END IF;
    IF NEW.fence < OLD.fence THEN
        RAISE EXCEPTION 'a resource fence cannot decrease' USING ERRCODE = '23514';
    END IF;
    IF NEW.observed_at < OLD.observed_at THEN
        RAISE EXCEPTION 'a resource observation cannot be older than the retained one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_resource_observations_guard BEFORE UPDATE ON run_resource_observations
    FOR EACH ROW EXECUTE FUNCTION kodama_run_resource_observation_guard();

-- ---------------------------------------------------------------------------
-- run_steps: attempt, the retained when evaluation, the workload result, and a
-- wait that is recorded before it opens.
-- ---------------------------------------------------------------------------
ALTER TABLE run_steps
    -- Current workload attempt of a container step: 0 before launch.
    ADD COLUMN attempt integer NOT NULL DEFAULT 0,
    -- Retained evaluation of the step's when predicate, written once. An
    -- unevaluated predicate (false) differs from an evaluated false result.
    ADD COLUMN when_evaluated boolean NOT NULL DEFAULT false,
    ADD COLUMN when_result boolean,
    ADD COLUMN when_input_digest text,
    ADD COLUMN when_evaluated_at timestamptz,
    -- Settled workload result of a container step, apart from the step `reason`.
    -- The exit code is the existing `exit_code`.
    ADD COLUMN result_code text,
    ADD COLUMN result_reason text,
    -- Why a cancelled wait was cancelled (run_cancelled or run_failed).
    ADD COLUMN wait_reason text;

-- A wait that cancelled before this migration has no recorded reason; give it
-- the one its step state implies. Nothing wrote wait columns before, so on a
-- database written only by the 004 schema no row matches.
UPDATE run_steps SET wait_reason = CASE WHEN status = 'failed' THEN 'run_failed' ELSE 'run_cancelled' END
    WHERE wait_state = 'cancelled';

-- Pre-open waits (the choice recorded in docs/implementation/database-schema.md):
-- the wait columns stay on the step row, and the constraints are relaxed in two
-- exact ways instead of adding a waits table. (1) A wait step that has not opened
-- (wait_state is null) may carry its identity, version and assignment, recorded at
-- admission and changed only by version-checked steps. (2) An opened wait is
-- unchanged: identity, version, expected input, correlation and deadline are all
-- required, except that a wait cancelled before it ever opened (state `cancelled`
-- with no correlation, request, expected input or deadline) is allowed. A waiting,
-- resolved or expired wait still requires correlation, expected input and deadline.
ALTER TABLE run_steps
    DROP CONSTRAINT run_steps_container_no_wait,
    DROP CONSTRAINT run_steps_wait_unopened,
    DROP CONSTRAINT run_steps_wait_open,
    DROP CONSTRAINT run_steps_wait_cancelled;

ALTER TABLE run_steps
    ADD CONSTRAINT run_steps_container_no_wait CHECK (step_kind = 'wait' OR
        num_nonnulls(wait_id, wait_assignment, wait_correlation, wait_request, wait_input, wait_outcomes,
                     wait_approving, wait_on_expiry, wait_deadline_at, wait_state, wait_outcome, wait_answer,
                     wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_version, wait_reason) = 0),
    -- A wait that has not opened carries nothing but identity, version and assignment.
    ADD CONSTRAINT run_steps_wait_unopened CHECK (wait_state IS NOT NULL OR step_kind <> 'wait' OR (
        num_nonnulls(wait_correlation, wait_request, wait_input, wait_outcomes, wait_approving, wait_on_expiry,
                     wait_deadline_at, wait_outcome, wait_answer, wait_actor_ref, wait_decided_at,
                     wait_consumed_signal_id, wait_reason) = 0
        AND (wait_id IS NULL) = (wait_version IS NULL)
        AND (wait_assignment IS NULL OR (wait_id IS NOT NULL AND length(wait_assignment) BETWEEN 1 AND 64))
        AND status IN ('pending', 'skipped', 'cancelled', 'failed'))),
    -- A recorded wait: identity and version, and either the complete opened shape
    -- or the cancelled-before-opening exception.
    ADD CONSTRAINT run_steps_wait_open CHECK (wait_state IS NULL OR (
        step_kind = 'wait'
        AND wait_state IN ('waiting', 'resolved', 'expired', 'cancelled')
        AND num_nonnulls(wait_id, wait_version) = 2
        AND (wait_assignment IS NULL OR length(wait_assignment) BETWEEN 1 AND 64)
        AND ((num_nonnulls(wait_correlation, wait_input, wait_outcomes, wait_on_expiry, wait_deadline_at) = 5
              AND kodama_object_size(wait_correlation) <= 4
              AND (wait_assignment IS NOT NULL OR kodama_object_size(wait_correlation) >= 1)
              AND (wait_request IS NULL OR octet_length(wait_request) <= 4096)
              AND wait_input ~ '^[a-z][a-z0-9_-]{0,31}$'
              AND jsonb_array_length(wait_outcomes) BETWEEN 1 AND 8
              AND NOT jsonb_exists(wait_outcomes, 'expired') AND NOT jsonb_exists(wait_outcomes, 'cancelled')
              AND (wait_approving IS NULL OR (wait_approving <> 'expired' AND jsonb_exists(wait_outcomes, wait_approving)))
              AND wait_on_expiry IN ('fail', 'continue')
              AND (wait_answer IS NULL OR octet_length(wait_answer) <= 4096)
              AND (wait_actor_ref IS NULL OR length(wait_actor_ref) BETWEEN 1 AND 256))
             OR (wait_state = 'cancelled'
                 AND num_nonnulls(wait_correlation, wait_request, wait_input, wait_outcomes, wait_approving,
                                  wait_on_expiry, wait_deadline_at) = 0)))),
    -- Cancelled with the run, opened or not: no outcome, actor or signal, and the reason.
    ADD CONSTRAINT run_steps_wait_cancelled CHECK (wait_state IS DISTINCT FROM 'cancelled' OR (
        status IN ('cancelled', 'failed', 'skipped') AND wait_decided_at IS NOT NULL
        AND wait_reason IS NOT NULL AND wait_reason IN ('run_cancelled', 'run_failed')
        AND num_nonnulls(wait_outcome, wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0)),
    ADD CONSTRAINT run_steps_wait_reason_shape CHECK (wait_state IS NOT DISTINCT FROM 'cancelled' OR wait_reason IS NULL),
    -- Only a container step has an attempt, and a pending step has not launched.
    ADD CONSTRAINT run_steps_attempt_shape CHECK (
        attempt >= 0 AND (attempt = 0 OR step_kind = 'container') AND (status <> 'pending' OR attempt = 0)),
    -- An unevaluated predicate records nothing; an evaluated one its result,
    -- input digest and time. A false result ends in skipped (condition_false),
    -- cancelled, or the pending step it has not yet moved.
    ADD CONSTRAINT run_steps_when_evidence CHECK (
        (NOT when_evaluated AND num_nonnulls(when_result, when_input_digest, when_evaluated_at) = 0)
        OR (when_evaluated AND when_result IS NOT NULL AND when_evaluated_at IS NOT NULL
            AND when_input_digest IS NOT NULL AND length(when_input_digest) BETWEEN 1 AND 128)),
    ADD CONSTRAINT run_steps_when_result_state CHECK (
        NOT when_evaluated OR when_result OR status IN ('pending', 'cancelled')
        OR (status = 'skipped' AND reason IS NOT DISTINCT FROM 'condition_false')),
    -- A container step's settled result agrees with its state; a wait has none.
    ADD CONSTRAINT run_steps_result_shape CHECK (
        (result_code IS NULL AND result_reason IS NULL)
        OR (result_code IS NOT NULL AND result_code IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'lost')
            AND step_kind = 'container' AND attempt >= 1
            AND ((status = 'succeeded' AND result_code = 'succeeded')
                 OR (status = 'failed' AND result_code IN ('failed', 'timed_out', 'lost'))
                 OR (status = 'cancelled' AND result_code = 'cancelled'))
            AND (result_code <> 'succeeded' OR (coalesce(exit_code, 0) = 0 AND result_reason IS NULL))
            AND (result_reason IS NULL OR result_reason ~ '^[a-z][a-z0-9_]{0,62}$')));

-- Replaces 004's guard. Unchanged: workload identity is immutable once set; the
-- wait version never decreases or clears; wait state moves only waiting -> a
-- decision; a decided wait is immutable. Changed: wait identity is fixed from the
-- moment it is recorded (before it opens), the opened content is fixed once the
-- wait has a state, and a change to any recorded wait (including its assignment
-- or its first opening or cancellation) must raise the version. Added: the
-- attempt never decreases, the retained when evaluation is written once, and a
-- settled step keeps its attempt and result (including the exit code).
CREATE OR REPLACE FUNCTION kodama_run_step_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.workload_id IS NOT NULL
       AND (NEW.workload_id, NEW.workload_spec_digest) IS DISTINCT FROM (OLD.workload_id, OLD.workload_spec_digest) THEN
        RAISE EXCEPTION 'workload identity is immutable once set' USING ERRCODE = '23514';
    END IF;
    IF OLD.wait_id IS NOT NULL AND NEW.wait_id IS DISTINCT FROM OLD.wait_id THEN
        RAISE EXCEPTION 'wait identity is immutable once recorded' USING ERRCODE = '23514';
    END IF;
    IF OLD.wait_state IS NOT NULL
       AND (NEW.wait_correlation, NEW.wait_request, NEW.wait_input, NEW.wait_outcomes,
            NEW.wait_approving, NEW.wait_on_expiry, NEW.wait_deadline_at)
           IS DISTINCT FROM
           (OLD.wait_correlation, OLD.wait_request, OLD.wait_input, OLD.wait_outcomes,
            OLD.wait_approving, OLD.wait_on_expiry, OLD.wait_deadline_at) THEN
        RAISE EXCEPTION 'wait identity, correlation and expected input are immutable once opened' USING ERRCODE = '23514';
    END IF;
    -- The wait version never goes backwards and is never cleared, whatever else
    -- the statement changes (or leaves unchanged).
    IF OLD.wait_version IS NOT NULL AND (NEW.wait_version IS NULL OR NEW.wait_version < OLD.wait_version) THEN
        RAISE EXCEPTION 'the wait version cannot decrease or be cleared' USING ERRCODE = '23514';
    END IF;
    IF NEW.wait_state IS DISTINCT FROM OLD.wait_state THEN
        -- coalesce: a comparison with an unopened (null) state is unknown, and an unknown
        -- condition would let the move through.
        IF NOT coalesce((OLD.wait_state IS NULL AND NEW.wait_state = 'waiting')
                OR (OLD.wait_state IS NULL AND NEW.wait_state = 'cancelled' AND NEW.wait_deadline_at IS NULL)
                OR (OLD.wait_state = 'waiting' AND NEW.wait_state IN ('resolved', 'expired', 'cancelled')), false) THEN
            RAISE EXCEPTION 'wait cannot move from % to %', coalesce(OLD.wait_state, 'unopened'), coalesce(NEW.wait_state, 'unopened')
                USING ERRCODE = '23514';
        END IF;
    END IF;
    IF OLD.wait_state IN ('resolved', 'expired', 'cancelled')
       AND (NEW.wait_assignment, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref, NEW.wait_decided_at,
            NEW.wait_consumed_signal_id, NEW.wait_version, NEW.wait_reason)
           IS DISTINCT FROM
           (OLD.wait_assignment, OLD.wait_outcome, OLD.wait_answer, OLD.wait_actor_ref, OLD.wait_decided_at,
            OLD.wait_consumed_signal_id, OLD.wait_version, OLD.wait_reason) THEN
        RAISE EXCEPTION 'a decided wait is immutable, including its version' USING ERRCODE = '23514';
    END IF;
    -- Every change to a recorded wait is one version-checked step forward.
    IF OLD.wait_id IS NOT NULL
       AND (NEW.wait_assignment, NEW.wait_state, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref,
            NEW.wait_decided_at, NEW.wait_consumed_signal_id, NEW.wait_reason)
           IS DISTINCT FROM
           (OLD.wait_assignment, OLD.wait_state, OLD.wait_outcome, OLD.wait_answer, OLD.wait_actor_ref,
            OLD.wait_decided_at, OLD.wait_consumed_signal_id, OLD.wait_reason)
       AND NEW.wait_version <= OLD.wait_version THEN
        RAISE EXCEPTION 'a wait change must increase the wait version' USING ERRCODE = '23514';
    END IF;
    IF NEW.attempt < OLD.attempt THEN
        RAISE EXCEPTION 'a step attempt cannot decrease' USING ERRCODE = '23514';
    END IF;
    -- The when predicate is evaluated once and never re-evaluated.
    IF OLD.when_evaluated
       AND (NEW.when_evaluated, NEW.when_result, NEW.when_input_digest, NEW.when_evaluated_at)
           IS DISTINCT FROM
           (OLD.when_evaluated, OLD.when_result, OLD.when_input_digest, OLD.when_evaluated_at) THEN
        RAISE EXCEPTION 'the when evaluation is recorded once and never changes' USING ERRCODE = '23514';
    END IF;
    -- A settled step is final: its attempt and result stay as recorded.
    IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled')
       AND (NEW.attempt, NEW.exit_code, NEW.result_code, NEW.result_reason)
           IS DISTINCT FROM (OLD.attempt, OLD.exit_code, OLD.result_code, OLD.result_reason) THEN
        RAISE EXCEPTION 'a settled step keeps its attempt and result' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

-- ---------------------------------------------------------------------------
-- RLS and grants for the new tables.
-- ---------------------------------------------------------------------------
ALTER TABLE run_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_commands FORCE ROW LEVEL SECURITY;
ALTER TABLE run_workload_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_workload_observations FORCE ROW LEVEL SECURITY;
ALTER TABLE run_resource_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_resource_observations FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    EXECUTE format('CREATE POLICY owner_all ON run_commands TO %I USING (true) WITH CHECK (true)', current_user);
    EXECUTE format('CREATE POLICY owner_all ON run_workload_observations TO %I USING (true) WITH CHECK (true)', current_user);
    EXECUTE format('CREATE POLICY owner_all ON run_resource_observations TO %I USING (true) WITH CHECK (true)', current_user);
END $$;
CREATE POLICY tenant_isolation ON run_commands
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON run_workload_observations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON run_resource_observations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- The runtime role inserts rows and moves only their state columns: a command
-- its state, reason and result; an observation its phase or state, fence, result
-- and time. Identity, target, specification and binding columns are insert-only.
-- Nothing is deletable by the runtime role.
GRANT SELECT, INSERT ON run_commands, run_workload_observations, run_resource_observations TO {{runtime_role}};
GRANT UPDATE (state, reason, result_code, result_exit_code, result_reason) ON run_commands TO {{runtime_role}};
GRANT UPDATE (fence, phase, result_code, result_exit_code, result_reason, observed_at) ON run_workload_observations TO {{runtime_role}};
GRANT UPDATE (fence, state, reason, observed_at) ON run_resource_observations TO {{runtime_role}};
-- Claim purpose and the cleanup reason are lifecycle columns; the cleanup bound
-- is fixed at admission.
GRANT UPDATE (lease_purpose, cleanup_reason) ON runs TO {{runtime_role}};
-- The attempt, the write-once when evaluation, the step result and the wait
-- cancellation reason.
GRANT UPDATE (attempt, when_evaluated, when_result, when_input_digest, when_evaluated_at,
    result_code, result_reason, wait_reason) ON run_steps TO {{runtime_role}};
