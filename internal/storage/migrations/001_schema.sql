-- T-002 baseline schema: every Kodama table, view, function, index and policy.
--
-- This file is the squashed, single baseline of all capability schemas. It is
-- applied in one transaction before 002_grants.sql, which grants the runtime
-- role its minimal privileges over what this file creates. The runtime role is
-- never the owner: the migration role owns every object. A REVOKE ... FROM
-- PUBLIC stays here beside the definition it protects, so an interrupted run
-- can never leave a SECURITY DEFINER function executable by PUBLIC.
--
-- Conventions (they held for every contribution this file replaces):
--   * IDs are opaque text constrained by the kodama_id domain; times are
--     timestamptz (instants, rendered in UTC); versions are integers >= 1.
--   * Tenant-owned relations carry tenant_id; project-owned relations also
--     carry project_id. Foreign keys are composite so ownership must match.
--   * Row level security is ENABLEd and FORCEd, with a policy on
--     kodama_tenant_id() that fails closed when no tenant context is set.
--   * Section markers name the capability and the migration that authored the
--     statements, in dependency order. Statements are concatenated verbatim.
--
-- Sections, in order (the old migration ID in parentheses):
--   base tenancy             T002_001_tenancy.sql
--   audit                    T002_002_audit.sql
--   discovery                T002_003_discovery.sql
--   event delivery           T005_001_delivery.sql
--   observation replay       T005_002_observation_replay.sql
--   HTTP idempotency         T004_001_idempotency.sql
--   identity authority       T010_001_identity_authority.sql
--   work items               T011_001_work.sql
--   variables and secrets    T012_001_configuration.sql
--   secret references        T012_002_references.sql
--   pipeline definitions     T014_001_pipeline_definitions.sql
--   workspaces               T013_001_workspaces.sql
--   artifacts                T015_001_artifacts.sql
--   execution runs           T020_002_runs.sql
--   execution workspace links T020_001_attempt_fk.sql
--   execution artifact links T020_003_artifact_fk.sql
--   execution secret pin links T020_004_secret_pin_fk.sql
--   runtime authority        T019_001_runtime_grants.sql
--   execution workload cleanup T086_001_workload_cleanup.sql
--   log records              T023_001_log_records.sql
--   agent profiles           T030_001_agent_profiles.sql
--   delivery                 T035_001_delivery.sql
--   gates                    T034_001_gates.sql
--   model gateway            T032_001_gateway.sql
--   MCP proxy                T033_001_mcp_proxy.sql
--   work dependencies        T045_001_work_dependencies.sql
--   flow summaries           T042_001_flow_intervals.sql
--   ChatOps                  T041_001_chatops.sql
--   Git triggers             T100_001_git_triggers.sql


-- ---- base tenancy (was T002_001_tenancy.sql) ----

-- T-002 base tenancy schema: tenants, projects, users, memberships.
--
-- Conventions for every module migration that follows (see README.md):
--   * IDs are opaque text constrained by the kodama_id domain; times are
--     timestamptz (instants, rendered in UTC); versions are integers >= 1.
--   * Tenant-owned relations carry tenant_id; project-owned relations also
--     carry project_id. Foreign keys are composite so ownership must match.
--   * Row level security is ENABLEd and FORCEd, with a policy on
--     kodama_tenant_id() that fails closed when no tenant context is set.
--   * Tables are owned by the migration role; only the runtime role named by
--     the runtime role (002_grants.sql) receives explicit, minimal grants.

-- Same syntax as domain.IDPattern.
CREATE DOMAIN kodama_id AS text
    CONSTRAINT kodama_id_syntax CHECK (VALUE ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$');

-- Transaction-local tenant context set by storage.WithTenant. Returns NULL
-- when the setting is absent or empty (including after the transaction that
-- set it ended), so every policy comparing against it matches nothing.
CREATE FUNCTION kodama_tenant_id() RETURNS text
    LANGUAGE sql STABLE PARALLEL SAFE
    AS $$ SELECT NULLIF(pg_catalog.current_setting('kodama.tenant_id', true), '') $$;

CREATE TABLE tenants (
    id           kodama_id PRIMARY KEY,
    key          text NOT NULL UNIQUE
                 CONSTRAINT tenants_key_syntax CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
    display_name text NOT NULL DEFAULT ''
                 CONSTRAINT tenants_display_name_length CHECK (char_length(display_name) <= 256),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    version      bigint NOT NULL DEFAULT 1
                 CONSTRAINT tenants_version_range CHECK (version BETWEEN 1 AND 9007199254740991)
);

CREATE TABLE projects (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    id               kodama_id NOT NULL,
    key              text NOT NULL
                     CONSTRAINT projects_key_syntax CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
    display_name     text NOT NULL DEFAULT ''
                     CONSTRAINT projects_display_name_length CHECK (char_length(display_name) <= 256),
    settings         jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT projects_settings_object CHECK (jsonb_typeof(settings) = 'object'),
    active_wip_limit integer NOT NULL
                     CONSTRAINT projects_wip_limit_range CHECK (active_wip_limit BETWEEN 1 AND 100000),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          bigint NOT NULL DEFAULT 1
                     CONSTRAINT projects_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, key)
);

-- Global identity mapping; not tenant-owned. Runtime visibility is limited to
-- users that hold a membership in the current tenant (policy below), and
-- principal discovery goes through kodama_discover_memberships (T002_003).
CREATE TABLE users (
    id         kodama_id PRIMARY KEY,
    issuer     text NOT NULL CONSTRAINT users_issuer_length CHECK (char_length(issuer) BETWEEN 1 AND 512),
    subject    text NOT NULL CONSTRAINT users_subject_length CHECK (char_length(subject) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT now(),
    version    bigint NOT NULL DEFAULT 1
               CONSTRAINT users_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    UNIQUE (issuer, subject)
);

-- A membership binds a user to one fixed role at tenant scope (project_id IS
-- NULL) or project scope. The composite foreign key rejects a project that
-- belongs to another tenant; MATCH SIMPLE skips it for tenant-wide bindings.
CREATE TABLE memberships (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    id         kodama_id NOT NULL,
    user_id    kodama_id NOT NULL REFERENCES users (id),
    project_id kodama_id,
    role       text NOT NULL
               CONSTRAINT memberships_role_fixed CHECK (role IN ('admin', 'analyst', 'engineer', 'reviewer', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    version    bigint NOT NULL DEFAULT 1
               CONSTRAINT memberships_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT memberships_binding_unique UNIQUE NULLS NOT DISTINCT (tenant_id, user_id, project_id, role)
);

CREATE INDEX memberships_user_idx ON memberships (user_id);

CREATE INDEX memberships_project_idx ON memberships (tenant_id, project_id);

-- Row level security. FORCE applies the policies to the owning migration role
-- too; the owner policies exist for administrative seeding and for the
-- SECURITY DEFINER discovery functions, and the migration role is never used
-- for request handling.
ALTER TABLE tenants     ENABLE ROW LEVEL SECURITY;

ALTER TABLE tenants     FORCE ROW LEVEL SECURITY;

ALTER TABLE projects    ENABLE ROW LEVEL SECURITY;

ALTER TABLE projects    FORCE ROW LEVEL SECURITY;

ALTER TABLE users       ENABLE ROW LEVEL SECURITY;

ALTER TABLE users       FORCE ROW LEVEL SECURITY;

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;

ALTER TABLE memberships FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON tenants     TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON projects    TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON users       TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON memberships TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON tenants
    USING (id = kodama_tenant_id()) WITH CHECK (id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON projects
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_members ON users FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM memberships m
        WHERE m.user_id = users.id AND m.tenant_id = kodama_tenant_id()
    ));

-- ---- audit (was T002_002_audit.sql) ----

-- T-002 audit table: append-oriented, tenant-owned, safe metadata only.
-- The runtime role receives INSERT and SELECT only; there is no UPDATE or
-- DELETE grant and no policy that would permit either.

CREATE TABLE audit (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    id               kodama_id NOT NULL,
    seq              bigint GENERATED ALWAYS AS IDENTITY,
    project_id       kodama_id,
    occurred_at      timestamptz NOT NULL DEFAULT now(),
    actor_type       text NOT NULL
                     CONSTRAINT audit_actor_type_known CHECK (actor_type IN ('user', 'workload', 'system', 'operator')),
    actor_id         text NOT NULL CONSTRAINT audit_actor_id_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    action           text NOT NULL CONSTRAINT audit_action_length CHECK (char_length(action) BETWEEN 1 AND 128),
    resource_type    text NOT NULL CONSTRAINT audit_resource_type_length CHECK (char_length(resource_type) BETWEEN 1 AND 64),
    resource_id      kodama_id NOT NULL,
    resource_version bigint
                     CONSTRAINT audit_resource_version_range CHECK (resource_version BETWEEN 1 AND 9007199254740991),
    reason           text CONSTRAINT audit_reason_length CHECK (char_length(reason) <= 2048),
    request_id       text CONSTRAINT audit_request_id_length CHECK (char_length(request_id) <= 256),
    -- Callers must store only values safe to disclose to every reader of the
    -- audit trail (never secret values or tokens); the database bounds shape
    -- and size only.
    metadata         jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT audit_metadata_object CHECK (jsonb_typeof(metadata) = 'object' AND pg_column_size(metadata) <= 16384),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE UNIQUE INDEX audit_seq_idx ON audit (seq);

CREATE INDEX audit_tenant_time_idx ON audit (tenant_id, occurred_at, seq);

CREATE INDEX audit_resource_idx ON audit (tenant_id, resource_type, resource_id, seq);

ALTER TABLE audit ENABLE ROW LEVEL SECURITY;

ALTER TABLE audit FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_select ON audit FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON audit FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON audit FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON audit FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- discovery (was T002_003_discovery.sql) ----

-- T-002 narrow non-tenant paths. Both functions are SECURITY DEFINER (owned by
-- the migration role, whose owner policies permit the reads) with a pinned
-- search_path. They are the only way the runtime role sees rows outside a
-- tenant context.

-- Principal-filtered membership discovery: returns bindings only for the one
-- (issuer, subject) user supplied by the authenticated caller.
CREATE FUNCTION kodama_discover_memberships(p_issuer text, p_subject text)
    RETURNS TABLE (user_id text, tenant_id text, project_id text, role text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT m.user_id::text, m.tenant_id::text, m.project_id::text, m.role
        FROM public.users u
        JOIN public.memberships m ON m.user_id = u.id
        WHERE u.issuer = p_issuer AND u.subject = p_subject
        ORDER BY m.tenant_id, m.project_id NULLS FIRST, m.role
    $$;

-- Worker tenant enumeration: tenant IDs only, no other column.
CREATE FUNCTION kodama_list_tenant_ids()
    RETURNS SETOF text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT t.id::text FROM public.tenants t ORDER BY t.id $$;

REVOKE ALL ON FUNCTION kodama_discover_memberships(text, text) FROM PUBLIC;

REVOKE ALL ON FUNCTION kodama_list_tenant_ids() FROM PUBLIC;

-- ---- event delivery (was T005_001_delivery.sql) ----

-- T-005 delivery tables: tenant subject tokens, serialized backlog accounting,
-- the outbox with durable attempt history, and the consumer inbox with attempt
-- history. Tenant-owned, forced RLS, minimal grants; see internal/events/README.md.
--
-- Lifetimes are independent: outbox rows, inbox entries, observations (T005_002)
-- and quarantine records are separate tables with no cascading between them
-- other than an attempt history following its own parent row.
--
-- Enforced by the database, not only by Go:
--   * event content (payload, digest, type, ownership) is immutable: the runtime
--     role gets column-level UPDATE on the delivery-state columns only;
--   * only published outbox rows and completed inbox entries can be deleted, so
--     pending publication and unresolved quarantine survive any cleanup bug.

-- Server-assigned, unguessable-by-construction tenant subject tokens. Subjects
-- are built only from these, never from tenant-supplied text.
CREATE TABLE event_tenant_subjects (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    subject_token text NOT NULL UNIQUE
                  CONSTRAINT event_subject_token_syntax CHECK (subject_token ~ '^[a-z0-9]{16,64}$'),
    created_at    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE event_tenant_subjects ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_tenant_subjects FORCE ROW LEVEL SECURITY;

-- Lets the definer function below resolve a presented token across tenants.
CREATE POLICY owner_select ON event_tenant_subjects FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY tenant_select ON event_tenant_subjects FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_tenant_subjects FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- Consumer-side trusted scope resolution: the subject token of a received
-- message maps to exactly one tenant. Returns NULL for an unknown token.
CREATE FUNCTION kodama_event_resolve_subject(p_token text)
    RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT s.tenant_id::text FROM public.event_tenant_subjects s WHERE s.subject_token = p_token $$;

REVOKE ALL ON FUNCTION kodama_event_resolve_subject(text) FROM PUBLIC;

-- One row per tenant, locked by every append. It serializes admission so the
-- count and byte limits cannot be overshot by concurrent writers, and gives a
-- single, consistent lock order: backlog row first, then the scope counter.
CREATE TABLE event_backlog (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    pending_count integer NOT NULL DEFAULT 0 CONSTRAINT event_backlog_count_nonneg CHECK (pending_count >= 0),
    pending_bytes bigint NOT NULL DEFAULT 0 CONSTRAINT event_backlog_bytes_nonneg CHECK (pending_bytes >= 0)
);

ALTER TABLE event_backlog ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_backlog FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_backlog FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_backlog FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_backlog FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE TABLE outbox (
    tenant_id         kodama_id NOT NULL REFERENCES tenants (id),
    event_id          kodama_id NOT NULL,
    project_id        kodama_id,
    event_type        text NOT NULL CONSTRAINT outbox_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    payload           bytea NOT NULL CONSTRAINT outbox_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    payload_digest    text NOT NULL CONSTRAINT outbox_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at        timestamptz NOT NULL DEFAULT now(),
    state             text NOT NULL DEFAULT 'pending'
                      CONSTRAINT outbox_state_known CHECK (state IN ('pending', 'claimed', 'published')),
    attempts          integer NOT NULL DEFAULT 0 CONSTRAINT outbox_attempts_nonneg CHECK (attempts >= 0),
    claim_token       bigint NOT NULL DEFAULT 0 CONSTRAINT outbox_token_nonneg CHECK (claim_token >= 0),
    lease_expires_at  timestamptz,
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    published_at      timestamptz,
    last_failure_class text CONSTRAINT outbox_failure_class_known CHECK (last_failure_class IN ('transient', 'permanent')),
    last_failure_at   timestamptz,
    last_error        text CONSTRAINT outbox_error_length CHECK (char_length(last_error) <= 512),
    PRIMARY KEY (tenant_id, event_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT outbox_state_consistent CHECK (
        (state = 'pending'   AND lease_expires_at IS NULL     AND published_at IS NULL) OR
        (state = 'claimed'   AND lease_expires_at IS NOT NULL AND published_at IS NULL) OR
        (state = 'published' AND lease_expires_at IS NULL     AND published_at IS NOT NULL))
);

CREATE INDEX outbox_due_idx ON outbox (tenant_id, next_attempt_at) WHERE state = 'pending';

CREATE INDEX outbox_lease_idx ON outbox (tenant_id, lease_expires_at) WHERE state = 'claimed';

CREATE INDEX outbox_published_idx ON outbox (tenant_id, published_at) WHERE state = 'published';

CREATE INDEX outbox_unpublished_age_idx ON outbox (tenant_id, created_at) WHERE state <> 'published';

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;

ALTER TABLE outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON outbox FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON outbox FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON outbox FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Only published rows can ever be deleted.
CREATE POLICY tenant_delete_published ON outbox FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'published');

-- Durable publication attempt history; follows its outbox row only.
CREATE TABLE outbox_attempts (
    tenant_id   kodama_id NOT NULL,
    event_id    kodama_id NOT NULL,
    attempt     integer NOT NULL CONSTRAINT outbox_attempts_attempt_positive CHECK (attempt >= 1),
    claim_token bigint NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    outcome     text NOT NULL DEFAULT 'claimed'
                CONSTRAINT outbox_attempts_outcome_known CHECK (outcome IN ('claimed', 'published', 'transient', 'permanent')),
    error       text CONSTRAINT outbox_attempts_error_length CHECK (char_length(error) <= 512),
    PRIMARY KEY (tenant_id, event_id, attempt),
    FOREIGN KEY (tenant_id, event_id) REFERENCES outbox (tenant_id, event_id) ON DELETE CASCADE
);

ALTER TABLE outbox_attempts ENABLE ROW LEVEL SECURITY;

ALTER TABLE outbox_attempts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON outbox_attempts FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON outbox_attempts FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON outbox_attempts FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- The consumer inbox: the correctness boundary against duplicate delivery.
-- Keyed by (tenant, consumer, event). It is not a foreign key to any event
-- table, so publication and observation pruning never erase deduplication.
CREATE TABLE event_inbox (
    tenant_id          kodama_id NOT NULL REFERENCES tenants (id),
    consumer           text NOT NULL CONSTRAINT event_inbox_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    event_id           kodama_id NOT NULL,
    project_id         kodama_id,
    event_type         text NOT NULL CONSTRAINT event_inbox_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    -- Content identity of the delivered event, kept for the life of the entry
    -- (the payload itself is cleared on completion). Append consults it so a
    -- producer retry of an event whose outbox row and observation were pruned
    -- is recognized as the same event, never given a fresh origin while this
    -- evidence still exists.
    payload_digest     text NOT NULL CONSTRAINT event_inbox_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    state              text NOT NULL DEFAULT 'pending'
                       CONSTRAINT event_inbox_state_known CHECK (state IN ('pending', 'completed', 'quarantined')),
    attempts           integer NOT NULL DEFAULT 0 CONSTRAINT event_inbox_attempts_nonneg CHECK (attempts >= 0),
    next_attempt_at    timestamptz,
    first_seen_at      timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    last_failure_class text CONSTRAINT event_inbox_class_known CHECK (last_failure_class IN ('transient', 'permanent')),
    last_error         text CONSTRAINT event_inbox_error_length CHECK (char_length(last_error) <= 512),
    replays            integer NOT NULL DEFAULT 0 CONSTRAINT event_inbox_replays_nonneg CHECK (replays >= 0),
    -- The delivered envelope, kept while the entry is unresolved so repair can
    -- re-drive a retry long after the observation history (a separate, API
    -- oriented lifetime) and the broker copy expired. Cleared on completion.
    payload            bytea CONSTRAINT event_inbox_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    PRIMARY KEY (tenant_id, consumer, event_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_inbox_completed_consistent CHECK ((state = 'completed') = (completed_at IS NOT NULL)),
    CONSTRAINT event_inbox_completed_without_payload CHECK (state <> 'completed' OR payload IS NULL)
);

CREATE INDEX event_inbox_event_idx ON event_inbox (tenant_id, event_id);

CREATE INDEX event_inbox_retry_idx ON event_inbox (tenant_id, consumer, next_attempt_at) WHERE state = 'pending';

CREATE INDEX event_inbox_completed_idx ON event_inbox (tenant_id, completed_at) WHERE state = 'completed';

CREATE INDEX event_inbox_quarantine_idx ON event_inbox (tenant_id, consumer) WHERE state = 'quarantined';

ALTER TABLE event_inbox ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_inbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_inbox FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_inbox FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_inbox FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Only completed entries can ever be deleted (deduplication evidence expiry).
CREATE POLICY tenant_delete_completed ON event_inbox FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'completed');

-- Retry, quarantine and replay history of one inbox entry.
CREATE TABLE event_inbox_attempts (
    seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   kodama_id NOT NULL,
    consumer    text NOT NULL,
    event_id    kodama_id NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    attempt     integer NOT NULL CONSTRAINT event_inbox_attempts_attempt_nonneg CHECK (attempt >= 0),
    outcome     text NOT NULL
                CONSTRAINT event_inbox_attempts_outcome_known CHECK (outcome IN ('completed', 'retry', 'quarantined', 'replay_reopened')),
    class       text CONSTRAINT event_inbox_attempts_class_known CHECK (class IN ('transient', 'permanent')),
    error       text CONSTRAINT event_inbox_attempts_error_length CHECK (char_length(error) <= 512),
    replay_id   kodama_id,
    FOREIGN KEY (tenant_id, consumer, event_id) REFERENCES event_inbox (tenant_id, consumer, event_id) ON DELETE CASCADE
);

CREATE INDEX event_inbox_attempts_entry_idx ON event_inbox_attempts (tenant_id, consumer, event_id, seq);

ALTER TABLE event_inbox_attempts ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_inbox_attempts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_inbox_attempts FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_inbox_attempts FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- Durable deduplication boundary. Pruning completed inbox entries forgets
-- evidence, and no later configuration can restore it; so the boundary of what
-- may have been forgotten is persisted, per tenant, in the transaction that
-- deletes. inbox_before is the latest cutoff (completion time) ever used for
-- forgetting completed entries; broker_before the latest cutoff (publication
-- time) below which a publication no longer protected its event's evidence.
-- Both only move forward, enforced by a trigger, so raising a retention setting
-- (or restarting with different options) can never make forgotten effects
-- admissible again. Consumers, repair and replay admission read it; nothing
-- deletes it.
CREATE TABLE event_dedup_boundary (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    inbox_before  timestamptz NOT NULL,
    broker_before timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION kodama_event_dedup_boundary_monotonic() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.inbox_before < OLD.inbox_before OR NEW.broker_before < OLD.broker_before THEN
        RAISE EXCEPTION 'the deduplication boundary of a tenant only moves forward' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER event_dedup_boundary_monotonic BEFORE UPDATE ON event_dedup_boundary
    FOR EACH ROW EXECUTE FUNCTION kodama_event_dedup_boundary_monotonic();

ALTER TABLE event_dedup_boundary ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_dedup_boundary FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_dedup_boundary FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_dedup_boundary FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_dedup_boundary FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- observation replay (was T005_002_observation_replay.sql) ----

-- T-005 observation history, quarantine, replay and recovery fences.
--
-- Observation history is the retained, per-scope sequence behind API cursors
-- (T-024). Sequences are allocated from a counter row that the appending
-- transaction locks until it commits, so a committed cursor can never skip an
-- earlier uncommitted event and a rolled-back append leaves no gap. Lock order
-- (documented in internal/events/README.md): event_backlog, then event_scopes.

-- One counter per (tenant, project) and one per tenant-wide scope
-- (scope_key = '', project_id NULL). floor_seq is the highest pruned sequence:
-- a cursor below it has expired.
CREATE TABLE event_scopes (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    scope_key  text NOT NULL,
    project_id kodama_id,
    last_seq   bigint NOT NULL DEFAULT 0 CONSTRAINT event_scopes_last_nonneg CHECK (last_seq >= 0),
    floor_seq  bigint NOT NULL DEFAULT 0 CONSTRAINT event_scopes_floor_nonneg CHECK (floor_seq >= 0),
    PRIMARY KEY (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_scopes_key_matches_project CHECK (
        (project_id IS NULL AND scope_key = '') OR (project_id IS NOT NULL AND scope_key = project_id)),
    CONSTRAINT event_scopes_floor_le_last CHECK (floor_seq <= last_seq)
);

ALTER TABLE event_scopes ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_scopes FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_scopes FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_scopes FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_scopes FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE TABLE event_observations (
    tenant_id        kodama_id NOT NULL,
    scope_key        text NOT NULL,
    seq              bigint NOT NULL CONSTRAINT event_observations_seq_positive CHECK (seq >= 1),
    event_id         kodama_id NOT NULL,
    event_type       text NOT NULL CONSTRAINT event_observations_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    project_id       kodama_id,
    resource_type    text NOT NULL CONSTRAINT event_observations_resource_type_length CHECK (char_length(resource_type) BETWEEN 1 AND 64),
    resource_id      kodama_id NOT NULL,
    resource_version bigint NOT NULL CONSTRAINT event_observations_version_range CHECK (resource_version BETWEEN 1 AND 9007199254740991),
    occurred_at      timestamptz NOT NULL,
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    payload          bytea NOT NULL CONSTRAINT event_observations_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    payload_digest   text NOT NULL CONSTRAINT event_observations_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, scope_key, seq),
    UNIQUE (tenant_id, event_id),
    FOREIGN KEY (tenant_id, scope_key) REFERENCES event_scopes (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE INDEX event_observations_recorded_idx ON event_observations (tenant_id, recorded_at);

CREATE INDEX event_observations_resource_idx ON event_observations (tenant_id, resource_type, resource_id, resource_version);

ALTER TABLE event_observations ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_observations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_observations FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_observations FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- Retention deletes expired history; the floor is advanced in the same
-- transaction by the retention operation. No UPDATE exists: history is immutable.
CREATE POLICY tenant_delete ON event_observations FOR DELETE USING (tenant_id = kodama_tenant_id());

-- Poison and unsupported deliveries, kept with bounded diagnostics and the
-- original transport identity until an operator resolves or replays them.
-- The tenant comes from the trusted subscription, never from the payload.
CREATE TABLE event_quarantine (
    tenant_id         kodama_id NOT NULL REFERENCES tenants (id),
    id                kodama_id NOT NULL,
    consumer          text NOT NULL CONSTRAINT event_quarantine_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    -- The event ID when the envelope parsed, otherwise raw:<sha256 of payload>;
    -- a delivery whose content contradicts an existing inbox entry is keyed by
    -- its payload hash too, so it never overwrites the original's record.
    delivery_key      text NOT NULL CONSTRAINT event_quarantine_key_length CHECK (char_length(delivery_key) BETWEEN 1 AND 200),
    event_id          text CONSTRAINT event_quarantine_event_id_length CHECK (char_length(event_id) <= 128),
    event_type        text CONSTRAINT event_quarantine_type_length CHECK (char_length(event_type) <= 128),
    reason_code       text NOT NULL
                      CONSTRAINT event_quarantine_reason_known CHECK (reason_code IN
                          ('unsupported_version', 'unsupported_type', 'malformed', 'too_large', 'scope_mismatch', 'permanent_failure', 'retries_exhausted', 'beyond_horizon', 'conflicting_content')),
    diagnostic        text NOT NULL DEFAULT '' CONSTRAINT event_quarantine_diagnostic_length CHECK (char_length(diagnostic) <= 1024),
    stream            text CONSTRAINT event_quarantine_stream_length CHECK (char_length(stream) <= 256),
    stream_seq        bigint,
    subject           text CONSTRAINT event_quarantine_subject_length CHECK (char_length(subject) <= 512),
    delivery_count    integer NOT NULL DEFAULT 0,
    payload           bytea NOT NULL CONSTRAINT event_quarantine_payload_size CHECK (octet_length(payload) <= 65536),
    payload_truncated boolean NOT NULL DEFAULT false,
    state             text NOT NULL DEFAULT 'quarantined'
                      CONSTRAINT event_quarantine_state_known CHECK (state IN ('quarantined', 'resolved')),
    quarantined_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz,
    resolution        text CONSTRAINT event_quarantine_resolution_known CHECK (resolution IN ('replayed', 'discarded')),
    resolved_by       text CONSTRAINT event_quarantine_resolved_by_length CHECK (char_length(resolved_by) <= 256),
    -- Increments each time a resolved entry is quarantined again, so every
    -- resolution has its own audit identity.
    cycle             integer NOT NULL DEFAULT 1 CONSTRAINT event_quarantine_cycle_positive CHECK (cycle >= 1),
    -- The audited replay that most recently reopened this quarantine cycle and
    -- its authorized operator. A replayed event may fail transiently and be
    -- completed later by a scheduled retry, repair or ordinary delivery, none
    -- of which carries a replay; this provenance lets that completion resolve
    -- the record for the operator who authorized the replay.
    replay_id         kodama_id,
    replay_by         text CONSTRAINT event_quarantine_replay_by_length CHECK (char_length(replay_by) BETWEEN 1 AND 256),
    CONSTRAINT event_quarantine_replay_pair CHECK ((replay_id IS NULL) = (replay_by IS NULL)),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, consumer, delivery_key),
    CONSTRAINT event_quarantine_resolved_consistent CHECK ((state = 'resolved') = (resolved_at IS NOT NULL))
);

CREATE INDEX event_quarantine_open_idx ON event_quarantine (tenant_id, consumer, quarantined_at) WHERE state = 'quarantined';

ALTER TABLE event_quarantine ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_quarantine FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_quarantine FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_quarantine FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_quarantine FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Only resolved records can ever be deleted.
CREATE POLICY tenant_delete_resolved ON event_quarantine FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'resolved');

-- Durable replay requests. A request is written with its audit row in one
-- transaction and resumed idempotently: progress is the next unprocessed
-- sequence, the work itself is deduplicated by the inbox.
CREATE TABLE event_replay_requests (
    tenant_id            kodama_id NOT NULL REFERENCES tenants (id),
    id                   kodama_id NOT NULL,
    consumer             text NOT NULL CONSTRAINT event_replay_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    scope_key            text NOT NULL,
    project_id           kodama_id,
    from_seq             bigint NOT NULL CONSTRAINT event_replay_from_positive CHECK (from_seq >= 1),
    to_seq               bigint NOT NULL,
    reason               text NOT NULL CONSTRAINT event_replay_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    operator_id          text NOT NULL CONSTRAINT event_replay_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    allow_beyond_horizon boolean NOT NULL DEFAULT false,
    -- Events recorded before this instant without inbox evidence were beyond the
    -- deduplication horizon when the request was admitted (the durable boundary
    -- or the inbox retention, whichever is later). Fixed at admission.
    evidence_floor       timestamptz NOT NULL,
    effect_class         text NOT NULL CONSTRAINT event_replay_effect_known CHECK (effect_class IN ('local', 'external_intent')),
    classification       jsonb NOT NULL CONSTRAINT event_replay_classification_object CHECK (jsonb_typeof(classification) = 'object'),
    state                text NOT NULL DEFAULT 'requested'
                         CONSTRAINT event_replay_state_known CHECK (state IN ('requested', 'running', 'completed')),
    next_seq             bigint NOT NULL,
    processed            integer NOT NULL DEFAULT 0,
    skipped              integer NOT NULL DEFAULT 0,
    failed               integer NOT NULL DEFAULT 0,
    claim_token          bigint NOT NULL DEFAULT 0,
    lease_expires_at     timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, scope_key) REFERENCES event_scopes (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_replay_range CHECK (to_seq >= from_seq),
    CONSTRAINT event_replay_progress_in_range CHECK (next_seq BETWEEN from_seq AND to_seq + 1),
    CONSTRAINT event_replay_completed_consistent CHECK ((state = 'completed') = (completed_at IS NOT NULL))
);

CREATE INDEX event_replay_active_idx ON event_replay_requests (tenant_id, created_at) WHERE state <> 'completed';

ALTER TABLE event_replay_requests ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_replay_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_replay_requests FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_replay_requests FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_replay_requests FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Only completed requests can ever be deleted.
CREATE POLICY tenant_delete_completed ON event_replay_requests FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'completed');

-- Restore fence: after a database restore an operator fences each consumer;
-- replay for it is refused until its owner confirms that external effects were
-- reconciled and old controllers fenced (event catalog, "Retention and recovery").
CREATE TABLE event_recovery_fences (
    tenant_id    kodama_id NOT NULL REFERENCES tenants (id),
    consumer     text NOT NULL CONSTRAINT event_fence_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    fenced_at    timestamptz NOT NULL DEFAULT now(),
    fenced_by    text NOT NULL CONSTRAINT event_fence_by_length CHECK (char_length(fenced_by) BETWEEN 1 AND 256),
    reason       text NOT NULL CONSTRAINT event_fence_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    cleared_at   timestamptz,
    cleared_by   text CONSTRAINT event_fence_cleared_by_length CHECK (char_length(cleared_by) <= 256),
    clear_reason text CONSTRAINT event_fence_clear_reason_length CHECK (char_length(clear_reason) <= 512),
    PRIMARY KEY (tenant_id, consumer)
);

ALTER TABLE event_recovery_fences ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_recovery_fences FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_select ON event_recovery_fences FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON event_recovery_fences FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON event_recovery_fences FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- HTTP idempotency (was T004_001_idempotency.sql) ----

-- T-004 idempotency records (docs/implementation/database-schema.md).
--
-- A row exists only for a completed, committed response: the adapter claims a
-- key with a transaction-scoped advisory lock, runs the business callback and
-- inserts this row in the same transaction. An unfinished claim therefore never
-- reaches disk, and a rollback leaves the key absent.
--
-- The unique key is exactly (tenant, principal, method, canonical route, key).
-- request_hash and project_id are deliberately NOT part of it: the hash is
-- compared on a match so a changed request conflicts instead of being accepted
-- as new, and the concrete canonical route already separates projects.

CREATE TABLE idempotency (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    principal        text NOT NULL
                     CONSTRAINT idempotency_principal_length CHECK (char_length(principal) BETWEEN 1 AND 512),
    method           text NOT NULL
                     CONSTRAINT idempotency_method CHECK (method IN ('POST', 'PUT', 'PATCH', 'DELETE')),
    canonical_route  text NOT NULL
                     CONSTRAINT idempotency_route_length CHECK (char_length(canonical_route) BETWEEN 1 AND 1024),
    key              text NOT NULL
                     CONSTRAINT idempotency_key_syntax CHECK (key ~ '^[A-Za-z0-9._~:-]{1,128}$'),
    project_id       kodama_id,
    request_hash     bytea NOT NULL
                     CONSTRAINT idempotency_hash_length CHECK (octet_length(request_hash) = 32),
    response_status  smallint NOT NULL
                     CONSTRAINT idempotency_status_range CHECK (response_status BETWEEN 200 AND 299),
    response_headers jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT idempotency_headers_object CHECK (jsonb_typeof(response_headers) = 'object' AND octet_length(response_headers::text) <= 8192),
    response_body    bytea NOT NULL
                     CONSTRAINT idempotency_body_size CHECK (octet_length(response_body) <= 1048576),
    target_kind      text,
    target_id        text,
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, principal, method, canonical_route, key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT idempotency_target_pair CHECK ((target_kind IS NULL) = (target_id IS NULL)),
    CONSTRAINT idempotency_target_length CHECK (char_length(target_kind) <= 64 AND char_length(target_id) <= 128),
    CONSTRAINT idempotency_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX idempotency_expiry_idx ON idempotency (tenant_id, expires_at);

ALTER TABLE idempotency ENABLE ROW LEVEL SECURITY;

ALTER TABLE idempotency FORCE ROW LEVEL SECURITY;

-- Administrative seeding and operator purges run as the owner; the owner role
-- never serves requests.
CREATE POLICY owner_all ON idempotency TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON idempotency
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- identity authority (was T010_001_identity_authority.sql) ----

-- T-010 standalone identity authority.
--
-- Reuses T-002's users, tenants, projects, memberships and audit tables and
-- adds only:
--   * identity_authz_revisions: one row per (tenant, user) whose revision
--     increases on every binding change and is retained after the last binding
--     is removed. It is the lock every permission-sensitive transaction takes,
--     so a revocation and a protected mutation serialize, and the source of
--     membership.changed.effectiveVersion.
--   * identity_bootstrap: the durable, operator-only completion record of the
--     one-time bootstrap. The runtime role has no privilege on it.
--   * three SECURITY DEFINER functions with a pinned search_path that give the
--     runtime role the narrow, principal-filtered global reads it needs.

CREATE TABLE identity_authz_revisions (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    user_id    kodama_id NOT NULL REFERENCES users (id),
    revision   bigint NOT NULL DEFAULT 0
               CONSTRAINT identity_authz_revision_range CHECK (revision BETWEEN 0 AND 9007199254740991),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE identity_authz_revisions ENABLE ROW LEVEL SECURITY;

ALTER TABLE identity_authz_revisions FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON identity_authz_revisions TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON identity_authz_revisions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- At most one row, ever: revoking the initial administrator later does not
-- delete it, so bootstrap cannot be re-enabled.
CREATE TABLE identity_bootstrap (
    singleton     boolean PRIMARY KEY DEFAULT true CONSTRAINT identity_bootstrap_singleton CHECK (singleton),
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    user_id       kodama_id NOT NULL REFERENCES users (id),
    binding_id    kodama_id NOT NULL,
    params_digest text NOT NULL CONSTRAINT identity_bootstrap_digest_length CHECK (char_length(params_digest) = 64),
    operator_id   text NOT NULL CONSTRAINT identity_bootstrap_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    completed_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE identity_bootstrap ENABLE ROW LEVEL SECURITY;

ALTER TABLE identity_bootstrap FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON identity_bootstrap TO CURRENT_USER USING (true) WITH CHECK (true);

-- Deliberately no runtime grant and no tenant policy: bootstrap is operator-only.

-- Maps a verified (issuer, subject) to its stable user, creating the user on
-- first login. It never creates a membership. Concurrent first logins converge
-- on one row through the unique (issuer, subject) constraint.
CREATE FUNCTION kodama_identity_map_principal(p_issuer text, p_subject text)
    RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_id text;
BEGIN
    IF p_issuer IS NULL OR p_subject IS NULL
       OR char_length(p_issuer) NOT BETWEEN 1 AND 512
       OR char_length(p_subject) NOT BETWEEN 1 AND 512 THEN
        RAISE EXCEPTION 'invalid principal' USING ERRCODE = '22023';
    END IF;
    SELECT u.id::text INTO v_id FROM public.users u WHERE u.issuer = p_issuer AND u.subject = p_subject;
    IF v_id IS NOT NULL THEN
        RETURN v_id;
    END IF;
    INSERT INTO public.users (id, issuer, subject)
    VALUES ('u_' || replace(gen_random_uuid()::text, '-', ''), p_issuer, p_subject)
    ON CONFLICT (issuer, subject) DO NOTHING
    RETURNING id::text INTO v_id;
    IF v_id IS NULL THEN
        SELECT u.id::text INTO v_id FROM public.users u WHERE u.issuer = p_issuer AND u.subject = p_subject;
    END IF;
    RETURN v_id;
END
$$;

-- The caller's own record, selected by the authenticated user ID.
CREATE FUNCTION kodama_identity_user(p_user text)
    RETURNS TABLE (id text, issuer text, subject text, created_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT u.id::text, u.issuer, u.subject, u.created_at
        FROM public.users u
        WHERE u.id = p_user
    $$;

-- Tenants where the user holds at least one binding, in ID order, after the given
-- ID. Only the authenticated user's own tenants are visible through it.
CREATE FUNCTION kodama_identity_tenants(p_user text, p_after text, p_limit integer)
    RETURNS TABLE (tenant_id text, key text, display_name text, roles text[])
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT t.id::text, t.key, t.display_name,
               COALESCE(ARRAY(
                   SELECT DISTINCT m.role FROM public.memberships m
                   WHERE m.tenant_id = t.id AND m.user_id = p_user AND m.project_id IS NULL
                   ORDER BY 1), ARRAY[]::text[])
        FROM public.tenants t
        WHERE t.id::text COLLATE "C" > p_after COLLATE "C"
          AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.tenant_id = t.id AND m.user_id = p_user)
        ORDER BY t.id::text COLLATE "C"
        LIMIT least(greatest(p_limit, 1), 1001)
    $$;

REVOKE ALL ON FUNCTION kodama_identity_map_principal(text, text) FROM PUBLIC;

REVOKE ALL ON FUNCTION kodama_identity_user(text) FROM PUBLIC;

REVOKE ALL ON FUNCTION kodama_identity_tenants(text, text, integer) FROM PUBLIC;

-- ---- work items (was T011_001_work.sql) ----

-- T-011 work-management schema: work items and their immutable history.
--
-- Follows the base conventions (T002_001): opaque kodama_id keys, tenant and
-- project ownership through composite foreign keys, ENABLE + FORCE row level
-- security on kodama_tenant_id(), and minimal runtime grants. Other modules
-- (workspaces, runs, artifacts) contribute their own cross-module foreign keys
-- to work_items from their migrations; this migration creates no project,
-- workspace, run, artifact, audit or outbox table.

CREATE TABLE work_items (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    kind             text NOT NULL
                     CONSTRAINT work_items_kind_known CHECK (kind IN ('epic', 'story', 'task', 'defect')),
    title            text NOT NULL
                     CONSTRAINT work_items_title_length CHECK (char_length(title) BETWEEN 1 AND 200),
    spec             text NOT NULL DEFAULT ''
                     CONSTRAINT work_items_spec_length CHECK (octet_length(spec) <= 65536),
    acceptance_criteria jsonb NOT NULL DEFAULT '[]'
                     CONSTRAINT work_items_criteria_bounds CHECK (
                         jsonb_typeof(acceptance_criteria) = 'array'
                         AND jsonb_array_length(acceptance_criteria) <= 100
                         AND pg_column_size(acceptance_criteria) <= 262144),
    evidence_links   jsonb NOT NULL DEFAULT '[]'
                     CONSTRAINT work_items_evidence_bounds CHECK (
                         jsonb_typeof(evidence_links) = 'array'
                         AND jsonb_array_length(evidence_links) <= 50
                         AND pg_column_size(evidence_links) <= 65536),
    -- Users are validated against current scoped membership inside the
    -- mutating transaction; a global user row is not project membership, so
    -- there is deliberately no foreign key to users.
    assignee_id      kodama_id,
    parent_id        kodama_id,
    state            text NOT NULL DEFAULT 'backlog'
                     CONSTRAINT work_items_state_known CHECK (state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled')),
    blocked_reason   text
                     CONSTRAINT work_items_blocked_reason_length CHECK (char_length(blocked_reason) BETWEEN 1 AND 2048),
    version          bigint NOT NULL DEFAULT 1
                     CONSTRAINT work_items_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    content_revision bigint NOT NULL DEFAULT 1
                     CONSTRAINT work_items_content_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    -- Review subject while in review (and kept while done); completion record
    -- while done. History keeps every earlier snapshot.
    review_subject   jsonb
                     CONSTRAINT work_items_review_subject_bounds CHECK (jsonb_typeof(review_subject) = 'object' AND pg_column_size(review_subject) <= 65536),
    completion       jsonb
                     CONSTRAINT work_items_completion_bounds CHECK (jsonb_typeof(completion) = 'object' AND pg_column_size(completion) <= 65536),
    created_by_type  text NOT NULL
                     CONSTRAINT work_items_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id    text NOT NULL
                     CONSTRAINT work_items_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    -- Referenced by children, history and by other modules that must prove the
    -- same project owns the item.
    CONSTRAINT work_items_scope_key UNIQUE (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, parent_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_items_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT work_items_review_state CHECK (state <> 'review' OR review_subject IS NOT NULL),
    CONSTRAINT work_items_completion_state CHECK ((state = 'done') = (completion IS NOT NULL))
);

-- Default list order and keyset pagination.
CREATE INDEX work_items_list_idx ON work_items (tenant_id, project_id, created_at, id);

CREATE INDEX work_items_state_idx ON work_items (tenant_id, project_id, state, created_at, id);

CREATE INDEX work_items_parent_idx ON work_items (tenant_id, project_id, parent_id) WHERE parent_id IS NOT NULL;

CREATE INDEX work_items_assignee_idx ON work_items (tenant_id, project_id, assignee_id) WHERE assignee_id IS NOT NULL;

-- WIP admission counts active items of one project.
CREATE INDEX work_items_active_idx ON work_items (tenant_id, project_id) WHERE state = 'active';

-- Cancellation reconciliation scans cancelled items by key.
CREATE INDEX work_items_cancelled_idx ON work_items (tenant_id, project_id, id) WHERE state = 'cancelled';

CREATE TABLE work_item_history (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    work_item_id     kodama_id NOT NULL,
    -- The resource version this mutation produced; history is ordered by it.
    version          bigint NOT NULL
                     CONSTRAINT work_item_history_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    content_revision bigint NOT NULL
                     CONSTRAINT work_item_history_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    change_kind      text NOT NULL
                     CONSTRAINT work_item_history_kind_known CHECK (change_kind IN ('created', 'updated', 'transitioned')),
    actor_type       text NOT NULL
                     CONSTRAINT work_item_history_actor_type_known CHECK (actor_type IN ('user', 'service', 'workload', 'operator')),
    actor_id         text NOT NULL
                     CONSTRAINT work_item_history_actor_id_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    occurred_at      timestamptz NOT NULL,
    reason           text
                     CONSTRAINT work_item_history_reason_length CHECK (char_length(reason) <= 2048),
    changed_fields   text[] NOT NULL DEFAULT '{}',
    from_state       text,
    to_state         text,
    before_values    jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT work_item_history_before_bounds CHECK (jsonb_typeof(before_values) = 'object' AND pg_column_size(before_values) <= 524288),
    after_values     jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT work_item_history_after_bounds CHECK (jsonb_typeof(after_values) = 'object' AND pg_column_size(after_values) <= 524288),
    review_subject   jsonb
                     CONSTRAINT work_item_history_review_bounds CHECK (jsonb_typeof(review_subject) = 'object' AND pg_column_size(review_subject) <= 65536),
    completion       jsonb
                     CONSTRAINT work_item_history_completion_bounds CHECK (jsonb_typeof(completion) = 'object' AND pg_column_size(completion) <= 65536),
    request_id       text
                     CONSTRAINT work_item_history_request_id_length CHECK (char_length(request_id) <= 256),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT work_item_history_version_key UNIQUE (tenant_id, work_item_id, version),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_item_history_states_known CHECK (
        (from_state IS NULL OR from_state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled'))
        AND (to_state IS NULL OR to_state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled')))
);

ALTER TABLE work_items        ENABLE ROW LEVEL SECURITY;

ALTER TABLE work_items        FORCE ROW LEVEL SECURITY;

ALTER TABLE work_item_history ENABLE ROW LEVEL SECURITY;

ALTER TABLE work_item_history FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON work_items TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON work_items
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- History is append-only: no UPDATE or DELETE policy exists for any role, and
-- the runtime role has no such grant.
CREATE POLICY owner_select ON work_item_history FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON work_item_history FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON work_item_history FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON work_item_history FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- variables and secrets (was T012_001_configuration.sql) ----

-- T-012 configuration schema: project variables, encrypted typed secrets and the
-- tenant scope keys that protect them (docs/specs/variables-and-secrets.md).
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration and restore, and
-- minimal runtime grants (column-level UPDATE where only some columns change).
--
-- Secret values are never stored here in plaintext: secret_versions holds the
-- AES-256-GCM ciphertext, nonce, format and algorithm identifiers, and the scope
-- key identity; secret_scope_key_wraps holds the scope key wrapped by a KEK
-- together with the immutable KEK reference. No plaintext-derived fingerprint is
-- stored anywhere.

-- ---- variables --------------------------------------------------------------

CREATE TABLE variables (
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    project_id    kodama_id NOT NULL,
    id            kodama_id NOT NULL,
    name          text NOT NULL
                  CONSTRAINT variables_name_syntax CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    type          text NOT NULL
                  CONSTRAINT variables_type_known CHECK (type IN ('string', 'boolean', 'integer')),
    description   text NOT NULL DEFAULT ''
                  CONSTRAINT variables_description_length CHECK (octet_length(description) <= 1024),
    value_text    text NOT NULL
                  CONSTRAINT variables_value_length CHECK (octet_length(value_text) <= 4096),
    value_version bigint NOT NULL
                  CONSTRAINT variables_value_version_range CHECK (value_version BETWEEN 1 AND 9007199254740991),
    version       bigint NOT NULL
                  CONSTRAINT variables_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    deleted_at    timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    -- Deleted rows keep their name: a pinned reference never changes meaning.
    CONSTRAINT variables_name_unique UNIQUE (tenant_id, project_id, name),
    CONSTRAINT variables_value_version_le_version CHECK (value_version <= version),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE INDEX variables_project_name_idx ON variables (tenant_id, project_id, name) WHERE deleted_at IS NULL;

-- Immutable value history: one row per value version.
CREATE TABLE variable_versions (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    variable_id kodama_id NOT NULL,
    version     bigint NOT NULL
                CONSTRAINT variable_versions_range CHECK (version BETWEEN 1 AND 9007199254740991),
    type        text NOT NULL,
    value_text  text NOT NULL
                CONSTRAINT variable_versions_value_length CHECK (octet_length(value_text) <= 4096),
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, variable_id, version),
    FOREIGN KEY (tenant_id, project_id, variable_id) REFERENCES variables (tenant_id, project_id, id)
);

CREATE FUNCTION t012_variables_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.name <> OLD.name OR NEW.type <> OLD.type OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'variable identity, name and type are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL THEN
        RAISE EXCEPTION 'a deleted variable cannot change' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'variable resource version must increase' USING ERRCODE = '23514';
    END IF;
    IF (NEW.value_text IS DISTINCT FROM OLD.value_text) <> (NEW.value_version = OLD.value_version + 1)
       OR NEW.value_version NOT IN (OLD.value_version, OLD.value_version + 1) THEN
        RAISE EXCEPTION 'a changed value requires exactly the next value version' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER variables_guard BEFORE UPDATE ON variables
    FOR EACH ROW EXECUTE FUNCTION t012_variables_guard();

-- ---- tenant scope keys ------------------------------------------------------

-- One random 32-byte key per tenant generation. The identity and generation never
-- change; only the wrapping revision advances, on rewrap.
CREATE TABLE secret_scope_keys (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    id               kodama_id NOT NULL,
    generation       integer NOT NULL
                     CONSTRAINT secret_scope_keys_generation_range CHECK (generation >= 1),
    current_revision bigint NOT NULL
                     CONSTRAINT secret_scope_keys_revision_range CHECK (current_revision BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    -- Serializes first creation: the loser of a race finds this key committed.
    CONSTRAINT secret_scope_keys_generation_unique UNIQUE (tenant_id, generation),
    UNIQUE (tenant_id, id, generation)
);

-- Every wrap ever made is retained, so restoring an older backup only needs the
-- KEK versions its wraps name.
CREATE TABLE secret_scope_key_wraps (
    tenant_id      kodama_id NOT NULL,
    key_id         kodama_id NOT NULL,
    revision       bigint NOT NULL
                   CONSTRAINT secret_scope_key_wraps_revision_range CHECK (revision BETWEEN 1 AND 9007199254740991),
    wrapped_key    bytea NOT NULL
                   CONSTRAINT secret_scope_key_wraps_size CHECK (octet_length(wrapped_key) BETWEEN 1 AND 4096),
    wrap_algorithm text NOT NULL
                   CONSTRAINT secret_scope_key_wraps_algorithm_length CHECK (char_length(wrap_algorithm) BETWEEN 1 AND 128),
    kek_provider   text NOT NULL CONSTRAINT secret_scope_key_wraps_provider_length CHECK (char_length(kek_provider) BETWEEN 1 AND 256),
    kek_id         text NOT NULL CONSTRAINT secret_scope_key_wraps_kek_id_length CHECK (char_length(kek_id) BETWEEN 1 AND 256),
    kek_version    text NOT NULL CONSTRAINT secret_scope_key_wraps_kek_version_length CHECK (char_length(kek_version) BETWEEN 1 AND 256),
    wrapped_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, key_id, revision),
    FOREIGN KEY (tenant_id, key_id) REFERENCES secret_scope_keys (tenant_id, id)
);

-- The current revision must exist; deferred so a key and its first wrap are
-- inserted in either order within one transaction.
ALTER TABLE secret_scope_keys
    ADD CONSTRAINT secret_scope_keys_current_wrap_fk
    FOREIGN KEY (tenant_id, id, current_revision) REFERENCES secret_scope_key_wraps (tenant_id, key_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION t012_scope_keys_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.id <> OLD.id OR NEW.generation <> OLD.generation
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'scope key identity and generation are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_revision <> OLD.current_revision + 1 THEN
        RAISE EXCEPTION 'the wrapping revision advances by exactly one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER secret_scope_keys_guard BEFORE UPDATE ON secret_scope_keys
    FOR EACH ROW EXECUTE FUNCTION t012_scope_keys_guard();

-- ---- secrets ----------------------------------------------------------------

CREATE TABLE secrets (
    tenant_id       kodama_id NOT NULL REFERENCES tenants (id),
    project_id      kodama_id NOT NULL,
    id              kodama_id NOT NULL,
    name            text NOT NULL
                    CONSTRAINT secrets_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    kind            text NOT NULL
                    CONSTRAINT secrets_kind_known CHECK (kind IN ('generic', 'git', 'model', 'mcp', 'kubeconfig')),
    current_version bigint NOT NULL
                    CONSTRAINT secrets_current_version_range CHECK (current_version BETWEEN 1 AND 9007199254740991),
    -- Management resource version (the ETag); distinct from value versions and
    -- from scope key wrapping revisions.
    version         bigint NOT NULL
                    CONSTRAINT secrets_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    deleted_at      timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    -- Deleted names stay reserved.
    CONSTRAINT secrets_name_unique UNIQUE (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE INDEX secrets_project_name_idx ON secrets (tenant_id, project_id, name) WHERE deleted_at IS NULL;

CREATE TABLE secret_versions (
    tenant_id      kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    secret_id      kodama_id NOT NULL,
    version        bigint NOT NULL
                   CONSTRAINT secret_versions_range CHECK (version BETWEEN 1 AND 9007199254740991),
    scope_key_id   kodama_id NOT NULL,
    key_generation integer NOT NULL,
    format_version smallint NOT NULL
                   CONSTRAINT secret_versions_format_known CHECK (format_version = 1),
    algorithm      text NOT NULL
                   CONSTRAINT secret_versions_algorithm_known CHECK (algorithm = 'aes-256-gcm/hkdf-sha256'),
    nonce          bytea NOT NULL
                   CONSTRAINT secret_versions_nonce_size CHECK (octet_length(nonce) = 12),
    ciphertext     bytea NOT NULL
                   CONSTRAINT secret_versions_ciphertext_size CHECK (octet_length(ciphertext) BETWEEN 16 AND 131088),
    created_at     timestamptz NOT NULL,
    created_by     text NOT NULL
                   CONSTRAINT secret_versions_created_by_length CHECK (char_length(created_by) BETWEEN 1 AND 256),
    -- One-way revocation: once set it never changes.
    revoked_at     timestamptz,
    revoked_by     text CONSTRAINT secret_versions_revoked_by_length CHECK (char_length(revoked_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, secret_id, version),
    UNIQUE (tenant_id, project_id, secret_id, version),
    CONSTRAINT secret_versions_revocation_pair CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    FOREIGN KEY (tenant_id, project_id, secret_id) REFERENCES secrets (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, scope_key_id, key_generation) REFERENCES secret_scope_keys (tenant_id, id, generation)
);

-- A secret always has its current value version; deferred so the secret and its
-- first version are inserted together.
ALTER TABLE secrets
    ADD CONSTRAINT secrets_current_version_fk
    FOREIGN KEY (tenant_id, id, current_version) REFERENCES secret_versions (tenant_id, secret_id, version)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION t012_secrets_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.name <> OLD.name OR NEW.kind <> OLD.kind OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'secret identity, name and kind are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
        RAISE EXCEPTION 'secret deletion is irreversible' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW.current_version <> OLD.current_version THEN
        RAISE EXCEPTION 'a deleted secret takes no new value versions' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'secret resource version must increase' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_version NOT IN (OLD.current_version, OLD.current_version + 1) THEN
        RAISE EXCEPTION 'value versions are monotonic and never skip' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER secrets_guard BEFORE UPDATE ON secrets
    FOR EACH ROW EXECUTE FUNCTION t012_secrets_guard();

CREATE FUNCTION t012_secret_versions_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'secret versions are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.secret_id <> OLD.secret_id
       OR NEW.version <> OLD.version OR NEW.scope_key_id <> OLD.scope_key_id OR NEW.key_generation <> OLD.key_generation
       OR NEW.format_version <> OLD.format_version OR NEW.algorithm <> OLD.algorithm
       OR NEW.nonce <> OLD.nonce OR NEW.ciphertext <> OLD.ciphertext
       OR NEW.created_at <> OLD.created_at OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'secret value versions are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL
       AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.revoked_by IS DISTINCT FROM OLD.revoked_by) THEN
        RAISE EXCEPTION 'revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER secret_versions_guard BEFORE UPDATE OR DELETE ON secret_versions
    FOR EACH ROW EXECUTE FUNCTION t012_secret_versions_guard();

-- ---- row level security -----------------------------------------------------

ALTER TABLE variables             ENABLE ROW LEVEL SECURITY;

ALTER TABLE variables             FORCE ROW LEVEL SECURITY;

ALTER TABLE variable_versions     ENABLE ROW LEVEL SECURITY;

ALTER TABLE variable_versions     FORCE ROW LEVEL SECURITY;

ALTER TABLE secret_scope_keys     ENABLE ROW LEVEL SECURITY;

ALTER TABLE secret_scope_keys     FORCE ROW LEVEL SECURITY;

ALTER TABLE secret_scope_key_wraps ENABLE ROW LEVEL SECURITY;

ALTER TABLE secret_scope_key_wraps FORCE ROW LEVEL SECURITY;

ALTER TABLE secrets               ENABLE ROW LEVEL SECURITY;

ALTER TABLE secrets               FORCE ROW LEVEL SECURITY;

ALTER TABLE secret_versions       ENABLE ROW LEVEL SECURITY;

ALTER TABLE secret_versions       FORCE ROW LEVEL SECURITY;

-- The owner policies serve administrative seeding and restore; the owner role
-- never serves requests.
CREATE POLICY owner_all ON variables              TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON variable_versions      TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON secret_scope_keys      TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON secret_scope_key_wraps TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON secrets                TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON secret_versions        TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON variables
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON variable_versions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON secret_scope_keys
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON secret_scope_key_wraps
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON secrets
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON secret_versions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- secret references (was T012_002_references.sql) ----

-- T-012 retained references: run pins of exact secret versions and the durable
-- consumer-handling records that revocation creates.
--
-- consumer_id names a pipeline run. The execution tables do not exist yet, so
-- there is no foreign key to them: T-020's migration adds the ownership
-- constraint (tenant_id, project_id, consumer_id) -> runs(tenant_id, project_id,
-- id) when it creates the run tables. Until then ownership is enforced by the
-- composite foreign key to the pinned secret version and by the transaction's
-- tenant context.

CREATE TABLE secret_pins (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    consumer_kind    text NOT NULL
                     CONSTRAINT secret_pins_consumer_kind_known CHECK (consumer_kind = 'run'),
    consumer_id      kodama_id NOT NULL,
    secret_id        kodama_id NOT NULL,
    secret_version   bigint NOT NULL,
    created_at       timestamptz NOT NULL,
    -- Set by the consumer's own transaction when it terminates (completion,
    -- cancellation or expiry). Ordered against resolution by the row lock.
    released_at      timestamptz,
    -- First and latest delivery of the value to this consumer, for revocation
    -- accounting. They record that a delivery happened, never its content.
    first_resolved_at timestamptz,
    last_resolved_at  timestamptz,
    resolution_count  integer NOT NULL DEFAULT 0
                      CONSTRAINT secret_pins_resolution_count_range CHECK (resolution_count >= 0),
    PRIMARY KEY (tenant_id, consumer_kind, consumer_id, secret_id, secret_version),
    FOREIGN KEY (tenant_id, project_id, secret_id, secret_version)
        REFERENCES secret_versions (tenant_id, project_id, secret_id, version)
);

CREATE INDEX secret_pins_secret_idx ON secret_pins (tenant_id, secret_id, secret_version);

CREATE INDEX secret_pins_consumer_idx ON secret_pins (tenant_id, consumer_kind, consumer_id);

CREATE FUNCTION t012_secret_pins_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'secret pins are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.consumer_kind <> OLD.consumer_kind
       OR NEW.consumer_id <> OLD.consumer_id OR NEW.secret_id <> OLD.secret_id
       OR NEW.secret_version <> OLD.secret_version OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a secret pin is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'a released pin stays released' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER secret_pins_guard BEFORE UPDATE OR DELETE ON secret_pins
    FOR EACH ROW EXECUTE FUNCTION t012_secret_pins_guard();

-- One owed external effect per (consumer, version, effect). pending until the
-- owning procedure acknowledges it; an acknowledgement is never rewritten.
CREATE TABLE secret_consumer_handling (
    tenant_id      kodama_id NOT NULL,
    id             kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    secret_id      kodama_id NOT NULL,
    secret_version bigint NOT NULL,
    consumer_kind  text NOT NULL,
    consumer_id    kodama_id NOT NULL,
    effect         text NOT NULL
                   CONSTRAINT secret_consumer_handling_effect_known CHECK (effect IN ('terminate_consumer', 'rotate_issued_credential')),
    state          text NOT NULL
                   CONSTRAINT secret_consumer_handling_state_known CHECK (state IN ('pending', 'completed', 'not_applicable')),
    delivered      boolean NOT NULL,
    created_at     timestamptz NOT NULL,
    resolved_at    timestamptz,
    resolved_by    text CONSTRAINT secret_consumer_handling_resolved_by_length CHECK (char_length(resolved_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT secret_consumer_handling_unique UNIQUE (tenant_id, secret_id, secret_version, consumer_kind, consumer_id, effect),
    CONSTRAINT secret_consumer_handling_resolution CHECK (
        (state = 'pending' AND resolved_at IS NULL AND resolved_by IS NULL)
        OR (state <> 'pending' AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL)),
    FOREIGN KEY (tenant_id, consumer_kind, consumer_id, secret_id, secret_version)
        REFERENCES secret_pins (tenant_id, consumer_kind, consumer_id, secret_id, secret_version),
    FOREIGN KEY (tenant_id, project_id, secret_id, secret_version)
        REFERENCES secret_versions (tenant_id, project_id, secret_id, version)
);

CREATE INDEX secret_consumer_handling_pending_idx ON secret_consumer_handling (tenant_id, project_id, id) WHERE state = 'pending';

CREATE FUNCTION t012_handling_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'consumer handling records are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'pending' THEN
        RAISE EXCEPTION 'an acknowledged effect is never rewritten' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id
       OR NEW.secret_id <> OLD.secret_id OR NEW.secret_version <> OLD.secret_version
       OR NEW.consumer_kind <> OLD.consumer_kind OR NEW.consumer_id <> OLD.consumer_id
       OR NEW.effect <> OLD.effect OR NEW.delivered <> OLD.delivered OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'consumer handling identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER secret_consumer_handling_guard BEFORE UPDATE OR DELETE ON secret_consumer_handling
    FOR EACH ROW EXECUTE FUNCTION t012_handling_guard();

ALTER TABLE secret_pins              ENABLE ROW LEVEL SECURITY;

ALTER TABLE secret_pins              FORCE ROW LEVEL SECURITY;

ALTER TABLE secret_consumer_handling ENABLE ROW LEVEL SECURITY;

ALTER TABLE secret_consumer_handling FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON secret_pins              TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON secret_consumer_handling TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON secret_pins
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON secret_consumer_handling
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- pipeline definitions (was T014_001_pipeline_definitions.sql) ----

-- T-014 pipeline definitions: immutable, content-addressed versions.
--
-- Follows the base conventions (T002_001): opaque kodama_id keys, tenant and
-- project ownership through composite foreign keys, ENABLE + FORCE row level
-- security on kodama_tenant_id(), and minimal runtime grants. Registration also
-- appends to the T-002 audit table; this migration creates no audit or outbox
-- table of its own. Later run tables reference (tenant_id, project_id, id) or
-- (tenant_id, project_id, id, digest) to prove the same project owns, and the
-- run pinned, one exact definition.

CREATE TABLE pipeline_definitions (
    tenant_id       kodama_id NOT NULL REFERENCES tenants (id),
    project_id      kodama_id NOT NULL,
    id              kodama_id NOT NULL,
    name            text NOT NULL
                    CONSTRAINT pipeline_definitions_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    -- Allocated per (project, name) under the project row lock.
    version         bigint NOT NULL
                    CONSTRAINT pipeline_definitions_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    digest          text NOT NULL
                    CONSTRAINT pipeline_definitions_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    -- Canonical UTF-8 JSON exactly as hashed. Kept as text, not jsonb, because
    -- jsonb would reorder keys and lose the bytes the digest covers.
    body            text NOT NULL
                    CONSTRAINT pipeline_definitions_body_size CHECK (octet_length(body) BETWEEN 2 AND 1048576),
    created_by_type text NOT NULL
                    CONSTRAINT pipeline_definitions_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id   text NOT NULL
                    CONSTRAINT pipeline_definitions_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT pipeline_definitions_scope_key UNIQUE (tenant_id, project_id, id),
    -- Lets a run prove it pinned this definition's exact content.
    CONSTRAINT pipeline_definitions_pin_key UNIQUE (tenant_id, project_id, id, digest),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT pipeline_definitions_name_version_key UNIQUE (tenant_id, project_id, name, version),
    CONSTRAINT pipeline_definitions_digest_key UNIQUE (tenant_id, project_id, digest),
    -- The stored digest and name must describe the stored body.
    CONSTRAINT pipeline_definitions_body_object CHECK (jsonb_typeof(body::jsonb) = 'object'),
    CONSTRAINT pipeline_definitions_digest_matches_body CHECK (
        digest = 'sha256:' || encode(sha256(convert_to(body, 'UTF8')), 'hex')),
    CONSTRAINT pipeline_definitions_name_matches_body CHECK (name = (body::jsonb #>> '{metadata,name}'))
);

-- Default list order and keyset pagination, with and without the name filter.
CREATE INDEX pipeline_definitions_list_idx ON pipeline_definitions (tenant_id, project_id, created_at, id);

CREATE INDEX pipeline_definitions_name_list_idx ON pipeline_definitions (tenant_id, project_id, name, created_at, id);

ALTER TABLE pipeline_definitions ENABLE ROW LEVEL SECURITY;

ALTER TABLE pipeline_definitions FORCE ROW LEVEL SECURITY;

-- Versions are immutable: no UPDATE or DELETE policy exists for any role, and
-- the runtime role has no such grant.
CREATE POLICY owner_select ON pipeline_definitions FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON pipeline_definitions FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON pipeline_definitions FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON pipeline_definitions FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- workspaces (was T013_001_workspaces.sql) ----

-- T-013 workspace schema: one Git-backed workspace per actionable work item,
-- publication receipts and retained source references.
--
-- Ownership and integrity enforced by the database, not only by Go:
--   * every row carries tenant_id and project_id; foreign keys are composite so a
--     workspace, receipt or pin can only reference rows of its own project;
--   * forced row level security on kodama_tenant_id(), minimal runtime grants and
--     no DELETE anywhere: source records are history;
--   * at most one workspace per work item and one workspace per repository branch;
--   * the pending-publication fields are all-or-nothing and can only exist while
--     the publishing attempt still holds the writer slot, so a writer cannot be
--     released (or reassigned) before its publication settles.
--
-- Deferred cross-module constraints (documented in internal/workspace/README.md):
-- work_item_id -> work_items (T-011), writer_attempt_id -> step_attempts (T-020)
-- and publish_artifact_id -> artifacts (T-015) are added by those modules'
-- migration contributions once their tables exist.

CREATE TABLE workspaces (
    tenant_id             kodama_id NOT NULL,
    project_id            kodama_id NOT NULL,
    id                    kodama_id NOT NULL,
    work_item_id          kodama_id NOT NULL,
    repository_id         kodama_id NOT NULL,
    repository_url        text NOT NULL
                          CONSTRAINT workspaces_repository_url_length CHECK (char_length(repository_url) BETWEEN 1 AND 2048),
    checkout_secret_ref   text NOT NULL
                          CONSTRAINT workspaces_checkout_ref_length CHECK (char_length(checkout_secret_ref) BETWEEN 1 AND 512),
    publisher_secret_ref  text NOT NULL
                          CONSTRAINT workspaces_publisher_ref_length CHECK (char_length(publisher_secret_ref) BETWEEN 1 AND 512),
    work_branch           text NOT NULL
                          CONSTRAINT workspaces_branch_syntax CHECK (work_branch ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$' AND char_length(work_branch) <= 255),
    initial_commit        text CONSTRAINT workspaces_initial_commit_syntax CHECK (initial_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    accepted_commit       text CONSTRAINT workspaces_accepted_commit_syntax CHECK (accepted_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    state                 text NOT NULL DEFAULT 'provisioning'
                          CONSTRAINT workspaces_state_known CHECK (state IN ('provisioning', 'available', 'archived')),
    version               bigint NOT NULL DEFAULT 1
                          CONSTRAINT workspaces_version_range CHECK (version BETWEEN 1 AND 9007199254740991),

    writer_attempt_id     kodama_id,
    writer_input_commit   text CONSTRAINT workspaces_writer_input_syntax CHECK (writer_input_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    writer_expected_tip   text CONSTRAINT workspaces_writer_tip_syntax CHECK (writer_expected_tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    writer_reserved_at    timestamptz,

    publish_attempt_id    kodama_id,
    publish_commit        text CONSTRAINT workspaces_publish_commit_syntax CHECK (publish_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    publish_expected_commit text CONSTRAINT workspaces_publish_expected_syntax CHECK (publish_expected_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    publish_artifact_id   kodama_id,
    publish_bundle_digest text CONSTRAINT workspaces_publish_digest_syntax CHECK (publish_bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    publish_started_at    timestamptz,
    publish_driver_id     text CONSTRAINT workspaces_publish_driver_length CHECK (char_length(publish_driver_id) <= 128),
    publish_driver_epoch  bigint,
    publish_driver_claimed_at timestamptz,
    publish_attempts      integer NOT NULL DEFAULT 0 CONSTRAINT workspaces_publish_attempts_nonneg CHECK (publish_attempts >= 0),
    publish_last_attempt_at timestamptz,

    archive_requested_at  timestamptz,
    last_error            text CONSTRAINT workspaces_last_error_code CHECK (last_error ~ '^[a-z][a-z0-9_]{0,63}$'),
    fence_epoch           bigint NOT NULL DEFAULT 1 CONSTRAINT workspaces_fence_epoch_positive CHECK (fence_epoch >= 1),
    publication_fenced    boolean NOT NULL DEFAULT false,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT workspaces_one_per_work_item UNIQUE (tenant_id, project_id, work_item_id),
    CONSTRAINT workspaces_one_per_branch UNIQUE (tenant_id, project_id, repository_id, work_branch),
    CONSTRAINT workspaces_distinct_credentials CHECK (checkout_secret_ref <> publisher_secret_ref),
    CONSTRAINT workspaces_accepted_when_usable CHECK (state = 'provisioning' OR accepted_commit IS NOT NULL),
    CONSTRAINT workspaces_writer_all_or_none CHECK (
        (writer_attempt_id IS NULL AND writer_input_commit IS NULL AND writer_expected_tip IS NULL AND writer_reserved_at IS NULL) OR
        (writer_attempt_id IS NOT NULL AND writer_input_commit IS NOT NULL AND writer_expected_tip IS NOT NULL AND writer_reserved_at IS NOT NULL)),
    CONSTRAINT workspaces_publication_all_or_none CHECK (
        (publish_attempt_id IS NULL AND publish_commit IS NULL AND publish_expected_commit IS NULL AND publish_artifact_id IS NULL
            AND publish_bundle_digest IS NULL AND publish_started_at IS NULL AND publish_driver_id IS NULL AND publish_driver_epoch IS NULL
            AND publish_driver_claimed_at IS NULL) OR
        (publish_attempt_id IS NOT NULL AND publish_commit IS NOT NULL AND publish_expected_commit IS NOT NULL AND publish_artifact_id IS NOT NULL
            AND publish_bundle_digest IS NOT NULL AND publish_started_at IS NOT NULL
            AND ((publish_driver_id IS NULL AND publish_driver_epoch IS NULL AND publish_driver_claimed_at IS NULL) OR
                 (publish_driver_id IS NOT NULL AND publish_driver_epoch IS NOT NULL AND publish_driver_claimed_at IS NOT NULL)))),
    -- Settlement before release: the publisher holds the slot until the
    -- publication is recorded.
    CONSTRAINT workspaces_publication_holds_writer CHECK (publish_attempt_id IS NULL OR (writer_attempt_id IS NOT NULL AND publish_attempt_id = writer_attempt_id)),
    CONSTRAINT workspaces_pending_differs CHECK (publish_commit IS NULL OR publish_commit <> publish_expected_commit),
    CONSTRAINT workspaces_pending_requires_available CHECK (publish_attempt_id IS NULL OR state IN ('available', 'archived'))
);

CREATE INDEX workspaces_pending_idx ON workspaces (tenant_id, id) WHERE publish_attempt_id IS NOT NULL;

CREATE INDEX workspaces_writer_idx ON workspaces (tenant_id, project_id, writer_attempt_id) WHERE writer_attempt_id IS NOT NULL;

CREATE INDEX workspaces_open_work_idx ON workspaces (tenant_id, id)
    WHERE state = 'provisioning' OR archive_requested_at IS NOT NULL OR publish_attempt_id IS NOT NULL;

-- One immutable receipt per proposing attempt; it outlives the writer slot.
CREATE TABLE workspace_publications (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    workspace_id     kodama_id NOT NULL,
    attempt_id       kodama_id NOT NULL,
    commit_oid       text NOT NULL CONSTRAINT workspace_publications_commit_syntax CHECK (commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    expected_commit  text NOT NULL CONSTRAINT workspace_publications_expected_syntax CHECK (expected_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    artifact_id      kodama_id NOT NULL,
    bundle_digest    text NOT NULL CONSTRAINT workspace_publications_digest_syntax CHECK (bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    status           text NOT NULL DEFAULT 'pending'
                     CONSTRAINT workspace_publications_status_known CHECK (status IN ('pending', 'published', 'conflict')),
    observed_tip     text CONSTRAINT workspace_publications_observed_syntax CHECK (observed_tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    conflict_reason  text CONSTRAINT workspace_publications_reason_code CHECK (conflict_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    outcome          text NOT NULL DEFAULT 'unsettled'
                     CONSTRAINT workspace_publications_outcome_known CHECK (outcome IN ('unsettled', 'succeeded', 'failed', 'cancelled')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    settled_at       timestamptz,
    PRIMARY KEY (tenant_id, project_id, workspace_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_publications_settled_consistent CHECK ((status = 'pending') = (settled_at IS NULL)),
    CONSTRAINT workspace_publications_conflict_reason CHECK ((status = 'conflict') = (conflict_reason IS NOT NULL)),
    -- Only a published proposal can be the output of a successful writer.
    CONSTRAINT workspace_publications_success_published CHECK (outcome <> 'succeeded' OR status = 'published'),
    CONSTRAINT workspace_publications_differs CHECK (commit_oid <> expected_commit)
);

-- Retained references protect commits with ordinary remote branch refs. The
-- reference name is persisted and never re-derived.
CREATE TABLE workspace_retained_refs (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    workspace_id     kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    commit_oid       text NOT NULL CONSTRAINT workspace_retained_refs_commit_syntax CHECK (commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    ref_name         text NOT NULL
                     CONSTRAINT workspace_retained_refs_ref_syntax CHECK (ref_name ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$' AND char_length(ref_name) <= 400),
    purpose          text NOT NULL CONSTRAINT workspace_retained_refs_purpose_known CHECK (purpose IN ('reader', 'writer_output')),
    run_id           kodama_id NOT NULL,
    attempt_id       kodama_id,
    status           text NOT NULL DEFAULT 'intent'
                     CONSTRAINT workspace_retained_refs_status_known CHECK (status IN ('intent', 'confirmed', 'released', 'removing', 'removed')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    confirmed_at     timestamptz,
    released_at      timestamptz,
    removal_requested_at timestamptz,
    removed_at       timestamptz,
    PRIMARY KEY (tenant_id, project_id, workspace_id, id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_retained_refs_unique_ref UNIQUE (tenant_id, project_id, workspace_id, ref_name),
    CONSTRAINT workspace_retained_refs_writer_attempt CHECK ((purpose = 'writer_output') = (attempt_id IS NOT NULL)),
    CONSTRAINT workspace_retained_refs_status_times CHECK (
        (status = 'intent' AND confirmed_at IS NULL) OR
        (status <> 'intent' AND confirmed_at IS NOT NULL))
);

CREATE INDEX workspace_retained_refs_open_idx ON workspace_retained_refs (tenant_id, workspace_id) WHERE status IN ('intent', 'removing');

CREATE INDEX workspace_retained_refs_commit_idx ON workspace_retained_refs (tenant_id, project_id, workspace_id, commit_oid);

ALTER TABLE workspaces              ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspaces              FORCE ROW LEVEL SECURITY;

ALTER TABLE workspace_publications  ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_publications  FORCE ROW LEVEL SECURITY;

ALTER TABLE workspace_retained_refs ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_retained_refs FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON workspaces              TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON workspace_publications  TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON workspace_retained_refs TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON workspaces
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON workspace_publications
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON workspace_retained_refs
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- artifacts (was T015_001_artifacts.sql) ----

-- T-015 artifact storage: durable metadata for retained outputs, the object
-- records that make cleanup recoverable, and retention pins.
--
-- run_id/attempt_id/step_id name the producing attempt. The execution tables do
-- not exist yet, so there is no foreign key to them: T-020's migration adds the
-- ownership constraint (tenant_id, project_id, attempt_id) -> attempts(...)
-- when it creates the run tables. Until then ownership is enforced by the
-- composite foreign key to projects and by the transaction's tenant context,
-- and the unique (tenant_id, project_id, attempt_id, name) key plus the
-- immutable ownership columns are exactly what that later constraint needs.
--
-- File bytes live in the object store; PostgreSQL holds only logical keys.

CREATE TABLE artifacts (
    tenant_id           kodama_id NOT NULL,
    project_id          kodama_id NOT NULL,
    id                  kodama_id NOT NULL,
    run_id              kodama_id NOT NULL,
    attempt_id          kodama_id NOT NULL,
    step_id             text NOT NULL
                        CONSTRAINT artifacts_step_length CHECK (char_length(step_id) BETWEEN 1 AND 128),
    name                text NOT NULL
                        CONSTRAINT artifacts_name_syntax CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    kind                text NOT NULL
                        CONSTRAINT artifacts_kind_known CHECK (kind IN ('file', 'oci')),
    role                text NOT NULL
                        CONSTRAINT artifacts_role_known CHECK (role IN ('output', 'diagnostic', 'bundle')),
    digest              text NOT NULL
                        CONSTRAINT artifacts_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    media_type          text
                        CONSTRAINT artifacts_media_type_syntax CHECK (media_type ~ '^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$'),
    size_bytes          bigint
                        CONSTRAINT artifacts_size_range CHECK (size_bytes BETWEEN 0 AND 1099511627776),
    registry            text
                        CONSTRAINT artifacts_registry_length CHECK (char_length(registry) BETWEEN 3 AND 512),
    source_repository_url text
                        CONSTRAINT artifacts_source_url_syntax CHECK (source_repository_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*)?$'),
    source_commit       text
                        CONSTRAINT artifacts_source_commit_syntax CHECK (source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    -- Content lifecycle: the runtime contract's states. reserved -> usable for
    -- files; an OCI registration exists only once verified (registered).
    state               text NOT NULL
                        CONSTRAINT artifacts_state_known CHECK (state IN ('reserved', 'registered', 'usable')),
    -- Cleanup bookkeeping, deliberately not part of the runtime state.
    disposition         text NOT NULL DEFAULT 'active'
                        CONSTRAINT artifacts_disposition_known CHECK (disposition IN ('active', 'abandoned', 'removed')),
    staging_key         text
                        CONSTRAINT artifacts_staging_key_shape CHECK (staging_key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    sealed_key          text
                        CONSTRAINT artifacts_sealed_key_shape CHECK (sealed_key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    -- Latest instant an issued upload instruction can still be honored.
    transfer_expires_at timestamptz,
    verified_at         timestamptz,
    verified_size_bytes bigint
                        CONSTRAINT artifacts_verified_size_range CHECK (verified_size_bytes BETWEEN 0 AND 1099511627776),
    -- Fenced completion lease. claim_generation only increases.
    claim_token         text,
    claim_generation    bigint NOT NULL DEFAULT 0
                        CONSTRAINT artifacts_claim_generation_range CHECK (claim_generation >= 0),
    claim_until         timestamptz,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    version             bigint NOT NULL
                        CONSTRAINT artifacts_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    -- The replay key; also the ownership key later execution constraints use.
    CONSTRAINT artifacts_attempt_name_unique UNIQUE (tenant_id, project_id, attempt_id, name),
    CONSTRAINT artifacts_source_both CHECK ((source_repository_url IS NULL) = (source_commit IS NULL)),
    CONSTRAINT artifacts_claim_both CHECK ((claim_token IS NULL) = (claim_until IS NULL)),
    CONSTRAINT artifacts_shape CHECK (
        (kind = 'file'
            AND state IN ('reserved', 'usable')
            AND media_type IS NOT NULL AND size_bytes IS NOT NULL AND registry IS NULL
            AND staging_key IS NOT NULL AND transfer_expires_at IS NOT NULL
            AND ((state = 'reserved' AND sealed_key IS NULL AND verified_at IS NULL AND verified_size_bytes IS NULL)
              OR (state = 'usable' AND sealed_key IS NOT NULL AND sealed_key <> staging_key
                  AND verified_at IS NOT NULL AND verified_size_bytes = size_bytes)))
        OR
        (kind = 'oci'
            AND state = 'registered' AND role = 'output'
            AND registry IS NOT NULL AND media_type IS NULL AND size_bytes IS NULL
            AND staging_key IS NULL AND sealed_key IS NULL AND transfer_expires_at IS NULL
            AND source_commit IS NOT NULL
            AND verified_at IS NOT NULL AND verified_size_bytes IS NOT NULL
            AND claim_token IS NULL)
    ),
    CONSTRAINT artifacts_disposition_rule CHECK (
        (disposition = 'active')
        OR (disposition = 'abandoned' AND state = 'reserved')
        OR (disposition = 'removed' AND state IN ('usable', 'registered'))
    )
);

CREATE INDEX artifacts_attempt_idx ON artifacts (tenant_id, project_id, attempt_id, role);

CREATE INDEX artifacts_reserved_idx ON artifacts (tenant_id, transfer_expires_at)
    WHERE state = 'reserved' AND disposition = 'active';

CREATE INDEX artifacts_retention_idx ON artifacts (tenant_id, id)
    WHERE state IN ('usable', 'registered') AND disposition = 'active';

-- Immutable identity, content declaration and provenance; one-way lifecycle.
CREATE FUNCTION t015_artifacts_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'artifact metadata and provenance are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id OR NEW.step_id <> OLD.step_id
       OR NEW.name <> OLD.name OR NEW.kind <> OLD.kind OR NEW.role <> OLD.role OR NEW.digest <> OLD.digest
       OR NEW.media_type IS DISTINCT FROM OLD.media_type OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
       OR NEW.registry IS DISTINCT FROM OLD.registry
       OR NEW.source_repository_url IS DISTINCT FROM OLD.source_repository_url
       OR NEW.source_commit IS DISTINCT FROM OLD.source_commit
       OR NEW.staging_key IS DISTINCT FROM OLD.staging_key
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'artifact identity, content declaration and provenance are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (OLD.state = 'reserved' AND NEW.state = 'usable') THEN
        RAISE EXCEPTION 'invalid artifact state transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    IF OLD.sealed_key IS NOT NULL AND NEW.sealed_key IS DISTINCT FROM OLD.sealed_key THEN
        RAISE EXCEPTION 'sealed content is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.verified_at IS NOT NULL AND (NEW.verified_at IS DISTINCT FROM OLD.verified_at
       OR NEW.verified_size_bytes IS DISTINCT FROM OLD.verified_size_bytes) THEN
        RAISE EXCEPTION 'verification evidence is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.disposition <> OLD.disposition AND OLD.disposition <> 'active' THEN
        RAISE EXCEPTION 'a closed artifact stays closed' USING ERRCODE = '23514';
    END IF;
    IF NEW.claim_generation < OLD.claim_generation OR NEW.claim_generation > OLD.claim_generation + 1 THEN
        RAISE EXCEPTION 'claim generations only increase by one' USING ERRCODE = '23514';
    END IF;
    IF NEW.transfer_expires_at IS DISTINCT FROM OLD.transfer_expires_at
       AND (NEW.transfer_expires_at IS NULL OR NEW.transfer_expires_at < OLD.transfer_expires_at) THEN
        RAISE EXCEPTION 'the latest issued transfer expiry never moves back' USING ERRCODE = '23514';
    END IF;
    -- The resource version moves exactly with state or disposition changes.
    IF NEW.state <> OLD.state OR NEW.disposition <> OLD.disposition THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'a lifecycle change increments the version by one' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'the version changes only with the lifecycle' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER artifacts_guard BEFORE UPDATE OR DELETE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION t015_artifacts_guard();

-- Every object this module may write is recorded here BEFORE the write, so a
-- crash can strand bytes but never an unrecorded object. Deletion intent and
-- acknowledgement are recorded around the provider call.
CREATE TABLE artifact_objects (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    artifact_id        kodama_id NOT NULL,
    key                text NOT NULL
                       CONSTRAINT artifact_objects_key_shape CHECK (key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    purpose            text NOT NULL
                       CONSTRAINT artifact_objects_purpose_known CHECK (purpose IN ('staging', 'candidate')),
    -- The completion claim generation that may write a candidate (0 for staging).
    generation         bigint NOT NULL DEFAULT 0
                       CONSTRAINT artifact_objects_generation_range CHECK (generation >= 0),
    state              text NOT NULL DEFAULT 'live'
                       CONSTRAINT artifact_objects_state_known CHECK (state IN ('live', 'delete_pending', 'deleted')),
    -- Winner: the sealed destination the usable artifact points at.
    winner             boolean NOT NULL DEFAULT false,
    -- Latest instant a holder of a credential or lease may still write the key.
    writable_until     timestamptz NOT NULL,
    delete_requested_at timestamptz,
    delete_token       text,
    delete_until       timestamptz,
    delete_attempts    integer NOT NULL DEFAULT 0
                       CONSTRAINT artifact_objects_attempts_range CHECK (delete_attempts >= 0),
    deleted_at         timestamptz,
    created_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, artifact_id, key),
    CONSTRAINT artifact_objects_key_unique UNIQUE (tenant_id, key),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id),
    CONSTRAINT artifact_objects_winner_is_candidate CHECK (NOT winner OR purpose = 'candidate'),
    CONSTRAINT artifact_objects_state_fields CHECK (
        (state = 'live' AND deleted_at IS NULL)
        OR (state = 'delete_pending' AND delete_requested_at IS NOT NULL AND deleted_at IS NULL)
        OR (state = 'deleted' AND deleted_at IS NOT NULL))
);

CREATE UNIQUE INDEX artifact_objects_one_winner ON artifact_objects (tenant_id, project_id, artifact_id) WHERE winner;

CREATE INDEX artifact_objects_sweep_idx ON artifact_objects (tenant_id, state, writable_until);

CREATE FUNCTION t015_objects_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'object records are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.artifact_id <> OLD.artifact_id
       OR NEW.key <> OLD.key OR NEW.purpose <> OLD.purpose OR NEW.generation <> OLD.generation OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'object identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.winner <> OLD.winner AND (OLD.winner OR OLD.state <> 'live') THEN
        RAISE EXCEPTION 'the winner is set once, on a live object' USING ERRCODE = '23514';
    END IF;
    IF NEW.writable_until < OLD.writable_until THEN
        RAISE EXCEPTION 'the latest possible write time never moves back' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state THEN
        IF NOT ((OLD.state = 'live' AND NEW.state = 'delete_pending')
             OR (OLD.state = 'delete_pending' AND NEW.state = 'deleted')
             -- A late staging upload is possible after cleanup: a renewed
             -- transfer reopens the staging record so a repeat sweep catches it.
             OR (OLD.purpose = 'staging' AND OLD.state IN ('delete_pending', 'deleted') AND NEW.state = 'live')) THEN
            RAISE EXCEPTION 'invalid object state transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
        END IF;
    END IF;
    -- The usable artifact's sealed bytes can only be scheduled for deletion by
    -- retention, which first marks the artifact removed in the same transaction.
    IF NEW.winner AND NEW.state = 'delete_pending' AND OLD.state = 'live' THEN
        IF NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id
                       AND a.id = NEW.artifact_id AND a.disposition = 'removed') THEN
            RAISE EXCEPTION 'the winner is deleted only after retention removed the artifact' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER artifact_objects_guard BEFORE UPDATE OR DELETE ON artifact_objects
    FOR EACH ROW EXECUTE FUNCTION t015_objects_guard();

-- Retention pins: a consumer (gate evidence, publication) protecting content.
CREATE TABLE artifact_pins (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    artifact_id kodama_id NOT NULL,
    owner_kind  text NOT NULL
                CONSTRAINT artifact_pins_owner_kind_length CHECK (char_length(owner_kind) BETWEEN 1 AND 32),
    owner_id    kodama_id NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, artifact_id, owner_kind, owner_id),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id)
);

ALTER TABLE artifacts        ENABLE ROW LEVEL SECURITY;

ALTER TABLE artifacts        FORCE ROW LEVEL SECURITY;

ALTER TABLE artifact_objects ENABLE ROW LEVEL SECURITY;

ALTER TABLE artifact_objects FORCE ROW LEVEL SECURITY;

ALTER TABLE artifact_pins    ENABLE ROW LEVEL SECURITY;

ALTER TABLE artifact_pins    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON artifacts        TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON artifact_objects TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON artifact_pins    TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON artifacts
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON artifact_objects
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON artifact_pins
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- execution runs (was T020_002_runs.sql) ----

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

-- ---- execution workspace links (was T020_001_attempt_fk.sql) ----

-- T-020 deferred workspace ownership constraints.
--
-- The workspace schema (T013_001) could not reference tables that did not exist
-- yet: work items (T-011), step attempts and runs (T-020) and artifacts
-- (T-015). Now that they do, the database itself enforces what the workspace
-- module checked in Go:
--   * a workspace belongs to a work item of its own project;
--   * the writer slot and the pending publication name an attempt of the same
--     project. The attempt row and the writer reservation commit in one
--     transaction (dispatch), so these two constraints are deferred to commit
--     instead of imposing an insert order on the dispatch transaction;
--   * a publication receipt belongs to its proposing attempt and bundle artifact;
--   * a retained reference names the run (and, for writer output, the attempt)
--     that depends on it.
--
-- This contribution is ordered after the execution tables (T020_002) and the
-- artifact tables (T015_001) in the manifest. Existing rows must already satisfy
-- the constraints; an installation that held writer reservations or retained
-- references before execution existed must settle them first.

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_work_item_fk
        FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id),
    ADD CONSTRAINT workspaces_writer_attempt_fk
        FOREIGN KEY (tenant_id, project_id, writer_attempt_id) REFERENCES step_attempts (tenant_id, project_id, id)
        DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT workspaces_publish_attempt_fk
        FOREIGN KEY (tenant_id, project_id, publish_attempt_id) REFERENCES step_attempts (tenant_id, project_id, id)
        DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT workspaces_publish_artifact_fk
        FOREIGN KEY (tenant_id, project_id, publish_artifact_id) REFERENCES artifacts (tenant_id, project_id, id);

ALTER TABLE workspace_publications
    ADD CONSTRAINT workspace_publications_attempt_fk
        FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    ADD CONSTRAINT workspace_publications_artifact_fk
        FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id);

ALTER TABLE workspace_retained_refs
    ADD CONSTRAINT workspace_retained_refs_run_fk
        FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    ADD CONSTRAINT workspace_retained_refs_attempt_fk
        FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id);

-- ---- execution artifact links (was T020_003_artifact_fk.sql) ----

-- T-020 artifact producer ownership. The artifact schema (T015_001) names the
-- producing run, step and attempt but could not reference execution tables that
-- did not exist yet. With them in place an artifact can only be produced by an
-- attempt of its own project whose run and step match the artifact's own
-- columns, so a runtime caller cannot register an artifact against another
-- run's attempt even with a forged identifier.
--
-- Ordered after T015_001 and T020_002 in the manifest.

ALTER TABLE artifacts
    ADD CONSTRAINT artifacts_producer_attempt_fk
        FOREIGN KEY (tenant_id, project_id, attempt_id, run_id, step_id)
        REFERENCES step_attempts (tenant_id, project_id, id, run_id, step_id);

-- ---- execution secret pin links (was T020_004_secret_pin_fk.sql) ----

-- T-020 secret pin ownership. T012_002 deferred the constraint
-- (tenant_id, project_id, consumer_id) -> runs to the run tables' migration: a
-- secret version is pinned by exactly one run of the same project. Run
-- admission inserts the run before registering its pins in the same
-- transaction, so the constraint is immediate.
--
-- Ordered after T012_002 and T020_002 in the manifest.

ALTER TABLE secret_pins
    ADD CONSTRAINT secret_pins_run_fk
        FOREIGN KEY (tenant_id, project_id, consumer_id) REFERENCES runs (tenant_id, project_id, id);

-- ---- runtime authority (was T019_001_runtime_grants.sql) ----

-- T-019 runtime grants: one-time admission credentials and the sessions they
-- open.
--
-- A grant is registered by the trusted executor with the SHA-256 digest of a
-- generated credential (the credential itself is never stored) together with
-- the attempt's immutable authority: owner, expected Job, action ceiling,
-- signing generation, authority revision and the pinned context document. The
-- first successful admission writes the consumption once; revocation is written
-- once and is irreversible. Triggers enforce those transitions even against a
-- buggy writer, the column-level grants give the runtime role no way to alter
-- the immutable columns, and there is no DELETE grant (retention is T-056's).
--
-- There is deliberately no foreign key to an attempt table: execution (T-020)
-- owns attempts and has not merged. The attempt reference is the composite
-- (tenant_id, attempt_id); T-020's migration may add a composite foreign key to
-- its attempts table on (tenant_id, project_id, id) without changing this one.

CREATE TABLE runtime_grants (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    job_namespace      text NOT NULL
                       CONSTRAINT runtime_grants_namespace_syntax CHECK (job_namespace ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$'),
    job_name           text NOT NULL
                       CONSTRAINT runtime_grants_job_name_syntax CHECK (job_name ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$'),
    credential_digest  bytea NOT NULL
                       CONSTRAINT runtime_grants_credential_digest_length CHECK (octet_length(credential_digest) = 32),
    signing_generation bigint NOT NULL
                       CONSTRAINT runtime_grants_generation_range CHECK (signing_generation BETWEEN 1 AND 9007199254740991),
    actions            text[] NOT NULL
                       CONSTRAINT runtime_grants_actions_bounded CHECK (cardinality(actions) BETWEEN 1 AND 16)
                       CONSTRAINT runtime_grants_actions_known CHECK (actions <@ ARRAY[
                           'context.read', 'token.renew', 'secret.resolve', 'artifact.reserve', 'artifact.complete',
                           'source.propose', 'result.submit', 'model.invoke', 'tool.invoke', 'deployment.call']::text[]),
    authority_revision bigint NOT NULL
                       CONSTRAINT runtime_grants_revision_range CHECK (authority_revision BETWEEN 1 AND 9007199254740991),
    context_digest     bytea NOT NULL
                       CONSTRAINT runtime_grants_context_digest_length CHECK (octet_length(context_digest) = 32),
    context_doc        bytea NOT NULL
                       CONSTRAINT runtime_grants_context_size CHECK (octet_length(context_doc) BETWEEN 2 AND 4194304),
    created_at         timestamptz NOT NULL,
    expires_at         timestamptz NOT NULL,
    -- first admission (all or none)
    session_id         text,
    nonce_digest       bytea,
    bound_job_uid      text,
    bound_pod_name     text,
    bound_pod_uid      text,
    consumed_at        timestamptz,
    max_expires_at     timestamptz,
    revoked_at         timestamptz,
    PRIMARY KEY (tenant_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT runtime_grants_credential_unique UNIQUE (credential_digest),
    CONSTRAINT runtime_grants_session_unique UNIQUE (session_id),
    CONSTRAINT runtime_grants_expiry_order CHECK (expires_at > created_at),
    CONSTRAINT runtime_grants_consumption_complete CHECK (
        (session_id IS NULL AND nonce_digest IS NULL AND bound_job_uid IS NULL AND bound_pod_name IS NULL
         AND bound_pod_uid IS NULL AND consumed_at IS NULL AND max_expires_at IS NULL)
        OR
        (session_id IS NOT NULL AND nonce_digest IS NOT NULL AND bound_job_uid IS NOT NULL AND bound_pod_name IS NOT NULL
         AND bound_pod_uid IS NOT NULL AND consumed_at IS NOT NULL AND max_expires_at IS NOT NULL)),
    CONSTRAINT runtime_grants_session_syntax CHECK (session_id IS NULL OR session_id ~ '^ses_[A-Za-z0-9_-]{22,43}$'),
    CONSTRAINT runtime_grants_nonce_digest_length CHECK (nonce_digest IS NULL OR octet_length(nonce_digest) = 32),
    CONSTRAINT runtime_grants_bound_uid_syntax CHECK (
        bound_job_uid IS NULL OR (
            bound_job_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND bound_pod_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND bound_pod_name ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$')),
    CONSTRAINT runtime_grants_lifetime_order CHECK (max_expires_at IS NULL OR max_expires_at > consumed_at),
    CONSTRAINT runtime_grants_revoked_after_creation CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

ALTER TABLE runtime_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE runtime_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON runtime_grants TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON runtime_grants
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express: immutable registration, a
-- consumption written exactly once, an irreversible revocation.
CREATE FUNCTION runtime_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.attempt_id, NEW.job_namespace, NEW.job_name,
        NEW.credential_digest, NEW.signing_generation, NEW.actions, NEW.authority_revision,
        NEW.context_digest, NEW.context_doc, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.attempt_id, OLD.job_namespace, OLD.job_name,
        OLD.credential_digest, OLD.signing_generation, OLD.actions, OLD.authority_revision,
        OLD.context_digest, OLD.context_doc, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'runtime grant registration is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.session_id IS NOT NULL AND
       (NEW.session_id, NEW.nonce_digest, NEW.bound_job_uid, NEW.bound_pod_name, NEW.bound_pod_uid, NEW.consumed_at, NEW.max_expires_at)
       IS DISTINCT FROM
       (OLD.session_id, OLD.nonce_digest, OLD.bound_job_uid, OLD.bound_pod_name, OLD.bound_pod_uid, OLD.consumed_at, OLD.max_expires_at) THEN
        RAISE EXCEPTION 'runtime grant consumption is write-once' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'runtime grant revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER runtime_grants_guard BEFORE UPDATE ON runtime_grants
    FOR EACH ROW EXECUTE FUNCTION runtime_grants_guard();

-- Admission carries no tenant selector, so the grant is found by credential
-- digest through the one narrow path outside a tenant context. The function
-- returns only the ownership locator of the single matching grant: no context,
-- no digest, no state. The caller must know the digest, which is only derivable
-- from the credential. Everything afterwards rechecks the credential inside a
-- tenant transaction.
CREATE FUNCTION kodama_runtime_locate_grant(p_digest bytea)
    RETURNS TABLE (tenant_id text, project_id text, attempt_id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT g.tenant_id::text, g.project_id::text, g.attempt_id::text
        FROM public.runtime_grants g
        WHERE g.credential_digest = p_digest
    $$;

REVOKE ALL ON FUNCTION kodama_runtime_locate_grant(bytea) FROM PUBLIC;

-- ---- execution workload cleanup (was T086_001_workload_cleanup.sql) ----

-- T-086 workload cleanup bookkeeping. A runtime removes what remains of an
-- attempt's workload only after the attempt settled. The removal is recorded
-- apart from settlement, once, so a failed or still-retained removal is
-- retried by a later reconciliation pass, for attempts of terminal runs too.
--
-- Attempts settled before this migration have no recorded removal: they are
-- pending, and removing a workload that no longer exists succeeds.
--
-- Registered as its own contribution after every earlier execution
-- contribution.

ALTER TABLE attempt_controls
    ADD COLUMN workload_removed_at timestamptz,
    ADD CONSTRAINT attempt_controls_removal_after_settlement
        CHECK (workload_removed_at IS NULL OR settled_at IS NOT NULL);

CREATE INDEX attempt_controls_removal_pending_idx ON attempt_controls (tenant_id, settled_at, project_id, attempt_id)
    WHERE settled_at IS NOT NULL AND workload_removed_at IS NULL;

CREATE FUNCTION t086_attempt_controls_removal_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.workload_removed_at IS NOT NULL AND NEW.workload_removed_at IS DISTINCT FROM OLD.workload_removed_at THEN
        RAISE EXCEPTION 'a recorded workload removal is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER attempt_controls_removal_guard BEFORE UPDATE ON attempt_controls
    FOR EACH ROW EXECUTE FUNCTION t086_attempt_controls_removal_guard();

-- ---- log records (was T023_001_log_records.sql) ----

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

-- ---- agent profiles (was T030_001_agent_profiles.sql) ----

-- T-030 agent profiles: immutable, content-addressed profile versions.
--
-- Follows the base conventions (T002_001) and the pipeline definition pattern
-- (T014_001): opaque kodama_id keys, tenant and project ownership through
-- composite foreign keys, ENABLE + FORCE row level security on
-- kodama_tenant_id(), and minimal runtime grants. Registration also appends to
-- the T-002 audit table; this migration creates no audit or outbox table and
-- registration emits no event. Runs do not reference this table: a run pins the
-- complete expanded profile content in its frozen step and attempt context, so
-- a later registration, restart or retry can never change it.

CREATE TABLE agent_profiles (
    tenant_id       kodama_id NOT NULL REFERENCES tenants (id),
    project_id      kodama_id NOT NULL,
    id              kodama_id NOT NULL,
    name            text NOT NULL
                    CONSTRAINT agent_profiles_name_syntax CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    -- Allocated per (project, name) under the project row lock.
    version         bigint NOT NULL
                    CONSTRAINT agent_profiles_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    digest          text NOT NULL
                    CONSTRAINT agent_profiles_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    -- Canonical UTF-8 JSON exactly as hashed. Kept as text, not jsonb, because
    -- jsonb would reorder keys and lose the bytes the digest covers.
    body            text NOT NULL
                    CONSTRAINT agent_profiles_body_size CHECK (octet_length(body) BETWEEN 2 AND 163840),
    created_by_type text NOT NULL
                    CONSTRAINT agent_profiles_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id   text NOT NULL
                    CONSTRAINT agent_profiles_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT agent_profiles_scope_key UNIQUE (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT agent_profiles_name_version_key UNIQUE (tenant_id, project_id, name, version),
    CONSTRAINT agent_profiles_digest_key UNIQUE (tenant_id, project_id, digest),
    -- The stored digest and name must describe the stored body.
    CONSTRAINT agent_profiles_body_object CHECK (jsonb_typeof(body::jsonb) = 'object'),
    CONSTRAINT agent_profiles_digest_matches_body CHECK (
        digest = 'sha256:' || encode(sha256(convert_to(body, 'UTF8')), 'hex')),
    CONSTRAINT agent_profiles_name_matches_body CHECK (name = (body::jsonb ->> 'name'))
);

-- Default list order and keyset pagination, with and without the name filter.
CREATE INDEX agent_profiles_list_idx ON agent_profiles (tenant_id, project_id, created_at, id);

CREATE INDEX agent_profiles_name_list_idx ON agent_profiles (tenant_id, project_id, name, created_at, id);

ALTER TABLE agent_profiles ENABLE ROW LEVEL SECURITY;

ALTER TABLE agent_profiles FORCE ROW LEVEL SECURITY;

-- Versions are immutable: no UPDATE or DELETE policy exists for any role, and
-- the runtime role has no such grant.
CREATE POLICY owner_select ON agent_profiles FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON agent_profiles FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON agent_profiles FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON agent_profiles FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- delivery (was T035_001_delivery.sql) ----

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

-- ---- gates (was T034_001_gates.sql) ----

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

-- ---- model gateway (was T032_001_gateway.sql) ----

-- T-032 model gateway: project model settings with immutable revisions, the
-- immutable per-run policy binding with its monotonic counters, admitted model
-- requests (one unresolved request per run, enforced here) and usage records.
--
--   * Every table is tenant owned with forced row level security; the runtime
--     role receives column-scoped grants and no DELETE.
--   * Composite foreign keys tie settings to projects, bindings to runs and
--     requests to the binding and attempt of the same project.
--   * Counters are nonnegative, bounded to 2^53-1 and only ever increase; policy
--     columns of a binding and the identity of a request never change; request
--     states follow the entity's transitions.
--   * No table has a column for prompts, completions, tool arguments, provider
--     error text or content digests. Raw content belongs to the optional
--     recorder, never to these rows.

CREATE TABLE model_settings_revisions (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    revision           bigint NOT NULL
                       CONSTRAINT model_settings_revisions_range CHECK (revision BETWEEN 2 AND 9007199254740991),
    enabled            boolean NOT NULL,
    endpoint           text NOT NULL
                       CONSTRAINT model_settings_revisions_endpoint CHECK (endpoint ~ '^https://[a-z0-9]' AND char_length(endpoint) <= 512),
    credential_ref     text NOT NULL
                       CONSTRAINT model_settings_revisions_credential_ref CHECK (credential_ref ~ '^secret://project/[a-z][a-z0-9-]{0,62}$'),
    credential_version bigint NOT NULL
                       CONSTRAINT model_settings_revisions_credential_version CHECK (credential_version BETWEEN 1 AND 9007199254740991),
    models             text[] NOT NULL
                       CONSTRAINT model_settings_revisions_models CHECK (cardinality(models) BETWEEN 1 AND 32),
    max_requests       bigint NOT NULL CONSTRAINT model_settings_revisions_max_requests CHECK (max_requests BETWEEN 1 AND 100000),
    token_allowance    bigint NOT NULL CONSTRAINT model_settings_revisions_allowance CHECK (token_allowance BETWEEN 1 AND 1099511627776),
    max_request_bytes  bigint NOT NULL CONSTRAINT model_settings_revisions_request_bytes CHECK (max_request_bytes BETWEEN 1 AND 4194304),
    max_input_tokens   bigint NOT NULL CONSTRAINT model_settings_revisions_input CHECK (max_input_tokens BETWEEN 1 AND 4000000),
    max_output_tokens  bigint NOT NULL CONSTRAINT model_settings_revisions_output CHECK (max_output_tokens BETWEEN 1 AND 1000000),
    recording          boolean NOT NULL,
    updated_at         timestamptz NOT NULL,
    updated_by         text NOT NULL CONSTRAINT model_settings_revisions_updated_by CHECK (char_length(updated_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, project_id, revision),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE TABLE model_settings (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    revision           bigint NOT NULL,
    enabled            boolean NOT NULL,
    endpoint           text NOT NULL,
    credential_ref     text NOT NULL,
    credential_version bigint NOT NULL,
    models             text[] NOT NULL,
    max_requests       bigint NOT NULL,
    token_allowance    bigint NOT NULL,
    max_request_bytes  bigint NOT NULL,
    max_input_tokens   bigint NOT NULL,
    max_output_tokens  bigint NOT NULL,
    recording          boolean NOT NULL,
    updated_at         timestamptz NOT NULL,
    updated_by         text NOT NULL,
    PRIMARY KEY (tenant_id, project_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, revision) REFERENCES model_settings_revisions (tenant_id, project_id, revision)
);

CREATE FUNCTION t032_settings_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'model_settings_revisions' THEN
        RAISE EXCEPTION 'settings revisions are immutable' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'model settings are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION 'a settings revision advances by exactly one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER model_settings_guard BEFORE UPDATE OR DELETE ON model_settings
    FOR EACH ROW EXECUTE FUNCTION t032_settings_guard();

CREATE TRIGGER model_settings_revisions_guard BEFORE UPDATE OR DELETE ON model_settings_revisions
    FOR EACH ROW EXECUTE FUNCTION t032_settings_guard();

-- The run's policy binding: captured once at the run's first model admission.
CREATE TABLE model_run_bindings (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    settings_revision  bigint NOT NULL,
    endpoint           text NOT NULL,
    credential_ref     text NOT NULL,
    credential_version bigint NOT NULL,
    models             text[] NOT NULL,
    max_requests       bigint NOT NULL,
    token_allowance    bigint NOT NULL,
    max_request_bytes  bigint NOT NULL,
    max_input_tokens   bigint NOT NULL,
    max_output_tokens  bigint NOT NULL,
    recording          boolean NOT NULL,
    requests_admitted  bigint NOT NULL DEFAULT 0
                       CONSTRAINT model_run_bindings_requests_range CHECK (requests_admitted BETWEEN 0 AND 100000),
    tokens_charged     bigint NOT NULL DEFAULT 0
                       CONSTRAINT model_run_bindings_tokens_range CHECK (tokens_charged BETWEEN 0 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, settings_revision) REFERENCES model_settings_revisions (tenant_id, project_id, revision),
    CONSTRAINT model_run_bindings_cap CHECK (requests_admitted <= max_requests)
);

CREATE FUNCTION t032_binding_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run bindings are retained' USING ERRCODE = '23514';
    END IF;
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.settings_revision, NEW.endpoint, NEW.credential_ref, NEW.credential_version,
        NEW.models, NEW.max_requests, NEW.token_allowance, NEW.max_request_bytes, NEW.max_input_tokens, NEW.max_output_tokens,
        NEW.recording, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.settings_revision, OLD.endpoint, OLD.credential_ref, OLD.credential_version,
        OLD.models, OLD.max_requests, OLD.token_allowance, OLD.max_request_bytes, OLD.max_input_tokens, OLD.max_output_tokens,
        OLD.recording, OLD.created_at) THEN
        RAISE EXCEPTION 'a run binding is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.requests_admitted < OLD.requests_admitted OR NEW.requests_admitted > OLD.requests_admitted + 1
       OR NEW.tokens_charged < OLD.tokens_charged THEN
        RAISE EXCEPTION 'run counters only increase' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER model_run_bindings_guard BEFORE UPDATE OR DELETE ON model_run_bindings
    FOR EACH ROW EXECUTE FUNCTION t032_binding_guard();

CREATE TABLE model_requests (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    ordinal            bigint NOT NULL CONSTRAINT model_requests_ordinal_range CHECK (ordinal BETWEEN 1 AND 100000),
    model              text NOT NULL CONSTRAINT model_requests_model_syntax CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$'),
    input_bound        bigint NOT NULL CONSTRAINT model_requests_input_range CHECK (input_bound BETWEEN 0 AND 4000000),
    output_max         bigint NOT NULL CONSTRAINT model_requests_output_range CHECK (output_max BETWEEN 1 AND 1000000),
    charge_bound       bigint NOT NULL CONSTRAINT model_requests_bound_range CHECK (charge_bound BETWEEN 1 AND 5000000),
    recording          boolean NOT NULL,
    recording_outcome  text CONSTRAINT model_requests_recording_outcome CHECK (recording_outcome IN ('recorded', 'failed')),
    state              text NOT NULL
                       CONSTRAINT model_requests_state_known CHECK (state IN ('admitted', 'forwarding', 'succeeded', 'failed', 'cancelled', 'unknown', 'settled')),
    outcome_code       text CONSTRAINT model_requests_code CHECK (outcome_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    stream_started     boolean NOT NULL DEFAULT false,
    admitted_at        timestamptz NOT NULL,
    forwarding_at      timestamptz,
    finished_at        timestamptz,
    heartbeat_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES model_run_bindings (tenant_id, project_id, run_id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    CONSTRAINT model_requests_ordinal_unique UNIQUE (tenant_id, project_id, run_id, ordinal),
    CONSTRAINT model_requests_bound_sum CHECK (charge_bound = input_bound + output_max),
    CONSTRAINT model_requests_shape CHECK (
        (state = 'admitted' AND forwarding_at IS NULL AND finished_at IS NULL)
        OR (state = 'forwarding' AND forwarding_at IS NOT NULL AND finished_at IS NULL)
        OR (state IN ('succeeded', 'failed', 'cancelled', 'unknown', 'settled') AND finished_at IS NOT NULL AND outcome_code IS NOT NULL))
);

-- At most one unresolved request per run, across all replicas: a request in
-- flight and a request whose remote outcome is unknown both hold the slot.
CREATE UNIQUE INDEX model_requests_one_unresolved ON model_requests (tenant_id, project_id, run_id)
    WHERE state IN ('admitted', 'forwarding', 'unknown');

CREATE INDEX model_requests_attempt_idx ON model_requests (tenant_id, project_id, attempt_id);

CREATE INDEX model_requests_abandoned_idx ON model_requests (tenant_id, heartbeat_at) WHERE state IN ('admitted', 'forwarding');

-- The attempt must belong to the request's run (the attempt table's own key
-- cannot be referenced with the run column).
CREATE FUNCTION t032_request_owner_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM step_attempts a WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id
                         AND a.id = NEW.attempt_id AND a.run_id = NEW.run_id) THEN
            RAISE EXCEPTION 'the attempt does not belong to the run' USING ERRCODE = '23503';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'model requests are retained' USING ERRCODE = '23514';
    END IF;
    IF (NEW.tenant_id, NEW.project_id, NEW.id, NEW.run_id, NEW.attempt_id, NEW.ordinal, NEW.model, NEW.input_bound, NEW.output_max,
        NEW.charge_bound, NEW.recording, NEW.admitted_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.id, OLD.run_id, OLD.attempt_id, OLD.ordinal, OLD.model, OLD.input_bound, OLD.output_max,
        OLD.charge_bound, OLD.recording, OLD.admitted_at) THEN
        RAISE EXCEPTION 'a model request identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
        (OLD.state = 'admitted' AND NEW.state IN ('forwarding', 'failed'))
        OR (OLD.state = 'forwarding' AND NEW.state IN ('succeeded', 'failed', 'cancelled', 'unknown'))
        OR (OLD.state = 'unknown' AND NEW.state = 'settled')) THEN
        RAISE EXCEPTION 'illegal model request transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    IF OLD.state IN ('succeeded', 'failed', 'cancelled', 'settled') AND
       (NEW.state, NEW.outcome_code, NEW.stream_started, NEW.finished_at) IS DISTINCT FROM (OLD.state, OLD.outcome_code, OLD.stream_started, OLD.finished_at) THEN
        RAISE EXCEPTION 'a settled model request is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.recording_outcome IS NOT NULL AND NEW.recording_outcome IS DISTINCT FROM OLD.recording_outcome THEN
        RAISE EXCEPTION 'a recording outcome is written once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER model_requests_guard BEFORE INSERT OR UPDATE OR DELETE ON model_requests
    FOR EACH ROW EXECUTE FUNCTION t032_request_owner_guard();

-- Exactly one usage record per request. It carries identifiers, the requested
-- and the provider-confirmed model, the actual destination, numeric usage and
-- the uncertainty code: never content.
CREATE TABLE model_usage (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    request_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    requested_model    text NOT NULL,
    actual_model       text,
    model_confirmed    boolean NOT NULL,
    destination_origin text NOT NULL CONSTRAINT model_usage_origin CHECK (char_length(destination_origin) BETWEEN 1 AND 300),
    destination_ip     text CONSTRAINT model_usage_ip CHECK (char_length(destination_ip) <= 64),
    input_tokens       bigint CONSTRAINT model_usage_input_range CHECK (input_tokens BETWEEN 0 AND 9007199254740991),
    output_tokens      bigint CONSTRAINT model_usage_output_range CHECK (output_tokens BETWEEN 0 AND 9007199254740991),
    charged_tokens     bigint NOT NULL CONSTRAINT model_usage_charged_range CHECK (charged_tokens BETWEEN 0 AND 9007199254740991),
    basis              text NOT NULL CONSTRAINT model_usage_basis CHECK (basis IN ('observed', 'estimated')),
    uncertainty        text NOT NULL CONSTRAINT model_usage_uncertainty CHECK (uncertainty ~ '^[a-z][a-z0-9_]{0,63}$'),
    outcome            text NOT NULL CONSTRAINT model_usage_outcome CHECK (outcome IN ('succeeded', 'failed', 'cancelled', 'unknown')),
    outcome_code       text NOT NULL CONSTRAINT model_usage_code CHECK (outcome_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, request_id),
    FOREIGN KEY (tenant_id, project_id, request_id) REFERENCES model_requests (tenant_id, project_id, id),
    CONSTRAINT model_usage_confirmed CHECK ((model_confirmed AND actual_model IS NOT NULL) OR (NOT model_confirmed AND actual_model IS NULL)),
    CONSTRAINT model_usage_observed CHECK (basis <> 'observed' OR (input_tokens IS NOT NULL AND output_tokens IS NOT NULL AND uncertainty = 'none'))
);

CREATE INDEX model_usage_run_idx ON model_usage (tenant_id, project_id, run_id);

CREATE FUNCTION t032_usage_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage records are immutable' USING ERRCODE = '23514';
END $$;

CREATE TRIGGER model_usage_guard BEFORE UPDATE OR DELETE ON model_usage
    FOR EACH ROW EXECUTE FUNCTION t032_usage_guard();

ALTER TABLE model_settings_revisions ENABLE ROW LEVEL SECURITY;

ALTER TABLE model_settings_revisions FORCE ROW LEVEL SECURITY;

ALTER TABLE model_settings           ENABLE ROW LEVEL SECURITY;

ALTER TABLE model_settings           FORCE ROW LEVEL SECURITY;

ALTER TABLE model_run_bindings       ENABLE ROW LEVEL SECURITY;

ALTER TABLE model_run_bindings       FORCE ROW LEVEL SECURITY;

ALTER TABLE model_requests           ENABLE ROW LEVEL SECURITY;

ALTER TABLE model_requests           FORCE ROW LEVEL SECURITY;

ALTER TABLE model_usage              ENABLE ROW LEVEL SECURITY;

ALTER TABLE model_usage              FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON model_settings_revisions TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON model_settings           TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON model_run_bindings       TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON model_requests           TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON model_usage              TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON model_settings_revisions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON model_settings
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON model_run_bindings
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON model_requests
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON model_usage
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- MCP proxy (was T033_001_mcp_proxy.sql) ----

-- T-033 MCP proxy: project registrations of approved remote MCP servers, the
-- run bindings that freeze a registration revision and exact credential
-- version, caller-bound remote sessions, tool-call receipts and the run-wide
-- call counter.
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration, and minimal
-- runtime grants (column-level UPDATE; no DELETE anywhere).
--
-- No relation holds a credential value, a tool argument, a tool result or a
-- remote session identifier: registrations reference an MCP-kind secret by
-- name and exact version, receipts hold identifiers and a classified outcome.
-- run_id has no foreign key: the run tables belong to the execution contribution
-- and the integration owner may add the ownership constraint after it.

CREATE TABLE mcp_servers (
    tenant_id      kodama_id NOT NULL REFERENCES tenants (id),
    project_id     kodama_id NOT NULL,
    id             kodama_id NOT NULL,
    endpoint       text NOT NULL
                   CONSTRAINT mcp_servers_endpoint_shape CHECK (
                       char_length(endpoint) BETWEEN 9 AND 512 AND endpoint ~ '^https://[^@?#[:space:]]+$'),
    secret_name    text NOT NULL
                   CONSTRAINT mcp_servers_secret_name_syntax CHECK (secret_name ~ '^[a-z][a-z0-9-]{0,62}$'),
    secret_version bigint NOT NULL
                   CONSTRAINT mcp_servers_secret_version_range CHECK (secret_version BETWEEN 1 AND 9007199254740991),
    tools          text[] NOT NULL
                   CONSTRAINT mcp_servers_tools_bound CHECK (cardinality(tools) <= 128),
    version        bigint NOT NULL
                   CONSTRAINT mcp_servers_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_by     text NOT NULL CONSTRAINT mcp_servers_created_by_length CHECK (char_length(created_by) BETWEEN 1 AND 256),
    updated_by     text NOT NULL CONSTRAINT mcp_servers_updated_by_length CHECK (char_length(updated_by) BETWEEN 1 AND 256),
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    deleted_at     timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE INDEX mcp_servers_project_idx ON mcp_servers (tenant_id, project_id, id) WHERE deleted_at IS NULL;

CREATE FUNCTION t033_servers_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'registration ownership and identity are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL THEN
        RAISE EXCEPTION 'a deleted registration cannot change' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'the registration version advances by exactly one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER mcp_servers_guard BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION t033_servers_guard();

-- The first authorized use of a registration by a run freezes the registration
-- revision and the exact credential version. A later registration change makes
-- the binding unusable instead of silently replacing the credential.
CREATE TABLE mcp_run_bindings (
    tenant_id      kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    run_id         kodama_id NOT NULL,
    server_id      kodama_id NOT NULL,
    server_version bigint NOT NULL,
    secret_name    text NOT NULL,
    secret_version bigint NOT NULL,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, run_id, server_id),
    FOREIGN KEY (tenant_id, project_id, server_id) REFERENCES mcp_servers (tenant_id, project_id, id)
);

CREATE TABLE mcp_sessions (
    tenant_id      kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    id             kodama_id NOT NULL,
    run_id         kodama_id NOT NULL,
    attempt_id     kodama_id NOT NULL,
    runtime_session text NOT NULL CONSTRAINT mcp_sessions_runtime_session_length CHECK (char_length(runtime_session) BETWEEN 1 AND 256),
    server_id      kodama_id NOT NULL,
    server_version bigint NOT NULL,
    secret_name    text NOT NULL,
    secret_version bigint NOT NULL,
    protocol       text NOT NULL CONSTRAINT mcp_sessions_protocol_length CHECK (char_length(protocol) BETWEEN 1 AND 32),
    incarnation    text NOT NULL CONSTRAINT mcp_sessions_incarnation_length CHECK (char_length(incarnation) BETWEEN 1 AND 128),
    state          text NOT NULL
                   CONSTRAINT mcp_sessions_state_known CHECK (state IN ('initializing', 'active', 'failed', 'closed', 'expired', 'invalid')),
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    CONSTRAINT mcp_sessions_lifetime CHECK (expires_at > created_at),
    FOREIGN KEY (tenant_id, project_id, server_id) REFERENCES mcp_servers (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, run_id, server_id) REFERENCES mcp_run_bindings (tenant_id, run_id, server_id)
);

CREATE INDEX mcp_sessions_live_idx ON mcp_sessions (tenant_id, expires_at) WHERE state IN ('initializing', 'active');

CREATE FUNCTION t033_sessions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id OR NEW.runtime_session <> OLD.runtime_session
       OR NEW.server_id <> OLD.server_id OR NEW.server_version <> OLD.server_version
       OR NEW.secret_name <> OLD.secret_name OR NEW.secret_version <> OLD.secret_version
       OR NEW.protocol <> OLD.protocol OR NEW.incarnation <> OLD.incarnation
       OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at THEN
        RAISE EXCEPTION 'a session binding is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state = OLD.state THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'initializing' AND NEW.state IN ('active', 'failed', 'closed', 'invalid', 'expired') THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'active' AND NEW.state IN ('closed', 'invalid', 'expired') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'a % session cannot become %', OLD.state, NEW.state USING ERRCODE = '23514';
END $$;

CREATE TRIGGER mcp_sessions_guard BEFORE UPDATE ON mcp_sessions
    FOR EACH ROW EXECUTE FUNCTION t033_sessions_guard();

CREATE TABLE mcp_invocations (
    tenant_id  kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    session_id kodama_id NOT NULL,
    id         text NOT NULL CONSTRAINT mcp_invocations_id_syntax CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'),
    run_id     kodama_id NOT NULL,
    attempt_id kodama_id NOT NULL,
    server_id  kodama_id NOT NULL,
    tool       text NOT NULL CONSTRAINT mcp_invocations_tool_syntax CHECK (tool ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'),
    state      text NOT NULL
               CONSTRAINT mcp_invocations_state_known CHECK (state IN ('dispatching', 'succeeded', 'failed', 'unknown')),
    code       text NOT NULL DEFAULT '' CONSTRAINT mcp_invocations_code_length CHECK (char_length(code) <= 64),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, session_id, id),
    FOREIGN KEY (tenant_id, project_id, session_id) REFERENCES mcp_sessions (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, server_id) REFERENCES mcp_servers (tenant_id, project_id, id)
);

CREATE INDEX mcp_invocations_stale_idx ON mcp_invocations (tenant_id, updated_at) WHERE state = 'dispatching';

CREATE FUNCTION t033_invocations_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.session_id <> OLD.session_id
       OR NEW.id <> OLD.id OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id
       OR NEW.server_id <> OLD.server_id OR NEW.tool <> OLD.tool OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a call receipt identity is immutable' USING ERRCODE = '23514';
    END IF;
    -- A dispatched call has exactly one terminal outcome, and it never changes:
    -- an unknown outcome is never rewritten into success or failure.
    IF OLD.state <> 'dispatching' THEN
        RAISE EXCEPTION 'a % call outcome is final', OLD.state USING ERRCODE = '23514';
    END IF;
    IF NEW.state NOT IN ('succeeded', 'failed', 'unknown') THEN
        RAISE EXCEPTION 'a dispatching call ends as succeeded, failed or unknown' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER mcp_invocations_guard BEFORE UPDATE ON mcp_invocations
    FOR EACH ROW EXECUTE FUNCTION t033_invocations_guard();

-- Admitted calls per run. Calls are charged at admission and never refunded, so
-- a retry never resets the claimed ceiling and an uncertain call stays charged.
CREATE TABLE mcp_run_calls (
    tenant_id  kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id     kodama_id NOT NULL,
    calls      bigint NOT NULL CONSTRAINT mcp_run_calls_range CHECK (calls BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, run_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

ALTER TABLE mcp_servers      ENABLE ROW LEVEL SECURITY;

ALTER TABLE mcp_servers      FORCE ROW LEVEL SECURITY;

ALTER TABLE mcp_run_bindings ENABLE ROW LEVEL SECURITY;

ALTER TABLE mcp_run_bindings FORCE ROW LEVEL SECURITY;

ALTER TABLE mcp_sessions     ENABLE ROW LEVEL SECURITY;

ALTER TABLE mcp_sessions     FORCE ROW LEVEL SECURITY;

ALTER TABLE mcp_invocations  ENABLE ROW LEVEL SECURITY;

ALTER TABLE mcp_invocations  FORCE ROW LEVEL SECURITY;

ALTER TABLE mcp_run_calls    ENABLE ROW LEVEL SECURITY;

ALTER TABLE mcp_run_calls    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON mcp_servers      TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON mcp_run_bindings TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON mcp_sessions     TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON mcp_invocations  TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON mcp_run_calls    TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON mcp_servers
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON mcp_run_bindings
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON mcp_sessions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON mcp_invocations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON mcp_run_calls
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- work dependencies (was T045_001_work_dependencies.sql) ----

-- T-045 business dependency graph: same-project prerequisite links between work
-- items.
--
-- A row says "dependent_id cannot start until prerequisite_id is done". It is a
-- business edge, not a pipeline step dependency. Follows the base conventions:
-- opaque kodama_id keys, tenant and project ownership through composite foreign
-- keys to the scoped work item key, ENABLE + FORCE row level security on
-- kodama_tenant_id(), and minimal runtime grants. Cycle rejection is not a
-- constraint (a graph property is not row-local): every insertion runs under the
-- project row lock and checks reachability in the same transaction.

CREATE TABLE work_item_dependencies (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    dependent_id     kodama_id NOT NULL,
    prerequisite_id  kodama_id NOT NULL,
    -- The history entry (and therefore the owner version) that created the link.
    history_id       kodama_id NOT NULL,
    created_by_type  text NOT NULL
                     CONSTRAINT work_item_dependencies_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id    text NOT NULL
                     CONSTRAINT work_item_dependencies_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, dependent_id, prerequisite_id),
    CONSTRAINT work_item_dependencies_not_self CHECK (dependent_id <> prerequisite_id),
    -- Both endpoints must be work items of the same tenant and project. There is
    -- deliberately no ON DELETE action: work items and history are never
    -- cascade-deleted.
    CONSTRAINT work_item_dependencies_dependent_fkey
        FOREIGN KEY (tenant_id, project_id, dependent_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_item_dependencies_prerequisite_fkey
        FOREIGN KEY (tenant_id, project_id, prerequisite_id) REFERENCES work_items (tenant_id, project_id, id)
);

-- The primary key serves prerequisite listing and the recursive reachability
-- walk (dependent -> prerequisites); this index serves the reverse direction.
CREATE INDEX work_item_dependencies_reverse_idx
    ON work_item_dependencies (tenant_id, project_id, prerequisite_id, dependent_id);

ALTER TABLE work_item_dependencies ENABLE ROW LEVEL SECURITY;

ALTER TABLE work_item_dependencies FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON work_item_dependencies TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON work_item_dependencies
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- flow summaries (was T042_001_flow_intervals.sql) ----

-- T-042 flow summaries: history-derived, non-materialized projections.
--
-- Everything is derived on read from the immutable T-011 work_item_history rows;
-- no table, trigger, worker or cached state is created, so there is nothing to
-- reconcile after a failure. The views are SECURITY INVOKER: they run with the
-- privileges and row level security of the caller (the runtime role under
-- kodama_tenant_id()), never of their owner. Every window partitions by
-- (tenant_id, project_id, work_item_id), so a tenant, project or item predicate
-- on the view is pushed below the window functions and only the selected items'
-- history is processed.
--
-- Semantics are documented in internal/metrics/README.md. In short:
--   * history resource `version` is the only ordering;
--   * the state after a row is its to_state, the blocked flag after a row is the
--     latest explicit after_values.blockedReason (string = set, JSON null =
--     cleared, key absent = unchanged);
--   * a segment is the time between a row and the next row of the same item; the
--     last segment is open (ended_at is NULL);
--   * done and cancelled segments do not accrue lead, active, review or blocked time;
--   * an episode starts at creation or at done -> active (a reopen) and ends at
--     the next done or cancelled transition;
--   * a row whose history cannot be a T-011 chain carries a non-NULL anomaly and
--     the consumer reports the item as invalid instead of inventing durations.

-- One row per history row with the carried state and blocked flag and a
-- chain-integrity anomaly.
CREATE VIEW work_flow_steps WITH (security_invoker = true) AS
SELECT s.tenant_id, s.project_id, s.work_item_id, s.version, s.history_id, s.change_kind,
       s.occurred_at, s.from_state, s.to_state AS state,
       coalesce(first_value(CASE s.block_type WHEN 'string' THEN true WHEN 'null' THEN false END)
                    OVER (PARTITION BY s.tenant_id, s.project_id, s.work_item_id, s.block_group ORDER BY s.version),
                false) AS is_blocked,
       CASE
           WHEN s.to_state IS NULL THEN 'state_unknown'
           WHEN s.block_type IS NOT NULL AND s.block_type NOT IN ('string', 'null') THEN 'blocked_value'
           WHEN s.prev_version IS NULL AND (s.version <> 1 OR s.change_kind <> 'created') THEN 'first_not_created'
           WHEN s.prev_version IS NULL THEN NULL
           WHEN s.change_kind = 'created' THEN 'duplicate_created'
           WHEN s.version <> s.prev_version + 1 THEN 'version_gap'
           WHEN s.occurred_at < s.prev_at THEN 'time_backwards'
           WHEN s.from_state IS DISTINCT FROM s.prev_state THEN 'state_chain'
           WHEN s.change_kind = 'updated' AND s.to_state <> s.from_state THEN 'state_chain'
           WHEN s.change_kind = 'transitioned' AND s.to_state = s.from_state THEN 'state_chain'
           WHEN s.prev_state = 'cancelled' THEN 'state_chain'
           WHEN s.prev_state = 'done' AND NOT (s.change_kind = 'transitioned' AND s.to_state = 'active') THEN 'state_chain'
       END AS anomaly
FROM (
    SELECT h.tenant_id, h.project_id, h.work_item_id, h.version, h.id AS history_id, h.change_kind,
           h.occurred_at, h.from_state, h.to_state,
           CASE WHEN h.after_values ? 'blockedReason' THEN jsonb_typeof(h.after_values -> 'blockedReason') END AS block_type,
           count(*) FILTER (WHERE h.after_values ? 'blockedReason')
               OVER (PARTITION BY h.tenant_id, h.project_id, h.work_item_id ORDER BY h.version) AS block_group,
           lag(h.version)     OVER w AS prev_version,
           lag(h.occurred_at) OVER w AS prev_at,
           lag(h.to_state)    OVER w AS prev_state
    FROM work_item_history h
    WINDOW w AS (PARTITION BY h.tenant_id, h.project_id, h.work_item_id ORDER BY h.version)
) s;

-- One row per step with the interval to the next step and its episode.
CREATE VIEW work_flow_segments WITH (security_invoker = true) AS
SELECT g.tenant_id, g.project_id, g.work_item_id, g.version, g.history_id, g.change_kind,
       g.occurred_at, g.from_state, g.state, g.is_blocked, g.anomaly,
       g.ended_at, g.ended_version, g.accrues, g.episode_no,
       (first_value(g.change_kind) OVER (PARTITION BY g.tenant_id, g.project_id, g.work_item_id, g.episode_no ORDER BY g.version)
            = 'created') AS episode_initial,
       (g.change_kind = 'transitioned' AND g.from_state = 'done' AND g.state = 'active') AS is_reopen
FROM (
    SELECT p.*,
           lead(p.occurred_at) OVER w AS ended_at,
           lead(p.version)     OVER w AS ended_version,
           (p.state NOT IN ('done', 'cancelled')) AS accrues,
           count(*) FILTER (WHERE p.change_kind = 'created'
                              OR (p.change_kind = 'transitioned' AND p.from_state = 'done' AND p.state = 'active'))
               OVER (PARTITION BY p.tenant_id, p.project_id, p.work_item_id ORDER BY p.version) AS episode_no
    FROM work_flow_steps p
    WINDOW w AS (PARTITION BY p.tenant_id, p.project_id, p.work_item_id ORDER BY p.version)
) g;

-- Unfinished episodes: created -> done/cancelled, and each reopen -> done/cancelled.
-- ended_at/outcome are NULL while the episode is unfinished at the end of history.
CREATE VIEW work_flow_episodes WITH (security_invoker = true) AS
SELECT s.tenant_id, s.project_id, s.work_item_id, s.episode_no,
       CASE WHEN bool_or(s.episode_initial) THEN 'initial' ELSE 'reopened' END AS kind,
       min(s.occurred_at) AS started_at,
       min(s.version) AS started_version,
       min(s.occurred_at) FILTER (WHERE NOT s.accrues) AS ended_at,
       min(s.version) FILTER (WHERE NOT s.accrues) AS ended_version,
       (array_agg(s.state ORDER BY s.version) FILTER (WHERE NOT s.accrues))[1] AS outcome
FROM work_flow_segments s
GROUP BY s.tenant_id, s.project_id, s.work_item_id, s.episode_no;

-- Maximal runs of consecutive unfinished segments in which a blocked reason was
-- recorded. Replacing the reason or editing other fields does not split a run;
-- a terminal state does, and a reopen starts a new run when the reason is still
-- recorded. ended_at is NULL for a run still open at the end of history.
CREATE VIEW work_flow_blocked_runs WITH (security_invoker = true) AS
SELECT r.tenant_id, r.project_id, r.work_item_id, r.run_no,
       min(r.episode_no) AS episode_no,
       min(r.occurred_at) AS started_at,
       min(r.version) AS started_version,
       (array_agg(r.ended_at ORDER BY r.version DESC))[1] AS ended_at,
       (array_agg(r.ended_version ORDER BY r.version DESC))[1] AS ended_version
FROM (
    SELECT f.*,
           count(*) FILTER (WHERE f.blocked AND NOT coalesce(f.prev_blocked, false))
               OVER (PARTITION BY f.tenant_id, f.project_id, f.work_item_id ORDER BY f.version) AS run_no
    FROM (
        SELECT s.tenant_id, s.project_id, s.work_item_id, s.version, s.occurred_at, s.ended_at, s.ended_version, s.episode_no,
               (s.accrues AND s.is_blocked) AS blocked,
               lag(s.accrues AND s.is_blocked) OVER (PARTITION BY s.tenant_id, s.project_id, s.work_item_id ORDER BY s.version) AS prev_blocked
        FROM work_flow_segments s
    ) f
) r
WHERE r.blocked
GROUP BY r.tenant_id, r.project_id, r.work_item_id, r.run_no;

-- ---- ChatOps (was T041_001_chatops.sql) ----

-- T-041 ChatOps schema: project-scoped generic webhook destinations, durable
-- notification deliveries with their complete attempt history, replay audit
-- metadata and the per-tenant restore fence.
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration and restore, and
-- minimal runtime grants (column-level UPDATE; no DELETE anywhere: unresolved
-- deliveries and their deduplication identities are never pruned
-- opportunistically).
--
-- Ordered after the base, event and identity contributions: it references
-- tenants and projects only; the event bus delivers into it through the
-- transaction of its own inbox.
--
-- No credential value, URL secret, response body or arbitrary event content is
-- stored. A destination holds a credential *reference* only.

-- ---- destinations -----------------------------------------------------------

CREATE TABLE chatops_destinations (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    name             text NOT NULL
                     CONSTRAINT chatops_destinations_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    endpoint         text NOT NULL
                     CONSTRAINT chatops_destinations_endpoint_shape CHECK (
                         char_length(endpoint) BETWEEN 9 AND 2048 AND endpoint ~ '^https://[^\s@/?#]+(/[^\s?#]*)?$'),
    events           text[] NOT NULL
                     CONSTRAINT chatops_destinations_events_known CHECK (
                         cardinality(events) BETWEEN 1 AND 16 AND events <@ ARRAY[
                             'work_item.transitioned', 'gate.requested', 'gate.decided', 'gate.invalidated',
                             'run.state_changed', 'deployment.state_changed']::text[]),
    -- A credential reference (never a value); empty means a credential-free receiver.
    credential       text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_destinations_credential_syntax CHECK (
                         credential = '' OR credential ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
    enabled          boolean NOT NULL,
    -- Advances when the endpoint or the credential reference changes: deliveries
    -- record the revision they were created under.
    binding_revision bigint NOT NULL
                     CONSTRAINT chatops_destinations_binding_range CHECK (binding_revision BETWEEN 1 AND 9007199254740991),
    version          bigint NOT NULL
                     CONSTRAINT chatops_destinations_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    CONSTRAINT chatops_destinations_name_unique UNIQUE (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE FUNCTION t041_destinations_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'destination identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'destination version must increase' USING ERRCODE = '23514';
    END IF;
    -- The binding revision moves exactly when the endpoint or credential does.
    IF (NEW.endpoint <> OLD.endpoint OR NEW.credential <> OLD.credential) THEN
        IF NEW.binding_revision <> OLD.binding_revision + 1 THEN
            RAISE EXCEPTION 'a changed endpoint or credential advances the binding revision' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.binding_revision <> OLD.binding_revision THEN
        RAISE EXCEPTION 'the binding revision changes only with the endpoint or credential' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER chatops_destinations_guard BEFORE UPDATE ON chatops_destinations
    FOR EACH ROW EXECUTE FUNCTION t041_destinations_guard();

-- ---- deliveries -------------------------------------------------------------

-- One row per (destination, event): the durable intent, created in the event
-- bus's inbox transaction before any external call.
CREATE TABLE chatops_deliveries (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    destination_id   kodama_id NOT NULL,
    event_id         text NOT NULL
                     CONSTRAINT chatops_deliveries_event_id_length CHECK (char_length(event_id) BETWEEN 1 AND 128),
    event_type       text NOT NULL
                     CONSTRAINT chatops_deliveries_event_type_known CHECK (event_type IN (
                         'work_item.transitioned', 'gate.requested', 'gate.decided', 'gate.invalidated',
                         'run.state_changed', 'deployment.state_changed')),
    binding_revision bigint NOT NULL
                     CONSTRAINT chatops_deliveries_binding_range CHECK (binding_revision BETWEEN 1 AND 9007199254740991),
    -- The exact bytes sent on every attempt.
    payload          bytea NOT NULL
                     CONSTRAINT chatops_deliveries_payload_bound CHECK (octet_length(payload) BETWEEN 2 AND 8192),
    payload_digest   text NOT NULL
                     CONSTRAINT chatops_deliveries_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    state            text NOT NULL
                     CONSTRAINT chatops_deliveries_state_known CHECK (state IN ('pending', 'claimed', 'delivered', 'failed')),
    attempts         integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_attempts_range CHECK (attempts BETWEEN 0 AND 100000),
    cycle_attempts   integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_cycle_range CHECK (cycle_attempts BETWEEN 0 AND attempts),
    replays          integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_replays_range CHECK (replays BETWEEN 0 AND 100000),
    next_attempt_at  timestamptz NOT NULL,
    claim_token      bigint NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_token_range CHECK (claim_token BETWEEN 0 AND 9007199254740991),
    lease_expires_at timestamptz,
    last_class       text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_class_known CHECK (last_class IN (
                         '', 'unreachable', 'timeout', 'connection_lost', 'bad_response', 'receiver_error', 'rate_limited', 'rejected',
                         'redirect_refused', 'policy_refused', 'credential_unavailable', 'credential_source_unavailable',
                         'destination_changed', 'lease_expired', 'internal')),
    last_uncertain   boolean NOT NULL DEFAULT false,
    last_status      integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_status_range CHECK (last_status BETWEEN 0 AND 599),
    receipt          text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_receipt_shape CHECK (receipt ~ '^[\x21-\x7e]{0,128}$'),
    delivered_at     timestamptz,
    failed_reason    text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_failed_reason_known CHECK (failed_reason IN ('', 'permanent', 'exhausted', 'refused')),
    version          bigint NOT NULL
                     CONSTRAINT chatops_deliveries_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    -- The delivery identity: one intent per destination and event, ever.
    CONSTRAINT chatops_deliveries_identity UNIQUE (tenant_id, project_id, destination_id, event_id),
    CONSTRAINT chatops_deliveries_delivered_consistent CHECK ((state = 'delivered') = (delivered_at IS NOT NULL)),
    CONSTRAINT chatops_deliveries_claim_consistent CHECK ((state = 'claimed') = (lease_expires_at IS NOT NULL)),
    CONSTRAINT chatops_deliveries_failed_consistent CHECK ((state = 'failed') = (failed_reason <> '')),
    FOREIGN KEY (tenant_id, project_id, destination_id) REFERENCES chatops_destinations (tenant_id, project_id, id)
);

CREATE INDEX chatops_deliveries_due_idx ON chatops_deliveries (tenant_id, next_attempt_at) WHERE state = 'pending';

CREATE INDEX chatops_deliveries_lease_idx ON chatops_deliveries (tenant_id, lease_expires_at) WHERE state = 'claimed';

CREATE INDEX chatops_deliveries_list_idx ON chatops_deliveries (tenant_id, project_id, created_at DESC, id DESC);

CREATE INDEX chatops_deliveries_destination_idx ON chatops_deliveries (tenant_id, project_id, destination_id, state);

CREATE FUNCTION t041_deliveries_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.destination_id <> OLD.destination_id OR NEW.event_id <> OLD.event_id OR NEW.event_type <> OLD.event_type
       OR NEW.payload <> OLD.payload OR NEW.payload_digest <> OLD.payload_digest OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'delivery identity and content are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'delivery version must increase' USING ERRCODE = '23514';
    END IF;
    IF NEW.attempts < OLD.attempts OR NEW.claim_token < OLD.claim_token OR NEW.replays < OLD.replays THEN
        RAISE EXCEPTION 'delivery counters and fencing tokens never decrease' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'delivered' THEN
        RAISE EXCEPTION 'a delivered notification is final' USING ERRCODE = '23514';
    END IF;
    IF NEW.binding_revision <> OLD.binding_revision AND NOT (OLD.state = 'failed' AND NEW.state = 'pending') THEN
        RAISE EXCEPTION 'a delivery is re-bound only by an authorized replay' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'pending' THEN
        IF NEW.state NOT IN ('claimed', 'failed') THEN
            RAISE EXCEPTION 'invalid delivery transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
        END IF;
        IF NEW.state = 'claimed' AND (NEW.claim_token <> OLD.claim_token + 1 OR NEW.attempts <> OLD.attempts + 1) THEN
            RAISE EXCEPTION 'a claim takes the next fencing token and records an attempt' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.state = 'claimed' THEN
        IF NEW.state = 'claimed' THEN
            IF NEW.claim_token <> OLD.claim_token + 1 OR NEW.attempts <> OLD.attempts + 1 THEN
                RAISE EXCEPTION 'a reclaim takes the next fencing token and records an attempt' USING ERRCODE = '23514';
            END IF;
        ELSIF NEW.claim_token <> OLD.claim_token THEN
            RAISE EXCEPTION 'a finalization keeps the claim token it was issued under' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.state = 'failed' THEN
        IF NEW.state <> 'pending' OR NEW.replays <> OLD.replays + 1 OR NEW.cycle_attempts <> 0 OR NEW.attempts <> OLD.attempts THEN
            RAISE EXCEPTION 'a failed delivery changes only through an authorized replay' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER chatops_deliveries_guard BEFORE UPDATE ON chatops_deliveries
    FOR EACH ROW EXECUTE FUNCTION t041_deliveries_guard();

-- ---- attempts (append-only history, written before the receiver is called) ---

CREATE TABLE chatops_delivery_attempts (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    delivery_id kodama_id NOT NULL,
    attempt     integer NOT NULL CONSTRAINT chatops_attempts_number_range CHECK (attempt BETWEEN 1 AND 100000),
    claim_token bigint NOT NULL,
    replay      integer NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    result      text NOT NULL DEFAULT ''
                CONSTRAINT chatops_attempts_result_known CHECK (result IN ('', 'delivered', 'retry', 'failed', 'abandoned')),
    class       text NOT NULL DEFAULT '',
    uncertain   boolean NOT NULL DEFAULT false,
    http_status integer NOT NULL DEFAULT 0 CONSTRAINT chatops_attempts_status_range CHECK (http_status BETWEEN 0 AND 599),
    receipt     text NOT NULL DEFAULT '' CONSTRAINT chatops_attempts_receipt_shape CHECK (receipt ~ '^[\x21-\x7e]{0,128}$'),
    PRIMARY KEY (tenant_id, project_id, delivery_id, attempt),
    CONSTRAINT chatops_attempts_finished_consistent CHECK ((finished_at IS NULL) = (result = '')),
    FOREIGN KEY (tenant_id, project_id, delivery_id) REFERENCES chatops_deliveries (tenant_id, project_id, id)
);

CREATE FUNCTION t041_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'delivery attempt history is append-only' USING ERRCODE = '23514';
    END IF;
    IF OLD.finished_at IS NOT NULL THEN
        RAISE EXCEPTION 'a finished attempt is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.delivery_id <> OLD.delivery_id
       OR NEW.attempt <> OLD.attempt OR NEW.claim_token <> OLD.claim_token OR NEW.replay <> OLD.replay OR NEW.started_at <> OLD.started_at THEN
        RAISE EXCEPTION 'attempt identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER chatops_attempts_guard BEFORE UPDATE OR DELETE ON chatops_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION t041_attempts_guard();

-- ---- replay audit metadata ---------------------------------------------------

CREATE TABLE chatops_replays (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    delivery_id  kodama_id NOT NULL,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    actor_id     text NOT NULL CONSTRAINT chatops_replays_actor_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    request_id   text CONSTRAINT chatops_replays_request_length CHECK (char_length(request_id) <= 256),
    reason       text NOT NULL CONSTRAINT chatops_replays_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    from_version bigint NOT NULL,
    to_version   bigint NOT NULL,
    from_binding bigint NOT NULL,
    to_binding   bigint NOT NULL,
    occurred_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, delivery_id, seq),
    FOREIGN KEY (tenant_id, project_id, delivery_id) REFERENCES chatops_deliveries (tenant_id, project_id, id)
);

CREATE FUNCTION t041_replays_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'replay audit metadata is append-only' USING ERRCODE = '23514';
END $$;

CREATE TRIGGER chatops_replays_guard BEFORE UPDATE OR DELETE ON chatops_replays
    FOR EACH ROW EXECUTE FUNCTION t041_replays_guard();

-- ---- restore fence ------------------------------------------------------------

-- A row per tenant that was ever fenced. While fenced, no delivery is claimed.
-- T-005's consumer fence stops event replay; this stops deliveries that were
-- already created.
CREATE TABLE chatops_delivery_fences (
    tenant_id   kodama_id PRIMARY KEY REFERENCES tenants (id),
    fenced      boolean NOT NULL,
    reason      text NOT NULL CONSTRAINT chatops_fences_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    operator_id text NOT NULL CONSTRAINT chatops_fences_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    updated_at  timestamptz NOT NULL
);

-- ---- row level security ------------------------------------------------------

ALTER TABLE chatops_destinations       ENABLE ROW LEVEL SECURITY;

ALTER TABLE chatops_destinations       FORCE ROW LEVEL SECURITY;

ALTER TABLE chatops_deliveries         ENABLE ROW LEVEL SECURITY;

ALTER TABLE chatops_deliveries         FORCE ROW LEVEL SECURITY;

ALTER TABLE chatops_delivery_attempts  ENABLE ROW LEVEL SECURITY;

ALTER TABLE chatops_delivery_attempts  FORCE ROW LEVEL SECURITY;

ALTER TABLE chatops_replays            ENABLE ROW LEVEL SECURITY;

ALTER TABLE chatops_replays            FORCE ROW LEVEL SECURITY;

ALTER TABLE chatops_delivery_fences    ENABLE ROW LEVEL SECURITY;

ALTER TABLE chatops_delivery_fences    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON chatops_destinations      TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON chatops_deliveries        TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON chatops_delivery_attempts TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON chatops_replays           TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY owner_all ON chatops_delivery_fences   TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON chatops_destinations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON chatops_deliveries
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON chatops_delivery_attempts
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON chatops_replays
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_isolation ON chatops_delivery_fences
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- Git triggers (was T100_001_git_triggers.sql) ----

-- T-100 native Git trigger persistence: triggers, the durable delivery claim
-- and the admission ledger (success/refusal decided once per idempotency key).
--
-- Follows the base conventions (T002_001) and the module patterns of T-014 and
-- T-020: opaque kodama_id keys, ownership through composite foreign keys,
-- ENABLE + FORCE row level security on kodama_tenant_id(), minimal runtime
-- grants. State changes also append to the T-002 audit table; nothing here
-- emits an event.
--
-- Ordered after the base, pipeline definitions, work items and the execution
-- tables (runs): the trigger references its definition and work item, and the
-- delivery and ledger reference the run they started.

-- A trigger. The identifier is the hook's address (it arrives in a URL before
-- any tenant is known), so it is unique across tenants; every other key is
-- tenant and project scoped. kodama_git_trigger_locate below is the one narrow
-- path that turns an address into its owner.
CREATE TABLE git_triggers (
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    project_id    kodama_id NOT NULL,
    id            kodama_id NOT NULL,
    repository_id text NOT NULL
                  CONSTRAINT git_triggers_repository_length CHECK (char_length(repository_id) BETWEEN 1 AND 128),
    definition_id kodama_id NOT NULL,
    work_item_id  kodama_id,
    branches      text[] NOT NULL
                  CONSTRAINT git_triggers_branches_count CHECK (cardinality(branches) BETWEEN 1 AND 16),
    secret_ref    text NOT NULL
                  CONSTRAINT git_triggers_secret_ref_length CHECK (char_length(secret_ref) BETWEEN 1 AND 256),
    installed_by  text NOT NULL
                  CONSTRAINT git_triggers_installed_by_length CHECK (char_length(installed_by) BETWEEN 1 AND 256),
    enabled       boolean NOT NULL,
    version       bigint NOT NULL
                  CONSTRAINT git_triggers_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    CONSTRAINT git_triggers_address_key UNIQUE (id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, definition_id) REFERENCES pipeline_definitions (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id)
);

-- One delivery of one trigger. The primary key is the durable claim: the first
-- insert wins and every redelivery sees its record. Admission progress (the
-- admitting mark, the lease) and the single terminal outcome live on the row;
-- the guard below makes the outcome write-once.
CREATE TABLE git_trigger_deliveries (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    trigger_id   kodama_id NOT NULL,
    delivery_id  text NOT NULL
                 CONSTRAINT git_trigger_deliveries_delivery_syntax CHECK (delivery_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    body_digest  text NOT NULL
                 CONSTRAINT git_trigger_deliveries_digest_length CHECK (char_length(body_digest) BETWEEN 1 AND 128),
    ref          text NOT NULL
                 CONSTRAINT git_trigger_deliveries_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255),
    commit_id    text NOT NULL
                 CONSTRAINT git_trigger_deliveries_commit_syntax CHECK (commit_id ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    received_at  timestamptz NOT NULL,
    admitting    boolean NOT NULL DEFAULT false,
    lease_holder text
                 CONSTRAINT git_trigger_deliveries_lease_holder_length CHECK (char_length(lease_holder) BETWEEN 1 AND 256),
    lease_until  timestamptz,
    done         boolean NOT NULL DEFAULT false,
    outcome      text
                 CONSTRAINT git_trigger_deliveries_outcome_known CHECK (outcome IN ('started', 'ignored', 'rejected')),
    reason       text
                 CONSTRAINT git_trigger_deliveries_reason_length CHECK (char_length(reason) BETWEEN 1 AND 128),
    run_id       kodama_id,
    completed_at timestamptz,
    PRIMARY KEY (tenant_id, project_id, trigger_id, delivery_id),
    -- The trigger address is global, so the claim is also unique by address.
    CONSTRAINT git_trigger_deliveries_claim_key UNIQUE (trigger_id, delivery_id),
    FOREIGN KEY (tenant_id, project_id, trigger_id) REFERENCES git_triggers (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CONSTRAINT git_trigger_deliveries_lease_pair CHECK ((lease_holder IS NULL) = (lease_until IS NULL)),
    -- Only an admitting delivery can hold a lease, and a finished one holds none.
    CONSTRAINT git_trigger_deliveries_lease_admitting CHECK (lease_holder IS NULL OR (admitting AND NOT done)),
    -- A finished delivery has exactly one outcome; started is the only outcome
    -- with a run, and it can only follow the admitting mark.
    CONSTRAINT git_trigger_deliveries_done_shape CHECK (
        (NOT done AND outcome IS NULL AND reason IS NULL AND run_id IS NULL AND completed_at IS NULL)
        OR (done AND outcome IS NOT NULL AND completed_at IS NOT NULL
            AND ((outcome = 'started') = (run_id IS NOT NULL))
            AND (outcome = 'started' OR reason IS NOT NULL)
            AND (outcome <> 'started' OR admitting)))
);

CREATE INDEX git_trigger_deliveries_pending_idx ON git_trigger_deliveries (tenant_id, project_id, received_at) WHERE NOT done;

-- The admission ledger: one row per idempotency key of a scope, recording
-- either the run admission created or the refusal admission decided, never
-- both. Run creation inserts its row in the transaction that creates the run,
-- and a refusal is inserted under the same key lock, so after a refusal commits
-- no run can be created for the key and after a run commits no refusal can be
-- recorded. Rows are immutable: the first decision wins and survives restarts.
CREATE TABLE git_trigger_admissions (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    idempotency_key text NOT NULL
                    CONSTRAINT git_trigger_admissions_key_length CHECK (char_length(idempotency_key) BETWEEN 1 AND 512),
    run_id          kodama_id,
    refusal_reason  text
                    CONSTRAINT git_trigger_admissions_reason_length CHECK (char_length(refusal_reason) BETWEEN 1 AND 128),
    decided_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, project_id, idempotency_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    -- A run belongs to exactly one key.
    CONSTRAINT git_trigger_admissions_run_key UNIQUE (tenant_id, run_id),
    -- Success and refusal are mutually exclusive durable outcomes.
    CONSTRAINT git_trigger_admissions_exclusive CHECK ((run_id IS NULL) <> (refusal_reason IS NULL))
);

-- Guards, effective for every role including the owner.
CREATE FUNCTION git_triggers_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.repository_id <> OLD.repository_id OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'git trigger identity and repository binding are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'git trigger version must advance by one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_triggers_guard BEFORE UPDATE ON git_triggers
    FOR EACH ROW EXECUTE FUNCTION git_triggers_guard();

CREATE FUNCTION git_trigger_deliveries_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.trigger_id <> OLD.trigger_id
       OR NEW.delivery_id <> OLD.delivery_id OR NEW.body_digest <> OLD.body_digest OR NEW.ref <> OLD.ref
       OR NEW.commit_id <> OLD.commit_id OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'git trigger delivery identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.done THEN
        RAISE EXCEPTION 'a finished git trigger delivery is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.admitting AND NOT NEW.admitting THEN
        RAISE EXCEPTION 'git trigger delivery admission cannot be withdrawn' USING ERRCODE = '23514';
    END IF;
    -- A pre-admission result is fenced out once admission began and an
    -- admission result requires that it began.
    IF NEW.done AND NEW.outcome = 'started' AND NOT NEW.admitting THEN
        RAISE EXCEPTION 'a run cannot start before admission began' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_trigger_deliveries_guard BEFORE UPDATE ON git_trigger_deliveries
    FOR EACH ROW EXECUTE FUNCTION git_trigger_deliveries_guard();

ALTER TABLE git_triggers ENABLE ROW LEVEL SECURITY;

ALTER TABLE git_triggers FORCE ROW LEVEL SECURITY;

ALTER TABLE git_trigger_deliveries ENABLE ROW LEVEL SECURITY;

ALTER TABLE git_trigger_deliveries FORCE ROW LEVEL SECURITY;

ALTER TABLE git_trigger_admissions ENABLE ROW LEVEL SECURITY;

ALTER TABLE git_trigger_admissions FORCE ROW LEVEL SECURITY;

-- The owner policies serve administrative seeding and the SECURITY DEFINER
-- locator; the owner role never serves requests. No DELETE policy exists.
CREATE POLICY owner_select ON git_triggers FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON git_triggers FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON git_triggers FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON git_triggers FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON git_triggers FOR UPDATE
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY owner_select ON git_trigger_deliveries FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON git_trigger_deliveries FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON git_trigger_deliveries FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON git_trigger_deliveries FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_update ON git_trigger_deliveries FOR UPDATE
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY owner_select ON git_trigger_admissions FOR SELECT TO CURRENT_USER USING (true);

CREATE POLICY owner_insert ON git_trigger_admissions FOR INSERT TO CURRENT_USER WITH CHECK (true);

CREATE POLICY tenant_select ON git_trigger_admissions FOR SELECT USING (tenant_id = kodama_tenant_id());

CREATE POLICY tenant_insert ON git_trigger_admissions FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- The hook URL carries only the trigger address, so the owning tenant cannot be
-- known before the lookup. This function returns the ownership locator of the
-- single trigger with that address (no secret reference, filter or state); the
-- caller then reads the record inside a tenant transaction, where row level
-- security and explicit predicates apply again.
CREATE FUNCTION kodama_git_trigger_locate(p_id text)
    RETURNS TABLE (tenant_id text, project_id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT t.tenant_id::text, t.project_id::text
        FROM public.git_triggers t
        WHERE t.id = p_id
    $$;

REVOKE ALL ON FUNCTION kodama_git_trigger_locate(text) FROM PUBLIC;
