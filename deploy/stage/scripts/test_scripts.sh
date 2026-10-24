#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd tofu helm shellcheck python3 openssl

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-tests.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

export KODAMA_IMAGE_DIGEST="sha256:a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
export KODAMA_ACME_EMAIL="ops@stage-tests.local"
export KODAMA_ISSUER_ALLOWED_CIDRS="192.0.2.10/32"
export KODAMA_WORKLOAD_IMAGE_DIGEST="sha256:b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1"

log_info "1. ShellCheck on all stage scripts and Markdown link/fence check on deploy/stage/README.md"
shellcheck -x \
  "${STAGE_DIR}/scripts/lib.sh" \
  "${STAGE_DIR}/scripts/deps-init.sh" \
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
  nats_url = "\${var.nats_tls ? "tls" : "nats"}://kodama-nats.\${var.control_plane_namespace}.svc.cluster.local:4222"
  loki_url = "http://kodama-loki.\${var.observability_namespace}.svc.cluster.local:3100"
  prom_url = "http://kodama-prometheus-server.\${var.observability_namespace}.svc.cluster.local:80"
  monitored_namespaces = concat([var.control_plane_namespace], [for t in var.stage_contract.tenants : t.namespace])

  kodama_values = templatefile("${STAGE_DIR}/shared/stack/values/kodama.yaml.tftpl", {
    contract       = var.stage_contract
    nats_url       = local.nats_url
    profile        = var.nats_tls ? "external" : "development"
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

log_info "4. Shared stack: OpenTofu validation and chart/Secret key drift checks"
MOCK_BIN="${WORK_DIR}/mock-bin"
mkdir -p "${MOCK_BIN}"

# Minimal kubectl stand-in: only the kubeconfig cluster-endpoint probe of run_stage_go_suite is exercised offline.
cat >"${MOCK_BIN}/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"config view --minify"* ]]; then
  printf '%s' "${MOCK_CLUSTER_SERVER:-https://k8s.aws.stage.optimi.dev}"
  exit 0
fi
exit 0
MOCK
chmod +x "${MOCK_BIN}/kubectl"
MOCK_STATE="${WORK_DIR}/mock-state"
mkdir -p "${MOCK_STATE}"

tofu -chdir="${STAGE_DIR}/shared/stack" init -input=false -backend=false >/dev/null
tofu -chdir="${STAGE_DIR}/shared/stack" validate >/dev/null

helm template kodama "${REPO_ROOT}/deploy/helm/kodama" --namespace kodama-system -f "${WORK_DIR}/values-scw.yaml" >"${WORK_DIR}/chart-scw.yaml"
python3 - "${WORK_DIR}/chart-scw.yaml" "${STAGE_DIR}/shared/stack/secrets.tf" <<'PY'
import re
import sys

chart = open(sys.argv[1], encoding="utf-8").read()
stack = open(sys.argv[2], encoding="utf-8").read()


def secret_block(name: str) -> str:
    m = re.search(r'resource "kubernetes_secret_v1" "%s" \{(.*?)\n\}\n' % name, stack, re.S)
    if not m:
        raise SystemExit(f"stack has no kubernetes_secret_v1.{name}")
    return m.group(1)


blocks = {"kodama-runtime": secret_block("runtime"), "kodama-migration": secret_block("migration")}
checked = 0
for secret_name, block in blocks.items():
    for m in re.finditer(r"secretName: %s\n(.*?)(?:\n        - name:|\n      containers:|\n---)" % re.escape(secret_name), chart, re.S):
        for key in re.findall(r"- key: (\S+)", m.group(1)):
            checked += 1
            if '"%s"' % key not in block:
                raise SystemExit(f"chart mounts {secret_name} key {key!r} but shared/stack does not generate it")
if checked < 8:
    raise SystemExit(f"only {checked} mounted Secret keys found; the drift check is not reading the chart")

for tpl in ("kodama", "gitea", "grafana"):
    text = open(sys.argv[2].replace("secrets.tf", f"values/{tpl}.yaml.tftpl"), encoding="utf-8").read()
    if "cert-manager.io/cluster-issuer: letsencrypt" not in text:
        raise SystemExit(f"the {tpl} ingress does not request a Let's Encrypt certificate")

oauth = open(sys.argv[2].replace("secrets.tf", "oauthsonas.tf"), encoding="utf-8").read()
for needle in ('"cert-manager.io/cluster-issuer" = "letsencrypt"', "whitelist-source-range", "OAUTHSONAS_ACCESS_CODE", 'type = "Recreate"'):
    if needle not in oauth:
        raise SystemExit(f"oauthsonas.tf lost its safeguard: {needle}")

variables = open(sys.argv[2].replace("secrets.tf", "variables.tf"), encoding="utf-8").read()
if not re.search(r'variable "oauthsonas_image".*?@sha256:[0-9a-f]{64}', variables, re.S):
    raise SystemExit("the oauthsonas image is not pinned by digest")
PY

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
log_info "PASS all stage OpenTofu, Helm, ShellCheck, stack validation and critical-path gate checks"
