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
  local expected_cloud="${2:-}"
  [[ -f "${contract_file}" ]] || die "stage_contract file not found: ${contract_file}"

  python3 - "${contract_file}" "${expected_cloud}" <<'PY'
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
if sys.argv[2] and cloud != sys.argv[2]:
    raise SystemExit("--cloud does not match the stage contract")
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

images = {}
images_file = os.environ.get("KODAMA_IMAGES_FILE", "")
if images_file:
    with open(images_file, "r", encoding="utf-8") as fh:
        images = json.load(fh)
registry = (c.get("images") or {}).get("registry_endpoint", "")


def image(kind: str, default_repo: str, default_tag: str, default_digest: str, env_prefix: str) -> dict:
    cfg = images.get(kind) or {}
    repo_default = f"{registry}/{kind}" if registry else default_repo
    return {
        "repository": os.environ.get(f"{env_prefix}_REPOSITORY") or cfg.get("repository") or repo_default,
        "tag": os.environ.get(f"{env_prefix}_TAG") or cfg.get("tag") or default_tag,
        "digest": os.environ.get(f"{env_prefix}_DIGEST") or cfg.get("digest") or default_digest,
    }


tfvars = {
    "kubeconfig_path": kubeconfig_path,
    "stage_contract": c,
    "control_plane_namespace": "kodama-system",
    "observability_namespace": "kodama-observability",
    "kodama_image": image("kodama", "ghcr.io/optimiware/kodama", "0.0.1", "sha256:" + "1" * 64, "KODAMA_IMAGE"),
    "example_workload_image": image("workload", "ghcr.io/optimiware/kodama-workload", "0.0.1", "sha256:" + "2" * 64, "KODAMA_WORKLOAD_IMAGE"),
    "git_url": os.environ.get("KODAMA_GIT_URL") or f"https://git.{host}/optimi/kodama-source.git",
    "registry_url": os.environ.get("KODAMA_REGISTRY_URL") or f"https://registry.{host}",
    "oidc_issuer": os.environ.get("KODAMA_OIDC_ISSUER") or f"https://auth.{host}",
    "oidc_audience": os.environ.get("KODAMA_OIDC_AUDIENCE") or f"https://{host}/api",
    "custom_ca": False,
    "acme_email": os.environ.get("KODAMA_ACME_EMAIL", ""),
    "issuer_allowed_cidrs": [c for c in os.environ.get("KODAMA_ISSUER_ALLOWED_CIDRS", "").split(",") if c],
    "nats_tls": os.environ.get("KODAMA_NATS_TLS", "false").lower() == "true",
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
    nats_url       = "\${local.v.nats_tls ? "tls" : "nats"}://kodama-nats.\${local.v.control_plane_namespace}.svc.cluster.local:4222"
    profile        = local.v.nats_tls ? "external" : "development"
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

  PYTHONUNBUFFERED=1 python3 - "${REPO_ROOT}" "${contract_file}" "${values_file}" "${contract_sha}" "${values_sha}" "${git_sha}" "${results_out}" "${pkgs[@]}" <<'PY'
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import hashlib
import time
import fcntl

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
Path(results_out).write_text(json.dumps({"passed": [], "failed": [], "skipped": [], "failure_phase": "preflight", "provenance": prov}) + "\n")

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

# Refuse mutable candidates and mismatched session inputs before expensive work begins.
def clean_candidate():
    return not subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=normal"], cwd=repo_root, text=True).strip()

if not clean_candidate():
    sys.exit("[gate-fail] commit all changes before qualification; candidate must remain clean")
state = Path(env.get("KODAMA_QUAL_STATE_DIR") or env.get("STAGE_STATE_DIR") or str(Path.home() / "kodama-stage/state"))
session = env.get("KODAMA_QUAL_SESSION", "")
if not re.fullmatch(r"[a-z][a-z0-9]{0,9}", session):
    sys.exit("[gate-fail] KODAMA_QUAL_SESSION must identify a prepared session")
sd = state / "qualification" / session
try:
    session_lock = (sd / "qualification.lock").open("a")
    fcntl.flock(session_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    handle = json.loads((sd / "session.json").read_text())
    consistent = (handle["name"] == session and handle["namespace"] == "kodama-q-" + session and handle["installation_id"] == c["installation_id"]
                  and handle["url"] == c["network"]["runtime_url"] and handle["tenants"] == c["tenants"]
                  and (sd / "contract.json").read_bytes() == Path(contract_path).read_bytes()
                  and (sd / "values.yaml").read_bytes() == Path(values_path).read_bytes())
except BlockingIOError:
    sys.exit("[gate-fail] qualification session is already locked by another gate")
except (OSError, ValueError, KeyError):
    sys.exit("[gate-fail] prepared session is missing or invalid")
if not consistent:
    sys.exit("[gate-fail] session, contract and values do not identify the same installation")
target = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, "--request-timeout=15s", "get", "namespace", handle["namespace"], "-o", "jsonpath={.metadata.name}"], capture_output=True, text=True)
if target.returncode != 0 or target.stdout.strip() != handle["namespace"]:
    Path(results_out).write_text(json.dumps({"infra_pause": "qualification namespace is missing or unreachable", "passed": [], "failed": [], "skipped": [], "provenance": prov}) + "\n")
    sys.exit(75)
probe_image = env.get("KODAMA_STAGE_PROBE_IMAGE", "")
readiness = "./test/e2e/kube-full/..." in pkgs
if readiness and not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", probe_image):
    sys.exit("[gate-fail] KODAMA_STAGE_PROBE_IMAGE must pin the test-only helper by digest")
if readiness and c["cloud"] == "scw" and not re.fullmatch(r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", env.get("SCW_DEFAULT_PROJECT_ID", "")):
    sys.exit("[gate-fail] SCW_DEFAULT_PROJECT_ID must identify the qualification project")
image_revisions = []
staging = "./test/e2e/staging/..." in pkgs
if readiness or staging:
    # The images under test must be the ones built from this very commit: the registry's own labels decide.
    # Readiness also uses the test-only helper; staging runs the release image alone.
    full_commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo_root, text=True).strip()
    verdict = subprocess.run([sys.executable, str(repo_root / "deploy/stage/scripts/verify_image_revision.py"), "--commit", full_commit, "--values", values_path, *([probe_image] if readiness else [])], capture_output=True, text=True)
    if verdict.returncode != 0:
        sys.exit(verdict.stderr.strip() or "[gate-fail] image provenance could not be established")
    image_revisions = [json.loads(line) for line in verdict.stdout.splitlines() if line.startswith("{")]
print(f"[stage] candidate={git_sha} session={session} inputs verified", flush=True)
if env.get("KODAMA_STAGE_PREFLIGHT_ONLY") == "1":
    sys.exit(0)
started = time.monotonic()
chart_tree = subprocess.check_output(["git", "rev-parse", "HEAD:deploy/helm/kodama"], cwd=repo_root, text=True).strip()

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
    "KODAMA_ASSEMBLY_BACKEND": "kube",
    "KODAMA_QUAL_STATE_DIR": env.get("KODAMA_QUAL_STATE_DIR") or env.get("STAGE_STATE_DIR") or str(Path.home() / "kodama-stage" / "state"),
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

# The case file lists the acceptances of both gates; each run is judged against its own.
ACCEPTANCE = {k: v for k, v in json.loads((repo_root / "deploy/stage/scripts/readiness-cases.json").read_text()).items()
              if v.get("gate", "readiness") == ("staging" if staging else "readiness")}

cmd = ["go", "test", "-json", "-p", "1", "-race", "-count=1", "-timeout", env.get("KODAMA_GO_TEST_TIMEOUT", "45m"), *pkgs]
passed, failed, skipped = set(), set(), set()
last = {}
pkg_actions = {}
saw_pkg_end = set()
incomplete_phases = []
fault_observed = []
def consume(raw):
    line = raw.strip()
    if not line.startswith("{"):
        sys.stdout.write(raw)
        return
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        sys.stdout.write(raw)
        return
    if ev.get("Action") == "output" and isinstance(ev.get("Output"), str):
        sys.stdout.write(ev["Output"])
        if "FAULT_CONFIG PX-A02 HTTPS loopback proxy; both TLS hops verified; test-owned only" in ev["Output"]:
            fault_observed.append(True)
    pkg = ev.get("Package")
    test = ev.get("Test")
    action = ev.get("Action")
    if isinstance(test, str) and test and action in ("pass", "fail", "skip"):
        last[(pkg, test)] = action
        if action == "pass":
            passed.add(test)
        elif action == "fail":
            failed.add(test)
        elif action == "skip":
            skipped.add(test)
    elif isinstance(pkg, str) and not test and action in ("pass", "fail", "skip"):
        pkg_actions[pkg] = action
        saw_pkg_end.add(pkg)

def run(args, patterns):
    saw_pkg_end.clear()
    expected = subprocess.check_output(["go", "list", *patterns], cwd=repo_root, env=env, text=True).split()
    proc = subprocess.Popen(args, cwd=repo_root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for raw in proc.stdout:
        consume(raw)
    rc = proc.wait()
    if not expected or set(expected) - saw_pkg_end:
        incomplete_phases.append(True)
    return rc

# Install first explicitly: Go's lexical/source test order is not an installation dependency.
base = cmd[:cmd.index(pkgs[0])]
if readiness:
    print("[stage] phase=chart-install", flush=True)
    rc = run([*base, "-run", "^TestKubeChartInstall$", "./test/e2e/kube-full/..."], ["./test/e2e/kube-full/..."])
else:
    rc = 0
if rc == 0:
    print("[stage] phase=scenarios", flush=True)
    rc = run([*base, "-skip", "^TestKubeChartInstall$", *pkgs], pkgs)
runner_exit = rc
current = subprocess.check_output(["git", "rev-parse", "--short=12", "HEAD"], cwd=repo_root, text=True).strip()
inputs_stable = all("sha256:" + hashlib.sha256(Path(p).read_bytes()).hexdigest() == digest
                    for p, digest in ((contract_path, contract_sha), (values_path, values_sha)))
stable = current == git_sha and clean_candidate() and inputs_stable
if not stable:
    print("[gate-fail] candidate or inputs changed during qualification", flush=True)
    rc = 1
truncated = bool(incomplete_phases)
acceptances = []
for name, spec in ACCEPTANCE.items():
    want_pkg, want_test, backend = spec["package"], spec["test"], spec["backend"]
    status = last.get((want_pkg, want_test), "")
    acceptances.append({
        "name": name,
        "package": want_pkg,
        "test": want_test,
        "backend": backend,
        "status": status or "missing",
        "cases": {case: last.get((want_pkg, want_test + "/" + case), "missing") for case in spec["cases"]},
    })
pkg_ok = bool(pkg_actions) and all(a == "pass" for a in pkg_actions.values())
cases_ok = all(a["status"] == "pass" and all(v == "pass" for v in a["cases"].values()) for a in acceptances) if (readiness or staging) else True
prov["suite_passed"] = (rc == 0 and pkg_ok and not failed and not truncated and cases_ok)
prov["session"] = env.get("KODAMA_QUAL_SESSION", "")
if readiness:
    # Only the readiness run instruments the Kubernetes API (the PX-A02 fault); a staging run has none.
    prov["fault_config"] = {
        "mechanism": "test_owned_https_loopback_proxy",
        "applied": bool(fault_observed),
        "case_status": last.get((ACCEPTANCE["kube_cancel_restart"]["package"], "TestKubeCancelRestart/lost_create_acknowledgment"), "missing"),
        "chart_install_instrumented": False,
    }
prov["probe_image"] = probe_image
prov["image_revisions"] = image_revisions
prov["chart_tree"] = chart_tree
prov["candidate_stable"] = stable
passed -= failed | skipped
out = {
    "passed": sorted(passed),
    "failed": sorted(failed),
    "skipped": sorted(skipped),
    "acceptances": acceptances,
    "packages": [{"package": p, "action": a} for p, a in sorted(pkg_actions.items())],
    "runner_exit": runner_exit,
    "qualification_exit": rc,
    "elapsed_seconds": round(time.monotonic() - started, 2),
    "truncated": truncated,
    "provenance": prov,
}
Path(results_out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
print(f"[stage] finished runner_exit={runner_exit} qualification_exit={rc} elapsed={out['elapsed_seconds']}s suite_passed={prov['suite_passed']}", flush=True)
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

  python3 - "${STAGE_DIR}/scripts/readiness-cases.json" "${results_file}" "${contract_file}" "${contract_sha}" "${values_sha}" "${git_sha}" "${required[@]}" <<'PY'
import json
import re
import sys

cases_path, results_path, contract_path, contract_sha, values_sha, git_sha = sys.argv[1:7]
required = sys.argv[7:]

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

ACCEPTANCE = json.load(open(cases_path))

def runner_flaws():
    # The runner's own completion evidence: a clean exit and every required package finished with pass.
    flaws = []
    if data.get("runner_exit") != 0:
        flaws.append("runner_exit is not 0")
    done = {p.get("package"): p.get("action") for p in data.get("packages", []) if isinstance(p, dict)}
    for req in required:
        spec = ACCEPTANCE.get(req)
        if spec and done.get(spec["package"]) != "pass":
            flaws.append(f"package {spec['package']} did not complete with pass")
    return flaws

if "stage_clean_install" in required:
    # A staging receipt carries the runner's own proof too: an unchanged candidate and the release image
    # labelled with this exact commit in the registry.
    import pathlib, subprocess
    repo = pathlib.Path(cases_path).resolve().parents[3]
    full_commit = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip()
    images = prov.get("image_revisions")
    flaws = runner_flaws()
    if prov.get("candidate_stable") is not True:
        flaws.append("provenance.candidate_stable is not true")
    if not isinstance(images, list) or len(images) < 1 or any(not isinstance(i, dict) or i.get("revision") != full_commit for i in images):
        flaws.append("provenance.image_revisions does not show the release image labelled with commit " + full_commit)
    for flaw in flaws:
        sys.stderr.write(f"[gate-fail] {flaw}\n")
    if flaws:
        sys.exit(1)

if "kube_chart_install" in required:
    # A readiness receipt must carry the runner's own proof, so a hand-written results file cannot stand in for a run:
    # an unchanged candidate, the fault actually applied, and registry images labelled with this exact commit.
    import pathlib, subprocess
    repo = pathlib.Path(cases_path).resolve().parents[3]
    full_commit = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip()
    images = prov.get("image_revisions")
    flaws = runner_flaws()
    if prov.get("candidate_stable") is not True:
        flaws.append("provenance.candidate_stable is not true")
    if not isinstance(prov.get("fault_config"), dict) or prov["fault_config"].get("applied") is not True:
        flaws.append("provenance.fault_config.applied is not true (the PX-A02 fault was not observed)")
    if not isinstance(images, list) or len(images) < 2 or any(not isinstance(i, dict) or i.get("revision") != full_commit for i in images):
        flaws.append("provenance.image_revisions does not show every candidate image labelled with commit " + full_commit)
    elif prov.get("probe_image") not in {i.get("image") for i in images}:
        flaws.append("provenance.probe_image was not among the verified images")
    for flaw in flaws:
        sys.stderr.write(f"[gate-fail] {flaw}\n")
    if flaws:
        sys.exit(1)

passed = {norm(x) for x in data.get("passed", [])} | set(data.get("passed", []))
failed = {norm(x) for x in data.get("failed", [])} | set(data.get("failed", []))
skipped = {norm(x) for x in data.get("skipped", [])} | set(data.get("skipped", []))
records = data.get("acceptances") or []

problems = []
if data.get("truncated") is True:
    problems.append("test output was truncated; package completion was not observed")
for req in required:
    spec = ACCEPTANCE.get(req)
    if spec is None:
        key = norm(req)
        if req in failed or key in failed:
            problems.append(f"required acceptance '{req}' FAILED")
        elif req in skipped or key in skipped:
            problems.append(f"required acceptance '{req}' was SKIPPED (never satisfies gate)")
        elif req not in passed and key not in passed:
            problems.append(f"required acceptance '{req}' did not run or pass")
        continue
    want_pkg, want_test, want_backend = spec["package"], spec["test"], spec["backend"]
    matches = [r for r in records if isinstance(r, dict) and r.get("name") == req]
    if not matches:
        problems.append(f"required acceptance '{req}' had zero matches")
        continue
    wrong = [r for r in matches if r.get("package") != want_pkg or r.get("test") != want_test or r.get("backend") != want_backend]
    if wrong:
        problems.append(f"required acceptance '{req}' matched the wrong package, test path or backend")
    exact = [r for r in matches if r.get("package") == want_pkg and r.get("test") == want_test and r.get("backend") == want_backend]
    if any(r.get("status") == "skip" for r in exact) or want_test in skipped or req in skipped:
        problems.append(f"required acceptance '{req}' was SKIPPED (a parent pass does not count)")
    elif not any(r.get("status") == "pass" for r in exact):
        problems.append(f"required acceptance '{req}' did not run or pass")
    for case in spec["cases"]:
        if not exact or any(r.get("cases", {}).get(case) != "pass" for r in exact):
            problems.append(f"required acceptance '{req}/{case}' did not run or pass")

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

# refuse_legacy_state_dir stops an operator whose OpenTofu state still lives in the former default location
# (~/.kube/kodama-stage/state) from silently starting over with an empty state in the new default.
refuse_legacy_state_dir() {
  local chosen="$1"
  local legacy="${HOME}/.kube/kodama-stage/state"
  if [[ -z "${STAGE_STATE_DIR:-}${KODAMA_QUAL_STATE_DIR:-}" && -d "${legacy}" && ! -d "${chosen}" ]]; then
    die "OpenTofu state exists in the former default ${legacy} but not in ${chosen}: move it, or set STAGE_STATE_DIR (and KODAMA_QUAL_STATE_DIR) explicitly"
  fi
}
