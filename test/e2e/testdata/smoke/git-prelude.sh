# Shared release-smoke workload prelude. Test data, never product code.
#
# Every path defaults to the product-materialized location; the hermetic
# fixture tests override the SMOKE_* variables to run the same bytes on the
# host. The execution credential is read from its mount on every Git call and
# reaches Git only through --config-env, so it never appears in argv, a remote
# URL or repository configuration.
W49_WORKSPACE=${SMOKE_WORKSPACE:-/workspace}
W49_TOKEN=${SMOKE_TOKEN_PATH:-/kodama/secrets/KODAMA_TOKEN}
W49_TASK_FILE=${SMOKE_TASK_FILE:-/kodama/task.json}
W49_INPUTS=${SMOKE_INPUTS_ROOT:-/kodama/inputs}
W49_OUTPUTS=${SMOKE_OUTPUTS_ROOT:-/kodama/outputs}
W49_CA=${SMOKE_CA_FILE:-}
export GIT_TERMINAL_PROMPT=0
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_ATTR_NOSYSTEM=1

w49_git() {
	if [ -n "$W49_CA" ]; then
		W49_AUTH="Authorization: Basic $(printf 'kodama:%s' "$(cat "$W49_TOKEN")" | base64 -w0)" \
		git -c credential.helper= -c http.followRedirects=false -c http.sslCAInfo="$W49_CA" \
			--config-env=http.extraHeader=W49_AUTH "$@"
	else
		W49_AUTH="Authorization: Basic $(printf 'kodama:%s' "$(cat "$W49_TOKEN")" | base64 -w0)" \
		git -c credential.helper= -c http.followRedirects=false \
			--config-env=http.extraHeader=W49_AUTH "$@"
	fi
}

w49_require_api_token() {
	if [ -z "${KODAMA_URL:-}" ]; then
		# The hermetic fixture scripts run without an installation.
		printf 'w49-token-api=not-configured\n'
		return 0
	fi
	w49_sync_attempts=${W49_TOKEN_SYNC_ATTEMPTS:-45}
	w49_attempt=1
	while :; do
		w49_api_token=$(cat "$W49_TOKEN") || return 1
		if [ -z "$w49_api_token" ]; then
			printf 'w49-token-api=empty-token-file\n' >&2
			return 1
		fi
		printf '%s' '{"input":{}}' >/tmp/w49-inspect.json
		if [ -n "$W49_CA" ]; then
			w49_api_status=$(printf 'header = "Authorization: Bearer %s"\n' "$w49_api_token" | \
				curl --config - --silent --show-error --proto '=https' --cacert "$W49_CA" \
					--connect-timeout 10 --max-time 20 --request GET \
					--header 'Content-Type: application/json' \
					--data-binary @/tmp/w49-inspect.json \
					--output /tmp/w49-inspect-response.json --write-out '%{http_code}' \
					"$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION") || w49_api_status=transport
		else
			w49_api_status=$(printf 'header = "Authorization: Bearer %s"\n' "$w49_api_token" | \
				curl --config - --silent --show-error --proto '=https' \
					--connect-timeout 10 --max-time 20 --request GET \
					--header 'Content-Type: application/json' \
					--data-binary @/tmp/w49-inspect.json \
					--output /tmp/w49-inspect-response.json --write-out '%{http_code}' \
					"$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION") || w49_api_status=transport
		fi
		rm -f /tmp/w49-inspect.json
		if [ "$w49_api_status" = 200 ]; then
			printf 'w49-token-api=200\n'
			rm -f /tmp/w49-inspect-response.json
			return 0
		fi
		# On Kubernetes a credential renewal updates the mounted Secret before
		# kubelet refreshes /kodama/secrets/KODAMA_TOKEN inside the container;
		# retry briefly on 401 while re-reading the file on every pass.
		if [ "$w49_api_status" != 401 ] || [ "$w49_attempt" -ge "$w49_sync_attempts" ]; then
			printf 'w49-token-api=%s\n' "$w49_api_status"
			head -c 400 /tmp/w49-inspect-response.json >&2 || true
			printf '\n' >&2
			rm -f /tmp/w49-inspect-response.json
			return 1
		fi
		rm -f /tmp/w49-inspect-response.json
		w49_attempt=$((w49_attempt + 1))
		sleep 2
	done
}

w49_require_context() {
	test -n "${KODAMA_EXECUTION:-}"
	test -s "$W49_TASK_FILE"
	test -n "${KODAMA_TASK:-}"
}

w49_remote_commit() {
	ref=$1
	w49_git ls-remote --refs "$2" "$ref" | cut -f1
}
