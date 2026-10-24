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

sha256_digest() {
  local file="$1"
  python3 - "${file}" <<'PY'
import hashlib, pathlib, sys
p = pathlib.Path(sys.argv[1])
print("sha256:" + hashlib.sha256(p.read_bytes()).hexdigest())
PY
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

# write_stack_tfvars writes a single canonical tfvars.json file consumed by both offline rendering
# and `tofu -chdir=deploy/stage/shared/stack apply`, guaranteeing that the values validated,
# applied, and exported into evidence are identical.
write_stack_tfvars() {
  local contract_file="$1"
  local tfvars_out="$2"
  local kubeconfig_path="${3:-${KUBECONFIG:-${HOME}/.kube/config}}"
  validate_stage_contract "${contract_file}"

  python3 - "${contract_file}" "${tfvars_out}" "${kubeconfig_path}" <<'PY'
import json
import os
import sys

contract_path, tfvars_out, kubeconfig_path = sys.argv[1:4]
c = json.load(open(contract_path, "r", encoding="utf-8"))
host = c["network"]["ingress_host"]

tfvars = {
    "kubeconfig_path": kubeconfig_path,
    "stage_contract": c,
    "control_plane_namespace": "kodama-system",
    "observability_namespace": "kodama-observability",
    "kodama_image": {
        "repository": os.environ.get("KODAMA_IMAGE_REPOSITORY", "ghcr.io/optimiware/kodama"),
        "tag": os.environ.get("KODAMA_IMAGE_TAG", "0.0.1"),
        "digest": os.environ.get("KODAMA_IMAGE_DIGEST", "sha256:" + "1" * 64),
    },
    "example_workload_image": {
        "repository": os.environ.get("KODAMA_WORKLOAD_IMAGE_REPOSITORY", "ghcr.io/optimiware/kodama-workload"),
        "tag": os.environ.get("KODAMA_WORKLOAD_IMAGE_TAG", "0.0.1"),
        "digest": os.environ.get("KODAMA_WORKLOAD_IMAGE_DIGEST", "sha256:" + "2" * 64),
    },
    "git_url": os.environ.get("KODAMA_GIT_URL") or f"https://git.{host}/optimi/kodama-source.git",
    "registry_url": os.environ.get("KODAMA_REGISTRY_URL") or f"https://registry.{host}",
    "oidc_issuer": os.environ.get("KODAMA_OIDC_ISSUER") or f"https://auth.{host}",
    "oidc_audience": os.environ.get("KODAMA_OIDC_AUDIENCE") or f"https://{host}/api",
    "custom_ca": True,
}
with open(tfvars_out, "w", encoding="utf-8") as fh:
    json.dump(tfvars, fh, indent=2)
    fh.write("\n")
PY
}

# render_kodama_values_from_contract renders deploy/stage/shared/stack/values/kodama.yaml.tftpl
# directly with `tofu console` from the canonical stack_tfvars JSON.
render_kodama_values_from_contract() {
  local contract_file="$1"
  local out_file="$2"
  local tfvars_out="${3:-}"
  validate_stage_contract "${contract_file}"
  require_cmd tofu python3

  local render_dir
  render_dir="$(mktemp -d "${TMPDIR:-/tmp}/kodama-tofu-render.XXXXXX")"
  local tfvars_file="${tfvars_out:-${render_dir}/stack.tfvars.json}"
  write_stack_tfvars "${contract_file}" "${tfvars_file}"
  local abs_tfvars
  abs_tfvars="$(cd "$(dirname "${tfvars_file}")" && pwd)/$(basename "${tfvars_file}")"

  cat >"${render_dir}/main.tf" <<EOF
locals {
  v = jsondecode(file("${abs_tfvars}"))
  rendered = templatefile("${STAGE_DIR}/shared/stack/values/kodama.yaml.tftpl", {
    contract       = local.v.stage_contract
    nats_url       = "tls://kodama-nats.\${local.v.control_plane_namespace}.svc.cluster.local:4222"
    kodama_image   = local.v.kodama_image
    workload_image = local.v.example_workload_image
    git_url        = local.v.git_url
    registry_url   = local.v.registry_url
    oidc_issuer    = local.v.oidc_issuer
    oidc_audience  = local.v.oidc_audience
    custom_ca      = local.v.custom_ca
  })
}
EOF

  tofu -chdir="${render_dir}" console <<<"jsonencode(local.rendered)" \
    | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${out_file}"
  rm -rf "${render_dir}"
}

# verify_values_provenance rejects placeholder image digests or .example.invalid domains unless STAGE_ALLOW_PLACEHOLDERS=1.
verify_values_provenance() {
  local values_file="$1"
  [[ -f "${values_file}" ]] || die "values file missing: ${values_file}"
  if [[ "${STAGE_ALLOW_PLACEHOLDERS:-0}" == "1" ]]; then
    return 0
  fi
  python3 - "${values_file}" <<'PY'
import pathlib, sys

text = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
bad_digests = (
    "sha256:" + "0" * 64,
    "sha256:" + "1" * 64,
    "sha256:" + "2" * 64,
)
for d in bad_digests:
    if d in text:
        raise SystemExit(
            f"values file {sys.argv[1]} contains placeholder image digest {d}; "
            "pass real deployed values (--values) or set KODAMA_IMAGE_DIGEST/KODAMA_WORKLOAD_IMAGE_DIGEST"
        )
if ".example.invalid" in text:
    raise SystemExit(
        f"values file {sys.argv[1]} contains placeholder host '.example.invalid'; "
        "real stage/production evidence requires actual deployed hostnames"
    )
PY
}

# run_stage_go_suite runs Go integration/e2e packages directly against the verified stage cluster,
# contract and values. It strips inherited local DEV_SCOPE variables, verifies that KUBECONFIG's
# active cluster endpoint matches stage_contract.cluster.endpoint, sets both KODAMA_KUBECONFIG and
# KODAMA_TEST_KUBECONFIG, and records package/suite completion plus full provenance.
run_stage_go_suite() {
  local contract_file="$1"
  local values_file="$2"
  local results_out="$3"
  shift 3
  local pkgs=("$@")

  require_cmd go kubectl python3
  local contract_sha values_sha git_sha
  contract_sha="$(sha256_digest "${contract_file}")"
  values_sha="$(sha256_digest "${values_file}")"
  git_sha="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"

  python3 - "${REPO_ROOT}" "${contract_file}" "${values_file}" "${contract_sha}" "${values_sha}" "${git_sha}" "${results_out}" "${pkgs[@]}" <<'PY'
import json
import os
from pathlib import Path
import re
import subprocess
import sys

repo_root = Path(sys.argv[1])
contract_path, values_path, contract_sha, values_sha, git_sha, results_out = sys.argv[2:8]
pkgs = sys.argv[8:]

c = json.load(open(contract_path, "r", encoding="utf-8"))
prov = {
    "cloud": c["cloud"],
    "installation_id": c["installation_id"],
    "git_commit": git_sha,
    "contract_sha256": contract_sha,
    "values_sha256": values_sha,
    "cluster_endpoint": c["cluster"]["endpoint"],
    "suite_passed": False,
}

env = dict(os.environ)
kubeconfig = env.get("KUBECONFIG") or env.get("KODAMA_KUBECONFIG") or ""
if not kubeconfig or not Path(kubeconfig).is_file():
    out = {
        "infra_pause": "KUBECONFIG is unset or unreadable for stage suite execution",
        "passed": [], "failed": [], "skipped": [],
        "provenance": prov,
    }
    Path(results_out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    sys.exit(75)

# Verify that the kubeconfig's selected cluster server matches stage_contract.cluster.endpoint
srv_res = subprocess.run(
    ["kubectl", "--kubeconfig", kubeconfig, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"],
    capture_output=True, text=True,
)
actual_srv = srv_res.stdout.strip().rstrip("/")
expected_srv = str(c["cluster"]["endpoint"]).strip().rstrip("/")
if srv_res.returncode != 0 or not actual_srv:
    out = {
        "infra_pause": f"unable to resolve cluster endpoint from KUBECONFIG {kubeconfig}",
        "passed": [], "failed": [], "skipped": [],
        "provenance": prov,
    }
    Path(results_out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    sys.exit(75)

if actual_srv != expected_srv:
    out = {
        "passed": [], "failed": ["cluster_target_mismatch"], "skipped": [],
        "provenance": prov,
    }
    Path(results_out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    sys.stderr.write(f"[gate-fail] KUBECONFIG cluster server {actual_srv!r} != stage_contract {expected_srv!r}\n")
    sys.exit(1)

# Clear inherited local-scope environment variables so tests never fall back to a local Podman/kind fixture.
for stale_var in (
    "DEV_SCOPE", "KODAMA_TEST_SCOPE", "KODAMA_RESOURCE_PREFIX",
    "KODAMA_TEST_POSTGRES_ADMIN_URL", "KODAMA_NATS_URL", "KODAMA_REGISTRY_URL",
    "KODAMA_GIT_URL", "KODAMA_SECRETS_DIR",
):
    env.pop(stale_var, None)

abs_kc = str(Path(kubeconfig).resolve())
env.update({
    "KODAMA_AUTO_PROVISION": "0",
    "KODAMA_REQUIRE_KUBERNETES": "1",
    "KUBECONFIG": abs_kc,
    "KODAMA_KUBECONFIG": abs_kc,
    "KODAMA_TEST_KUBECONFIG": abs_kc,
    "KODAMA_STAGE_CONTRACT": str(Path(contract_path).resolve()),
    "KODAMA_STAGE_VALUES": str(Path(values_path).resolve()),
    "KODAMA_CLOUD": c["cloud"],
    "KODAMA_INSTALLATION_ID": c["installation_id"],
    "KODAMA_RUNTIME_URL": c["network"]["runtime_url"],
    "KODAMA_S3_ENDPOINT": c["object_storage"]["endpoint"],
    "KODAMA_S3_BUCKET": c["object_storage"]["artifacts_bucket"],
    "KODAMA_S3_PREFIX": c["object_storage"]["artifacts_prefix"],
})

def norm(name: str) -> str:
    s = re.sub(r"[^a-z0-9]", "", name.lower())
    return s[4:] if s.startswith("test") else s

cmd = ["go", "test", "-json", "-race", "-count=1", "-timeout", env.get("KODAMA_GO_TEST_TIMEOUT", "30m"), *pkgs]
proc = subprocess.Popen(cmd, cwd=repo_root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
assert proc.stdout is not None

passed, failed, skipped = set(), set(), set()
pkg_actions = {}
for raw in proc.stdout:
    line = raw.strip()
    if not line.startswith("{"):
        sys.stdout.write(raw)
        continue
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        sys.stdout.write(raw)
        continue
    if ev.get("Action") == "output" and isinstance(ev.get("Output"), str):
        sys.stdout.write(ev["Output"])
    pkg = ev.get("Package")
    test = ev.get("Test")
    action = ev.get("Action")
    if isinstance(test, str) and test:
        for part in test.split("/"):
            if action == "pass":
                passed.add(part)
                passed.add(norm(part))
            elif action == "fail":
                failed.add(part)
                failed.add(norm(part))
            elif action == "skip":
                skipped.add(part)
                skipped.add(norm(part))
    elif isinstance(pkg, str) and action in ("pass", "fail", "skip"):
        pkg_actions[pkg] = action

rc = proc.wait()
pkg_ok = bool(pkg_actions) and all(a == "pass" for a in pkg_actions.values())
prov["suite_passed"] = (rc == 0 and pkg_ok and not failed)
passed -= failed | skipped
out = {
    "passed": sorted(passed),
    "failed": sorted(failed),
    "skipped": sorted(skipped),
    "provenance": prov,
}
Path(results_out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
sys.exit(0 if prov["suite_passed"] else 1)
PY
}

# verify_forge_results checks a FORGE_RESULTS JSON file against required acceptance test names
# and requires complete, matching target provenance (cloud, installation_id, git_commit,
# contract_sha256, values_sha256, cluster_endpoint, suite_passed == true).
# Exit 75 (EX_TEMPFAIL) on infra_pause, exit 1 on any missing/mismatched provenance or failed/skipped test.
verify_forge_results() {
  local results_file="$1"
  local contract_file="$2"
  local values_file="$3"
  shift 3
  local required=("$@")

  [[ -f "${results_file}" ]] || die "FORGE_RESULTS file missing: ${results_file}"

  local contract_sha values_sha git_sha
  contract_sha="$(sha256_digest "${contract_file}")"
  values_sha="$(sha256_digest "${values_file}")"
  git_sha="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"

  python3 - "${results_file}" "${contract_file}" "${contract_sha}" "${values_sha}" "${git_sha}" "${required[@]}" <<'PY'
import json
import re
import sys

results_path, contract_path, contract_sha, values_sha, git_sha = sys.argv[1:6]
required = sys.argv[6:]

def norm(name: str) -> str:
    s = re.sub(r"[^a-z0-9]", "", name.lower())
    return s[4:] if s.startswith("test") else s

with open(results_path, "r", encoding="utf-8") as fh:
    data = json.load(fh)
with open(contract_path, "r", encoding="utf-8") as fh:
    contract = json.load(fh)

infra_pause = data.get("infra_pause", "")
if isinstance(infra_pause, str) and infra_pause.strip():
    sys.stderr.write(f"INFRA_PAUSE in {results_path}: {infra_pause.strip()}\n")
    sys.exit(75)

prov = data.get("provenance")
if not isinstance(prov, dict):
    sys.stderr.write("[gate-fail] FORGE_RESULTS is missing required 'provenance' object\n")
    sys.exit(1)

expected_prov = {
    "cloud": contract["cloud"],
    "installation_id": contract["installation_id"],
    "git_commit": git_sha,
    "contract_sha256": contract_sha,
    "values_sha256": values_sha,
    "cluster_endpoint": contract["cluster"]["endpoint"],
}
for k, want in expected_prov.items():
    got = prov.get(k)
    if not got or got != want:
        sys.stderr.write(f"[gate-fail] provenance.{k} = {got!r}, want {want!r}\n")
        sys.exit(1)

if prov.get("suite_passed") is not True:
    sys.stderr.write("[gate-fail] provenance.suite_passed is not true (package or runner failure)\n")
    sys.exit(1)

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

# verify_prerequisite_receipt validates a prerequisite gate's latest receipt, re-hashes its
# stored bundle artifacts to detect tampering, and verifies that its topology and deployed values
# digest match the current deployment.
verify_prerequisite_receipt() {
  local evidence_root="$1"
  local prereq_gate="$2"
  local cloud="$3"
  local contract_file="$4"
  local values_file="$5"

  local receipt_path="${evidence_root}/latest-${prereq_gate}-${cloud}.json"
  [[ -f "${receipt_path}" ]] || die "missing prerequisite receipt for gate '${prereq_gate}': ${receipt_path}"

  local git_sha values_sha
  git_sha="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"
  values_sha="$(sha256_digest "${values_file}")"

  python3 - "${receipt_path}" "${prereq_gate}" "${cloud}" "${contract_file}" "${values_sha}" "${git_sha}" <<'PY'
import hashlib
import json
from pathlib import Path
import sys

receipt_path, prereq_gate, cloud, contract_path, values_sha, git_sha = sys.argv[1:7]
rec = json.load(open(receipt_path, "r", encoding="utf-8"))
cur_c = json.load(open(contract_path, "r", encoding="utf-8"))

if rec.get("status") != "passed":
    raise SystemExit(f"prerequisite '{prereq_gate}' status is {rec.get('status')!r}, want 'passed'")
if rec.get("cloud") != cloud:
    raise SystemExit(f"prerequisite '{prereq_gate}' cloud {rec.get('cloud')!r} != {cloud!r}")
if rec.get("installation_id") != cur_c.get("installation_id"):
    raise SystemExit(f"prerequisite '{prereq_gate}' installation_id mismatch")
if rec.get("git_commit") != git_sha:
    raise SystemExit(f"prerequisite '{prereq_gate}' commit {rec.get('git_commit')} != HEAD {git_sha}")

bundle_dir = Path(rec.get("bundle_dir", ""))
if not bundle_dir.is_dir():
    raise SystemExit(f"prerequisite '{prereq_gate}' bundle_dir missing: {bundle_dir}")

artifacts = rec.get("artifacts") or {}
for name in ("stage_contract.json", "kodama-values.yaml", "forge_results.json"):
    p = bundle_dir / name
    if not p.is_file():
        raise SystemExit(f"prerequisite '{prereq_gate}' missing artifact {p}")
    actual = "sha256:" + hashlib.sha256(p.read_bytes()).hexdigest()
    if artifacts.get(name) != actual:
        raise SystemExit(f"prerequisite '{prereq_gate}' artifact {name} digest mismatch (tampered or modified)")

if artifacts.get("kodama-values.yaml") != values_sha:
    raise SystemExit(
        f"prerequisite '{prereq_gate}' kodama-values.yaml digest {artifacts.get('kodama-values.yaml')} "
        f"does not match current deployment {values_sha}"
    )

prev_c = json.loads((bundle_dir / "stage_contract.json").read_text(encoding="utf-8"))
for key in ("cluster", "kms", "object_storage", "identities", "network", "tenants"):
    if prev_c.get(key) != cur_c.get(key):
        raise SystemExit(f"prerequisite '{prereq_gate}' contract field '{key}' differs from current contract")

for pg_key in ("host", "port", "database", "migration_role", "runtime_role", "ssl_mode"):
    if prev_c.get("postgres", {}).get(pg_key) != cur_c.get("postgres", {}).get(pg_key):
        raise SystemExit(f"prerequisite '{prereq_gate}' postgres.{pg_key} differs from current contract")
PY
}

# invalidate_gate_receipt removes any existing latest-<gate>-<cloud>.json before a gate run starts.
invalidate_gate_receipt() {
  local evidence_root="$1"
  local gate="$2"
  local cloud="$3"
  rm -f "${evidence_root}/latest-${gate}-${cloud}.json"
}

# scan_no_plaintext_secrets refuses evidence files that accidentally contain private keys or credentials.
scan_no_plaintext_secrets() {
  local target_dir="$1"
  if grep -R -E -n '(BEGIN (EC |RSA )?PRIVATE KEY|postgres_admin_password|s3_secret_key|scw_kms_secret_key|AKIA[0-9A-Z]{16})' "${target_dir}" >/dev/null 2>&1; then
    die "sensitive credential or private key pattern detected inside ${target_dir}"
  fi
}
