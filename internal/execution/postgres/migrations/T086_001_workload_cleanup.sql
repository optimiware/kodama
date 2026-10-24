-- T-086 workload cleanup bookkeeping. A runtime removes what remains of an
-- attempt's workload only after the attempt settled. The removal is recorded
-- apart from settlement, once, so a failed or still-retained removal is
-- retried by a later reconciliation pass, for attempts of terminal runs too.
--
-- Attempts settled before this migration have no recorded removal: they are
-- pending, and removing a workload that no longer exists succeeds.
--
-- Registered as its own contribution after every earlier execution
-- contribution.

ALTER TABLE attempt_controls
    ADD COLUMN workload_removed_at timestamptz,
    ADD CONSTRAINT attempt_controls_removal_after_settlement
        CHECK (workload_removed_at IS NULL OR settled_at IS NOT NULL);

CREATE INDEX attempt_controls_removal_pending_idx ON attempt_controls (tenant_id, settled_at, project_id, attempt_id)
    WHERE settled_at IS NOT NULL AND workload_removed_at IS NULL;

CREATE FUNCTION t086_attempt_controls_removal_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.workload_removed_at IS NOT NULL AND NEW.workload_removed_at IS DISTINCT FROM OLD.workload_removed_at THEN
        RAISE EXCEPTION 'a recorded workload removal is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER attempt_controls_removal_guard BEFORE UPDATE ON attempt_controls
    FOR EACH ROW EXECUTE FUNCTION t086_attempt_controls_removal_guard();

GRANT UPDATE (workload_removed_at) ON attempt_controls TO {{runtime_role}};
