-- T-130 Git push records (additive; contribution of internal/gitpush).
--
-- A push record is the durable intent and outcome of one receive operation
-- (git-repositories.md, "Push journal and events"): the actor and grant, the
-- repository and one entry per (ref, old, new) with its own state and a
-- persisted kind. It is created by authorization before any ref changes (GR-09)
-- and settled entry by entry; its status is derived from its entries and kept
-- in a column only so the seed and writer queries can be indexed. PostgreSQL
-- owns these rows; the tenant's Git worker owns refs, objects and the ref
-- journal. Git is never called from a transaction that touches these tables.
--
-- Ownership is a composite foreign key to repositories (migration 007), so this
-- contribution is registered after it. A receive is unique per repository
-- (tenant, repository, receive), which makes authorization idempotent and makes
-- two receives with identical (ref, old, new) tuples two records; attribution
-- is by receive, never by ref values. Row level security is enabled and forced
-- with a tenant policy that fails closed. The runtime role can never delete.
--
-- Invariants the database also enforces, so a bug or a second writer cannot
-- break them:
--   * an entry moves authorized -> committing | refused | unknown and
--     committing -> applied | refused | unknown only; applied, refused and
--     unknown never change here (an unknown outcome waits for an operator);
--   * ref, old, new, kind and ordinal never change; the kind agrees with the
--     old and new values (create: old all-zero; delete: new all-zero; fast_forward
--     and rewrite: both non-zero);
--   * a refused or unknown entry states its reason, others have none;
--   * the record's version advances by exactly one with a visible change and the
--     identity, actor, grant, request digest and deadline are immutable;
--   * the known journal head of a repository never decreases.

CREATE TABLE git_push_records (
    tenant_id            kodama_id NOT NULL,
    project_id           kodama_id NOT NULL,
    id                   kodama_id NOT NULL,
    repository_id        kodama_id NOT NULL,
    receive_id           kodama_id NOT NULL,
    grant_id             kodama_id NOT NULL,
    grant_kind           text NOT NULL
                         CONSTRAINT git_push_records_grant_kind_known CHECK (grant_kind IN ('human', 'seed', 'preparation', 'attempt', 'integration')),
    actor_type           text NOT NULL
                         CONSTRAINT git_push_records_actor_type_known CHECK (actor_type IN ('user', 'workload')),
    actor_id             kodama_id NOT NULL,
    run_id               kodama_id,
    attempt_id           kodama_id,
    push_options         jsonb NOT NULL DEFAULT '[]'
                         CONSTRAINT git_push_records_options_array CHECK (jsonb_typeof(push_options) = 'array' AND pg_column_size(push_options) <= 32768),
    repository_empty     boolean NOT NULL,
    empty_report_pending boolean NOT NULL DEFAULT false,
    request_digest       text NOT NULL
                         CONSTRAINT git_push_records_digest_syntax CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    deadline             timestamptz NOT NULL,
    status               text NOT NULL
                         CONSTRAINT git_push_records_status_known CHECK (status IN ('authorized', 'applied', 'refused', 'unknown')),
    version              bigint NOT NULL DEFAULT 1
                         CONSTRAINT git_push_records_version_range CHECK (version BETWEEN 1 AND 9007199254740991),
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT git_push_records_receive_unique UNIQUE (tenant_id, repository_id, receive_id),
    CONSTRAINT git_push_records_owner_unique UNIQUE (tenant_id, id, project_id, repository_id),
    FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories (tenant_id, project_id, id),
    CONSTRAINT git_push_records_actor_shape CHECK (
        (actor_type = 'user' AND run_id IS NULL AND attempt_id IS NULL) OR (actor_type = 'workload' AND run_id IS NOT NULL)),
    CONSTRAINT git_push_records_update_order CHECK (updated_at >= created_at)
);

CREATE INDEX git_push_records_list_idx ON git_push_records (tenant_id, repository_id, created_at DESC, id DESC);

CREATE INDEX git_push_records_unsettled_idx ON git_push_records (tenant_id, repository_id)
    WHERE status IN ('authorized', 'unknown');

CREATE INDEX git_push_records_empty_report_idx ON git_push_records (tenant_id, repository_id)
    WHERE empty_report_pending;

CREATE TABLE git_push_entries (
    tenant_id     kodama_id NOT NULL,
    push_id       kodama_id NOT NULL,
    ordinal       integer NOT NULL
                  CONSTRAINT git_push_entries_ordinal_range CHECK (ordinal BETWEEN 0 AND 1023),
    project_id    kodama_id NOT NULL,
    repository_id kodama_id NOT NULL,
    ref           text NOT NULL
                  CONSTRAINT git_push_entries_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255)
                  CONSTRAINT git_push_entries_ref_syntax CHECK (ref ~ '^refs/[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$'),
    old_value     text NOT NULL
                  CONSTRAINT git_push_entries_old_syntax CHECK (old_value ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    new_value     text NOT NULL
                  CONSTRAINT git_push_entries_new_syntax CHECK (new_value ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    kind          text NOT NULL
                  CONSTRAINT git_push_entries_kind_known CHECK (kind IN ('create', 'fast_forward', 'rewrite', 'delete')),
    state         text NOT NULL
                  CONSTRAINT git_push_entries_state_known CHECK (state IN ('authorized', 'committing', 'applied', 'refused', 'unknown')),
    reason        text
                  CONSTRAINT git_push_entries_reason_syntax CHECK (reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    PRIMARY KEY (tenant_id, push_id, ordinal),
    CONSTRAINT git_push_entries_ref_unique UNIQUE (tenant_id, push_id, ref),
    FOREIGN KEY (tenant_id, push_id, project_id, repository_id) REFERENCES git_push_records (tenant_id, id, project_id, repository_id),
    CONSTRAINT git_push_entries_values_differ CHECK (old_value <> new_value AND char_length(old_value) = char_length(new_value)),
    CONSTRAINT git_push_entries_kind_matches CHECK (
        (kind = 'create' AND old_value ~ '^0+$')
        OR (kind = 'delete' AND new_value ~ '^0+$')
        OR (kind IN ('fast_forward', 'rewrite') AND old_value !~ '^0+$' AND new_value !~ '^0+$')),
    CONSTRAINT git_push_entries_reason_matches_state CHECK ((state IN ('refused', 'unknown')) = (reason IS NOT NULL))
);

CREATE INDEX git_push_entries_unknown_idx ON git_push_entries (tenant_id, repository_id, ref)
    WHERE state = 'unknown';

-- The highest ref-journal head the control plane has seen per repository: a
-- journal whose head is lower came from an older volume (GR-A13).
CREATE TABLE git_journal_heads (
    tenant_id     kodama_id NOT NULL,
    repository_id kodama_id NOT NULL,
    head          bigint NOT NULL
                  CONSTRAINT git_journal_heads_range CHECK (head BETWEEN 0 AND 9007199254740991),
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, repository_id),
    FOREIGN KEY (tenant_id, repository_id) REFERENCES repositories (tenant_id, id)
);

ALTER TABLE git_push_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_push_records FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_push_records TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_push_records
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_push_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_push_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_push_entries TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_push_entries
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_journal_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_journal_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_journal_heads TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_journal_heads
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- Transitions the column grants cannot express.
CREATE FUNCTION git_push_records_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.project_id, NEW.id, NEW.repository_id, NEW.receive_id, NEW.grant_id, NEW.grant_kind,
        NEW.actor_type, NEW.actor_id, NEW.run_id, NEW.attempt_id, NEW.push_options, NEW.repository_empty,
        NEW.request_digest, NEW.deadline, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.id, OLD.repository_id, OLD.receive_id, OLD.grant_id, OLD.grant_kind,
        OLD.actor_type, OLD.actor_id, OLD.run_id, OLD.attempt_id, OLD.push_options, OLD.repository_empty,
        OLD.request_digest, OLD.deadline, OLD.created_at) THEN
        RAISE EXCEPTION 'push record identity, actor, grant, request and deadline are immutable' USING ERRCODE = '23514';
    END IF;

    IF NEW.version = OLD.version THEN
        -- Telling the repository module changes nothing else.
        IF (NEW.status, NEW.updated_at) IS DISTINCT FROM (OLD.status, OLD.updated_at) THEN
            RAISE EXCEPTION 'a push record change advances its version' USING ERRCODE = '23514';
        END IF;
        IF NEW.empty_report_pending AND NOT OLD.empty_report_pending THEN
            RAISE EXCEPTION 'an emptiness report obligation is set with a settlement' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'push record version advances by one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_push_records_guard BEFORE UPDATE ON git_push_records
    FOR EACH ROW EXECUTE FUNCTION git_push_records_guard();

CREATE FUNCTION git_push_entries_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.push_id, NEW.ordinal, NEW.project_id, NEW.repository_id, NEW.ref, NEW.old_value, NEW.new_value, NEW.kind)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.push_id, OLD.ordinal, OLD.project_id, OLD.repository_id, OLD.ref, OLD.old_value, OLD.new_value, OLD.kind) THEN
        RAISE EXCEPTION 'a push entry names its ref, values and kind for good' USING ERRCODE = '23514';
    END IF;
    IF NEW.state = OLD.state THEN
        IF NEW.reason IS DISTINCT FROM OLD.reason THEN
            RAISE EXCEPTION 'a push entry keeps its reason until it changes state' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT ((OLD.state = 'authorized' AND NEW.state IN ('committing', 'refused', 'unknown'))
         OR (OLD.state = 'committing' AND NEW.state IN ('applied', 'refused', 'unknown'))) THEN
        RAISE EXCEPTION 'push entry state % cannot become %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_push_entries_guard BEFORE UPDATE ON git_push_entries
    FOR EACH ROW EXECUTE FUNCTION git_push_entries_guard();

CREATE FUNCTION git_journal_heads_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.head < OLD.head THEN
        RAISE EXCEPTION 'the known journal head never decreases' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER git_journal_heads_guard BEFORE UPDATE ON git_journal_heads
    FOR EACH ROW EXECUTE FUNCTION git_journal_heads_guard();

-- Least privilege: the runtime role reads and inserts records and entries and
-- updates only what settlement changes. It can never delete.
GRANT SELECT, INSERT ON git_push_records TO {{runtime_role}};
GRANT UPDATE (status, empty_report_pending, version, updated_at) ON git_push_records TO {{runtime_role}};

GRANT SELECT, INSERT ON git_push_entries TO {{runtime_role}};
GRANT UPDATE (state, reason) ON git_push_entries TO {{runtime_role}};

GRANT SELECT, INSERT ON git_journal_heads TO {{runtime_role}};
GRANT UPDATE (head, updated_at) ON git_journal_heads TO {{runtime_role}};
