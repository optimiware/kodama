---
id: T-102
milestone: M1
status: done
depends_on: [T-086, T-095]
decision_blockers: []
owns: [internal/execution/runtime.go, internal/execution/runtime_test.go, internal/runtime/runtimetest/, internal/runtime/process/, internal/runtime/podman/, internal/kube/]
check: make check-go PKGS="./internal/execution/... ./internal/runtime/... ./internal/kube/..."
resources: [go]
acceptance: [runtime_never_started_capability, runtime_stop_unknown_preserved, runtime_stop_no_relaunch]
---
# T-102 — Runtime launch-evidence capability

## Goal
Implement owner-selected O-25 so the shared runtime contract tests only launch-history guarantees a backend can prove, unblocking T-089.

## Contract
Follow [ADR 0008](../adr/0008-runtime-seam.md#launch-evidence--o-25) and [pipeline execution](../specs/pipeline-execution.md#scheduling-and-runtime). The shared `runtimetest.Contract` currently requires `LaunchNotStarted` after stopping a fresh workload. Kubernetes cannot prove this from absent Secret/Job/Pod records: an issued create may have had an unknown outcome and lost its Secret.

Declare an explicit backend capability for affirmative never-started proof. Kubernetes does not advertise it; Process and Podman retain the stronger declaration only within their proven contract. Gate only the extra proof assertion: every backend must still stop/fence the workload, reject a subsequent `Start`, preserve known launch evidence and protect foreign instances. A non-advertising backend may return `LaunchNotStarted` when concrete evidence proves it; missing records alone must remain `LaunchUnknown`.

Keep launch-history proof distinct from mandatory container/admission guarantees: do not accidentally make Kubernetes ineligible for the production policy or change existing run snapshots. Preserve `StopEvidence.Stopped` semantics, uncertain-effect reconciliation, and writer/capacity fencing. Do not replace the assertion with a skipped contract case or a permissive fake. No durable fence object, RBAC/chart expansion or lost-Secret relaxation is authorized.

## Deliverables
Capability declarations, the conditional shared assertion and named regression tests. Preserve T-095's stopped/removed `Start` conflict fix and the existing lost-Secret/in-flight-create tests. Update owned runtime README notes where needed. Changes required outside these paths return to the coordinator as a bounded prerequisite.

## Acceptance
Use Go test names matching the declared acceptance: `runtime_never_started_capability` proves declarations and both branches of the shared assertion; `runtime_stop_unknown_preserved` proves lost records or uncertain creation never become affirmative never-started evidence; `runtime_stop_no_relaunch` proves stopping before start still fences future launch, regardless of the optional proof capability. These cover PX-A12 (PX-07/PX-08/PX-15/PX-16). Run the existing Process and fake-client runtime tests in the unit gate. T-089 runs the same contract against real Kubernetes; Podman's real-runtime evidence remains separately required at its integration checkpoint.

## Out of scope
Installed-server readiness, product retry-policy changes, production KMS and extra runtime backends.
