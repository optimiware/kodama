-- T-005 observation history, quarantine, replay and recovery fences.
--
-- Observation history is the retained, per-scope sequence behind API cursors
-- (T-024). Sequences are allocated from a counter row that the appending
-- transaction locks until it commits, so a committed cursor can never skip an
-- earlier uncommitted event and a rolled-back append leaves no gap. Lock order
-- (documented in internal/events/README.md): event_backlog, then event_scopes.

-- One counter per (tenant, project) and one per tenant-wide scope
-- (scope_key = '', project_id NULL). floor_seq is the highest pruned sequence:
-- a cursor below it has expired.
CREATE TABLE event_scopes (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    scope_key  text NOT NULL,
    project_id kodama_id,
    last_seq   bigint NOT NULL DEFAULT 0 CONSTRAINT event_scopes_last_nonneg CHECK (last_seq >= 0),
    floor_seq  bigint NOT NULL DEFAULT 0 CONSTRAINT event_scopes_floor_nonneg CHECK (floor_seq >= 0),
    PRIMARY KEY (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_scopes_key_matches_project CHECK (
        (project_id IS NULL AND scope_key = '') OR (project_id IS NOT NULL AND scope_key = project_id)),
    CONSTRAINT event_scopes_floor_le_last CHECK (floor_seq <= last_seq)
);
ALTER TABLE event_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_scopes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_scopes FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_scopes FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_scopes FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_scopes TO {{runtime_role}};
GRANT UPDATE (last_seq, floor_seq) ON event_scopes TO {{runtime_role}};

CREATE TABLE event_observations (
    tenant_id        kodama_id NOT NULL,
    scope_key        text NOT NULL,
    seq              bigint NOT NULL CONSTRAINT event_observations_seq_positive CHECK (seq >= 1),
    event_id         kodama_id NOT NULL,
    event_type       text NOT NULL CONSTRAINT event_observations_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    project_id       kodama_id,
    resource_type    text NOT NULL CONSTRAINT event_observations_resource_type_length CHECK (char_length(resource_type) BETWEEN 1 AND 64),
    resource_id      kodama_id NOT NULL,
    resource_version bigint NOT NULL CONSTRAINT event_observations_version_range CHECK (resource_version BETWEEN 1 AND 9007199254740991),
    occurred_at      timestamptz NOT NULL,
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    payload          bytea NOT NULL CONSTRAINT event_observations_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    payload_digest   text NOT NULL CONSTRAINT event_observations_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, scope_key, seq),
    UNIQUE (tenant_id, event_id),
    FOREIGN KEY (tenant_id, scope_key) REFERENCES event_scopes (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE INDEX event_observations_recorded_idx ON event_observations (tenant_id, recorded_at);
CREATE INDEX event_observations_resource_idx ON event_observations (tenant_id, resource_type, resource_id, resource_version);

ALTER TABLE event_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_observations FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_observations FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
-- Retention deletes expired history; the floor is advanced in the same
-- transaction by the retention operation. No UPDATE exists: history is immutable.
CREATE POLICY tenant_delete ON event_observations FOR DELETE USING (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT, DELETE ON event_observations TO {{runtime_role}};

-- Poison and unsupported deliveries, kept with bounded diagnostics and the
-- original transport identity until an operator resolves or replays them.
-- The tenant comes from the trusted subscription, never from the payload.
CREATE TABLE event_quarantine (
    tenant_id         kodama_id NOT NULL REFERENCES tenants (id),
    id                kodama_id NOT NULL,
    consumer          text NOT NULL CONSTRAINT event_quarantine_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    -- The event ID when the envelope parsed, otherwise raw:<sha256 of payload>;
    -- a delivery whose content contradicts an existing inbox entry is keyed by
    -- its payload hash too, so it never overwrites the original's record.
    delivery_key      text NOT NULL CONSTRAINT event_quarantine_key_length CHECK (char_length(delivery_key) BETWEEN 1 AND 200),
    event_id          text CONSTRAINT event_quarantine_event_id_length CHECK (char_length(event_id) <= 128),
    event_type        text CONSTRAINT event_quarantine_type_length CHECK (char_length(event_type) <= 128),
    reason_code       text NOT NULL
                      CONSTRAINT event_quarantine_reason_known CHECK (reason_code IN
                          ('unsupported_version', 'unsupported_type', 'malformed', 'too_large', 'scope_mismatch', 'permanent_failure', 'retries_exhausted', 'beyond_horizon', 'conflicting_content')),
    diagnostic        text NOT NULL DEFAULT '' CONSTRAINT event_quarantine_diagnostic_length CHECK (char_length(diagnostic) <= 1024),
    stream            text CONSTRAINT event_quarantine_stream_length CHECK (char_length(stream) <= 256),
    stream_seq        bigint,
    subject           text CONSTRAINT event_quarantine_subject_length CHECK (char_length(subject) <= 512),
    delivery_count    integer NOT NULL DEFAULT 0,
    payload           bytea NOT NULL CONSTRAINT event_quarantine_payload_size CHECK (octet_length(payload) <= 65536),
    payload_truncated boolean NOT NULL DEFAULT false,
    state             text NOT NULL DEFAULT 'quarantined'
                      CONSTRAINT event_quarantine_state_known CHECK (state IN ('quarantined', 'resolved')),
    quarantined_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz,
    resolution        text CONSTRAINT event_quarantine_resolution_known CHECK (resolution IN ('replayed', 'discarded')),
    resolved_by       text CONSTRAINT event_quarantine_resolved_by_length CHECK (char_length(resolved_by) <= 256),
    -- Increments each time a resolved entry is quarantined again, so every
    -- resolution has its own audit identity.
    cycle             integer NOT NULL DEFAULT 1 CONSTRAINT event_quarantine_cycle_positive CHECK (cycle >= 1),
    -- The audited replay that most recently reopened this quarantine cycle and
    -- its authorized operator. A replayed event may fail transiently and be
    -- completed later by a scheduled retry, repair or ordinary delivery, none
    -- of which carries a replay; this provenance lets that completion resolve
    -- the record for the operator who authorized the replay.
    replay_id         kodama_id,
    replay_by         text CONSTRAINT event_quarantine_replay_by_length CHECK (char_length(replay_by) BETWEEN 1 AND 256),
    CONSTRAINT event_quarantine_replay_pair CHECK ((replay_id IS NULL) = (replay_by IS NULL)),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, consumer, delivery_key),
    CONSTRAINT event_quarantine_resolved_consistent CHECK ((state = 'resolved') = (resolved_at IS NOT NULL))
);
CREATE INDEX event_quarantine_open_idx ON event_quarantine (tenant_id, consumer, quarantined_at) WHERE state = 'quarantined';
ALTER TABLE event_quarantine ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_quarantine FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_quarantine FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_quarantine FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_quarantine FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
-- Only resolved records can ever be deleted.
CREATE POLICY tenant_delete_resolved ON event_quarantine FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'resolved');
GRANT SELECT, INSERT, DELETE ON event_quarantine TO {{runtime_role}};
GRANT UPDATE (reason_code, diagnostic, delivery_count, cycle, state, resolved_at, resolution, resolved_by, replay_id, replay_by) ON event_quarantine TO {{runtime_role}};

-- Durable replay requests. A request is written with its audit row in one
-- transaction and resumed idempotently: progress is the next unprocessed
-- sequence, the work itself is deduplicated by the inbox.
CREATE TABLE event_replay_requests (
    tenant_id            kodama_id NOT NULL REFERENCES tenants (id),
    id                   kodama_id NOT NULL,
    consumer             text NOT NULL CONSTRAINT event_replay_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    scope_key            text NOT NULL,
    project_id           kodama_id,
    from_seq             bigint NOT NULL CONSTRAINT event_replay_from_positive CHECK (from_seq >= 1),
    to_seq               bigint NOT NULL,
    reason               text NOT NULL CONSTRAINT event_replay_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    operator_id          text NOT NULL CONSTRAINT event_replay_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    allow_beyond_horizon boolean NOT NULL DEFAULT false,
    -- Events recorded before this instant without inbox evidence were beyond the
    -- deduplication horizon when the request was admitted (the durable boundary
    -- or the inbox retention, whichever is later). Fixed at admission.
    evidence_floor       timestamptz NOT NULL,
    effect_class         text NOT NULL CONSTRAINT event_replay_effect_known CHECK (effect_class IN ('local', 'external_intent')),
    classification       jsonb NOT NULL CONSTRAINT event_replay_classification_object CHECK (jsonb_typeof(classification) = 'object'),
    state                text NOT NULL DEFAULT 'requested'
                         CONSTRAINT event_replay_state_known CHECK (state IN ('requested', 'running', 'completed')),
    next_seq             bigint NOT NULL,
    processed            integer NOT NULL DEFAULT 0,
    skipped              integer NOT NULL DEFAULT 0,
    failed               integer NOT NULL DEFAULT 0,
    claim_token          bigint NOT NULL DEFAULT 0,
    lease_expires_at     timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, scope_key) REFERENCES event_scopes (tenant_id, scope_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_replay_range CHECK (to_seq >= from_seq),
    CONSTRAINT event_replay_progress_in_range CHECK (next_seq BETWEEN from_seq AND to_seq + 1),
    CONSTRAINT event_replay_completed_consistent CHECK ((state = 'completed') = (completed_at IS NOT NULL))
);
CREATE INDEX event_replay_active_idx ON event_replay_requests (tenant_id, created_at) WHERE state <> 'completed';
ALTER TABLE event_replay_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_replay_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_replay_requests FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_replay_requests FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_replay_requests FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
-- Only completed requests can ever be deleted.
CREATE POLICY tenant_delete_completed ON event_replay_requests FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'completed');
GRANT SELECT, INSERT, DELETE ON event_replay_requests TO {{runtime_role}};
GRANT UPDATE (state, next_seq, processed, skipped, failed, claim_token, lease_expires_at, updated_at, completed_at) ON event_replay_requests TO {{runtime_role}};

-- Restore fence: after a database restore an operator fences each consumer;
-- replay for it is refused until its owner confirms that external effects were
-- reconciled and old controllers fenced (event catalog, "Retention and recovery").
CREATE TABLE event_recovery_fences (
    tenant_id    kodama_id NOT NULL REFERENCES tenants (id),
    consumer     text NOT NULL CONSTRAINT event_fence_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    fenced_at    timestamptz NOT NULL DEFAULT now(),
    fenced_by    text NOT NULL CONSTRAINT event_fence_by_length CHECK (char_length(fenced_by) BETWEEN 1 AND 256),
    reason       text NOT NULL CONSTRAINT event_fence_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    cleared_at   timestamptz,
    cleared_by   text CONSTRAINT event_fence_cleared_by_length CHECK (char_length(cleared_by) <= 256),
    clear_reason text CONSTRAINT event_fence_clear_reason_length CHECK (char_length(clear_reason) <= 512),
    PRIMARY KEY (tenant_id, consumer)
);
ALTER TABLE event_recovery_fences ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_recovery_fences FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_recovery_fences FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_recovery_fences FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_recovery_fences FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_recovery_fences TO {{runtime_role}};
GRANT UPDATE (fenced_at, fenced_by, reason, cleared_at, cleared_by, clear_reason) ON event_recovery_fences TO {{runtime_role}};
