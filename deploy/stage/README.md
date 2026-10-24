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
      secrets.tf          # Namespaces, installation CA and leaf certificates, credentials, database roles, every Secret
      values/
        kodama.yaml.tftpl # Renders external profile values validated by deploy/helm/kodama/values.schema.json
        nats.yaml.tftpl   # NATS JetStream (TLS + token auth + persistent fileStore)
        loki.yaml.tftpl   # Loki SingleBinary (S3-backed, 30d retention, structured metadata enabled)
        alloy.yaml.tftpl  # Grafana Alloy (non-root Pod API log collection -> Loki structured metadata)
        prometheus.yaml.tftpl # Minimal Prometheus server (drops high-cardinality tenant/attempt labels)
        grafana.yaml.tftpl    # Operator-authenticated Grafana with Loki/Prometheus & pipeline log search dashboard
        dex.yaml.tftpl        # Dex OIDC issuer (standalone authority) with static personas
        gitea.yaml.tftpl      # Gitea Git HTTPS server (SQLite, no SSH, no registration)
        ingress-nginx.yaml    # Ingress controller behind a Scaleway Load Balancer
  images/workload/        # Stage workload image (shell, Git, CA certificates)
  scripts/
    lib.sh                # Contract validator, Helm renderer, FORGE_RESULTS verifier & secret-leak scanner
    deploy.sh             # Cloud apply/validate -> Helm lint -> shared stack apply (secrets first, then charts)
    deps-init.sh          # Seeds Gitea (owner, repository, scoped read/write tokens) during the stack apply
    gate.sh               # Critical-path gate runner (readiness -> staging -> recovery -> platform -> release)
    evidence.sh           # Tamper-evident evidence bundle emitter (manifest.json + SUMMARY.md)
    checks/               # Individual gate checks for T-104, T-090, T-029, T-061 and T-056
    test_scripts.sh       # Offline verification suite for OpenTofu, Helm, ShellCheck and gate failure paths
```

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

Secrets, certificates and database roles are generated by `deploy/stage/shared/stack` itself and pushed into
Kubernetes before any chart installs, so they are stable across applies and live in OpenTofu state. State is kept
outside the checkout (`$STAGE_STATE_DIR`, default `~/.kube/kodama-stage/state`, mode 0700 and 0600 files): it holds
database, API-key and signing material, so protect it like a credential store. Image coordinates come from
`$KODAMA_IMAGES_FILE` (JSON with `kodama` and `workload` repository/tag/digest) or the `KODAMA_IMAGE_*` and
`KODAMA_WORKLOAD_IMAGE_*` variables; placeholder digests are refused by the gates.

Scaleway database roles are created through the platform API (`scaleway_rdb_user`/`scaleway_rdb_privilege`) because
the managed admin user cannot grant on the platform-owned database; the stack falls back to a SQL Job when a cloud
root does not hand over role passwords.

**Recorded deviation (O-27, T-106).** NATS runs plaintext (`nats://`, token authentication) under chart
`profile: development` (stack variable `nats_tls=false`), because the server's NATS and OIDC clients trust only the
system CA store and no private-CA trust design exists yet. `KODAMA_ENV=production` still refuses every development
and test adapter. Evidence from this stage must name the profile and cannot claim NATS TLS. The OIDC issuer must
present a certificate the server's system trust store accepts; see the open items in the stage handoff.

Run critical-path qualification gates and emit evidence bundles:

```bash
# Single gate (T-104 readiness, T-090 staging, T-029 recovery, T-061 platform, or T-056 release)
./deploy/stage/scripts/gate.sh --gate readiness --cloud aws --contract stage_contract.json

# Complete critical path in dependency order
./deploy/stage/scripts/gate.sh --gate critical-path --cloud aws --contract stage_contract.json
```
