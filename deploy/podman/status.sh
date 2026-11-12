#!/usr/bin/env bash
# status.sh: report the local Kodama install without printing a secret.
#
# Exits 0 only when the installation descriptor is this installation's valid
# private descriptor, the control plane answers /readyz 200, both owned TLS
# forwarders and all owned containers run, and both Git listeners answer.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_command jq

identity_ok=1
containers_ok=1
forwarders_ok=1
readiness_ok=0
git_ok=0
git_https_ok=0

printf 'prefix: %s\n' "$PREFIX"
printf 'state:  %s\n' "$STATE_DIR"
printf 'tenant: %s\n' "$TENANT"
printf 'project: %s\n' "$PROJECT"
printf 'kodama: %s\n' "$PUBLIC_URL"
printf 'git:    http://127.0.0.1:%s (loopback only)\n' "$GIT_PORT"
printf 'git https: %s (workloads: %s)\n' "$GIT_PUBLIC_URL" "$GIT_WORKLOAD_URL"
printf 'oauth:  %s\n' "$OIDC_ISSUER"
printf 'ports:  KODAMA_LOCAL_HTTPS_PORT=%s KODAMA_LOCAL_OIDC_PORT=%s KODAMA_LOCAL_GIT_PORT=%s KODAMA_LOCAL_GIT_HTTPS_PORT=%s KODAMA_LOCAL_POSTGRES_PORT=%s KODAMA_LOCAL_NATS_PORT=%s\n' \
	"$HTTPS_PORT" "$OIDC_PORT" "$GIT_PORT" "$GIT_HTTPS_PORT" "$POSTGRES_PORT" "$NATS_PORT"
printf 'overrides: KODAMA_LOCAL_PREFIX KODAMA_LOCAL_STATE_DIR KODAMA_LOCAL_TENANT KODAMA_LOCAL_WORKLOAD_IMAGE KODAMA_LOCAL_EXTERNAL_HARNESS_IMAGE KODAMA_LOCAL_GIT_RESOURCES\n'

printf 'installation: %s\n' "$installation_file"
if [ -f "$installation_file" ]; then
	if reason="$(validate_installation_descriptor "$installation_file")"; then
		printf 'installation identity: valid\n'
	else
		printf 'installation identity: invalid (%s)\n' "$reason"
		identity_ok=0
	fi
else
	printf 'installation identity: missing (run install.sh)\n'
	identity_ok=0
fi

if [ -e "$STATE_DIR" ] && ! check_state_dir "$STATE_DIR" "$PREFIX" install; then
	printf 'warning: %s does not carry the marker prefix=%s\n' "$STATE_DIR" "$PREFIX"
fi

printf '\ncontainers (label kodama.local.prefix=%s):\n' "$PREFIX"
if podman ps -a --filter "label=kodama.local.prefix=${PREFIX}" --format '  {{.Names}}\t{{.Status}}' | grep -q .; then
	podman ps -a --filter "label=kodama.local.prefix=${PREFIX}" --format '  {{.Names}}\t{{.Status}}'
else
	printf '  none\n'
	containers_ok=0
fi
for name in "$container_server" "$container_git" "$container_oauth" "$container_nats" "$container_postgres"; do
	if [ "$(podman inspect --format '{{.State.Running}}' "$name" 2>/dev/null || true)" != "true" ]; then
		containers_ok=0
	fi
done

printf '\nvolumes (label kodama.local.prefix=%s):\n' "$PREFIX"
if podman volume ls --filter "label=kodama.local.prefix=${PREFIX}" --format '  {{.Name}}' | grep -q .; then
	podman volume ls --filter "label=kodama.local.prefix=${PREFIX}" --format '  {{.Name}}'
else
	printf '  none\n'
fi

printf '\nTLS forwarders (owned host processes, not detached or reboot-surviving):\n'
oauth_forwarder="$(forwarder_state "$socat_pid_file" "OPENSSL-LISTEN:${OIDC_PORT},")"
git_forwarder="$(forwarder_state "$git_socat_pid_file" "OPENSSL-LISTEN:${GIT_HTTPS_PORT},")"
printf '  oauthsonas: %s\n' "$oauth_forwarder"
printf '  git https:  %s\n' "$git_forwarder"
case "$oauth_forwarder" in
running:*) ;;
*) forwarders_ok=0 ;;
esac
case "$git_forwarder" in
running:*) ;;
*) forwarders_ok=0 ;;
esac

printf '\nreadiness: '
ready="$(curl --silent --show-error --max-time 4 --cacert "$tls_dir/ca.crt" \
	--output /dev/null --write-out '%{http_code}' "${PUBLIC_URL}/readyz" 2>/dev/null || true)"
if [ "$ready" = "200" ]; then
	printf 'ready (HTTP %s)\n' "$ready"
	readiness_ok=1
else
	printf 'not ready (HTTP %s)\n' "${ready:-none}"
fi

printf 'git serve: '
git_code="$(curl --silent --show-error --max-time 4 --output /dev/null \
	--write-out '%{http_code}' "http://127.0.0.1:${GIT_PORT}/" 2>/dev/null || true)"
if [ -n "$git_code" ] && [ "$git_code" != "000" ]; then
	printf 'listening (loopback HTTP %s)\n' "$git_code"
	git_ok=1
else
	printf 'not listening\n'
fi

printf 'git https: '
git_https_code="$(curl --silent --show-error --max-time 4 --cacert "$tls_dir/ca.crt" \
	--output /dev/null --write-out '%{http_code}' "${GIT_PUBLIC_URL}/" 2>/dev/null || true)"
if [ -n "$git_https_code" ] && [ "$git_https_code" != "000" ]; then
	printf 'listening (HTTPS %s)\n' "$git_https_code"
	git_https_ok=1
else
	printf 'not listening\n'
fi

printf '\nservice credentials: expire after 8h; renew with deploy/podman/install.sh --refresh-credentials\n'
for label in git engineer; do
	version_file="$(printf '%s/%s' "$versions_dir" "$label")"
	if [ -s "$version_file" ]; then
		printf '  %-8s service version %s\n' "$label" "$(cat "$version_file")"
	else
		printf '  %-8s service version unknown (no credential mutation recorded)\n' "$label"
	fi
done

if [ "$identity_ok" -eq 1 ] && [ "$containers_ok" -eq 1 ] && [ "$forwarders_ok" -eq 1 ] &&
	[ "$readiness_ok" -eq 1 ] && [ "$git_ok" -eq 1 ] && [ "$git_https_ok" -eq 1 ]; then
	exit 0
fi
exit 1
