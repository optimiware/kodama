-- T-005 delivery tables: tenant subject tokens, serialized backlog accounting,
-- the outbox with durable attempt history, and the consumer inbox with attempt
-- history. Tenant-owned, forced RLS, minimal grants; see internal/events/README.md.
--
-- Lifetimes are independent: outbox rows, inbox entries, observations (T005_002)
-- and quarantine records are separate tables with no cascading between them
-- other than an attempt history following its own parent row.
--
-- Enforced by the database, not only by Go:
--   * event content (payload, digest, type, ownership) is immutable: the runtime
--     role gets column-level UPDATE on the delivery-state columns only;
--   * only published outbox rows and completed inbox entries can be deleted, so
--     pending publication and unresolved quarantine survive any cleanup bug.

-- Server-assigned, unguessable-by-construction tenant subject tokens. Subjects
-- are built only from these, never from tenant-supplied text.
CREATE TABLE event_tenant_subjects (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    subject_token text NOT NULL UNIQUE
                  CONSTRAINT event_subject_token_syntax CHECK (subject_token ~ '^[a-z0-9]{16,64}$'),
    created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE event_tenant_subjects ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_tenant_subjects FORCE ROW LEVEL SECURITY;
-- Lets the definer function below resolve a presented token across tenants.
CREATE POLICY owner_select ON event_tenant_subjects FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY tenant_select ON event_tenant_subjects FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_tenant_subjects FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_tenant_subjects TO {{runtime_role}};

-- Consumer-side trusted scope resolution: the subject token of a received
-- message maps to exactly one tenant. Returns NULL for an unknown token.
CREATE FUNCTION kodama_event_resolve_subject(p_token text)
    RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT s.tenant_id::text FROM public.event_tenant_subjects s WHERE s.subject_token = p_token $$;
REVOKE ALL ON FUNCTION kodama_event_resolve_subject(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kodama_event_resolve_subject(text) TO {{runtime_role}};

-- One row per tenant, locked by every append. It serializes admission so the
-- count and byte limits cannot be overshot by concurrent writers, and gives a
-- single, consistent lock order: backlog row first, then the scope counter.
CREATE TABLE event_backlog (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    pending_count integer NOT NULL DEFAULT 0 CONSTRAINT event_backlog_count_nonneg CHECK (pending_count >= 0),
    pending_bytes bigint NOT NULL DEFAULT 0 CONSTRAINT event_backlog_bytes_nonneg CHECK (pending_bytes >= 0)
);
ALTER TABLE event_backlog ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_backlog FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_backlog FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_backlog FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_backlog FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_backlog TO {{runtime_role}};
GRANT UPDATE (pending_count, pending_bytes) ON event_backlog TO {{runtime_role}};

CREATE TABLE outbox (
    tenant_id         kodama_id NOT NULL REFERENCES tenants (id),
    event_id          kodama_id NOT NULL,
    project_id        kodama_id,
    event_type        text NOT NULL CONSTRAINT outbox_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    payload           bytea NOT NULL CONSTRAINT outbox_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    payload_digest    text NOT NULL CONSTRAINT outbox_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at        timestamptz NOT NULL DEFAULT now(),
    state             text NOT NULL DEFAULT 'pending'
                      CONSTRAINT outbox_state_known CHECK (state IN ('pending', 'claimed', 'published')),
    attempts          integer NOT NULL DEFAULT 0 CONSTRAINT outbox_attempts_nonneg CHECK (attempts >= 0),
    claim_token       bigint NOT NULL DEFAULT 0 CONSTRAINT outbox_token_nonneg CHECK (claim_token >= 0),
    lease_expires_at  timestamptz,
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    published_at      timestamptz,
    last_failure_class text CONSTRAINT outbox_failure_class_known CHECK (last_failure_class IN ('transient', 'permanent')),
    last_failure_at   timestamptz,
    last_error        text CONSTRAINT outbox_error_length CHECK (char_length(last_error) <= 512),
    PRIMARY KEY (tenant_id, event_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT outbox_state_consistent CHECK (
        (state = 'pending'   AND lease_expires_at IS NULL     AND published_at IS NULL) OR
        (state = 'claimed'   AND lease_expires_at IS NOT NULL AND published_at IS NULL) OR
        (state = 'published' AND lease_expires_at IS NULL     AND published_at IS NOT NULL))
);
CREATE INDEX outbox_due_idx ON outbox (tenant_id, next_attempt_at) WHERE state = 'pending';
CREATE INDEX outbox_lease_idx ON outbox (tenant_id, lease_expires_at) WHERE state = 'claimed';
CREATE INDEX outbox_published_idx ON outbox (tenant_id, published_at) WHERE state = 'published';
CREATE INDEX outbox_unpublished_age_idx ON outbox (tenant_id, created_at) WHERE state <> 'published';

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON outbox FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON outbox FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON outbox FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
-- Only published rows can ever be deleted.
CREATE POLICY tenant_delete_published ON outbox FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'published');
GRANT SELECT, INSERT, DELETE ON outbox TO {{runtime_role}};
GRANT UPDATE (state, attempts, claim_token, lease_expires_at, next_attempt_at, published_at, last_failure_class, last_failure_at, last_error)
    ON outbox TO {{runtime_role}};

-- Durable publication attempt history; follows its outbox row only.
CREATE TABLE outbox_attempts (
    tenant_id   kodama_id NOT NULL,
    event_id    kodama_id NOT NULL,
    attempt     integer NOT NULL CONSTRAINT outbox_attempts_attempt_positive CHECK (attempt >= 1),
    claim_token bigint NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    outcome     text NOT NULL DEFAULT 'claimed'
                CONSTRAINT outbox_attempts_outcome_known CHECK (outcome IN ('claimed', 'published', 'transient', 'permanent')),
    error       text CONSTRAINT outbox_attempts_error_length CHECK (char_length(error) <= 512),
    PRIMARY KEY (tenant_id, event_id, attempt),
    FOREIGN KEY (tenant_id, event_id) REFERENCES outbox (tenant_id, event_id) ON DELETE CASCADE
);
ALTER TABLE outbox_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON outbox_attempts FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON outbox_attempts FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON outbox_attempts FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON outbox_attempts TO {{runtime_role}};
GRANT UPDATE (finished_at, outcome, error) ON outbox_attempts TO {{runtime_role}};

-- The consumer inbox: the correctness boundary against duplicate delivery.
-- Keyed by (tenant, consumer, event). It is not a foreign key to any event
-- table, so publication and observation pruning never erase deduplication.
CREATE TABLE event_inbox (
    tenant_id          kodama_id NOT NULL REFERENCES tenants (id),
    consumer           text NOT NULL CONSTRAINT event_inbox_consumer_syntax CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    event_id           kodama_id NOT NULL,
    project_id         kodama_id,
    event_type         text NOT NULL CONSTRAINT event_inbox_type_length CHECK (char_length(event_type) BETWEEN 1 AND 64),
    -- Content identity of the delivered event, kept for the life of the entry
    -- (the payload itself is cleared on completion). Append consults it so a
    -- producer retry of an event whose outbox row and observation were pruned
    -- is recognized as the same event, never given a fresh origin while this
    -- evidence still exists.
    payload_digest     text NOT NULL CONSTRAINT event_inbox_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    state              text NOT NULL DEFAULT 'pending'
                       CONSTRAINT event_inbox_state_known CHECK (state IN ('pending', 'completed', 'quarantined')),
    attempts           integer NOT NULL DEFAULT 0 CONSTRAINT event_inbox_attempts_nonneg CHECK (attempts >= 0),
    next_attempt_at    timestamptz,
    first_seen_at      timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    last_failure_class text CONSTRAINT event_inbox_class_known CHECK (last_failure_class IN ('transient', 'permanent')),
    last_error         text CONSTRAINT event_inbox_error_length CHECK (char_length(last_error) <= 512),
    replays            integer NOT NULL DEFAULT 0 CONSTRAINT event_inbox_replays_nonneg CHECK (replays >= 0),
    -- The delivered envelope, kept while the entry is unresolved so repair can
    -- re-drive a retry long after the observation history (a separate, API
    -- oriented lifetime) and the broker copy expired. Cleared on completion.
    payload            bytea CONSTRAINT event_inbox_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    PRIMARY KEY (tenant_id, consumer, event_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    CONSTRAINT event_inbox_completed_consistent CHECK ((state = 'completed') = (completed_at IS NOT NULL)),
    CONSTRAINT event_inbox_completed_without_payload CHECK (state <> 'completed' OR payload IS NULL)
);
CREATE INDEX event_inbox_event_idx ON event_inbox (tenant_id, event_id);
CREATE INDEX event_inbox_retry_idx ON event_inbox (tenant_id, consumer, next_attempt_at) WHERE state = 'pending';
CREATE INDEX event_inbox_completed_idx ON event_inbox (tenant_id, completed_at) WHERE state = 'completed';
CREATE INDEX event_inbox_quarantine_idx ON event_inbox (tenant_id, consumer) WHERE state = 'quarantined';

ALTER TABLE event_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_inbox FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_inbox FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_inbox FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
-- Only completed entries can ever be deleted (deduplication evidence expiry).
CREATE POLICY tenant_delete_completed ON event_inbox FOR DELETE USING (tenant_id = kodama_tenant_id() AND state = 'completed');
GRANT SELECT, INSERT, DELETE ON event_inbox TO {{runtime_role}};
GRANT UPDATE (state, attempts, next_attempt_at, completed_at, last_failure_class, last_error, replays, payload) ON event_inbox TO {{runtime_role}};

-- Retry, quarantine and replay history of one inbox entry.
CREATE TABLE event_inbox_attempts (
    seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   kodama_id NOT NULL,
    consumer    text NOT NULL,
    event_id    kodama_id NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    attempt     integer NOT NULL CONSTRAINT event_inbox_attempts_attempt_nonneg CHECK (attempt >= 0),
    outcome     text NOT NULL
                CONSTRAINT event_inbox_attempts_outcome_known CHECK (outcome IN ('completed', 'retry', 'quarantined', 'replay_reopened')),
    class       text CONSTRAINT event_inbox_attempts_class_known CHECK (class IN ('transient', 'permanent')),
    error       text CONSTRAINT event_inbox_attempts_error_length CHECK (char_length(error) <= 512),
    replay_id   kodama_id,
    FOREIGN KEY (tenant_id, consumer, event_id) REFERENCES event_inbox (tenant_id, consumer, event_id) ON DELETE CASCADE
);
CREATE INDEX event_inbox_attempts_entry_idx ON event_inbox_attempts (tenant_id, consumer, event_id, seq);
ALTER TABLE event_inbox_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_inbox_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_inbox_attempts FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_inbox_attempts FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_inbox_attempts TO {{runtime_role}};

-- Durable deduplication boundary. Pruning completed inbox entries forgets
-- evidence, and no later configuration can restore it; so the boundary of what
-- may have been forgotten is persisted, per tenant, in the transaction that
-- deletes. inbox_before is the latest cutoff (completion time) ever used for
-- forgetting completed entries; broker_before the latest cutoff (publication
-- time) below which a publication no longer protected its event's evidence.
-- Both only move forward, enforced by a trigger, so raising a retention setting
-- (or restarting with different options) can never make forgotten effects
-- admissible again. Consumers, repair and replay admission read it; nothing
-- deletes it.
CREATE TABLE event_dedup_boundary (
    tenant_id     kodama_id PRIMARY KEY REFERENCES tenants (id),
    inbox_before  timestamptz NOT NULL,
    broker_before timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION kodama_event_dedup_boundary_monotonic() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.inbox_before < OLD.inbox_before OR NEW.broker_before < OLD.broker_before THEN
        RAISE EXCEPTION 'the deduplication boundary of a tenant only moves forward' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER event_dedup_boundary_monotonic BEFORE UPDATE ON event_dedup_boundary
    FOR EACH ROW EXECUTE FUNCTION kodama_event_dedup_boundary_monotonic();
ALTER TABLE event_dedup_boundary ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_dedup_boundary FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON event_dedup_boundary FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON event_dedup_boundary FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON event_dedup_boundary FOR UPDATE USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON event_dedup_boundary TO {{runtime_role}};
GRANT UPDATE (inbox_before, broker_before, updated_at) ON event_dedup_boundary TO {{runtime_role}};
