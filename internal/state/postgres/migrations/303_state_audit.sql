-- Append-only state audit. Values and artifact references are deliberately not
-- copied here: an operator can inspect the state entry by its key/revision.
CREATE TABLE state_audit (
    id          bigserial PRIMARY KEY,
    tenant_id   text NOT NULL,
    project_id  text NOT NULL,
    namespace   text NOT NULL,
    key         text NOT NULL,
    generation  bigint NOT NULL,
    revision    bigint NOT NULL,
    action      text NOT NULL CHECK (action IN ('create', 'update', 'delete')),
    actor       text NOT NULL,
    at          timestamptz NOT NULL
);
CREATE INDEX state_audit_scope_idx ON state_audit (tenant_id, project_id, namespace, key, id);

ALTER TABLE state_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE state_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY state_audit_tenant ON state_audit
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));

GRANT SELECT, INSERT ON state_audit TO {{runtime_role}};
GRANT USAGE, SELECT ON SEQUENCE state_audit_id_seq TO {{runtime_role}};
