# Full Kubernetes installation readiness (T-104) — running report

**Status: qualified at candidate `e6660bca8488` only; merged to `main` on the owner's approval (2026-10-25).** One `readiness` gate invocation passed all seven acceptances and every required child case on a fresh session, with both images verified at the registry as labelled with that commit. The earlier functional pass at `5aee4af54e2e` is superseded (below). Evidence for any other commit (`b0c4fdca4c0c`, `c1bff5cfeb51`, `90f813df67c4`, `3002bebb`, `5aee4af54e2e`) qualifies nothing later, and a documentation commit after the receipt does not retarget it.

## Readiness receipt (candidate `e6660bca8488`, 2026-10-25)

| Item | Value |
| --- | --- |
| Gate | `deploy/stage/scripts/gate.sh --gate readiness --cloud scw`, one background invocation, exit 0, `suite_passed=true`, `candidate_stable=true`, 670.63s runner time |
| Qualified commit | `e6660bca8488` (`e6660bca8488f9ff4ef69f44f4faf6a42298ea51`), clean checkout, no edit, commit or values change during the run |
| Session | `kg` (installation `kodama-stage-scw-q-kg`), prepared fresh from the original base stage contract; chart installed by the gate |
| Product image | `rg.fr-par.scw.cloud/kodama-stage-scw/kodama:0.0.2-stage-e6660bca8488@sha256:5d480ddb75eff8099fe2b680c5cccb85881b409691fb39956d6a76edfdbed3be` |
| Test-only helper | `rg.fr-par.scw.cloud/kodama-stage-scw/stageprobe@sha256:385393e498c861b9cee0dada2f20ad9e2b1624e52e94cbfeed7752fed2584e2a`, built with `--ignorefile test/support/stage/probe/build.ignore -f test/support/stage/probe/Containerfile` |
| Image provenance | The preflight read each image's manifest and config **from the registry** (both digests checked) and required `org.opencontainers.image.revision` = the full candidate commit; recorded as `suite_provenance.image_revisions` (two entries, both `e6660bca8488f9ff…`). The label is declared at build time with `--label`; it is not an attestation. |
| Workload image | `rg.fr-par.scw.cloud/kodama-stage-scw/workload:1@sha256:24cff5ad7765e56c3242014e64e2d911ca02053b92ab9c14f2beeb6519d929ea` (unchanged, not rebuilt per candidate, not label-verified) |
| Chart tree | `14436ec71a4c143328fbbda9a2265cef94487138` (`deploy/helm/kodama`) |
| Contract / values digests | `sha256:241dc89451563156987fe4403ad1a647582c58468405af6354cda69f5a3ee30a` / `sha256:bd949a32249f0aa770ac3b442d130519f9b1c49044fb522cad7eb7e91d045e18` |
| Receipt | `~/kodama-stage/evidence/T-104-readiness-e6660bca8488/` (read-only, `SHA256SUMS`); bundle `deploy/stage/.evidence/readiness/20261025T193132Z-scw-e6660bca8488-86552934762e` (git-ignored) |
| Cluster | Scaleway Kapsule `kodama-stage-scw`, `fr-par`, Kubernetes 1.37.1, Cilium; KMS provider `scw`, current label `v1` |

| Acceptance | Outcome | Required child cases |
| --- | --- | --- |
| `kube_chart_install` | pass | clean install, readiness reached |
| `kube_m1_roundtrip` | pass | roundtrip; a quiet step retains a usable `command.log` and a complete empty log; publication conflict recorded `conflict`/`tip_moved`/`failed`, none `published`, the human commit still the branch tip |
| `kube_cancel_restart` | pass | `lost_create_acknowledgment` (PX-A02), `cancel_committed_while_delete_is_refused`, `restart_adopts_the_running_Job`: all pass |
| `kube_secrets_kms` | pass | `installed_secret_restart`, `workload_provider_procedure`: both pass |
| `execution_assembly_roundtrip` | pass | |
| `execution_assembly_cancel_restart` | pass | three child cases pass |
| `execution_assembly_result_guard` | pass | |

`TestM1PostgresContractSuites` was skipped on Kubernetes by design. The Podman assembly regression (all three scenarios and the PostgreSQL contracts, 193.7s, scope removed) passed on the tree that contains the entrypoint fix, before the final commit; product code is unchanged since. `make lint`, `make docs-check`, the entrypoint and stage unit tests, the runner tests and `deploy/stage/scripts/test_scripts.sh` passed on the committed candidate.

### What each live case proved, and its limits

- **PX-A02:** the test-owned HTTPS loopback proxy (both TLS hops verified, no insecure mode) committed the first Job create of the run and withheld its response and read-backs; the server was force-replaced about 2s later. No second Job was created, and the attempt's recorded workload UID (`observedJobUid`) equals the original Job's. The Pod hold is lifted when the replacement is ready (the Job controller's backoff under a refused Pod create otherwise exceeds the product's 60s startup deadline), so the replacement's confirmation may come from redrive adoption or from observing the started Pod; either binds the original Job. The test cannot tell which, and does not claim to.
- **KMS, installed:** a secret was encrypted and decrypted by the installed server's Scaleway KMS provider and consumed by a step before and after a forced server restart through **environment-bound delivery** (a placeholder kubeconfig credential, never used to connect). A bare project secret cannot be delivered today (ENV bindings refused at registration; a file binding gets 403 without a pinned environment): VS-A10's ENV binding is **not qualified** ([T-107](../tasks/T-107-direct-secret-delivery.md)).
- **KMS, provider procedure:** in-cluster Job with the deployment's service account and credential: roundtrip, tamper rejection, rotation with a retained key, a fresh process, a bounded outage and restored access. The denied identity is a valid application key with no IAM policy; the probe now requires **HTTP 403** from the key service for it (a 401, an invalid credential, fails the procedure; an offline regression covers it) in addition to the non-retryable `KeyDenied` class. Throwaway keys used `unprotected:true` with `protected:false` in the response and were deleted; no `t028-*` IAM application or key remains.

### Limitations that remain

- **O-27:** chart `profile: development` (plaintext token-authenticated NATS) with `KODAMA_ENV=production`; the issuer is the `oauthsonas` persona fixture behind an access code and ingress allowlist. No NATS TLS and no authentication-strength claim. Exit criteria: [T-106](../tasks/T-106-production-tls-hardening.md).
- **Backups:** scheduled only, retention 7 days, `qualifies_o11_pitr=false`; no restore or recovery evidence ([T-029](../tasks/T-029-recovery-qualification.md)).
- **Secret delivery:** environment-bound only; direct ENV/file delivery is T-107.
- **Operability:** the failed-result acknowledgment races workload termination and failure logs are unclassified; one trivial writer step timed out once across six focused runs and was not diagnosed ([T-108](../tasks/T-108-execution-finalization-diagnostics.md)).
- **Tenant boundary notes from review:** the tenant NetworkPolicy now always allows egress to kube-dns on port 53 (a DNS exfiltration channel the earlier empty-egress shape did not have); the entrypoint's Git runs with `safe.directory=*` for the kubelet-owned workspace (narrowing to the workspace path, or `fsGroup`, is a follow-up); `KODAMA_GIT_CA_FILE` falls back to the callback CA bundle, so Git hosts signed by other public CAs need a private-CA setting; the tenant Role can `update` Secrets in its namespace (documented, regression-tested).
- **Operator tooling:** `qualify.sh destroy` swallows Helm and namespace deletion failures with `|| true` and cannot reconcile state naming a database that is already gone (stale session `ka` state remains for that reason); it does not delete a session's `qualification/<session>/` object prefix (this task removed its own prefixes by hand); the image label check is declared, not attested.
- Review: the branch had an independent read-only review (dispositions below) and the owner then approved the merge. Nothing is pushed.

### Review of the first receipt and its disposition

An independent read-only review of the branch (on the candidate before `e6660bca8488`) found no code blocker and these documentation/evidence issues, all addressed in the candidate:

| Finding | Disposition |
| --- | --- |
| The `5aee4af54e2e` receipt's helper image was labelled `dirty-debug` in the registry | Receipt marked superseded; builds now force `--label`; the gate preflight verifies both images at the registry and the receipt records `image_revisions` |
| A product fix and new assertions were not in any receipt | Committed, rebuilt, and covered by this receipt |
| `verify_forge_results` accepted a hand-written results file | For readiness it now requires `candidate_stable`, the applied fault, and two verified image revisions equal to the full commit, with offline negative tests |
| "Genuine KeyDenied" could be a 401 | HTTP 403 required; offline regression added |
| PX-A02 "same Job UID" overstated | Reworded; the attempt's recorded workload UID is asserted |
| Stale current-tense report sections | Rewritten or marked historical |
| State directory default changed silently | `deploy.sh` and `qualify.sh` refuse when the old directory exists and the new one does not |
| Tenant-boundary notes (`safe.directory=*`, kube-dns egress, Git CA fallback, Secret `update`) | Recorded above; narrowing `safe.directory` is a follow-up |

## Later receipt: candidate `8448adc623b5` (2026-10-26, with T-090)

The readiness gate was requalified at `8448adc623b5` on session `kj`, together with the `staging` gate (see
[the staging record](../operations/staging.md#qualification-record)): all seven acceptances and every child case passed,
now including `kube_secrets_kms/direct_secret_delivery` (ENV and file delivery of declared project secrets without a
deployment environment, before and after a server restart, never in the Job or Pod objects), a `kube_m1_roundtrip`
check that the conflicted writer's result is logged accepted and never unavailable, and per-request bounds in the
entrypoint. Receipt: `~/kodama-stage/evidence/T-090-qualification-8448adc623b5/readiness`, contract
`sha256:b31cfe0c…59ea`, values `sha256:5b469858…1453`, release image `sha256:2e937d6c…4421`, helper `sha256:2e9b3488306be77b9621fe495e8ecb8273c721d04b2aff9f849c8a58be87ec1b`, both labelled with the commit at
the registry. It qualifies that commit only; the `e6660bca8488` receipt above stays valid for that commit.

## Superseded readiness receipt (candidate `5aee4af54e2e`, 2026-10-25)

**Superseded, not qualifying.** It passed functionally (exit 0, `suite_passed=true`, 643s, session `ke`, product image `…/kodama@sha256:0c3b7859…791b`, receipt `~/kodama-stage/evidence/T-104-readiness-5aee4af54e2e/`), but review found (a) its helper image `…/stageprobe@sha256:091cd647…e47f1` labelled `dirty-debug` in the registry, because a cached label layer from an uncommitted debug build was reused, so the PX-A02 proxy and KMS procedure ran from an image of unproven origin; and (b) a product defect: a zero-byte `command.log` upload went out chunked (Go sends `ContentLength: 0` with a non-nil body as unknown length), which the signed zero-length presigned PUT refuses (HTTP 405 on Scaleway storage, logged as `diagnostic upload failed … unexpected HTTP 405` for every no-output step). Fixed in `internal/entrypoint/client.go` with a unit test and proven live: an unfixed image retains no `command.log` for a quiet step, the fixed one does.

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
| 14 | `logs/kubernetes` refuses a non-HTTPS API endpoint; the earlier HTTP fault sidecar crashed the server. | HTTP helper removed. A test-owned HTTPS loopback proxy with explicit trust on both hops is implemented and live-qualified at `e6660bca8488` (PX-A02, see the receipt). Earlier evidence was crash-restart adoption only. |

## Work state and handoff (2026-10-25)

Branch `task/T-104-kube-full` merged to `main` on the owner's approval (2026-10-25); its worktree and branch were removed after the merge. Not pushed. Everything below this line is history or reference; the current result is the receipt above.

### Historical runs against session `kb` (before the hardening, now destroyed)

| Acceptance | First run | Subsequent run at `c1bff5cfeb51` |
| --- | --- | --- |
| `kube_chart_install` | passed | passed |
| `kube_m1_roundtrip` | passed | passed |
| `kube_cancel_restart` | passed (crash-restart only) | passed (crash-restart only) |
| `kube_secrets_kms` | failed (409 key collision) | passed (operator-host provider procedure only) |
| `execution_assembly_roundtrip` | passed | passed |
| `execution_assembly_cancel_restart` | passed | passed |
| `execution_assembly_result_guard` | passed | passed |

These predate the lost-acknowledgment and in-cluster KMS cases and qualify nothing now. A rejected gate in that period took about 500 seconds of test execution; an additional hour was wasted by a `pgrep` wait that matched itself. Run the whole gate as one background command and wait for its completion notification.

### Operating notes

| Item | Value |
| --- | --- |
| Kubeconfig / state | `~/kodama-stage/scw.yaml`; `~/kodama-stage/state` (`STAGE_STATE_DIR` and `KODAMA_QUAL_STATE_DIR`) |
| Tools | `/tmp/opencode/kodama-stage-tools` (`tofu` 1.13.2, `helm` 4.3.1, `shellcheck` 0.10.0) |
| Operator API | the kubeconfig token is a Scaleway secret key (`SCW_SECRET_KEY`); the provider access key is in `/tmp/opencode/scw-access-key`; project `943031a5-cc3c-4202-aea1-697e4e55863c`. Never print them. |
| Issuer allowlist | the live annotation on `kodama-deps/kodama-oauthsonas-restricted` includes `151.115.90.46/32` plus the three stage-deploy addresses; a later stack apply can drop it |

Do not reapply the HTTP kube-proxy sidecar. A graceful `scale` to 0 can reconcile a running Job away; adoption tests crash the server pod (`--force --grace-period=0`) and wait for the Deployment to recreate it. Qualification sessions `kb` through `kg` were destroyed with the original base contract after the receipt was preserved; this task's `qualification/<session>/` object prefixes were deleted; no `kodama-q-*` namespace, session admission policy, session database, throwaway Key Manager key or `t028-*` IAM identity remains. The protected `t028-probe-kb` key (`35b06b3f-e8ce-4b87-bb84-b81bea48d9f7`) and the installation keys were not touched.

### O-27 (name these; do not gloss)

Chart `profile: development` (plaintext token-authenticated NATS) with `KODAMA_ENV=production`. Issuer is the oauthsonas persona fixture behind an access code and ingress allowlist. No NATS TLS or authentication-strength qualification. Backup posture: scheduled backups only, `qualifies_o11_pitr=false`.

### Key Manager keys

Scaleway's create field is **`unprotected:true`**, not the response-only `protected:false`; creates request an unprotected key, require `protected:false` in the response, use random per-run names, register cleanup immediately for each key and check unprotect/delete statuses. No retained installation key is modified.

### Hardening baseline and disposition

Baseline: `90f813df67c4`, specifications `docs/specs/pipeline-execution.md` (PX-03/PX-04/PX-08, PX-A02/PX-A06), `docs/specs/variables-and-secrets.md` (VS-03/VS-06/VS-07/VS-09), T-104 and `test/integration/kms-production/README.md`; O-24/O-26/O-27 unchanged. Bounded shared-path extension: stage gate/evidence scripts, their offline tests and stage runbook; no T-090 product behavior, Alloy or staging suite changes. Disposition: implementation strengthens the existing acceptance contract and is live-qualified at `e6660bca8488`; owner approved the merge.

- The gate runs clean chart installation first, then all remaining scenarios as one invocation with one candidate/contract/values binding and checked required child cases. It refuses dirty candidates, input/session mismatch, mislabelled images and mid-run drift; a per-session lock prevents concurrent gate mutations; `--preflight` checks inputs without issuing a receipt.
- The test-only `stageprobe` image runs the HTTPS fault proxy and the real-provider KMS procedure; it is built from the committed candidate and supplied as `KODAMA_STAGE_PROBE_IMAGE=repository@sha256:...`. It is not the product image.
- Fault cleanup restores database relays, server replicas, admission policies and the exact pre-fault Pod template; command errors omit arguments and provider bodies; calls are bounded.
- Publication conflicts use a post-checkout barrier; required authorization denials no longer accept server errors; foreign-job refusal must match one structured event.

### Remaining

1. Merged locally to `main` on the owner's approval; nothing is pushed.
2. Follow-ups outside this task: [T-107](../tasks/T-107-direct-secret-delivery.md) (direct secret delivery), [T-108](../tasks/T-108-execution-finalization-diagnostics.md) (acknowledgment ordering, classified logs, the unexplained timeout), [T-106](../tasks/T-106-production-tls-hardening.md) (O-27), narrowing the entrypoint's `safe.directory=*`, and `qualify.sh destroy` robustness (T-090).
3. Any later commit that changes product, chart or stage inputs needs a new fresh session and a new gate run.

Alloy `attemptId` normalization and `docs/operations/staging.md` stay T-090.
