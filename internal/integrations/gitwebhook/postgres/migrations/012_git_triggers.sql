-- T-137 native Git triggers and task routing (additive; contribution of
-- internal/integrations/gitwebhook).
--
-- Three tables, none of which touches the external receive-hook tables of the
-- baseline (git_triggers, git_trigger_deliveries, git_trigger_admissions):
--
--   git_push_triggers   the configuration of a trigger of the internal Git
--                       service: one repository, one to 32 ref patterns, the
--                       origin filter, the definition and the enabled flag
--                       (docs/contracts/git-service.md, "Trigger configuration");
--   git_task_routes     a project's pipeline for ready work items (at most one);
--   git_trigger_inbox   the durable claim and write-once outcome of one
--                       admission: (trigger, push record, ref) for a push, or
--                       (project, readiness transition) for a task. The run's
--                       stable identity derived from the key is what makes
--                       admission itself idempotent; the inbox records which
--                       keys were decided, how, and who is attempting one.
--
-- enabled_at is when a trigger or route was created enabled or last re-enabled.
-- The consumers ignore an event committed before it, so a late redelivery cannot
-- start runs for pushes (or readiness transitions) the configuration was not yet
-- watching.
--
-- Retention. Inbox rows are kept: the runtime role has no DELETE and the guard
-- makes a decided key final, because a decided row is what stops a redelivered
-- event from starting its run again. Removing old decided rows is a maintenance
-- action of the owner role, not of the request path; it belongs to the
-- retention and maintenance work of T-138 (internal/gitops), scheduled by T-132's
-- composition. No horizon is chosen here. Any horizon must exceed the longest
-- event redelivery window of the bus (the stream's maximum age plus its
-- redelivery backoff), because deleting a decided row re-opens its key.
--
-- Ownership is a composite foreign key to the project (and, for a trigger, to
-- its repository and definition), so a trigger can name neither a foreign
-- repository nor a foreign definition. Row level security is enabled and forced
-- with a tenant policy that fails closed. The inbox keeps no foreign key to
-- runs: the run is created by the execution module's own transaction, before
-- the outcome is recorded here, and a deleted trigger must not erase the record
-- that stops a redelivered event from starting its run again.

CREATE TABLE git_push_triggers (
    tenant_id           kodama_id NOT NULL,
    project_id          kodama_id NOT NULL,
    id                  kodama_id NOT NULL,
    repository_id       kodama_id NOT NULL,
    ref_patterns        text[] NOT NULL
                        CONSTRAINT git_push_triggers_patterns_count CHECK (cardinality(ref_patterns) BETWEEN 1 AND 32),
    exclude_origin_runs boolean NOT NULL,
    definition_id       kodama_id NOT NULL,
    enabled             boolean NOT NULL,
    enabled_at          timestamptz NOT NULL,
    configured_by       text NOT NULL
                        CONSTRAINT git_push_triggers_configured_by_length CHECK (char_length(configured_by) BETWEEN 1 AND 256),
    version             bigint NOT NULL
                        CONSTRAINT git_push_triggers_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, definition_id) REFERENCES pipeline_definitions (tenant_id, project_id, id),
    CONSTRAINT git_push_triggers_update_order CHECK (updated_at >= created_at),
    CONSTRAINT git_push_triggers_enabled_order CHECK (enabled_at >= created_at)
);

CREATE INDEX git_push_triggers_enabled_idx ON git_push_triggers (tenant_id, project_id, repository_id, id) WHERE enabled;

CREATE TABLE git_task_routes (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    definition_id kodama_id NOT NULL,
    enabled       boolean NOT NULL,
    enabled_at    timestamptz NOT NULL,
    configured_by text NOT NULL
                  CONSTRAINT git_task_routes_configured_by_length CHECK (char_length(configured_by) BETWEEN 1 AND 256),
    version       bigint NOT NULL
                  CONSTRAINT git_task_routes_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, definition_id) REFERENCES pipeline_definitions (tenant_id, project_id, id),
    CONSTRAINT git_task_routes_update_order CHECK (updated_at >= created_at),
    CONSTRAINT git_task_routes_enabled_order CHECK (enabled_at >= created_at)
);

CREATE TABLE git_trigger_inbox (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    kind         text NOT NULL
                 CONSTRAINT git_trigger_inbox_kind_known CHECK (kind IN ('push', 'task_ready')),
    source       text NOT NULL
                 CONSTRAINT git_trigger_inbox_source_length CHECK (char_length(source) BETWEEN 1 AND 512),
    subject      text NOT NULL
                 CONSTRAINT git_trigger_inbox_subject_length CHECK (char_length(subject) BETWEEN 1 AND 512),
    -- The pushed ref; empty for a task.
    ref          text NOT NULL
                 CONSTRAINT git_trigger_inbox_ref_length CHECK (char_length(ref) <= 512),
    lease_holder text
                 CONSTRAINT git_trigger_inbox_lease_holder_length CHECK (char_length(lease_holder) BETWEEN 1 AND 256),
    lease_until  timestamptz,
    done         boolean NOT NULL DEFAULT false,
    outcome      text
                 CONSTRAINT git_trigger_inbox_outcome_known CHECK (outcome IN ('started', 'ignored', 'rejected')),
    reason       text
                 CONSTRAINT git_trigger_inbox_reason_length CHECK (char_length(reason) BETWEEN 1 AND 128),
    run_id       kodama_id,
    received_at  timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (tenant_id, project_id, kind, source, subject, ref),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT git_trigger_inbox_ref_shape CHECK ((kind = 'push') = (ref <> '')),
    CONSTRAINT git_trigger_inbox_lease_pair CHECK ((lease_holder IS NULL) = (lease_until IS NULL)),
    -- A finished key holds no lease and has exactly one outcome; started is the
    -- only outcome with a run, every other outcome states its reason.
    CONSTRAINT git_trigger_inbox_done_shape CHECK (
        (NOT done AND outcome IS NULL AND reason IS NULL AND run_id IS NULL AND completed_at IS NULL)
        OR (done AND outcome IS NOT NULL AND completed_at IS NOT NULL AND lease_holder IS NULL
            AND ((outcome = 'started') = (run_id IS NOT NULL))
            AND (outcome = 'started' OR reason IS NOT NULL)))
);

CREATE INDEX git_trigger_inbox_decided_idx ON git_trigger_inbox (tenant_id, project_id, kind, source, completed_at DESC) WHERE done;

CREATE INDEX git_trigger_inbox_pending_idx ON git_trigger_inbox (tenant_id, project_id, received_at) WHERE NOT done;

ALTER TABLE git_push_triggers ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_push_triggers FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_push_triggers TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_push_triggers
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_task_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_task_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_task_routes TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_task_routes
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_trigger_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_trigger_inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_trigger_inbox TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_trigger_inbox
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Guards, effective for every role including the owner.
CREATE FUNCTION git_push_triggers_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.id, NEW.repository_id, NEW.created_at)
       IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.id, OLD.repository_id, OLD.created_at) THEN
        RAISE EXCEPTION 'a push trigger keeps its identity and repository binding' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'push trigger version must advance by one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_push_triggers_guard BEFORE UPDATE ON git_push_triggers
    FOR EACH ROW EXECUTE FUNCTION git_push_triggers_guard();

CREATE FUNCTION git_task_routes_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.created_at) IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.created_at) THEN
        RAISE EXCEPTION 'a task route keeps its project' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'task route version must advance by one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_task_routes_guard BEFORE UPDATE ON git_task_routes
    FOR EACH ROW EXECUTE FUNCTION git_task_routes_guard();

CREATE FUNCTION git_trigger_inbox_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.kind, NEW.source, NEW.subject, NEW.ref, NEW.received_at)
       IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.kind, OLD.source, OLD.subject, OLD.ref, OLD.received_at) THEN
        RAISE EXCEPTION 'an inbox key never changes' USING ERRCODE = '23514';
    END IF;
    IF OLD.done THEN
        RAISE EXCEPTION 'a decided inbox key is final' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_trigger_inbox_guard BEFORE UPDATE ON git_trigger_inbox
    FOR EACH ROW EXECUTE FUNCTION git_trigger_inbox_guard();

-- Least privilege. Configuration can be altered (a trigger is disabled, not
-- deleted, so its history and the inbox keys that name it stay explainable); the
-- inbox can only be claimed, leased and decided. The runtime role never deletes.
GRANT SELECT, INSERT ON git_push_triggers TO {{runtime_role}};
GRANT UPDATE (ref_patterns, exclude_origin_runs, definition_id, enabled, enabled_at, configured_by, version, updated_at) ON git_push_triggers TO {{runtime_role}};

GRANT SELECT, INSERT ON git_task_routes TO {{runtime_role}};
GRANT UPDATE (definition_id, enabled, enabled_at, configured_by, version, updated_at) ON git_task_routes TO {{runtime_role}};

GRANT SELECT, INSERT ON git_trigger_inbox TO {{runtime_role}};
GRANT UPDATE (lease_holder, lease_until, done, outcome, reason, run_id, completed_at) ON git_trigger_inbox TO {{runtime_role}};
