#!/usr/bin/env bash
# apply.sh — ordinary deployment apply script (task T-065, DQ-07..DQ-10).
#
# One logical deployment per step action key. In order:
#   1. validate inputs and parse the manifests locally (nothing external yet);
#   2. register the pre-apply deployment record (idempotent by action key: a retry
#      after a lost response receives the original record and its annotations);
#   3. cross-check the receipt's verified source/image against the inputs;
#   4. branch on the record, never on memory: settled -> verify and report;
#      failed -> refuse; running+unknown -> reconcile by observing the cluster by
#      annotations; running -> observe, then re-apply the same intended objects;
#      pending -> first apply;
#   5. record the intended objects BEFORE the first external change;
#   6. kubectl apply the annotated objects into the deployment's namespace;
#   7. observe the live objects and decide readiness from their structured status;
#   8. report the observed outcome (uid + ready) and write the outputs.
#
# Inputs (environment; see README.md for the full contract):
#   DEPLOY_MANIFESTS            file or directory of manifests (@@IMAGE@@ is replaced)
#   DEPLOY_REPOSITORY_URL       source repository URL        (or KODAMA_INPUT_REPOSITORY_URL)
#   DEPLOY_COMMIT               source commit, 40 hex        (or KODAMA_INPUT_COMMIT)
#   DEPLOY_IMAGE_ARTIFACT_ID    registered image artifact id (or KODAMA_INPUT_IMAGE_ARTIFACT_ID)
#   DEPLOY_IMAGE                <repo>@sha256:<digest>       (or KODAMA_INPUT_IMAGE)
#   DEPLOY_READY_TIMEOUT        seconds to wait for readiness (default 120)
#   DEPLOY_ADOPT_EXISTING       "true" allows replacing unmanaged same-named objects
#   DEPLOY_ALLOWED_IMAGES       comma list of digest-pinned auxiliary (sidecar) images
#                               that may run next to the verified image
#   DEPLOY_OUTPUTS              comma list restricting the outputs written
set -euo pipefail
TOOL=apply
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

init_common

: "${DEPLOY_REPOSITORY_URL:=${KODAMA_INPUT_REPOSITORY_URL:-}}"
: "${DEPLOY_COMMIT:=${KODAMA_INPUT_COMMIT:-}}"
: "${DEPLOY_IMAGE_ARTIFACT_ID:=${KODAMA_INPUT_IMAGE_ARTIFACT_ID:-}}"
: "${DEPLOY_IMAGE:=${KODAMA_INPUT_IMAGE:-}}"
MANIFESTS=${DEPLOY_MANIFESTS:-}
READY_TIMEOUT=${DEPLOY_READY_TIMEOUT:-120}

[ -n "$MANIFESTS" ] || die input_invalid "DEPLOY_MANIFESTS is required"
[ -n "$DEPLOY_REPOSITORY_URL" ] || die input_invalid "DEPLOY_REPOSITORY_URL is required"
[[ "$DEPLOY_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die input_invalid "DEPLOY_COMMIT must be a full 40 hex commit"
[[ "$DEPLOY_IMAGE_ARTIFACT_ID" =~ ^[A-Za-z0-9_.-]{1,128}$ ]] || die input_invalid "DEPLOY_IMAGE_ARTIFACT_ID is required"
valid_image_ref "$DEPLOY_IMAGE" || die input_invalid "DEPLOY_IMAGE must be a digest-pinned reference <repo>@sha256:<64 hex>"
[[ "$READY_TIMEOUT" =~ ^[0-9]{1,5}$ ]] || die input_invalid "DEPLOY_READY_TIMEOUT is a number of seconds"
INPUT_DIGEST=${DEPLOY_IMAGE##*@}

# 1. Local validation: a bad manifest registers nothing and touches nothing. The
# image binding is part of it: every container in the manifests must be the
# verified image (or an explicitly allowed auxiliary one), and one must be.
render_manifests "$MANIFESTS" "$DEPLOY_IMAGE" "" '{"kodama.invalid/validate":"1"}' "$WORK/validate.json"
check_images "$WORK/validate.json" "$INPUT_DIGEST"

# 2. The pre-apply record. Idempotent by the step's logical action.
jq -cn --arg url "$DEPLOY_REPOSITORY_URL" --arg commit "$DEPLOY_COMMIT" --arg art "$DEPLOY_IMAGE_ARTIFACT_ID" --arg dig "$INPUT_DIGEST" \
  '{source:{repositoryUrl:$url,commit:$commit},image:{artifactId:$art,digest:$dig}}' >"$WORK/register.json"
rt POST "/runtime/v1/attempts/$ATTEMPT_ID/deployments" "$WORK/register.json"
case "$RT_STATUS" in 200 | 201) ;; *) rt_fail "registering the deployment" ;; esac
load_receipt
log "deployment $DEP_ID is $DEP_STATUS${DEP_REASON:+ ($DEP_REASON)} in namespace $NAMESPACE"

# 3. The receipt is server-verified provenance; the inputs must be what it records.
[ "$(jq -r '.deployment.source.commit' "$RECEIPT")" = "$DEPLOY_COMMIT" ] || die provenance_mismatch "the recorded source commit is not the input commit"
[ "$(jq -r '.deployment.source.repositoryUrl' "$RECEIPT")" = "$DEPLOY_REPOSITORY_URL" ] || die provenance_mismatch "the recorded source repository is not the input repository"
[ "$IMAGE_DIGEST" = "$INPUT_DIGEST" ] || die provenance_mismatch "the recorded image digest is not the digest of DEPLOY_IMAGE"
[ "$(jq -r '.deployment.image.artifactId' "$RECEIPT")" = "$DEPLOY_IMAGE_ARTIFACT_ID" ] || die provenance_mismatch "the recorded image artifact is not the input artifact"
valid_digest "$CONFIG_DIGEST" || die callback_rejected "the receipt carries no configuration digest"
valid_digest "$IMAGE_DIGEST" || die callback_rejected "the receipt carries no image digest"

outputs_json() {
  jq -cn --arg id "$DEP_ID" --arg cfg "$CONFIG_DIGEST" --arg img "$IMAGE_DIGEST" --arg src "$SOURCE_COMMIT" --argjson n "$1" \
    '{deployment_id:$id,deployment_status:"succeeded",configuration_digest:$cfg,image_digest:$img,source_commit:$src,ready:true,object_count:$n}'
}

# observe_or_die <refs-file> <obs-file>
observe_or_die() {
  observe_all "$1" "$2" || die "${OBS_ERR:-cluster_unavailable}" "the cluster could not be observed: $(head -c 300 "$WORK/kc.err" | tr '\n' ' ')"
}

all_ready() { jq -e 'length > 0 and all(.present and .owned and .bound and .ready) and any(.hasv)' "$1" >/dev/null; }

# unbound_live <observation-file>: why the live objects of this deployment are not
# running the verified image — either some owned object runs an image other than the
# verified or an allowed one, or every recorded object is present and owned yet no
# container anywhere runs the verified digest (the application container was removed
# or replaced by an allowed auxiliary image). Empty when the binding holds.
# Annotations and readiness never vouch for the image.
unbound_live() {
  jq -r '([.[] | select(.present and .owned and (.bound | not)) | .kind + "/" + .name] | join(",")) as $u
    | if $u != "" then $u
      elif length > 0 and all(.present and .owned) and (any(.hasv) | not) then "no live container runs the verified image"
      else "" end' "$1"
}
# wait_ready <refs-file> <obs-file>: polls the live objects until all are present,
# annotated by this deployment and structurally ready, or the timeout.
wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT))
  while :; do
    observe_or_die "$1" "$2"
    all_ready "$2" && return 0
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 2
  done
}

# settle_ready <refs-file>: wait, then report succeeded or rollout_timeout.
settle_ready() {
  local refs=$1 obs="$WORK/final-observation.json"
  if wait_ready "$refs" "$obs"; then
    observed_objects "$obs" >"$WORK/final-objects.json"
    post_outcome succeeded "" "$WORK/final-objects.json"
    [ "$RT_STATUS" = 200 ] || die outcome_unacknowledged "the objects are applied and ready but the outcome was not acknowledged (HTTP $RT_STATUS); a retry observes and reports it"
    [ "$(jq -r '.deployment.status' "$RT_BODY")" = succeeded ] || die callback_rejected "the success report was not recorded as succeeded"
    emit_outputs "$(outputs_json "$(jq 'length' "$obs")")"
    exit 0
  fi
  observed_objects "$obs" >"$WORK/final-objects.json"
  local why=rollout_timeout
  [ -z "$(unbound_live "$obs")" ] || why=image_mismatch
  post_outcome failed "$why" "$WORK/final-objects.json"
  [ "$RT_STATUS" = 200 ] || die outcome_unacknowledged "the rollout did not succeed but the failure was not acknowledged (HTTP $RT_STATUS)"
  [ "$why" = rollout_timeout ] || die image_mismatch "live objects do not run exactly the verified image: $(unbound_live "$obs")"
  die rollout_timeout "the objects were not all ready within ${READY_TIMEOUT}s: $(jq -c '[.[] | select((.present and .owned and .ready) | not) | .kind + "/" + .name]' "$obs")"
}

# 4. Branch on the recorded state.
case "$DEP_STATUS" in
  succeeded)
    # A retry of a settled action: verify, never re-apply.
    refs_from_receipt >"$WORK/recorded.json"
    observe_or_die "$WORK/recorded.json" "$WORK/observation.json"
    [ -z "$(unbound_live "$WORK/observation.json")" ] || die image_mismatch "the deployment is recorded as succeeded but live objects do not run exactly the verified image: $(unbound_live "$WORK/observation.json")"
    all_ready "$WORK/observation.json" || die recorded_state_diverged "the deployment is recorded as succeeded but its objects are absent, foreign or not ready"
    emit_outputs "$(outputs_json "$(jq 'length' "$WORK/observation.json")")"
    exit 0
    ;;
  failed)
    die previously_failed "deployment $DEP_ID settled as failed (${DEP_REASON:-unknown}); use a new run for a new attempt"
    ;;
  running)
    if [ "$DEP_UNKNOWN" = true ]; then
      # The previous apply may have taken effect. Never apply again: observe by
      # annotations and settle from what exists.
      refs_from_receipt >"$WORK/recorded.json"
      observe_or_die "$WORK/recorded.json" "$WORK/observation.json"
      present=$(jq '[.[] | select(.present and .owned)] | length' "$WORK/observation.json")
      total=$(jq 'length' "$WORK/observation.json")
      if [ -n "$(unbound_live "$WORK/observation.json")" ]; then
        observed_objects "$WORK/observation.json" >"$WORK/final-objects.json"
        post_outcome failed image_mismatch "$WORK/final-objects.json"
        [ "$RT_STATUS" = 200 ] || die outcome_unacknowledged "the reconciliation was not acknowledged (HTTP $RT_STATUS)"
        die image_mismatch "the unknown outcome was settled failed: live objects do not run exactly the verified image: $(unbound_live "$WORK/observation.json")"
      fi
      if [ "$present" = "$total" ] && [ "$total" -gt 0 ]; then
        log "reconciling an unknown outcome: every recorded object exists with this deployment's annotations"
        settle_ready "$WORK/recorded.json"
      fi
      observed_objects "$WORK/observation.json" >"$WORK/final-objects.json"
      reason=apply_not_observed
      [ "$present" = 0 ] || reason=apply_partial
      post_outcome failed "$reason" "$WORK/final-objects.json"
      [ "$RT_STATUS" = 200 ] || die outcome_unacknowledged "the reconciliation was not acknowledged (HTTP $RT_STATUS)"
      die "$reason" "the unknown outcome was settled by observation: $present of $total recorded objects exist"
    fi
    log "resuming an apply recorded by an earlier attempt"
    ;;
  pending) ;;
  *) die callback_rejected "the deployment has an unknown status $DEP_STATUS" ;;
esac

# 5-8. First apply, or resume of a running deployment whose outcome is known.
render_manifests "$MANIFESTS" "$DEPLOY_IMAGE" "$NAMESPACE" "$ANNOTATIONS" "$WORK/rendered.json"
# The binding again, against the receipt's verified digest, before the intent.
check_images "$WORK/rendered.json" "$IMAGE_DIGEST"
refs_from_rendered "$WORK/rendered.json" >"$WORK/intended.json"
jq '{apiVersion:"v1",kind:"List",items:.}' "$WORK/rendered.json" >"$WORK/apply-list.json"

# Observe before applying: an unmanaged object with an intended name is never
# overwritten silently, and a lost acknowledgment is visible here.
observe_or_die "$WORK/intended.json" "$WORK/observation.json"
foreign=$(jq -r '[.[] | select(.present and (.owned | not) and .holder == null) | .kind + "/" + .name] | join(",")' "$WORK/observation.json")
if [ -n "$foreign" ] && [ "${DEPLOY_ADOPT_EXISTING:-}" != true ]; then
  die object_conflict "objects not created by a deployment already exist: $foreign (set DEPLOY_ADOPT_EXISTING=true to replace them)"
fi
log "intended: $(jq -c '[.[] | .kind + "/" + .name]' "$WORK/intended.json"); already present and owned: $(jq '[.[] | select(.present and .owned)] | length' "$WORK/observation.json")"

# The durable intention, before the first external change.
post_outcome applying "" "$WORK/intended.json"
[ "$RT_STATUS" = 200 ] || rt_fail "recording the intended objects"

set +e
kc -n "$NAMESPACE" apply -f "$WORK/apply-list.json" --field-manager=kodama-deploy >"$WORK/apply.out" 2>"$WORK/apply.err"
apply_rc=$?
set -e
# kubectl's own words are logged for people; they are never evidence of anything.
sed 's/^/kubectl: /' "$WORK/apply.out" >&2 || true
if [ "$apply_rc" -ne 0 ]; then
  tail_text=$(head -c 400 "$WORK/apply.err" | tr '\n' ' ')
  if grep -qiE 'forbidden|unauthorized|is invalid|no matches for kind|admission webhook|denied the request|immutable|error validating|invalid value|unsupported' "$WORK/apply.err"; then
    # A definite refusal. Some objects may still have been applied: report what exists.
    observe_all "$WORK/intended.json" "$WORK/observation.json" || echo '[]' >"$WORK/observation.json"
    observed_objects "$WORK/observation.json" >"$WORK/final-objects.json"
    post_outcome failed apply_rejected "$WORK/final-objects.json"
    [ "$RT_STATUS" = 200 ] || die outcome_unacknowledged "the apply was rejected but the failure was not acknowledged (HTTP $RT_STATUS)"
    die apply_rejected "kubectl apply was refused: $tail_text"
  fi
  # No definite answer: the apply may have taken effect. Record that fact and stop;
  # the next attempt reconciles by observation and never repeats the apply.
  post_outcome unknown "" ""
  die apply_unknown "kubectl apply gave no definite answer: $tail_text"
fi

settle_ready "$WORK/intended.json"
