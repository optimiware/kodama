---
id: T-090
milestone: production
status: planned
depends_on: [T-104]
decision_blockers: []
owns: [deploy/stage/, test/e2e/staging/, docs/operations/staging.md]
check: deploy/stage/scripts/gate.sh --gate staging --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
resources: [go, integration]
acceptance: [stage_clean_install, stage_e2e_roundtrip, stage_restart_reconcile, stage_loki_log_search, stage_dev_adapter_refused, stage_retention_uninstall]
---
# T-090 — Trusted-engineer staging deployment

## Goal
Run the qualified appliance on Platform's stage cluster, the first staging target ([C-19](../decisions.md#confirmed-direction)), and record what that proves.

## Contract
Use `KODAMA_ENV=production` with the standalone authority, a separate Kodama control-plane namespace and release, scoped execution namespaces, distinct database roles, storage prefixes, service accounts and KMS permissions, and the cloud KMS of the installation. Regular Kubernetes containers with the existing restricted baseline are the trust model (O-07, P-14); name it in the handoff. The environment refuses every development and test adapter; the development keyfile is never a cloud KMS.

Qualify only what is deployed: the chart and images with T-089 component evidence and T-104 full-readiness evidence, the migration sequence under a database lock, readiness distinguishing dependencies, and one end-to-end run (submit a pipeline, retain logs and artifacts, cancel, restart the server and reconcile). O-24's component checkpoint alone cannot authorize staging; T-104 also requires O-23 cleanup and T-105 production startup wiring under O-26, and qualifies live KMS during deployment. A standalone smoke deployment may precede hosted qualification; do not relabel it as hosted integration (T-061).

## Deliverables
Stage values and an install/upgrade/uninstall runbook with exact commands, the stage handoff (cluster and context, namespaces, image and chart revisions, KMS and authority profile, trust model), and a bounded smoke suite with its report.

## Acceptance
`stage_clean_install` installs from a clean namespace set; `stage_e2e_roundtrip` submits a pipeline and retains logs and artifacts; `stage_restart_reconcile` cancels, restarts the server and reconciles; `stage_loki_log_search` finds pipeline and control-plane logs by `attempt_id` in Loki through the operator-authenticated Grafana; `stage_dev_adapter_refused` proves the deployed configuration refuses every development and test adapter; `stage_retention_uninstall` uninstalls and shows databases, buckets and KMS keys retained. The `staging` gate requires a passing, digest-verified `readiness` (T-104) receipt for the same commit, contract and values, and the suite passes only with complete provenance.

## Check contract (scheduled 2026-10-24)
The first stage target is the Kodama-owned standalone cluster `kodama-stage-scw` (`deploy/stage/scw`, Kubernetes 1.37). It is a standalone smoke deployment and is not relabelled as Platform stage or hosted integration (T-061). Its Scaleway database has scheduled backups only (no 30-day PITR), which this gate records and T-029/T-056 refuse. The task owns `deploy/stage/` (OpenTofu roots, shared stack, bootstrap/deploy/gate/evidence scripts); T-104 consumes it unchanged.

**Recorded deviation (O-27).** The stage's NATS is plaintext with token authentication (chart `profile: development`, `KODAMA_ENV=production`); the stage handoff must state this and may not claim NATS TLS. [T-106](T-106-production-tls-hardening.md) tracks its removal. The issuer is the `oauthsonas` persona fixture; the handoff must say so.

## Out of scope
Production release and support claims (T-056), hosted Platform authority (T-061), measured capacity, recovery drills (T-029) and stronger isolation (T-055).

## Progress (2026-10-25)
Not started beyond the infrastructure it owns: `deploy/stage/` includes the qualification root and `qualify.sh` (isolated per-session installations used by both gates), the stage deploy with the `oauthsonas` issuer, and `scripts/verify_image_revision.py` (the gate refuses images whose registry copy is not labelled with the candidate commit). `test/e2e/staging/`, the Alloy attempt-ID normalization needed by `stage_loki_log_search` and `docs/operations/staging.md` remain.

The `staging` gate requires a passing T-104 `readiness` receipt for the **same commit, contract and values**. The T-104 functional pass at `5aee4af54e2e` used a helper image later found to be mislabelled and a product image with a zero-byte log-upload defect; its requalified receipt, not that one, is the prerequisite (see [the T-104 report](../acceptance/kube-full.md#superseded-readiness-receipt-candidate-5aee4af54e2e-2026-10-25)). Carry the T-104 limitations into the stage handoff: O-27, scheduled-only backups, environment-bound (not direct) secret delivery ([T-107](T-107-direct-secret-delivery.md)), and the teardown gap: `qualify.sh destroy` swallows Helm/namespace deletion failures and cannot reconcile state that names a database that is already gone.
