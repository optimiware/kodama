-- T-013 workspace schema: one Git-backed workspace per actionable work item,
-- publication receipts and retained source references.
--
-- Ownership and integrity enforced by the database, not only by Go:
--   * every row carries tenant_id and project_id; foreign keys are composite so a
--     workspace, receipt or pin can only reference rows of its own project;
--   * forced row level security on kodama_tenant_id(), minimal runtime grants and
--     no DELETE anywhere: source records are history;
--   * at most one workspace per work item and one workspace per repository branch;
--   * the pending-publication fields are all-or-nothing and can only exist while
--     the publishing attempt still holds the writer slot, so a writer cannot be
--     released (or reassigned) before its publication settles.
--
-- Deferred cross-module constraints (documented in internal/workspace/README.md):
-- work_item_id -> work_items (T-011), writer_attempt_id -> step_attempts (T-020)
-- and publish_artifact_id -> artifacts (T-015) are added by those modules'
-- migration contributions once their tables exist.

CREATE TABLE workspaces (
    tenant_id             kodama_id NOT NULL,
    project_id            kodama_id NOT NULL,
    id                    kodama_id NOT NULL,
    work_item_id          kodama_id NOT NULL,
    repository_id         kodama_id NOT NULL,
    repository_url        text NOT NULL
                          CONSTRAINT workspaces_repository_url_length CHECK (char_length(repository_url) BETWEEN 1 AND 2048),
    checkout_secret_ref   text NOT NULL
                          CONSTRAINT workspaces_checkout_ref_length CHECK (char_length(checkout_secret_ref) BETWEEN 1 AND 512),
    publisher_secret_ref  text NOT NULL
                          CONSTRAINT workspaces_publisher_ref_length CHECK (char_length(publisher_secret_ref) BETWEEN 1 AND 512),
    work_branch           text NOT NULL
                          CONSTRAINT workspaces_branch_syntax CHECK (work_branch ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$' AND char_length(work_branch) <= 255),
    initial_commit        text CONSTRAINT workspaces_initial_commit_syntax CHECK (initial_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    accepted_commit       text CONSTRAINT workspaces_accepted_commit_syntax CHECK (accepted_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    state                 text NOT NULL DEFAULT 'provisioning'
                          CONSTRAINT workspaces_state_known CHECK (state IN ('provisioning', 'available', 'archived')),
    version               bigint NOT NULL DEFAULT 1
                          CONSTRAINT workspaces_version_range CHECK (version BETWEEN 1 AND 9007199254740991),

    writer_attempt_id     kodama_id,
    writer_input_commit   text CONSTRAINT workspaces_writer_input_syntax CHECK (writer_input_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    writer_expected_tip   text CONSTRAINT workspaces_writer_tip_syntax CHECK (writer_expected_tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    writer_reserved_at    timestamptz,

    publish_attempt_id    kodama_id,
    publish_commit        text CONSTRAINT workspaces_publish_commit_syntax CHECK (publish_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    publish_expected_commit text CONSTRAINT workspaces_publish_expected_syntax CHECK (publish_expected_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    publish_artifact_id   kodama_id,
    publish_bundle_digest text CONSTRAINT workspaces_publish_digest_syntax CHECK (publish_bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    publish_started_at    timestamptz,
    publish_driver_id     text CONSTRAINT workspaces_publish_driver_length CHECK (char_length(publish_driver_id) <= 128),
    publish_driver_epoch  bigint,
    publish_driver_claimed_at timestamptz,
    publish_attempts      integer NOT NULL DEFAULT 0 CONSTRAINT workspaces_publish_attempts_nonneg CHECK (publish_attempts >= 0),
    publish_last_attempt_at timestamptz,

    archive_requested_at  timestamptz,
    last_error            text CONSTRAINT workspaces_last_error_code CHECK (last_error ~ '^[a-z][a-z0-9_]{0,63}$'),
    fence_epoch           bigint NOT NULL DEFAULT 1 CONSTRAINT workspaces_fence_epoch_positive CHECK (fence_epoch >= 1),
    publication_fenced    boolean NOT NULL DEFAULT false,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT workspaces_one_per_work_item UNIQUE (tenant_id, project_id, work_item_id),
    CONSTRAINT workspaces_one_per_branch UNIQUE (tenant_id, project_id, repository_id, work_branch),
    CONSTRAINT workspaces_distinct_credentials CHECK (checkout_secret_ref <> publisher_secret_ref),
    CONSTRAINT workspaces_accepted_when_usable CHECK (state = 'provisioning' OR accepted_commit IS NOT NULL),
    CONSTRAINT workspaces_writer_all_or_none CHECK (
        (writer_attempt_id IS NULL AND writer_input_commit IS NULL AND writer_expected_tip IS NULL AND writer_reserved_at IS NULL) OR
        (writer_attempt_id IS NOT NULL AND writer_input_commit IS NOT NULL AND writer_expected_tip IS NOT NULL AND writer_reserved_at IS NOT NULL)),
    CONSTRAINT workspaces_publication_all_or_none CHECK (
        (publish_attempt_id IS NULL AND publish_commit IS NULL AND publish_expected_commit IS NULL AND publish_artifact_id IS NULL
            AND publish_bundle_digest IS NULL AND publish_started_at IS NULL AND publish_driver_id IS NULL AND publish_driver_epoch IS NULL
            AND publish_driver_claimed_at IS NULL) OR
        (publish_attempt_id IS NOT NULL AND publish_commit IS NOT NULL AND publish_expected_commit IS NOT NULL AND publish_artifact_id IS NOT NULL
            AND publish_bundle_digest IS NOT NULL AND publish_started_at IS NOT NULL
            AND ((publish_driver_id IS NULL AND publish_driver_epoch IS NULL AND publish_driver_claimed_at IS NULL) OR
                 (publish_driver_id IS NOT NULL AND publish_driver_epoch IS NOT NULL AND publish_driver_claimed_at IS NOT NULL)))),
    -- Settlement before release: the publisher holds the slot until the
    -- publication is recorded.
    CONSTRAINT workspaces_publication_holds_writer CHECK (publish_attempt_id IS NULL OR (writer_attempt_id IS NOT NULL AND publish_attempt_id = writer_attempt_id)),
    CONSTRAINT workspaces_pending_differs CHECK (publish_commit IS NULL OR publish_commit <> publish_expected_commit),
    CONSTRAINT workspaces_pending_requires_available CHECK (publish_attempt_id IS NULL OR state IN ('available', 'archived'))
);
CREATE INDEX workspaces_pending_idx ON workspaces (tenant_id, id) WHERE publish_attempt_id IS NOT NULL;
CREATE INDEX workspaces_writer_idx ON workspaces (tenant_id, project_id, writer_attempt_id) WHERE writer_attempt_id IS NOT NULL;
CREATE INDEX workspaces_open_work_idx ON workspaces (tenant_id, id)
    WHERE state = 'provisioning' OR archive_requested_at IS NOT NULL OR publish_attempt_id IS NOT NULL;

-- One immutable receipt per proposing attempt; it outlives the writer slot.
CREATE TABLE workspace_publications (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    workspace_id     kodama_id NOT NULL,
    attempt_id       kodama_id NOT NULL,
    commit_oid       text NOT NULL CONSTRAINT workspace_publications_commit_syntax CHECK (commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    expected_commit  text NOT NULL CONSTRAINT workspace_publications_expected_syntax CHECK (expected_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    artifact_id      kodama_id NOT NULL,
    bundle_digest    text NOT NULL CONSTRAINT workspace_publications_digest_syntax CHECK (bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    status           text NOT NULL DEFAULT 'pending'
                     CONSTRAINT workspace_publications_status_known CHECK (status IN ('pending', 'published', 'conflict')),
    observed_tip     text CONSTRAINT workspace_publications_observed_syntax CHECK (observed_tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    conflict_reason  text CONSTRAINT workspace_publications_reason_code CHECK (conflict_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    outcome          text NOT NULL DEFAULT 'unsettled'
                     CONSTRAINT workspace_publications_outcome_known CHECK (outcome IN ('unsettled', 'succeeded', 'failed', 'cancelled')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    settled_at       timestamptz,
    PRIMARY KEY (tenant_id, project_id, workspace_id, attempt_id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_publications_settled_consistent CHECK ((status = 'pending') = (settled_at IS NULL)),
    CONSTRAINT workspace_publications_conflict_reason CHECK ((status = 'conflict') = (conflict_reason IS NOT NULL)),
    -- Only a published proposal can be the output of a successful writer.
    CONSTRAINT workspace_publications_success_published CHECK (outcome <> 'succeeded' OR status = 'published'),
    CONSTRAINT workspace_publications_differs CHECK (commit_oid <> expected_commit)
);

-- Retained references protect commits with ordinary remote branch refs. The
-- reference name is persisted and never re-derived.
CREATE TABLE workspace_retained_refs (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    workspace_id     kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    commit_oid       text NOT NULL CONSTRAINT workspace_retained_refs_commit_syntax CHECK (commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    ref_name         text NOT NULL
                     CONSTRAINT workspace_retained_refs_ref_syntax CHECK (ref_name ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$' AND char_length(ref_name) <= 400),
    purpose          text NOT NULL CONSTRAINT workspace_retained_refs_purpose_known CHECK (purpose IN ('reader', 'writer_output')),
    run_id           kodama_id NOT NULL,
    attempt_id       kodama_id,
    status           text NOT NULL DEFAULT 'intent'
                     CONSTRAINT workspace_retained_refs_status_known CHECK (status IN ('intent', 'confirmed', 'released', 'removing', 'removed')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    confirmed_at     timestamptz,
    released_at      timestamptz,
    removal_requested_at timestamptz,
    removed_at       timestamptz,
    PRIMARY KEY (tenant_id, project_id, workspace_id, id),
    FOREIGN KEY (tenant_id, project_id, workspace_id) REFERENCES workspaces (tenant_id, project_id, id),
    CONSTRAINT workspace_retained_refs_unique_ref UNIQUE (tenant_id, project_id, workspace_id, ref_name),
    CONSTRAINT workspace_retained_refs_writer_attempt CHECK ((purpose = 'writer_output') = (attempt_id IS NOT NULL)),
    CONSTRAINT workspace_retained_refs_status_times CHECK (
        (status = 'intent' AND confirmed_at IS NULL) OR
        (status <> 'intent' AND confirmed_at IS NOT NULL))
);
CREATE INDEX workspace_retained_refs_open_idx ON workspace_retained_refs (tenant_id, workspace_id) WHERE status IN ('intent', 'removing');
CREATE INDEX workspace_retained_refs_commit_idx ON workspace_retained_refs (tenant_id, project_id, workspace_id, commit_oid);

ALTER TABLE workspaces              ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces              FORCE ROW LEVEL SECURITY;
ALTER TABLE workspace_publications  ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_publications  FORCE ROW LEVEL SECURITY;
ALTER TABLE workspace_retained_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_retained_refs FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON workspaces              TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON workspace_publications  TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON workspace_retained_refs TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON workspaces
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON workspace_publications
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON workspace_retained_refs
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

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
