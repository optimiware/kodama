# AC08 delivery commit action (tools image). The prepared checkout is at the
# reviewed base. Write the exact digest-pinned image reference the build action
# recorded into a deploy manifest, commit it as the candidate, push the candidate
# to the granted work ref and materialize the candidate/test-result values the
# next action submits. No image is rebuilt: the manifest names the same digest.
set -eu
w49_require_context
w49_require_api_token || exit 1
repository_path=$(cat "$W49_INPUTS/repository_path")
git_url=$(cat "$W49_INPUTS/git_url")
expected_base=$(cat "$W49_INPUTS/expected_base")
work_ref=$(cat "$W49_INPUTS/work_ref")
test -n "$repository_path"
test -n "$git_url"
test -n "$expected_base"
test -n "$work_ref"
test -n "$SMOKE_IMAGE"
test "$(cat /kodama/outputs/build/image)" = "$SMOKE_IMAGE"

repo=$W49_WORKSPACE/repo
cd "$repo"
test "$(git rev-parse HEAD)" = "$expected_base"
mkdir -p deploy
printf '%s\n' "$SMOKE_IMAGE" >deploy/image.txt
# The manifest names the exact digest; the run marker makes the candidate
# unique even when the reviewed base already carries the same manifest from an
# earlier delivery run, so the commit is never an empty no-op.
printf '%s\n' "$SMOKE_COMMIT_MARKER" >deploy/run.txt
git add -- deploy/image.txt deploy/run.txt
git -c user.name=kodama -c user.email=kodama@example.invalid \
	commit --quiet -m 'AC08 digest-pinned delivery candidate'
candidate=$(git rev-parse HEAD)
test "$candidate" != "$expected_base"
test "$(git rev-parse HEAD^)" = "$expected_base"
test "$(git show HEAD:deploy/image.txt)" = "$SMOKE_IMAGE"
test "$(git show HEAD:deploy/run.txt)" = "$SMOKE_COMMIT_MARKER"

w49_git push --quiet origin "$candidate:$work_ref"
printf '%s\n' "$candidate" >"$W49_WORKSPACE/ac08-candidate"
printf 'pass:%s\n' "$candidate" >"$W49_WORKSPACE/ac08-test-result"
printf '%s\n' "$SMOKE_COMMIT_MARKER"
