-- T-035 delivery schema: project environments, immutable configuration
-- snapshots, pre-apply deployment records and their append-only history
-- (docs/specs/delivery-and-quality-gates.md).
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration and restore, and
-- minimal runtime grants (column-level UPDATE; no DELETE anywhere).
--
-- Ordered after the base, identity, secrets, pipeline, workspace, artifact and
-- execution contributions: deployments reference the producing and originating
-- attempts (step_attempts) and the image artifact.
--
-- No secret value, kubeconfig content or manifest is stored. Credential
-- references and pinned versions live in the snapshot; plaintext stays with the
-- secrets module.

-- ---- environments -----------------------------------------------------------

CREATE TABLE environments (
    tenant_id   kodama_id NOT NULL REFERENCES tenants (id),
    project_id  kodama_id NOT NULL,
    id          kodama_id NOT NULL,
    name        text NOT NULL
                CONSTRAINT environments_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    kind        text NOT NULL
                CONSTRAINT environments_kind_known CHECK (kind IN ('preview', 'stage')),
    target      text NOT NULL
                CONSTRAINT environments_target_syntax CHECK (target ~ '^[a-z][a-z0-9._-]{0,62}$'),
    namespace   text NOT NULL
                CONSTRAINT environments_namespace_syntax CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    -- Credential references (never values or versions): {name, ref, delivery}.
    kubeconfig  jsonb NOT NULL
                CONSTRAINT environments_kubeconfig_object CHECK (jsonb_typeof(kubeconfig) = 'object'),
    secrets     jsonb NOT NULL DEFAULT '[]'
                CONSTRAINT environments_secrets_array CHECK (jsonb_typeof(secrets) = 'array' AND jsonb_array_length(secrets) <= 32),
    config      jsonb NOT NULL DEFAULT '{}'
                CONSTRAINT environments_config_object CHECK (jsonb_typeof(config) = 'object' AND pg_column_size(config) <= 131072),
    preview_ttl_seconds bigint NOT NULL
                CONSTRAINT environments_ttl_range CHECK (preview_ttl_seconds BETWEEN 0 AND 2592000),
    version     bigint NOT NULL
                CONSTRAINT environments_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    CONSTRAINT environments_name_unique UNIQUE (tenant_id, project_id, name),
    CONSTRAINT environments_ttl_by_kind CHECK ((kind = 'preview') = (preview_ttl_seconds > 0)),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE FUNCTION t035_environments_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.name <> OLD.name OR NEW.kind <> OLD.kind OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'environment identity, name and kind are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'environment resource version must increase' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER environments_guard BEFORE UPDATE ON environments
    FOR EACH ROW EXECUTE FUNCTION t035_environments_guard();

-- ---- immutable configuration snapshots --------------------------------------

-- One row per distinct effective configuration of an environment. The digest
-- covers every column below it, so the row can be recomputed and compared.
CREATE TABLE environment_snapshots (
    tenant_id      kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    environment_id kodama_id NOT NULL,
    digest         text NOT NULL
                   CONSTRAINT environment_snapshots_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    kind           text NOT NULL CONSTRAINT environment_snapshots_kind_known CHECK (kind IN ('preview', 'stage')),
    target         text NOT NULL,
    namespace      text NOT NULL,
    config         jsonb NOT NULL CONSTRAINT environment_snapshots_config_object CHECK (jsonb_typeof(config) = 'object'),
    preview_ttl_seconds bigint NOT NULL CONSTRAINT environment_snapshots_ttl_range CHECK (preview_ttl_seconds BETWEEN 0 AND 2592000),
    kubeconfig     text NOT NULL,
    -- Exact bindings: [{ref, version, name, delivery}], sorted by name.
    secrets        jsonb NOT NULL
                   CONSTRAINT environment_snapshots_secrets_array CHECK (jsonb_typeof(secrets) = 'array' AND jsonb_array_length(secrets) BETWEEN 1 AND 33),
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, environment_id, digest),
    FOREIGN KEY (tenant_id, project_id, environment_id) REFERENCES environments (tenant_id, project_id, id)
);

CREATE FUNCTION t035_snapshots_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a configuration snapshot is immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER environment_snapshots_guard BEFORE UPDATE OR DELETE ON environment_snapshots
    FOR EACH ROW EXECUTE FUNCTION t035_snapshots_guard();

-- ---- deployments ------------------------------------------------------------

CREATE TABLE deployments (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    environment_id     kodama_id NOT NULL,
    config_digest      text NOT NULL,
    namespace          text NOT NULL,
    source_repository_url text NOT NULL
                       CONSTRAINT deployments_source_url_syntax CHECK (source_repository_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*)?$'),
    source_commit      text NOT NULL
                       CONSTRAINT deployments_source_commit_syntax CHECK (source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    image_artifact_id  kodama_id NOT NULL,
    image_digest       text NOT NULL
                       CONSTRAINT deployments_image_digest_syntax CHECK (image_digest ~ '^sha256:[0-9a-f]{64}$'),
    -- Verified producer provenance: the attempt that produced the image and the
    -- tool image of its immutable command context.
    producer_attempt_id kodama_id NOT NULL,
    producer_run_id    kodama_id NOT NULL,
    producer_step_id   text NOT NULL CONSTRAINT deployments_producer_step_length CHECK (char_length(producer_step_id) BETWEEN 1 AND 128),
    tool_image         text NOT NULL CONSTRAINT deployments_tool_image_syntax CHECK (tool_image ~ '^[^\s@]+@sha256:[0-9a-f]{64}$'),
    -- The logical action: one deployment per (tenant, project, action key).
    run_id             kodama_id NOT NULL,
    step_id            text NOT NULL CONSTRAINT deployments_step_length CHECK (char_length(step_id) BETWEEN 1 AND 128),
    action_key         text NOT NULL
                       CONSTRAINT deployments_action_key_syntax CHECK (action_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    attempt_id         kodama_id NOT NULL,
    request_fingerprint text NOT NULL
                       CONSTRAINT deployments_fingerprint_syntax CHECK (request_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    status             text NOT NULL
                       CONSTRAINT deployments_status_known CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    reason             text NOT NULL
                       CONSTRAINT deployments_reason_code CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    -- Bounded external object identities: intended before apply, observed after.
    objects            jsonb NOT NULL DEFAULT '[]'
                       CONSTRAINT deployments_objects_array CHECK (jsonb_typeof(objects) = 'array' AND jsonb_array_length(objects) <= 64),
    expires_at         timestamptz,
    cleanup_requested_at timestamptz,
    cleanup_state      text NOT NULL DEFAULT ''
                       CONSTRAINT deployments_cleanup_state_known CHECK (cleanup_state IN ('', 'requested', 'failed', 'unknown', 'done')),
    cleanup_reason     text NOT NULL DEFAULT ''
                       CONSTRAINT deployments_cleanup_reason_code CHECK (cleanup_reason = '' OR cleanup_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    deleted_at         timestamptz,
    version            bigint NOT NULL
                       CONSTRAINT deployments_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    CONSTRAINT deployments_action_key_unique UNIQUE (tenant_id, project_id, action_key),
    CONSTRAINT deployments_cleanup_consistent CHECK (
        (cleanup_state = '') = (cleanup_requested_at IS NULL)
        AND ((cleanup_state = 'done') = (deleted_at IS NOT NULL))),
    CONSTRAINT deployments_failed_has_no_applying CHECK (NOT (status IN ('succeeded', 'failed') AND reason IN ('applying', 'outcome_unknown', 'registered'))),
    CONSTRAINT deployments_succeeded_ready CHECK ((status = 'succeeded') = (reason = 'ready')),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    -- The pinned environment and its retained configuration: a deployment can
    -- only reference a snapshot of an environment of its own project.
    FOREIGN KEY (tenant_id, project_id, environment_id, config_digest)
        REFERENCES environment_snapshots (tenant_id, project_id, environment_id, digest),
    -- The originating attempt, with run and step matching its own columns.
    FOREIGN KEY (tenant_id, project_id, attempt_id, run_id, step_id)
        REFERENCES step_attempts (tenant_id, project_id, id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, producer_attempt_id, producer_run_id, producer_step_id)
        REFERENCES step_attempts (tenant_id, project_id, id, run_id, step_id),
    FOREIGN KEY (tenant_id, project_id, image_artifact_id) REFERENCES artifacts (tenant_id, project_id, id)
);
CREATE INDEX deployments_environment_idx ON deployments (tenant_id, project_id, environment_id, id);
CREATE INDEX deployments_expiry_idx ON deployments (tenant_id, project_id, expires_at) WHERE expires_at IS NOT NULL AND deleted_at IS NULL;

CREATE FUNCTION t035_deployments_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.environment_id <> OLD.environment_id OR NEW.config_digest <> OLD.config_digest OR NEW.namespace <> OLD.namespace
       OR NEW.source_repository_url <> OLD.source_repository_url OR NEW.source_commit <> OLD.source_commit
       OR NEW.image_artifact_id <> OLD.image_artifact_id OR NEW.image_digest <> OLD.image_digest
       OR NEW.producer_attempt_id <> OLD.producer_attempt_id OR NEW.producer_run_id <> OLD.producer_run_id
       OR NEW.producer_step_id <> OLD.producer_step_id OR NEW.tool_image <> OLD.tool_image
       OR NEW.run_id <> OLD.run_id OR NEW.step_id <> OLD.step_id OR NEW.action_key <> OLD.action_key
       OR NEW.attempt_id <> OLD.attempt_id OR NEW.request_fingerprint <> OLD.request_fingerprint
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'deployment identity, provenance, configuration and expiry are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'deployment version must increase' USING ERRCODE = '23514';
    END IF;
    -- The outcome lifecycle. A settled outcome never changes; only the cleanup
    -- columns may move afterwards.
    IF OLD.status IN ('succeeded', 'failed') THEN
        IF NEW.status <> OLD.status OR NEW.reason <> OLD.reason OR NEW.objects <> OLD.objects THEN
            RAISE EXCEPTION 'a settled deployment outcome is immutable' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status = 'pending' THEN
        IF NEW.status NOT IN ('pending', 'running', 'failed') THEN
            RAISE EXCEPTION 'invalid deployment transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status = 'running' THEN
        IF NEW.status NOT IN ('running', 'succeeded', 'failed') THEN
            RAISE EXCEPTION 'invalid deployment transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
        END IF;
        -- An unknown outcome never returns to applying: it is settled explicitly.
        IF OLD.reason = 'outcome_unknown' AND NEW.status = 'running' AND NEW.reason <> 'outcome_unknown' THEN
            RAISE EXCEPTION 'an unknown outcome is settled, not reapplied' USING ERRCODE = '23514';
        END IF;
    END IF;
    -- Intended objects are recorded once, at the pending -> running step.
    IF OLD.status = 'running' AND NEW.status = 'running' AND NEW.objects <> OLD.objects THEN
        RAISE EXCEPTION 'intended objects do not change while running' USING ERRCODE = '23514';
    END IF;
    IF OLD.cleanup_requested_at IS NOT NULL AND NEW.cleanup_requested_at IS DISTINCT FROM OLD.cleanup_requested_at THEN
        RAISE EXCEPTION 'the cleanup request time is set once' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND (NEW.deleted_at IS DISTINCT FROM OLD.deleted_at OR NEW.cleanup_state <> OLD.cleanup_state) THEN
        RAISE EXCEPTION 'a deleted deployment stays deleted' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER deployments_guard BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION t035_deployments_guard();

-- ---- append-only history (durable receipts) ---------------------------------

CREATE TABLE deployment_history (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    deployment_id kodama_id NOT NULL,
    seq           bigint GENERATED ALWAYS AS IDENTITY,
    kind          text NOT NULL CONSTRAINT deployment_history_kind_syntax CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
    version       bigint NOT NULL CONSTRAINT deployment_history_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    from_status   text NOT NULL,
    to_status     text NOT NULL,
    reason        text NOT NULL,
    actor_type    text NOT NULL CONSTRAINT deployment_history_actor_type_known CHECK (actor_type IN ('user', 'workload', 'system')),
    actor_id      text NOT NULL CONSTRAINT deployment_history_actor_id_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    attempt_id    text NOT NULL DEFAULT '',
    detail        jsonb NOT NULL DEFAULT '{}'
                  CONSTRAINT deployment_history_detail_object CHECK (jsonb_typeof(detail) = 'object' AND pg_column_size(detail) <= 8192),
    occurred_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, deployment_id, seq),
    FOREIGN KEY (tenant_id, project_id, deployment_id) REFERENCES deployments (tenant_id, project_id, id)
);
CREATE UNIQUE INDEX deployment_history_seq_idx ON deployment_history (seq);

CREATE FUNCTION t035_history_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'deployment history is append-only' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER deployment_history_guard BEFORE UPDATE OR DELETE ON deployment_history
    FOR EACH ROW EXECUTE FUNCTION t035_history_guard();

-- ---- row level security -----------------------------------------------------

ALTER TABLE environments          ENABLE ROW LEVEL SECURITY;
ALTER TABLE environments          FORCE ROW LEVEL SECURITY;
ALTER TABLE environment_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE environment_snapshots FORCE ROW LEVEL SECURITY;
ALTER TABLE deployments           ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployments           FORCE ROW LEVEL SECURITY;
ALTER TABLE deployment_history    ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_history    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON environments          TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON environment_snapshots TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON deployments           TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON deployment_history    TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON environments
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON environment_snapshots
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON deployments
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON deployment_history
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Runtime grants: INSERT and SELECT, and UPDATE only on mutable columns.
-- Snapshots and history are never updated or deleted.
GRANT SELECT, INSERT ON environments TO {{runtime_role}};
GRANT UPDATE (target, namespace, kubeconfig, secrets, config, preview_ttl_seconds, version, updated_at) ON environments TO {{runtime_role}};
GRANT SELECT, INSERT ON environment_snapshots TO {{runtime_role}};
GRANT SELECT, INSERT ON deployments TO {{runtime_role}};
GRANT UPDATE (status, reason, objects, cleanup_requested_at, cleanup_state, cleanup_reason, deleted_at, version, updated_at) ON deployments TO {{runtime_role}};
GRANT SELECT, INSERT ON deployment_history TO {{runtime_role}};
