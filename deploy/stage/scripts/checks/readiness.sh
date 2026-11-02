#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-104 — Full Kubernetes installation readiness gate for the ADR 0013 kernel.
# Required acceptance checks:
#   - kernel_clean_install
#   - kernel_pipeline
#   - kernel_git
#   - kernel_secrets_kms
#   - kernel_cancel_restart

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
    *) die "readiness.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}" "${CLOUD}"
require_cmd helm python3

# Immediately invalidate any previous receipt before running so a failed rerun cannot leave a stale pass.
invalidate_gate_receipt "${EVIDENCE_ROOT}" "readiness" "${CLOUD}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-readiness.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${VALUES_FILE}" ]]; then
  VALUES_FILE="${WORK_DIR}/kodama-values.yaml"
  render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}"
fi
verify_values_provenance "${VALUES_FILE}"
helm lint --strict "${REPO_ROOT}/deploy/charts/kodama-kernel" -f "${VALUES_FILE}" >/dev/null

suite_rc=0
if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/kube-kernel" ]]; then
    die_infra "T-104 suite directory test/e2e/kube-kernel is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  set +e
  run_stage_go_suite "${CONTRACT_FILE}" "${VALUES_FILE}" "${RESULTS_FILE}" \
    ./test/e2e/kube-kernel/...
  suite_rc=$?
  set -e
  if [[ "${suite_rc}" -eq "${EX_TEMPFAIL}" ]]; then
    exit "${EX_TEMPFAIL}"
  fi
fi

REQUIRED_TESTS=(
  kernel_clean_install
  kernel_pipeline
  kernel_git
  kernel_secrets_kms
  kernel_cancel_restart
)

set +e
verify_forge_results "${RESULTS_FILE}" "${CONTRACT_FILE}" "${VALUES_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${suite_rc}" -ne 0 || "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate readiness --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-104 readiness acceptance checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate readiness --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-104 kernel readiness and KMS-backed secrets verified")"
log_info "PASS gate=readiness (T-104) evidence=${bundle}"
