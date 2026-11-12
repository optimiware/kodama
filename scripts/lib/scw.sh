#!/usr/bin/env bash
# Scaleway (scw) backend, kubeconfig, and KMS helpers for Kodama deployments.

set -euo pipefail

scw_resolve_project_id() {
  local project_id="${KODAMA_SCW_PROJECT_ID:-${SCW_DEFAULT_PROJECT_ID:-${TF_VAR_project_id:-}}}"
  [[ -n "$project_id" ]] || die "Scaleway project ID is required (set SCW_DEFAULT_PROJECT_ID, KODAMA_SCW_PROJECT_ID, or --project)"
  printf '%s\n' "$project_id"
}

scw_state_key_for_cluster() {
  local project_id="$1"
  local cluster_region="$2"
  local cluster="$3"
  printf 'kodama/scw/%s/%s/%s/infrastructure.tfstate\n' "$project_id" "$cluster_region" "$cluster"
}

scw_write_backend_config() {
  local output_file="$1"
  local bucket="$2"
  local project_id="$3"
  local cluster_region="$4"
  local cluster="$5"
  local bucket_region="${6:-$cluster_region}"
  local state_key
  state_key="$(scw_state_key_for_cluster "$project_id" "$cluster_region" "$cluster")"

  (
    umask 077
    cat >"$output_file" <<EOF
bucket                      = "${bucket}"
key                         = "${state_key}"
region                      = "${bucket_region}"
endpoints = {
  s3 = "https://s3.${bucket_region}.scw.cloud"
}
skip_credentials_validation = true
skip_region_validation      = true
skip_requesting_account_id  = true
use_path_style              = true
use_lockfile                = true
EOF
  )
}

scw_prepare_s3_credentials() {
  if [[ -z "${AWS_ACCESS_KEY_ID:-}" && -n "${SCW_ACCESS_KEY:-}" ]]; then
    export AWS_ACCESS_KEY_ID="$SCW_ACCESS_KEY"
  fi
  if [[ -z "${AWS_SECRET_ACCESS_KEY:-}" && -n "${SCW_SECRET_KEY:-}" ]]; then
    export AWS_SECRET_ACCESS_KEY="$SCW_SECRET_KEY"
  fi
}

scw_write_kubeconfig() {
  local iac_dir="$1"
  local tf_data_dir="$2"
  local kubeconfig_path="$3"

  require_cmd tofu
  (
    umask 077
    TF_DATA_DIR="$tf_data_dir" tofu -chdir="$iac_dir" output -raw kubeconfig_raw >"$kubeconfig_path"
  )
}

scw_read_kms_credentials_json() {
  local iac_dir="$1"
  local tf_data_dir="$2"

  require_cmd tofu
  TF_DATA_DIR="$tf_data_dir" tofu -chdir="$iac_dir" output -json kms_credentials 2>/dev/null || printf '{}\n'
}

scw_kms_encrypt_blob() {
  local key_id="$1"
  local region="$2"
  local plaintext_b64="$3"
  local raw_key_id="${key_id#"${region}/"}"

  require_cmd scw jq

  # The KEK must never appear on a command line. The Scaleway CLI reads the
  # value from a 0600 file through the documented plaintext=@<file> form; the
  # file lives in a private 0700 directory and the EXIT/INT/TERM trap removes
  # it on every path, including a failing key manager call.
  (
    set -euo pipefail
    local tmp_dir plaintext_file
    tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/kodama-scw-kek.XXXXXX")"
    chmod 700 "$tmp_dir"
    cleanup_scw_kek() {
      if [[ -n "${tmp_dir:-}" ]]; then
        rm -rf -- "${tmp_dir}"
      fi
    }
    trap cleanup_scw_kek EXIT
    trap 'cleanup_scw_kek; exit 130' INT
    trap 'cleanup_scw_kek; exit 143' TERM
    plaintext_file="${tmp_dir}/plaintext"
    (
      umask 077
      printf '%s' "$plaintext_b64" >"$plaintext_file"
    )
    chmod 600 "$plaintext_file"
    scw keymanager key encrypt "$raw_key_id" \
      region="$region" \
      plaintext="@${plaintext_file}" \
      -o json | jq -r '.ciphertext'
  )
}
