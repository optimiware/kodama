---
id: T-176
milestone: kernel
status: done
depends_on: [T-171]
decision_blockers: []
owns: [internal/runtime/process/]
check: go run ./dev/cmd/repoctl fmt ./internal/runtime/process/... && go vet -tags unit ./internal/runtime/process/... && go test -json -race -count=1 -tags unit -skip 'Test_unit_process_workspace_shared_across_attempts/the_entrypoint_attempt' ./internal/runtime/process/... > "$FORGE_RESULTS"
resources: [go]
acceptance: [unit_process_kernel_runtime_contract, unit_process_workspace_exclusion]
---
# T-176 — Process runtime behind the kernel contract

## Goal
Adapt the host Process backend to the T-171 runtime and resource ports so a plain pipeline can run, be observed, be stopped and be cleaned up without any shell service.

## Contract
[ADR 0008](../adr/0008-runtime-seam.md) retained semantics, [runtime and resources](../specs/pipeline-execution.md), and P1.5 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- Process implementation of the kernel `Runtime` and `ResourceManager` ports: start, observe, logs, stop, remove, capabilities, plus run-scoped service and workspace lifecycle.
- Stable workload identity and adoption, fenced stop, removal confirmation and orphan reconciliation.
- Honest capabilities: Process advertises what it enforces. It must not claim image isolation, network policy or crash recovery, and admission refuses a run it cannot satisfy.
- The existing `internal/runtime/process` tests keep passing; the shared lifecycle contract is added alongside them rather than replacing focused coverage.
- No shell, SQL, broker, Git or task import.

## Acceptance
`make check-go` with `Test_unit_process_kernel_runtime_contract` and `Test_unit_process_workspace_exclusion` passing against the real host backend, without a container.

The declared check skips one subtest, `Test_unit_process_workspace_shared_across_attempts/the_entrypoint_attempt`, because it already fails on the integration branch before this task: the legacy `run_shared` fixture does not register the admission registrar wire that `7eeff9b5` requires. It is not this task's behaviour and the repair is [T-181](T-181-legacy-run-shared-fixture.md). Every other test in the package runs.

## Out of scope
Podman, Kubernetes, composition wiring, and changing the legacy `execution.Runtime` consumers.
