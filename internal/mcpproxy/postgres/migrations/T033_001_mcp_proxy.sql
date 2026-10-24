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

GRANT SELECT, INSERT ON mcp_servers TO {{runtime_role}};
GRANT UPDATE (endpoint, secret_name, secret_version, tools, version, updated_by, updated_at, deleted_at) ON mcp_servers TO {{runtime_role}};
GRANT SELECT, INSERT ON mcp_run_bindings TO {{runtime_role}};
GRANT SELECT, INSERT ON mcp_sessions TO {{runtime_role}};
GRANT UPDATE (state, updated_at) ON mcp_sessions TO {{runtime_role}};
GRANT SELECT, INSERT ON mcp_invocations TO {{runtime_role}};
GRANT UPDATE (state, code, updated_at) ON mcp_invocations TO {{runtime_role}};
GRANT SELECT, INSERT ON mcp_run_calls TO {{runtime_role}};
GRANT UPDATE (calls) ON mcp_run_calls TO {{runtime_role}};
