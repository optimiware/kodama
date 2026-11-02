-- 005_artifact_releases_and_publications.sql: ADR-0012 immutable release manifests
-- and verified destination publication receipts (T-141/T-144).

CREATE TABLE artifact_releases (
    tenant_id       text        NOT NULL,
    project_id      text        NOT NULL,
    id              text        NOT NULL,
    name            text        NOT NULL,
    version_label   text        NOT NULL,
    manifest_digest text        NOT NULL,
    manifest        bytea       NOT NULL,
    created_by_type text        NOT NULL,
    created_by_id   text        NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    retired_at      timestamptz NULL,
    version         bigint      NOT NULL CHECK (version >= 1),
    PRIMARY KEY (tenant_id, project_id, id),
    UNIQUE (tenant_id, project_id, name, version_label),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id) ON DELETE RESTRICT
);

REVOKE ALL ON artifact_releases FROM PUBLIC;
ALTER TABLE artifact_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_releases FORCE ROW LEVEL SECURITY;

CREATE POLICY artifact_releases_tenant_isolation ON artifact_releases
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));

CREATE TABLE artifact_release_entries (
    tenant_id   text NOT NULL,
    project_id  text NOT NULL,
    release_id  text NOT NULL,
    entry_name  text NOT NULL,
    artifact_id text NOT NULL,
    digest      text NOT NULL,
    kind        text NOT NULL,
    media_type  text NULL,
    PRIMARY KEY (tenant_id, project_id, release_id, entry_name),
    FOREIGN KEY (tenant_id, project_id, release_id) REFERENCES artifact_releases (tenant_id, project_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id) ON DELETE RESTRICT
);

REVOKE ALL ON artifact_release_entries FROM PUBLIC;
ALTER TABLE artifact_release_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_release_entries FORCE ROW LEVEL SECURITY;

CREATE POLICY artifact_release_entries_tenant_isolation ON artifact_release_entries
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));

CREATE TABLE artifact_publications (
    tenant_id             text        NOT NULL,
    project_id            text        NOT NULL,
    id                    text        NOT NULL,
    artifact_id           text        NOT NULL,
    artifact_digest       text        NOT NULL,
    destination_kind      text        NOT NULL,
    destination_reference text        NOT NULL,
    destination_digest    text        NOT NULL,
    run_id                text        NOT NULL,
    attempt_id            text        NOT NULL,
    step_id               text        NOT NULL,
    complete_graph        boolean     NOT NULL CHECK (complete_graph = true),
    verified_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    UNIQUE (tenant_id, project_id, artifact_id, destination_kind, destination_reference),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id) ON DELETE RESTRICT
);

REVOKE ALL ON artifact_publications FROM PUBLIC;
ALTER TABLE artifact_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_publications FORCE ROW LEVEL SECURITY;

CREATE POLICY artifact_publications_tenant_isolation ON artifact_publications
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));

GRANT SELECT, INSERT ON artifact_releases TO {{runtime_role}};
GRANT UPDATE (retired_at, updated_at, version) ON artifact_releases TO {{runtime_role}};
GRANT SELECT, INSERT ON artifact_release_entries TO {{runtime_role}};
GRANT SELECT, INSERT ON artifact_publications TO {{runtime_role}};
