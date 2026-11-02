---
id: T-115
milestone: production
status: done
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

## Completion evidence
Implemented on `task/T-115-local-run-resources` (`de2c9c6e`…`39ae0dad`). Independent review (`ses_e76682a18ffebzlByAciMUQjVK`) requested changes six times, each round narrowing the evidence the local adapters may claim: preparation as a workspace holder, launch identity recorded before a workload may run (Process launch gate), create-once workspaces pinned by filesystem identity, credential cleanup independent of launch, stop intent versus verified stop, an enforced per-container user namespace on Podman, identity-keeping tombstones, one create in flight per name with the answer bound atomically to its object, no elapsed-time or uptime inference, no automatic placeholders (an ambiguous create stays unresolved until observed or explicitly retired by an operator, scoped by retirement epochs), and fail-closed epoch reads. It approved `39ae0dad`. Coordinator checks on the final candidate: `make check-go PKGS='./internal/runtime/podman/... ./internal/runtime/process/... ./internal/runtime/runtimetest/...'` exit 0 with all four pinned acceptances PASS; real `make test-podman` exit 0 (174s) including the exported contract suite over the real engine and the multi-image roundtrip. Local lifecycle evidence only: admission still refuses `run_shared` on Process and Podman (no `network_policy`). Composition in `development`/`test` is T-134. Squashed to one commit; nothing pushed.
