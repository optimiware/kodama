#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy/stage/scripts/deploy.sh --cloud <aws|gcp|scw> [--contract-out <path>] [--values-out <path>] [--validate-only]

1. Validates (or applies) the cloud OpenTofu root under deploy/stage/<cloud>.
2. Exports the normalized stage_contract JSON and validates its C-18/O-05/O-26 invariants.
3. Renders the shared Kodama Helm values and validates them with `helm lint --strict` and `helm template`.
4. Unless --validate-only is set, applies deploy/stage/shared/stack, which generates and pushes every
   Secret (CA and leaf certificates, credentials, database roles) into Kubernetes before installing the
   charts, and writes the deployed Kodama Helm values to --values-out for downstream evidence gates.
EOF
}

CLOUD=""
VALIDATE_ONLY=0
CONTRACT_OUT=""
VALUES_OUT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud) CLOUD="$2"; shift 2 ;;
    --contract-out) CONTRACT_OUT="$2"; shift 2 ;;
    --values-out) VALUES_OUT="$2"; shift 2 ;;
    --validate-only) VALIDATE_ONLY=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_cloud "${CLOUD}"
require_cmd tofu helm python3

# Operator-owned directory for OpenTofu state (database/API-key secrets never live in the checkout).
STATE_DIR="${STAGE_STATE_DIR:-${HOME}/.kube/kodama-stage/state}"
CLOUD_DIR="${STAGE_DIR}/${CLOUD}"
STACK_DIR="${STAGE_DIR}/shared/stack"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-stage-deploy.XXXXXX")"
chmod 0700 "${WORK_DIR}"
trap 'rm -rf "${WORK_DIR}"' EXIT

log_info "checking OpenTofu formatting and configuration for ${CLOUD} and shared/stack"
tofu -chdir="${CLOUD_DIR}" fmt -check
tofu -chdir="${STACK_DIR}" fmt -check

if [[ "${VALIDATE_ONLY}" -eq 1 ]]; then
  CONTRACT_FILE="${CONTRACT_OUT:-${WORK_DIR}/stage_contract.${CLOUD}.json}"
  python3 - "${CLOUD}" "${CONTRACT_FILE}" <<'PY'
import json
import sys

cloud, out_path = sys.argv[1], sys.argv[2]
regions = {"aws": "eu-west-1", "gcp": "europe-west1", "scw": "fr-par"}
region = regions[cloud]
kms_resources = {
    "aws": {
        "v1": f"arn:aws:kms:{region}:123456789012:key/11111111-1111-4111-8111-111111111111",
        "v2": f"arn:aws:kms:{region}:123456789012:key/22222222-2222-4222-8222-222222222222",
    },
    "gcp": {
        "v1": f"projects/example-stage/locations/{region}/keyRings/kodama/cryptoKeys/kek-v1",
        "v2": f"projects/example-stage/locations/{region}/keyRings/kodama/cryptoKeys/kek-v2",
    },
    "scw": {
        "v1": "fr-par/11111111-1111-4111-8111-111111111111",
        "v2": "fr-par/22222222-2222-4222-8222-222222222222",
    },
}[cloud]
s3_endpoints = {
    "aws": f"https://s3.{region}.amazonaws.com",
    "gcp": "https://storage.googleapis.com",
    "scw": f"https://s3.{region}.scw.cloud",
}
sa_annotations = {
    "aws": {"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/kodama-stage-aws-server"},
    "gcp": {"iam.gke.io/gcp-service-account": "kodama-stage-gcp-srv@example-stage.iam.gserviceaccount.com"},
    "scw": {},
}[cloud]

contract = {
    "cloud": cloud,
    "installation_id": f"kodama-stage-{cloud}",
    "region": region,
    "environment_tier": "staging",
    "cluster": {
        "name": f"kodama-stage-{cloud}",
        "endpoint": f"https://k8s.{cloud}.stage.optimi.dev",
        "ca_certificate": "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==",
        "oidc_issuer": f"https://oidc.{cloud}.stage.optimi.dev",
    },
    "postgres": {
        "host": f"pg.{cloud}.stage.internal",
        "port": 5432,
        "database": "kodama",
        "migration_role": "kodama_migration",
        "runtime_role": "kodama_runtime",
        "ssl_mode": "require",
        "pitr_enabled": cloud in ("aws", "gcp"),
        "backup_retention_days": 7,
        "rpo_minutes_estimate": 5 if cloud in ("aws", "gcp") else 1440,
        "qualifies_o11_pitr": False,
    },
    "kms": {
        "provider": cloud,
        "kek_id": f"kodama-stage-{cloud}",
        "current": "v1",
        "resources": kms_resources,
        "aws_region": region if cloud == "aws" else "",
        "scw_credentials_secret": "kodama-kms-scw" if cloud == "scw" else "",
    },
    "object_storage": {
        "endpoint": s3_endpoints[cloud],
        "region": region,
        "artifacts_bucket": f"kodama-stage-{cloud}-artifacts",
        "artifacts_prefix": "kodama/",
        "logs_bucket": f"kodama-stage-{cloud}-loki-logs",
        "logs_prefix": "loki/",
        "path_style": False,
    },
    "identities": {
        "kodama_server_sa_annotations": sa_annotations,
        "loki_sa_annotations": {},
    },
    "network": {
        "runtime_url": f"https://kodama.stage.{cloud}.optimi.dev",
        "ingress_host": f"kodama.stage.{cloud}.optimi.dev",
        "runtime_egress_cidrs": ["10.42.0.0/16"],
        "dependency_egress_rules": [
            {"name": "git-https", "cidr": "198.51.100.0/24", "ports": [443]},
            {"name": "object-storage", "cidr": "203.0.113.0/24", "ports": [443]},
            {"name": "oci-registry", "cidr": "192.0.2.0/24", "ports": [443]},
        ],
    },
    "tenants": [
        {"namespace": "kodama-tenant-stage", "tenant_id": "t_stage"},
    ],
}
with open(out_path, "w", encoding="utf-8") as fh:
    json.dump(contract, fh, indent=2)
    fh.write("\n")
PY

  VALUES_FILE="${VALUES_OUT:-${WORK_DIR}/kodama-values.${CLOUD}.yaml}"
  STACK_TFVARS="${WORK_DIR}/stack.tfvars.json"
  render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}" "${STACK_TFVARS}"
  helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${VALUES_FILE}" >/dev/null
  helm template kodama "${REPO_ROOT}/deploy/helm/kodama" --namespace kodama-system -f "${VALUES_FILE}" >/dev/null
  tofu -chdir="${STACK_DIR}" init -input=false -backend=false >/dev/null
  tofu -chdir="${STACK_DIR}" validate
  log_info "validate-only succeeded for cloud=${CLOUD}"
  exit 0
fi

mkdir -p "${STATE_DIR}"
chmod 0700 "${STATE_DIR}"
tofu -chdir="${CLOUD_DIR}" init -input=false -backend-config="path=${STATE_DIR}/${CLOUD}.tfstate"
tofu -chdir="${CLOUD_DIR}" validate
tofu -chdir="${CLOUD_DIR}" apply -input=false -auto-approve

CONTRACT_FILE="${CONTRACT_OUT:-${WORK_DIR}/stage_contract.json}"
SECRETS_FILE="${WORK_DIR}/stage_bootstrap_secrets.json"
tofu -chdir="${CLOUD_DIR}" output -json stage_contract >"${CONTRACT_FILE}"
tofu -chdir="${CLOUD_DIR}" output -json stage_bootstrap_secrets >"${SECRETS_FILE}"
chmod 0600 "${SECRETS_FILE}"

VALUES_FILE="${VALUES_OUT:-${WORK_DIR}/kodama-values.yaml}"
STACK_TFVARS="${WORK_DIR}/stack.tfvars.json"
render_kodama_values_from_contract "${CONTRACT_FILE}" "${VALUES_FILE}" "${STACK_TFVARS}"
verify_values_provenance "${VALUES_FILE}"
helm lint --strict "${REPO_ROOT}/deploy/helm/kodama" -f "${VALUES_FILE}" >/dev/null

# Cloud secrets reach the stack as a sensitive variable file (0600, removed with the work directory).
SECRETS_TFVARS="${WORK_DIR}/stack.secrets.tfvars.json"
python3 - "${SECRETS_FILE}" "${SECRETS_TFVARS}" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    secrets = json.load(fh)
with open(sys.argv[2], "w", encoding="utf-8") as fh:
    json.dump({"bootstrap_secrets": secrets}, fh)
PY
chmod 0600 "${SECRETS_TFVARS}"

tofu -chdir="${STACK_DIR}" init -input=false -backend-config="path=${STATE_DIR}/${CLOUD}-stack.tfstate"
tofu -chdir="${STACK_DIR}" validate

# Adopt a controller installed before the stack managed it (same release, same chart), once.
if ! tofu -chdir="${STACK_DIR}" state list 2>/dev/null | grep -qx 'helm_release.ingress_nginx' &&
  helm --kubeconfig "${KUBECONFIG:-${HOME}/.kube/config}" status ingress-nginx -n ingress-nginx >/dev/null 2>&1; then
  log_info "importing the existing ingress-nginx release into the stack state"
  tofu -chdir="${STACK_DIR}" import -input=false -var-file="${STACK_TFVARS}" -var-file="${SECRETS_TFVARS}" helm_release.ingress_nginx ingress-nginx/ingress-nginx
fi
tofu -chdir="${STACK_DIR}" apply -input=false -auto-approve \
  -var-file="${STACK_TFVARS}" -var-file="${SECRETS_TFVARS}"

APPLIED_VALUES="${WORK_DIR}/applied-kodama-values.yaml"
tofu -chdir="${STACK_DIR}" output -raw rendered_kodama_values >"${APPLIED_VALUES}"
cmp -s "${VALUES_FILE}" "${APPLIED_VALUES}" || die "applied stack Kodama values differ from validated values file"

if [[ -n "${VALUES_OUT}" ]]; then
  cp "${APPLIED_VALUES}" "${VALUES_OUT}"
fi

log_info "deployment complete for cloud=${CLOUD}"
