# Stage & Cloud Deployment (`deploy/stage/`)

Thin OpenTofu roots for **AWS**, **GCP**, and **Scaleway (`scw`)**, one shared Helm stack (`deploy/stage/shared/stack`), and critical-path qualification gate and evidence scripts (`deploy/stage/scripts/`).

## Layout

```text
deploy/stage/
  aws/                    # Dedicated EKS, RDS PostgreSQL, S3 (artifacts + Loki logs), AWS KMS + IRSA
  gcp/                    # Dedicated GKE (Dataplane V2), Cloud SQL PostgreSQL, GCS XML HMAC, Cloud KMS + WI
  scw/                    # Dedicated Kapsule (Cilium), Managed PostgreSQL, Object Storage, Key Manager (fr-par)
  shared/
    stack/                # Shared Helm releases consuming stage_contract from aws, gcp, or scw
      secrets.tf          # Namespaces, credentials, database roles, every Secret
      oauthsonas.tf       # Persona OIDC issuer (digest-pinned) with access code and ingress allowlist
      values/
        kodama.yaml.tftpl # Renders external profile values validated by deploy/helm/kodama/values.schema.json
        nats.yaml.tftpl   # NATS JetStream (TLS + token auth + persistent fileStore)
        loki.yaml.tftpl   # Loki SingleBinary (S3-backed, 30d retention, structured metadata enabled)
        alloy.yaml.tftpl  # Grafana Alloy (non-root Pod API log collection -> Loki structured metadata)
        prometheus.yaml.tftpl # Minimal Prometheus server (drops high-cardinality tenant/attempt labels)
        grafana.yaml.tftpl    # Operator-authenticated Grafana with Loki/Prometheus & pipeline log search dashboard
        gitea.yaml.tftpl      # Gitea Git HTTPS server (SQLite, no SSH, no registration)
        ingress-nginx.yaml    # Ingress controller behind a Scaleway Load Balancer
  qualification/          # One qualification session of the stage: its own database, namespace, keys, Secrets, NATS and DB relays
  images/workload/        # Stage workload image (shell, Git, CA certificates)
  scripts/
    lib.sh                # Contract validator, Helm renderer, FORGE_RESULTS verifier & secret-leak scanner
    deploy.sh             # Cloud apply/validate -> Helm lint -> shared stack apply (secrets first, then charts)
    qualify.sh            # prepare/destroy/show a qualification session (state: ~/kodama-stage/state/qualification/<session>/)
    deps-init.sh          # Seeds Gitea (owner, repository, scoped read/write tokens) during the stack apply
    gate.sh               # Critical-path gate runner (readiness -> staging -> recovery -> platform -> release)
    evidence.sh           # Tamper-evident evidence bundle emitter (manifest.json + SUMMARY.md)
    checks/               # Individual gate checks for T-104, T-090, T-029, T-061 and T-056
    test_scripts.sh       # Offline verification suite for OpenTofu, Helm, ShellCheck and gate failure paths
```

## Qualification sessions

The qualification suites (`test/e2e/kube-full`, `test/e2e/staging`, the Kubernetes backend of
`test/integration/assembly-execution`) never run against the stage's own release. `qualify.sh prepare --session <name>
--contract <stage_contract.json>` (needs `KUBECONFIG`, `KODAMA_IMAGES_FILE` and the operator's `SCW_*`) creates an isolated
installation of the same chart and images: a database on the stage PostgreSQL instance (same roles, granted on that database
only), namespace `kodama-q-<name>` and tenant namespaces `kodama-q-<name>-a` / `-b`, generated keys and Secrets pushed before
any chart installs, a dedicated NATS server, and two database relays. `kq-pgrelay` is the server's database path (scaling it to
zero is a real outage with the database untouched); `kq-pgprobe` is the test process's inspection path (a port forward). Git
tokens, object-store and Key Manager credentials are copied from the stage; the ingress host is `<name>.<ip>.nip.io` with a
Let's Encrypt certificate; the OIDC issuer and audience are the stage's. The session's Helm values and derived contract are
written next to its state; the tests install, upgrade and uninstall the release themselves. `qualify.sh destroy` removes the
release, tenant namespaces, database and all session resources.

By default the session database is created through the Scaleway API (operator `SCW_*` credentials). With
`--database sql` (or `KODAMA_QUAL_DATABASE=sql`) `qualify.sh` creates and drops `kodama_q_<name>` with SQL from a short-lived
Pod in the stage namespace as the stage database admin, so the session needs only `KUBECONFIG` plus
`KODAMA_STAGE_BOOTSTRAP_SECRETS` (a JSON file with `postgres_admin_username` and `postgres_admin_password`). Use the same
mode for `prepare` and `destroy`. The readiness gate's KMS acceptance still needs `SCW_DEFAULT_PROJECT_ID` and accepts
the operator kubeconfig token as the API credential. The test process runs on the operator machine
(`KODAMA_QUAL_SESSION=<name>`, `KUBECONFIG`) through `test/support/stage`.

## Cloud Infrastructure & Staging vs. Production Retention

Each cloud root (`aws`, `gcp`, `scw`) provisions a dedicated Kubernetes cluster, private managed PostgreSQL, separated S3-compatible buckets for Kodama artifacts and Loki logs, and two symmetric cloud KMS keys (`v1` current and `v2` rotation target). Each root emits the identical non-secret `stage_contract` output consumed by `shared/stack`.

| Cloud | Kubernetes | Managed PostgreSQL | Staging backup default (`environment_tier=staging`) | O-11 30d PITR / 15m RPO (`qualifies_o11_pitr`) |
| --- | --- | --- | --- | --- |
| `aws` | Amazon EKS | Amazon RDS PostgreSQL 17 | `db.t4g.medium`, 7-day PITR | `true` when `environment_tier=production` (>= 30d PITR) |
| `gcp` | GKE Standard | Cloud SQL PostgreSQL 17 | `ENTERPRISE`, 7-day PITR | `true` when `environment_tier=production` (`ENTERPRISE_PLUS`, >= 30d PITR) |
| `scw` | Kapsule (`fr-par`) | Scaleway RDB PostgreSQL 17 | `DB-DEV-S`, 24h scheduled backups, 7-day retention | `false` (Scaleway RDB provides scheduled backups/snapshots, not continuous WAL PITR) |

Lower staging database targets are accepted by the **T-090 (`staging`)** gate and recorded in the evidence manifest, but are refused by the **T-029 (`recovery`)** and **T-056 (`release`)** gates.

Fresh installations default to Kubernetes `1.35` on AWS/GCP and `1.37.1` on Scaleway (`kubernetes_version`; Kapsule needs a full `x.y.z`) and PostgreSQL `17` (`db_engine_version`). Existing installations must pin `kubernetes_version` and `db_engine_version` in `terraform.tfvars`: advance Kubernetes one minor version at a time (control plane then worker node pool), and only perform a PostgreSQL major version upgrade after taking a verified snapshot/PITR checkpoint and explicitly setting `db_allow_major_version_upgrade = true` (AWS RDS).

## Pipeline Log Search Architecture

Kodama's built-in live Kubernetes log proxy and retained S3 log artifacts operate without Loki ([P-10](../../docs/decisions.md#implementation-baseline)). For operator search across control-plane and tenant pipeline Pods:

1. **Grafana Alloy** tails Pod logs over the Kubernetes API (`loki.source.kubernetes`) across `kodama-system` and `kodama-tenant-*` without privileged containers, `hostPath` mounts, or root access.
2. **Structured metadata** attaches `job_name`, `pod`, and `attempt_id` (parsed from `kodama step` JSON output) in Loki while dropping them from stream/metric labels so cardinality stays bounded ([observability](../../docs/operations/observability-and-audit.md)).
3. **Grafana** is operator-authenticated (`auth.anonymous.enabled=false`) and pre-provisions the **Kodama Pipeline & Control-Plane Logs** dashboard (`{namespace=~"$namespace"} | attempt_id=~"$attempt_id.*" |= "$search"`).

## Usage

Validate formatting, rendered Helm schemas (`aws`, `gcp`, `scw`), stack validation, chart/Secret key drift and gate pass/fail paths locally:

```bash
./deploy/stage/scripts/test_scripts.sh
```

Validate or deploy a specific cloud (`aws`, `gcp`, or `scw`):

```bash
# Offline validation (no cloud credentials or cluster required)
./deploy/stage/scripts/deploy.sh --cloud aws --validate-only

# Full deployment against configured OpenTofu backend and KUBECONFIG
./deploy/stage/scripts/deploy.sh --cloud aws
```

Secrets and database credentials are generated by `deploy/stage/shared/stack` itself and pushed into
Kubernetes before any chart installs, so they are stable across applies and live in OpenTofu state. Public ingress
certificates (Kodama, Dex, Gitea, Grafana) are issued and renewed by cert-manager from Let's Encrypt over HTTP-01
(`KODAMA_ACME_EMAIL` is required; the nip.io hostnames share that domain's Let's Encrypt rate limits, so a real
domain is a one-line change of `ingress_host`). There is no installation CA in the stage. State is kept
outside the checkout (`$STAGE_STATE_DIR`, default `~/kodama-stage/state`, mode 0700 and 0600 files): it holds
database, API-key and signing material, so protect it like a credential store. Image coordinates come from
`$KODAMA_IMAGES_FILE` (JSON with `kodama` and `workload` repository/tag/digest) or the `KODAMA_IMAGE_*` and
`KODAMA_WORKLOAD_IMAGE_*` variables; placeholder digests are refused by the gates.

Scaleway database roles are created through the platform API (`scaleway_rdb_user`/`scaleway_rdb_privilege`) because
the managed admin user cannot grant on the platform-owned database; the stack falls back to a SQL Job when a cloud
root does not hand over role passwords.

**Recorded deviation (O-27, T-106).** NATS runs plaintext (`nats://`, token authentication) under chart
`profile: development` (stack variable `nats_tls=false`), because the server's NATS and OIDC clients trust only the
system CA store, so a private CA cannot secure NATS or the OIDC issuer yet (T-106). `KODAMA_ENV=production` still refuses every development
and test adapter. Evidence from this stage must name the profile and cannot claim NATS TLS. The OIDC issuer therefore
uses a publicly trusted (Let's Encrypt) certificate.

**Identity fixture (O-27).** The standalone authority's issuer is the Optimi `oauthsonas` persona provider, pinned by
digest. It is a development/test fixture: persona selection replaces authentication, so anyone who completes its
authorize flow can act as any persona (a persona token for `admin` becomes the tenant administrator). The stage
therefore gates the authorize flow with a random access code (Secret `kodama-oauthsonas-access` in `kodama-deps`),
restricts `/oauth2`, `/userinfo` and `/logout` at the ingress to the operator CIDRs in
`KODAMA_ISSUER_ALLOWED_CIDRS` (`deploy.sh` defaults it to this machine's current public address; list every
egress address if you have several), and leaves discovery and JWKS open. The ingress sees real client addresses
because ingress-nginx and the Scaleway load balancer use PROXY protocol v2; in-cluster callers appear as node
addresses. It runs as one replica with all state in memory, so a restart invalidates every token. Do not treat it
as an identity system; hosted authority (T-061) replaces it.

## Repeatable readiness workflow

Qualification remains on the task worktree, **before merge**. Finish edits and scoped checks, review the candidate,
and commit before starting a gate. The gate refuses a dirty checkout and fails if HEAD, tracked/untracked source,
contract or values change during the run. Do not commit a report while the gate is running. A later report commit
does not retarget a historical receipt; downstream gates still require matching candidate/configuration provenance.

### Build once from committed source

The product image uses the root `Containerfile`. A separate **test-only** helper image uses
`test/support/stage/probe/Containerfile`; it contains the HTTPS Kubernetes fault proxy and in-cluster KMS procedure,
not a different product server. Both accept the existing OCI `REVISION` build argument. Set `KODAMA_STAGE_IMAGE_TAG`
and `KODAMA_STAGE_PROBE_TAG` to operator-selected registry tags before these commands:

```sh
test -z "$(git status --porcelain)"
candidate="$(git rev-parse HEAD)"
podman build -f Containerfile --build-arg "REVISION=$candidate" \
  --label "org.opencontainers.image.revision=$candidate" -t "$KODAMA_STAGE_IMAGE_TAG" .
podman build --ignorefile test/support/stage/probe/build.ignore \
  -f test/support/stage/probe/Containerfile --build-arg "REVISION=$candidate" \
  --label "org.opencontainers.image.revision=$candidate" -t "$KODAMA_STAGE_PROBE_TAG" .
```

The explicit `--label` is required, not decoration: build-arg values are not part of the layer cache key, so a
rebuild of unchanged binaries reuses the earlier image's `LABEL` layer and silently keeps its revision (a
helper built from a dirty tree once reached a gate labelled `dirty-debug`). The gate refuses that: its preflight
runs `scripts/verify_image_revision.py`, which reads the **registry's** manifest and config for the release image in
the values file and for `KODAMA_STAGE_PROBE_IMAGE`, checks both digests, and requires
`org.opencontainers.image.revision` to equal the full candidate commit. Run it yourself after publishing:

```sh
deploy/stage/scripts/verify_image_revision.py --commit "$(git rev-parse HEAD)" --values "$KODAMA_STAGE_VALUES" "$KODAMA_STAGE_PROBE_IMAGE"
```

This is a **declared** revision, not an attestation: it proves the registry image was labelled with the candidate,
not that the label is true. What makes it trustworthy is the rule that the candidate was clean when built, the
build commands above, and the evidence that records the registry digests. A build-time attestation (or a
rebuild-and-compare-digest step) would be a stronger control and is not implemented.

The verified revisions are recorded in the receipt (`suite_provenance.image_revisions`). The workload image is a
separate artifact and is not rebuilt per candidate. The registry is read anonymously (public-read namespace); a
private registry would need a documented credentialed path first.

Publish those images to the authorized stage registry and record registry manifest digests, not local image IDs.
Keep credentials in the operator's registry auth store. Put the product digest in the external `KODAMA_IMAGES_FILE`
before preparing the session; set `KODAMA_STAGE_PROBE_IMAGE=repository@sha256:...` to the helper's published digest.
Tags alone are rejected. These instructions do not authorize a Git push or publication to the main release channel.

### Preflight, then one full run

Use one state root: qualification resolves `KODAMA_QUAL_STATE_DIR`, then `STAGE_STATE_DIR`, then
`~/kodama-stage/state`. Existing state in the old `~/.kube/` location is not moved: select it explicitly if needed.
Set both root variables consistently when deploying and qualifying. Keep kubeconfig, tfvars, Terraform state, registry
auth and provider credentials outside the checkout. Set `SCW_DEFAULT_PROJECT_ID` and operator `SCW_*` credentials;
the probe itself mounts only the deployment's KMS credential, not the operator's. IAM administration is needed to
create and remove a throwaway denied application/API key with **no policy**; missing permission is a blocker, not
a reason to substitute a fabricated token or treat `KeyUnavailable` as a successful denial.

Prepare a **fresh session** for each final clean-install attempt. `TestKubeChartInstall` refuses an existing release
or migrated database rather than mislabeling an upgrade as a clean install. Use `qualify.sh prepare --session ...
--contract ...` with the original base stage contract, not another session's derived contract. Keep session `kb`
until a passing gate, as instructed by the owner. Focused debugging may reuse an installed session, but its results
are not combined into the final receipt.

Set `KODAMA_STAGE_CONTRACT` and `KODAMA_STAGE_VALUES` to that session's `contract.json` and `values.yaml`, and
`KODAMA_QUAL_SESSION` to its name. The gate checks byte equality against session inputs, installation identity,
namespace reachability and the selected cluster endpoint; it holds a per-session lock while running.

```sh
deploy/stage/scripts/gate.sh --gate readiness --cloud scw \
  --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES" --preflight
KODAMA_GO_TEST_TIMEOUT=60m deploy/stage/scripts/gate.sh --gate readiness --cloud scw \
  --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
```

Preflight issues **no receipt**. A full invocation runs chart installation first, then the remaining scenarios,
sequentially on that session, and requires all seven exact package/test/backend matches plus the mandatory child
cases in `scripts/readiness-cases.json`. No cross-run stitching or automatic retry-to-green. Skips, missing package
completion, required-child skips, process failures and source/configuration drift cannot pass. Output is streamed
unbuffered and ends with the runner exit, elapsed time and suite outcome; evidence records the helper image,
chart tree, contract/values digests and actual fault configuration. O-27 and scheduled-only backups remain deviations.

For agents, execute the **entire** long-running shell command with the harness's background option and await its
completion notification. Do not add `nohup`, shell `&`, a detached child, or `pgrep` polling: these can report shell
completion before tests finish or match the waiter's own command. Never pipe checks into `tail` without preserving
the original exit status. Run local unit/lint checks before freezing the candidate, not commits during qualification.

### Focused checks and Podman regression

Offline hardening checks do not use stage credentials:

```sh
go test -race -count=1 ./test/support/stage/...
go test -race -count=1 -run 'TestKeyCleanup|TestCreateKey' ./test/e2e/kube-full/
python3 -B deploy/stage/scripts/test_runner.py
deploy/stage/scripts/test_scripts.sh
make lint test docs-check
```

The Podman regression needs the **services**, not cluster, stack. Keep the backend variable unset and use an
isolated scope; cleanup is in the same shell command so a test failure still removes only its resources:

```bash
set -euo pipefail
trap 'make dev-down DEV_SCOPE=t-104-local' EXIT
make dev-up DEV_SCOPE=t-104-local DEV_STACK=services
env -u KODAMA_ASSEMBLY_BACKEND make test-integration DEV_SCOPE=t-104-local \
  INTEGRATION_PKGS=./test/integration/assembly-execution/...
```

### Fault boundaries and cleanup

PX-A02 is explicitly **fault-instrumented**: the helper listens on loopback HTTPS, trusts the real cluster CA
upstream and receives the server's forwarded service-account token. An ephemeral loopback certificate is projected
through a temporary Secret and `KODAMA_KUBE_CA_FILE`; neither TLS hop uses insecure verification. Only the armed
Job create is intercepted. Its successful acknowledgment and read-back are withheld; Pod creation is held to prevent
callbacks confirming dispatch before the server crashes. A forced Pod replacement starts a transparent helper,
and the server must confirm the original Job UID, retain one attempt/grant/Pod, then finish. The original Pod template
and admission policies are restored even after failure. The historical HTTP helper must not be reapplied.

KMS acceptance includes real project-secret consumption, a server crash/restart and consumption of the persisted
encrypted secret. An isolated Job, using the deployment's service account and mounted KMS credential, then checks
roundtrip, context/ciphertext/version tampering, retained-key rotation, a fresh executable restoring an old envelope,
genuine permission denial, bounded startup/runtime outage and recovery. Transport fault injection is confined to
the probe executable. Retained installation keys/mappings are untouched. Throwaway keys request `unprotected:true`
(the response must have `protected:false`), get unique names and immediate checked cleanup. The protected historical
probe key stays untouched unless the owner separately authorizes removal.

After a passing gate, retain the safe evidence bundle before `qualify.sh destroy --session ... --contract ...`
(use the original base stage contract). Remove `kb` and only this task's qualification sessions. Verify namespace
and session-labeled admission-policy absence; never delete another task's objects. Credentials, private keys, secret
values, wrapped bytes and provider response bodies must not be printed or copied into evidence. A failed cleanup is
a visible failure, not a silent success. Test failures run registered cleanup; forcibly killing the test process
can leave fixtures behind, so inspect the exact session's labeled resources before retrying. Independent review
remains required before merge; do not push Git changes.

Run critical-path qualification gates and emit evidence bundles:

```bash
# Single gate (T-104 readiness, T-090 staging, T-029 recovery, T-061 platform, or T-056 release)
./deploy/stage/scripts/gate.sh --gate readiness --cloud aws --contract stage_contract.json

# Complete critical path in dependency order
./deploy/stage/scripts/gate.sh --gate critical-path --cloud aws --contract stage_contract.json
```
