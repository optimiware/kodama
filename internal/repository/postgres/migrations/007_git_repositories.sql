-- T-127 Git repositories (additive; contribution of internal/repository).
--
-- A repository is a tenant- and project-owned resource (GR-01, git-repositories.md
-- "Model"): an immutable server-assigned id, a project-unique immutable name, a
-- default branch, a hosting (internal: Kodama's tenant Git service; external: a
-- read-only mapping of the project's existing configured repository), a status
-- (provisioning, ready, archived) with its reason, the ref policy and the
-- worker-reported emptiness. PostgreSQL owns these fields; Git owns refs and
-- objects. Git is never called from a transaction that touches this table.
--
-- Ownership is a composite foreign key to projects; the unique keys are
-- (tenant, id), (tenant, project, id) (so later Git tables can reference a
-- repository together with its project) and (tenant, project, name). Row level
-- security is enabled and forced with a tenant policy that fails closed. The
-- runtime role can read, insert and update only the columns that change; it can
-- never delete (deletion follows retention and records audit, GR-14).
--
-- Lifecycle invariants the database also enforces, so a bug or a second writer
-- cannot break them:
--   * provisioning -> ready, ready <-> archived only; never back to
--     provisioning; the version advances by exactly one with a visible change;
--   * a status other than ready states its reason, a ready one has none;
--   * an external record is a ready, non-empty mapping with its credential-free
--     URL and the all-false policy, an internal repository has no URL;
--   * identity, name, hosting, URL, creator and creation time are immutable;
--   * the only change without a version bump is a newer emptiness observation.
--
-- Registered as its own contribution (storage.Contribution) by T-132 after the
-- earlier ones; nothing here is wired into a composition.

CREATE TABLE repositories (
    tenant_id             kodama_id NOT NULL,
    project_id            kodama_id NOT NULL,
    id                    kodama_id NOT NULL,
    name                  text NOT NULL
                          CONSTRAINT repositories_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    default_branch        text NOT NULL
                          CONSTRAINT repositories_branch_length CHECK (char_length(default_branch) BETWEEN 1 AND 200)
                          CONSTRAINT repositories_branch_syntax CHECK (
                              default_branch ~ '^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$'
                              AND default_branch !~ '\.\.|\.lock(/|$)|\.(/|$)')
                          CONSTRAINT repositories_branch_not_reserved CHECK (
                              default_branch <> 'HEAD' AND default_branch NOT LIKE 'refs/%'
                              AND default_branch <> 'kodama' AND default_branch NOT LIKE 'kodama/%'),
    hosting               text NOT NULL
                          CONSTRAINT repositories_hosting_known CHECK (hosting IN ('internal', 'external')),
    status                text NOT NULL
                          CONSTRAINT repositories_status_known CHECK (status IN ('provisioning', 'ready', 'archived')),
    reason                text
                          CONSTRAINT repositories_reason_syntax CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    direct_push_to_default boolean NOT NULL DEFAULT false,
    empty                 boolean NOT NULL DEFAULT false,
    empty_observed_at     timestamptz,
    external_url          text
                          CONSTRAINT repositories_external_url_syntax CHECK (
                              char_length(external_url) <= 2048 AND external_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*)?$'),
    created_by_type       text NOT NULL
                          CONSTRAINT repositories_creator_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id         text NOT NULL
                          CONSTRAINT repositories_creator_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    version               bigint NOT NULL DEFAULT 1
                          CONSTRAINT repositories_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT repositories_project_id_unique UNIQUE (tenant_id, project_id, id),
    CONSTRAINT repositories_name_unique UNIQUE (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT repositories_reason_matches_status CHECK ((status = 'ready') = (reason IS NULL)),
    CONSTRAINT repositories_hosting_url CHECK ((hosting = 'external') = (external_url IS NOT NULL)),
    CONSTRAINT repositories_external_shape CHECK (hosting = 'internal' OR (status = 'ready' AND NOT empty AND NOT direct_push_to_default)),
    CONSTRAINT repositories_provisioning_not_empty CHECK (status <> 'provisioning' OR NOT empty),
    CONSTRAINT repositories_update_order CHECK (updated_at >= created_at)
);

CREATE INDEX repositories_list_idx ON repositories (tenant_id, project_id, created_at, id);

CREATE INDEX repositories_provisioning_idx ON repositories (tenant_id, id) WHERE status = 'provisioning';

ALTER TABLE repositories ENABLE ROW LEVEL SECURITY;

ALTER TABLE repositories FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON repositories TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON repositories
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express.
CREATE FUNCTION repositories_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.id, NEW.name, NEW.hosting, NEW.external_url,
        NEW.created_by_type, NEW.created_by_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.id, OLD.name, OLD.hosting, OLD.external_url,
        OLD.created_by_type, OLD.created_by_id, OLD.created_at) THEN
        RAISE EXCEPTION 'repository identity, name, hosting, URL and creator are immutable' USING ERRCODE = '23514';
    END IF;

    IF NEW.version = OLD.version THEN
        -- A newer observation of the same facts only advances the observation time.
        IF (NEW.default_branch, NEW.status, NEW.reason, NEW.direct_push_to_default, NEW.empty, NEW.updated_at)
           IS DISTINCT FROM
           (OLD.default_branch, OLD.status, OLD.reason, OLD.direct_push_to_default, OLD.empty, OLD.updated_at) THEN
            RAISE EXCEPTION 'a repository change advances its version' USING ERRCODE = '23514';
        END IF;
        IF NEW.empty_observed_at IS NOT NULL AND OLD.empty_observed_at IS NOT NULL AND NEW.empty_observed_at < OLD.empty_observed_at THEN
            RAISE EXCEPTION 'an emptiness observation never moves back' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'repository version advances by one' USING ERRCODE = '23514';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'provisioning' AND NEW.status = 'ready')
        OR (OLD.status = 'ready' AND NEW.status = 'archived')
        OR (OLD.status = 'archived' AND NEW.status = 'ready')) THEN
        RAISE EXCEPTION 'repository status % cannot become %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    IF NEW.empty_observed_at IS NOT NULL AND OLD.empty_observed_at IS NOT NULL AND NEW.empty_observed_at < OLD.empty_observed_at THEN
        RAISE EXCEPTION 'an emptiness observation never moves back' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER repositories_guard BEFORE UPDATE ON repositories
    FOR EACH ROW EXECUTE FUNCTION repositories_guard();

-- Least privilege: the runtime role reads and inserts repositories and updates
-- only what the lifecycle changes. It can never delete.
GRANT SELECT, INSERT ON repositories TO {{runtime_role}};

GRANT UPDATE (default_branch, status, reason, direct_push_to_default, empty, empty_observed_at, version, updated_at)
    ON repositories TO {{runtime_role}};
