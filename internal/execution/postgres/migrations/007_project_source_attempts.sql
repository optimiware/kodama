-- A source-backed project attempt reads the pinned project commit without a
-- work-item workspace row. The commit is still required; write access continues
-- to require a real workspace and expected tip.
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_workspace_shape;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_workspace_shape CHECK (
    (access = 'none' AND workspace_id IS NULL AND input_commit IS NULL AND expected_tip IS NULL)
    OR (access = 'read' AND input_commit IS NOT NULL AND expected_tip IS NULL)
    OR (access = 'write' AND workspace_id IS NOT NULL AND input_commit IS NOT NULL AND expected_tip IS NOT NULL));
