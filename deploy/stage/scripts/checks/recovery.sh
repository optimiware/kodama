#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# T-029 — Production source/data/key recovery gate.
# Enforces O-11 retention/recovery baseline:
#   - postgres.qualifies_o11_pitr == true (30-day PITR, RPO <= 15m; reduced staging targets cannot pass T-029)
# Required acceptance checks:
#   - restore_full_factory
#   - stale_publisher_after_restore
#   - missing_retained_object
#   - old_token_denied

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
    *) die "recovery.sh: unknown argument $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}"

# Verify that the database profile meets the O-11 30-day PITR / 15-minute RPO baseline.
python3 - "${CONTRACT_FILE}" <<'PY'
import json
import sys

c = json.load(open(sys.argv[1], "r", encoding="utf-8"))
pg = c["postgres"]
if not pg.get("qualifies_o11_pitr", False):
    raise SystemExit(
        f"T-029 recovery gate refused: cloud={c['cloud']} tier={c['environment_tier']} "
        f"(pitr_enabled={pg.get('pitr_enabled')}, retention={pg.get('backup_retention_days')}d, "
        f"rpo={pg.get('rpo_minutes_estimate')}m) does not satisfy the O-11 30-day PITR / 15m RPO baseline. "
        "Lower staging targets are permitted for T-090 staging only, never for T-029/T-056."
    )
if int(pg.get("backup_retention_days", 0)) < 30 or int(pg.get("rpo_minutes_estimate", 999)) > 15:
    raise SystemExit("T-029 recovery gate requires backup_retention_days >= 30 and rpo_minutes_estimate <= 15")
PY

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-recovery.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${RESULTS_FILE}" ]]; then
  if [[ ! -d "${REPO_ROOT}/test/e2e/recovery" ]]; then
    die_infra "T-029 suite directory test/e2e/recovery is not yet scheduled/implemented"
  fi
  RESULTS_FILE="${WORK_DIR}/forge_results.json"
  FORGE_RESULTS="${RESULTS_FILE}" DEV_STACK=cluster \
    python3 "${REPO_ROOT}/scripts/dev/go_results.py" --integration ./test/e2e/recovery/...
fi

REQUIRED_TESTS=(
  restore_full_factory
  stale_publisher_after_restore
  missing_retained_object
  old_token_denied
)

set +e
verify_forge_results "${RESULTS_FILE}" "${REQUIRED_TESTS[@]}"
rc=$?
set -e

if [[ "${rc}" -eq "${EX_TEMPFAIL}" ]]; then
  exit "${EX_TEMPFAIL}"
elif [[ "${rc}" -ne 0 ]]; then
  "${STAGE_DIR}/scripts/evidence.sh" \
    --gate recovery --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
    --results "${RESULTS_FILE}" --status failed --evidence-root "${EVIDENCE_ROOT}" \
    --notes "T-029 recovery drill acceptance checks failed" >/dev/null
  exit 1
fi

bundle="$("${STAGE_DIR}/scripts/evidence.sh" \
  --gate recovery --cloud "${CLOUD}" --contract "${CONTRACT_FILE}" \
  --results "${RESULTS_FILE}" --status passed --evidence-root "${EVIDENCE_ROOT}" \
  --notes "T-029 full factory recovery and O-11 30-day PITR baseline verified")"
log_info "PASS gate=recovery (T-029) evidence=${bundle}"
