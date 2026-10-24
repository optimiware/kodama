package postgres

// summarySQL derives the flow summary of a bounded set of items in one
// statement, hence one snapshot.
//
// Parameters: $1 tenant, $2 project, $3 item identifiers (text[]), $4 window
// start, $5 window end (half-open [$4, $5)).
//
// The segments are reconstructed from the item's whole history before they are
// clipped, so an interval that began before the window is measured from the
// window start with its true state, and an interval that ends after the window
// is capped at $5 without revealing how it later ended. Equal timestamps give
// zero-length segments that contribute nothing.
const summarySQL = `
WITH seg AS (
    SELECT s.work_item_id, s.version, s.state, s.is_blocked, s.accrues, s.episode_initial, s.is_reopen, s.anomaly,
           s.occurred_at AS a, s.ended_at AS b,
           CASE WHEN least(coalesce(s.ended_at, $5::timestamptz), $5::timestamptz) > greatest(s.occurred_at, $4::timestamptz)
                THEN round(extract(epoch FROM least(coalesce(s.ended_at, $5::timestamptz), $5::timestamptz)
                                              - greatest(s.occurred_at, $4::timestamptz)) * 1000000)::bigint
                ELSE 0 END AS us
    FROM work_flow_segments s
    WHERE s.tenant_id = $1 AND s.project_id = $2 AND s.work_item_id = ANY($3::text[])
), item AS (
    SELECT work_item_id,
           min(anomaly) AS anomaly,
           coalesce(max(version) FILTER (WHERE a < $5::timestamptz), 0) AS observed_version,
           coalesce(sum(us) FILTER (WHERE accrues), 0)::bigint AS lead_us,
           coalesce(sum(us) FILTER (WHERE state = 'active'), 0)::bigint AS active_us,
           coalesce(sum(us) FILTER (WHERE state = 'review'), 0)::bigint AS review_us,
           coalesce(sum(us) FILTER (WHERE accrues AND is_blocked), 0)::bigint AS blocked_us,
           coalesce(sum(us) FILTER (WHERE accrues AND NOT episode_initial), 0)::bigint AS reopened_us,
           count(*) FILTER (WHERE is_reopen AND a >= $4::timestamptz AND a < $5::timestamptz) AS reopen_count,
           coalesce(bool_or(accrues AND a < $5::timestamptz AND (b IS NULL OR b >= $5::timestamptz)), false) AS open_at_end,
           (array_agg(state) FILTER (WHERE a < $5::timestamptz AND (b IS NULL OR b >= $5::timestamptz)))[1] AS state_at_end
    FROM seg
    GROUP BY work_item_id
), runs AS (
    SELECT r.work_item_id, count(*) AS blocked_intervals
    FROM work_flow_blocked_runs r
    WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.work_item_id = ANY($3::text[])
      AND least(coalesce(r.ended_at, $5::timestamptz), $5::timestamptz) > greatest(r.started_at, $4::timestamptz)
    GROUP BY r.work_item_id
), eps AS (
    SELECT e.work_item_id,
           count(*) FILTER (WHERE e.outcome = 'done' AND e.ended_at >= $4::timestamptz AND e.ended_at < $5::timestamptz) AS completed,
           count(*) FILTER (WHERE e.outcome = 'cancelled' AND e.ended_at >= $4::timestamptz AND e.ended_at < $5::timestamptz) AS cancelled,
           coalesce(sum(round(extract(epoch FROM e.ended_at - e.started_at) * 1000000)::bigint)
                    FILTER (WHERE e.outcome = 'done' AND e.ended_at >= $4::timestamptz AND e.ended_at < $5::timestamptz), 0)::bigint AS completed_lead_us,
           coalesce(bool_or(e.started_at < $4::timestamptz AND (e.ended_at IS NULL OR e.ended_at >= $4::timestamptz)), false) AS in_flight_at_start
    FROM work_flow_episodes e
    WHERE e.tenant_id = $1 AND e.project_id = $2 AND e.work_item_id = ANY($3::text[])
    GROUP BY e.work_item_id
)
SELECT i.work_item_id, i.anomaly, i.observed_version, i.lead_us, i.active_us, i.review_us, i.blocked_us, i.reopened_us,
       i.reopen_count, coalesce(r.blocked_intervals, 0), coalesce(e.completed, 0), coalesce(e.cancelled, 0),
       coalesce(e.completed_lead_us, 0), coalesce(e.in_flight_at_start, false), i.open_at_end, i.state_at_end
FROM item i
LEFT JOIN runs r ON r.work_item_id = i.work_item_id
LEFT JOIN eps e ON e.work_item_id = i.work_item_id
ORDER BY i.work_item_id`
