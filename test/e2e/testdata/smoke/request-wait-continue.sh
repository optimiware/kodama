#!/bin/sh
# S1 RequestWait continuation action: the same execution and workspace, the
# promoted request answer, and exactly one continuation marker.
set -eu
marker=${SMOKE_MARKER:?}
continuation=${SMOKE_CONTINUATION:?}
answer=${SMOKE_ANSWER:?}
test "$(cat /workspace/prepare-marker)" = "$marker"
test "$(cat /workspace/execution-id)" = "${KODEMA_EXECUTION:?}"
test "$(cat /kodema/outputs/answer/answer)" = "$answer"
printf '%s\n' "$continuation"
