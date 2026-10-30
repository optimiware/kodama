#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
Usage: deploy/stage/scripts/cluster.sh list
       deploy/stage/scripts/cluster.sh destroy --cloud <aws|gcp|scw> --name <cluster> [--yes]

list     Prints the clusters that have OpenTofu state in the bucket (<cloud>/<name>/infra.tfstate), and whether this
         machine holds their operator files.
destroy  Destroys the cluster's cloud infrastructure (cluster, database, buckets, Key Manager keys, registry; everything
         inside the cluster goes with it, including its qualification sessions), then removes its state objects and its
         local files except <cloud>.tfvars. The state bucket and every other cluster are never touched; the removed state
         objects stay recoverable as earlier versions of the bucket. Stage clusters are disposable; a cluster whose
         tfvars say environment_tier = "production" is refused.

Both need KODAMA_TFSTATE_BUCKET and SCW_ACCESS_KEY / SCW_SECRET_KEY (see state-bucket.sh). destroy asks you to type the
cluster name unless --yes is given.
USAGE
}

CMD="${1:-}"
if [[ -n "${CMD}" ]]; then shift; fi
CLOUD=""
NAME="${KODAMA_STAGE_CLUSTER:-}"
ASSUME_YES=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud) CLOUD="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --yes) ASSUME_YES=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

case "${CMD}" in
  list)
    require_cmd python3
    state_objects list | python3 -c '
import os, re, sys
state = os.environ.get("STAGE_STATE_DIR") or os.path.expanduser("~/kodama-stage/state")
for key in sorted(sys.stdin.read().split()):
    m = re.fullmatch(r"([a-z]+)/([^/]+)/infra\.tfstate", key)
    if m:
        has_files = os.path.isdir(os.path.join(state, "clusters", m.group(2)))
        print(m.group(1), m.group(2), "local files" if has_files else "no local files", sep="\t")'
    ;;
  destroy)
    require_cmd tofu python3
    [[ -n "${CLOUD}" ]] || die "--cloud is required"
    use_cluster "${CLOUD}" "${NAME}"
    require_cluster_tfvars
    if grep -Eq '^[[:space:]]*environment_tier[[:space:]]*=[[:space:]]*"production"' "${TFVARS}"; then
      die "cluster '${NAME}' is a production tier: this script only destroys disposable stage clusters"
    fi
    if [[ "${ASSUME_YES}" -ne 1 ]]; then
      [[ -t 0 ]] || die "not a terminal: pass --yes to destroy cluster '${NAME}' without a prompt"
      printf 'This destroys every resource of %s cluster %s (%s). Type its name to continue: ' "${CLOUD}" "${NAME}" "${INSTALLATION_ID}" >&2
      read -r answer
      [[ "${answer}" == "${NAME}" ]] || die "aborted"
    fi

    init_infra_backend
    infra_tf validate
    infra_tf destroy -input=false -auto-approve "${CLOUD_VARS[@]}"

    # Exact keys only: this cluster's state objects and their lock files, nothing else in the bucket.
    prefix="${CLOUD}/${NAME}/"
    keys="$(state_objects list "${prefix}")"
    if [[ -n "${keys}" ]]; then
      # shellcheck disable=SC2086 # keys are bucket object keys without whitespace
      state_objects delete ${keys}
    fi
    rm -rf "${CDIR}/tfdata" "${CDIR}/qualification" "${CDIR}/kubeconfig" "${CDIR}/${CLOUD}.tfstate" "${CDIR}/${CLOUD}.tfstate.backup"
    log_info "cluster ${NAME} destroyed; ${TFVARS} was kept so the name can be deployed again"
    ;;
  *)
    usage >&2
    die "unknown command '${CMD}'"
    ;;
esac
