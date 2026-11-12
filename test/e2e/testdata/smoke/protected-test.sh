# S2 ProtectedTask test action (tools image). Commit the external harness's
# exact change as C, push C to the granted work ref (the authentication and
# reachability positive control), run the committed test, and require the
# protected target ref to refuse the unapproved C push while it stays at B.
# Declared outputs are submitted only after the committed test passes.
set -eu
w49_require_context
w49_require_api_token || exit 1
test "$(cat "$W49_WORKSPACE/execution-marker")" = "$KODAMA_EXECUTION"
test "$(cat "$W49_WORKSPACE/run-marker")" = "$SMOKE_RUN_MARKER"
expected_base=$(cat "$W49_INPUTS/expected_base")
target_ref=$(cat "$W49_INPUTS/target_ref")
work_ref=$(cat "$W49_INPUTS/work_ref")
test -n "$expected_base"
test -n "$target_ref"
test -n "$work_ref"

repo=$W49_WORKSPACE/repo
cd "$repo"
test "$(git rev-parse HEAD)" = "$expected_base"
test -f answer.sh
test -f test.sh
test -f w49-run.txt
git add -- answer.sh test.sh w49-run.txt
git -c user.name=kodama -c user.email=kodama@example.invalid \
	commit --quiet -m 'W49 protected candidate'
candidate=$(git rev-parse HEAD)
test "$candidate" != "$expected_base"
test "$(git rev-parse HEAD^)" = "$expected_base"

# Positive control: the granted work ref accepts this execution's push.
w49_git push --quiet origin "$candidate:$work_ref"

# The committed test must pass with clean tracked files.
/bin/sh ./test.sh
test -z "$(git status --porcelain --untracked-files=no)"

# The protected ref must refuse the unapproved execution push. The lease is the
# exact reviewed base, so only the missing approval can refuse it.
if w49_git push --quiet --force-with-lease="$target_ref:$expected_base" \
	origin "$candidate:$target_ref" 2>"$W49_WORKSPACE/preapproval-push.log"; then
	printf 'the protected ref accepted an unapproved execution push\n' >&2
	exit 1
fi
if ! grep -q 'update rejected' "$W49_WORKSPACE/preapproval-push.log"; then
	printf 'the pre-approval push was refused without the update-hook rejection\n' >&2
	cat "$W49_WORKSPACE/preapproval-push.log" >&2
	exit 1
fi
test "$(w49_remote_commit "$target_ref" origin)" = "$expected_base"
