# Staging: trusted-engineer deployment and qualification (T-090)

This runbook installs, upgrades, searches and uninstalls the Kodama appliance on the Kodama-owned stage cluster
`kodama-stage-scw` and records what its qualification proves. It is a **standalone smoke deployment**. It is not
Platform stage and not hosted integration ([T-061](../tasks/T-061-platform-integration.md)), and it is not a supported
production profile.

## What is deployed, and what it is not

| Item | Value |
| --- | --- |
| Cluster | Scaleway Kapsule `kodama-stage-scw`, `fr-par`, Kubernetes 1.37, Cilium |
| Control plane | `KODAMA_ENV=production`, `kodama server --assembly core`, standalone authority, one release in `kodama-system` |
| Execution | Kubernetes Jobs in tenant namespaces under the restricted Pod Security baseline |
| Trust model | **Trusted engineers on regular Kubernetes containers** (O-07, P-14). Untrusted-code isolation is T-055 and is not claimed |
| Secrets | Scaleway Key Manager (`scw` provider); the development keyfile is refused |
| Database | Scaleway Managed PostgreSQL 17, scheduled backups only: `qualifies_o11_pitr=false`, no 30-day PITR |
| Observability | Grafana Alloy to Loki (object-store backed) to Grafana behind an operator login |
| Issuer | the `oauthsonas` persona fixture behind an access code and an ingress allowlist |

**Recorded deviation (O-27, [T-106](../tasks/T-106-production-tls-hardening.md)).** NATS is plaintext `nats://` with
token authentication under chart `profile: development`, and the issuer is a fixture. Nothing in staging qualifies NATS
TLS, a private CA, PostgreSQL certificate verification or authentication strength. A hand-off must state this and must
not claim otherwise.

Result acknowledgment ordering and classified failure logs are
[T-108](../tasks/T-108-execution-finalization-diagnostics.md); direct project-secret delivery is
[T-107](../tasks/T-107-direct-secret-delivery.md).

## Prerequisites

Tools (`tofu`, `helm`, `kubectl`, `go`, `python3`), the stage kubeconfig, and an operator Scaleway credential
(`SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`). Keep Terraform state, tfvars, kubeconfig and credentials
outside the checkout: state lives in `$STAGE_STATE_DIR` (default `~/kodama-stage/state`, mode 0700). Set
`STAGE_STATE_DIR` and `KODAMA_QUAL_STATE_DIR` to the same directory. The scripts refuse to start if state exists only
in the former default `~/.kube/kodama-stage/state`.

## Install and upgrade the stage

The stage's own release and its dependencies come from `deploy/stage/scripts/deploy.sh` (OpenTofu roots `scw/` and
`shared/stack`, then the Helm releases). It needs digest-pinned images:

```sh
export KODAMA_IMAGES_FILE=~/kodama-stage/images-<commit>.json   # kodama and workload: repository, tag, digest
export KODAMA_ACME_EMAIL=...                                      # Let's Encrypt account contact
deploy/stage/scripts/deploy.sh --cloud scw
```

Build and publish the images from a **clean, committed** tree with the explicit revision label, and verify them at the
registry before deploying (see the [stage README](../../deploy/stage/README.md#build-once-from-committed-source)). An
upgrade is the same command with a new images file; the chart's migration Job runs under the migration role before the
server rolls. A rollback is a deploy with the previous images file; migrations are forward-only, so a schema rollback
means restoring a database snapshot.

`deploy.sh --validate-only` checks formatting, rendered Helm schemas and stack validation offline.

## Uninstall and what is retained

```sh
helm uninstall kodama -n kodama-system --wait
```

Uninstall removes the workloads and chart objects. It **retains** the database with its schema and rows, the
object-store buckets and their data, and the Key Manager keys. Deleting any of those is a separate, deliberate action
(`qualify.sh destroy` for a qualification session, which also purges that session's own `qualification/<session>/`
objects). The protected `t028-probe-kb` key and the installation keys are never touched by these scripts.

## Qualify a candidate

Qualification is bound to one commit, one contract and one values file, on a freshly prepared session
(`qualify.sh prepare`, original base contract). Both gates run on the **same** session and candidate:

```sh
deploy/stage/scripts/gate.sh --gate readiness --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
deploy/stage/scripts/gate.sh --gate staging   --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
```

Run each as one background command and wait for its completion notification (see the stage README). The `staging` gate
refuses to run without a passing, digest-verified `readiness` receipt for the same commit, contract and values, and it
requires the release image to be labelled with the candidate commit **in the registry**.

| Acceptance | What the test does |
| --- | --- |
| `stage_clean_install` | removes any earlier release, clears jobs, pods and per-attempt bookkeeping from the tenant namespaces, installs the chart, and requires readiness under `KODAMA_ENV=production` with `--assembly core` |
| `stage_e2e_roundtrip` | submits a pipeline, runs it, and reads the retained log and the output artifact after the workload is gone |
| `stage_restart_reconcile` | cancels a run (workload removed), then kills the server mid-run (`--force --grace-period=0`) and requires the replacement to adopt the same Job and finish with one attempt |
| `stage_loki_log_search` | finds the pipeline's and the control plane's log lines for one attempt by `attempt_id` in Loki through Grafana, and requires Grafana to refuse an unauthenticated Loki query |
| `stage_dev_adapter_refused` | checks the deployed configuration is production with the cloud KMS and no development selection, then starts the **deployed** server specification with one development or test selection at a time (development key file, OIDC fixture, S3 fixture, Process/Podman runtime) and requires a non-zero exit that names the refused adapter |
| `stage_retention_uninstall` | uninstalls, then requires the run records, the artifacts bucket and its objects, and the installation's enabled Key Manager keys to remain. It runs last and leaves the session uninstalled |

The tests run in that order; a failing step stops later tests from claiming anything about a half-built installation.

## Search logs by attempt

Alloy tails Pod logs over the Kubernetes API (no hostPath, no root) in `kodama-system`, the contract's tenant
namespaces and the qualification sessions (`kodama-q-*`). It stores `job_name`, `pod` and `attempt_id` as Loki
structured metadata, so they never become stream labels. The step entrypoint logs the attempt as `attempt` and the
control plane as `attemptId`; Alloy normalizes both to `attempt_id`.

Grafana is `https://grafana.<ingress_host>` and is operator-authenticated (anonymous access disabled). The admin
credential is the Secret `kodama-grafana-admin` in `kodama-observability`; read it with `kubectl`, never print it. In
Explore (Loki data source) or the **Kodama Pipeline & Control-Plane Logs** dashboard:

```logql
{namespace="kodama-tenant-stage"} | attempt_id="att_..."
{namespace="kodama-system", container="server"} | attempt_id="att_..."
```

Loki retains 30 days (720h). Search is for operators; tenants read their own logs through the Kodama API.

## Hand-off record

Record with each qualified candidate: the commit; product, workload and helper image digests and their registry
revision labels; the chart tree; the contract and values digests; both receipt paths; the O-27 deviation; the backup
posture; and every limitation below. Evidence bundles are written under `deploy/stage/.evidence/` (git-ignored): copy
them read-only, with checksums, outside the checkout before removing a worktree.

## Known limitations

- O-27 (NATS, issuer) and scheduled-only backups, as above.
- Secrets reach steps through deployment environments until T-107 lands.
- `qualify.sh destroy` stops on real teardown failures and retries once from recorded state when a resource is already
  gone; a session whose state it still cannot reconcile needs a manual decision, not a forced removal.
- Image provenance is a declared registry label, not a build attestation.
- Loki search depends on Alloy being deployed from this stack revision; an older Alloy tails fewer namespaces and does
  not normalize `attemptId`.
