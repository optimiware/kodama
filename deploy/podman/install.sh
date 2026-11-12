#!/usr/bin/env bash
# install.sh: install and start the single-host Kodama `test` environment with
# rootless Podman.
#
# Prerequisites: rootless Podman with a running `podman system service` socket,
# OpenSSL, curl, jq, git >= 2.44 and socat. Run as the user that owns the
# Podman store: workloads and the control plane share one image store.
#
# The install is re-runnable: it reuses an existing CA, keyfile and database,
# reuses every container whose configuration did not change, keeps still-valid
# service credentials and only rotates the ones that expired or are forced with
# --refresh-credentials.
#
# Everything is configurable from the environment; see lib.sh and the
# quickstart. Secrets are generated into 0600 files under the state directory
# and are handed to containers only through --env-file files. No secret is
# printed and none appears on a command line.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# ---- owned-process cleanup registered before any effect --------------------

started_forwarder_records=()
installer_header_tmp=""
installer_header_file=""

cleanup_admin_header() {
	if [ -n "$installer_header_tmp" ]; then
		installer_header_cleanup "$installer_header_tmp"
	fi
}

record_started_forwarder() {
	started_forwarder_records+=("$1|$2|$3|$4")
}

# stop_started_forwarders joins only the forwarders this run spawned; a
# forwarder reused from an earlier run is not ours to stop on a failure.
stop_started_forwarders() {
	local record pid_file conf_file anchor label
	for record in "${started_forwarder_records[@]}"; do
		IFS='|' read -r pid_file conf_file anchor label <<<"$record"
		stop_forwarder "$pid_file" "$conf_file" "$anchor" "$label"
	done
	started_forwarder_records=()
}

install_exit() {
	local status=$?
	trap - EXIT
	cleanup_admin_header
	if [ "$status" -ne 0 ]; then
		stop_started_forwarders
	fi
	exit "$status"
}
trap install_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM


usage() {
	cat >&2 <<'USAGE'
usage: deploy/podman/install.sh [--refresh-credentials]

  --refresh-credentials  rotate the git and engineer service credentials now
                         (the normal install keeps a still-valid credential)

Environment (all optional, safe defaults):
  KODAMA_LOCAL_PREFIX              container/label prefix      (kodama-local)
  KODAMA_LOCAL_STATE_DIR           absolute state directory    (~/.local/state/kodama/podman/<prefix>)
  KODAMA_LOCAL_HTTPS_PORT          Kodama HTTPS port           (18443)
  KODAMA_LOCAL_OIDC_PORT           OAuthSonas TLS port         (18444)
  KODAMA_LOCAL_GIT_PORT            git serve HTTP port         (18081)
  KODAMA_LOCAL_GIT_HTTPS_PORT      Git HTTPS front-end port    (18445)
  KODAMA_LOCAL_POSTGRES_PORT       PostgreSQL port on loopback (15432)
  KODAMA_LOCAL_NATS_PORT           NATS client port on loopback (14222)
  KODAMA_LOCAL_NATS_MONITOR_PORT   NATS monitor port           (18222)
  KODAMA_LOCAL_OAUTH_UPSTREAM_PORT OAuthSonas plain HTTP port  (18181)
  KODAMA_LOCAL_TENANT              tenant id                   (tenant-local)
  KODAMA_LOCAL_PROJECT             project id (defaults to the tenant; OAuthSonas
                                   carries one claim, so humans are project-scoped
                                   and the default project is named like the tenant)
  KODAMA_LOCAL_POSTGRES_IMAGE      pinned PostgreSQL image
  KODAMA_LOCAL_NATS_IMAGE          pinned NATS image
  KODAMA_LOCAL_OAUTHSONAS_IMAGE    pinned OAuthSonas image
  KODAMA_LOCAL_WORKLOAD_IMAGE      pinned pre-pulled workload image
  KODAMA_LOCAL_EXTERNAL_HARNESS_IMAGE pinned external-harness workload image
  KODAMA_LOCAL_GIT_RESOURCES       comma-separated repo=resource mappings (admin=git-admin)
  KODAMA_LOCAL_KODAMA_IMAGE        use an existing kodama image instead of building
  KODAMA_LOCAL_PODMAN_SOCKET       Podman API socket           ($XDG_RUNTIME_DIR/podman/podman.sock)

Every image override must be tag@sha256:<64 hex>; a bare tag is refused.
USAGE
}

refresh_credentials=0
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--refresh-credentials)
	refresh_credentials=1
	;;
"") ;;
*)
	usage
	exit 2
	;;
esac
[ "$#" -le 1 ] || {
	usage
	exit 2
}

umask 077
require_command podman
require_command openssl
require_command curl
require_command jq
require_command git
require_command socat
require_command realpath
require_command sha256sum

git_version="$(git --version | awk '{print $3}')"
case "$git_version" in
2.4[4-9]* | 2.[5-9][0-9]* | [3-9].*) ;;
*) die "git >= 2.44 is required by git serve (found ${git_version})" ;;
esac

podman info >/dev/null 2>&1 || die "rootless Podman is not usable for this user"
[ -S "$PODMAN_SOCKET" ] || die "KODAMA_LOCAL_PODMAN_SOCKET does not name an existing Unix socket: ${PODMAN_SOCKET} (start it with: systemctl --user enable --now podman.socket; if the unit is already active, systemctl --user restart podman.socket)"

host_ip="$(host_address)"
log "host ${host_ip}: Kodama https ${HTTPS_PORT}, OAuthSonas https ${OIDC_PORT}, git ${GIT_PORT}, git https ${GIT_HTTPS_PORT}, PostgreSQL ${POSTGRES_PORT}, NATS ${NATS_PORT}"

# ---- state directory and markers ------------------------------------------

if [ -e "$STATE_DIR" ]; then
	[ -d "$STATE_DIR" ] || die "state path exists and is not a directory: ${STATE_DIR}"
	check_state_dir "$STATE_DIR" "$PREFIX" install ||
		die "refusing state directory ${STATE_DIR}: it is not empty and does not carry the marker prefix=${PREFIX}"
else
	mkdir -p -- "$STATE_DIR"
	chmod 0700 -- "$STATE_DIR"
fi
if [ ! -s "$marker_file" ]; then
	(
		umask 077
		printf 'prefix=%s\n' "$PREFIX" >"$marker_file"
	)
	chmod 600 "$marker_file"
fi

for directory in "$tls_dir" "$secrets_dir" "$versions_dir" "$data_dir" "$oauth_dir" "$git_storage" "$git_journal" "$run_dir" "$logs_dir" "$git_credential_dir"; do
	ensure_dir "$directory" 700
done

# ---- TLS: one private CA, one Kodama leaf, one OAuthSonas leaf -------------

if [ ! -s "$tls_dir/ca.crt" ] || [ ! -s "$tls_dir/ca.key" ]; then
	log "creating the private installation CA"
	# keyUsage is explicit: a CA certificate without it is refused by strict
	# RFC 5280 verifiers (Python 3.13 and newer enable that mode by default).
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
		-keyout "$tls_dir/ca.key" -out "$tls_dir/ca.crt" -days 825 \
		-subj "/CN=Kodama local install CA" \
		-addext "keyUsage=critical,keyCertSign,cRLSign" >/dev/null 2>&1
fi
chmod 600 "$tls_dir/ca.key"
chmod 644 "$tls_dir/ca.crt"

issue_leaf() {
	local name=$1 common_name=$2 san=$3
	local key="${tls_dir}/${name}.key" crt="${tls_dir}/${name}.crt" csr="${tls_dir}/${name}.csr" ext="${tls_dir}/${name}.ext"
	if [ -s "$crt" ] && [ -s "$key" ] && openssl verify -CAfile "$tls_dir/ca.crt" "$crt" >/dev/null 2>&1; then
		return 0
	fi
	log "issuing the ${name} certificate"
	printf 'subjectAltName=%s\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$san" >"$ext"
	chmod 600 "$ext"
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
		-keyout "$key" -out "$csr" -subj "/CN=${common_name}" >/dev/null 2>&1
	openssl x509 -req -in "$csr" -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
		-CAcreateserial -out "$crt" -days 825 -sha256 -extfile "$ext" >/dev/null 2>&1
	rm -f -- "$csr" "$ext"
	chmod 600 "$key"
	chmod 644 "$crt"
}

issue_leaf tls "$HOST_ALIAS" "$(forwarder_san "$HOST_ALIAS" "$host_ip")"
issue_leaf oauthsonas localhost "DNS:localhost,IP:127.0.0.1"
issue_leaf git localhost "$(forwarder_san "$HOST_ALIAS" "$host_ip")"

# One trust bundle for the Git companion: the host's public roots plus the
# installation CA, so it can both verify the local HTTPS API and any public
# origin. The server itself keeps the system roots (its OIDC CA is explicit).
ca_bundle="${tls_dir}/ca-bundle.crt"
if [ -s /etc/ssl/certs/ca-certificates.crt ]; then
	cat /etc/ssl/certs/ca-certificates.crt "$tls_dir/ca.crt" >"$ca_bundle"
else
	cp "$tls_dir/ca.crt" "$ca_bundle"
fi
chmod 644 "$ca_bundle"

# ---- secrets and 0600 environment files -----------------------------------

ensure_secret "$secrets_dir/postgres-superuser-password" 24
ensure_secret "$secrets_dir/kodama-admin-password" 24
ensure_secret "$secrets_dir/runtime-password" 24
ensure_secret "$secrets_dir/controller-password" 24
ensure_secret "$secrets_dir/publisher-password" 24
ensure_secret "$secrets_dir/auth-password" 24
ensure_secret "$access_code_file" 16

postgres_password="$(cat "$secrets_dir/postgres-superuser-password")"
admin_password="$(cat "$secrets_dir/kodama-admin-password")"
runtime_password="$(cat "$secrets_dir/runtime-password")"
controller_password="$(cat "$secrets_dir/controller-password")"
publisher_password="$(cat "$secrets_dir/publisher-password")"
auth_password="$(cat "$secrets_dir/auth-password")"
access_code="$(cat "$access_code_file")"

database_url="postgres://kodama_runtime:${runtime_password}@127.0.0.1:${POSTGRES_PORT}/${DATABASE}?sslmode=disable"
controller_url="postgres://kodama_controller:${controller_password}@127.0.0.1:${POSTGRES_PORT}/${DATABASE}?sslmode=disable"
publisher_url="postgres://kodama_publisher:${publisher_password}@127.0.0.1:${POSTGRES_PORT}/${DATABASE}?sslmode=disable"
auth_url="postgres://kodama_auth:${auth_password}@127.0.0.1:${POSTGRES_PORT}/${DATABASE}?sslmode=disable"
admin_url="postgres://kodama_admin:${admin_password}@127.0.0.1:${POSTGRES_PORT}/postgres?sslmode=disable"

write_env "$postgres_env" "POSTGRES_PASSWORD=${postgres_password}"
write_env "$migrate_env" \
	"KODAMA_PROFILE=test" \
	"KODAMA_ADMIN_DATABASE_URL=${admin_url}" \
	"KODAMA_DATABASE_URL=${database_url}" \
	"KODAMA_CONTROLLER_DATABASE_URL=${controller_url}" \
	"KODAMA_PUBLISHER_DATABASE_URL=${publisher_url}" \
	"KODAMA_AUTH_DATABASE_URL=${auth_url}"
write_env "$bootstrap_env" \
	"KODAMA_PROFILE=test" \
	"KODAMA_INSTALLATION_ID=${INSTALLATION_ID}" \
	"KODAMA_DATABASE_URL=${database_url}" \
	"KODAMA_KEY_ADAPTER=keyfile" \
	"KODAMA_KEY_REF=keyfile:kek-v1" \
	"KODAMA_KEYFILE=${secrets_dir}/keys/kek-v1.key" \
	"KODAMA_OIDC_ISSUER=${OIDC_ISSUER}" \
	"KODAMA_OIDC_AUDIENCE=${OIDC_AUDIENCE}" \
	"KODAMA_OIDC_JWKS_URL=${OIDC_ISSUER}/.well-known/jwks.json" \
	"KODAMA_OIDC_CA_FILE=${tls_dir}/ca.crt" \
	"KODAMA_OIDC_TENANT_CLAIM=org_id"
ensure_dir "${secrets_dir}/keys" 700
server_common=(
	"KODAMA_PROFILE=test"
	"KODAMA_INSTALLATION_ID=${INSTALLATION_ID}"
	"KODAMA_DATA_DIR=${data_dir}"
	"KODAMA_DATABASE_URL=${database_url}"
	"KODAMA_CONTROLLER_DATABASE_URL=${controller_url}"
	"KODAMA_PUBLISHER_DATABASE_URL=${publisher_url}"
	"KODAMA_AUTH_DATABASE_URL=${auth_url}"
	"KODAMA_NATS_URL=nats://127.0.0.1:${NATS_PORT}"
	"KODAMA_PODMAN_SOCKET=${PODMAN_SOCKET}"
	"KODAMA_KEY_ADAPTER=keyfile"
	"KODAMA_KEY_REF=keyfile:kek-v1"
	"KODAMA_KEYFILE=${secrets_dir}/keys/kek-v1.key"
	"KODAMA_TLS_DIR=${tls_dir}"
	"KODAMA_OIDC_ISSUER=${OIDC_ISSUER}"
	"KODAMA_OIDC_AUDIENCE=${OIDC_AUDIENCE}"
	"KODAMA_OIDC_JWKS_URL=${OIDC_ISSUER}/.well-known/jwks.json"
	"KODAMA_OIDC_CA_FILE=${tls_dir}/ca.crt"
	"KODAMA_OIDC_TENANT_CLAIM=org_id"
	"KODAMA_PUBLIC_URL=${PUBLIC_URL}"
	"KODAMA_WORKLOAD_URL=${WORKLOAD_URL}"
	"KODAMA_ADDRESS=0.0.0.0:${HTTPS_PORT}"
)
# OAuthSonas carries one string claim (org_id). The final server maps it to the
# project as well as the tenant, so humans and project commands are project-
# scoped and the project is named like the tenant. The tenant-wide form is used
# exactly once, to create that first project: a tenant-scoped command refuses a
# project-scoped actor. The bootstrap environment above is the tenant-wide form.
write_env "$server_tenant_env" "${server_common[@]}"
write_env "$server_env" "${server_common[@]}" "KODAMA_OIDC_PROJECT_CLAIM=org_id"
write_env "$oauth_env" "OAUTHSONAS_ACCESS_CODE=${access_code}"
write_env "$oauthsonas_env" \
	"OAUTHSONAS_URL=${OIDC_ISSUER}" \
	"OAUTHSONAS_CA=${tls_dir}/ca.crt" \
	"OAUTHSONAS_ACCESS_CODE_FILE=${access_code_file}" \
	"OAUTHSONAS_CLIENT_ID=${OIDC_CLIENT_ID}" \
	"OAUTHSONAS_REDIRECT=${OIDC_REDIRECT}"
write_env "$operator_env" \
	"# Generated by deploy/podman/install.sh; source this file, do not commit it." \
	"KODAMA_LOCAL_PREFIX=${PREFIX}" \
	"KODAMA_LOCAL_STATE_DIR=${STATE_DIR}" \
	"KODAMA_LOCAL_HTTPS_PORT=${HTTPS_PORT}" \
	"KODAMA_LOCAL_OIDC_PORT=${OIDC_PORT}" \
	"KODAMA_LOCAL_GIT_PORT=${GIT_PORT}" \
	"KODAMA_LOCAL_GIT_HTTPS_PORT=${GIT_HTTPS_PORT}" \
	"KODAMA_LOCAL_TENANT=${TENANT}" \
	"KODAMA_LOCAL_GIT_RESOURCES=${GIT_RESOURCES}" \
	"KODAMA_PUBLIC_URL=${PUBLIC_URL}" \
	"KODAMA_WORKLOAD_URL=${WORKLOAD_URL}" \
	"KODAMA_GIT_URL=${GIT_PUBLIC_URL}" \
	"KODAMA_GIT_WORKLOAD_URL=${GIT_WORKLOAD_URL}" \
	"KODAMA_INSTALLATION_FILE=${installation_file}" \
	"KODAMA_CA_FILE=${tls_dir}/ca.crt" \
	"KODAMA_ADMIN_TOKEN_FILE=${admin_token_file}"

# OAuthSonas configuration carries no secret; the access code stays in the
# environment file. The file is mounted directly, so its mode is world-readable
# only because it holds public client/persona configuration.
umask 022
cat >"$oauth_config" <<EOF
issuer: ${OIDC_ISSUER}
api_audience: ${OIDC_AUDIENCE}
clients:
  - id: ${OIDC_CLIENT_ID}
    name: ${OIDC_CLIENT_ID}
    public: true
    redirect_uris:
      - ${OIDC_REDIRECT}
personas:
  - id: admin
    name: Local Administrator
    email: admin@localhost.test
    subject: admin
    organization_id: ${TENANT}
    roles: [admin]
  - id: engineer
    name: Local Engineer
    email: engineer@localhost.test
    subject: engineer
    organization_id: ${TENANT}
    roles: [engineer]
  - id: reviewer
    name: Local Reviewer
    email: reviewer@localhost.test
    subject: reviewer
    organization_id: ${TENANT}
    roles: [reviewer]
  - id: operator
    name: Local Operator
    email: operator@localhost.test
    subject: operator
    organization_id: ${TENANT}
    roles: [operator]
EOF
umask 077
chmod 644 "$oauth_config"

# ---- images ----------------------------------------------------------------

for image in "$POSTGRES_IMAGE" "$NATS_IMAGE" "$OAUTHSONAS_IMAGE" "$WORKLOAD_IMAGE" "$EXTERNAL_HARNESS_IMAGE"; do
	ensure_pinned_image "$image"
done

revision="$(git -C "$repo_root" rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')"
source_revision="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || printf 'unknown')"

# git_backend_present reports whether the image carries git-http-backend, the
# program `git serve` runs for every smart-HTTP exchange. The project image
# installs it; a prebuilt image supplied by the operator must carry it too.
git_backend_present() {
	podman run --rm --label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.command=image-check" \
		--entrypoint /bin/sh "$1" -c 'test -x "$(git --exec-path)/git-http-backend"' >/dev/null 2>&1
}

if [ -n "$KODAMA_IMAGE" ]; then
	image_exists "$KODAMA_IMAGE" || die "KODAMA_LOCAL_KODAMA_IMAGE is not present in the local image store: ${KODAMA_IMAGE}"
	git_backend_present "$KODAMA_IMAGE" ||
		die "the prebuilt kodama image ${KODAMA_IMAGE} has no git-http-backend; it must install Alpine's git-daemon package (see deploy/images/kodama/Containerfile) so the Git service can serve smart HTTP"
	log "using the prebuilt kodama image ${KODAMA_IMAGE}"
else
	KODAMA_IMAGE="localhost/${PREFIX}/kodama:${KODAMA_IMAGE_TAG}"
	if [ -s "$image_rev_file" ] && [ "$(cat "$image_rev_file")" = "$revision" ] &&
		image_exists "$KODAMA_IMAGE" && git_backend_present "$KODAMA_IMAGE"; then
		log "reusing the kodama image ${KODAMA_IMAGE} for ${revision}"
	else
		log "building the kodama image from ${revision}"
		podman build --format oci -q \
			-f "${repo_root}/deploy/images/kodama/Containerfile" \
			--label "org.opencontainers.image.revision=${revision}" \
			-t "$KODAMA_IMAGE" "$repo_root" >/dev/null
		git_backend_present "$KODAMA_IMAGE" ||
			die "the built kodama image ${KODAMA_IMAGE} has no git-http-backend; deploy/images/kodama/Containerfile must install Alpine's git-daemon package"
		printf '%s\n' "$revision" >"$image_rev_file"
	fi
fi
kodama_image_id="$(podman image inspect --format '{{.Id}}' "$KODAMA_IMAGE")"
[ -n "$kodama_image_id" ] || die "the kodama image ${KODAMA_IMAGE} has no local identity"
server_digest="$(podman image inspect --format '{{.Digest}}' "$KODAMA_IMAGE" 2>/dev/null || true)"
[ -n "$server_digest" ] || server_digest="$kodama_image_id"
if [ -n "${KODAMA_LOCAL_KODAMA_IMAGE:-}" ]; then
	image_revision="$(podman image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$KODAMA_IMAGE" 2>/dev/null || true)"
	case "$image_revision" in
	"" | "<no value>") die "KODAMA_LOCAL_KODAMA_IMAGE carries no org.opencontainers.image.revision label; its revision provenance cannot be recorded" ;;
	esac
	source_revision="$image_revision"
fi

# ---- container helpers -----------------------------------------------------

container_current() {
	local name=$1 hash=$2
	podman container exists "$name" >/dev/null 2>&1 || return 1
	[ "$(podman inspect --format '{{.State.Running}}' "$name" 2>/dev/null)" = "true" ] || return 1
	[ "$(podman inspect --format '{{index .Config.Labels "kodama.local.confighash"}}' "$name" 2>/dev/null)" = "$hash" ] || return 1
}

remove_container() {
	podman rm -f -- "$1" >/dev/null 2>&1 || true
}

# ensure_own_volume <name>: create a labelled volume, or refuse a foreign one
# that already uses the name.
ensure_own_volume() {
	local name=$1 label
	if podman volume exists "$name" >/dev/null 2>&1; then
		label="$(podman volume inspect --format '{{index .Labels "kodama.local.prefix"}}' "$name" 2>/dev/null || true)"
		[ "$label" = "$PREFIX" ] ||
			die "refusing volume ${name}: it is not labelled kodama.local.prefix=${PREFIX}"
		return 0
	fi
	podman volume create --label "kodama.local.prefix=${PREFIX}" "$name" >/dev/null
}

start_postgres() {
	local hash
	hash="$(config_hash "$POSTGRES_IMAGE" "$(cat "$postgres_env")")"
	container_current "$container_postgres" "$hash" && return 0
	log "starting PostgreSQL"
	remove_container "$container_postgres"
	ensure_own_volume "$postgres_volume"
	podman run -d --name "$container_postgres" \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.component=postgres" \
		--label "kodama.local.confighash=${hash}" \
		-p "127.0.0.1:${POSTGRES_PORT}:5432" \
		-v "${postgres_volume}:/var/lib/postgresql" \
		--env-file "$postgres_env" \
		"$POSTGRES_IMAGE" >/dev/null
	wait_for_postgres
}

wait_for_postgres() {
	local deadline=$((SECONDS + 60))
	until podman exec "$container_postgres" psql -U postgres -d postgres -Atc 'SELECT 1' >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "PostgreSQL did not accept connections within 60s"
		sleep 0.5
	done
}

start_nats() {
	local hash
	hash="$(config_hash "$NATS_IMAGE")"
	container_current "$container_nats" "$hash" && return 0
	log "starting NATS JetStream"
	remove_container "$container_nats"
	ensure_own_volume "$nats_volume"
	podman run -d --name "$container_nats" \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.component=nats" \
		--label "kodama.local.confighash=${hash}" \
		-p "127.0.0.1:${NATS_PORT}:4222" \
		-p "127.0.0.1:${NATS_MONITOR_PORT}:8222" \
		-v "${nats_volume}:/data" \
		"$NATS_IMAGE" -js -sd /data -m 8222 >/dev/null
	wait_http "http://127.0.0.1:${NATS_MONITOR_PORT}/healthz" 30 || die "NATS did not become ready within 30s"
}

start_oauthsonas() {
	local hash
	hash="$(config_hash "$OAUTHSONAS_IMAGE" "$(cat "$oauth_config")" "$(cat "$oauth_env")")"
	container_current "$container_oauth" "$hash" && return 0
	log "starting OAuthSonas"
	remove_container "$container_oauth"
	podman run -d --name "$container_oauth" \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.component=oauthsonas" \
		--label "kodama.local.confighash=${hash}" \
		--network=host \
		--env-file "$oauth_env" \
		-v "${oauth_config}:/etc/oauthsonas/config.yaml:ro" \
		"$OAUTHSONAS_IMAGE" --config /etc/oauthsonas/config.yaml --listen "127.0.0.1:${OAUTH_UPSTREAM_PORT}" >/dev/null
	wait_http "http://127.0.0.1:${OAUTH_UPSTREAM_PORT}/healthz" 30 || die "OAuthSonas did not become ready within 30s"
}

# start_forwarder <label> <pid-file> <conf-file> <log-file> <bind> <port>
#                 <upstream-port> <cert> <key> <ready-url> <anchor>
#
# One bundle-owned socat TLS front end. It reuses only a live process whose
# config and exact LISTEN specification still match, restarts only such an owned
# process, and never signals a process merely because a pid file exists.
start_forwarder() {
	local label=$1 pid_file=$2 conf_file=$3 log_file=$4 bind=$5
	local port=$6 upstream=$7 cert=$8 key=$9 ready_url=${10} anchor=${11}
	local desired current_pid
	desired="$(printf 'listen=%s:%s\nupstream=127.0.0.1:%s\ncert=%s\nkey=%s\n' \
		"$bind" "$port" "$upstream" "$cert" "$key")"
	if [ -s "$pid_file" ] && [ -s "$conf_file" ] &&
		[ "$(cat -- "$conf_file")" = "$desired" ] &&
		process_matches_anchor "$(cat -- "$pid_file")" "$anchor"; then
		return 0
	fi
	if [ -s "$pid_file" ] && process_matches_anchor "$(cat -- "$pid_file")" "$anchor"; then
		stop_forwarder "$pid_file" "$conf_file" "$anchor" "$label"
	else
		rm -f -- "$pid_file" "$conf_file"
	fi
	log "starting the ${label}"
	# The forwarder is a long-lived child: close every inherited descriptor
	# above the standard three so it cannot hold the infrastructure slot's lock
	# after this install exits. The subshell execs socat, so the recorded pid is
	# the forwarder's own.
	(
		close_inherited_fds
		exec socat "OPENSSL-LISTEN:${port},bind=${bind},reuseaddr,fork,cert=${cert},key=${key},verify=0" \
			"TCP:127.0.0.1:${upstream}"
	) >"$log_file" 2>&1 &
	current_pid=$!
	printf '%s\n' "$current_pid" >"$pid_file"
	chmod 600 -- "$pid_file"
	printf '%s\n' "$desired" >"$conf_file"
	chmod 600 -- "$conf_file"
	record_started_forwarder "$pid_file" "$conf_file" "$anchor" "$label"
	wait_http "$ready_url" 30 --cacert "$tls_dir/ca.crt" ||
		die "${label} did not become ready within 30s (see ${log_file})"
}

start_oauth_tls() {
	start_forwarder "OAuthSonas TLS terminator" "$socat_pid_file" "$socat_conf_file" "$socat_log" \
		127.0.0.1 "$OIDC_PORT" "$OAUTH_UPSTREAM_PORT" \
		"${tls_dir}/oauthsonas.crt" "${tls_dir}/oauthsonas.key" \
		"${OIDC_ISSUER}/.well-known/openid-configuration" "OPENSSL-LISTEN:${OIDC_PORT},"
}

start_git_tls() {
	start_forwarder "Git HTTPS front end" "$git_socat_pid_file" "$git_socat_conf_file" "$git_socat_log" \
		0.0.0.0 "$GIT_HTTPS_PORT" "$GIT_PORT" \
		"${tls_dir}/git.crt" "${tls_dir}/git.key" \
		"https://localhost:${GIT_HTTPS_PORT}/" "OPENSSL-LISTEN:${GIT_HTTPS_PORT},"
}

start_server() {
	local env_file=$1 hash
	hash="$(config_hash "$KODAMA_IMAGE" "$kodama_image_id" "$(cat "$env_file")")"
	container_current "$container_server" "$hash" && return 0
	log "starting kodama server --profile test"
	remove_container "$container_server"
	podman run -d --name "$container_server" \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.component=server" \
		--label "kodama.local.confighash=${hash}" \
		--network=host \
		--userns=keep-id:uid=65532,gid=65532 \
		-v "${STATE_DIR}:${STATE_DIR}" \
		-v "$(dirname "$PODMAN_SOCKET"):$(dirname "$PODMAN_SOCKET")" \
		--env-file "$env_file" \
		"$KODAMA_IMAGE" server --profile test >/dev/null
	wait_server_ready
}

stop_server() {
	podman stop --time 10 "$container_server" >/dev/null 2>&1 || true
	podman rm -f "$container_server" >/dev/null 2>&1 || true
}

wait_server_ready() {
	local deadline=$((SECONDS + 90)) ready=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		if ! podman container exists "$container_server" >/dev/null 2>&1; then
			die "the server container disappeared"
		fi
		if [ "$(podman inspect --format '{{.State.Running}}' "$container_server")" != "true" ]; then
			podman logs "$container_server" >&2 || true
			die "kodama server exited before readiness"
		fi
		if [ "$(curl --silent --show-error --max-time 3 --cacert "$tls_dir/ca.crt" --output /dev/null --write-out '%{http_code}' "${PUBLIC_URL}/readyz" 2>/dev/null || true)" = "200" ]; then
			ready=1
			break
		fi
		sleep 1
	done
	[ -n "$ready" ] || die "kodama server was not ready within 90s (podman logs ${container_server})"
}

start_git() {
	local hash resource_args=()
	hash="$(config_hash "$KODAMA_IMAGE" "$kodama_image_id" "$(cat "$git_env")" "$GIT_RESOURCES")"
	container_current "$container_git" "$hash" && return 0
	log "starting kodama git serve"
	remove_container "$container_git"
	IFS=',' read -r -a git_resource_mappings <<<"$GIT_RESOURCES"
	for mapping in "${git_resource_mappings[@]}"; do
		resource_args+=(--resource "$mapping")
	done
	podman run -d --name "$container_git" \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.component=git" \
		--label "kodama.local.confighash=${hash}" \
		--network=host \
		--userns=keep-id:uid=65532,gid=65532 \
		-v "${git_root}:${git_root}" \
		-v "${git_credential_dir}:${git_credential_dir}:ro" \
		-v "${ca_bundle}:${ca_bundle}:ro" \
		--env-file "$git_env" \
		"$KODAMA_IMAGE" git serve \
		--tenant "$TENANT" \
		--listen "127.0.0.1:${GIT_PORT}" \
		--storage-root "$git_storage" \
		--journal-dir "$git_journal" \
		--base-url "$PUBLIC_URL" \
		--credential-file "$git_token_file" \
		"${resource_args[@]}" >/dev/null
	local deadline=$((SECONDS + 30)) git_ready=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		if ! podman container exists "$container_git" >/dev/null 2>&1 ||
			[ "$(podman inspect --format '{{.State.Running}}' "$container_git")" != "true" ]; then
			podman logs "$container_git" >&2 || true
			die "kodama git serve exited before listening"
		fi
		if curl --silent --show-error --max-time 2 --output /dev/null "http://127.0.0.1:${GIT_PORT}/" 2>/dev/null; then
			git_ready=1
			break
		fi
		sleep 0.5
	done
	[ -n "$git_ready" ] || die "kodama git serve did not listen within 30s"
}

# ---- infrastructure --------------------------------------------------------

start_postgres
# A non-superuser administrator owns migrations; the four runtime roles are
# created by `kodama migrate`. The statement travels on stdin, never argv.
podman exec -i "$container_postgres" psql -v ON_ERROR_STOP=1 -U postgres -d postgres >/dev/null <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kodama_admin') THEN
    CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS;
  END IF;
END
\$\$;
ALTER ROLE kodama_admin WITH PASSWORD '${admin_password}';
SQL

start_nats
start_oauthsonas
start_oauth_tls

# ---- migrate and bootstrap -------------------------------------------------

log "applying migrations"
kodama_run "$migrate_env" migrate

log "obtaining an OAuthSonas administrator token"
OAUTHSONAS_URL="$OIDC_ISSUER" \
	OAUTHSONAS_CA="$tls_dir/ca.crt" \
	OAUTHSONAS_ACCESS_CODE_FILE="$access_code_file" \
	OAUTHSONAS_CLIENT_ID="$OIDC_CLIENT_ID" \
	OAUTHSONAS_REDIRECT="$OIDC_REDIRECT" \
	"${repo_root}/scripts/oauthsonas-token" admin >"$admin_token_file"
chmod 600 "$admin_token_file"

if [ ! -s "$bootstrap_marker" ]; then
	log "bootstrapping the first tenant and administrator through OAuthSonas"
	init_flag=""
	if [ ! -s "${secrets_dir}/keys/kek-v1.key" ]; then
		init_flag="--init-keyfile"
	fi
	if ! bootstrap_output="$(kodama_run "$bootstrap_env" bootstrap-admin --profile test ${init_flag} \
		--tenant "$TENANT" --tenant-name "Local Kodama tenant" \
		--token-file "$admin_token_file")"; then
		die "bootstrap-admin failed; if the tenant already exists without ${bootstrap_marker}, run down.sh --purge and install again"
	fi
	printf '%s\n' "$bootstrap_output" >"$bootstrap_output_file"
	chmod 600 "$bootstrap_output_file"
	admin_principal="$(printf '%s\n' "$bootstrap_output" | sed -n 's/^tenant .*; first administrator \(.*\)$/\1/p')"
	[ -n "$admin_principal" ] || die "bootstrap-admin did not report the first administrator identity"
	printf '%s\n' "$admin_principal" >"$admin_principal_file"
	chmod 600 "$admin_principal_file"
	printf 'bootstrapped\n' >"$bootstrap_marker"
	chmod 600 "$bootstrap_marker"
else
	log "tenant ${TENANT} is already bootstrapped"
fi

# The installer keeps its own administrator header in a private directory it
# creates for this invocation. The operator's ${secrets_dir}/admin-header is
# never read, rewritten or removed here.
installer_header_tmp="$(installer_header_dir)" ||
	die "could not create the installer's private header directory under ${run_dir}"
installer_header_file="${installer_header_tmp}/admin-header"
{
	umask 077
	printf 'Authorization: Bearer %s\n' "$(cat "$admin_token_file")"
	printf 'Content-Type: application/json\n'
} >"$installer_header_file"
chmod 600 "$installer_header_file"

# ---- HTTP helpers ----------------------------------------------------------

api_call() {
	# <method> <path> <input-json> <idempotency-key> <body-out> [expected-version]
	local method=$1 path=$2 input=$3 key=$4 body_out=$5 expected=${6:-0}
	local response="${secrets_dir}/api-response.$$"
	{
		umask 077
		jq -cn --argjson input "$input" --arg key "$key" --argjson expected "$expected" \
			'if $expected > 0 then {input: $input, expected_version: $expected, idempotency_key: $key} else {input: $input, idempotency_key: $key} end' \
			>"${secrets_dir}/api-request.$$"
	}
	chmod 600 "${secrets_dir}/api-request.$$"
	local status
	status="$(curl --silent --show-error --max-time 20 \
		--cacert "$tls_dir/ca.crt" \
		--request "$method" \
		--header @"$installer_header_file" \
		--data-binary @"${secrets_dir}/api-request.$$" \
		--output "$response" \
		--write-out '%{http_code}' \
		"${PUBLIC_URL}${path}" 2>/dev/null || true)"
	rm -f -- "${secrets_dir}/api-request.$$"
	[ -f "$response" ] || : >"$response"
	printf '%s' "$status" >"${body_out}.status"
	mv -f -- "$response" "$body_out"
	[ "$status" = "200" ]
}

api_code() { jq -r '.code // empty' "$1" 2>/dev/null || true; }
api_message() { jq -r '.message // empty' "$1" 2>/dev/null || true; }

# token_valid returns 0 for a usable service token, 1 for a refused one and 2
# for a dependency failure. It performs one read with the service credential.
token_valid() {
	local token_file=$1 out="${secrets_dir}/token-check.$$"
	if [ ! -s "$token_file" ]; then
		return 1
	fi
	{
		umask 077
		printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$(cat "$token_file")"
	} >"${secrets_dir}/token-check-header.$$"
	chmod 600 "${secrets_dir}/token-check-header.$$"
	local status
	status="$(curl --silent --show-error --max-time 10 --cacert "$tls_dir/ca.crt" \
		--request GET --header @"${secrets_dir}/token-check-header.$$" \
		--data-binary '{"input":{}}' --output "$out" --write-out '%{http_code}' \
		"${PUBLIC_URL}/api/v1/projects/${PROJECT}/executions" 2>/dev/null || true)"
	rm -f -- "${secrets_dir}/token-check-header.$$" "$out"
	case "$status" in
	200) return 0 ;;
	401 | 403) return 1 ;;
	*) return 2 ;;
	esac
}

version_file() { printf '%s/%s' "$versions_dir" "$1"; }

write_atomic_value() {
	local file=$1 value=$2 temporary="${1}.tmp.$$"
	(
		umask 077
		printf '%s\n' "$value" >"$temporary"
	)
	chmod 600 "$temporary"
	mv -f -- "$temporary" "$file"
}

read_version() {
	local file
	file="$(version_file "$1")"
	if [ -s "$file" ]; then
		cat "$file"
		return
	fi
	printf '1'
}

write_version() {
	write_atomic_value "$(version_file "$1")" "$2"
}

current_version_from_error() {
	sed -n 's/.*current version \([0-9][0-9]*\).*/\1/p' "$1" | head -n 1
}

# mutate_credential <label> <service> <token-file> <ttl> <requested-version>
mutate_credential() {
	local label=$1 service=$2 token_file=$3 ttl=$4 version=$5
	local out="${secrets_dir}/credential-${label}.$$"
	local attempt key rotated=0 issued=0
	for attempt in 1 2 3; do
		key="${PREFIX}-${label}-rotate-$(date +%s)-$$-${attempt}"
		if api_call POST "/api/v1/services/${service}/credentials/rotate" \
			"$(jq -cn --arg service "$service" --argjson ttl "$ttl" '{service_id: $service, ttl_seconds: $ttl}')" \
			"$key" "$out" "$version"; then
			rotated=1
			break
		fi
		if [ "$(api_code "$out")" = "version_conflict" ]; then
			local current
			current="$(current_version_from_error "$out")"
			[ -n "$current" ] || die "credential rotation for ${service} reported a version conflict without the current version"
			version="$current"
			continue
		fi
		case "$(api_message "$out")" in
		*"no current service credential"*) break ;;
		esac
		die "service.credential.rotate failed for ${service}: $(api_code "$out"): $(api_message "$out")"
	done
	if [ "$rotated" -eq 0 ]; then
		for attempt in 1 2 3; do
			key="${PREFIX}-${label}-issue-$(date +%s)-$$-${attempt}"
			if api_call POST "/api/v1/services/${service}/credentials" \
				"$(jq -cn --arg service "$service" --argjson ttl "$ttl" '{service_id: $service, ttl_seconds: $ttl}')" \
				"$key" "$out" "$version"; then
				issued=1
				break
			fi
			if [ "$(api_code "$out")" = "version_conflict" ]; then
				local current
				current="$(current_version_from_error "$out")"
				[ -n "$current" ] || die "credential issue for ${service} reported a version conflict without the current version"
				version="$current"
				continue
			fi
			case "$(api_message "$out")" in
			*"already has a current credential"*) die "credential issue for ${service} raced with an existing credential; re-run install.sh" ;;
			esac
			die "service.credential.issue failed for ${service}: $(api_code "$out"): $(api_message "$out")"
		done
	fi
	if [ "$rotated" -eq 0 ] && [ "$issued" -eq 0 ]; then
		die "could not rotate or issue the credential for ${service}"
	fi
	jq -er '.body.token | select(type == "string" and length > 0)' "$out" >"${out}.token.tmp" ||
		die "credential mutation for ${service} returned no token"
	mv -f -- "${out}.token.tmp" "$token_file"
	chmod 600 "$token_file"
	write_version "$label" "$(jq -er '.body.version' "$out")"
	rm -f -- "$out"
}

# refresh_credential <label> <service> <token-file> <ttl> <force>
refresh_credential() {
	local label=$1 service=$2 token_file=$3 ttl=$4 force=$5
	if [ "$force" -eq 0 ]; then
		local token_rc=0
		token_valid "$token_file" || token_rc=$?
		case "$token_rc" in
		0)
			log "keeping the valid ${label} service credential"
			return 0
			;;
		1) ;;
		*) die "could not verify the ${label} service credential (dependency failure)" ;;
		esac
	fi
	local version
	version="$(read_version "$label")"
	mutate_credential "$label" "$service" "$token_file" "$ttl" "$version"
	log "rotated the ${label} service credential"
}

# ---- first-run project and roles ------------------------------------------

tenant_setup=0
[ -s "$project_marker" ] || tenant_setup=1
[ -s "$roles_marker" ] || tenant_setup=1

engineer_principal="$(principal_id "$OIDC_ISSUER" engineer)" || die "cannot derive the engineer principal id"
reviewer_principal="$(principal_id "$OIDC_ISSUER" reviewer)" || die "cannot derive the reviewer principal id"
operator_principal="$(principal_id "$OIDC_ISSUER" operator)" || die "cannot derive the operator principal id"
personas_fingerprint="$(config_hash "$OIDC_ISSUER" "$PROJECT" \
	"$engineer_principal" engineer "$reviewer_principal" reviewer "$operator_principal" operator)"
personas_current=0
if [ -s "$personas_marker" ] && [ "$(cat -- "$personas_marker")" = "$personas_fingerprint" ]; then
	personas_current=1
fi

tenant_server=0
if [ "$tenant_setup" -eq 1 ]; then
	log "starting the tenant-wide setup server for the first project and roles"
	stop_server
	start_server "$server_tenant_env"
	tenant_server=1
	if [ ! -s "$project_marker" ]; then
		log "provisioning project ${PROJECT}"
		if ! api_call POST "/api/v1/tenants/${TENANT}/projects" \
			"$(jq -cn --arg tenant "$TENANT" --arg project "$PROJECT" '{tenant_id: $tenant, project_id: $project, name: "Local project"}')" \
			"${PREFIX}-project-create" "${secrets_dir}/api-project"; then
			if [ "$(api_code "${secrets_dir}/api-project")" != "version_conflict" ]; then
				die "project.create failed: $(api_code "${secrets_dir}/api-project"): $(api_message "${secrets_dir}/api-project")"
			fi
		fi
		printf 'project\n' >"$project_marker"
		chmod 600 "$project_marker"
	fi
	if [ ! -s "$roles_marker" ]; then
		if [ ! -s "$admin_principal_file" ]; then
			die "the first administrator identity is missing; run down.sh --purge and install again"
		fi
		admin_principal="$(cat "$admin_principal_file")"
		log "granting the first administrator explicit engineer, reviewer and operator roles"
		if ! api_call PUT "/api/v1/projects/${PROJECT}/memberships/${admin_principal}" \
			"$(jq -cn --arg project "$PROJECT" --arg principal "$admin_principal" '{project_id: $project, principal_id: $principal, roles: ["engineer", "reviewer", "operator"]}')" \
			"${PREFIX}-membership-admin" "${secrets_dir}/api-membership-admin"; then
			if [ "$(api_code "${secrets_dir}/api-membership-admin")" != "version_conflict" ]; then
				die "membership.grant failed: $(api_code "${secrets_dir}/api-membership-admin"): $(api_message "${secrets_dir}/api-membership-admin")"
			fi
		fi
		printf 'roles\n' >"$roles_marker"
		chmod 600 "$roles_marker"
	fi
elif [ "$personas_current" -eq 0 ]; then
	# Upgrade path: an existing install has no persona marker, so grant the
	# four personas through the tenant-wide server without touching the old
	# roles.done marker. Membership operations are tenant-scoped.
	log "starting the tenant-wide setup server for the persona memberships"
	stop_server
	start_server "$server_tenant_env"
	tenant_server=1
fi

if [ "$personas_current" -eq 0 ]; then
	log "granting the engineer, reviewer and operator personas their project memberships"
	grant_persona_membership "$engineer_principal" '["engineer"]' engineer
	grant_persona_membership "$reviewer_principal" '["reviewer"]' reviewer
	grant_persona_membership "$operator_principal" '["operator"]' operator
	write_private_file "$personas_marker" "$personas_fingerprint"
	log "persona memberships confirmed"
else
	log "persona memberships already confirmed for this installation"
fi

if [ "$tenant_server" -eq 1 ]; then
	stop_server
fi

start_server "$server_env"
log "server is ready at ${PUBLIC_URL}"

# ---- services and credentials ---------------------------------------------

ensure_service() {
	# <service-id> <roles-json> <version-label>
	local service=$1 roles=$2 label=$3
	local out="${secrets_dir}/service-${service}"
	if api_call POST "/api/v1/projects/${PROJECT}/services" \
		"$(jq -cn --arg project "$PROJECT" --arg service "$service" --argjson roles "$roles" '{project_id: $project, service_id: $service, roles: $roles}')" \
		"${PREFIX}-service-create-${service}" "$out"; then
		if [ ! -s "$(version_file "$label")" ]; then
			write_version "$label" "$(jq -er '.body.version' "$out")"
		fi
	else
		if [ "$(api_code "$out")" != "version_conflict" ]; then
			die "service.create failed for ${service}: $(api_code "$out"): $(api_message "$out")"
		fi
	fi
	rm -f -- "$out"
}

ensure_service "$SERVICE" '["viewer"]' git
ensure_service "$ENGINEER_SERVICE" '["engineer"]' engineer

if [ "$refresh_credentials" -eq 1 ]; then
	refresh_credential git "$SERVICE" "$git_token_file" 28800 1
	refresh_credential engineer "$ENGINEER_SERVICE" "$engineer_token_file" 28800 1
else
	refresh_credential git "$SERVICE" "$git_token_file" 28800 0
	refresh_credential engineer "$ENGINEER_SERVICE" "$engineer_token_file" 28800 0
fi

write_env "$git_env" \
	"KODAMA_BASE_URL=${PUBLIC_URL}" \
	"KODAMA_CREDENTIAL_FILE=${git_token_file}" \
	"SSL_CERT_FILE=${ca_bundle}"

# ensure_git_binding registers the admin repository binding on a fresh install
# and reconciles its trusted origins on a re-run. The Git origins are the
# installation's real HTTPS front ends (host and workload); the API origin is
# not a Git endpoint.
ensure_git_binding() {
	local binding="git-admin" desired version current out read_out attempt
	desired="$(jq -cn --arg host "$GIT_PUBLIC_URL" --arg workload "$GIT_WORKLOAD_URL" '[$host, $workload]')"
	local register_body update_body
	register_body="$(jq -cn --arg project "$PROJECT" --arg service "$SERVICE" --argjson origins "$desired" \
		'{project_id: $project, binding_id: "git-admin", resource_id: "git-admin", enforcing_service: $service, trusted_origins: $origins, operations: ["git.read", "git.ref.update"], reportable_events: ["git.ref.updated"]}')"
	update_body="$(jq -cn --arg service "$SERVICE" --argjson origins "$desired" \
		'{resource_id: "git-admin", enforcing_service: $service, trusted_origins: $origins, operations: ["git.read", "git.ref.update"], reportable_events: ["git.ref.updated"]}')"
	for attempt in 1 2 3; do
		out="${secrets_dir}/api-binding-${binding}.$$"
		read_out="${out}.read"
		if api_call GET "/api/v1/bindings/${binding}" \
			"$(jq -cn --arg binding "$binding" '{binding_id: $binding}')" \
			"${PREFIX}-binding-read-${binding}-${attempt}-$(date +%s)" "$read_out"; then
			version="$(jq -er '.body.version' "$read_out")" || die "the ${binding} binding has no version"
			current="$(jq -c '.body.trusted_origins | sort' "$read_out")"
			if [ "$current" = "$(printf '%s' "$desired" | jq -c 'sort')" ]; then
				rm -f -- "$out" "$read_out" "${read_out}.status"
				return 0
			fi
			if api_call PATCH "/api/v1/bindings/${binding}" "$update_body" \
				"${PREFIX}-binding-update-${binding}-${attempt}-$(date +%s)" "$out" "$version"; then
				rm -f -- "$out" "$read_out" "${out}.status" "${read_out}.status"
				return 0
			fi
			if [ "$(api_code "$out")" = "version_conflict" ]; then
				continue
			fi
			die "resource.binding.update failed: $(api_code "$out"): $(api_message "$out")"
		fi
		rm -f -- "$read_out" "${read_out}.status"
		if api_call POST "/api/v1/projects/${PROJECT}/bindings" "$register_body" \
			"${PREFIX}-binding-register-${binding}-${attempt}-$(date +%s)" "$out"; then
			rm -f -- "$out" "${out}.status"
			return 0
		fi
		if [ "$(api_code "$out")" = "version_conflict" ]; then
			continue
		fi
		die "resource.binding.register failed: $(api_code "$out"): $(api_message "$out")"
	done
	die "could not reconcile the ${binding} resource binding"
}

log "registering the admin repository binding with the Git HTTPS origins"
ensure_git_binding

log "provisioning the tenant admin repository"
kodama_run "$server_env" git provision-tenant \
	--tenant "$TENANT" --storage-root "$git_storage" --journal-dir "$git_journal" >/dev/null

start_git
start_git_tls

write_installation_descriptor "$installation_file" "$source_revision" "$server_digest" ||
	die "could not write the installation descriptor ${installation_file}"

log "installed and ready"
log "  Kodama:    ${PUBLIC_URL} (CA ${tls_dir}/ca.crt)"
log "  git serve: http://127.0.0.1:${GIT_PORT} (loopback only)"
log "  git https: ${GIT_PUBLIC_URL} (workloads: ${GIT_WORKLOAD_URL})"
log "  installation: ${installation_file}"
log "  operator environment: ${operator_env}"
log "  status: deploy/podman/status.sh"
log "  service credentials expire after 8h; renew with install.sh --refresh-credentials"
