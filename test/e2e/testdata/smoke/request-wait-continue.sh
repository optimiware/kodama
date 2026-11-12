#!/bin/sh
# S1 RequestWait continuation action: the same execution and workspace, the
# promoted request answer, and exactly one continuation marker.
set -eu
marker=${SMOKE_MARKER:?}
continuation=${SMOKE_CONTINUATION:?}
answer=${SMOKE_ANSWER:?}
test "$(cat /workspace/prepare-marker)" = "$marker"
test "$(cat /workspace/execution-id)" = "${KODAMA_EXECUTION:?}"
test "$(cat /kodama/outputs/answer/answer)" = "$answer"
printf '%s\n' "$continuation"
