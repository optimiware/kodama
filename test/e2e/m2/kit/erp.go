package kit

import (
	"sort"
	"strconv"
	"strings"
)

// The ERP inventory-gap pipeline (docs/examples/workflows/erp-gap-to-preview.md)
// on the Podman runtime: every step runs in the tools image (it holds git,
// curl, jq and the shipped apply/delete scripts), which is also the deterministic
// agent's image. The agent is the stub of agent.go in its curl form: it calls
// the gateway and the MCP proxy with its own runtime token, then WRITES the
// candidate into its checked-out workspace (the quality and build scripts and
// the application manifests), which it commits and the entrypoint publishes, and
// it declares its design conformance report for the candidate it committed. It
// is a fixture of the control plane's behavior, not OpenCode.
//
// The program is the profile's instructions: they travel as canonical JSON, so
// they contain no backslash and no tab.

const erpAgentTemplate = `set -eu
API=@ORIGIN@
TOKEN=$(cat /kodama/identity/token)
ATT=$KODAMA_ATTEMPT_ID
REG=$KODAMA_INPUT_REGISTRATION
echo "agent: attempt $ATT"
BODY='{"model":"@MODEL@","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"implement the work item"}]}'
code=$(curl -sS -o /tmp/model.out -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data "$BODY" "$API/gateway/v1/attempts/$ATT/chat/completions")
echo "agent: model request answered $code"
[ "$code" = 200 ] && grep -q DONE /tmp/model.out
H='Accept: application/json, text/event-stream'
MCP="$API/internal/mcp/v1/attempts/$ATT/servers/$REG"
curl -sS -D /tmp/mcp.hdr -o /tmp/mcp.out -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H "$H" --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t038-stub","version":"1"}}}' "$MCP"
SID=$(grep -i '^mcp-session-id:' /tmp/mcp.hdr | tail -1 | cut -d: -f2 | tr -cd '[:alnum:]_.-')
curl -sS -o /dev/null -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H "$H" -H "Mcp-Session-Id: $SID" --data '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$MCP"
curl -sS -o /tmp/call.out -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H "$H" -H "Mcp-Session-Id: $SID" --data '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"inventory guidance"}}}' "$MCP"
jq -e '.result.isError | not' /tmp/call.out
echo "agent: tool call ok"
cd /workspace
@FILES@git -c user.name=agent -c user.email=agent@kodama.invalid add -A
git -c user.name=agent -c user.email=agent@kodama.invalid commit -q -m "Implement the inventory gap resolution"
COMMIT=$(git rev-parse HEAD)
BASEDIGEST=$(jq -r '.content.designBaseline.digest' /tmp/payload.json)
RT=@RUNTIME@
CA=/kodama/bootstrap/ca.crt
echo "design review: module boundaries and tenant authority checked for $COMMIT" > /tmp/design-review.txt
DG=sha256:$(sha256sum /tmp/design-review.txt | cut -d' ' -f1)
SZ=$(wc -c < /tmp/design-review.txt | tr -d ' ')
rc=$(curl -sS --cacert "$CA" -o /tmp/reserve.json -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data '{"name":"design-review","kind":"file","role":"diagnostic","digest":"'$DG'","mediaType":"text/plain","sizeBytes":'$SZ',"registry":null}' "$RT/runtime/v1/attempts/$ATT/artifacts")
if [ "$rc" != 200 ] && [ "$rc" != 201 ]; then echo "agent: reserve answered $rc"; cat /tmp/reserve.json; exit 1; fi
EID=$(jq -r .artifactId /tmp/reserve.json)
UURL=$(jq -r .upload.url /tmp/reserve.json)
jq -r '.upload.headers // {} | to_entries[] | .key + ": " + .value' /tmp/reserve.json > /tmp/upload-headers
set --
while read -r line; do set -- "$@" -H "$line"; done < /tmp/upload-headers
curl -fsS -X PUT "$@" --data-binary @/tmp/design-review.txt "$UURL"
curl -fsS --cacert "$CA" -o /tmp/complete.json -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data '{"digest":"'$DG'","sizeBytes":'$SZ'}' "$RT/runtime/v1/attempts/$ATT/artifacts/$EID/complete"
echo "agent: evidence artifact $EID"
mkdir -p /kodama/outputs
REPORT=${KODAMA_INPUT_REPORT:-ok}
CC=$COMMIT
BD=$BASEDIGEST
CITE=$EID
DISP=conforms
DEV='[]'
case "$REPORT" in
  wrongcommit) CC=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;
  wrongbaseline) BD=sha256:0000000000000000000000000000000000000000000000000000000000000000;;
  blocked) DISP=blocked; DEV='[{"requirement":"AR-14","reason":"the candidate widens a module boundary"}]';;
  uncited) CITE=;;
esac
if [ "$REPORT" != missing ]; then
  jq -cn --arg b "$BD" --arg c "$CC" --arg e "$CITE" --argjson dev "$DEV" --arg disp "$DISP" '(if $e == "" then [] else [$e] end) as $ev | {baselineDigest:$b,candidateCommit:$c,requirements:["AR-01","AR-14"],checks:[{id:"module-boundaries",outcome:"passed",evidenceArtifactIds:$ev},{id:"tenant-authority",outcome:"passed",evidenceArtifactIds:$ev}],unresolvedDeviations:$dev,disposition:$disp}' > /kodama/outputs/conformance.json
fi
jq -cn --arg s "implemented in commit $COMMIT" '{outcome:"completed",outputs:{summary:$s}}' > "$KODAMA_RESULTS_FILE"
`

// CandidateFile is one file of the candidate the deterministic agent writes (and
// a human writes in the scenarios that need a candidate without an agent).
type CandidateFile struct {
	Path, Content string
	Executable    bool
}

// CandidateFiles are the quality script, the build script and the application
// manifests of the candidate. build.sh "builds" by publishing the shipped tools
// image's manifest under the project's own repository of the scope registry
// (cross-repository blob mount, then the manifest by digest) and reports it as
// the step's ociArtifact output: a deterministic builder, since no image build
// runs in a restricted step container here.
var CandidateFiles = []CandidateFile{
	{Path: "ci/quality.sh", Executable: true, Content: `#!/bin/sh
set -eu
test -f deploy/deployment.yaml
echo '{"passed":true,"checks":["application-tenant-isolation","audited-resolution","idempotent-erp-writes","warehouse-usability"]}' > /kodama/outputs/quality.json
`},
	{Path: "ci/build.sh", Executable: true, Content: `#!/bin/sh
set -eu
REG=http://$REGISTRY
ACCEPT='application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
curl -fsS -H "Accept: $ACCEPT" "$REG/v2/$SRC_REPO/manifests/$SRC_DIGEST" -o /tmp/manifest.json
CT=$(jq -r '.mediaType // "application/vnd.oci.image.manifest.v1+json"' /tmp/manifest.json)
for d in $(jq -r '.config.digest, (.layers[].digest)' /tmp/manifest.json); do
  curl -fsS -o /dev/null -X POST "$REG/v2/$DST_REPO/blobs/uploads/?mount=$d&from=$SRC_REPO"
done
curl -fsS -o /dev/null -X PUT -H "Content-Type: $CT" --data-binary @/tmp/manifest.json "$REG/v2/$DST_REPO/manifests/$SRC_DIGEST"
curl -fsS -o /dev/null -I -H "Accept: $ACCEPT" "$REG/v2/$DST_REPO/manifests/$SRC_DIGEST"
jq -cn --arg d "$SRC_DIGEST" --arg r "$REGISTRY/$DST_REPO" --arg n "$NODE_REGISTRY/$DST_REPO@$SRC_DIGEST" '{outcome:"completed",outputs:{image:{digest:$d,registryRef:$r},image_ref:$n}}' > "$KODAMA_RESULTS_FILE"
`},
	{Path: "deploy/configmap.yaml", Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: inventory-config
data:
  REPLICAS: "1"
`},
	{Path: "deploy/deployment.yaml", Content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: inventory
spec:
  replicas: 1
  selector:
    matchLabels: {app: inventory}
  template:
    metadata:
      labels: {app: inventory}
    spec:
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 1
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: app
          image: @@IMAGE@@
          imagePullPolicy: IfNotPresent
          command: ["sleep", "3600"]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
`},
}

// agentWrites renders the files as shell the agent runs in its workspace.
func agentWrites() string {
	var b strings.Builder
	for _, f := range CandidateFiles {
		if i := strings.LastIndex(f.Path, "/"); i > 0 {
			b.WriteString("mkdir -p " + f.Path[:i] + "\n")
		}
		b.WriteString("cat > " + f.Path + " <<'EOF2'\n" + f.Content + "EOF2\n")
		if f.Executable {
			b.WriteString("chmod +x " + f.Path + "\n")
		}
	}
	return b.String()
}

// ERPInstructions is the curl-form agent program (origin: the plain-HTTP front of
// the gateway and MCP paths; runtime: the TLS origin of the runtime API).
func ERPInstructions(origin, runtime string) string {
	r := strings.NewReplacer("@FILES@", agentWrites(), "@ORIGIN@", origin, "@MODEL@", ModelName, "@RUNTIME@", runtime)
	return r.Replace(erpAgentTemplate)
}

// ERPOptions parameterize the example's steps.
type ERPOptions struct {
	Profile     string
	Environment string
	Tools       Image
	// ServerURL is the runtime origin the apply/delete scripts call.
	ServerURL string
	// RepositoryURL is the project's repository (the receipt's source).
	RepositoryURL string
	// ReadyTimeout is the apply script's readiness bound in seconds.
	ReadyTimeout int
	// Env adds or overrides entries of the preview step's environment.
	Env map[string]string
}

// ERPInputs are the declared pipeline inputs.
func ERPInputs() map[string]any {
	return map[string]any{
		"registration": map[string]any{"type": "string", "default": ""},
		// report selects the conformance report the agent declares: ok (default),
		// missing, blocked, wrongcommit, wrongbaseline or uncited.
		"report": map[string]any{"type": "string", "default": ""},
	}
}

func (o ERPOptions) previewEnv() map[string]string {
	env := map[string]string{
		"KODAMA_SERVER_URL":        o.ServerURL,
		"DEPLOY_MANIFESTS":         "/workspace/deploy",
		"DEPLOY_REPOSITORY_URL":    o.RepositoryURL,
		"DEPLOY_COMMIT":            "${{ inputs.commit }}",
		"DEPLOY_IMAGE_ARTIFACT_ID": "${{ inputs.image }}",
		"DEPLOY_IMAGE":             "${{ inputs.image_ref }}",
		"DEPLOY_READY_TIMEOUT":     strconv.Itoa(o.ReadyTimeout),
		"RT_RETRY_DELAY":           "0",
		"RT_ATTEMPTS":              "3",
	}
	for k, v := range o.Env {
		env[k] = v
	}
	return env
}

// nodeRegistry is the registry as the kind nodes' containerd mirror names it.
func nodeRegistry() string {
	h := RegistryHost()
	return "localhost:" + h[strings.LastIndex(h, ":")+1:]
}

// ERPSteps renders the candidate pipeline: implement (agent writer) -> test
// (report artifact) -> review (gate over the report and the conformance
// evidence) -> build (an image registered in the scope registry).
//
// The example's own preview step, `IMAGE_ARTIFACT_ID: "${{ steps.build.outputs.image }}"`
// in the same pipeline, is refused by the pipeline validator (an ociArtifact
// reference is accepted nowhere in env or args: type_mismatch), so the preview
// is the promotion form: a second run (PreviewSteps) takes the verified image's
// artifact identifier, its reference and the candidate commit as inputs.
func (e *Env) ERPSteps(o ERPOptions) []any {
	tools := o.Tools
	read := map[string]any{"access": "read", "from": "implement"}
	step := func(id string, needs []string, extra map[string]any) map[string]any {
		st := map[string]any{"id": id, "kind": "container", "needs": needs, "image": tools.Ref(), "workspace": read, "timeoutSeconds": 300}
		for k, v := range extra {
			st[k] = v
		}
		return st
	}
	implement := e.AgentStepFor("implement", o.Profile)
	implement["workspace"] = map[string]any{"access": "write", "from": "run"}
	implement["outputs"] = map[string]any{
		"summary":     map[string]any{"type": "string"},
		"conformance": map[string]any{"type": "artifact", "path": "/kodama/outputs/conformance.json"},
	}
	test := step("test", []string{"implement"}, map[string]any{
		"command": []string{"/bin/sh", "-ec"}, "args": []string{"cd /workspace && sh ci/quality.sh"},
		"outputs": map[string]any{"report": map[string]any{"type": "artifact", "path": "/kodama/outputs/quality.json"}},
	})
	review := GateStep("review", []string{"${{ steps.test.outputs.report }}"}, "test")
	build := step("build", []string{"review"}, map[string]any{
		"command": []string{"/bin/sh", "-ec"}, "args": []string{"cd /workspace && sh ci/build.sh"},
		"env": map[string]string{
			"REGISTRY": RegistryHost(), "NODE_REGISTRY": nodeRegistry(),
			"SRC_REPO": tools.Path, "SRC_DIGEST": string(tools.Digest), "DST_REPO": RegistryRepositoryPrefix() + "/inventory",
		},
		"outputs": map[string]any{"image": map[string]any{"type": "ociArtifact"}, "image_ref": map[string]any{"type": "string"}},
	})
	return []any{implement, test, review, build}
}

// BuildSteps is the build alone, for the scenarios that need a verified image
// of a candidate a human wrote (no agent, no gate): the same step the candidate
// pipeline's build is.
func (e *Env) BuildSteps(o ERPOptions) []any {
	steps := e.ERPSteps(o)
	build := steps[3].(map[string]any)
	build["needs"] = []string{}
	build["workspace"] = map[string]any{"access": "read", "from": "run"}
	return []any{build}
}

// PreviewInputs are the declared inputs of the preview pipeline.
func PreviewInputs() map[string]any {
	in := map[string]any{}
	for _, n := range []string{"image", "image_ref", "commit"} {
		in[n] = map[string]any{"type": "string", "required": true}
	}
	return in
}

// PreviewSteps is the preview pipeline's one step: the delivery script over
// the project's preview environment, reading the candidate commit's source.
func (e *Env) PreviewSteps(o ERPOptions) []any {
	return []any{map[string]any{
		"id": "preview", "kind": "container", "image": o.Tools.Ref(), "workspace": map[string]any{"access": "read", "from": "run"},
		"environment": o.Environment, "command": []string{"/opt/kodama/deploy/apply.sh"}, "env": o.previewEnv(),
		"timeoutSeconds": 600, "outputs": DeployOutputs(),
		"retry": map[string]any{"maxAttempts": 3, "backoffSeconds": 1, "on": []string{"infrastructure"}},
	}}
}

// DeployOutputs are the outputs apply.sh documents; the preview step declares
// exactly these so the result path accepts them.
func DeployOutputs() map[string]any {
	str := map[string]any{"type": "string"}
	return map[string]any{
		"deployment_id": str, "deployment_status": str, "configuration_digest": str, "image_digest": str, "source_commit": str,
		"ready": map[string]any{"type": "boolean"}, "object_count": map[string]any{"type": "integer"},
	}
}

// CleanupSteps is the project's ONE cleanup step: it declares the deployment's
// environment and runs delete.sh; the deployment comes from the run's
// deployment_id input, never from the step.
func (e *Env) CleanupSteps(env string, tools Image, serverURL string) []any {
	cenv := map[string]string{"KODAMA_SERVER_URL": serverURL, "DEPLOY_DELETE_TIMEOUT": "60", "RT_RETRY_DELAY": "0", "RT_ATTEMPTS": "3"}
	keys := make([]string, 0, len(cenv))
	for k := range cenv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return []any{map[string]any{
		"id": "cleanup", "kind": "container", "workspace": map[string]any{"access": "none"}, "environment": env, "image": tools.Ref(),
		"command": []string{"/opt/kodama/deploy/delete.sh"}, "env": cenv, "timeoutSeconds": 300,
		"outputs": map[string]any{"deployment_id": map[string]any{"type": "string"}, "cleanup_state": map[string]any{"type": "string"}, "deleted_count": map[string]any{"type": "integer"}},
	}}
}
