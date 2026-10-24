-- T-020 artifact producer ownership. The artifact schema (T015_001) names the
-- producing run, step and attempt but could not reference execution tables that
-- did not exist yet. With them in place an artifact can only be produced by an
-- attempt of its own project whose run and step match the artifact's own
-- columns, so a runtime caller cannot register an artifact against another
-- run's attempt even with a forged identifier.
--
-- Ordered after T015_001 and T020_002 in the manifest.

ALTER TABLE artifacts
    ADD CONSTRAINT artifacts_producer_attempt_fk
        FOREIGN KEY (tenant_id, project_id, attempt_id, run_id, step_id)
        REFERENCES step_attempts (tenant_id, project_id, id, run_id, step_id);
