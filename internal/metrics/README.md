# internal/metrics

SQL flow summaries (task T-042): lead, active, review, blocked and reopen
observations derived from the immutable work history of T-011, following
[work management](../../docs/specs/work-management.md) (WM-06, WM-12, WM-A08), the
[lean-flow chapter](../../docs/book/04-lean-flow-and-governance.md) and
[identity and tenancy](../../docs/specs/identity-tenancy-and-rbac.md) (IT-03).
This is not a metrics product: there is no resource, route, table, worker,
materialized state or event. A summary is a read model computed on request and
contributed to the items of the existing work list.

Status: implemented and tested against real PostgreSQL. Nothing wires the
store, authorizer or list contribution yet; that is T-046's job, with the
confirmations listed below.

## Layout

| Path | Purpose |
| --- | --- |
| `window.go`, `summary.go`, `errors.go` | `Window`, the `WorkSummary` read model and its coherence check, failure classes |
| `ports.go`, `service.go` | Consumer-owned ports (`SummaryReader`, `ReadAuthorizer`) and `Service.Summarize` |
| `auth/` | `metricsauth.WorkReadAuthorizer`: delegates to `work.Authority` for `work_item:read`; no role map, no policy |
| `postgres/` | The four flow views (in the baseline schema, `internal/storage/migrations/001_schema.sql`), `Store` (`SummaryReader`), batch SQL, tests |
| `http/` | Types generated from `api/work-summaries.yaml`, option parsing, mapping; no routes |
| `PLAN.md` | Retained query-plan workload and result |
| `../../api/work-summaries.yaml` | OpenAPI 3.0.3 fragment: query options, `FlowSummary`, optional list-item member, examples |

Dependencies point inward: `postgres`, `http` and `auth` import `metrics`; `metrics`
imports only `domain`. Entities and use cases hold no pgx or HTTP types. Work
entities, tables, grants and mutations stay T-011's; only `work_item_history` is read.

## Executable semantics

All of this is implemented in the four flow views of the baseline schema
(`internal/storage/migrations/001_schema.sql`) and the summary statement;
the fixtures in `postgres/*_test.go` pin each rule.

1. **Ordering.** History resource `version` orders an item's rows, never the
   timestamp or the history id. Timestamps have microsecond precision, are kept
   exactly, and may be equal (zero-length intervals contribute nothing).
2. **State and blocked flag after a row.** The state is the row's `to_state`. The
   blocked flag is the latest *explicitly recorded* `after_values.blockedReason`:
   a JSON string means set, JSON `null` means cleared, and an absent key means
   unchanged (unrelated edits and transitions carry no replacement). Replacing a
   non-null reason is not a new blocked interval.
3. **Segments.** A segment lasts from a row to the next row of the same item; the
   last segment is open. `done` and `cancelled` segments accrue nothing.
4. **Episodes.** An unfinished episode starts at creation or at a **reopen** and
   ends at the next transition to `done` or `cancelled`. A reopen is exactly
   `change_kind = 'transitioned'`, `from_state = 'done'`, `to_state = 'active'`;
   review rework (`review → active`) is not a reopen. Time between an episode's
   completion and the next reopen is excluded. Cancellation ends an episode but is
   not a successful completion (`completedEpisodes` vs `cancelledEpisodes`).
5. **Durations** (summed over segments clipped to the window):
   `lead` = unfinished time (backlog, ready, active, review); `active`, `review` =
   time in that state; `blocked` = unfinished time with the flag set; `reopened` =
   lead of episodes begun by a reopen. **Blocked overlaps the other durations** and
   is not additive with them. A terminal state suspends blocking; a reopen resumes
   it when the reason is still recorded (T-011 keeps that field).
6. **Blocked intervals** are maximal runs of consecutive unfinished segments with
   the flag set (`work_flow_blocked_runs`). Content edits, state changes and
   reason replacement do not split a run; a terminal state does. Only runs with a
   non-empty overlap with the window are counted.
7. **Window.** `[from, to)`, UTC, microsecond precision, both required, `from <
   to`, `to` not in the future, `from` not before 2000-01-01. Sub-microsecond
   bounds are rejected, never rounded. The whole history is reconstructed first
   and then clipped, so an interval that began before the window is measured from
   `from` with its true state, and nothing recorded at or after `to` is used
   (a completion exactly at `to` happens outside the window).
8. **Censoring.** `inFlightAtStart`: an unfinished episode began before `from` and
   had not ended before it. `openAtEnd`: the item was unfinished just before `to`;
   its last interval is capped at `to` and the cap is **not** a completion.
   `stateAtEnd` is the state just before `to`. `completedLead` sums the whole,
   unclipped duration of episodes completed inside the window.
9. **Counts in the window.** `reopens` counts reopen transitions with
   `from ≤ occurred_at < to`; a reopened interval that began earlier still
   contributes its clipped time.
10. **Invalid history.** The first anomaly of an item's whole chain makes its
    status `invalid_history` with the anomaly name and **no figures** (never
    negative or invented durations): `first_not_created`, `duplicate_created`,
    `version_gap`, `time_backwards`, `state_chain`, `state_unknown`,
    `blocked_value`. An item with no row before `to` is `not_started`. Other items
    of the batch are unaffected.
11. **Observed version.** `observedVersion` is the newest history version before
    `to`: the revision the figures came from. History is append-only, so a later
    read of the same window sees the same figures unless rows were recorded with
    timestamps before `to` in the meantime; compare `observedVersion` to detect it.

## Tenant authority

* `Service.Summarize` validates scope, identifiers, window and batch size, then
  calls `ReadAuthorizer` **before** the reader; denied, hidden and unavailable
  answers never reach SQL, and every call authorizes again. A nil collaborator
  fails closed at construction; a zero or nil `Service` returns unavailable.
* The reader port returns only requested items of the scope; the service rejects an
  unrequested, foreign-scope, duplicate or incoherent summary as unavailable
  instead of passing it on.
* The store reads through `storage.WithTenantOptions` (read-only, repeatable read)
  under the non-owner runtime role. The statement has explicit tenant, project and
  item predicates **and** is bounded by forced row level security because the
  views are `SECURITY INVOKER` (never owner-executed, no bypass policy). Requested
  items of another tenant or project, and missing items, are omitted identically.
* The context is transaction-local: a pooled connection carries none afterwards,
  on success or failure (tested with a one-connection pool).

## Failure and recovery

Reads write nothing and are safely retryable after fresh authorization. A query
failure, cancellation or outage returns an `ErrUnavailable` error and **no**
summaries. The batch is one statement, hence one snapshot: a concurrent atomic
work/history mutation is seen entirely or not at all, and uncommitted history is
invisible. There is no replay worker or materialized state to reconcile.

## Interfaces

* `metrics.NewService(Config) (*Service, error)`;
  `Service.Summarize(ctx, scope, principalID, ids, window) ([]WorkSummary, error)`;
  `Service.Window(from, to)`; `metrics.NewWindow(from, to, now)`; `metrics.MaxBatch = 200`.
* `metricsauth.NewWorkReadAuthorizer(work.Authority)`.
* `metricspg.NewStore(storage.TxBeginner)`; the four flow views are part of the
  baseline schema (`internal/storage/migrations/001_schema.sql`), so this
  module contributes no migration.
* `metricshttp.ParseOptions(include, from, to, now) (Options, error)`,
  `Options.Binding()`, `FromSummary`, `Contributions`.
* Events and commands: none.

## Integration instructions (T-046)

1. Register no migration for this module: the flow views are part of the baseline
   schema (`internal/storage/migrations/001_schema.sql`) that
   `storage.BaseContribution()` applies.
2. Construct `metricspg.NewStore(pool)`, `metricsauth.NewWorkReadAuthorizer(workAuthority)`
   and `metrics.NewService`.
3. Compose `api/work-summaries.yaml` into the published document: add the three
   parameters to `listWorkItems` and the optional `flowSummary` member to the list item.
4. In the list handler, call `metricshttp.ParseOptions` (an invalid option set is
   `400 invalid_request`); when `Include`, bind `Options.Binding()` into the cursor
   validation, list the page as today, call `Service.Summarize` with the page's ids
   and attach `Contributions(...)[id]`. An `ErrDenied`/`ErrHidden` outcome maps like the
   list's own authorization; `ErrUnavailable` is `503 unavailable` (do not return the
   page without the requested summaries). Authorize every page.
5. Register no `/metrics` route, CLI command, worker or consumer.

## Assumptions to confirm

* **Interval definitions** (above) are task-owned: the specifications name the
  observations but not their formulas. Confirm lead as "unfinished episodes
  excluding done gaps", blocked as overlapping state time, and cancellation as an
  episode end that is not a completion.
* **Option names** `includeFlowSummary`, `flowFrom`, `flowTo`, the UTC half-open
  window, the explicit-window requirement, rejection of a future `to`, and the
  200-item batch bound.
* **Unit** `…Micros` as integers (exact; JSON-safe for 285 years).
* The 2000-01-01 lower bound on `from` is a sanity bound, not a retention policy.

## Query plans and indexes

`PLAN.md` retains the named workload and measured plan. The existing
`work_item_history_version_key (tenant_id, work_item_id, version)` serves the
batch statement (the tenant, project and item predicates are pushed below the
window functions because every window partitions by `(tenant_id, project_id,
work_item_id)`); no index was added. Re-run with
`go test -run Test_flow_query_plan -v ./internal/metrics/postgres/`
(`KODAMA_PRINT_PLAN=1` prints the JSON plan).

## Tests

Run PostgreSQL tests with `KODAMA_TEST_POSTGRES_ADMIN_URL` set and
`KODAMA_REQUIRE_POSTGRES=1`; they create `t002_<random>` databases through the shared
helper. Without the URL they skip with a message.

```sh
go test -race -count=1 ./internal/metrics/...
go generate ./internal/metrics/... && git diff --exit-code -- internal/metrics/http/work-summaries.gen.go
```
