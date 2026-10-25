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
  [--values <kodama-values.yaml>] \
  [--results-dir <dir>] \
  [--evidence-root <dir>] \
  [--preflight]

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
VALUES_FILE=""
RESULTS_DIR=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"
PREFLIGHT=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gate) GATE="$2"; shift 2 ;;
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --values) VALUES_FILE="$2"; shift 2 ;;
    --results-dir) RESULTS_DIR="$2"; shift 2 ;;
    --evidence-root) EVIDENCE_ROOT="$2"; shift 2 ;;
    --preflight) PREFLIGHT=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_cloud "${CLOUD}"
[[ -n "${CONTRACT_FILE}" ]] || die "--contract <stage_contract.json> is required"
validate_stage_contract "${CONTRACT_FILE}" "${CLOUD}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-gate-runner.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT

if [[ -z "${VALUES_FILE}" ]]; then
  VALUES_FILE="${WORK_DIR}/kodama-values.yaml"
  render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}"
fi
verify_values_provenance "${VALUES_FILE}"

if [[ "${PREFLIGHT}" == 1 ]]; then
  [[ "${GATE}" == readiness ]] || die "--preflight is supported for readiness only"
  require_cmd helm
  helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${VALUES_FILE}" >/dev/null
  KODAMA_STAGE_PREFLIGHT_ONLY=1 run_stage_go_suite "${CONTRACT_FILE}" "${VALUES_FILE}" "${WORK_DIR}/preflight.json" \
    ./test/e2e/kube-full/... ./test/integration/assembly-execution/...
  log_info "PASS preflight only; no acceptance run or receipt issued"
  exit 0
fi

run_single_gate() {
  local g="$1"
  local check_script="${STAGE_DIR}/scripts/checks/${g}.sh"
  [[ -x "${check_script}" ]] || die "gate script not found or not executable: ${check_script}"

  local args=(
    --cloud "${CLOUD}"
    --contract "${CONTRACT_FILE}"
    --values "${VALUES_FILE}"
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
