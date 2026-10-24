-- Durable ERP intents, idempotency records and append-only audit history.

CREATE TABLE resolution_intents (
    tenant_id         uuid NOT NULL,
    id                uuid NOT NULL,
    exception_id      uuid NOT NULL,
    operation_key     text NOT NULL,
    payload_hash      text NOT NULL,
    erp_ref           text NOT NULL,
    adjusted_quantity bigint NOT NULL CHECK (adjusted_quantity >= 0),
    reason            text NOT NULL,
    requested_by      text NOT NULL,
    status            text NOT NULL CHECK (status IN ('queued', 'dispatching', 'acknowledged', 'rejected', 'unknown')),
    attempts          integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner       text,
    lease_until       timestamptz,
    next_attempt_at   timestamptz,
    erp_reference     text,
    detail            text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, exception_id) REFERENCES exceptions (tenant_id, id),
    UNIQUE (tenant_id, exception_id),
    UNIQUE (tenant_id, operation_key)
);
CREATE INDEX resolution_intents_due_idx ON resolution_intents (tenant_id, status, created_at);

CREATE TABLE idempotency_records (
    tenant_id    uuid NOT NULL,
    idem_key     text NOT NULL CHECK (length(idem_key) BETWEEN 8 AND 128),
    request_hash text NOT NULL,
    exception_id uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idem_key),
    FOREIGN KEY (tenant_id, exception_id) REFERENCES exceptions (tenant_id, id)
);

CREATE TABLE audit_events (
    tenant_id    uuid NOT NULL,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    exception_id uuid NOT NULL,
    actor        text NOT NULL,
    action       text NOT NULL,
    detail       text NOT NULL DEFAULT '',
    at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, seq),
    FOREIGN KEY (tenant_id, exception_id) REFERENCES exceptions (tenant_id, id)
);

CREATE FUNCTION audit_events_append_only() RETURNS trigger
LANGUAGE plpgsql
AS $$ BEGIN RAISE EXCEPTION 'audit_events is append-only'; END $$;
CREATE TRIGGER audit_events_no_change BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

ALTER TABLE resolution_intents  ENABLE ROW LEVEL SECURITY;
ALTER TABLE resolution_intents  FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events        ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events        FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON resolution_intents  USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant());
CREATE POLICY tenant_isolation ON idempotency_records USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant());
CREATE POLICY tenant_isolation ON audit_events        USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant());

GRANT SELECT, INSERT, UPDATE ON resolution_intents TO {{runtime_role}};
GRANT SELECT, INSERT ON idempotency_records TO {{runtime_role}};
GRANT SELECT, INSERT ON audit_events TO {{runtime_role}};
