-- T-023 log capture records: which verified artifacts hold an attempt's log,
-- from which source, with what completeness evidence, plus the fenced
-- collection state of the controller fallback.
--
-- Only ownership, capture generation, lease/fencing information, artifact
-- references and safe completeness metadata live here: never log bytes, secret
-- values, transfer URLs or object keys. Bytes stay in the artifact module's
-- object store; artifact lifecycle, pins and deletion stay with that module.
--
-- Ordered after the artifact (T015) and execution (T020) migrations: it
-- references step_attempts and artifacts.

CREATE TABLE log_records (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    id              kodama_id NOT NULL,
    run_id          kodama_id NOT NULL,
    attempt_id      kodama_id NOT NULL,
    step_id         text NOT NULL
                    CONSTRAINT log_records_step_length CHECK (char_length(step_id) BETWEEN 1 AND 128),
    source          text NOT NULL
                    CONSTRAINT log_records_source_known CHECK (source IN ('wrapper', 'controller')),
    generation      integer NOT NULL
                    CONSTRAINT log_records_generation_range CHECK (generation BETWEEN 1 AND 3),
    state           text NOT NULL
                    CONSTRAINT log_records_state_known CHECK (state IN ('pending', 'collecting', 'retained', 'unavailable')),
    -- Fenced collection lease. claim_fence only increases.
    claim_token     text,
    claim_fence     bigint NOT NULL DEFAULT 0
                    CONSTRAINT log_records_fence_range CHECK (claim_fence >= 0),
    claim_until     timestamptz,
    claims          integer NOT NULL DEFAULT 0
                    CONSTRAINT log_records_claims_range CHECK (claims BETWEEN 0 AND 5),
    failure_class   text
                    CONSTRAINT log_records_failure_known CHECK (failure_class IN
                    ('workload_missing', 'backend_unavailable', 'store_unavailable', 'frozen_bytes_lost', 'limit_exceeded')),
    -- The frozen snapshot of the bytes a controller capture uploads, recorded
    -- before any upload so unknown outcomes reconcile with one declaration.
    snap_log_name   text
                    CONSTRAINT log_records_snap_log_name CHECK (snap_log_name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    snap_meta_name  text
                    CONSTRAINT log_records_snap_meta_name CHECK (snap_meta_name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    snap_digest     text
                    CONSTRAINT log_records_snap_digest CHECK (snap_digest ~ '^sha256:[0-9a-f]{64}$'),
    snap_size       bigint
                    CONSTRAINT log_records_snap_size CHECK (snap_size BETWEEN 0 AND 1099511627776),
    snap_completeness text
                    CONSTRAINT log_records_snap_completeness CHECK (snap_completeness IN ('complete', 'partial', 'unknown')),
    snap_reasons    text[],
    -- The verified linkage, set once.
    log_artifact_id kodama_id,
    log_digest      text
                    CONSTRAINT log_records_log_digest CHECK (log_digest ~ '^sha256:[0-9a-f]{64}$'),
    log_size        bigint
                    CONSTRAINT log_records_log_size CHECK (log_size BETWEEN 0 AND 1099511627776),
    meta_artifact_id kodama_id,
    meta_digest     text
                    CONSTRAINT log_records_meta_digest CHECK (meta_digest ~ '^sha256:[0-9a-f]{64}$'),
    meta_size       bigint
                    CONSTRAINT log_records_meta_size CHECK (meta_size BETWEEN 0 AND 4096),
    completeness    text
                    CONSTRAINT log_records_completeness_known CHECK (completeness IN ('complete', 'partial', 'unknown')),
    reasons         text[]
                    CONSTRAINT log_records_reasons_bound CHECK (cardinality(reasons) <= 8),
    -- Retention removed the content: availability, not completeness.
    removed_at      timestamptz,
    version         bigint NOT NULL
                    CONSTRAINT log_records_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    -- The attempt must be this project's, and the run and step must be its own.
    FOREIGN KEY (tenant_id, project_id, attempt_id, run_id, step_id)
        REFERENCES step_attempts (tenant_id, project_id, id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, log_artifact_id) REFERENCES artifacts (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, meta_artifact_id) REFERENCES artifacts (tenant_id, project_id, id),
    CONSTRAINT log_records_capture_unique UNIQUE (tenant_id, project_id, attempt_id, source, generation),
    CONSTRAINT log_records_state_shape CHECK (
        (state = 'pending' AND claim_token IS NULL AND claim_until IS NULL AND failure_class IS NULL
            AND snap_digest IS NULL AND log_artifact_id IS NULL)
     OR (state = 'collecting' AND source = 'controller' AND claim_token IS NOT NULL AND claim_until IS NOT NULL
            AND failure_class IS NULL AND log_artifact_id IS NULL AND claim_fence >= 1 AND claims >= 1)
     OR (state = 'retained' AND claim_token IS NULL AND claim_until IS NULL AND failure_class IS NULL
            AND log_artifact_id IS NOT NULL AND log_digest IS NOT NULL AND log_size IS NOT NULL
            AND completeness IS NOT NULL AND reasons IS NOT NULL)
     OR (state = 'unavailable' AND source = 'controller' AND claim_token IS NULL AND claim_until IS NULL
            AND failure_class IS NOT NULL AND log_artifact_id IS NULL)
    ),
    CONSTRAINT log_records_snapshot_shape CHECK (
        (snap_digest IS NULL AND snap_log_name IS NULL AND snap_meta_name IS NULL AND snap_size IS NULL
            AND snap_completeness IS NULL AND snap_reasons IS NULL)
     OR (source = 'controller' AND snap_digest IS NOT NULL AND snap_log_name IS NOT NULL AND snap_meta_name IS NOT NULL
            AND snap_size IS NOT NULL AND snap_completeness IS NOT NULL AND snap_reasons IS NOT NULL
            AND snap_completeness <> 'complete')
    ),
    CONSTRAINT log_records_link_shape CHECK (
        (log_artifact_id IS NULL AND log_digest IS NULL AND log_size IS NULL AND meta_artifact_id IS NULL
            AND meta_digest IS NULL AND meta_size IS NULL AND completeness IS NULL AND reasons IS NULL AND removed_at IS NULL)
     OR (log_artifact_id IS NOT NULL AND log_digest IS NOT NULL AND log_size IS NOT NULL
            AND completeness IS NOT NULL AND reasons IS NOT NULL
            AND ((meta_artifact_id IS NULL AND meta_digest IS NULL AND meta_size IS NULL)
              OR (meta_artifact_id IS NOT NULL AND meta_digest IS NOT NULL AND meta_size IS NOT NULL
                  AND meta_artifact_id <> log_artifact_id))
            -- A complete statement has no reasons; any other has at least one.
            AND ((completeness = 'complete') = (cardinality(reasons) = 0)))
    ),
    CONSTRAINT log_records_wrapper_shape CHECK (
        source <> 'wrapper' OR (claims = 0 AND claim_fence = 0 AND snap_digest IS NULL AND state IN ('pending', 'retained'))
    ),
    CONSTRAINT log_records_controller_shape CHECK (
        source <> 'controller' OR state <> 'retained'
        OR (snap_digest IS NOT NULL AND meta_artifact_id IS NOT NULL AND completeness <> 'complete'
            AND log_digest = snap_digest AND log_size = snap_size AND completeness = snap_completeness
            AND reasons = snap_reasons)
    ),
    CONSTRAINT log_records_removed_shape CHECK (removed_at IS NULL OR state = 'retained')
);
CREATE INDEX log_records_attempt_idx ON log_records (tenant_id, project_id, attempt_id);
CREATE INDEX log_records_log_artifact_idx ON log_records (tenant_id, project_id, log_artifact_id)
    WHERE log_artifact_id IS NOT NULL;

-- Identity, snapshot and linkage are immutable once set; the lifecycle moves
-- one way; the fence only increases; a new generation exists only after lost
-- frozen bytes; a link refers only to usable diagnostic files of the same
-- attempt.
CREATE FUNCTION t023_log_records_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'log records are retained' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.generation > 1 AND NOT EXISTS (
            SELECT 1 FROM log_records p
            WHERE p.tenant_id = NEW.tenant_id AND p.project_id = NEW.project_id AND p.attempt_id = NEW.attempt_id
              AND p.source = NEW.source AND p.generation = NEW.generation - 1
              AND p.state = 'unavailable' AND p.failure_class = 'frozen_bytes_lost') THEN
            RAISE EXCEPTION 'a new capture generation follows lost frozen bytes only' USING ERRCODE = '23514';
        END IF;
        IF NEW.state <> 'pending' THEN
            RAISE EXCEPTION 'a log record starts pending' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id OR NEW.step_id <> OLD.step_id
       OR NEW.source <> OLD.source OR NEW.generation <> OLD.generation OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'log record identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
           (OLD.state = 'pending' AND NEW.state IN ('collecting', 'retained'))
        OR (OLD.state = 'collecting' AND NEW.state IN ('retained', 'unavailable'))
        OR (OLD.state = 'unavailable' AND NEW.state IN ('collecting', 'retained'))) THEN
        RAISE EXCEPTION 'invalid log record transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'retained' AND (
           NEW.state <> 'retained' OR NEW.log_artifact_id IS DISTINCT FROM OLD.log_artifact_id
        OR NEW.log_digest IS DISTINCT FROM OLD.log_digest OR NEW.log_size IS DISTINCT FROM OLD.log_size
        OR NEW.meta_artifact_id IS DISTINCT FROM OLD.meta_artifact_id OR NEW.meta_digest IS DISTINCT FROM OLD.meta_digest
        OR NEW.meta_size IS DISTINCT FROM OLD.meta_size OR NEW.completeness IS DISTINCT FROM OLD.completeness
        OR NEW.reasons IS DISTINCT FROM OLD.reasons) THEN
        RAISE EXCEPTION 'verified log linkage and its completeness are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.removed_at IS NOT NULL AND NEW.removed_at IS DISTINCT FROM OLD.removed_at THEN
        RAISE EXCEPTION 'the removal is recorded once' USING ERRCODE = '23514';
    END IF;
    IF OLD.snap_digest IS NOT NULL AND (
           NEW.snap_digest IS DISTINCT FROM OLD.snap_digest OR NEW.snap_log_name IS DISTINCT FROM OLD.snap_log_name
        OR NEW.snap_meta_name IS DISTINCT FROM OLD.snap_meta_name OR NEW.snap_size IS DISTINCT FROM OLD.snap_size
        OR NEW.snap_completeness IS DISTINCT FROM OLD.snap_completeness OR NEW.snap_reasons IS DISTINCT FROM OLD.snap_reasons) THEN
        RAISE EXCEPTION 'the frozen snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.claim_fence < OLD.claim_fence OR NEW.claim_fence > OLD.claim_fence + 1 THEN
        RAISE EXCEPTION 'claim fences only increase by one' USING ERRCODE = '23514';
    END IF;
    IF NEW.claims < OLD.claims OR NEW.claims > OLD.claims + 1 THEN
        RAISE EXCEPTION 'claims are counted one at a time' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'a change increments the version by one' USING ERRCODE = '23514';
    END IF;
    -- Verified linkage: both artifacts are usable, active diagnostic files of the
    -- same attempt, with exactly the recorded digest and size. Set once.
    IF NEW.log_artifact_id IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.log_artifact_id IS NULL) THEN
        IF NOT EXISTS (
            SELECT 1 FROM artifacts a
            WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id AND a.id = NEW.log_artifact_id
              AND a.attempt_id = NEW.attempt_id AND a.run_id = NEW.run_id AND a.kind = 'file' AND a.role = 'diagnostic'
              AND a.state = 'usable' AND a.disposition = 'active'
              AND a.digest = NEW.log_digest AND a.size_bytes = NEW.log_size) THEN
            RAISE EXCEPTION 'the linked log is not a usable diagnostic file of the attempt' USING ERRCODE = '23514';
        END IF;
        IF NEW.meta_artifact_id IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM artifacts a
            WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id AND a.id = NEW.meta_artifact_id
              AND a.attempt_id = NEW.attempt_id AND a.run_id = NEW.run_id AND a.kind = 'file' AND a.role = 'diagnostic'
              AND a.state = 'usable' AND a.disposition = 'active'
              AND a.digest = NEW.meta_digest AND a.size_bytes = NEW.meta_size) THEN
            RAISE EXCEPTION 'the linked sidecar is not a usable diagnostic file of the attempt' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER log_records_guard BEFORE INSERT OR UPDATE OR DELETE ON log_records
    FOR EACH ROW EXECUTE FUNCTION t023_log_records_guard();

ALTER TABLE log_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE log_records FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON log_records TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON log_records
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Runtime grants: no DELETE; UPDATE only on the mutable lifecycle columns.
GRANT SELECT, INSERT ON log_records TO {{runtime_role}};
GRANT UPDATE (state, claim_token, claim_fence, claim_until, claims, failure_class,
              snap_log_name, snap_meta_name, snap_digest, snap_size, snap_completeness, snap_reasons,
              log_artifact_id, log_digest, log_size, meta_artifact_id, meta_digest, meta_size,
              completeness, reasons, removed_at, version, updated_at) ON log_records TO {{runtime_role}};
