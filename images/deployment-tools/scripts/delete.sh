#!/usr/bin/env bash
# delete.sh — scoped, repeatable deployment delete (task T-065, DQ-09).
#
# Runs as the ordinary cleanup step of an internal project cleanup run. It deletes
# exactly the objects the recorded deployment owns, and only while the live object
# is the object it observed to be owned: each delete carries the observed uid AND
# resourceVersion as API preconditions, so a replacement object, or a same-uid
# object whose annotations changed hands in between, makes the API server answer
# Conflict instead of deleting. A conflict is re-observed; an object that is no
# longer this deployment's is left untouched and reported. It cannot delete
# another tenant's, another deployment's or an unmanaged object.
#
# Authority comes before any mutation:
#   1. the record must show a requested cleanup (requested, or failed/unknown from
#      an earlier run of it) — an authorized *reader* of the deployment is not a
#      cleaner;
#   2. the run must be acknowledged as the deployment's bound cleanup run: the
#      script reports the published cleanup-result `unknown` (`delete_started`)
#      first, which only a bound run may do. The originating step's token, an
#      unbound run and a record without a cleanup request are all refused here,
#      before Kubernetes is touched. If the run dies afterwards the record keeps
#      the visible exception `unknown` and the cleanup is retried.
#
# A completed cleanup lists every owned object; absent objects count as deleted, so
# a repeat after a lost acknowledgment converges to the same report.
#
# Inputs:
#   DEPLOY_DEPLOYMENT_ID   the recorded deployment to clean up (required)
#   DEPLOY_DELETE_TIMEOUT  seconds to wait for deletion to finish (default 60)
#   DEPLOY_OUTPUTS         comma list restricting the outputs written
set -euo pipefail
TOOL=delete
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

init_common

TARGET=${DEPLOY_DEPLOYMENT_ID:-${KODAMA_INPUT_DEPLOYMENT_ID:-}}
DELETE_TIMEOUT=${DEPLOY_DELETE_TIMEOUT:-60}
[[ "$TARGET" =~ ^[A-Za-z0-9_.-]{1,128}$ ]] || die input_invalid "DEPLOY_DEPLOYMENT_ID is required"
[[ "$DELETE_TIMEOUT" =~ ^[0-9]{1,5}$ ]] || die input_invalid "DEPLOY_DELETE_TIMEOUT is a number of seconds"

# The record is the only source of what may be deleted. Reading it is not
# authority to delete (the originating step may read it too): see the preflight.
rt GET "/runtime/v1/attempts/$ATTEMPT_ID/deployments/$TARGET"
[ "$RT_STATUS" = 200 ] || rt_fail "reading the deployment"
load_receipt
[ "$DEP_ID" = "$TARGET" ] || die callback_rejected "the receipt is for another deployment"
CLEANUP_STATE=$(jq -r '.deployment.cleanup.state // "none"' "$RECEIPT")

refs_from_receipt >"$WORK/recorded.json"
total=$(jq 'length' "$WORK/recorded.json")
[ "$total" -gt 0 ] || die nothing_recorded "the deployment recorded no objects, so there is nothing this run may delete"

report() { # <state> <reason> <deleted-file|"">
  local state=$1 reason=$2 deleted=${3:-} body="$WORK/cleanup-body.json"
  if [ -n "$deleted" ]; then
    jq -c --arg s "$state" --arg r "$reason" '{state:$s}+(if $r != "" then {reason:$r} else {} end)+{deleted:.}' "$deleted" >"$body"
  else
    jq -cn --arg s "$state" --arg r "$reason" '{state:$s}+(if $r != "" then {reason:$r} else {} end)' >"$body"
  fi
  rt POST "/runtime/v1/attempts/$ATTEMPT_ID/deployments/$DEP_ID/cleanup-result" "$body"
}

# A cluster that cannot answer is an unknown outcome, never "already gone".
observe_or_report() {
  if ! observe_all "$1" "$2"; then
    local reason=${OBS_ERR:-cluster_unavailable}
    report unknown "$reason" "" || true
    die "$reason" "the cluster could not be observed: $(head -c 300 "$WORK/kc.err" | tr '\n' ' ')"
  fi
}

# listed <observation-file> <out-file>: every owned object, with the uid observed
# (or, when already absent, the recorded one).
write_listed() {
  jq -c --slurpfile r "$RECEIPT" '
    [ .[] as $a
      | ($r[0].deployment.objects | map(select(.apiVersion == $a.apiVersion and .kind == $a.kind and .name == $a.name)) | .[0].uid // "") as $recorded
      | {apiVersion:$a.apiVersion,kind:$a.kind,namespace:$a.namespace,name:$a.name}
        + (if ($a.uid // "") != "" then {uid:$a.uid} elif $recorded != "" then {uid:$recorded} else {} end)]' "$1" >"$2"
}

# 1. Cleanup must have been requested for this deployment.
case "$CLEANUP_STATE" in
  requested | failed | unknown) ;;
  done)
    # A repeat of a completed cleanup: nothing to delete; converge on the same report.
    observe_or_report "$WORK/recorded.json" "$WORK/after.json"
    [ "$(jq '[.[] | select(.present)] | length' "$WORK/after.json")" = 0 ] ||
      die cleanup_already_done "the cleanup is recorded as done but recorded objects exist again; they are not this run's to delete"
    write_listed "$WORK/after.json" "$WORK/listed.json"
    report done "" "$WORK/listed.json"
    [ "$RT_STATUS" = 200 ] || rt_fail "reporting the completed cleanup"
    emit_outputs "$(jq -cn --arg id "$DEP_ID" --argjson n "$total" '{deployment_id:$id,cleanup_state:"done",deleted_count:$n}')"
    exit 0
    ;;
  *) die cleanup_not_requested "no cleanup was requested for deployment $DEP_ID (cleanup state: $CLEANUP_STATE); nothing was deleted" ;;
esac

# 2. Acknowledged cleanup-specific authorization before any Kubernetes mutation.
# Only a cleanup run bound to this deployment can record this; every other caller
# (the originating step, an unbound run) is refused and nothing is deleted.
report unknown delete_started ""
[ "$RT_STATUS" = 200 ] || rt_fail "authorizing the cleanup"

observe_or_report "$WORK/recorded.json" "$WORK/before.json"

# 3. One object at a time: observe, check ownership, delete with preconditions.
: >"$WORK/delete.err"
: >"$WORK/results.lines"
MAX_CONFLICTS=8

# delete_one <apiVersion> <kind> <name> <recorded-uid>
# Writes {status, uid} to $WORK/one.json: status is gone | deleted | foreign.
# Dies (after reporting) when the cluster refuses or cannot answer. Called in the
# main shell, never in a command substitution, so that `die` ends the script.
delete_one() {
  local api=$1 kind=$2 name=$3 recorded=$4 base live uid rv tries=0 rc
  base=$(resource_url "$api" "$kind") || {
    report unknown cluster_unavailable "" || true
    die cluster_unavailable "cannot resolve the API resource of $kind: $(head -c 200 "$WORK/kc.err" | tr '\n' ' ')"
  }
  while [ "$tries" -lt "$MAX_CONFLICTS" ]; do
    tries=$((tries + 1))
    if ! live=$(observe_ref "$api" "$kind" "$name"); then
      local reason
      reason=$(kc_error_class)
      report unknown "$reason" "" || true
      die "$reason" "the cluster could not be observed: $(head -c 300 "$WORK/kc.err" | tr '\n' ' ')"
    fi
    if [ -z "$live" ]; then
      jq -cn '{status:"gone",uid:null}' >"$WORK/one.json"
      return 0
    fi
    # Ownership of THIS observation: every receipt annotation, and the recorded uid.
    if ! jq -e --argjson ann "$ANNOTATIONS" --arg rec "$recorded" '
        (.metadata.annotations // {}) as $have
        | ($ann | to_entries | all($have[.key] == .value))
          and ($rec == "" or $rec == .metadata.uid)' <<<"$live" >/dev/null; then
      jq -cn --arg u "$(jq -r '.metadata.uid' <<<"$live")" '{status:"foreign",uid:$u}' >"$WORK/one.json"
      return 0
    fi
    uid=$(jq -r '.metadata.uid' <<<"$live")
    rv=$(jq -r '.metadata.resourceVersion' <<<"$live")
    jq -cn --arg u "$uid" --arg r "$rv" \
      '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Background",preconditions:{uid:$u,resourceVersion:$r}}' >"$WORK/delete-options.json"
    log "deleting $kind/$name (uid $uid, resourceVersion $rv, owned by $DEP_ID)"
    set +e
    kc delete --raw "$base/$name" -f "$WORK/delete-options.json" >"$WORK/delete.out" 2>"$WORK/delete-one.err"
    rc=$?
    set -e
    cat "$WORK/delete-one.err" >>"$WORK/delete.err"
    if [ "$rc" -eq 0 ]; then
      jq -cn --arg u "$uid" '{status:"deleted",uid:$u}' >"$WORK/one.json"
      return 0
    fi
    if grep -qi 'NotFound' "$WORK/delete-one.err"; then
      jq -cn '{status:"gone",uid:null}' >"$WORK/one.json"
      return 0
    fi
    if grep -qi 'Conflict' "$WORK/delete-one.err"; then
      # The object changed between the observation and the delete: nothing was
      # deleted. Look again; the new state decides.
      log "$kind/$name changed between observation and delete; observing again"
      continue
    fi
    cp "$WORK/delete-one.err" "$WORK/kc.err"
    local reason
    reason=$(kc_error_class)
    report unknown "$reason" "" || true
    die "$reason" "the delete of $kind/$name was refused: $(head -c 300 "$WORK/delete-one.err" | tr '\n' ' ')"
  done
  report unknown conflict_exhausted "" || true
  die conflict_exhausted "$kind/$name kept changing; it was not deleted"
}

n=$(jq 'length' "$WORK/recorded.json")
for ((i = 0; i < n; i++)); do
  api=$(jq -r ".[$i].apiVersion" "$WORK/recorded.json")
  kind=$(jq -r ".[$i].kind" "$WORK/recorded.json")
  name=$(jq -r ".[$i].name" "$WORK/recorded.json")
  recorded=$(jq -r --arg a "$api" --arg k "$kind" --arg n "$name" '.deployment.objects[] | select(.apiVersion == $a and .kind == $k and .name == $n) | .uid // ""' "$RECEIPT")
  delete_one "$api" "$kind" "$name" "$recorded"
  jq -c --arg a "$api" --arg k "$kind" --arg n "$name" '. + {apiVersion:$a,kind:$k,name:$n}' "$WORK/one.json" >>"$WORK/results.lines"
done
jq -s '.' "$WORK/results.lines" >"$WORK/results.json"

# 4. Wait for the deleted objects to disappear (finalizers). Only the uid this run
# deleted is waited for: anything else that appears under the name is not ours.
deadline=$((SECONDS + DELETE_TIMEOUT))
while :; do
  pending=0
  for ((i = 0; i < n; i++)); do
    [ "$(jq -r ".[$i].status" "$WORK/results.json")" = deleted ] || continue
    api=$(jq -r ".[$i].apiVersion" "$WORK/results.json")
    kind=$(jq -r ".[$i].kind" "$WORK/results.json")
    name=$(jq -r ".[$i].name" "$WORK/results.json")
    want=$(jq -r ".[$i].uid" "$WORK/results.json")
    live=$(observe_ref "$api" "$kind" "$name") || live='{"metadata":{"uid":"unknown"}}'
    if [ -n "$live" ] && [ "$(jq -r '.metadata.uid' <<<"$live")" = "$want" ]; then
      pending=$((pending + 1))
    fi
  done
  [ "$pending" = 0 ] && break
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 1
done

# Convergence is judged by observation, not by what kubectl printed.
observe_or_report "$WORK/recorded.json" "$WORK/after.json"
# An object counts as still ours only while it is the very uid this run deleted
# (terminating). Anything else that is present — an object that was foreign when
# observed, or one that appeared under the name afterwards — is not this run's to
# delete: it is reported and left untouched.
jq -r --slurpfile r "$WORK/results.json" '
  [ .[] as $a | select($a.present)
    | ($r[0][] | select(.apiVersion == $a.apiVersion and .kind == $a.kind and .name == $a.name)) as $b
    | select(($b.status == "deleted" and $b.uid == $a.uid) | not) | $a.kind + "/" + $a.name ] | join(",")' "$WORK/after.json" >"$WORK/unowned.txt"
jq -r --slurpfile r "$WORK/results.json" '
  [ .[] as $a | select($a.present)
    | ($r[0][] | select(.apiVersion == $a.apiVersion and .kind == $a.kind and .name == $a.name)) as $b
    | select($b.status == "deleted" and $b.uid == $a.uid) | $a.kind + "/" + $a.name ] | join(",")' "$WORK/after.json" >"$WORK/terminating.txt"
foreign=$(jq -r '[.[] | select(.status == "foreign") | .kind + "/" + .name] | join(",")' "$WORK/results.json")
unowned=$(cat "$WORK/unowned.txt")
terminating=$(cat "$WORK/terminating.txt")

write_listed "$WORK/after.json" "$WORK/listed.json"

if [ -n "$foreign" ] || [ -n "$unowned" ]; then
  report failed object_not_owned "" || true
  die object_not_owned "objects named by the record are not owned by this deployment and were left untouched: ${foreign:+$foreign }$unowned"
fi
if [ -n "$terminating" ]; then
  report failed delete_incomplete "" || true
  die delete_incomplete "recorded objects still exist after the delete: $terminating"
fi

report done "" "$WORK/listed.json"
case "$RT_STATUS" in 200) ;; *) die outcome_unacknowledged "the objects are deleted but the cleanup result was not acknowledged (HTTP $RT_STATUS); a repeat converges" ;; esac

emit_outputs "$(jq -cn --arg id "$DEP_ID" --argjson n "$total" '{deployment_id:$id,cleanup_state:"done",deleted_count:$n}')"
