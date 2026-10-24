#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-090 — Trusted-engineer staging deployment gate.
# Requires a passing T-104 (readiness) receipt with verified artifact digests and matching contract/values.
# Required acceptance checks:
#   - stage_clean_install
#   - stage_e2e_roundtrip
#   - stage_restart_reconcile
#   - stage_loki_log_search
#   - stage_dev_adapter_refused
#   - stage_retention_uninstall

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
    *) die "staging.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"
invalidate_gate_receipt "${EVIDENCE_ROOT}" "staging" "${CLOUD}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-staging.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${VALUES_FILE}" ]]; then
  VALUES_FILE="${WORK_DIR}/kodama-values.yaml"
  render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}"
fi
verify_values_provenance "${VALUES_FILE}"

# Enforce prerequisite: T-104 readiness receipt + artifact hash integrity + matching topology/values.
verify_prerequisite_receipt "${EVIDENCE_ROOT}" "readiness" "${CLOUD}" "${CONTRACT_FILE}" "${VALUES_FILE}"

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/staging" ]]; then
    die_infra "T-090 suite directory test/e2e/staging is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  set +e
  run_stage_go_suite "${CONTRACT_FILE}" "${VALUES_FILE}" "${RESULTS_FILE}" ./test/e2e/staging/...
  suite_rc=$?
  set -e
  if [[ "${suite_rc}" -eq "${EX_TEMPFAIL}" ]]; then
    exit "${EX_TEMPFAIL}"
  fi
fi

REQUIRED_TESTS=(
  stage_clean_install
  stage_e2e_roundtrip
  stage_restart_reconcile
  stage_loki_log_search
  stage_dev_adapter_refused
  stage_retention_uninstall
)

set +e
verify_forge_results "${RESULTS_FILE}" "${CONTRACT_FILE}" "${VALUES_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate staging --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-090 staging smoke or Loki search check failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate staging --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" --values "${VALUES_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-090 trusted-engineer staging smoke and Loki log search verified")"
log_info "PASS gate=staging (T-090) evidence=${bundle}"
