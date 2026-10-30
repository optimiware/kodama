-- Per-project state quotas. The trigger serializes on the usage row, so two
-- concurrent writes cannot both pass an otherwise stale count.
CREATE TABLE state_usage (
    tenant_id   text NOT NULL,
    project_id  text NOT NULL,
    entries     bigint NOT NULL DEFAULT 0,
    value_bytes bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, project_id),
    CONSTRAINT state_usage_counts CHECK (entries >= 0 AND value_bytes >= 0)
);

ALTER TABLE state_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE state_usage FORCE ROW LEVEL SECURITY;
CREATE POLICY state_usage_tenant ON state_usage
    USING (tenant_id = current_setting('kodama.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('kodama.tenant_id', true));

GRANT SELECT, INSERT, UPDATE ON state_usage TO {{runtime_role}};

CREATE OR REPLACE FUNCTION state_usage_apply() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    delta_entries bigint;
    delta_bytes   bigint;
    v_tenant      text;
    v_project     text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        v_tenant := NEW.tenant_id;
        v_project := NEW.project_id;
        delta_entries := 1;
        delta_bytes := octet_length(NEW.value::text) + octet_length(NEW.artifact_references::text);
    ELSIF TG_OP = 'DELETE' THEN
        v_tenant := OLD.tenant_id;
        v_project := OLD.project_id;
        delta_entries := -1;
        delta_bytes := -(octet_length(OLD.value::text) + octet_length(OLD.artifact_references::text));
    ELSE
        v_tenant := NEW.tenant_id;
        v_project := NEW.project_id;
        delta_entries := 0;
        delta_bytes := (octet_length(NEW.value::text) + octet_length(NEW.artifact_references::text))
                     - (octet_length(OLD.value::text) + octet_length(OLD.artifact_references::text));
    END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO state_usage (tenant_id, project_id, entries, value_bytes)
        VALUES (v_tenant, v_project, delta_entries, delta_bytes)
        ON CONFLICT (tenant_id, project_id) DO UPDATE
           SET entries = state_usage.entries + delta_entries,
               value_bytes = state_usage.value_bytes + delta_bytes;
    ELSE
        UPDATE state_usage SET entries = entries + delta_entries,
                               value_bytes = value_bytes + delta_bytes
         WHERE tenant_id = v_tenant AND project_id = v_project;
        IF NOT FOUND AND TG_OP = 'UPDATE' THEN
            INSERT INTO state_usage (tenant_id, project_id, entries, value_bytes)
            VALUES (v_tenant, v_project, 1, delta_bytes);
        END IF;
    END IF;
    IF (SELECT entries FROM state_usage WHERE tenant_id = v_tenant AND project_id = v_project) > 10000
       OR (SELECT value_bytes FROM state_usage WHERE tenant_id = v_tenant AND project_id = v_project) > 67108864 THEN
        RAISE EXCEPTION 'state quota exceeded for project %', v_project USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER state_entries_usage
AFTER INSERT OR UPDATE OR DELETE ON state_entries
FOR EACH ROW EXECUTE FUNCTION state_usage_apply();

-- The trigger must be able to update the usage row even when the caller only
-- has INSERT/UPDATE on state_entries.
GRANT SELECT, INSERT, UPDATE ON state_usage TO {{runtime_role}};
