---
id: T-192
milestone: kernel
status: done
depends_on: [T-189]
decision_blockers: []
owns: [internal/storage/, docs/implementation/database-schema.md, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go]
check: make check-go PKGS='./internal/storage/...'
resources: [go, integration]
acceptance: [unit_storage_signal_decision_reason]
---
# T-192 — Self-contained signal decision reasons

## Goal
Make a signal decision record complete on its own, so a replay after ordinary wait and run retention cleanup returns the original reason without reading the wait row.

## Contract
`internal/kernel/runtime` (`SignalRecord`, `SignalTombstone`), PX-43 and PX-A38 in [pipeline execution](../specs/pipeline-execution.md), EC-03 in the [event catalog](../contracts/event-catalog.md), and the T-182 adapter.

## Background
`signal_records` stores the decision kind, outcome, answer, actor, version and time, but not the decision reason. When a signal lost to a cancellation, T-182 reads `run_steps.wait_reason`; if that row is gone it returns an error rather than guessing. A non-expiring decision record must not depend on a row that ordinary retention removes.

## Deliverables
- An appended migration adding the decision reason to `signal_records`, with the same non-expiring retention and project-scoped removal, plus grants and RLS unchanged in shape.
- The column is nullable for existing rows and required for new decision records, so an existing database upgrades without rewriting history.
- A real-PostgreSQL regression records a cancelled decision and reads the reason back from `signal_records` alone, with the wait row removed. The adapter read itself lands in T-182.
- Migration-count assertions updated to the new exact baseline.

## Acceptance
`make check-go` with `Test_unit_storage_signal_decision_reason` passing, plus real-PostgreSQL evidence from a non-owner role.

## Out of scope
The contract shape, other tables, and the engine.
