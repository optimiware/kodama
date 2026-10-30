-- Generic state entries shared by container plugins.
--
-- The value is opaque JSON to the server. Artifact references are explicit
-- retention roots, not a grant of access. The runtime role is granted below;
-- the table is protected by tenant RLS just like other tenant-owned data.

CREATE TABLE state_entries (
    tenant_id          text NOT NULL,
    project_id         text NOT NULL,
    namespace          text NOT NULL,
    key                text NOT NULL,
    value              jsonb NOT NULL,
    artifact_references jsonb NOT NULL DEFAULT '[]'::jsonb,
    generation         bigint NOT NULL,
    revision           bigint NOT NULL,
    updated_at         timestamptz NOT NULL,
    updated_by         text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, project_id, namespace, key),
    CONSTRAINT state_entries_name CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]{0,126}[a-z0-9])?$'),
    CONSTRAINT state_entries_key CHECK (char_length(key) BETWEEN 1 AND 512
        AND key ~ '^[A-Za-z0-9][A-Za-z0-9._/-]*$'
        AND key !~ '(^|/)\.\.?($|/)' AND position('//' in key) = 0
        AND left(key, 1) <> '/' AND right(key, 1) <> '/'),
    CONSTRAINT state_entries_generation CHECK (generation > 0),
    CONSTRAINT state_entries_revision CHECK (revision > 0),
    CONSTRAINT state_entries_value CHECK (jsonb_typeof(value) IS NOT NULL),
    CONSTRAINT state_entries_references CHECK (jsonb_typeof(artifact_references) = 'array')
);

CREATE INDEX state_entries_prefix_idx ON state_entries (tenant_id, project_id, namespace, key);

ALTER TABLE state_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE state_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY state_entries_tenant ON state_entries
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));
