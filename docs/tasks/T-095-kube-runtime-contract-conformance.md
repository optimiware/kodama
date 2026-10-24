---
id: T-095
milestone: M1
status: done
depends_on: [T-021, T-086]
decision_blockers: []
owns: [internal/kube/, test/integration/kube/]
check: make check-go PKGS="./internal/kube/..."
resources: [go]
acceptance: [kube_start_stopped_conflict]
---
# T-095 — Kubernetes runtime contract conformance

## Goal
Make the Kubernetes runtime answer the shared runtime contract exactly as the Process and Podman runtimes do, so T-089 can run `runtimetest.Contract` against `kube` unchanged.

## Contract
T-089's first run of the shared contract on the real Kubernetes runtime found two deviations in `internal/kube` (T-021/T-086): (1) `Start` of a stopped or removed workload returns `kube.ErrNotDispatchable`, where the contract requires `execution.ErrConflict`; (2) `Stop` before any create reports `LaunchUnknown`, where the contract requires `LaunchNotStarted`. Repair (1) is this task. Repair (2) was found unprovable on Kubernetes without a new durable fence object or a relaxed expectation, so it is owner decision O-25 and is out of this task.

Repair the adapter to the contract, not the contract to the adapter. A stop of a workload that was never created is `LaunchNotStarted` only when the adapter can prove nothing was created (no create request was issued for the attempt and no record or Job exists under its identity); a create that may be in flight or whose answer was lost stays `LaunchUnknown`, as the existing recovery tests require. If proving this is not possible on Kubernetes without weakening those tests, stop and report it as a blocker with the evidence; do not edit `runtimetest` or the contract.

## Deliverables
The two repairs in `internal/kube/`, unit tests with the Kubernetes fake client, and an update to `internal/kube/README.md` where the behaviour is described. Run the shared contract against the fake-backed adapter if the package already does so and keep it green. Change nothing outside the owned paths.

## Acceptance
`kube_start_stopped_conflict` proves a `Start` of a stopped or removed workload returns an error satisfying `errors.Is(err, execution.ErrConflict)` and still names the cause. The Stop cases already in the branch (provable no-create, in-flight create, lost answer, lost record) stay as regression coverage but are not acceptance. Keep the T-021 and T-086 acceptance passing.

## Out of scope
The installed chart and production key wiring (T-029/T-056), the Kubernetes e2e suite (T-089), and any change to the shared runtime contract.
