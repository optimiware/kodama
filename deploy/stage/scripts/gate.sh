#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy/stage/scripts/gate.sh \
  --gate <readiness|staging|recovery|platform|release|critical-path> \
  --cloud <aws|gcp|scw> \
  --contract <stage_contract.json> \
  [--results-dir <dir>] \
  [--evidence-root <dir>]

Runs the critical-path qualification gate(s) in topological order:
  1. readiness (T-104) — Full Kubernetes installation readiness & live KMS
  2. staging   (T-090) — Trusted-engineer staging deployment & Loki log search
  3. recovery  (T-029) — Production source/data/key recovery (requires O-11 30d PITR)
  4. platform  (T-061) — Optimi Platform hosted authority & credential integration
  5. release   (T-056) — Supported production & public release gate
EOF
}

GATE=""
CLOUD=""
CONTRACT_FILE=""
RESULTS_DIR=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gate)
      GATE="$2"
      shift 2
      ;;
    --cloud)
      CLOUD="$2"
      shift 2
      ;;
    --contract)
      CONTRACT_FILE="$2"
      shift 2
      ;;
    --results-dir)
      RESULTS_DIR="$2"
      shift 2
      ;;
    --evidence-root)
      EVIDENCE_ROOT="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

validate_cloud "${CLOUD}"
[[ -n "${CONTRACT_FILE}" ]] || die "--contract <stage_contract.json> is required"
validate_stage_contract "${CONTRACT_FILE}"

run_single_gate() {
  local g="$1"
  local check_script="${STAGE_DIR}/scripts/checks/${g}.sh"
  [[ -x "${check_script}" ]] || die "gate script not found or not executable: ${check_script}"

  local args=(
    --cloud "${CLOUD}"
    --contract "${CONTRACT_FILE}"
    --evidence-root "${EVIDENCE_ROOT}"
  )
  if [[ -n "${RESULTS_DIR}" && -f "${RESULTS_DIR}/${g}.json" ]]; then
    args+=(--results "${RESULTS_DIR}/${g}.json")
  fi

  log_info "=== Running gate: ${g} (cloud=${CLOUD}) ==="
  "${check_script}" "${args[@]}"
}

case "${GATE}" in
  readiness | staging | recovery | platform | release)
    run_single_gate "${GATE}"
    ;;
  critical-path)
    for g in readiness staging recovery platform release; do
      run_single_gate "${g}"
    done
    ;;
  *)
    die "unknown gate '${GATE}' (expected: readiness, staging, recovery, platform, release, or critical-path)"
    ;;
esac
