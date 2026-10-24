---
id: T-089
milestone: M1
status: done
depends_on: [T-026, T-018, T-021, T-064, T-086, T-095, T-102]
decision_blockers: []
owns: [test/e2e/kube/, docs/acceptance/kube.md]
check: make check-go-integration PKGS=./test/e2e/kube/...
resources: [go, integration]
acceptance: [kube_runtime_contract, kube_component_roundtrip, kube_component_cancel_restart, kube_restricted_baseline, kube_chart_fail_closed]
---
# T-089 — Kubernetes runtime and chart conformance

## Goal
Prove on kind the Kubernetes runtime, restricted baseline and chart component checkpoint approved in O-24, after M1 has passed on Podman.

## Contract
Use the `cluster` stack and the real Kubernetes runtime. Run `runtimetest.Contract` with T-102's O-25 capability-aware launch-evidence assertion. Retain real Job lifecycle/log roundtrips, adoption after lost create acknowledgment, cancellation during create and runtime restart, restricted Pods, tenant NetworkPolicy and quota denial. Component fixtures must identify injected control-plane/callback dependencies; they cannot claim assembled-server acceptance.

Install the real chart with `KODAMA_ENV=production`, verifying rendered/applied resources and the restricted baseline. For missing production keys or invalid dependency configuration, require the specific fail-closed configuration error and an unready server; arbitrary crashes, image-pull failures or unrelated installation errors do not pass. Never enable a development key or weaken TLS/environment guards to make this checkpoint start.

O-24 transfers the full `kube_m1_roundtrip`, `kube_cancel_restart` and `kube_chart_install` acceptance to [T-104](T-104-kubernetes-full-readiness.md), including T-026's three backend-parameterized `execution_assembly_*` scenarios. Their original names retain the full obligations. Component checkpoint completion is not full Kubernetes M1 closure, stage deployment, cloud KMS, hosted or production qualification.

## Deliverables
A component suite under `test/e2e/kube/`, the chart installation/refusal check, and an acceptance report recording kind/chart versions, stack provenance, injected dependencies and the outstanding T-104 acceptance. Product bugs return to their owners.

## Acceptance
`kube_runtime_contract` runs the shared contract on real `kube`, including the explicit launch-history capability without skipping the stop/fencing case. `kube_component_roundtrip` verifies actual Job execution, observation and logs. `kube_component_cancel_restart` verifies lost-create adoption, cancellation during create and runtime restart (component evidence for PX-A02/PX-A06). `kube_restricted_baseline` verifies privileged Pods and platform data access are denied, bounded storage and enforced default-deny network (IT-A06). `kube_chart_fail_closed` installs/renders/applies the chart from a clean scope and observes the expected configuration refusal without readiness or executable admission. Use matching Go test names. A skipped test or missing cluster fails; it never passes.

## Out of scope
Changing T-026's acceptance, full installed-server readiness/assembled roundtrips (T-104), the Podman runtime, stage deployment, cloud KMS and untrusted-execution isolation.
