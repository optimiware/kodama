---
id: T-185
milestone: kernel
status: done
depends_on: [T-179]
decision_blockers: []
owns: [internal/storage/, docs/implementation/database-schema.md, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go]
check: make check-go PKGS='./internal/storage/...'
resources: [go, integration]
acceptance: [unit_storage_durable_run_state, unit_storage_run_state_guards]
---
# T-185 — Durable run-state schema for the kernel record

## Goal
Extend the expand-only schema so a `StateStore` can round-trip the complete `runtime.RunRecord` without a second source of truth in Go. T-182 found the gap while implementing the PostgreSQL adapter.

## Contract
`internal/kernel/runtime` (`RunRecord`, `Transition`, `Command`, `WorkloadObservation`, `ResourceObservation`, `PredicateEvaluation`, `Cleanup`, `Wait`), the T-174 contract suite, and P2.1 stage 1 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
One appended migration, expand-only, with manifest entry, RLS, grants and guards:

- **Lease and cleanup on `runs`:** `lease_purpose` (`execute`, `reconcile`, `cleanup`), `cleanup_max_attempts`, `cleanup_reason`.
- **Commands:** a tenant-scoped `run_commands` table keyed `(tenant_id, project_id, run_id, command_id)` carrying kind, target, spec digest, fence, state (`intent`, `confirmed`, `rejected`, `uncertain`), reason, result, binding columns and `recorded_at`, with a state-transition guard and runtime grants limited to insert plus state/reason/result updates. Resource commands belong to no step, so they must live here rather than on `run_steps`.
- **Workload observations:** `run_workload_observations` keyed `(tenant_id, project_id, run_id, step_key, attempt)` with binding, phase, result and `observed_at`, guarded so identity is fixed, fence and time never decrease, and an exited workload keeps its result.
- **Resource observations:** `run_resource_observations` keyed `(tenant_id, project_id, run_id, kind, name, identity)` carrying owner, lifetime, handle fence, state, reason and `observed_at`. Workspace observations must be representable, not only services.
- **Predicate evidence on `run_steps`:** `when_evaluated`, `when_result`, `when_input_digest`, `when_evaluated_at`, write-once, added to the runtime update grant.
- **Step result:** `result_code` and `result_reason` separate from the step `reason`.
- **Pre-open waits:** a way to record a wait identity and version before it opens, and a cancellation decision taken before it opened, without relaxing the rule that an opened wait must have a correlation or deadline. Choose either relaxed constraints with a `cancelled` exception or a dedicated waits table, and state which in the schema documentation.
- **Migration-count assertions** in the owned test files updated to the new baseline, exact rather than loosened.

Nothing is dropped; `projects.task_workflow` and the legacy task columns remain readable until the contract stage.

## Acceptance
`make check-go` with `Test_unit_storage_durable_run_state` and `Test_unit_storage_run_state_guards` passing, plus real-PostgreSQL evidence from a non-owner role: every `RunRecord` field round-trips, the guards reject regressions, RLS still isolates tenants, and a failed transition leaves no partial row.

## Out of scope
The adapter itself (T-182), the engine, NATS, and the contract stage that drops legacy columns.
