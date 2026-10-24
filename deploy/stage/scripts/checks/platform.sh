#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-061 — Optimi Platform integration and hosted acceptance gate.
# Required acceptance checks:
#   - core_tenant_authority
#   - core_unavailable_no_fallback
#   - core_pinned_credentials
#   - core_tool_audience_and_entitlement
#   - core_unknown_outcome
#   - core_data_egress
#   - core_agent_delivery_slice
#   - core_disable_restore

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
    *) die "platform.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-platform.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/platform-integration" ]]; then
    die_infra "T-061 suite directory test/e2e/platform-integration is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  FORGE_RESULTS="${RESULTS_FILE}" DEV_STACK=cluster \
    python3 "${REPO_ROOT}/scripts/dev/go_results.py" --integration ./test/e2e/platform-integration/...
fi

REQUIRED_TESTS=(
  core_tenant_authority
  core_unavailable_no_fallback
  core_pinned_credentials
  core_tool_audience_and_entitlement
  core_unknown_outcome
  core_data_egress
  core_agent_delivery_slice
  core_disable_restore
)

set +e
verify_forge_results "${RESULTS_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate platform --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-061 Optimi Platform integration checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate platform --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-061 Optimi Platform authority and credential custody verified")"
log_info "PASS gate=platform (T-061) evidence=${bundle}"
