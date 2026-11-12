# S5 HumanPush subscribed-pipeline action (tools image). The automatic start
# supplies only repository, ref, commit and previous_commit from the accepted
# human-push report. This action proves those materialized inputs are the
# human event's, not another push's labels, and prints one exact marker.
set -eu
repository=$(cat "$W49_INPUTS/repository")
ref=$(cat "$W49_INPUTS/ref")
commit=$(cat "$W49_INPUTS/commit")
previous=$(cat "$W49_INPUTS/previous_commit")
printf 'w49-input-repository=%s\n' "$repository"
printf 'w49-input-ref=%s\n' "$ref"
printf 'w49-input-commit=%s\n' "$commit"
printf 'w49-input-previous_commit=%s\n' "$previous"
test "$repository" = "$SMOKE_EXPECT_REPOSITORY"
test "$ref" = "$SMOKE_EXPECT_REF"
test "$commit" = "$SMOKE_EXPECT_COMMIT"
test "$previous" = "$SMOKE_EXPECT_PREVIOUS"
printf '%s\n' "$SMOKE_PUSH_ACTION_MARKER"
