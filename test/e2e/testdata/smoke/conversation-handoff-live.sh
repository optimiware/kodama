#!/bin/sh
# S3 ConversationHandoff action. With no reply in the task snapshot it writes
# scratch state, publishes one public observation.custom readiness event, and
# stays live. The assigned reply's fresh execution finds the exact answer
# message, proves the cancelled execution's scratch is absent and succeeds.
#
# The readiness signal is a custom event rather than a retained log chunk:
# retained logs are collected when a workload is removed, so a running action
# has no execution.logs.read output yet. The event is the public, durable proof
# that the action ran; the retained log is asserted after cancellation.
set -eu
answer=${SMOKE_ANSWER:?}
restoration=${SMOKE_RESTORATION_MARKER:?}
ready=${SMOKE_READY_MARKER:?}
project=${SMOKE_PROJECT:?}
ready_key=${SMOKE_READY_KEY:?}
workspace=${SMOKE_WORKSPACE:-/workspace}
task_file=${SMOKE_TASK_FILE:-/kodama/task.json}
token_file=${SMOKE_TOKEN_PATH:-/kodama/secrets/KODAMA_TOKEN}
scratch=$workspace/e1-scratch
test -f "$task_file"
if python3 - "$answer" "$task_file" <<'PY'
import json
import sys

messages = json.load(open(sys.argv[2]))
want = sys.argv[1]
raise SystemExit(0 if any(isinstance(m, dict) and m.get("body") == want for m in messages) else 1)
PY
then
	test ! -e "$scratch" || {
		printf 'the fresh attempt inherited the cancelled attempt scratch\n' >&2
		exit 1
	}
	printf '%s\n' "$restoration"
	# Keep the task open long enough for the identical task.reply replay to
	# converge on this execution rather than on a completed task.
	sleep 5
	exit 0
fi
printf 'scratch\n' >"$scratch"

# Print the readiness marker before publishing the event. A consumer can
# observe the committed event and cancel while the publish request is still in
# flight, so the event only proves readiness once the marker is already on
# stdout.
printf '%s\n' "$ready"

# Publish the readiness observation with the mounted execution credential. The
# token is read from its file into a curl config on stdin, never argv.
test -r "$token_file"
body=$(printf '{"input":{"project_id":"%s","type":"observation.custom","subject_id":"%s","correlation_id":"%s"},"idempotency_key":"%s"}' \
	"$project" "$ready" "${KODAMA_EXECUTION:?}" "$ready_key")
status=$(printf 'header = "Authorization: Bearer %s"\n' "$(cat "$token_file")" | \
	curl -sS --noproxy '*' --max-time 20 -X POST -K - \
		-H 'Content-Type: application/json' -H 'Accept: application/json' \
		--data "$body" -o "$workspace/handoff-ready.json" -w '%{http_code}' \
		"${KODAMA_URL:?}/api/v1/projects/${project}/events")
if [ "$status" != 200 ]; then
	printf 'w49-handoff: event.publish answered %s\n' "$status" >&2
	cat "$workspace/handoff-ready.json" >&2 || true
	exit 1
fi
grep -q '"event_id"' "$workspace/handoff-ready.json"

sleep 300
