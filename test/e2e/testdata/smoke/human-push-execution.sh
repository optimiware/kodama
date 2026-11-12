# S5 HumanPush execution-originated control push (tools image). This is a
# separate inline execution: it clones or initialises the run repository,
# creates one commit and pushes it to a subscribed work ref with its own mounted
# execution credential. The report must carry this execution as its
# correlation, which is exactly what the push trigger must exclude.
set -eu
test -r "$W49_TOKEN" || { printf 'the mounted execution token is not readable\n' >&2; exit 1; }
w49_require_api_token || exit 1
test -n "$SMOKE_GIT_URL"
test -n "$SMOKE_REPOSITORY"
test -n "$SMOKE_EXECUTION_REF"

repo=$W49_WORKSPACE/pusher-repo
rm -rf -- "$repo"
w49_git clone --quiet "$SMOKE_GIT_URL/$SMOKE_REPOSITORY" "$repo"
cd "$repo"
git checkout -B w49-execution >/dev/null 2>&1 || true
printf 'execution push %s\n' "$SMOKE_RUN_MARKER" >w49-execution.txt
git add -- w49-execution.txt
git -c user.name=kodama -c user.email=kodama@example.invalid \
	commit --quiet -m 'W49 execution-originated push'
w49_git push --quiet origin "HEAD:$SMOKE_EXECUTION_REF"
printf 'w49-pushed-commit=%s\n' "$(git rev-parse HEAD)"
printf '%s\n' "$SMOKE_PUSHER_MARKER"
