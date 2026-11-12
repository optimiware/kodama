#!/bin/sh
# S1 RequestWait preparation action: write the shared-workspace marker and the
# execution identity the successor must still see across the request wait.
set -eu
marker=${SMOKE_MARKER:?}
ready=${SMOKE_PREPARE_READY:?}
execution=${KODAMA_EXECUTION:?}
if [ -e /kodama/task.json ]; then
	printf 'a task-free request-wait preparation materialized a task snapshot\n' >&2
	exit 1
fi
printf '%s\n' "$marker" >/workspace/prepare-marker
printf '%s\n' "$execution" >/workspace/execution-id
printf '%s\n' "$ready"
