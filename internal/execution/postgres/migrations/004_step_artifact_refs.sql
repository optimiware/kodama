-- Verified descendant artifact descriptors pinned by the producing step.
-- The map is keyed by the declared output alias and contains only server-
-- verified IDs and digests. It is an additive execution-owned column.
ALTER TABLE step_runs
    ADD COLUMN artifact_refs jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT step_runs_artifact_refs_shape CHECK (jsonb_typeof(artifact_refs) = 'object');
