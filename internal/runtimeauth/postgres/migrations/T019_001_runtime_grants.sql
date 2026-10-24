-- T-019 runtime grants: one-time admission credentials and the sessions they
-- open.
--
-- A grant is registered by the trusted executor with the SHA-256 digest of a
-- generated credential (the credential itself is never stored) together with
-- the attempt's immutable authority: owner, expected Job, action ceiling,
-- signing generation, authority revision and the pinned context document. The
-- first successful admission writes the consumption once; revocation is written
-- once and is irreversible. Triggers enforce those transitions even against a
-- buggy writer, the column-level grants give the runtime role no way to alter
-- the immutable columns, and there is no DELETE grant (retention is T-056's).
--
-- There is deliberately no foreign key to an attempt table: execution (T-020)
-- owns attempts and has not merged. The attempt reference is the composite
-- (tenant_id, attempt_id); T-020's migration may add a composite foreign key to
-- its attempts table on (tenant_id, project_id, id) without changing this one.

CREATE TABLE runtime_grants (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    job_namespace      text NOT NULL
                       CONSTRAINT runtime_grants_namespace_syntax CHECK (job_namespace ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$'),
    job_name           text NOT NULL
                       CONSTRAINT runtime_grants_job_name_syntax CHECK (job_name ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$'),
    credential_digest  bytea NOT NULL
                       CONSTRAINT runtime_grants_credential_digest_length CHECK (octet_length(credential_digest) = 32),
    signing_generation bigint NOT NULL
                       CONSTRAINT runtime_grants_generation_range CHECK (signing_generation BETWEEN 1 AND 9007199254740991),
    actions            text[] NOT NULL
                       CONSTRAINT runtime_grants_actions_bounded CHECK (cardinality(actions) BETWEEN 1 AND 16)
                       CONSTRAINT runtime_grants_actions_known CHECK (actions <@ ARRAY[
                           'context.read', 'token.renew', 'secret.resolve', 'artifact.reserve', 'artifact.complete',
                           'source.propose', 'result.submit', 'model.invoke', 'tool.invoke', 'deployment.call']::text[]),
    authority_revision bigint NOT NULL
                       CONSTRAINT runtime_grants_revision_range CHECK (authority_revision BETWEEN 1 AND 9007199254740991),
    context_digest     bytea NOT NULL
                       CONSTRAINT runtime_grants_context_digest_length CHECK (octet_length(context_digest) = 32),
    context_doc        bytea NOT NULL
                       CONSTRAINT runtime_grants_context_size CHECK (octet_length(context_doc) BETWEEN 2 AND 4194304),
    created_at         timestamptz NOT NULL,
    expires_at         timestamptz NOT NULL,
    -- first admission (all or none)
    session_id         text,
    nonce_digest       bytea,
    bound_job_uid      text,
    bound_pod_name     text,
    bound_pod_uid      text,
    consumed_at        timestamptz,
    max_expires_at     timestamptz,
    revoked_at         timestamptz,
    PRIMARY KEY (tenant_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT runtime_grants_credential_unique UNIQUE (credential_digest),
    CONSTRAINT runtime_grants_session_unique UNIQUE (session_id),
    CONSTRAINT runtime_grants_expiry_order CHECK (expires_at > created_at),
    CONSTRAINT runtime_grants_consumption_complete CHECK (
        (session_id IS NULL AND nonce_digest IS NULL AND bound_job_uid IS NULL AND bound_pod_name IS NULL
         AND bound_pod_uid IS NULL AND consumed_at IS NULL AND max_expires_at IS NULL)
        OR
        (session_id IS NOT NULL AND nonce_digest IS NOT NULL AND bound_job_uid IS NOT NULL AND bound_pod_name IS NOT NULL
         AND bound_pod_uid IS NOT NULL AND consumed_at IS NOT NULL AND max_expires_at IS NOT NULL)),
    CONSTRAINT runtime_grants_session_syntax CHECK (session_id IS NULL OR session_id ~ '^ses_[A-Za-z0-9_-]{22,43}$'),
    CONSTRAINT runtime_grants_nonce_digest_length CHECK (nonce_digest IS NULL OR octet_length(nonce_digest) = 32),
    CONSTRAINT runtime_grants_bound_uid_syntax CHECK (
        bound_job_uid IS NULL OR (
            bound_job_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND bound_pod_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND bound_pod_name ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$')),
    CONSTRAINT runtime_grants_lifetime_order CHECK (max_expires_at IS NULL OR max_expires_at > consumed_at),
    CONSTRAINT runtime_grants_revoked_after_creation CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

ALTER TABLE runtime_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE runtime_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON runtime_grants TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON runtime_grants
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express: immutable registration, a
-- consumption written exactly once, an irreversible revocation.
CREATE FUNCTION runtime_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.attempt_id, NEW.job_namespace, NEW.job_name,
        NEW.credential_digest, NEW.signing_generation, NEW.actions, NEW.authority_revision,
        NEW.context_digest, NEW.context_doc, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.attempt_id, OLD.job_namespace, OLD.job_name,
        OLD.credential_digest, OLD.signing_generation, OLD.actions, OLD.authority_revision,
        OLD.context_digest, OLD.context_doc, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'runtime grant registration is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.session_id IS NOT NULL AND
       (NEW.session_id, NEW.nonce_digest, NEW.bound_job_uid, NEW.bound_pod_name, NEW.bound_pod_uid, NEW.consumed_at, NEW.max_expires_at)
       IS DISTINCT FROM
       (OLD.session_id, OLD.nonce_digest, OLD.bound_job_uid, OLD.bound_pod_name, OLD.bound_pod_uid, OLD.consumed_at, OLD.max_expires_at) THEN
        RAISE EXCEPTION 'runtime grant consumption is write-once' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'runtime grant revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER runtime_grants_guard BEFORE UPDATE ON runtime_grants
    FOR EACH ROW EXECUTE FUNCTION runtime_grants_guard();

-- Least privilege: the runtime role registers, reads, consumes and revokes. It
-- can update only the consumption and revocation columns and can never delete.
GRANT SELECT, INSERT ON runtime_grants TO {{runtime_role}};
GRANT UPDATE (session_id, nonce_digest, bound_job_uid, bound_pod_name, bound_pod_uid, consumed_at, max_expires_at, revoked_at)
    ON runtime_grants TO {{runtime_role}};

-- Admission carries no tenant selector, so the grant is found by credential
-- digest through the one narrow path outside a tenant context. The function
-- returns only the ownership locator of the single matching grant: no context,
-- no digest, no state. The caller must know the digest, which is only derivable
-- from the credential. Everything afterwards rechecks the credential inside a
-- tenant transaction.
CREATE FUNCTION kodama_runtime_locate_grant(p_digest bytea)
    RETURNS TABLE (tenant_id text, project_id text, attempt_id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT g.tenant_id::text, g.project_id::text, g.attempt_id::text
        FROM public.runtime_grants g
        WHERE g.credential_digest = p_digest
    $$;

REVOKE ALL ON FUNCTION kodama_runtime_locate_grant(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kodama_runtime_locate_grant(bytea) TO {{runtime_role}};
