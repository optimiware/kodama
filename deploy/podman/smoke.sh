#!/usr/bin/env bash
# smoke.sh: disposable local orchestration for the release smoke.
#
# It owns one throwaway installation: it requires an infra slot and a complete,
# distinct port set, derives a run-unique prefix and state directory itself,
# installs the bundle; provisions a run-unique repository with the installed
# `kodama git provision-repository`; registers its Shell binding; re-runs the
# install so `git serve` maps the new repository (keeping every existing
# mapping); proves the installation access prerequisites; and only then invokes
# the common installation runner
# (test/e2e/internal/harness/run-installation.sh). Teardown removes only the
# installation this invocation recorded.
#
#   KODAMA_SLOT=stack-1 KODAMA_LOCAL_HTTPS_PORT=... ... deploy/podman/smoke.sh [--preflight]
#
# Identity is never inherited: an exported KODAMA_LOCAL_PREFIX,
# KODAMA_LOCAL_STATE_DIR or KODAMA_LOCAL_GIT_RESOURCES (for example from a
# sourced secrets/operator.env) is cleared before lib.sh resolves them, and only
# KODAMA_SMOKE_PREFIX/KODAMA_SMOKE_STATE_DIR override the run-unique defaults.
# A stopped real installation keeps its CA, keyfile and volumes in its own state
# directory, so the run refuses a non-empty state directory and an existing
# prefix container or data volume before it installs.
#
# --preflight proves installation access (descriptor, four personas with real
# memberships, host and workload Git/API TLS including an unrelated-CA refusal)
# and then invokes the runner's TestE2E_InstallationAccess selection. The
# default mode invokes the full selected release tests. Both modes go through
# test/e2e/internal/harness/verify-go-test.sh with the exact expected list, so a
# zero-test selection, a removed test, a skipped subtest or a failure fails the
# run; --print-selection prints that selection without installing anything. The
# runner and the verifier are required: without them this script fails with an
# explicit message.
#
# Every port is required from the caller (a parallel writer must choose a
# complete distinct set). This script never sets TMPDIR.
set -euo pipefail

bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${bundle_dir}/../.." && pwd)"

smoke_fail() {
	printf 'kodama-smoke: %s\n' "$*" >&2
	exit 2
}

usage() {
	cat >&2 <<'USAGE'
usage: deploy/podman/smoke.sh [--preflight] [--print-selection]

Runs one disposable local installation. Requires KODAMA_SLOT (set by
scripts/infra-slot.sh) and the complete port set:
  KODAMA_LOCAL_HTTPS_PORT KODAMA_LOCAL_OIDC_PORT KODAMA_LOCAL_GIT_PORT
  KODAMA_LOCAL_GIT_HTTPS_PORT KODAMA_LOCAL_POSTGRES_PORT
  KODAMA_LOCAL_NATS_PORT KODAMA_LOCAL_NATS_MONITOR_PORT
  KODAMA_LOCAL_OAUTH_UPSTREAM_PORT

Identity is invocation-owned and never inherited. The prefix defaults to
smoke-<run> and the state directory to /tmp/oc/smoke/state-<run>; override them
only with KODAMA_SMOKE_PREFIX and KODAMA_SMOKE_STATE_DIR. Every inherited
KODAMA_LOCAL_* identity or endpoint setting is ignored (for example
KODAMA_LOCAL_PREFIX, KODAMA_LOCAL_STATE_DIR, KODAMA_LOCAL_GIT_RESOURCES,
KODAMA_LOCAL_TENANT, KODAMA_LOCAL_PROJECT, the service ids and the URL/OIDC
overrides, as exported by a sourced secrets/operator.env). The state directory
must be absent or empty, and the run refuses an existing container or the
<prefix>-pgdata/<prefix>-natsdata volumes before it installs. It purges only the
installation it recorded itself.

Optional: KODAMA_SMOKE_RUN, KODAMA_SMOKE_PREFIX, KODAMA_SMOKE_STATE_DIR,
Q01_EVIDENCE_DIR, KODAMA_SMOKE_RUNNER, KODAMA_SMOKE_TEST_RUN,
KODAMA_SMOKE_EXPECT, KODAMA_SMOKE_SHARD_REPOS.

KODAMA_SMOKE_SHARD_REPOS (0..8, default 0) provisions that many extra
repositories, so a run that selects several protected-Git proofs gives each
its own repository instead of making them take turns on one.

--print-selection prints the exact release selection (one argument per line)
without installing anything; it is diagnostics only, never release evidence.
USAGE
}

preflight=0
print_selection=0
for argument in "$@"; do
	case "$argument" in
	-h | --help)
		usage
		exit 0
		;;
	--preflight) preflight=1 ;;
	--print-selection) print_selection=1 ;;
	*)
		usage
		exit 2
		;;
	esac
done
[ "$#" -le 2 ] || {
	usage
	exit 2
}

required_ports=(
	KODAMA_LOCAL_HTTPS_PORT
	KODAMA_LOCAL_OIDC_PORT
	KODAMA_LOCAL_GIT_PORT
	KODAMA_LOCAL_GIT_HTTPS_PORT
	KODAMA_LOCAL_POSTGRES_PORT
	KODAMA_LOCAL_NATS_PORT
	KODAMA_LOCAL_NATS_MONITOR_PORT
	KODAMA_LOCAL_OAUTH_UPSTREAM_PORT
)

run="${KODAMA_SMOKE_RUN:-$(date -u +%Y%m%d%H%M%S)-$$-${RANDOM}}"
case "$run" in
"" | *[!a-z0-9-]*) smoke_fail "KODAMA_SMOKE_RUN must be lower-case letters, digits and '-' (it names containers and a repository): ${run}" ;;
esac
shard_repositories="${KODAMA_SMOKE_SHARD_REPOS:-0}"
case "$shard_repositories" in
[0-8]) ;;
*) smoke_fail "KODAMA_SMOKE_SHARD_REPOS must be a number from 0 to 8: ${shard_repositories}" ;;
esac

# Identity is invocation-owned: clear every inherited KODAMA_LOCAL_* identity
# setting before lib.sh resolves its defaults, so a sourced secrets/operator.env
# (which exports KODAMA_LOCAL_PREFIX, KODAMA_LOCAL_STATE_DIR, the tenant and the
# Git resources) cannot steer this run into another installation's prefix,
# state, containers, volumes or endpoints. Ports stay caller-supplied: the
# complete KODAMA_LOCAL_*_PORT set above is required and is never derived from
# the run.
for name in \
	KODAMA_LOCAL_PREFIX KODAMA_LOCAL_STATE_DIR KODAMA_LOCAL_GIT_RESOURCES \
	KODAMA_LOCAL_INSTALLATION_ID KODAMA_LOCAL_TENANT KODAMA_LOCAL_PROJECT \
	KODAMA_LOCAL_GIT_SERVICE KODAMA_LOCAL_ENGINEER_SERVICE KODAMA_LOCAL_DATABASE \
	KODAMA_LOCAL_HOST_ALIAS KODAMA_LOCAL_PUBLIC_URL KODAMA_LOCAL_WORKLOAD_URL \
	KODAMA_LOCAL_GIT_PUBLIC_URL KODAMA_LOCAL_GIT_WORKLOAD_URL \
	KODAMA_LOCAL_OIDC_ISSUER KODAMA_LOCAL_OIDC_AUDIENCE \
	KODAMA_LOCAL_OIDC_REDIRECT KODAMA_LOCAL_OIDC_CLIENT_ID; do
	unset "$name"
done
export KODAMA_LOCAL_PREFIX="${KODAMA_SMOKE_PREFIX:-smoke-${run}}"
export KODAMA_LOCAL_STATE_DIR="${KODAMA_SMOKE_STATE_DIR:-/tmp/oc/smoke/state-${run}}"
export Q01_EVIDENCE_DIR="${Q01_EVIDENCE_DIR:-/tmp/oc/evidence/smoke-${run}}"

# A narrowed selection is a diagnostic, never release evidence. Record it in
# the summary and announce it loudly at the start and the end of the run.
selection=full
if [ "$preflight" -eq 1 ]; then
	selection=preflight
elif [ -n "${KODAMA_SMOKE_TEST_RUN:-}" ] || [ -n "${KODAMA_SMOKE_EXPECT:-}" ]; then
	selection=narrowed
fi
if [ "$selection" = narrowed ]; then
	printf 'kodama-smoke: NARROWED SELECTION - NOT RELEASE EVIDENCE\n' >&2
fi

# shellcheck source=lib.sh
. "${bundle_dir}/lib.sh"

repository="smoke/${run}"
resource="w49-repo-${run}"
runner="${KODAMA_SMOKE_RUNNER:-${repo_root}/test/e2e/internal/harness/run-installation.sh}"
verifier="${repo_root}/test/e2e/internal/harness/verify-go-test.sh"

# The release selection, shared by --print-selection and the real invocation so
# the two can never drift. Every mode goes through verify-go-test.sh with an
# explicit expected-name list: a zero-test selection, a renamed test, a skipped
# subtest or a failure fails the run, and a '^$' KODAMA_SMOKE_TEST_RUN can never
# pass as release evidence.
expected=(
	TestE2E_AC01InstalledPipeline
	TestE2E_AC02ImmutableAdmission
	TestE2E_AC09CleanupAttributable
	TestE2E_AC11Materialization
	TestE2E_AC12Outputs
	TestE2E_AC13OrderedActions
	TestE2E_AC16PlannedWait
	TestReleaseSmoke/RequestWait
	TestReleaseSmoke/OperatorRetry
	TestReleaseSmoke/ConversationHandoff
	TestReleaseSmoke/ProtectedTask
	TestReleaseSmoke/HumanPush
)
if [ -n "${KODAMA_SMOKE_EXPECT:-}" ]; then
	read -r -a expected <<<"$KODAMA_SMOKE_EXPECT"
fi

selection_arguments() {
	local expect_args=() name
	if [ "$selection" = preflight ]; then
		printf '%s\n' bash "$runner" -- env \
			KODAMA_REQUIRE_INFRA=1 Q01_REQUIRE_INSTALLATION=1 \
			Q01_INSTALLATION_FILE="$installation_file" \
			Q01_EVIDENCE_DIR="$Q01_EVIDENCE_DIR" \
			"$verifier" --expect TestE2E_InstallationAccess -- \
			go test -count=1 -tags=e2e -timeout=10m -json \
			-run '^TestE2E_InstallationAccess$' ./test/e2e/
		return 0
	fi
	for name in "${expected[@]}"; do
		expect_args+=(--expect "$name")
	done
	printf '%s\n' bash "$runner" -- env \
		KODAMA_REQUIRE_INFRA=1 Q01_REQUIRE_INSTALLATION=1 \
		Q01_INSTALLATION_FILE="$installation_file" \
		Q01_EVIDENCE_DIR="$Q01_EVIDENCE_DIR" \
		"$verifier" "${expect_args[@]}" -- \
		go test -race -tags=e2e -count=1 -timeout=30m -json \
		-run "${KODAMA_SMOKE_TEST_RUN:-^(TestE2E_AC(01|02|09|11|12|13|16)|TestReleaseSmoke)}" ./test/e2e/
}

if [ "$print_selection" -eq 1 ]; then
	printf 'selection=%s\n' "$selection"
	selection_arguments
	exit 0
fi

[ -n "${KODAMA_SLOT:-}" ] ||
	smoke_fail "KODAMA_SLOT must be set by scripts/infra-slot.sh; smoke.sh runs only inside a real-infrastructure slot"
for name in "${required_ports[@]}"; do
	[ -n "${!name:-}" ] ||
		smoke_fail "${name} must be set: smoke.sh needs the complete distinct port set for its disposable installation"
done

require_command podman
require_command jq
require_command curl
require_command git

require_fresh_state_dir "$STATE_DIR" ||
	die "refusing state directory ${STATE_DIR}: it is not empty; a disposable smoke never reuses an installation"
existing="$(smoke_require_no_podman_resources "$PREFIX")" ||
	die "refusing prefix ${PREFIX}: ${existing} already exist; the disposable run never installs over another installation"
ensure_dir "$Q01_EVIDENCE_DIR" 700
token_dir="$(mktemp -d "${TMPDIR:-/tmp}/kodama-smoke-${run}.XXXXXX")"
chmod 700 "$token_dir"

install_attempted=0
cleanup_done=0
cleanup_status=0
cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [ "$cleanup_done" -eq 0 ]; then
		cleanup_done=1
		if [ "$install_attempted" -eq 1 ]; then
			if smoke_cleanup_allowed "$STATE_DIR" "$run" "$PREFIX"; then
				if ! "${bundle_dir}/down.sh" --purge >"${Q01_EVIDENCE_DIR}/teardown.log" 2>&1; then
					cleanup_status=1
					printf 'kodama-smoke: teardown failed; state kept at %s (see %s/teardown.log)\n' \
						"$STATE_DIR" "$Q01_EVIDENCE_DIR" >&2
				else
					rm -f -- "$(smoke_marker_path "$STATE_DIR")"
				fi
			else
				printf 'kodama-smoke: leaving %s untouched: this run recorded no installation there\n' \
					"$STATE_DIR" >&2
				rm -f -- "$(smoke_marker_path "$STATE_DIR")"
			fi
		fi
		rm -rf -- "$token_dir"
	fi
	local final
	final="$(combine_status "$status" "$cleanup_status")"
	if [ "$status" -ne 0 ] && [ "$cleanup_status" -ne 0 ]; then
		printf 'kodama-smoke: the run failed (%s) and cleanup also failed; preserving status %s\n' "$status" "$status" >&2
	fi
	exit "$final"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

KODAMA_IMAGE="${KODAMA_LOCAL_KODAMA_IMAGE:-localhost/${PREFIX}/kodama:${KODAMA_IMAGE_TAG:-local}}"

run_install() {
	"${bundle_dir}/install.sh"
}

validate_descriptor() {
	[ -f "$installation_file" ] || die "the install did not write ${installation_file}"
	[ "$(stat -c '%a' -- "$installation_file" 2>/dev/null || true)" = "600" ] ||
		die "the installation descriptor ${installation_file} is not mode 0600"
	local reason
	reason="$(validate_installation_descriptor "$installation_file")" ||
		die "the installation descriptor is invalid: ${reason}"
	printf 'kodama-smoke: installation descriptor valid (mode 0600): %s\n' "$installation_file"
}

write_admin_header() {
	(
		umask 077
		printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$(cat -- "$admin_token_file")"
	) >"${token_dir}/admin-header"
	chmod 600 "${token_dir}/admin-header"
}

# api_call issues one public request with the installation administrator token.
api_call() {
	local method=$1 path=$2 input=$3 key=$4 body_out=$5 expected=${6:-0}
	local request="${token_dir}/request.$$" response="${token_dir}/response.$$" status
	jq -cn --argjson input "$input" --arg key "$key" --argjson expected "$expected" \
		'if $expected > 0 then {input: $input, expected_version: $expected, idempotency_key: $key} else {input: $input, idempotency_key: $key} end' \
		>"$request"
	status="$(curl --silent --show-error --max-time 30 --cacert "$tls_dir/ca.crt" \
		--request "$method" --header @"${token_dir}/admin-header" --data-binary @"$request" \
		--output "$response" --write-out '%{http_code}' "${PUBLIC_URL}${path}" 2>/dev/null || true)"
	rm -f -- "$request"
	[ -f "$response" ] || : >"$response"
	printf '%s' "$status" >"${body_out}.status"
	mv -f -- "$response" "$body_out"
	[ "$status" = "200" ]
}

# request_status issues one request with one persona token and prints the status.
request_status() {
	local token_file=$1 method=$2 path=$3 input=$4 output=$5
	local header="${token_dir}/header.$$" request="${token_dir}/request.$$" status
	(
		umask 077
		printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$(cat -- "$token_file")"
	) >"$header"
	jq -cn --argjson input "$input" '{input: $input}' >"$request"
	status="$(curl --silent --show-error --max-time 30 --cacert "$tls_dir/ca.crt" \
		--request "$method" --header @"$header" --data-binary @"$request" \
		--output "$output" --write-out '%{http_code}' "${PUBLIC_URL}${path}" 2>/dev/null || true)"
	rm -f -- "$header" "$request"
	printf '%s' "$status"
}

acquire_token() {
	local persona=$1 file="${token_dir}/${persona}-token"
	(
		umask 077
		OAUTHSONAS_URL="$OIDC_ISSUER" \
			OAUTHSONAS_CA="${tls_dir}/ca.crt" \
			OAUTHSONAS_CLIENT_ID="$OIDC_CLIENT_ID" \
			OAUTHSONAS_REDIRECT="$OIDC_REDIRECT" \
			OAUTHSONAS_ACCESS_CODE_FILE="$access_code_file" \
			"${repo_root}/scripts/oauthsonas-token" "$persona" >"$file"
	)
	chmod 600 "$file"
}

# expect_status asserts one public operation status for one persona.
expect_status() {
	local persona=$1 method=$2 path=$3 input=$4 expected=$5 out="${token_dir}/probe.$$"
	local status
	status="$(request_status "${token_dir}/${persona}-token" "$method" "$path" "$input" "$out")"
	rm -f -- "$out"
	[ "$status" = "$expected" ] ||
		die "${persona} ${method} ${path} answered HTTP ${status}; want ${expected}"
	printf 'kodama-smoke: %s %s %s -> HTTP %s\n' "$persona" "$method" "$path" "$status"
}

# persona_access_checks proves each acquired persona token carries a real
# distinct membership: one role-appropriate operation succeeds and one the role
# lacks is refused. The installed server is project-scoped, so membership.list
# itself is tenant-scoped and stays an install-time verification (it ran on the
# tenant-wide setup server).
persona_access_checks() {
	expect_status admin GET "/api/v1/projects/${PROJECT}/executions" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project}')" 200
	expect_status engineer GET "/api/v1/projects/${PROJECT}/executions" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project}')" 200
	expect_status reviewer GET "/api/v1/projects/${PROJECT}/inbox" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project, kinds: ["request"]}')" 200
	expect_status operator GET "/api/v1/projects/${PROJECT}/inbox" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project, kinds: ["failed_task"]}')" 200
	# The reviewer has no execution-read role.
	expect_status reviewer GET "/api/v1/projects/${PROJECT}/executions" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project}')" 403
	# A reviewer cannot start a pipeline; a missing pipeline keeps the refusal
	# side-effect free even if a defect admitted it.
	expect_status reviewer POST "/api/v1/projects/${PROJECT}/pipelines/smoke-denied-${run}/start" \
		"$(jq -cn --arg project "$PROJECT" '{project_id: $project, pipeline_id: "smoke-denied-'"${run}"'"}')" 403
	# An engineer cannot create projects.
	expect_status engineer POST "/api/v1/tenants/${TENANT}/projects" \
		"$(jq -cn --arg tenant "$TENANT" '{tenant_id: $tenant, project_id: "smoke-denied-'"${run}"'", name: "smoke denied"}')" 403
	# An operator cannot create projects.
	expect_status operator POST "/api/v1/tenants/${TENANT}/projects" \
		"$(jq -cn --arg tenant "$TENANT" '{tenant_id: $tenant, project_id: "smoke-denied-'"${run}"'", name: "smoke denied"}')" 403
}

host_git_read() {
	local home="${token_dir}/git-home"
	ensure_dir "$home" 700
	# shellcheck disable=SC2016
	cat >"${token_dir}/askpass.sh" <<'ASKPASS'
#!/bin/sh
case "$1" in
*[Uu]sername*) printf 'kodama\n' ;;
*) cat -- "${KODAMA_GIT_TOKEN_FILE:?}" ;;
esac
ASKPASS
	chmod 700 "${token_dir}/askpass.sh"
	GIT_TERMINAL_PROMPT=0 GIT_ASKPASS="${token_dir}/askpass.sh" GIT_ASKPASS_REQUIRE=force \
		KODAMA_GIT_TOKEN_FILE="${token_dir}/admin-token" \
		GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null HOME="$home" \
		git -c credential.helper= -c http.sslVerify=true -c http.sslCAInfo="${tls_dir}/ca.crt" \
		-c http.followRedirects=false ls-remote "${GIT_PUBLIC_URL}/admin" >/dev/null ||
		die "host Git could not read ${GIT_PUBLIC_URL}/admin with the installation CA"
	GIT_TERMINAL_PROMPT=0 GIT_ASKPASS="${token_dir}/askpass.sh" GIT_ASKPASS_REQUIRE=force \
		KODAMA_GIT_TOKEN_FILE="${token_dir}/admin-token" \
		GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null HOME="$home" \
		git -c credential.helper= -c http.sslVerify=true -c http.sslCAInfo="${tls_dir}/ca.crt" \
		-c http.followRedirects=false ls-remote "${GIT_PUBLIC_URL}/${repository}" >/dev/null ||
		die "host Git could not read the provisioned repository ${GIT_PUBLIC_URL}/${repository} with the installation CA"
	printf 'kodama-smoke: host Git read %s/admin and the provisioned %s over HTTPS\n' "$GIT_PUBLIC_URL" "$repository"
}

workload_probe() {
	podman run --rm --label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.command=smoke-probe" \
		-v "${tls_dir}/ca.crt:/run/kodama/ca.crt:ro" \
		-v "${token_dir}/admin-token:/run/kodama/token:ro" \
		-e API_WORKLOAD_URL="$WORKLOAD_URL" \
		-e GIT_WORKLOAD_URL="$GIT_WORKLOAD_URL" \
		-e SMOKE_REPOSITORY="$repository" \
		-e GIT_TERMINAL_PROMPT=0 \
		"$WORKLOAD_IMAGE" sh -eu -c '
git -c http.sslCAInfo=/run/kodama/ca.crt -c http.followRedirects=false \
    -c credential.helper="!f() { printf \"username=kodama\\npassword=\"; cat /run/kodama/token; printf \"\\n\"; }; f" \
    ls-remote "$GIT_WORKLOAD_URL/admin" >/dev/null
git -c http.sslCAInfo=/run/kodama/ca.crt -c http.followRedirects=false \
    -c credential.helper="!f() { printf \"username=kodama\\npassword=\"; cat /run/kodama/token; printf \"\\n\"; }; f" \
    ls-remote "$GIT_WORKLOAD_URL/$SMOKE_REPOSITORY" >/dev/null
curl --silent --show-error --fail --max-time 20 --cacert /run/kodama/ca.crt "$API_WORKLOAD_URL/readyz" >/dev/null
' || die "a workload could not read Git or the API through the workload endpoints with the installation CA"
	printf 'kodama-smoke: workload read %s/admin and the provisioned %s, and %s/readyz, with the declared CA\n' "$GIT_WORKLOAD_URL" "$repository" "$WORKLOAD_URL"
}

unrelated_ca_refusal() {
	if curl --silent --show-error --max-time 10 --cacert /etc/ssl/certs/ca-certificates.crt \
		--output /dev/null "${PUBLIC_URL}/readyz" 2>/dev/null; then
		die "the API accepted an unrelated public CA"
	fi
	podman run --rm --label "kodama.local.prefix=${PREFIX}" \
		--label "kodama.local.command=smoke-probe" \
		-v "${tls_dir}/ca.crt:/run/kodama/ca.crt:ro" \
		-v "${token_dir}/admin-token:/run/kodama/token:ro" \
		-e API_WORKLOAD_URL="$WORKLOAD_URL" \
		-e GIT_WORKLOAD_URL="$GIT_WORKLOAD_URL" \
		-e GIT_TERMINAL_PROMPT=0 \
		"$WORKLOAD_IMAGE" sh -eu -c '
if curl --silent --max-time 20 --cacert /etc/ssl/certs/ca-certificates.crt "$API_WORKLOAD_URL/readyz" >/dev/null 2>&1; then
  echo "the workload API accepted an unrelated CA" >&2
  exit 1
fi
if git -c http.sslCAInfo=/etc/ssl/certs/ca-certificates.crt \
    ls-remote "$GIT_WORKLOAD_URL/admin" >/dev/null 2>&1; then
  echo "the workload Git accepted an unrelated CA" >&2
  exit 1
fi
' || die "a workload did not refuse the unrelated public CA"
	printf 'kodama-smoke: the workload endpoints refused an unrelated CA\n'
}

container_identities() {
	local name
	for name in "$container_postgres" "$container_nats" "$container_oauth" "$container_server" "$container_git"; do
		podman inspect --format '{{.Name}} {{.Id}}' "$name" 2>/dev/null ||
			die "the container ${name} is missing during the re-run reuse check"
	done
}

re_run_reuse() {
	local before after
	before="$(container_identities)"
	run_install
	after="$(container_identities)"
	[ "$before" = "$after" ] ||
		die "a same-configuration re-run replaced a container instead of reusing it"
	printf 'kodama-smoke: a same-configuration re-run kept every container identity\n'
}

write_smoke_marker "$STATE_DIR" "$run" ||
	die "could not record this run's ownership of ${STATE_DIR}"
install_attempted=1
log "installing the disposable installation ${PREFIX} in slot ${KODAMA_SLOT}"
run_install
validate_descriptor
"${bundle_dir}/status.sh" >"${Q01_EVIDENCE_DIR}/status-install.log"
write_admin_header

# provision_repository <repository> <resource> <log-suffix> provisions one
# run-unique repository with the installed binary and registers its binding.
provision_repository() {
	local name=$1 id=$2 suffix=$3 output binding
	log "provisioning the run-unique repository ${name}"
	output="$(kodama_run "$server_env" git provision-repository \
		--tenant "$TENANT" --storage-root "$git_storage" \
		--repository "$name" --resource "$id")" ||
		die "kodama git provision-repository failed for ${name}"
	printf '%s\n' "$output" >"${Q01_EVIDENCE_DIR}/provision${suffix}.log"
	case "$output" in
	*"--resource ${name}=${id}"*) ;;
	*) die "git provision-repository did not print the expected --resource ${name}=${id} mapping" ;;
	esac

	log "registering the Shell binding ${id}"
	binding="$(jq -cn --arg project "$PROJECT" --arg binding "$id" --arg resource "$id" \
		--arg service "$SERVICE" --arg host "$GIT_PUBLIC_URL" --arg workload "$GIT_WORKLOAD_URL" \
		'{project_id: $project, binding_id: $binding, resource_id: $resource, enforcing_service: $service, trusted_origins: [$host, $workload], operations: ["git.read", "git.ref.update"], reportable_events: ["git.ref.updated"]}')"
	api_call POST "/api/v1/projects/${PROJECT}/bindings" "$binding" \
		"smoke-${run}-binding-${id}-$(date +%s)" "${token_dir}/binding${suffix}.json" ||
		die "resource.binding.register failed: $(api_code "${token_dir}/binding${suffix}.json"): $(api_message "${token_dir}/binding${suffix}.json")"
}

mapped_resources="${GIT_RESOURCES},${repository}=${resource}"
provision_repository "$repository" "$resource" ""
for ((shard = 1; shard <= shard_repositories; shard++)); do
	provision_repository "${repository}-s${shard}" "${resource}-s${shard}" "-s${shard}"
	mapped_resources="${mapped_resources},${repository}-s${shard}=${resource}-s${shard}"
done

export KODAMA_LOCAL_GIT_RESOURCES="$mapped_resources"
log "re-running the install so git serve maps ${repository} (keeping every mapping)"
run_install
validate_descriptor
"${bundle_dir}/status.sh" >"${Q01_EVIDENCE_DIR}/status-mapped.log"
write_admin_header
re_run_reuse
add_smoke_repository "$installation_file" "$repository" "$resource" "$resource"
validate_descriptor
jq -e --arg name "$repository" 'any(.git.repositories[]?; .name == $name)' "$installation_file" >/dev/null ||
	die "the descriptor does not list the provisioned repository ${repository}"

log "acquiring the four persona tokens"
for persona in admin engineer reviewer operator; do
	acquire_token "$persona"
done
persona_access_checks
host_git_read
workload_probe
unrelated_ca_refusal

{
	printf 'run=%s\n' "$run"
	printf 'slot=%s\n' "$KODAMA_SLOT"
	printf 'prefix=%s\n' "$PREFIX"
	printf 'state=%s\n' "$STATE_DIR"
	printf 'installation=%s\n' "$installation_file"
	printf 'repository=%s\n' "$repository"
	printf 'resource=%s\n' "$resource"
	printf 'git_url=%s\n' "$GIT_PUBLIC_URL"
	printf 'git_workload_url=%s\n' "$GIT_WORKLOAD_URL"
	printf 'api_url=%s\n' "$PUBLIC_URL"
	printf 'api_workload_url=%s\n' "$WORKLOAD_URL"
	printf 'revision=%s\n' "$(jq -r '.revision' "$installation_file")"
	printf 'server_image_digest=%s\n' "$(jq -r '.server_image_digest' "$installation_file")"
	printf 'selection=%s\n' "$selection"
} >"${Q01_EVIDENCE_DIR}/summary.txt"
chmod 600 "${Q01_EVIDENCE_DIR}/summary.txt"

if [ ! -f "$runner" ]; then
	printf 'kodama-smoke: installation access proof passed, but the common runner %s is absent; this wrapper cannot run the selected tests without it (rerun with the runner in place)\n' \
		"$runner" >&2
	exit 77
fi

printf 'kodama-smoke: invoking %s\n' "$runner"
[ -f "$verifier" ] ||
	die "the verifying wrapper ${verifier} is absent; a release run must fail on a missing or skipped expected test"
selection_command=()
while IFS= read -r selection_argument; do
	selection_command+=("$selection_argument")
done < <(selection_arguments)
"${selection_command[@]}"

case "$selection" in
full)
	printf 'kodama-smoke: full selection verified: %d tests\n' "${#expected[@]}"
	;;
narrowed)
	printf 'kodama-smoke: NARROWED SELECTION - NOT RELEASE EVIDENCE\n' >&2
	;;
preflight)
	printf 'kodama-smoke: preflight selection verified: 1 test\n'
	;;
esac
