-- T-149 source-set provenance (additive; contribution of internal/artifacts).
--
-- Under ADR 0011 (DQ-17) the evidence, the approval and the deployment of a
-- multi-repository candidate identify the whole source set: the canonical digest
-- of the run's pinned set and, for every repository of it, one commit. This
-- migration adds that identity beside the single repository URL and commit,
-- without rewriting any row:
--
--   * artifacts          source_set_digest, source_commits: the provenance of an
--                        attempt of a source-set run (the commits it worked from);
--                        an OCI registration carries one or the other;
--   * deployments        the same pair beside the repository URL and commit, which
--                        stay NOT NULL: for a set they are the primary source's
--                        (the first writable source in repository order, else the
--                        first), so every reader of one repository and commit still
--                        sees a valid one (AC-A08); the set defines the candidate;
--   * gates              source_set_digest, and candidate_commit becomes NULL
--                        exactly for a gate whose canonical subject names a source
--                        set (the subject holds the per-repository commits and
--                        integration bases; the API presents the primary source's
--                        commit as candidateCommit).
--
-- Every earlier row has NULL in the new columns and keeps its stored shape; a
-- single-source subject keeps its canonical bytes and digest. Nothing here
-- deletes, grants or opens anything: the new columns are covered by the table
-- grants of the baseline.
--
-- Ownership. A source set names repositories by ID inside a JSON list, so a
-- foreign key is not possible; triggers check, in the writing transaction, that
-- every repository of a stored set belongs to the row's own tenant and project
-- (artifacts, deployments, gates), and that a gate's set is the pinned set of its
-- own run: its digest is the run's source_set_digest and each repository is one
-- of the run's run_sources. An artifact's or deployment's set is tied to its run
-- by the service layer only: the artifact owner takes it from the attempt's own
-- trusted grant and delivery compares it with the attempt's pinned context, and a
-- row does not always have a run row at hand (the artifact fixtures keep none).
--
-- Order. 013 depends on 007 (repositories) and 011 (runs.source_set_digest and
-- run_sources): T-132 registers it after both and before 012. The function and
-- the triggers read those tables when a row is written, not when the migration is
-- applied.

-- A stored per-repository commit list: an array of {repositoryId, commit}
-- objects (exactly those members), ordered strictly by repository ID (bytewise),
-- 1 to 32 entries, every commit a non-zero object name.
CREATE FUNCTION t149_source_commits_valid(doc jsonb) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    e    jsonb;
    prev text;
    n    integer := 0;
BEGIN
    IF doc IS NULL OR jsonb_typeof(doc) <> 'array' THEN
        RETURN false;
    END IF;
    FOR e IN SELECT value FROM jsonb_array_elements(doc) LOOP
        IF jsonb_typeof(e) <> 'object' OR (SELECT count(*) FROM jsonb_object_keys(e)) <> 2
           OR NOT (e ? 'repositoryId' AND e ? 'commit')
           OR jsonb_typeof(e->'repositoryId') <> 'string' OR jsonb_typeof(e->'commit') <> 'string'
           OR (e->>'repositoryId') !~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$'
           OR (e->>'commit') !~ '^([0-9a-f]{40}|[0-9a-f]{64})$' OR (e->>'commit') ~ '^0+$'
           OR (prev IS NOT NULL AND prev COLLATE "C" >= (e->>'repositoryId') COLLATE "C") THEN
            RETURN false;
        END IF;
        prev := e->>'repositoryId';
        n := n + 1;
    END LOOP;
    RETURN n BETWEEN 1 AND 32;
END $$;

-- A commit is one of a stored set's.
CREATE FUNCTION t149_commit_in_set(doc jsonb, c text) RETURNS boolean
    LANGUAGE sql IMMUTABLE AS $$
    SELECT EXISTS (SELECT 1 FROM jsonb_array_elements(doc) e WHERE e->>'commit' = c)
$$;

-- Every repository of a stored set belongs to the tenant and project.
CREATE FUNCTION t149_source_commits_owned(p_tenant text, p_project text, doc jsonb) RETURNS boolean
    LANGUAGE sql STABLE AS $$
    SELECT NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(doc) e
        WHERE NOT EXISTS (SELECT 1 FROM repositories r WHERE r.tenant_id = p_tenant AND r.project_id = p_project AND r.id = e->>'repositoryId'))
$$;

-- ---- artifacts ------------------------------------------------------------

ALTER TABLE artifacts
    ADD COLUMN source_set_digest text
        CONSTRAINT artifacts_source_set_digest_syntax CHECK (source_set_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN source_commits jsonb
        CONSTRAINT artifacts_source_commits_shape CHECK (source_commits IS NULL OR t149_source_commits_valid(source_commits)),
    ADD CONSTRAINT artifacts_source_set_both CHECK ((source_set_digest IS NULL) = (source_commits IS NULL)),
    -- A single source and a source set are alternatives.
    ADD CONSTRAINT artifacts_source_single_or_set CHECK (source_set_digest IS NULL OR (source_repository_url IS NULL AND source_commit IS NULL));

-- An OCI registration needs trusted provenance of either kind.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_shape;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_shape CHECK (
    (kind = 'file'
        AND state IN ('reserved', 'usable')
        AND media_type IS NOT NULL AND size_bytes IS NOT NULL AND registry IS NULL
        AND staging_key IS NOT NULL AND transfer_expires_at IS NOT NULL
        AND ((state = 'reserved' AND sealed_key IS NULL AND verified_at IS NULL AND verified_size_bytes IS NULL)
          OR (state = 'usable' AND sealed_key IS NOT NULL AND sealed_key <> staging_key
              AND verified_at IS NOT NULL AND verified_size_bytes = size_bytes)))
    OR
    (kind = 'oci'
        AND state = 'registered' AND role = 'output'
        AND registry IS NOT NULL AND media_type IS NULL AND size_bytes IS NULL
        AND staging_key IS NULL AND sealed_key IS NULL AND transfer_expires_at IS NULL
        AND (source_commit IS NOT NULL OR source_set_digest IS NOT NULL)
        AND verified_at IS NOT NULL AND verified_size_bytes IS NOT NULL
        AND claim_token IS NULL)
);

-- The provenance of an artifact is written once (the baseline guard compares the
-- single source with IS DISTINCT FROM; this one covers the source set).
CREATE FUNCTION t149_artifacts_source_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_set_digest IS DISTINCT FROM OLD.source_set_digest OR NEW.source_commits IS DISTINCT FROM OLD.source_commits THEN
        RAISE EXCEPTION 'artifact source set provenance is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER artifacts_source_set_guard BEFORE UPDATE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION t149_artifacts_source_guard();

CREATE FUNCTION t149_artifacts_source_owned() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_commits IS NOT NULL AND NOT t149_source_commits_owned(NEW.tenant_id, NEW.project_id, NEW.source_commits) THEN
        RAISE EXCEPTION 'an artifact source belongs to a repository of the artifact''s project' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER artifacts_source_set_owned BEFORE INSERT ON artifacts
    FOR EACH ROW EXECUTE FUNCTION t149_artifacts_source_owned();

-- ---- deployments ----------------------------------------------------------

ALTER TABLE deployments
    ADD COLUMN source_set_digest text
        CONSTRAINT deployments_source_set_digest_syntax CHECK (source_set_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN source_commits jsonb
        CONSTRAINT deployments_source_commits_shape CHECK (source_commits IS NULL OR t149_source_commits_valid(source_commits)),
    ADD CONSTRAINT deployments_source_set_both CHECK ((source_set_digest IS NULL) = (source_commits IS NULL)),
    -- The repository URL and commit of a set deployment are its primary source's:
    -- the commit is one of the set's.
    ADD CONSTRAINT deployments_source_primary_in_set CHECK (
        source_commits IS NULL OR t149_commit_in_set(source_commits, source_commit));

CREATE FUNCTION t149_deployments_source_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_set_digest IS DISTINCT FROM OLD.source_set_digest OR NEW.source_commits IS DISTINCT FROM OLD.source_commits THEN
        RAISE EXCEPTION 'deployment source set provenance is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER deployments_source_set_guard BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION t149_deployments_source_guard();

CREATE FUNCTION t149_deployments_source_owned() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_commits IS NOT NULL AND NOT t149_source_commits_owned(NEW.tenant_id, NEW.project_id, NEW.source_commits) THEN
        RAISE EXCEPTION 'a deployment source belongs to a repository of the deployment''s project' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER deployments_source_set_owned BEFORE INSERT ON deployments
    FOR EACH ROW EXECUTE FUNCTION t149_deployments_source_owned();

-- ---- gates ----------------------------------------------------------------

ALTER TABLE gates
    ALTER COLUMN candidate_commit DROP NOT NULL,
    ADD COLUMN source_set_digest text
        CONSTRAINT gates_source_set_digest_syntax CHECK (source_set_digest ~ '^sha256:[0-9a-f]{64}$'),
    -- A subject names one candidate commit or one source set, never both or none.
    ADD CONSTRAINT gates_candidate_or_source_set CHECK ((candidate_commit IS NULL) <> (source_set_digest IS NULL));

-- The searchable copies equal the canonical subject, and a source-set subject is
-- the pinned set of the gate's own run: its digest is the run's
-- source_set_digest and every repository is one of the run's run_sources (which
-- belong to the run's project by their own composite key), so a subject can name
-- neither a foreign repository nor another run's set.
CREATE FUNCTION t149_gates_source_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    doc     jsonb := NEW.subject::jsonb;
    sources jsonb;
    src     jsonb;
    n       integer := 0;
    prev    text;
BEGIN
    IF (doc->>'candidateCommit') IS DISTINCT FROM NEW.candidate_commit
       OR (doc->'sourceSet'->>'digest') IS DISTINCT FROM NEW.source_set_digest THEN
        RAISE EXCEPTION 'the gate columns do not match its canonical subject' USING ERRCODE = '23514';
    END IF;
    IF NEW.source_set_digest IS NULL THEN
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM runs WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND id = NEW.run_id
                   AND source_set_digest = NEW.source_set_digest) THEN
        RAISE EXCEPTION 'a gate''s source set is the pinned set of its own run' USING ERRCODE = '23514';
    END IF;
    sources := doc->'sourceSet'->'sources';
    IF sources IS NULL OR jsonb_typeof(sources) <> 'array' THEN
        RAISE EXCEPTION 'a source-set subject lists its sources' USING ERRCODE = '23514';
    END IF;
    FOR src IN SELECT value FROM jsonb_array_elements(sources) LOOP
        IF jsonb_typeof(src) <> 'object' OR jsonb_typeof(src->'repositoryId') <> 'string'
           OR (src->>'commit') !~ '^([0-9a-f]{40}|[0-9a-f]{64})$'
           OR (prev IS NOT NULL AND prev COLLATE "C" >= (src->>'repositoryId') COLLATE "C") THEN
            RAISE EXCEPTION 'a source-set subject lists its sources ordered by repository' USING ERRCODE = '23514';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM run_sources WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND run_id = NEW.run_id
                       AND repository_id = src->>'repositoryId') THEN
            RAISE EXCEPTION 'a gate source is one of its run''s sources' USING ERRCODE = '23514';
        END IF;
        prev := src->>'repositoryId';
        n := n + 1;
    END LOOP;
    IF n < 1 OR n > 32 THEN
        RAISE EXCEPTION 'a source-set subject lists 1 to 32 sources' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER gates_source_set_guard BEFORE INSERT ON gates
    FOR EACH ROW EXECUTE FUNCTION t149_gates_source_guard();

CREATE FUNCTION t149_gates_source_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.candidate_commit IS DISTINCT FROM OLD.candidate_commit OR NEW.source_set_digest IS DISTINCT FROM OLD.source_set_digest THEN
        RAISE EXCEPTION 'the subject of a gate is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER gates_source_set_immutable BEFORE UPDATE ON gates
    FOR EACH ROW EXECUTE FUNCTION t149_gates_source_immutable();
