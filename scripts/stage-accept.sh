#!/usr/bin/env bash
# Inline Vite acceptance driver for the Kodama Scaleway stage (M2).
#
# Submits exactly the checked-in YAML bytes once on the task-free
# pipeline.start alternate binding, waits for success AND complete cleanup,
# pages the retained stdout log, asserts the Vite version, the build marker
# and a dist/ artifact, refuses kodex./kodsvc. credential text, and writes a
# 0600 summary. The token is read from a private file into a 0600 curl header
# file: it never appears in argv, output or the summary.
#
# Required inputs:
#   KODAMA_STAGE_API_URL          https public API base URL
#   KODAMA_STAGE_CA_FILE          readable public CA file
#   KODAMA_STAGE_ADMIN_TOKEN_FILE mode 0600 token file
#   KODAMA_STAGE_EVIDENCE_DIR     absolute private evidence directory
#   KODAMA_STAGE_RUN_ID           bounded per-run acceptance identity; required
#                                 unless Q01_DEPLOYED_REVISION supplies the
#                                 run discriminator (or replay is allowed)
# Optional:
#   Q01_DEPLOYED_REVISION         deployed revision recorded in the summary
#   KODAMA_STAGE_REVISION         explicit revision alias, wins over Q01_...
#   KODAMA_STAGE_ALLOW_REPLAY     set to 1 to accept a replayed execution and
#                                 to reuse the YAML-only key when no run or
#                                 revision identity is available
#   KODAMA_STAGE_PROJECT_ID       project id (default tenant-a)
#   KODAMA_STAGE_PIPELINE_FILE    inline YAML (default deploy/stage/vite-acceptance.yaml)
#   KODAMA_STAGE_ACCEPT_TIMEOUT   overall poll bound in seconds (default 900)
#   KODAMA_STAGE_IMAGE_DIGEST     optional server image digest recorded in the summary
#
# The submit idempotency key is derived from the pipeline YAML hash and a
# run/revision discriminator, so a new deployed revision or run never replays
# the previous execution. A submit response carrying replayed=true fails
# unless KODAMA_STAGE_ALLOW_REPLAY=1.

set -euo pipefail
set +x
umask 077

readonly VITE_VERSION='8.3.6'
readonly BUILD_MARKER='KODAMA_VITE_OK'
readonly POLL_INTERVAL="${KODAMA_STAGE_ACCEPT_POLL_SECONDS:-5}"
readonly POLL_TIMEOUT="${KODAMA_STAGE_ACCEPT_TIMEOUT:-900}"
readonly MAX_LOG_BYTES=$((8 * 1024 * 1024))
readonly MAX_LOG_PAGES=200

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly DEFAULT_PIPELINE="$SCRIPT_DIR/../deploy/stage/vite-acceptance.yaml"

refuse() {
  printf 'refusing: %s\n' "$*" >&2
  exit 1
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "required command '$1' is not on PATH"
}

stat_mode() {
  stat -c '%a' -- "$1"
}

# --- Guards ------------------------------------------------------------------

api_url="${KODAMA_STAGE_API_URL:-}"
[[ -n "$api_url" ]] || refuse 'KODAMA_STAGE_API_URL is required'
[[ "$api_url" == https://* ]] || refuse "KODAMA_STAGE_API_URL must be an https URL, got '$api_url'"
api_url="${api_url%/}"

ca_file="${KODAMA_STAGE_CA_FILE:-}"
[[ -n "$ca_file" ]] || refuse 'KODAMA_STAGE_CA_FILE is required'
[[ -f "$ca_file" && -r "$ca_file" ]] || refuse "CA file '$ca_file' is not readable"

token_file="${KODAMA_STAGE_ADMIN_TOKEN_FILE:-}"
[[ -n "$token_file" ]] || refuse 'KODAMA_STAGE_ADMIN_TOKEN_FILE is required'
[[ -f "$token_file" && -r "$token_file" ]] || refuse "token file '$token_file' is not readable"
token_mode="$(stat_mode "$token_file")"
[[ "$token_mode" == '600' ]] || refuse "token file '$token_file' has mode $token_mode; want 0600"
[[ -s "$token_file" ]] || refuse "token file '$token_file' is empty"

project="${KODAMA_STAGE_PROJECT_ID:-tenant-a}"
[[ "$project" =~ ^[A-Za-z0-9_-]{1,64}$ ]] || refuse "KODAMA_STAGE_PROJECT_ID '$project' is not a bounded project identity"

run_id="${KODAMA_STAGE_RUN_ID:-}"
if [[ -n "$run_id" ]]; then
  [[ "$run_id" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$ ]] ||
    refuse "KODAMA_STAGE_RUN_ID '$run_id' is not a bounded run identity"
fi

allow_replay="${KODAMA_STAGE_ALLOW_REPLAY:-}"
[[ -z "$allow_replay" || "$allow_replay" == '1' ]] ||
  refuse "KODAMA_STAGE_ALLOW_REPLAY must be '1' when set, got '$allow_replay'"

revision="${KODAMA_STAGE_REVISION:-${Q01_DEPLOYED_REVISION:-}}"
if [[ -n "$revision" ]]; then
  [[ "$revision" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$ ]] ||
    refuse "deployed revision '$revision' is not a bounded revision identity"
fi
[[ -n "$run_id" || -n "$revision" || "$allow_replay" == '1' ]] ||
  refuse 'KODAMA_STAGE_RUN_ID or Q01_DEPLOYED_REVISION is required so the acceptance key is per revision (set KODAMA_STAGE_ALLOW_REPLAY=1 to reuse the YAML-only key)'

image_digest="${KODAMA_STAGE_IMAGE_DIGEST:-}"

pipeline="${KODAMA_STAGE_PIPELINE_FILE:-$DEFAULT_PIPELINE}"
[[ -f "$pipeline" && -r "$pipeline" ]] || refuse "pipeline file '$pipeline' is not readable"

evidence="${KODAMA_STAGE_EVIDENCE_DIR:-}"
[[ -n "$evidence" ]] || refuse 'KODAMA_STAGE_EVIDENCE_DIR is required'
[[ "$evidence" == /* ]] || refuse "KODAMA_STAGE_EVIDENCE_DIR must be an absolute path, got '$evidence'"
[[ "$evidence" != '/' ]] || refuse 'KODAMA_STAGE_EVIDENCE_DIR must not be the filesystem root'
[[ ! -e "$evidence" || -d "$evidence" ]] || refuse "KODAMA_STAGE_EVIDENCE_DIR '$evidence' is not a directory"

[[ "$POLL_INTERVAL" =~ ^[1-9][0-9]*$ ]] || refuse 'KODAMA_STAGE_ACCEPT_POLL_SECONDS must be a positive integer'
[[ "$POLL_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || refuse 'KODAMA_STAGE_ACCEPT_TIMEOUT must be a positive integer'

require_cmd curl
require_cmd python3
require_cmd sha256sum

# --- Private working files ---------------------------------------------------

mkdir -p -- "$evidence"
chmod 700 -- "$evidence"

header_file="$evidence/.auth-header"
submit_body="$evidence/.submit-body.json"
submit_response="$evidence/.submit-response.json"
inspect_body="$evidence/.inspect-body.json"
inspect_response="$evidence/.inspect-response.json"
logs_body="$evidence/.logs-body.json"
logs_response="$evidence/.logs-response.json"
log_file="$evidence/vite.log"
summary_file="$evidence/stage-accept-summary.json"

cleanup() {
  rm -f -- "$header_file" "$submit_body" "$submit_response" \
    "$inspect_body" "$inspect_response" "$logs_body" "$logs_response"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

token="$(cat "$token_file")"
[[ -n "$token" ]] || refuse "token file '$token_file' is empty"
printf 'Authorization: Bearer %s\n' "$token" >"$header_file"
printf '{"input":{}}\n' >"$inspect_body"

yaml_sha="$(sha256sum -- "$pipeline" | awk '{print $1}')"
readonly yaml_sha
if [[ -n "$run_id" || -n "$revision" ]]; then
  discriminator="run=${run_id:-none};revision=${revision:-none}"
  discriminator_sha="$(printf '%s\n' "$discriminator" | sha256sum | awk '{print $1}')"
  [[ "$discriminator_sha" =~ ^[a-f0-9]{64}$ ]] || fail 'could not hash the acceptance run identity'
  readonly discriminator_sha
  readonly idempotency_key="vite-accept-$yaml_sha-${discriminator_sha:0:32}"
else
  readonly idempotency_key="vite-accept-$yaml_sha"
fi
readonly started_epoch="$(date +%s)"

# --- Submit exactly once -----------------------------------------------------

python3 - "$pipeline" "$submit_body" "$project" "$idempotency_key" <<'PY'
import base64
import json
import sys

with open(sys.argv[1], "rb") as source:
    document = source.read()
envelope = {
    "input": {
        "project_id": sys.argv[3],
        "yaml": base64.b64encode(document).decode("ascii"),
    },
    "idempotency_key": sys.argv[4],
}
with open(sys.argv[2], "w", encoding="utf-8") as target:
    json.dump(envelope, target, separators=(",", ":"))
PY

submit_code=""
if ! submit_code="$(curl --silent --show-error --proto '=https' --max-time 60 \
  --cacert "$ca_file" --header @"$header_file" \
  --header 'Content-Type: application/json' \
  --data-binary @"$submit_body" \
  --output "$submit_response" --write-out '%{http_code}' \
  "$api_url/api/v1/projects/$project/executions")"; then
  fail "pipeline.start submit request failed (see $evidence)"
fi
[[ "$submit_code" =~ ^2[0-9][0-9]$ ]] ||
  fail "pipeline.start submit returned HTTP $submit_code (response: $submit_response)"

if ! submit_meta="$(python3 - "$submit_response" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    document = json.load(source)
body = document.get("body") or {}
execution_id = body.get("execution_id")
if not isinstance(execution_id, str) or not execution_id:
    raise SystemExit("submit response carries no execution_id")
replayed = document.get("replayed") is True or body.get("replayed") is True
print(execution_id + "\t" + ("true" if replayed else "false"))
PY
)"; then
  fail "could not read the execution id from the submit response"
fi
IFS=$'\t' read -r execution_id submit_replayed <<<"$submit_meta"
[[ -n "${execution_id:-}" ]] || fail 'the submit response carried an empty execution id'
if [[ "$submit_replayed" == 'true' && "$allow_replay" != '1' ]]; then
  fail "pipeline.start replayed execution $execution_id (replayed=true); refusing a replayed acceptance (set KODAMA_STAGE_ALLOW_REPLAY=1 to accept)"
fi

# --- Poll until succeeded AND cleanup complete -------------------------------

inspect_code=""
state=""
cleanup_state=""
action_id=""
while :; do
  now_epoch="$(date +%s)"
  elapsed=$((now_epoch - started_epoch))
  (( elapsed < POLL_TIMEOUT )) ||
    fail "timed out after ${POLL_TIMEOUT}s waiting for execution $execution_id (last state: ${state:-unknown}, cleanup: ${cleanup_state:-unknown})"

  if ! inspect_code="$(curl --silent --show-error --proto '=https' --max-time 60 \
    --cacert "$ca_file" --header @"$header_file" \
    --header 'Content-Type: application/json' --request GET \
    --data-binary @"$inspect_body" \
    --output "$inspect_response" --write-out '%{http_code}' \
    "$api_url/api/v1/executions/$execution_id")"; then
    fail "execution.inspect request failed for $execution_id"
  fi
  [[ "$inspect_code" =~ ^2[0-9][0-9]$ ]] ||
    fail "execution.inspect returned HTTP $inspect_code for $execution_id"

  if ! meta="$(python3 - "$inspect_response" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    body = (json.load(source).get("body") or {})
actions = body.get("actions") or []
action_id = ""
for action in actions:
    if action.get("name") == "vite-build":
        action_id = action.get("action_id") or ""
        break
if not action_id and actions:
    action_id = actions[0].get("action_id") or ""
print(body.get("state") or "", (body.get("cleanup") or {}).get("state") or "", action_id, sep="\t")
PY
)"; then
    fail "could not parse the execution.inspect response for $execution_id"
  fi
  IFS=$'\t' read -r state cleanup_state action_id <<<"$meta"

  case "$state" in
    succeeded)
      [[ "$cleanup_state" == 'complete' ]] || { sleep "$POLL_INTERVAL"; continue; }
      [[ -n "$action_id" ]] || fail "execution $execution_id succeeded without an action id"
      break
      ;;
    failed|cancelled)
      fail "execution $execution_id ended '$state' (cleanup: ${cleanup_state:-unknown})"
      ;;
  esac
  sleep "$POLL_INTERVAL"
done

# --- Read and reassemble the retained stdout log -----------------------------

: >"$log_file"
chmod 600 -- "$log_file"
cursor=""
page=0
while :; do
  page=$((page + 1))
  (( page <= MAX_LOG_PAGES )) || fail "execution $execution_id has more than $MAX_LOG_PAGES log pages"

  python3 - "$logs_body" "$action_id" "$cursor" <<'PY'
import json
import sys

page = {"limit": 100}
if sys.argv[3]:
    page["cursor"] = sys.argv[3]
with open(sys.argv[1], "w", encoding="utf-8") as target:
    # The public operations all take the frozen {input: {...}} envelope; a
    # top-level action_id/stream is a strict-decode 400 at the edge.
    json.dump({"input": {"action_id": sys.argv[2], "stream": "stdout", "page": page}}, target)
PY

  logs_code=""
  if ! logs_code="$(curl --silent --show-error --proto '=https' --max-time 60 \
    --cacert "$ca_file" --header @"$header_file" \
    --header 'Content-Type: application/json' --request GET \
    --data-binary @"$logs_body" \
    --output "$logs_response" --write-out '%{http_code}' \
    "$api_url/api/v1/executions/$execution_id/logs")"; then
    fail "execution.logs.read request failed for $execution_id"
  fi
  [[ "$logs_code" =~ ^2[0-9][0-9]$ ]] ||
    fail "execution.logs.read returned HTTP $logs_code for $execution_id"

  if ! logs_meta="$(python3 - "$logs_response" "$log_file" <<'PY'
import base64
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    body = (json.load(source).get("body") or {})
with open(sys.argv[2], "ab") as target:
    for chunk in body.get("chunks") or []:
        target.write(base64.b64decode(chunk.get("data") or ""))
print((body.get("next_cursor") or "") + "|" + ("truncated" if body.get("truncated") else "complete"))
PY
)"; then
    fail "could not reassemble the retained log for $execution_id"
  fi
  IFS='|' read -r next_cursor truncation <<<"$logs_meta"
  [[ "$truncation" == 'complete' ]] ||
    fail "retained log for $execution_id is truncated by the retention policy"

  size="$(wc -c <"$log_file")"
  (( size <= MAX_LOG_BYTES )) || fail "retained log exceeds $MAX_LOG_BYTES bytes"
  [[ -n "$next_cursor" ]] || break
  cursor="$next_cursor"
done

# --- Sanitize, assert and summarize ------------------------------------------

ended_epoch="$(date +%s)"
duration=$((ended_epoch - started_epoch))

if ! python3 - "$log_file" "$summary_file" "$project" "$api_url" "$pipeline" \
  "$yaml_sha" "$execution_id" "$action_id" "$duration" "$state" "$cleanup_state" \
  "$token_file" "$revision" "$image_digest" "$run_id" "$submit_replayed" <<'PY'
import base64
import json
import os
import sys

(log_path, summary_path, project, api_url, pipeline, yaml_sha,
 execution_id, action_id, duration, state, cleanup_state, token_path,
 revision, image_digest, run_id, submit_replayed) = sys.argv[1:17]
with open(log_path, "rb") as source:
    raw = source.read()
text = raw.decode("utf-8", "replace")


def drop_log():
    with open(log_path, "wb"):
        pass


with open(token_path, encoding="utf-8") as source:
    secret = source.read().strip()
if secret:
    if secret in text:
        drop_log()
        raise SystemExit("retained log contains the issued token")
    if base64.b64encode(secret.encode("utf-8")).decode("ascii") in text:
        drop_log()
        raise SystemExit("retained log contains the base64 form of the issued token")

for marker in ("kodex.", "kodsvc."):
    if marker in text:
        drop_log()
        raise SystemExit("retained log contains the credential marker %r" % marker)
if "8.3.6" not in text:
    raise SystemExit("retained log does not show Vite 8.3.6")
if "KODAMA_VITE_OK" not in text:
    raise SystemExit("retained log does not show the KODAMA_VITE_OK build marker")
if not any("dist/" in line for line in text.splitlines()):
    raise SystemExit("retained log has no dist/ artifact line")

lines = [line for line in text.splitlines() if line.strip()]
excerpt_lines = lines[-40:]
excerpt = "\n".join(excerpt_lines)
excerpt = "".join(character for character in excerpt
                  if character in "\n\t" or ord(character) >= 32)
excerpt = excerpt[:4000]

summary = {
    "api_url": api_url,
    "project_id": project,
    "pipeline_file": pipeline,
    "run_id": run_id,
    "revision": revision,
    "image_digest": image_digest,
    "yaml_sha256": "sha256:" + yaml_sha,
    "execution_id": execution_id,
    "action_id": action_id,
    "duration_seconds": int(duration),
    "replayed": submit_replayed == "true",
    "state": state,
    "cleanup_state": cleanup_state,
    "vite_version": "8.3.6",
    "build_marker": "KODAMA_VITE_OK",
    "sanitized_excerpt": excerpt,
}
with open(summary_path, "w", encoding="utf-8") as target:
    json.dump(summary, target, indent=2, sort_keys=True)
    target.write("\n")
os.chmod(summary_path, 0o600)
PY
then
  fail 'the retained log did not pass the Vite acceptance assertions'
fi

printf 'stage acceptance succeeded\n'
printf '  execution_id=%s\n' "$execution_id"
printf '  action_id=%s\n' "$action_id"
printf '  yaml_sha256=sha256:%s\n' "$yaml_sha"
printf '  revision=%s\n' "${revision:-unknown}"
printf '  replayed=%s\n' "$submit_replayed"
printf '  duration_seconds=%s\n' "$duration"
printf '  cleanup_state=%s\n' "$cleanup_state"
printf '  summary=%s\n' "$summary_file"
