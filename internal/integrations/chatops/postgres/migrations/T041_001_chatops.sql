-- T-041 ChatOps schema: project-scoped generic webhook destinations, durable
-- notification deliveries with their complete attempt history, replay audit
-- metadata and the per-tenant restore fence.
--
-- Conventions follow T002_001: tenant_id on every relation, composite foreign
-- keys so ownership must match, ENABLE + FORCE row level security with a tenant
-- policy that fails closed, an owner policy for administration and restore, and
-- minimal runtime grants (column-level UPDATE; no DELETE anywhere: unresolved
-- deliveries and their deduplication identities are never pruned
-- opportunistically).
--
-- Ordered after the base, event and identity contributions: it references
-- tenants and projects only; the event bus delivers into it through the
-- transaction of its own inbox.
--
-- No credential value, URL secret, response body or arbitrary event content is
-- stored. A destination holds a credential *reference* only.

-- ---- destinations -----------------------------------------------------------

CREATE TABLE chatops_destinations (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    name             text NOT NULL
                     CONSTRAINT chatops_destinations_name_syntax CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    endpoint         text NOT NULL
                     CONSTRAINT chatops_destinations_endpoint_shape CHECK (
                         char_length(endpoint) BETWEEN 9 AND 2048 AND endpoint ~ '^https://[^\s@/?#]+(/[^\s?#]*)?$'),
    events           text[] NOT NULL
                     CONSTRAINT chatops_destinations_events_known CHECK (
                         cardinality(events) BETWEEN 1 AND 16 AND events <@ ARRAY[
                             'work_item.transitioned', 'gate.requested', 'gate.decided', 'gate.invalidated',
                             'run.state_changed', 'deployment.state_changed']::text[]),
    -- A credential reference (never a value); empty means a credential-free receiver.
    credential       text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_destinations_credential_syntax CHECK (
                         credential = '' OR credential ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
    enabled          boolean NOT NULL,
    -- Advances when the endpoint or the credential reference changes: deliveries
    -- record the revision they were created under.
    binding_revision bigint NOT NULL
                     CONSTRAINT chatops_destinations_binding_range CHECK (binding_revision BETWEEN 1 AND 9007199254740991),
    version          bigint NOT NULL
                     CONSTRAINT chatops_destinations_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    CONSTRAINT chatops_destinations_name_unique UNIQUE (tenant_id, project_id, name),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE FUNCTION t041_destinations_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'destination identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'destination version must increase' USING ERRCODE = '23514';
    END IF;
    -- The binding revision moves exactly when the endpoint or credential does.
    IF (NEW.endpoint <> OLD.endpoint OR NEW.credential <> OLD.credential) THEN
        IF NEW.binding_revision <> OLD.binding_revision + 1 THEN
            RAISE EXCEPTION 'a changed endpoint or credential advances the binding revision' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.binding_revision <> OLD.binding_revision THEN
        RAISE EXCEPTION 'the binding revision changes only with the endpoint or credential' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER chatops_destinations_guard BEFORE UPDATE ON chatops_destinations
    FOR EACH ROW EXECUTE FUNCTION t041_destinations_guard();

-- ---- deliveries -------------------------------------------------------------

-- One row per (destination, event): the durable intent, created in the event
-- bus's inbox transaction before any external call.
CREATE TABLE chatops_deliveries (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    id               kodama_id NOT NULL,
    destination_id   kodama_id NOT NULL,
    event_id         text NOT NULL
                     CONSTRAINT chatops_deliveries_event_id_length CHECK (char_length(event_id) BETWEEN 1 AND 128),
    event_type       text NOT NULL
                     CONSTRAINT chatops_deliveries_event_type_known CHECK (event_type IN (
                         'work_item.transitioned', 'gate.requested', 'gate.decided', 'gate.invalidated',
                         'run.state_changed', 'deployment.state_changed')),
    binding_revision bigint NOT NULL
                     CONSTRAINT chatops_deliveries_binding_range CHECK (binding_revision BETWEEN 1 AND 9007199254740991),
    -- The exact bytes sent on every attempt.
    payload          bytea NOT NULL
                     CONSTRAINT chatops_deliveries_payload_bound CHECK (octet_length(payload) BETWEEN 2 AND 8192),
    payload_digest   text NOT NULL
                     CONSTRAINT chatops_deliveries_digest_syntax CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    state            text NOT NULL
                     CONSTRAINT chatops_deliveries_state_known CHECK (state IN ('pending', 'claimed', 'delivered', 'failed')),
    attempts         integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_attempts_range CHECK (attempts BETWEEN 0 AND 100000),
    cycle_attempts   integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_cycle_range CHECK (cycle_attempts BETWEEN 0 AND attempts),
    replays          integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_replays_range CHECK (replays BETWEEN 0 AND 100000),
    next_attempt_at  timestamptz NOT NULL,
    claim_token      bigint NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_token_range CHECK (claim_token BETWEEN 0 AND 9007199254740991),
    lease_expires_at timestamptz,
    last_class       text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_class_known CHECK (last_class IN (
                         '', 'unreachable', 'timeout', 'connection_lost', 'bad_response', 'receiver_error', 'rate_limited', 'rejected',
                         'redirect_refused', 'policy_refused', 'credential_unavailable', 'credential_source_unavailable',
                         'destination_changed', 'lease_expired', 'internal')),
    last_uncertain   boolean NOT NULL DEFAULT false,
    last_status      integer NOT NULL DEFAULT 0 CONSTRAINT chatops_deliveries_status_range CHECK (last_status BETWEEN 0 AND 599),
    receipt          text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_receipt_shape CHECK (receipt ~ '^[\x21-\x7e]{0,128}$'),
    delivered_at     timestamptz,
    failed_reason    text NOT NULL DEFAULT ''
                     CONSTRAINT chatops_deliveries_failed_reason_known CHECK (failed_reason IN ('', 'permanent', 'exhausted', 'refused')),
    version          bigint NOT NULL
                     CONSTRAINT chatops_deliveries_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    -- The delivery identity: one intent per destination and event, ever.
    CONSTRAINT chatops_deliveries_identity UNIQUE (tenant_id, project_id, destination_id, event_id),
    CONSTRAINT chatops_deliveries_delivered_consistent CHECK ((state = 'delivered') = (delivered_at IS NOT NULL)),
    CONSTRAINT chatops_deliveries_claim_consistent CHECK ((state = 'claimed') = (lease_expires_at IS NOT NULL)),
    CONSTRAINT chatops_deliveries_failed_consistent CHECK ((state = 'failed') = (failed_reason <> '')),
    FOREIGN KEY (tenant_id, project_id, destination_id) REFERENCES chatops_destinations (tenant_id, project_id, id)
);
CREATE INDEX chatops_deliveries_due_idx ON chatops_deliveries (tenant_id, next_attempt_at) WHERE state = 'pending';
CREATE INDEX chatops_deliveries_lease_idx ON chatops_deliveries (tenant_id, lease_expires_at) WHERE state = 'claimed';
CREATE INDEX chatops_deliveries_list_idx ON chatops_deliveries (tenant_id, project_id, created_at DESC, id DESC);
CREATE INDEX chatops_deliveries_destination_idx ON chatops_deliveries (tenant_id, project_id, destination_id, state);

CREATE FUNCTION t041_deliveries_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.destination_id <> OLD.destination_id OR NEW.event_id <> OLD.event_id OR NEW.event_type <> OLD.event_type
       OR NEW.payload <> OLD.payload OR NEW.payload_digest <> OLD.payload_digest OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'delivery identity and content are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'delivery version must increase' USING ERRCODE = '23514';
    END IF;
    IF NEW.attempts < OLD.attempts OR NEW.claim_token < OLD.claim_token OR NEW.replays < OLD.replays THEN
        RAISE EXCEPTION 'delivery counters and fencing tokens never decrease' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'delivered' THEN
        RAISE EXCEPTION 'a delivered notification is final' USING ERRCODE = '23514';
    END IF;
    IF NEW.binding_revision <> OLD.binding_revision AND NOT (OLD.state = 'failed' AND NEW.state = 'pending') THEN
        RAISE EXCEPTION 'a delivery is re-bound only by an authorized replay' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'pending' THEN
        IF NEW.state NOT IN ('claimed', 'failed') THEN
            RAISE EXCEPTION 'invalid delivery transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
        END IF;
        IF NEW.state = 'claimed' AND (NEW.claim_token <> OLD.claim_token + 1 OR NEW.attempts <> OLD.attempts + 1) THEN
            RAISE EXCEPTION 'a claim takes the next fencing token and records an attempt' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.state = 'claimed' THEN
        IF NEW.state = 'claimed' THEN
            IF NEW.claim_token <> OLD.claim_token + 1 OR NEW.attempts <> OLD.attempts + 1 THEN
                RAISE EXCEPTION 'a reclaim takes the next fencing token and records an attempt' USING ERRCODE = '23514';
            END IF;
        ELSIF NEW.claim_token <> OLD.claim_token THEN
            RAISE EXCEPTION 'a finalization keeps the claim token it was issued under' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.state = 'failed' THEN
        IF NEW.state <> 'pending' OR NEW.replays <> OLD.replays + 1 OR NEW.cycle_attempts <> 0 OR NEW.attempts <> OLD.attempts THEN
            RAISE EXCEPTION 'a failed delivery changes only through an authorized replay' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER chatops_deliveries_guard BEFORE UPDATE ON chatops_deliveries
    FOR EACH ROW EXECUTE FUNCTION t041_deliveries_guard();

-- ---- attempts (append-only history, written before the receiver is called) ---

CREATE TABLE chatops_delivery_attempts (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    delivery_id kodama_id NOT NULL,
    attempt     integer NOT NULL CONSTRAINT chatops_attempts_number_range CHECK (attempt BETWEEN 1 AND 100000),
    claim_token bigint NOT NULL,
    replay      integer NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    result      text NOT NULL DEFAULT ''
                CONSTRAINT chatops_attempts_result_known CHECK (result IN ('', 'delivered', 'retry', 'failed', 'abandoned')),
    class       text NOT NULL DEFAULT '',
    uncertain   boolean NOT NULL DEFAULT false,
    http_status integer NOT NULL DEFAULT 0 CONSTRAINT chatops_attempts_status_range CHECK (http_status BETWEEN 0 AND 599),
    receipt     text NOT NULL DEFAULT '' CONSTRAINT chatops_attempts_receipt_shape CHECK (receipt ~ '^[\x21-\x7e]{0,128}$'),
    PRIMARY KEY (tenant_id, project_id, delivery_id, attempt),
    CONSTRAINT chatops_attempts_finished_consistent CHECK ((finished_at IS NULL) = (result = '')),
    FOREIGN KEY (tenant_id, project_id, delivery_id) REFERENCES chatops_deliveries (tenant_id, project_id, id)
);

CREATE FUNCTION t041_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'delivery attempt history is append-only' USING ERRCODE = '23514';
    END IF;
    IF OLD.finished_at IS NOT NULL THEN
        RAISE EXCEPTION 'a finished attempt is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.delivery_id <> OLD.delivery_id
       OR NEW.attempt <> OLD.attempt OR NEW.claim_token <> OLD.claim_token OR NEW.replay <> OLD.replay OR NEW.started_at <> OLD.started_at THEN
        RAISE EXCEPTION 'attempt identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER chatops_attempts_guard BEFORE UPDATE OR DELETE ON chatops_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION t041_attempts_guard();

-- ---- replay audit metadata ---------------------------------------------------

CREATE TABLE chatops_replays (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    delivery_id  kodama_id NOT NULL,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    actor_id     text NOT NULL CONSTRAINT chatops_replays_actor_length CHECK (char_length(actor_id) BETWEEN 1 AND 256),
    request_id   text CONSTRAINT chatops_replays_request_length CHECK (char_length(request_id) <= 256),
    reason       text NOT NULL CONSTRAINT chatops_replays_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    from_version bigint NOT NULL,
    to_version   bigint NOT NULL,
    from_binding bigint NOT NULL,
    to_binding   bigint NOT NULL,
    occurred_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, delivery_id, seq),
    FOREIGN KEY (tenant_id, project_id, delivery_id) REFERENCES chatops_deliveries (tenant_id, project_id, id)
);

CREATE FUNCTION t041_replays_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'replay audit metadata is append-only' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER chatops_replays_guard BEFORE UPDATE OR DELETE ON chatops_replays
    FOR EACH ROW EXECUTE FUNCTION t041_replays_guard();

-- ---- restore fence ------------------------------------------------------------

-- A row per tenant that was ever fenced. While fenced, no delivery is claimed.
-- T-005's consumer fence stops event replay; this stops deliveries that were
-- already created.
CREATE TABLE chatops_delivery_fences (
    tenant_id   kodama_id PRIMARY KEY REFERENCES tenants (id),
    fenced      boolean NOT NULL,
    reason      text NOT NULL CONSTRAINT chatops_fences_reason_length CHECK (char_length(reason) BETWEEN 1 AND 512),
    operator_id text NOT NULL CONSTRAINT chatops_fences_operator_length CHECK (char_length(operator_id) BETWEEN 1 AND 256),
    updated_at  timestamptz NOT NULL
);

-- ---- row level security ------------------------------------------------------

ALTER TABLE chatops_destinations       ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatops_destinations       FORCE ROW LEVEL SECURITY;
ALTER TABLE chatops_deliveries         ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatops_deliveries         FORCE ROW LEVEL SECURITY;
ALTER TABLE chatops_delivery_attempts  ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatops_delivery_attempts  FORCE ROW LEVEL SECURITY;
ALTER TABLE chatops_replays            ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatops_replays            FORCE ROW LEVEL SECURITY;
ALTER TABLE chatops_delivery_fences    ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatops_delivery_fences    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON chatops_destinations      TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON chatops_deliveries        TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON chatops_delivery_attempts TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON chatops_replays           TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON chatops_delivery_fences   TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON chatops_destinations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON chatops_deliveries
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON chatops_delivery_attempts
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON chatops_replays
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON chatops_delivery_fences
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

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
