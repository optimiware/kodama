---
id: T-189
milestone: kernel
status: planned
depends_on: [T-185]
decision_blockers: []
owns: [internal/storage/, docs/implementation/database-schema.md, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go]
check: make check-go PKGS='./internal/storage/...'
resources: [go, integration]
acceptance: [unit_storage_wait_decision_ordering, unit_storage_materialization_records]
---
# T-189 — Wait decision ordering and materialization records

## Goal
Close the two gaps T-182 found while implementing the PostgreSQL adapter, so a wait can be decided before its step settles and secret materializations have a durable record.

## Contract
`internal/kernel/runtime` (`Wait`, `Decision`, `MaterializationRecord`, `CheckMaterializations`), `test/support/kernelcontracts/` (`wait/*`, `cancel/*`, `materialization/*`), and P2.1 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
One appended, expand-only migration, plus manifest, RLS, grants and guards:

- **Wait decisions before settlement.** The 004/005 checks `run_steps_wait_resolved`, `run_steps_wait_expired` and `run_steps_wait_cancelled` force the step status to agree with the wait decision, so a wait decided while its step is still `waiting` or `pending` cannot be stored. The port decides waits independently of their steps, and the contract suite asserts it. Relax those constraints to allow a complete decision on a step that has not settled, while keeping the existing requirements that decision columns are complete and immutable once decided, and that a decided wait can never be reopened.
- **Materialization records.** Add a tenant-isolated table keyed by scope, run, step and attempt holding the materialization identity, issue time and revocation time/evidence, with forced RLS, runtime grants limited to the needed columns, and a guard making `revoked` final and the identity and issue time immutable.
- **Outputs immutability.** Guard `run_steps.outputs` so recorded step outputs cannot be rewritten after the settling commit, rather than relying on Go alone.
- **Unpublished outbox index.** Add a partial index on `audit_events (tenant_id, created_at, id) WHERE published_at IS NULL`, so the bounded outbox query does not sort the tenant's whole unpublished set on every page.
- Update the migration-count assertions in the owned test files to the new baseline, exact rather than loosened.

Nothing is dropped; legacy task columns stay readable until the contract stage.

## Acceptance
`make check-go` with `Test_unit_storage_wait_decision_ordering` and `Test_unit_storage_materialization_records` passing, plus real-PostgreSQL evidence from a non-owner role: a decided wait persists while its step stays `waiting`, a decided wait cannot be reopened, a materialization records issue and revocation once with `revoked` final, outputs cannot be rewritten after settlement, RLS isolates tenants, and a refused write leaves no partial row.

## Out of scope
The PostgreSQL adapter (T-182), the engine, and the contract stage that drops legacy columns.
