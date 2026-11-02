-- ADR 0013. Request authorization stays in use cases; SQL enforces ownership,
-- immutable admitted inputs and append-only evidence even under the runtime role.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO {{runtime_role}};
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM {{runtime_role}};
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM {{runtime_role}};

GRANT SELECT ON tenants, users TO {{runtime_role}};
GRANT SELECT, INSERT, UPDATE ON projects TO {{runtime_role}};
GRANT SELECT, INSERT, UPDATE, DELETE ON memberships, repositories, pipelines, services, secrets, mcp_servers TO {{runtime_role}};
GRANT SELECT, INSERT ON tasks, runs, run_steps, artifacts, agent_sessions, audit_events TO {{runtime_role}};
GRANT UPDATE (parent_id, blocked_by_id, title, body, state, parked_at, resume_state,
    assigned_role, assigned_user_id, repo_id, branch, context, resolution, resolved_at,
    version, updated_at) ON tasks TO {{runtime_role}};
GRANT UPDATE (status, reason, cancel_requested_at, cleanup_pending, version, started_at, finished_at) ON runs TO {{runtime_role}};
GRANT UPDATE (status, reason, runtime_id, exit_code, log_uri, outputs, version, started_at, finished_at) ON run_steps TO {{runtime_role}};
GRANT UPDATE (status, prompt_tokens, completion_tokens, messages, transcript_uri, result, version, updated_at) ON agent_sessions TO {{runtime_role}};
GRANT UPDATE (published_at, publish_attempts, next_attempt_at) ON audit_events TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_discover_memberships(text, text), kodama_list_tenant_ids(), kodama_tenant_lookup(text) TO {{runtime_role}};
GRANT UPDATE (backend, deadline_at, workspace_key, execution_order, admission_error, status, reason)
    ON runs TO {{runtime_role}};
GRANT UPDATE (action_key, dispatch_fingerprint, begin_at, attempts)
    ON run_steps TO {{runtime_role}};
GRANT UPDATE (storage_key, desired) ON services TO {{runtime_role}};
