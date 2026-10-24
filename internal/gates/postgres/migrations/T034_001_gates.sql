-- T-034 human gates: one gate per run step, its canonical subject and
-- required-evidence snapshot, the immutable decision and the separate
-- invalidation.
--
-- Ownership and integrity enforced by the database, not only by Go:
--   * every row carries tenant_id and project_id; foreign keys are composite, so
--     a gate can only pause a step of its own project's run, can only name a work
--     item of its own project, and every evidence reference can only name an
--     artifact of its own project;
--   * forced row level security on kodama_tenant_id(), column-scoped runtime
--     grants and no DELETE anywhere: gates are history;
--   * the subject, the required-evidence snapshot, the deadline and the identity
--     of a gate are immutable; the status moves from pending to approved or
--     rejected exactly once; the decision (actor, reason, time) is recorded once
--     and never rewritten; an invalidation is recorded once and never touches
--     the decision (guards below apply to every role, the owner included);
--   * one gate per run step.
--
-- Depends on the identity, work, pipeline, artifact, execution and event/HTTP
-- foundations (tenants, projects, work_items, step_runs, artifacts, audit);
-- list it after them in the manifest.

CREATE TABLE gates (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    step_id            text NOT NULL
                       CONSTRAINT gates_step_syntax CHECK (step_id ~ '^[a-z][a-z0-9-]{0,62}$'),
    work_item_id       kodama_id NOT NULL,
    -- Searchable copies of the subject fields; the canonical subject is the
    -- authority and the guard below keeps both in step.
    content_revision   bigint NOT NULL
                       CONSTRAINT gates_content_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    candidate_commit   text NOT NULL
                       CONSTRAINT gates_candidate_commit_syntax CHECK (candidate_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    definition_id      kodama_id NOT NULL,
    deployment_id      kodama_id,
    deployment_config_digest text
                       CONSTRAINT gates_config_digest_syntax CHECK (deployment_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    subject            text NOT NULL
                       CONSTRAINT gates_subject_bounds CHECK (octet_length(subject) BETWEEN 2 AND 262144),
    subject_digest     text NOT NULL
                       CONSTRAINT gates_subject_digest_syntax CHECK (subject_digest ~ '^sha256:[0-9a-f]{64}$'),
    required           jsonb NOT NULL
                       CONSTRAINT gates_required_bounds CHECK (jsonb_typeof(required) = 'object' AND pg_column_size(required) <= 1048576),
    status             text NOT NULL DEFAULT 'pending'
                       CONSTRAINT gates_status_known CHECK (status IN ('pending', 'approved', 'rejected')),
    version            bigint NOT NULL DEFAULT 1
                       CONSTRAINT gates_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    requested_at       timestamptz NOT NULL,
    deadline           timestamptz NOT NULL,
    decided_by_type    text
                       CONSTRAINT gates_decided_by_type_known CHECK (decided_by_type IN ('user')),
    decided_by_id      text
                       CONSTRAINT gates_decided_by_id_length CHECK (char_length(decided_by_id) BETWEEN 1 AND 256),
    decision           text
                       CONSTRAINT gates_decision_known CHECK (decision IN ('approve', 'reject')),
    decision_reason    text
                       CONSTRAINT gates_decision_reason_length CHECK (char_length(decision_reason) BETWEEN 1 AND 2048),
    decided_at         timestamptz,
    invalidated_at     timestamptz,
    invalidation_reason text
                       CONSTRAINT gates_invalidation_reason_code CHECK (invalidation_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    superseding_commit text
                       CONSTRAINT gates_superseding_commit_syntax CHECK (superseding_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    superseding_revision bigint
                       CONSTRAINT gates_superseding_revision_range CHECK (superseding_revision BETWEEN 1 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_id) REFERENCES step_runs (tenant_id, project_id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id),
    -- One gate per run step: creation is idempotent under this identity.
    CONSTRAINT gates_step_unique UNIQUE (tenant_id, project_id, run_id, step_id),
    CONSTRAINT gates_deployment_both CHECK ((deployment_id IS NULL) = (deployment_config_digest IS NULL)),
    CONSTRAINT gates_decision_shape CHECK (
        (status = 'pending' AND decision IS NULL AND decided_by_type IS NULL AND decided_by_id IS NULL AND decision_reason IS NULL AND decided_at IS NULL)
        OR (status = 'approved' AND decision = 'approve' AND decided_by_type IS NOT NULL AND decided_by_id IS NOT NULL AND decision_reason IS NOT NULL AND decided_at IS NOT NULL)
        OR (status = 'rejected' AND decision = 'reject' AND decided_by_type IS NOT NULL AND decided_by_id IS NOT NULL AND decision_reason IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT gates_invalidation_both CHECK ((invalidated_at IS NULL) = (invalidation_reason IS NULL)),
    CONSTRAINT gates_superseding_needs_invalidation CHECK ((superseding_commit IS NULL AND superseding_revision IS NULL) OR invalidated_at IS NOT NULL),
    CONSTRAINT gates_deadline_after_request CHECK (deadline > requested_at)
);
CREATE INDEX gates_list_idx ON gates (tenant_id, project_id, created_at, id);
CREATE INDEX gates_run_idx ON gates (tenant_id, project_id, run_id);
-- Reconciliation scans gates that were not invalidated.
CREATE INDEX gates_open_idx ON gates (tenant_id, project_id, id) WHERE invalidated_at IS NULL;

-- Evidence references of a gate: the artifacts its subject binds and why. The
-- composite key to artifacts proves each belongs to the gate's project.
CREATE TABLE gate_evidence (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    gate_id      kodama_id NOT NULL,
    artifact_id  kodama_id NOT NULL,
    role         text NOT NULL
                 CONSTRAINT gate_evidence_role_known CHECK (role IN ('evidence', 'report', 'support')),
    digest       text NOT NULL
                 CONSTRAINT gate_evidence_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, project_id, gate_id, artifact_id, role),
    FOREIGN KEY (tenant_id, project_id, gate_id) REFERENCES gates (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id)
);
CREATE INDEX gate_evidence_artifact_idx ON gate_evidence (tenant_id, project_id, artifact_id);

-- ---- guards ----

CREATE FUNCTION t034_gates_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'gates are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id OR NEW.run_id <> OLD.run_id
       OR NEW.step_id <> OLD.step_id OR NEW.work_item_id <> OLD.work_item_id OR NEW.content_revision <> OLD.content_revision
       OR NEW.candidate_commit <> OLD.candidate_commit OR NEW.definition_id <> OLD.definition_id
       OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.deployment_config_digest IS DISTINCT FROM OLD.deployment_config_digest
       OR NEW.subject <> OLD.subject OR NEW.subject_digest <> OLD.subject_digest OR NEW.required IS DISTINCT FROM OLD.required
       OR NEW.requested_at <> OLD.requested_at OR NEW.deadline <> OLD.deadline OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the subject, evidence snapshot, deadline and identity of a gate are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status <> OLD.status AND NOT (OLD.status = 'pending' AND NEW.status IN ('approved', 'rejected')) THEN
        RAISE EXCEPTION 'invalid gate transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'pending' AND (NEW.decision IS DISTINCT FROM OLD.decision OR NEW.decided_by_type IS DISTINCT FROM OLD.decided_by_type
       OR NEW.decided_by_id IS DISTINCT FROM OLD.decided_by_id OR NEW.decision_reason IS DISTINCT FROM OLD.decision_reason
       OR NEW.decided_at IS DISTINCT FROM OLD.decided_at) THEN
        RAISE EXCEPTION 'a gate decision is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.invalidated_at IS NOT NULL AND (NEW.invalidated_at IS DISTINCT FROM OLD.invalidated_at
       OR NEW.invalidation_reason IS DISTINCT FROM OLD.invalidation_reason
       OR NEW.superseding_commit IS DISTINCT FROM OLD.superseding_commit
       OR NEW.superseding_revision IS DISTINCT FROM OLD.superseding_revision) THEN
        RAISE EXCEPTION 'a gate invalidation is recorded once' USING ERRCODE = '23514';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'a resource version never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER gates_guard BEFORE UPDATE OR DELETE ON gates FOR EACH ROW EXECUTE FUNCTION t034_gates_guard();

-- The searchable copies must equal the canonical subject they were taken from.
CREATE FUNCTION t034_gates_subject_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    doc jsonb;
BEGIN
    doc := NEW.subject::jsonb;
    IF doc->>'tenantId' <> NEW.tenant_id OR doc->>'projectId' <> NEW.project_id OR doc->>'workItemId' <> NEW.work_item_id
       OR (doc->>'contentRevision')::bigint <> NEW.content_revision OR doc->>'candidateCommit' <> NEW.candidate_commit
       OR doc->>'definitionId' <> NEW.definition_id OR (doc->>'deploymentId') IS DISTINCT FROM NEW.deployment_id
       OR (doc->>'deploymentConfigDigest') IS DISTINCT FROM NEW.deployment_config_digest THEN
        RAISE EXCEPTION 'the gate columns do not match its canonical subject' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER gates_subject_guard BEFORE INSERT ON gates FOR EACH ROW EXECUTE FUNCTION t034_gates_subject_guard();

CREATE FUNCTION t034_gate_evidence_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'gate evidence references are immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER gate_evidence_guard BEFORE UPDATE OR DELETE ON gate_evidence FOR EACH ROW EXECUTE FUNCTION t034_gate_evidence_guard();

-- ---- row level security ----

ALTER TABLE gates         ENABLE ROW LEVEL SECURITY;
ALTER TABLE gates         FORCE ROW LEVEL SECURITY;
ALTER TABLE gate_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE gate_evidence FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON gates TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON gates
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Evidence references are append-only: no UPDATE or DELETE policy exists.
CREATE POLICY owner_select ON gate_evidence FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON gate_evidence FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON gate_evidence FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON gate_evidence FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- grants: no DELETE; UPDATE is column-scoped to the mutable state ----

GRANT SELECT, INSERT ON gates TO {{runtime_role}};
GRANT UPDATE (status, version, decided_by_type, decided_by_id, decision, decision_reason, decided_at,
              invalidated_at, invalidation_reason, superseding_commit, superseding_revision, updated_at)
    ON gates TO {{runtime_role}};

GRANT SELECT, INSERT ON gate_evidence TO {{runtime_role}};
