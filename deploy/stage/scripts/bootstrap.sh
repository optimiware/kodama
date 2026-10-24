#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy/stage/scripts/bootstrap.sh --contract <stage_contract.json> [--secrets <bootstrap_secrets.json>] [--work-dir <dir>] [--rotate] [--dry-run]

Idempotently prepares:
  1. Distinct PostgreSQL roles (kodama_migration schema owner and kodama_runtime non-superuser NOBYPASSRLS role)
     executed against the cloud PostgreSQL database via psql before publishing Secrets.
  2. Existing Kubernetes Secrets in kodama-system and kodama-observability are inspected and reused on re-run
     (unless --rotate is set), preserving active database passwords, runtime-signing-key, cursor-key,
     idempotency-hash-key, NATS token and CA certificates across redeployments.
  3. Leaf TLS certificates signed by the installation CA:
     - nats-tls.crt (CA:FALSE, serverAuth, SAN: kodama-nats.kodama-system.svc.cluster.local)
     - runtime-tls.crt (CA:FALSE, serverAuth, SAN: <ingress_host>)
EOF
}

CONTRACT_FILE=""
SECRETS_FILE=""
DRY_RUN=0
ROTATE=0
WORK_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --secrets) SECRETS_FILE="$2"; shift 2 ;;
    --work-dir) WORK_DIR="$2"; shift 2 ;;
    --rotate) ROTATE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
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

kube() {
  if [[ -n "${KUBECONFIG:-}" ]]; then
    kubectl --kubeconfig "${KUBECONFIG}" "$@"
  else
    kubectl "$@"
  fi
}

# Fetch existing Kubernetes Secrets when running against a cluster (unless --rotate is requested)
# so redeployments preserve existing credentials and signing keys.
EXISTING_JSON="${WORK_DIR}/existing-secrets.json"
printf '{}\n' >"${EXISTING_JSON}"
if [[ "${DRY_RUN}" -eq 0 && "${ROTATE}" -eq 0 ]] && command -v kubectl >/dev/null 2>&1; then
  python3 - "${EXISTING_JSON}" "${KUBECONFIG:-}" <<'PY'
import json
import subprocess
import sys

out_path = sys.argv[1]
kubeconfig = sys.argv[2]
kube_args = ["--kubeconfig", kubeconfig] if kubeconfig else []

def get_secret(ns: str, name: str) -> dict:
    res = subprocess.run(
        ["kubectl", *kube_args, "-n", ns, "get", "secret", name, "-o", "json"],
        capture_output=True, text=True,
    )
    if res.returncode != 0:
        return {}
    try:
        doc = json.loads(res.stdout)
        return doc.get("data") or {}
    except Exception:
        return {}

data = {
    "kodama-runtime": get_secret("kodama-system", "kodama-runtime"),
    "kodama-migration": get_secret("kodama-system", "kodama-migration"),
    "kodama-nats-tls": get_secret("kodama-system", "kodama-nats-tls"),
    "kodama-grafana-admin": get_secret("kodama-observability", "kodama-grafana-admin"),
}
with open(out_path, "w", encoding="utf-8") as fh:
    json.dump(data, fh)
PY
fi

python3 - "${CONTRACT_FILE}" "${SECRETS_FILE}" "${WORK_DIR}" "${EXISTING_JSON}" "${ROTATE}" <<'PY'
import base64
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
from urllib.parse import urlparse

contract_path, secrets_path, work_dir, existing_path, rotate_flag = (
    sys.argv[1], sys.argv[2], Path(sys.argv[3]), Path(sys.argv[4]), sys.argv[5] == "1"
)
with open(contract_path, "r", encoding="utf-8") as fh:
    c = json.load(fh)

boot = {}
if secrets_path and os.path.isfile(secrets_path):
    with open(secrets_path, "r", encoding="utf-8") as fh:
        boot = json.load(fh)

existing = json.loads(existing_path.read_text(encoding="utf-8")) if existing_path.exists() else {}
ex_rt = existing.get("kodama-runtime") or {}
ex_mig = existing.get("kodama-migration") or {}
ex_tls = existing.get("kodama-nats-tls") or {}
ex_graf = existing.get("kodama-grafana-admin") or {}

def existing_bytes(secret_map: dict, key: str, local_path: Path):
    if not rotate_flag:
        if key in secret_map:
            return base64.b64decode(secret_map[key])
        if local_path.is_file():
            return local_path.read_bytes()
    return None

def password_from_url(raw_bytes) -> str:
    if not raw_bytes:
        return ""
    try:
        u = urlparse(raw_bytes.decode("utf-8").strip())
        return u.password or ""
    except Exception:
        return ""

rt_dir = work_dir / "kodama-runtime"
rt_dir.mkdir(exist_ok=True)
mig_dir = work_dir / "kodama-migration"
mig_dir.mkdir(exist_ok=True)
obs_dir = work_dir / "observability"
obs_dir.mkdir(exist_ok=True)

pg = c["postgres"]
mig_role = pg["migration_role"]
rt_role = pg["runtime_role"]

prev_rt_url = existing_bytes(ex_rt, "database-runtime-url", rt_dir / "database-runtime-url")
prev_mig_url = existing_bytes(ex_mig, "database-migration-url", mig_dir / "database-migration-url")

mig_pass = password_from_url(prev_mig_url) or boot.get("migration_password") or secrets.token_hex(24)
rt_pass = password_from_url(prev_rt_url) or boot.get("runtime_password") or secrets.token_hex(24)

nats_token = (
    (existing_bytes(ex_rt, "nats-token", rt_dir / "nats-token") or b"").decode("utf-8")
    or boot.get("nats_token")
    or secrets.token_hex(24)
)
git_read = (
    (existing_bytes(ex_rt, "git-read-token", rt_dir / "git-read-token") or b"").decode("utf-8")
    or boot.get("git_read_token")
    or secrets.token_hex(24)
)
git_write = (
    (existing_bytes(ex_rt, "git-write-token", rt_dir / "git-write-token") or b"").decode("utf-8")
    or boot.get("git_write_token")
    or secrets.token_hex(24)
)
s3_ak = (
    boot.get("artifacts_s3_access_key")
    or (existing_bytes(ex_rt, "s3-access-key", rt_dir / "s3-access-key") or b"").decode("utf-8")
    or "STAGE_ARTIFACTS_ACCESS_KEY"
)
s3_sk = (
    boot.get("artifacts_s3_secret_key")
    or (existing_bytes(ex_rt, "s3-secret-key", rt_dir / "s3-secret-key") or b"").decode("utf-8")
    or secrets.token_hex(24)
)
logs_ak = boot.get("logs_s3_access_key") or "STAGE_LOGS_ACCESS_KEY"
logs_sk = boot.get("logs_s3_secret_key") or secrets.token_hex(24)
grafana_pass = (
    (existing_bytes(ex_graf, "admin-password", obs_dir / "grafana-password") or b"").decode("utf-8")
    or boot.get("grafana_admin_password")
    or secrets.token_hex(16)
)

# 1. PostgreSQL role separation SQL (creates or updates roles and enforces NOSUPERUSER NOBYPASSRLS)
sql = f"""-- Idempotent PostgreSQL role bootstrap for {c['installation_id']}
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{mig_role}') THEN
    CREATE ROLE {mig_role} LOGIN PASSWORD '{mig_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  ELSE
    ALTER ROLE {mig_role} WITH LOGIN PASSWORD '{mig_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{rt_role}') THEN
    CREATE ROLE {rt_role} LOGIN PASSWORD '{rt_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  ELSE
    ALTER ROLE {rt_role} WITH LOGIN PASSWORD '{rt_pass}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END
$$;
GRANT ALL PRIVILEGES ON DATABASE {pg['database']} TO {mig_role};
GRANT ALL ON SCHEMA public TO {mig_role};
GRANT CONNECT ON DATABASE {pg['database']} TO {rt_role};
"""
(work_dir / "roles.sql").write_text(sql, encoding="utf-8")

# 2. Installation CA + distinct leaf server certificates (CA:FALSE, serverAuth, DNS SANs)
ca_key = work_dir / "ca.key"
ca_crt = work_dir / "ca.crt"
prev_ca_crt = existing_bytes(ex_rt, "ca.crt", ca_crt)
prev_ca_key = existing_bytes(ex_tls, "ca.key", ca_key)

if prev_ca_crt and prev_ca_key:
    ca_crt.write_bytes(prev_ca_crt)
    ca_key.write_bytes(prev_ca_key)
else:
    ca_ext = work_dir / "ca-ext.cnf"
    ca_ext.write_text(
        "[req]\ndistinguished_name=dn\nx509_extensions=v3_ca\nprompt=no\n"
        "[dn]\nCN=" + c["installation_id"] + "-ca\n"
        "[v3_ca]\nbasicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign\n"
        "subjectKeyIdentifier=hash\n",
        encoding="utf-8",
    )
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
            "-keyout", str(ca_key), "-out", str(ca_crt),
            "-days", "365", "-config", str(ca_ext), "-extensions", "v3_ca",
        ],
        check=True,
        capture_output=True,
    )

def ensure_leaf_cert(prefix: str, cn: str, dns_names: list[str], ex_crt_key: str, ex_key_key: str):
    key_file = work_dir / f"{prefix}.key"
    crt_file = work_dir / f"{prefix}.crt"
    csr_file = work_dir / f"{prefix}.csr"
    ext_file = work_dir / f"{prefix}-ext.cnf"

    prev_crt = existing_bytes(ex_tls, ex_crt_key, crt_file)
    prev_key = existing_bytes(ex_tls, ex_key_key, key_file)
    if prev_crt and prev_key and prev_ca_crt:
        crt_file.write_bytes(prev_crt)
        key_file.write_bytes(prev_key)
        return

    san = ",".join(f"DNS:{d}" for d in dns_names)
    ext_file.write_text(
        f"basicConstraints=critical,CA:FALSE\n"
        f"keyUsage=critical,digitalSignature,keyEncipherment\n"
        f"extendedKeyUsage=serverAuth\n"
        f"subjectAltName={san}\n",
        encoding="utf-8",
    )
    subprocess.run(
        [
            "openssl", "req", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
            "-keyout", str(key_file), "-out", str(csr_file),
            "-subj", f"/CN={cn}",
        ],
        check=True,
        capture_output=True,
    )
    subprocess.run(
        [
            "openssl", "x509", "-req",
            "-in", str(csr_file), "-CA", str(ca_crt), "-CAkey", str(ca_key),
            "-CAcreateserial", "-out", str(crt_file),
            "-days", "365", "-extfile", str(ext_file),
        ],
        check=True,
        capture_output=True,
    )

ensure_leaf_cert(
    "nats-tls",
    "kodama-nats.kodama-system.svc.cluster.local",
    [
        "kodama-nats",
        "kodama-nats.kodama-system",
        "kodama-nats.kodama-system.svc",
        "kodama-nats.kodama-system.svc.cluster.local",
    ],
    "tls.crt",
    "tls.key",
)

ingress_host = c["network"]["ingress_host"]
ensure_leaf_cert(
    "runtime-tls",
    ingress_host,
    [ingress_host, f"grafana.{ingress_host}"],
    "runtime-tls.crt",
    "runtime-tls.key",
)

# 3. Runtime P-256 signing key (PKCS#8 PEM), preserved across reruns
rt_key = work_dir / "runtime-signing-key"
prev_rt_key = existing_bytes(ex_rt, "runtime-signing-key", rt_dir / "runtime-signing-key")
if prev_rt_key:
    rt_key.write_bytes(prev_rt_key)
else:
    subprocess.run(
        [
            "openssl", "genpkey", "-algorithm", "EC",
            "-pkeyopt", "ec_paramgen_curve:P-256",
            "-out", str(rt_key),
        ],
        check=True,
        capture_output=True,
    )

cursor_bytes = existing_bytes(ex_rt, "cursor-key", rt_dir / "cursor-key") or secrets.token_bytes(32)
idem_bytes = existing_bytes(ex_rt, "idempotency-hash-key", rt_dir / "idempotency-hash-key") or secrets.token_bytes(32)

# 4. Runtime & Migration Secret files (/secrets contract of deploy/helm/kodama)
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
git_origin = f"https://git.{ingress_host}/"
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
(rt_dir / "cursor-key").write_bytes(cursor_bytes)
(rt_dir / "idempotency-hash-key").write_bytes(idem_bytes)
(rt_dir / "runtime-signing-key").write_bytes(rt_key.read_bytes())
(rt_dir / "runtime-ca.crt").write_bytes(ca_crt.read_bytes())
(rt_dir / "ca.crt").write_bytes(ca_crt.read_bytes())
(rt_dir / "s3-access-key").write_text(s3_ak, encoding="utf-8")
(rt_dir / "s3-secret-key").write_text(s3_sk, encoding="utf-8")

(mig_dir / "database-migration-url").write_text(
    f"postgres://{mig_role}:{mig_pass}@{host}:{port}/{db}?sslmode={sslmode}",
    encoding="utf-8",
)
(mig_dir / "ca.crt").write_bytes(ca_crt.read_bytes())

# 5. Observability & NATS secret payloads
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

require_cmd psql kubectl

# Execute roles.sql against the cloud PostgreSQL database before publishing Kubernetes Secrets.
ADMIN_DSN="$(python3 - "${CONTRACT_FILE}" "${SECRETS_FILE}" <<'PY'
import json, os, sys
from urllib.parse import quote
c = json.load(open(sys.argv[1], "r", encoding="utf-8"))
boot = json.load(open(sys.argv[2], "r", encoding="utf-8")) if sys.argv[2] and os.path.isfile(sys.argv[2]) else {}
pg = c["postgres"]
user = boot.get("postgres_admin_username") or "kodama_admin"
pw = boot.get("postgres_admin_password") or ""
if not pw:
    raise SystemExit("postgres_admin_password is required in --secrets to execute roles.sql")
print(f"postgres://{quote(user)}:{quote(pw)}@{pg['host']}:{pg['port']}/{pg['database']}?sslmode={pg['ssl_mode']}")
PY
)"

psql "${ADMIN_DSN}" -v ON_ERROR_STOP=1 -f "${WORK_DIR}/roles.sql" >/dev/null

for ns in kodama-system kodama-observability; do
  kube create namespace "${ns}" --dry-run=client -o yaml | kube apply -f -
done

kube -n kodama-system create secret generic kodama-runtime \
  --from-file="${WORK_DIR}/kodama-runtime" --dry-run=client -o yaml | kube apply -f -

kube -n kodama-system create secret generic kodama-migration \
  --from-file="${WORK_DIR}/kodama-migration" --dry-run=client -o yaml | kube apply -f -

kube -n kodama-system create secret generic kodama-nats-auth \
  --from-file=nats-token="${WORK_DIR}/observability/nats-token" --dry-run=client -o yaml | kube apply -f -

kube -n kodama-system create secret generic kodama-nats-tls \
  --from-file=ca.crt="${WORK_DIR}/ca.crt" \
  --from-file=ca.key="${WORK_DIR}/ca.key" \
  --from-file=tls.crt="${WORK_DIR}/nats-tls.crt" \
  --from-file=tls.key="${WORK_DIR}/nats-tls.key" \
  --from-file=runtime-tls.crt="${WORK_DIR}/runtime-tls.crt" \
  --from-file=runtime-tls.key="${WORK_DIR}/runtime-tls.key" \
  --dry-run=client -o yaml | kube apply -f -

kube -n kodama-system create secret tls kodama-tls \
  --cert="${WORK_DIR}/runtime-tls.crt" \
  --key="${WORK_DIR}/runtime-tls.key" \
  --dry-run=client -o yaml | kube apply -f -

if [[ -d "${WORK_DIR}/kodama-kms-scw" ]]; then
  kube -n kodama-system create secret generic kodama-kms-scw \
    --from-file=secret-key="${WORK_DIR}/kodama-kms-scw/secret-key" \
    --dry-run=client -o yaml | kube apply -f -
fi

kube -n kodama-observability create secret generic kodama-loki-s3 \
  --from-env-file="${WORK_DIR}/observability/loki-s3.env" --dry-run=client -o yaml | kube apply -f -

kube -n kodama-observability create secret generic kodama-grafana-admin \
  --from-file=admin-user="${WORK_DIR}/observability/grafana-user" \
  --from-file=admin-password="${WORK_DIR}/observability/grafana-password" \
  --dry-run=client -o yaml | kube apply -f -

kube -n kodama-observability create secret tls kodama-grafana-tls \
  --cert="${WORK_DIR}/runtime-tls.crt" \
  --key="${WORK_DIR}/runtime-tls.key" \
  --dry-run=client -o yaml | kube apply -f -

kube -n kodama-observability create configmap kodama-grafana-dashboards \
  --from-file=kodama-pipeline-logs.json="${WORK_DIR}/observability/kodama-pipeline-logs.json" \
  --dry-run=client -o yaml | kube apply -f -

log_info "bootstrap complete for namespaces kodama-system and kodama-observability"
