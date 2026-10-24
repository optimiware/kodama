---
id: T-073
milestone: M2
status: done
depends_on: [T-034, T-062, T-086, T-091]
decision_blockers: []
owns: [test/integration/gate-composition/]
check: make check-go-integration PKGS=./test/integration/gate-composition/...
resources: [go, integration]
acceptance: [gate_join_rollback, gate_join_revocation, gate_join_restart]
---
# T-073 — Gate/execution transaction qualification prerequisite

## Goal
Prove the active gate owner's merged contribution composes with execution before enabling it in the app.

## Contract
Consume T-034's actual subject/decision/progression and lock-order contracts with T-062's execution/results bindings. Record where gate decision, current authorization, applicability and execution progression are transactional, and where durable intent/reconciliation is required. Do not invent a generic transaction API or assume two independent transactions are atomic.

## Deliverables
Real PostgreSQL non-owner-role test composition, constructor/binding fixtures and lock-order/failure record in the suite. Force errors between decision/applicability checks and progression; verify required state/history/audit/outbox changes roll back. If the real join is missing, park activation and request a bounded repair with explicit ownership/dependencies; tests cannot declare an unsupported composition passed.

## Acceptance
`gate_join_rollback`, `gate_join_revocation`, and `gate_join_restart` verify SD-04/SD-10/SD-11, IT-03, DQ-03/DQ-04/DQ-11/DQ-14 and PX-13. Include concurrent reviewer revocation, changed spec/source/configuration/conformance evidence, pooled tenant reuse, duplicate decisions and recovery without Job/capacity allocation. T-034's original acceptance remains required; T-072 repeats the selected executable path.

## Out of scope
Editing the active gate owner, root wiring, permissive fixtures or postponing atomicity/RLS checks to T-038.
