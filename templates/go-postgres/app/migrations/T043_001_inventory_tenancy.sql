-- Application tenancy and inventory exceptions.
-- Forward-only. The runtime role receives least-privilege grants; it owns nothing.
-- {{runtime_role}} is substituted by the migration command from --runtime-role.

CREATE FUNCTION app_tenant() RETURNS uuid
LANGUAGE sql STABLE
AS $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

CREATE TABLE tenants (
    id         uuid PRIMARY KEY,
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    tenant_id  uuid NOT NULL REFERENCES tenants (id),
    subject    text NOT NULL CHECK (length(subject) BETWEEN 1 AND 256),
    role       text NOT NULL CHECK (role IN ('viewer', 'reviewer', 'admin', 'worker')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, subject)
);

CREATE TABLE exceptions (
    tenant_id        uuid NOT NULL REFERENCES tenants (id),
    id               uuid NOT NULL,
    erp_ref          text NOT NULL CHECK (length(erp_ref) BETWEEN 1 AND 128),
    description      text NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
    erp_quantity     bigint NOT NULL CHECK (erp_quantity BETWEEN 0 AND 1000000000),
    counted_quantity bigint NOT NULL CHECK (counted_quantity BETWEEN 0 AND 1000000000),
    state            text NOT NULL CHECK (state IN ('open', 'pending', 'resolved', 'failed', 'unknown')),
    version          bigint NOT NULL CHECK (version >= 1),
    created_by       text NOT NULL,
    resolution_operation_key     text,
    resolution_adjusted_quantity bigint,
    resolution_reason            text,
    resolution_requested_by      text,
    resolution_payload_hash      text,
    resolution_erp_reference     text,
    resolution_detail            text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CHECK (erp_quantity <> counted_quantity),
    CHECK ((state = 'open') = (resolution_operation_key IS NULL)),
    CHECK (state <> 'resolved' OR resolution_erp_reference IS NOT NULL)
);
CREATE INDEX exceptions_page_idx ON exceptions (tenant_id, created_at, id);

ALTER TABLE tenants     ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants     FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
ALTER TABLE exceptions  ENABLE ROW LEVEL SECURITY;
ALTER TABLE exceptions  FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants     USING (id = app_tenant())        WITH CHECK (id = app_tenant());
CREATE POLICY tenant_isolation ON memberships USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant());
CREATE POLICY tenant_isolation ON exceptions  USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant());

-- Membership and tenant administration belongs to provisioning, not the runtime.
GRANT SELECT ON tenants, memberships TO {{runtime_role}};
GRANT SELECT, INSERT, UPDATE ON exceptions TO {{runtime_role}};
