-- T-002 baseline grants: the runtime role's minimal privileges.
--
-- Every GRANT and REVOKE that names the runtime role lives here so least
-- privilege can be reviewed in one place. PUBLIC revocations stay in
-- 001_schema.sql beside the function definitions they protect.
-- {{runtime_role}} is replaced with the deployment's quoted runtime role name
-- by the migration runner (storage.MigrateOptions.RuntimeRole); a run without
-- that role fails before applying anything. The migration role owns the
-- objects 001_schema.sql creates; the runtime role never owns or alters them.
--
-- Sections, in order (the old migration ID in parentheses):
--   base tenancy             T002_001_tenancy.sql
--   audit                    T002_002_audit.sql
--   discovery                T002_003_discovery.sql
--   event delivery           T005_001_delivery.sql
--   observation replay       T005_002_observation_replay.sql
--   HTTP idempotency         T004_001_idempotency.sql
--   identity authority       T010_001_identity_authority.sql
--   work items               T011_001_work.sql
--   variables and secrets    T012_001_configuration.sql
--   secret references        T012_002_references.sql
--   pipeline definitions     T014_001_pipeline_definitions.sql
--   workspaces               T013_001_workspaces.sql
--   artifacts                T015_001_artifacts.sql
--   execution runs           T020_002_runs.sql
--   runtime authority        T019_001_runtime_grants.sql
--   execution workload cleanup T086_001_workload_cleanup.sql
--   log records              T023_001_log_records.sql
--   agent profiles           T030_001_agent_profiles.sql
--   delivery                 T035_001_delivery.sql
--   gates                    T034_001_gates.sql
--   model gateway            T032_001_gateway.sql
--   MCP proxy                T033_001_mcp_proxy.sql
--   work dependencies        T045_001_work_dependencies.sql
--   flow summaries           T042_001_flow_intervals.sql
--   ChatOps                  T041_001_chatops.sql
--   Git triggers             T100_001_git_triggers.sql


-- ---- base tenancy (was T002_001_tenancy.sql) ----

GRANT USAGE ON SCHEMA public TO {{runtime_role}};

-- Runtime grants. Tenant and user rows are provisioned by the operator or by
-- the identity module's own migration contribution, not by request handling.
GRANT SELECT                         ON tenants     TO {{runtime_role}};

GRANT SELECT, INSERT, UPDATE         ON projects    TO {{runtime_role}};

GRANT SELECT                         ON users       TO {{runtime_role}};

GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO {{runtime_role}};

-- ---- audit (was T002_002_audit.sql) ----

GRANT SELECT, INSERT ON audit TO {{runtime_role}};

-- ---- discovery (was T002_003_discovery.sql) ----

GRANT EXECUTE ON FUNCTION kodama_discover_memberships(text, text) TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_list_tenant_ids() TO {{runtime_role}};

-- ---- event delivery (was T005_001_delivery.sql) ----

GRANT SELECT, INSERT ON event_tenant_subjects TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_event_resolve_subject(text) TO {{runtime_role}};

GRANT SELECT, INSERT ON event_backlog TO {{runtime_role}};

GRANT UPDATE (pending_count, pending_bytes) ON event_backlog TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON outbox TO {{runtime_role}};

GRANT UPDATE (state, attempts, claim_token, lease_expires_at, next_attempt_at, published_at, last_failure_class, last_failure_at, last_error)
    ON outbox TO {{runtime_role}};

GRANT SELECT, INSERT ON outbox_attempts TO {{runtime_role}};

GRANT UPDATE (finished_at, outcome, error) ON outbox_attempts TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON event_inbox TO {{runtime_role}};

GRANT UPDATE (state, attempts, next_attempt_at, completed_at, last_failure_class, last_error, replays, payload) ON event_inbox TO {{runtime_role}};

GRANT SELECT, INSERT ON event_inbox_attempts TO {{runtime_role}};

GRANT SELECT, INSERT ON event_dedup_boundary TO {{runtime_role}};

GRANT UPDATE (inbox_before, broker_before, updated_at) ON event_dedup_boundary TO {{runtime_role}};

-- ---- observation replay (was T005_002_observation_replay.sql) ----

GRANT SELECT, INSERT ON event_scopes TO {{runtime_role}};

GRANT UPDATE (last_seq, floor_seq) ON event_scopes TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON event_observations TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON event_quarantine TO {{runtime_role}};

GRANT UPDATE (reason_code, diagnostic, delivery_count, cycle, state, resolved_at, resolution, resolved_by, replay_id, replay_by) ON event_quarantine TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON event_replay_requests TO {{runtime_role}};

GRANT UPDATE (state, next_seq, processed, skipped, failed, claim_token, lease_expires_at, updated_at, completed_at) ON event_replay_requests TO {{runtime_role}};

GRANT SELECT, INSERT ON event_recovery_fences TO {{runtime_role}};

GRANT UPDATE (fenced_at, fenced_by, reason, cleared_at, cleared_by, clear_reason) ON event_recovery_fences TO {{runtime_role}};

-- ---- HTTP idempotency (was T004_001_idempotency.sql) ----

-- Completed records are immutable until expiry: no UPDATE grant. DELETE only
-- removes expired rows (the adapter's predicate) and is tenant-scoped by policy.
GRANT SELECT, INSERT, DELETE ON idempotency TO {{runtime_role}};

-- ---- identity authority (was T010_001_identity_authority.sql) ----

GRANT SELECT, INSERT, UPDATE ON identity_authz_revisions TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_identity_map_principal(text, text) TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_identity_user(text) TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_identity_tenants(text, text, integer) TO {{runtime_role}};

-- ---- work items (was T011_001_work.sql) ----

-- Work items are never deleted by request handling (cancellation is a state).
GRANT SELECT, INSERT, UPDATE ON work_items        TO {{runtime_role}};

GRANT SELECT, INSERT         ON work_item_history TO {{runtime_role}};

-- ---- variables and secrets (was T012_001_configuration.sql) ----

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

-- ---- secret references (was T012_002_references.sql) ----

GRANT SELECT, INSERT ON secret_pins TO {{runtime_role}};

GRANT UPDATE (released_at, first_resolved_at, last_resolved_at, resolution_count) ON secret_pins TO {{runtime_role}};

GRANT SELECT, INSERT ON secret_consumer_handling TO {{runtime_role}};

GRANT UPDATE (state, resolved_at, resolved_by) ON secret_consumer_handling TO {{runtime_role}};

-- ---- pipeline definitions (was T014_001_pipeline_definitions.sql) ----

GRANT SELECT, INSERT ON pipeline_definitions TO {{runtime_role}};

-- ---- workspaces (was T013_001_workspaces.sql) ----

-- Runtime grants: no DELETE. Identity and ownership columns are immutable
-- because UPDATE is column-scoped to the mutable state.
GRANT SELECT, INSERT ON workspaces TO {{runtime_role}};

GRANT UPDATE (initial_commit, accepted_commit, state, version,
              writer_attempt_id, writer_input_commit, writer_expected_tip, writer_reserved_at,
              publish_attempt_id, publish_commit, publish_expected_commit, publish_artifact_id, publish_bundle_digest,
              publish_started_at, publish_driver_id, publish_driver_epoch, publish_driver_claimed_at,
              publish_attempts, publish_last_attempt_at,
              archive_requested_at, last_error, fence_epoch, publication_fenced, updated_at)
    ON workspaces TO {{runtime_role}};

GRANT SELECT, INSERT ON workspace_publications TO {{runtime_role}};

GRANT UPDATE (status, observed_tip, conflict_reason, outcome, settled_at) ON workspace_publications TO {{runtime_role}};

GRANT SELECT, INSERT ON workspace_retained_refs TO {{runtime_role}};

GRANT UPDATE (status, confirmed_at, released_at, removal_requested_at, removed_at) ON workspace_retained_refs TO {{runtime_role}};

-- ---- artifacts (was T015_001_artifacts.sql) ----

-- Runtime grants: no DELETE or TRUNCATE anywhere except releasing a pin, and
-- UPDATE only on mutable lifecycle columns (identity and provenance are
-- immutable by grant and by trigger).
GRANT SELECT, INSERT ON artifacts TO {{runtime_role}};

GRANT UPDATE (state, disposition, sealed_key, transfer_expires_at, verified_at, verified_size_bytes,
              claim_token, claim_generation, claim_until, updated_at, version) ON artifacts TO {{runtime_role}};

GRANT SELECT, INSERT ON artifact_objects TO {{runtime_role}};

GRANT UPDATE (state, winner, writable_until, delete_requested_at, delete_token, delete_until,
              delete_attempts, deleted_at) ON artifact_objects TO {{runtime_role}};

GRANT SELECT, INSERT, DELETE ON artifact_pins TO {{runtime_role}};

-- ---- execution runs (was T020_002_runs.sql) ----

-- ---- grants: no DELETE; UPDATE is column-scoped to the mutable state ----
GRANT SELECT, INSERT ON runs TO {{runtime_role}};

GRANT UPDATE (status, status_reason, cancel_requested_at, cancel_reason, failure_intent_at, failure_reason,
              source_retention, version, updated_at, started_at, finished_at) ON runs TO {{runtime_role}};

GRANT SELECT, INSERT ON step_runs TO {{runtime_role}};

GRANT UPDATE (status, reason, next_attempt_at, gate_ref, attempts_started, output_commit, outputs,
              version, updated_at, finished_at) ON step_runs TO {{runtime_role}};

GRANT SELECT, INSERT ON step_attempts TO {{runtime_role}};

GRANT UPDATE (status, reason, failure_class, observed_job_uid, execution_deadline_at, started_at, finished_at,
              result_hash, result, authority_revision, version, updated_at) ON step_attempts TO {{runtime_role}};

GRANT SELECT, INSERT ON attempt_controls TO {{runtime_role}};

GRANT UPDATE (dispatch_confirmed_at, claim_token, claim_until, revoked_at, termination_requested_at,
              termination_confirmed_at, launch, effect, reconciled_at, replay_safe, settled_at, step_resolved_at, updated_at)
    ON attempt_controls TO {{runtime_role}};

GRANT SELECT, INSERT ON attempt_evidence TO {{runtime_role}};

GRANT SELECT, INSERT ON run_history TO {{runtime_role}};

GRANT SELECT, INSERT ON execution_quota_scopes TO {{runtime_role}};

GRANT UPDATE (touched_at) ON execution_quota_scopes TO {{runtime_role}};

GRANT SELECT, INSERT ON execution_reservations TO {{runtime_role}};

GRANT UPDATE (released_at) ON execution_reservations TO {{runtime_role}};

GRANT SELECT, INSERT ON work_cancellation_requests TO {{runtime_role}};

GRANT UPDATE (version) ON work_cancellation_requests TO {{runtime_role}};

-- ---- runtime authority (was T019_001_runtime_grants.sql) ----

-- Least privilege: the runtime role registers, reads, consumes and revokes. It
-- can update only the consumption and revocation columns and can never delete.
GRANT SELECT, INSERT ON runtime_grants TO {{runtime_role}};

GRANT UPDATE (session_id, nonce_digest, bound_job_uid, bound_pod_name, bound_pod_uid, consumed_at, max_expires_at, revoked_at)
    ON runtime_grants TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_runtime_locate_grant(bytea) TO {{runtime_role}};

-- ---- execution workload cleanup (was T086_001_workload_cleanup.sql) ----

GRANT UPDATE (workload_removed_at) ON attempt_controls TO {{runtime_role}};

-- ---- log records (was T023_001_log_records.sql) ----

-- Runtime grants: no DELETE; UPDATE only on the mutable lifecycle columns.
GRANT SELECT, INSERT ON log_records TO {{runtime_role}};

GRANT UPDATE (state, claim_token, claim_fence, claim_until, claims, failure_class,
              snap_log_name, snap_meta_name, snap_digest, snap_size, snap_completeness, snap_reasons,
              log_artifact_id, log_digest, log_size, meta_artifact_id, meta_digest, meta_size,
              completeness, reasons, removed_at, version, updated_at) ON log_records TO {{runtime_role}};

-- ---- agent profiles (was T030_001_agent_profiles.sql) ----

GRANT SELECT, INSERT ON agent_profiles TO {{runtime_role}};

-- ---- delivery (was T035_001_delivery.sql) ----

-- Runtime grants: INSERT and SELECT, and UPDATE only on mutable columns.
-- Snapshots and history are never updated or deleted.
GRANT SELECT, INSERT ON environments TO {{runtime_role}};

GRANT UPDATE (target, namespace, kubeconfig, secrets, config, preview_ttl_seconds, version, updated_at) ON environments TO {{runtime_role}};

GRANT SELECT, INSERT ON environment_snapshots TO {{runtime_role}};

GRANT SELECT, INSERT ON deployments TO {{runtime_role}};

GRANT UPDATE (status, reason, objects, cleanup_requested_at, cleanup_state, cleanup_reason, deleted_at, version, updated_at) ON deployments TO {{runtime_role}};

GRANT SELECT, INSERT ON deployment_history TO {{runtime_role}};

-- ---- gates (was T034_001_gates.sql) ----

-- ---- grants: no DELETE; UPDATE is column-scoped to the mutable state ----
GRANT SELECT, INSERT ON gates TO {{runtime_role}};

GRANT UPDATE (status, version, decided_by_type, decided_by_id, decision, decision_reason, decided_at,
              invalidated_at, invalidation_reason, superseding_commit, superseding_revision, updated_at)
    ON gates TO {{runtime_role}};

GRANT SELECT, INSERT ON gate_evidence TO {{runtime_role}};

-- ---- model gateway (was T032_001_gateway.sql) ----

GRANT SELECT, INSERT ON model_settings_revisions TO {{runtime_role}};

GRANT SELECT, INSERT ON model_settings TO {{runtime_role}};

GRANT UPDATE (revision, enabled, endpoint, credential_ref, credential_version, models, max_requests, token_allowance,
              max_request_bytes, max_input_tokens, max_output_tokens, recording, updated_at, updated_by)
    ON model_settings TO {{runtime_role}};

GRANT SELECT, INSERT ON model_run_bindings TO {{runtime_role}};

GRANT UPDATE (requests_admitted, tokens_charged, updated_at) ON model_run_bindings TO {{runtime_role}};

GRANT SELECT, INSERT ON model_requests TO {{runtime_role}};

GRANT UPDATE (state, outcome_code, stream_started, forwarding_at, finished_at, heartbeat_at, recording_outcome)
    ON model_requests TO {{runtime_role}};

GRANT SELECT, INSERT ON model_usage TO {{runtime_role}};

-- ---- MCP proxy (was T033_001_mcp_proxy.sql) ----

GRANT SELECT, INSERT ON mcp_servers TO {{runtime_role}};

GRANT UPDATE (endpoint, secret_name, secret_version, tools, version, updated_by, updated_at, deleted_at) ON mcp_servers TO {{runtime_role}};

GRANT SELECT, INSERT ON mcp_run_bindings TO {{runtime_role}};

GRANT SELECT, INSERT ON mcp_sessions TO {{runtime_role}};

GRANT UPDATE (state, updated_at) ON mcp_sessions TO {{runtime_role}};

GRANT SELECT, INSERT ON mcp_invocations TO {{runtime_role}};

GRANT UPDATE (state, code, updated_at) ON mcp_invocations TO {{runtime_role}};

GRANT SELECT, INSERT ON mcp_run_calls TO {{runtime_role}};

GRANT UPDATE (calls) ON mcp_run_calls TO {{runtime_role}};

-- ---- work dependencies (was T045_001_work_dependencies.sql) ----

-- Links are inserted and deleted by request handling and never updated; the
-- change itself is recorded in work_item_history, audit and the outbox.
GRANT SELECT, INSERT, DELETE ON work_item_dependencies TO {{runtime_role}};

-- ---- flow summaries (was T042_001_flow_intervals.sql) ----

GRANT SELECT ON work_flow_steps, work_flow_segments, work_flow_episodes, work_flow_blocked_runs TO {{runtime_role}};

-- ---- ChatOps (was T041_001_chatops.sql) ----

-- Runtime grants: INSERT and SELECT, and UPDATE only on mutable columns.
-- Attempts and replay metadata are never deleted; nothing here is ever deleted.
GRANT SELECT, INSERT ON chatops_destinations TO {{runtime_role}};

GRANT UPDATE (name, endpoint, events, credential, enabled, binding_revision, version, updated_at) ON chatops_destinations TO {{runtime_role}};

GRANT SELECT, INSERT ON chatops_deliveries TO {{runtime_role}};

GRANT UPDATE (state, attempts, cycle_attempts, replays, next_attempt_at, claim_token, lease_expires_at, last_class, last_uncertain,
              last_status, receipt, delivered_at, failed_reason, binding_revision, version, updated_at) ON chatops_deliveries TO {{runtime_role}};

GRANT SELECT, INSERT ON chatops_delivery_attempts TO {{runtime_role}};

GRANT UPDATE (finished_at, result, class, uncertain, http_status, receipt) ON chatops_delivery_attempts TO {{runtime_role}};

GRANT SELECT, INSERT ON chatops_replays TO {{runtime_role}};

GRANT SELECT, INSERT ON chatops_delivery_fences TO {{runtime_role}};

GRANT UPDATE (fenced, reason, operator_id, updated_at) ON chatops_delivery_fences TO {{runtime_role}};

-- ---- Git triggers (was T100_001_git_triggers.sql) ----

-- Least privilege. Only the mutable columns are updatable and nothing is ever
-- deleted: deliveries are the deduplication history and the ledger is final.
GRANT SELECT, INSERT ON git_triggers TO {{runtime_role}};

GRANT UPDATE (definition_id, work_item_id, branches, secret_ref, installed_by, enabled, version, updated_at)
    ON git_triggers TO {{runtime_role}};

GRANT SELECT, INSERT ON git_trigger_deliveries TO {{runtime_role}};

GRANT UPDATE (admitting, lease_holder, lease_until, done, outcome, reason, run_id, completed_at)
    ON git_trigger_deliveries TO {{runtime_role}};

GRANT SELECT, INSERT ON git_trigger_admissions TO {{runtime_role}};

GRANT EXECUTE ON FUNCTION kodama_git_trigger_locate(text) TO {{runtime_role}};
