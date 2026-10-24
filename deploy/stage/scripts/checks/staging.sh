#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-090 — Trusted-engineer staging deployment gate.
# Requires a passing T-104 (readiness) receipt for the same cloud, installation_id and git commit.
# Required acceptance checks:
#   - stage_clean_install
#   - stage_e2e_roundtrip
#   - stage_restart_reconcile
#   - stage_loki_log_search
#   - stage_dev_adapter_refused
#   - stage_retention_uninstall

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
    *) die "staging.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"

# Enforce prerequisite: T-104 readiness must have passed for this cloud and installation.
READINESS_RECEIPT="${EVIDENCE_ROOT}/latest-readiness-${CLOUD}.json"
[[ -f "${READINESS_RECEIPT}" ]] || die "T-090 staging gate requires a passing T-104 readiness receipt (${READINESS_RECEIPT})"

GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"
python3 - "${READINESS_RECEIPT}" "${CONTRACT_FILE}" "${GIT_SHA}" <<'PY'
import json
import sys

receipt_path, contract_path, git_sha = sys.argv[1:4]
r = json.load(open(receipt_path, "r", encoding="utf-8"))
c = json.load(open(contract_path, "r", encoding="utf-8"))

if r.get("status") != "passed":
    raise SystemExit("T-104 readiness receipt status is not 'passed'")
if r.get("installation_id") != c.get("installation_id"):
    raise SystemExit("T-104 readiness receipt installation_id does not match stage_contract")
if r.get("git_commit") != git_sha:
    raise SystemExit(f"T-104 readiness receipt commit {r.get('git_commit')} != current HEAD {git_sha}")
PY

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-staging.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/staging" ]]; then
    die_infra "T-090 suite directory test/e2e/staging is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  FORGE_RESULTS="${RESULTS_FILE}" DEV_STACK=cluster \
    python3 "${REPO_ROOT}/scripts/dev/go_results.py" --integration ./test/e2e/staging/...
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
verify_forge_results "${RESULTS_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate staging --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-090 staging smoke or Loki search check failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate staging --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-090 trusted-engineer staging smoke and Loki log search verified")"
log_info "PASS gate=staging (T-090) evidence=${bundle}"
