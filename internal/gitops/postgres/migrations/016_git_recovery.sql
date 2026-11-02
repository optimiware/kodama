-- T-138 Git recovery state (additive; contribution of internal/gitops, slot 016).
--
-- The durable state that makes backups, restores and branch pruning safe across
-- control-plane instances and restarts (docs/specs/git-repositories.md,
-- "Retention, maintenance and recovery"; GR-13 to GR-15):
--
--   git_backup_runs        one row per backup run. At most one run per tenant is
--                          active (a partial unique index), so backups and the
--                          expiry that follows one are serialized across
--                          instances. A run holds a lease that it renews; an
--                          expired active run is abandoned (failed) by the next
--                          Acquire. Only a run recorded complete is a backup;
--                          only a complete or failed run's objects are ever
--                          removed, never an upload merely because a newer
--                          manifest exists.
--   git_recoveries         the recovery selected for a tenant (snapshot and/or
--                          backup) and whether its volume restore completed.
--                          At most one is active per tenant. Selecting a source
--                          is durable, so a failed step, a restart or another
--                          instance never reopens Git access on a half-restored
--                          volume.
--   git_recovery_replays   the repositories whose initialization and bundle
--                          replay completed, with the replay's result
--                          (insert-only; the first record stands).
--   git_recovery_resolutions
--                          an operator's accepted discrepancy between the
--                          recovery point and the restored volume (insert-only,
--                          written with its audit row in one transaction).
--   git_prune_intents      the reservation branch pruning declares before it
--                          deletes and that hold acquisition checks, so a hold
--                          cannot be taken on a branch while it is being pruned.
--
-- Time. Leases and intents expire on the clock the store is given
-- (postgres.Options.Now); without one the database's now() decides, so control-
-- plane instances need no synchronized clocks.
--
-- Every table is owned by tenant_id with forced row level security. Children
-- reference their parent through the composite key. The runtime role never
-- deletes: a released prune intent is marked, and the rows that are evidence
-- are insert-only. Guards the column grants cannot express are triggers with a
-- pinned search_path.

CREATE TABLE git_backup_runs (
    tenant_id        kodama_id NOT NULL REFERENCES tenants (id),
    backup_id        kodama_id NOT NULL,
    state            text NOT NULL DEFAULT 'active'
                     CONSTRAINT git_backup_runs_state_known CHECK (state IN ('active', 'complete', 'failed')),
    lease_expires_at timestamptz NOT NULL,
    started_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, backup_id),
    CONSTRAINT git_backup_runs_update_order CHECK (updated_at >= started_at)
);

-- One active run per tenant: the cross-instance serialization.
CREATE UNIQUE INDEX git_backup_runs_one_active ON git_backup_runs (tenant_id) WHERE state = 'active';
CREATE INDEX git_backup_runs_list_idx ON git_backup_runs (tenant_id, started_at DESC, backup_id DESC);

CREATE TABLE git_recoveries (
    tenant_id       kodama_id NOT NULL REFERENCES tenants (id),
    id              kodama_id NOT NULL,
    seq             bigint GENERATED ALWAYS AS IDENTITY,
    snapshot_id     text CONSTRAINT git_recoveries_snapshot_length CHECK (char_length(snapshot_id) BETWEEN 1 AND 256),
    backup_id       text CONSTRAINT git_recoveries_backup_length CHECK (char_length(backup_id) BETWEEN 1 AND 256),
    status          text NOT NULL DEFAULT 'active'
                    CONSTRAINT git_recoveries_status_known CHECK (status IN ('active', 'complete')),
    volume_restored boolean NOT NULL,
    started_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    completed_at    timestamptz,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT git_recoveries_source CHECK (snapshot_id IS NOT NULL OR backup_id IS NOT NULL),
    CONSTRAINT git_recoveries_volume_matches_source CHECK (snapshot_id IS NOT NULL OR volume_restored),
    CONSTRAINT git_recoveries_completed_matches CHECK ((status = 'complete') = (completed_at IS NOT NULL)),
    CONSTRAINT git_recoveries_update_order CHECK (updated_at >= started_at)
);

CREATE UNIQUE INDEX git_recoveries_seq_idx ON git_recoveries (seq);
CREATE UNIQUE INDEX git_recoveries_one_active ON git_recoveries (tenant_id) WHERE status = 'active';
CREATE INDEX git_recoveries_latest_idx ON git_recoveries (tenant_id, seq DESC);

CREATE TABLE git_recovery_replays (
    tenant_id     kodama_id NOT NULL,
    recovery_id   kodama_id NOT NULL,
    repository_id text NOT NULL
                  CONSTRAINT git_recovery_replays_repository_length CHECK (char_length(repository_id) BETWEEN 1 AND 128),
    created       integer NOT NULL CONSTRAINT git_recovery_replays_created_range CHECK (created BETWEEN 0 AND 10000),
    unchanged     integer NOT NULL CONSTRAINT git_recovery_replays_unchanged_range CHECK (unchanged BETWEEN 0 AND 10000),
    conflicts     jsonb NOT NULL DEFAULT '[]'
                  CONSTRAINT git_recovery_replays_conflicts_array CHECK (jsonb_typeof(conflicts) = 'array' AND pg_column_size(conflicts) <= 4194304),
    completed_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, recovery_id, repository_id),
    FOREIGN KEY (tenant_id, recovery_id) REFERENCES git_recoveries (tenant_id, id)
);

CREATE TABLE git_recovery_resolutions (
    tenant_id     kodama_id NOT NULL,
    recovery_id   kodama_id NOT NULL,
    repository_id text NOT NULL
                  CONSTRAINT git_recovery_resolutions_repository_length CHECK (char_length(repository_id) BETWEEN 1 AND 128),
    ref           text NOT NULL
                  CONSTRAINT git_recovery_resolutions_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255),
    kind          text NOT NULL
                  CONSTRAINT git_recovery_resolutions_kind_known CHECK (kind IN ('missing', 'extra', 'differs')),
    expected      text NOT NULL
                  CONSTRAINT git_recovery_resolutions_expected_syntax CHECK (expected ~ '^([0-9a-f]{40}|[0-9a-f]{64})?$'),
    actual        text NOT NULL
                  CONSTRAINT git_recovery_resolutions_actual_syntax CHECK (actual ~ '^([0-9a-f]{40}|[0-9a-f]{64})?$'),
    resolved_by   text NOT NULL
                  CONSTRAINT git_recovery_resolutions_by_length CHECK (char_length(resolved_by) BETWEEN 1 AND 256),
    reason        text NOT NULL
                  CONSTRAINT git_recovery_resolutions_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1024),
    resolved_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, recovery_id, repository_id, ref, kind, actual),
    FOREIGN KEY (tenant_id, recovery_id) REFERENCES git_recoveries (tenant_id, id),
    -- A discrepancy's shape: a missing ref has no volume value, an extra one no
    -- recovery-point value, a differing one both and different.
    CONSTRAINT git_recovery_resolutions_shape CHECK (
        (kind = 'missing' AND expected <> '' AND actual = '')
        OR (kind = 'extra' AND expected = '' AND actual <> '')
        OR (kind = 'differs' AND expected <> '' AND actual <> '' AND expected <> actual))
);

CREATE TABLE git_prune_intents (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    repository_id kodama_id NOT NULL,
    ref           text NOT NULL
                  CONSTRAINT git_prune_intents_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255),
    tip           text NOT NULL
                  CONSTRAINT git_prune_intents_tip_syntax CHECK (tip ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    -- The pruner that declared it: another owner's Declare of a live reservation
    -- is refused and only the owner releases it, so two control-plane instances
    -- pruning one repository never release each other's reservation.
    owner         text NOT NULL
                  CONSTRAINT git_prune_intents_owner_length CHECK (char_length(owner) BETWEEN 1 AND 128),
    expires_at    timestamptz NOT NULL,
    released_at   timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, repository_id, ref),
    FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories (tenant_id, project_id, id),
    CONSTRAINT git_prune_intents_update_order CHECK (updated_at >= created_at)
);

CREATE INDEX git_prune_intents_tip_idx ON git_prune_intents (tenant_id, repository_id, tip)
    WHERE released_at IS NULL;

ALTER TABLE git_backup_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_backup_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_backup_runs TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_backup_runs
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_recoveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_recoveries FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_recoveries TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_recoveries
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_recovery_replays ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_recovery_replays FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_recovery_replays TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_recovery_replays
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_recovery_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_recovery_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_recovery_resolutions TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_recovery_resolutions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_prune_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_prune_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_prune_intents TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_prune_intents
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Guards. A backup run's identity never changes and it leaves active once: to
-- complete (published) or failed (abandoned); both are final. Only an active run
-- has a lease to move.
CREATE FUNCTION git_backup_runs_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.backup_id, NEW.started_at) IS DISTINCT FROM (OLD.tenant_id, OLD.backup_id, OLD.started_at) THEN
        RAISE EXCEPTION 'a backup run''s identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'active' THEN
        RAISE EXCEPTION 'a % backup run is final', OLD.state USING ERRCODE = '23514';
    END IF;
    IF NEW.state NOT IN ('active', 'complete', 'failed') THEN
        RAISE EXCEPTION 'unknown backup state %', NEW.state USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_backup_runs_guard BEFORE UPDATE ON git_backup_runs
    FOR EACH ROW EXECUTE FUNCTION git_backup_runs_guard();

-- A recovery keeps its source for good, completes once and never un-restores its
-- volume; a complete recovery is final.
CREATE FUNCTION git_recoveries_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.id, NEW.seq, NEW.snapshot_id, NEW.backup_id, NEW.started_at)
       IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.seq, OLD.snapshot_id, OLD.backup_id, OLD.started_at) THEN
        RAISE EXCEPTION 'a recovery''s identity and source are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'complete' THEN
        RAISE EXCEPTION 'a complete recovery is final' USING ERRCODE = '23514';
    END IF;
    IF OLD.volume_restored AND NOT NEW.volume_restored THEN
        RAISE EXCEPTION 'a restored volume is not un-restored' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'complete' AND NOT NEW.volume_restored THEN
        RAISE EXCEPTION 'a recovery completes only after its volume restore' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_recoveries_guard BEFORE UPDATE ON git_recoveries
    FOR EACH ROW EXECUTE FUNCTION git_recoveries_guard();

-- The recovery's step records are written only while it is active, and the
-- parent row is held against completion until the insert commits.
CREATE FUNCTION git_recovery_children_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM 1 FROM public.git_recoveries r
        WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.recovery_id AND r.status = 'active' FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the recovery is not active' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_recovery_replays_active BEFORE INSERT ON git_recovery_replays
    FOR EACH ROW EXECUTE FUNCTION git_recovery_children_guard();
CREATE TRIGGER git_recovery_resolutions_active BEFORE INSERT ON git_recovery_resolutions
    FOR EACH ROW EXECUTE FUNCTION git_recovery_children_guard();

CREATE FUNCTION git_recovery_rows_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'recovery step and resolution rows are never changed' USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER git_recovery_replays_immutable BEFORE UPDATE OR DELETE ON git_recovery_replays
    FOR EACH ROW EXECUTE FUNCTION git_recovery_rows_immutable();
CREATE TRIGGER git_recovery_resolutions_immutable BEFORE UPDATE OR DELETE ON git_recovery_resolutions
    FOR EACH ROW EXECUTE FUNCTION git_recovery_rows_immutable();

-- A prune intent's identity is fixed; only its tip, lifetime and release move.
CREATE FUNCTION git_prune_intents_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.repository_id, NEW.ref, NEW.created_at)
       IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.repository_id, OLD.ref, OLD.created_at) THEN
        RAISE EXCEPTION 'a prune intent''s identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_prune_intents_guard BEFORE UPDATE ON git_prune_intents
    FOR EACH ROW EXECUTE FUNCTION git_prune_intents_guard();

-- Least privilege: the runtime role reads and inserts, updates only what the
-- operations change and never deletes.
GRANT SELECT ON git_backup_runs TO {{runtime_role}};
GRANT INSERT (tenant_id, backup_id, lease_expires_at, started_at, updated_at) ON git_backup_runs TO {{runtime_role}};
GRANT UPDATE (state, lease_expires_at, updated_at) ON git_backup_runs TO {{runtime_role}};

GRANT SELECT ON git_recoveries TO {{runtime_role}};
GRANT INSERT (tenant_id, id, snapshot_id, backup_id, volume_restored, started_at, updated_at) ON git_recoveries TO {{runtime_role}};
GRANT UPDATE (status, volume_restored, updated_at, completed_at) ON git_recoveries TO {{runtime_role}};

GRANT SELECT ON git_recovery_replays TO {{runtime_role}};
GRANT INSERT (tenant_id, recovery_id, repository_id, created, unchanged, conflicts, completed_at) ON git_recovery_replays TO {{runtime_role}};

GRANT SELECT ON git_recovery_resolutions TO {{runtime_role}};
GRANT INSERT (tenant_id, recovery_id, repository_id, ref, kind, expected, actual, resolved_by, reason, resolved_at)
    ON git_recovery_resolutions TO {{runtime_role}};

GRANT SELECT ON git_prune_intents TO {{runtime_role}};
GRANT INSERT (tenant_id, project_id, repository_id, ref, tip, owner, expires_at, released_at, created_at, updated_at)
    ON git_prune_intents TO {{runtime_role}};
GRANT UPDATE (tip, owner, expires_at, released_at, updated_at) ON git_prune_intents TO {{runtime_role}};
