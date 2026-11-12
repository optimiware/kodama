//go:build integration

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
)

// pipelineDoc is the API-posted pipeline document this suite authors.
type pipelineDoc struct {
	Inputs  []string            `yaml:"inputs,omitempty"`
	Env     map[string]string   `yaml:"env,omitempty"`
	Actions []pipelineActionDoc `yaml:"actions"`
}

// pipelineActionDoc is one action of the API-posted pipeline. Exactly one of
// Image (container) or Request (request action) is rendered.
type pipelineActionDoc struct {
	Name     string         `yaml:"name"`
	Image    string         `yaml:"image,omitempty"`
	Commands []string       `yaml:"commands,omitempty"`
	Request  string         `yaml:"request,omitempty"`
	Subject  map[string]any `yaml:"subject,omitempty"`
	Outputs  []string       `yaml:"outputs,omitempty"`
	Secrets  []string       `yaml:"secrets,omitempty"`
	Timeout  string         `yaml:"timeout,omitempty"`
}

// marshalPipeline renders one exact pipeline YAML byte sequence.
func marshalPipeline(t *testing.T, document pipelineDoc) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal pipeline yaml: %v", err)
	}
	return encoded
}

// applyPipeline posts one pipeline through the public API and returns the
// stored pipeline identity and version.
func applyPipeline(t *testing.T, harness *factory.Harness, name string, document []byte) api.PipelineApplyResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// updatePipeline replaces one stored pipeline at its expected version.
func updatePipeline(t *testing.T, harness *factory.Harness, pipelineID, name string, document []byte, expected uint64) api.PipelineUpdateResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "pipeline.update", api.PipelineUpdateRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
		Name:       name,
		YAML:       document,
	}, &expected, "update-"+name)
	var updated api.PipelineUpdateResult
	harness.Decode(result, &updated)
	return updated
}

// readPipeline reads one stored pipeline definition.
func readPipeline(t *testing.T, harness *factory.Harness, pipelineID string) api.PipelineReadResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var read api.PipelineReadResult
	harness.Decode(result, &read)
	return read
}

// startPipeline starts one stored pipeline over REST at its currently observed
// stored version.
func startPipeline(t *testing.T, harness *factory.Harness, token, pipelineID string, inputs map[string]string, key string) api.PipelineStartResult {
	t.Helper()
	stored := readPipeline(t, harness, pipelineID)
	if stored.Version == 0 {
		t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	result := harness.MustREST(token, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
		Inputs:     inputs,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	harness.Decode(result, &started)
	return started
}

// inspectExecution reads one execution through the public API.
func inspectExecution(t *testing.T, harness *factory.Harness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	result := harness.MustREST(token, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	var inspected api.ExecutionInspectResult
	harness.Decode(result, &inspected)
	return inspected
}

// executionResources is the live external-resource observation used to justify
// a terminal "none" cleanup record. A "none" record claims that no external
// resource requires cleanup, so it is only accepted once the live state proves
// that claim: no run-labelled container for the execution and no retained
// workspace volume remain.
type executionResources struct {
	containers      []string
	workspaceID     string
	workspaceVolume bool
	err             error
}

// clean reports whether the live state matches a "none" cleanup record.
func (resources executionResources) clean() bool {
	return resources.err == nil && len(resources.containers) == 0 && !resources.workspaceVolume
}

// String renders the probe for a failure message. It never contains a secret.
func (resources executionResources) String() string {
	if resources.err != nil {
		return fmt.Sprintf("resource probe failed: %v (containers=%v workspace_id=%q workspace_volume_present=%t)",
			resources.err, resources.containers, resources.workspaceID, resources.workspaceVolume)
	}
	return fmt.Sprintf("containers=%v workspace_id=%q workspace_volume_present=%t",
		resources.containers, resources.workspaceID, resources.workspaceVolume)
}

// probeExecutionResources observes the live containers and retained workspace
// volume of one execution. It never fails the test: a transient probe error is
// returned as evidence so the caller keeps waiting.
func probeExecutionResources(ctx context.Context, harness *factory.Harness, executionID string) executionResources {
	containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, executionID)
	if err != nil {
		return executionResources{err: fmt.Errorf("list execution containers: %w", err)}
	}
	workspaceID, err := harness.ExecutionWorkspaceID(ctx, executionID)
	if err != nil {
		return executionResources{containers: containers, err: fmt.Errorf("read execution workspace: %w", err)}
	}
	present, err := harness.WorkspaceVolumePresent(ctx, workspaceID)
	if err != nil {
		return executionResources{containers: containers, workspaceID: workspaceID, err: fmt.Errorf("probe workspace volume: %w", err)}
	}
	return executionResources{containers: containers, workspaceID: workspaceID, workspaceVolume: present}
}

// assertRequestWaitResources proves the live probes observe the real retained
// workspace and the stopped earlier workload before any absence is trusted. A
// probe that filters the wrong label or reports a missing volume would
// otherwise make every later "released"/"no running workload" assertion pass
// vacuously. The running-container check stays negative here; its positive
// control is assertRunningContainersPositiveControl, which starts its own
// running container.
func assertRequestWaitResources(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	resources := probeExecutionResources(t.Context(), harness, executionID)
	if resources.err != nil {
		t.Fatalf("probe request-wait resources of execution %s: %v", executionID, resources.err)
	}
	if len(resources.containers) == 0 {
		t.Fatalf("positive control: execution %s has no stopped workload container while the request waits", executionID)
	}
	if !resources.workspaceVolume {
		t.Fatalf("positive control: execution %s has no retained workspace volume %q while the request waits", executionID, resources.workspaceID)
	}
	running, err := harness.RunningContainers(t.Context(), runtimepodman.LabelExecution, executionID)
	if err != nil {
		t.Fatalf("list running execution containers: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("request wait has running workload containers %v", running)
	}
}

// assertRunningContainersPositiveControl proves RunningContainers observes a
// real running container selected by the execution label. It starts and removes
// its own probe container, so the empty result asserted during the request wait
// is meaningful instead of the vacuous output of a wrong label filter.
func assertRunningContainersPositiveControl(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	const probeBase = "running-probe"
	// The base names the probe fixture; the run scope makes it a per-run
	// Podman name so two concurrent suite runs cannot force-remove each
	// other's probe.
	probeName := factory.ProbeContainerName(probeBase)
	ctx := t.Context()
	_ = harness.RemoveProbeContainer(ctx, probeName)
	if err := harness.StartProbeContainer(ctx, probeName, runtimepodman.LabelExecution, executionID); err != nil {
		t.Fatalf("start running-container positive control: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = harness.RemoveProbeContainer(cleanup, probeName)
	})
	harness.Eventually(t, 30*time.Second, func() bool {
		running, err := harness.RunningContainers(ctx, runtimepodman.LabelExecution, executionID)
		return err == nil && len(running) == 1 && running[0] == probeName
	})
	if err := harness.RemoveProbeContainer(ctx, probeName); err != nil {
		t.Fatalf("remove running-container positive control: %v", err)
	}
	harness.Eventually(t, 30*time.Second, func() bool {
		running, err := harness.RunningContainers(ctx, runtimepodman.LabelExecution, executionID)
		return err == nil && len(running) == 0
	})
}

// assertDeadlineRelation proves the persisted deadline is exactly the activation
// time plus the admitted timeout. Both fields are persisted from one activation
// clock reading, so a different relation means a wrong persisted value.
func assertDeadlineRelation(t *testing.T, action api.ExecutionAction) {
	t.Helper()
	if action.ActivatedAt == nil || action.Deadline == nil {
		t.Fatalf("action %q activated_at/deadline = %v/%v; want both persisted", action.Name, action.ActivatedAt, action.Deadline)
	}
	if action.TimeoutSeconds <= 0 {
		t.Fatalf("action %q timeout_seconds = %d; want a positive timeout", action.Name, action.TimeoutSeconds)
	}
	want := time.Duration(action.TimeoutSeconds) * time.Second
	if got := action.Deadline.Sub(*action.ActivatedAt); got != want {
		t.Fatalf("action %q deadline - activated_at = %s; want %s (activated_at=%s deadline=%s)",
			action.Name, got, want, action.ActivatedAt.UTC(), action.Deadline.UTC())
	}
}

// cleanupNoneCanBeTrue reports whether a "none" cleanup record is consistent
// with the execution's own action history: no container action may still be
// pending or running. An activated container action owns a stable workload
// identity whose container exists until terminal cleanup removes it, so the
// record alone is never enough; the caller also probes the live state.
func cleanupNoneCanBeTrue(execution api.ExecutionInspectResult) bool {
	for _, action := range execution.Actions {
		if action.Kind != "container" {
			continue
		}
		switch action.State {
		case "pending", "running":
			return false
		}
	}
	return true
}

// waitExecution waits for one execution to reach a terminal state and for its
// independent cleanup record to settle.
//
// "complete" is the product's own claim that owned workloads were removed and
// the retained workspace was released, so it settles immediately. "pending"
// means cleanup is still owed. "none" claims no external resource requires
// cleanup; it is accepted only after a short grace (a terminal commit and the
// cleanup intent that follows it are separate commits) and only when the
// execution has no active container action and the live state proves the claim:
// no run-labelled container for the execution and no retained workspace volume.
// Anything else keeps waiting for "complete" instead of masking a leaked
// workload or workspace behind a "none" record.
//
// When the wait fails it logs the last inspection, the live resource probe and
// the retained workload stdout/stderr through the public logs operation, so a
// red test carries its own evidence.
func waitExecution(t *testing.T, harness *factory.Harness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	var resources executionResources
	probed := false
	defer func() {
		if t.Failed() {
			// The test context is already cancelled when a failed wait unwinds
			// through a cleanup, so the evidence reads take their own bound.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if !probed {
				resources = probeExecutionResources(ctx, harness, executionID)
			}
			t.Logf("execution %s last inspect: state=%q version=%d cleanup=%+v failure=%+v actions=%+v",
				last.ExecutionID, last.State, last.Version, last.Cleanup, last.Failure, last.Actions)
			t.Logf("execution %s runtime evidence: %s", executionID, harness.ExecutionRuntimeEvidence(ctx, executionID))
			t.Logf("execution %s last live resources: %s", executionID, resources)
			dumpExecutionLogs(t, harness, token, executionID, last)
		}
	}()
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		switch last.State {
		case "succeeded", "failed", "cancelled":
			return true
		default:
			return false
		}
	})

	// The cleanup intent for a terminal execution is a separate commit, so give
	// a "none" record one grace period before probing the live state once. A
	// clean live state then settles the wait; a dirty one can only settle by
	// reaching "complete".
	const noneGrace = 2 * time.Second
	probeAfter := time.Now().Add(noneGrace)
	probedNone := false
	deadline := time.Now().Add(time.Minute)
	for {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code == "" {
			harness.Decode(result, &last)
			switch last.Cleanup.State {
			case "complete":
				return last
			case "none":
				if !probedNone && !time.Now().Before(probeAfter) {
					probedNone = true
					probed = true
					resources = probeExecutionResources(t.Context(), harness, executionID)
					if cleanupNoneCanBeTrue(last) && resources.clean() {
						return last
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			if !probedNone {
				probed = true
				resources = probeExecutionResources(t.Context(), harness, executionID)
			}
			t.Fatalf("execution %s reached state %q but cleanup did not settle within %s: cleanup=%+v; live resources: %s\ncomposition log:\n%s",
				executionID, last.State, time.Minute, last.Cleanup, resources, factory.SanitizeDiagnostic(harness.Logs()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitExecutionSettled waits for one execution's terminal cleanup to complete
// and then for the served execution view to become quiescent: cleanup complete
// with the same execution version observed by two consecutive reads.
//
// The served execution view is assembled from several rows, so one inspect can
// read the execution row before a cleanup commit and the cleanup row after it.
// That read reports cleanup complete with a version one commit stale. A second
// read that starts after the first completed starts after any commit the first
// read straddled, so an equal version proves the view is no longer straddling a
// commit. This is a barrier, never a fixed delay.
func waitExecutionSettled(t *testing.T, harness *factory.Harness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	waitExecution(t, harness, token, executionID)
	var last api.ExecutionInspectResult
	var lastVersion uint64
	haveBaseline := false
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			haveBaseline = false
			return false
		}
		var current api.ExecutionInspectResult
		harness.Decode(result, &current)
		if current.Cleanup.State != "complete" {
			haveBaseline = false
			last = current
			return false
		}
		if haveBaseline && current.Version == lastVersion {
			last = current
			return true
		}
		haveBaseline = true
		lastVersion = current.Version
		last = current
		return false
	})
	return last
}

// assertExecutionResourcesReleased proves a terminal execution's cleanup
// removed every run-labelled container and released the retained workspace
// volume. Cleanup is separate from the execution outcome, but the contract
// still requires owned resources to be removed and the workspace released
// after a confirmed stop; this checks those external effects independently of
// the cleanup record's own state.
func assertExecutionResourcesReleased(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	resources := probeExecutionResources(t.Context(), harness, executionID)
	if resources.err != nil {
		t.Fatalf("probe released resources of execution %s: %v", executionID, resources.err)
	}
	if len(resources.containers) != 0 {
		t.Fatalf("execution %s left run-labelled containers %v after terminal cleanup", executionID, resources.containers)
	}
	if resources.workspaceVolume {
		t.Fatalf("execution %s retained workspace volume %s after terminal cleanup", executionID, resources.workspaceID)
	}
}

// dumpExecutionLogs logs the retained stdout/stderr of every action of one
// execution. It is diagnostics for a failed wait, never an assertion: a refused
// or missing log read is logged and skipped, so it cannot mask the real failure.
func dumpExecutionLogs(t *testing.T, harness *factory.Harness, token, executionID string, execution api.ExecutionInspectResult) {
	t.Helper()
	actions := execution.Actions
	if len(actions) == 0 {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			t.Logf("execution %s retained logs unavailable: inspect refused: %s (%s)", executionID, publicError.Code, publicError.Message)
			return
		}
		var current api.ExecutionInspectResult
		harness.Decode(result, &current)
		actions = current.Actions
	}
	for _, action := range actions {
		for _, stream := range []string{"stdout", "stderr"} {
			result, publicError := harness.REST(token, "execution.logs.read", api.ExecutionLogsReadRequest{
				ExecutionID: executionID,
				ActionID:    action.ActionID,
				Stream:      stream,
			}, nil, "")
			if publicError.Code != "" {
				t.Logf("execution %s action %s %s logs refused: %s (%s)", executionID, action.Name, stream, publicError.Code, publicError.Message)
				continue
			}
			var read api.ExecutionLogsReadResult
			harness.Decode(result, &read)
			if len(read.Chunks) == 0 {
				t.Logf("execution %s action %s %s logs: (no retained chunks)", executionID, action.Name, stream)
				continue
			}
			var builder strings.Builder
			for _, chunk := range read.Chunks {
				_, _ = builder.Write(chunk.Data)
			}
			t.Logf("execution %s action %s %s logs:\n%s", executionID, action.Name, stream, factory.SanitizeDiagnostic(builder.String()))
		}
	}
}

// actionByName returns one action observation by its admitted name.
func actionByName(t *testing.T, execution api.ExecutionInspectResult, name string) api.ExecutionAction {
	t.Helper()
	for _, action := range execution.Actions {
		if action.Name == name {
			return action
		}
	}
	t.Fatalf("execution %s has no action %q; actions=%+v", execution.ExecutionID, name, execution.Actions)
	return api.ExecutionAction{}
}

// requireSucceeded fails with the typed failure when the execution did not
// succeed.
func requireSucceeded(t *testing.T, execution api.ExecutionInspectResult) {
	t.Helper()
	if execution.State != "succeeded" {
		t.Fatalf("execution %s state = %q; failure=%+v; actions=%+v",
			execution.ExecutionID, execution.State, execution.Failure, execution.Actions)
	}
}

// assertSameBody proves two results name the same operation and carry an equal
// JSON body.
func assertSameBody(t *testing.T, left, right api.Result) {
	t.Helper()
	if left.Operation != right.Operation {
		t.Fatalf("operations left=%q right=%q", left.Operation, right.Operation)
	}
	var leftBody, rightBody any
	if err := json.Unmarshal(left.Body, &leftBody); err != nil {
		t.Fatalf("decode left body: %v; body=%s", err, string(left.Body))
	}
	if err := json.Unmarshal(right.Body, &rightBody); err != nil {
		t.Fatalf("decode right body: %v; body=%s", err, string(right.Body))
	}
	if !reflect.DeepEqual(leftBody, rightBody) {
		t.Fatalf("%s bodies differ:\nleft =%s\nright=%s", left.Operation, string(left.Body), string(right.Body))
	}
}

// submitTemplate is the workload-side output submission. It reads the mounted
// execution credential, inspects its own execution to learn the current
// execution version and its action id (KODAMA_ACTION is deliberately the action
// slug, not the id), then stages the declared output with the public envelope.
// It fails the workload on any refusal so a broken submission cannot look like
// a successful action.
const submitTemplate = `TOKEN=$(cat /kodama/secrets/KODAMA_TOKEN)
ENDPOINT=${KODAMA_URL#http://}
API_HOST=${ENDPOINT%:*}
API_PORT=${ENDPOINT##*:}
# api sends one public envelope over BusyBox nc. The request is written
# first and stdin is then held open for a bounded time: nc half-closes the
# connection as soon as its stdin reaches EOF, and Go's net/http server cancels
# the request context on a client half-close. Closing immediately cancelled the
# composition's credential lookup mid-flight and surfaced as the uniform
# "presented credential is invalid" even though the mounted token was valid.
api() {
  method=$1
  path=$2
  body=$3
  length=$(printf '%s' "$body" | wc -c)
  { printf '%s %s HTTP/1.0\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
      "$method" "$path" "$ENDPOINT" "$TOKEN" "$length" "$body"; sleep 2; } \
    | nc -w 10 "$API_HOST" "$API_PORT" | tr -d '\r' | sed '1,/^$/d'
}
# The admitted execution version advances whenever the composition moves the
# execution (credential issuance, activation, settlement), so a legitimate
# submission can race it. Mirror submitExecutionOutputs in
# internal/shell/app/workload_bearer_test.go: re-inspect for the current
# version and retry a version_conflict with a fresh idempotency key, because
# execution.action.outputs is not idempotent across reused keys.
attempt=0
submitted=0
while [ "$attempt" -lt 20 ]; do
  attempt=$((attempt+1))
  inspect=$(api GET "/api/v1/executions/$KODAMA_EXECUTION" '{}')
  case "$inspect" in
    *'"operation":"execution.inspect"'*) ;;
    *) echo "execution inspect failed: $inspect" >&2; exit 1 ;;
  esac
  version=$(printf '%s' "$inspect" | grep -o '"version":[0-9]*' | head -n 1 | cut -d: -f2)
  action=$(printf '%s' "$inspect" | sed 's/},{/}\n{/g' | grep -F "\"name\":\"$KODAMA_ACTION\"" | grep -o '"action_id":"[^"]*"' | head -n 1 | cut -d'"' -f4)
  test -n "$version"
  test -n "$action"
  response=$(api PUT "/api/v1/executions/$KODAMA_EXECUTION/actions/$action/outputs" "{\"input\":{\"outputs\":{\"__OUTPUT_KEY__\":\"$__OUTPUT_VAR__\"}},\"expected_version\":$version,\"idempotency_key\":\"outputs:$KODAMA_EXECUTION:$action:$attempt\"}")
  case "$response" in
    *'"operation":"execution.action.outputs"'*) submitted=1; break ;;
    *version_conflict*) echo "execution outputs hit a version conflict; retrying with a fresh key" >&2 ;;
    *) echo "output submission failed: $response" >&2; exit 1 ;;
  esac
done
if [ "$submitted" -ne 1 ]; then
  echo "output submission did not converge on a current execution version" >&2
  exit 1
fi`

// submitScript returns the submission script for one declared output.
func submitScript(outputKey, outputVariable string) string {
	return strings.NewReplacer(
		"__OUTPUT_KEY__", outputKey,
		"__OUTPUT_VAR__", outputVariable,
	).Replace(submitTemplate)
}

// prepareChecks verifies the task-free context, the admitted input, the
// materialized secret and the shared workspace, then sets the first artifact.
func prepareChecks(seed string, secretLength int, secretPrefix string) string {
	return fmt.Sprintf(`test ! -e /kodama/task.json
test -z "${KODAMA_TASK:-}"
test "$(cat /kodama/inputs/seed)" = %q
test "$(wc -c < /kodama/secrets/PIPELINE_SECRET | tr -d ' ')" = %q
test "$(head -c 8 /kodama/secrets/PIPELINE_SECRET)" = %q
printf 'shared workspace\n' > /workspace/marker
ARTIFACT="built:$(cat /kodama/inputs/seed)"`, seed, strconv.Itoa(secretLength), secretPrefix)
}

// consumeChecks verifies the ordered materialization of the earlier succeeded
// output and the shared workspace, then sets the final output.
func consumeChecks(artifact string) string {
	return fmt.Sprintf(`test ! -e /kodama/task.json
test "$(cat /workspace/marker)" = "shared workspace"
test "$(cat /kodama/outputs/prepare/artifact)" = %q
FINAL="consumed:$(cat /kodama/outputs/prepare/artifact)"`, artifact)
}

// startViaClient starts one stored pipeline through the public Go client at its
// currently observed stored version.
func startViaClient(t *testing.T, transport *client.HTTPTransport, pipelineID string, inputs map[string]string, key string) api.PipelineStartResult {
	t.Helper()
	readInput, err := json.Marshal(api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	})
	if err != nil {
		t.Fatalf("marshal read input: %v", err)
	}
	read, err := transport.Invoke(t.Context(), api.Invocation{
		Operation: "pipeline.read",
		Request:   api.Request{Input: readInput},
	})
	if err != nil {
		t.Fatalf("pipeline.read over client: %v", err)
	}
	var stored api.PipelineReadResult
	if err := json.Unmarshal(read.Body, &stored); err != nil {
		t.Fatalf("decode client read: %v; body=%s", err, string(read.Body))
	}
	if stored.Version == 0 {
		t.Fatalf("client pipeline.read %s version = 0", pipelineID)
	}
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
		Inputs:     inputs,
	})
	if err != nil {
		t.Fatalf("marshal start input: %v", err)
	}
	result, err := transport.Invoke(t.Context(), api.Invocation{
		Operation: "pipeline.start",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: &stored.Version,
			IdempotencyKey:  key,
		},
	})
	if err != nil {
		t.Fatalf("pipeline.start over client: %v", err)
	}
	var started api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil {
		t.Fatalf("decode client start: %v; body=%s", err, string(result.Body))
	}
	return started
}

// inspectViaClient reads one execution through the public Go client.
func inspectViaClient(t *testing.T, transport *client.HTTPTransport, executionID string) api.Result {
	t.Helper()
	input, err := json.Marshal(api.ExecutionInspectRequest{ExecutionID: executionID})
	if err != nil {
		t.Fatalf("marshal inspect input: %v", err)
	}
	result, err := transport.Invoke(t.Context(), api.Invocation{
		Operation: "execution.inspect",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		t.Fatalf("execution.inspect over client: %v", err)
	}
	return result
}

// dropOnceRoundTripper forwards the request to the real edge, reads and closes
// the response, and then reports a transport failure exactly once. The server
// has already committed the effect, so the client's retry exercises the
// lost-response-after-effect path.
type dropOnceRoundTripper struct {
	mu      sync.Mutex
	dropped bool
	base    http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (transport *dropOnceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	drop := !transport.dropped
	transport.dropped = true
	transport.mu.Unlock()

	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if !drop {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, errors.New("simulated lost response after the server committed the effect")
}
