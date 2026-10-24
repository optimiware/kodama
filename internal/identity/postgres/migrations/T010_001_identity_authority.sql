-- T-010 standalone identity authority.
--
-- Reuses T-002's users, tenants, projects, memberships and audit tables and
-- adds only:
--   * identity_authz_revisions: one row per (tenant, user) whose revision
--     increases on every binding change and is retained after the last binding
--     is removed. It is the lock every permission-sensitive transaction takes,
--     so a revocation and a protected mutation serialize, and the source of
--     membership.changed.effectiveVersion.
--   * identity_bootstrap: the durable, operator-only completion record of the
--     one-time bootstrap. The runtime role has no privilege on it.
--   * three SECURITY DEFINER functions with a pinned search_path that give the
--     runtime role the narrow, principal-filtered global reads it needs.

CREATE TABLE identity_authz_revisions (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    user_id    kodama_id NOT NULL REFERENCES users (id),
    revision   bigint NOT NULL DEFAULT 0
               CONSTRAINT identity_authz_revision_range CHECK (revision BETWEEN 0 AND 9007199254740991),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE identity_authz_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_authz_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON identity_authz_revisions TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON identity_authz_revisions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT, UPDATE ON identity_authz_revisions TO {{runtime_role}};

-- At most one row, ever: revoking the initial administrator later does not
-- delete it, so bootstrap cannot be re-enabled.
CREATE TABLE identity_bootstrap (
    singleton     boolean PRIMARY KEY DEFAULT true CONSTRAINT identity_bootstrap_singleton CHECK (singleton),
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    user_id       kodama_id NOT NULL REFERENCES users (id),
    binding_id    kodama_id NOT NULL,
    params_digest text NOT NULL CONSTRAINT identity_bootstrap_digest_length CHECK (char_length(params_digest) = 64),
    operator_id   text NOT NULL CONSTRAINT identity_bootstrap_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    completed_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE identity_bootstrap ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_bootstrap FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON identity_bootstrap TO CURRENT_USER USING (true) WITH CHECK (true);
-- Deliberately no runtime grant and no tenant policy: bootstrap is operator-only.

-- Maps a verified (issuer, subject) to its stable user, creating the user on
-- first login. It never creates a membership. Concurrent first logins converge
-- on one row through the unique (issuer, subject) constraint.
CREATE FUNCTION kodama_identity_map_principal(p_issuer text, p_subject text)
    RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_id text;
BEGIN
    IF p_issuer IS NULL OR p_subject IS NULL
       OR char_length(p_issuer) NOT BETWEEN 1 AND 512
       OR char_length(p_subject) NOT BETWEEN 1 AND 512 THEN
        RAISE EXCEPTION 'invalid principal' USING ERRCODE = '22023';
    END IF;
    SELECT u.id::text INTO v_id FROM public.users u WHERE u.issuer = p_issuer AND u.subject = p_subject;
    IF v_id IS NOT NULL THEN
        RETURN v_id;
    END IF;
    INSERT INTO public.users (id, issuer, subject)
    VALUES ('u_' || replace(gen_random_uuid()::text, '-', ''), p_issuer, p_subject)
    ON CONFLICT (issuer, subject) DO NOTHING
    RETURNING id::text INTO v_id;
    IF v_id IS NULL THEN
        SELECT u.id::text INTO v_id FROM public.users u WHERE u.issuer = p_issuer AND u.subject = p_subject;
    END IF;
    RETURN v_id;
END
$$;

-- The caller's own record, selected by the authenticated user ID.
CREATE FUNCTION kodama_identity_user(p_user text)
    RETURNS TABLE (id text, issuer text, subject text, created_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT u.id::text, u.issuer, u.subject, u.created_at
        FROM public.users u
        WHERE u.id = p_user
    $$;

-- Tenants where the user holds at least one binding, in ID order, after the given
-- ID. Only the authenticated user's own tenants are visible through it.
CREATE FUNCTION kodama_identity_tenants(p_user text, p_after text, p_limit integer)
    RETURNS TABLE (tenant_id text, key text, display_name text, roles text[])
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT t.id::text, t.key, t.display_name,
               COALESCE(ARRAY(
                   SELECT DISTINCT m.role FROM public.memberships m
                   WHERE m.tenant_id = t.id AND m.user_id = p_user AND m.project_id IS NULL
                   ORDER BY 1), ARRAY[]::text[])
        FROM public.tenants t
        WHERE t.id::text COLLATE "C" > p_after COLLATE "C"
          AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.tenant_id = t.id AND m.user_id = p_user)
        ORDER BY t.id::text COLLATE "C"
        LIMIT least(greatest(p_limit, 1), 1001)
    $$;

REVOKE ALL ON FUNCTION kodama_identity_map_principal(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kodama_identity_user(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kodama_identity_tenants(text, text, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kodama_identity_map_principal(text, text) TO {{runtime_role}};
GRANT EXECUTE ON FUNCTION kodama_identity_user(text) TO {{runtime_role}};
GRANT EXECUTE ON FUNCTION kodama_identity_tenants(text, text, integer) TO {{runtime_role}};
