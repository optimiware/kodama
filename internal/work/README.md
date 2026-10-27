# internal/work

Work management (task T-011): work items, immutable history, explicit business
transitions, human completion and the single active work-in-progress (WIP)
limit, following [work management](../../docs/specs/work-management.md) and the
[software design](../../docs/specs/software-design.md).

Status: implemented and tested against real PostgreSQL (and NATS for the
cancellation path). Nothing wires these routes, migrations or guards yet; that is
T-027's job, with the confirmations listed below.

## Layout

| Path | Purpose |
| --- | --- |
| `values.go`, `item.go`, `errors.go` | `Item` entity, value objects, patches, history entries, failure classes |
| `transitions.go`, `completion.go` | The transition table, `Item.Transition`, review subjects and the human completion decision |
| `ports.go`, `service.go`, `commands.go` | Consumer-owned ports (`Repository`, `Authority`, guards) and the use cases |
| `cancellation.go` | Cancellation intent seam: event decoding, scan source, `ReconcileCancellations` |
| `postgres/` | Tables (in the baseline schema, `internal/storage/migrations/001_schema.sql`), `Store` (`work.Repository`), guard binding, admission and cancellation seams |
| `http/` | Routes, DTO mapping, error mapping, generated types (`work.gen.go`) |
| `worktest/` | Test-only fixture stack used by the HTTP tests |
| `../../api/work.yaml` | OpenAPI 3.0.3 fragment with examples |

Dependencies point inward: `http` and `postgres` import `work`; `work` imports
only `domain` and the event contract types. Entities and use cases contain no
pgx, net/http, NATS, Git or S3 types.

## Model

* **Version vs. content revision.** Every accepted mutation increments `version`
  (the ETag). Only an actual change of `spec` or `acceptanceCriteria` increments
  `contentRevision`; title, blocking, assignee, parent, evidence links and every
  transition keep it. A patch that changes nothing writes nothing.
* **History.** Each mutation appends one `work_item_history` row (id = transition
  id, resource version, actor, time, reason, changed fields, before/after values,
  review/completion snapshots). Rows are append-only: no UPDATE/DELETE grant or policy.
* **Terminal states.** `done` and `cancelled` items accept no edits (reopen a done
  item first). `cancelled` has no outgoing edge.
* **Epics** group children (parent must be an epic of the same project; no cycles;
  checked under the project lock), have no workspace, count toward WIP like any
  item, and enter review with a child summary (at least one child, all children
  `done` or `cancelled`, at least one `done`) instead of a candidate.
* **Completion** (`review -> done`): the caller must be a person (actor type from
  the authority port) holding `work_item:complete` *at commit time*; the request
  names the observed `contentRevision`, the recorded candidate, every acceptance
  criterion, and every open finding of the verified subject by name (the reason
  must also name them). The review subject records the content revision at review;
  an edit afterwards makes it stale. No other code path completes work.

## Permissions

Permission names are owned here; the fixed role mapping is the identity module's.

| Permission | Required for |
| --- | --- |
| `work_item:read` | get, list, history |
| `work_item:create` | create |
| `work_item:edit_spec` | title, spec, acceptance, parent, evidence links |
| `work_item:edit_technical` | assignee, blocked reason |
| `work_item:refine` | `backlog <-> ready` |
| `work_item:work` | `ready -> active`, `active -> ready/review`, `review -> active` |
| `work_item:reopen` | `done -> active` |
| `work_item:complete` | `review -> done` (human only) |
| `work_item:cancel` | any unfinished state `-> cancelled` |

Authorization runs before persistence (`Service`) and the exact edge permission
is re-evaluated inside the mutating transaction through `TxGuards.Permissions`.

## Ports owned by this module

| Port | Implemented by (confirms) | Fails closed |
| --- | --- | --- |
| `Authority` (`ActorResolver` + `PermissionChecker`) | identity T-010 / T-027 | outage -> `ErrUnavailable`; unknown principal -> hidden |
| `TxGuards.Permissions` (`PermissionChecker`) | identity, bound to the transaction | missing -> `ErrUnavailable` |
| `TxGuards.Writers` (`WriterGuard`) | workspace T-013 / execution T-020 | missing -> `ErrUnavailable` |
| `TxGuards.Dependencies` (`DependencyGuard`) | M3 graph; `work.NoDependencyGraph` is the explicit M1 selection | missing -> `ErrUnavailable` |
| `TxGuards.Subjects` (`SubjectGuard`) | workspace/evidence modules | missing -> `ErrUnavailable` |
| `SubjectRefresher` (optional) | workspace, for human Git pushes | n/a (error -> unavailable) |
| `CancellationRequester` / `TxCancellationRequester` | execution T-020 | requester error -> retried |

`postgres.GuardBinder func(pgx.Tx) work.TxGuards` binds those guards to one
transaction so each answer reflects state after the item lock was acquired.
Memberships (assignee eligibility), children, parents and WIP are answered by
the adapter from work-owned or base tables.

## Concurrency and lock order

Every module touching these rows follows this order:

1. project row, `FOR NO KEY UPDATE` (only transitions and parent changes; it does
   not block foreign-key checks of concurrent inserts);
2. work item row (`FOR UPDATE` for mutations, `FOR SHARE` for admission);
3. scoped prerequisite rows (children, memberships, workspace/evidence rows),
   read without locking;
4. event backlog/scope rows inside `events/postgres.Store.Append`, always last.

* **Edits** lock only the item, compare the expected version under the lock and
  in the `UPDATE ... WHERE version = $n` predicate (stale = 412, hidden/missing =
  404).
* **WIP.** Every entry into `active` (first start, rework, reopen) goes through
  `AdmitActive`: project lock, then a separate `count(*)` of active items
  (blocked ones included) after the lock, then the transition write in the same
  transaction.
* **Uncertain commits** are reported as `ErrUnavailable` ("reread before
  retrying") and are never replayed automatically.

## Integration seams

### Workspace admission (T-013, T-020)

`postgres.LockActionable(ctx, tx, scope, id)` runs inside the caller's tenant
transaction, locks the item `FOR SHARE` and returns its kind, state, version and
content revision. It refuses epics (`ErrNotActionable`), cancelled items
(`ErrCancelled`) and anything outside the scope (`ErrNotFound`). The caller must
hold `tx` through its durable reservation (unique workspace row, dispatch claim,
writer reservation); a concurrent cancellation takes the same row `FOR UPDATE`,
so it either commits first (the call refuses) or waits for the reservation. An
earlier advisory read is not a substitute. T-013 owns the production unique
constraint on the work item; T-011 only supplies the lock and eligibility.
Leave cleanup-run eligibility to execution: cancellation restrictions apply to
work dispatch, not to unrelated cleanup purposes.

### Cancellation (T-020)

Cancellation commits immediately with reason, history, audit row and the existing
`work_item.transitioned` (`to: cancelled`) event, even with an unsettled writer
or a broker outage. That committed state is the durable intent:

* `postgres.CancellationBinding(name, TxCancellationRequester)` is an event-bus
  consumer (current-state, external-intent) that records a request per cancelled
  item; duplicate delivery is absorbed by the inbox and the requester must be
  idempotent;
* `Store.CancelledItems` + `work.ReconcileCancellations` rescan cancelled items
  so a lost or late event cannot leave a run unrequested.

Execution rechecks cancelled items periodically and when claiming work.

## Adapter-local representations (confirmed in review)

* Logical `wip_limit_active` is the existing `projects.active_wip_limit`; the
  project row is the lock.
* Event actor `service` is written to the audit table as `system` (the audit
  table has no `service`); the actor id is preserved.
* Default list order is `(created_at, id)`; history order is the resource
  version. Bounded evidence references and the completion request are defined in
  `api/work.yaml` using artifact IDs and commits, not new resources.
* Users are not foreign-keyed: assignees need a current tenant-wide or project
  membership row, checked in the mutating transaction.

## Tests

| Test | Covers |
| --- | --- |
| `Test_concurrent_spec_edit` | WM-02, WM-08, WM-A02 |
| `Test_last_wip_slot`, `Test_blocked_items_count_toward_wip`, `Test_reopen_wip` | WM-03, WM-04, WM-A03 |
| `Test_human_completion`, `Test_completion_decision` | WM-07, WM-A05 |
| `Test_cancel_no_new_work` | WM-08, WM-09, WM-A06 (PostgreSQL + NATS) |
| `Test_transition_matrix`, `Test_item_content_and_versions` | WM-03 (no services) |
| `Test_blocking_history` | WM-06, WM-A08 (history portion) |
| `Test_tenant_ownership`, `Test_cursor_scope` | WM-01, WM-10, WM-A01 |
| `Test_workspace_admission` | WM-11, WM-A07 (seam only) |
| `Test_prerequisite_races`, `Test_mutation_rollback_and_unknown_commit`, `Test_migration_reapply` | WM-03, WM-07, WM-08 |
| `Test_work_http_contract` | HTTP contract, strict JSON, status mapping |

```sh
go generate ./internal/work/...
git diff --exit-code -- internal/work/http/work.gen.go
export KODAMA_REQUIRE_POSTGRES=1   # with KODAMA_TEST_POSTGRES_ADMIN_URL and KODAMA_NATS_URL
go test -race -count=1 ./internal/work/...
make test-task TASK=T-011
```

## Not implemented here

M3 dependency graph and SQL flow reports (WM-05, WM-12), workspace provisioning
and Git (T-013), run cancellation and dispatch (T-020), identity and role
mapping (T-010), configurable completion policies, separate blocker or finding
resources, the composition root (T-027).
