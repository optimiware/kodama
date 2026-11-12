# S2 ProtectedTask prepare action (tools image). Clone the workload Git HTTPS
# endpoint at the reviewed base B, prove the served target ref is that base, and
# write the shared-workspace markers every later workload action requires.
set -eu
w49_require_context
w49_require_api_token || exit 1
repository=$(cat "$W49_INPUTS/repository")
repository_path=$(cat "$W49_INPUTS/repository_path")
git_url=$(cat "$W49_INPUTS/git_url")
target_ref=$(cat "$W49_INPUTS/target_ref")
expected_base=$(cat "$W49_INPUTS/expected_base")
test -n "$repository"
test -n "$repository_path"
test -n "$git_url"
test -n "$target_ref"
test -n "$expected_base"
if [ -n "$W49_CA" ]; then
	test -s "$W49_CA" || { printf 'the declared CA file is not readable\n' >&2; exit 1; }
fi
test -r "$W49_TOKEN" || { printf 'the mounted execution token is not readable\n' >&2; exit 1; }

repo=$W49_WORKSPACE/repo
rm -rf -- "$repo"
w49_git clone --quiet "$git_url/$repository_path" "$repo"
cd "$repo"
test "$(git rev-parse HEAD)" = "$expected_base"
test "$(w49_remote_commit "$target_ref" origin)" = "$expected_base"
printf '%s\n' "$KODAMA_EXECUTION" >"$W49_WORKSPACE/execution-marker"
printf '%s\n' "$SMOKE_RUN_MARKER" >"$W49_WORKSPACE/run-marker"
printf '%s\n' "$SMOKE_PREPARE_MARKER"
