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

GRANT SELECT, INSERT ON agent_profiles TO {{runtime_role}};
