#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd tofu helm shellcheck python3 openssl

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-tests.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

export KODAMA_IMAGE_DIGEST="sha256:a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
export KODAMA_WORKLOAD_IMAGE_DIGEST="sha256:b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1"

log_info "1. ShellCheck on all stage scripts and Markdown link/fence check on deploy/stage/README.md"
shellcheck -x \
  "${STAGE_DIR}/scripts/lib.sh" \
  "${STAGE_DIR}/scripts/bootstrap.sh" \
  "${STAGE_DIR}/scripts/deploy.sh" \
  "${STAGE_DIR}/scripts/gate.sh" \
  "${STAGE_DIR}/scripts/evidence.sh" \
  "${STAGE_DIR}/scripts/test_scripts.sh" \
  "${STAGE_DIR}/scripts/checks/"*.sh

python3 - "${STAGE_DIR}/README.md" "${REPO_ROOT}" <<'PY'
from pathlib import Path
import re
import subprocess
import sys

readme = Path(sys.argv[1])
root = Path(sys.argv[2])
text = readme.read_text(encoding="utf-8")
if len(re.findall(r"^```", text, re.MULTILINE)) % 2:
    raise SystemExit(f"{readme}: unclosed code fence")
for lang, body in re.findall(r"^```(\w*)\n(.*?)^```", text, re.MULTILINE | re.DOTALL):
    if lang in ("bash", "sh", "shell"):
        subprocess.run(["bash", "-n"], input=body, text=True, check=True)
without_code = re.sub(r"^```.*?^```", "", text, flags=re.MULTILINE | re.DOTALL)
for link in re.findall(r"\[[^\]]*\]\(([^ )]+)(?: [^)]*)?\)", without_code):
    if re.match(r"[a-zA-Z]+:", link):
        continue
    dest, _, _ = link.partition("#")
    target = (readme.parent / dest).resolve() if dest else readme
    if not target.exists():
        raise SystemExit(f"{readme.relative_to(root)}: broken link {link}")
PY

log_info "2. OpenTofu fmt check across aws, gcp, scw and shared/stack"
for dir in "${STAGE_DIR}/aws" "${STAGE_DIR}/gcp" "${STAGE_DIR}/scw" "${STAGE_DIR}/shared/stack"; do
  tofu -chdir="${dir}" fmt -check
done

log_info "3. Validate-only deploy check + shared/stack tfvars equivalence for aws, gcp, scw"
for cloud in aws gcp scw; do
  "${STAGE_DIR}/scripts/deploy.sh" \
    --cloud "${cloud}" \
    --contract-out "${WORK_DIR}/contract-${cloud}.json" \
    --values-out "${WORK_DIR}/values-${cloud}.yaml" \
    --validate-only

  # Verify that shared/stack's local.kodama_values expression with stack.tfvars.json produces byte-identical YAML
  RENDER_DIR="${WORK_DIR}/tofu-render-${cloud}"
  mkdir -p "${RENDER_DIR}"
  cp "${STAGE_DIR}/shared/stack/variables.tf" "${RENDER_DIR}/variables.tf"
  write_stack_tfvars "${WORK_DIR}/contract-${cloud}.json" "${RENDER_DIR}/terraform.tfvars.json" "/tmp/kubeconfig"
  cat >"${RENDER_DIR}/main.tf" <<EOF
locals {
  nats_url = "tls://kodama-nats.\${var.control_plane_namespace}.svc.cluster.local:4222"
  loki_url = "http://kodama-loki.\${var.observability_namespace}.svc.cluster.local:3100"
  prom_url = "http://kodama-prometheus-server.\${var.observability_namespace}.svc.cluster.local:80"
  monitored_namespaces = concat([var.control_plane_namespace], [for t in var.stage_contract.tenants : t.namespace])

  kodama_values = templatefile("${STAGE_DIR}/shared/stack/values/kodama.yaml.tftpl", {
    contract       = var.stage_contract
    nats_url       = local.nats_url
    kodama_image   = var.kodama_image
    workload_image = var.example_workload_image
    git_url        = coalesce(var.git_url, "https://git.\${var.stage_contract.network.ingress_host}/optimi/kodama-source.git")
    registry_url   = coalesce(var.registry_url, "https://registry.\${var.stage_contract.network.ingress_host}")
    oidc_issuer    = coalesce(var.oidc_issuer, "https://auth.\${var.stage_contract.network.ingress_host}")
    oidc_audience  = coalesce(var.oidc_audience, "https://\${var.stage_contract.network.ingress_host}/api")
    custom_ca      = var.custom_ca
  })
  nats = templatefile("${STAGE_DIR}/shared/stack/values/nats.yaml.tftpl", {
    namespace = var.control_plane_namespace
  })
  loki = templatefile("${STAGE_DIR}/shared/stack/values/loki.yaml.tftpl", {
    object_storage = var.stage_contract.object_storage
    sa_annotations = var.stage_contract.identities.loki_sa_annotations
  })
  alloy = templatefile("${STAGE_DIR}/shared/stack/values/alloy.yaml.tftpl", {
    loki_push_url        = "\${local.loki_url}/loki/api/v1/push"
    monitored_namespaces = local.monitored_namespaces
  })
  prometheus = templatefile("${STAGE_DIR}/shared/stack/values/prometheus.yaml.tftpl", {
    control_plane_namespace = var.control_plane_namespace
    observability_namespace = var.observability_namespace
  })
  grafana = templatefile("${STAGE_DIR}/shared/stack/values/grafana.yaml.tftpl", {
    loki_url     = local.loki_url
    prom_url     = local.prom_url
    ingress_host = "grafana.\${var.stage_contract.network.ingress_host}"
  })
}
EOF
  tofu -chdir="${RENDER_DIR}" console <<<"jsonencode(local.kodama_values)" \
    | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${RENDER_DIR}/stack-kodama.yaml"
  cmp -s "${WORK_DIR}/values-${cloud}.yaml" "${RENDER_DIR}/stack-kodama.yaml" \
    || die "rendered values from lib.sh differ from shared/stack Kodama values for ${cloud}"

  for tpl in nats loki alloy prometheus grafana; do
    tofu -chdir="${RENDER_DIR}" console <<<"jsonencode(local.${tpl})" \
      | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${RENDER_DIR}/${tpl}.yaml"
  done
  helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${WORK_DIR}/values-${cloud}.yaml" >/dev/null
  helm template kodama "${REPO_ROOT}/deploy/helm/kodama" --namespace kodama-system -f "${WORK_DIR}/values-${cloud}.yaml" >/dev/null
done

log_info "4. Bootstrap leaf TLS SANs, psql execution, Secret reuse & failure injection (Forbidden / incomplete Secret)"
MOCK_BIN="${WORK_DIR}/mock-bin"
MOCK_STATE="${WORK_DIR}/mock-state"
mkdir -p "${MOCK_BIN}" "${MOCK_STATE}"

cat >"${MOCK_BIN}/psql" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >>"${MOCK_STATE}/psql.calls"
for i in "\$@"; do
  if [[ -f "\$i" ]]; then
    cat "\$i" >>"${MOCK_STATE}/psql.sql"
  fi
done
EOF
chmod +x "${MOCK_BIN}/psql"

cat >"${WORK_DIR}/mock_kubectl.py" <<'PY'
import base64
import json
import os
from pathlib import Path
import sys

state_dir = Path(os.environ["MOCK_KUBE_STATE"])
state_dir.mkdir(parents=True, exist_ok=True)
args = sys.argv[1:]

clean = []
i = 0
while i < len(args):
    if args[i] == "--kubeconfig":
        i += 2
        continue
    clean.append(args[i])
    i += 1
args = clean

if args[:3] == ["config", "view", "--minify"]:
    sys.stdout.write(os.environ.get("MOCK_CLUSTER_SERVER", "https://k8s.aws.stage.optimi.dev"))
    sys.exit(0)

ns = "default"
if len(args) >= 2 and args[0] == "-n":
    ns = args[1]
    args = args[2:]

if len(args) >= 4 and args[0] == "get" and args[1] == "secret":
    if os.environ.get("MOCK_KUBE_FORBIDDEN") == "1":
        sys.stderr.write("Error from server (Forbidden): secrets is forbidden\n")
        sys.exit(1)
    if os.environ.get("MOCK_KUBE_EXEC_NOT_FOUND") == "1":
        sys.stderr.write("Unable to connect to the server: getting credentials: exec: executable aws not found\n")
        sys.exit(1)
    name = args[2]
    ignore_missing = "--ignore-not-found" in args
    p = state_dir / f"{ns}__{name}.json"
    if not p.is_file():
        if ignore_missing:
            sys.exit(0)
        sys.stderr.write(f'Error from server (NotFound): secrets "{name}" not found\n')
        sys.exit(1)
    sys.stdout.write(p.read_text(encoding="utf-8"))
    sys.exit(0)

if args[:2] == ["delete", "pod"]:
    sys.exit(0)

if args and args[0] == "run":
    # The role SQL Pod: the first stdin line is the DSN, the rest is the SQL script.
    stdin = sys.stdin.read()
    dsn, _, sql = stdin.partition("\n")
    with open(state_dir / "pod.calls", "a", encoding="utf-8") as fh:
        fh.write(" ".join(a for a in args if not a.startswith("--overrides")) + "\n")
        fh.write("dsn-first-line=" + ("postgres://" in dsn and "yes" or "no") + "\n")
    with open(state_dir / "pod.sql", "a", encoding="utf-8") as fh:
        fh.write(sql)
    sys.exit(0)

if len(args) >= 2 and args[0] == "create" and args[1] == "namespace":
    sys.stdout.write(json.dumps({"kind": "Namespace", "metadata": {"name": args[2]}}))
    sys.exit(0)

if len(args) >= 3 and args[0] == "create" and args[1] in ("secret", "configmap"):
    kind = args[1]
    name = args[3] if kind == "secret" else args[2]
    data = {}
    for a in args:
        if a.startswith("--from-file="):
            rest = a.split("=", 1)[1]
            if "=" in rest:
                k, fp = rest.split("=", 1)
                data[k] = base64.b64encode(Path(fp).read_bytes()).decode("ascii")
            else:
                d = Path(rest)
                for child in sorted(d.iterdir()):
                    if child.is_file():
                        data[child.name] = base64.b64encode(child.read_bytes()).decode("ascii")
        elif a.startswith("--from-env-file="):
            fp = Path(a.split("=", 1)[1])
            for line in fp.read_text(encoding="utf-8").splitlines():
                if "=" in line:
                    k, v = line.split("=", 1)
                    data[k] = base64.b64encode(v.encode("utf-8")).decode("ascii")
        elif a.startswith("--cert="):
            fp = Path(a.split("=", 1)[1])
            data["tls.crt"] = base64.b64encode(fp.read_bytes()).decode("ascii")
        elif a.startswith("--key="):
            fp = Path(a.split("=", 1)[1])
            data["tls.key"] = base64.b64encode(fp.read_bytes()).decode("ascii")
    doc = {"kind": kind.capitalize(), "namespace": ns, "name": name, "data": data}
    sys.stdout.write(json.dumps(doc))
    sys.exit(0)

if args[:2] == ["apply", "-f"]:
    raw = sys.stdin.read().strip()
    if raw:
        doc = json.loads(raw)
        if doc.get("kind") == "Secret":
            p = state_dir / f"{doc['namespace']}__{doc['name']}.json"
            p.write_text(json.dumps(doc), encoding="utf-8")
    sys.exit(0)
PY

cat >"${MOCK_BIN}/kubectl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
exec python3 "${WORK_DIR}/mock_kubectl.py" "\$@"
EOF
chmod +x "${MOCK_BIN}/kubectl"

cat >"${WORK_DIR}/boot-secrets.json" <<'EOF'
{
  "postgres_admin_username": "kodama_admin",
  "postgres_admin_password": "test_admin_password_123",
  "artifacts_s3_access_key": "AKIASTAGEARTIFACTS",
  "artifacts_s3_secret_key": "s3_art_secret",
  "logs_s3_access_key": "AKIASTAGELOGS",
  "logs_s3_secret_key": "s3_logs_secret"
}
EOF

# 4a. Forbidden or exec-plugin-not-found Secret read must abort with exit code 75 BEFORE calling psql
set +e
PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" MOCK_KUBE_FORBIDDEN="1" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${WORK_DIR}/boot-forbidden" 2>/dev/null
forb_rc=$?
PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" MOCK_KUBE_EXEC_NOT_FOUND="1" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${WORK_DIR}/boot-exec-nf" 2>/dev/null
exec_nf_rc=$?
set -e
[[ "${forb_rc}" -eq 75 ]] || die "expected Forbidden Secret read to exit 75, got ${forb_rc}"
[[ "${exec_nf_rc}" -eq 75 ]] || die "expected exec-plugin-not-found Secret read to exit 75, got ${exec_nf_rc}"
[[ ! -f "${MOCK_STATE}/psql.calls" ]] || die "psql must not be called when Secret inspection fails"

BOOT_DIR1="${WORK_DIR}/boot-run1"
BOOT_DIR2="${WORK_DIR}/boot-run2"
KODAMA_GIT_URL="https://git.external.optimi.dev:8443/org/kodama-source.git" \
  PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${BOOT_DIR1}"

[[ -s "${MOCK_STATE}/pod.calls" ]] || die "bootstrap.sh did not run the role SQL Pod"
grep -q "dsn-first-line=yes" "${MOCK_STATE}/pod.calls" || die "the role SQL Pod did not receive the DSN as the first stdin line"
grep -q "CREATE ROLE kodama_migration" "${MOCK_STATE}/pod.sql" || die "roles.sql was not sent to the role SQL Pod"
[[ ! -f "${MOCK_STATE}/psql.calls" ]] || die "the local psql must only run with STAGE_PSQL_LOCAL=1"

# The one runtime/ingress certificate covers every public stage hostname (auth, git, registry, grafana).
for san in kodama.stage.aws.optimi.dev grafana.kodama.stage.aws.optimi.dev auth.kodama.stage.aws.optimi.dev git.kodama.stage.aws.optimi.dev registry.kodama.stage.aws.optimi.dev; do
  openssl x509 -in "${BOOT_DIR1}/runtime-tls.crt" -noout -text | grep -q "DNS:${san}" || die "runtime-tls.crt missing SAN ${san}"
done
python3 - "${BOOT_DIR1}/deps/personas.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
assert set(p) == {"admin", "analyst", "engineer", "reviewer", "viewer", "outsider", "foreign"}, sorted(p)
for name, v in p.items():
    assert v["hash"].startswith("$2a$10$") and len(v["password"]) == 32, name
PY

# Verify external Git URL override is reflected in git-credentials.json urlPrefix
python3 - "${BOOT_DIR1}/kodama-runtime/git-credentials.json" <<'PY'
import json, sys
creds = json.load(open(sys.argv[1]))
assert len(creds) == 2, creds
for entry in creds:
    assert entry["urlPrefix"] == "https://git.external.optimi.dev:8443/", entry
    assert entry["tenants"] == ["t_stage"], entry
PY

openssl verify -CAfile "${BOOT_DIR1}/ca.crt" "${BOOT_DIR1}/nats-tls.crt" >/dev/null
openssl x509 -in "${BOOT_DIR1}/nats-tls.crt" -noout -text | grep -q "CA:FALSE" || die "nats-tls.crt must be CA:FALSE"
openssl x509 -in "${BOOT_DIR1}/nats-tls.crt" -noout -text | grep -q "DNS:kodama-nats.kodama-system.svc.cluster.local" || die "nats-tls.crt missing NATS service DNS SAN"

# Run bootstrap a second time into a fresh work-dir and verify existing Secrets in cluster are preserved!
KODAMA_GIT_URL="https://git.external.optimi.dev:8443/org/kodama-source.git" \
  PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${BOOT_DIR2}"

for preserved in \
  "kodama-runtime/database-runtime-url" \
  "kodama-runtime/git-credentials.json" \
  "kodama-runtime/runtime-signing-key" \
  "kodama-runtime/cursor-key" \
  "kodama-runtime/idempotency-hash-key" \
  "kodama-runtime/nats-token" \
  "kodama-runtime/ca.crt" \
  "kodama-migration/database-migration-url" \
  "deps/personas.json" \
  "deps/gitea-admin-password" \
  "deps/dex-api-client-secret"; do
  cmp -s "${BOOT_DIR1}/${preserved}" "${BOOT_DIR2}/${preserved}" || die "bootstrap rerun did not preserve ${preserved}"
done

# 4b. Corrupting an existing Secret (removing cursor-key) must fail closed on rerun
python3 - "${MOCK_STATE}/kodama-system__kodama-runtime.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
doc["data"].pop("cursor-key", None)
json.dump(doc, open(p, "w"))
PY
if PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${WORK_DIR}/boot-corrupt" 2>/dev/null; then
  die "expected incomplete existing Secret to abort bootstrap"
fi

log_info "5. Negative contract, values provenance, and kubeconfig cluster-mismatch checks"
python3 - "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/bad-kms.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["kms"]["provider"] = "gcp"
json.dump(c, open(sys.argv[2], "w"))
PY
if (validate_stage_contract "${WORK_DIR}/bad-kms.json") 2>/dev/null; then
  die "expected cross-provider KMS mismatch to fail validation"
fi

python3 - "${WORK_DIR}/contract-scw.json" "${WORK_DIR}/bad-scw-region.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["region"] = "nl-ams"
json.dump(c, open(sys.argv[2], "w"))
PY
if (validate_stage_contract "${WORK_DIR}/bad-scw-region.json") 2>/dev/null; then
  die "expected Scaleway region != fr-par to fail validation"
fi

sed 's/a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90/1111111111111111111111111111111111111111111111111111111111111111/g' \
  "${WORK_DIR}/values-aws.yaml" >"${WORK_DIR}/placeholder-values.yaml"
if (verify_values_provenance "${WORK_DIR}/placeholder-values.yaml") 2>/dev/null; then
  die "expected placeholder image digest to be rejected by verify_values_provenance"
fi

# Verify run_stage_go_suite rejects a KUBECONFIG pointing at a different cluster endpoint
touch "${WORK_DIR}/dummy.kubeconfig"
if PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" MOCK_CLUSTER_SERVER="https://wrong-cluster.example.com" \
  KUBECONFIG="${WORK_DIR}/dummy.kubeconfig" \
  run_stage_go_suite "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/values-aws.yaml" "${WORK_DIR}/mismatch-results.json" ./internal/domain/... 2>/dev/null; then
  die "expected run_stage_go_suite to fail when KUBECONFIG cluster server mismatches stage_contract"
fi

log_info "6. Gate failure-path checks (missing provenance, suite_passed=false, skipped test, infra_pause=75, rerun invalidation, staging DB rejected by T-029)"
RESULTS_DIR="${WORK_DIR}/results"
EVIDENCE_ROOT="${WORK_DIR}/evidence"
mkdir -p "${RESULTS_DIR}" "${EVIDENCE_ROOT}"

GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD)"

# Helper to write a provenance-bound FORGE_RESULTS file for a contract + values pair
write_bound_results() {
  local contract_file="$1"
  local values_file="$2"
  local out_file="$3"
  shift 3
  local tests=("$@")
  local c_sha v_sha
  c_sha="$(sha256_digest "${contract_file}")"
  v_sha="$(sha256_digest "${values_file}")"

  python3 - "${contract_file}" "${c_sha}" "${v_sha}" "${GIT_SHA}" "${out_file}" "${tests[@]}" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c_sha, v_sha, git_sha, out_file = sys.argv[2:6]
tests = sys.argv[6:]
doc = {
    "passed": tests,
    "failed": [],
    "skipped": [],
    "provenance": {
        "cloud": c["cloud"],
        "installation_id": c["installation_id"],
        "git_commit": git_sha,
        "contract_sha256": c_sha,
        "values_sha256": v_sha,
        "cluster_endpoint": c["cluster"]["endpoint"],
        "suite_passed": True,
    },
}
json.dump(doc, open(out_file, "w"), indent=2)
PY
}

prepare_results_for_contract() {
  local c_file="$1"
  local v_file="$2"
  write_bound_results "${c_file}" "${v_file}" "${RESULTS_DIR}/readiness.json" \
    kube_chart_install kube_m1_roundtrip kube_cancel_restart kube_secrets_kms \
    execution_assembly_roundtrip execution_assembly_cancel_restart execution_assembly_result_guard
  write_bound_results "${c_file}" "${v_file}" "${RESULTS_DIR}/staging.json" \
    stage_clean_install stage_e2e_roundtrip stage_restart_reconcile \
    stage_loki_log_search stage_dev_adapter_refused stage_retention_uninstall
  write_bound_results "${c_file}" "${v_file}" "${RESULTS_DIR}/recovery.json" \
    restore_full_factory stale_publisher_after_restore missing_retained_object old_token_denied
  write_bound_results "${c_file}" "${v_file}" "${RESULTS_DIR}/platform.json" \
    core_tenant_authority core_unavailable_no_fallback core_pinned_credentials \
    core_tool_audience_and_entitlement core_unknown_outcome core_data_egress \
    core_agent_delivery_slice core_disable_restore
  write_bound_results "${c_file}" "${v_file}" "${RESULTS_DIR}/release.json" \
    production_upgrade tenant_offboarding reference_load profile_install_restore hosted_core_compatibility
}

prepare_results_for_contract "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/values-aws.yaml"

# 6a. Results without provenance must be rejected even when all required test names are present
cat >"${WORK_DIR}/no-prov.json" <<'EOF'
{
  "passed": [
    "kube_chart_install", "kube_m1_roundtrip", "kube_cancel_restart", "kube_secrets_kms",
    "execution_assembly_roundtrip", "execution_assembly_cancel_restart", "execution_assembly_result_guard"
  ],
  "failed": [],
  "skipped": []
}
EOF
if "${STAGE_DIR}/scripts/checks/readiness.sh" \
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results "${WORK_DIR}/no-prov.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected FORGE_RESULTS without provenance to be rejected"
fi

# 6b. Results with suite_passed=false (package/TestMain failure after tests pass) must be rejected
python3 - "${RESULTS_DIR}/readiness.json" "${WORK_DIR}/suite-failed.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["provenance"]["suite_passed"] = False
json.dump(d, open(sys.argv[2], "w"))
PY
if "${STAGE_DIR}/scripts/checks/readiness.sh" \
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results "${WORK_DIR}/suite-failed.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected FORGE_RESULTS with suite_passed=false to be rejected"
fi

# 6c. Pass readiness, then run a failing readiness and confirm the prior receipt is invalidated and blocks staging
"${STAGE_DIR}/scripts/gate.sh" \
  --gate readiness --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

python3 - "${RESULTS_DIR}/readiness.json" "${WORK_DIR}/skipped-readiness.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["passed"].remove("kube_secrets_kms")
d["skipped"] = ["kube_secrets_kms"]
json.dump(d, open(sys.argv[2], "w"))
PY
if "${STAGE_DIR}/scripts/checks/readiness.sh" \
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results "${WORK_DIR}/skipped-readiness.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected skipped test to fail readiness gate"
fi
if "${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected failed readiness rerun to invalidate prior receipt and block staging"
fi

# 6d. infra_pause must exit 75 (EX_TEMPFAIL)
cat >"${WORK_DIR}/paused.json" <<'EOF'
{
  "infra_pause": "cluster unreachable",
  "passed": [],
  "failed": [],
  "skipped": []
}
EOF
set +e
"${STAGE_DIR}/scripts/checks/readiness.sh" \
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results "${WORK_DIR}/paused.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null
pause_rc=$?
set -e
[[ "${pause_rc}" -eq 75 ]] || die "expected infra_pause to exit 75, got ${pause_rc}"

# 6e. Re-pass readiness + staging on staging contract, and verify T-029 recovery refuses lower staging backup retention
"${STAGE_DIR}/scripts/gate.sh" \
  --gate readiness --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"
"${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

if "${STAGE_DIR}/scripts/gate.sh" \
  --gate recovery --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected recovery gate (T-029) to refuse lower staging backup retention (qualifies_o11_pitr=false)"
fi

# 6f. Verify contract drift detection on prerequisite receipts
python3 - "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/drifted-contract.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["kms"]["current"] = "v2"
json.dump(c, open(sys.argv[2], "w"), indent=2)
PY
if "${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/drifted-contract.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected staging gate to reject drifted contract against readiness receipt"
fi

log_info "7. Full critical-path traversal on production-qualified contract (30d PITR, RPO <= 15m)"
python3 - "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/contract-aws-prod.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["environment_tier"] = "production"
c["postgres"]["backup_retention_days"] = 30
c["postgres"]["rpo_minutes_estimate"] = 5
c["postgres"]["qualifies_o11_pitr"] = True
json.dump(c, open(sys.argv[2], "w"), indent=2)
PY

prepare_results_for_contract "${WORK_DIR}/contract-aws-prod.json" "${WORK_DIR}/values-aws.yaml"

"${STAGE_DIR}/scripts/gate.sh" \
  --gate critical-path --cloud aws --contract "${WORK_DIR}/contract-aws-prod.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

[[ -f "${EVIDENCE_ROOT}/latest-release-aws.json" ]] || die "missing latest-release-aws.json receipt"
log_info "PASS all stage OpenTofu, Helm, ShellCheck, bootstrap idempotency and critical-path gate checks"
