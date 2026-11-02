-- Native Git hosting on the ADR 0013 kernel (O-36, T-167). Appended after the
-- baseline: it changes no earlier object except by adding three columns to
-- repositories. There is no push-record, grant or workspace-association table:
-- a receive is recorded as one git.refs_updated audit_events row, and the
-- worker's own journal plus repositories.journal_ack recover a lost outcome.

ALTER TABLE repositories
    -- directPushToDefault (default false, O-33) and integrationPipeline.
    ADD COLUMN policy kodama_object NOT NULL DEFAULT '{}',
    -- The tenant worker's last report: true while the repository has no refs.
    ADD COLUMN empty boolean NOT NULL DEFAULT true,
    -- Highest worker journal sequence below which every receive is recorded.
    ADD COLUMN journal_ack bigint NOT NULL DEFAULT 0 CHECK (journal_ack >= 0);

-- Short-lived opaque Git credentials. Only the SHA-256 digest of the secret is
-- stored, never the secret. Authority is not frozen here: claims record what was
-- issued (a ceiling), and current roles, run state and repository policy are
-- re-read at every use.
CREATE TABLE git_credentials (
    tenant_id kodama_id NOT NULL REFERENCES tenants (id),
    id kodama_id NOT NULL,
    project_id kodama_id,
    repo_id kodama_id NOT NULL,
    kind text NOT NULL CHECK (kind IN ('human', 'seed', 'run', 'integration')),
    -- Hex SHA-256 of the credential secret.
    secret_digest text NOT NULL CHECK (secret_digest ~ '^[0-9a-f]{64}$'),
    claims kodama_object NOT NULL,
    user_id kodama_id REFERENCES users (id),
    run_id kodama_id,
    issued_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    -- A seed is consumed when its receive is authorized; settled_at records that
    -- the receive's outcome was recorded.
    consumed_at timestamptz,
    consumed_receive text,
    settled_at timestamptz,
    version kodama_version NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, secret_digest),
    FOREIGN KEY (tenant_id, repo_id) REFERENCES repositories (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CHECK (expires_at > issued_at),
    CHECK ((kind IN ('human', 'seed')) = (user_id IS NOT NULL)),
    CHECK ((kind IN ('run', 'integration')) = (run_id IS NOT NULL AND project_id IS NOT NULL)),
    CHECK (kind = 'seed' OR (consumed_at IS NULL AND consumed_receive IS NULL AND settled_at IS NULL)),
    CHECK ((consumed_at IS NULL) = (consumed_receive IS NULL)),
    CHECK (settled_at IS NULL OR consumed_at IS NOT NULL)
);
CREATE INDEX git_credentials_repo ON git_credentials (tenant_id, repo_id, kind);
CREATE INDEX git_credentials_run ON git_credentials (tenant_id, project_id, run_id) WHERE run_id IS NOT NULL;
-- At most one live (unconsumed, unrevoked) seed per repository: a second request
-- while one is outstanding is refused rather than adding another grant.
CREATE UNIQUE INDEX git_credentials_open_seed ON git_credentials (tenant_id, repo_id)
    WHERE kind = 'seed' AND consumed_at IS NULL AND revoked_at IS NULL;

-- A credential names the same project scope as its repository, checked here
-- rather than by a composite foreign key: a tenant-level repository has a null
-- project, which the key does not check (MATCH SIMPLE) and which a run's
-- credential cannot share. The one exception is a run credential for the tenant's
-- own repository (its context): it carries the run's project, so that the run's
-- liveness is read with the credential. Its read-only claims are issued by the
-- server and never name an update.
CREATE FUNCTION kodama_git_credential_owner() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM repositories r
                   WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.repo_id
                     AND (r.project_id IS NOT DISTINCT FROM NEW.project_id
                          OR (r.project_id IS NULL AND NEW.kind = 'run'))) THEN
        RAISE EXCEPTION 'git credential scope does not match its repository' USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER git_credentials_owner BEFORE INSERT ON git_credentials
    FOR EACH ROW EXECUTE FUNCTION kodama_git_credential_owner();

ALTER TABLE git_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_credentials FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    EXECUTE format('CREATE POLICY owner_all ON git_credentials TO %I USING (true) WITH CHECK (true)', current_user);
END $$;
CREATE POLICY tenant_isolation ON git_credentials
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Runtime grants, in the style of 002_grants.sql: ownership columns, digests and
-- claims are immutable; only the lifecycle columns can change.
GRANT SELECT, INSERT ON git_credentials TO {{runtime_role}};
GRANT UPDATE (revoked_at, consumed_at, consumed_receive, settled_at, version) ON git_credentials TO {{runtime_role}};
