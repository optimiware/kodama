#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-104 — Full Kubernetes installation readiness gate.
# Required acceptance checks:
#   - kube_chart_install
#   - kube_m1_roundtrip
#   - kube_cancel_restart
#   - kube_secrets_kms
#   - execution_assembly_roundtrip
#   - execution_assembly_cancel_restart
#   - execution_assembly_result_guard

CLOUD=""
CONTRACT_FILE=""
RESULTS_FILE=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --results) RESULTS_FILE="$2"; shift 2 ;;
    --evidence-root) EVIDENCE_ROOT="$2"; shift 2 ;;
    *) die "readiness.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"
require_cmd helm python3

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-readiness.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

VALUES_FILE="${WORK_DIR}/kodama-values.yaml"
render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}"
helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${VALUES_FILE}" >/dev/null

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/kube-full" ]]; then
    die_infra "T-104 suite directory test/e2e/kube-full is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  FORGE_RESULTS="${RESULTS_FILE}" DEV_STACK=cluster \
    python3 "${REPO_ROOT}/scripts/dev/go_results.py" --integration \
    ./test/e2e/kube-full/... ./test/integration/assembly-execution/...
fi

REQUIRED_TESTS=(
  kube_chart_install
  kube_m1_roundtrip
  kube_cancel_restart
  kube_secrets_kms
  execution_assembly_roundtrip
  execution_assembly_cancel_restart
  execution_assembly_result_guard
)

set +e
verify_forge_results "${RESULTS_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate readiness --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-104 readiness acceptance checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate readiness --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-104 full Kubernetes readiness and live KMS verified")"
log_info "PASS gate=readiness (T-104) evidence=${bundle}"
