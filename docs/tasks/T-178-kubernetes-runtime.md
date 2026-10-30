---
id: T-178
milestone: kernel
status: planned
depends_on: [T-171]
decision_blockers: []
owns: [internal/kube/]
check: make check-go PKGS='./internal/kube/...'
resources: [go, integration]
acceptance: [unit_kube_kernel_runtime_contract, unit_kube_restricted_workload_and_cleanup]
---
# T-178 — Kubernetes runtime behind the kernel contract

## Goal
Adapt the Kubernetes backend to the T-171 ports without importing the legacy execution, gate or workspace packages.

## Contract
[ADR 0001](../adr/0001-native-kubernetes-jobs.md), [ADR 0010](../adr/0010-run-workspace-services-builders.md) retained run-resource semantics, and P1.5 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- Kubernetes implementation of the kernel `Runtime` and `ResourceManager` ports: Job and service lifecycle, workspace PVC, readiness, stop, removal and capabilities.
- Preserve committed-intent rereads, UID checks, restricted workload policy, orphan reconciliation and workspace release evidence.
- Port retained behaviour; do not keep the legacy execution, gate or workspace dependency chain.
- Capabilities declared honestly; admission refuses a run the cluster policy cannot satisfy.
- The shared kernel lifecycle contract runs against the kind-backed conformance profile.

## Acceptance
`make check-go` with `Test_unit_kube_kernel_runtime_contract` and `Test_unit_kube_restricted_workload_and_cleanup` passing. Fake-client tests may prove mapping but cannot claim CSI, CNI or quota behaviour; real conformance stays with the P3 qualification.

## Out of scope
Chart changes, staging installation, and composition wiring.
