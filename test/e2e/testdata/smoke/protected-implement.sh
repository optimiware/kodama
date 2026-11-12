# S2 ProtectedTask implement action (small external-harness image, Alpine).
# No Git and no network: it consumes the prepared checkout and writes the
# deterministic fixture sources the following tools action commits and tests.
set -eu
test -n "${KODAMA_TASK:-}"
test -s "$W49_TASK_FILE"
test "$(cat "$W49_WORKSPACE/execution-marker")" = "$KODAMA_EXECUTION"
test "$(cat "$W49_WORKSPACE/run-marker")" = "$SMOKE_RUN_MARKER"
test -d "$W49_WORKSPACE/repo"
cat >"$W49_WORKSPACE/repo/answer.sh" <<'W49_ANSWER'
#!/bin/sh
set -eu
printf '42\n'
W49_ANSWER
cat >"$W49_WORKSPACE/repo/test.sh" <<'W49_TEST'
#!/bin/sh
set -eu
test "$(/bin/sh ./answer.sh)" = "42"
W49_TEST
# The run marker makes the candidate unique even when the current protected
# base already carries an identical fixture from an earlier run.
printf '%s\n' "$SMOKE_RUN_MARKER" >"$W49_WORKSPACE/repo/w49-run.txt"
printf '%s\n' "$SMOKE_IMPLEMENT_MARKER"
