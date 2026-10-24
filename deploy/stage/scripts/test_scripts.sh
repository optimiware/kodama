#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd tofu helm shellcheck python3 openssl

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-tests.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

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

log_info "3. Validate-only deploy check (contract + strict Helm lint/template + bootstrap dry-run) for aws, gcp, scw"
for cloud in aws gcp scw; do
  "${STAGE_DIR}/scripts/deploy.sh" \
    --cloud "${cloud}" \
    --contract-out "${WORK_DIR}/contract-${cloud}.json" \
    --validate-only

  # Also render all 6 OpenTofu .tftpl templates directly via `tofu console` and lint the rendered Kodama values.
  RENDER_DIR="${WORK_DIR}/tofu-render-${cloud}"
  mkdir -p "${RENDER_DIR}"
  cat >"${RENDER_DIR}/main.tf" <<EOF
locals {
  contract = jsondecode(file("${WORK_DIR}/contract-${cloud}.json"))
  kodama = templatefile("${STAGE_DIR}/shared/stack/values/kodama.yaml.tftpl", {
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
    git_url       = "https://git.stage.example.invalid/optimi/kodama-source.git"
    registry_url  = "https://registry.stage.example.invalid"
    oidc_issuer   = "https://auth.stage.example.invalid"
    oidc_audience = "https://kodama.stage.example.invalid/api"
    custom_ca     = true
  })
  nats = templatefile("${STAGE_DIR}/shared/stack/values/nats.yaml.tftpl", {
    namespace = "kodama-system"
  })
  loki = templatefile("${STAGE_DIR}/shared/stack/values/loki.yaml.tftpl", {
    object_storage = local.contract.object_storage
    sa_annotations = local.contract.identities.loki_sa_annotations
  })
  alloy = templatefile("${STAGE_DIR}/shared/stack/values/alloy.yaml.tftpl", {
    loki_push_url        = "http://kodama-loki.kodama-observability.svc.cluster.local:3100/loki/api/v1/push"
    monitored_namespaces = ["kodama-system", "kodama-tenant-stage"]
  })
  prometheus = templatefile("${STAGE_DIR}/shared/stack/values/prometheus.yaml.tftpl", {
    control_plane_namespace = "kodama-system"
    observability_namespace = "kodama-observability"
  })
  grafana = templatefile("${STAGE_DIR}/shared/stack/values/grafana.yaml.tftpl", {
    loki_url     = "http://kodama-loki.kodama-observability.svc.cluster.local:3100"
    prom_url     = "http://kodama-prometheus-server.kodama-observability.svc.cluster.local:80"
    ingress_host = "grafana.kodama.stage.example.invalid"
  })
}
EOF
  tofu -chdir="${RENDER_DIR}" init -backend=false -input=false >/dev/null
  tofu -chdir="${RENDER_DIR}" console <<<"jsonencode(local.kodama)" | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${RENDER_DIR}/kodama.yaml"
  for tpl in nats loki alloy prometheus grafana; do
    tofu -chdir="${RENDER_DIR}" console <<<"jsonencode(local.${tpl})" | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${RENDER_DIR}/${tpl}.yaml"
  done
  helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${RENDER_DIR}/kodama.yaml" >/dev/null
  helm template kodama "${REPO_ROOT}/deploy/helm/kodama" --namespace kodama-system -f "${RENDER_DIR}/kodama.yaml" >/dev/null
done

log_info "4. Negative contract checks (cross-provider KMS mismatch & Scaleway non-fr-par region)"
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

log_info "5. Gate failure-path checks (missing prerequisite, skipped test, infra_pause=75, staging DB rejected by T-029)"
RESULTS_DIR="${WORK_DIR}/results"
EVIDENCE_ROOT="${WORK_DIR}/evidence"
mkdir -p "${RESULTS_DIR}" "${EVIDENCE_ROOT}"

cat >"${RESULTS_DIR}/readiness.json" <<'EOF'
{
  "passed": [
    "kube_chart_install",
    "kube_m1_roundtrip",
    "kube_cancel_restart",
    "kube_secrets_kms",
    "execution_assembly_roundtrip",
    "execution_assembly_cancel_restart",
    "execution_assembly_result_guard"
  ],
  "failed": [],
  "skipped": []
}
EOF

cat >"${RESULTS_DIR}/staging.json" <<'EOF'
{
  "passed": [
    "stage_clean_install",
    "stage_e2e_roundtrip",
    "stage_restart_reconcile",
    "stage_loki_log_search",
    "stage_dev_adapter_refused",
    "stage_retention_uninstall"
  ],
  "failed": [],
  "skipped": []
}
EOF

cat >"${RESULTS_DIR}/recovery.json" <<'EOF'
{
  "passed": [
    "restore_full_factory",
    "stale_publisher_after_restore",
    "missing_retained_object",
    "old_token_denied"
  ],
  "failed": [],
  "skipped": []
}
EOF

cat >"${RESULTS_DIR}/platform.json" <<'EOF'
{
  "passed": [
    "core_tenant_authority",
    "core_unavailable_no_fallback",
    "core_pinned_credentials",
    "core_tool_audience_and_entitlement",
    "core_unknown_outcome",
    "core_data_egress",
    "core_agent_delivery_slice",
    "core_disable_restore"
  ],
  "failed": [],
  "skipped": []
}
EOF

cat >"${RESULTS_DIR}/release.json" <<'EOF'
{
  "passed": [
    "production_upgrade",
    "tenant_offboarding",
    "reference_load",
    "profile_install_restore",
    "hosted_core_compatibility"
  ],
  "failed": [],
  "skipped": []
}
EOF

# 5a. T-090 staging must fail before T-104 readiness has passed
if "${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected staging gate to fail when readiness receipt is missing"
fi

# 5b. Skipped required test must fail readiness gate
cat >"${WORK_DIR}/skipped-readiness.json" <<'EOF'
{
  "passed": ["kube_chart_install", "kube_m1_roundtrip", "kube_cancel_restart"],
  "failed": [],
  "skipped": ["kube_secrets_kms"]
}
EOF
if "${STAGE_DIR}/scripts/checks/readiness.sh" \
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results "${WORK_DIR}/skipped-readiness.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected skipped test to fail readiness gate"
fi

# 5c. infra_pause must exit 75 (EX_TEMPFAIL)
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
  --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results "${WORK_DIR}/paused.json" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null
pause_rc=$?
set -e
[[ "${pause_rc}" -eq 75 ]] || die "expected infra_pause to exit 75, got ${pause_rc}"

# 5d. T-104 + T-090 succeed on staging contract (7d backup retention), but T-029 recovery must refuse it!
"${STAGE_DIR}/scripts/gate.sh" \
  --gate readiness --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"
"${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

if "${STAGE_DIR}/scripts/gate.sh" \
  --gate recovery --cloud aws --contract "${WORK_DIR}/contract-aws.json" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected recovery gate (T-029) to refuse lower staging backup retention (qualifies_o11_pitr=false)"
fi

log_info "6. Full critical-path traversal on production-qualified contract (30d PITR, RPO <= 15m)"
python3 - "${WORK_DIR}/contract-aws.json" "${WORK_DIR}/contract-aws-prod.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["environment_tier"] = "production"
c["postgres"]["backup_retention_days"] = 30
c["postgres"]["rpo_minutes_estimate"] = 5
c["postgres"]["qualifies_o11_pitr"] = True
json.dump(c, open(sys.argv[2], "w"), indent=2)
PY

"${STAGE_DIR}/scripts/gate.sh" \
  --gate critical-path --cloud aws --contract "${WORK_DIR}/contract-aws-prod.json" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

[[ -f "${EVIDENCE_ROOT}/latest-release-aws.json" ]] || die "missing latest-release-aws.json receipt"
log_info "PASS all stage OpenTofu, Helm, ShellCheck and critical-path gate checks"
