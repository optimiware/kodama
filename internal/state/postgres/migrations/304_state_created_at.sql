-- Additive creation metadata for state entries. Existing rows use their last
-- update as the best available creation time; new rows set both explicitly.
ALTER TABLE state_entries ADD COLUMN created_at timestamptz;
UPDATE state_entries SET created_at = updated_at WHERE created_at IS NULL;
ALTER TABLE state_entries ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE state_entries ALTER COLUMN created_at SET DEFAULT now();
