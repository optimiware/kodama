-- T-020 secret pin ownership. T012_002 deferred the constraint
-- (tenant_id, project_id, consumer_id) -> runs to the run tables' migration: a
-- secret version is pinned by exactly one run of the same project. Run
-- admission inserts the run before registering its pins in the same
-- transaction, so the constraint is immediate.
--
-- Ordered after T012_002 and T020_002 in the manifest.

ALTER TABLE secret_pins
    ADD CONSTRAINT secret_pins_run_fk
        FOREIGN KEY (tenant_id, project_id, consumer_id) REFERENCES runs (tenant_id, project_id, id);
