# Full Kubernetes installation readiness (T-104) — running report

**Status: qualified at candidate `5aee4af54e2e` only; independent review and merge are pending.** One `readiness` gate invocation passed all seven acceptances and every required child case on a fresh session (receipt below). Earlier evidence in this report (`b0c4fdca4c0c`, `c1bff5cfeb51`, `90f813df67c4`, `3002bebb`) predates the final repairs and does not qualify that commit or any later one; a documentation commit after the receipt does not retarget it.

## Readiness receipt (candidate `5aee4af54e2e`, 2026-10-25)

| Item | Value |
| --- | --- |
| Gate | `deploy/stage/scripts/gate.sh --gate readiness --cloud scw`, one invocation, exit 0, `suite_passed=true`, `candidate_stable=true`, 643.18s runner time |
| Qualified commit | `5aee4af54e2e` (`5aee4af54e2e11285b7a156f50c323f430f37c3b`), clean checkout, no edit, commit or values change during the run |
| Session | `ke` (installation `kodama-stage-scw-q-ke`), prepared fresh from the original base stage contract; chart installed by the gate |
| Product image | `rg.fr-par.scw.cloud/kodama-stage-scw/kodama:0.0.2-stage-5aee4af54e2e@sha256:0c3b7859dc1787c78fd171daae11574b4ae6945a69640769b6d38b908978791b`, built from the candidate with `REVISION` |
| Workload image | `rg.fr-par.scw.cloud/kodama-stage-scw/workload:1@sha256:24cff5ad7765e56c3242014e64e2d911ca02053b92ab9c14f2beeb6519d929ea` (unchanged) |
| Test-only helper | `rg.fr-par.scw.cloud/kodama-stage-scw/stageprobe@sha256:091cd647816f299fdf10506d7b04a2ecb4b2b61713a3131f375052d7a16e47f1`, built from the candidate with `--ignorefile test/support/stage/probe/build.ignore -f test/support/stage/probe/Containerfile` |
| Chart tree | `14436ec71a4c143328fbbda9a2265cef94487138` (`deploy/helm/kodama`) |
| Contract / values digests | `sha256:ae8f9f9514fe0f2fb9f300dbbf258a4e1d053e6dde61d139158be8554997c5ca` / `sha256:24cee247583e2b9ca9f9e0ab10dc7fde0c275e1054b83476abd1787c1e1dbf6b` |
| Receipt | `~/kodama-stage/evidence/T-104-readiness-5aee4af54e2e/` (read-only, `SHA256SUMS`); original bundle `deploy/stage/.evidence/readiness/20261025T182850Z-scw-5aee4af54e2e-5f375c691593` (git-ignored) |
| Cluster | Scaleway Kapsule `kodama-stage-scw`, `fr-par`, Kubernetes 1.37.1, Cilium; KMS provider `scw`, current label `v1` |

| Acceptance | Outcome | Required child cases |
| --- | --- | --- |
| `kube_chart_install` | pass | clean install, readiness reached |
| `kube_m1_roundtrip` | pass | reader/writer/follow-up roundtrip, publication conflict recorded as `conflict`/`tip_moved` with the human commit intact |
| `kube_cancel_restart` | pass | `lost_create_acknowledgment` (PX-A02), `cancel_committed_while_delete_is_refused`, `restart_adopts_the_running_Job`: all pass |
| `kube_secrets_kms` | pass | `installed_secret_restart`, `workload_provider_procedure`: both pass |
| `execution_assembly_roundtrip` | pass | |
| `execution_assembly_cancel_restart` | pass | three child cases pass |
| `execution_assembly_result_guard` | pass | |

`TestM1PostgresContractSuites` was skipped on Kubernetes by design. The Podman assembly regression last ran during hardening (before `3002bebb`); it was **not** rerun after the final test-only repairs, which do not touch the Podman path or the default backend.

### What each live case proved, and its limits

- **PX-A02:** the test-owned HTTPS loopback proxy (both TLS hops verified, no insecure mode) committed the Job create, withheld the response and read-backs; the server was force-replaced 2s later and the replacement confirmed the same Job UID with one attempt, grant and Pod. The Pod hold is lifted when the replacement is ready (the Job controller's exponential backoff under a refused Pod create otherwise exceeds the product's 60s startup deadline), so the replacement's confirmation may come from either redrive adoption or observation of the started Pod. The receipt records `fault_config.applied=true`, `case_status=pass`.
- **KMS, installed:** the secret was encrypted and decrypted by the installed server's Scaleway KMS provider and consumed by a step before and after a forced server restart, through **environment-bound delivery** (placeholder kubeconfig credential, never used to connect). A bare project secret cannot be delivered by the installed server today (ENV bindings are refused at registration; a file binding gets 403 without a pinned environment), so VS-A10's ENV binding is **not qualified**; this is a product limitation for the owner (T-097/T-101/T-105), not closed by this receipt.
- **KMS, provider procedure:** in-cluster Job with the deployment's service account and credential: roundtrip, tamper rejection, rotation with a retained key, a fresh process, a valid identity with no policy returning a genuine `KeyDenied`, a bounded outage and restored access. Throwaway keys used `unprotected:true` with `protected:false` in the response and were deleted; a post-run key listing shows only the installation KEKs and the protected `t028-probe-kb`.

### Limitations that remain

- **O-27:** chart `profile: development` (plaintext token-authenticated NATS) with `KODAMA_ENV=production`; the issuer is the `oauthsonas` persona fixture behind an access code and ingress allowlist. No NATS TLS and no authentication-strength claim.
- **Backups:** scheduled only, retention 7 days, `qualifies_o11_pitr=false`.
- Retained diagnostics: every workload logged `diagnostic upload failed … unexpected HTTP 405` for its `command.log` artifact upload (non-fatal; not investigated here, for the log/artifact owner).
- One `execution_timeout` of a trivial writer step occurred once across six focused `TestKubeM1Roundtrip` runs on a reused disposable session; it did not recur and did not occur in the gate. It was not diagnosed.
- Independent review and owner approval are outstanding; nothing is pushed or merged.

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
| 8 | The server's cluster-scoped Role could not create `SubjectAccessReview`s, which the tenant preflight posts before every dispatch. No Job was created and the run stayed `queued`. | Fixed in the chart with `Test_helm_preflight_access_review_rbac` and exercised in the historical stage runs. Review still required. |
| 9 | The server logs an unclassified `unavailable` for a refused dispatch (`execution: dispatch error=unavailable`) without naming the failing dependency or call. Finding 8 needed code reading to diagnose. | Observation for the execution owner: log the classified cause (preflight reason, API operation and status). |
| 10 | The tenant Role granted Secrets `create, get, list, delete` but not `update`. The workload record is an annotation on the admission Secret, replaced with PUT as the attempt advances. Every dispatch died `unavailable` after creating that Secret. | Fixed in `deploy/helm/kodama/templates/rbac.yaml`; `Test_helm_secret_management_rbac` now requires `update`. |
| 11 | Cilium with kube-proxy replacement does not honour a ClusterIP `ipBlock` for CoreDNS. Workloads could not resolve the callback hostname and died at the execution deadline. | Chart always renders a kube-dns pod-selector egress (port 53). `internal/kube/preflight.go` allows only that peer. |
| 12 | `customCA: false` left `KODAMA_GIT_CA_FILE` unset. Source checkout refuses an empty Git CA, so every reader/writer dispatch was `unavailable` (logged that way even when the cause is `ErrCheckoutDenied`). | Chart sets `KODAMA_GIT_CA_FILE` to `/secrets/runtime-ca.crt` unless a private CA is configured. Stage `runtime-ca.crt` is the ISRG roots. |
| 13 | The workspace volume is owned by the kubelet, not uid 65532. Git refuses the checkout (`dubious ownership in repository at '/workspace'`). Restricted Pod Security forbids a chown. | Entrypoint git sets `safe.directory=*`; the step environment sets `safe.directory` to the workspace unless the step already configures git. Requires the image built after this change. |
| 14 | `logs/kubernetes` refuses a non-HTTPS API endpoint; the earlier HTTP fault sidecar crashed the server. | HTTP helper removed. A test-owned HTTPS loopback proxy with explicit trust on both hops is now implemented, but not live-qualified. Historical evidence is crash-restart adoption, not a lost acknowledgment. |

## Work state and handoff (2026-10-25)

Branch `task/T-104-kube-full` (worktree `.work/T-104-kube-full`). Not pushed. Not reviewed. Not merged.

### What passed live (session `kb`, 2026-10-25)

Historical runs against session `kb` (before the new mandatory coverage):

| Acceptance | First run | Subsequent run at `c1bff5cfeb51` |
| --- | --- | --- |
| `kube_chart_install` | passed | passed |
| `kube_m1_roundtrip` | passed | passed |
| `kube_cancel_restart` | passed (crash-restart only) | passed (crash-restart only) |
| `kube_secrets_kms` | failed (409 key collision) | passed (operator-host provider procedure only) |
| `execution_assembly_roundtrip` | passed | passed |
| `execution_assembly_cancel_restart` | passed | passed |
| `execution_assembly_result_guard` | passed | passed |

`TestM1PostgresContractSuites` was correctly skipped on Kubernetes. The later **Podman** regression at `90f813df` passed all three assembly scenarios and the PostgreSQL contracts (210.921s, exit 0); scope `t-104-local` was removed. `make lint`, `make test`, `make docs-check` and the stage script checks passed on the earlier candidate. These are historical results, not validation of the subsequent hardening changes.

The rejected gate took about 500 seconds of test execution. An additional hour was wasted by an operator wait whose `pgrep` pattern matched itself; the tests were already finished. Detached shells and buffered runner output obscured completion. Do not repeat that wait pattern or commit while qualification runs.

### Session still live — do not destroy until the gate passes or the owner says so

| Item | Value |
| --- | --- |
| Session | `kb` (`~/kodama-stage/state/qualification/kb`) |
| URL | `https://kb.195-154-68-111.nip.io` |
| Namespaces | `kodama-q-kb`, `kodama-q-kb-a`, `kodama-q-kb-b` |
| Kubeconfig | `~/kodama-stage/scw.yaml` (not `~/.kube/kodama-stage/`) |
| State dir | `~/kodama-stage/state` (`STAGE_STATE_DIR` / `KODAMA_QUAL_STATE_DIR`) |
| Historical contract / values | `~/kodama-stage/state/qualification/kb/contract.json` and `values.yaml`; a fresh session is required for the next clean-install gate |
| Image under test | `rg.fr-par.scw.cloud/kodama-stage-scw/kodama:0.0.2-stage-b0c4fdca4c0c-3@sha256:30d79c1d43ff87ea60fbd38bbe89904de755efee165d7a3e78ce8fd6ba595f04` |
| Workload image | unchanged `sha256:24cff5ad7765e56c3242014e64e2d911ca02053b92ab9c14f2beeb6519d929ea` |
| Tools | `/tmp/opencode/kodama-stage-tools` (`tofu` 1.13.2, `helm` 4.3.1, `shellcheck` 0.10.0) |
| Issuer allowlist | live annotation on `kodama-deps/kodama-oauthsonas-restricted` includes `151.115.90.46/32` plus the three addresses from the stage deploy. A later stack apply can drop `151.115.90.46`. |
| Operator API | kubeconfig token is a Scaleway secret key (`SCW_SECRET_KEY`). Access key for the provider is in `/tmp/opencode/scw-access-key` (do not print it). Project `943031a5-cc3c-4202-aea1-697e4e55863c`. |

Session `ka` is gone. Do not reapply the HTTP kube-proxy sidecar. A graceful `scale` to 0 can reconcile a running Job away; adoption tests crash the server pod (`--force --grace-period=0`) and wait for the Deployment to recreate it.

### PX-A02

Not live-qualified. The new `lost_create_acknowledgment` subcase uses a test-owned HTTPS loopback proxy via `KODAMA_KUBE_API`, with a temporary trust root through `KODAMA_KUBE_CA_FILE`; it does not weaken TLS checks or the product binary. Job creation commits upstream, its response and subsequent read-back are withheld, and Pod admission is held so callbacks cannot confirm dispatch. A forced server-Pod replacement resets the fault; the replacement must confirm the same Job UID and complete with one attempt/grant/Pod. The original chart Pod template is restored afterwards. Offline proxy tests are not installed-server acceptance. Continue to describe historical evidence as crash-restart adoption until this subcase passes live.

### O-27 (name these; do not gloss)

Chart `profile: development` (plaintext token-authenticated NATS) with `KODAMA_ENV=production`. Issuer is the oauthsonas persona fixture behind an access code and ingress allowlist. No NATS TLS or authentication-strength qualification. Backup posture: scheduled backups only, `qualifies_o11_pitr=false`.

### Leftover Key Manager key

`t028-probe-kb` (`35b06b3f-e8ce-4b87-bb84-b81bea48d9f7`) remains protected and untouched per the owner's instruction. The old `t028-a-kb` / `t028-b-kb` were unprotected and deleted successfully. Scaleway's create field is **`unprotected:true`**, not the response-only `protected:false`; the earlier request used the wrong field. New creates request an unprotected key, require `protected:false` in the response, use random per-run names, register cleanup immediately for each key and check unprotect/delete statuses. No retained installation key is modified.

### Hardening baseline and disposition

Baseline: `90f813df67c4`, specifications `docs/specs/pipeline-execution.md` (PX-03/PX-04/PX-08, PX-A02/PX-A06), `docs/specs/variables-and-secrets.md` (VS-03/VS-06/VS-07/VS-09), T-104 and `test/integration/kms-production/README.md`; O-24/O-26/O-27 remain unchanged. Bounded shared-path extension: stage gate/evidence scripts, their offline tests and stage runbook; no T-090 product behavior, Alloy or staging suite changes. Disposition: implementation strengthens the existing acceptance contract; live conformance and review remain pending.

- The gate runs clean chart installation first, then all remaining scenarios as **one gate invocation**, with one candidate/contract/values binding and checked required child cases. It refuses dirty candidates, input/session mismatch and mid-run drift; a per-session lock prevents concurrent gate mutations. `--preflight` checks inputs without issuing a receipt.
- The test-only `stageprobe` image runs the HTTPS fault proxy and the real-provider KMS procedure. It must be built from the committed candidate and supplied as `KODAMA_STAGE_PROBE_IMAGE=repository@sha256:...`. It is not the product image.
- KMS acceptance now requires both installed secret consumption across a real server restart and the provider procedure in an in-cluster Job with the deployment's credential/service account. The provider procedure requires rotation, configured-version tamper rejection, a genuinely fresh process, a valid identity with no decrypt permission, a retryable bounded outage and successful restored access. The denied identity has no IAM policy and is deleted with the throwaway keys.
- Fault cleanup restores database relays, server replicas, admission policies and the exact pre-fault Pod template. Command errors omit arguments/provider bodies, calls are bounded, and failed cluster queries cannot establish workload absence.
- Publication conflicts use a post-checkout barrier instead of a 25-second guess. Long fault workloads have explicit 900-second budgets. Required authorization denials no longer accept server errors, and foreign-job refusal must match one structured event.

### Hardening verification (2026-10-25)

| Check | Result |
| --- | --- |
| `make lint test docs-check` | Passed; final support-only guard changes were rechecked with the scoped tests below and `make lint docs-check`. |
| `go test -race -count=1 ./test/support/stage/...` | Passed: cleanup registration on mutation failure, API-error/absence distinction, session-input binding, bounded commands, log-field matching, loopback certificate trust, HTTP/1.1 and HTTP/2 lost responses, read-back refusal, replacement proxy, streaming, untrusted upstream rejection, and offline KMS procedure/denial/outage regressions. |
| `go test -race -count=1 -run 'TestKeyCleanup\|TestCreateKey' ./test/e2e/kube-full/` | Passed offline: create fields, immediate per-key cleanup and checked provider statuses; no live keys created. |
| `python3 -B deploy/stage/scripts/test_runner.py` | Eight offline runner tests passed: phases, exact required cases, skip refusal, dirty/changed candidates, commit drift with preserved process exit, changed values, truncated packages, busy session and preflight/input mismatch. |
| `deploy/stage/scripts/test_scripts.sh` | Passed: the runner tests plus OpenTofu, Helm, ShellCheck, cloud/input mismatch and receipt/required-case failure paths. Its synthetic gate receipts are **not live qualification**. |
| `make test-integration DEV_SCOPE=t-104-hardening INTEGRATION_PKGS=./test/integration/assembly-execution/...` | Passed with `KODAMA_ASSEMBLY_BACKEND` unset, on `DEV_STACK=services`: all three Podman assembly scenarios and PostgreSQL contracts (215.602s). Scope teardown passed. Final helper transport/session checks do not execute on the Podman path. |
| `KODAMA_ASSEMBLY_BACKEND=kube go test -run '^$' ./test/integration/assembly-execution/` | Compilation/TestMain selection passed; no Kubernetes scenarios executed. |
| `go build -o /tmp/opencode/t104-stageprobe ./test/support/stage/probe/cmd/stageprobe` | Passed. A scratch-image COPY audit with the helper's explicit `build.ignore` also passed; the audit image was removed. The product `.dockerignore` excludes `test/`, so the helper build must use that ignore-file override. |

No new product/probe image has been published, no new qualification session prepared, and no new live gate run.
The HTTPS fault and in-cluster KMS cases still need live qualification; their offline checks do not close that gap.
Session `kb` and the protected historical probe key were not changed. Independent review remains pending.

### Live qualification attempt on candidate `3002bebb` (2026-10-25, not a gate run)

Product image `rg.fr-par.scw.cloud/kodama-stage-scw/kodama@sha256:3070e33c…e17e`, helper `…/stageprobe@sha256:c9068772…8e3e`, both built from committed `3002bebbbcd9`. Preflight passed on fresh session `kc` (kept unused for the final gate). Focused debugging used disposable session `kd`; its results are not part of any receipt.

| Case | Classification | Finding / disposition |
| --- | --- | --- |
| `kube_cancel_restart/lost_create_acknowledgment` | fixture defect (fixed) | The lost create happened on the redrive, 30s after dispatch intent, because the Job-create hold consumed the first dispatch. The redrive re-claimed the 30s lease, so the replacement could not redrive before the product's 60s startup deadline (`startup_timeout`). The fault is now armed for the first Job create in the tenant namespace before the run starts (name-less arming; the withheld create must be the attempt's Job), and the Pod hold ends when the replacement is ready, because the Job controller's exponential backoff under a refused Pod create otherwise also exceeds the deadline. The same Job UID is confirmed with one attempt/grant/Pod. Passed live on `kd` once with these changes. |
| `kube_secrets_kms/workload_provider_procedure` | passed live on `kd` (not yet in a gate) | |
| `kube_secrets_kms/installed_secret_restart` | fixture defect (fixed) plus a product limitation (recorded) | The test used a bare project secret in a step. The installed server refuses `env: {secretRef}` at registration (`CheckSecret` admits `secretFiles` only) and answers `resolveSecret` 403 for a project secret that no pinned deployment environment names (`SecretRoute` yields no bindings without an environment pin). The case now consumes the secret through **environment-bound delivery**: a placeholder kubeconfig-kind credential (never used to connect), the generic secret named as a file binding by a project environment, and a step pinned to that environment, consumed before and after a forced server restart. The decrypt goes through the installed server's Scaleway KMS provider. Limitation for the owner: plain step ENV/file delivery of project secrets (VS-A10 ENV binding) is not implemented in the installed composition. |
| `kube_m1_roundtrip` publication conflict | fixture defect (fixed) | The hardening asserted attempt reason `publication_conflict`. Execution keeps a specific attempt reason only for `output_invalid`/`output_upload_failed`; every other failed-result reason becomes `command_failed`. The test now asserts the failed run, one workspace publication recorded `conflict`/`tip_moved`/`failed`, none `published`, and the human's commit still the branch tip. |

### Remaining

1. Independent review of `task/T-104-kube-full` (commits `c0fdf77e` and `5aee4af5` on top of `3002bebb`) with the receipt above; nothing is pushed or merged.
2. Owner follow-ups outside this task: plain step secret delivery (VS-A10 ENV binding), the `command.log` upload 405 warning, and T-106 (NATS/issuer TLS) for O-27.
3. A later commit that changes product, chart or stage inputs needs a new fresh session and a new gate run.

Qualification sessions `kb`, `kc`, `kd` and `ke` were destroyed with the original base contract after the receipt was preserved; the protected `t028-probe-kb` key was not touched. After destruction no `kodama-q-*` namespace, session admission policy or throwaway Key Manager key remains. A stale local state directory for the long-gone session `ka` is left: its `qualify.sh destroy` fails because the state names a database (`kodama_q_ka`) that no longer exists; nothing of `ka` is live, and its state was not hand-edited.

Alloy `attemptId` normalization and `docs/operations/staging.md` stay T-090.
