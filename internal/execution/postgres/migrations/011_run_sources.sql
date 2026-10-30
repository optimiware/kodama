-- T-148 run origins and source sets (additive; contribution of internal/execution).
--
-- A run of the internal Git service (ADR 0011, PX-23-PX-25) pins its ORIGIN (a
-- work item, an applied push record and ref, a person, or a provisioning run) and
-- a SOURCE SET: one exact commit per repository of its own project with a unique
-- checkout path each. Both are written once at admission and never change (a
-- guard trigger refuses any rewrite, the owner included). A replay of the same
-- admission returns the stored commits whatever the branches did since.
--
-- Adds, without rewriting any existing row:
--   * runs.origin_*, runs.source_*  the origin and the endpoint, declaration and
--                      digest of the set; NULL for every earlier run, which keeps
--                      its stored shape, contexts and hashes. A run with an
--                      origin is a run_shared run executing under runtime
--                      context 3;
--   * run_sources      the pinned entries, one per repository of the set, with a
--                      composite foreign key to the project's repositories, so a
--                      foreign repository cannot be recorded;
--   * attempt_commits  the refs a successful attempt updated (its result's
--                      commits), append-only: the run's lineage and source heads;
--   * relaxed shapes   a run without a work item (push, manual, provisioning)
--                      and an attempt of a run without a workspace are valid
--                      shapes now; every earlier shape is unchanged;
--   * one attempt per run  a run_shared run never has two unsettled attempts.
--
-- Registered as its own contribution after the earlier ones (T-132 lists it in
-- numeric order, after 007 repositories: this migration references them).
-- Nothing here is wired into a composition.

ALTER TABLE runs
    ADD COLUMN origin_kind          text,
    ADD COLUMN origin_push_id       kodama_id,
    ADD COLUMN origin_repository_id kodama_id,
    ADD COLUMN origin_ref           text,
    ADD COLUMN source_base_url      text,
    ADD COLUMN source_ca_file       text,
    ADD COLUMN source_declaration   text,
    ADD COLUMN source_set_digest    text,
    ADD CONSTRAINT runs_origin_known CHECK (origin_kind IN ('work_item', 'push', 'manual', 'provision')),
    ADD CONSTRAINT runs_origin_members CHECK (
        (origin_kind IS NULL AND origin_push_id IS NULL AND origin_repository_id IS NULL AND origin_ref IS NULL
            AND source_base_url IS NULL AND source_ca_file IS NULL AND source_declaration IS NULL AND source_set_digest IS NULL)
        OR (origin_kind = 'work_item' AND work_item_id IS NOT NULL AND origin_push_id IS NULL AND origin_repository_id IS NULL AND origin_ref IS NULL)
        OR (origin_kind = 'push' AND work_item_id IS NULL AND origin_push_id IS NOT NULL AND origin_repository_id IS NOT NULL AND origin_ref IS NOT NULL)
        OR (origin_kind IN ('manual', 'provision') AND work_item_id IS NULL AND origin_push_id IS NULL AND origin_repository_id IS NULL AND origin_ref IS NULL)),
    ADD CONSTRAINT runs_origin_ref_syntax CHECK (origin_ref IS NULL OR (
        char_length(origin_ref) <= 255 AND origin_ref ~ '^refs/[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$'
        AND origin_ref NOT LIKE 'refs/kodama/%' AND origin_ref !~ '\.\.|\.lock(/|$)|\.(/|$)')),
    ADD CONSTRAINT runs_source_endpoint CHECK (source_base_url IS NULL OR (
        char_length(source_base_url) <= 512 AND source_base_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*[^\s@?#/])?$')),
    ADD CONSTRAINT runs_source_ca_file CHECK (source_ca_file IS NULL OR source_ca_file ~ '^/kodama/git/[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    ADD CONSTRAINT runs_source_declaration_syntax CHECK (source_declaration IS NULL OR source_declaration ~ '^sha256:[0-9a-f]{64}$'),
    ADD CONSTRAINT runs_source_digest_syntax CHECK (source_set_digest IS NULL OR source_set_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD CONSTRAINT runs_origin_needs_shared CHECK (origin_kind IS NULL OR run_shared IS NOT NULL),
    ADD CONSTRAINT runs_origin_repository_fk FOREIGN KEY (tenant_id, project_id, origin_repository_id)
        REFERENCES repositories (tenant_id, project_id, id);

CREATE INDEX runs_origin_push_idx ON runs (tenant_id, project_id, origin_push_id, origin_ref) WHERE origin_push_id IS NOT NULL;

-- The shape of a run: the two earlier shapes are unchanged; a run with an origin
-- is a work run without a deployment, a retained source reference or a source
-- commit (its commits are in run_sources). A work item's run keeps its
-- specification and revision; every other origin has neither work item nor
-- workspace. A source-free run (a provisioning or manual run) needs no retention;
-- a run with sources has it pending until the pinned commits are retained.
ALTER TABLE runs DROP CONSTRAINT runs_purpose_shape;
ALTER TABLE runs ADD CONSTRAINT runs_purpose_shape CHECK (
    (origin_kind IS NULL AND (
        (purpose = 'work' AND work_item_id IS NOT NULL AND workspace_id IS NOT NULL AND deployment_id IS NULL
            AND content_revision IS NOT NULL AND specification IS NOT NULL AND source_commit IS NOT NULL
            AND source_retention IN ('pending', 'confirmed') AND retained_ref_id IS NOT NULL)
        OR
        (purpose = 'cleanup' AND work_item_id IS NULL AND workspace_id IS NULL AND deployment_id IS NOT NULL
            AND content_revision IS NULL AND specification IS NULL AND source_commit IS NULL
            AND source_retention = 'not_required' AND retained_ref_id IS NULL)))
    OR
    (origin_kind IS NOT NULL AND purpose = 'work' AND deployment_id IS NULL AND source_commit IS NULL AND retained_ref_id IS NULL
        AND source_base_url IS NOT NULL AND source_declaration IS NOT NULL
        AND (source_set_digest IS NULL) = (source_retention = 'not_required')
        AND ((origin_kind = 'work_item' AND content_revision IS NOT NULL AND specification IS NOT NULL)
          OR (origin_kind <> 'work_item' AND workspace_id IS NULL AND content_revision IS NULL AND specification IS NULL))));

-- The origin and the set are written once.
CREATE FUNCTION t148_runs_origin_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.origin_kind IS DISTINCT FROM OLD.origin_kind OR NEW.origin_push_id IS DISTINCT FROM OLD.origin_push_id
       OR NEW.origin_repository_id IS DISTINCT FROM OLD.origin_repository_id OR NEW.origin_ref IS DISTINCT FROM OLD.origin_ref
       OR NEW.source_base_url IS DISTINCT FROM OLD.source_base_url OR NEW.source_ca_file IS DISTINCT FROM OLD.source_ca_file
       OR NEW.source_declaration IS DISTINCT FROM OLD.source_declaration OR NEW.source_set_digest IS DISTINCT FROM OLD.source_set_digest THEN
        RAISE EXCEPTION 'the origin and the source set of a run are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER runs_origin_guard BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION t148_runs_origin_guard();

CREATE TABLE run_sources (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    run_id          kodama_id NOT NULL,
    ordinal         integer NOT NULL CONSTRAINT run_sources_ordinal_range CHECK (ordinal BETWEEN 0 AND 31),
    path            text NOT NULL
                    CONSTRAINT run_sources_path_syntax CHECK (
                        char_length(path) <= 255 AND path ~ '^(\.|[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*)$' AND path !~ '\.\.'),
    repository_id   kodama_id NOT NULL,
    repository_name text NOT NULL CONSTRAINT run_sources_name_syntax CHECK (repository_name ~ '^[a-z][a-z0-9-]{0,62}$'),
    repository_url  text NOT NULL
                    CONSTRAINT run_sources_url_syntax CHECK (char_length(repository_url) <= 2048 AND repository_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*)?$'),
    commit          text NOT NULL CONSTRAINT run_sources_commit_syntax CHECK (commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'
                                                                            AND commit !~ '^0+$'),
    access          text NOT NULL CONSTRAINT run_sources_access_known CHECK (access IN ('read', 'write')),
    branch          text CONSTRAINT run_sources_branch_syntax CHECK (
                        char_length(branch) <= 255 AND branch ~ '^refs/heads/[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$'),
    PRIMARY KEY (tenant_id, project_id, run_id, path),
    CONSTRAINT run_sources_ordinal_unique UNIQUE (tenant_id, project_id, run_id, ordinal),
    -- A run holds a repository once: a source set has one entry per repository.
    CONSTRAINT run_sources_repository_unique UNIQUE (tenant_id, project_id, run_id, repository_id),
    -- The source belongs to the run's own project, and the repository to it too.
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories (tenant_id, project_id, id),
    CONSTRAINT run_sources_branch_for_write CHECK ((access = 'write') = (branch IS NOT NULL))
);

CREATE FUNCTION t148_run_sources_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    other record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'the pinned sources of a run are immutable' USING ERRCODE = '23514';
    END IF;
    -- The set is valid as a whole: the root path is the only entry and no path
    -- contains another.
    FOR other IN SELECT path FROM run_sources WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND run_id = NEW.run_id LOOP
        IF NEW.path = '.' OR other.path = '.' OR NEW.path = other.path
           OR starts_with(NEW.path, other.path || '/') OR starts_with(other.path, NEW.path || '/') THEN
            RAISE EXCEPTION 'source paths are unique, none contains another, and . is the only source when used' USING ERRCODE = '23514';
        END IF;
    END LOOP;
    -- Only a run with a source set has entries, and no more than the bound.
    IF NOT EXISTS (SELECT 1 FROM runs WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND id = NEW.run_id
                   AND origin_kind IS NOT NULL AND source_set_digest IS NOT NULL) THEN
        RAISE EXCEPTION 'a run without a source set has no sources' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_sources_guard BEFORE INSERT OR UPDATE OR DELETE ON run_sources FOR EACH ROW EXECUTE FUNCTION t148_run_sources_guard();

CREATE TABLE attempt_commits (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    attempt_id    kodama_id NOT NULL,
    run_id        kodama_id NOT NULL,
    ordinal       integer NOT NULL CONSTRAINT attempt_commits_ordinal_range CHECK (ordinal BETWEEN 0 AND 1023),
    repository_id kodama_id NOT NULL,
    ref           text NOT NULL
                  CONSTRAINT attempt_commits_ref_syntax CHECK (
                      char_length(ref) <= 255 AND ref ~ '^refs/[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$' AND ref NOT LIKE 'refs/kodama/%'),
    commit        text NOT NULL CONSTRAINT attempt_commits_commit_syntax CHECK (commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$' AND commit !~ '^0+$'),
    push_id       kodama_id NOT NULL,
    PRIMARY KEY (tenant_id, project_id, attempt_id, ordinal),
    CONSTRAINT attempt_commits_ref_unique UNIQUE (tenant_id, project_id, attempt_id, repository_id, ref),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories (tenant_id, project_id, id)
);
CREATE INDEX attempt_commits_run_idx ON attempt_commits (tenant_id, project_id, run_id);

CREATE FUNCTION t148_attempt_commits_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the commits an attempt reported are immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER attempt_commits_guard BEFORE UPDATE OR DELETE ON attempt_commits FOR EACH ROW EXECUTE FUNCTION t148_attempt_commits_guard();

-- An attempt of a run without a workspace (a run with an origin that has no work
-- item's workspace) binds none: its sources are in its pinned context. The CHECK
-- admits that shape, and the trigger below keeps it to runs with an origin: an
-- attempt of every earlier run (origin_kind IS NULL) still needs its workspace
-- for any access but none.
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_workspace_shape;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_workspace_shape CHECK (
    (access = 'none' AND workspace_id IS NULL AND input_commit IS NULL AND expected_tip IS NULL)
    OR (access = 'read' AND workspace_id IS NOT NULL AND input_commit IS NOT NULL AND expected_tip IS NULL)
    OR (access = 'write' AND workspace_id IS NOT NULL AND input_commit IS NOT NULL AND expected_tip IS NOT NULL)
    OR (access IN ('read', 'write') AND workspace_id IS NULL AND input_commit IS NULL AND expected_tip IS NULL));

CREATE FUNCTION t148_attempt_workspace_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.access <> 'none' AND NEW.workspace_id IS NULL AND NOT EXISTS (
        SELECT 1 FROM runs WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND id = NEW.run_id AND origin_kind IS NOT NULL) THEN
        RAISE EXCEPTION 'an attempt of a run without an origin binds a workspace when it reads or writes' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER step_attempts_workspace_guard BEFORE INSERT ON step_attempts
    FOR EACH ROW EXECUTE FUNCTION t148_attempt_workspace_guard();

-- A source-set preparation reports one receipt per source (up to 32): the cap of
-- the column is raised, and the trigger keeps the earlier 4096-byte cap for every
-- run without an origin.
ALTER TABLE run_resources DROP CONSTRAINT run_resources_receipt_bounds;
ALTER TABLE run_resources ADD CONSTRAINT run_resources_receipt_bounds CHECK (jsonb_typeof(receipt) = 'object' AND pg_column_size(receipt) <= 65536);

CREATE FUNCTION t148_receipt_size_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.receipt IS NOT NULL AND pg_column_size(NEW.receipt) > 4096 AND NOT EXISTS (
        SELECT 1 FROM runs WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND id = NEW.run_id AND origin_kind IS NOT NULL) THEN
        RAISE EXCEPTION 'the preparation receipt of a run without an origin is at most 4096 bytes' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_resources_receipt_size_guard BEFORE INSERT OR UPDATE ON run_resources
    FOR EACH ROW EXECUTE FUNCTION t148_receipt_size_guard();

-- A run_shared run executes one attempt at a time, whatever its step: the one
-- scratch and the run's Git authority belong to one attempt. The claim refuses
-- earlier; this is the database's own guard behind it.
CREATE FUNCTION t148_one_attempt_per_run_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM runs WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id AND id = NEW.run_id AND run_shared IS NOT NULL)
       AND EXISTS (
        SELECT 1 FROM step_attempts a
        LEFT JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
        WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id AND a.run_id = NEW.run_id AND a.id <> NEW.id AND c.settled_at IS NULL) THEN
        RAISE EXCEPTION 'a run_shared run has an unsettled attempt' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER step_attempts_one_per_run_guard BEFORE INSERT ON step_attempts
    FOR EACH ROW EXECUTE FUNCTION t148_one_attempt_per_run_guard();

-- ---- row level security: forced, tenant-scoped, owner policy as the rest of execution ----

ALTER TABLE run_sources     ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_sources     FORCE ROW LEVEL SECURITY;
ALTER TABLE attempt_commits ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_commits FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON run_sources     TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON attempt_commits TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON run_sources
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON attempt_commits
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- grants: append-only ----

GRANT SELECT, INSERT ON run_sources TO {{runtime_role}};
GRANT SELECT, INSERT ON attempt_commits TO {{runtime_role}};
