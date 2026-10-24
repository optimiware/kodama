#!/usr/bin/env bash
# Shared functions of the ordinary deployment scripts (task T-065). Sourced by
# apply.sh, lookup.sh and delete.sh; never executed on its own.
#
# This is not a deployment engine. The scripts are ordinary commands of an
# ordinary step: they call the deployment record interface with the attempt's own
# runtime token (internal/delivery, api/delivery.yaml), talk to one namespace
# through the project's namespace-scoped kubeconfig and report what they
# *observed*. Exit status and output text are never evidence of readiness; only
# a structured observation of the live objects is.
#
# Contract summary (the full contract is images/deployment-tools/README.md):
#   * a script exits 0 when a result manifest ($KODAMA_RESULTS_FILE) was durably
#     written, whether its outcome is "completed" or "failed" (so the failure
#     reason code reaches the control plane); it exits 2 on misuse and 3 when it
#     cannot write the manifest;
#   * logs go to stderr, never contain the token or kubeconfig content.

[ -n "${KODAMA_DEPLOY_LIB_LOADED:-}" ] && return 0
KODAMA_DEPLOY_LIB_LOADED=1

umask 077
export LC_ALL=C
: "${TMPDIR:=/tmp}"
export TMPDIR
# A read-only root filesystem leaves only the scratch directory writable.
export KUBECACHEDIR="${KUBECACHEDIR:-$TMPDIR/.kube-cache}"
export KUBECTL_EXTERNAL_DIFF=
unset KUBE_EDITOR 2>/dev/null || true

WORK=""
RESULT_WRITTEN=0

log() { printf '%s %s: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${TOOL:-deploy}" "$*" >&2; }

usage_error() {
  log "usage error: $*"
  exit 2
}

cleanup_work() { [ -n "$WORK" ] && rm -rf -- "$WORK"; return 0; }

# ---------------------------------------------------------------- results ----

# write_manifest <json>: atomically replaces the results manifest.
write_manifest() {
  [ -n "${KODAMA_RESULTS_FILE:-}" ] || usage_error "KODAMA_RESULTS_FILE is required"
  local tmp="$KODAMA_RESULTS_FILE.tmp.$$"
  if ! printf '%s\n' "$1" >"$tmp" 2>/dev/null || ! mv -f -- "$tmp" "$KODAMA_RESULTS_FILE" 2>/dev/null; then
    rm -f -- "$tmp" 2>/dev/null || true
    log "cannot write the results manifest $KODAMA_RESULTS_FILE"
    exit 3
  fi
  RESULT_WRITTEN=1
}

# die <reason-code> <message>: the step failed with a reason code (^[a-z][a-z0-9_]{0,63}$).
# The manifest carries the reason and the process exits 0: a failed command is
# reported through the result, exactly as `kodama step` expects.
die() {
  local reason=$1
  shift
  log "failed: $reason: $*"
  if [ "$RESULT_WRITTEN" = 0 ]; then
    write_manifest "$(jq -cn --arg r "$reason" '{outcome:"failed",reason:$r}')"
  fi
  exit 0
}

# emit_outputs <json object>: writes the completed manifest. DEPLOY_OUTPUTS (a
# comma separated list of names) restricts the outputs to those the step
# declares; an undeclared output is refused by the entrypoint.
emit_outputs() {
  local all=$1 want="${DEPLOY_OUTPUTS:-}"
  local sel missing
  if [ -n "$want" ]; then
    missing=$(jq -rn --arg w "$want" --argjson a "$all" '($w|split(",")|map(select(length>0)))-($a|keys)|join(",")')
    [ -z "$missing" ] || die outputs_unknown "DEPLOY_OUTPUTS names outputs this script does not produce: $missing"
    sel=$(jq -c --arg w "$want" '($w|split(",")|map(select(length>0))) as $n | with_entries(select(.key as $k | $n|index($k)))' <<<"$all")
  else
    sel=$all
  fi
  write_manifest "$(jq -cn --argjson o "$sel" '{outcome:"completed",outputs:$o}')"
}

# ------------------------------------------------------------------- setup ----

# init_common: validates the environment shared by every script.
init_common() {
  command -v jq >/dev/null || usage_error "jq is required"
  command -v curl >/dev/null || usage_error "curl is required"
  command -v kubectl >/dev/null || usage_error "kubectl is required"

  [ -n "${KODAMA_RESULTS_FILE:-}" ] || usage_error "KODAMA_RESULTS_FILE is required"
  WORK=$(mktemp -d "$TMPDIR/kodama-deploy.XXXXXX") || usage_error "no writable scratch directory ($TMPDIR)"
  trap cleanup_work EXIT
  rm -f -- "$KODAMA_RESULTS_FILE" 2>/dev/null || true

  SERVER_URL=${KODAMA_SERVER_URL:-}
  [ -n "$SERVER_URL" ] || usage_error "KODAMA_SERVER_URL is required (the entrypoint does not export it; set it in the step's command.env)"
  SERVER_URL=${SERVER_URL%/}
  case "$SERVER_URL" in
    https://*) ;;
    http://127.0.0.1 | http://127.0.0.1:* | http://localhost | http://localhost:* | 'http://[::1]' | 'http://[::1]:'*) ;;
    *) usage_error "KODAMA_SERVER_URL must be https (plain http only for loopback)" ;;
  esac
  ATTEMPT_ID=${KODAMA_ATTEMPT_ID:-}
  [[ "$ATTEMPT_ID" =~ ^[A-Za-z0-9_.-]{1,128}$ ]] || usage_error "KODAMA_ATTEMPT_ID is required"
  TOKEN_FILE=${KODAMA_TOKEN_FILE:-/kodama/identity/token}
  [ -f "$TOKEN_FILE" ] && [ -r "$TOKEN_FILE" ] || usage_error "the runtime token file $TOKEN_FILE is not readable"
  CA_FILE=${KODAMA_CA_FILE:-/kodama/bootstrap/ca.crt}

  # Auxiliary (sidecar) images that may run next to the verified image, each an
  # explicit digest-pinned reference. Matching is by content digest.
  ALLOWED_DIGESTS=""
  local ref
  for ref in $(printf '%s' "${DEPLOY_ALLOWED_IMAGES:-}" | tr ',' ' '); do
    valid_image_ref "$ref" || die input_invalid "DEPLOY_ALLOWED_IMAGES must list digest-pinned references <repo>@sha256:<64 hex>"
    ALLOWED_DIGESTS="${ALLOWED_DIGESTS:+$ALLOWED_DIGESTS,}${ref##*@}"
  done

  KUBECONFIG=${KUBECONFIG:-${KODAMA_SECRETS_DIR:-/kodama/secrets}/KUBECONFIG}
  case "$KUBECONFIG" in *:*) usage_error "KUBECONFIG must name exactly one file" ;; esac
  export KUBECONFIG
  [ -f "$KUBECONFIG" ] && [ -r "$KUBECONFIG" ] || die kubeconfig_missing "the project's namespace-scoped kubeconfig is not delivered at $KUBECONFIG"
  # The scoped credential is the only kubectl authority; nothing else is read.
  unset KUBERNETES_SERVICE_HOST KUBERNETES_SERVICE_PORT
}

# ------------------------------------------------------- runtime interface ----

RT_STATUS=000
RT_BODY=""

# rt <METHOD> <path> [body-file]: one idempotent call of the deployment record
# interface with the attempt's current token (re-read for every request, so a
# renewal needs no restart). The token travels on stdin, never in argv. Transport
# loss, 429 and 5xx are retried; every call here is idempotent by its logical
# action. Sets RT_STATUS (000 = no answer) and RT_BODY.
rt() {
  local method=$1 path=$2 body=${3:-} n=0 max=${RT_ATTEMPTS:-5} delay=${RT_RETRY_DELAY:-1} token code
  local -a args
  RT_BODY="$WORK/rt-response.json"
  while :; do
    n=$((n + 1))
    token=$(tr -d '\r\n' <"$TOKEN_FILE" 2>/dev/null || true)
    if [ -z "$token" ]; then
      RT_STATUS=000
      return 0
    fi
    args=(--silent --show-error --max-time 30 --proto '=https,http' --request "$method"
      --header 'Accept: application/json' --header @- --output "$RT_BODY" --write-out '%{http_code}')
    [ -f "$CA_FILE" ] && args+=(--cacert "$CA_FILE")
    [ -n "$body" ] && args+=(--header 'Content-Type: application/json' --data-binary "@$body")
    : >"$RT_BODY"
    code=$(printf 'Authorization: Bearer %s\n' "$token" | curl "${args[@]}" "$SERVER_URL$path" 2>"$WORK/curl.err") || code=000
    [[ "$code" =~ ^[0-9]{3}$ ]] || code=000
    RT_STATUS=$code
    case "$code" in
      000 | 429 | 502 | 503 | 504)
        if [ "$n" -lt "$max" ]; then
          sleep $((delay * n))
          continue
        fi
        ;;
    esac
    return 0
  done
}

rt_error_code() { jq -r '.error.code // empty' "$RT_BODY" 2>/dev/null || true; }

# rt_fail <what>: dies with the reason code that matches the failed call.
rt_fail() {
  local what=$1 code
  code=$(rt_error_code)
  case "$RT_STATUS:$code" in
    000:* | 429:* | 502:* | 503:* | 504:*) die callback_unavailable "$what: the deployment record interface did not answer (HTTP $RT_STATUS)" ;;
    401:* | 403:*) die callback_denied "$what: refused (${code:-HTTP $RT_STATUS})" ;;
    404:*) die deployment_not_found "$what: not found" ;;
    409:outcome_unknown) die outcome_unknown "$what: the previous apply has an unknown outcome; reconcile it first" ;;
    409:*) die callback_conflict "$what: conflicts with the recorded state" ;;
    *) die callback_rejected "$what: rejected (HTTP $RT_STATUS ${code:-})" ;;
  esac
}

# load_receipt: reads the receipt in RT_BODY into globals.
load_receipt() {
  jq -e '.deployment.id and .deployment.status and (.annotations|type=="object")' "$RT_BODY" >/dev/null 2>&1 ||
    die callback_rejected "the receipt is malformed"
  cp -- "$RT_BODY" "$WORK/receipt.json"
  RECEIPT="$WORK/receipt.json"
  DEP_ID=$(jq -r '.deployment.id' "$RECEIPT")
  DEP_STATUS=$(jq -r '.deployment.status' "$RECEIPT")
  DEP_REASON=$(jq -r '.deployment.reason // ""' "$RECEIPT")
  DEP_UNKNOWN=$(jq -r '.deployment.outcomeUnknown // false' "$RECEIPT")
  NAMESPACE=$(jq -r '.deployment.namespace' "$RECEIPT")
  CONFIG_DIGEST=$(jq -r '.deployment.configurationDigest' "$RECEIPT")
  IMAGE_DIGEST=$(jq -r '.deployment.image.digest' "$RECEIPT")
  SOURCE_COMMIT=$(jq -r '.deployment.source.commit' "$RECEIPT")
  [[ "$DEP_ID" =~ ^[A-Za-z0-9_.-]{1,128}$ ]] || die callback_rejected "the receipt names an invalid deployment id"
  [[ "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || die callback_rejected "the receipt names an invalid namespace"
  # The receipt's annotations are the reconciliation identity of every object.
  ANNOTATIONS=$(jq -c '.annotations' "$RECEIPT")
  # The annotation whose value is the deployment id tells another deployment's
  # object from an unmanaged one.
  DEP_ANNOTATION_KEY=$(jq -r --arg id "$DEP_ID" '.annotations|to_entries|map(select(.value==$id))|.[0].key // ""' "$RECEIPT")
  [ -n "$DEP_ANNOTATION_KEY" ] || die callback_rejected "the receipt carries no deployment annotation"
}

# post_outcome <state> [reason] [objects-json-file]
post_outcome() {
  local state=$1 reason=${2:-} objects=${3:-} body="$WORK/outcome-body.json"
  if [ -n "$objects" ]; then
    jq -c --arg s "$state" --arg r "$reason" '{state:$s} + (if $r != "" then {reason:$r} else {} end) + {objects:.}' "$objects" >"$body"
  else
    jq -cn --arg s "$state" --arg r "$reason" '{state:$s} + (if $r != "" then {reason:$r} else {} end)' >"$body"
  fi
  rt POST "/runtime/v1/attempts/$ATTEMPT_ID/deployments/$DEP_ID/outcome" "$body"
}

# ---------------------------------------------------------------- cluster ----

kc() { kubectl --request-timeout=30s "$@"; }

# READY_JQ decides readiness from the live object's structured status. It is
# the only definition of "ready" in these scripts.
READY_JQ='
def gen_ok: (.metadata.generation // 0) <= (.status.observedGeneration // 0);
def ready:
  if .kind == "Deployment" then
    gen_ok and (.spec.replicas // 1) == (.status.updatedReplicas // 0)
    and (.spec.replicas // 1) == (.status.readyReplicas // 0)
    and (.spec.replicas // 1) == (.status.replicas // 0)
  elif .kind == "StatefulSet" then
    gen_ok and (.spec.replicas // 1) == (.status.updatedReplicas // 0)
    and (.spec.replicas // 1) == (.status.readyReplicas // 0)
  elif .kind == "DaemonSet" then
    gen_ok and (.status.desiredNumberScheduled // 0) == (.status.numberReady // 0)
    and (.status.desiredNumberScheduled // 0) == (.status.updatedNumberScheduled // 0)
  elif .kind == "ReplicaSet" then
    (.spec.replicas // 1) == (.status.readyReplicas // 0)
  elif .kind == "Job" then
    (.status.succeeded // 0) >= (.spec.completions // 1)
  elif .kind == "Pod" then
    .status.phase == "Running" and ((.status.conditions // []) | any(.type == "Ready" and .status == "True"))
  elif .kind == "PersistentVolumeClaim" then
    .status.phase == "Bound"
  else true end;
'

# IMAGES_JQ is the application-container/image binding. A pod spec is any object
# holding containers, initContainers or ephemeralContainers arrays (Pod, Deployment,
# StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, and custom resources that embed
# one). Every image in them must carry the content digest of the receipt's verified
# image, or a digest the step explicitly allows (DEPLOY_ALLOWED_IMAGES). A tag or a
# reference without a digest is never bound.
IMAGES_JQ='
def images: [.. | objects | (.containers?, .initContainers?, .ephemeralContainers?) | arrays[] | objects | .image? | strings];
def digest: ((capture("@(?<d>sha256:[0-9a-f]{64})$") | .d)? // null);
def okimg($v; $ok): digest as $d | ($d != null and (($d == $v) or ($ok | any(. == $d))));
'

# observe_ref <apiVersion> <kind> <name>: prints the live object as JSON, or
# nothing when it is absent. Returns 1 when the cluster could not answer;
# "absent" is never inferred from an error (the class is in kc_error_class).
observe_ref() {
  local out
  if ! out=$(jq -cn --arg a "$1" --arg k "$2" --arg n "$3" '{apiVersion:$a,kind:$k,metadata:{name:$n}}' |
    kc -n "$NAMESPACE" get -f - --ignore-not-found -o json 2>"$WORK/kc.err"); then
    return 1
  fi
  # `get -f` answers with a List (empty when the object is absent).
  printf '%s' "$out" | jq -c 'if (.kind // "") == "List" then (.items[0] // empty) else . end'
}

OBS_ERR=""
# kc_error_class: the reason code that matches the last kubectl failure.
kc_error_class() {
  if grep -qiE 'forbidden|unauthorized' "$WORK/kc.err" 2>/dev/null; then echo cluster_forbidden; else echo cluster_unavailable; fi
}

# observe_all <refs-json-file> <out-file>: one record per reference:
#   {apiVersion,kind,namespace,name,present,owned,holder,uid,rv,bound,hasv,ready}
# owned: every receipt annotation matches the live object; holder: the
# deployment id the live object claims (null when unmanaged); bound: every
# container image of the live object is the verified image or an allowed one;
# hasv: at least one container of the live object runs the verified image (the
# deployment-level rule, see image_drift in apply.sh, is that one owned workload does).
observe_all() {
  local refs=$1 out=$2 n i live a k nm
  n=$(jq 'length' "$refs")
  : >"$out.lines"
  for ((i = 0; i < n; i++)); do
    a=$(jq -r ".[$i].apiVersion" "$refs")
    k=$(jq -r ".[$i].kind" "$refs")
    nm=$(jq -r ".[$i].name" "$refs")
    if ! live=$(observe_ref "$a" "$k" "$nm"); then
      OBS_ERR=$(kc_error_class)
      return 1
    fi
    if [ -z "$live" ]; then
      jq -cn --arg a "$a" --arg k "$k" --arg ns "$NAMESPACE" --arg n "$nm" \
        '{apiVersion:$a,kind:$k,namespace:$ns,name:$n,present:false,owned:false,holder:null,uid:null,rv:null,bound:true,hasv:false,ready:false}' >>"$out.lines"
    else
      jq -c --arg ns "$NAMESPACE" --argjson ann "$ANNOTATIONS" --arg key "$DEP_ANNOTATION_KEY" \
        --arg v "${IMAGE_DIGEST:-}" --arg allowed "${ALLOWED_DIGESTS:-}" "$READY_JQ$IMAGES_JQ"'
        ($allowed | split(",") | map(select(length > 0))) as $ok
        | (.metadata.annotations // {}) as $have
        | {apiVersion:.apiVersion,kind:.kind,namespace:$ns,name:.metadata.name,present:true,
           owned:($ann|to_entries|all($have[.key] == .value)),
           holder:($have[$key] // null),uid:.metadata.uid,rv:.metadata.resourceVersion,
           bound:(images | all(okimg($v; $ok))),hasv:(images | any(digest == $v)),ready:ready}' <<<"$live" >>"$out.lines"
    fi
  done
  jq -s '.' "$out.lines" >"$out"
}

# refs_of <receipt-objects|file>: identity-only references (no uid/ready).
refs_from_receipt() {
  jq -c '[.deployment.objects[] | {apiVersion,kind,namespace,name}]' "$RECEIPT"
}

# observed_objects <observation-file> <only-present-owned:0|1>: the object list
# of an outcome report: identity plus the observed uid and (when true) ready;
# nothing is reported ready unless the image binding holds (every container bound
# and the verified image present in at least one owned workload).
observed_objects() {
  jq -c '(any(.hasv)) as $verified | [.[] | select(.present and .owned) | {apiVersion,kind,namespace,name,uid} + (if .ready and .bound and $verified then {ready:true} else {} end)]' "$1"
}

# ---------------------------------------------------------------- rendering ----

# Kinds that are cluster scoped. A namespace-scoped credential cannot create
# them anyway; refusing here fails early with a precise reason and keeps the
# recorded intent honest.
CLUSTER_SCOPED_KINDS='["Namespace","Node","PersistentVolume","StorageClass","ClusterRole","ClusterRoleBinding","CustomResourceDefinition","APIService","PriorityClass","IngressClass","RuntimeClass","CSIDriver","CSINode","VolumeAttachment","ValidatingWebhookConfiguration","MutatingWebhookConfiguration","ValidatingAdmissionPolicy","ValidatingAdmissionPolicyBinding","MutatingAdmissionPolicy","MutatingAdmissionPolicyBinding","PodSecurityPolicy","TokenReview","SubjectAccessReview","CertificateSigningRequest","ClusterTrustBundle"]'

# render_manifests <path> <image-ref> <namespace-or-empty> <annotations-json> <out-file>
# Produces the JSON array of objects to apply: the image token replaced, every
# receipt annotation set, the namespace pinned to the deployment's. Parsing is
# local (no cluster access). Refuses another namespace, cluster-scoped kinds,
# generateName and nameless objects.
render_manifests() {
  local src=$1 image=$2 ns=$3 ann=$4 out=$5
  local -a files=() ann_args=()
  if [ -d "$src" ]; then
    while IFS= read -r f; do files+=("$f"); done < <(find "$src" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.json' \) | LC_ALL=C sort)
  elif [ -f "$src" ]; then
    files=("$src")
  else
    die manifest_invalid "DEPLOY_MANIFESTS does not name a file or directory"
  fi
  [ "${#files[@]}" -gt 0 ] || die manifest_invalid "no manifests found"
  while IFS= read -r line; do ann_args+=("$line"); done < <(jq -r 'to_entries[] | "\(.key)=\(.value)"' <<<"$ann")
  local f
  for f in "${files[@]}"; do
    cat -- "$f"
    printf '\n---\n'
  done | sed "s|@@IMAGE@@|$image|g" >"$WORK/manifests.yaml"
  if ! kubectl annotate --local -f "$WORK/manifests.yaml" -o json --overwrite "${ann_args[@]}" >"$WORK/rendered.stream" 2>"$WORK/render.err"; then
    die manifest_invalid "the manifests could not be parsed: $(head -c 300 "$WORK/render.err" | tr '\n' ' ')"
  fi
  jq -s --arg ns "$ns" '
    map(if ((.kind // "") | endswith("List")) and (.items | type == "array") then .items[] else . end)
    | map(if ($ns != "" and ((.metadata.namespace // "") == "")) then .metadata.namespace = $ns else . end)' \
    "$WORK/rendered.stream" >"$out"
  local problem
  problem=$(jq -r --arg ns "$ns" --argjson cluster "$CLUSTER_SCOPED_KINDS" '
    if length == 0 then "no objects"
    elif length > 64 then "more than 64 objects"
    else
      (map(select((.apiVersion // "") == "" or (.kind // "") == "" or ((.metadata.name // "") == "")))
        | if length > 0 then "an object has no apiVersion, kind or name (generateName is not supported)" else empty end),
      (map(select(.kind as $k | $cluster | index($k))) | if length > 0 then "cluster-scoped object " + (.[0].kind) else empty end),
      (if $ns != "" then (map(select(.metadata.namespace != $ns)) | if length > 0 then "object " + (.[0].metadata.name) + " names namespace " + (.[0].metadata.namespace // "") + " instead of " + $ns else empty end) else empty end),
      ((map(.apiVersion + "|" + .kind + "|" + .metadata.name) | group_by(.) | map(select(length > 1)) | if length > 0 then "an object is listed twice" else empty end))
    end' "$out" | head -n 1)
  if [ -n "$problem" ]; then
    case "$problem" in
      cluster-scoped*) die manifest_cluster_scoped "$problem" ;;
      object*namespace*) die manifest_namespace "$problem" ;;
      *) die manifest_invalid "$problem" ;;
    esac
  fi
}

# refs_from_rendered <rendered-file>: sorted identity references.
refs_from_rendered() {
  jq -c 'map({apiVersion,kind,namespace:.metadata.namespace,name:.metadata.name}) | sort_by(.apiVersion+.kind+.name)' "$1"
}

valid_image_ref() { [[ "$1" =~ ^[a-z0-9][A-Za-z0-9._/:-]{0,254}@sha256:[0-9a-f]{64}$ ]]; }
valid_digest() { [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]; }

# check_images <rendered-file> <verified-digest>: binds the rendered workloads to
# the verified image BEFORE anything is applied: every container image must be the
# verified digest or an allowed one, and at least one container must run the
# verified image, so success can never name an artifact that was not deployed.
check_images() {
  local file=$1 verified=$2 verdict
  verdict=$(jq -r --arg v "$verified" --arg allowed "${ALLOWED_DIGESTS:-}" "$IMAGES_JQ"'
    ($allowed | split(",") | map(select(length > 0))) as $ok
    | [.[] | (.kind + "/" + .metadata.name) as $k | images[] | {k: $k, i: .}] as $all
    | ([$all[] | select((.i | okimg($v; $ok)) | not) | .k + " runs " + .i] | .[0]) as $bad
    | if $bad != null then "bound:" + $bad
      elif ([$all[] | select(.i | digest == $v)] | length) == 0 then "none"
      else "ok" end' "$file") || die manifest_invalid "the manifests could not be inspected for images"
  case "$verdict" in
    ok) ;;
    none) die image_not_deployed "no container in the manifests runs the verified image $verified (use @@IMAGE@@)" ;;
    bound:*) die image_not_bound "a manifest runs an image that is not the verified digest or an allowed one: ${verdict#bound:}" ;;
    *) die manifest_invalid "unexpected image inspection result" ;;
  esac
}

# resource_url <apiVersion> <kind>: the namespaced collection URL of a kind, from
# the API server's own discovery document (needs no extra authority: discovery is
# readable by every authenticated identity).
resource_url() {
  local api=$1 kind=$2 prefix plural
  case "$api" in
    v1) prefix=/api/v1 ;;
    */*) prefix=/apis/$api ;;
    *) return 1 ;;
  esac
  kc get --raw "$prefix" >"$WORK/discovery.json" 2>"$WORK/kc.err" || return 1
  plural=$(jq -r --arg k "$kind" '[.resources[] | select(.kind == $k and .namespaced == true and (.name | contains("/") | not)) | .name] | .[0] // empty' "$WORK/discovery.json")
  [ -n "$plural" ] || return 2
  printf '%s/namespaces/%s/%s' "$prefix" "$NAMESPACE" "$plural"
}
