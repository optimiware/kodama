-- T-045 business dependency graph: same-project prerequisite links between work
-- items.
--
-- A row says "dependent_id cannot start until prerequisite_id is done". It is a
-- business edge, not a pipeline step dependency. Follows the base conventions:
-- opaque kodama_id keys, tenant and project ownership through composite foreign
-- keys to the scoped work item key, ENABLE + FORCE row level security on
-- kodama_tenant_id(), and minimal runtime grants. Cycle rejection is not a
-- constraint (a graph property is not row-local): every insertion runs under the
-- project row lock and checks reachability in the same transaction.

CREATE TABLE work_item_dependencies (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    dependent_id     kodama_id NOT NULL,
    prerequisite_id  kodama_id NOT NULL,
    -- The history entry (and therefore the owner version) that created the link.
    history_id       kodama_id NOT NULL,
    created_by_type  text NOT NULL
                     CONSTRAINT work_item_dependencies_created_by_type_known CHECK (created_by_type IN ('user', 'service', 'workload', 'operator')),
    created_by_id    text NOT NULL
                     CONSTRAINT work_item_dependencies_created_by_id_length CHECK (char_length(created_by_id) BETWEEN 1 AND 256),
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, dependent_id, prerequisite_id),
    CONSTRAINT work_item_dependencies_not_self CHECK (dependent_id <> prerequisite_id),
    -- Both endpoints must be work items of the same tenant and project. There is
    -- deliberately no ON DELETE action: work items and history are never
    -- cascade-deleted.
    CONSTRAINT work_item_dependencies_dependent_fkey
        FOREIGN KEY (tenant_id, project_id, dependent_id) REFERENCES work_items (tenant_id, project_id, id),
    CONSTRAINT work_item_dependencies_prerequisite_fkey
        FOREIGN KEY (tenant_id, project_id, prerequisite_id) REFERENCES work_items (tenant_id, project_id, id)
);

-- The primary key serves prerequisite listing and the recursive reachability
-- walk (dependent -> prerequisites); this index serves the reverse direction.
CREATE INDEX work_item_dependencies_reverse_idx
    ON work_item_dependencies (tenant_id, project_id, prerequisite_id, dependent_id);

ALTER TABLE work_item_dependencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_dependencies FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON work_item_dependencies TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON work_item_dependencies
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Links are inserted and deleted by request handling and never updated; the
-- change itself is recorded in work_item_history, audit and the outbox.
GRANT SELECT, INSERT, DELETE ON work_item_dependencies TO {{runtime_role}};
