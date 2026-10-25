# Full Kubernetes installation readiness (T-104) — running report

**Status: in progress.** No acceptance check has passed yet; this file records what the first real installation on the Scaleway stage cluster (`kodama-stage-scw`) established, the defects it found, and the recorded deviations. It becomes the T-104 report when the `kube-full` suite exists and passes.

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

## Work state and handoff (2026-10-25)

Nothing has been qualified: no acceptance check has passed and no gate has run. What exists:

- **Stage installation** (`deploy.sh`): ready, bootstrap done, `/api/v1/me` works through the persona issuer.
- **Qualification sessions** (`deploy/stage/qualification`, `qualify.sh`; see `deploy/stage/README.md`): `prepare` was run for session `ka` and works end to end in about two minutes; `helm upgrade --install` of the committed chart into it takes about a minute and reaches readiness with a Let's Encrypt certificate. Session `ka` is still live on the stage cluster (namespaces `kodama-q-ka*`, database `kodama_q_ka`, log level DEBUG); it runs the chart **without** the finding-8 fix. Remove it with `qualify.sh destroy --session ka --contract ~/.kube/kodama-stage/stage_contract.json` (needs `KUBECONFIG`, `KODAMA_IMAGES_FILE`, `SCW_*`), or `helm upgrade` it with the fixed chart to continue.
- **Fixture** (`test/support/stage`): session handle, kubectl/helm/port-forward wrappers, `oauthsonas` login (access-code gate, S256 PKCE), install/uninstall/restart/stop/start, database relay outage (`RelayDown`/`RelayUp`), `kodama bootstrap` as a Pod, owner/runtime database inspection through the probe relay, a per-test Gitea repository, the seeded two-tenant world and the API helpers (adapted from the M1 kit), Job/Pod inspection, runtime-token extraction, and admission-policy holds (`HoldJobs` CREATE/DELETE for the server's service account only). Verified live: login, `/me`, bootstrap, `Seed` (tenants, personas, project, memberships), workspace provisioning and run admission. The first pipeline run stopped at finding 8.
- **Not started**: the backend interface in `test/integration/assembly-execution` (it still hard-codes Podman through `kit`), `test/e2e/kube-full`, `test/e2e/staging`, the exact acceptance map in `lib.sh` (the current parser drops package and backend identity), the Alloy `attemptId` normalization (the control plane logs `attemptId`, Alloy extracts `attempt`) and `docs/operations/staging.md`.

Resuming from a fresh checkout: the operator-local inputs live outside the repository in `~/.kube/kodama-stage/` (`scw.yaml` kubeconfig, `stage_contract.json`, `kodama-values.yaml`, `images.json`, `scw.tfvars`, `state/` with the OpenTofu states). Copy `scw.tfvars` to `deploy/stage/scw/terraform.tfvars` (git-ignored) before running `deploy.sh`; `qualify.sh` takes the project from `SCW_DEFAULT_PROJECT_ID` or that file. The deploy command is in `deploy/stage/README.md`; the issuer allowlist needs `KODAMA_ISSUER_ALLOWED_CIDRS` (the operator machine egresses through several addresses). The stage cluster and its database keep billing until scaled down or destroyed.

Decisions taken against the earlier plan: the tests run on the operator machine (as the gate already does) and reach the private database through the `kq-pgprobe` relay and a port forward, so no in-cluster runner image is needed; deterministic delete/create windows use ValidatingAdmissionPolicies scoped to the server's service account instead of a sidecar proxy. Lost-create-response (PX-A02) cannot be made deterministic that way; a loopback Kubernetes API proxy sidecar (`KODAMA_KUBE_API` accepts loopback http) remains the planned means, and a relay-timed variant must be reported as such if used instead.

Fixture facts learned: `baseRef` of a project repository must be a full `refs/heads/...` ref; tenant namespaces must start with `kodama-`; the generated client's base is `<url>/api/v1`; project repository credentials are matched by URL prefix and tenant list (`git-credentials.json`).

## Remaining for T-104

Everything below runs against a prepared session after the chart fix is installed in it: the backend-parameterized `execution_assembly_*` scenarios on Kubernetes (with the Podman backend kept), `test/e2e/kube-full` (`kube_chart_install`, `kube_m1_roundtrip`, `kube_cancel_restart`, `kube_secrets_kms` with throwaway Key Manager keys and the live procedure of `test/integration/kms-production/README.md`), exact package/backend evidence matching, and the readiness gate receipt. Until they pass, T-104 and T-090 stay open.
