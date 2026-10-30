-- Project-origin runs (source-free maintenance and CI) have no work item,
-- workspace, deployment or retained source. Their declared inputs, snapshot
-- and request authority are still pinned by the execution service.
ALTER TABLE runs DROP CONSTRAINT runs_purpose_known;
ALTER TABLE runs DROP CONSTRAINT runs_purpose_shape;
ALTER TABLE runs ADD CONSTRAINT runs_purpose_known
    CHECK (purpose IN ('work', 'cleanup', 'project'));
ALTER TABLE runs ADD CONSTRAINT runs_purpose_shape CHECK (
    (purpose = 'work' AND work_item_id IS NOT NULL AND workspace_id IS NOT NULL AND deployment_id IS NULL
        AND content_revision IS NOT NULL AND specification IS NOT NULL AND source_commit IS NOT NULL
        AND source_retention IN ('pending', 'confirmed') AND retained_ref_id IS NOT NULL)
    OR
    (purpose = 'cleanup' AND work_item_id IS NULL AND workspace_id IS NULL AND deployment_id IS NOT NULL
        AND content_revision IS NULL AND specification IS NULL AND source_commit IS NULL
        AND source_retention = 'not_required' AND retained_ref_id IS NULL)
    OR
    (purpose = 'project' AND work_item_id IS NULL AND workspace_id IS NULL AND deployment_id IS NULL
        AND content_revision IS NULL AND specification IS NULL AND source_commit IS NULL
        AND source_retention = 'not_required' AND retained_ref_id IS NULL));
