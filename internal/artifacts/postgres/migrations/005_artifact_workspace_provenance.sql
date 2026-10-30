-- T-116 workspace provenance of artifacts produced by run_shared attempts
-- (additive, append-only).
--
-- A run_shared attempt works in a scratch workspace that earlier steps may have
-- changed. When its command begins, the server records whether the workspace was
-- exactly the commit it started from (clean) or not (modified), whether ignored
-- files were present, and the lineage of the earlier attempts that touched the
-- scratch. An artifact the attempt produces carries that record beside its source
-- repository and commit, so a modified workspace can never stand for the commit
-- it started from (DQ-16).
--
-- The record lives in its own table, written once with the artifact and never
-- updated: the artifacts table, its guards and every existing reader are
-- unchanged, and an artifact of an attempt_private producer has no row.

CREATE TABLE artifact_workspace_provenance (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    artifact_id     kodama_id NOT NULL,
    workspace_state text NOT NULL
                    CONSTRAINT artifact_workspace_provenance_state_known CHECK (workspace_state IN ('clean', 'modified')),
    ignored_present boolean NOT NULL,
    lineage         text
                    CONSTRAINT artifact_workspace_provenance_lineage_syntax CHECK (lineage ~ '^sha256:[0-9a-f]{64}$'),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, artifact_id),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id)
);

ALTER TABLE artifact_workspace_provenance ENABLE ROW LEVEL SECURITY;

ALTER TABLE artifact_workspace_provenance FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON artifact_workspace_provenance TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON artifact_workspace_provenance
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE FUNCTION artifact_workspace_provenance_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'artifact workspace provenance is write-once and retained' USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER artifact_workspace_provenance_guard BEFORE UPDATE OR DELETE ON artifact_workspace_provenance
    FOR EACH ROW EXECUTE FUNCTION artifact_workspace_provenance_guard();

-- Least privilege: written with the artifact and read with it, never changed.
GRANT SELECT, INSERT ON artifact_workspace_provenance TO {{runtime_role}};
