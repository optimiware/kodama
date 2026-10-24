-- T-015 artifact storage: durable metadata for retained outputs, the object
-- records that make cleanup recoverable, and retention pins.
--
-- run_id/attempt_id/step_id name the producing attempt. The execution tables do
-- not exist yet, so there is no foreign key to them: T-020's migration adds the
-- ownership constraint (tenant_id, project_id, attempt_id) -> attempts(...)
-- when it creates the run tables. Until then ownership is enforced by the
-- composite foreign key to projects and by the transaction's tenant context,
-- and the unique (tenant_id, project_id, attempt_id, name) key plus the
-- immutable ownership columns are exactly what that later constraint needs.
--
-- File bytes live in the object store; PostgreSQL holds only logical keys.

CREATE TABLE artifacts (
    tenant_id           kodama_id NOT NULL,
    project_id          kodama_id NOT NULL,
    id                  kodama_id NOT NULL,
    run_id              kodama_id NOT NULL,
    attempt_id          kodama_id NOT NULL,
    step_id             text NOT NULL
                        CONSTRAINT artifacts_step_length CHECK (char_length(step_id) BETWEEN 1 AND 128),
    name                text NOT NULL
                        CONSTRAINT artifacts_name_syntax CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    kind                text NOT NULL
                        CONSTRAINT artifacts_kind_known CHECK (kind IN ('file', 'oci')),
    role                text NOT NULL
                        CONSTRAINT artifacts_role_known CHECK (role IN ('output', 'diagnostic', 'bundle')),
    digest              text NOT NULL
                        CONSTRAINT artifacts_digest_syntax CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    media_type          text
                        CONSTRAINT artifacts_media_type_syntax CHECK (media_type ~ '^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$'),
    size_bytes          bigint
                        CONSTRAINT artifacts_size_range CHECK (size_bytes BETWEEN 0 AND 1099511627776),
    registry            text
                        CONSTRAINT artifacts_registry_length CHECK (char_length(registry) BETWEEN 3 AND 512),
    source_repository_url text
                        CONSTRAINT artifacts_source_url_syntax CHECK (source_repository_url ~ '^https://[^\s@/?#]+(/[^\s@?#]*)?$'),
    source_commit       text
                        CONSTRAINT artifacts_source_commit_syntax CHECK (source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    -- Content lifecycle: the runtime contract's states. reserved -> usable for
    -- files; an OCI registration exists only once verified (registered).
    state               text NOT NULL
                        CONSTRAINT artifacts_state_known CHECK (state IN ('reserved', 'registered', 'usable')),
    -- Cleanup bookkeeping, deliberately not part of the runtime state.
    disposition         text NOT NULL DEFAULT 'active'
                        CONSTRAINT artifacts_disposition_known CHECK (disposition IN ('active', 'abandoned', 'removed')),
    staging_key         text
                        CONSTRAINT artifacts_staging_key_shape CHECK (staging_key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    sealed_key          text
                        CONSTRAINT artifacts_sealed_key_shape CHECK (sealed_key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    -- Latest instant an issued upload instruction can still be honored.
    transfer_expires_at timestamptz,
    verified_at         timestamptz,
    verified_size_bytes bigint
                        CONSTRAINT artifacts_verified_size_range CHECK (verified_size_bytes BETWEEN 0 AND 1099511627776),
    -- Fenced completion lease. claim_generation only increases.
    claim_token         text,
    claim_generation    bigint NOT NULL DEFAULT 0
                        CONSTRAINT artifacts_claim_generation_range CHECK (claim_generation >= 0),
    claim_until         timestamptz,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    version             bigint NOT NULL
                        CONSTRAINT artifacts_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    -- The replay key; also the ownership key later execution constraints use.
    CONSTRAINT artifacts_attempt_name_unique UNIQUE (tenant_id, project_id, attempt_id, name),
    CONSTRAINT artifacts_source_both CHECK ((source_repository_url IS NULL) = (source_commit IS NULL)),
    CONSTRAINT artifacts_claim_both CHECK ((claim_token IS NULL) = (claim_until IS NULL)),
    CONSTRAINT artifacts_shape CHECK (
        (kind = 'file'
            AND state IN ('reserved', 'usable')
            AND media_type IS NOT NULL AND size_bytes IS NOT NULL AND registry IS NULL
            AND staging_key IS NOT NULL AND transfer_expires_at IS NOT NULL
            AND ((state = 'reserved' AND sealed_key IS NULL AND verified_at IS NULL AND verified_size_bytes IS NULL)
              OR (state = 'usable' AND sealed_key IS NOT NULL AND sealed_key <> staging_key
                  AND verified_at IS NOT NULL AND verified_size_bytes = size_bytes)))
        OR
        (kind = 'oci'
            AND state = 'registered' AND role = 'output'
            AND registry IS NOT NULL AND media_type IS NULL AND size_bytes IS NULL
            AND staging_key IS NULL AND sealed_key IS NULL AND transfer_expires_at IS NULL
            AND source_commit IS NOT NULL
            AND verified_at IS NOT NULL AND verified_size_bytes IS NOT NULL
            AND claim_token IS NULL)
    ),
    CONSTRAINT artifacts_disposition_rule CHECK (
        (disposition = 'active')
        OR (disposition = 'abandoned' AND state = 'reserved')
        OR (disposition = 'removed' AND state IN ('usable', 'registered'))
    )
);
CREATE INDEX artifacts_attempt_idx ON artifacts (tenant_id, project_id, attempt_id, role);
CREATE INDEX artifacts_reserved_idx ON artifacts (tenant_id, transfer_expires_at)
    WHERE state = 'reserved' AND disposition = 'active';
CREATE INDEX artifacts_retention_idx ON artifacts (tenant_id, id)
    WHERE state IN ('usable', 'registered') AND disposition = 'active';

-- Immutable identity, content declaration and provenance; one-way lifecycle.
CREATE FUNCTION t015_artifacts_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'artifact metadata and provenance are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.id <> OLD.id
       OR NEW.run_id <> OLD.run_id OR NEW.attempt_id <> OLD.attempt_id OR NEW.step_id <> OLD.step_id
       OR NEW.name <> OLD.name OR NEW.kind <> OLD.kind OR NEW.role <> OLD.role OR NEW.digest <> OLD.digest
       OR NEW.media_type IS DISTINCT FROM OLD.media_type OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
       OR NEW.registry IS DISTINCT FROM OLD.registry
       OR NEW.source_repository_url IS DISTINCT FROM OLD.source_repository_url
       OR NEW.source_commit IS DISTINCT FROM OLD.source_commit
       OR NEW.staging_key IS DISTINCT FROM OLD.staging_key
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'artifact identity, content declaration and provenance are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (OLD.state = 'reserved' AND NEW.state = 'usable') THEN
        RAISE EXCEPTION 'invalid artifact state transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    IF OLD.sealed_key IS NOT NULL AND NEW.sealed_key IS DISTINCT FROM OLD.sealed_key THEN
        RAISE EXCEPTION 'sealed content is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.verified_at IS NOT NULL AND (NEW.verified_at IS DISTINCT FROM OLD.verified_at
       OR NEW.verified_size_bytes IS DISTINCT FROM OLD.verified_size_bytes) THEN
        RAISE EXCEPTION 'verification evidence is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.disposition <> OLD.disposition AND OLD.disposition <> 'active' THEN
        RAISE EXCEPTION 'a closed artifact stays closed' USING ERRCODE = '23514';
    END IF;
    IF NEW.claim_generation < OLD.claim_generation OR NEW.claim_generation > OLD.claim_generation + 1 THEN
        RAISE EXCEPTION 'claim generations only increase by one' USING ERRCODE = '23514';
    END IF;
    IF NEW.transfer_expires_at IS DISTINCT FROM OLD.transfer_expires_at
       AND (NEW.transfer_expires_at IS NULL OR NEW.transfer_expires_at < OLD.transfer_expires_at) THEN
        RAISE EXCEPTION 'the latest issued transfer expiry never moves back' USING ERRCODE = '23514';
    END IF;
    -- The resource version moves exactly with state or disposition changes.
    IF NEW.state <> OLD.state OR NEW.disposition <> OLD.disposition THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'a lifecycle change increments the version by one' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'the version changes only with the lifecycle' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER artifacts_guard BEFORE UPDATE OR DELETE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION t015_artifacts_guard();

-- Every object this module may write is recorded here BEFORE the write, so a
-- crash can strand bytes but never an unrecorded object. Deletion intent and
-- acknowledgement are recorded around the provider call.
CREATE TABLE artifact_objects (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    artifact_id        kodama_id NOT NULL,
    key                text NOT NULL
                       CONSTRAINT artifact_objects_key_shape CHECK (key ~ '^artifacts/[A-Za-z0-9_/-]+$'),
    purpose            text NOT NULL
                       CONSTRAINT artifact_objects_purpose_known CHECK (purpose IN ('staging', 'candidate')),
    -- The completion claim generation that may write a candidate (0 for staging).
    generation         bigint NOT NULL DEFAULT 0
                       CONSTRAINT artifact_objects_generation_range CHECK (generation >= 0),
    state              text NOT NULL DEFAULT 'live'
                       CONSTRAINT artifact_objects_state_known CHECK (state IN ('live', 'delete_pending', 'deleted')),
    -- Winner: the sealed destination the usable artifact points at.
    winner             boolean NOT NULL DEFAULT false,
    -- Latest instant a holder of a credential or lease may still write the key.
    writable_until     timestamptz NOT NULL,
    delete_requested_at timestamptz,
    delete_token       text,
    delete_until       timestamptz,
    delete_attempts    integer NOT NULL DEFAULT 0
                       CONSTRAINT artifact_objects_attempts_range CHECK (delete_attempts >= 0),
    deleted_at         timestamptz,
    created_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, artifact_id, key),
    CONSTRAINT artifact_objects_key_unique UNIQUE (tenant_id, key),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id),
    CONSTRAINT artifact_objects_winner_is_candidate CHECK (NOT winner OR purpose = 'candidate'),
    CONSTRAINT artifact_objects_state_fields CHECK (
        (state = 'live' AND deleted_at IS NULL)
        OR (state = 'delete_pending' AND delete_requested_at IS NOT NULL AND deleted_at IS NULL)
        OR (state = 'deleted' AND deleted_at IS NOT NULL))
);
CREATE UNIQUE INDEX artifact_objects_one_winner ON artifact_objects (tenant_id, project_id, artifact_id) WHERE winner;
CREATE INDEX artifact_objects_sweep_idx ON artifact_objects (tenant_id, state, writable_until);

CREATE FUNCTION t015_objects_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'object records are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.artifact_id <> OLD.artifact_id
       OR NEW.key <> OLD.key OR NEW.purpose <> OLD.purpose OR NEW.generation <> OLD.generation OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'object identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.winner <> OLD.winner AND (OLD.winner OR OLD.state <> 'live') THEN
        RAISE EXCEPTION 'the winner is set once, on a live object' USING ERRCODE = '23514';
    END IF;
    IF NEW.writable_until < OLD.writable_until THEN
        RAISE EXCEPTION 'the latest possible write time never moves back' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state THEN
        IF NOT ((OLD.state = 'live' AND NEW.state = 'delete_pending')
             OR (OLD.state = 'delete_pending' AND NEW.state = 'deleted')
             -- A late staging upload is possible after cleanup: a renewed
             -- transfer reopens the staging record so a repeat sweep catches it.
             OR (OLD.purpose = 'staging' AND OLD.state IN ('delete_pending', 'deleted') AND NEW.state = 'live')) THEN
            RAISE EXCEPTION 'invalid object state transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
        END IF;
    END IF;
    -- The usable artifact's sealed bytes can only be scheduled for deletion by
    -- retention, which first marks the artifact removed in the same transaction.
    IF NEW.winner AND NEW.state = 'delete_pending' AND OLD.state = 'live' THEN
        IF NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id
                       AND a.id = NEW.artifact_id AND a.disposition = 'removed') THEN
            RAISE EXCEPTION 'the winner is deleted only after retention removed the artifact' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER artifact_objects_guard BEFORE UPDATE OR DELETE ON artifact_objects
    FOR EACH ROW EXECUTE FUNCTION t015_objects_guard();

-- Retention pins: a consumer (gate evidence, publication) protecting content.
CREATE TABLE artifact_pins (
    tenant_id   kodama_id NOT NULL,
    project_id  kodama_id NOT NULL,
    artifact_id kodama_id NOT NULL,
    owner_kind  text NOT NULL
                CONSTRAINT artifact_pins_owner_kind_length CHECK (char_length(owner_kind) BETWEEN 1 AND 32),
    owner_id    kodama_id NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, artifact_id, owner_kind, owner_id),
    FOREIGN KEY (tenant_id, project_id, artifact_id) REFERENCES artifacts (tenant_id, project_id, id)
);

ALTER TABLE artifacts        ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifacts        FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_objects FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_pins    ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_pins    FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON artifacts        TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON artifact_objects TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON artifact_pins    TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON artifacts
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON artifact_objects
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON artifact_pins
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Runtime grants: no DELETE or TRUNCATE anywhere except releasing a pin, and
-- UPDATE only on mutable lifecycle columns (identity and provenance are
-- immutable by grant and by trigger).
GRANT SELECT, INSERT ON artifacts TO {{runtime_role}};
GRANT UPDATE (state, disposition, sealed_key, transfer_expires_at, verified_at, verified_size_bytes,
              claim_token, claim_generation, claim_until, updated_at, version) ON artifacts TO {{runtime_role}};
GRANT SELECT, INSERT ON artifact_objects TO {{runtime_role}};
GRANT UPDATE (state, winner, writable_until, delete_requested_at, delete_token, delete_until,
              delete_attempts, deleted_at) ON artifact_objects TO {{runtime_role}};
GRANT SELECT, INSERT, DELETE ON artifact_pins TO {{runtime_role}};
