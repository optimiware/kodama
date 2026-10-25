#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy/stage/scripts/evidence.sh \
  --gate <readiness|staging|recovery|platform|release> \
  --cloud <aws|gcp|scw> \
  --contract <stage_contract.json> \
  --values <kodama-values.yaml> \
  --results <forge_results.json> \
  --status <passed|failed> \
  [--evidence-root <dir>] \
  [--notes <text>]

Produces an immutable evidence directory containing:
  - manifest.json (commit, cloud, installation_id, gate, status, bundle_dir, SHA-256 digests, backup/PITR posture)
  - stage_contract.json
  - kodama-values.yaml (exact deployed values, verified free of placeholder digests unless STAGE_ALLOW_PLACEHOLDERS=1)
  - forge_results.json
  - SUMMARY.md
And updates <evidence-root>/latest-<gate>-<cloud>.json for downstream gates.
EOF
}

GATE=""
CLOUD=""
CONTRACT_FILE=""
VALUES_FILE=""
RESULTS_FILE=""
STATUS=""
EVIDENCE_ROOT="${STAGE_EVIDENCE_ROOT:-${STAGE_DIR}/.evidence}"
NOTES=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gate) GATE="$2"; shift 2 ;;
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --values) VALUES_FILE="$2"; shift 2 ;;
    --results) RESULTS_FILE="$2"; shift 2 ;;
    --status) STATUS="$2"; shift 2 ;;
    --evidence-root) EVIDENCE_ROOT="$2"; shift 2 ;;
    --notes) NOTES="$2"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_cloud "${CLOUD}"
validate_stage_contract "${CONTRACT_FILE}" "${CLOUD}"
[[ -f "${VALUES_FILE}" ]] || die "values file missing: ${VALUES_FILE}"
[[ -f "${RESULTS_FILE}" ]] || die "results file missing: ${RESULTS_FILE}"
verify_values_provenance "${VALUES_FILE}"

GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BUNDLE_DIR="${EVIDENCE_ROOT}/${GATE}/${TIMESTAMP}-${CLOUD}-${GIT_SHA}-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
mkdir -p "${BUNDLE_DIR}"

cp "${CONTRACT_FILE}" "${BUNDLE_DIR}/stage_contract.json"
cp "${VALUES_FILE}" "${BUNDLE_DIR}/kodama-values.yaml"
cp "${RESULTS_FILE}" "${BUNDLE_DIR}/forge_results.json"

scan_no_plaintext_secrets "${BUNDLE_DIR}"

python3 - "${BUNDLE_DIR}" "${GATE}" "${CLOUD}" "${GIT_SHA}" "${TIMESTAMP}" "${STATUS}" "${NOTES}" "${EVIDENCE_ROOT}" <<'PY'
import hashlib
import json
from pathlib import Path
import sys
import subprocess
import os

bundle_dir = Path(sys.argv[1]).resolve()
gate, cloud, git_sha, ts, status, notes, root_dir = sys.argv[2:9]

def sha256_file(p: Path) -> str:
    return "sha256:" + hashlib.sha256(p.read_bytes()).hexdigest()

contract = json.loads((bundle_dir / "stage_contract.json").read_text(encoding="utf-8"))
results = json.loads((bundle_dir / "forge_results.json").read_text(encoding="utf-8"))
pg = contract["postgres"]
provenance = results.get("provenance") or {}
if status == "passed":
    for field, expected in {
        "git_commit": git_sha,
        "contract_sha256": sha256_file(bundle_dir / "stage_contract.json"),
        "values_sha256": sha256_file(bundle_dir / "kodama-values.yaml"),
    }.items():
        if provenance.get(field) != expected:
            raise SystemExit(f"refusing passed evidence: {field} changed before publication")
    if provenance.get("suite_passed") is not True:
        raise SystemExit("refusing passed evidence: suite did not pass")
    if provenance.get("candidate_stable") is True:
        dirty = subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=normal"], cwd=os.environ["REPO_ROOT"], text=True).strip()
        if dirty:
            raise SystemExit("refusing passed evidence: candidate became dirty before publication")

manifest = {
    "schema_version": 1,
    "gate": gate,
    "status": status,
    "timestamp_utc": ts,
    "git_commit": git_sha,
    "bundle_dir": str(bundle_dir),
    "cloud": cloud,
    "installation_id": contract["installation_id"],
    "region": contract["region"],
    "environment_tier": contract["environment_tier"],
    "kms_provider": contract["kms"]["provider"],
    "kms_current_key_label": contract["kms"]["current"],
    "postgres_backup": {
        "pitr_enabled": pg["pitr_enabled"],
        "backup_retention_days": pg["backup_retention_days"],
        "rpo_minutes_estimate": pg["rpo_minutes_estimate"],
        "qualifies_o11_pitr": pg["qualifies_o11_pitr"],
    },
    "artifacts": {
        "stage_contract.json": sha256_file(bundle_dir / "stage_contract.json"),
        "kodama-values.yaml": sha256_file(bundle_dir / "kodama-values.yaml"),
        "forge_results.json": sha256_file(bundle_dir / "forge_results.json"),
    },
    "passed_tests": results.get("passed", []),
    "failed_tests": results.get("failed", []),
    "skipped_tests": results.get("skipped", []),
    "notes": notes,
    "suite_provenance": provenance,
    "acceptances": results.get("acceptances", []),
}

manifest_path = bundle_dir / "manifest.json"
manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")

summary = f"""# Stage Gate Evidence — `{gate}` ({cloud})

- **Status:** `{status}`
- **Timestamp (UTC):** `{ts}`
- **Git Commit:** `{git_sha}`
- **Installation ID:** `{contract['installation_id']}`
- **Cloud / Region / Tier:** `{cloud}` / `{contract['region']}` / `{contract['environment_tier']}`
- **KMS Provider:** `{contract['kms']['provider']}` (current label `{contract['kms']['current']}`)
- **PostgreSQL Backup Posture:** PITR=`{pg['pitr_enabled']}`, retention=`{pg['backup_retention_days']}d`, est. RPO=`{pg['rpo_minutes_estimate']}m`, O-11 30d PITR qualified=`{pg['qualifies_o11_pitr']}`
- **Values Digest:** `{manifest['artifacts']['kodama-values.yaml']}`
- **Passed Acceptance Checks ({len(manifest['passed_tests'])}):** `{', '.join(manifest['passed_tests']) or 'none'}`
- **Notes:** {notes or 'none'}
"""
(bundle_dir / "SUMMARY.md").write_text(summary, encoding="utf-8")

latest_path = Path(root_dir) / f"latest-{gate}-{cloud}.json"
latest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
print(str(bundle_dir))
PY
