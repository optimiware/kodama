---
id: T-088
milestone: M1
status: done
depends_on: [T-087, T-005, T-011, T-012, T-013, T-015, T-019]
decision_blockers: []
owns: [internal/events/eventstest/, internal/work/worktest/, internal/workspace/workspacetest/, internal/artifacts/artifactstest/, internal/runtimeauth/rtest/, internal/identity/idtest/, internal/secrets/secretstest/]
check: make check-go PKGS="./internal/events/eventstest/... ./internal/work/worktest/... ./internal/workspace/workspacetest/... ./internal/artifacts/artifactstest/... ./internal/runtimeauth/rtest/... ./internal/identity/idtest/... ./internal/secrets/secretstest/..."
resources: [go]
acceptance: [unit_memory_events_outbox, unit_memory_work_contract, unit_memory_workspace_contract, unit_memory_artifacts_contract, unit_memory_runtimeauth_contract, unit_memory_identity_contract, unit_memory_secrets_contract]
---
# T-088 — Foundation memory adapters and contracts

## Goal
Let the assembled M1 slice run in `development` with no database, broker or object store, without a second behavior to maintain.

## Contract
For each foundation port the M1 slice consumes, provide a memory adapter over `memdb` in the module's `*test` package, and an exported `Contract(t, open)` suite that the memory adapter and the existing PostgreSQL adapter both run ([SD-13](../specs/software-design.md)): events (outbox, inbox, replay, `events.BrokerPublisher` in process), work, workspaces (with temporary bare Git for `RefReader`/`RefPublisher`), artifacts (`artifacts.ObjectStore`), runtime authority, identity and secrets (with an ephemeral `secrets.KeyWrapper`). Reuse the existing `idtest.Mem`, `MemObjects` and peer fakes where they already hold the semantics; do not rewrite working adapters.

Cross-module joins take the same `memdb.Tx` as their PostgreSQL bridges take `pgx.Tx`. Memory adapters are flagged development-only so `production` refuses them ([SD-12](../specs/software-design.md)). Contract suites assert tenant isolation, unique and conflict outcomes, version checks, all-or-nothing updates and replay; they do not assert lock order or isolation levels.

## Deliverables
The adapters, the contract suites with their PostgreSQL wiring (behind `layer.SkipService`), and a short per-module note of anything the memory side deliberately does not model. T-080 and T-081 build execution and results/logs on top; M2 modules follow in their own tasks.

## Acceptance
Each `unit_memory_*_contract` runs the module's `Contract` on the memory adapter without a service; the same suites run on PostgreSQL in the `test` environment and are recorded at T-026. `unit_memory_events_outbox` proves an event written in one `Update` is published once, deduplicated on redelivery and not published when the update rolls back. Cover SD-A11 and SD-A12.

## Out of scope
Execution, results and logs adapters (T-080, T-081), M2 and M3 modules, restart persistence, and any SQL, RLS or locking claim.
