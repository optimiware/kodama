#!/usr/bin/env bash
# run-stage.sh: run the Q02 qualification package against the deployed stage.
#
# Invoke this script only through the host-wide stage lock:
#
#   scripts/infra-slot.sh --exclusive scw-stage stack -- \
#     test/qualification/production/run-stage.sh go test -race -tags=qualification \
#     -count=1 ./test/qualification/production/...
#
# The script opens kubectl port-forwards to svc/kodama and svc/oauthsonas,
# obtains the admin and developer tokens by capturing scripts/oauthsonas-token
# stdout directly into exported variables, and runs the supplied command. It
# never enables shell tracing, never dumps the environment, headers, bodies,
# Secrets or raw subprocess diagnostics, and joins and kills both port-forwards
# on exit. The picker access code is read from the oauthsonas-access Secret into
# a 0600 file and removed on exit.
#
# Required inputs:
#   Q02_KUBE_CONTEXT      the kubeconfig context
#   Q02_CA_PEM            the installation CA bundle (path)
#   Q02_PROJECT_ID        the admin's own project (for readiness and probes)
#
# Optional inputs:
#   Q02_API_URL           default https://127.0.0.1:${Q02_API_PORT}
#   Q02_API_PORT          default 18443
#   Q02_OAUTHSONAS_PORT   default the port in Q02_OAUTHSONAS_URL (8443)
#   Q02_OAUTHSONAS_HOST   default oauthsonas.kodama-system.svc.cluster.local
#   Q02_OAUTHSONAS_SERVICE default oauthsonas
#   Q02_KODAMA_SERVICE    default kodama
#   Q02_RELEASE_NAMESPACE default kodama-system
#   Q02_EVIDENCE_DIR      default $TMPDIR/oc/q02-evidence/<UTC timestamp>
#   Q02_READY_ATTEMPTS    readiness poll attempts (default 120)
#   Q02_READY_INTERVAL    seconds between readiness polls (default 0.25)
#   Q02_CLEANUP_ATTEMPTS  cleanup wait attempts before KILL (default 50)
#   Q02_CLEANUP_INTERVAL  seconds between cleanup waits (default 0.1)
#   Q02_ADMIN_PERSONA     default admin
#   Q02_DEVELOPER_PERSONA default developer
#   Q02_SECOND_ADMIN_PERSONA  optional second-tenant persona; when set the
#                         script acquires a token for it. Otherwise the caller
#                         may supply Q02_SECOND_ADMIN_TOKEN. The runner captures
#                         that value before unsetting every caller-exported
#                         qualification token and exports it only to the final
#                         command, never to kubectl, curl, the token script or a
#                         filesystem child.
#   Q02_TENANT_ID, Q02_TENANT_NAMESPACE, Q02_IMAGE, Q02_NETWORK_IMAGE,
#   Q02_HARNESS_REVISION,
#   Q02_DEPLOYED_REVISION, Q02_SECOND_TENANT_ID, Q02_SECOND_PROJECT_ID,
#   Q02_SECOND_TENANT_NAMESPACE, Q02_GIT_REPOSITORY_A, Q02_GIT_REPOSITORY_B,
#   Q02_GIT_SERVICE_A, Q02_GIT_SERVICE_B, Q02_WORKSPACE_LOSS_NAMESPACE,
#   Q02_WORKSPACE_LOSS_CONFIRM pass through to the harness.
#
# The script performs no cloud, tofu, node or cluster-scoped change.
set -euo pipefail
set +x
# Never inherit a caller-exported qualification token: only the final test
# command receives the tokens this script acquires. A caller-supplied second
# tenant token is captured first so the runner can export it to that command
# alone.
caller_second_admin_token="${Q02_SECOND_ADMIN_TOKEN:-}"
unset Q02_ADMIN_TOKEN Q02_DEVELOPER_TOKEN Q02_SECOND_ADMIN_TOKEN

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$here/../../.." && pwd -P)"

die() {
  printf 'run-stage: %s\n' "$*" >&2
  exit 1
}

missing=()
for name in Q02_KUBE_CONTEXT Q02_CA_PEM Q02_PROJECT_ID; do
  if [[ -z "${!name:-}" ]]; then
    missing+=("$name")
  fi
done
if (( ${#missing[@]} > 0 )); then
  die "missing required environment input(s): ${missing[*]}"
fi

release_namespace="${Q02_RELEASE_NAMESPACE:-kodama-system}"
kodama_service="${Q02_KODAMA_SERVICE:-kodama}"
oauth_service="${Q02_OAUTHSONAS_SERVICE:-oauthsonas}"
oauth_host="${Q02_OAUTHSONAS_HOST:-oauthsonas.kodama-system.svc.cluster.local}"
api_port="${Q02_API_PORT:-18443}"
# OAuthSonas keeps the issuer at https://<host>:8443 and scripts/oauthsonas-token
# maps that host to the port-forward with curl --resolve, so the local port must
# match the port in the issuer URL. Only one holder runs under the stage lock.
oauth_url="${Q02_OAUTHSONAS_URL:-https://${oauth_host}:8443}"
oauth_url_port=""
if [[ "$oauth_url" =~ ^https://([^/:]+):([0-9]+)$ ]]; then
  [[ "${BASH_REMATCH[1]}" == "$oauth_host" ]] || die "Q02_OAUTHSONAS_URL and Q02_OAUTHSONAS_HOST must name the same host"
  oauth_url_port="${BASH_REMATCH[2]}"
else
  die "Q02_OAUTHSONAS_URL must be https://host:port"
fi
oauth_port="${Q02_OAUTHSONAS_PORT:-$oauth_url_port}"
tls_server_name="${Q02_TLS_SERVER_NAME:-kodama.kodama-system.svc.cluster.local}"
admin_persona="${Q02_ADMIN_PERSONA:-admin}"
developer_persona="${Q02_DEVELOPER_PERSONA:-developer}"
second_admin_persona="${Q02_SECOND_ADMIN_PERSONA:-}"
token_script="${Q02_TOKEN_SCRIPT:-$repo_root/scripts/oauthsonas-token}"
access_secret="${Q02_ACCESS_CODE_SECRET:-oauthsonas-access}"
access_key="${Q02_ACCESS_CODE_KEY:-access_code}"
ready_attempts="${Q02_READY_ATTEMPTS:-120}"
ready_interval="${Q02_READY_INTERVAL:-0.25}"
cleanup_attempts="${Q02_CLEANUP_ATTEMPTS:-50}"
cleanup_interval="${Q02_CLEANUP_INTERVAL:-0.1}"

[[ "$Q02_KUBE_CONTEXT" != -* ]] || die "Q02_KUBE_CONTEXT must not look like a kubectl flag"
[[ "$Q02_PROJECT_ID" =~ ^[A-Za-z0-9_-]+$ ]] || die "Q02_PROJECT_ID is not a bounded identity"
[[ "$release_namespace" == "kodama-system" ]] || die "Q02_RELEASE_NAMESPACE must be kodama-system"
[[ "$access_secret" =~ ^[a-z0-9.-]+$ ]] || die "Q02_ACCESS_CODE_SECRET is not a kubernetes Secret name"
# Secret data keys may contain underscore (the stage key is access_code);
# Secret names may not.
[[ "$access_key" =~ ^[a-z0-9._-]+$ ]] || die "Q02_ACCESS_CODE_KEY is not a Secret data key"
[[ "$kodama_service" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Q02_KODAMA_SERVICE is not a kubernetes service name"
[[ "$oauth_service" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Q02_OAUTHSONAS_SERVICE is not a kubernetes service name"
[[ "$api_port" =~ ^[0-9]+$ ]] && (( api_port >= 1 && api_port <= 65535 )) || die "Q02_API_PORT is not a port"
[[ "$oauth_port" =~ ^[0-9]+$ ]] && (( oauth_port >= 1 && oauth_port <= 65535 )) || die "Q02_OAUTHSONAS_PORT is not a port"
[[ "$oauth_port" == "$oauth_url_port" ]] || die "Q02_OAUTHSONAS_PORT must match the port in Q02_OAUTHSONAS_URL"
[[ "$api_port" != "$oauth_port" ]] || die "the API and OAuthSonas port-forwards need distinct local ports"
[[ "$ready_attempts" =~ ^[0-9]+$ ]] && (( ready_attempts >= 1 && ready_attempts <= 1200 )) || die "Q02_READY_ATTEMPTS is not a bounded attempt count"
[[ "$ready_interval" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "Q02_READY_INTERVAL is not a non-negative number of seconds"
[[ "$cleanup_attempts" =~ ^[0-9]+$ ]] && (( cleanup_attempts >= 1 && cleanup_attempts <= 1200 )) || die "Q02_CLEANUP_ATTEMPTS is not a bounded attempt count"
[[ "$cleanup_interval" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "Q02_CLEANUP_INTERVAL is not a non-negative number of seconds"
[[ -f "$Q02_CA_PEM" ]] || die "Q02_CA_PEM is not a readable file"
[[ -f "$token_script" ]] || die "the oauthsonas token script is missing"
for tool in kubectl curl base64; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

case "${Q02_EVIDENCE_DIR:-}" in
  "" | /*) ;;
  *) die "Q02_EVIDENCE_DIR must be absolute" ;;
esac

umask 077
work="$(mktemp -d "${TMPDIR:-/tmp}/q02-stage.XXXXXX")"
access_file="$work/access-code"
forward_pids=()

cleanup() {
  local status=$?
  set +e
  unset Q02_ADMIN_TOKEN Q02_DEVELOPER_TOKEN Q02_SECOND_ADMIN_TOKEN caller_second_admin_token
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
  kubectl --context "$Q02_KUBE_CONTEXT" --namespace "$release_namespace" \
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
    code="$(curl -sS --noproxy '*' --max-time 5 --cacert "$Q02_CA_PEM" --resolve "$resolve" \
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

# The kodama probe accepts 400: the edge decodes the request envelope before it
# authenticates, so a bodyless probe is invalid_input rather than
# unauthenticated. 400 still proves the verified TLS endpoint is reachable.
wait_http "kodama" \
  "https://${tls_server_name}:${api_port}/api/v1/projects/${Q02_PROJECT_ID}/executions" \
  "${tls_server_name}:${api_port}:127.0.0.1" \
  '200|400|401|403|404'
wait_http "oauthsonas" \
  "https://${oauth_host}:${oauth_port}/oauth2/auth" \
  "${oauth_host}:${oauth_port}:127.0.0.1" \
  '200|302|303|307|308|400'

for pid in "${forward_pids[@]}"; do
  kill -0 "$pid" 2>/dev/null || die "a port-forward exited before the probe reached the service"
done

access_b64="$work/access-code.b64"
if ! kubectl --context "$Q02_KUBE_CONTEXT" --namespace "$release_namespace" \
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
    OAUTHSONAS_CA="$Q02_CA_PEM" \
    OAUTHSONAS_ACCESS_CODE_FILE="$access_file" \
    "$token_script" "$persona" 2>"$log")"; then
    die "token acquisition failed for persona ${persona}"
  fi
  [[ -n "$token" ]] || die "token acquisition returned an empty token for persona ${persona}"
  printf '%s' "$token"
}

Q02_ADMIN_TOKEN="$(acquire_token "$admin_persona" "$work/admin-token.log")" || exit 1
Q02_DEVELOPER_TOKEN="$(acquire_token "$developer_persona" "$work/developer-token.log")" || exit 1
if [[ -n "$second_admin_persona" ]]; then
  Q02_SECOND_ADMIN_TOKEN="$(acquire_token "$second_admin_persona" "$work/second-admin-token.log")" || exit 1
fi
rm -f -- "$access_file"

export Q02_API_URL="${Q02_API_URL:-https://127.0.0.1:${api_port}}"
export Q02_TLS_SERVER_NAME="$tls_server_name"
export Q02_CA_PEM Q02_KUBE_CONTEXT Q02_PROJECT_ID
for pass_through in Q02_TENANT_ID Q02_TENANT_NAMESPACE Q02_IMAGE Q02_NETWORK_IMAGE Q02_HARNESS_REVISION Q02_DEPLOYED_REVISION \
  Q02_SECOND_TENANT_ID Q02_SECOND_PROJECT_ID Q02_SECOND_TENANT_NAMESPACE \
  Q02_GIT_REPOSITORY_A Q02_GIT_REPOSITORY_B Q02_GIT_SERVICE_A Q02_GIT_SERVICE_B \
  Q02_WORKSPACE_LOSS_NAMESPACE Q02_WORKSPACE_LOSS_CONFIRM; do
  if [[ -n "${!pass_through:-}" ]]; then
    export "$pass_through"
  fi
done
evidence="${Q02_EVIDENCE_DIR:-${TMPDIR:-/tmp}/oc/q02-evidence/$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p -- "$evidence"
chmod 700 -- "$evidence" 2>/dev/null || true
export Q02_EVIDENCE_DIR="$evidence"
printf 'run-stage: evidence directory %s\n' "$evidence" >&2

if (( $# == 0 )); then
  set -- go test -race -tags=qualification -count=1 -timeout=10m ./test/qualification/production/...
fi

# Only the command under test receives the acquired tokens. Every earlier child
# (kubectl, curl, the token script, rm/date/mkdir) ran without them. A
# caller-supplied second tenant token is also exported here and nowhere earlier.
if [[ -z "${Q02_SECOND_ADMIN_TOKEN:-}" ]]; then
  Q02_SECOND_ADMIN_TOKEN="$caller_second_admin_token"
fi
export Q02_ADMIN_TOKEN Q02_DEVELOPER_TOKEN
if [[ -n "${Q02_SECOND_ADMIN_TOKEN:-}" ]]; then
  export Q02_SECOND_ADMIN_TOKEN
fi
set +e
"$@"
status=$?
unset Q02_ADMIN_TOKEN Q02_DEVELOPER_TOKEN Q02_SECOND_ADMIN_TOKEN
set -e
exit "$status"
