#!/usr/bin/env bash
# run-stage.sh: run the e2e acceptance suite against the deployed stage.
#
# Invoke this script only through the host-wide stage lock:
#
#   scripts/infra-slot.sh --exclusive scw-stage stack -- \
#     test/e2e/internal/harness/run-stage.sh go test -race -tags=e2e \
#     -count=1 -timeout=20m ./test/e2e/...
#
# The script opens kubectl port-forwards to svc/kodama and svc/oauthsonas,
# acquires all four canonical persona tokens from scripts/oauthsonas-token into
# private 0600 files, hands only those paths to the common run-installation.sh
# runner, and runs the supplied command. It never enables shell tracing, never
# dumps the environment, headers, bodies, Secrets or raw subprocess diagnostics,
# and joins and kills both port-forwards on exit. The picker access code is read
# from the oauthsonas-access Secret into a 0600 file, kept alive while the
# runner executes, and removed on exit so just-in-time acquisition stays
# possible.
#
# Required inputs:
#   Q01_KUBE_CONTEXT      the kubeconfig context
#   Q01_CA_PEM            the installation CA bundle (path)
#   Q01_PROJECT_ID        the project the acceptance pipelines run in
#
# Optional inputs:
#   Q01_API_URL           default https://127.0.0.1:${Q01_API_PORT}
#   Q01_API_PORT          default 18443
#   Q01_OAUTHSONAS_PORT   default the port in Q01_OAUTHSONAS_URL (8443)
#   Q01_OAUTHSONAS_HOST   default oauthsonas.kodama-system.svc.cluster.local
#   Q01_OAUTHSONAS_SERVICE default oauthsonas
#   Q01_KODAMA_SERVICE    default kodama
#   Q01_RELEASE_NAMESPACE default kodama-system
#   Q01_EVIDENCE_DIR      default $TMPDIR/oc/q01-evidence/<UTC timestamp>
#   Q01_READY_ATTEMPTS    readiness poll attempts (default 120)
#   Q01_READY_INTERVAL    seconds between readiness polls (default 0.25)
#   Q01_CLEANUP_ATTEMPTS  cleanup wait attempts before KILL (default 50)
#   Q01_CLEANUP_INTERVAL  seconds between cleanup waits (default 0.1)
#   Q01_SMOKE_GIT         0 selects the non-Git scenarios (default 0); 1 runs
#                         ProtectedTask and HumanPush too
#   Q01_IMAGE, Q01_EXTERNAL_HARNESS_IMAGE, Q01_HARNESS_REVISION,
#   Q01_DEPLOYED_REVISION pass through.
#   Q01_NATS_URL            the broker endpoint the live subscription probes use.
#                           It passes through unchanged and is never invented.
#   Q01_NATS_CA_FILE        the broker CA PEM bundle. When Q01_NATS_URL is
#                           tls:// it defaults to $Q01_CA_PEM.
#   Q01_NATS_TOKEN_FILE     optional override: a private 0600 file holding a
#                           store-backed Kodama service credential. The live
#                           probes mint their own run-unique service credential,
#                           because a human OIDC token has no store-backed
#                           expiry and the stage callout refuses it by design
#                           (AC07). A file supplied here only wins over the
#                           minted credential, so it must be a service token.
#   Q01_NATS_TLS_SERVER_NAME
#                           optional verified broker TLS name; empty uses the
#                           URL host.
#   Q01_NATS_CLIENT_CERT_FILE / Q01_NATS_CLIENT_KEY_FILE
#                           the client certificate pair the stage broker's mTLS
#                           handshake requires (extracted from Secret
#                           kodama-broker: tls.crt/tls.key). Supplied together
#                           or not at all.
#
#   Stage live-subscription procedure. The stage broker is cluster-internal
#   (tls://nats.kodama-system.svc.cluster.local:4222) with TLS and a
#   per-credential authorization callout, so open a forward outside this script
#   (it forwards only svc/kodama, svc/oauthsonas and, with Q01_SMOKE_GIT=1, the
#   Git service):
#
#     kubectl --context "$Q01_KUBE_CONTEXT" --namespace kodama-system \
#       port-forward svc/nats 14222:4222
#
#   Then run the suite with the broker URL reachable through that forward:
#
#     Q01_NATS_URL=tls://127.0.0.1:14222 \
#     Q01_NATS_TLS_SERVER_NAME=nats.kodama-system.svc.cluster.local \
#     Q01_NATS_CA_FILE="$Q01_CA_PEM" \
#     scripts/infra-slot.sh --exclusive scw-stage stack -- \
#       test/e2e/internal/harness/run-stage.sh go test -race -tags=e2e \
#       -count=1 -timeout=20m ./test/e2e/...
#
#   The admin token this script acquires is never presented to the broker: the
#   live probes mint a service credential and use it for both the broker
#   connection and the served subscription grant. A plain local nats:// broker
#   still needs only Q01_NATS_URL.
#
# Optional stage installation descriptor (schema version 1):
#   Q01_STAGE_DESCRIPTOR          1 generates one in a private work file
#   Q01_INSTALLATION_FILE         an existing descriptor wins over generation
#   Q01_STAGE_INSTALLATION_ID     default kodama-stage
#   Q01_SERVER_IMAGE_DIGEST       required when generating (sha256:<64 hex>)
#   Q01_DEPLOYED_REVISION         required when generating
#   Q01_OAUTHSONAS_ISSUER         default $Q01_OAUTHSONAS_URL
#   Q01_OAUTHSONAS_AUDIENCE       required when generating
#   Q01_OAUTHSONAS_CA_FILE         default $Q01_CA_PEM
#   Q01_OAUTHSONAS_CLIENT_ID      default kodactl
#   Q01_OAUTHSONAS_REDIRECT_URI   default http://127.0.0.1:52790/callback
#   Q01_TENANT_ID                 default $Q01_PROJECT_ID
#   Q01_WORKLOAD_API_URL          default https://<tls name>:8443
#   Q01_WORKLOAD_CA_FILE          default $Q01_CA_PEM
#   Q01_SUBJECT_ADMIN/ENGINEER/REVIEWER/OPERATOR  default the persona name
#
# The staged descriptor carries no Git block: the stage run is the ten non-Git
# scenarios. Supplying an existing descriptor is how a Git-enabled run is
# described.
#
# The script performs no cloud, tofu, node or cluster-scoped change.
set -euo pipefail
set +x
# Never inherit a caller-exported token: only the final test command receives
# the token this script acquires.
unset Q01_TOKEN

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$here/../../../.." && pwd -P)"

die() {
  printf 'run-stage: %s\n' "$*" >&2
  exit 1
}

missing=()
for name in Q01_KUBE_CONTEXT Q01_CA_PEM Q01_PROJECT_ID; do
  if [[ -z "${!name:-}" ]]; then
    missing+=("$name")
  fi
done
if (( ${#missing[@]} > 0 )); then
  die "missing required environment input(s): ${missing[*]}"
fi

release_namespace="${Q01_RELEASE_NAMESPACE:-kodama-system}"
kodama_service="${Q01_KODAMA_SERVICE:-kodama}"
oauth_service="${Q01_OAUTHSONAS_SERVICE:-oauthsonas}"
oauth_host="${Q01_OAUTHSONAS_HOST:-oauthsonas.kodama-system.svc.cluster.local}"
api_port="${Q01_API_PORT:-18443}"
api_url="${Q01_API_URL:-https://127.0.0.1:${api_port}}"
default_tools_image="docker.io/library/buildpack-deps@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c"
default_harness_image="docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b"
# OAuthSonas keeps the issuer at https://<host>:8443 and scripts/oauthsonas-token
# maps that host to the port-forward with curl --resolve, so the local port must
# match the port in the issuer URL. Only one holder runs under the stage lock.
oauth_url="${Q01_OAUTHSONAS_URL:-https://${oauth_host}:8443}"
oauth_url_port=""
if [[ "$oauth_url" =~ ^https://([^/:]+):([0-9]+)$ ]]; then
  [[ "${BASH_REMATCH[1]}" == "$oauth_host" ]] || die "Q01_OAUTHSONAS_URL and Q01_OAUTHSONAS_HOST must name the same host"
  oauth_url_port="${BASH_REMATCH[2]}"
else
  die "Q01_OAUTHSONAS_URL must be https://host:port"
fi
oauth_port="${Q01_OAUTHSONAS_PORT:-$oauth_url_port}"
tls_server_name="${Q01_TLS_SERVER_NAME:-kodama.kodama-system.svc.cluster.local}"
smoke_git="${Q01_SMOKE_GIT:-0}"
tenant="${Q01_TENANT_ID:-$Q01_PROJECT_ID}"
git_service="${Q01_GIT_SERVICE:-kodama-git-${tenant}}"
git_host="${Q01_GIT_HOST:-${git_service}.${release_namespace}.svc.cluster.local}"
git_port="${Q01_GIT_PORT:-18445}"
git_url="${Q01_GIT_URL:-https://127.0.0.1:${git_port}}"
git_workload_url="${Q01_GIT_WORKLOAD_URL:-https://${git_host}:8443}"
git_service_id="${Q01_GIT_SERVICE_ID:-git}"
git_admin_binding_id="${Q01_GIT_ADMIN_BINDING_ID:-git-admin}"
git_admin_resource_id="${Q01_GIT_ADMIN_RESOURCE_ID:-git-admin}"
smoke_repository="${Q01_SMOKE_REPOSITORY:-}"
smoke_resource_id="${Q01_SMOKE_RESOURCE_ID:-}"
smoke_binding_id="${Q01_SMOKE_BINDING_ID:-$smoke_resource_id}"
token_script="${Q01_TOKEN_SCRIPT:-$repo_root/scripts/oauthsonas-token}"
access_secret="${Q01_ACCESS_CODE_SECRET:-oauthsonas-access}"
access_key="${Q01_ACCESS_CODE_KEY:-access_code}"
ready_attempts="${Q01_READY_ATTEMPTS:-120}"
ready_interval="${Q01_READY_INTERVAL:-0.25}"
cleanup_attempts="${Q01_CLEANUP_ATTEMPTS:-50}"
cleanup_interval="${Q01_CLEANUP_INTERVAL:-0.1}"

[[ "$Q01_KUBE_CONTEXT" != -* ]] || die "Q01_KUBE_CONTEXT must not look like a kubectl flag"
[[ "$Q01_PROJECT_ID" =~ ^[A-Za-z0-9_-]+$ ]] || die "Q01_PROJECT_ID is not a bounded identity"
[[ "$smoke_git" == "0" || "$smoke_git" == "1" ]] || die "Q01_SMOKE_GIT must be 0 or 1"
[[ "${Q01_STAGE_DESCRIPTOR:-0}" == "0" || "${Q01_STAGE_DESCRIPTOR:-0}" == "1" ]] || die "Q01_STAGE_DESCRIPTOR must be 0 or 1"
[[ "$release_namespace" == "kodama-system" ]] || die "Q01_RELEASE_NAMESPACE must be kodama-system"
[[ "$access_secret" =~ ^[a-z0-9.-]+$ ]] || die "Q01_ACCESS_CODE_SECRET is not a kubernetes Secret name"
# Secret data keys may contain underscore (the stage key is access_code);
# Secret names may not.
[[ "$access_key" =~ ^[a-z0-9._-]+$ ]] || die "Q01_ACCESS_CODE_KEY is not a Secret data key"
[[ "$kodama_service" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Q01_KODAMA_SERVICE is not a kubernetes service name"
[[ "$oauth_service" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Q01_OAUTHSONAS_SERVICE is not a kubernetes service name"
[[ "$api_port" =~ ^[0-9]+$ ]] && (( api_port >= 1 && api_port <= 65535 )) || die "Q01_API_PORT is not a port"
[[ "$oauth_port" =~ ^[0-9]+$ ]] && (( oauth_port >= 1 && oauth_port <= 65535 )) || die "Q01_OAUTHSONAS_PORT is not a port"
[[ "$oauth_port" == "$oauth_url_port" ]] || die "Q01_OAUTHSONAS_PORT must match the port in Q01_OAUTHSONAS_URL"
[[ "$api_port" != "$oauth_port" ]] || die "the API and OAuthSonas port-forwards need distinct local ports"
if [[ "$smoke_git" == "1" ]]; then
  [[ "$git_service" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Q01_GIT_SERVICE is not a kubernetes service name"
  [[ "$git_port" =~ ^[0-9]+$ ]] && (( git_port >= 1 && git_port <= 65535 )) || die "Q01_GIT_PORT is not a port"
  [[ "$git_port" != "$api_port" && "$git_port" != "$oauth_port" ]] || die "the Git port-forward needs a distinct local port"
fi
[[ "$ready_attempts" =~ ^[0-9]+$ ]] && (( ready_attempts >= 1 && ready_attempts <= 1200 )) || die "Q01_READY_ATTEMPTS is not a bounded attempt count"
[[ "$ready_interval" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "Q01_READY_INTERVAL is not a non-negative number of seconds"
[[ "$cleanup_attempts" =~ ^[0-9]+$ ]] && (( cleanup_attempts >= 1 && cleanup_attempts <= 1200 )) || die "Q01_CLEANUP_ATTEMPTS is not a bounded attempt count"
[[ "$cleanup_interval" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "Q01_CLEANUP_INTERVAL is not a non-negative number of seconds"
[[ -f "$Q01_CA_PEM" ]] || die "Q01_CA_PEM is not a readable file"
[[ -f "$token_script" ]] || die "the oauthsonas token script is missing"
for tool in kubectl curl base64 python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

case "${Q01_EVIDENCE_DIR:-}" in
  "" | /*) ;;
  *) die "Q01_EVIDENCE_DIR must be absolute" ;;
esac

umask 077
work="$(mktemp -d "${TMPDIR:-/tmp}/q01-stage.XXXXXX")"
access_file="$work/access-code"
forward_pids=()

cleanup() {
  local status=$?
  set +e
  unset Q01_TOKEN Q01_ADMIN_TOKEN_FILE Q01_ENGINEER_TOKEN_FILE Q01_REVIEWER_TOKEN_FILE Q01_OPERATOR_TOKEN_FILE
  trap - EXIT INT TERM HUP
  if (( ${#forward_pids[@]} > 0 )); then
    local pid attempt alive
    for pid in "${forward_pids[@]}"; do
      kill -TERM "$pid" 2>/dev/null
    done
    for attempt in $(seq 1 "$cleanup_attempts"); do
      alive=0
      for pid in "${forward_pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
          alive=1
        fi
      done
      (( alive == 0 )) && break
      sleep "$cleanup_interval"
    done
    for pid in "${forward_pids[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        kill -KILL "$pid" 2>/dev/null
      fi
    done
    wait "${forward_pids[@]}" 2>/dev/null
  fi
  rm -f -- "$access_file"
  rm -rf -- "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

start_forward() {
  local service=$1
  local local_port=$2
  local log=$3
  kubectl --context "$Q01_KUBE_CONTEXT" --namespace "$release_namespace" \
    port-forward "svc/$service" "${local_port}:8443" >"$log" 2>&1 &
  forward_pids+=("$!")
}

# wait_http polls one TLS endpoint through the given port-forward. Redirects are
# never followed; any accepted status proves the service is reachable.
wait_http() {
  local name=$1
  local url=$2
  local resolve=$3
  local accept=$4
  local attempt code pid
  for attempt in $(seq 1 "$ready_attempts"); do
    code="$(curl -sS --noproxy '*' --max-time 5 --cacert "$Q01_CA_PEM" --resolve "$resolve" \
      -o /dev/null -w '%{http_code}' "$url" 2>"$work/curl.log" || true)"
    if [[ "$code" =~ ^($accept)$ ]]; then
      return 0
    fi
    for pid in "${forward_pids[@]}"; do
      kill -0 "$pid" 2>/dev/null || die "a port-forward exited before the probe reached the service"
    done
    sleep "$ready_interval"
  done
  die "the $name endpoint did not become ready through its port-forward"
}

start_forward "$kodama_service" "$api_port" "$work/kodama-port-forward.log"
start_forward "$oauth_service" "$oauth_port" "$work/oauthsonas-port-forward.log"
if [[ "$smoke_git" == "1" ]]; then
  start_forward "$git_service" "$git_port" "$work/git-port-forward.log"
fi

# The kodama probe accepts 400: the edge decodes the request envelope before it
# authenticates, so a bodyless probe is invalid_input rather than
# unauthenticated. 400 still proves the verified TLS endpoint is reachable.
wait_http "kodama" \
  "https://${tls_server_name}:${api_port}/api/v1/projects/${Q01_PROJECT_ID}/executions" \
  "${tls_server_name}:${api_port}:127.0.0.1" \
  '200|400|401|403|404'
wait_http "oauthsonas" \
  "https://${oauth_host}:${oauth_port}/oauth2/auth" \
  "${oauth_host}:${oauth_port}:127.0.0.1" \
  '200|302|303|307|308|400'
if [[ "$smoke_git" == "1" ]]; then
  wait_http "git" \
    "https://${git_host}:${git_port}/admin/info/refs?service=git-upload-pack" \
    "${git_host}:${git_port}:127.0.0.1" \
    '200|400|401|403|404'
fi

for pid in "${forward_pids[@]}"; do
  kill -0 "$pid" 2>/dev/null || die "a port-forward exited before the probe reached the service"
done

access_b64="$work/access-code.b64"
if ! kubectl --context "$Q01_KUBE_CONTEXT" --namespace "$release_namespace" \
  get secret "$access_secret" -o "jsonpath={.data.${access_key}}" \
  >"$access_b64" 2>"$work/secret.log"; then
  die "cannot read Secret ${release_namespace}/${access_secret}"
fi
if [[ ! -s "$access_b64" ]]; then
  die "Secret ${release_namespace}/${access_secret} has no ${access_key} value"
fi
if ! base64 -d <"$access_b64" >"$access_file" 2>"$work/base64.log"; then
  die "Secret ${release_namespace}/${access_secret} is not base64 data"
fi
chmod 600 "$access_file"
rm -f -- "$access_b64"
[[ -s "$access_file" ]] || die "the picker access code is empty"

acquire_token() {
  local persona=$1
  local log=$2
  local token
  if ! token="$(OAUTHSONAS_URL="$oauth_url" \
    OAUTHSONAS_RESOLVE="${oauth_host}:${oauth_port}:127.0.0.1" \
    OAUTHSONAS_CA="$Q01_CA_PEM" \
    OAUTHSONAS_ACCESS_CODE_FILE="$access_file" \
    "$token_script" "$persona" 2>"$log")"; then
    die "token acquisition failed for persona ${persona}"
  fi
  [[ -n "$token" ]] || die "token acquisition returned an empty token for persona ${persona}"
  printf '%s' "$token"
}

personas=(admin engineer reviewer operator)
declare -A token_files=()
for persona in "${personas[@]}"; do
  token_file="$work/${persona}-token"
  acquire_token "$persona" "$work/token-${persona}.log" >"$token_file"
  chmod 600 "$token_file"
  [[ -s "$token_file" ]] || die "the acquired ${persona} token is empty"
  token_files[$persona]="$token_file"
done
# The picker access code stays in place until the runner exits: a descriptor
# generated below names it as the just-in-time acquisition source.

installation_file="${Q01_INSTALLATION_FILE:-}"
if [[ -n "$installation_file" ]]; then
  [[ -f "$installation_file" && -r "$installation_file" ]] ||
    die "Q01_INSTALLATION_FILE is not a readable file"
else
  if [[ "${Q01_STAGE_DESCRIPTOR:-0}" == "1" ]]; then
    stage_id="${Q01_STAGE_INSTALLATION_ID:-kodama-stage}"
    revision="${Q01_DEPLOYED_REVISION:-}"
    server_digest="${Q01_SERVER_IMAGE_DIGEST:-}"
    audience="${Q01_OAUTHSONAS_AUDIENCE:-}"
    oauth_ca="${Q01_OAUTHSONAS_CA_FILE:-$Q01_CA_PEM}"
    issuer="${Q01_OAUTHSONAS_ISSUER:-$oauth_url}"
    client_id="${Q01_OAUTHSONAS_CLIENT_ID:-kodactl}"
    redirect_uri="${Q01_OAUTHSONAS_REDIRECT_URI:-http://127.0.0.1:52790/callback}"
    tenant="${Q01_TENANT_ID:-$Q01_PROJECT_ID}"
    workload_url="${Q01_WORKLOAD_API_URL:-https://${tls_server_name}:8443}"
    workload_ca="${Q01_WORKLOAD_CA_FILE:-$Q01_CA_PEM}"
    tools_image="${Q01_IMAGE:-$default_tools_image}"
    harness_image="${Q01_EXTERNAL_HARNESS_IMAGE:-$default_harness_image}"
    [[ "$revision" =~ ^[a-f0-9]{7,64}$ ]] || die "Q01_DEPLOYED_REVISION is required when generating a stage descriptor"
    [[ "$server_digest" =~ ^sha256:[a-f0-9]{64}$ ]] || die "Q01_SERVER_IMAGE_DIGEST must be a sha256 digest when generating a stage descriptor"
    [[ -n "$audience" ]] || die "Q01_OAUTHSONAS_AUDIENCE is required when generating a stage descriptor"
    [[ "$audience" =~ ^https://[^[:space:]]+$ ]] || die "Q01_OAUTHSONAS_AUDIENCE must be an https URL"
    [[ "$issuer" =~ ^https://[^[:space:]]+$ ]] || die "Q01_OAUTHSONAS_ISSUER must be an https URL"
    [[ "$workload_url" =~ ^https://[^[:space:]]+$ ]] || die "Q01_WORKLOAD_API_URL must be an https URL"
    [[ "$stage_id" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || die "Q01_STAGE_INSTALLATION_ID is not a bounded identity"
    [[ "$tenant" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || die "Q01_TENANT_ID is not a bounded identity"
    [[ -f "$oauth_ca" ]] || die "Q01_OAUTHSONAS_CA_FILE is not a readable file"
    [[ -f "$workload_ca" ]] || die "Q01_WORKLOAD_CA_FILE is not a readable file"
    [[ "$tools_image" == *"@sha256:"* ]] || die "Q01_IMAGE must be digest-pinned when generating a stage descriptor"
    [[ "$harness_image" == *"@sha256:"* ]] || die "Q01_EXTERNAL_HARNESS_IMAGE must be digest-pinned when generating a stage descriptor"
    if [[ "$smoke_git" == "1" ]]; then
      [[ -n "$smoke_repository" ]] || die "Q01_SMOKE_REPOSITORY is required when Q01_SMOKE_GIT=1 and generating a stage descriptor"
      [[ -n "$smoke_resource_id" ]] || die "Q01_SMOKE_RESOURCE_ID is required when Q01_SMOKE_GIT=1 and generating a stage descriptor"
    fi
    installation_file="$work/stage-installation.json"
    python3 - "$installation_file" \
      "$stage_id" "$revision" "$server_digest" "$tenant" "$Q01_PROJECT_ID" \
      "$api_url" "$Q01_CA_PEM" "$tls_server_name" "$workload_url" "$workload_ca" \
      "$tools_image" "$harness_image" \
      "$issuer" "$audience" "$oauth_ca" "$client_id" "$redirect_uri" \
      "${oauth_host}:${oauth_port}:127.0.0.1" "$access_file" \
      "${Q01_SUBJECT_ADMIN:-${KODAMA_STAGE_SUBJECT_ADMIN:-admin}}" "${Q01_SUBJECT_ENGINEER:-${KODAMA_STAGE_SUBJECT_ENGINEER:-engineer}}" \
      "${Q01_SUBJECT_REVIEWER:-${KODAMA_STAGE_SUBJECT_REVIEWER:-reviewer}}" "${Q01_SUBJECT_OPERATOR:-${KODAMA_STAGE_SUBJECT_OPERATOR:-operator}}" \
      "$smoke_git" "$git_url" "$git_workload_url" "$git_service_id" "$git_admin_binding_id" "$git_admin_resource_id" \
      "$smoke_repository" "$smoke_resource_id" "$smoke_binding_id" <<'PY'
import json
import sys

(
    path,
    installation_id,
    revision,
    server_digest,
    tenant,
    project,
    api_url,
    api_ca,
    tls_server_name,
    workload_url,
    workload_ca,
    tools_image,
    harness_image,
    issuer,
    audience,
    oauth_ca,
    client_id,
    redirect_uri,
    resolve,
    access_code_file,
    admin_subject,
    engineer_subject,
    reviewer_subject,
    operator_subject,
    smoke_git,
    git_url,
    git_workload_url,
    git_service_id,
    git_admin_binding_id,
    git_admin_resource_id,
    smoke_repository,
    smoke_resource_id,
    smoke_binding_id,
) = sys.argv[1:34]
document = {
    "schema_version": 1,
    "installation_id": installation_id,
    "profile": "production",
    "runtime": "kubernetes",
    "revision": revision,
    "server_image_digest": server_digest,
    "tenant_id": tenant,
    "project_id": project,
    "api": {
        "url": api_url,
        "ca_file": api_ca,
        "tls_server_name": tls_server_name,
        "workload_url": workload_url,
        "workload_ca_file": workload_ca,
    },
    "images": {"tools": tools_image, "external_harness": harness_image},
    "oauthsonas": {
        "issuer": issuer,
        "audience": audience,
        "ca_file": oauth_ca,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "resolve": resolve,
        "access_code_file": access_code_file,
        "personas": {
            "admin": {"id": "admin", "subject": admin_subject},
            "engineer": {"id": "engineer", "subject": engineer_subject},
            "reviewer": {"id": "reviewer", "subject": reviewer_subject},
            "operator": {"id": "operator", "subject": operator_subject},
        },
    },
}
if smoke_git == "1":
    document["git"] = {
        "url": git_url,
        "workload_url": git_workload_url,
        "ca_file": api_ca,
        "workload_ca_file": workload_ca,
        "service_id": git_service_id,
        "admin_binding_id": git_admin_binding_id,
        "admin_resource_id": git_admin_resource_id,
        "repositories": [
            {
                "name": "admin",
                "resource_id": git_admin_resource_id,
                "binding_id": git_admin_binding_id,
            },
            {
                "name": smoke_repository,
                "resource_id": smoke_resource_id,
                "binding_id": smoke_binding_id,
            },
        ],
    }
    document["smoke_repository"] = {
        "name": smoke_repository,
        "resource_id": smoke_resource_id,
        "binding_id": smoke_binding_id,
    }
with open(path, "w", encoding="utf-8") as handle:
    json.dump(document, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
    chmod 600 "$installation_file"
  fi
fi

export Q01_API_URL="$api_url"
export Q01_TLS_SERVER_NAME="$tls_server_name"
# The stage adapter feeds the common installation runner with private token
# files instead of exporting any credential value into the environment.
export Q01_ADMIN_TOKEN_FILE="${token_files[admin]}"
export Q01_ENGINEER_TOKEN_FILE="${token_files[engineer]}"
export Q01_REVIEWER_TOKEN_FILE="${token_files[reviewer]}"
export Q01_OPERATOR_TOKEN_FILE="${token_files[operator]}"
if [[ -n "$installation_file" ]]; then
  export Q01_INSTALLATION_FILE="$installation_file"
fi
# The stage run is the ten non-Git scenarios; an explicit Q01_SMOKE_GIT=1 keeps
# the protected-Git subtests for a Git-enabled installation.
export Q01_SMOKE_GIT="$smoke_git"
unset Q01_TOKEN
# A runner invocation is a live acceptance run: a missing input must fail the
# suite instead of silently skipping it.
export Q01_REQUIRE_STAGE=1
export Q01_REQUIRE_INSTALLATION="${Q01_REQUIRE_INSTALLATION:-1}"
export Q01_CA_PEM Q01_KUBE_CONTEXT Q01_PROJECT_ID
for pass_through in Q01_IMAGE Q01_EXTERNAL_HARNESS_IMAGE Q01_WORKLOAD_CA_FILE Q01_HARNESS_REVISION Q01_DEPLOYED_REVISION Q01_NATS_URL Q01_NATS_CA_FILE Q01_NATS_TOKEN_FILE Q01_NATS_TLS_SERVER_NAME Q01_NATS_CLIENT_CERT_FILE Q01_NATS_CLIENT_KEY_FILE; do
  if [[ -n "${!pass_through:-}" ]]; then
    export "$pass_through"
  fi
done
# A TLS stage broker needs the installation CA. The live probes mint their own
# store-backed service credential for the callout, so the acquired admin token
# is not defaulted into the broker token input; an explicit
# Q01_NATS_TOKEN_FILE still wins. A plain nats:// broker is left untouched.
if [[ "${Q01_NATS_URL:-}" == tls://* ]]; then
  export Q01_NATS_CA_FILE="${Q01_NATS_CA_FILE:-$Q01_CA_PEM}"
  export Q01_NATS_TOKEN_FILE="${Q01_NATS_TOKEN_FILE:-${token_files[admin]}}"
fi
if [[ -z "$installation_file" ]]; then
  # No descriptor supplies the digest pins or the workload trust root, so keep
  # the harness defaults instead of leaving the non-Git smoke requirements
  # unmet.
  export Q01_IMAGE="${Q01_IMAGE:-$default_tools_image}"
  export Q01_EXTERNAL_HARNESS_IMAGE="${Q01_EXTERNAL_HARNESS_IMAGE:-$default_harness_image}"
  export Q01_WORKLOAD_CA_FILE="${Q01_WORKLOAD_CA_FILE:-$Q01_CA_PEM}"
fi
evidence="${Q01_EVIDENCE_DIR:-${TMPDIR:-/tmp}/oc/q01-evidence/$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p -- "$evidence"
chmod 700 -- "$evidence" 2>/dev/null || true
export Q01_EVIDENCE_DIR="$evidence"
printf 'run-stage: evidence directory %s\n' "$evidence" >&2

if (( $# == 0 )); then
  set -- go test -race -tags=e2e -count=1 -timeout=20m ./test/e2e/...
fi

# Only the common runner receives the private token file path. Every earlier
# child (kubectl, curl, the token script, rm/date/mkdir) ran without it.
set +e
"$here/run-installation.sh" -- "$@"
status=$?
set -e
exit "$status"
