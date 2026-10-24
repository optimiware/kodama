-- T-042 flow summaries: history-derived, non-materialized projections.
--
-- Everything is derived on read from the immutable T-011 work_item_history rows;
-- no table, trigger, worker or cached state is created, so there is nothing to
-- reconcile after a failure. The views are SECURITY INVOKER: they run with the
-- privileges and row level security of the caller (the runtime role under
-- kodama_tenant_id()), never of their owner. Every window partitions by
-- (tenant_id, project_id, work_item_id), so a tenant, project or item predicate
-- on the view is pushed below the window functions and only the selected items'
-- history is processed.
--
-- Semantics are documented in internal/metrics/README.md. In short:
--   * history resource `version` is the only ordering;
--   * the state after a row is its to_state, the blocked flag after a row is the
--     latest explicit after_values.blockedReason (string = set, JSON null =
--     cleared, key absent = unchanged);
--   * a segment is the time between a row and the next row of the same item; the
--     last segment is open (ended_at is NULL);
--   * done and cancelled segments do not accrue lead, active, review or blocked time;
--   * an episode starts at creation or at done -> active (a reopen) and ends at
--     the next done or cancelled transition;
--   * a row whose history cannot be a T-011 chain carries a non-NULL anomaly and
--     the consumer reports the item as invalid instead of inventing durations.

-- One row per history row with the carried state and blocked flag and a
-- chain-integrity anomaly.
CREATE VIEW work_flow_steps WITH (security_invoker = true) AS
SELECT s.tenant_id, s.project_id, s.work_item_id, s.version, s.history_id, s.change_kind,
       s.occurred_at, s.from_state, s.to_state AS state,
       coalesce(first_value(CASE s.block_type WHEN 'string' THEN true WHEN 'null' THEN false END)
                    OVER (PARTITION BY s.tenant_id, s.project_id, s.work_item_id, s.block_group ORDER BY s.version),
                false) AS is_blocked,
       CASE
           WHEN s.to_state IS NULL THEN 'state_unknown'
           WHEN s.block_type IS NOT NULL AND s.block_type NOT IN ('string', 'null') THEN 'blocked_value'
           WHEN s.prev_version IS NULL AND (s.version <> 1 OR s.change_kind <> 'created') THEN 'first_not_created'
           WHEN s.prev_version IS NULL THEN NULL
           WHEN s.change_kind = 'created' THEN 'duplicate_created'
           WHEN s.version <> s.prev_version + 1 THEN 'version_gap'
           WHEN s.occurred_at < s.prev_at THEN 'time_backwards'
           WHEN s.from_state IS DISTINCT FROM s.prev_state THEN 'state_chain'
           WHEN s.change_kind = 'updated' AND s.to_state <> s.from_state THEN 'state_chain'
           WHEN s.change_kind = 'transitioned' AND s.to_state = s.from_state THEN 'state_chain'
           WHEN s.prev_state = 'cancelled' THEN 'state_chain'
           WHEN s.prev_state = 'done' AND NOT (s.change_kind = 'transitioned' AND s.to_state = 'active') THEN 'state_chain'
       END AS anomaly
FROM (
    SELECT h.tenant_id, h.project_id, h.work_item_id, h.version, h.id AS history_id, h.change_kind,
           h.occurred_at, h.from_state, h.to_state,
           CASE WHEN h.after_values ? 'blockedReason' THEN jsonb_typeof(h.after_values -> 'blockedReason') END AS block_type,
           count(*) FILTER (WHERE h.after_values ? 'blockedReason')
               OVER (PARTITION BY h.tenant_id, h.project_id, h.work_item_id ORDER BY h.version) AS block_group,
           lag(h.version)     OVER w AS prev_version,
           lag(h.occurred_at) OVER w AS prev_at,
           lag(h.to_state)    OVER w AS prev_state
    FROM work_item_history h
    WINDOW w AS (PARTITION BY h.tenant_id, h.project_id, h.work_item_id ORDER BY h.version)
) s;

-- One row per step with the interval to the next step and its episode.
CREATE VIEW work_flow_segments WITH (security_invoker = true) AS
SELECT g.tenant_id, g.project_id, g.work_item_id, g.version, g.history_id, g.change_kind,
       g.occurred_at, g.from_state, g.state, g.is_blocked, g.anomaly,
       g.ended_at, g.ended_version, g.accrues, g.episode_no,
       (first_value(g.change_kind) OVER (PARTITION BY g.tenant_id, g.project_id, g.work_item_id, g.episode_no ORDER BY g.version)
            = 'created') AS episode_initial,
       (g.change_kind = 'transitioned' AND g.from_state = 'done' AND g.state = 'active') AS is_reopen
FROM (
    SELECT p.*,
           lead(p.occurred_at) OVER w AS ended_at,
           lead(p.version)     OVER w AS ended_version,
           (p.state NOT IN ('done', 'cancelled')) AS accrues,
           count(*) FILTER (WHERE p.change_kind = 'created'
                              OR (p.change_kind = 'transitioned' AND p.from_state = 'done' AND p.state = 'active'))
               OVER (PARTITION BY p.tenant_id, p.project_id, p.work_item_id ORDER BY p.version) AS episode_no
    FROM work_flow_steps p
    WINDOW w AS (PARTITION BY p.tenant_id, p.project_id, p.work_item_id ORDER BY p.version)
) g;

-- Unfinished episodes: created -> done/cancelled, and each reopen -> done/cancelled.
-- ended_at/outcome are NULL while the episode is unfinished at the end of history.
CREATE VIEW work_flow_episodes WITH (security_invoker = true) AS
SELECT s.tenant_id, s.project_id, s.work_item_id, s.episode_no,
       CASE WHEN bool_or(s.episode_initial) THEN 'initial' ELSE 'reopened' END AS kind,
       min(s.occurred_at) AS started_at,
       min(s.version) AS started_version,
       min(s.occurred_at) FILTER (WHERE NOT s.accrues) AS ended_at,
       min(s.version) FILTER (WHERE NOT s.accrues) AS ended_version,
       (array_agg(s.state ORDER BY s.version) FILTER (WHERE NOT s.accrues))[1] AS outcome
FROM work_flow_segments s
GROUP BY s.tenant_id, s.project_id, s.work_item_id, s.episode_no;

-- Maximal runs of consecutive unfinished segments in which a blocked reason was
-- recorded. Replacing the reason or editing other fields does not split a run;
-- a terminal state does, and a reopen starts a new run when the reason is still
-- recorded. ended_at is NULL for a run still open at the end of history.
CREATE VIEW work_flow_blocked_runs WITH (security_invoker = true) AS
SELECT r.tenant_id, r.project_id, r.work_item_id, r.run_no,
       min(r.episode_no) AS episode_no,
       min(r.occurred_at) AS started_at,
       min(r.version) AS started_version,
       (array_agg(r.ended_at ORDER BY r.version DESC))[1] AS ended_at,
       (array_agg(r.ended_version ORDER BY r.version DESC))[1] AS ended_version
FROM (
    SELECT f.*,
           count(*) FILTER (WHERE f.blocked AND NOT coalesce(f.prev_blocked, false))
               OVER (PARTITION BY f.tenant_id, f.project_id, f.work_item_id ORDER BY f.version) AS run_no
    FROM (
        SELECT s.tenant_id, s.project_id, s.work_item_id, s.version, s.occurred_at, s.ended_at, s.ended_version, s.episode_no,
               (s.accrues AND s.is_blocked) AS blocked,
               lag(s.accrues AND s.is_blocked) OVER (PARTITION BY s.tenant_id, s.project_id, s.work_item_id ORDER BY s.version) AS prev_blocked
        FROM work_flow_segments s
    ) f
) r
WHERE r.blocked
GROUP BY r.tenant_id, r.project_id, r.work_item_id, r.run_no;

GRANT SELECT ON work_flow_steps, work_flow_segments, work_flow_episodes, work_flow_blocked_runs TO {{runtime_role}};
