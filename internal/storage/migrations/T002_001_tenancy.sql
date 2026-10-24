-- T-002 base tenancy schema: tenants, projects, users, memberships.
--
-- Conventions for every module migration that follows (see README.md):
--   * IDs are opaque text constrained by the kodama_id domain; times are
--     timestamptz (instants, rendered in UTC); versions are integers >= 1.
--   * Tenant-owned relations carry tenant_id; project-owned relations also
--     carry project_id. Foreign keys are composite so ownership must match.
--   * Row level security is ENABLEd and FORCEd, with a policy on
--     kodama_tenant_id() that fails closed when no tenant context is set.
--   * Tables are owned by the migration role; only the runtime role named by
--     {{runtime_role}} receives explicit, minimal grants.

GRANT USAGE ON SCHEMA public TO {{runtime_role}};

-- Same syntax as domain.IDPattern.
CREATE DOMAIN kodama_id AS text
    CONSTRAINT kodama_id_syntax CHECK (VALUE ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$');

-- Transaction-local tenant context set by storage.WithTenant. Returns NULL
-- when the setting is absent or empty (including after the transaction that
-- set it ended), so every policy comparing against it matches nothing.
CREATE FUNCTION kodama_tenant_id() RETURNS text
    LANGUAGE sql STABLE PARALLEL SAFE
    AS $$ SELECT NULLIF(pg_catalog.current_setting('kodama.tenant_id', true), '') $$;

CREATE TABLE tenants (
    id           kodama_id PRIMARY KEY,
    key          text NOT NULL UNIQUE
                 CONSTRAINT tenants_key_syntax CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
    display_name text NOT NULL DEFAULT ''
                 CONSTRAINT tenants_display_name_length CHECK (char_length(display_name) <= 256),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    version      bigint NOT NULL DEFAULT 1
                 CONSTRAINT tenants_version_range CHECK (version BETWEEN 1 AND 9007199254740991)
);

CREATE TABLE projects (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    id               kodama_id NOT NULL,
    key              text NOT NULL
                     CONSTRAINT projects_key_syntax CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
    display_name     text NOT NULL DEFAULT ''
                     CONSTRAINT projects_display_name_length CHECK (char_length(display_name) <= 256),
    settings         jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT projects_settings_object CHECK (jsonb_typeof(settings) = 'object'),
    active_wip_limit integer NOT NULL
                     CONSTRAINT projects_wip_limit_range CHECK (active_wip_limit BETWEEN 1 AND 100000),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          bigint NOT NULL DEFAULT 1
                     CONSTRAINT projects_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, key)
);

-- Global identity mapping; not tenant-owned. Runtime visibility is limited to
-- users that hold a membership in the current tenant (policy below), and
-- principal discovery goes through kodama_discover_memberships (T002_003).
CREATE TABLE users (
    id         kodama_id PRIMARY KEY,
    issuer     text NOT NULL CONSTRAINT users_issuer_length CHECK (char_length(issuer) BETWEEN 1 AND 512),
    subject    text NOT NULL CONSTRAINT users_subject_length CHECK (char_length(subject) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT now(),
    version    bigint NOT NULL DEFAULT 1
               CONSTRAINT users_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    UNIQUE (issuer, subject)
);

-- A membership binds a user to one fixed role at tenant scope (project_id IS
-- NULL) or project scope. The composite foreign key rejects a project that
-- belongs to another tenant; MATCH SIMPLE skips it for tenant-wide bindings.
CREATE TABLE memberships (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    id         kodama_id NOT NULL,
    user_id    kodama_id NOT NULL REFERENCES users (id),
    project_id kodama_id,
    role       text NOT NULL
               CONSTRAINT memberships_role_fixed CHECK (role IN ('admin', 'analyst', 'engineer', 'reviewer', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    version    bigint NOT NULL DEFAULT 1
               CONSTRAINT memberships_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT memberships_binding_unique UNIQUE NULLS NOT DISTINCT (tenant_id, user_id, project_id, role)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);
CREATE INDEX memberships_project_idx ON memberships (tenant_id, project_id);

-- Row level security. FORCE applies the policies to the owning migration role
-- too; the owner policies exist for administrative seeding and for the
-- SECURITY DEFINER discovery functions, and the migration role is never used
-- for request handling.
ALTER TABLE tenants     ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants     FORCE ROW LEVEL SECURITY;
ALTER TABLE projects    ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects    FORCE ROW LEVEL SECURITY;
ALTER TABLE users       ENABLE ROW LEVEL SECURITY;
ALTER TABLE users       FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON tenants     TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON projects    TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON users       TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON memberships TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON tenants
    USING (id = kodama_tenant_id()) WITH CHECK (id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON projects
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_members ON users FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM memberships m
        WHERE m.user_id = users.id AND m.tenant_id = kodama_tenant_id()
    ));

-- Runtime grants. Tenant and user rows are provisioned by the operator or by
-- the identity module's own migration contribution, not by request handling.
GRANT SELECT                         ON tenants     TO {{runtime_role}};
GRANT SELECT, INSERT, UPDATE         ON projects    TO {{runtime_role}};
GRANT SELECT                         ON users       TO {{runtime_role}};
GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO {{runtime_role}};
