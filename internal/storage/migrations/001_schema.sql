-- ADR 0013: prerelease reset, 14 domain tables plus audit_events.
-- Fresh databases only. The migration runner rejects the previous baseline's
-- checksum; it never drops data or upgrades the old schema implicitly.
CREATE DOMAIN kodama_id AS text
    CHECK (VALUE ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$');
CREATE DOMAIN kodama_version AS bigint
    CHECK (VALUE BETWEEN 1 AND 9007199254740991);
CREATE DOMAIN kodama_object AS jsonb CHECK (jsonb_typeof(VALUE) = 'object');
CREATE DOMAIN kodama_array AS jsonb CHECK (jsonb_typeof(VALUE) = 'array');

CREATE FUNCTION kodama_tenant_id() RETURNS text
    LANGUAGE sql STABLE PARALLEL SAFE
    AS $$ SELECT NULLIF(current_setting('kodama.tenant_id', true), '') $$;

CREATE TABLE tenants (
    id kodama_id PRIMARY KEY,
    key text NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    display_name text NOT NULL DEFAULT '',
    settings kodama_object NOT NULL DEFAULT '{}',
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id kodama_id PRIMARY KEY,
    issuer text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),
    display_name text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

CREATE TABLE projects (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    display_name text NOT NULL DEFAULT '',
    -- No product-wide task states: the application validates this workflow.
    task_workflow kodama_object NOT NULL,
    settings kodama_object NOT NULL DEFAULT '{}',
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, key)
);

CREATE TABLE memberships (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    user_id kodama_id NOT NULL REFERENCES users (id),
    role text NOT NULL CHECK (role IN ('admin', 'engineer', 'reviewer', 'operator', 'viewer')),
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, user_id, role)
);
CREATE INDEX memberships_user ON memberships (user_id, tenant_id);

CREATE TABLE repositories (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    -- Relative repository name. Full paths are rendered from owner keys, so
    -- renaming a project cannot leave a contradictory stored ownership path.
    name text NOT NULL CHECK (name ~ '^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$' AND length(name) <= 512),
    default_branch text NOT NULL DEFAULT 'main' CHECK (length(default_branch) BETWEEN 1 AND 255),
    status text NOT NULL DEFAULT 'provisioning' CHECK (status IN ('provisioning', 'ready', 'archived')),
    reason text,
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CHECK (project_id IS NOT NULL OR name = 'context')
);

CREATE TABLE pipelines (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    repo_id kodama_id,
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    -- One authoritative complete document, not independently editable step rows.
    definition kodama_object NOT NULL,
    source text NOT NULL CHECK (source IN ('git', 'api')),
    source_commit text,
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, repo_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, repo_id) REFERENCES repositories (tenant_id, project_id, id),
    CHECK ((source = 'api' AND source_commit IS NULL) OR
           (source = 'git' AND repo_id IS NOT NULL AND source_commit IS NOT NULL
            AND source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'))
);

CREATE TABLE tasks (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    parent_id kodama_id,
    blocked_by_id kodama_id,
    kind text NOT NULL DEFAULT 'task' CHECK (kind IN ('task', 'decision', 'promotion', 'incident')),
    title text NOT NULL CHECK (length(title) BETWEEN 1 AND 512),
    body text NOT NULL DEFAULT '',
    state text NOT NULL CHECK (state ~ '^[a-z][a-z0-9_-]{0,62}$'),
    -- Parking is orthogonal to project-defined state names.
    parked_at timestamptz,
    resume_state text,
    assigned_role text CHECK (assigned_role IN ('admin', 'engineer', 'reviewer', 'operator', 'viewer')),
    assigned_user_id kodama_id REFERENCES users (id),
    repo_id kodama_id,
    branch text,
    context kodama_object NOT NULL DEFAULT '{}',
    resolution kodama_object,
    resolved_at timestamptz,
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, parent_id) REFERENCES tasks (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, blocked_by_id) REFERENCES tasks (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, repo_id) REFERENCES repositories (tenant_id, project_id, id),
    CHECK (parent_id IS NULL OR parent_id <> id),
    CHECK (blocked_by_id IS NULL OR blocked_by_id <> id),
    CHECK ((parked_at IS NULL AND blocked_by_id IS NULL AND resume_state IS NULL) OR
           (parked_at IS NOT NULL AND blocked_by_id IS NOT NULL AND resume_state IS NOT NULL)),
    CHECK ((resolution IS NULL) = (resolved_at IS NULL))
);
CREATE INDEX tasks_inbox ON tasks (tenant_id, project_id, assigned_role, created_at, id)
    WHERE resolved_at IS NULL;
CREATE INDEX tasks_state ON tasks (tenant_id, project_id, state, id);
CREATE INDEX tasks_blocked_by ON tasks (tenant_id, project_id, blocked_by_id) WHERE blocked_by_id IS NOT NULL;

CREATE TABLE runs (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    pipeline_id kodama_id NOT NULL,
    task_id kodama_id,
    definition_snapshot kodama_object NOT NULL,
    inputs kodama_object NOT NULL DEFAULT '{}',
    -- Exact repository commits, image digests and secret revision references.
    sources kodama_array NOT NULL DEFAULT '[]',
    bindings kodama_object NOT NULL DEFAULT '{}',
    trigger_key text,
    request_key text,
    request_principal text,
    request_hash text,
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    reason text,
    cancel_requested_at timestamptz,
    cleanup_pending boolean NOT NULL DEFAULT false,
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, pipeline_id) REFERENCES pipelines (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, task_id) REFERENCES tasks (tenant_id, project_id, id),
    UNIQUE (tenant_id, project_id, pipeline_id, trigger_key),
    UNIQUE (tenant_id, project_id, request_principal, request_key),
    CHECK ((request_key IS NULL AND request_principal IS NULL AND request_hash IS NULL) OR
           (request_key IS NOT NULL AND request_principal IS NOT NULL AND request_hash IS NOT NULL
            AND length(request_key) BETWEEN 1 AND 128 AND length(request_principal) > 0 AND request_hash ~ '^[0-9a-f]{64}$')),
    CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CHECK (finished_at IS NULL OR finished_at >= created_at)
);
CREATE INDEX runs_pending ON runs (tenant_id, project_id, created_at, id) WHERE status IN ('queued', 'running');
CREATE INDEX runs_cleanup ON runs (tenant_id, id) WHERE cleanup_pending;

CREATE TABLE run_steps (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    step_key text NOT NULL CHECK (step_key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    image text NOT NULL CHECK (length(image) > 0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'cancelled')),
    reason text,
    runtime_id text,
    exit_code integer,
    log_uri text,
    outputs kodama_object NOT NULL DEFAULT '{}',
    version kodama_version NOT NULL DEFAULT 1,
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (tenant_id, project_id, run_id, id),
    UNIQUE (tenant_id, project_id, run_id, step_key),
    UNIQUE (tenant_id, project_id, run_id, ordinal),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CHECK ((status IN ('succeeded', 'failed', 'skipped', 'cancelled')) = (finished_at IS NOT NULL))
);

CREATE TABLE services (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    run_id kodama_id,
    scope text NOT NULL CHECK (scope IN ('tenant', 'project', 'pipeline')),
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    image text NOT NULL CHECK (length(image) > 0),
    config kodama_object NOT NULL DEFAULT '{}',
    endpoint text,
    runtime_id text,
    status text NOT NULL DEFAULT 'starting' CHECK (status IN ('starting', 'ready', 'failed', 'stopped')),
    reason text,
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, run_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CHECK ((scope = 'tenant' AND project_id IS NULL AND run_id IS NULL) OR
           (scope = 'project' AND project_id IS NOT NULL AND run_id IS NULL) OR
           (scope = 'pipeline' AND project_id IS NOT NULL AND run_id IS NOT NULL))
);

CREATE TABLE secrets (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    scope_key text GENERATED ALWAYS AS (coalesce(project_id::text, '')) STORED,
    name text NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_-]{0,127}$'),
    encrypted_value bytea NOT NULL CHECK (octet_length(encrypted_value) > 0),
    -- Self-contained envelope: key identity, wrapped key, nonce/algorithm.
    -- Encryption continues to bind tenant/project/id/revision as AAD.
    key_id text NOT NULL CHECK (length(key_id) > 0),
    envelope kodama_object NOT NULL,
    version kodama_version NOT NULL DEFAULT 1,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, scope_key, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE TABLE artifacts (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    step_id kodama_id,
    task_id kodama_id,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    kind text NOT NULL CHECK (kind IN ('image', 'package', 'preview', 'report', 'release')),
    uri text NOT NULL CHECK (length(uri) > 0),
    digest text CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    metadata kodama_object NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_id) REFERENCES run_steps (tenant_id, project_id, run_id, id),
    FOREIGN KEY (tenant_id, project_id, task_id) REFERENCES tasks (tenant_id, project_id, id),
    CHECK (kind = 'preview' OR digest IS NOT NULL)
);
CREATE INDEX artifacts_run ON artifacts (tenant_id, project_id, run_id);

CREATE TABLE agent_sessions (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    step_id kodama_id NOT NULL,
    task_id kodama_id,
    model text NOT NULL CHECK (length(model) > 0),
    status text NOT NULL CHECK (status IN ('running', 'completed', 'parked', 'failed', 'cancelled')),
    prompt_tokens bigint NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens bigint NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    messages kodama_array NOT NULL DEFAULT '[]',
    transcript_uri text,
    result kodama_object NOT NULL DEFAULT '{}',
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_id) REFERENCES run_steps (tenant_id, project_id, run_id, id),
    FOREIGN KEY (tenant_id, project_id, task_id) REFERENCES tasks (tenant_id, project_id, id)
);

CREATE TABLE mcp_servers (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    url text NOT NULL CHECK (length(url) > 0),
    secret_id kodama_id,
    secret_project_id kodama_id,
    secret_scope_key text GENERATED ALWAYS AS (coalesce(secret_project_id::text, '')) STORED,
    allowed_tools kodama_array NOT NULL DEFAULT '[]',
    version kodama_version NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, secret_scope_key, secret_id) REFERENCES secrets (tenant_id, scope_key, id),
    CHECK (secret_project_id IS NULL OR (secret_id IS NOT NULL AND project_id IS NOT NULL AND secret_project_id = project_id))
);

-- Payload is immutable; only publication progress can be updated by runtime.
-- Consumer deduplication uses resource uniqueness (e.g. runs.trigger_key) and
-- expected versions, not a claim of exactly-once delivery by JetStream.
CREATE TABLE audit_events (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    actor_type text NOT NULL CHECK (actor_type IN ('user', 'workload', 'system', 'operator')),
    actor_id text NOT NULL CHECK (length(actor_id) > 0),
    event_type text NOT NULL CHECK (length(event_type) > 0),
    resource_type text NOT NULL CHECK (length(resource_type) > 0),
    resource_id kodama_id NOT NULL,
    payload kodama_object NOT NULL DEFAULT '{}',
    published_at timestamptz,
    publish_attempts integer NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE INDEX audit_events_pending ON audit_events (tenant_id, next_attempt_at, id) WHERE published_at IS NULL;
CREATE INDEX audit_events_resource ON audit_events (tenant_id, project_id, resource_type, resource_id, created_at, id);


-- Phase 3 operational fields (ADR 0013). Observations and control progress
-- only: admitted inputs stay immutable in the run snapshot columns.
ALTER TABLE pipelines ADD COLUMN disabled_at timestamptz;
ALTER TABLE runs
    ADD COLUMN backend text,
    ADD COLUMN deadline_at timestamptz,
    ADD COLUMN workspace_key text,
    ADD COLUMN execution_order text[] NOT NULL DEFAULT '{}',
    ADD COLUMN admission_error text;
ALTER TABLE run_steps
    ADD COLUMN action_key text,
    ADD COLUMN dispatch_fingerprint text,
    ADD COLUMN begin_at timestamptz,
    ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0);
ALTER TABLE services
    ADD COLUMN storage_key text,
    ADD COLUMN desired text NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped'));

-- Resolve a user-facing tenant key to its opaque ID for /tenants/{key} routing.
CREATE FUNCTION kodama_tenant_lookup(p_key text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$ SELECT id::text FROM public.tenants WHERE key = p_key $$;
REVOKE ALL ON FUNCTION kodama_tenant_lookup(text) FROM PUBLIC;

-- Owner policies support administrative seeding and the narrow discovery
-- functions. The owner role is never used by request handlers.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tenants', 'users', 'projects', 'memberships', 'repositories',
        'pipelines', 'tasks', 'runs', 'run_steps', 'services', 'secrets', 'artifacts',
        'agent_sessions', 'mcp_servers', 'audit_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY owner_all ON %I TO %I USING (true) WITH CHECK (true)', t, current_user);
        IF t NOT IN ('tenants', 'users') THEN
            EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())', t);
        END IF;
    END LOOP;
END $$;
CREATE POLICY tenant_isolation ON tenants USING (id = kodama_tenant_id()) WITH CHECK (id = kodama_tenant_id());
CREATE POLICY tenant_members ON users FOR SELECT USING (
    EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = kodama_tenant_id())
);

CREATE FUNCTION kodama_discover_memberships(p_issuer text, p_subject text)
    RETURNS TABLE (user_id text, tenant_id text, project_id text, role text)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$ SELECT u.id::text, m.tenant_id::text, m.project_id::text, m.role
          FROM public.users u JOIN public.memberships m ON m.user_id = u.id
          WHERE u.issuer = p_issuer AND u.subject = p_subject
          ORDER BY m.tenant_id, m.project_id NULLS FIRST, m.role $$;
REVOKE ALL ON FUNCTION kodama_discover_memberships(text, text) FROM PUBLIC;
CREATE FUNCTION kodama_list_tenant_ids() RETURNS SETOF text
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$ SELECT id::text FROM public.tenants ORDER BY id $$;
REVOKE ALL ON FUNCTION kodama_list_tenant_ids() FROM PUBLIC;
