#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Seeds the stage Git server and publishes its scoped tokens. Idempotent:
#   1. waits for the Gitea Deployment;
#   2. ensures the repository owner user, the empty-history source repository and its `main` branch;
#   3. ensures a read-scoped and a write-scoped access token that the API still accepts, replacing a
#      token only when the one stored in kodama-runtime is missing or rejected;
#   4. publishes both tokens as the kodama-git-tokens Secret (keys read, write) in the deps namespace; OpenTofu
#      reads that Secret back and renders the runtime Secret from it.
# Inputs (environment): KUBECONFIG, KODAMA_DEPS_NAMESPACE, KODAMA_GITEA_RELEASE, KODAMA_GIT_URL.

require_cmd kubectl python3 curl
DEPS_NS="${KODAMA_DEPS_NAMESPACE:-kodama-deps}"
RELEASE="${KODAMA_GITEA_RELEASE:-kodama-gitea}"
GIT_URL="${KODAMA_GIT_URL:?KODAMA_GIT_URL is required}"
OWNER="$(python3 -c 'import sys; from urllib.parse import urlparse; print(urlparse(sys.argv[1]).path.strip("/").split("/")[0])' "${GIT_URL}")"
REPO="$(python3 -c 'import sys; from urllib.parse import urlparse; print(urlparse(sys.argv[1]).path.strip("/").split("/")[1].removesuffix(".git"))' "${GIT_URL}")"

kube() { kubectl "$@"; }

kube -n "${DEPS_NS}" rollout status "deployment/${RELEASE}" --timeout=420s >/dev/null

ADMIN_USER="$(kube -n "${DEPS_NS}" get secret kodama-gitea-admin -o jsonpath='{.data.username}' | base64 -d)"
ADMIN_PASS="$(kube -n "${DEPS_NS}" get secret kodama-gitea-admin -o jsonpath='{.data.password}' | base64 -d)"

# api METHOD PATH [BODY] [AUTH_USER:AUTH_PASS] -> prints "<status> <body>"; runs inside the Gitea pod so the
# test never depends on public DNS or the Load Balancer.
api() {
  local method="$1" path="$2" body="${3:-}" auth="${4:-${ADMIN_USER}:${ADMIN_PASS}}"
  local args=(-s -o /dev/stdout -w '\n%{http_code}' -u "${auth}" -X "${method}" -H 'Content-Type: application/json')
  [[ -n "${body}" ]] && args+=(-d "${body}")
  kube -n "${DEPS_NS}" exec "deployment/${RELEASE}" -c gitea -- curl "${args[@]}" "http://127.0.0.1:3000/api/v1${path}"
}
status_of() { tail -n1 <<<"$1"; }
body_of() { sed '$d' <<<"$1"; }

# 1. The owner user (a regular user; the admin account never serves Kodama).
res="$(api GET "/users/${OWNER}")"
if [[ "$(status_of "${res}")" == "404" ]]; then
  OWNER_PASS="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
  res="$(api POST /admin/users "$(python3 -c 'import json,sys; print(json.dumps({"username": sys.argv[1], "email": sys.argv[1] + "@stage.kodama.invalid", "password": sys.argv[2], "must_change_password": False, "visibility": "private"}))' "${OWNER}" "${OWNER_PASS}")")"
  [[ "$(status_of "${res}")" == "201" ]] || die "creating Git owner ${OWNER} failed: HTTP $(status_of "${res}")"
  log_info "created Git owner ${OWNER}"
fi

# 2. The repository with a `main` branch (auto-initialised: one README commit).
res="$(api GET "/repos/${OWNER}/${REPO}")"
if [[ "$(status_of "${res}")" == "404" ]]; then
  res="$(api POST "/admin/users/${OWNER}/repos" "$(python3 -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "private": True, "auto_init": True, "default_branch": "main", "readme": "Default", "description": "Kodama stage source repository"}))' "${REPO}")")"
  [[ "$(status_of "${res}")" == "201" ]] || die "creating repository ${OWNER}/${REPO} failed: HTTP $(status_of "${res}")"
  log_info "created repository ${OWNER}/${REPO}"
fi

# 3. Tokens: keep a stored token only while the API accepts it with the expected scope.
stored() { kube -n "${DEPS_NS}" get secret kodama-git-tokens -o "jsonpath={.data.$1}" 2>/dev/null | base64 -d 2>/dev/null || true; }
token_ok() { # token
  local tok="$1" code
  [[ -n "${tok}" ]] || return 1
  code="$(status_of "$(api GET "/repos/${OWNER}/${REPO}" "" "git:${tok}")")"
  [[ "${code}" == "200" ]] || return 1
}
new_token() { # name scopes -> token
  local name="$1" scopes="$2"
  # Token names are unique per user, so each mint carries a timestamp. The owner's password is unknown here, so the
  # admin CLI mints the token inside the pod.
  kube -n "${DEPS_NS}" exec "deployment/${RELEASE}" -c gitea -- \
    gitea admin user generate-access-token --username "${OWNER}" --token-name "${name}-$(date -u +%Y%m%d%H%M%S)" --scopes "${scopes}" --raw
}

READ_TOKEN="$(stored read)"
WRITE_TOKEN="$(stored write)"
if ! token_ok "${READ_TOKEN}"; then
  READ_TOKEN="$(new_token kodama-read read:repository | tr -d '\r\n')"
  log_info "minted a read-scoped Git token"
fi
if ! token_ok "${WRITE_TOKEN}"; then
  WRITE_TOKEN="$(new_token kodama-write write:repository,read:repository | tr -d '\r\n')"
  log_info "minted a write-scoped Git token"
fi
token_ok "${READ_TOKEN}" || die "the read token does not authenticate"
token_ok "${WRITE_TOKEN}" || die "the write token does not authenticate"

# 4. Publish.
TOKEN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kodama-git-tokens.XXXXXX")"
trap 'rm -rf "${TOKEN_DIR}"' EXIT
printf '%s' "${READ_TOKEN}" >"${TOKEN_DIR}/read"
printf '%s' "${WRITE_TOKEN}" >"${TOKEN_DIR}/write"
kube -n "${DEPS_NS}" create secret generic kodama-git-tokens \
  --from-file=read="${TOKEN_DIR}/read" --from-file=write="${TOKEN_DIR}/write" \
  --dry-run=client -o yaml | kube apply -f - >/dev/null
log_info "Git server ready: ${GIT_URL} (owner ${OWNER}); tokens published to ${DEPS_NS}/kodama-git-tokens"
