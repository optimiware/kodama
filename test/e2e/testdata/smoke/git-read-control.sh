# S2 workload Git read control (tools image). A fresh execution, with no
# request wait, proves the mounted execution credential reads the served Git
# repository through the workload HTTPS endpoint and the public CA. It is the
# positive control that separates the Git edge from the API.
set -eu
test -r "$W49_TOKEN" || { printf 'the mounted execution token is not readable\n' >&2; exit 1; }
w49_require_api_token || exit 1
test -n "$SMOKE_GIT_URL"
test -n "$SMOKE_REPOSITORY"
test -n "$SMOKE_TARGET_REF"
w49_git ls-remote --refs "$SMOKE_GIT_URL/$SMOKE_REPOSITORY" "$SMOKE_TARGET_REF" >/dev/null
printf 'w49-git-read-control=ok\n'
