---
id: T-195
milestone: kernel
status: done
depends_on: [T-182]
decision_blockers: []
owns: [internal/kernel/runtime/, internal/kernel/runner/, internal/shell/runstate/, internal/storage/, test/support/kernelcontracts/, docs/implementation/database-schema.md, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go]
check: make check-go PKGS='./internal/kernel/runtime/... ./internal/kernel/runner/... ./internal/shell/runstate/... ./internal/storage/...'
resources: [go, integration]
acceptance: [unit_kernel_admission_carries_task, unit_runstate_postgres_task_link]
---
# T-195 — Carry the task through admission and persist it on the run

## Goal
Make a task's run linkage real in the kernel and the state adapter, so a task's current run is enforced by the database rather than recorded by a test.

## Contract
C3 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), [work management](../specs/work-management.md), `internal/kernel/runtime` (`Admission`, `RunRecord`), and the composite `current_run_id` key from migration 004.

## Background
T-184's review found that nothing in the product records `runs.task_id`: `runtime.Admission` has no task field, the PostgreSQL adapter's run insert omits the column, and the runtime role has no grant to update it. Because `tasks.current_run_id` is constrained to `(tenant, project, task_id, id)`, `Start` and `Rerun` fail against the real stores; T-184's PostgreSQL evidence records the link as the table owner to prove the projection paths.

## Deliverables
- `Admission` carries the optional owning task, validated to be in the same project as the run; a task-free run stays valid.
- The PostgreSQL state adapter writes `task_id` in the same insert as the run and reads it back; the memory adapter matches.
- The runtime role gets exactly the grant it needs for that write, with no widening of unrelated columns.
- The kernel engine passes the task through when a run is admitted for a task, and omits it otherwise.
- Extend the shared contract suite with the linkage: a run admitted for a task reports it, a foreign-project task is refused, and a task-free run is unchanged.
- A real-PostgreSQL regression admits through the unmodified kernel path and shows `tasks.current_run_id` can be set and validated without owner intervention.

## Acceptance
`make check-go` with `Test_unit_kernel_admission_carries_task` and `Test_unit_runstate_postgres_task_link` passing, plus real-PostgreSQL evidence from a non-owner role.

## Out of scope
The task use cases, the projection, and the contract stage that drops legacy task columns.
