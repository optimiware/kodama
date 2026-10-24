#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy/stage/scripts/bootstrap.sh --contract <stage_contract.json> [--secrets <bootstrap_secrets.json>] [--dry-run]

Prepares the namespaces, distinct PostgreSQL roles (kodama_migration, kodama_runtime) and
pre-existing Kubernetes Secrets referenced by deploy/helm/kodama and the observability stack.
Credentials are written only to Kubernetes Secrets (or a 0700 staging directory in --dry-run),
never to Helm values or OpenTofu state outputs in plaintext.
EOF
}

CONTRACT_FILE=""
SECRETS_FILE=""
DRY_RUN=0
WORK_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --contract)
      CONTRACT_FILE="$2"
      shift 2
      ;;
    --secrets)
      SECRETS_FILE="$2"
      shift 2
      ;;
    --work-dir)
      WORK_DIR="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "${CONTRACT_FILE}" ]] || die "--contract <stage_contract.json> is required"
validate_stage_contract "${CONTRACT_FILE}"
require_cmd python3 openssl

if [[ -z "${WORK_DIR}" ]]; then
  WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-bootstrap.XXXXXX")"
  chmod 0700 "${WORK_DIR}"
  if [[ "${DRY_RUN}" -eq 0 ]]; then
    trap 'rm -rf "${WORK_DIR}"' EXIT
  fi
else
  mkdir -p "${WORK_DIR}"
  chmod 0700 "${WORK_DIR}"
fi

# Generate idempotent secret material and SQL role bootstrap script.
python3 - "${CONTRACT_FILE}" "${SECRETS_FILE}" "${WORK_DIR}" <<'PY'
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys

contract_path, secrets_path, work_dir = sys.argv[1], sys.argv[2], Path(sys.argv[3])
with open(contract_path, "r", encoding="utf-8") as fh:
    c = json.load(fh)

boot = {}
if secrets_path and os.path.isfile(secrets_path):
    with open(secrets_path, "r", encoding="utf-8") as fh:
        boot = json.load(fh)

pg = c["postgres"]
mig_role = pg["migration_role"]
rt_role = pg["runtime_role"]
mig_pass = boot.get("migration_password") or secrets.token_hex(24)
rt_pass = boot.get("runtime_password") or secrets.token_hex(24)
nats_token = boot.get("nats_token") or secrets.token_hex(24)
git_read = boot.get("git_read_token") or secrets.token_hex(24)
git_write = boot.get("git_write_token") or secrets.token_hex(24)
s3_ak = boot.get("artifacts_s3_access_key") or "STAGE_ARTIFACTS_ACCESS_KEY"
s3_sk = boot.get("artifacts_s3_secret_key") or secrets.token_hex(24)
logs_ak = boot.get("logs_s3_access_key") or "STAGE_LOGS_ACCESS_KEY"
logs_sk = boot.get("logs_s3_secret_key") or secrets.token_hex(24)
grafana_pass = boot.get("grafana_admin_password") or secrets.token_hex(16)

# 1. PostgreSQL role separation SQL (migration owner vs runtime non-superuser, NOBYPASSRLS)
sql = f"""-- Idempotent PostgreSQL role bootstrap for {c['installation_id']}
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{mig_role}') THEN
    CREATE ROLE {mig_role} LOGIN PASSWORD '{mig_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{rt_role}') THEN
    CREATE ROLE {rt_role} LOGIN PASSWORD '{rt_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END
$$;
GRANT ALL PRIVILEGES ON DATABASE {pg['database']} TO {mig_role};
GRANT CONNECT ON DATABASE {pg['database']} TO {rt_role};
"""
(work_dir / "roles.sql").write_text(sql, encoding="utf-8")

# 2. Internal CA and NATS / Runtime TLS certificates
ca_key = work_dir / "ca.key"
ca_crt = work_dir / "ca.crt"
if not ca_crt.exists():
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
            "-keyout", str(ca_key), "-out", str(ca_crt),
            "-days", "365", "-subj", f"/CN={c['installation_id']}-ca",
        ],
        check=True,
        capture_output=True,
    )

# 3. Runtime P-256 signing key (PKCS#8 PEM)
rt_key = work_dir / "runtime-signing-key"
if not rt_key.exists():
    subprocess.run(
        [
            "openssl", "genpkey", "-algorithm", "EC",
            "-pkeyopt", "ec_paramgen_curve:P-256",
            "-out", str(rt_key),
        ],
        check=True,
        capture_output=True,
    )

# 4. Runtime Secret files (/secrets contract of deploy/helm/kodama)
rt_dir = work_dir / "kodama-runtime"
rt_dir.mkdir(exist_ok=True)
host = pg["host"]
port = pg["port"]
db = pg["database"]
sslmode = pg["ssl_mode"]

(rt_dir / "database-runtime-url").write_text(
    f"postgres://{rt_role}:{rt_pass}@{host}:{port}/{db}?sslmode={sslmode}",
    encoding="utf-8",
)
(rt_dir / "nats-token").write_text(nats_token, encoding="utf-8")
(rt_dir / "git-read-token").write_text(git_read, encoding="utf-8")
(rt_dir / "git-write-token").write_text(git_write, encoding="utf-8")

tenant_ids = [t["tenant_id"] for t in c["tenants"]]
git_origin = f"https://git.{c['network']['ingress_host']}/"
git_creds = [
    {
        "ref": "secret://installation/checkout",
        "username": "git",
        "passwordFile": "git-read-token",
        "urlPrefix": git_origin,
        "tenants": tenant_ids,
    },
    {
        "ref": "secret://installation/publisher",
        "username": "git",
        "passwordFile": "git-write-token",
        "urlPrefix": git_origin,
        "tenants": tenant_ids,
    },
]
(rt_dir / "git-credentials.json").write_text(json.dumps(git_creds), encoding="utf-8")
(rt_dir / "cursor-key").write_bytes(secrets.token_bytes(32))
(rt_dir / "idempotency-hash-key").write_bytes(secrets.token_bytes(32))
(rt_dir / "runtime-signing-key").write_bytes(rt_key.read_bytes())
(rt_dir / "runtime-ca.crt").write_bytes(ca_crt.read_bytes())
(rt_dir / "ca.crt").write_bytes(ca_crt.read_bytes())
(rt_dir / "s3-access-key").write_text(s3_ak, encoding="utf-8")
(rt_dir / "s3-secret-key").write_text(s3_sk, encoding="utf-8")

mig_dir = work_dir / "kodama-migration"
mig_dir.mkdir(exist_ok=True)
(mig_dir / "database-migration-url").write_text(
    f"postgres://{mig_role}:{mig_pass}@{host}:{port}/{db}?sslmode={sslmode}",
    encoding="utf-8",
)
(mig_dir / "ca.crt").write_bytes(ca_crt.read_bytes())

# 5. Observability & NATS secret payloads
obs_dir = work_dir / "observability"
obs_dir.mkdir(exist_ok=True)
(obs_dir / "nats-token").write_text(nats_token, encoding="utf-8")
(obs_dir / "loki-s3.env").write_text(
    f"AWS_ACCESS_KEY_ID={logs_ak}\nAWS_SECRET_ACCESS_KEY={logs_sk}\n",
    encoding="utf-8",
)
(obs_dir / "grafana-user").write_text("operator", encoding="utf-8")
(obs_dir / "grafana-password").write_text(grafana_pass, encoding="utf-8")

# 6. Pre-provisioned Grafana pipeline log search dashboard
dashboard = {
    "title": "Kodama Pipeline & Control-Plane Logs",
    "uid": "kodama-pipeline-logs",
    "schemaVersion": 39,
    "refresh": "10s",
    "templating": {
        "list": [
            {
                "name": "namespace",
                "type": "query",
                "datasource": {"type": "loki", "uid": "kodama-loki"},
                "query": "label_values(namespace)",
            },
            {
                "name": "attempt_id",
                "type": "textbox",
                "query": "",
            },
            {
                "name": "search",
                "type": "textbox",
                "query": "",
            },
        ]
    },
    "panels": [
        {
            "id": 1,
            "title": "Pipeline Step & Control-Plane Logs (Structured Metadata Search)",
            "type": "logs",
            "datasource": {"type": "loki", "uid": "kodama-loki"},
            "targets": [
                {
                    "refId": "A",
                    "expr": '{namespace=~"$namespace"} | attempt_id=~"$attempt_id.*" |= "$search"',
                }
            ],
        }
    ],
}
(obs_dir / "kodama-pipeline-logs.json").write_text(json.dumps(dashboard, indent=2), encoding="utf-8")

if c["cloud"] == "scw":
    scw_dir = work_dir / "kodama-kms-scw"
    scw_dir.mkdir(exist_ok=True)
    scw_key = boot.get("scw_kms_secret_key") or "00000000-0000-4000-8000-000000000000"
    (scw_dir / "secret-key").write_text(scw_key, encoding="utf-8")
PY

if [[ "${DRY_RUN}" -eq 1 ]]; then
  log_info "dry-run bootstrap materials prepared in ${WORK_DIR}"
  exit 0
fi

require_cmd kubectl
KUBE_ARGS=()
if [[ -n "${KUBECONFIG:-}" ]]; then
  KUBE_ARGS=(--kubeconfig "${KUBECONFIG}")
fi

for ns in kodama-system kodama-observability; do
  kubectl "${KUBE_ARGS[@]}" create namespace "${ns}" --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -
done

kubectl "${KUBE_ARGS[@]}" -n kodama-system create secret generic kodama-runtime \
  --from-file="${WORK_DIR}/kodama-runtime" --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

kubectl "${KUBE_ARGS[@]}" -n kodama-system create secret generic kodama-migration \
  --from-file="${WORK_DIR}/kodama-migration" --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

kubectl "${KUBE_ARGS[@]}" -n kodama-system create secret generic kodama-nats-auth \
  --from-file=nats-token="${WORK_DIR}/observability/nats-token" --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

kubectl "${KUBE_ARGS[@]}" -n kodama-system create secret generic kodama-nats-tls \
  --from-file=ca.crt="${WORK_DIR}/ca.crt" \
  --from-file=tls.crt="${WORK_DIR}/ca.crt" \
  --from-file=tls.key="${WORK_DIR}/ca.key" \
  --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

if [[ -d "${WORK_DIR}/kodama-kms-scw" ]]; then
  kubectl "${KUBE_ARGS[@]}" -n kodama-system create secret generic kodama-kms-scw \
    --from-file=secret-key="${WORK_DIR}/kodama-kms-scw/secret-key" \
    --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -
fi

kubectl "${KUBE_ARGS[@]}" -n kodama-observability create secret generic kodama-loki-s3 \
  --from-env-file="${WORK_DIR}/observability/loki-s3.env" --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

kubectl "${KUBE_ARGS[@]}" -n kodama-observability create secret generic kodama-grafana-admin \
  --from-file=admin-user="${WORK_DIR}/observability/grafana-user" \
  --from-file=admin-password="${WORK_DIR}/observability/grafana-password" \
  --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

kubectl "${KUBE_ARGS[@]}" -n kodama-observability create configmap kodama-grafana-dashboards \
  --from-file=kodama-pipeline-logs.json="${WORK_DIR}/observability/kodama-pipeline-logs.json" \
  --dry-run=client -o yaml | kubectl "${KUBE_ARGS[@]}" apply -f -

log_info "bootstrap complete for namespaces kodama-system and kodama-observability"
