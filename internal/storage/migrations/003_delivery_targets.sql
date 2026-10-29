-- Deployment target profiles (T-122): an environment may select an
-- installation-approved target profile instead of the legacy Kubernetes
-- namespace. The profile is configuration; provider-specific work stays in the
-- pinned driver or the project's ordinary deploy script.
--
-- Existing rows keep the legacy columns and the v1 snapshot digest. A profile
-- environment stores its target configuration, while namespace and kubeconfig
-- stay empty so the two target shapes cannot be mixed silently.

ALTER TABLE environments
    ADD COLUMN target_profile text NOT NULL DEFAULT '',
    ADD COLUMN target_profile_digest text NOT NULL DEFAULT '',
    ADD COLUMN target_provider text NOT NULL DEFAULT '',
    ADD COLUMN target_artifact_kinds jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN target_egress jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN target_config jsonb NOT NULL DEFAULT '{}';

ALTER TABLE environments DROP CONSTRAINT environments_kind_known;
ALTER TABLE environments ADD CONSTRAINT environments_kind_known CHECK (kind IN ('preview', 'stage', 'production'));
ALTER TABLE environments DROP CONSTRAINT environments_namespace_syntax;
ALTER TABLE environments ADD CONSTRAINT environments_namespace_syntax CHECK (namespace = '' OR namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$');
ALTER TABLE environments ADD CONSTRAINT environments_target_profile_syntax CHECK (target_profile = '' OR target_profile ~ '^[a-z][a-z0-9-]{0,62}$');
ALTER TABLE environments ADD CONSTRAINT environments_target_profile_digest_syntax CHECK (target_profile_digest = '' OR target_profile_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE environments ADD CONSTRAINT environments_target_provider_syntax CHECK (target_provider = '' OR target_provider ~ '^[a-z][a-z0-9-]{0,62}$');
ALTER TABLE environments ADD CONSTRAINT environments_target_artifact_kinds_array CHECK (jsonb_typeof(target_artifact_kinds) = 'array' AND jsonb_array_length(target_artifact_kinds) <= 4);
ALTER TABLE environments ADD CONSTRAINT environments_target_egress_array CHECK (jsonb_typeof(target_egress) = 'array' AND jsonb_array_length(target_egress) <= 8);
ALTER TABLE environments ADD CONSTRAINT environments_target_config_object CHECK (jsonb_typeof(target_config) = 'object' AND pg_column_size(target_config) <= 131072);
ALTER TABLE environments ADD CONSTRAINT environments_target_shape CHECK (
    (target_profile = '' AND target_profile_digest = '' AND target_provider = '')
    OR (target_profile <> '' AND target_profile_digest <> '' AND target_provider <> '' AND jsonb_array_length(target_artifact_kinds) > 0)
);

ALTER TABLE environment_snapshots
    ADD COLUMN target_profile text NOT NULL DEFAULT '',
    ADD COLUMN target_profile_digest text NOT NULL DEFAULT '',
    ADD COLUMN target_provider text NOT NULL DEFAULT '',
    ADD COLUMN target_artifact_kinds jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN target_egress jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN target_config jsonb NOT NULL DEFAULT '{}';

ALTER TABLE environment_snapshots DROP CONSTRAINT environment_snapshots_kind_known;
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_kind_known CHECK (kind IN ('preview', 'stage', 'production'));
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_namespace_syntax CHECK (namespace = '' OR namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$');
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_profile_syntax CHECK (target_profile = '' OR target_profile ~ '^[a-z][a-z0-9-]{0,62}$');
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_profile_digest_syntax CHECK (target_profile_digest = '' OR target_profile_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_provider_syntax CHECK (target_provider = '' OR target_provider ~ '^[a-z][a-z0-9-]{0,62}$');
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_artifact_kinds_array CHECK (jsonb_typeof(target_artifact_kinds) = 'array' AND jsonb_array_length(target_artifact_kinds) <= 4);
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_egress_array CHECK (jsonb_typeof(target_egress) = 'array' AND jsonb_array_length(target_egress) <= 8);
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_config_object CHECK (jsonb_typeof(target_config) = 'object' AND pg_column_size(target_config) <= 131072);
ALTER TABLE environment_snapshots ADD CONSTRAINT environment_snapshots_target_shape CHECK (
    (target_profile = '' AND target_profile_digest = '' AND target_provider = '')
    OR (target_profile <> '' AND target_profile_digest <> '' AND target_provider <> '' AND jsonb_array_length(target_artifact_kinds) > 0)
);

GRANT UPDATE (target_profile, target_profile_digest, target_provider, target_artifact_kinds, target_egress, target_config)
    ON environments TO {{runtime_role}};

-- A deployment may carry an OCI image or an immutable bundle. The legacy image
-- columns remain the OCI projection; for a bundle they hold the bundle artifact
-- id and digest so provenance and retention use one identity.
ALTER TABLE deployments
    ADD COLUMN artifact_kind text NOT NULL DEFAULT 'ociImage',
    ADD COLUMN artifact_media_type text NOT NULL DEFAULT '',
    ADD COLUMN resources jsonb NOT NULL DEFAULT '[]';

ALTER TABLE deployments ADD CONSTRAINT deployments_artifact_kind_known CHECK (artifact_kind IN ('ociImage', 'bundle'));
ALTER TABLE deployments ADD CONSTRAINT deployments_artifact_media_type_length CHECK (char_length(artifact_media_type) <= 128);
ALTER TABLE deployments ADD CONSTRAINT deployments_resources_array CHECK (jsonb_typeof(resources) = 'array' AND jsonb_array_length(resources) <= 64);

CREATE OR REPLACE FUNCTION t035_deployments_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.environment_id <> OLD.environment_id OR NEW.config_digest <> OLD.config_digest OR NEW.namespace <> OLD.namespace
       OR NEW.source_repository_url <> OLD.source_repository_url OR NEW.source_commit <> OLD.source_commit
       OR NEW.image_artifact_id <> OLD.image_artifact_id OR NEW.image_digest <> OLD.image_digest
       OR NEW.artifact_kind <> OLD.artifact_kind OR NEW.artifact_media_type <> OLD.artifact_media_type
       OR NEW.producer_attempt_id <> OLD.producer_attempt_id OR NEW.producer_run_id <> OLD.producer_run_id
       OR NEW.producer_step_id <> OLD.producer_step_id OR NEW.tool_image <> OLD.tool_image
       OR NEW.run_id <> OLD.run_id OR NEW.step_id <> OLD.step_id OR NEW.action_key <> OLD.action_key
       OR NEW.attempt_id <> OLD.attempt_id OR NEW.request_fingerprint <> OLD.request_fingerprint
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'deployment identity, provenance, configuration and expiry are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'deployment version must increase' USING ERRCODE = '23514';
    END IF;
    IF OLD.status IN ('succeeded', 'failed') THEN
        IF NEW.status <> OLD.status OR NEW.reason <> OLD.reason OR NEW.objects <> OLD.objects OR NEW.resources <> OLD.resources THEN
            RAISE EXCEPTION 'a settled deployment outcome is immutable' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status = 'pending' THEN
        IF NEW.status NOT IN ('pending', 'running', 'failed') THEN
            RAISE EXCEPTION 'invalid deployment transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status = 'running' THEN
        IF NEW.status NOT IN ('running', 'succeeded', 'failed') THEN
            RAISE EXCEPTION 'invalid deployment transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
        END IF;
        IF OLD.reason = 'outcome_unknown' AND NEW.status = 'running' AND NEW.reason <> 'outcome_unknown' THEN
            RAISE EXCEPTION 'an unknown outcome is settled, not reapplied' USING ERRCODE = '23514';
        END IF;
    END IF;
    IF OLD.status = 'running' AND NEW.status = 'running' AND NEW.objects <> OLD.objects THEN
        RAISE EXCEPTION 'intended objects do not change while running' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'running' AND NEW.status = 'running' AND NEW.resources <> OLD.resources THEN
        RAISE EXCEPTION 'intended resources do not change while running' USING ERRCODE = '23514';
    END IF;
    IF OLD.cleanup_requested_at IS NOT NULL AND NEW.cleanup_requested_at IS DISTINCT FROM OLD.cleanup_requested_at THEN
        RAISE EXCEPTION 'the cleanup request time is set once' USING ERRCODE = '23514';
    END IF;
    IF OLD.deleted_at IS NOT NULL AND (NEW.deleted_at IS DISTINCT FROM OLD.deleted_at OR NEW.cleanup_state <> OLD.cleanup_state) THEN
        RAISE EXCEPTION 'a deleted deployment stays deleted' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

GRANT UPDATE (resources) ON deployments TO {{runtime_role}};
