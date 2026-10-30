-- T-147 workspace repository associations and internal checkpoints (additive;
-- contribution of internal/workspace).
--
-- An internally hosted workspace (ADR 0011, WS-17 to WS-19) keeps the one
-- workspace per work item and the one writer reservation of the baseline
-- tables; this migration adds what internal hosting changes:
--
--   * workspaces.hosting: 'external' for every existing row (the publisher
--     model, unchanged) or 'internal'. It is immutable: the runtime role's
--     column-scoped UPDATE grant of the baseline does not include it, and a
--     workspace never switches between the two models.
--   * workspace_repositories: for each repository an internal workspace
--     touches, its task branch refs/heads/kodama/work/<work-item-id>, the base
--     commit the branch is created at (recorded before it is created), the
--     commit last accepted there and whether the branch was created.
--   * workspace_checkpoints: one row per accepted push entry of the reserved
--     writer, keyed by (repository, push record, ref) so acceptance is
--     idempotent, with the retained ref refs/kodama/runs/<run>/<n> that keeps
--     the commit reachable on the internal host and its confirmation state.
--
-- PostgreSQL owns these records; the repository's worker owns refs and objects.
-- Git is never called from a transaction that touches them. The repository
-- column is an opaque identifier whose project ownership the service verifies
-- against the repository module before it writes (T-132 may add a foreign key
-- to repositories once both migrations are registered). Row level security is
-- enabled and forced with a tenant policy that fails closed; the runtime role
-- never deletes and updates only the mutable columns.
--
-- Invariants the database also enforces:
--   * a created association has an accepted commit and an intent has none;
--   * the task branch is a Kodama task branch and unique per project and
--     repository, so two workspaces never share one;
--   * a checkpoint's retained ref is a refs/kodama/runs/ ref, unique per
--     project and repository, and a confirmed pin has its confirmation time.

ALTER TABLE workspaces
    ADD COLUMN hosting text NOT NULL DEFAULT 'external'
        CONSTRAINT workspaces_hosting_known CHECK (hosting IN ('external', 'internal'));

CREATE TABLE workspace_repositories (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    workspace_id    kodama_id NOT NULL,
    repository_id   kodama_id NOT NULL,
    task_branch     text NOT NULL
                    CONSTRAINT workspace_repositories_branch_syntax CHECK (task_branch ~ '^refs/heads/kodama/work/[A-Za-z0-9][A-Za-z0-9_-]{0,127}$'),
    base_commit     text NOT NULL
                    CONSTRAINT workspace_repositories_base_syntax CHECK (base_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    accepted_commit text
                    CONSTRAINT workspace_repositories_accepted_syntax CHECK (accepted_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    state           text NOT NULL DEFAULT 'intent'
                    CONSTRAINT workspace_repositories_state_known CHECK (state IN ('intent', 'created')),
    last_error      text
                    CONSTRAINT workspace_repositories_reason_syntax CHECK (last_error ~ '^[a-z][a-z0-9_]{0,63}$'),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, workspace_id, repository_id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_repositories_one_branch UNIQUE (tenant_id, project_id, repository_id, task_branch),
    CONSTRAINT workspace_repositories_accepted_when_created CHECK ((state = 'created') = (accepted_commit IS NOT NULL)),
    CONSTRAINT workspace_repositories_update_order CHECK (updated_at >= created_at)
);

CREATE INDEX workspace_repositories_intent_idx ON workspace_repositories (tenant_id, workspace_id) WHERE state = 'intent';

CREATE TABLE workspace_checkpoints (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    workspace_id    kodama_id NOT NULL,
    repository_id   kodama_id NOT NULL,
    push_id         kodama_id NOT NULL,
    ref             text NOT NULL
                    CONSTRAINT workspace_checkpoints_ref_syntax CHECK (ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$' AND char_length(ref) <= 255),
    commit_oid      text NOT NULL
                    CONSTRAINT workspace_checkpoints_commit_syntax CHECK (commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    previous_commit text NOT NULL
                    CONSTRAINT workspace_checkpoints_previous_syntax CHECK (previous_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    advanced        boolean NOT NULL,
    attempt_id      kodama_id NOT NULL,
    run_id          kodama_id NOT NULL,
    pin_ref         text NOT NULL
                    CONSTRAINT workspace_checkpoints_pin_syntax CHECK (pin_ref ~ '^refs/kodama/runs/[A-Za-z0-9][A-Za-z0-9_-]{0,127}/[1-9][0-9]{0,15}$'),
    pin_status      text NOT NULL DEFAULT 'intent'
                    CONSTRAINT workspace_checkpoints_pin_status_known CHECK (pin_status IN ('intent', 'confirmed')),
    created_at      timestamptz NOT NULL,
    pinned_at       timestamptz,
    PRIMARY KEY (tenant_id, project_id, workspace_id, repository_id, push_id, ref),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_checkpoints_pin_unique UNIQUE (tenant_id, project_id, repository_id, pin_ref),
    CONSTRAINT workspace_checkpoints_pinned_consistent CHECK ((pin_status = 'confirmed') = (pinned_at IS NOT NULL)),
    CONSTRAINT workspace_checkpoints_moves CHECK (NOT advanced OR commit_oid <> previous_commit)
);

CREATE INDEX workspace_checkpoints_open_idx ON workspace_checkpoints (tenant_id, workspace_id) WHERE pin_status = 'intent';

ALTER TABLE workspace_repositories ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_repositories FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON workspace_repositories TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON workspace_repositories
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE workspace_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON workspace_checkpoints TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON workspace_checkpoints
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Least privilege. The baseline's table-level SELECT and INSERT on workspaces
-- already cover the new hosting column; its column-scoped UPDATE does not, so
-- the hosting of a workspace never changes.
GRANT SELECT, INSERT ON workspace_repositories TO {{runtime_role}};
GRANT UPDATE (accepted_commit, state, last_error, updated_at) ON workspace_repositories TO {{runtime_role}};

GRANT SELECT, INSERT ON workspace_checkpoints TO {{runtime_role}};
GRANT UPDATE (pin_ref, pin_status, pinned_at) ON workspace_checkpoints TO {{runtime_role}};
