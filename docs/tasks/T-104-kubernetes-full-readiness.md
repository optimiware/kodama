---
id: T-104
milestone: production
status: planned
depends_on: [T-089, T-105, T-103]
decision_blockers: []
owns: [test/e2e/kube-full/, test/integration/assembly-execution/, test/support/stage/, docs/acceptance/kube-full.md]
check: deploy/stage/scripts/gate.sh --gate readiness --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
resources: [go, integration]
acceptance: [kube_chart_install, kube_m1_roundtrip, kube_cancel_restart, kube_secrets_kms, execution_assembly_roundtrip, execution_assembly_cancel_restart, execution_assembly_result_guard]
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

## Check contract (scheduled 2026-10-24)
The qualified target is the Kodama-owned standalone stage cluster `kodama-stage-scw` (Scaleway Kapsule, `fr-par`, Kubernetes 1.37, Scaleway Key Manager) provisioned by `deploy/stage/scw`, not a kind scope: a live cloud KMS cannot be qualified on kind, and `KODAMA_ENV=production` refuses its fixtures. The check is the evidence-bound `readiness` gate, which runs `./test/e2e/kube-full/... ./test/integration/assembly-execution/...` with `go test -race -count=1` against the verified cluster endpoint and records provenance (commit, contract and values digests, cluster endpoint). `KODAMA_STAGE_CONTRACT` is the `stage_contract` output and `KODAMA_STAGE_VALUES` the deployed Helm values; the deployment, bootstrap and gate scripts live under `deploy/stage/` (owned by T-090) and are consumed unchanged. Missing cluster, credentials or images exit 75 (infrastructure pause), never a pass. Podman coverage of the `execution_assembly_*` suite is retained, not replaced.

**Recorded deviation ([O-27](../decisions.md#open-decisions-and-resolved-selections)).** On this stage cluster NATS is plaintext `nats://` with token authentication under chart `profile: development`; the server still runs `KODAMA_ENV=production`. Readiness and the assembled roundtrips are valid evidence for that configuration only. They do not qualify NATS TLS or a private CA; [T-106](T-106-production-tls-hardening.md) owns that after a reviewed design. The report must name the profile. The authority's issuer is the `oauthsonas` persona fixture behind an access code and ingress allowlist, so authentication strength is not qualified either.

## Out of scope
Deploying to Platform stage, hosted Platform integration, live-agent qualification or supported-release/recovery claims beyond the consumed T-029 evidence. Scheduling this task and its deferred prerequisites still requires explicit approval.

## Progress (2026-10-25)
Not complete; no acceptance has passed. The stage installation, the per-session qualification infrastructure (`deploy/stage/qualification`, `qualify.sh`) and the shared fixture `test/support/stage/` exist and were exercised live; the first real run found a chart RBAC defect (fixed on `task/stage-scw-deploy`, review requested). The suites, the Kubernetes backend of `assembly-execution`, the exact acceptance-to-package evidence map and the readiness receipt remain. Work state, decisions and next steps: [docs/acceptance/kube-full.md](../acceptance/kube-full.md#work-state-and-handoff-2026-10-25). Bounded ownership extension: `test/support/stage/` (shared with T-090, which depends on this task).
