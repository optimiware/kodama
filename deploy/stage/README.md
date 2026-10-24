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
      values/
        kodama.yaml.tftpl # Renders external profile values validated by deploy/helm/kodama/values.schema.json
        nats.yaml.tftpl   # NATS JetStream (TLS + token auth + persistent fileStore)
        loki.yaml.tftpl   # Loki SingleBinary (S3-backed, 30d retention, structured metadata enabled)
        alloy.yaml.tftpl  # Grafana Alloy (non-root Pod API log collection -> Loki structured metadata)
        prometheus.yaml.tftpl # Minimal Prometheus server (drops high-cardinality tenant/attempt labels)
        grafana.yaml.tftpl    # Operator-authenticated Grafana with Loki/Prometheus & pipeline log search dashboard
  scripts/
    lib.sh                # Contract validator, Helm renderer, FORGE_RESULTS verifier & secret-leak scanner
    bootstrap.sh          # Idempotent PostgreSQL role separation & Kubernetes Secret provisioning
    deploy.sh             # Cloud apply/validate -> bootstrap -> Helm lint -> shared stack apply
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

Validate formatting, rendered Helm schemas (`aws`, `gcp`, `scw`), bootstrap dry-run, and gate pass/fail paths locally:

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

Run critical-path qualification gates and emit evidence bundles:

```bash
# Single gate (T-104 readiness, T-090 staging, T-029 recovery, T-061 platform, or T-056 release)
./deploy/stage/scripts/gate.sh --gate readiness --cloud aws --contract stage_contract.json

# Complete critical path in dependency order
./deploy/stage/scripts/gate.sh --gate critical-path --cloud aws --contract stage_contract.json
```
