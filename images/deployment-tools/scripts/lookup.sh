#!/usr/bin/env bash
# lookup.sh — read-only reconciliation lookup (task T-065, DQ-08).
#
# After a lost response, a restart or a retry attempt: ask the deployment record
# for the step's own logical action, then observe the recorded objects in the
# cluster BY THEIR ANNOTATIONS. Nothing is changed in the record or the cluster.
# The verdict is computed from structured live status, never from command output.
#
# Inputs:
#   DEPLOY_ACTION_KEY   default "$KODAMA_RUN_ID:$KODAMA_STEP_ID" (the attempt's own key)
#   DEPLOY_OUTPUTS      comma list restricting the outputs written to the manifest
#
# stdout: one JSON document (the lookup output contract, README.md):
#   {"verdict": ..., "deployment": {...}|null, "observed": [...]}
# Verdicts: unregistered | registered | failed | absent | partial | foreign |
#           image_mismatch | applied_not_ready | applied_ready
# Exit status: 0 whenever the observation was completed (any verdict); the result
# manifest fails with callback_*/cluster_* reasons when it could not be.
set -euo pipefail
TOOL=lookup
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

init_common

ACTION_KEY=${DEPLOY_ACTION_KEY:-${KODAMA_RUN_ID:-}:${KODAMA_STEP_ID:-}}
[[ "$ACTION_KEY" =~ ^[A-Za-z0-9_.:-]{3,256}$ ]] || die input_invalid "DEPLOY_ACTION_KEY (or KODAMA_RUN_ID and KODAMA_STEP_ID) is required"

rt GET "/runtime/v1/attempts/$ATTEMPT_ID/deployments/by-action/$ACTION_KEY"
case "$RT_STATUS" in
  200) ;;
  404)
    out=$(jq -cn '{verdict:"unregistered",deployment:null,observed:[]}')
    printf '%s\n' "$out"
    emit_outputs '{"verdict":"unregistered","ready":false}'
    exit 0
    ;;
  *) rt_fail "looking up the deployment" ;;
esac
load_receipt

refs_from_receipt >"$WORK/recorded.json"
if [ "$(jq 'length' "$WORK/recorded.json")" = 0 ]; then
  echo '[]' >"$WORK/observation.json"
else
  observe_all "$WORK/recorded.json" "$WORK/observation.json" ||
    die "${OBS_ERR:-cluster_unavailable}" "the cluster could not be observed: $(head -c 300 "$WORK/kc.err" | tr '\n' ' ')"
fi

verdict=$(jq -r --arg status "$DEP_STATUS" '
  if $status == "failed" then "failed"
  elif length == 0 then "registered"
  elif any(.present and (.owned | not)) then "foreign"
  elif any(.present and .owned and (.bound | not)) then "image_mismatch"
  elif all(.present | not) then "absent"
  elif any(.present | not) then "partial"
  elif (any(.hasv) | not) then "image_mismatch"
  elif all(.ready) then "applied_ready"
  else "applied_not_ready" end' "$WORK/observation.json")

jq -c --arg v "$verdict" --slurpfile r "$RECEIPT" '
  $r[0].deployment as $d
  | {verdict:$v,
     deployment:{id:$d.id,status:$d.status,reason:$d.reason,outcomeUnknown:($d.outcomeUnknown // false),
                 namespace:$d.namespace,configurationDigest:$d.configurationDigest,
                 imageDigest:$d.image.digest,sourceCommit:$d.source.commit,actionKey:$d.actionKey},
     annotations:$r[0].annotations,
     observed:.}' "$WORK/observation.json"

ready=false
[ "$verdict" = applied_ready ] && ready=true
emit_outputs "$(jq -cn --arg id "$DEP_ID" --arg st "$DEP_STATUS" --arg cfg "$CONFIG_DIGEST" --arg img "$IMAGE_DIGEST" --arg src "$SOURCE_COMMIT" \
  --arg v "$verdict" --argjson ready "$ready" \
  '{deployment_id:$id,deployment_status:$st,configuration_digest:$cfg,image_digest:$img,source_commit:$src,verdict:$v,ready:$ready}')"
