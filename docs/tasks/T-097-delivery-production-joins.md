---
id: T-097
milestone: M2
status: done
depends_on: [T-035, T-067, T-073, T-093]
decision_blockers: []
owns: [internal/delivery/adapters/, internal/gates/adapters/, internal/execution/adapters/]
check: make check-go PKGS="./internal/delivery/... ./internal/gates/... ./internal/execution/..."
resources: [go]
acceptance: [delivery_tx_environments, gate_deployment_binder, runtime_secret_route]
---
# T-097 — Delivery production joins

## Goal
Give execution, the gate store and the runtime secret route the production adapters they need to admit a step that declares an environment and to decide a deployment-bearing gate.

## Contract
T-038 found a step that declares an environment is refused with 503 because `execpg.Peers.Environments` has no production adapter; the cleanup environment is unbound; the runtime secret route is unwired; and deployment-bearing gates fail closed because the gate store's `DeploymentBinder` has no implementation (the T-073 bounded repair request). T-067's `test/integration/delivery/stack/txdelivery.go` is the test-local reference for the first. Implement over T-035's delivery module and T-077's environment pinning: (1) a transaction-bound delivery reader satisfying `execpg.Peers.Environments` (current authority, pinned environment and credential snapshot read in the admission transaction); (2) an adapter implementing `gates.Deployments` and the gate store's `DeploymentBinder` over the deployment record (configuration digest, scope and cleanup state); (3) the runtime secret-resolution route adapter, resolving only the secrets the pinned environment names, once per attempt, with a read-for-use audit. Every adapter is scoped by tenant/project and fails closed: an unreadable record is unavailable, a foreign or hidden record is not found, never a grant.

## Deliverables
The three adapters in the owned packages with their README notes and unit tests over port doubles. Replace nothing in `internal/app`; T-101 binds them. Do not edit the delivery service or the gate store themselves.

## Acceptance
`delivery_tx_environments` proves environment pins are read inside the admission transaction, a stale or revoked authority refuses admission, and an outage is unavailable and not a pass. `gate_deployment_binder` proves a deployment-bearing gate binds the recorded deployment's digest and scope, refuses a foreign or cleaned-up deployment, and is atomic with the gate decision. `runtime_secret_route` proves only the pinned environment's secrets resolve, a second resolution for the same attempt is refused or idempotent as the secrets module specifies, and nothing is returned for another scope. Covers DQ-A04–DQ-A06, PX-06/PX-07 and SD-11.

## Out of scope
Process binding (T-101), changes to the delivery service or gate store, and the Kubernetes deployment path.
