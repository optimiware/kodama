---
id: T-174
milestone: kernel
status: planned
depends_on: [T-171, T-173]
decision_blockers: []
owns: [internal/shell/runstate/, test/support/kernelcontracts/]
check: make check-go PKGS='./internal/shell/runstate/... ./test/support/kernelcontracts/...'
resources: [go]
acceptance: [unit_kernel_state_contract, unit_kernel_state_claim_races, unit_kernel_state_wait_consumption]
---
# T-174 — Memory state adapter and kernel state contract suite

## Goal
Implement the kernel `StateStore` port in memory and freeze one exported contract suite that every later state adapter must satisfy.

## Contract
[T-171](T-171-kernel-runtime-contract.md) ports, [pipeline execution](../specs/pipeline-execution.md), and P1.2 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- A memory adapter behind the T-171 port, built on the existing `internal/memdb` transaction kernel where it fits; it is test and development support, never a production fallback.
- One exported contract suite in `test/support/kernelcontracts` covering: claim races and takeover, fence rejection of a stale writer, optimistic versions, duplicate transitions, outbox intent committed with state and rolled back with it, cancellation that still permits cleanup claims, wait consumption and duplicate-signal convergence, and rebuild of a run view from retained records.
- Tenant and project scope enforced on every operation; a foreign or mismatched scope is a not-found, not a silent success.
- Tests that the suite itself fails when a deliberately broken adapter is supplied, so it cannot pass vacuously.

## Acceptance
`make check-go` with `Test_unit_kernel_state_contract`, `Test_unit_kernel_state_claim_races` and `Test_unit_kernel_state_wait_consumption` passing. This proves policy over memory state only; real PostgreSQL semantics stay with the persistence task.

## Out of scope
The engine, PostgreSQL, NATS, schema, and any change to `internal/kernel/runner`.
