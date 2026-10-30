-- T-113 runtime grants of run resources (additive, append-only).
--
-- A run_shared run's preparation workload and support services have their own
-- runtime subject, separate from attempts: one-time admission credential
-- (digest only), at most one session, bound to (tenant, project, run, resource
-- key) and, once admitted, to the workload instance execution committed. Tokens
-- carry the resource audience; the admitted binding, the pinned spec fingerprint
-- and the resource deadline live here and are re-read, with execution's
-- committed state, on every call.
--
-- Like runtime_grants there is no foreign key to execution's resource table:
-- the reference is the composite (tenant_id, run_id, resource_key), validated
-- under lock against execution's committed state by the service. Registration is
-- immutable, consumption is written once and revocation is irreversible, by
-- trigger and by the column-level grants below. There is no DELETE grant
-- (retention is the retention task's).
--
-- Registered as its own contribution after the run-resource lifecycle's.
-- Nothing here is wired into a composition (T-116 lists it).

CREATE TABLE runtime_resource_grants (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    resource_key       text NOT NULL
                       CONSTRAINT runtime_resource_grants_key_syntax CHECK (resource_key ~ '^(preparation|service\.[a-z][a-z0-9-]{0,29})$'),
    resource_kind      text NOT NULL
                       CONSTRAINT runtime_resource_grants_kind_known CHECK (
                           (resource_kind = 'preparation' AND resource_key = 'preparation')
                           OR (resource_kind = 'service' AND resource_key LIKE 'service.%')),
    credential_digest  bytea NOT NULL
                       CONSTRAINT runtime_resource_grants_credential_digest_length CHECK (octet_length(credential_digest) = 32),
    signing_generation bigint NOT NULL
                       CONSTRAINT runtime_resource_grants_generation_range CHECK (signing_generation BETWEEN 1 AND 9007199254740991),
    spec_fingerprint   text NOT NULL
                       CONSTRAINT runtime_resource_grants_fingerprint_syntax CHECK (spec_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    deadline_at        timestamptz NOT NULL,
    created_at         timestamptz NOT NULL,
    expires_at         timestamptz NOT NULL,
    -- first admission (all or none)
    session_id         text,
    nonce_digest       bytea,
    bound_workload_name text,
    bound_workload_uid  text,
    bound_pod_name     text,
    bound_pod_uid      text,
    consumed_at        timestamptz,
    max_expires_at     timestamptz,
    revoked_at         timestamptz,
    PRIMARY KEY (tenant_id, run_id, resource_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT runtime_resource_grants_credential_unique UNIQUE (credential_digest),
    CONSTRAINT runtime_resource_grants_session_unique UNIQUE (session_id),
    CONSTRAINT runtime_resource_grants_expiry_order CHECK (expires_at > created_at AND expires_at <= deadline_at),
    CONSTRAINT runtime_resource_grants_consumption_complete CHECK (
        (session_id IS NULL AND nonce_digest IS NULL AND bound_workload_name IS NULL AND bound_workload_uid IS NULL
         AND consumed_at IS NULL AND max_expires_at IS NULL AND bound_pod_name IS NULL AND bound_pod_uid IS NULL)
        OR
        (session_id IS NOT NULL AND nonce_digest IS NOT NULL AND bound_workload_name IS NOT NULL AND bound_workload_uid IS NOT NULL
         AND consumed_at IS NOT NULL AND max_expires_at IS NOT NULL
         AND (bound_pod_name IS NULL) = (bound_pod_uid IS NULL))),
    CONSTRAINT runtime_resource_grants_session_syntax CHECK (session_id IS NULL OR session_id ~ '^ses_[A-Za-z0-9_-]{22,43}$'),
    CONSTRAINT runtime_resource_grants_nonce_digest_length CHECK (nonce_digest IS NULL OR octet_length(nonce_digest) = 32),
    CONSTRAINT runtime_resource_grants_bound_syntax CHECK (
        bound_workload_uid IS NULL OR (
            bound_workload_uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
            AND bound_workload_name ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$'
            AND (bound_pod_uid IS NULL OR (bound_pod_uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
                                          AND bound_pod_name ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$')))),
    CONSTRAINT runtime_resource_grants_lifetime_order CHECK (max_expires_at IS NULL OR (max_expires_at > consumed_at AND max_expires_at <= deadline_at)),
    CONSTRAINT runtime_resource_grants_revoked_after_creation CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

ALTER TABLE runtime_resource_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE runtime_resource_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON runtime_resource_grants TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON runtime_resource_grants
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express: immutable registration, a
-- consumption written exactly once, an irreversible revocation.
CREATE FUNCTION runtime_resource_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.resource_key, NEW.resource_kind, NEW.credential_digest,
        NEW.signing_generation, NEW.spec_fingerprint, NEW.deadline_at, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.resource_key, OLD.resource_kind, OLD.credential_digest,
        OLD.signing_generation, OLD.spec_fingerprint, OLD.deadline_at, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'runtime resource grant registration is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.session_id IS NOT NULL AND
       (NEW.session_id, NEW.nonce_digest, NEW.bound_workload_name, NEW.bound_workload_uid, NEW.bound_pod_name, NEW.bound_pod_uid,
        NEW.consumed_at, NEW.max_expires_at)
       IS DISTINCT FROM
       (OLD.session_id, OLD.nonce_digest, OLD.bound_workload_name, OLD.bound_workload_uid, OLD.bound_pod_name, OLD.bound_pod_uid,
        OLD.consumed_at, OLD.max_expires_at) THEN
        RAISE EXCEPTION 'runtime resource grant consumption is write-once' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'runtime resource grant revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER runtime_resource_grants_guard BEFORE UPDATE ON runtime_resource_grants
    FOR EACH ROW EXECUTE FUNCTION runtime_resource_grants_guard();

-- Admission carries no tenant selector, so the grant is found by credential
-- digest through the one narrow path outside a tenant context. The function
-- returns only the ownership locator of the single matching grant: no spec, no
-- digest, no state. Everything afterwards rechecks the credential inside a
-- tenant transaction.
CREATE FUNCTION kodama_runtime_locate_resource_grant(p_digest bytea)
    RETURNS TABLE (tenant_id text, project_id text, run_id text, resource_key text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT g.tenant_id::text, g.project_id::text, g.run_id::text, g.resource_key
        FROM public.runtime_resource_grants g
        WHERE g.credential_digest = p_digest
    $$;

REVOKE ALL ON FUNCTION kodama_runtime_locate_resource_grant(bytea) FROM PUBLIC;

-- Least privilege: the runtime role registers, reads, consumes and revokes. It
-- can update only the consumption and revocation columns and can never delete.
GRANT SELECT, INSERT ON runtime_resource_grants TO {{runtime_role}};

GRANT UPDATE (session_id, nonce_digest, bound_workload_name, bound_workload_uid, bound_pod_name, bound_pod_uid,
              consumed_at, max_expires_at, revoked_at)
    ON runtime_resource_grants TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_runtime_locate_resource_grant(bytea) TO {{runtime_role}};
