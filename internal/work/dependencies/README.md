# internal/work/dependencies

Business dependency graph of work management (task T-045): same-project,
acyclic prerequisite links between work items, evaluated when a work item is
admitted to `active`. Baseline: [work management](../../../docs/specs/work-management.md)
(WM-01, WM-03, WM-05), [identity and RBAC](../../../docs/specs/identity-tenancy-and-rbac.md)
and [software design](../../../docs/specs/software-design.md); acceptance WM-A04 and
the contract in [`api/work-dependencies.yaml`](../../../api/work-dependencies.yaml).

Status: implemented and tested against real PostgreSQL. Nothing registers these
routes, the migration or the guard yet; that is T-046's job, as described under
[Integration](#integration).

## Business links are not pipeline DAGs

A **link** says "the dependent work item cannot start until its prerequisite is
done". It orders business intent across work items of one project. A pipeline
definition's step graph orders executable steps inside one run; the two never
share tables, identifiers or code, and this module never reads pipeline data.

## Model

* **Direction.** A link is `(tenant, project, dependent, prerequisite)`.
  `GET /work-items/{id}/dependencies` lists the *direct prerequisites* of `{id}`;
  `dependencyId` always names the prerequisite. Reverse listing ("what waits for
  me?") is not exposed.
* **Same project, acyclic.** Both endpoints must be work items of the link's
  tenant and project, enforced by composite foreign keys to
  `work_items (tenant_id, project_id, id)`. A self-link and a link that would make
  the dependent reachable from its prerequisite are `ErrGraph`. A work item has at
  most `MaxPrerequisites` (100) direct prerequisites.
* **Version semantics.** A link change is an edit of the *dependent*: an actual
  change increments the dependent's `version` (its ETag), appends one history
  entry (`changed field "dependencies"`, bounded before/after delta
  `{"prerequisiteId": ...}`), one audit row and one `work_item.updated` event. It
  never changes `contentRevision`, the state or any other field of either item,
  and never touches the prerequisite at all.
* **Terminal items.** `done` and `cancelled` dependents accept no link changes,
  like any other edit (reopen a done item first); that applies even to no-op
  requests. Prerequisites may be in any state.
* **Duplicates are no-ops.** After authorization and version validation, adding an
  existing link or removing an absent link writes nothing (no version, history,
  audit or event) and returns the unchanged version. Both items must still exist
  in the project.
* **Admission.** `ready -> active` requires every direct prerequisite `done`
  (`work.DependenciesComplete`, a pure policy in `internal/work`). Only `done`
  satisfies a link: a cancelled prerequisite never does, and reopening a done
  prerequisite withdraws the satisfaction. Deleting the *link* is the only way to
  drop a prerequisite deliberately. Work items are never deleted by request
  handling, so "dependency deleted before admission" means a link deletion.
  The merged T-011 transition table calls the dependency guard for
  `ready -> active` only; rework (`review -> active`) and reopen (`done -> active`)
  use WIP admission alone, and this module does not change that.
* **Failure text.** A refused admission is `work.ErrPrerequisite` with a fixed
  message naming neither the prerequisite nor its state. Graph failures carry no
  path. Missing, foreign-tenant and foreign-project references are one answer
  (`work.ErrNotFound`), produced before any graph diagnostic.

## Permissions

| Operation | Permission | Notes |
| --- | --- | --- |
| list | `work_item:read` | authorized again on every page |
| add / remove | `work_item:edit_spec` | the permission of parent edits; no new role or name |

The service resolves the actor and authorizes before persistence. The store
re-evaluates the permission through a transaction-bound `work.PermissionChecker`
**after** the project and item locks are held and **before** the version is
compared, so a revocation committed during a lock wait is seen and a revoked
caller learns nothing about versions. A missing binder, a nil checker and an
identity outage are `work.ErrUnavailable`, never access.

## Layout

| Path | Purpose |
| --- | --- |
| `link.go`, `commands.go`, `errors.go` | `Link`, `Owner` (the dependent as the graph sees it), `Facts`, `Change`, commands, `ErrGraph` |
| `ports.go`, `service.go` | Consumer-owned `Repository` and `Tx` ports; the `List`/`Add`/`Remove` use cases |
| `postgres/` | `work_item_dependencies` (`T045_001_work_dependencies.sql`), `Store`, `NewDependencyGuard`, `Migrations()` |
| `http/` | Routes, DTO mapping, error mapping, generated types (`dependencies.gen.go`) |
| `depstest/` | Test-only fixture stack: real work store with the real guard, real dependency store, fixture authority |
| `../dependency-admission.go` | Pure completion policy (`work.PrerequisiteStatus`, `work.DependenciesComplete`) |
| `../../../api/work-dependencies.yaml` | OpenAPI 3.0.3 fragment with examples |

Dependencies point inward: `postgres` and `http` import this package and `work`;
this package imports `work` and `domain` only; `work` never imports this package.
Entities and use cases contain no pgx or net/http types.

## Concurrency and lock order

Identical to [internal/work](../README.md#concurrency-and-lock-order); this module
introduces no lock scheme of its own.

1. the project row, `FOR NO KEY UPDATE`: **every** link insertion and removal takes
   it, so graph changes serialize with each other and with work transitions;
2. the dependent work item row, `FOR UPDATE`;
3. link and prerequisite rows, read without locking;
4. event backlog and scope rows inside `events/postgres.Store.Append`, last.

Cycle rejection is not a constraint: a graph property is not row-local. After
the project lock is held, a duplicate-eliminating recursive query
(`UNION`, not `UNION ALL`) asks whether the proposed prerequisite already reaches
the dependent; every graph writer holds the same lock, so the answer is exact.
The admission guard runs inside a work transition that already holds locks 1 and
2; it only reads, opens no second transaction, caches nothing and never takes the
project lock after the item lock.

## Recovery and idempotency

* Database commit is the mutation boundary: edge, version, history, audit and
  outbox event commit together. No broker or other network call runs inside the
  transaction; graph correctness never depends on NATS. Delivery and
  deduplication of the event belong to the existing outbox machinery.
* A failed or unknown commit is `work.ErrUnavailable` ("reread before retrying").
  Nothing is replayed automatically. After an uncertain response, reread
  `GET .../dependencies` (its `ETag` is the dependent's version) and retry with
  that version: a retry of an already applied change is a no-op and writes
  nothing; a blind replay with the old version is `412`.
* A full event backlog fails the mutation atomically (`503`); reads and no-ops
  keep working.

## Integration

T-046 (integration owner) wires, without editing this package:

1. **Migration order.** `storage.BaseContribution()`, events, T-011 work, then
   `dependencies/postgres.Migrations()`.
2. **Guard injection.** Replace the explicit M1 `work.NoDependencyGraph{}` in the
   work store's `GuardBinder` with `dependencies/postgres.NewDependencyGuard(tx)`:

   ```go
   func(tx pgx.Tx) work.TxGuards {
       return work.TxGuards{
           Permissions:  identity.Bind(tx),
           Writers:      workspace.Bind(tx),
           Dependencies: deppg.NewDependencyGuard(tx),
           Subjects:     evidence.Bind(tx),
       }
   }
   ```

3. **Services and routes.**

   ```go
   store, _ := deppg.NewStore(pool, eventStore, func(tx pgx.Tx) work.PermissionChecker { return identity.Bind(tx) })
   svc, _ := dependencies.NewService(dependencies.Config{Repository: store, Authority: authority})
   h, _ := dependencieshttp.NewHandler(svc, cursorCodec)
   _ = h.RegisterRoutes(router)
   ```

4. **Authorization actions** registered with the router's authorizer:
   `work_item.dependency.list`, `work_item.dependency.add`,
   `work_item.dependency.remove`, each addressing the dependent work item
   (`ResourceKind: work_item`).
5. **OpenAPI.** Compose `api/work-dependencies.yaml` into the published document.
   `422 prerequisite_unmet` for admission failures is the existing work transition
   response.

## Tests

| Test | Covers |
| --- | --- |
| `Test_concurrent_cycle_insert`, `Test_dependency_graph_cases` | WM-05, WM-A04 |
| `Test_foreign_dependency` | WM-01, WM-A01, IT-A02 |
| `Test_dependency_not_done`, `Test_dependency_admission_races` | WM-03, WM-05, WM-A04 |
| `Test_dependency_deleted_before_admission` | WM-03, WM-05, IT-11 |
| `Test_dependency_entity`, `Test_dependency_admission_policy` | WM-05 (no services) |
| `Test_dependency_authorization` | IT-02, IT-03, SD-A04 |
| `Test_dependency_atomicity_and_recovery`, `Test_dependency_history_events` | WM-08, SD-A06 |
| `Test_dependency_isolation_and_migrations` | WM-01, IT-04 |
| `Test_dependency_http_contract` | HTTP contract |

```sh
go generate ./internal/work/dependencies/...
git diff --exit-code -- internal/work/dependencies/http/dependencies.gen.go
export KODAMA_REQUIRE_POSTGRES=1   # with KODAMA_TEST_POSTGRES_ADMIN_URL; NATS is not needed
go test -race -count=1 ./internal/work/dependencies/...
make test-task TASK=T-045
```

## Not implemented here

Cross-project dependencies, reverse ("dependents of") listing, a generic workflow
language, SQL flow reports (T-042), the composition root (T-046) and any change to
T-011's transition table or files.
