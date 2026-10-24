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

-- Completed records are immutable until expiry: no UPDATE grant. DELETE only
-- removes expired rows (the adapter's predicate) and is tenant-scoped by policy.
GRANT SELECT, INSERT, DELETE ON idempotency TO {{runtime_role}};
