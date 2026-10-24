package kit

import (
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/entrypoint"
)

// The deterministic test-only agent. It is the wrapper of the one catalogue
// tuple the test installation qualifies: a /bin/sh program in the pinned
// image that plays the part a real harness plays toward the control plane —
// it reads its own runtime token, makes streaming chat-completion requests
// through the model gateway, calls tools through the MCP proxy and writes the
// structured result — and does nothing else. Its behavior is chosen by the
// run's inputs (KODAMA_INPUT_*), so one pinned profile serves every scenario:
//
//	outcome     completed (default) | needs_input | failed | none (exit 0, no result)
//	model_calls how many chat-completion requests to make (default 1)
//	tool_calls  how many tools/call requests to make (default 1)
//	tool        the tool name to call (default echo)
//	max_tokens  the max_tokens each request asks for (default 16)
//	hold        seconds to keep running after the calls (before the result), so a
//	            test can act on the live attempt (its token is /kodama/identity/token,
//	            its MCP session /tmp/mcp-session)
//	tool        one tool name, or a comma list the calls cycle through
//	probe       a public-API path the agent POSTs to with its own runtime token
//	            (the front forwards /api/v1/ so the refusal is the server's, not
//	            the network's); recorded as p<status>
//
// The recorded trace (one token per call: m<status> for a model request,
// t<status> for a tool call) is the step's `trace` output, so a test reads what
// the control plane answered. It proves the local control-plane behavior with
// deterministic responses; it is not an OpenCode wrapper and establishes no
// live-provider, wrapper-image or release acceptance (T-085).
const (
	WrapperID      = "t038-stub-wrapper"
	HarnessID      = "stub-harness"
	HarnessVersion = "1.0.0"
	// ModelName is the one model of the test provider.
	ModelName = "t038-model"
)

// WrapperVersion is the entrypoint version: runtime admission requires the
// profile's pinned wrapper version to equal the version the step entrypoint
// reports (the entrypoint is the wrapper's runtime contract).
var WrapperVersion = entrypoint.Version

// Instructions is the profile's instruction text: for the stub harness it is
// the program itself (the pinned image holds no wrapper binary, and a catalogue
// command item is at most 512 bytes, so the wrapper command below only decodes
// the pinned profile payload and runs its instructions). origin is the
// plain-HTTP origin of the workload front (gateway and MCP paths only).
func Instructions(origin string) string {
	return strings.ReplaceAll(strings.ReplaceAll(agentTemplate, "@ORIGIN@", origin), "@MODEL@", ModelName)
}

// WrapperCommand is the catalogue tuple's command: it joins the base64url
// chunks that follow the payload marker, decodes the canonical payload JSON,
// extracts the profile's instructions and runs them. The marker is $0.
func WrapperCommand() []string {
	return []string{"/bin/sh", "-c", bootstrap}
}

const bootstrap = `c=;for a in "$@";do c=$c$a;done;while [ $((${#c}%4)) != 0 ];do c=$c=;done;printf %s "$c"|tr _- /+|base64 -d|tee /tmp/payload.json|sed -E 's/.*"instructions":"(([^"\\]|\\.)*)".*/\1/;s/\\"/"/g;s/\\n/\n/g'>/tmp/agent.sh;exec /bin/sh /tmp/agent.sh`

const agentTemplate = `set -u
API=@ORIGIN@
TOKEN=$(cat /kodama/identity/token)
ATT=$KODAMA_ATTEMPT_ID
OUTCOME=${KODAMA_INPUT_OUTCOME:-completed}
MC=${KODAMA_INPUT_MODEL_CALLS:-1}
TC=${KODAMA_INPUT_TOOL_CALLS:-1}
TOOL=${KODAMA_INPUT_TOOL:-echo}
MAXTOK=${KODAMA_INPUT_MAX_TOKENS:-16}
REG=${KODAMA_INPUT_REGISTRATION:-}
PROBE=${KODAMA_INPUT_PROBE:-}
HOLD=${KODAMA_INPUT_HOLD:-0}
TRACE=""
status_of() { grep -o 'HTTP/[0-9.]* [0-9]*' "$1" | tail -1 | cut -d' ' -f2; }
echo "agent: attempt $ATT outcome=$OUTCOME model_calls=$MC tool_calls=$TC"
i=0
while [ "$i" -lt "$MC" ]; do
  i=$((i+1))
  rm -f /tmp/model.out /tmp/model.hdr
  BODY='{"model":"@MODEL@","stream":true,"max_tokens":'$MAXTOK',"messages":[{"role":"user","content":"implement the work item, request '$i'"}]}'
  wget -q -S -O /tmp/model.out -T 30 --header "Authorization: Bearer $TOKEN" --header 'Content-Type: application/json' --post-data "$BODY" "$API/gateway/v1/attempts/$ATT/chat/completions" 2>/tmp/model.hdr
  st=$(status_of /tmp/model.hdr)
  if [ "$st" = "200" ] && [ -s /tmp/model.out ] && grep -q 'DONE' /tmp/model.out; then echo "agent: model request $i ok"; else
    if [ "$st" = "200" ]; then st=200x; fi
    echo "agent: model request $i answered ${st:-none}"; head -c 300 /tmp/model.out; cat /tmp/model.hdr; echo; fi
  TRACE="$TRACE,m${st:-0}"
done
mcp() { wget -q -S -O "$1" -T 30 --header "Authorization: Bearer $TOKEN" --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' ${SID:+--header "Mcp-Session-Id: $SID"} --post-data "$2" "$API/internal/mcp/v1/attempts/$ATT/servers/$REG" 2>"$3"; }
if [ "$TC" -gt 0 ]; then
  SID=""
  mcp /tmp/mcp.out '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t038-stub","version":"1"}}}' /tmp/mcp.hdr
  ist=$(status_of /tmp/mcp.hdr)
  SID=$(grep -i '^ *mcp-session-id:' /tmp/mcp.hdr | tail -1 | cut -d: -f2 | tr -cd '[:alnum:]_.-')
  echo "$SID" > /tmp/mcp-session
  echo "agent: mcp initialize ${ist:-none} session=${SID:+set}"
  TRACE="$TRACE,i${ist:-0}"
  if [ -n "$SID" ]; then
    mcp /dev/null '{"jsonrpc":"2.0","method":"notifications/initialized"}' /dev/null
    j=0
    while [ "$j" -lt "$TC" ]; do
      j=$((j+1))
      NTOOLS=$(echo "$TOOL" | tr ',' ' ' | wc -w)
      NAME=$(echo "$TOOL" | cut -d, -f$(( (j-1) % NTOOLS + 1 )))
      case "$NAME" in
        echo) ARGS='{"text":"call '$j'"}';;
        mutate) ARGS='{"key":"k'$j'"}';;
        *) ARGS='{}';;
      esac
      mcp /tmp/call.out '{"jsonrpc":"2.0","id":'$((j+1))',"method":"tools/call","params":{"name":"'$NAME'","arguments":'"$ARGS"'}}' /tmp/call.hdr
      cst=$(status_of /tmp/call.hdr)
      verdict=ok
      if [ "$cst" = "200" ] && grep -q '"isError":true' /tmp/call.out; then verdict=err; fi
      if [ "$cst" = "200" ] && grep -q '"error":{' /tmp/call.out; then verdict=rpcerr; fi
      echo "agent: tool call $j answered ${cst:-none} $verdict"; head -c 300 /tmp/call.out; echo
      case $verdict in ok) TRACE="$TRACE,t${cst:-0}";; *) TRACE="$TRACE,t${cst:-0}$verdict";; esac
      rm -f /tmp/call.out /tmp/call.hdr
    done
  fi
fi
if [ -n "$PROBE" ]; then
  rm -f /tmp/probe.hdr
  wget -q -S -O /dev/null -T 30 --header "Authorization: Bearer $TOKEN" --header 'Content-Type: application/json' --post-data '{}' "$API$PROBE" 2>/tmp/probe.hdr
  pst=$(status_of /tmp/probe.hdr)
  echo "agent: probe $PROBE answered ${pst:-none}"
  TRACE="$TRACE,p${pst:-0}"
fi
TRACE=${TRACE#,}
echo "agent: trace $TRACE"
echo "$TRACE" > /tmp/trace
if [ "$HOLD" -gt 0 ]; then echo "agent: holding $HOLD s"; sleep "$HOLD"; fi
case "$OUTCOME" in
  completed) printf '{"outcome":"completed","outputs":{"trace":"%s","summary":"deterministic agent finished"}}' "$TRACE" > "$KODAMA_RESULTS_FILE";;
  needs_input) printf '{"outcome":"needs_input","question":"Which warehouse owns the reconciliation adjustment?"}' > "$KODAMA_RESULTS_FILE";;
  failed) printf '{"outcome":"failed","reason":"command_failed"}' > "$KODAMA_RESULTS_FILE";;
  none) :;;
esac
exit 0
`

// Origin formats the front's origin for the script.
func Origin(host string, port int) string { return fmt.Sprintf("http://%s:%d", host, port) }
