-- T-138 Git operations (additive; contribution of internal/gitpush, slot 015).
--
-- Two operator actions need the database to relax what migration 009 made
-- absolute, and only for those actions:
--
--   * An entry the reconciler left unknown is terminal ("unknown waits for an
--     operator"). An administrator who inspected the repository may settle it
--     as applied or refused. The move out of unknown is accepted only when a
--     git_push_resolutions row for that entry and state (who, why, which value
--     of the ref the control plane observed) was inserted in the same
--     transaction. The row is insert-only and one per entry, so a resolution is
--     an audited, single, attributed act and cannot be forged by a plain UPDATE.
--   * The known journal head of a repository never decreases (a lower head is an
--     older, restored volume: GR-A13). Once the restore reconciliation has
--     settled every in-flight entry, the restored journal becomes the new
--     baseline: the decrease is accepted only when a git_journal_resets row for
--     that repository and new head was inserted in the same transaction.
--
-- Trust boundary of the unknown-exit guard: the database proves that an operator
-- resolution row for that entry and state (an attributed, reasoned, insert-only
-- record) was written in the same transaction as the state change. It does not
-- itself check that the use case (gitpush.Service.ResolveUnknown) also wrote the
-- audit rows, compared the record's version or authorized the administrator:
-- those are the use case's, in the same transaction, and tested through it. The
-- resolution row is itself the durable audit of the decision; the audit table
-- rows are written beside it.
--
-- (The guard functions pin their search_path, so they name the schema.)
-- "Same transaction" is the row's txid, which a BEFORE INSERT trigger assigns
-- unconditionally (NEW.txid := txid_current(); any caller-supplied value is
-- overwritten) and which the column-level INSERT grant does not even let the
-- runtime role name, so a row cannot carry another transaction's identity. The
-- guards compare it with the current transaction's. Every other transition of
-- 009 is unchanged. The runtime role inserts and reads the new tables and never
-- updates or deletes them.

CREATE TABLE git_push_resolutions (
    tenant_id     kodama_id NOT NULL,
    push_id       kodama_id NOT NULL,
    ordinal       integer NOT NULL,
    project_id    kodama_id NOT NULL,
    repository_id kodama_id NOT NULL,
    ref           text NOT NULL
                  CONSTRAINT git_push_resolutions_ref_length CHECK (char_length(ref) BETWEEN 1 AND 255),
    resolution    text NOT NULL
                  CONSTRAINT git_push_resolutions_known CHECK (resolution IN ('applied', 'refused')),
    reason        text NOT NULL
                  CONSTRAINT git_push_resolutions_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1024),
    resolved_by   text NOT NULL
                  CONSTRAINT git_push_resolutions_by_length CHECK (char_length(resolved_by) BETWEEN 1 AND 256),
    observed      text
                  CONSTRAINT git_push_resolutions_observed_syntax CHECK (observed ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    resolved_at   timestamptz NOT NULL,
    txid          bigint NOT NULL DEFAULT txid_current(),
    PRIMARY KEY (tenant_id, push_id, ordinal),
    FOREIGN KEY (tenant_id, push_id, ordinal) REFERENCES git_push_entries (tenant_id, push_id, ordinal),
    FOREIGN KEY (tenant_id, push_id, project_id, repository_id) REFERENCES git_push_records (tenant_id, id, project_id, repository_id)
);

CREATE INDEX git_push_resolutions_repository_idx ON git_push_resolutions (tenant_id, repository_id, resolved_at DESC);

-- The per-ref read queries of the workspace (gitpush.Service.UnsettledOnRef and
-- AppliedOnRef) look up the entries of one ref of one repository.
CREATE INDEX git_push_entries_ref_idx ON git_push_entries (tenant_id, repository_id, ref, state);

-- The order in which entries applied, persisted: AppliedOnRef returns the records
-- with an applied entry for a ref newest first by this number, not by the record's
-- last change. It is assigned by the database when an entry becomes applied
-- (settlement, reconciliation or an operator resolution), from one monotone
-- sequence, so it follows the order of those state changes; the runtime role
-- cannot write the column.
CREATE SEQUENCE git_push_apply_seq;

ALTER TABLE git_push_entries ADD COLUMN applied_seq bigint;

-- A database that already recorded applied entries (009 was applied and used
-- before this migration): number them in the order their records last changed,
-- then by push ID and ordinal, before the constraint and the trigger exist, and
-- continue the sequence after them. The only column the statement changes is the
-- new one, which the entry guard of 009 does not look at.
UPDATE git_push_entries e SET applied_seq = n.seq
FROM (SELECT x.tenant_id, x.push_id, x.ordinal,
             row_number() OVER (ORDER BY r.updated_at, x.push_id, x.ordinal) AS seq
      FROM git_push_entries x
      JOIN git_push_records r ON r.tenant_id = x.tenant_id AND r.id = x.push_id
      WHERE x.state = 'applied') n
WHERE e.tenant_id = n.tenant_id AND e.push_id = n.push_id AND e.ordinal = n.ordinal;
SELECT setval('git_push_apply_seq', COALESCE((SELECT max(applied_seq) FROM git_push_entries), 1),
              EXISTS (SELECT 1 FROM git_push_entries WHERE applied_seq IS NOT NULL));

ALTER TABLE git_push_entries ADD CONSTRAINT git_push_entries_applied_seq_matches CHECK ((state = 'applied') = (applied_seq IS NOT NULL));

CREATE FUNCTION git_push_entries_assign_applied_seq() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.state = 'applied' AND OLD.state <> 'applied' THEN
        NEW.applied_seq := nextval('public.git_push_apply_seq');
    ELSE
        NEW.applied_seq := OLD.applied_seq;
    END IF;
    RETURN NEW;
END
$$;

-- Named to run before the entry guard (triggers fire alphabetically).
CREATE TRIGGER git_push_entries_a_applied_seq BEFORE UPDATE ON git_push_entries
    FOR EACH ROW EXECUTE FUNCTION git_push_entries_assign_applied_seq();

CREATE TABLE git_journal_resets (
    tenant_id     kodama_id NOT NULL,
    repository_id kodama_id NOT NULL,
    from_head     bigint NOT NULL
                  CONSTRAINT git_journal_resets_from_range CHECK (from_head BETWEEN 1 AND 9007199254740991),
    to_head       bigint NOT NULL
                  CONSTRAINT git_journal_resets_to_range CHECK (to_head BETWEEN 0 AND 9007199254740991),
    reset_by      text NOT NULL
                  CONSTRAINT git_journal_resets_by_length CHECK (char_length(reset_by) BETWEEN 1 AND 256),
    reason        text NOT NULL
                  CONSTRAINT git_journal_resets_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1024),
    reset_at      timestamptz NOT NULL,
    txid          bigint NOT NULL DEFAULT txid_current(),
    PRIMARY KEY (tenant_id, repository_id, txid, to_head),
    FOREIGN KEY (tenant_id, repository_id) REFERENCES repositories (tenant_id, id),
    CONSTRAINT git_journal_resets_lowers CHECK (to_head < from_head)
);

ALTER TABLE git_push_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_push_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_push_resolutions TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_push_resolutions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

ALTER TABLE git_journal_resets ENABLE ROW LEVEL SECURITY;
ALTER TABLE git_journal_resets FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON git_journal_resets TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON git_journal_resets
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

-- The transaction identity is the database's, never the caller's.
CREATE FUNCTION git_operator_rows_txid() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    NEW.txid := txid_current();
    RETURN NEW;
END
$$;

CREATE TRIGGER git_push_resolutions_txid BEFORE INSERT ON git_push_resolutions
    FOR EACH ROW EXECUTE FUNCTION git_operator_rows_txid();
CREATE TRIGGER git_journal_resets_txid BEFORE INSERT ON git_journal_resets
    FOR EACH ROW EXECUTE FUNCTION git_operator_rows_txid();

-- The rows are immutable.
CREATE FUNCTION git_operator_rows_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'operator resolution and reset rows are never changed' USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER git_push_resolutions_immutable BEFORE UPDATE OR DELETE ON git_push_resolutions
    FOR EACH ROW EXECUTE FUNCTION git_operator_rows_immutable();
CREATE TRIGGER git_journal_resets_immutable BEFORE UPDATE OR DELETE ON git_journal_resets
    FOR EACH ROW EXECUTE FUNCTION git_operator_rows_immutable();

-- The entry guard of 009 with the one relaxation.
CREATE OR REPLACE FUNCTION git_push_entries_guard() RETURNS trigger
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
    IF OLD.state = 'unknown' AND NEW.state IN ('applied', 'refused') THEN
        IF NOT EXISTS (SELECT 1 FROM public.git_push_resolutions r
                       WHERE r.tenant_id = OLD.tenant_id AND r.push_id = OLD.push_id AND r.ordinal = OLD.ordinal
                         AND r.resolution = NEW.state AND r.txid = txid_current()) THEN
            RAISE EXCEPTION 'an unknown push entry is resolved only by an operator resolution recorded in the same transaction' USING ERRCODE = '23514';
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

-- The head guard of 009 with the one relaxation.
CREATE OR REPLACE FUNCTION git_journal_heads_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.head < OLD.head AND NOT EXISTS (SELECT 1 FROM public.git_journal_resets r
                                           WHERE r.tenant_id = OLD.tenant_id AND r.repository_id = OLD.repository_id
                                             AND r.from_head = OLD.head AND r.to_head = NEW.head AND r.txid = txid_current()) THEN
        RAISE EXCEPTION 'the known journal head never decreases except by a recorded restore reset' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

GRANT SELECT ON git_push_resolutions TO {{runtime_role}};
GRANT INSERT (tenant_id, push_id, ordinal, project_id, repository_id, ref, resolution, reason, resolved_by, observed, resolved_at)
    ON git_push_resolutions TO {{runtime_role}};
GRANT SELECT ON git_journal_resets TO {{runtime_role}};
GRANT INSERT (tenant_id, repository_id, from_head, to_head, reset_by, reason, reset_at)
    ON git_journal_resets TO {{runtime_role}};
GRANT USAGE ON SEQUENCE git_push_apply_seq TO {{runtime_role}};
