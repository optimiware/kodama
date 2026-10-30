# Stage & Cloud Deployment (`deploy/stage/`)

Thin OpenTofu roots for **AWS**, **GCP**, and **Scaleway (`scw`)**, one shared Helm stack (`deploy/stage/shared/stack`), and critical-path qualification gate and evidence scripts (`deploy/stage/scripts/`). The Helm charts live in [`deploy/charts/`](../charts/kodama/README.md) (`kodama`, and the stage-only `cluster-issuer`).

Stage clusters are **disposable and named**: any number run side by side (`--name blue`, `--name green`), each with its own installation id, state, kubeconfig and qualification sessions. Start with the [deployment runbook](#deployment-runbook), which begins with the one-time creation of the state bucket; [Named clusters and state](#named-clusters-and-state) is the reference for where everything lives.

## Deployment runbook

Stage clusters are disposable and named. This is the whole path from nothing to a running cluster and back; the sections
after it are reference. It was written against the scripts and their offline tests (`scripts/test_scripts.sh`): the
first live Scaleway run has not been recorded yet.

### Prerequisites

| Need | For |
| --- | --- |
| `tofu` (OpenTofu >= 1.8; developed with 1.13), `helm`, `kubectl`, `python3`, `curl` | every deployment |
| `scw` CLI, authenticated for the target project | creating the state bucket (step 1), once |
| `SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID` | the Scaleway provider **and** the state backend (the key needs Object Storage read/write and the rights to create the cluster resources) |
| `KODAMA_IMAGES_FILE` (JSON with `kodama` and `workload` repository/tag/digest) | the release; placeholder digests are refused ([build the images](#build-once-from-committed-source)) |
| `KODAMA_ACME_EMAIL` | Let's Encrypt account contact |
| `go` | qualification sessions only |

Keep all of these, the kubeconfig and the tfvars outside the checkout. Check the scripts without any cloud access first:

```bash
deploy/stage/scripts/test_scripts.sh
deploy/stage/scripts/deploy.sh --cloud scw --validate-only
```

### 1. Create the state bucket (once per Scaleway project)

OpenTofu state for the `scw` roots, the shared stack and the qualification sessions lives in one Scaleway Object Storage
bucket (S3 backend with native `use_lockfile` locking). Create it **once, outside OpenTofu**, with the `scw` CLI: the
bucket is private and versioned, and because no OpenTofu root manages it and no script deletes it, no `tofu destroy` can
reach the state. Versioning keeps every earlier state object, so an overwritten or removed state can be restored
([step 7](#7-recover-a-state-object-or-a-stuck-lock)).

```bash
export SCW_ACCESS_KEY=... SCW_SECRET_KEY=... SCW_DEFAULT_PROJECT_ID=...
eval "$(deploy/stage/scripts/state-bucket.sh)"      # creates the bucket if needed, then exports KODAMA_TFSTATE_BUCKET
echo "$KODAMA_TFSTATE_BUCKET"                       # kodama-tfstate-<first 8 characters of the project id>
```

Bucket names are global to Scaleway: to choose another name, export `KODAMA_TFSTATE_BUCKET` before running the script.
The script is safe to run again: it creates nothing that exists, turns versioning on if it is off and refuses to go on if
it cannot. `state-bucket.sh show` prints the name and the versioning status without changing anything. Put the
`KODAMA_TFSTATE_BUCKET` export in your shell profile; every other script needs it. Restrict access to the bucket like a
credential store: the state holds database, API-key and signing material.

### 2. Configure a cluster

Each cluster has a name (1-16 lowercase letters, digits and `-`, starting with a letter) and its own variables file. The
installation id is always `kodama-<name>`; an `installation_id` in the file is overridden.

```bash
export KODAMA_STAGE_CLUSTER=blue                     # or pass --name blue to every script
mkdir -p ~/kodama-stage/state/clusters/blue
cp deploy/stage/scw/terraform.tfvars.example ~/kodama-stage/state/clusters/blue/scw.tfvars
```

Edit `scw.tfvars`:

- `project_id` and `ingress_host`: the host must resolve to the ingress load balancer. The `.example.invalid` placeholder
  is refused. The load balancer's address is known only after the first stack apply, so with a `nip.io` name pick a
  temporary host, deploy, find the load balancer with `kubectl --kubeconfig ~/kodama-stage/state/clusters/blue/kubeconfig -n
  ingress-nginx get service`, set `ingress_host` to the final name and deploy again. A real DNS name pointing at the load
  balancer avoids the second pass and the shared `nip.io` Let's Encrypt rate limits.
- `operator_user_ids`: your Scaleway IAM user UUID. The bucket policies list only the application identities and these
  users, so an operator left out is locked out of refresh, update and destroy.
- Optionally `kubernetes_version`, `node_*`, `db_node_type` and `environment_tier` (`staging` is the default and the only
  tier `cluster.sh destroy` accepts).

The scripts refuse a `terraform.tfvars` placed next to a root, because it would apply to every cluster.

### 3. Deploy

```bash
export KODAMA_IMAGES_FILE=~/kodama-stage/images.json
export KODAMA_ACME_EMAIL=ops@example.com
deploy/stage/scripts/deploy.sh --cloud scw --name blue
```

In order, the script: initializes the cluster's state in the bucket (`scw/blue/infra.tfstate`) and applies the `scw` root;
writes the cluster's kubeconfig to `~/kodama-stage/state/clusters/blue/kubeconfig`; **checks that this kubeconfig selects the
cluster named in the contract**; renders and lints the Kodama Helm values; then applies the shared stack
(`scw/blue/stack.tfstate`), which pushes every Secret before any chart installs. `KODAMA_ISSUER_ALLOWED_CIDRS` (comma
separated) defaults to this machine's public address; see the [identity fixture](#usage). Add `--contract-out` and
`--values-out` to keep the contract and values for the qualification gates.

Re-running the command is the way to change anything: edit the tfvars or point `KODAMA_IMAGES_FILE` at new images.

### 4. Check the result

```bash
export KUBECONFIG=~/kodama-stage/state/clusters/blue/kubeconfig
kubectl get nodes
kubectl get pods --all-namespaces
deploy/stage/scripts/cluster.sh list                 # every cluster with state in the bucket
```

Open `https://<ingress_host>` for Kodama and `https://grafana.<ingress_host>` for logs once cert-manager has issued the
certificates (a few minutes). Use a dedicated shell or `--kubeconfig` per cluster: an ambient `KUBECONFIG` of another
cluster is never used by the scripts, but it is by your own `kubectl`.

### 5. Run another cluster

A second cluster is another name with its own variables; nothing is shared with the first, and the two can be deployed at the
same time from one checkout.

```bash
mkdir -p ~/kodama-stage/state/clusters/green
cp deploy/stage/scw/terraform.tfvars.example ~/kodama-stage/state/clusters/green/scw.tfvars   # distinct ingress_host
deploy/stage/scripts/deploy.sh --cloud scw --name green
```

Every Scaleway resource is prefixed with `kodama-green`, each cluster gets its own VPC, and the state lives under
`scw/green/`. The project's quotas (Kapsule clusters, managed databases, load balancers, IPs) apply to the sum of all
clusters. See [Named clusters and state](#named-clusters-and-state) for where each file and state object lives.

### 6. Upgrade

Deploy again with the new images file; the chart's migration Job runs before the server rolls. Advance
`kubernetes_version` one minor at a time and only raise the PostgreSQL major version with a verified snapshot (see the
[retention notes](#cloud-infrastructure--staging-vs-production-retention)).

### 7. Recover a state object or a stuck lock

Every write to the bucket keeps the earlier version. To roll a cluster's state back, list the versions of its object with
any S3 client (the Scaleway endpoint is `https://s3.fr-par.scw.cloud`, path-style) and copy the wanted version over the current
one; for example with the AWS CLI:

```bash
export AWS_ACCESS_KEY_ID="$SCW_ACCESS_KEY" AWS_SECRET_ACCESS_KEY="$SCW_SECRET_KEY" AWS_DEFAULT_REGION=fr-par
aws --endpoint-url https://s3.fr-par.scw.cloud s3api list-object-versions \
  --bucket "$KODAMA_TFSTATE_BUCKET" --prefix scw/blue/
```

A crashed run can leave a lock object, `<state key>.tflock`, next to its state. Once you are sure no run is active for that
cluster, remove exactly that key:

```bash
deploy/stage/scripts/state_s3.py delete scw/blue/infra.tfstate.tflock
```

### 8. Tear down

```bash
deploy/stage/scripts/cluster.sh destroy --cloud scw --name blue     # asks you to type the name; --yes skips the prompt
```

This destroys the cluster's infrastructure (the cluster and its load balancers and volumes, the database, the buckets, the
Key Manager keys and the registry namespace), which takes everything inside the cluster with it, including its
qualification sessions. It then removes the cluster's state objects (earlier versions stay in the bucket) and its local
files, keeping `scw.tfvars` so the name can be deployed again. It never touches the state bucket or another cluster, and it
refuses a cluster whose tfvars say `environment_tier = "production"`. Deleting the state bucket itself is deliberately
manual: empty every version, then use the `scw` CLI.

### Troubleshooting

| Message | Cause and fix |
| --- | --- |
| `KODAMA_TFSTATE_BUCKET is not set` | Run step 1, or export the name it printed. |
| `SCW_ACCESS_KEY and SCW_SECRET_KEY are required` | State is in Scaleway Object Storage, so even `aws` and `gcp` stack deployments need them. |
| `could not create bucket ...` | Bucket names are global: choose another with `KODAMA_TFSTATE_BUCKET`. |
| `versioning could not be enabled ...` | The credential cannot update the bucket; fix its Object Storage permissions. The scripts will not use an unversioned bucket. |
| `missing .../<cloud>.tfvars` | Copy the example for the cluster (step 2). |
| `terraform.tfvars would apply to every cluster` | Move it to `~/kodama-stage/state/clusters/<name>/<cloud>.tfvars`. |
| `cluster name '...' is invalid` | 1-16 lowercase letters, digits or `-`, starting with a letter, not ending with `-`. |
| `kubeconfig ... selects cluster ..., but the contract is for ...` | `KUBECONFIG` points at another cluster; unset it so the script uses the cluster's own file. |
| `values file ... contains placeholder host` or `placeholder image digest` | Set a real `ingress_host` and real image digests. |
| `Error acquiring the state lock` | Another run is active for that cluster, or one crashed: see step 7. |

## Layout

```text
deploy/stage/
  aws/                    # Dedicated EKS, RDS PostgreSQL, S3 (artifacts + Loki logs), AWS KMS + IRSA
  gcp/                    # Dedicated GKE (Dataplane V2), Cloud SQL PostgreSQL, GCS XML HMAC, Cloud KMS + WI
  scw/                    # Dedicated Kapsule (Cilium), Managed PostgreSQL, Object Storage, Key Manager (fr-par)
  shared/
    stack/                # Shared Helm releases consuming stage_contract from aws, gcp, or scw (state always in the bucket)
      secrets.tf          # Namespaces, credentials, database roles, every Secret
      oauthsonas.tf       # Persona OIDC issuer (digest-pinned) with access code and ingress allowlist
      values/
        kodama.yaml.tftpl # Renders external profile values validated by deploy/charts/kodama/values.schema.json
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
    state-bucket.sh       # Creates the versioned OpenTofu state bucket once, with the scw CLI (never deleted by any script)
    state_s3.py           # Lists and deletes state objects in that bucket (stdlib SigV4; the scw CLI cannot list objects)
    cluster.sh            # list the named clusters; destroy one (infrastructure, state objects, local files)
    deploy.sh             # Cloud apply/validate -> Helm lint -> shared stack apply (secrets first, then charts)
    qualify.sh            # prepare/destroy/show a qualification session (files: <state dir>/clusters/<name>/qualification/<session>/)
    deps-init.sh          # Seeds Gitea (owner, repository, scoped read/write tokens) during the stack apply
    gate.sh               # Critical-path gate runner (readiness -> staging -> recovery -> platform -> release)
    evidence.sh           # Tamper-evident evidence bundle emitter (manifest.json + SUMMARY.md)
    checks/               # Individual gate checks for T-104, T-090, T-029, T-061 and T-056
    test_scripts.sh       # Offline verification suite for OpenTofu, Helm, ShellCheck and gate failure paths
```

## Qualification sessions

The qualification suites (`test/e2e/kube-full`, `test/e2e/staging`, the Kubernetes backend of
`test/integration/assembly-execution`) never run against the stage's own release. `qualify.sh prepare --name <cluster> --session <name>
--contract <stage_contract.json>` (needs `KODAMA_IMAGES_FILE` and the operator's `SCW_*`; the kubeconfig defaults to the cluster's own) creates an isolated
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
(`KODAMA_QUAL_SESSION=<name>`, `KODAMA_STAGE_CLUSTER=<cluster>`, `KUBECONFIG`) through `test/support/stage`.

## Named clusters and state

`--name <cluster>` (or `KODAMA_STAGE_CLUSTER`) selects the cluster in `deploy.sh`, `qualify.sh`, `gate.sh` and `cluster.sh`;
the [runbook](#deployment-runbook) shows the workflow. Where everything lives (`<state dir>` is `$STAGE_STATE_DIR`, default
`~/kodama-stage/state`, mode 0700 with 0600 files):

| Item | Location |
| --- | --- |
| Installation id (names every cloud resource, binds envelope encryption) | `kodama-<name>`, always set by the script; a value in the tfvars is overridden |
| Variables | `<state dir>/clusters/<name>/<cloud>.tfvars` (start from `<cloud>/terraform.tfvars.example`) |
| Operator kubeconfig | `<state dir>/clusters/<name>/kubeconfig`, written from the `scw` root's output |
| State objects (in the bucket) | `scw/<name>/infra.tfstate`, `<cloud>/<name>/stack.tfstate`, `scw/<name>/qualification/<session>.tfstate`, each with a `.tflock` object while a run holds it |
| Qualification sessions | `<state dir>/clusters/<name>/qualification/<session>/` |
| OpenTofu data directories | `<state dir>/clusters/<name>/tfdata/<root>` and `.../<session>/tfdata` |

Every cluster and session initializes OpenTofu in its own data directory (`TF_DATA_DIR`), so clusters can be deployed from
one checkout at the same time. Before the stack or a session is applied, the kubeconfig is checked against the contract's
cluster endpoint, so an ambient `KUBECONFIG` for another cluster cannot receive the apply.

The shared stack is cloud-neutral, so its state is in the bucket even for `aws` and `gcp` (those deployments also need the
`SCW_*` credentials, and have no generated kubeconfig: set `KUBECONFIG` to the cluster's own). The `aws` and `gcp`
infrastructure roots keep a local state file, `<state dir>/clusters/<name>/<cloud>.tfstate`, which is **not** backed up: protect it.

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

Validate or deploy a specific cloud (`aws`, `gcp`, or `scw`); the [runbook](#deployment-runbook) has the full first-time sequence:

```bash
# Offline validation (no cloud credentials or cluster required)
./deploy/stage/scripts/deploy.sh --cloud aws --validate-only

# Full deployment of the cluster named "blue" (state bucket and per-cluster tfvars: see the deployment runbook)
./deploy/stage/scripts/deploy.sh --cloud aws --name blue
```

Secrets and database credentials are generated by `deploy/stage/shared/stack` itself and pushed into
Kubernetes before any chart installs, so they are stable across applies and live in OpenTofu state (the state bucket). Public ingress
certificates (Kodama, Dex, Gitea, Grafana) are issued and renewed by cert-manager from Let's Encrypt over HTTP-01
(`KODAMA_ACME_EMAIL` is required; the nip.io hostnames share that domain's Let's Encrypt rate limits, so a real
domain is a one-line change of `ingress_host`). There is no installation CA in the stage. Operator files are kept
outside the checkout (`$STAGE_STATE_DIR`, default `~/kodama-stage/state`, mode 0700 and 0600 files) and the OpenTofu state
is in the state bucket: both hold database, API-key and signing material, so protect them like a credential store. Image coordinates come from
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

Use one state root and one cluster name: qualification looks in `<state dir>/clusters/<name>/qualification/` (`<state dir>` is
`STAGE_STATE_DIR`, default `~/kodama-stage/state`; `KODAMA_STAGE_CLUSTER` or `--name` selects the cluster; an explicit
`KODAMA_QUAL_STATE_DIR` replaces the directory holding `qualification/`). Existing state in the old `~/.kube/` location is
not moved: select it explicitly if needed. Keep kubeconfig, tfvars, Terraform state, registry
auth and provider credentials outside the checkout. Set `SCW_DEFAULT_PROJECT_ID` and operator `SCW_*` credentials;
the probe itself mounts only the deployment's KMS credential, not the operator's. IAM administration is needed to
create and remove a throwaway denied application/API key with **no policy**; missing permission is a blocker, not
a reason to substitute a fabricated token or treat `KeyUnavailable` as a successful denial.

Prepare a **fresh session** for each final clean-install attempt. `TestKubeChartInstall` refuses an existing release
or migrated database rather than mislabeling an upgrade as a clean install. Use `qualify.sh prepare --name <cluster> --session ...
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

After a passing gate, retain the safe evidence bundle before `qualify.sh destroy --name <cluster> --session ... --contract ...`
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
