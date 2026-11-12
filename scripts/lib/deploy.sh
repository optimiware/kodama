#!/usr/bin/env bash
# Shared multi-cloud OpenTofu + Helm deployment helpers for Kodama.

set -euo pipefail

LC_ALL=C
export LC_ALL

KODAMA_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly KODAMA_REPO_ROOT

log_info() {
  printf '[kodama-deploy] %s\n' "$*" >&2
}

log_error() {
  printf '[kodama-deploy] ERROR: %s\n' "$*" >&2
}

die() {
  log_error "$*"
  exit 1
}

require_cmd() {
  local cmd
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "required command not found in PATH: ${cmd}"
  done
}

validate_cloud() {
  local cloud="${1:-}"
  case "$cloud" in
    scw|gcp|aws) ;;
    *)
      die "invalid cloud target '${cloud}' (expected one of: scw, gcp, aws)"
      ;;
  esac
}

validate_cluster_name() {
  local cluster="${1:-}"
  if [[ ! "$cluster" =~ ^[a-z][a-z0-9-]{0,16}[a-z0-9]$ ]]; then
    die "invalid cluster name '${cluster}': must be 2-18 lowercase alphanumeric characters or hyphens, starting with a letter and ending with an alphanumeric character"
  fi
}

validate_state_bucket() {
  local bucket="${1:-}"
  if [[ ! "$bucket" =~ ^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$ || "$bucket" == *..* ]]; then
    die "invalid state bucket name '${bucket}': must be 3-63 lowercase alphanumeric, '.', '_' or '-' characters without '..'"
  fi
}

validate_state_region() {
  local region="${1:-}"
  if [[ ! "$region" =~ ^[a-z][a-z0-9-]{1,62}[a-z0-9]$ ]]; then
    die "invalid cloud region '${region}': must be lowercase alphanumeric characters or hyphens, starting with a letter"
  fi
}

validate_state_project_or_account() {
  local id="${1:-}"
  if [[ ! "$id" =~ ^[A-Za-z0-9]([A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$ ]]; then
    die "invalid project or account ID '${id}': must be 1-64 alphanumeric, '_' or '-' characters"
  fi
}

validate_image_digest() {
  local digest="${1:-}"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    die "invalid KODAMA_IMAGE_DIGEST '${digest}': want sha256:<64 lowercase hex>"
  fi
}

default_region_for_cloud() {
  local cloud="$1"
  case "$cloud" in
    scw) printf 'fr-par\n' ;;
    gcp) printf 'europe-west1\n' ;;
    aws) printf 'eu-west-1\n' ;;
    *) die "unknown cloud: ${cloud}" ;;
  esac
}

key_adapter_for_cloud() {
  local cloud="$1"
  case "$cloud" in
    scw) printf 'scw-kms\n' ;;
    gcp) printf 'gcp-kms\n' ;;
    aws) printf 'aws-kms\n' ;;
    *) die "unknown cloud: ${cloud}" ;;
  esac
}

deploy_state_root() {
  local default_state_home="${XDG_STATE_HOME:-${HOME}/.local/state}"
  printf '%s\n' "${KODAMA_DEPLOY_STATE_DIR:-${default_state_home}/kodama/deploy}"
}

cluster_work_dir() {
  local cloud="$1"
  local cluster="$2"
  printf '%s/%s/%s\n' "$(deploy_state_root)" "$cloud" "$cluster"
}

cloud_work_dir() {
  local cloud="$1"
  printf '%s/%s\n' "$(deploy_state_root)" "$cloud"
}

cloud_iac_dir() {
  local cloud="$1"
  printf '%s/deploy/%s\n' "$KODAMA_REPO_ROOT" "$cloud"
}

cloud_bootstrap_dir() {
  local cloud="$1"
  printf '%s/deploy/%s/bootstrap\n' "$KODAMA_REPO_ROOT" "$cloud"
}

ensure_cluster_work_dir() {
  local cloud="$1"
  local cluster="$2"
  local dir
  dir="$(cluster_work_dir "$cloud" "$cluster")"
  (
    umask 077
    mkdir -p "${dir}/tfdata" "${dir}/pki"
  )
  printf '%s\n' "$dir"
}

resolve_tfvars_file() {
  local cloud="$1"
  local cluster="$2"
  local explicit="${3:-}"
  if [[ -n "$explicit" ]]; then
    [[ -f "$explicit" ]] || die "tfvars file not found: ${explicit}"
    printf '%s\n' "$explicit"
    return 0
  fi

  local iac_dir
  iac_dir="$(cloud_iac_dir "$cloud")"
  local candidate
  for candidate in \
    "${iac_dir}/clusters/${cluster}.tfvars.json" \
    "${iac_dir}/clusters/${cluster}.tfvars"; do
    if [[ -f "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  printf '\n'
}

verify_tfvars_cluster_identity() {
  local var_file="$1"
  local expected_cluster="$2"
  if [[ -z "$var_file" || ! -f "$var_file" ]]; then
    return 0
  fi
  if [[ "$var_file" == *.json ]]; then
    require_cmd jq
    local file_cluster
    file_cluster="$(jq -r '.cluster_name // empty' "$var_file")"
    if [[ -n "$file_cluster" && "$file_cluster" != "$expected_cluster" ]]; then
      die "cluster_name mismatch in ${var_file}: file declares '${file_cluster}', but --cluster is '${expected_cluster}'"
    fi
  fi
}

state_backend_env_file() {
  local cloud="$1"
  local project_or_account="${2:-}"
  local region="${3:-}"
  local work_dir
  work_dir="$(cloud_work_dir "$cloud")"
  if [[ -n "$project_or_account" && -n "$region" ]]; then
    local safe_id="${project_or_account//[^a-zA-Z0-9_-]/_}"
    local safe_region="${region//[^a-zA-Z0-9_-]/_}"
    local scoped="${work_dir}/state-backend-${safe_id}-${safe_region}.env"
    if [[ -f "$scoped" ]]; then
      printf '%s\n' "$scoped"
      return 0
    fi
  fi
  printf '%s/state-backend.env\n' "$work_dir"
}

load_state_backend_env() {
  local cloud="$1"
  local project_or_account="${2:-}"
  local region="${3:-}"
  local env_file
  env_file="$(state_backend_env_file "$cloud" "$project_or_account" "$region")"
  [[ -f "$env_file" ]] || return 0

  local raw_line line key value
  while IFS= read -r raw_line || [[ -n "$raw_line" ]]; do
    line="${raw_line%$'\r'}"
    if [[ "$line" =~ ^[[:space:]]*$ || "$line" =~ ^[[:space:]]*# ]]; then
      continue
    fi
    if [[ "$line" != *=* ]]; then
      die "malformed line in state backend env file ${env_file}: expected KEY=VALUE"
    fi
    key="${line%%=*}"
    value="${line#*=}"
    if [[ ! "$key" =~ ^KODAMA_STATE_(CLOUD|BUCKET|REGION|PROJECT_OR_ACCOUNT)$ ]]; then
      die "disallowed key '${key}' in state backend env file ${env_file}"
    fi
    if [[ "${#value}" -ge 2 && "$value" == \"*\" && "$value" == *\" ]]; then
      value="${value:1:${#value}-2}"
    elif [[ "${#value}" -ge 2 && "$value" == \'*\' && "$value" == *\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    case "$key" in
      KODAMA_STATE_CLOUD)
        validate_cloud "$value"
        [[ "$value" == "$cloud" ]] || die "KODAMA_STATE_CLOUD '${value}' in ${env_file} does not match target cloud '${cloud}'"
        KODAMA_STATE_CLOUD="$value"
        ;;
      KODAMA_STATE_BUCKET)
        validate_state_bucket "$value"
        KODAMA_STATE_BUCKET="$value"
        ;;
      KODAMA_STATE_REGION)
        validate_state_region "$value"
        KODAMA_STATE_REGION="$value"
        ;;
      KODAMA_STATE_PROJECT_OR_ACCOUNT)
        validate_state_project_or_account "$value"
        KODAMA_STATE_PROJECT_OR_ACCOUNT="$value"
        ;;
    esac
  done <"$env_file"
}

verify_descriptor_identity() {
  local descriptor_file="$1"
  local expected_cloud="$2"
  local expected_cluster="$3"

  [[ -f "$descriptor_file" ]] || die "deployment descriptor not found at ${descriptor_file} (run 'scripts/deploy ${expected_cloud} output --cluster ${expected_cluster}' first)"
  require_cmd jq

  local actual_cloud actual_cluster
  actual_cloud="$(jq -r '.cloud // empty' "$descriptor_file")"
  actual_cluster="$(jq -r '.cluster_name // empty' "$descriptor_file")"

  if [[ "$actual_cloud" != "$expected_cloud" ]]; then
    die "descriptor cloud mismatch in ${descriptor_file}: got '${actual_cloud}', expected '${expected_cloud}'"
  fi
  if [[ "$actual_cluster" != "$expected_cluster" ]]; then
    die "descriptor cluster_name mismatch in ${descriptor_file}: got '${actual_cluster}', expected '${expected_cluster}'"
  fi
}

render_helm_values_from_descriptor() {
  local descriptor_file="$1"
  local output_values_file="$2"
  local image_repo="${KODAMA_IMAGE_REPOSITORY:-registry.example.com/kodama}"
  local image_tag="${KODAMA_IMAGE_TAG:-0.1.0}"
  local image_digest="${KODAMA_IMAGE_DIGEST:-}"
  local oidc_issuer="${KODAMA_OIDC_ISSUER:-}"
  local oidc_audience="${KODAMA_OIDC_AUDIENCE:-}"
  local oidc_jwks_url="${KODAMA_OIDC_JWKS_URL:-}"
  local oidc_tenant_claim="${KODAMA_OIDC_TENANT_CLAIM:-}"
  local oidc_subject_claim="${KODAMA_OIDC_SUBJECT_CLAIM:-}"
  local oidc_project_claim="${KODAMA_OIDC_PROJECT_CLAIM:-}"
  local oidc_ca_file="${KODAMA_OIDC_CA_FILE:-}"
  # One stage-wide value for the broker callout account. The chart's values.yaml
  # also anchors this name; scripts/bootstrap-secrets writes the same value into
  # the kodama-nats-auth Secret. The rendered-chart and bootstrap-contract tests
  # fail if any of the three drift.
  local nats_auth_secret="kodama-nats-auth"
  local nats_auth_account="KODAMA"

  if [[ -n "$image_digest" ]]; then
    validate_image_digest "$image_digest"
  fi

  require_cmd jq

  (
    umask 077
    jq \
      --arg image_repo "$image_repo" \
      --arg image_tag "$image_tag" \
      --arg image_digest "$image_digest" \
      --arg nats_auth_secret "$nats_auth_secret" \
      --arg nats_auth_account "$nats_auth_account" \
      --arg oidc_issuer "$oidc_issuer" \
      --arg oidc_audience "$oidc_audience" \
      --arg oidc_jwks_url "$oidc_jwks_url" \
      --arg oidc_tenant_claim "$oidc_tenant_claim" \
      --arg oidc_subject_claim "$oidc_subject_claim" \
      --arg oidc_project_claim "$oidc_project_claim" \
      --arg oidc_ca_file "$oidc_ca_file" \
      '(
        {}
        | if $oidc_issuer != "" then . + {KODAMA_OIDC_ISSUER: $oidc_issuer} else . end
        | if $oidc_audience != "" then . + {KODAMA_OIDC_AUDIENCE: $oidc_audience} else . end
        | if $oidc_jwks_url != "" then . + {KODAMA_OIDC_JWKS_URL: $oidc_jwks_url} else . end
        | if $oidc_tenant_claim != "" then . + {KODAMA_OIDC_TENANT_CLAIM: $oidc_tenant_claim} else . end
        | if $oidc_subject_claim != "" then . + {KODAMA_OIDC_SUBJECT_CLAIM: $oidc_subject_claim} else . end
        | if $oidc_project_claim != "" then . + {KODAMA_OIDC_PROJECT_CLAIM: $oidc_project_claim} else . end
        | if $oidc_ca_file != "" then . + {KODAMA_OIDC_CA_FILE: $oidc_ca_file} else . end
      ) as $extra_env |
      {
        nats: {
          enabled: true,
          replicaCount: 1,
          port: 4222,
          tls: {
            existingSecret: "kodama-broker-tls"
          },
          auth: {
            mode: "callout",
            callout: {
              existingSecret: $nats_auth_secret,
              account: $nats_auth_account
            }
          },
          jetstream: {
            maxMemory: "256M",
            maxFile: "8G",
            storageClass: .kubernetes.storage_class,
            size: "10Gi"
          }
        },
        kodama: {
          environment: .environment,
          replicaCount: 1,
          image: {
            repository: $image_repo,
            tag: $image_tag,
            digest: $image_digest,
            pullPolicy: "IfNotPresent"
          },
          config: ({
            installationID: .installation_id,
            publicURL: ("https://kodama." + .release_namespace + ".svc.cluster.local:8443"),
            databaseURL: "postgres://kodama_runtime@database:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt",
            natsURL: ("tls://nats." + .release_namespace + ".svc.cluster.local:4222"),
            keyAdapter: .kms.key_adapter,
            keyReference: .kms.key_ref,
            keyCurrentVersion: 1,
            mcpPath: "/mcp",
            workspacePVC: {
              storageClass: .kubernetes.storage_class,
              size: "10Gi",
              accessMode: "ReadWriteOnce"
            },
            workloadCA: {
              secretName: "kodama-workload-ca"
            }
          } + (if ($extra_env | length) > 0 then {extraEnv: $extra_env} else {} end)),
          # Only Scaleway has a static KMS credential: scripts/bootstrap-secrets
          # creates kodama-kms-credentials for scw alone. AWS (IRSA) and GCP
          # (Workload Identity) authenticate through the ServiceAccount
          # annotation rendered below, and the chart references no credential
          # Secret for their adapters.
          secrets: ({
            database: { existingSecret: "kodama-database" },
            broker:   { existingSecret: "kodama-broker" },
            keys:     { existingSecret: "kodama-keys" },
            tls:      { existingSecret: "kodama-tls" },
            natsAuth: {
              existingSecret: $nats_auth_secret,
              account: $nats_auth_account
            }
          } + (
            if .cloud == "scw" then
              { kmsCredentials: { existingSecret: "kodama-kms-credentials" } }
            else
              {}
            end
          )),
          tenants: .tenants,
          networkPolicy: {
            defaultDeny: true,
            permitKodama: true,
            permitGit: true
          },
          securityContext: {
            runAsNonRoot: true,
            runAsUser: 65532
          },
          service: {
            type: "ClusterIP",
            port: 8443,
            targetPort: 8443
          },
          serviceAccount: {
            create: true,
            name: "kodama",
            annotations: (
              if .cloud == "gcp" and (.kubernetes.workload_identity_ref // "") != "" then
                { "iam.gke.io/gcp-service-account": .kubernetes.workload_identity_ref }
              elif .cloud == "aws" and (.kubernetes.workload_identity_ref // "") != "" then
                { "eks.amazonaws.com/role-arn": .kubernetes.workload_identity_ref }
              else
                {}
              end
            )
          },
          rbac: {
            create: true
          },
          persistence: {
            logs: {
              enabled: true,
              storageClass: .kubernetes.storage_class,
              size: "10Gi",
              accessMode: "ReadWriteOnce",
              mountPath: "/var/lib/kodama"
            }
          },
          probes: {
            mode: "httpGet",
            livenessPath: "/healthz",
            readinessPath: "/readyz",
            timeoutSeconds: 10
          },
          resources: {
            limits: {
              cpu: "1",
              memory: "1Gi"
            },
            requests: {
              cpu: "250m",
              memory: "256Mi"
            }
          },
          jobs: {
            restartPolicy: "Never",
            backoffLimit: 0
          }
        }
      }' "$descriptor_file" >"$output_values_file"
  )
}
