-- T-100 native Git trigger persistence: triggers, the durable delivery claim
-- and the admission ledger (success/refusal decided once per idempotency key).
--
-- Follows the base conventions (T002_001) and the module patterns of T-014 and
-- T-020: opaque kodama_id keys, ownership through composite foreign keys,
-- ENABLE + FORCE row level security on kodama_tenant_id(), minimal runtime
-- grants. State changes also append to the T-002 audit table; nothing here
-- emits an event.
--
-- Ordered after the base, pipeline definitions, work items and the execution
-- tables (runs): the trigger references its definition and work item, and the
-- delivery and ledger reference the run they started.

-- A trigger. The identifier is the hook's address (it arrives in a URL before
-- any tenant is known), so it is unique across tenants; every other key is
-- tenant and project scoped. kodama_git_trigger_locate below is the one narrow
-- path that turns an address into its owner.
CREATE TABLE git_triggers (
    tenant_id     kodama_id NOT NULL REFERENCES tenants (id),
    project_id    kodama_id NOT NULL,
    id            kodama_id NOT NULL,
    repository_id text NOT NULL
                  CONSTRAINT git_triggers_repository_length CHECK (char_length(repository_id) BETWEEN 1 AND 128),
    definition_id kodama_id NOT NULL,
    work_item_id  kodama_id,
    branches      text[] NOT NULL
                  CONSTRAINT git_triggers_branches_count CHECK (cardinality(branches) BETWEEN 1 AND 16),
    secret_ref    text NOT NULL
                  CONSTRAINT git_triggers_secret_ref_length CHECK (char_length(secret_ref) BETWEEN 1 AND 256),
    installed_by  text NOT NULL
                  CONSTRAINT git_triggers_installed_by_length CHECK (char_length(installed_by) BETWEEN 1 AND 256),
    enabled       boolean NOT NULL,
    version       bigint NOT NULL
                  CONSTRAINT git_triggers_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    CONSTRAINT git_triggers_address_key UNIQUE (id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, definition_id) REFERENCES pipeline_definitions (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, work_item_id) REFERENCES work_items (tenant_id, project_id, id)
);

-- One delivery of one trigger. The primary key is the durable claim: the first
-- insert wins and every redelivery sees its record. Admission progress (the
-- admitting mark, the lease) and the single terminal outcome live on the row;
-- the guard below makes the outcome write-once.
CREATE TABLE git_trigger_deliveries (
    tenant_id    kodama_id NOT NULL,
    project_id   kodama_id NOT NULL,
    trigger_id   kodama_id NOT NULL,
    delivery_id  text NOT NULL
                 CONSTRAINT git_trigger_deliveries_delivery_syntax CHECK (delivery_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    body_digest  text NOT NULL
                 CONSTRAINT git_trigger_deliveries_digest_length CHECK (char_length(body_digest) BETWEEN 1 AND 128),
    ref          text NOT NULL
                 CONSTRAINT git_trigger_deliveries_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255),
    commit_id    text NOT NULL
                 CONSTRAINT git_trigger_deliveries_commit_syntax CHECK (commit_id ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    received_at  timestamptz NOT NULL,
    admitting    boolean NOT NULL DEFAULT false,
    lease_holder text
                 CONSTRAINT git_trigger_deliveries_lease_holder_length CHECK (char_length(lease_holder) BETWEEN 1 AND 256),
    lease_until  timestamptz,
    done         boolean NOT NULL DEFAULT false,
    outcome      text
                 CONSTRAINT git_trigger_deliveries_outcome_known CHECK (outcome IN ('started', 'ignored', 'rejected')),
    reason       text
                 CONSTRAINT git_trigger_deliveries_reason_length CHECK (char_length(reason) BETWEEN 1 AND 128),
    run_id       kodama_id,
    completed_at timestamptz,
    PRIMARY KEY (tenant_id, project_id, trigger_id, delivery_id),
    -- The trigger address is global, so the claim is also unique by address.
    CONSTRAINT git_trigger_deliveries_claim_key UNIQUE (trigger_id, delivery_id),
    FOREIGN KEY (tenant_id, project_id, trigger_id) REFERENCES git_triggers (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    CONSTRAINT git_trigger_deliveries_lease_pair CHECK ((lease_holder IS NULL) = (lease_until IS NULL)),
    -- Only an admitting delivery can hold a lease, and a finished one holds none.
    CONSTRAINT git_trigger_deliveries_lease_admitting CHECK (lease_holder IS NULL OR (admitting AND NOT done)),
    -- A finished delivery has exactly one outcome; started is the only outcome
    -- with a run, and it can only follow the admitting mark.
    CONSTRAINT git_trigger_deliveries_done_shape CHECK (
        (NOT done AND outcome IS NULL AND reason IS NULL AND run_id IS NULL AND completed_at IS NULL)
        OR (done AND outcome IS NOT NULL AND completed_at IS NOT NULL
            AND ((outcome = 'started') = (run_id IS NOT NULL))
            AND (outcome = 'started' OR reason IS NOT NULL)
            AND (outcome <> 'started' OR admitting)))
);
CREATE INDEX git_trigger_deliveries_pending_idx ON git_trigger_deliveries (tenant_id, project_id, received_at) WHERE NOT done;

-- The admission ledger: one row per idempotency key of a scope, recording
-- either the run admission created or the refusal admission decided, never
-- both. Run creation inserts its row in the transaction that creates the run,
-- and a refusal is inserted under the same key lock, so after a refusal commits
-- no run can be created for the key and after a run commits no refusal can be
-- recorded. Rows are immutable: the first decision wins and survives restarts.
CREATE TABLE git_trigger_admissions (
    tenant_id       kodama_id NOT NULL,
    project_id      kodama_id NOT NULL,
    idempotency_key text NOT NULL
                    CONSTRAINT git_trigger_admissions_key_length CHECK (char_length(idempotency_key) BETWEEN 1 AND 512),
    run_id          kodama_id,
    refusal_reason  text
                    CONSTRAINT git_trigger_admissions_reason_length CHECK (char_length(refusal_reason) BETWEEN 1 AND 128),
    decided_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, project_id, idempotency_key),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    -- A run belongs to exactly one key.
    CONSTRAINT git_trigger_admissions_run_key UNIQUE (tenant_id, run_id),
    -- Success and refusal are mutually exclusive durable outcomes.
    CONSTRAINT git_trigger_admissions_exclusive CHECK ((run_id IS NULL) <> (refusal_reason IS NULL))
);

-- Guards, effective for every role including the owner.
CREATE FUNCTION git_triggers_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.repository_id <> OLD.repository_id OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'git trigger identity and repository binding are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'git trigger version must advance by one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_triggers_guard BEFORE UPDATE ON git_triggers
    FOR EACH ROW EXECUTE FUNCTION git_triggers_guard();

CREATE FUNCTION git_trigger_deliveries_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.trigger_id <> OLD.trigger_id
       OR NEW.delivery_id <> OLD.delivery_id OR NEW.body_digest <> OLD.body_digest OR NEW.ref <> OLD.ref
       OR NEW.commit_id <> OLD.commit_id OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'git trigger delivery identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.done THEN
        RAISE EXCEPTION 'a finished git trigger delivery is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.admitting AND NOT NEW.admitting THEN
        RAISE EXCEPTION 'git trigger delivery admission cannot be withdrawn' USING ERRCODE = '23514';
    END IF;
    -- A pre-admission result is fenced out once admission began and an
    -- admission result requires that it began.
    IF NEW.done AND NEW.outcome = 'started' AND NOT NEW.admitting THEN
        RAISE EXCEPTION 'a run cannot start before admission began' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_trigger_deliveries_guard BEFORE UPDATE ON git_trigger_deliveries
    FOR EACH ROW EXECUTE FUNCTION git_trigger_deliveries_guard();

ALTER TABLE git_triggers ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_triggers FORCE ROW LEVEL SECURITY;
ALTER TABLE git_trigger_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_trigger_deliveries FORCE ROW LEVEL SECURITY;
ALTER TABLE git_trigger_admissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_trigger_admissions FORCE ROW LEVEL SECURITY;

-- The owner policies serve administrative seeding and the SECURITY DEFINER
-- locator; the owner role never serves requests. No DELETE policy exists.
CREATE POLICY owner_select ON git_triggers FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON git_triggers FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON git_triggers FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON git_triggers FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON git_triggers FOR UPDATE
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY owner_select ON git_trigger_deliveries FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON git_trigger_deliveries FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON git_trigger_deliveries FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON git_trigger_deliveries FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_update ON git_trigger_deliveries FOR UPDATE
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

CREATE POLICY owner_select ON git_trigger_admissions FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_insert ON git_trigger_admissions FOR INSERT TO CURRENT_USER WITH CHECK (true);
CREATE POLICY tenant_select ON git_trigger_admissions FOR SELECT USING (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_insert ON git_trigger_admissions FOR INSERT WITH CHECK (tenant_id = kodama_tenant_id());

-- Least privilege. Only the mutable columns are updatable and nothing is ever
-- deleted: deliveries are the deduplication history and the ledger is final.
GRANT SELECT, INSERT ON git_triggers TO {{runtime_role}};
GRANT UPDATE (definition_id, work_item_id, branches, secret_ref, installed_by, enabled, version, updated_at)
    ON git_triggers TO {{runtime_role}};
GRANT SELECT, INSERT ON git_trigger_deliveries TO {{runtime_role}};
GRANT UPDATE (admitting, lease_holder, lease_until, done, outcome, reason, run_id, completed_at)
    ON git_trigger_deliveries TO {{runtime_role}};
GRANT SELECT, INSERT ON git_trigger_admissions TO {{runtime_role}};

-- The hook URL carries only the trigger address, so the owning tenant cannot be
-- known before the lookup. This function returns the ownership locator of the
-- single trigger with that address (no secret reference, filter or state); the
-- caller then reads the record inside a tenant transaction, where row level
-- security and explicit predicates apply again.
CREATE FUNCTION kodama_git_trigger_locate(p_id text)
    RETURNS TABLE (tenant_id text, project_id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
        SELECT t.tenant_id::text, t.project_id::text
        FROM public.git_triggers t
        WHERE t.id = p_id
    $$;

REVOKE ALL ON FUNCTION kodama_git_trigger_locate(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kodama_git_trigger_locate(text) TO {{runtime_role}};
