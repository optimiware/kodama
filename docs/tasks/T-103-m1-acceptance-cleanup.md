---
id: T-103
milestone: M1
status: done
depends_on: [T-026, T-099, T-101]
decision_blockers: []
owns: [internal/app/, internal/results/, internal/execution/aggregate.go, internal/execution/values.go, test/integration/assembly-core/, docs/acceptance/m1.md]
check: DEV_STACK=services make check-go-integration PKGS="./internal/app/... ./internal/results/... ./test/integration/assembly-core/..."
resources: [go, integration]
acceptance: [core_spool_directories, result_output_failure_reason, core_assembly_boot, core_assembly_current_runtime]
---
# T-103 — Remaining M1 acceptance cleanup

## Goal
Resolve the three remaining O-23 findings before full Kubernetes/staging acceptance.

## Contract
The [M1 report](../acceptance/m1.md#findings) records fixture workarounds and stale checks. Recheck each against current main, then repair:

1. `NewCore` configures `<scratch>/artifact-spool` and `<scratch>/log-spool` without creating them. Initialize the required directories with appropriate access before use, support restart/idempotent construction, and report an unusable path without starting a broken service. Artifact/log completion must not require manual fixture setup.
2. `output_invalid` and `output_upload_failed` results lose their specific reason as `command_failed`. Preserve supported reasons through finalization/storage/API reads, while maintaining failure classification, output validation and diagnostic retention. Unknown or invalid reasons must not bypass validation.
3. `test/integration/assembly-core` still omits the required environment and assumes no dispatch/runtime routes. Update its fixtures and expectations for the actual `test` composition and Podman runtime, retaining its migration, readiness, authorization, replay and CLI coverage. Do not merely delete failing checks or replace real services with doubles.

T-099 already repaired artifact output naming; T-094 repaired workspace-free claim shape. Do not repeat those fixes. Preserve their evidence and update the historical M1 report with dated follow-up dispositions, not a retroactive claim that its original run tested new behavior. Preserve all production environment guards.

## Deliverables
Bounded initialization/classification fixes, focused unit regressions, repaired real-service assembly tests and updated finding dispositions. Use the isolated `services` stack. If a required contract or fixture change lies outside ownership, request a prerequisite rather than weakening the check.

## Acceptance
Go test names match the declared acceptance: `core_spool_directories` covers fresh/repeated startup and unusable spool paths; `result_output_failure_reason` verifies both specific failure reasons survive finalization and retrieval with diagnostics intact; retain `core_assembly_boot`; `core_assembly_current_runtime` proves current environment/runtime wiring and artifact/log use without manually creating spool directories. The declared integration command includes the changed packages' unit tests and real assembly tests in one results receipt. Missing services cannot count as passing acceptance.

## Out of scope
Production KMS, chart readiness, new output syntax, broad assembly redesign and optional blueprint/chassis work.

## Ownership extension (orchestrator)
The output-failure reason is lost in `internal/execution` (`Aggregate.CompleteAttempt` and `stepFailureReason` in `aggregate.go`, the reason constants in `values.go`). Under O-23 the owner scheduled this defect, so this task owns those two files for the ~10-line repair documented in `docs/acceptance/m1.md`; the committed `Test_result_output_failure_reason` must pass. Change nothing else in `internal/execution`.
