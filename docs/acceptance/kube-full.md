# Full Kubernetes installation readiness (T-104) — running report

**Status: in progress, not qualified.** Six of the seven readiness acceptances have passed live, individually or inside one gate run. The readiness gate has not passed: `kube_secrets_kms` failed in that run (Key Manager HTTP 409, throwaway key name already taken), so `suite_passed` is false and the receipt was not issued. This file is the handoff, not a qualification claim.

## Target and provenance

| Item | Value |
| --- | --- |
| Cluster | Scaleway Kapsule `kodama-stage-scw`, `fr-par`, Kubernetes 1.37.1, Cilium, 4–8 `DEV1-L` workers |
| Authority / environment | Standalone authority, `KODAMA_ENV=production` (set by the chart) |
| Server and workload images | Built from this checkout, published digest-pinned to a public-read Scaleway registry namespace (the CI image on ghcr is private) |
| Database | Scaleway Managed PostgreSQL 17; roles created through the platform API; scheduled backups only (no 30-day PITR) |
| Secret encryption | Scaleway Key Manager (`scw` provider); the startup probe passes against the live service |
| Ingress certificates | Let's Encrypt via cert-manager (HTTP-01) on `nip.io` hostnames |
| Deployment | `deploy/stage` (OpenTofu); state outside the checkout |

## Recorded deviations (O-27)

- **NATS is plaintext** `nats://` with token authentication under chart `profile: development`; the server's NATS and OIDC clients trust only the system CA store, so no private CA is used (T-106 owns that design). Nothing here qualifies NATS TLS.
- **The issuer is the `oauthsonas` persona fixture**, behind an access code and an ingress allowlist on its token paths, with PROXY protocol so the ingress sees real client addresses. It is a development/test fixture, not an identity system.

## Findings from the first installation

| # | Finding | Disposition |
| --- | --- | --- |
| 1 | The tenant-namespace Role granted Secrets `create, get, delete` but the server's reconciler lists managed Secrets, so every reconcile pass failed with HTTP 403 on a real install. Earlier evidence ran under administrator credentials. | Fixed in `deploy/helm/kodama/templates/rbac.yaml` with regression test `Test_helm_secret_management_rbac`; owner review requested. |
| 2 | The server's NATS client and its OIDC client use only the system trust store; `config.tls.customCA` covers Git only, so a private CA cannot secure NATS or the issuer. | Open, tracked by T-106; stage deviation O-27. |
| 3 | Dex access tokens carry `at_hash`, which the server's verifier rejects (an ID-token claim); `AcceptIDTokenClaims` is not exposed as configuration. | Worked around by using `oauthsonas`, whose tokens carry neither `at_hash` nor `nonce`. |
| 4 | Observed once: a long-running server answered `unavailable` for valid tokens while the ingress and load balancer were being switched to PROXY protocol, and a fresh process verified the same token. Retested afterwards: restarting the issuer (new signing key) leaves the running server healthy; the new token returns 200 and the old one 401. | Not reproducible; attributed to the transient load-balancer change. No product defect recorded. |
| 5 | Scaleway object storage: `HeadBucket` (used by the readiness probe) needs the bucket-read permission set, not only object permissions. | Fixed in the stage IAM policy. |
| 6 | NATS `$VAR` interpolation: a quoted token in the rendered JSON config makes the literal text the token. | Fixed in the stage NATS values (`<< >>` unquoted form). |
| 7 | Scaleway's managed admin user cannot grant on the platform-owned database. | Roles and privileges are created through the platform API. |
| 8 | The server's cluster-scoped Role could not create `SubjectAccessReview`s, which the tenant preflight posts before every dispatch (it asks the API server what the tenant's execution service account may do). On a real chart installation every dispatch therefore failed with an unclassified `unavailable` and no Job was ever created; the run stayed `queued`. Found by the first real run on a qualification session (2026-10-25). | Fixed in `deploy/helm/kodama/templates/rbac.yaml` (`authorization.k8s.io` `subjectaccessreviews` `create`, nothing else) with regression test `Test_helm_preflight_access_review_rbac`; owner review requested together with finding 1. Not yet rolled out to a live session. |
| 9 | The server logs an unclassified `unavailable` for a refused dispatch (`execution: dispatch error=unavailable`) without naming the failing dependency or call. Finding 8 needed code reading to diagnose. | Observation for the execution owner: log the classified cause (preflight reason, API operation and status). |
| 10 | The tenant Role granted Secrets `create, get, list, delete` but not `update`. The workload record is an annotation on the admission Secret, replaced with PUT as the attempt advances. Every dispatch died `unavailable` after creating that Secret. | Fixed in `deploy/helm/kodama/templates/rbac.yaml`; `Test_helm_secret_management_rbac` now requires `update`. |
| 11 | Cilium with kube-proxy replacement does not honour a ClusterIP `ipBlock` for CoreDNS. Workloads could not resolve the callback hostname and died at the execution deadline. | Chart always renders a kube-dns pod-selector egress (port 53). `internal/kube/preflight.go` allows only that peer. |
| 12 | `customCA: false` left `KODAMA_GIT_CA_FILE` unset. Source checkout refuses an empty Git CA, so every reader/writer dispatch was `unavailable` (logged that way even when the cause is `ErrCheckoutDenied`). | Chart sets `KODAMA_GIT_CA_FILE` to `/secrets/runtime-ca.crt` unless a private CA is configured. Stage `runtime-ca.crt` is the ISRG roots. |
| 13 | The workspace volume is owned by the kubelet, not uid 65532. Git refuses the checkout (`dubious ownership in repository at '/workspace'`). Restricted Pod Security forbids a chown. | Entrypoint git sets `safe.directory=*`; the step environment sets `safe.directory` to the workspace unless the step already configures git. Requires the image built after this change. |
| 14 | PX-A02 cannot use the planned loopback HTTP sidecar. `logs/kubernetes` refuses a non-HTTPS API endpoint, and that refusal crashes the server (`core assembly failed to start`). A sidecar was applied once and rolled back by deleting the Deployment and upgrading the chart. | Not qualified. Installed evidence is crash-restart adoption of a live Job, not a lost-create acknowledgment. Do not reapply an HTTP proxy sidecar. |

## Work state and handoff (2026-10-25)

Branch `task/T-104-kube-full` (worktree `.work/T-104-kube-full`). Not pushed. Not reviewed. Not merged.

### What passed live (session `kb`, 2026-10-25)

One readiness gate run (`gate.sh --gate readiness --cloud scw`, session contract and values) executed the suites. Results:

| Acceptance | Result in that run |
| --- | --- |
| `kube_chart_install` | passed (`TestKubeChartInstall`) |
| `kube_m1_roundtrip` | passed (`TestKubeM1Roundtrip`) |
| `kube_cancel_restart` | passed (`TestKubeCancelRestart`, including crash-restart adoption) |
| `kube_secrets_kms` | **failed**: `create throwaway key: HTTP 409` (`t028-a-kb` / `t028-b-kb` already exist). This failed the package, so the gate rejected the run. |
| `execution_assembly_roundtrip` | passed (`TestExecutionAssemblyRoundtrip/kube`) |
| `execution_assembly_cancel_restart` | passed (`TestExecutionAssemblyCancelRestart/kube`) |
| `execution_assembly_result_guard` | passed (`TestExecutionAssemblyResultGuard/kube`) |

`TestM1PostgresContractSuites` was skipped on purpose (`KODAMA_ASSEMBLY_BACKEND=kube`). That skip is not an acceptance. Podman was not re-run in this session. `make lint`, `make test`, and `make docs-check` were not run. `deploy/stage/scripts/test_scripts.sh` passed (needs `shellcheck` on `PATH`; a copy lives in `/tmp/opencode/kodama-stage-tools`).

Do not treat the six passes as a gate receipt. The gate requires all seven in one run with `suite_passed=true`.

### Session still live — do not destroy until the gate passes or the owner says so

| Item | Value |
| --- | --- |
| Session | `kb` (`~/kodama-stage/state/qualification/kb`) |
| URL | `https://kb.195-154-68-111.nip.io` |
| Namespaces | `kodama-q-kb`, `kodama-q-kb-a`, `kodama-q-kb-b` |
| Kubeconfig | `~/kodama-stage/scw.yaml` (not `~/.kube/kodama-stage/`) |
| State dir | `~/kodama-stage/state` (`STAGE_STATE_DIR` / `KODAMA_QUAL_STATE_DIR`) |
| Contract / values the gate should use | `~/kodama-stage/state/qualification/kb/contract.json` and `values.yaml` |
| Image under test | `rg.fr-par.scw.cloud/kodama-stage-scw/kodama:0.0.2-stage-b0c4fdca4c0c-3@sha256:30d79c1d43ff87ea60fbd38bbe89904de755efee165d7a3e78ce8fd6ba595f04` |
| Workload image | unchanged `sha256:24cff5ad7765e56c3242014e64e2d911ca02053b92ab9c14f2beeb6519d929ea` |
| Tools | `/tmp/opencode/kodama-stage-tools` (`tofu` 1.13.2, `helm` 4.3.1, `shellcheck` 0.10.0) |
| Issuer allowlist | live annotation on `kodama-deps/kodama-oauthsonas-restricted` includes `151.115.90.46/32` plus the three addresses from the stage deploy. A later stack apply can drop `151.115.90.46`. |
| Operator API | kubeconfig token is a Scaleway secret key (`SCW_SECRET_KEY`). Access key for the provider is in `/tmp/opencode/scw-access-key` (do not print it). Project `943031a5-cc3c-4202-aea1-697e4e55863c`. |

Session `ka` is gone. Do not reapply the HTTP kube-proxy sidecar. A graceful `scale` to 0 can reconcile a running Job away; adoption tests crash the server pod (`--force --grace-period=0`) and wait for the Deployment to recreate it.

### PX-A02

Not run. Preferred loopback HTTP proxy is incompatible with `logs/kubernetes` (HTTPS required) and crashed the server when tried. Report must say the installed evidence is crash-restart adoption, not a lost-create acknowledgment. Do not claim PX-A02.

### O-27 (name these; do not gloss)

Chart `profile: development` (plaintext token-authenticated NATS) with `KODAMA_ENV=production`. Issuer is the oauthsonas persona fixture behind an access code and ingress allowlist. No NATS TLS or authentication-strength qualification. Backup posture: scheduled backups only, `qualifies_o11_pitr=false`.

### Leftover Key Manager key

`t028-probe-kb` (`35b06b3f-e8ce-4b87-bb84-b81bea48d9f7`) was created protected and could not be deleted (HTTP 412). Later creates must set `protected: false`. The 409 on the gate run is a name collision on `t028-a-<session>` / `t028-b-<session>`, not that protected key. List `t028-*` keys, delete unprotected ones, and give each test run a unique name.

### Remaining

1. Make `TestKubeSecretsKMS` use a unique key name (and delete unprotected `t028-a-kb` / `t028-b-kb` if they are still there). Re-run only that test, then the readiness gate end to end.
2. `make lint test docs-check`. Podman regression (`make test-podman` or the assembly package with `KODAMA_ASSEMBLY_BACKEND` unset).
3. Finish the report with image/chart/contract/values digests, O-27, Scaleway KMS, backup/PITR, and the PX-A02 gap. Independent review before merge. Do not push unless the owner says so.
4. After a passing gate, `qualify.sh destroy --session kb` and confirm no `kodama-q-*` namespaces remain.

Alloy `attemptId` normalization and `docs/operations/staging.md` stay T-090.
