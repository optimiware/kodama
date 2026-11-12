# S2 repository preparation, integrate action (tools image, task-free). The
# host engineer pushed the initial commit B to a granted work ref; this action
# fetches exactly B through the served workload Git URL and creates the absent
# protected target ref with it. The empty lease expectation is the update
# hook's empty expected base for a created ref.
set -eu
test -r "$W49_TOKEN" || { printf 'the mounted execution token is not readable\n' >&2; exit 1; }
w49_require_api_token || exit 1
repository=$(cat "$W49_INPUTS/repository")
repository_path=$(cat "$W49_INPUTS/repository_path")
git_url=$(cat "$W49_INPUTS/git_url")
target_ref=$(cat "$W49_INPUTS/target_ref")
expected_base=$(cat "$W49_INPUTS/expected_base")
candidate=$(cat "$W49_INPUTS/candidate")
work_ref=$(cat "$W49_INPUTS/work_ref")
test -n "$repository"
test -n "$repository_path"
test -n "$git_url"
test -n "$target_ref"
test -n "$candidate"
test -n "$work_ref"
test -z "$expected_base"

seed=$W49_WORKSPACE/seed-repo
rm -rf -- "$seed"
mkdir -p "$seed"
cd "$seed"
git init --quiet
w49_git remote add origin "$git_url/$repository_path"
w49_git fetch --quiet origin "$work_ref"
test "$(git rev-parse FETCH_HEAD)" = "$candidate"
w49_git push --quiet --force-with-lease="$target_ref:" origin "$candidate:$target_ref"
printf '%s\n' "$SMOKE_SEED_MARKER"
