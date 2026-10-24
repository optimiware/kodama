-- T-002 narrow non-tenant paths. Both functions are SECURITY DEFINER (owned by
-- the migration role, whose owner policies permit the reads) with a pinned
-- search_path. They are the only way the runtime role sees rows outside a
-- tenant context.

-- Principal-filtered membership discovery: returns bindings only for the one
-- (issuer, subject) user supplied by the authenticated caller.
CREATE FUNCTION kodama_discover_memberships(p_issuer text, p_subject text)
    RETURNS TABLE (user_id text, tenant_id text, project_id text, role text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT m.user_id::text, m.tenant_id::text, m.project_id::text, m.role
        FROM public.users u
        JOIN public.memberships m ON m.user_id = u.id
        WHERE u.issuer = p_issuer AND u.subject = p_subject
        ORDER BY m.tenant_id, m.project_id NULLS FIRST, m.role
    $$;

-- Worker tenant enumeration: tenant IDs only, no other column.
CREATE FUNCTION kodama_list_tenant_ids()
    RETURNS SETOF text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT t.id::text FROM public.tenants t ORDER BY t.id $$;

REVOKE ALL ON FUNCTION kodama_discover_memberships(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kodama_list_tenant_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kodama_discover_memberships(text, text) TO {{runtime_role}};
GRANT EXECUTE ON FUNCTION kodama_list_tenant_ids() TO {{runtime_role}};
