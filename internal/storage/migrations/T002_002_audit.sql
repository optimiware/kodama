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

GRANT SELECT, INSERT ON audit TO {{runtime_role}};
