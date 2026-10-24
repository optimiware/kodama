#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-056 — Supported production and public release gate.
# Requires passing receipts (with verified artifact hashes and matching topology/values) for:
#   - readiness (T-104)
#   - staging   (T-090)
#   - recovery  (T-029, with qualifies_o11_pitr == true)
#   - platform  (T-061)
# Required acceptance checks:
#   - production_upgrade
#   - tenant_offboarding
#   - reference_load
#   - profile_install_restore
#   - hosted_core_compatibility

CLOUD=""
CONTRACT_FILE=""
VALUES_FILE=""
RESULTS_FILE=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --values) VALUES_FILE="$2"; shift 2 ;;
    --results) RESULTS_FILE="$2"; shift 2 ;;
    --evidence-root) EVIDENCE_ROOT="$2"; shift 2 ;;
    *) die "release.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"
invalidate_gate_receipt "${EVIDENCE_ROOT}" "release" "${CLOUD}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-release.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${VALUES_FILE}" ]]; then
  VALUES_FILE="${WORK_DIR}/kodama-values.yaml"
  render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}"
fi
verify_values_provenance "${VALUES_FILE}"

for prereq in readiness staging recovery platform; do
  verify_prerequisite_receipt "${EVIDENCE_ROOT}" "${prereq}" "${CLOUD}" "${CONTRACT_FILE}" "${VALUES_FILE}"
done

python3 - "${EVIDENCE_ROOT}" "${CLOUD}" "${CONTRACT_FILE}" <<'PY'
import json
from pathlib import Path
import sys

root = Path(sys.argv[1])
cloud, contract_path = sys.argv[2:4]
c = json.load(open(contract_path, "r", encoding="utf-8"))

if not c["postgres"].get("qualifies_o11_pitr", False):
    raise SystemExit("T-056 release gate refused: stage_contract does not qualify O-11 30-day PITR")

rec = json.load(open(root / f"latest-recovery-{cloud}.json", "r", encoding="utf-8"))
if not rec.get("postgres_backup", {}).get("qualifies_o11_pitr", False):
    raise SystemExit("T-056 prerequisite 'recovery' receipt did not qualify O-11 30-day PITR")
PY

suite_rc=0
if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/production" ]]; then
    die_infra "T-056 suite directory test/e2e/production is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  set +e
  run_stage_go_suite "${CONTRACT_FILE}" "${VALUES_FILE}" "${RESULTS_FILE}" ./test/e2e/production/...
  suite_rc=$?
  set -e
  if [[ "${suite_rc}" -eq "${EX_TEMPFAIL}" ]]; then
    exit "${EX_TEMPFAIL}"
  fi
fi

REQUIRED_TESTS=(
  production_upgrade
  tenant_offboarding
  reference_load
  profile_install_restore
  hosted_core_compatibility
)

set +e
verify_forge_results "${RESULTS_FILE}" "${CONTRACT_FILE}" "${VALUES_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${suite_rc}" -ne 0 || "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate release --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-056 release acceptance checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate release --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-056 supported production release gate and O-10 reference workload verified")"
log_info "PASS gate=release (T-056) evidence=${bundle}"
