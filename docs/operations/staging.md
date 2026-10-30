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
| Execution | Kubernetes Jobs in tenant namespaces under the restricted Pod Security baseline (`attempt_private`). The designed shared-workspace/services/builders model is not part of this stage's qualification |
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
outside the checkout: operator files live in `$STAGE_STATE_DIR` (default `~/kodama-stage/state`, mode 0700), one directory
per cluster name under `clusters/`, and the OpenTofu state is in the versioned Scaleway bucket `KODAMA_TFSTATE_BUCKET`,
created once with `deploy/stage/scripts/state-bucket.sh`. Several clusters can run at the same time; every script takes
`--name <cluster>` (or `KODAMA_STAGE_CLUSTER`) and the installation id is `kodama-<name>`. See
[named clusters and state](../../deploy/stage/README.md#named-clusters-and-state). The scripts refuse to start if state
exists only in the former default `~/.kube/kodama-stage/state`.

## Install and upgrade the stage

The stage's own release and its dependencies come from `deploy/stage/scripts/deploy.sh` (OpenTofu roots `scw/` and
`shared/stack`, then the Helm releases). It needs digest-pinned images:

```sh
export KODAMA_IMAGES_FILE=~/kodama-stage/images-<commit>.json   # kodama and workload: repository, tag, digest
export KODAMA_ACME_EMAIL=...                                      # Let's Encrypt account contact
export KODAMA_TFSTATE_BUCKET=...                                  # from deploy/stage/scripts/state-bucket.sh
deploy/stage/scripts/deploy.sh --cloud scw --name blue            # variables: ~/kodama-stage/state/clusters/blue/scw.tfvars
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
(`cluster.sh destroy --cloud scw --name <cluster>` removes a whole disposable stage cluster and its state objects, never the state bucket;
`qualify.sh destroy` for a qualification session, which also purges that session's own `qualification/<session>/`
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
| `stage_clean_install` | **Historical core-era scenario.** The current kernel readiness path is `test/e2e/kube-kernel`; the staging suite remains to be migrated and its old receipt is not relabelled |
| `stage_e2e_roundtrip` | submits a pipeline, runs it, and reads the retained log and the output artifact after the workload is gone |
| `stage_restart_reconcile` | cancels a run (workload removed), then kills the server mid-run (`--force --grace-period=0`) and requires the replacement to adopt the same Job and finish with one attempt |
| `stage_loki_log_search` | finds the pipeline's and the control plane's log lines for one attempt by `attempt_id` in Loki through Grafana, and requires Grafana to refuse an unauthenticated Loki query |
| `stage_dev_adapter_refused` | checks the deployed configuration is production with the cloud KMS and no development selection, then starts the **deployed** server specification with one development or test selection at a time (development key file, OIDC fixture, S3 fixture, Process/Podman runtime) and requires a non-zero exit that names the refused adapter |
| `stage_retention_uninstall` | uninstalls, then requires the run records, the artifacts bucket and its objects, and the installation's enabled Key Manager keys to remain. It runs last and leaves the session uninstalled |

The tests run in that order. Go keeps running after a failure, but each test re-establishes what it needs and the gate requires all six (and the four `stage_dev_adapter_refused` selections) to pass.

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

## Qualification record

**Qualified at candidate `0a905ef5989d` (`0a905ef5989db6d1088a9eb1134132fbae94d111`), 2026-10-26.** One `readiness` gate
(857.37s) and one `staging` gate (194.46s) passed on the same fresh session `kl`, the same frozen clean commit, the same
contract and the same values; the `staging` gate verified the readiness receipt's digests before running.

| Item | Value |
| --- | --- |
| Release image | `rg.fr-par.scw.cloud/kodama-stage-scw/kodama@sha256:ddffa673eff974017894b2cba832563ff73ba07e6fa51b347a999026e0fa46bd`, registry revision label = the full commit (verified at the registry by both gates) |
| Test-only helper (readiness only) | `.../stageprobe@sha256:a9961a48dc4c6a9790d799b03b33019448e528fec178508d2da78db824b7d7d9`, same label check |
| Workload image | `.../workload:1@sha256:24cff5ad7765e56c3242014e64e2d911ca02053b92ab9c14f2beeb6519d929ea` (not rebuilt per candidate, not label-verified) |
| Chart tree | `14436ec71a4c143328fbbda9a2265cef94487138` |
| Contract / values | `sha256:a8ae6475be12fdbe30ac060662b5161bfb99463f907931f002cc741d546c4533` / `sha256:7a217477c8ec656e269eeb4c622cc5cf5db3058e227a4b4fd29034893d5e9e68` |
| Receipts | `~/kodama-stage/evidence/T-090-qualification-0a905ef5989d/{readiness,staging}` (read-only, `SHA256SUMS`) |
| Stage deployment | the stage's own release in `kodama-system` runs this image and chart; Alloy tails the control plane, the contract's tenant namespaces and `kodama-q-*` |

Readiness: all seven acceptances and every child case passed, including `kube_secrets_kms/direct_secret_delivery`
([T-107](../tasks/T-107-direct-secret-delivery.md)) and the conflicted writer's Job being removed after its result.
Staging: `stage_clean_install`, `stage_e2e_roundtrip`, `stage_restart_reconcile`, `stage_loki_log_search`,
`stage_dev_adapter_refused` (four selections, each a required child case, each refused with the adapter named) and
`stage_retention_uninstall` passed; nothing was skipped. The staging receipt carries the runner's own completion
evidence (`runner_exit=0`, the staging package finished with pass) and the release image's registry label. The full unit
loop and the Podman regression (all three assembly scenarios and the PostgreSQL contracts) passed on the product code
before the candidate was built; the final commit changed only stage scripts and their tests.

### What the earlier attempts found

Three candidates were qualified before this one, and each exposed something real:

- `4daa4516f044` passed readiness but failed `stage_loki_log_search`: a trivial step ended `execution_timeout`. The control
  plane's new `attempt deadline missed` log and Loki showed the command had run and printed its output, then the
  entrypoint went silent until the deadline aborted it. The entrypoint's HTTP client had no per-request deadline, so one
  stalled request waited out the whole five-minute finalization budget, longer than the step's own deadline. Every
  request is now bounded and the finalization phases are logged ([T-108](../tasks/T-108-execution-finalization-diagnostics.md)).
  The stalled request is inferred from that evidence, not observed. It was an intermittent product defect, which is why
  the gate was not simply rerun.
- `8448adc623b5` passed both gates, but review then showed the acknowledgment-ordering change of the same task would have
  left the Job of every run that the result itself made terminal (the reconciler no longer scans such a run), plus a
  versioned-bucket purge gap, a hand-written-results gap and weak assertions. Those were repaired, so that receipt does
  not cover the merged code.
- `403e8a42e49d` passed all six staging tests but the gate's own runner crashed afterwards (a case-table lookup made
  readiness-only by the review fixes), so the gate failed. The runner's staging path now has an offline test.

The superseded attempts' evidence is kept next to the receipts as `T-090-superseded-*`.

### Limits of this qualification

- O-27, scheduled-only backups and the issuer fixture, as above. No NATS TLS, no PostgreSQL certificate verification, no
  authentication-strength claim ([T-106](../tasks/T-106-production-tls-hardening.md), deferred to a design session).
- Recovery objectives (PITR, restore drills) are not exercised ([T-029](../tasks/T-029-recovery-qualification.md)).
- The execution deadline still covers finalization (uploads and the result submission count against the step's timeout);
  the bounds shorten a stall but do not separate the two budgets.
- A writer step that declares a secret cannot publish source yet, and a step that pins an environment and declares its own
  secrets receives only the environment's ([T-107](../tasks/T-107-direct-secret-delivery.md)).
- Receipts are bound to a commit, a contract and a values file and carry self-declared provenance the runner writes; they
  are evidence for an honest operator, not tamper-proof against a malicious one.
- Staging qualifies the Kubernetes appliance on regular containers for trusted engineers; untrusted-code isolation,
  hosted authority and supported-release claims are not made.
- Any later change to product, chart or stage inputs needs new receipts.

## Known limitations

- O-27 (NATS, issuer) and scheduled-only backups, as above.
- Declared project secrets reach steps directly (ENV and file) since T-107; a writer step that declares a secret cannot publish source yet, and a step that pins an environment and declares its own secrets receives only the environment's.
- `qualify.sh destroy` purges a session's `qualification/<session>/` objects, but the artifacts bucket is **versioned** and the stage runtime credential may not list versions: the tool then deletes the current objects, which only adds delete markers, and prints a WARNING that earlier versions remain. Removing them needs a credential allowed to list and delete object versions, or a lifecycle rule that expires non-current versions under `qualification/` (an infrastructure decision for the owner).
- `qualify.sh destroy` stops on real teardown failures and retries once from recorded state when a resource is already
  gone; a session whose state it still cannot reconcile needs a manual decision, not a forced removal.
- Image provenance is a declared registry label, not a build attestation.
- Loki search depends on Alloy being deployed from this stack revision; an older Alloy tails fewer namespaces and does
  not normalize `attemptId`.
