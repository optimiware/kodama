# Flow summary query plan evidence (T-042)

Named workload **t042-flow-v1**, loaded by `loadWorkload` in
`postgres/query_plan_test.go`: 3 tenants x 2 projects x 200 work items x 21 history
rows (created, ready, active, then alternating review/active, one hour apart) =
25,200 `work_item_history` rows, `ANALYZE`d. One list page of 50 items of one
project, window `[2026-10-01T00:00Z, +10h)`, executed as the runtime role in a
read-only repeatable-read tenant transaction (row level security applies).

Result of one local run (PostgreSQL 17, development container; a single sample,
not a latency claim or an SLO):

| Property | Observed |
| --- | --- |
| History scans in the plan | 3 (summary, blocked runs, episodes each derive from the view) |
| Access path | Bitmap Index Scan on `work_item_history_version_key (tenant_id, work_item_id, version)` with `project_id` as a heap filter |
| History rows read | 3,150 = 3 x 50 items x 21 rows, not the 25,200 in the table |
| Execution time | about 10 ms |

The tenant, project and item predicates reach the scan because every window
function partitions by `(tenant_id, project_id, work_item_id)`, so PostgreSQL pushes
the quals below the `WindowAgg` nodes of the nested views.

**Index decision.** The existing unique index is used and the page reads only the
selected items' rows, so `T042_002_flow_history_index.sql` was **not** added: an
extra index would cost write amplification and storage on the append-only history
table of every mutation without a measured benefit. Revisit with a new measurement
if the workload grows (much longer histories or much larger pages); never add an
index speculatively.

Reproduce: `go test -run Test_flow_query_plan -v ./internal/metrics/postgres/`
(`KODAMA_PRINT_PLAN=1` also prints the JSON plan). The test asserts the properties
above (no sequential scan of the history, the existing index, a bounded number of
rows read), not exact planner output.
