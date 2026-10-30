---
id: T-176
milestone: kernel
status: planned
depends_on: [T-171]
decision_blockers: []
owns: [internal/runtime/process/]
check: make check-go PKGS='./internal/runtime/process/...'
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

## Out of scope
Podman, Kubernetes, composition wiring, and changing the legacy `execution.Runtime` consumers.
