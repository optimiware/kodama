---
id: T-192
milestone: kernel
status: planned
depends_on: [T-182]
decision_blockers: []
owns: [internal/storage/, internal/shell/runstate/postgres/signal.go, internal/shell/runstate/postgres/contract_test.go, docs/implementation/database-schema.md, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go]
check: make check-go PKGS='./internal/storage/... ./internal/shell/runstate/postgres/...'
resources: [go, integration]
acceptance: [unit_storage_signal_decision_reason, unit_runstate_signal_replay_after_retention]
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
- The PostgreSQL adapter reads and writes the reason from the record, and the wait-row fallback is removed.
- A regression that replays a refused decision after the wait, step and run rows are removed and still returns the original reason.
- Migration-count assertions updated to the new exact baseline.

## Acceptance
`make check-go` with `Test_unit_storage_signal_decision_reason` and `Test_unit_runstate_signal_replay_after_retention` passing, plus real-PostgreSQL evidence from a non-owner role.

## Out of scope
The contract shape, other tables, and the engine.
