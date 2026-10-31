-- Wait decisions before settlement, secret materialization records, a guard on
-- recorded step outputs and an index for unpublished outbox pages (T-189): what
-- the kernel's StateStore (T-182) needs that 004 and 005 cannot store. Appended after 005_durable_run_state.sql,
-- which is left untouched. Expand-only: no table, column or data is dropped, no
-- row is rewritten, and the legacy composition keeps reading and writing the
-- columns it uses today. Three check constraints of 004 are replaced because their
-- old form forbade the new shape (a decided wait whose step has not settled); no
-- other earlier object changes except by adding triggers, one table, one index and grants.
--
-- The whole file is one transaction (the runner applies it atomically with its
-- ledger row): a refusal or a failure leaves the database exactly as it was.

-- ---------------------------------------------------------------------------
-- Wait decisions before settlement.
-- ---------------------------------------------------------------------------
-- The kernel decides a wait independently of its step. A signal resolves or
-- expires an open wait and leaves its step `waiting` for the engine to settle; a
-- run's cancellation cancels a pending or open wait and leaves the step for the
-- engine. 004 forced the step status to agree with the decision, so such a
-- decision could not be stored. The replacements below keep every requirement on
-- the decision itself (its outcome, actor, signal, time and the absence of what a
-- decision must not carry) and widen only the step status:
--   resolved   the step is `waiting` (decided, not yet settled) or `succeeded`;
--   expired    `continue`: `waiting` or `succeeded`, with the reserved outcome;
--              `fail`: `waiting` or `failed`, with no outcome;
--   cancelled  `pending` (the wait never opened), `waiting`, or settled as
--              `cancelled`, `failed` or `skipped`.
-- A decided wait still requires `run_steps_wait_open` (identity and version, and
-- the opened shape or a cancellation before opening), so the unopened-versus-opened
-- distinction of `run_steps_wait_unopened` is untouched: an unopened wait has no
-- decision and no `waiting` status. `run_steps_wait_waiting` is unchanged: an open
-- wait is `waiting` and carries no decision.
ALTER TABLE run_steps
    DROP CONSTRAINT run_steps_wait_resolved,
    DROP CONSTRAINT run_steps_wait_expired,
    DROP CONSTRAINT run_steps_wait_cancelled;

ALTER TABLE run_steps
    ADD CONSTRAINT run_steps_wait_resolved CHECK (wait_state IS DISTINCT FROM 'resolved' OR (
        status IN ('waiting', 'succeeded')
        AND wait_outcome IS NOT NULL AND jsonb_exists(wait_outcomes, wait_outcome)
        AND wait_actor_ref IS NOT NULL AND wait_decided_at IS NOT NULL AND wait_consumed_signal_id IS NOT NULL)),
    ADD CONSTRAINT run_steps_wait_expired CHECK (wait_state IS DISTINCT FROM 'expired' OR (
        wait_decided_at IS NOT NULL
        AND num_nonnulls(wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0
        AND ((wait_on_expiry = 'continue' AND status IN ('waiting', 'succeeded') AND wait_outcome IS NOT DISTINCT FROM 'expired')
             OR (wait_on_expiry = 'fail' AND status IN ('waiting', 'failed') AND wait_outcome IS NULL)))),
    ADD CONSTRAINT run_steps_wait_cancelled CHECK (wait_state IS DISTINCT FROM 'cancelled' OR (
        status IN ('pending', 'waiting', 'cancelled', 'failed', 'skipped') AND wait_decided_at IS NOT NULL
        AND wait_reason IS NOT NULL AND wait_reason IN ('run_cancelled', 'run_failed')
        AND num_nonnulls(wait_outcome, wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0));

-- A step of a wait never moves backwards, and a settled one never changes. With
-- the checks above a decided wait may sit on a `waiting` step; without this guard
-- a settled wait step could be set back to `waiting` (or `pending`) and appear to
-- reopen its wait. The order is pending, then waiting, then one settled state.
-- 004's guard already keeps the wait itself immutable once decided and never lets
-- a decided wait return to `waiting`.
CREATE FUNCTION kodama_run_step_wait_order_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.step_kind = 'wait' AND NEW.status IS DISTINCT FROM OLD.status THEN
        IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled') THEN
            RAISE EXCEPTION 'a settled wait step cannot change its state' USING ERRCODE = '23514';
        END IF;
        IF OLD.status = 'waiting' AND NEW.status = 'pending' THEN
            RAISE EXCEPTION 'a wait step cannot move back to pending' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_steps_wait_order_guard BEFORE UPDATE ON run_steps
    FOR EACH ROW EXECUTE FUNCTION kodama_run_step_wait_order_guard();

-- ---------------------------------------------------------------------------
-- Step outputs are recorded with the settling commit and never rewritten.
-- ---------------------------------------------------------------------------
-- The kernel records the declared outputs of a container step in the very commit
-- that settles it as `succeeded`; a step that failed, was skipped or was cancelled
-- has none, and a recorded set never changes. The column existed without a guard,
-- so the rule lived in Go alone. A change to `outputs` is therefore accepted only
-- in the update that moves a step from an unsettled state to `succeeded`; every
-- other change is refused. An update that leaves the value as it is (a replay of
-- the settling commit) is not a change.
CREATE FUNCTION kodama_run_step_outputs_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.outputs IS DISTINCT FROM OLD.outputs THEN
        IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled') THEN
            RAISE EXCEPTION 'the outputs of a settled step are immutable' USING ERRCODE = '23514';
        END IF;
        IF NEW.status <> 'succeeded' THEN
            RAISE EXCEPTION 'outputs are recorded only with the settlement of a succeeded step' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_steps_outputs_guard BEFORE UPDATE ON run_steps
    FOR EACH ROW EXECUTE FUNCTION kodama_run_step_outputs_guard();

-- ---------------------------------------------------------------------------
-- run_materializations: the durable identity of one step attempt's secret material.
-- ---------------------------------------------------------------------------
-- A record is written before the resolver is asked to create material under its
-- identity (`issued`), and is marked `revoked` once the resolver confirmed it
-- removed the material and revoked access. It carries no directory and no value:
-- the identity alone lets a replacement engine, after a takeover or crash, revoke
-- material it never saw, and know when nothing is left to revoke. The key is the
-- scope (tenant and project), run, step and attempt; the identity is a pure
-- function of that key (runtime.NewMaterializationRef) and the table enforces it.
-- `revoked_at` is when the revocation was recorded and `revocation_evidence` an
-- optional, bounded reference to the resolver's confirmation.
CREATE TABLE run_materializations (
    tenant_id kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    run_id kodama_id NOT NULL,
    step_key text NOT NULL CHECK (step_key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    attempt integer NOT NULL CHECK (attempt >= 1),
    materialization_id text NOT NULL,
    state text NOT NULL DEFAULT 'issued' CHECK (state IN ('issued', 'revoked')),
    issued_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revocation_evidence text CHECK (length(revocation_evidence) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, project_id, run_id, step_key, attempt),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id, step_key) REFERENCES run_steps (tenant_id, project_id, run_id, step_key),
    -- The identity is exactly the one derived from the key, so a record cannot name
    -- material it was not issued for.
    CONSTRAINT run_materializations_identity CHECK (
        materialization_id = 'mat:' || tenant_id || ':' || project_id || ':' || run_id || ':' || step_key || ':' || attempt::text),
    -- Revocation time and evidence exist exactly when the record is revoked, and the
    -- revocation does not precede the issue.
    CONSTRAINT run_materializations_revocation_shape CHECK (
        (state = 'revoked') = (revoked_at IS NOT NULL)
        AND (state = 'revoked' OR revocation_evidence IS NULL)
        AND (revoked_at IS NULL OR revoked_at >= issued_at))
);
-- Material that still needs revoking: reconciliation scans one tenant at a time.
CREATE INDEX run_materializations_unrevoked ON run_materializations (tenant_id, project_id, run_id) WHERE state = 'issued';

-- Identity and issue time never change. `issued` moves to `revoked` exactly once,
-- recording the time (and optionally the evidence); `revoked` is final.
CREATE FUNCTION kodama_run_materialization_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.step_key, NEW.attempt, NEW.materialization_id, NEW.issued_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.step_key, OLD.attempt, OLD.materialization_id, OLD.issued_at) THEN
        RAISE EXCEPTION 'a materialization keeps its identity and issue time' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'revoked'
       AND (NEW.state, NEW.revoked_at, NEW.revocation_evidence)
           IS DISTINCT FROM (OLD.state, OLD.revoked_at, OLD.revocation_evidence) THEN
        RAISE EXCEPTION 'a revoked materialization is final' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER run_materializations_guard BEFORE UPDATE ON run_materializations
    FOR EACH ROW EXECUTE FUNCTION kodama_run_materialization_guard();

-- ---------------------------------------------------------------------------
-- Unpublished outbox pages.
-- ---------------------------------------------------------------------------
-- The bounded outbox query reads a tenant's unpublished events in creation order,
-- one page at a time. `audit_events_pending` is ordered by `next_attempt_at` for
-- the publisher's retry schedule, so a page by (created_at, id) had to sort the
-- tenant's whole unpublished set on every call. This partial index serves that
-- order directly and holds only events not yet published; it changes no row.
CREATE INDEX audit_events_unpublished ON audit_events (tenant_id, created_at, id) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- RLS and grants.
-- ---------------------------------------------------------------------------
ALTER TABLE run_materializations ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_materializations FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    EXECUTE format('CREATE POLICY owner_all ON run_materializations TO %I USING (true) WITH CHECK (true)', current_user);
END $$;
CREATE POLICY tenant_isolation ON run_materializations
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- The runtime role inserts a record and moves only its revocation columns;
-- identity and issue time are insert-only, and nothing is deletable.
GRANT SELECT, INSERT ON run_materializations TO {{runtime_role}};
GRANT UPDATE (state, revoked_at, revocation_evidence) ON run_materializations TO {{runtime_role}};
