# S2 ProtectedTask integrate action (tools image). After the human pass, push
# the exact reviewed candidate C to the protected target ref with the reviewed
# base B as an explicit lease. No new commit, no rebuild, and the current
# mounted execution token signs the push.
set -eu
w49_require_context
w49_require_api_token || exit 1
expected_base=$(cat "$W49_INPUTS/expected_base")
target_ref=$(cat "$W49_INPUTS/target_ref")
test -n "$expected_base"
test -n "$target_ref"
test "$(cat "$W49_WORKSPACE/execution-marker")" = "$KODAMA_EXECUTION"
test "$(cat "$W49_WORKSPACE/run-marker")" = "$SMOKE_RUN_MARKER"

candidate=$(cat "$W49_OUTPUTS/test/candidate")
test -n "$candidate"
test "$(cat "$W49_OUTPUTS/test/test_result")" = "pass:$candidate"

cd "$W49_WORKSPACE/repo"
test "$(git rev-parse HEAD)" = "$candidate"
test "$(git rev-parse HEAD^)" = "$expected_base"
test "$(w49_remote_commit "$target_ref" origin)" = "$expected_base"
w49_git push --quiet --force-with-lease="$target_ref:$expected_base" \
	origin "$candidate:$target_ref"
printf '%s\n' "$SMOKE_INTEGRATE_MARKER"
