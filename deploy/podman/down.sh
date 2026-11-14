#!/usr/bin/env bash
# down.sh: stop and remove only the resources this installation created.
#
# By default the PostgreSQL/NATS data, the keyfile, the certificates and the
# operator environment are kept, so a later install resumes the same
# installation. --purge also deletes this installation's own state children and
# then removes the now-empty state directory; it refuses any directory whose
# marker does not name this exact prefix.
#
# Nothing here prunes the host or another run's containers: the selection is
# the explicit `kodama.local.prefix` label and this installation's own names.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

purge=0
case "${1:-}" in
-h | --help)
	cat >&2 <<'USAGE'
usage: deploy/podman/down.sh [--purge]

  --purge   also delete this installation's state directory (keys, data and TLS)
USAGE
	exit 0
	;;
--purge) purge=1 ;;
"") ;;
*)
	printf 'kodama-local: unknown argument %s\n' "$1" >&2
	exit 2
	;;
esac
[ "$#" -le 1 ] || {
	printf 'kodama-local: unexpected extra arguments\n' >&2
	exit 2
}

if [ "$purge" -eq 1 ]; then
	check_state_dir "$STATE_DIR" "$PREFIX" purge ||
		die "refusing to purge ${STATE_DIR}: its marker does not name prefix=${PREFIX} (or the directory is absent)"
fi

# The TLS front ends are host processes owned by this installation. Signal one
# only after proving the pid still runs our exact listen port.
stop_forwarder "$socat_pid_file" "$socat_conf_file" "OPENSSL-LISTEN:${OIDC_PORT}," "OAuthSonas TLS terminator"
stop_forwarder "$git_socat_pid_file" "$git_socat_conf_file" "OPENSSL-LISTEN:${GIT_HTTPS_PORT}," "Git HTTPS front end"

log "removing containers labelled kodama.local.prefix=${PREFIX}"
podman_rm_own
# Exact names cover a container created by an older revision of this bundle
# before it carried the label; they are still this installation's names.
for name in "$container_server" "$container_git" "$container_oauth" "$container_nats" "$container_postgres"; do
	if podman container exists "$name" >/dev/null 2>&1; then
		podman rm -f -- "$name" >/dev/null
	fi
done

remove_own_volume() {
	local name=$1 label
	if ! podman volume exists "$name" >/dev/null 2>&1; then
		return 0
	fi
	label="$(podman volume inspect --format '{{index .Labels "kodama.local.prefix"}}' "$name" 2>/dev/null || true)"
	[ "$label" = "$PREFIX" ] ||
		die "refusing to remove volume ${name}: it is not labelled kodama.local.prefix=${PREFIX}"
	podman volume rm -f "$name" >/dev/null
}

if [ "$purge" -eq 1 ]; then
	log "purging the installation state in ${STATE_DIR}"
	remove_own_volume "$postgres_volume"
	remove_own_volume "$nats_volume"
	# Remove only the children this bundle creates. The state directory itself
	# is removed with rmdir, so an unknown file is reported rather than deleted.
	for child in "$tls_dir" "$secrets_dir" "$versions_dir" "$data_dir" "$oauth_dir" \
		"$git_root" "$git_credential_dir" "$run_dir" "$logs_dir"; do
		if [ -e "$child" ]; then
			rm -rf -- "$child"
		fi
	done
	for file in "$image_rev_file" "$bootstrap_marker" "$project_marker" "$roles_marker" "$personas_marker" \
		"$admin_principal_file" "$bootstrap_output_file"; do
		if [ -e "$file" ]; then
			rm -f -- "$file"
		fi
	done
	# The marker goes last, and only when nothing else is left. A directory
	# that keeps unknown files also keeps its marker, so it still belongs to
	# this installation: a later install or a second purge accepts it.
	unknown="$(ls -A -- "$STATE_DIR" 2>/dev/null | grep -Fxv -- "$(basename -- "$marker_file")" || true)"
	if [ -n "$unknown" ]; then
		log "state directory ${STATE_DIR} still contains unknown files:"
		printf '%s\n' "$unknown" >&2
		die "refusing to delete unknown content in ${STATE_DIR}; move or remove these files, then run down.sh --purge again"
	fi
	rm -f -- "$marker_file"
	rmdir -- "$STATE_DIR" || die "cannot remove the state directory ${STATE_DIR}"
	log "purged"
else
	log "containers removed; state kept in ${STATE_DIR}"
fi

remaining="$(podman ps -a --filter "label=kodama.local.prefix=${PREFIX}" --format '{{.Names}}' 2>/dev/null || true)"
if [ -n "$remaining" ]; then
	die "resources with prefix ${PREFIX} remain: ${remaining}"
fi
if [ "$purge" -eq 1 ]; then
	remaining_volumes="$(podman volume ls -q --filter "label=kodama.local.prefix=${PREFIX}" 2>/dev/null || true)"
	if [ -n "$remaining_volumes" ]; then
		die "volumes with prefix ${PREFIX} remain: ${remaining_volumes}"
	fi
	[ ! -e "$STATE_DIR" ] || die "state directory ${STATE_DIR} still exists"
fi
log "no resource with prefix ${PREFIX} remains"
