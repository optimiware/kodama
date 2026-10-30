-- T-136 Git grants (additive, append-only).
--
-- A Git grant is the durable record of one Git credential of the internal Git
-- service (git-repositories.md GR-05, GR-06, GR-18; IT-17 to IT-19): who it was
-- issued to, for which tenant, project and repositories, which ref patterns it
-- may update, whether it is a seed or an integration grant, when it was issued,
-- when it expires, whether it was revoked and, for a seed, the one receive that
-- consumed it. The wire form of the grant is the GrantClaims document of
-- contracts/git/v1; it is stored verbatim in `claims`, and the columns beside it
-- exist to be queried and constrained (the checks below keep them equal to the
-- document). The credential itself is an opaque 256-bit secret that is never
-- stored: only its SHA-256 digest is.
--
-- Registration is immutable. The only changes are a replaced credential digest
-- (a retried issuance of a workload grant returns the same grant with a fresh
-- secret, allowed only while the grant is unrevoked and unconsumed), the
-- write-once consumption of a seed and the irreversible revocation, by trigger
-- and by the column-level grants below. There is no DELETE grant (retention is
-- the retention task's).
--
-- Like runtime_grants there is no foreign key to execution's attempt table or to
-- the repository table (T-127, migration 007): the references are the opaque
-- composite (tenant_id, attempt_id) and (tenant_id, repository id) and are
-- validated against the owners' current state through ports at issuance and at
-- every use. The project reference is a composite foreign key, so a grant can
-- never name another tenant's project.
--
-- Registered as its own contribution after the repository migration (007) and
-- before the push records (009). Nothing here is wired into a composition (T-132
-- lists it).

CREATE TABLE git_grants (
    tenant_id           kodama_id NOT NULL,
    id                  kodama_id NOT NULL,
    project_id          kodama_id NOT NULL,
    kind                text NOT NULL
                        CONSTRAINT git_grants_kind_known CHECK (kind IN ('human', 'seed', 'preparation', 'attempt', 'integration')),
    actor_type          text NOT NULL
                        CONSTRAINT git_grants_actor_type_known CHECK (actor_type IN ('user', 'workload')),
    actor_id            kodama_id NOT NULL,
    run_id              kodama_id,
    attempt_id          kodama_id,
    repository_ids      text[] NOT NULL
                        CONSTRAINT git_grants_repositories_bounded CHECK (cardinality(repository_ids) BETWEEN 1 AND 64),
    claims              jsonb NOT NULL
                        CONSTRAINT git_grants_claims_object CHECK (jsonb_typeof(claims) = 'object'),
    credential_digest   bytea NOT NULL
                        CONSTRAINT git_grants_credential_digest_length CHECK (octet_length(credential_digest) = 32),
    issue_key           text
                        CONSTRAINT git_grants_issue_key_length CHECK (issue_key IS NULL OR char_length(issue_key) BETWEEN 1 AND 400),
    issued_at           timestamptz NOT NULL,
    expires_at          timestamptz NOT NULL,
    consumed_receive_id kodama_id,
    consumed_at         timestamptz,
    revoked_at          timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT git_grants_credential_unique UNIQUE (tenant_id, credential_digest),
    CONSTRAINT git_grants_issue_key_unique UNIQUE (tenant_id, issue_key),
    -- the columns equal the stored claims document
    CONSTRAINT git_grants_claims_match CHECK (
        claims ->> 'grantId' = id
        AND claims ->> 'tenantId' = tenant_id
        AND claims ->> 'projectId' = project_id
        AND claims ->> 'kind' = kind
        AND claims ->> 'audience' = 'kodama-git'
        AND claims #>> '{actor,type}' = actor_type
        AND claims #>> '{actor,id}' = actor_id
        AND (claims #>> '{actor,runId}') IS NOT DISTINCT FROM run_id
        AND (claims #>> '{actor,attemptId}') IS NOT DISTINCT FROM attempt_id),
    -- the shape of each kind
    CONSTRAINT git_grants_kind_shape CHECK (
        (kind = 'human' AND actor_type = 'user' AND attempt_id IS NULL AND run_id IS NULL AND issue_key IS NULL)
        OR (kind = 'seed' AND (actor_type = 'user' AND attempt_id IS NULL AND run_id IS NULL
                               OR actor_type = 'workload' AND attempt_id IS NOT NULL AND run_id IS NOT NULL))
        OR (kind = 'preparation' AND actor_type = 'workload' AND attempt_id IS NULL AND run_id IS NOT NULL AND issue_key IS NOT NULL)
        OR (kind IN ('attempt', 'integration') AND actor_type = 'workload' AND attempt_id IS NOT NULL AND run_id IS NOT NULL
            AND issue_key IS NOT NULL)),
    CONSTRAINT git_grants_expiry_order CHECK (expires_at > issued_at),
    CONSTRAINT git_grants_lifetime_bounded CHECK (
        expires_at - issued_at <= CASE kind
            WHEN 'human' THEN interval '1 hour'
            WHEN 'seed' THEN interval '15 minutes'
            ELSE interval '24 hours' END),
    CONSTRAINT git_grants_consumption_complete CHECK ((consumed_receive_id IS NULL) = (consumed_at IS NULL)),
    CONSTRAINT git_grants_consumption_seed_only CHECK (consumed_at IS NULL OR kind = 'seed'),
    CONSTRAINT git_grants_consumed_after_issue CHECK (consumed_at IS NULL OR consumed_at >= issued_at),
    CONSTRAINT git_grants_revoked_after_issue CHECK (revoked_at IS NULL OR revoked_at >= issued_at)
);

CREATE INDEX git_grants_attempt ON git_grants (tenant_id, project_id, attempt_id) WHERE attempt_id IS NOT NULL;

CREATE INDEX git_grants_open_seed ON git_grants (tenant_id) WHERE kind = 'seed' AND consumed_at IS NULL AND revoked_at IS NULL;

ALTER TABLE git_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE git_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON git_grants TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON git_grants
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express: immutable registration, a
-- replaced credential only while the grant is unrevoked and unconsumed, a
-- consumption written exactly once, an irreversible revocation.
CREATE FUNCTION git_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.id, NEW.project_id, NEW.kind, NEW.actor_type, NEW.actor_id, NEW.run_id, NEW.attempt_id,
        NEW.repository_ids, NEW.claims, NEW.issue_key, NEW.issued_at, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.id, OLD.project_id, OLD.kind, OLD.actor_type, OLD.actor_id, OLD.run_id, OLD.attempt_id,
        OLD.repository_ids, OLD.claims, OLD.issue_key, OLD.issued_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'git grant registration is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.credential_digest IS DISTINCT FROM OLD.credential_digest
       AND (OLD.revoked_at IS NOT NULL OR OLD.consumed_at IS NOT NULL) THEN
        RAISE EXCEPTION 'the credential of a revoked or consumed git grant cannot be replaced' USING ERRCODE = '23514';
    END IF;
    IF OLD.consumed_at IS NOT NULL AND
       (NEW.consumed_receive_id, NEW.consumed_at) IS DISTINCT FROM (OLD.consumed_receive_id, OLD.consumed_at) THEN
        RAISE EXCEPTION 'git grant consumption is write-once' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'git grant revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_grants_guard BEFORE UPDATE ON git_grants
    FOR EACH ROW EXECUTE FUNCTION git_grants_guard();

-- Least privilege: the runtime role registers, reads, consumes and revokes. It
-- can update only the credential digest, the consumption and the revocation and
-- can never delete. A credential is found by its digest inside a tenant
-- transaction (the Git endpoint knows the tenant from the URL), so, unlike the
-- runtime admission credential, no lookup function outside tenant context exists.
GRANT SELECT, INSERT ON git_grants TO {{runtime_role}};

GRANT UPDATE (credential_digest, consumed_receive_id, consumed_at, revoked_at) ON git_grants TO {{runtime_role}};
