#!/usr/bin/env bash
# Shared configuration and helpers for the local Podman install bundle.
#
# Sourced by install.sh, status.sh and down.sh. Every setting comes from the
# environment with a safe default; names, ports and the state directory are
# configurable. This file never writes, reads aloud or prints a secret.
#
# The bundle runs PostgreSQL, JetStream NATS, OAuthSonas (with a host-side TLS
# terminator), `kodama server --profile test` and `kodama git serve` as
# containers on the same rootless Podman store. Secrets live only in 0600
# files under the state directory and reach containers through --env-file.

set -euo pipefail

die() {
	printf 'kodama-local: %s\n' "$*" >&2
	exit 1
}

log() {
	printf 'kodama-local: %s\n' "$*" >&2
}

bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${bundle_dir}/../.." && pwd)"

# ---- validation helpers ----------------------------------------------------

# validate_state_path canonicalises an absolute state directory and refuses
# degenerate targets: empty, relative, "/", the user's home and its parent.
# It prints the canonical path; it never creates or changes anything.
validate_state_path() {
	local raw=$1 canonical home parent
	[ -n "$raw" ] || return 1
	case "$raw" in
	/*) ;;
	*) return 1 ;;
	esac
	canonical="$(realpath -m -- "$raw" 2>/dev/null)" || return 1
	[ -n "$canonical" ] || return 1
	[ "$canonical" != "/" ] || return 1
	case "$canonical" in
	//*) return 1 ;;
	esac
	if [ -n "${HOME:-}" ]; then
		home="$(realpath -m -- "$HOME" 2>/dev/null)" || home=""
		if [ -n "$home" ]; then
			parent="$(dirname -- "$home")"
			[ "$canonical" != "$home" ] || return 1
			[ "$canonical" != "$parent" ] || return 1
		fi
	fi
	printf '%s' "$canonical"
}

# marker_prefix prints the prefix recorded in one installation marker. The
# whole file is one line; extra lines or any other content are refused.
marker_prefix() {
	local file=$1 content
	[ -f "$file" ] || return 1
	content="$(cat -- "$file" 2>/dev/null)" || return 1
	case "$content" in
	prefix=*) content="${content#prefix=}" ;;
	*) return 1 ;;
	esac
	case "$content" in
	*$'\n'*) return 1 ;;
	esac
	[ -n "$content" ] || return 1
	printf '%s' "$content"
}

# check_state_dir applies the installation-marker rules.
#   install: an absent or empty directory is acceptable; a non-empty directory
#            must carry a marker for this exact prefix.
#   purge:   the directory must exist, be non-empty and carry this prefix.
check_state_dir() {
	local path=$1 prefix=$2 mode=$3 marker recorded
	marker="${path}/.kodama-local"
	case "$mode" in
	install | purge) ;;
	*) return 2 ;;
	esac
	if [ ! -e "$path" ]; then
		[ "$mode" = "install" ]
		return
	fi
	[ -d "$path" ] || return 1
	if [ -z "$(ls -A -- "$path" 2>/dev/null)" ]; then
		[ "$mode" = "install" ]
		return
	fi
	recorded="$(marker_prefix "$marker")" || return 1
	[ "$recorded" = "$prefix" ]
}

# validate_image_pin accepts only repository[:tag]@sha256:<64 hex>. A bare tag
# or any other mutable reference is refused.
validate_image_pin() {
	local reference=$1
	[[ "$reference" =~ ^[^[:space:]@]+(:[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$ ]]
}

# validate_port checks one port number.
validate_port() {
	local name=$1 value=$2
	[[ "$value" =~ ^[0-9]+$ ]] || die "${name} must be a decimal port number: ${value}"
	[ "$value" -ge 1 ] && [ "$value" -le 65535 ] || die "${name} must be between 1 and 65535: ${value}"
}

# ---- naming and network ----------------------------------------------------

PREFIX="${KODAMA_LOCAL_PREFIX:-kodama-local}"
case "$PREFIX" in
"" | *[!a-z0-9-]* | [!a-z0-9]*)
	die "KODAMA_LOCAL_PREFIX must be a lower-case DNS label (it names containers, labels and the state marker): ${PREFIX}"
	;;
esac

STATE_DIR="${KODAMA_LOCAL_STATE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/kodama/podman/${PREFIX}}"
STATE_DIR="$(validate_state_path "$STATE_DIR")" ||
	die "KODAMA_LOCAL_STATE_DIR must be an absolute, non-degenerate directory (not /, the home directory or its parent): ${KODAMA_LOCAL_STATE_DIR:-}"

HTTPS_PORT="${KODAMA_LOCAL_HTTPS_PORT:-18443}"
OIDC_PORT="${KODAMA_LOCAL_OIDC_PORT:-18444}"
GIT_PORT="${KODAMA_LOCAL_GIT_PORT:-18081}"
GIT_HTTPS_PORT="${KODAMA_LOCAL_GIT_HTTPS_PORT:-18445}"
POSTGRES_PORT="${KODAMA_LOCAL_POSTGRES_PORT:-15432}"
NATS_PORT="${KODAMA_LOCAL_NATS_PORT:-14222}"
NATS_MONITOR_PORT="${KODAMA_LOCAL_NATS_MONITOR_PORT:-18222}"
OAUTH_UPSTREAM_PORT="${KODAMA_LOCAL_OAUTH_UPSTREAM_PORT:-18181}"

validate_port KODAMA_LOCAL_HTTPS_PORT "$HTTPS_PORT"
validate_port KODAMA_LOCAL_OIDC_PORT "$OIDC_PORT"
validate_port KODAMA_LOCAL_GIT_PORT "$GIT_PORT"
validate_port KODAMA_LOCAL_GIT_HTTPS_PORT "$GIT_HTTPS_PORT"
validate_port KODAMA_LOCAL_POSTGRES_PORT "$POSTGRES_PORT"
validate_port KODAMA_LOCAL_NATS_PORT "$NATS_PORT"
validate_port KODAMA_LOCAL_NATS_MONITOR_PORT "$NATS_MONITOR_PORT"
validate_port KODAMA_LOCAL_OAUTH_UPSTREAM_PORT "$OAUTH_UPSTREAM_PORT"
declare -A seen_ports=()
for entry in "https:${HTTPS_PORT}" "oidc:${OIDC_PORT}" "git:${GIT_PORT}" "git-https:${GIT_HTTPS_PORT}" "postgres:${POSTGRES_PORT}" \
	"nats:${NATS_PORT}" "nats-monitor:${NATS_MONITOR_PORT}" "oauth-upstream:${OAUTH_UPSTREAM_PORT}"; do
	name=${entry%%:*}
	value=${entry#*:}
	[ -z "${seen_ports[$value]:-}" ] || die "ports ${seen_ports[$value]} and ${name} collide on ${value}"
	seen_ports[$value]=$name
done
unset seen_ports

TENANT="${KODAMA_LOCAL_TENANT:-tenant-local}"
PROJECT="${KODAMA_LOCAL_PROJECT:-$TENANT}"
SERVICE="${KODAMA_LOCAL_GIT_SERVICE:-git}"
ENGINEER_SERVICE="${KODAMA_LOCAL_ENGINEER_SERVICE:-engineer}"
GIT_RESOURCES="${KODAMA_LOCAL_GIT_RESOURCES:-admin=git-admin}"
INSTALLATION_ID="${KODAMA_LOCAL_INSTALLATION_ID:-installation-${PREFIX}}"
DATABASE="${KODAMA_LOCAL_DATABASE:-kodama}"
HOST_ALIAS="${KODAMA_LOCAL_HOST_ALIAS:-host.containers.internal}"

case "$GIT_RESOURCES" in
"" | ,* | *, | *,,*) die "KODAMA_LOCAL_GIT_RESOURCES must be a comma-separated repository=resource list" ;;
esac
IFS=',' read -r -a git_resource_mappings <<<"$GIT_RESOURCES"
for mapping in "${git_resource_mappings[@]}"; do
	case "$mapping" in
	*=*) ;;
	*) die "KODAMA_LOCAL_GIT_RESOURCES mapping '${mapping}' must be repository=resource-id" ;;
	esac
done
unset git_resource_mappings

PUBLIC_URL="${KODAMA_LOCAL_PUBLIC_URL:-https://localhost:${HTTPS_PORT}}"
WORKLOAD_URL="${KODAMA_LOCAL_WORKLOAD_URL:-https://${HOST_ALIAS}:${HTTPS_PORT}}"
GIT_PUBLIC_URL="${KODAMA_LOCAL_GIT_PUBLIC_URL:-https://localhost:${GIT_HTTPS_PORT}}"
GIT_WORKLOAD_URL="${KODAMA_LOCAL_GIT_WORKLOAD_URL:-https://${HOST_ALIAS}:${GIT_HTTPS_PORT}}"
OIDC_ISSUER="${KODAMA_LOCAL_OIDC_ISSUER:-https://localhost:${OIDC_PORT}}"
OIDC_AUDIENCE="${KODAMA_LOCAL_OIDC_AUDIENCE:-$PUBLIC_URL}"
OIDC_REDIRECT="${KODAMA_LOCAL_OIDC_REDIRECT:-http://127.0.0.1:52790/callback}"
OIDC_CLIENT_ID="${KODAMA_LOCAL_OIDC_CLIENT_ID:-kodactl}"
DISCOVERY_AUTHORIZATION_ENDPOINT="${KODAMA_LOCAL_DISCOVERY_AUTHORIZATION_ENDPOINT:-${OIDC_ISSUER}/oauth2/auth}"
DISCOVERY_TOKEN_ENDPOINT="${KODAMA_LOCAL_DISCOVERY_TOKEN_ENDPOINT:-${OIDC_ISSUER}/oauth2/token}"
DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT="${KODAMA_LOCAL_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT:-${OIDC_ISSUER}/oauth2/device}"
DISCOVERY_REVOCATION_ENDPOINT="${KODAMA_LOCAL_DISCOVERY_REVOCATION_ENDPOINT:-${OIDC_ISSUER}/oauth2/revoke}"
DISCOVERY_SCOPES="${KODAMA_LOCAL_DISCOVERY_SCOPES:-openid,profile,email}"

# Images that run with installation or operator credentials are pinned by tag
# and manifest digest, exactly like the deployment chart. A digest-only override
# is accepted; a bare tag is refused.
POSTGRES_IMAGE="${KODAMA_LOCAL_POSTGRES_IMAGE:-docker.io/library/postgres:18-alpine@sha256:300ebd02c6d4de9ee27ad7f6855d5d21b4d3a4a5117767c1197d284c635d9736}"
NATS_IMAGE="${KODAMA_LOCAL_NATS_IMAGE:-docker.io/library/nats:2.15-alpine@sha256:89c5c61b5969261e05106a98e465ccb85c7f4bf4e94c1e70786c189759d79e7e}"
OAUTHSONAS_IMAGE="${KODAMA_LOCAL_OAUTHSONAS_IMAGE:-ghcr.io/optimiware/oauthsonas:1.5.0@sha256:39f371048640a95aef951d4ffbc5efd58168f58910e9bb4b972694fa7e82d178}"
WORKLOAD_IMAGE="${KODAMA_LOCAL_WORKLOAD_IMAGE:-docker.io/library/buildpack-deps:trixie-scm@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c}"
EXTERNAL_HARNESS_IMAGE="${KODAMA_LOCAL_EXTERNAL_HARNESS_IMAGE:-docker.io/library/alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b}"
KODAMA_IMAGE="${KODAMA_LOCAL_KODAMA_IMAGE:-}"
KODAMA_IMAGE_TAG="${KODAMA_LOCAL_KODAMA_IMAGE_TAG:-local}"

for image_ref in "$POSTGRES_IMAGE" "$NATS_IMAGE" "$OAUTHSONAS_IMAGE" "$WORKLOAD_IMAGE" "$EXTERNAL_HARNESS_IMAGE"; do
	validate_image_pin "$image_ref" || die "image ${image_ref} must be pinned as repository[:tag]@sha256:<64 hex>"
done
unset image_ref

PODMAN_SOCKET="${KODAMA_LOCAL_PODMAN_SOCKET:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock}"

# ---- state layout ----------------------------------------------------------

marker_file="${STATE_DIR}/.kodama-local"
tls_dir="${STATE_DIR}/tls"
secrets_dir="${STATE_DIR}/secrets"
versions_dir="${STATE_DIR}/service-versions"
data_dir="${STATE_DIR}/data"
oauth_dir="${STATE_DIR}/oauth"
git_root="${STATE_DIR}/git"
git_storage="${git_root}/repos"
git_journal="${git_root}/journal"
git_credential_dir="${STATE_DIR}/git-credential"
run_dir="${STATE_DIR}/run"
logs_dir="${STATE_DIR}/logs"
image_rev_file="${STATE_DIR}/image.rev"

# PostgreSQL and NATS keep their mutable stores in podman-managed volumes: the
# images drop privileges to their own users, which a host bind mount owned by
# the operator cannot always serve. The volumes carry the installation label
# and are removed by name on --purge, never by a host-wide prune.
postgres_volume="${PREFIX}-pgdata"
nats_volume="${PREFIX}-natsdata"

postgres_env="${secrets_dir}/postgres.env"
migrate_env="${secrets_dir}/migrate.env"
bootstrap_env="${secrets_dir}/bootstrap.env"
server_env="${secrets_dir}/server.env"
server_tenant_env="${secrets_dir}/server-tenant.env"
git_env="${secrets_dir}/git.env"
oauth_env="${secrets_dir}/oauth.env"
oauthsonas_env="${secrets_dir}/oauthsonas.env"
admin_token_file="${secrets_dir}/admin-token"
git_token_file="${git_credential_dir}/token"
engineer_token_file="${secrets_dir}/engineer-token"
admin_principal_file="${STATE_DIR}/admin-principal"
bootstrap_output_file="${STATE_DIR}/bootstrap.out"
access_code_file="${secrets_dir}/oauth-access-code"
# The operator's own administrator header, written by the quickstart's sign-in
# block. An install never reads, rewrites or removes it; the installer uses
# installer_header_dir instead.
admin_header_file="${secrets_dir}/admin-header"
operator_env="${secrets_dir}/operator.env"
oauth_config="${oauth_dir}/config.yaml"
bootstrap_marker="${STATE_DIR}/bootstrap.done"
project_marker="${STATE_DIR}/project.done"
roles_marker="${STATE_DIR}/roles.done"
personas_marker="${STATE_DIR}/personas.done"
socat_pid_file="${run_dir}/socat.pid"
socat_conf_file="${run_dir}/socat.conf"
socat_log="${logs_dir}/socat.log"
git_socat_pid_file="${run_dir}/git-https.pid"
git_socat_conf_file="${run_dir}/git-https.conf"
git_socat_log="${logs_dir}/git-https.log"
installation_file="${run_dir}/installation.json"
git_credentials_file="${secrets_dir}/git-credentials"

container_postgres="${PREFIX}-postgres"
container_nats="${PREFIX}-nats"
container_oauth="${PREFIX}-oauthsonas"
container_server="${PREFIX}-server"
container_git="${PREFIX}-git"

# ---- helpers ---------------------------------------------------------------

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

host_address() {
	local address=""
	if command -v ip >/dev/null 2>&1; then
		address="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}')"
	fi
	if [ -z "$address" ] && command -v hostname >/dev/null 2>&1; then
		address="$(hostname -I 2>/dev/null | awk '{print $1}')"
	fi
	case "$address" in
	"" | 127.* | 0.*) die "cannot determine this host's default-route IPv4 address; workloads reach the server through ${HOST_ALIAS}" ;;
	esac
	printf '%s' "$address"
}

# ensure_secret <file> <bytes>: create a random secret once and keep it private.
ensure_secret() {
	local file=$1 bytes=$2
	if [ ! -s "$file" ]; then
		openssl rand -hex "$bytes" >"$file"
	fi
	chmod 600 "$file"
}

# ensure_dir <path> <mode>: create a directory with an explicit private mode.
ensure_dir() {
	local path=$1 mode=$2
	mkdir -p -- "$path"
	chmod "$mode" -- "$path"
}

# write_env <file> <line>...: atomically write a 0600 environment file.
write_env() {
	local file=$1
	shift
	local temporary="${file}.tmp.$$"
	(
		umask 077
		printf '%s\n' "$@" >"$temporary"
	)
	chmod 600 "$temporary"
	mv -f -- "$temporary" "$file"
}

# config_hash prints one stable digest for a set of configuration values.
config_hash() {
	printf '%s\n' "$@" | sha256sum | awk '{print $1}'
}

# podman_rm_own removes only containers carrying this installation's label.
podman_rm_own() {
	local ids
	ids="$(podman ps -aq --filter "label=kodama.local.prefix=${PREFIX}" 2>/dev/null || true)"
	[ -n "$ids" ] || return 0
	# shellcheck disable=SC2086
	podman rm -f -- $ids >/dev/null
}

# kodama_run <env-file> <args>... runs one short-lived kodama command in the
# bundle image with the state directory mounted at its host-absolute path.
kodama_run() {
	local env_file=$1
	shift
	podman run --rm --network=host \
		--userns=keep-id:uid=65532,gid=65532 \
		--label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.command=one-shot" \
		-v "${STATE_DIR}:${STATE_DIR}" \
		--env-file "$env_file" \
		"$KODAMA_IMAGE" "$@"
}

# image_exists reports whether a local image reference is present.
image_exists() {
	podman image exists "$1" >/dev/null 2>&1
}

# ensure_pinned_image pulls a tag@digest reference when absent and refuses a
# local digest that differs from the pin.
ensure_pinned_image() {
	local reference=$1 repository digest observed
	repository="${reference%@*}"
	digest="${reference#*@}"
	if ! image_exists "$reference"; then
		log "pulling ${reference}"
		podman pull --quiet "$reference" >/dev/null
	fi
	observed="$(podman image inspect --format '{{.Digest}}' "$reference" 2>/dev/null || true)"
	[ "$observed" = "$digest" ] ||
		die "image ${repository} resolved to ${observed:-nothing}; expected the pinned ${digest}"
}

# wait_http <url> <seconds> <curl args>... waits for any HTTP response.
wait_http() {
	local url=$1 seconds=$2
	shift 2
	local deadline=$((SECONDS + seconds))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if curl --silent --show-error --max-time 2 "$@" --output /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

# ---- API response helpers ---------------------------------------------------

# api_code and api_message read the stable public error body one api_call wrote.
api_code() { jq -r '.code // empty' "$1" 2>/dev/null || true; }
api_message() { jq -r '.message // empty' "$1" 2>/dev/null || true; }

# current_version_from_error reads the observed version from a version_conflict
# message so a caller can re-read and retry once.
current_version_from_error() {
	sed -n 's/.*current version \([0-9][0-9]*\).*/\1/p' "$1" | head -n 1
}

# installer_header_dir prints one invocation-private 0700 directory for the
# installer's own administrator header. The operator's
# ${secrets_dir}/admin-header is never read, rewritten or removed by an
# install, so a header an operator generated from the quickstart survives every
# install and re-run.
installer_header_dir() {
	mktemp -d -- "${run_dir}/header.XXXXXX"
}

# installer_header_cleanup <directory> removes only the private directory
# installer_header_dir created. Any other path is left alone.
installer_header_cleanup() {
	local directory=$1
	[ -n "$directory" ] || return 0
	case "$directory" in
	"${run_dir}"/header.*) ;;
	*) return 0 ;;
	esac
	rm -f -- "${directory}/admin-header"
	rmdir -- "$directory" 2>/dev/null || true
}

# ---- private atomic files ---------------------------------------------------

# write_private_file <path> <content>: atomically replace one 0600 file. The
# temporary file lives in the destination directory, so the rename is atomic.
write_private_file() {
	local path=$1 content=$2
	local temporary="${path}.tmp.$$"
	(
		umask 077
		printf '%s' "$content" >"$temporary"
	)
	chmod 600 -- "$temporary"
	mv -f -- "$temporary" "$path"
}

# ---- installation descriptor ------------------------------------------------

# principal_id <issuer> <subject> prints the stable verified human principal id:
# "oidc-" plus the first 16 SHA-256 bytes of issuer, NUL and subject, exactly as
# internal/shell/integration/oidc.PrincipalIDFor derives it.
principal_id() {
	local issuer=$1 subject=$2
	[ -n "$issuer" ] || return 1
	[ -n "$subject" ] || return 1
	printf 'oidc-%s' "$(printf '%s\0%s' "$issuer" "$subject" | sha256sum | cut -c1-32)"
}

# forwarder_san <host alias> <host address> prints the leaf SAN list shared by
# the API and Git HTTPS front ends.
forwarder_san() {
	printf 'DNS:localhost,DNS:%s,IP:127.0.0.1,IP:%s' "$1" "$2"
}

# installation_descriptor_json <source revision> <server image digest> prints
# the schema-version-1 installation descriptor. It carries only observed public
# values and paths: no token, CA private key, database URL or socket path.
installation_descriptor_json() {
	local revision=$1 server_digest=$2 repositories
	local admin engineer reviewer operator
	repositories="$(
		IFS=',' read -r -a mappings <<<"$GIT_RESOURCES"
		for mapping in "${mappings[@]}"; do
			printf '%s\t%s\n' "${mapping%%=*}" "${mapping#*=}"
		done |
			jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {name: .[0], resource_id: .[1], binding_id: .[1]})'
	)" || return 1
	admin="$(principal_id "$OIDC_ISSUER" admin)" || return 1
	engineer="$(principal_id "$OIDC_ISSUER" engineer)" || return 1
	reviewer="$(principal_id "$OIDC_ISSUER" reviewer)" || return 1
	operator="$(principal_id "$OIDC_ISSUER" operator)" || return 1

	jq -n \
		--argjson schema_version 1 \
		--arg installation_id "$INSTALLATION_ID" \
		--arg profile "test" \
		--arg runtime "podman" \
		--arg revision "$revision" \
		--arg server_image_digest "$server_digest" \
		--arg tenant_id "$TENANT" \
		--arg project_id "$PROJECT" \
		--arg api_url "$PUBLIC_URL" \
		--arg api_ca_file "${tls_dir}/ca.crt" \
		--arg tls_server_name "localhost" \
		--arg workload_url "$WORKLOAD_URL" \
		--arg tools_image "$WORKLOAD_IMAGE" \
		--arg external_harness_image "$EXTERNAL_HARNESS_IMAGE" \
		--arg issuer "$OIDC_ISSUER" \
		--arg audience "$OIDC_AUDIENCE" \
		--arg client_id "$OIDC_CLIENT_ID" \
		--arg redirect_uri "$OIDC_REDIRECT" \
		--arg access_code_file "$access_code_file" \
		--arg admin_principal "$admin" \
		--arg engineer_principal "$engineer" \
		--arg reviewer_principal "$reviewer" \
		--arg operator_principal "$operator" \
		--arg git_url "$GIT_PUBLIC_URL" \
		--arg git_workload_url "$GIT_WORKLOAD_URL" \
		--arg git_token_file "$git_token_file" \
		--arg service_id "$SERVICE" \
		--argjson repositories "$repositories" \
		'{
			schema_version: $schema_version,
			installation_id: $installation_id,
			profile: $profile,
			runtime: $runtime,
			revision: $revision,
			server_image_digest: $server_image_digest,
			tenant_id: $tenant_id,
			project_id: $project_id,
			api: {
				url: $api_url,
				ca_file: $api_ca_file,
				tls_server_name: $tls_server_name,
				workload_url: $workload_url,
				workload_ca_file: $api_ca_file
			},
			images: {
				tools: $tools_image,
				external_harness: $external_harness_image
			},
			oauthsonas: {
				issuer: $issuer,
				audience: $audience,
				ca_file: $api_ca_file,
				client_id: $client_id,
				redirect_uri: $redirect_uri,
				resolve: "",
				access_code_file: $access_code_file,
				personas: {
					admin: {id: "admin", subject: "admin", principal_id: $admin_principal},
					engineer: {id: "engineer", subject: "engineer", principal_id: $engineer_principal},
					reviewer: {id: "reviewer", subject: "reviewer", principal_id: $reviewer_principal},
					operator: {id: "operator", subject: "operator", principal_id: $operator_principal}
				}
			},
			credentials: {
				admin_file: "",
				engineer_file: "",
				reviewer_file: "",
				operator_file: ""
			},
			git: {
				url: $git_url,
				workload_url: $git_workload_url,
				ca_file: $api_ca_file,
				workload_ca_file: $api_ca_file,
				service_id: $service_id,
				service_token_file: $git_token_file,
				admin_binding_id: "git-admin",
				admin_resource_id: "git-admin",
				repositories: $repositories
			}
		}'
}

# write_installation_descriptor <file> <revision> <server-digest> writes the
# installation descriptor only after the generator succeeded. Generating into a
# checked variable first means a failing or empty generator is a refusal, never
# an empty descriptor that reads as a successful install.
write_installation_descriptor() {
	local file=$1 revision=$2 server_digest=$3 json
	json="$(installation_descriptor_json "$revision" "$server_digest")" || return 1
	[ -n "$json" ] || return 1
	write_private_file "$file" "$json"
}

# validate_installation_descriptor <file> prints one reason and returns 1 unless
# the file is this selected installation's private descriptor with the expected
# identity, endpoints, images and persona principals.
validate_installation_descriptor() {
	local file=$1 mode
	if [ ! -f "$file" ]; then
		printf 'missing'
		return 1
	fi
	mode="$(stat -c '%a' -- "$file" 2>/dev/null || true)"
	if [ "$mode" != "600" ]; then
		printf 'mode %s, want 0600' "${mode:-unknown}"
		return 1
	fi
	if ! jq -e . "$file" >/dev/null 2>&1; then
		printf 'not valid JSON'
		return 1
	fi
	if [ "$(jq -r '.schema_version' "$file")" != "1" ]; then
		printf 'schema_version is not 1'
		return 1
	fi
	if [ "$(jq -r '.installation_id' "$file")" != "$INSTALLATION_ID" ]; then
		printf 'installation_id does not match %s' "$INSTALLATION_ID"
		return 1
	fi
	if [ "$(jq -r '.profile' "$file")" != "test" ] || [ "$(jq -r '.runtime' "$file")" != "podman" ]; then
		printf 'profile/runtime is not the local test installation'
		return 1
	fi
	if [ "$(jq -r '.tenant_id' "$file")" != "$TENANT" ] || [ "$(jq -r '.project_id' "$file")" != "$PROJECT" ]; then
		printf 'tenant/project does not match this installation'
		return 1
	fi
	if [ "$(jq -r '.api.url' "$file")" != "$PUBLIC_URL" ] ||
		[ "$(jq -r '.api.ca_file' "$file")" != "${tls_dir}/ca.crt" ] ||
		[ "$(jq -r '.api.workload_url' "$file")" != "$WORKLOAD_URL" ]; then
		printf 'api endpoint or CA does not match this installation'
		return 1
	fi
	if [ "$(jq -r '.git.url' "$file")" != "$GIT_PUBLIC_URL" ] ||
		[ "$(jq -r '.git.workload_url' "$file")" != "$GIT_WORKLOAD_URL" ] ||
		[ "$(jq -r '.git.ca_file' "$file")" != "${tls_dir}/ca.crt" ]; then
		printf 'Git HTTPS endpoint does not match this installation'
		return 1
	fi
	if [ -z "$(jq -r '.server_image_digest' "$file")" ] ||
		[ "$(jq -r '.git.repositories | length' "$file")" -lt 1 ]; then
		printf 'server digest or repository list is missing'
		return 1
	fi
	if [ "$(jq -r '.images.tools' "$file")" != "$WORKLOAD_IMAGE" ] ||
		[ "$(jq -r '.images.external_harness' "$file")" != "$EXTERNAL_HARNESS_IMAGE" ]; then
		printf 'workload image pins do not match this installation'
		return 1
	fi
	local persona
	for persona in admin engineer reviewer operator; do
		if [ "$(jq -r --arg persona "$persona" '.oauthsonas.personas[$persona].principal_id' "$file")" != "$(principal_id "$OIDC_ISSUER" "$persona")" ]; then
			printf '%s persona principal does not match the configured issuer' "$persona"
			return 1
		fi
	done
	if [ "$(jq -r '.oauthsonas.issuer' "$file")" != "$OIDC_ISSUER" ] ||
		[ "$(jq -r '.oauthsonas.client_id' "$file")" != "$OIDC_CLIENT_ID" ]; then
		printf 'OAuthSonas endpoint does not match this installation'
		return 1
	fi
	return 0
}

# add_smoke_repository <file> <name> <resource-id> <binding-id> adds the
# run-specific repository metadata to the installation descriptor after the
# smoke wrapper provisions it, atomically and still 0600.
add_smoke_repository() {
	local file=$1 name=$2 resource=$3 binding=$4
	local temporary="${file}.tmp.$$"
	(
		umask 077
		jq --arg name "$name" --arg resource "$resource" --arg binding "$binding" \
			'. + {smoke_repository: {name: $name, resource_id: $resource, binding_id: $binding}}' \
			"$file" >"$temporary"
	)
	chmod 600 -- "$temporary"
	mv -f -- "$temporary" "$file"
}

# ---- membership grants ------------------------------------------------------

# membership_version <principal> prints the committed membership version, or 0
# when the principal has none yet.
membership_version() {
	local principal=$1 out="${secrets_dir}/membership-list.$$"
	if ! membership_list "$out"; then
		local status code message
		status="$(cat -- "${out}.status" 2>/dev/null || true)"
		code="$(api_code "$out")"
		message="$(api_message "$out")"
		printf 'kodama-local: membership.list answered HTTP %s (%s: %s)\n' \
			"${status:-none}" "${code:-no code}" "${message:-no message}" >&2
		rm -f -- "$out" "${out}.status"
		return 1
	fi
	local version
	version="$(jq -r --arg principal "$principal" '[.body.memberships[]? | select(.principal_id == $principal) | .version] | first // 0' "$out")"
	rm -f -- "$out" "${out}.status"
	printf '%s' "$version"
}

# membership_list <body-out> reads the complete membership set.
membership_list() {
	local out=$1
	api_call GET "/api/v1/projects/${PROJECT}/memberships" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project}')" \
		"${PREFIX}-membership-list-$(date +%s)-$$-${RANDOM}" "$out"
}

# verify_membership <principal> <roles-json> confirms the committed role set.
verify_membership() {
	local principal=$1 expected=$2 out="${secrets_dir}/membership-verify.$$" actual wanted
	membership_list "$out" || {
		rm -f -- "$out" "${out}.status"
		return 1
	}
	actual="$(jq -c --arg principal "$principal" '[.body.memberships[]? | select(.principal_id == $principal) | .roles] | first // [] | sort' "$out")"
	rm -f -- "$out" "${out}.status"
	wanted="$(printf '%s' "$expected" | jq -c 'sort')"
	[ "$actual" = "$wanted" ]
}

# grant_persona_membership <principal> <roles-json> <label> replaces one human
# membership through the public membership.grant operation. A first grant omits
# the expected version; an existing membership uses the version observed by
# membership.list; a version conflict is retried once from a fresh observation.
grant_persona_membership() {
	local principal=$1 roles=$2 label=$3 out version attempt
	out="${secrets_dir}/membership-${label}"
	for attempt in 1 2 3; do
		version="$(membership_version "$principal")" ||
			die "membership.list failed before granting ${label}"
		if api_call PUT "/api/v1/projects/${PROJECT}/memberships/${principal}" \
			"$(jq -cn --arg project "$PROJECT" --arg principal "$principal" --argjson roles "$roles" \
				'{project_id: $project, principal_id: $principal, roles: $roles}')" \
			"${PREFIX}-membership-${label}-$(date +%s)-$$-${attempt}" "$out" "$version"; then
			verify_membership "$principal" "$roles" ||
				die "membership.list did not confirm the ${label} roles after the grant"
			rm -f -- "$out" "${out}.status"
			return 0
		fi
		if [ "$(api_code "$out")" = "version_conflict" ]; then
			continue
		fi
		die "membership.grant failed for ${label}: $(api_code "$out"): $(api_message "$out")"
	done
	die "membership.grant for ${label} kept conflicting after a fresh observation"
}

# ---- owned TLS forwarders ---------------------------------------------------

# close_inherited_fds closes every descriptor above the standard three in this
# process. A forwarder is a long-lived background process started inside a real
# infrastructure slot; without this it inherits the slot's acquisition lock and
# an orphaned forwarder keeps the slot busy after the wrapper that started it is
# gone. Standard input, output and error are left untouched.
close_inherited_fds() {
	local path fd
	for path in /proc/self/fd/*; do
		fd=${path##*/}
		case "$fd" in
		"" | *[!0-9]* | 0 | 1 | 2) continue ;;
		esac
		eval "exec ${fd}>&-" 2>/dev/null || true
	done
}

# process_matches_anchor <pid> <anchor> reports whether the live process still
# runs the exact socat LISTEN specification this installation started.
process_matches_anchor() {
	local pid=$1 anchor=$2
	case "$pid" in
	"" | *[!0-9]*) return 1 ;;
	esac
	[ -r "/proc/${pid}/cmdline" ] || return 1
	tr '\0' ' ' <"/proc/${pid}/cmdline" | grep -qF -- "$anchor"
}

# stop_forwarder <pid-file> <conf-file> <anchor> <label> terminates and joins
# only the exact process recorded for this installation, then removes its
# private files. A stale pid file alone never signals anything.
stop_forwarder() {
	local pid_file=$1 conf_file=$2 anchor=$3 label=$4 pid deadline
	if [ -s "$pid_file" ]; then
		pid="$(cat -- "$pid_file")"
		if process_matches_anchor "$pid" "$anchor"; then
			log "stopping the ${label}"
			kill "$pid" 2>/dev/null || true
			deadline=$((SECONDS + 10))
			while process_matches_anchor "$pid" "$anchor" && [ "$SECONDS" -lt "$deadline" ]; do
				sleep 0.2
			done
			if process_matches_anchor "$pid" "$anchor"; then
				kill -9 "$pid" 2>/dev/null || true
			fi
			wait "$pid" 2>/dev/null || true
		fi
	fi
	rm -f -- "$pid_file" "$conf_file"
}

# forwarder_state <pid-file> <anchor> prints running:<pid>, stale or absent.
forwarder_state() {
	local pid_file=$1 anchor=$2 pid
	if [ ! -s "$pid_file" ]; then
		printf 'absent'
		return 0
	fi
	pid="$(cat -- "$pid_file")"
	if process_matches_anchor "$pid" "$anchor"; then
		printf 'running:%s' "$pid"
	else
		printf 'stale'
	fi
}

# ---- smoke safety helpers ---------------------------------------------------

# require_fresh_state_dir <path> accepts only an absent or empty state
# directory. The disposable smoke never reuses a marker: a stopped installation
# keeps its CA, keyfile and PostgreSQL/NATS volumes there, and the smoke's own
# --purge would then delete another run's material.
require_fresh_state_dir() {
	local path=$1
	if [ ! -e "$path" ]; then
		return 0
	fi
	[ -d "$path" ] || return 1
	[ -z "$(ls -A -- "$path" 2>/dev/null)" ]
}

# smoke_require_no_podman_resources <prefix> refuses, before anything is
# installed, a prefix whose containers or PostgreSQL/NATS data volumes already
# exist. It prints what it found; the caller decides how to report it.
smoke_require_no_podman_resources() {
	local prefix=$1 existing volume found=""
	existing="$(podman ps -a --filter "label=kodama.local.prefix=${prefix}" --format '{{.Names}}' 2>/dev/null || true)"
	if [ -n "$existing" ]; then
		printf 'containers %s' "$(printf '%s' "$existing" | tr '\n' ' ')"
		return 1
	fi
	for volume in "${prefix}-pgdata" "${prefix}-natsdata"; do
		if podman volume exists "$volume" >/dev/null 2>&1; then
			found="${found:+${found} }${volume}"
		fi
	done
	if [ -n "$found" ]; then
		printf 'volumes %s' "$found"
		return 1
	fi
	return 0
}

# smoke_marker_path <state-dir> prints the sibling marker that records which run
# owns a state directory. It lives outside the state directory, so the install's
# empty-directory rule and down.sh --purge both see exactly the installation
# this run created.
smoke_marker_path() {
	printf '%s.smoke-run' "$1"
}

# write_smoke_marker <state-dir> <run> records this invocation's ownership
# before anything is installed.
write_smoke_marker() {
	local path
	path="$(smoke_marker_path "$1")"
	mkdir -p -- "$(dirname -- "$path")"
	(
		umask 077
		printf 'run=%s\n' "$2" >"$path"
	)
	chmod 600 -- "$path"
}

# smoke_cleanup_allowed <state-dir> <run> <prefix> is true only for the run that
# recorded this exact state directory and only while the directory still
# carries this run's installation marker.
smoke_cleanup_allowed() {
	local state=$1 run=$2 prefix=$3 path
	path="$(smoke_marker_path "$state")"
	[ -s "$path" ] || return 1
	[ "$(cat -- "$path" 2>/dev/null || true)" = "run=${run}" ] || return 1
	check_state_dir "$state" "$prefix" purge
}

# combine_status <test-status> <cleanup-status> prints the status the wrapper
# exits with: the test failure wins when cleanup also fails.
combine_status() {
	if [ "$1" -ne 0 ]; then
		printf '%s' "$1"
	else
		printf '%s' "$2"
	fi
}
