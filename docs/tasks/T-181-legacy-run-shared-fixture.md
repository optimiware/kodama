---
id: T-181
milestone: kernel
status: done
depends_on: [T-176]
decision_blockers: []
owns: [internal/runtime/process/resources_test.go, internal/runtime/process/process.go, internal/runtime/runtimetest/, internal/runtime/bootstrap/, internal/runtime/podman/podman.go, internal/runtime/podman/resources_test.go, internal/runtime/podman/checkout_test.go]
check: make check-go PKGS='./internal/runtime/process/... ./internal/runtime/runtimetest/... ./internal/runtime/bootstrap/... ./internal/runtime/podman/...'
resources: [go]
acceptance: [unit_process_workspace_shared_across_attempts, podman_run_resources_shared_attempt]
---
# T-181 — Repair the legacy run_shared fixture

## Goal
Restore the pre-existing `run_shared` test fixtures so the package suites and the repository batch are green again, without changing product behaviour.

## Contract
The runtime seam in [ADR 0008](../adr/0008-runtime-seam.md) and the run-resource work in [run workspace and builders](../implementation/run-workspace-services-builders.md). This is test-fixture repair, not a feature.

## Background
`7eeff9b5` made the bootstrap issuer require a registrar that serves the `run_shared` wire. The legacy fixtures in `internal/runtime/process/resources_test.go`, `internal/runtime/runtimetest` and `internal/runtime/bootstrap` do not provide it, so:

- `Test_unit_process_workspace_shared_across_attempts/the_entrypoint_attempt_selects_the_v2_wire_in_the_shared_workspace_without_a_checkout_credential` fails with `execution: not configured: the admission registrar does not serve the run_shared wire`;
- `Test_podman_run_resources_shared_attempt` fails from the same cause;
- `make test-go-unit` is red on the integration branch before this workstream's changes.

## Deliverables
- Provide the `RegisterV2` behaviour the fixtures need, or retire the obsolete fixture with an explicit rationale and an equivalent regression that still covers the shared-workspace v2 wire.
- If a second failure appears behind the first (for example an unreadable attempt context from `DecodeContextV2` or `runtimetest.SharedSpec`), fix the fixture rather than weakening the assertion.
- Do not change production code outside the owned paths. If the real cause is in `internal/execution`, stop and report it as a blocker with the exact failing path.

## Acceptance
Both named tests pass, and `make check-go` over the three packages is green. Report the exact command and result.

## Out of scope
Changing the runtime seam, the admission protocol, or any behaviour the fixtures exercise.
