#!/usr/bin/env bash
set -euo pipefail

STAGE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${STAGE_DIR}/../.." && pwd)"
export STAGE_DIR REPO_ROOT

EX_TEMPFAIL=75

log_info() {
  printf '[stage][INFO] %s\n' "$*" >&2
}

log_error() {
  printf '[stage][ERROR] %s\n' "$*" >&2
}

die() {
  log_error "$*"
  exit 1
}

die_infra() {
  log_error "INFRA_PAUSE: $*"
  exit "${EX_TEMPFAIL}"
}

require_cmd() {
  local cmd
  for cmd in "$@"; do
    command -v "${cmd}" >/dev/null 2>&1 || die_infra "required command not found on PATH: ${cmd}"
  done
}

validate_cloud() {
  local cloud="${1:-}"
  case "${cloud}" in
    aws | gcp | scw) ;;
    *) die "unsupported cloud '${cloud}' (expected: aws, gcp, or scw)" ;;
  esac
}

# validate_stage_contract verifies that a stage_contract JSON file satisfies C-18/O-05/O-26 invariants.
validate_stage_contract() {
  local contract_file="$1"
  [[ -f "${contract_file}" ]] || die "stage_contract file not found: ${contract_file}"

  python3 - "${contract_file}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as fh:
    c = json.load(fh)

required_top = [
    "cloud", "installation_id", "region", "environment_tier",
    "cluster", "postgres", "kms", "object_storage", "identities", "network", "tenants",
]
for key in required_top:
    if key not in c:
        raise SystemExit(f"stage_contract missing key: {key}")

cloud = c["cloud"]
if cloud not in ("aws", "gcp", "scw"):
    raise SystemExit(f"invalid cloud: {cloud}")

kms = c["kms"]
if kms.get("provider") != cloud:
    raise SystemExit(f"KMS provider {kms.get('provider')!r} does not match cloud {cloud!r} (no cross-provider fallback)")

if cloud == "scw" and c.get("region") != "fr-par":
    raise SystemExit(f"Scaleway region must be 'fr-par', got {c.get('region')!r}")

if kms.get("current") not in kms.get("resources", {}):
    raise SystemExit("kms.current must exist in kms.resources")

pg = c["postgres"]
if pg.get("migration_role") == pg.get("runtime_role"):
    raise SystemExit("postgres.migration_role and postgres.runtime_role must be distinct")

obj = c["object_storage"]
if obj.get("artifacts_bucket") == obj.get("logs_bucket"):
    raise SystemExit("artifacts_bucket and logs_bucket must be separate buckets")
if not str(obj.get("endpoint", "")).startswith("https://"):
    raise SystemExit("object_storage.endpoint must use https://")

net = c["network"]
if not str(net.get("runtime_url", "")).startswith("https://"):
    raise SystemExit("network.runtime_url must use https://")

tenants = c.get("tenants") or []
if not tenants:
    raise SystemExit("at least one tenant namespace is required")
seen_ns, seen_t = set(), set()
for t in tenants:
    ns, tid = t.get("namespace", ""), t.get("tenant_id", "")
    if not ns.startswith("kodama-"):
        raise SystemExit(f"tenant namespace {ns!r} must start with 'kodama-'")
    if ns in seen_ns or tid in seen_t:
        raise SystemExit("duplicate tenant namespace or tenant_id")
    seen_ns.add(ns)
    seen_t.add(tid)
PY
}

# render_kodama_values_from_contract renders deploy/stage/shared/stack/values/kodama.yaml.tftpl
# directly with `tofu console` into a YAML file without requiring a live cluster connection,
# so `helm lint --strict` and `helm template` gate the exact shared template offline and in CI.
render_kodama_values_from_contract() {
  local contract_file="$1"
  local out_file="$2"
  validate_stage_contract "${contract_file}"
  require_cmd tofu python3

  local abs_contract
  abs_contract="$(cd "$(dirname "${contract_file}")" && pwd)/$(basename "${contract_file}")"
  local render_dir
  render_dir="$(mktemp -d "${TMPDIR:-/tmp}/kodama-tofu-render.XXXXXX")"

  cat >"${render_dir}/main.tf" <<EOF
locals {
  contract = jsondecode(file("${abs_contract}"))
  rendered = templatefile("${STAGE_DIR}/shared/stack/values/kodama.yaml.tftpl", {
    contract = local.contract
    nats_url = "tls://kodama-nats.kodama-system.svc.cluster.local:4222"
    kodama_image = {
      repository = "ghcr.io/optimiware/kodama"
      tag        = "0.0.1"
      digest     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
    }
    workload_image = {
      repository = "ghcr.io/optimiware/kodama-workload"
      tag        = "0.0.1"
      digest     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    }
    git_url       = "https://git.\${local.contract.network.ingress_host}/optimi/kodama-source.git"
    registry_url  = "https://registry.\${local.contract.network.ingress_host}"
    oidc_issuer   = "https://auth.\${local.contract.network.ingress_host}"
    oidc_audience = "https://\${local.contract.network.ingress_host}/api"
    custom_ca     = true
  })
}
EOF

  tofu -chdir="${render_dir}" console <<<"jsonencode(local.rendered)" \
    | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${out_file}"
  rm -rf "${render_dir}"
}

# verify_forge_results checks a FORGE_RESULTS JSON file against required acceptance test names.
# Exit 75 (EX_TEMPFAIL) on infra_pause, exit 1 if any required test is missing, failed, or skipped.
verify_forge_results() {
  local results_file="$1"
  shift
  local required=("$@")

  [[ -f "${results_file}" ]] || die "FORGE_RESULTS file missing: ${results_file}"

  python3 - "${results_file}" "${required[@]}" <<'PY'
import json
import re
import sys

results_path = sys.argv[1]
required = sys.argv[2:]

def norm(name: str) -> str:
    s = re.sub(r"[^a-z0-9]", "", name.lower())
    return s[4:] if s.startswith("test") else s

with open(results_path, "r", encoding="utf-8") as fh:
    data = json.load(fh)

infra_pause = data.get("infra_pause", "")
if isinstance(infra_pause, str) and infra_pause.strip():
    sys.stderr.write(f"INFRA_PAUSE in {results_path}: {infra_pause.strip()}\n")
    sys.exit(75)

passed = {norm(x) for x in data.get("passed", [])} | set(data.get("passed", []))
failed = {norm(x) for x in data.get("failed", [])} | set(data.get("failed", []))
skipped = {norm(x) for x in data.get("skipped", [])} | set(data.get("skipped", []))

problems = []
for req in required:
    key = norm(req)
    if req in failed or key in failed:
        problems.append(f"required acceptance '{req}' FAILED")
    elif req in skipped or key in skipped:
        problems.append(f"required acceptance '{req}' was SKIPPED (never satisfies gate)")
    elif req not in passed and key not in passed:
        problems.append(f"required acceptance '{req}' did not run or pass")

if problems:
    for p in problems:
        sys.stderr.write(f"[gate-fail] {p}\n")
    sys.exit(1)
PY
}

# scan_no_plaintext_secrets refuses evidence files that accidentally contain private keys or credentials.
scan_no_plaintext_secrets() {
  local target_dir="$1"
  if grep -R -E -n '(BEGIN (EC |RSA )?PRIVATE KEY|postgres_admin_password|s3_secret_key|scw_kms_secret_key|AKIA[0-9A-Z]{16})' "${target_dir}" >/dev/null 2>&1; then
    die "sensitive credential or private key pattern detected inside ${target_dir}"
  fi
}
