---
id: T-191
milestone: kernel
status: done
depends_on: [T-186, T-187]
decision_blockers: []
owns: [internal/shell/sources/, internal/shell/secrets/, internal/shell/serve/runsources.go, internal/shell/serve/runsources_test.go]
check: make check-go PKGS='./internal/shell/sources/... ./internal/shell/secrets/... ./internal/shell/serve/...'
resources: [go]
acceptance: [unit_shell_source_preparation, unit_shell_secret_revocation]
---
# T-191 — Shell source preparation and secret custody adapters

## Goal
Give the kernel real shell-owned implementations of `WorkspacePreparer` and `SecretResolver`, so a run can be prepared from pinned Git sources with scoped credentials and secrets can be delivered and revoked durably.

## Contract
`internal/kernel/runtime` (`WorkspacePreparer`, `SecretResolver`, `ResolveRequest`, `MaterializationRef`, `ErrMaterializationRevoked`), the resolver conformance suite in `test/support/kernelcontracts`, [variables and secrets](../specs/variables-and-secrets.md), [Git repositories](../specs/git-repositories.md) (run-scoped credentials), and P2.5 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- **Source preparation** in `internal/shell/sources`: prepare or adopt a run workspace from pinned inputs — exact commits, context repositories, and run-scoped Git credentials that are revoked with the run. Source-free runs remain supported. The kernel learns nothing about Git.
- **Secret custody** in `internal/shell/secrets`: resolve declared references into a private materialization outside `/workspace`, and implement `Revoke` durably so that a revoked identity can never be resolved again — the guarantee T-186 placed in the resolver contract. Rotation and revocation must be observable and must never leak values into logs, events or retained metadata.
- Move the run-source wiring out of `internal/shell/serve` into these packages and keep the composition thin.
- Both adapters must pass the shared contract suites: the resolver conformance suite for secrets, and the kernel workspace-preparation contract for sources.

## Acceptance
`make check-go` with `Test_unit_shell_source_preparation` and `Test_unit_shell_secret_revocation` passing, including the resolver conformance suite against the real custody adapter. A test that only records calls is not evidence; material must really be created and removed.

## Out of scope
The kernel ports, the PostgreSQL state adapter, the HTTP/MCP/CLI surfaces, and knowledge-base repository conventions beyond pinned checkout.
