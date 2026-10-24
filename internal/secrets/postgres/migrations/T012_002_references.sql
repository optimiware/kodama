-- T-012 retained references: run pins of exact secret versions and the durable
-- consumer-handling records that revocation creates.
--
-- consumer_id names a pipeline run. The execution tables do not exist yet, so
-- there is no foreign key to them: T-020's migration adds the ownership
-- constraint (tenant_id, project_id, consumer_id) -> runs(tenant_id, project_id,
-- id) when it creates the run tables. Until then ownership is enforced by the
-- composite foreign key to the pinned secret version and by the transaction's
-- tenant context.

CREATE TABLE secret_pins (
    tenant_id        kodama_id NOT NULL,
    project_id       kodama_id NOT NULL,
    consumer_kind    text NOT NULL
                     CONSTRAINT secret_pins_consumer_kind_known CHECK (consumer_kind = 'run'),
    consumer_id      kodama_id NOT NULL,
    secret_id        kodama_id NOT NULL,
    secret_version   bigint NOT NULL,
    created_at       timestamptz NOT NULL,
    -- Set by the consumer's own transaction when it terminates (completion,
    -- cancellation or expiry). Ordered against resolution by the row lock.
    released_at      timestamptz,
    -- First and latest delivery of the value to this consumer, for revocation
    -- accounting. They record that a delivery happened, never its content.
    first_resolved_at timestamptz,
    last_resolved_at  timestamptz,
    resolution_count  integer NOT NULL DEFAULT 0
                      CONSTRAINT secret_pins_resolution_count_range CHECK (resolution_count >= 0),
    PRIMARY KEY (tenant_id, consumer_kind, consumer_id, secret_id, secret_version),
    FOREIGN KEY (tenant_id, project_id, secret_id, secret_version)
        REFERENCES secret_versions (tenant_id, project_id, secret_id, version)
);
CREATE INDEX secret_pins_secret_idx ON secret_pins (tenant_id, secret_id, secret_version);
CREATE INDEX secret_pins_consumer_idx ON secret_pins (tenant_id, consumer_kind, consumer_id);

CREATE FUNCTION t012_secret_pins_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'secret pins are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.consumer_kind <> OLD.consumer_kind
       OR NEW.consumer_id <> OLD.consumer_id OR NEW.secret_id <> OLD.secret_id
       OR NEW.secret_version <> OLD.secret_version OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a secret pin is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'a released pin stays released' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER secret_pins_guard BEFORE UPDATE OR DELETE ON secret_pins
    FOR EACH ROW EXECUTE FUNCTION t012_secret_pins_guard();

-- One owed external effect per (consumer, version, effect). pending until the
-- owning procedure acknowledges it; an acknowledgement is never rewritten.
CREATE TABLE secret_consumer_handling (
    tenant_id      kodama_id NOT NULL,
    id             kodama_id NOT NULL,
    project_id     kodama_id NOT NULL,
    secret_id      kodama_id NOT NULL,
    secret_version bigint NOT NULL,
    consumer_kind  text NOT NULL,
    consumer_id    kodama_id NOT NULL,
    effect         text NOT NULL
                   CONSTRAINT secret_consumer_handling_effect_known CHECK (effect IN ('terminate_consumer', 'rotate_issued_credential')),
    state          text NOT NULL
                   CONSTRAINT secret_consumer_handling_state_known CHECK (state IN ('pending', 'completed', 'not_applicable')),
    delivered      boolean NOT NULL,
    created_at     timestamptz NOT NULL,
    resolved_at    timestamptz,
    resolved_by    text CONSTRAINT secret_consumer_handling_resolved_by_length CHECK (char_length(resolved_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT secret_consumer_handling_unique UNIQUE (tenant_id, secret_id, secret_version, consumer_kind, consumer_id, effect),
    CONSTRAINT secret_consumer_handling_resolution CHECK (
        (state = 'pending' AND resolved_at IS NULL AND resolved_by IS NULL)
        OR (state <> 'pending' AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL)),
    FOREIGN KEY (tenant_id, consumer_kind, consumer_id, secret_id, secret_version)
        REFERENCES secret_pins (tenant_id, consumer_kind, consumer_id, secret_id, secret_version),
    FOREIGN KEY (tenant_id, project_id, secret_id, secret_version)
        REFERENCES secret_versions (tenant_id, project_id, secret_id, version)
);
CREATE INDEX secret_consumer_handling_pending_idx ON secret_consumer_handling (tenant_id, project_id, id) WHERE state = 'pending';

CREATE FUNCTION t012_handling_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'consumer handling records are retained' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'pending' THEN
        RAISE EXCEPTION 'an acknowledged effect is never rewritten' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id
       OR NEW.secret_id <> OLD.secret_id OR NEW.secret_version <> OLD.secret_version
       OR NEW.consumer_kind <> OLD.consumer_kind OR NEW.consumer_id <> OLD.consumer_id
       OR NEW.effect <> OLD.effect OR NEW.delivered <> OLD.delivered OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'consumer handling identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER secret_consumer_handling_guard BEFORE UPDATE OR DELETE ON secret_consumer_handling
    FOR EACH ROW EXECUTE FUNCTION t012_handling_guard();

ALTER TABLE secret_pins              ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_pins              FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_consumer_handling ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_consumer_handling FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON secret_pins              TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON secret_consumer_handling TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON secret_pins
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON secret_consumer_handling
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

GRANT SELECT, INSERT ON secret_pins TO {{runtime_role}};
GRANT UPDATE (released_at, first_resolved_at, last_resolved_at, resolution_count) ON secret_pins TO {{runtime_role}};
GRANT SELECT, INSERT ON secret_consumer_handling TO {{runtime_role}};
GRANT UPDATE (state, resolved_at, resolved_by) ON secret_consumer_handling TO {{runtime_role}};
