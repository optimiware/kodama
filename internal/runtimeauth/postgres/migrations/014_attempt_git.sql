-- T-150 attempt Git state (additive, append-only).
--
-- Two kinds of Git grant cannot exist when an attempt is admitted and are issued
-- during the attempt through the runtime routes of the source-set wire: the
-- single-use seed grant of a repository the attempt's run created and the
-- integration grant of an approved gate (runtime-v3.md, "Runtime-issued Git
-- grants"). The Git authority (internal/gitauth) records the grant itself; this
-- table is the runtime authority's own record of which runtime grants it issued
-- to which attempt, so that the result module can recognize a commit output as
-- covered by a runtime grant from the control plane's records and never from the
-- caller's word (runtimev3.CheckResult's issued grants).
--
-- A row is the claims document of the grant (not secret: no credential, no
-- digest) beside the queryable columns, which the check constraints keep equal to
-- the document. Rows are written once, idempotently by grant ID, and never
-- changed or deleted by the runtime role (retention is the retention task's).
-- Like runtime_grants there is no foreign key to execution's attempt table or to
-- gitauth's grant table: the references are opaque and validated against the
-- owners' state by the use cases. The project reference is a composite foreign
-- key, so a record can never name another tenant's project.
--
-- Registered as its own contribution after the run sources (011) and source
-- provenance (013) contributions. Nothing here is wired into a composition (T-132
-- lists it).

CREATE TABLE runtime_git_grants (
    tenant_id     kodama_id NOT NULL,
    grant_id      kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    run_id        kodama_id NOT NULL,
    attempt_id    kodama_id NOT NULL,
    kind          text NOT NULL
                  CONSTRAINT runtime_git_grants_kind_known CHECK (kind IN ('seed', 'integration')),
    repository_id kodama_id NOT NULL,
    claims        jsonb NOT NULL
                  CONSTRAINT runtime_git_grants_claims_object CHECK (jsonb_typeof(claims) = 'object'),
    recorded_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, grant_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    -- the columns equal the stored claims document
    CONSTRAINT runtime_git_grants_claims_match CHECK (
        claims ->> 'grantId' = grant_id
        AND claims ->> 'tenantId' = tenant_id
        AND claims ->> 'projectId' = project_id
        AND claims ->> 'kind' = kind
        AND claims ->> 'audience' = 'kodama-git'
        AND claims #>> '{actor,type}' = 'workload'
        AND claims #>> '{actor,runId}' = run_id
        AND claims #>> '{actor,attemptId}' = attempt_id
        AND jsonb_array_length(claims -> 'repositories') = 1
        AND claims #>> '{repositories,0,id}' = repository_id)
);

CREATE INDEX runtime_git_grants_attempt ON runtime_git_grants (tenant_id, attempt_id);

ALTER TABLE runtime_git_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE runtime_git_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON runtime_git_grants TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON runtime_git_grants
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- A record is immutable. The column grants below already withhold UPDATE and
-- DELETE from the runtime role; the trigger states the rule for every other
-- role, the owner included.
CREATE FUNCTION runtime_git_grants_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'a runtime Git grant record is immutable' USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER runtime_git_grants_immutable BEFORE UPDATE OR DELETE ON runtime_git_grants
    FOR EACH ROW EXECUTE FUNCTION runtime_git_grants_immutable();

-- Least privilege: the runtime role records and reads, nothing else.
GRANT SELECT, INSERT ON runtime_git_grants TO {{runtime_role}};
