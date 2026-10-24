-- T-011 work-management schema: work items and their immutable history.
--
-- Follows the base conventions (T002_001): opaque kodama_id keys, tenant and
-- project ownership through composite foreign keys, ENABLE + FORCE row level
-- security on kodama_tenant_id(), and minimal runtime grants. Other modules
-- (workspaces, runs, artifacts) contribute their own cross-module foreign keys
-- to work_items from their migrations; this migration creates no project,
-- workspace, run, artifact, audit or outbox table.

CREATE TABLE work_items (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    kind             text NOT NULL
                     CONSTRAINT work_items_kind_known CHECK (kind IN ('epic', 'story', 'task', 'defect')),
    title            text NOT NULL
                     CONSTRAINT work_items_title_length CHECK (char_length(title) BETWEEN 1 AND 200),
    spec             text NOT NULL DEFAULT ''
                     CONSTRAINT work_items_spec_length CHECK (octet_length(spec) <= 65536),
    acceptance_criteria jsonb NOT NULL DEFAULT '[]'
                     CONSTRAINT work_items_criteria_bounds CHECK (
                         jsonb_typeof(acceptance_criteria) = 'array'
                         AND jsonb_array_length(acceptance_criteria) <= 100
                         AND pg_column_size(acceptance_criteria) <= 262144),
    evidence_links   jsonb NOT NULL DEFAULT '[]'
                     CONSTRAINT work_items_evidence_bounds CHECK (
                         jsonb_typeof(evidence_links) = 'array'
                         AND jsonb_array_length(evidence_links) <= 50
                         AND pg_column_size(evidence_links) <= 65536),
    -- Users are validated against current scoped membership inside the
    -- mutating transaction; a global user row is not project membership, so
    -- there is deliberately no foreign key to users.
    assignee_id      kodama_id,
    parent_id        kodama_id,
    state            text NOT NULL DEFAULT 'backlog'
                     CONSTRAINT work_items_state_known CHECK (state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled')),
    blocked_reason   text
                     CONSTRAINT work_items_blocked_reason_length CHECK (char_length(blocked_reason) BETWEEN 1 AND 2048),
    version          bigint NOT NULL DEFAULT 1
                     CONSTRAINT work_items_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    content_revision bigint NOT NULL DEFAULT 1
                     CONSTRAINT work_items_content_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    -- Review subject while in review (and kept while done); completion record
    -- while done. History keeps every earlier snapshot.
    review_subject   jsonb
                     CONSTRAINT work_items_review_subject_bounds CHECK (jsonb_typeof(review_subject) = 'object' AND pg_column_size(review_subject) <= 65536),
    completion       jsonb
                     CONSTRAINT work_items_completion_bounds CHECK (jsonb_typeof(completion) = 'object' AND pg_column_size(completion) <= 65536),
    created_by_type  text NOT NULL
                     CONSTRAINT work_items_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id    text NOT NULL
                     CONSTRAINT work_items_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    -- Referenced by children, history and by other modules that must prove the
    -- same project owns the item.
    CONSTRAINT work_items_scope_key UNIQUE (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, parent_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_items_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT work_items_review_state CHECK (state <> 'review' OR review_subject IS NOT NULL),
    CONSTRAINT work_items_completion_state CHECK ((state = 'done') = (completion IS NOT NULL))
);

-- Default list order and keyset pagination.
CREATE INDEX work_items_list_idx ON work_items (tenant_id, project_id, created_at, id);
CREATE INDEX work_items_state_idx ON work_items (tenant_id, project_id, state, created_at, id);
CREATE INDEX work_items_parent_idx ON work_items (tenant_id, project_id, parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX work_items_assignee_idx ON work_items (tenant_id, project_id, assignee_id) WHERE assignee_id IS NOT NULL;
-- WIP admission counts active items of one project.
CREATE INDEX work_items_active_idx ON work_items (tenant_id, project_id) WHERE state = 'active';
-- Cancellation reconciliation scans cancelled items by key.
CREATE INDEX work_items_cancelled_idx ON work_items (tenant_id, project_id, id) WHERE state = 'cancelled';

CREATE TABLE work_item_history (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    work_item_id     kodama_id NOT NULL,
    -- The resource version this mutation produced; history is ordered by it.
    version          bigint NOT NULL
                     CONSTRAINT work_item_history_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    content_revision bigint NOT NULL
                     CONSTRAINT work_item_history_revision_range CHECK (content_revision BETWEEN 1 AND 9007199254740991),
    change_kind      text NOT NULL
                     CONSTRAINT work_item_history_kind_known CHECK (change_kind IN ('created', 'updated', 'transitioned')),
    actor_type       text NOT NULL
                     CONSTRAINT work_item_history_actor_type_known CHECK (actor_type IN ('user', 'service', 'workload', 'operator')),
    actor_id         text NOT NULL
                     CONSTRAINT work_item_history_actor_id_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    occurred_at      timestamptz NOT NULL,
    reason           text
                     CONSTRAINT work_item_history_reason_length CHECK (char_length(reason) <= 2048),
    changed_fields   text[] NOT NULL DEFAULT '{}',
    from_state       text,
    to_state         text,
    before_values    jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT work_item_history_before_bounds CHECK (jsonb_typeof(before_values) = 'object' AND pg_column_size(before_values) <= 524288),
    after_values     jsonb NOT NULL DEFAULT '{}'
                     CONSTRAINT work_item_history_after_bounds CHECK (jsonb_typeof(after_values) = 'object' AND pg_column_size(after_values) <= 524288),
    review_subject   jsonb
                     CONSTRAINT work_item_history_review_bounds CHECK (jsonb_typeof(review_subject) = 'object' AND pg_column_size(review_subject) <= 65536),
    completion       jsonb
                     CONSTRAINT work_item_history_completion_bounds CHECK (jsonb_typeof(completion) = 'object' AND pg_column_size(completion) <= 65536),
    request_id       text
                     CONSTRAINT work_item_history_request_id_length CHECK (char_length(request_id) <= 256),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT work_item_history_version_key UNIQUE (tenant_id, work_item_id, version),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_item_history_states_known CHECK (
        (from_state IS NULL OR from_state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled'))
        AND (to_state IS NULL OR to_state IN ('backlog', 'ready', 'active', 'review', 'done', 'cancelled')))
);

ALTER TABLE work_items        ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_items        FORCE ROW LEVEL SECURITY;
ALTER TABLE work_item_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_history FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON work_items TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON work_items
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- History is append-only: no UPDATE or DELETE policy exists for any role, and
-- the runtime role has no such grant.
CREATE POLICY owner_select ON work_item_history FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON work_item_history FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON work_item_history FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON work_item_history FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- Work items are never deleted by request handling (cancellation is a state).
GRANT SELECT, INSERT, UPDATE ON work_items        TO {{runtime_role}};
GRANT SELECT, INSERT         ON work_item_history TO {{runtime_role}};
