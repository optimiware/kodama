#!/usr/bin/env bash
# run-installation.sh: run the e2e suite against any documented Kodama
# installation.
#
#   scripts/infra-slot.sh stack -- \
#     env Q01_REQUIRE_INSTALLATION=1 \
#         Q01_INSTALLATION_FILE=/absolute/installation.json \
#     test/e2e/internal/harness/run-installation.sh -- \
#     go test -race -tags=e2e -count=1 -timeout=30m ./test/e2e/
#
# A wrapper may instead append "-- env VAR=value ... command"; those
# assignments are applied to this runner before the command runs.
#
# The runner connects to an existing installation. It never installs, never
# calls Podman or kubectl, and never destroys the supplied installation. When a
# descriptor names OAuthSonas personas it acquires each requested persona token
# with scripts/oauthsonas-token under umask 077 directly into a private file,
# exposes only the file path to the command, and removes the files it owns on
# exit after joining its child. A caller-supplied file is never modified or
# removed.
#
# Inputs:
#   Q01_INSTALLATION_FILE   optional schema version 1 installation descriptor
#   Q01_TOKEN_SCRIPT        token script (default scripts/oauthsonas-token)
#   Q01_ACQUIRE_PERSONAS    optional space/comma list; default: descriptor personas
#   Q01_RUN_SECONDS         expected selected-run duration (default 600)
#   Q01_TOKEN_MIN_LIFETIME  seconds an acquired token must remain valid
#                           (default: Q01_RUN_SECONDS; use 0 only for diagnosis)
#   Q01_EVIDENCE_DIR        optional absolute evidence directory
#   Q01_TOKEN               optional legacy single basic-test token passed through
#
# The caller's exported Q01_TOKEN is passed through for the legacy seven tests;
# per-persona acquisition never overwrites it.
set -euo pipefail
set +x

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$here/../../../.." && pwd -P)"

die() {
	printf 'run-installation: %s\n' "$*" >&2
	exit 1
}

if [ "${1:-}" = "--" ]; then
	shift
fi
if [ "$#" -eq 0 ]; then
	die "usage: run-installation.sh -- <command> [args...]"
fi

# Accept the wrapper form "-- env VAR=value ... command" by applying the
# assignments to this runner and re-executing the documented form. Without this
# a wrapper that passes its settings only to the child would silently skip
# descriptor validation and persona acquisition.
if [ "${1:-}" = "env" ]; then
	env_arguments=("$@")
	env_index=1
	environment_assignments=()
	while [ "$env_index" -lt "${#env_arguments[@]}" ]; do
		argument="${env_arguments[$env_index]}"
		case "$argument" in
		[A-Za-z_][A-Za-z0-9_]*=*)
			environment_assignments+=("$argument")
			env_index=$((env_index + 1))
			;;
		*)
			break
			;;
		esac
	done
	# Only re-exec when the leading words are plain assignments and a command
	# follows. `env -i ...` or `env -u X ...` keep their env command and
	# options untouched, so the runner never turns them into an unknown
	# command.
	if [ "${#environment_assignments[@]}" -gt 0 ] && [ "$env_index" -lt "${#env_arguments[@]}" ]; then
		command_word="${env_arguments[$env_index]}"
		case "$command_word" in
		-*) ;;
		*)
			exec env "${environment_assignments[@]}" "$0" -- "${env_arguments[@]:$env_index}"
			;;
		esac
	fi
fi

installation_file="${Q01_INSTALLATION_FILE:-}"
token_script="${Q01_TOKEN_SCRIPT:-$repo_root/scripts/oauthsonas-token}"
run_seconds="${Q01_RUN_SECONDS:-600}"
min_lifetime="${Q01_TOKEN_MIN_LIFETIME:-$run_seconds}"

[[ "$run_seconds" =~ ^[0-9]+$ ]] || die "Q01_RUN_SECONDS must be a whole number of seconds"
[[ "$min_lifetime" =~ ^[0-9]+$ ]] || die "Q01_TOKEN_MIN_LIFETIME must be a whole number of seconds"

desc_personas=""
desc_oauth_issuer=""
desc_oauth_ca=""
desc_oauth_client=""
desc_oauth_redirect=""
desc_oauth_resolve=""
desc_oauth_access=""
desc_cred_admin=""
desc_cred_engineer=""
desc_cred_reviewer=""
desc_cred_operator=""

if [ -n "$installation_file" ]; then
	[[ -f "$installation_file" ]] || die "Q01_INSTALLATION_FILE is not a readable file"
	[[ -r "$installation_file" ]] || die "Q01_INSTALLATION_FILE is not readable"
	command -v python3 >/dev/null 2>&1 || die "python3 is required to read the descriptor"
	if ! parsed="$(python3 - "$installation_file" <<'PY'
import json
import shlex
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
except Exception as error:
    print("run-installation: descriptor is not valid JSON: %s" % error, file=sys.stderr)
    sys.exit(1)
if not isinstance(data, dict) or data.get("schema_version") != 1:
    print("run-installation: descriptor schema_version must be 1", file=sys.stderr)
    sys.exit(1)

def field(*keys):
    value = data
    for key in keys:
        if not isinstance(value, dict):
            return ""
        value = value.get(key, "")
    return value if isinstance(value, str) else ""

oauth = data.get("oauthsonas") if isinstance(data.get("oauthsonas"), dict) else {}
credentials = data.get("credentials") if isinstance(data.get("credentials"), dict) else {}
personas = oauth.get("personas") if isinstance(oauth.get("personas"), dict) else {}
values = {
    "desc_personas": ",".join(sorted(personas)),
    "desc_oauth_issuer": field("oauthsonas", "issuer"),
    "desc_oauth_ca": field("oauthsonas", "ca_file"),
    "desc_oauth_client": field("oauthsonas", "client_id"),
    "desc_oauth_redirect": field("oauthsonas", "redirect_uri"),
    "desc_oauth_resolve": field("oauthsonas", "resolve"),
    "desc_oauth_access": field("oauthsonas", "access_code_file"),
    "desc_cred_admin": field("credentials", "admin_file"),
    "desc_cred_engineer": field("credentials", "engineer_file"),
    "desc_cred_reviewer": field("credentials", "reviewer_file"),
    "desc_cred_operator": field("credentials", "operator_file"),
}
for name, value in values.items():
    print("%s=%s" % (name, shlex.quote(value)))
PY
	)"; then
		exit 1
	fi
	eval "$parsed"
fi

# A legacy value and a file for the same admin credential are ambiguous.
if [ -n "${Q01_TOKEN:-}" ]; then
	if [ -n "${Q01_ADMIN_TOKEN_FILE:-}" ] || [ -n "$desc_cred_admin" ]; then
		die "Q01_TOKEN and an admin token file are ambiguous; configure exactly one"
	fi
fi

persona_file_variable() {
	case "$1" in
	admin) printf '%s' "Q01_ADMIN_TOKEN_FILE" ;;
	engineer) printf '%s' "Q01_ENGINEER_TOKEN_FILE" ;;
	reviewer) printf '%s' "Q01_REVIEWER_TOKEN_FILE" ;;
	operator) printf '%s' "Q01_OPERATOR_TOKEN_FILE" ;;
	*) return 1 ;;
	esac
}

persona_descriptor_file() {
	case "$1" in
	admin) printf '%s' "$desc_cred_admin" ;;
	engineer) printf '%s' "$desc_cred_engineer" ;;
	reviewer) printf '%s' "$desc_cred_reviewer" ;;
	operator) printf '%s' "$desc_cred_operator" ;;
	*) return 1 ;;
	esac
}

acquire_personas="${Q01_ACQUIRE_PERSONAS:-}"
if [ -n "$acquire_personas" ]; then
	acquire_personas="${acquire_personas//,/ }"
elif [ -n "$installation_file" ]; then
	acquire_personas="${desc_personas//,/ }"
fi

umask 077
# A SIGKILL of this runner cannot run the EXIT trap, so in that one case the
# private 0700 work directory below (token files mode 0600) can outlive the
# process. It stays under the launcher's short TMPDIR and the next slot run
# removes leftover q01-installation.* directories; nothing is written outside.
work="$(mktemp -d "${TMPDIR:-/tmp}/q01-installation.XXXXXX")"
owned_files=()
child_pid=""

# group_alive reports whether any process remains in the background command's
# process group. The command may exit on the first TERM while a TERM-ignoring
# helper survives, so leader liveness alone is not enough.
group_alive() {
	[ -n "$child_pid" ] && kill -0 -- "-$child_pid" 2>/dev/null
}

cleanup() {
	local status=$?
	set +e
	trap - EXIT INT TERM HUP
	if group_alive; then
		# set -m makes the background command a process-group leader whose pid
		# is the group id, so the command and its children all receive the
		# signal; the pid is known at start, with no ps race.
		kill -TERM -- "-$child_pid" 2>/dev/null || kill -TERM "$child_pid" 2>/dev/null
		local attempt
		for ((attempt = 0; attempt < 20; attempt++)); do
			group_alive || break
			sleep 0.1
		done
		if group_alive; then
			kill -KILL -- "-$child_pid" 2>/dev/null || kill -KILL "$child_pid" 2>/dev/null
		fi
		wait "$child_pid" 2>/dev/null
	fi
	local file
	for file in ${owned_files[@]+"${owned_files[@]}"}; do
		rm -f -- "$file"
	done
	rm -rf -- "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

require_oauth_inputs() {
	[[ -n "$desc_oauth_issuer" ]] || die "the descriptor has no oauthsonas.issuer; supply a persona token file"
	[[ -n "$desc_oauth_ca" && -f "$desc_oauth_ca" ]] || die "the descriptor has no readable oauthsonas.ca_file"
	[[ -n "$desc_oauth_access" && -f "$desc_oauth_access" ]] || die "the descriptor has no readable oauthsonas.access_code_file"
	[[ -f "$token_script" ]] || die "the OAuthSonas token script is missing"
}

check_token_lifetime() {
	local persona=$1 file=$2
	if ! python3 - "$file" "$min_lifetime" <<'PY'
import base64
import json
import sys
import time

raw = open(sys.argv[1], "r", encoding="utf-8").read().strip()
minimum = int(sys.argv[2])
parts = raw.split(".")
if len(parts) != 3:
    # An opaque token carries no readable expiry; the installation decides.
    sys.exit(0)
payload = parts[1]
payload += "=" * (-len(payload) % 4)
try:
    claims = json.loads(base64.urlsafe_b64decode(payload.encode("ascii")))
except Exception:
    sys.exit(0)
expiry = claims.get("exp")
if not isinstance(expiry, int):
    sys.exit(0)
if expiry < int(time.time()) + minimum:
    sys.exit(1)
PY
	then
		die "the acquired ${persona} token is expired or expires inside the bounded run"
	fi
}

for persona in $acquire_personas; do
	variable="$(persona_file_variable "$persona")" || die "unknown persona '${persona}'"
	descriptor_file="$(persona_descriptor_file "$persona")"
	if [ -n "${!variable:-}" ]; then
		continue
	fi
	if [ -n "$descriptor_file" ]; then
		[[ -f "$descriptor_file" && -r "$descriptor_file" ]] || die "the descriptor ${persona} credential file is unreadable"
		export "$variable=$descriptor_file"
		continue
	fi
	if [ "$persona" = "admin" ] && [ -n "${Q01_TOKEN:-}" ]; then
		continue
	fi
	require_oauth_inputs
	token_file="$work/${persona}.token"
	owned_files+=("$token_file")
	oauth_environment=(
		"OAUTHSONAS_URL=$desc_oauth_issuer"
		"OAUTHSONAS_CA=$desc_oauth_ca"
		"OAUTHSONAS_ACCESS_CODE_FILE=$desc_oauth_access"
	)
	if [ -n "$desc_oauth_client" ]; then
		oauth_environment+=("OAUTHSONAS_CLIENT_ID=$desc_oauth_client")
	fi
	if [ -n "$desc_oauth_redirect" ]; then
		oauth_environment+=("OAUTHSONAS_REDIRECT=$desc_oauth_redirect")
	fi
	if [ -n "$desc_oauth_resolve" ]; then
		oauth_environment+=("OAUTHSONAS_RESOLVE=$desc_oauth_resolve")
	fi
	# The acquisition child never inherits the caller's legacy token.
	if ! (
		unset Q01_TOKEN Q01_ADMIN_TOKEN_FILE Q01_ENGINEER_TOKEN_FILE Q01_REVIEWER_TOKEN_FILE Q01_OPERATOR_TOKEN_FILE
		env "${oauth_environment[@]}" "$token_script" "$persona"
	) >"$token_file" 2>"$work/${persona}.log"; then
		die "token acquisition failed for persona ${persona}"
	fi
	chmod 600 "$token_file"
	[[ -s "$token_file" ]] || die "token acquisition returned an empty token for persona ${persona}"
	check_token_lifetime "$persona" "$token_file"
	export "$variable=$token_file"
done

if [ -z "${Q01_EVIDENCE_DIR:-}" ]; then
	evidence="${TMPDIR:-/tmp}/oc/q01-evidence/$(date -u +%Y%m%dT%H%M%SZ)"
	mkdir -p -- "$evidence"
	chmod 700 -- "$evidence" 2>/dev/null || true
	export Q01_EVIDENCE_DIR="$evidence"
fi

printf 'run-installation: evidence directory %s\n' "$Q01_EVIDENCE_DIR" >&2

# Start the command in its own process group so a signal reaches go test and
# its test binary, not only the immediate shell.
set -m
"$@" &
child_pid=$!
set +m
status=0
wait "$child_pid" || status=$?
child_pid=""
exit "$status"
