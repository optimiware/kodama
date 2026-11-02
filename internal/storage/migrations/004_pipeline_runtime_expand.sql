-- Pipeline-runtime schema, stages 1 and 2 of P2.1 (expand, then validate and map).
-- Appended after the Git hosting contribution: it changes no earlier object
-- except by adding columns, constraints, indexes, one table and grants. Nothing
-- is dropped. `projects.task_workflow` and the task columns `state`, `parked_at`,
-- `resume_state`, `blocked_by_id`, `resolution` and `resolved_at` stay readable
-- and writable by the current composition until the later contract stage.
--
-- The whole file is one transaction (the runner applies it atomically with its
-- ledger row). Validation runs first: a refusal leaves the database exactly as
-- it was and names what an operator must resolve by hand. The migration never
-- invents a pipeline, merges two definitions or picks a repository.

-- Exclude concurrent application writers for the whole of validation and mapping.
-- The migration runner's advisory lock serializes runners, not application
-- transactions: without this lock a run (or a pipeline) committed after the checks
-- but before the mapping would be mapped by the mapping without ever having been
-- validated. SHARE ROW EXCLUSIVE conflicts with the row-level write locks that
-- INSERT, UPDATE and DELETE take, so writers wait until this transaction ends,
-- while readers continue; it also conflicts with itself, so two migrations cannot
-- interleave. The lock is held to commit, through the mapping and every later
-- statement. One statement acquires all three tables, in the same order every time.
LOCK TABLE pipelines, runs, tasks IN SHARE ROW EXCLUSIVE MODE;

-- Stage 2a: refuse duplicate project-scoped pipeline names. The baseline unique
-- key includes the repository, so one name can be defined by an API row and by
-- one or more repositories; name resolution would then depend on row order.
DO $$
DECLARE
    dup_count integer;
    dup record;
BEGIN
    SELECT count(*) INTO dup_count FROM (
        SELECT 1 FROM pipelines GROUP BY tenant_id, project_id, name HAVING count(*) > 1
    ) d;
    IF dup_count > 0 THEN
        SELECT tenant_id, project_id, name, count(*) AS definitions INTO dup
            FROM pipelines GROUP BY tenant_id, project_id, name HAVING count(*) > 1
            ORDER BY tenant_id, project_id, name LIMIT 1;
        RAISE EXCEPTION 'pipelines: % project-scoped name(s) are defined by more than one row (first: tenant %, project %, name %, % rows); resolve the conflicting definition sources explicitly, nothing was changed',
            dup_count, dup.tenant_id, dup.project_id, dup.name, dup.definitions
            USING ERRCODE = '23505';
    END IF;
END $$;

-- Stage 2b: refuse an ambiguous task-to-pipeline mapping. A task selects one
-- pipeline; the only existing evidence of a selection is its runs. A task whose
-- runs name more than one pipeline cannot be mapped without choosing one.
DO $$
DECLARE
    amb_count integer;
    amb record;
BEGIN
    SELECT count(*) INTO amb_count FROM (
        SELECT 1 FROM runs WHERE task_id IS NOT NULL
        GROUP BY tenant_id, project_id, task_id HAVING count(DISTINCT pipeline_id) > 1
    ) a;
    IF amb_count > 0 THEN
        SELECT tenant_id, project_id, task_id, count(DISTINCT pipeline_id) AS pipelines INTO amb
            FROM runs WHERE task_id IS NOT NULL
            GROUP BY tenant_id, project_id, task_id HAVING count(DISTINCT pipeline_id) > 1
            ORDER BY tenant_id, project_id, task_id LIMIT 1;
        RAISE EXCEPTION 'tasks: % task(s) have runs on more than one pipeline and cannot be mapped to one (first: tenant %, project %, task %, % pipelines); map them explicitly, nothing was changed',
            amb_count, amb.tenant_id, amb.project_id, amb.task_id, amb.pipelines
            USING ERRCODE = '23514';
    END IF;
END $$;

-- A pipeline name is unique inside its project. The baseline key
-- (tenant, project, repository, name) is left in place; it is implied by this one
-- and is retired with the other legacy objects at the contract stage.
ALTER TABLE pipelines ADD CONSTRAINT pipelines_name_unique UNIQUE (tenant_id, project_id, name);

-- Number of keys of a JSON object, usable in a CHECK (a subquery is not).
CREATE FUNCTION kodama_object_size(o jsonb) RETURNS integer
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$ SELECT count(*)::integer FROM jsonb_object_keys(o) $$;

-- ---------------------------------------------------------------------------
-- runs: lease, workload/resource identity, cleanup.
-- ---------------------------------------------------------------------------
ALTER TABLE runs
    -- Durable claim: owner identity, expiry, and a fence that only grows. A writer
    -- holding an older fence is stale after takeover.
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN lease_fence bigint NOT NULL DEFAULT 0,
    -- Stable identity and immutable specification identity of the run's
    -- workspace, preparation and service resources.
    ADD COLUMN resource_identity text,
    ADD COLUMN resource_spec_digest text,
    -- Cleanup is tracked apart from the execution outcome: none (no resource),
    -- pending (stop/remove in progress, bounded retries), retained (retries
    -- exhausted; resources stay visible) or done (confirmed stopped and removed).
    ADD COLUMN cleanup_state text NOT NULL DEFAULT 'none',
    ADD COLUMN cleanup_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN cleanup_next_at timestamptz,
    ADD COLUMN retained_resources kodama_array NOT NULL DEFAULT '[]',
    ADD CONSTRAINT runs_lease_owner_shape CHECK (lease_owner IS NULL OR length(lease_owner) BETWEEN 1 AND 256),
    ADD CONSTRAINT runs_lease_pair CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    ADD CONSTRAINT runs_lease_fence_range CHECK (lease_fence BETWEEN 0 AND 9007199254740991),
    ADD CONSTRAINT runs_resource_identity_shape CHECK (resource_identity IS NULL OR length(resource_identity) BETWEEN 1 AND 256),
    ADD CONSTRAINT runs_resource_spec_digest_shape CHECK (resource_spec_digest IS NULL OR resource_spec_digest ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT runs_cleanup_state_known CHECK (cleanup_state IN ('none', 'pending', 'retained', 'done')),
    ADD CONSTRAINT runs_cleanup_attempts_range CHECK (cleanup_attempts >= 0),
    ADD CONSTRAINT runs_cleanup_next_shape CHECK (cleanup_next_at IS NULL OR cleanup_state = 'pending'),
    ADD CONSTRAINT runs_retained_resources_shape CHECK (
        jsonb_array_length(retained_resources) <= 256
        AND (cleanup_state <> 'retained' OR jsonb_array_length(retained_resources) > 0)
        AND (cleanup_state <> 'done' OR jsonb_array_length(retained_resources) = 0)),
    -- Target of tasks.current_run_id: a run is current only for the task and
    -- project it belongs to.
    ADD CONSTRAINT runs_task_key UNIQUE (tenant_id, project_id, task_id, id);
CREATE INDEX runs_lease_expiry ON runs (tenant_id, lease_expires_at, id) WHERE lease_owner IS NOT NULL;
CREATE INDEX runs_cleanup_due ON runs (tenant_id, cleanup_next_at, id) WHERE cleanup_state IN ('pending', 'retained');
CREATE INDEX runs_task ON runs (tenant_id, project_id, task_id, created_at DESC, id) WHERE task_id IS NOT NULL;

CREATE FUNCTION kodama_run_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- The fence never decreases, and every new holder takes a strictly higher one.
    -- Renewal (same owner) and release (owner cleared) keep it.
    IF NEW.lease_fence < OLD.lease_fence THEN
        RAISE EXCEPTION 'run lease fence cannot decrease' USING ERRCODE = '23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND NEW.lease_owner IS DISTINCT FROM OLD.lease_owner
       AND NEW.lease_fence <= OLD.lease_fence THEN
        RAISE EXCEPTION 'a new run lease holder must take a higher fence' USING ERRCODE = '23514';
    END IF;
    IF OLD.resource_identity IS NOT NULL
       AND (NEW.resource_identity, NEW.resource_spec_digest) IS DISTINCT FROM (OLD.resource_identity, OLD.resource_spec_digest) THEN
        RAISE EXCEPTION 'run resource identity is immutable once set' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER runs_guard BEFORE UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION kodama_run_guard();

-- ---------------------------------------------------------------------------
-- signal_records: durable, self-contained, non-expiring signal identity.
-- ---------------------------------------------------------------------------
-- Identity is (tenant, project, caller principal, caller key). A decision record
-- keeps the original target (plain identifiers, deliberately no foreign key to
-- run, step or wait: ordinary retention may remove those) and the complete
-- replayable decision, so a replay never reads them. A tombstone keeps only the
-- identity, digest, reason and time of a refusal that resolved no target. Rows
-- are append-only for the runtime role and are removed only with their project.
CREATE TABLE signal_records (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    caller_principal text NOT NULL CHECK (length(caller_principal) BETWEEN 1 AND 512),
    caller_key text NOT NULL CHECK (length(caller_key) BETWEEN 1 AND 128),
    -- Opaque identity derived by the shell from the tuple; the only form that
    -- appears in events and in run_steps.wait_consumed_signal_id.
    signal_id kodama_id NOT NULL,
    shape text NOT NULL CHECK (shape IN ('decision', 'tombstone')),
    -- Digest of the request (address, outcome, answer).
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    -- True for the winning signal; false for every refusal.
    applied boolean NOT NULL,
    refusal_reason text CHECK (refusal_reason IN ('decided', 'version_conflict', 'invalid_outcome', 'unknown_wait', 'ambiguous_correlation')),
    target_run_id kodama_id,
    target_step_id kodama_id,
    target_wait_id kodama_id,
    decision_kind text CHECK (decision_kind IN ('resolved', 'expired', 'cancelled')),
    decision_outcome text CHECK (decision_outcome ~ '^[a-z][a-z0-9_]{0,31}$'),
    decision_answer text CHECK (octet_length(decision_answer) <= 4096),
    decision_actor_ref text CHECK (length(decision_actor_ref) BETWEEN 1 AND 256),
    decision_version kodama_version,
    decided_at timestamptz,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, caller_principal, caller_key),
    UNIQUE (tenant_id, project_id, signal_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT signal_records_tombstone_shape CHECK (shape <> 'tombstone' OR (
        NOT applied AND refusal_reason IS NOT NULL AND refusal_reason IN ('unknown_wait', 'ambiguous_correlation')
        AND num_nonnulls(target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome,
                         decision_answer, decision_actor_ref, decision_version, decided_at) = 0)),
    CONSTRAINT signal_records_decision_target CHECK (shape <> 'decision' OR (
        num_nonnulls(target_run_id, target_step_id, target_wait_id) = 3
        AND (refusal_reason IS NULL OR refusal_reason IN ('decided', 'version_conflict', 'invalid_outcome'))
        AND applied = (refusal_reason IS NULL))),
    -- Either a complete decision or none. A refusal that lost to a decision
    -- records it; version_conflict and invalid_outcome have none to record.
    CONSTRAINT signal_records_decision_complete CHECK (shape <> 'decision' OR (
        (decision_kind IS NULL
            AND refusal_reason IS NOT NULL AND refusal_reason IN ('version_conflict', 'invalid_outcome')
            AND num_nonnulls(decision_outcome, decision_answer, decision_actor_ref, decision_version, decided_at) = 0)
        OR (decision_kind IS NOT NULL AND decision_version IS NOT NULL AND decided_at IS NOT NULL
            AND (refusal_reason IS NULL OR refusal_reason = 'decided')
            AND (decision_kind = 'resolved') = (decision_actor_ref IS NOT NULL)
            AND (decision_kind <> 'resolved' OR decision_outcome IS NOT NULL)
            AND (decision_kind = 'resolved' OR decision_answer IS NULL)
            AND (decision_kind <> 'expired' OR decision_outcome IS NULL OR decision_outcome = 'expired')
            AND (decision_kind <> 'cancelled' OR decision_outcome IS NULL)))),
    CONSTRAINT signal_records_winner_resolved CHECK (NOT applied OR decision_kind IS NOT DISTINCT FROM 'resolved')
);
-- A wait has at most one winning signal.
CREATE UNIQUE INDEX signal_records_one_winner ON signal_records (tenant_id, project_id, target_wait_id) WHERE applied;
CREATE INDEX signal_records_target ON signal_records (tenant_id, project_id, target_wait_id) WHERE target_wait_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- run_steps: step kind, workload identity, command intent, workspace exclusion,
-- and the durable wait.
-- ---------------------------------------------------------------------------
-- Replace the checks that require an image and list the step states: a wait has
-- no image, and a wait that is open has the state `waiting`.
ALTER TABLE run_steps DROP CONSTRAINT run_steps_image_check;
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ALTER COLUMN image DROP NOT NULL;
ALTER TABLE run_steps
    ADD COLUMN step_kind text NOT NULL DEFAULT 'container',
    -- Stable identity of the step's workload and the identity of the exact
    -- specification it was launched with, so a delayed or replayed create can be
    -- recognised and refused.
    ADD COLUMN workload_id text,
    ADD COLUMN workload_spec_digest text,
    -- What the engine committed to do before the effect, under which lease fence.
    ADD COLUMN command_intent text,
    ADD COLUMN command_intent_fence bigint,
    ADD COLUMN command_intent_at timestamptz,
    -- Conservative launch evidence: only not_started permits a retry.
    ADD COLUMN launch_evidence text NOT NULL DEFAULT 'none',
    -- The step currently holds the run's one workspace.
    ADD COLUMN workspace_held boolean NOT NULL DEFAULT false,
    -- Wait (null for a container step, and for a wait that has not opened).
    ADD COLUMN wait_id kodama_id,
    ADD COLUMN wait_assignment text,
    ADD COLUMN wait_correlation kodama_object,
    ADD COLUMN wait_request text,
    ADD COLUMN wait_input text,
    ADD COLUMN wait_outcomes kodama_array,
    ADD COLUMN wait_approving text,
    ADD COLUMN wait_on_expiry text,
    ADD COLUMN wait_deadline_at timestamptz,
    ADD COLUMN wait_state text,
    -- Result: the decision, recorded once.
    ADD COLUMN wait_outcome text,
    ADD COLUMN wait_answer text,
    ADD COLUMN wait_actor_ref text,
    ADD COLUMN wait_decided_at timestamptz,
    ADD COLUMN wait_consumed_signal_id kodama_id,
    ADD COLUMN wait_version kodama_version;

ALTER TABLE run_steps
    ADD CONSTRAINT run_steps_status_check CHECK (status IN ('pending', 'running', 'waiting', 'succeeded', 'failed', 'skipped', 'cancelled')),
    ADD CONSTRAINT run_steps_kind_known CHECK (step_kind IN ('container', 'wait')),
    ADD CONSTRAINT run_steps_waiting_is_wait CHECK (status <> 'waiting' OR step_kind = 'wait'),
    ADD CONSTRAINT run_steps_image_shape CHECK (
        (step_kind = 'container' AND image IS NOT NULL AND length(image) > 0) OR
        (step_kind = 'wait' AND image IS NULL)),
    ADD CONSTRAINT run_steps_workload_shape CHECK (
        (workload_id IS NULL OR length(workload_id) BETWEEN 1 AND 256)
        AND (workload_spec_digest IS NULL OR (workload_id IS NOT NULL AND workload_spec_digest ~ '^[0-9a-f]{64}$'))),
    ADD CONSTRAINT run_steps_intent_shape CHECK (
        num_nonnulls(command_intent, command_intent_fence, command_intent_at) IN (0, 3)
        AND (command_intent IS NULL OR (command_intent IN ('launch', 'stop', 'remove')
             AND workload_id IS NOT NULL AND command_intent_fence BETWEEN 0 AND 9007199254740991))),
    ADD CONSTRAINT run_steps_launch_evidence_known CHECK (
        launch_evidence IN ('none', 'not_started', 'started', 'uncertain')
        AND (launch_evidence = 'none' OR workload_id IS NOT NULL)),
    ADD CONSTRAINT run_steps_workspace_held_shape CHECK (NOT workspace_held OR (status = 'running' AND step_kind = 'container')),
    -- A wait step runs no command and owns no workload.
    ADD CONSTRAINT run_steps_wait_holds_nothing CHECK (step_kind = 'container' OR
        num_nonnulls(runtime_id, exit_code, log_uri, workload_id, workload_spec_digest, command_intent) = 0),
    -- A container step has no wait columns.
    ADD CONSTRAINT run_steps_container_no_wait CHECK (step_kind = 'wait' OR
        num_nonnulls(wait_id, wait_assignment, wait_correlation, wait_request, wait_input, wait_outcomes,
                     wait_approving, wait_on_expiry, wait_deadline_at, wait_state, wait_outcome, wait_answer,
                     wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_version) = 0),
    -- A wait that has not opened carries nothing, and is not waiting.
    ADD CONSTRAINT run_steps_wait_unopened CHECK (wait_state IS NOT NULL OR step_kind <> 'wait' OR (
        num_nonnulls(wait_id, wait_assignment, wait_correlation, wait_request, wait_input, wait_outcomes,
                     wait_approving, wait_on_expiry, wait_deadline_at, wait_outcome, wait_answer,
                     wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_version) = 0
        AND status IN ('pending', 'skipped', 'cancelled', 'failed'))),
    -- An opened wait: its identity, expected input and deadline are complete,
    -- bounded, and it is assigned, correlated, or both.
    ADD CONSTRAINT run_steps_wait_open CHECK (wait_state IS NULL OR (
        step_kind = 'wait'
        AND wait_state IN ('waiting', 'resolved', 'expired', 'cancelled')
        AND num_nonnulls(wait_id, wait_correlation, wait_input, wait_outcomes, wait_on_expiry, wait_deadline_at, wait_version) = 7
        AND (wait_assignment IS NULL OR length(wait_assignment) BETWEEN 1 AND 64)
        AND kodama_object_size(wait_correlation) <= 4
        AND (wait_assignment IS NOT NULL OR kodama_object_size(wait_correlation) >= 1)
        AND (wait_request IS NULL OR octet_length(wait_request) <= 4096)
        AND wait_input ~ '^[a-z][a-z0-9_-]{0,31}$'
        AND jsonb_array_length(wait_outcomes) BETWEEN 1 AND 8
        AND NOT jsonb_exists(wait_outcomes, 'expired') AND NOT jsonb_exists(wait_outcomes, 'cancelled')
        AND (wait_approving IS NULL OR (wait_approving <> 'expired' AND jsonb_exists(wait_outcomes, wait_approving)))
        AND wait_on_expiry IN ('fail', 'continue')
        AND (wait_answer IS NULL OR octet_length(wait_answer) <= 4096)
        AND (wait_actor_ref IS NULL OR length(wait_actor_ref) BETWEEN 1 AND 256))),
    ADD CONSTRAINT run_steps_wait_waiting CHECK (wait_state IS DISTINCT FROM 'waiting' OR (
        status = 'waiting'
        AND num_nonnulls(wait_outcome, wait_answer, wait_actor_ref, wait_decided_at, wait_consumed_signal_id) = 0)),
    -- Resolved: an outcome from the declared set, the actor, and the consumed signal.
    ADD CONSTRAINT run_steps_wait_resolved CHECK (wait_state IS DISTINCT FROM 'resolved' OR (
        status = 'succeeded'
        AND wait_outcome IS NOT NULL AND jsonb_exists(wait_outcomes, wait_outcome)
        AND wait_actor_ref IS NOT NULL AND wait_decided_at IS NOT NULL AND wait_consumed_signal_id IS NOT NULL)),
    -- Expired: by the deadline, never by a signal. `continue` records the reserved
    -- outcome and succeeds; `fail` fails the step.
    ADD CONSTRAINT run_steps_wait_expired CHECK (wait_state IS DISTINCT FROM 'expired' OR (
        wait_decided_at IS NOT NULL
        AND num_nonnulls(wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0
        AND ((wait_on_expiry = 'continue' AND status = 'succeeded' AND wait_outcome IS NOT DISTINCT FROM 'expired')
             OR (wait_on_expiry = 'fail' AND status = 'failed' AND wait_outcome IS NULL)))),
    -- Cancelled with the run: no outcome, actor or signal.
    ADD CONSTRAINT run_steps_wait_cancelled CHECK (wait_state IS DISTINCT FROM 'cancelled' OR (
        status IN ('cancelled', 'failed', 'skipped') AND wait_decided_at IS NOT NULL
        AND num_nonnulls(wait_outcome, wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0)),
    ADD CONSTRAINT run_steps_wait_identity UNIQUE (tenant_id, project_id, wait_id),
    -- The winning signal's record commits in the same transaction as the decision,
    -- in either order; the record outlives the step.
    ADD CONSTRAINT run_steps_wait_signal_fk FOREIGN KEY (tenant_id, project_id, wait_consumed_signal_id)
        REFERENCES signal_records (tenant_id, project_id, signal_id) DEFERRABLE INITIALLY DEFERRED;

-- One executable step holds the workspace at a time.
CREATE UNIQUE INDEX run_steps_workspace_exclusive ON run_steps (tenant_id, project_id, run_id) WHERE workspace_held;
-- Due waits (reconciliation scans one tenant at a time), the role inbox, and
-- signal routing by exact correlation over open waits only.
CREATE INDEX run_steps_wait_due ON run_steps (tenant_id, wait_deadline_at, project_id, run_id, id) WHERE wait_state = 'waiting';
CREATE INDEX run_steps_wait_inbox ON run_steps (tenant_id, project_id, wait_assignment, wait_deadline_at)
    WHERE wait_state = 'waiting' AND wait_assignment IS NOT NULL;
CREATE INDEX run_steps_wait_correlation ON run_steps (tenant_id, project_id, wait_correlation) WHERE wait_state = 'waiting';

CREATE FUNCTION kodama_run_step_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.workload_id IS NOT NULL
       AND (NEW.workload_id, NEW.workload_spec_digest) IS DISTINCT FROM (OLD.workload_id, OLD.workload_spec_digest) THEN
        RAISE EXCEPTION 'workload identity is immutable once set' USING ERRCODE = '23514';
    END IF;
    IF OLD.wait_id IS NOT NULL
       AND (NEW.wait_id, NEW.wait_correlation, NEW.wait_request, NEW.wait_input, NEW.wait_outcomes,
            NEW.wait_approving, NEW.wait_on_expiry, NEW.wait_deadline_at)
           IS DISTINCT FROM
           (OLD.wait_id, OLD.wait_correlation, OLD.wait_request, OLD.wait_input, OLD.wait_outcomes,
            OLD.wait_approving, OLD.wait_on_expiry, OLD.wait_deadline_at) THEN
        RAISE EXCEPTION 'wait identity, correlation and expected input are immutable once opened' USING ERRCODE = '23514';
    END IF;
    -- The wait version never goes backwards and is never cleared, whatever else
    -- the statement changes (or leaves unchanged).
    IF OLD.wait_version IS NOT NULL AND (NEW.wait_version IS NULL OR NEW.wait_version < OLD.wait_version) THEN
        RAISE EXCEPTION 'the wait version cannot decrease or be cleared' USING ERRCODE = '23514';
    END IF;
    IF NEW.wait_state IS DISTINCT FROM OLD.wait_state THEN
        IF NOT ((OLD.wait_state IS NULL AND NEW.wait_state = 'waiting')
                OR (OLD.wait_state = 'waiting' AND NEW.wait_state IN ('resolved', 'expired', 'cancelled'))) THEN
            RAISE EXCEPTION 'wait cannot move from % to %', coalesce(OLD.wait_state, 'unopened'), coalesce(NEW.wait_state, 'unopened')
                USING ERRCODE = '23514';
        END IF;
    END IF;
    IF OLD.wait_state IN ('resolved', 'expired', 'cancelled')
       AND (NEW.wait_assignment, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref, NEW.wait_decided_at, NEW.wait_consumed_signal_id, NEW.wait_version)
           IS DISTINCT FROM
           (OLD.wait_assignment, OLD.wait_outcome, OLD.wait_answer, OLD.wait_actor_ref, OLD.wait_decided_at, OLD.wait_consumed_signal_id, OLD.wait_version) THEN
        RAISE EXCEPTION 'a decided wait is immutable, including its version' USING ERRCODE = '23514';
    END IF;
    -- Every change to an opened wait is one version-checked step forward.
    IF OLD.wait_state IS NOT NULL
       AND (NEW.wait_assignment, NEW.wait_state, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref, NEW.wait_decided_at, NEW.wait_consumed_signal_id)
           IS DISTINCT FROM
           (OLD.wait_assignment, OLD.wait_state, OLD.wait_outcome, OLD.wait_answer, OLD.wait_actor_ref, OLD.wait_decided_at, OLD.wait_consumed_signal_id)
       AND NEW.wait_version <= OLD.wait_version THEN
        RAISE EXCEPTION 'a wait change must increase the wait version' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_steps_guard BEFORE UPDATE ON run_steps
    FOR EACH ROW EXECUTE FUNCTION kodama_run_step_guard();

-- ---------------------------------------------------------------------------
-- tasks: selected pipeline, current run and the execution projection. All
-- nullable at this stage; the legacy workflow columns are untouched. The
-- existing `assigned_role` column serves as the projected assignment role.
-- ---------------------------------------------------------------------------
ALTER TABLE tasks
    ADD COLUMN pipeline_id kodama_id,
    ADD COLUMN current_run_id kodama_id,
    ADD COLUMN status text,
    ADD COLUMN current_step_key text,
    ADD COLUMN waiting_for text,
    ADD COLUMN result kodama_object,
    ADD COLUMN progress_version kodama_version,
    ADD COLUMN progress_run_version kodama_version;

-- Map each task that has runs to the one pipeline those runs name. Validation
-- above guarantees at most one; a task with no run stays unmapped for an explicit
-- mapping before the contract stage, and a pipeline is never chosen for it.
UPDATE tasks t SET pipeline_id = m.pipeline_id
    FROM (SELECT tenant_id, project_id, task_id, min(pipeline_id) AS pipeline_id
          FROM runs WHERE task_id IS NOT NULL GROUP BY tenant_id, project_id, task_id) m
    WHERE t.tenant_id = m.tenant_id AND t.project_id = m.project_id AND t.id = m.task_id
      AND t.pipeline_id IS NULL;

ALTER TABLE tasks
    ADD CONSTRAINT tasks_status_known CHECK (status IN ('pending', 'queued', 'running', 'waiting', 'succeeded', 'failed', 'cancelled')),
    ADD CONSTRAINT tasks_current_step_key_shape CHECK (current_step_key IS NULL OR current_step_key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    ADD CONSTRAINT tasks_waiting_for_shape CHECK (waiting_for IS NULL OR (status IS NOT DISTINCT FROM 'waiting' AND length(waiting_for) BETWEEN 1 AND 64)),
    ADD CONSTRAINT tasks_result_bounded CHECK (result IS NULL OR octet_length(result::text) <= 16384),
    ADD CONSTRAINT tasks_progress_pair CHECK ((progress_version IS NULL) = (progress_run_version IS NULL)),
    -- A task with no current run has nothing projected beyond `pending`; one with
    -- a current run has a selected pipeline and a status.
    ADD CONSTRAINT tasks_projection_shape CHECK (
        (current_run_id IS NULL AND (status IS NULL OR status = 'pending')
            AND current_step_key IS NULL AND waiting_for IS NULL AND result IS NULL)
        OR (current_run_id IS NOT NULL AND pipeline_id IS NOT NULL AND status IS NOT NULL AND status <> 'pending')),
    -- The selected pipeline is in the task's own project.
    ADD CONSTRAINT tasks_pipeline_fk FOREIGN KEY (tenant_id, project_id, pipeline_id)
        REFERENCES pipelines (tenant_id, project_id, id),
    -- The current run belongs to this task and this project, not merely the
    -- project. Unchecked (MATCH SIMPLE) while current_run_id is null.
    ADD CONSTRAINT tasks_current_run_fk FOREIGN KEY (tenant_id, project_id, id, current_run_id)
        REFERENCES runs (tenant_id, project_id, task_id, id);
CREATE INDEX tasks_current_run ON tasks (tenant_id, project_id, current_run_id) WHERE current_run_id IS NOT NULL;
CREATE INDEX tasks_pipeline ON tasks (tenant_id, project_id, pipeline_id) WHERE pipeline_id IS NOT NULL;

CREATE FUNCTION kodama_task_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.pipeline_id IS NOT NULL AND NEW.pipeline_id IS DISTINCT FROM OLD.pipeline_id THEN
        RAISE EXCEPTION 'a task keeps its selected pipeline' USING ERRCODE = '23514';
    END IF;
    -- A projection, once initialized, is never cleared and never goes backwards.
    -- Each comparison handles NULL explicitly: an unknown result would pass.
    IF OLD.progress_version IS NOT NULL
       AND (NEW.progress_version IS NULL OR NEW.progress_version < OLD.progress_version) THEN
        RAISE EXCEPTION 'task progress version cannot decrease or be cleared' USING ERRCODE = '23514';
    END IF;
    IF OLD.progress_run_version IS NOT NULL AND NEW.progress_run_version IS NULL THEN
        RAISE EXCEPTION 'task progress run version cannot be cleared' USING ERRCODE = '23514';
    END IF;
    -- The run version belongs to the current run: it may restart for a different
    -- run, but never decreases for the same one.
    IF OLD.progress_run_version IS NOT NULL
       AND NEW.current_run_id IS NOT DISTINCT FROM OLD.current_run_id
       AND NEW.progress_run_version < OLD.progress_run_version THEN
        RAISE EXCEPTION 'task progress run version cannot decrease for the same run' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER tasks_guard BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION kodama_task_guard();

-- ---------------------------------------------------------------------------
-- RLS and grants.
-- ---------------------------------------------------------------------------
ALTER TABLE signal_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE signal_records FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    EXECUTE format('CREATE POLICY owner_all ON signal_records TO %I USING (true) WITH CHECK (true)', current_user);
END $$;
CREATE POLICY tenant_isolation ON signal_records
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Signal records are append-only: no UPDATE and no DELETE for the runtime role;
-- they leave only through the project's removal (foreign key cascade).
GRANT SELECT, INSERT ON signal_records TO {{runtime_role}};
-- Lease, intent, identity and cleanup progress are lifecycle columns; admitted
-- inputs and snapshots stay immutable. Wait identity and correlation can be set
-- once (the guard triggers refuse a change); a decided wait is immutable.
GRANT UPDATE (lease_owner, lease_expires_at, lease_fence, resource_identity, resource_spec_digest,
    cleanup_state, cleanup_attempts, cleanup_next_at, retained_resources) ON runs TO {{runtime_role}};
GRANT UPDATE (workload_id, workload_spec_digest, command_intent, command_intent_fence, command_intent_at,
    launch_evidence, workspace_held, wait_id, wait_assignment, wait_correlation, wait_request, wait_input,
    wait_outcomes, wait_approving, wait_on_expiry, wait_deadline_at, wait_state, wait_outcome, wait_answer,
    wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_version) ON run_steps TO {{runtime_role}};
GRANT UPDATE (pipeline_id, current_run_id, status, current_step_key, waiting_for, result,
    progress_version, progress_run_version) ON tasks TO {{runtime_role}};
