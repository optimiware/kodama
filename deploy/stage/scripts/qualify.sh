#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
Usage: deploy/stage/scripts/qualify.sh <command> --session <name> [--contract <stage_contract.json>] [--database scaleway|sql]

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

--database (or KODAMA_QUAL_DATABASE) selects how the session's database kodama_q_<session> is provided:
  scaleway  (default) through the Scaleway API, with the operator's SCW_* credentials and project.
  sql       with SQL run from a short-lived Pod in the stage namespace as the stage database admin, so the session
            needs only KUBECONFIG. The admin credentials come from KODAMA_STAGE_BOOTSTRAP_SECRETS, a JSON file
            holding postgres_admin_username and postgres_admin_password (the cloud root's stage_bootstrap_secrets).
            The same mode must be used for prepare and destroy.
USAGE
}

CMD="${1:-}"
if [[ -n "${CMD}" ]]; then shift; fi
SESSION=""
CONTRACT_FILE=""
DATABASE_MODE="${KODAMA_QUAL_DATABASE:-scaleway}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --session) SESSION="$2"; shift 2 ;;
    --contract) CONTRACT_FILE="$2"; shift 2 ;;
    --database) DATABASE_MODE="$2"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ "${CMD}" =~ ^(prepare|destroy|show)$ ]] || { usage >&2; die "unknown command '${CMD}'"; }
[[ "${DATABASE_MODE}" =~ ^(scaleway|sql)$ ]] || die "--database must be scaleway or sql"
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
PROJECT_ID=""
if [[ "${DATABASE_MODE}" == "scaleway" ]]; then
  PROJECT_ID="${SCW_DEFAULT_PROJECT_ID:-$(python3 - "${STAGE_DIR}/scw/terraform.tfvars" <<'PY' 2>/dev/null || true
import re, sys
print(re.search(r'project_id\s*=\s*"([^"]+)"', open(sys.argv[1]).read()).group(1))
PY
)}"
  [[ -n "${PROJECT_ID}" ]] || die "the Scaleway project id is unknown (SCW_DEFAULT_PROJECT_ID or deploy/stage/scw/terraform.tfvars)"
fi

VARS="${WORK_DIR}/qualification.tfvars.json"
python3 - "${STACK_TFVARS}" "${CONTRACT_FILE}" "${VARS}" "${SESSION}" "${PROJECT_ID}" "${KUBECONFIG_PATH}" "${DATABASE_MODE}" <<'PY'
import json, sys
stack, contract, out, session, project, kubeconfig, mode = sys.argv[1:8]
s = json.load(open(stack))
v = {
    "kubeconfig_path": kubeconfig,
    "project_id": project,
    "manage_database": mode == "scaleway",
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

# sql_database <create|drop> runs the session database's SQL as the stage database admin, from a short-lived Pod in the
# stage namespace (the managed instance has no public endpoint). The admin DSN reaches the Pod as a temporary Secret
# that is removed with the Pod, whatever the outcome. The stage roles are granted exactly as the stage stack grants
# them on its own database; the migration run grants the runtime role the rest by name.
sql_database() {
  local action="$1" database="kodama_q_${SESSION}" pod="kq-sql-${SESSION}" ns="${KODAMA_STAGE_NAMESPACE:-kodama-system}" image rc=0
  local secrets="${KODAMA_STAGE_BOOTSTRAP_SECRETS:-}"
  [[ -n "${secrets}" && -f "${secrets}" ]] || die "--database sql needs KODAMA_STAGE_BOOTSTRAP_SECRETS (JSON with postgres_admin_username and postgres_admin_password)"
  image="$(sed -n '/^variable "psql_image"/,/^}/p' "${STAGE_DIR}/shared/stack/variables.tf" | sed -n 's/^ *default *= *"\(.*\)"/\1/p')"
  [[ -n "${image}" ]] || die "the psql image is not declared in shared/stack/variables.tf"

  python3 - "${secrets}" "${CONTRACT_FILE}" "${WORK_DIR}/admin.dsn" <<'PY'
import json, sys, urllib.parse
secrets, contract, out = sys.argv[1:4]
b = json.load(open(secrets))
pg = json.load(open(contract))["postgres"]
dsn = "postgres://%s:%s@%s:%s/postgres?sslmode=%s" % (
    urllib.parse.quote(b["postgres_admin_username"], safe=""), urllib.parse.quote(b["postgres_admin_password"], safe=""),
    pg["host"], pg["port"], pg["ssl_mode"])
with open(out, "w") as fh:
    fh.write(dsn)
PY
  chmod 0600 "${WORK_DIR}/admin.dsn"
  local migration runtime
  migration="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["postgres"]["migration_role"])' "${CONTRACT_FILE}")"
  runtime="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["postgres"]["runtime_role"])' "${CONTRACT_FILE}")"

  # shellcheck disable=SC2016 # $DSN must expand in the Pod, from its Secret, not here
  local remote='psql "$DSN" -v ON_ERROR_STOP=1 -q'
  local sql
  case "${action}" in
    create) sql="SELECT 'CREATE DATABASE ${database}' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${database}')\\gexec
GRANT ALL PRIVILEGES ON DATABASE ${database} TO ${migration};
GRANT CONNECT ON DATABASE ${database} TO ${runtime};
\\connect ${database}
GRANT ALL ON SCHEMA public TO ${migration};
GRANT USAGE ON SCHEMA public TO ${runtime};" ;;
    drop) sql="DROP DATABASE IF EXISTS ${database} WITH (FORCE);" ;;
  esac

  kubectl -n "${ns}" delete "pod/${pod}" "secret/${pod}" --ignore-not-found --wait=true >/dev/null
  kubectl -n "${ns}" create secret generic "${pod}" --from-file=dsn="${WORK_DIR}/admin.dsn" >/dev/null
  {
    kubectl -n "${ns}" apply -f - >/dev/null <<EOF &&
apiVersion: v1
kind: Pod
metadata: {name: ${pod}}
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 70, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: psql
    image: ${image}
    command: ["sleep", "600"]
    env: [{name: DSN, valueFrom: {secretKeyRef: {name: ${pod}, key: dsn}}}]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
      kubectl -n "${ns}" wait --for=condition=Ready "pod/${pod}" --timeout=3m >/dev/null &&
      printf '%s\n' "${sql}" | kubectl -n "${ns}" exec -i "${pod}" -- sh -c "${remote}"
  } || rc=$?
  kubectl -n "${ns}" delete "pod/${pod}" "secret/${pod}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ "${rc}" -eq 0 ]] || die "could not ${action} database ${database} of session ${SESSION}"
}

case "${CMD}" in
  prepare)
    if [[ "${DATABASE_MODE}" == "sql" ]]; then sql_database create; fi
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
    if [[ "${DATABASE_MODE}" == "sql" ]]; then sql_database drop; fi
    purge_session_objects "${SESSION}" "${CONTRACT_FILE}"
    rm -rf "${SESSION_DIR}"
    log_info "session ${SESSION} destroyed"
    ;;
esac
