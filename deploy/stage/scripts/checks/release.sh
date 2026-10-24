#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-056 — Supported production and public release gate.
# Requires passing receipts for:
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
RESULTS_FILE=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --results) RESULTS_FILE="$2"; shift 2 ;;
    --evidence-root) EVIDENCE_ROOT="$2"; shift 2 ;;
    *) die "release.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"

GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"

python3 - "${EVIDENCE_ROOT}" "${CLOUD}" "${CONTRACT_FILE}" "${GIT_SHA}" <<'PY'
import json
from pathlib import Path
import sys

root = Path(sys.argv[1])
cloud, contract_path, git_sha = sys.argv[2:5]
c = json.load(open(contract_path, "r", encoding="utf-8"))

if not c["postgres"].get("qualifies_o11_pitr", False):
    raise SystemExit("T-056 release gate refused: stage_contract does not qualify O-11 30-day PITR")

for prereq in ("readiness", "staging", "recovery", "platform"):
    p = root / f"latest-{prereq}-{cloud}.json"
    if not p.is_file():
        raise SystemExit(f"T-056 release gate missing prerequisite receipt: {p}")
    rec = json.load(open(p, "r", encoding="utf-8"))
    if rec.get("status") != "passed":
        raise SystemExit(f"T-056 prerequisite '{prereq}' status is {rec.get('status')!r}, want 'passed'")
    if rec.get("installation_id") != c.get("installation_id"):
        raise SystemExit(f"T-056 prerequisite '{prereq}' installation_id mismatch")
    if rec.get("git_commit") != git_sha:
        raise SystemExit(f"T-056 prerequisite '{prereq}' commit {rec.get('git_commit')} != HEAD {git_sha}")
    if prereq == "recovery" and not rec.get("postgres_backup", {}).get("qualifies_o11_pitr", False):
        raise SystemExit("T-056 prerequisite 'recovery' receipt did not qualify O-11 30-day PITR")
PY

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-release.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/production" ]]; then
    die_infra "T-056 suite directory test/e2e/production is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  FORGE_RESULTS="${RESULTS_FILE}" DEV_STACK=cluster \
    python3 "${REPO_ROOT}/scripts/dev/go_results.py" --integration ./test/e2e/production/...
fi

REQUIRED_TESTS=(
  production_upgrade
  tenant_offboarding
  reference_load
  profile_install_restore
  hosted_core_compatibility
)

set +e
verify_forge_results "${RESULTS_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate release --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-056 release acceptance checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate release --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-056 supported production release gate and O-10 reference workload verified")"
log_info "PASS gate=release (T-056) evidence=${bundle}"
