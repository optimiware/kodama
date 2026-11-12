#!/bin/sh
# S4 OperatorRetry work action. The first attempt finds no repair marker,
# writes scratch state and exits non-zero. The operator's retry sees the exact
# durable repair comment, proves the failed attempt's scratch is absent from its
# fresh workspace, and succeeds.
set -eu
repair=${SMOKE_REPAIR_MARKER:?}
success=${SMOKE_SUCCESS_MARKER:?}
failure=${SMOKE_FAILURE_MARKER:?}
scratch=/workspace/retry-scratch
test -f /kodama/task.json
if python3 - "$repair" <<'PY'
import json
import sys

messages = json.load(open("/kodama/task.json"))
want = sys.argv[1]
raise SystemExit(0 if any(isinstance(m, dict) and m.get("body") == want for m in messages) else 1)
PY
then
	test ! -e "$scratch" || {
		printf 'the fresh attempt inherited the failed attempt scratch\n' >&2
		exit 1
	}
	printf '%s\n' "$success"
	# Keep the task open long enough for the identical task.start replay to
	# converge on this execution rather than on a completed task.
	sleep 5
	exit 0
fi
printf 'scratch\n' >"$scratch"
printf '%s\n' "$failure"
exit 12
