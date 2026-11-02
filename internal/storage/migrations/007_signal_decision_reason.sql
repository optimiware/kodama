-- The decision reason on a signal record (T-192): what a refused signal that lost
-- to a cancellation needs to replay on its own. Appended after
-- 006_wait_decision_and_materializations.sql, which is left untouched. Expand-only:
-- one nullable column and one constraint are added; no table, column or row is
-- dropped, rewritten or backfilled, and no grant, policy or trigger changes.
--
-- The whole file is one transaction (the runner applies it atomically with its
-- ledger row): a failure leaves the database exactly as it was.

-- A cancelled decision (run_cancelled or run_failed) was kept only in
-- run_steps.wait_reason, a row that ordinary wait and run retention removes. A
-- replay of a signal that lost to the cancellation then had no reason to return.
-- The record now carries it, so it stays complete with no dependency on the wait
-- row. The column is nullable and the value set is closed to the two reasons the
-- kernel's cancelled decision allows (runtime.ReasonRunCancelled, ReasonRunFailed).
ALTER TABLE signal_records
    ADD COLUMN decision_reason text CHECK (decision_reason IN ('run_cancelled', 'run_failed'));

-- A new record carries a reason exactly when its decision is a cancellation: a
-- cancelled decision requires one, and a resolved or expired decision, a
-- decision-less refusal and a tombstone carry none. The constraint is NOT VALID so
-- an upgrade neither scans nor rewrites history: a cancelled record written before
-- this migration keeps a null reason (a replay of it reports that the reason was
-- not recorded), while every insert and update from now on is checked.
ALTER TABLE signal_records
    ADD CONSTRAINT signal_records_decision_reason_shape CHECK (
        (decision_kind IS NOT DISTINCT FROM 'cancelled') = (decision_reason IS NOT NULL)) NOT VALID;

-- Retention, removal and access are unchanged: the record never expires and is
-- removed only with its project (the existing cascade); the runtime role keeps
-- table-level SELECT and INSERT from 004 and no UPDATE or DELETE, so the new
-- column is append-only like the rest of the row; forced row security and the
-- tenant policy of 004 cover it as they cover every column.
