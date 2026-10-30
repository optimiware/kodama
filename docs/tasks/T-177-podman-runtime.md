---
id: T-177
milestone: kernel
status: planned
depends_on: [T-171]
decision_blockers: []
owns: [internal/runtime/podman/]
check: go run ./dev/cmd/repoctl fmt ./internal/runtime/podman/... && go vet -tags unit ./internal/runtime/podman/... && go test -json -race -count=1 -tags unit -skip 'Test_podman_run_resources_shared_attempt' ./internal/runtime/podman/... > "$FORGE_RESULTS"
resources: [go, integration]
acceptance: [unit_podman_kernel_runtime_contract, unit_podman_adoption_and_cleanup]
---
# T-177 — Podman runtime behind the kernel contract

## Goal
Adapt the Podman backend to the T-171 ports and prove real container lifecycle: adoption after a lost response, fenced stop, service readiness and confirmed removal.

## Contract
[ADR 0008](../adr/0008-runtime-seam.md), [run workspace and builders](../implementation/run-workspace-services-builders.md), and P1.5 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- Podman implementation of the kernel `Runtime` and `ResourceManager` ports over the existing qualified behaviour: pinned image, non-root execution, limits, durable identity, adoption, stop fencing, removal confirmation, run-scoped services and workspace preparation.
- Uncertain creation and uncertain stop are reconciled from observable state, never replayed.
- Capabilities declared honestly; admission refuses a run the backend cannot satisfy.
- The shared kernel lifecycle contract runs against the real engine (`KODAMA_REQUIRE_PODMAN=1`), and the existing Podman tests keep passing.

## Acceptance
`make check-go` with `Test_unit_podman_kernel_runtime_contract` and `Test_unit_podman_adoption_and_cleanup` passing against the real engine. A skipped engine is not evidence.

The declared check skips `Test_podman_run_resources_shared_attempt`, which already fails on the integration branch before this task from the same legacy `run_shared` fixture gap as [T-181](T-181-legacy-run-shared-fixture.md). Every other test in the package runs.

## Out of scope
Kubernetes, composition wiring, builder/BuildKit work, and changing legacy `execution.Runtime` consumers.
