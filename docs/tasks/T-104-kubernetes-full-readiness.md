---
id: T-104
milestone: production
status: deferred
depends_on: [T-089, T-105, T-103]
decision_blockers: []
owns: [test/e2e/kube-full/, test/integration/assembly-execution/, docs/acceptance/kube-full.md]
check: DEV_STACK=cluster make check-go-integration PKGS="./test/e2e/kube-full/... ./test/integration/assembly-execution/..."
resources: [go, integration]
acceptance: [kube_chart_install, kube_m1_roundtrip, kube_cancel_restart, kube_secrets_kms]
---
# T-104 — Full Kubernetes installation readiness

## Goal
Retain the full installed-server and assembled Kubernetes acceptance transferred from T-089 by O-24, before T-090 staging.

## Contract
T-089's component checkpoint cannot prove a ready production installation. After T-105 supplies secrets startup wiring, run the actual chart/server with `KODAMA_ENV=production`, the explicitly selected standalone authority and valid TLS dependencies. Supply trusted HTTPS fixtures and a supported real cloud KMS configuration; the development keyfile, plain-HTTP shortcuts and injected permissive adapters are not production qualification. Under O-26 this task owns live-provider validation during Kubernetes deployment.

Run T-026's `execution_assembly_roundtrip`, `execution_assembly_cancel_restart` and `execution_assembly_result_guard` against Kubernetes through the existing backend-parameterized suite. Retain Podman coverage. Prove source-consuming checkout/publication, retained logs/artifacts, tenant/result guards, lost-create adoption, cancellation and restart with the real assembled server. Do not rewrite these scenarios as runtime-only simulations.

## Deliverables
Full chart readiness check, backend-parameterized assembled roundtrips and a report naming chart/image/kind versions, TLS and cloud-provider provenance and limitations. Product bugs go to their owners. T-105 owns startup wiring; T-029 retains full recovery ownership. Assign a bounded prerequisite for shared HTTPS fixture work. Recheck the check contract and installation configuration when this deferred task is activated.

## Acceptance
`kube_secrets_kms` runs the [real-provider procedure](../../test/integration/kms-production/README.md#real-provider-procedure-per-cloud) with the deployed workload identity: actual secret use, restart/decryption, new-current/retained-key rotation, controlled denial/outage and restored access. Record the cloud actually qualified. A local startup test alone does not satisfy it.

`kube_chart_install` retains T-089's original obligation: install the chart from a clean scope and reach actual readiness with supported configuration; expected startup refusal cannot satisfy it. `kube_m1_roundtrip` covers the assembled entrypoint/source/result/log path and result guards; `kube_cancel_restart` covers assembled cancellation, replay and recovery, retaining PX-A02/PX-A06/IT-A05. Include original `execution_assembly_*` evidence in the report. Missing credentials/services/cluster, skipped cases or zero matching tests never establish acceptance.

## Out of scope
Deploying to Platform stage, hosted Platform integration, live-agent qualification or supported-release/recovery claims beyond the consumed T-029 evidence. Scheduling this task and its deferred prerequisites still requires explicit approval.
