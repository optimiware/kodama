//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// liveInputs are the inputs every live e2e test requires. Q01_TLS_SERVER_NAME
// is optional (inferred from the URL unless a port-forward needs an override);
// the image and revision inputs are optional outside a release run.
var liveInputs = []harness.Input{
	harness.InputAPIURL,
	harness.InputCAPEM,
	harness.InputToken,
	harness.InputProjectID,
	harness.InputEvidenceDir,
}

// live is one configured installation connection plus its evidence writer.
type live struct {
	config    harness.Config
	api       *harness.API
	writer    *harness.EvidenceWriter
	ctx       context.Context
	token     harness.Secret
	tokens    map[harness.Persona]harness.Secret
	projectID string
	image     string
	runID     string
}

// startLive loads the installation inputs. An unconfigured environment skips
// with every missing input named; Q01_REQUIRE_STAGE=1 or
// Q01_REQUIRE_INSTALLATION=1 turns that skip into a failure.
func startLive(t *testing.T) *live {
	t.Helper()
	config, err := harness.LoadConfig(os.Getenv)
	if err != nil {
		t.Fatalf("the configured installation inputs are malformed: %v", err)
	}
	if missing := config.Missing(liveInputs...); !missing.Empty() {
		if harness.RequireStage() || harness.RequireInstallation() {
			t.Fatalf("required installation input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured installation input(s): %s", missing)
	}
	if harness.RequireInstallation() && config.ImageDigest() == "" {
		t.Fatalf("a required installation run needs a digest-pinned tools image; %q carries no digest", config.ImageReference())
	}
	client, err := harness.NewAPI(config)
	if err != nil {
		t.Fatalf("build the installation API helper: %v", err)
	}
	writer, err := harness.NewEvidenceWriter(config.EvidenceDir)
	if err != nil {
		t.Fatalf("the configured evidence directory is unusable: %v", err)
	}
	writer.Secrets = append(config.PersonaSecrets(), config.Token)
	tokens := map[harness.Persona]harness.Secret{harness.PersonaAdmin: config.Token}
	for _, persona := range harness.Personas {
		if secret, ok := config.PersonaToken(persona); ok {
			tokens[persona] = secret
		}
	}
	return &live{
		config:    config,
		api:       client,
		writer:    writer,
		ctx:       t.Context(),
		token:     config.Token,
		tokens:    tokens,
		projectID: config.ProjectID,
		image:     config.ImageReference(),
		runID:     newRunID(t),
	}
}

// newRunID returns a bounded random run identity used in idempotency keys and
// pipeline names.
func newRunID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate run identity: %v", err)
	}
	return hex.EncodeToString(raw)
}

// key returns one run-unique idempotency key.
func (instance *live) key(suffix string) string {
	return "q01-" + instance.runID + "-" + suffix
}

// invoke performs one public operation with an explicit persona token and
// bounded context, returning the refusal instead of failing the test.
func (instance *live) invoke(ctx context.Context, token harness.Secret, method api.Method, operation string, input any, idempotencyKey string, expectedVersion *uint64) (api.Result, error) {
	return instance.api.Invoke(ctx, harness.Call{
		Method:          method,
		Token:           token,
		Operation:       operation,
		Input:           input,
		IdempotencyKey:  idempotencyKey,
		ExpectedVersion: expectedVersion,
	})
}

// call invokes one public operation as the basic-test token and returns the
// result.
func (instance *live) call(t *testing.T, method api.Method, operation string, input any, idempotencyKey string, expectedVersion *uint64) api.Result {
	t.Helper()
	result, err := instance.invoke(instance.ctx, instance.token, method, operation, input, idempotencyKey, expectedVersion)
	if err != nil {
		t.Fatalf("%s was refused: %v", operation, err)
	}
	return result
}

// decodeResult decodes one typed result body.
func decodeResult[T any](t *testing.T, result api.Result) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.Body, &value); err != nil {
		t.Fatalf("%s result body is not %T: %v", result.Operation, value, err)
	}
	return value
}

// startInline posts one inline task-free document.
func (instance *live) startInline(t *testing.T, document []byte, idempotencyKey string) api.PipelineStartResult {
	t.Helper()
	result := instance.call(t, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID: instance.projectID,
		YAML:      document,
	}, idempotencyKey, nil)
	return decodeResult[api.PipelineStartResult](t, result)
}

// startStored starts one stored pipeline at its expected version.
func (instance *live) startStored(t *testing.T, pipelineID string, expected uint64, idempotencyKey string) api.PipelineStartResult {
	t.Helper()
	result := instance.call(t, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: pipelineID,
	}, idempotencyKey, &expected)
	return decodeResult[api.PipelineStartResult](t, result)
}

// applyPipeline stores one exact pipeline document.
func (instance *live) applyPipeline(t *testing.T, name string, document []byte, idempotencyKey string) api.PipelineApplyResult {
	t.Helper()
	result := instance.call(t, api.MethodPut, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: instance.projectID,
		Name:      name,
		YAML:      document,
	}, idempotencyKey, nil)
	return decodeResult[api.PipelineApplyResult](t, result)
}

// readPipeline reads one stored pipeline definition.
func (instance *live) readPipeline(t *testing.T, pipelineID string) api.PipelineReadResult {
	t.Helper()
	result := instance.call(t, api.MethodGet, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  instance.projectID,
		PipelineID: pipelineID,
	}, "", nil)
	return decodeResult[api.PipelineReadResult](t, result)
}

// updatePipeline replaces one stored pipeline at its expected version.
func (instance *live) updatePipeline(t *testing.T, pipelineID, name string, document []byte, expected uint64, idempotencyKey string) api.PipelineUpdateResult {
	t.Helper()
	result := instance.call(t, api.MethodPatch, "pipeline.update", api.PipelineUpdateRequest{
		ProjectID:  instance.projectID,
		PipelineID: pipelineID,
		Name:       name,
		YAML:       document,
	}, idempotencyKey, &expected)
	return decodeResult[api.PipelineUpdateResult](t, result)
}

// inspect reads one execution view.
func (instance *live) inspect(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	result := instance.call(t, api.MethodGet, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, "", nil)
	return decodeResult[api.ExecutionInspectResult](t, result)
}

// inspectResult reads one execution view for a poll callback, returning
// transport failures instead of failing the test inside the loop.
func (instance *live) inspectResult(ctx context.Context, executionID string) (api.ExecutionInspectResult, error) {
	result, err := instance.api.Invoke(ctx, harness.Call{
		Method:    api.MethodGet,
		Token:     instance.token,
		Operation: "execution.inspect",
		Input:     api.ExecutionInspectRequest{ExecutionID: executionID},
	})
	if err != nil {
		return api.ExecutionInspectResult{}, err
	}
	var view api.ExecutionInspectResult
	if err := json.Unmarshal(result.Body, &view); err != nil {
		return api.ExecutionInspectResult{}, fmt.Errorf("decode execution.inspect: %w", err)
	}
	return view, nil
}

// waitComplete polls until the execution is terminal and its independent
// cleanup record is complete, then validates the declared state vocabulary.
func (instance *live) waitComplete(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	pollCtx, cancel := context.WithTimeout(instance.ctx, 6*time.Minute)
	defer cancel()
	final, _, err := harness.Poll(pollCtx, harness.RealClock{}, harness.PollOptions{
		Interval: 250 * time.Millisecond,
		Timeout:  6 * time.Minute,
		Stop: func(result api.ExecutionInspectResult) bool {
			status, interpretErr := harness.Interpret(result)
			return interpretErr == nil && status.Terminal && status.CleanupComplete
		},
	}, func(ctx context.Context) (api.ExecutionInspectResult, error) {
		result, err := instance.api.Invoke(ctx, harness.Call{
			Method:    api.MethodGet,
			Token:     instance.token,
			Operation: "execution.inspect",
			Input:     api.ExecutionInspectRequest{ExecutionID: executionID},
		})
		if err != nil {
			return api.ExecutionInspectResult{}, err
		}
		var view api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &view); err != nil {
			return api.ExecutionInspectResult{}, fmt.Errorf("decode execution.inspect: %w", err)
		}
		return view, nil
	})
	if err != nil {
		last, _ := json.Marshal(final)
		t.Fatalf("execution %s did not reach terminal cleanup within the bound: %v; last=%s", executionID, err, last)
	}
	if _, err := harness.Interpret(final); err != nil {
		t.Fatalf("execution %s served an invalid terminal view: %v", executionID, err)
	}
	return final
}

// pollTerminal observes every intermediate state until the execution is
// terminal, so an ordering test can prove single-action progression.
func (instance *live) pollTerminal(t *testing.T, executionID string) (api.ExecutionInspectResult, []api.ExecutionInspectResult) {
	t.Helper()
	pollCtx, cancel := context.WithTimeout(instance.ctx, 6*time.Minute)
	defer cancel()
	final, observations, err := harness.Poll(pollCtx, harness.RealClock{}, harness.PollOptions{
		Interval: 100 * time.Millisecond,
		Timeout:  6 * time.Minute,
	}, func(ctx context.Context) (api.ExecutionInspectResult, error) {
		result, err := instance.api.Invoke(ctx, harness.Call{
			Method:    api.MethodGet,
			Token:     instance.token,
			Operation: "execution.inspect",
			Input:     api.ExecutionInspectRequest{ExecutionID: executionID},
		})
		if err != nil {
			return api.ExecutionInspectResult{}, err
		}
		var view api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &view); err != nil {
			return api.ExecutionInspectResult{}, fmt.Errorf("decode execution.inspect: %w", err)
		}
		return view, nil
	})
	if err != nil {
		t.Fatalf("execution %s did not reach a terminal state: %v", executionID, err)
	}
	return final, observations
}

// readLogs reads and reassembles one action's retained stream, following every
// page cursor.
func (instance *live) readLogs(t *testing.T, executionID, actionID, stream string) []byte {
	t.Helper()
	var chunks []api.LogChunk
	cursor := ""
	for range 64 {
		request := api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      stream,
		}
		if cursor != "" {
			request.Page = &api.Page{Cursor: cursor}
		}
		result := instance.call(t, api.MethodGet, "execution.logs.read", request, "", nil)
		decoded := decodeResult[api.ExecutionLogsReadResult](t, result)
		if decoded.Truncated {
			t.Fatalf("retained %s log of action %s is truncated", stream, actionID)
		}
		chunks = append(chunks, decoded.Chunks...)
		if decoded.NextCursor == "" {
			assembled, err := harness.ReassembleLog(chunks, actionID, stream)
			if err != nil {
				t.Fatalf("reassemble %s log of action %s: %v", stream, actionID, err)
			}
			return assembled
		}
		cursor = decoded.NextCursor
	}
	t.Fatalf("retained %s log of action %s did not finish within 64 pages", stream, actionID)
	return nil
}

// actionByName returns one action or fails.
func actionByName(t *testing.T, view api.ExecutionInspectResult, name string) api.ExecutionAction {
	t.Helper()
	for _, action := range view.Actions {
		if action.Name == name {
			return action
		}
	}
	t.Fatalf("execution %s has no action %q; actions=%+v", view.ExecutionID, name, view.Actions)
	return api.ExecutionAction{}
}

// record writes one allowlisted evidence file for an acceptance row.
func (instance *live) record(t *testing.T, ac, traceRow, testName string, view *api.ExecutionInspectResult, started, finished time.Time, extra map[string]any) {
	t.Helper()
	record := map[string]any{
		"probe":       testName,
		"ac":          ac,
		"trace_row":   traceRow,
		"started_at":  started.UTC(),
		"finished_at": finished.UTC(),
		"duration_ms": finished.Sub(started).Milliseconds(),
		"outcome":     "passed",
	}
	if instance.config.HarnessRevision != "" {
		record["harness_revision"] = instance.config.HarnessRevision
	}
	if instance.config.DeployedRevision != "" {
		record["deployed_revision"] = instance.config.DeployedRevision
	}
	installation := instance.config.Installation
	if installation.ID != "" {
		record["installation_id"] = installation.ID
	}
	if installation.Profile != "" {
		record["profile"] = installation.Profile
	}
	if installation.Runtime != "" {
		record["runtime"] = installation.Runtime
	}
	if installation.ServerImageDigest != "" {
		record["server_image_digest"] = installation.ServerImageDigest
	}
	if digest := instance.config.ImageDigest(); digest != "" {
		record["image_digest"] = digest
		record["tools_image_digest"] = digest
	} else if instance.config.RequireInstallation() {
		t.Fatalf("release evidence needs a digest-pinned tools image; %q carries no digest", instance.config.ImageReference())
	} else {
		// A non-release invocation records that fact explicitly rather than
		// pretending the image was pinned.
		record["image_digest"] = "unpinned"
	}
	if digest := instance.config.ExternalHarnessDigest(); digest != "" {
		record["external_harness_digest"] = digest
	}
	if view != nil {
		record["execution_id"] = view.ExecutionID
		record["execution_state"] = view.State
		record["cleanup_state"] = view.Cleanup.State
	}
	maps.Copy(record, extra)
	name := strings.ToLower(ac) + "-row-" + strings.NewReplacer(".", "-").Replace(traceRow)
	if _, err := instance.writer.Write(name, record); err != nil {
		t.Fatalf("write %s evidence: %v", ac, err)
	}
}

// workloadCA is the installation CA written into a workload's workspace. The
// certificate is public data, so it is carried as bytes rather than a secret.
//
// The test profile's workload secret-name allow-list is deny-by-default and no
// entry configures it, so a declared secret name is refused at activation
// ("secret name is not authorized"). Until an installation exposes that
// allow-list, materializing the public CA bytes is the smallest
// verified-TLS-preserving path; it never falls back to an unverified
// connection.
type workloadCA struct {
	path    string
	command string
}

// materializeCA returns the command that writes the configured CA file into
// the shared workspace. A system-roots installation returns an empty path.
func (instance *live) materializeCA(t *testing.T) *workloadCA {
	t.Helper()
	if instance.config.CAPEM == "" {
		return &workloadCA{}
	}
	data, err := os.ReadFile(instance.config.CAPEM)
	if err != nil {
		t.Fatalf("read the installation CA: %v", err)
	}
	path := "/workspace/w49-ca.crt"
	command := fmt.Sprintf("printf '%%s' '%s' | base64 -d > %s && chmod 600 %s",
		base64.StdEncoding.EncodeToString(data), path, path)
	return &workloadCA{path: path, command: command}
}

// commands prepends the CA materialization to an action's command list.
func (ca *workloadCA) commands(commands ...string) []string {
	if ca == nil || ca.command == "" {
		return commands
	}
	return append([]string{ca.command}, commands...)
}

// environment returns the TLS environment that points every network client at
// the materialized CA file.
func (ca *workloadCA) environment() map[string]string {
	if ca == nil || ca.path == "" {
		return nil
	}
	return map[string]string{
		"SSL_CERT_FILE":  ca.path,
		"CURL_CA_BUNDLE": ca.path,
		"GIT_SSL_CAINFO": ca.path,
	}
}

// opaqueIdentityPattern accepts any attributable, bounded installation
// identity; the runtime's prefixes are not part of the public contract.
var opaqueIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

// requireOpaqueIdentity asserts one nonempty, bounded, opaque identity.
func requireOpaqueIdentity(t *testing.T, label, value string) {
	t.Helper()
	if !opaqueIdentityPattern.MatchString(value) {
		t.Fatalf("%s identity = %q; want a nonempty bounded opaque identity", label, value)
	}
}

// requireStableIdentity asserts two observations report the same identity.
func requireStableIdentity(t *testing.T, label, first, second string) {
	t.Helper()
	requireOpaqueIdentity(t, label, first)
	if first != second {
		t.Fatalf("%s identity changed between inspections: %q != %q", label, first, second)
	}
}

// recordScenario writes one allowlisted scenario record that is not an
// acceptance-criterion row. The installation preflight uses it so recording its
// result never credits an AC whose full proof belongs to another order.
func (instance *live) recordScenario(t *testing.T, scenario, testName string, view *api.ExecutionInspectResult, started, finished time.Time, extra map[string]any) {
	t.Helper()
	record := map[string]any{
		"scenario":    scenario,
		"probe":       testName,
		"started_at":  started.UTC(),
		"finished_at": finished.UTC(),
		"duration_ms": finished.Sub(started).Milliseconds(),
		"outcome":     "passed",
	}
	if instance.config.HarnessRevision != "" {
		record["harness_revision"] = instance.config.HarnessRevision
	}
	if instance.config.DeployedRevision != "" {
		record["deployed_revision"] = instance.config.DeployedRevision
	}
	installation := instance.config.Installation
	if installation.ID != "" {
		record["installation_id"] = installation.ID
	}
	if installation.Profile != "" {
		record["profile"] = installation.Profile
	}
	if installation.Runtime != "" {
		record["runtime"] = installation.Runtime
	}
	if installation.ServerImageDigest != "" {
		record["server_image_digest"] = installation.ServerImageDigest
	}
	if digest := instance.config.ImageDigest(); digest != "" {
		record["tools_image_digest"] = digest
	}
	if view != nil {
		record["execution_id"] = view.ExecutionID
		record["execution_state"] = view.State
		record["cleanup_state"] = view.Cleanup.State
	}
	maps.Copy(record, extra)
	if _, err := instance.writer.Write(strings.ToLower(scenario)+"-scenario", record); err != nil {
		t.Fatalf("write %s scenario evidence: %v", scenario, err)
	}
}

// actionNames returns one execution's action-name sequence.
func actionNames(view api.ExecutionInspectResult) []string {
	names := make([]string, 0, len(view.Actions))
	for _, action := range view.Actions {
		names = append(names, action.Name)
	}
	return names
}

// equalNames reports whether two action-name sequences are identical.
func equalNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// findAction returns one action by name and whether it exists.
func findAction(view api.ExecutionInspectResult, name string) (api.ExecutionAction, bool) {
	for _, action := range view.Actions {
		if action.Name == name {
			return action, true
		}
	}
	return api.ExecutionAction{}, false
}

// requireAbsolutePwd asserts the run reported an absolute working directory.
func requireAbsolutePwd(t *testing.T, log []byte) {
	t.Helper()
	for line := range strings.SplitSeq(string(log), "\n") {
		if !strings.HasPrefix(line, "ac11-pwd=") {
			continue
		}
		value := strings.TrimPrefix(line, "ac11-pwd=")
		if !strings.HasPrefix(value, "/") {
			t.Fatalf("materialized working directory %q is not absolute", value)
		}
		return
	}
	t.Fatalf("materialization log does not report a working directory (log bytes=%d)", len(log))
}

// cancelCurrent cancels an execution at its current version, retrying one
// version conflict after a fresh inspection.
func (instance *live) cancelCurrent(t *testing.T, executionID, idempotencyKey string) api.ExecutionCancelResult {
	t.Helper()
	for attempt := range 5 {
		view := instance.inspect(t, executionID)
		result, err := instance.invoke(instance.ctx, instance.token, api.MethodPost, "execution.cancel",
			api.ExecutionCancelRequest{ExecutionID: executionID},
			idempotencyKey+"-"+fmt.Sprint(attempt), &view.Version)
		if err == nil {
			return decodeResult[api.ExecutionCancelResult](t, result)
		}
		if publicErrorCode(err) != api.CodeVersionConflict {
			t.Fatalf("execution.cancel was refused: %v", err)
		}
	}
	t.Fatalf("execution.cancel did not converge on a current version")
	return api.ExecutionCancelResult{}
}

// requireSucceeded asserts the terminal state and cleanup.
func requireSucceeded(t *testing.T, view api.ExecutionInspectResult) {
	t.Helper()
	status, err := harness.Interpret(view)
	if err != nil {
		t.Fatalf("invalid execution view: %v", err)
	}
	if !status.Succeeded {
		t.Fatalf("execution %s state = %q; want succeeded (failure=%+v)", view.ExecutionID, view.State, view.Failure)
	}
	if !status.CleanupComplete {
		t.Fatalf("execution %s cleanup = %q; want complete", view.ExecutionID, view.Cleanup.State)
	}
}

// requireLogContains asserts one exact log marker. Callers scan credentials
// before this check; a failure reports only the label, marker and byte count so
// no captured output reaches the test log unscanned.
func requireLogContains(t *testing.T, label string, log []byte, wanted string) {
	t.Helper()
	if !bytes.Contains(log, []byte(wanted)) {
		t.Fatalf("%s log does not contain %q (log bytes=%d)", label, wanted, len(log))
	}
}

// publicErrorCode extracts a public refusal code from an API error.
func publicErrorCode(err error) string {
	if refusal, ok := errors.AsType[*harness.APIError](err); ok {
		return refusal.Code
	}
	return ""
}
