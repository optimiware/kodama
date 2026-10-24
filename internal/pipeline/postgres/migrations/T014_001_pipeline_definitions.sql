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

GRANT SELECT, INSERT ON pipeline_definitions TO {{runtime_role}};
