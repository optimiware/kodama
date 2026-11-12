#!/usr/bin/env bash
# Google Cloud Platform (gcp) backend, kubeconfig, and KMS helpers for Kodama deployments.

set -euo pipefail

gcp_resolve_project_id() {
  local project_id="${KODAMA_GCP_PROJECT_ID:-${GOOGLE_PROJECT:-${CLOUDSDK_CORE_PROJECT:-${TF_VAR_project_id:-}}}}"
  [[ -n "$project_id" ]] || die "GCP project ID is required (set GOOGLE_PROJECT, KODAMA_GCP_PROJECT_ID, or --project)"
  printf '%s\n' "$project_id"
}

gcp_state_prefix_for_cluster() {
  local project_id="$1"
  local region="$2"
  local cluster="$3"
  printf 'kodama/gcp/%s/%s/%s\n' "$project_id" "$region" "$cluster"
}

gcp_write_backend_config() {
  local output_file="$1"
  local bucket="$2"
  local project_id="$3"
  local region="$4"
  local cluster="$5"
  local prefix
  prefix="$(gcp_state_prefix_for_cluster "$project_id" "$region" "$cluster")"

  (
    umask 077
    cat >"$output_file" <<EOF
bucket = "${bucket}"
prefix = "${prefix}"
EOF
  )
}

gcp_write_kubeconfig() {
  local descriptor_file="$1"
  local kubeconfig_path="$2"

  require_cmd gcloud jq
  local project_id cluster_name zone region is_ha location
  project_id="$(jq -r '.account_or_project_id' "$descriptor_file")"
  cluster_name="$(jq -r '.kubernetes.cluster_name' "$descriptor_file")"
  zone="$(jq -r '.zone' "$descriptor_file")"
  region="$(jq -r '.region' "$descriptor_file")"
  is_ha="$(jq -r '.cost_profile == "ha"' "$descriptor_file")"
  if [[ "$is_ha" == "true" ]]; then
    location="$region"
  else
    location="$zone"
  fi

  (
    umask 077
    KUBECONFIG="$kubeconfig_path" gcloud container clusters get-credentials "$cluster_name" \
      --project="$project_id" \
      --location="$location"
  )
}

gcp_kms_encrypt_blob() {
  local crypto_key_id="$1"
  local plaintext_file="$2"
  local ciphertext_file="$3"

  require_cmd gcloud base64
  gcloud kms encrypt \
    --key="$crypto_key_id" \
    --plaintext-file="$plaintext_file" \
    --ciphertext-file="$ciphertext_file"
  base64 <"$ciphertext_file" | tr -d '\n'
}
