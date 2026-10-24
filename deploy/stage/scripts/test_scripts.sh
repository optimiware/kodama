#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd tofu helm shellcheck python3 openssl

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-tests.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

# Non-placeholder image digests for realistic gate tests
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

log_info "3. Validate-only deploy check + template rendering for aws, gcp, scw"
for cloud in aws gcp scw; do
  "${STAGE_DIR}/scripts/deploy.sh" \
    --cloud "${cloud}" \
    --contract-out "${WORK_DIR}/contract-${cloud}.json" \
    --values-out "${WORK_DIR}/values-${cloud}.yaml" \
    --validate-only

  RENDER_DIR="${WORK_DIR}/tofu-render-${cloud}"
  mkdir -p "${RENDER_DIR}"
  cat >"${RENDER_DIR}/main.tf" <<EOF
locals {
  contract = jsondecode(file("${WORK_DIR}/contract-${cloud}.json"))
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
    ingress_host = "grafana.kodama.stage.${cloud}.optimi.dev"
  })
}
EOF
  for tpl in nats loki alloy prometheus grafana; do
    tofu -chdir="${RENDER_DIR}" console <<<"jsonencode(local.${tpl})" \
      | python3 -c 'import json,sys; sys.stdout.write(json.loads(json.loads(sys.stdin.read().strip())))' >"${RENDER_DIR}/${tpl}.yaml"
  done
  helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${WORK_DIR}/values-${cloud}.yaml" >/dev/null
  helm template kodama "${REPO_ROOT}/deploy/helm/kodama" --namespace kodama-system -f "${WORK_DIR}/values-${cloud}.yaml" >/dev/null
done

log_info "4. Bootstrap leaf TLS SANs, psql execution and idempotent Secret reuse across consecutive runs"
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

ns = "default"
if len(args) >= 2 and args[0] == "-n":
    ns = args[1]
    args = args[2:]

if len(args) >= 4 and args[0] == "get" and args[1] == "secret":
    name = args[2]
    p = state_dir / f"{ns}__{name}.json"
    if not p.is_file():
        sys.exit(1)
    sys.stdout.write(p.read_text(encoding="utf-8"))
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

BOOT_DIR1="${WORK_DIR}/boot-run1"
BOOT_DIR2="${WORK_DIR}/boot-run2"
PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${BOOT_DIR1}"

# Verify psql actually executed roles.sql
[[ -s "${MOCK_STATE}/psql.calls" ]] || die "bootstrap.sh did not invoke psql"
grep -q "CREATE ROLE kodama_migration" "${MOCK_STATE}/psql.sql" || die "roles.sql was not executed by psql"

# Verify leaf TLS certificate is CA:FALSE, serverAuth, and carries NATS DNS SAN
openssl verify -CAfile "${BOOT_DIR1}/ca.crt" "${BOOT_DIR1}/nats-tls.crt" >/dev/null
openssl x509 -in "${BOOT_DIR1}/nats-tls.crt" -noout -text | grep -q "CA:FALSE" || die "nats-tls.crt must be CA:FALSE"
openssl x509 -in "${BOOT_DIR1}/nats-tls.crt" -noout -text | grep -q "DNS:kodama-nats.kodama-system.svc.cluster.local" || die "nats-tls.crt missing NATS service DNS SAN"

# Run bootstrap a second time into a fresh work-dir and verify existing Secrets in cluster are preserved!
PATH="${MOCK_BIN}:${PATH}" MOCK_KUBE_STATE="${MOCK_STATE}" \
  "${STAGE_DIR}/scripts/bootstrap.sh" --contract "${WORK_DIR}/contract-aws.json" --secrets "${WORK_DIR}/boot-secrets.json" --work-dir "${BOOT_DIR2}"

for preserved in \
  "kodama-runtime/database-runtime-url" \
  "kodama-runtime/runtime-signing-key" \
  "kodama-runtime/cursor-key" \
  "kodama-runtime/idempotency-hash-key" \
  "kodama-runtime/nats-token" \
  "kodama-runtime/ca.crt" \
  "kodama-migration/database-migration-url"; do
  cmp -s "${BOOT_DIR1}/${preserved}" "${BOOT_DIR2}/${preserved}" || die "bootstrap rerun did not preserve ${preserved}"
done

log_info "5. Negative contract and values provenance checks"
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

# Placeholder digest must be rejected by verify_values_provenance
sed 's/a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90/1111111111111111111111111111111111111111111111111111111111111111/g' \
  "${WORK_DIR}/values-aws.yaml" >"${WORK_DIR}/placeholder-values.yaml"
if (verify_values_provenance "${WORK_DIR}/placeholder-values.yaml") 2>/dev/null; then
  die "expected placeholder image digest to be rejected by verify_values_provenance"
fi

log_info "6. Gate failure-path checks (missing prerequisite, skipped test, infra_pause=75, rerun invalidation, tampered receipt, staging DB rejected by T-029)"
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

# 6a. T-090 staging must fail before T-104 readiness has passed
if "${STAGE_DIR}/scripts/gate.sh" \
  --gate staging --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}" 2>/dev/null; then
  die "expected staging gate to fail when readiness receipt is missing"
fi

# 6b. Pass readiness, then run a failing readiness and confirm the prior receipt is invalidated and blocks staging
"${STAGE_DIR}/scripts/gate.sh" \
  --gate readiness --cloud aws --contract "${WORK_DIR}/contract-aws.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

cat >"${WORK_DIR}/skipped-readiness.json" <<'EOF'
{
  "passed": ["kube_chart_install", "kube_m1_roundtrip", "kube_cancel_restart"],
  "failed": [],
  "skipped": ["kube_secrets_kms"]
}
EOF
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

# 6c. infra_pause must exit 75 (EX_TEMPFAIL)
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

# 6d. Re-pass readiness + staging on staging contract, and verify T-029 recovery refuses lower staging backup retention
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

# 6e. Verify contract drift / tampered bundle detection on prerequisite receipts
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

"${STAGE_DIR}/scripts/gate.sh" \
  --gate critical-path --cloud aws --contract "${WORK_DIR}/contract-aws-prod.json" --values "${WORK_DIR}/values-aws.yaml" \
  --results-dir "${RESULTS_DIR}" --evidence-root "${EVIDENCE_ROOT}"

[[ -f "${EVIDENCE_ROOT}/latest-release-aws.json" ]] || die "missing latest-release-aws.json receipt"
log_info "PASS all stage OpenTofu, Helm, ShellCheck, bootstrap idempotency and critical-path gate checks"
