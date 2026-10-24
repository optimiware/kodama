---
id: T-076
milestone: M2
status: done
depends_on: [T-020, T-022]
decision_blockers: []
owns: [internal/execution/admission.go, internal/execution/context.go, internal/execution/service.go, internal/execution/values.go, internal/execution/environment.go, internal/execution/environment_test.go, internal/execution/executiontest/env.go, test/integration/environment-pinning-contract/, docs/implementation/environment-pinning.md]
check: make check-go PKGS="./internal/execution/... ./test/integration/environment-pinning-contract/..."
resources: [go]
---
# T-076 — Execution environment snapshot contract

## Goal
Publish and implement the execution-owned environment snapshot seam so T-035 can build independently against a concrete Go contract.

## Contract
Implement the [environment-pinning checkpoint](../implementation/environment-pinning.md): `EnvironmentResolver`, scoped immutable `EnvironmentSnapshot`, per-step snapshot identities, exact credential bindings and `BuildContext` population. Use the existing runtime wire fields and JSONB snapshot persistence. Preserve historical no-environment JSON/digests and the pure Job planner. `NewServiceWithEnvironments` explicitly injects this optional capability; ordinary service construction refuses environment declarations without disabling M1 runs.

Resolve a shared environment once per admission, check returned tenant/project/environment identity, and merge only unambiguous credential bindings into the existing secret-pin transaction. Missing/foreign/conflicting pins fail closed. No current-settings lookup occurs at dispatch. T-077 consumes T-035's real resolver and T-062's transaction seams to qualify snapshot retention/current revocation and deployment-bound cleanup before activation. Environment-declaring cleanup remains explicitly unavailable until that binding exists.

## Deliverables
Small execution-owned Go port, admission/snapshot/context implementation, compatibility fixtures and real PostgreSQL persistence/credential-rollback tests using a bounded environment-provider double. Leave active T-062 `ports.go`/adapter/PostgreSQL paths untouched. Export the provider contract for T-035; do not claim that a component double proves real delivery retention/revocation or cleanup.

## Acceptance
`environment_snapshot_contract`, `environment_snapshot_denials`, `environment_credential_conflicts`, `environment_runtime_context`, and `environment_snapshot_identity` verify the Go seam, scoped validation, missing-pin rejection, stable runtime/digest semantics and no-environment compatibility. `environment_admission_roundtrip`, `environment_credential_rollback`, `environment_alias_rejected`, and `environment_missing_resolver_and_scope` prove actual execution/secret persistence, changed-configuration/restart stability, exact credential pins, failed-admission rollback, refusal of ambiguous reference/version aliases, foreign-scope denial and explicit cleanup refusal with non-owner PostgreSQL roles. Together these supply component evidence for DQ-15, PX-01/PX-02/PX-05, VS-02/VS-03/VS-07 and SD-04/SD-10/SD-11. T-077 retains full real-delivery acceptance; T-067 repeats apply/reconcile/cleanup.

## Out of scope
A deployment/configuration framework, impure Planner, runtime wire/schema expansion, plaintext snapshots, root wiring, production activation, real delivery/cleanup qualification or modification of T-062's owned paths.
