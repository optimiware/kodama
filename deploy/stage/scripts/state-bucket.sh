#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
Usage: deploy/stage/scripts/state-bucket.sh [ensure|show]

ensure  (default) Creates the OpenTofu state bucket once with the scw CLI, private and versioned, or confirms that it is
        already there and versioned. Safe to run again. Prints the export line for KODAMA_TFSTATE_BUCKET.
show    Prints the bucket name and its versioning status without changing anything.

The bucket is deliberately created outside OpenTofu and no script deletes it, so `tofu destroy` of any cluster can never
reach the state. Versioning keeps every earlier state object, so a bad overwrite or delete can be restored.

Needs the scw CLI, authenticated for the project (SCW_ACCESS_KEY, SCW_SECRET_KEY, SCW_DEFAULT_PROJECT_ID).
The name is KODAMA_TFSTATE_BUCKET, or kodama-tfstate-<first 8 characters of SCW_DEFAULT_PROJECT_ID>.
USAGE
}

CMD="${1:-ensure}"
case "${CMD}" in
  ensure | show) ;;
  -h | --help) usage; exit 0 ;;
  *) usage >&2; die "unknown command '${CMD}'" ;;
esac

require_cmd scw python3
BUCKET="${KODAMA_TFSTATE_BUCKET:-}"
if [[ -z "${BUCKET}" ]]; then
  [[ -n "${SCW_DEFAULT_PROJECT_ID:-}" ]] || die "set KODAMA_TFSTATE_BUCKET or SCW_DEFAULT_PROJECT_ID"
  BUCKET="kodama-tfstate-${SCW_DEFAULT_PROJECT_ID:0:8}"
fi
[[ "${BUCKET}" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || die "invalid bucket name '${BUCKET}'"

# bucket_versioning prints enabled or disabled; it fails when the bucket is not readable with this credential.
bucket_versioning() {
  local info
  info="$(scw object bucket get "name=${BUCKET}" "region=${SCW_S3_REGION}" -o json 2>/dev/null)" || return 1
  python3 -c '
import json, sys
info = json.load(sys.stdin)
on = False
for key, value in info.items():
    if "versioning" in key.lower():
        on = value is True or str(value).lower() in ("enabled", "true")
print("enabled" if on else "disabled")' <<<"${info}"
}

if [[ "${CMD}" == "show" ]]; then
  status="$(bucket_versioning)" || die "bucket ${BUCKET} does not exist or is not readable"
  printf '%s versioning=%s\n' "${BUCKET}" "${status}"
  exit 0
fi

if status="$(bucket_versioning)"; then
  log_info "state bucket ${BUCKET} already exists (versioning ${status})"
else
  log_info "creating state bucket ${BUCKET} in ${SCW_S3_REGION}"
  scw object bucket create "name=${BUCKET}" "region=${SCW_S3_REGION}" enable-versioning=true >/dev/null ||
    die "could not create bucket ${BUCKET}: bucket names are global, so pick another with KODAMA_TFSTATE_BUCKET"
  status="$(bucket_versioning)" || die "bucket ${BUCKET} was created but cannot be read back"
fi

if [[ "${status}" != "enabled" ]]; then
  log_info "enabling versioning on ${BUCKET}"
  scw object bucket update "name=${BUCKET}" "region=${SCW_S3_REGION}" enable-versioning=true >/dev/null
  [[ "$(bucket_versioning)" == "enabled" ]] || die "versioning could not be enabled on ${BUCKET}; refusing to use it for state"
fi

printf 'export KODAMA_TFSTATE_BUCKET=%s\n' "${BUCKET}"
