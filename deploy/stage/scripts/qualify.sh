#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
Usage: deploy/stage/scripts/qualify.sh <command> --session <name> [--contract <stage_contract.json>]

  prepare   Creates (idempotently) the qualification session of the stage installation: its database on the stage
            PostgreSQL instance, namespace, generated keys and Secrets, NATS server and database relays, through
            deploy/stage/qualification. Writes the session handle, its derived contract and the Helm values of its
            Kodama release to <state>/qualification/<session>/. The Kodama release itself is installed by the tests.
  destroy   Uninstalls the session's Kodama release, deletes its tenant namespaces, destroys its resources and
            removes its database. The session's state is removed with it.
  show      Prints the session handle.

State lives outside the checkout (KODAMA_QUAL_STATE_DIR, then STAGE_STATE_DIR, default ~/kodama-stage/state).
prepare and destroy need KUBECONFIG, KODAMA_IMAGES_FILE and the operator's Scaleway credentials (SCW_*); destroy
also needs go (it purges the session's qualification/<session>/ objects).
USAGE
}

CMD="${1:-}"
if [[ -n "${CMD}" ]]; then shift; fi
SESSION=""
CONTRACT_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --session) SESSION="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ "${CMD}" =~ ^(prepare|destroy|show)$ ]] || { usage >&2; die "unknown command '${CMD}'"; }
[[ "${SESSION}" =~ ^[a-z][a-z0-9]{0,9}$ ]] || die "--session must be 1-10 lowercase letters or digits, starting with a letter"

STATE_ROOT="${KODAMA_QUAL_STATE_DIR:-${STAGE_STATE_DIR:-${HOME}/kodama-stage/state}}"
refuse_legacy_state_dir "${STATE_ROOT}"
SESSION_DIR="${STATE_ROOT}/qualification/${SESSION}"
QUAL_DIR="${STAGE_DIR}/qualification"

if [[ "${CMD}" == "show" ]]; then
  [[ -f "${SESSION_DIR}/session.json" ]] || die "no prepared session '${SESSION}'"
  cat "${SESSION_DIR}/session.json"
  exit 0
fi

require_cmd tofu helm kubectl python3
[[ -n "${CONTRACT_FILE}" ]] || die "--contract is required"
validate_stage_contract "${CONTRACT_FILE}"
KUBECONFIG_PATH="${KUBECONFIG:-${HOME}/.kube/config}"
[[ -f "${KUBECONFIG_PATH}" ]] || die_infra "KUBECONFIG is unset or unreadable"

mkdir -p "${SESSION_DIR}"
chmod 0700 "${STATE_ROOT}" "${SESSION_DIR}" 2>/dev/null || true
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-qualify.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

# The images, endpoints and issuer settings are those of the stage deployment, so the session qualifies the same
# artifacts: they come from the same inputs that write the stage's own variables.
STACK_TFVARS="${WORK_DIR}/stack.tfvars.json"
write_stack_tfvars "${CONTRACT_FILE}" "${STACK_TFVARS}" "${KUBECONFIG_PATH}"
PROJECT_ID="${SCW_DEFAULT_PROJECT_ID:-$(python3 - "${STAGE_DIR}/scw/terraform.tfvars" <<'PY' 2>/dev/null || true
import re, sys
print(re.search(r'project_id\s*=\s*"([^"]+)"', open(sys.argv[1]).read()).group(1))
PY
)}"
[[ -n "${PROJECT_ID}" ]] || die "the Scaleway project id is unknown (SCW_DEFAULT_PROJECT_ID or deploy/stage/scw/terraform.tfvars)"

VARS="${WORK_DIR}/qualification.tfvars.json"
python3 - "${STACK_TFVARS}" "${CONTRACT_FILE}" "${VARS}" "${SESSION}" "${PROJECT_ID}" "${KUBECONFIG_PATH}" <<'PY'
import json, sys
stack, contract, out, session, project, kubeconfig = sys.argv[1:7]
s = json.load(open(stack))
v = {
    "kubeconfig_path": kubeconfig,
    "project_id": project,
    "session": session,
    "stage_contract": json.load(open(contract)),
    "kodama_image": s["kodama_image"],
    "example_workload_image": s["example_workload_image"],
    "git_url": s["git_url"],
    "registry_url": s["registry_url"],
    "oidc_issuer": s["oidc_issuer"],
    "oidc_audience": s["oidc_audience"],
}
with open(out, "w") as fh:
    json.dump(v, fh)
PY
chmod 0600 "${VARS}"

tf() { tofu -chdir="${QUAL_DIR}" "$@"; }
tf init -input=false -backend-config="path=${SESSION_DIR}/qualification.tfstate" -reconfigure >/dev/null

json_field() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d[sys.argv[2]])' "$1" "$2"; }

# purge_session_objects removes the session's object prefix (qualification/<session>/) from the artifacts bucket.
# The installation's own data and other sessions are untouchable by construction (see the tool).
purge_session_objects() {
  local session="$1" contract="$2" endpoint region bucket access secret
  read -r endpoint region bucket < <(python3 -c 'import json,sys; o=json.load(open(sys.argv[1]))["object_storage"]; print(o["endpoint"], o["region"], o["artifacts_bucket"])' "${contract}")
  access="$(kubectl -n kodama-system get secret kodama-runtime -o jsonpath='{.data.s3-access-key}' | base64 -d)"
  secret="$(kubectl -n kodama-system get secret kodama-runtime -o jsonpath='{.data.s3-secret-key}' | base64 -d)"
  [[ -n "${access}" && -n "${secret}" ]] || die "the stage object-store credential is unavailable; session ${session}'s objects were not purged"
  PURGE_ACCESS_KEY="${access}" PURGE_SECRET_KEY="${secret}" \
    go run "${REPO_ROOT}/deploy/stage/cmd/purge-session-objects" --endpoint "${endpoint}" --region "${region}" --bucket "${bucket}" --session "${session}"
}

case "${CMD}" in
  prepare)
    tf apply -input=false -auto-approve -var-file="${VARS}"
    tf output -json session >"${SESSION_DIR}/session.json"
    tf output -json contract >"${SESSION_DIR}/contract.json"
    tf output -raw kodama_values >"${SESSION_DIR}/values.yaml"
    helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${SESSION_DIR}/values.yaml" >/dev/null
    log_info "session ${SESSION} prepared: $(json_field "${SESSION_DIR}/session.json" url)"
    ;;
  destroy)
    if [[ -f "${SESSION_DIR}/session.json" ]]; then
      ns="$(json_field "${SESSION_DIR}/session.json" namespace)"
      # Only "already gone" is tolerated; any other failure stops the teardown with the session state intact.
      uninstall_out="$(helm uninstall kodama -n "${ns}" --wait --timeout 5m 2>&1)" || {
        grep -qiE 'release: not found|not found' <<<"${uninstall_out}" || die "helm uninstall failed for session ${SESSION}: ${uninstall_out}"
      }
      for tenant_ns in $(python3 -c 'import json,sys; print(" ".join(t["namespace"] for t in json.load(open(sys.argv[1]))["tenants"]))' "${SESSION_DIR}/session.json"); do
        kubectl delete namespace "${tenant_ns}" --ignore-not-found --wait=true --timeout=5m >/dev/null ||
          die "could not delete tenant namespace ${tenant_ns} of session ${SESSION}"
      done
    fi
    if ! tf destroy -input=false -auto-approve -var-file="${VARS}"; then
      # State can name a resource that no longer exists (a session torn down by hand), which makes the refresh
      # fail. Destroy again from the recorded state without refreshing; a real failure still fails here.
      log_info "WARNING: destroy failed after a refresh; retrying from the recorded state without refreshing"
      tf destroy -input=false -auto-approve -refresh=false -var-file="${VARS}"
    fi
    purge_session_objects "${SESSION}" "${CONTRACT_FILE}"
    rm -rf "${SESSION_DIR}"
    log_info "session ${SESSION} destroyed"
    ;;
esac
