-- T-020 execution schema: runs, step runs, attempts, controller bookkeeping,
-- append-only history and evidence, capacity reservations and the idempotent
-- work-item cancellation intent.
--
-- Ownership and integrity enforced by the database, not only by Go:
--   * every row carries tenant_id and project_id; foreign keys are composite so a
--     run, step, attempt, reservation or history row can only reference rows of
--     its own project, and a run can only pin a definition of its own project
--     with the exact digest it recorded;
--   * forced row level security on kodama_tenant_id(), column-scoped runtime
--     grants and no DELETE anywhere: execution records are history;
--   * the pinned inputs of a run (definition, work item revision, specification,
--     source commit, resolved inputs and secret versions) and the identity of a
--     step or attempt are immutable, terminal rows cannot change at all, and
--     status edges are one-way (guards below apply to every role, the owner
--     included);
--   * one attempt ordinal and one Job name per step/tenant, and no second attempt
--     of a step while an earlier one is still unsettled;
--   * the immutable outcome of an attempt lives apart from its mutable controller
--     bookkeeping (attempt_controls) and from append-only evidence
--     (attempt_evidence): late observations and reconciliation never rewrite a
--     terminal outcome.
--
-- Cross-module constraints that need these tables (workspace writer/publication
-- attempts, artifact producers, secret pins) are separate, explicitly ordered
-- migration contributions: T020_001, T020_003 and T020_004.

CREATE TABLE runs (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    purpose            text NOT NULL
                       CONSTRAINT runs_purpose_known CHECK (purpose IN ('work', 'cleanup')),
    work_item_id       kodama_id,
    workspace_id       kodama_id,
    deployment_id      kodama_id,
    definition_id      kodama_id NOT NULL,
    definition_digest  text NOT NULL
                       CONSTRAINT runs_definition_digest_syntax CHECK (definition_digest ~ '^sha256:[0-9a-f]{64}$'),
    content_revision   bigint
                       CONSTRAINT runs_content_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    specification      jsonb
                       CONSTRAINT runs_specification_bounds CHECK (jsonb_typeof(specification) = 'object' AND pg_column_size(specification) <= 1572864),
    source_commit      text
                       CONSTRAINT runs_source_commit_syntax CHECK (source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    -- Resolved inputs with provenance and the exact secret versions per step.
    -- Never plaintext.
    inputs             jsonb NOT NULL
                       CONSTRAINT runs_inputs_bounds CHECK (jsonb_typeof(inputs) = 'object' AND pg_column_size(inputs) <= 1048576),
    snapshot_digest    text NOT NULL
                       CONSTRAINT runs_snapshot_digest_syntax CHECK (snapshot_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_retention   text NOT NULL
                       CONSTRAINT runs_source_retention_known CHECK (source_retention IN ('not_required', 'pending', 'confirmed')),
    retained_ref_id    kodama_id,
    requested_by_type  text NOT NULL
                       CONSTRAINT runs_requested_by_type_known CHECK (requested_by_type IN ('user', 'service', 'workload', 'operator')),
    requested_by_id    text NOT NULL
                       CONSTRAINT runs_requested_by_id_length CHECK (char_length(requested_by_id) BETWEEN 1 AND 256),
    status             text NOT NULL DEFAULT 'queued'
                       CONSTRAINT runs_status_known CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    status_reason      text NOT NULL
                       CONSTRAINT runs_reason_code CHECK (status_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    cancel_requested_at timestamptz,
    cancel_reason      text CONSTRAINT runs_cancel_reason_code CHECK (cancel_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    failure_intent_at  timestamptz,
    failure_reason     text CONSTRAINT runs_failure_reason_code CHECK (failure_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    version            bigint NOT NULL DEFAULT 1
                       CONSTRAINT runs_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    started_at         timestamptz,
    finished_at        timestamptz,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    -- The run proves it pinned this definition's exact content.
    FOREIGN KEY (tenant_id, project_id, definition_id, definition_digest)
        REFERENCES pipeline_definitions (tenant_id, project_id, id, digest),
    -- A purpose=work run requires an actionable work item and its workspace
    -- source; a cleanup run is project-scoped, uses no workspace and is bound to
    -- one recorded deployment.
    CONSTRAINT runs_purpose_shape CHECK (
        (purpose = 'work' AND work_item_id IS NOT NULL AND workspace_id IS NOT NULL AND deployment_id IS NULL
            AND content_revision IS NOT NULL AND specification IS NOT NULL AND source_commit IS NOT NULL
            AND source_retention IN ('pending', 'confirmed') AND retained_ref_id IS NOT NULL)
        OR
        (purpose = 'cleanup' AND work_item_id IS NULL AND workspace_id IS NULL AND deployment_id IS NOT NULL
            AND content_revision IS NULL AND specification IS NULL AND source_commit IS NULL
            AND source_retention = 'not_required' AND retained_ref_id IS NULL)),
    CONSTRAINT runs_terminal_finished CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CONSTRAINT runs_cancel_both CHECK ((cancel_requested_at IS NULL) = (cancel_reason IS NULL)),
    CONSTRAINT runs_failure_both CHECK ((failure_intent_at IS NULL) = (failure_reason IS NULL)),
    CONSTRAINT runs_cancelled_requested CHECK (status <> 'cancelled' OR cancel_requested_at IS NOT NULL),
    CONSTRAINT runs_failed_intent CHECK (status <> 'failed' OR failure_intent_at IS NOT NULL),
    CONSTRAINT runs_started_when_running CHECK (status <> 'running' OR started_at IS NOT NULL)
);
CREATE INDEX runs_list_idx ON runs (tenant_id, project_id, created_at, id);
CREATE INDEX runs_work_item_idx ON runs (tenant_id, project_id, work_item_id, id) WHERE work_item_id IS NOT NULL;
-- Reconciliation and dispatch scan nonterminal runs by key.
CREATE INDEX runs_nonterminal_idx ON runs (tenant_id, created_at, project_id, id) WHERE status IN ('queued', 'running');

CREATE TABLE step_runs (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    step_id            text NOT NULL
                       CONSTRAINT step_runs_step_syntax CHECK (step_id ~ '^[a-z][a-z0-9-]{0,62}$'),
    ordinal            integer NOT NULL CONSTRAINT step_runs_ordinal_range CHECK (ordinal >= 0),
    spec               jsonb NOT NULL
                       CONSTRAINT step_runs_spec_bounds CHECK (jsonb_typeof(spec) = 'object' AND pg_column_size(spec) <= 524288),
    action_key         text NOT NULL
                       CONSTRAINT step_runs_action_key_syntax CHECK (action_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    status             text NOT NULL DEFAULT 'pending'
                       CONSTRAINT step_runs_status_known CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'cancelled')),
    reason             text NOT NULL
                       CONSTRAINT step_runs_reason_code CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    next_attempt_at    timestamptz,
    gate_ref           text CONSTRAINT step_runs_gate_ref_length CHECK (char_length(gate_ref) BETWEEN 1 AND 128),
    attempts_started   integer NOT NULL DEFAULT 0 CONSTRAINT step_runs_attempts_started_range CHECK (attempts_started >= 0),
    max_attempts       integer NOT NULL DEFAULT 1 CONSTRAINT step_runs_max_attempts_range CHECK (max_attempts BETWEEN 1 AND 100),
    backoff_seconds    bigint NOT NULL DEFAULT 0 CONSTRAINT step_runs_backoff_range CHECK (backoff_seconds BETWEEN 0 AND 86400),
    retry_on           text[] NOT NULL DEFAULT '{}'
                       CONSTRAINT step_runs_retry_on_known CHECK (retry_on <@ ARRAY['infrastructure', 'transient']),
    -- Finalized success outputs; empty unless the step succeeded.
    output_commit      text CONSTRAINT step_runs_output_commit_syntax CHECK (output_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    outputs            jsonb NOT NULL DEFAULT '{}'
                       CONSTRAINT step_runs_outputs_bounds CHECK (jsonb_typeof(outputs) = 'object' AND pg_column_size(outputs) <= 262144),
    version            bigint NOT NULL DEFAULT 1
                       CONSTRAINT step_runs_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    finished_at        timestamptz,
    PRIMARY KEY (tenant_id, project_id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CONSTRAINT step_runs_ordinal_unique UNIQUE (tenant_id, project_id, run_id, ordinal),
    CONSTRAINT step_runs_terminal_finished CHECK ((status IN ('succeeded', 'failed', 'skipped', 'cancelled')) = (finished_at IS NOT NULL)),
    CONSTRAINT step_runs_attempts_bounded CHECK (attempts_started <= max_attempts),
    CONSTRAINT step_runs_outputs_only_on_success CHECK (status = 'succeeded' OR (output_commit IS NULL AND outputs = '{}'::jsonb))
);
-- Pending steps ordered by their next-attempt time.
CREATE INDEX step_runs_pending_idx ON step_runs (tenant_id, project_id, run_id, next_attempt_at) WHERE status = 'pending';

CREATE TABLE step_attempts (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    step_id            text NOT NULL,
    ordinal            integer NOT NULL CONSTRAINT step_attempts_ordinal_range CHECK (ordinal >= 1),
    action_key         text NOT NULL
                       CONSTRAINT step_attempts_action_key_syntax CHECK (action_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    status             text NOT NULL DEFAULT 'running'
                       CONSTRAINT step_attempts_status_known CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    reason             text NOT NULL
                       CONSTRAINT step_attempts_reason_code CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    failure_class      text CONSTRAINT step_attempts_class_known CHECK (failure_class IN
                       ('infrastructure', 'transient', 'command', 'validation', 'policy', 'timeout', 'cancelled')),
    -- Expected Job identity and spec, derived before any Job exists.
    job_name           text NOT NULL
                       CONSTRAINT step_attempts_job_name_syntax CHECK (job_name ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$'),
    spec_fingerprint   text NOT NULL
                       CONSTRAINT step_attempts_fingerprint_syntax CHECK (spec_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    observed_job_uid   text CONSTRAINT step_attempts_uid_syntax CHECK (observed_job_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    workspace_id       kodama_id,
    access             text NOT NULL
                       CONSTRAINT step_attempts_access_known CHECK (access IN ('none', 'read', 'write')),
    input_commit       text CONSTRAINT step_attempts_input_commit_syntax CHECK (input_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    expected_tip       text CONSTRAINT step_attempts_expected_tip_syntax CHECK (expected_tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    timeout_seconds    bigint NOT NULL CONSTRAINT step_attempts_timeout_range CHECK (timeout_seconds BETWEEN 1 AND 604800),
    -- Distinct deadlines: startup (dispatch intent to observed start) and
    -- execution (observed start plus the step timeout). Gate waits have none.
    startup_deadline_at   timestamptz NOT NULL,
    execution_deadline_at timestamptz,
    context_digest     text NOT NULL
                       CONSTRAINT step_attempts_context_digest_syntax CHECK (context_digest ~ '^sha256:[0-9a-f]{64}$'),
    context            text NOT NULL
                       CONSTRAINT step_attempts_context_size CHECK (octet_length(context) BETWEEN 2 AND 2097152),
    started_at         timestamptz,
    finished_at        timestamptz,
    result_hash        text CONSTRAINT step_attempts_result_hash_syntax CHECK (result_hash ~ '^sha256:[0-9a-f]{64}$'),
    result             jsonb CONSTRAINT step_attempts_result_bounds CHECK (jsonb_typeof(result) = 'object' AND pg_column_size(result) <= 262144),
    authority_revision bigint NOT NULL DEFAULT 1 CONSTRAINT step_attempts_revision_range CHECK (authority_revision BETWEEN 1 AND 9007199254740991),
    version            bigint NOT NULL DEFAULT 1 CONSTRAINT step_attempts_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_id) REFERENCES step_runs (tenant_id, project_id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    -- The ownership key later modules (artifacts) reference.
    CONSTRAINT step_attempts_owner_key UNIQUE (tenant_id, project_id, id, run_id, step_id),
    CONSTRAINT step_attempts_ordinal_unique UNIQUE (tenant_id, project_id, run_id, step_id, ordinal),
    CONSTRAINT step_attempts_job_name_unique UNIQUE (tenant_id, job_name),
    CONSTRAINT step_attempts_terminal_finished CHECK ((status = 'running') = (finished_at IS NULL)),
    CONSTRAINT step_attempts_workspace_shape CHECK (
        (access = 'none' AND workspace_id IS NULL AND input_commit IS NULL AND expected_tip IS NULL)
        OR (access = 'read' AND workspace_id IS NOT NULL AND input_commit IS NOT NULL AND expected_tip IS NULL)
        OR (access = 'write' AND workspace_id IS NOT NULL AND input_commit IS NOT NULL AND expected_tip IS NOT NULL)),
    CONSTRAINT step_attempts_failure_class CHECK ((status = 'failed') = (failure_class IS NOT NULL) OR status = 'cancelled'),
    CONSTRAINT step_attempts_result_both CHECK ((result_hash IS NULL) = (result IS NULL)),
    CONSTRAINT step_attempts_success_has_result CHECK (status <> 'succeeded' OR result_hash IS NOT NULL)
);
-- At most one running attempt per step; the trigger below also refuses a new
-- attempt while an earlier one is unsettled.
CREATE UNIQUE INDEX step_attempts_one_running_idx ON step_attempts (tenant_id, project_id, run_id, step_id) WHERE status = 'running';
CREATE INDEX step_attempts_run_idx ON step_attempts (tenant_id, project_id, run_id, step_id, ordinal);
CREATE INDEX step_attempts_workspace_idx ON step_attempts (tenant_id, project_id, workspace_id) WHERE workspace_id IS NOT NULL;

CREATE TABLE attempt_controls (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    dispatch_confirmed_at   timestamptz,
    claim_token        text CONSTRAINT attempt_controls_claim_token_length CHECK (char_length(claim_token) <= 128),
    claim_until        timestamptz,
    revoked_at         timestamptz,
    termination_requested_at timestamptz,
    termination_confirmed_at timestamptz,
    launch             text NOT NULL DEFAULT 'unknown'
                       CONSTRAINT attempt_controls_launch_known CHECK (launch IN ('unknown', 'not_started', 'started')),
    effect             text NOT NULL DEFAULT 'possible'
                       CONSTRAINT attempt_controls_effect_known CHECK (effect IN ('none', 'possible', 'unknown', 'settled')),
    reconciled_at      timestamptz,
    replay_safe        boolean NOT NULL DEFAULT false,
    settled_at         timestamptz,
    step_resolved_at   timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CONSTRAINT attempt_controls_claim_both CHECK ((claim_token IS NULL) = (claim_until IS NULL)),
    CONSTRAINT attempt_controls_replay_needs_reconciliation CHECK (NOT replay_safe OR reconciled_at IS NOT NULL)
);
CREATE INDEX attempt_controls_unsettled_idx ON attempt_controls (tenant_id, run_id) WHERE settled_at IS NULL;

-- No attempt is created while an earlier attempt of the same step is unsettled:
-- an uncertain or unterminated attempt blocks replay of its action.
CREATE FUNCTION t020_attempt_insert_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM step_attempts a
        LEFT JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
        WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id AND a.run_id = NEW.run_id AND a.step_id = NEW.step_id
          AND a.id <> NEW.id AND (c.settled_at IS NULL)) THEN
        RAISE EXCEPTION 'an earlier attempt of this step is unsettled' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER step_attempts_insert_guard BEFORE INSERT ON step_attempts
    FOR EACH ROW EXECUTE FUNCTION t020_attempt_insert_guard();

CREATE TABLE attempt_evidence (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    id           kodama_id NOT NULL,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    run_id       kodama_id NOT NULL,
    attempt_id   kodama_id NOT NULL,
    kind         text NOT NULL
                 CONSTRAINT attempt_evidence_kind_known CHECK (kind IN ('late_observation', 'late_result', 'observation_mismatch', 'reconciliation', 'termination')),
    detail       jsonb NOT NULL DEFAULT '{}'
                 CONSTRAINT attempt_evidence_detail_bounds CHECK (jsonb_typeof(detail) = 'object' AND pg_column_size(detail) <= 4096),
    occurred_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id)
);
CREATE UNIQUE INDEX attempt_evidence_seq_idx ON attempt_evidence (seq);
CREATE INDEX attempt_evidence_attempt_idx ON attempt_evidence (tenant_id, project_id, attempt_id, seq);

CREATE TABLE run_history (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    id           kodama_id NOT NULL,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    run_id       kodama_id NOT NULL,
    subject_kind text NOT NULL CONSTRAINT run_history_subject_known CHECK (subject_kind IN ('run', 'step', 'attempt')),
    subject_id   text NOT NULL CONSTRAINT run_history_subject_length CHECK (char_length(subject_id) BETWEEN 1 AND 128),
    step_id      text,
    from_status  text,
    to_status    text NOT NULL,
    reason       text NOT NULL CONSTRAINT run_history_reason_code CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    run_version  bigint NOT NULL CONSTRAINT run_history_version_range CHECK (run_version BETWEEN 1 AND 9007199254740991),
    actor_type   text NOT NULL CONSTRAINT run_history_actor_type_known CHECK (actor_type IN ('user', 'service', 'workload', 'operator')),
    actor_id     text NOT NULL CONSTRAINT run_history_actor_id_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    occurred_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id)
);
CREATE UNIQUE INDEX run_history_seq_idx ON run_history (seq);
CREATE INDEX run_history_run_idx ON run_history (tenant_id, project_id, run_id, seq);

-- Capacity. The scope rows are lock targets (tenant, then project): dispatch
-- locks them in that order, counts the held reservations and inserts one, so
-- concurrent claims cannot exceed a limit. A reservation is released exactly
-- once, by verified settlement.
CREATE TABLE execution_quota_scopes (
    tenant_id    kodama_id NOT NULL REFERENCES tenants (id),
    scope_key    text NOT NULL CONSTRAINT execution_quota_scope_key_syntax CHECK (scope_key = 'tenant' OR scope_key ~ '^project:[A-Za-z0-9][A-Za-z0-9_-]{0,127}$'),
    touched_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, scope_key)
);

CREATE TABLE execution_reservations (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    attempt_id   kodama_id NOT NULL,
    run_id       kodama_id NOT NULL,
    held_at      timestamptz NOT NULL,
    released_at  timestamptz,
    PRIMARY KEY (tenant_id, project_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    CONSTRAINT execution_reservations_released_after CHECK (released_at IS NULL OR released_at >= held_at)
);
CREATE INDEX execution_reservations_held_idx ON execution_reservations (tenant_id, project_id) WHERE released_at IS NULL;

-- The durable, idempotent request to cancel the runs of a cancelled work item,
-- recorded in the event consumer's transaction.
CREATE TABLE work_cancellation_requests (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    work_item_id kodama_id NOT NULL,
    version      bigint NOT NULL CONSTRAINT work_cancellation_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    requested_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, work_item_id),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id)
);

-- ---- guards ----

CREATE FUNCTION t020_runs_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'runs are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.status IN ('succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'a terminal run is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id OR NEW.purpose <> OLD.purpose
       OR NEW.work_item_id IS DISTINCT FROM OLD.work_item_id OR NEW.workspace_id IS DISTINCT FROM OLD.workspace_id
       OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.definition_id <> OLD.definition_id
       OR NEW.definition_digest <> OLD.definition_digest OR NEW.content_revision IS DISTINCT FROM OLD.content_revision
       OR NEW.specification IS DISTINCT FROM OLD.specification OR NEW.source_commit IS DISTINCT FROM OLD.source_commit
       OR NEW.inputs IS DISTINCT FROM OLD.inputs OR NEW.snapshot_digest <> OLD.snapshot_digest
       OR NEW.requested_by_type <> OLD.requested_by_type OR NEW.requested_by_id <> OLD.requested_by_id
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the pinned inputs and identity of a run are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'queued' AND NEW.status IN ('running', 'failed', 'cancelled', 'succeeded'))
        OR (OLD.status = 'running' AND NEW.status IN ('succeeded', 'failed', 'cancelled'))) THEN
        RAISE EXCEPTION 'invalid run transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    IF OLD.source_retention = 'confirmed' AND (NEW.source_retention <> 'confirmed' OR NEW.retained_ref_id IS DISTINCT FROM OLD.retained_ref_id) THEN
        RAISE EXCEPTION 'a confirmed retained reference is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.source_retention <> NEW.source_retention AND NOT (OLD.source_retention = 'pending' AND NEW.source_retention = 'confirmed') THEN
        RAISE EXCEPTION 'invalid source retention transition' USING ERRCODE = '23514';
    END IF;
    IF NEW.retained_ref_id IS DISTINCT FROM OLD.retained_ref_id THEN
        RAISE EXCEPTION 'the retained reference identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.cancel_requested_at IS NOT NULL AND (NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at OR NEW.cancel_reason IS DISTINCT FROM OLD.cancel_reason) THEN
        RAISE EXCEPTION 'a cancellation request is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.failure_intent_at IS NOT NULL AND (NEW.failure_intent_at IS DISTINCT FROM OLD.failure_intent_at OR NEW.failure_reason IS DISTINCT FROM OLD.failure_reason) THEN
        RAISE EXCEPTION 'failure intent is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'a resource version never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER runs_guard BEFORE UPDATE OR DELETE ON runs FOR EACH ROW EXECUTE FUNCTION t020_runs_guard();

CREATE FUNCTION t020_step_runs_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'step runs are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled') THEN
        RAISE EXCEPTION 'a terminal step run is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.run_id <> OLD.run_id OR NEW.step_id <> OLD.step_id
       OR NEW.ordinal <> OLD.ordinal OR NEW.spec IS DISTINCT FROM OLD.spec OR NEW.action_key <> OLD.action_key
       OR NEW.max_attempts <> OLD.max_attempts OR NEW.backoff_seconds <> OLD.backoff_seconds OR NEW.retry_on IS DISTINCT FROM OLD.retry_on
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the definition and identity of a step run are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.attempts_started < OLD.attempts_started OR NEW.attempts_started > OLD.attempts_started + 1 THEN
        RAISE EXCEPTION 'attempts are counted one at a time' USING ERRCODE = '23514';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'a resource version never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER step_runs_guard BEFORE UPDATE OR DELETE ON step_runs FOR EACH ROW EXECUTE FUNCTION t020_step_runs_guard();

CREATE FUNCTION t020_step_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'attempts are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'running' THEN
        RAISE EXCEPTION 'a terminal attempt is immutable; late evidence is recorded separately' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id OR NEW.run_id <> OLD.run_id
       OR NEW.step_id <> OLD.step_id OR NEW.ordinal <> OLD.ordinal OR NEW.action_key <> OLD.action_key
       OR NEW.job_name <> OLD.job_name OR NEW.spec_fingerprint <> OLD.spec_fingerprint
       OR NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR NEW.access <> OLD.access
       OR NEW.input_commit IS DISTINCT FROM OLD.input_commit OR NEW.expected_tip IS DISTINCT FROM OLD.expected_tip
       OR NEW.timeout_seconds <> OLD.timeout_seconds OR NEW.startup_deadline_at <> OLD.startup_deadline_at
       OR NEW.context_digest <> OLD.context_digest OR NEW.context <> OLD.context OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity and pinned context of an attempt are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.observed_job_uid IS NOT NULL AND NEW.observed_job_uid IS DISTINCT FROM OLD.observed_job_uid THEN
        RAISE EXCEPTION 'the observed Job UID is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at THEN
        RAISE EXCEPTION 'the actual start is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'a resource version never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER step_attempts_guard BEFORE UPDATE OR DELETE ON step_attempts FOR EACH ROW EXECUTE FUNCTION t020_step_attempts_guard();

CREATE FUNCTION t020_attempt_controls_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'attempt bookkeeping is retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.attempt_id <> OLD.attempt_id OR NEW.run_id <> OLD.run_id THEN
        RAISE EXCEPTION 'attempt bookkeeping identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
       OR (OLD.settled_at IS NOT NULL AND NEW.settled_at IS DISTINCT FROM OLD.settled_at)
       OR (OLD.termination_requested_at IS NOT NULL AND NEW.termination_requested_at IS DISTINCT FROM OLD.termination_requested_at)
       OR (OLD.termination_confirmed_at IS NOT NULL AND NEW.termination_confirmed_at IS DISTINCT FROM OLD.termination_confirmed_at)
       OR (OLD.reconciled_at IS NOT NULL AND (NEW.reconciled_at IS DISTINCT FROM OLD.reconciled_at OR NEW.replay_safe <> OLD.replay_safe)) THEN
        RAISE EXCEPTION 'revocation, termination, reconciliation and settlement are recorded once' USING ERRCODE = '23514';
    END IF;
    IF OLD.launch <> 'unknown' AND NEW.launch <> OLD.launch THEN
        RAISE EXCEPTION 'launch evidence only becomes more informative' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER attempt_controls_guard BEFORE UPDATE OR DELETE ON attempt_controls FOR EACH ROW EXECUTE FUNCTION t020_attempt_controls_guard();

CREATE FUNCTION t020_reservations_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reservations are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'a reservation is released once' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.attempt_id <> OLD.attempt_id
       OR NEW.run_id <> OLD.run_id OR NEW.held_at <> OLD.held_at THEN
        RAISE EXCEPTION 'reservation identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER execution_reservations_guard BEFORE UPDATE OR DELETE ON execution_reservations
    FOR EACH ROW EXECUTE FUNCTION t020_reservations_guard();

-- ---- row level security ----

ALTER TABLE runs                     ENABLE ROW LEVEL SECURITY;
ALTER TABLE runs                     FORCE ROW LEVEL SECURITY;
ALTER TABLE step_runs                ENABLE ROW LEVEL SECURITY;
ALTER TABLE step_runs                FORCE ROW LEVEL SECURITY;
ALTER TABLE step_attempts            ENABLE ROW LEVEL SECURITY;
ALTER TABLE step_attempts            FORCE ROW LEVEL SECURITY;
ALTER TABLE attempt_controls         ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_controls         FORCE ROW LEVEL SECURITY;
ALTER TABLE attempt_evidence         ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_evidence         FORCE ROW LEVEL SECURITY;
ALTER TABLE run_history              ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_history              FORCE ROW LEVEL SECURITY;
ALTER TABLE execution_quota_scopes   ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_quota_scopes   FORCE ROW LEVEL SECURITY;
ALTER TABLE execution_reservations   ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_reservations   FORCE ROW LEVEL SECURITY;
ALTER TABLE work_cancellation_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_cancellation_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON runs                       TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON step_runs                  TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON step_attempts              TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON attempt_controls           TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON execution_quota_scopes     TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON execution_reservations     TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON work_cancellation_requests TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON runs
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON step_runs
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON step_attempts
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON attempt_controls
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON execution_quota_scopes
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON execution_reservations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON work_cancellation_requests
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Evidence and history are append-only: no UPDATE or DELETE policy exists.
CREATE POLICY owner_select ON attempt_evidence FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON attempt_evidence FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON attempt_evidence FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON attempt_evidence FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY owner_select ON run_history FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON run_history FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON run_history FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON run_history FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- grants: no DELETE; UPDATE is column-scoped to the mutable state ----

GRANT SELECT, INSERT ON runs TO {{runtime_role}};
GRANT UPDATE (status, status_reason, cancel_requested_at, cancel_reason, failure_intent_at, failure_reason,
              source_retention, version, updated_at, started_at, finished_at) ON runs TO {{runtime_role}};

GRANT SELECT, INSERT ON step_runs TO {{runtime_role}};
GRANT UPDATE (status, reason, next_attempt_at, gate_ref, attempts_started, output_commit, outputs,
              version, updated_at, finished_at) ON step_runs TO {{runtime_role}};

GRANT SELECT, INSERT ON step_attempts TO {{runtime_role}};
GRANT UPDATE (status, reason, failure_class, observed_job_uid, execution_deadline_at, started_at, finished_at,
              result_hash, result, authority_revision, version, updated_at) ON step_attempts TO {{runtime_role}};

GRANT SELECT, INSERT ON attempt_controls TO {{runtime_role}};
GRANT UPDATE (dispatch_confirmed_at, claim_token, claim_until, revoked_at, termination_requested_at,
              termination_confirmed_at, launch, effect, reconciled_at, replay_safe, settled_at, step_resolved_at, updated_at)
    ON attempt_controls TO {{runtime_role}};

GRANT SELECT, INSERT ON attempt_evidence TO {{runtime_role}};
GRANT SELECT, INSERT ON run_history TO {{runtime_role}};

GRANT SELECT, INSERT ON execution_quota_scopes TO {{runtime_role}};
GRANT UPDATE (touched_at) ON execution_quota_scopes TO {{runtime_role}};

GRANT SELECT, INSERT ON execution_reservations TO {{runtime_role}};
GRANT UPDATE (released_at) ON execution_reservations TO {{runtime_role}};

GRANT SELECT, INSERT ON work_cancellation_requests TO {{runtime_role}};
GRANT UPDATE (version) ON work_cancellation_requests TO {{runtime_role}};
