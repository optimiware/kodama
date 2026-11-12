#!/usr/bin/env bash
# AWS backend, kubeconfig, and KMS helpers for Kodama deployments.

set -euo pipefail

aws_resolve_account_id() {
  local account_id="${KODAMA_AWS_ACCOUNT_ID:-${TF_VAR_account_id:-}}"
  if [[ -z "$account_id" ]] && command -v aws >/dev/null 2>&1; then
    account_id="$(aws sts get-caller-identity --query Account --output text 2>/dev/null || true)"
  fi
  [[ -n "$account_id" ]] || die "AWS account ID is required (set KODAMA_AWS_ACCOUNT_ID, TF_VAR_account_id, or --account)"
  printf '%s\n' "$account_id"
}

aws_state_key_for_cluster() {
  local account_id="$1"
  local cluster_region="$2"
  local cluster="$3"
  printf 'kodama/aws/%s/%s/%s/infrastructure.tfstate\n' "$account_id" "$cluster_region" "$cluster"
}

aws_write_backend_config() {
  local output_file="$1"
  local bucket="$2"
  local account_id="$3"
  local cluster_region="$4"
  local cluster="$5"
  local bucket_region="${6:-$cluster_region}"
  local state_key
  state_key="$(aws_state_key_for_cluster "$account_id" "$cluster_region" "$cluster")"

  (
    umask 077
    cat >"$output_file" <<EOF
bucket       = "${bucket}"
key          = "${state_key}"
region       = "${bucket_region}"
encrypt      = true
use_lockfile = true
EOF
  )
}

aws_write_kubeconfig() {
  local descriptor_file="$1"
  local kubeconfig_path="$2"

  require_cmd aws jq
  local region cluster_name
  region="$(jq -r '.region' "$descriptor_file")"
  cluster_name="$(jq -r '.kubernetes.cluster_name' "$descriptor_file")"

  (
    umask 077
    aws eks update-kubeconfig \
      --region "$region" \
      --name "$cluster_name" \
      --kubeconfig "$kubeconfig_path"
  )
}

aws_kms_encrypt_blob() {
  local key_id="$1"
  local region="$2"
  local plaintext_file="$3"

  require_cmd aws
  aws kms encrypt \
    --region "$region" \
    --key-id "$key_id" \
    --plaintext "fileb://${plaintext_file}" \
    --query CiphertextBlob \
    --output text | tr -d '\n'
}
