---
id: T-115
milestone: production
status: planned
depends_on: [T-112, T-122]
decision_blockers: []
owns: [internal/runtime/podman/, internal/runtime/process/, internal/runtime/runtimetest/]
check: make check-go PKGS='./internal/runtime/podman/... ./internal/runtime/process/... ./internal/runtime/runtimetest/...'
resources: [go]
acceptance: [unit_run_resources_contract_process, unit_process_workspace_shared_across_attempts, unit_podman_run_resources_program, unit_local_capabilities_refuse_unenforced]
---
# T-115 — P5: Podman and Process run resources

## Goal
Implement [P5](../implementation/run-workspace-services-builders.md#p5--podman-parity-and-process-fixtures) with an exported backend-neutral companion-port contract suite. Under O-30 Kubernetes is the delivery backend; local backends give lifecycle evidence and refuse what they do not enforce.

## Contract
ADR 0010 as amended by O-30, seams S1, S2 and S4. Process advertises no image/network/builder member and lacks `network_policy`, so admission refuses `run_shared` on it; its adapter is exercised directly by the contract suite. Podman advertises only enforced, real-tested members.

## Deliverables
- `runtimetest.RunResourcesContract(t, factory)`: idempotent ensure/adopt, UID binding, collision refusal, fenced restart, unknown-never-absent, logs denial for workspace/network, stop/release/remove evidence, restart adoption.
- Process: run-owned temporary workspace shared across attempts (S2), explicit host-program services and preparation fixtures, idempotent cleanup.
- Podman: run-owned volume, run network, service containers with readiness, attempt `/workspace` from the run volume for `AttemptSpec.Shared`, `KODAMA_RUNTIME_API=v2`, idempotent cleanup; no host socket.

## Acceptance
Declared check with every named test passing; `make test-podman` extended with a real multi-image volume/service roundtrip where Podman is available. Local evidence never certifies Kubernetes.

## Out of scope
Shared bootstrap/entrypoint (T-113), execution, chart and assembly.
