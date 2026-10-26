-- T-112 run_shared resource lifecycle and persistence (additive).
--
-- Adds, without rewriting any existing table's data or any existing guard:
--   * runs.run_shared  the immutable run_shared pin set (run model, context
--                      version, pinned policy id/digest/body, binding, limits,
--                      endpoints, effective access, execution order, resolved
--                      services). NULL for every attempt_private run, which
--                      keeps its stored shape, hashes and readers unchanged;
--   * runs.deadline_at the total run deadline, set once when resource holding
--                      starts (it covers gate waits);
--   * run_resources    one durable record per run resource (network, workspace,
--                      preparation, service): intent FIRST (canonical spec and
--                      fingerprint), then the bound UID, readiness, the trusted
--                      preparation receipt, stop and removal evidence and safe
--                      reasons. No scratch content, no secret plaintext;
--   * run_holders      the single workspace-volume holder of a run, separate from
--                      the logical workspace's writer reservation;
--   * attempt_begins   the append-only command-begin journal;
--   * execution_run_reservations the run-level capacity reservation (services,
--                      preparation, the largest attempt and the scratch volume).
--
-- Registered as its own final contribution after the cleanup bookkeeping: the
-- migration ledger refuses a pending migration ordered before an applied one.
-- Nothing here is wired into a composition (T-116 lists it).

ALTER TABLE runs
    ADD COLUMN run_shared jsonb,
    ADD COLUMN deadline_at timestamptz,
    ADD CONSTRAINT runs_run_shared_bounds
        CHECK (run_shared IS NULL OR (jsonb_typeof(run_shared) = 'object' AND pg_column_size(run_shared) <= 262144)),
    ADD CONSTRAINT runs_deadline_needs_shared CHECK (deadline_at IS NULL OR run_shared IS NOT NULL);

CREATE FUNCTION t112_runs_shared_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.run_shared IS DISTINCT FROM OLD.run_shared THEN
        RAISE EXCEPTION 'the run_shared pins of a run are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deadline_at IS NOT NULL AND NEW.deadline_at IS DISTINCT FROM OLD.deadline_at THEN
        RAISE EXCEPTION 'the run deadline is pinned once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER runs_shared_guard BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION t112_runs_shared_guard();

GRANT UPDATE (deadline_at) ON runs TO {{runtime_role}};

-- Resource transitions are history/audit rows like run, step and attempt ones.
ALTER TABLE run_history DROP CONSTRAINT run_history_subject_known;
ALTER TABLE run_history ADD CONSTRAINT run_history_subject_known CHECK (subject_kind IN ('run', 'step', 'attempt', 'resource'));

CREATE TABLE run_resources (
    tenant_id           kodama_id NOT NULL,
    project_id          kodama_id NOT NULL,
    run_id              kodama_id NOT NULL,
    key                 text NOT NULL,
    kind                text NOT NULL
                        CONSTRAINT run_resources_kind_known CHECK (kind IN ('network', 'workspace', 'preparation', 'service')),
    name                text NOT NULL
                        CONSTRAINT run_resources_name_syntax CHECK (name ~ '^kr-[0-9a-f]{24}$'),
    ordinal             integer NOT NULL CONSTRAINT run_resources_ordinal_range CHECK (ordinal BETWEEN 0 AND 63),
    backend             text NOT NULL CONSTRAINT run_resources_backend_length CHECK (char_length(backend) BETWEEN 1 AND 128),
    -- The canonical provider-neutral spec and its fingerprint: intent, written
    -- with the row before any runtime call. UID is not part of it.
    spec                jsonb NOT NULL
                        CONSTRAINT run_resources_spec_bounds CHECK (jsonb_typeof(spec) = 'object' AND pg_column_size(spec) <= 262144),
    fingerprint         text NOT NULL CONSTRAINT run_resources_fingerprint_syntax CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    ensure_requested_at timestamptz,
    -- Server-side observation request generations: a request is committed before
    -- its runtime call, an answer is applied only if newer than every applied
    -- one, so a delayed older answer cannot overwrite a newer observation.
    observe_gen         bigint NOT NULL DEFAULT 0 CONSTRAINT run_resources_observe_gen_range CHECK (observe_gen BETWEEN 0 AND 9007199254740991),
    applied_gen         bigint NOT NULL DEFAULT 0 CONSTRAINT run_resources_applied_gen_range CHECK (applied_gen BETWEEN 0 AND 9007199254740991),
    observe_requested_at timestamptz,
    uid                 text CONSTRAINT run_resources_uid_syntax CHECK (uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    bound_at            timestamptz,
    phase               text NOT NULL
                        CONSTRAINT run_resources_phase_known CHECK (phase IN ('pending', 'ready', 'failed', 'stopped', 'absent', 'unknown')),
    reason              text CONSTRAINT run_resources_reason_code CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    observed_at         timestamptz,
    ready_at            timestamptz,
    probe_kind          text CONSTRAINT run_resources_probe_known CHECK (probe_kind IN ('tcp', 'http', 'exec')),
    not_ready_since     timestamptz,
    unknown_since       timestamptz,
    released            boolean NOT NULL DEFAULT false,
    released_at         timestamptz,
    -- The trusted checkout receipt (preparation only) and its canonical digest,
    -- bound to the run, the resource key and the workspace's bound UID.
    receipt             jsonb CONSTRAINT run_resources_receipt_bounds CHECK (jsonb_typeof(receipt) = 'object' AND pg_column_size(receipt) <= 4096),
    receipt_digest      text CONSTRAINT run_resources_receipt_digest_syntax CHECK (receipt_digest ~ '^sha256:[0-9a-f]{64}$'),
    receipt_at          timestamptz,
    stop_requested_at   timestamptz,
    stop_reason         text CONSTRAINT run_resources_stop_reason_code CHECK (stop_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    stopped_at          timestamptz,
    stop_launch         text CONSTRAINT run_resources_stop_launch_known CHECK (stop_launch IN ('unknown', 'not_started', 'started')),
    removed_at          timestamptz,
    failures            integer NOT NULL DEFAULT 0 CONSTRAINT run_resources_failures_range CHECK (failures BETWEEN 0 AND 1000000),
    next_action_at      timestamptz,
    version             bigint NOT NULL DEFAULT 1 CONSTRAINT run_resources_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id, key),
    -- A resource can only belong to a run of its own project.
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    -- Deterministic names are unique per tenant: a name collision cannot be
    -- recorded as a second resource.
    CONSTRAINT run_resources_name_unique UNIQUE (tenant_id, name),
    CONSTRAINT run_resources_key_matches_kind CHECK (
           (kind = 'network' AND key = 'network') OR (kind = 'workspace' AND key = 'workspace')
        OR (kind = 'preparation' AND key = 'preparation') OR (kind = 'service' AND key ~ '^service\.[a-z][a-z0-9-]{0,29}$')),
    CONSTRAINT run_resources_bound_both CHECK ((uid IS NULL) = (bound_at IS NULL)),
    CONSTRAINT run_resources_receipt_only_preparation CHECK (receipt IS NULL OR kind = 'preparation'),
    CONSTRAINT run_resources_receipt_all CHECK ((receipt IS NULL) = (receipt_digest IS NULL) AND (receipt IS NULL) = (receipt_at IS NULL)),
    CONSTRAINT run_resources_receipt_needs_uid CHECK (receipt IS NULL OR uid IS NOT NULL),
    CONSTRAINT run_resources_stop_all CHECK ((stopped_at IS NULL) OR (stop_requested_at IS NOT NULL AND stop_launch IS NOT NULL)),
    CONSTRAINT run_resources_removed_after_stop CHECK (removed_at IS NULL OR stopped_at IS NOT NULL),
    CONSTRAINT run_resources_applied_le_observe CHECK (applied_gen <= observe_gen),
    CONSTRAINT run_resources_ready_not_stopped CHECK (NOT (phase = 'ready' AND stopped_at IS NOT NULL))
);
-- One provider instance is bound to at most one resource of a backend.
CREATE UNIQUE INDEX run_resources_uid_unique_idx ON run_resources (tenant_id, backend, uid) WHERE uid IS NOT NULL;
CREATE INDEX run_resources_run_idx ON run_resources (tenant_id, project_id, run_id, ordinal);
-- The cleanup scan: resources not yet verified removed.
CREATE INDEX run_resources_unfinished_idx ON run_resources (tenant_id, project_id, run_id) WHERE removed_at IS NULL;

CREATE FUNCTION t112_run_resources_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run resources are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.removed_at IS NOT NULL THEN
        RAISE EXCEPTION 'a removed run resource is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.run_id <> OLD.run_id OR NEW.key <> OLD.key
       OR NEW.kind <> OLD.kind OR NEW.name <> OLD.name OR NEW.ordinal <> OLD.ordinal OR NEW.backend <> OLD.backend
       OR NEW.spec IS DISTINCT FROM OLD.spec OR NEW.fingerprint <> OLD.fingerprint OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity, spec and fingerprint of a run resource are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.uid IS NOT NULL AND (NEW.uid IS DISTINCT FROM OLD.uid OR NEW.bound_at IS DISTINCT FROM OLD.bound_at) THEN
        RAISE EXCEPTION 'the bound resource UID is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.stop_requested_at IS NOT NULL AND (NEW.stop_requested_at IS DISTINCT FROM OLD.stop_requested_at OR NEW.stop_reason IS DISTINCT FROM OLD.stop_reason) THEN
        RAISE EXCEPTION 'the stop intent is recorded once' USING ERRCODE = '23514';
    END IF;
    IF OLD.stopped_at IS NOT NULL AND (NEW.stopped_at IS DISTINCT FROM OLD.stopped_at OR NEW.stop_launch IS DISTINCT FROM OLD.stop_launch) THEN
        RAISE EXCEPTION 'a verified stop is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.receipt IS NOT NULL AND (NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_digest IS DISTINCT FROM OLD.receipt_digest
                                    OR NEW.receipt_at IS DISTINCT FROM OLD.receipt_at) THEN
        RAISE EXCEPTION 'the preparation receipt is recorded once' USING ERRCODE = '23514';
    END IF;
    IF NEW.observe_gen < OLD.observe_gen OR NEW.applied_gen < OLD.applied_gen THEN
        RAISE EXCEPTION 'observation generations only grow' USING ERRCODE = '23514';
    END IF;
    IF OLD.ready_at IS NOT NULL AND NEW.ready_at IS DISTINCT FROM OLD.ready_at THEN
        RAISE EXCEPTION 'the first ready time is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.ensure_requested_at IS NOT NULL AND NEW.ensure_requested_at IS NULL THEN
        RAISE EXCEPTION 'an ensure intent is never forgotten' USING ERRCODE = '23514';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'a resource version never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_resources_guard BEFORE UPDATE OR DELETE ON run_resources FOR EACH ROW EXECUTE FUNCTION t112_run_resources_guard();

CREATE TABLE run_holders (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    run_id       kodama_id NOT NULL,
    attempt_id   kodama_id NOT NULL,
    step_id      text NOT NULL,
    held_at      timestamptz NOT NULL,
    released_at  timestamptz,
    PRIMARY KEY (tenant_id, project_id, run_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    CONSTRAINT run_holders_released_after CHECK (released_at IS NULL OR released_at >= held_at)
);
-- At most one holder of a run's workspace volume is unreleased. It is the
-- database's own guard behind the claim transaction, independent of the logical
-- workspace's single writer slot.
CREATE UNIQUE INDEX run_holders_one_unreleased_idx ON run_holders (tenant_id, project_id, run_id) WHERE released_at IS NULL;

CREATE FUNCTION t112_run_holders_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run holders are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id
       OR NEW.step_id <> OLD.step_id OR NEW.held_at <> OLD.held_at THEN
        RAISE EXCEPTION 'holder identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'a holder release is recorded once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_holders_guard BEFORE UPDATE OR DELETE ON run_holders FOR EACH ROW EXECUTE FUNCTION t112_run_holders_guard();

CREATE TABLE attempt_begins (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    attempt_id      kodama_id NOT NULL,
    run_id          kodama_id NOT NULL,
    step_id         text NOT NULL,
    access          text NOT NULL CONSTRAINT attempt_begins_access_known CHECK (access IN ('none', 'read', 'write')),
    began_at        timestamptz NOT NULL,
    workspace_state text NOT NULL CONSTRAINT attempt_begins_state_known CHECK (workspace_state IN ('none', 'clean', 'modified')),
    ignored_present boolean NOT NULL,
    body_digest     text NOT NULL CONSTRAINT attempt_begins_digest_syntax CHECK (body_digest ~ '^sha256:[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, project_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id)
);
CREATE INDEX attempt_begins_run_idx ON attempt_begins (tenant_id, project_id, run_id, began_at);

CREATE FUNCTION t112_attempt_begins_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a recorded command-begin is immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER attempt_begins_guard BEFORE UPDATE OR DELETE ON attempt_begins FOR EACH ROW EXECUTE FUNCTION t112_attempt_begins_guard();

CREATE TABLE execution_run_reservations (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    run_id        kodama_id NOT NULL,
    services      bigint NOT NULL CONSTRAINT execution_run_reservations_services CHECK (services BETWEEN 0 AND 64),
    cpu_millis    bigint NOT NULL CONSTRAINT execution_run_reservations_cpu CHECK (cpu_millis >= 0),
    memory_mib    bigint NOT NULL CONSTRAINT execution_run_reservations_memory CHECK (memory_mib >= 0),
    ephemeral_mib bigint NOT NULL CONSTRAINT execution_run_reservations_ephemeral CHECK (ephemeral_mib >= 0),
    storage_mib   bigint NOT NULL CONSTRAINT execution_run_reservations_storage CHECK (storage_mib >= 0),
    held_at       timestamptz NOT NULL,
    released_at   timestamptz,
    PRIMARY KEY (tenant_id, project_id, run_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CONSTRAINT execution_run_reservations_released_after CHECK (released_at IS NULL OR released_at >= held_at)
);
CREATE INDEX execution_run_reservations_held_idx ON execution_run_reservations (tenant_id, project_id) WHERE released_at IS NULL;

CREATE FUNCTION t112_run_reservations_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run reservations are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'a reservation is released once' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.run_id <> OLD.run_id OR NEW.held_at <> OLD.held_at
       OR NEW.services <> OLD.services OR NEW.cpu_millis <> OLD.cpu_millis OR NEW.memory_mib <> OLD.memory_mib
       OR NEW.ephemeral_mib <> OLD.ephemeral_mib OR NEW.storage_mib <> OLD.storage_mib THEN
        RAISE EXCEPTION 'reservation identity and quantities are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER execution_run_reservations_guard BEFORE UPDATE OR DELETE ON execution_run_reservations
    FOR EACH ROW EXECUTE FUNCTION t112_run_reservations_guard();

-- ---- row level security: forced, tenant-scoped, owner policy as the rest of execution ----

ALTER TABLE run_resources              ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_resources              FORCE ROW LEVEL SECURITY;
ALTER TABLE run_holders                ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_holders                FORCE ROW LEVEL SECURITY;
ALTER TABLE attempt_begins             ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_begins             FORCE ROW LEVEL SECURITY;
ALTER TABLE execution_run_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_run_reservations FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON run_resources              TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON run_holders                TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON attempt_begins             TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON execution_run_reservations TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON run_resources
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON run_holders
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON attempt_begins
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON execution_run_reservations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- grants: no DELETE; UPDATE is column-scoped to the mutable state ----

GRANT SELECT, INSERT ON run_resources TO {{runtime_role}};
GRANT UPDATE (ensure_requested_at, observe_gen, applied_gen, observe_requested_at, uid, bound_at, phase, reason, observed_at, ready_at, probe_kind, not_ready_since, unknown_since,
              released, released_at, receipt, receipt_digest, receipt_at, stop_requested_at, stop_reason, stopped_at, stop_launch,
              removed_at, failures, next_action_at, version, updated_at) ON run_resources TO {{runtime_role}};

GRANT SELECT, INSERT ON run_holders TO {{runtime_role}};
GRANT UPDATE (released_at) ON run_holders TO {{runtime_role}};

GRANT SELECT, INSERT ON attempt_begins TO {{runtime_role}};

GRANT SELECT, INSERT ON execution_run_reservations TO {{runtime_role}};
GRANT UPDATE (released_at) ON execution_run_reservations TO {{runtime_role}};
