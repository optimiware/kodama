---
id: T-062
milestone: M1
status: done
depends_on: [T-020, T-022]
decision_blockers: []
owns: [internal/execution/postgres/, internal/execution/adapters/, internal/execution/ports.go, internal/execution/cancellation.go, internal/results/postgres/, test/integration/execution-composition/, docs/implementation/execution-composition.md]
check: make check-go-integration PKGS="./internal/execution/... ./internal/results/... ./test/integration/execution-composition/... ./test/integration/results/..."
resources: [go, integration]
---
# T-062 — Execution/results transaction composition prerequisite

## Goal
Resolve the concrete transaction-binding risk before executable assembly, preserving the merged execution/results contracts.

## Contract
Follow SD-04/SD-09–SD-11 and the [seam checkpoint](../implementation/repository-layout.md#transaction-composition-checkpoint). Inventory actual `Env`, PostgreSQL `Peers`, `UpdateInTx`, session decorators and cancellation/revocation bindings. A callback opening its own tenant transaction is not a join. Preserve T-022's receipt-first result ordering and terminal `result.submit` exception (merged at `deff2e1`).

## Deliverables
Constructor/lock-order/transaction participation record and bounded adapter corrections needed for execution/results composition. In particular prove cancellation revokes runtime grants in the owning transaction, result/source routes use guarded stores, peer reservations/pins/events roll back together, and trusted reconciliation remains explicit. Use the existing adapter-local pgx seams; no generic transaction framework or domain dependency on pgx. Wider owner/API changes require an ordered prerequisite before continuing.

T-034's active gate paths and acceptance remain its owner's. Document the required gate participation boundary without inventing its implementation; T-073 qualifies its actual merged join before T-072 activation. Do not enable gate kinds in M1.

## Acceptance
`execution_peer_rollback`, `cancel_grant_atomicity`, `guarded_result_replay`, and `composition_tenant_pool` verify SD-04/SD-10/SD-11, IT-03/IT-06, PX-07/PX-10/PX-12 and WS-11 against real non-owner PostgreSQL roles. Inject peer/event/commit failures, cancellation versus callback, duplicate/unknown result outcomes and pooled cross-tenant reuse. Rerun affected T-020/T-022 checks; no skipped integration check establishes the prerequisite.

The explicit pre-review package scope includes the T-022 `test/integration/results` suite, alongside T-020 execution packages, so review receives the required regression evidence before final integration adds any further affected consumers.

## Out of scope

Rewriting orchestration, changing T-022 history, editing active gate/executor ownership, root composition, or holding SQL locks over Git/S3/Kubernetes calls.
