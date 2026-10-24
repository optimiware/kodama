-- T-012 configuration schema: project variables, encrypted typed secrets and the
-- tenant scope keys that protect them (docs/specs/variables-and-secrets.md).
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration and restore, and
-- minimal runtime grants (column-level UPDATE where only some columns change).
--
-- Secret values are never stored here in plaintext: secret_versions holds the
-- AES-256-GCM ciphertext, nonce, format and algorithm identifiers, and the scope
-- key identity; secret_scope_key_wraps holds the scope key wrapped by a KEK
-- together with the immutable KEK reference. No plaintext-derived fingerprint is
-- stored anywhere.

-- ---- variables --------------------------------------------------------------

CREATE TABLE variables (
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    project_id    kodama_id NOT NULL,
    id            kodama_id NOT NULL,
    name          text NOT NULL
                  CONSTRAINT variables_name_syntax CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    type          text NOT NULL
                  CONSTRAINT variables_type_known CHECK (type IN ('string', 'boolean', 'integer')),
    description   text NOT NULL DEFAULT ''
                  CONSTRAINT variables_description_length CHECK (octet_length(description) <= 1024),
    value_text    text NOT NULL
                  CONSTRAINT variables_value_length CHECK (octet_length(value_text) <= 4096),
    value_version bigint NOT NULL
                  CONSTRAINT variables_value_version_range CHECK (value_version BETWEEN 1 AND 9007199254740991),
    version       bigint NOT NULL
                  CONSTRAINT variables_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    deleted_at    timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    -- Deleted rows keep their name: a pinned reference never changes meaning.
    CONSTRAINT variables_name_unique UNIQUE (tenant_id, project_id, name),
    CONSTRAINT variables_value_version_le_version CHECK (value_version <= version),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE INDEX variables_project_name_idx ON variables (tenant_id, project_id, name) WHERE deleted_at IS NULL;

-- Immutable value history: one row per value version.
CREATE TABLE variable_versions (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    variable_id kodama_id NOT NULL,
    version     bigint NOT NULL
                CONSTRAINT variable_versions_range CHECK (version BETWEEN 1 AND 9007199254740991),
    type        text NOT NULL,
    value_text  text NOT NULL
                CONSTRAINT variable_versions_value_length CHECK (octet_length(value_text) <= 4096),
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, variable_id, version),
    FOREIGN KEY (tenant_id, project_id, variable_id) REFERENCES variables (tenant_id, project_id, id)
);

CREATE FUNCTION t012_variables_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.name <> OLD.name OR NEW.type <> OLD.type OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'variable identity, name and type are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL THEN
        RAISE EXCEPTION 'a deleted variable cannot change' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'variable resource version must increase' USING ERRCODE = '23514';
    END IF;
    IF (NEW.value_text IS DISTINCT FROM OLD.value_text) <> (NEW.value_version = OLD.value_version + 1)
       OR NEW.value_version NOT IN (OLD.value_version, OLD.value_version + 1) THEN
        RAISE EXCEPTION 'a changed value requires exactly the next value version' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER variables_guard BEFORE UPDATE ON variables
    FOR EACH ROW EXECUTE FUNCTION t012_variables_guard();

-- ---- tenant scope keys ------------------------------------------------------

-- One random 32-byte key per tenant generation. The identity and generation never
-- change; only the wrapping revision advances, on rewrap.
CREATE TABLE secret_scope_keys (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    id               kodama_id NOT NULL,
    generation       integer NOT NULL
                     CONSTRAINT secret_scope_keys_generation_range CHECK (generation >= 1),
    current_revision bigint NOT NULL
                     CONSTRAINT secret_scope_keys_revision_range CHECK (current_revision BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    -- Serializes first creation: the loser of a race finds this key committed.
    CONSTRAINT secret_scope_keys_generation_unique UNIQUE (tenant_id, generation),
    UNIQUE (tenant_id, id, generation)
);

-- Every wrap ever made is retained, so restoring an older backup only needs the
-- KEK versions its wraps name.
CREATE TABLE secret_scope_key_wraps (
    tenant_id      kodama_id NOT NULL,
    key_id         kodama_id NOT NULL,
    revision       bigint NOT NULL
                   CONSTRAINT secret_scope_key_wraps_revision_range CHECK (revision BETWEEN 1 AND 9007199254740991),
    wrapped_key    bytea NOT NULL
                   CONSTRAINT secret_scope_key_wraps_size CHECK (octet_length(wrapped_key) BETWEEN 1 AND 4096),
    wrap_algorithm text NOT NULL
                   CONSTRAINT secret_scope_key_wraps_algorithm_length CHECK (char_length(wrap_algorithm) BETWEEN 1 AND 128),
    kek_provider   text NOT NULL CONSTRAINT secret_scope_key_wraps_provider_length CHECK (char_length(kek_provider) BETWEEN 1 AND 256),
    kek_id         text NOT NULL CONSTRAINT secret_scope_key_wraps_kek_id_length CHECK (char_length(kek_id) BETWEEN 1 AND 256),
    kek_version    text NOT NULL CONSTRAINT secret_scope_key_wraps_kek_version_length CHECK (char_length(kek_version) BETWEEN 1 AND 256),
    wrapped_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, key_id, revision),
    FOREIGN KEY (tenant_id, key_id) REFERENCES secret_scope_keys (tenant_id, id)
);

-- The current revision must exist; deferred so a key and its first wrap are
-- inserted in either order within one transaction.
ALTER TABLE secret_scope_keys
    ADD CONSTRAINT secret_scope_keys_current_wrap_fk
    FOREIGN KEY (tenant_id, id, current_revision) REFERENCES secret_scope_key_wraps (tenant_id, key_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION t012_scope_keys_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.id <> OLD.id OR NEW.generation <> OLD.generation
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'scope key identity and generation are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_revision <> OLD.current_revision + 1 THEN
        RAISE EXCEPTION 'the wrapping revision advances by exactly one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER secret_scope_keys_guard BEFORE UPDATE ON secret_scope_keys
    FOR EACH ROW EXECUTE FUNCTION t012_scope_keys_guard();

-- ---- secrets ----------------------------------------------------------------

CREATE TABLE secrets (
    tenant_id       kodama_id NOT NULL REFERENCES tenants (id),
    project_id      kodama_id NOT NULL,
    id              kodama_id NOT NULL,
    name            text NOT NULL
                    CONSTRAINT secrets_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    kind            text NOT NULL
                    CONSTRAINT secrets_kind_known CHECK (kind IN ('generic', 'git', 'model', 'mcp', 'kubeconfig')),
    current_version bigint NOT NULL
                    CONSTRAINT secrets_current_version_range CHECK (current_version BETWEEN 1 AND 9007199254740991),
    -- Management resource version (the ETag); distinct from value versions and
    -- from scope key wrapping revisions.
    version         bigint NOT NULL
                    CONSTRAINT secrets_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    deleted_at      timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, project_id, id),
    -- Deleted names stay reserved.
    CONSTRAINT secrets_name_unique UNIQUE (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE INDEX secrets_project_name_idx ON secrets (tenant_id, project_id, name) WHERE deleted_at IS NULL;

CREATE TABLE secret_versions (
    tenant_id      kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    secret_id      kodama_id NOT NULL,
    version        bigint NOT NULL
                   CONSTRAINT secret_versions_range CHECK (version BETWEEN 1 AND 9007199254740991),
    scope_key_id   kodama_id NOT NULL,
    key_generation integer NOT NULL,
    format_version smallint NOT NULL
                   CONSTRAINT secret_versions_format_known CHECK (format_version = 1),
    algorithm      text NOT NULL
                   CONSTRAINT secret_versions_algorithm_known CHECK (algorithm = 'aes-256-gcm/hkdf-sha256'),
    nonce          bytea NOT NULL
                   CONSTRAINT secret_versions_nonce_size CHECK (octet_length(nonce) = 12),
    ciphertext     bytea NOT NULL
                   CONSTRAINT secret_versions_ciphertext_size CHECK (octet_length(ciphertext) BETWEEN 16 AND 131088),
    created_at     timestamptz NOT NULL,
    created_by     text NOT NULL
                   CONSTRAINT secret_versions_created_by_length CHECK (char_length(created_by) BETWEEN 1 AND 256),
    -- One-way revocation: once set it never changes.
    revoked_at     timestamptz,
    revoked_by     text CONSTRAINT secret_versions_revoked_by_length CHECK (char_length(revoked_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, secret_id, version),
    UNIQUE (tenant_id, project_id, secret_id, version),
    CONSTRAINT secret_versions_revocation_pair CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    FOREIGN KEY (tenant_id, project_id, secret_id) REFERENCES secrets (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, scope_key_id, key_generation) REFERENCES secret_scope_keys (tenant_id, id, generation)
);

-- A secret always has its current value version; deferred so the secret and its
-- first version are inserted together.
ALTER TABLE secrets
    ADD CONSTRAINT secrets_current_version_fk
    FOREIGN KEY (tenant_id, id, current_version) REFERENCES secret_versions (tenant_id, secret_id, version)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION t012_secrets_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.name <> OLD.name OR NEW.kind <> OLD.kind OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'secret identity, name and kind are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
        RAISE EXCEPTION 'secret deletion is irreversible' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW.current_version <> OLD.current_version THEN
        RAISE EXCEPTION 'a deleted secret takes no new value versions' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'secret resource version must increase' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_version NOT IN (OLD.current_version, OLD.current_version + 1) THEN
        RAISE EXCEPTION 'value versions are monotonic and never skip' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER secrets_guard BEFORE UPDATE ON secrets
    FOR EACH ROW EXECUTE FUNCTION t012_secrets_guard();

CREATE FUNCTION t012_secret_versions_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'secret versions are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.secret_id <> OLD.secret_id
       OR NEW.version <> OLD.version OR NEW.scope_key_id <> OLD.scope_key_id OR NEW.key_generation <> OLD.key_generation
       OR NEW.format_version <> OLD.format_version OR NEW.algorithm <> OLD.algorithm
       OR NEW.nonce <> OLD.nonce OR NEW.ciphertext <> OLD.ciphertext
       OR NEW.created_at <> OLD.created_at OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'secret value versions are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NOT NULL
       AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.revoked_by IS DISTINCT FROM OLD.revoked_by) THEN
        RAISE EXCEPTION 'revocation is irreversible' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER secret_versions_guard BEFORE UPDATE OR DELETE ON secret_versions
    FOR EACH ROW EXECUTE FUNCTION t012_secret_versions_guard();

-- ---- row level security -----------------------------------------------------

ALTER TABLE variables             ENABLE ROW LEVEL SECURITY;
ALTER TABLE variables             FORCE ROW LEVEL SECURITY;
ALTER TABLE variable_versions     ENABLE ROW LEVEL SECURITY;
ALTER TABLE variable_versions     FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_scope_keys     ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_scope_keys     FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_scope_key_wraps ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_scope_key_wraps FORCE ROW LEVEL SECURITY;
ALTER TABLE secrets               ENABLE ROW LEVEL SECURITY;
ALTER TABLE secrets               FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_versions       ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_versions       FORCE ROW LEVEL SECURITY;

-- The owner policies serve administrative seeding and restore; the owner role
-- never serves requests.
CREATE POLICY owner_all ON variables              TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON variable_versions      TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON secret_scope_keys      TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON secret_scope_key_wraps TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON secrets                TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON secret_versions        TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON variables
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON variable_versions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON secret_scope_keys
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON secret_scope_key_wraps
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON secrets
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON secret_versions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- ---- runtime grants (minimal; no DELETE anywhere) ---------------------------

GRANT SELECT, INSERT ON variables TO {{runtime_role}};
GRANT UPDATE (description, value_text, value_version, version, updated_at, deleted_at) ON variables TO {{runtime_role}};
GRANT SELECT, INSERT ON variable_versions TO {{runtime_role}};
GRANT SELECT, INSERT ON secret_scope_keys TO {{runtime_role}};
GRANT UPDATE (current_revision) ON secret_scope_keys TO {{runtime_role}};
GRANT SELECT, INSERT ON secret_scope_key_wraps TO {{runtime_role}};
GRANT SELECT, INSERT ON secrets TO {{runtime_role}};
GRANT UPDATE (current_version, version, updated_at, deleted_at) ON secrets TO {{runtime_role}};
GRANT SELECT, INSERT ON secret_versions TO {{runtime_role}};
GRANT UPDATE (revoked_at, revoked_by) ON secret_versions TO {{runtime_role}};
