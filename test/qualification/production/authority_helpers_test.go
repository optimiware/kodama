//go:build qualification

package production

// Authority-probe helpers. Everything here is test-only: the qualification
// package exists only under the qualification build tag and this file is never
// compiled into a product binary.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
)

// serviceCredentialPrefix is the frozen derived service-credential prefix.
const serviceCredentialPrefix = "kodsvc."

// stageGate decides the environment-only skip rule: a missing input is a named
// skip unless the run requires the stage, in which case it is an error naming
// the same inputs.
func stageGate(required bool, missing MissingInputs) (bool, error) {
	if missing.Empty() {
		return false, nil
	}
	if required {
		return false, fmt.Errorf("required Q02 stage input(s) are missing: %s", missing)
	}
	return true, nil
}

// loadStageConfig loads the qualification environment and fails on a malformed
// present value.
func loadStageConfig(t *testing.T) Config {
	t.Helper()
	config, err := LoadConfig(os.Getenv)
	if err != nil {
		t.Fatalf("the configured Q02 stage inputs are malformed: %v", err)
	}
	return config
}

// requireStageInputs applies stageGate to one probe's required inputs.
func requireStageInputs(t *testing.T, config Config, inputs ...Input) {
	t.Helper()
	skip, err := stageGate(os.Getenv("Q02_REQUIRE_STAGE") == "1", config.Missing(inputs...))
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skipf("unconfigured Q02 stage input(s): %s", config.Missing(inputs...))
	}
}

// newStageClient builds the public API helper from the stage inputs.
func newStageClient(t *testing.T, config Config) *API {
	t.Helper()
	client, err := NewAPI(config)
	if err != nil {
		t.Fatalf("build the stage API helper: %v", err)
	}
	return client
}

// probeID returns one bounded, prefixed, random probe identity.
func probeID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate a probe identity")
	}
	identity := prefix + "-" + hex.EncodeToString(raw[:])
	if !identityPattern.MatchString(identity) {
		return "", fmt.Errorf("generate a bounded probe identity")
	}
	return identity, nil
}

// probeIdempotencyKey returns one bounded, prefixed, random idempotency key.
func probeIdempotencyKey(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate an idempotency key")
	}
	key := prefix + "-" + hex.EncodeToString(raw[:])
	if len(key) > api.MaxIdempotencyKeyBytes || !validIdempotencyKey(key) {
		return "", fmt.Errorf("generate a bounded idempotency key")
	}
	return key, nil
}

// validIdempotencyKey accepts the frozen idempotency-key grammar.
func validIdempotencyKey(key string) bool {
	if key == "" {
		return false
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '_', character == ':', character == '-':
		default:
			return false
		}
	}
	return true
}

// buildMCPToolsCall builds one MCP JSON-RPC tools/call message carrying the
// frozen request envelope. It applies the same idempotency-key rule as the
// public edge before any request is sent.
func buildMCPToolsCall(id, operation string, request api.Request) ([]byte, error) {
	if _, ok := api.LookupRoute(operation); !ok {
		return nil, fmt.Errorf("unknown public operation %q", operation)
	}
	if id == "" || len(id) > 64 {
		return nil, fmt.Errorf("the MCP request id is empty or too long")
	}
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("the MCP request envelope is invalid")
	}
	if operationRequiresIdempotencyKey(operation) && request.IdempotencyKey == "" {
		return nil, fmt.Errorf("operation %q requires an idempotency key", operation)
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode the MCP arguments")
	}
	params, err := json.Marshal(api.MCPToolsCallParams{Name: operation, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("encode the MCP tools/call parameters")
	}
	encodedID, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("encode the MCP request id")
	}
	message, err := json.Marshal(api.MCPRequest{
		JSONRPC: api.MCPJSONRPC,
		ID:      encodedID,
		Method:  api.MCPMethodToolsCall,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("encode the MCP request")
	}
	if len(message) > api.MaxEnvelopeBytes(operation) {
		return nil, fmt.Errorf("the MCP request exceeds %d bytes", api.MaxEnvelopeBytes(operation))
	}
	return message, nil
}

// decodeMCPToolsCall validates one JSON-RPC response and returns either the
// public result or the public refusal. A forged error number, unknown public
// code, or result naming another operation is a malformed response, not a
// refusal.
func decodeMCPToolsCall(body []byte, operation string) (api.Result, *APIError, error) {
	if len(body) > 3*api.MaxResponseBytes {
		return api.Result{}, nil, fmt.Errorf("the MCP response exceeds %d bytes", 3*api.MaxResponseBytes)
	}
	var response api.MCPResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return api.Result{}, nil, fmt.Errorf("the MCP response is not valid JSON-RPC")
	}
	if err := response.Validate(); err != nil {
		return api.Result{}, nil, fmt.Errorf("the MCP response is not valid JSON-RPC")
	}
	if response.Error != nil {
		public, err := api.DecodeMCPErrorData(response.Error.Data)
		if err != nil {
			return api.Result{}, nil, fmt.Errorf("the MCP error data is not a public error")
		}
		number, ok := api.MCPErrorCode(public.Code)
		if !ok {
			return api.Result{}, nil, fmt.Errorf("the MCP error data names an unknown public code")
		}
		if number != response.Error.Code {
			return api.Result{}, nil, fmt.Errorf("the MCP error number does not match its public code")
		}
		status, ok := api.HTTPStatus(public.Code)
		if !ok {
			return api.Result{}, nil, fmt.Errorf("the public code has no frozen status")
		}
		return api.Result{}, &APIError{Status: status, Code: public.Code, RequestID: public.RequestID}, nil
	}
	result, err := api.DecodeMCPResult(response.Result)
	if err != nil {
		return api.Result{}, nil, fmt.Errorf("the MCP result is not a public result")
	}
	if result.Operation != operation {
		return api.Result{}, nil, fmt.Errorf("the MCP result names operation %q, want %q", result.Operation, operation)
	}
	return result, nil, nil
}

// mcpCall sends one tools/call message and returns the decoded public result or
// the public refusal.
func mcpCall(ctx context.Context, client *API, token Secret, id, operation string, request api.Request) (api.Result, error) {
	body, err := buildMCPToolsCall(id, operation, request)
	if err != nil {
		return api.Result{}, err
	}
	response, err := client.PostMCP(ctx, token, body)
	if err != nil {
		return api.Result{}, err
	}
	result, refusal, err := decodeMCPToolsCall(response, operation)
	if err != nil {
		return api.Result{}, err
	}
	if refusal != nil {
		return api.Result{}, refusal
	}
	return result, nil
}

// stageResult is the result DTO family: every frozen result validates itself.
type stageResult interface {
	Validate() error
}

// decodeStageResult decodes one result body without echoing the body or a
// credential in a refusal message.
func decodeStageResult[T stageResult](result api.Result, operation string) (T, error) {
	var zero T
	if result.Operation != operation {
		return zero, fmt.Errorf("the result names operation %q, want %q", result.Operation, operation)
	}
	var value T
	if err := json.Unmarshal(result.Body, &value); err != nil {
		return zero, fmt.Errorf("the %s result body is not the expected DTO", operation)
	}
	if err := value.Validate(); err != nil {
		return zero, fmt.Errorf("the %s result body is invalid", operation)
	}
	return value, nil
}

// stageCredential wraps one fresh issue/rotate token as a redacting Secret. The
// prefix must match the frozen derived kind; a refusal never echoes the value.
func stageCredential(token *string, prefix string) (Secret, error) {
	if token == nil {
		return "", fmt.Errorf("the credential result carried no fresh token")
	}
	if !strings.HasPrefix(*token, prefix) {
		return "", fmt.Errorf("the issued credential does not carry the %q prefix", prefix)
	}
	if err := validateToken(*token); err != nil {
		return "", fmt.Errorf("the issued credential is not a bounded token")
	}
	return Secret(*token), nil
}

// publicRefusalError checks one public refusal class and status without a
// testing dependency, so a whole-probe harness can share the live assertion.
func publicRefusalError(err error, status int, code string) error {
	var refusal *APIError
	if !errors.As(err, &refusal) {
		return fmt.Errorf("request returned %v, want a public %s refusal", err, code)
	}
	if refusal.Status != status || refusal.Code != code {
		return fmt.Errorf("refusal = %s (status %d), want %s (status %d)", refusal.Code, refusal.Status, code, status)
	}
	return nil
}

// expectPublicRefusal asserts one public refusal class and status.
func expectPublicRefusal(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err := publicRefusalError(err, status, code); err != nil {
		t.Fatal(err)
	}
}

// startInlineExecution admits one task-free inline pipeline through the
// alternate pipeline.start binding and returns the execution identity.
func startInlineExecution(ctx context.Context, client *API, token Secret, projectID string, document []byte, idempotencyKey string) (string, error) {
	if projectID == "" {
		return "", fmt.Errorf("the inline probe requires a project")
	}
	if len(document) == 0 {
		return "", fmt.Errorf("the inline probe requires a pipeline document")
	}
	result, err := client.PostAlternate(ctx, token, "pipeline.start",
		api.PipelineStartRequest{ProjectID: projectID, YAML: document}, idempotencyKey)
	if err != nil {
		return "", err
	}
	started, err := decodeStageResult[api.PipelineStartResult](result, "pipeline.start")
	if err != nil {
		return "", err
	}
	return started.ExecutionID, nil
}

// mcpStartInlineExecution admits one task-free inline pipeline through the MCP
// tools/call surface and returns the execution identity.
func mcpStartInlineExecution(ctx context.Context, client *API, token Secret, projectID string, document []byte, idempotencyKey string) (string, error) {
	if projectID == "" {
		return "", fmt.Errorf("the MCP inline probe requires a project")
	}
	raw, err := encodeInput(api.PipelineStartRequest{ProjectID: projectID, YAML: document})
	if err != nil {
		return "", err
	}
	result, err := mcpCall(ctx, client, token, "q02-start", "pipeline.start",
		api.Request{Input: raw, IdempotencyKey: idempotencyKey})
	if err != nil {
		return "", err
	}
	started, err := decodeStageResult[api.PipelineStartResult](result, "pipeline.start")
	if err != nil {
		return "", err
	}
	return started.ExecutionID, nil
}

// inlineAction is one container action of a qualification probe document.
type inlineAction struct {
	Name     string   `yaml:"name"`
	Image    string   `yaml:"image"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Commands []string `yaml:"commands,omitempty"`
	Outputs  []string `yaml:"outputs,omitempty"`
}

// inlinePipeline is the probe document shape: ordered container actions only.
type inlinePipeline struct {
	Actions []inlineAction `yaml:"actions"`
}

// refusalPipelineDocument is a syntactically valid inline pipeline a viewer must
// never be allowed to start. If a product defect allowed the viewer through, the
// fake image would fail admission instead of starting a workload, so the probe
// can only observe a defensive refusal.
func refusalPipelineDocument() ([]byte, error) {
	return yaml.Marshal(inlinePipeline{Actions: []inlineAction{{
		Name:     "refused",
		Image:    "registry.example.com/q02/refused@sha256:" + strings.Repeat("a", 64),
		Timeout:  "2m0s",
		Commands: []string{"true"},
	}}})
}

// singleActionPipeline builds the one-action inline document used by the REST
// and MCP read probes.
func singleActionPipeline(image string) ([]byte, error) {
	if image == "" {
		return nil, fmt.Errorf("the inline read probe requires a pinned image")
	}
	return yaml.Marshal(inlinePipeline{Actions: []inlineAction{{
		Name:     "probe",
		Image:    image,
		Timeout:  "2m0s",
		Commands: []string{"true"},
	}}})
}

// renewalPaths are the fixed workload mount paths the renewal probe reads. They
// are parameterized only so the command logic can be exercised locally with
// fakes.
//
// The first action never writes a file: the pinned curl image runs as a
// non-root user and the workspace claim is root-owned, so the cross-action
// fingerprint travels through the public API as the first action's declared
// output and is materialized read-only at SettledOutput for the second action.
type renewalPaths struct {
	TokenFile     string
	CAFile        string
	SettledOutput string
}

// renewalProbe builds the two-action mounted-credential renewal document and
// its in-workload assertions.
type renewalProbe struct {
	Image         string
	FirstTimeout  time.Duration
	SecondTimeout time.Duration
	// DeadlineWait is slept by the second action before it requires a renewed
	// credential. It must exceed the first action's timeout so the check runs
	// after the first action's deadline.
	DeadlineWait time.Duration
	PollAttempts int
	PollInterval time.Duration
	Paths        renewalPaths
	// SkipValidation lets the command-logic harness use zero waits; the live
	// probe never sets it.
	SkipValidation bool
}

// Renewal probe timing. The whole probe is bounded below the 10-minute default
// go test timeout used by run-stage.sh: a 135 s wait past the first action's
// two-minute deadline plus a 200 s mounted-file poll is under six minutes once
// action startup is included. The values are pinned because the probe's proof
// depends on the wait exceeding the first deadline.
const (
	renewalFirstTimeout  = 2 * time.Minute
	renewalSecondTimeout = 10 * time.Minute
	renewalDeadlineWait  = 135 * time.Second
	renewalPollAttempts  = 40
	renewalPollInterval  = 5 * time.Second
	renewalWaitTimeout   = 8 * time.Minute
	renewalProbeTimeout  = 9 * time.Minute
)

// stageRenewalProbe returns the live probe: a two-minute first action and a
// ten-minute second action, with a bounded poll for the mounted replacement.
// The first timeout must stay above the token manager's one-minute minimum TTL
// plus activation overhead: the credential expiry is bounded by the first
// action's deadline, so an exact one-minute action would leave less than the
// minimum lifetime and fail preparation.
func stageRenewalProbe(image string) renewalProbe {
	return renewalProbe{
		Image:         image,
		FirstTimeout:  renewalFirstTimeout,
		SecondTimeout: renewalSecondTimeout,
		DeadlineWait:  renewalDeadlineWait,
		PollAttempts:  renewalPollAttempts,
		PollInterval:  renewalPollInterval,
		Paths: renewalPaths{
			TokenFile:     "/kodama/secrets/KODAMA_TOKEN",
			CAFile:        "/etc/kodama/ca/ca.crt",
			SettledOutput: "/kodama/outputs/short-lived/fingerprint",
		},
	}
}

// validateLive refuses a renewal probe that cannot prove the spec's behavior on
// a real stage.
func (probe renewalProbe) validateLive() error {
	if probe.SkipValidation {
		return nil
	}
	if probe.Image == "" {
		return fmt.Errorf("the renewal probe requires a pinned image")
	}
	if probe.FirstTimeout != renewalFirstTimeout {
		return fmt.Errorf("the renewal probe's first action timeout %s is not the pinned %s", probe.FirstTimeout, renewalFirstTimeout)
	}
	if probe.SecondTimeout != renewalSecondTimeout {
		return fmt.Errorf("the renewal probe's second action timeout %s is not the pinned %s", probe.SecondTimeout, renewalSecondTimeout)
	}
	if probe.DeadlineWait != renewalDeadlineWait {
		return fmt.Errorf("the renewal probe's deadline wait %s is not the pinned %s past the first action's deadline", probe.DeadlineWait, renewalDeadlineWait)
	}
	if probe.DeadlineWait <= probe.FirstTimeout {
		return fmt.Errorf("the renewal probe must wait past the first action's timeout before requiring a renewal")
	}
	if probe.PollAttempts != renewalPollAttempts || probe.PollInterval != renewalPollInterval {
		return fmt.Errorf("the renewal probe's poll window %d x %s is not the pinned %d x %s", probe.PollAttempts, probe.PollInterval, renewalPollAttempts, renewalPollInterval)
	}
	if probe.Paths.TokenFile != "/kodama/secrets/KODAMA_TOKEN" ||
		probe.Paths.CAFile != "/etc/kodama/ca/ca.crt" ||
		probe.Paths.SettledOutput != "/kodama/outputs/short-lived/fingerprint" {
		return fmt.Errorf("the renewal probe paths are not the frozen workload mounts")
	}
	if probe.Paths.TokenFile == "" || probe.Paths.CAFile == "" || probe.Paths.SettledOutput == "" {
		return fmt.Errorf("the renewal probe requires the workload credential, CA and settled-output paths")
	}
	return nil
}

// document builds the two-action inline pipeline. The first action declares the
// fingerprint output that the public API settles and the second action reads.
func (probe renewalProbe) document() ([]byte, error) {
	if probe.Image == "" {
		return nil, fmt.Errorf("the renewal probe requires a pinned image")
	}
	return yaml.Marshal(inlinePipeline{Actions: []inlineAction{
		{
			Name:     "short-lived",
			Image:    probe.Image,
			Timeout:  probe.FirstTimeout.String(),
			Commands: probe.firstCommands(),
			Outputs:  []string{"fingerprint"},
		},
		{
			Name:     "long-lived",
			Image:    probe.Image,
			Timeout:  probe.SecondTimeout.String(),
			Commands: probe.secondCommands(),
		},
	}})
}

// firstCommands prove the freshly mounted execution credential authenticates,
// then submit only its sha256 fingerprint through the public output operation.
// KODAMA_ACTION is the action slug, so the action id comes from the execution
// inspection (the first action is index 0); a version conflict is retried from a
// fresh inspection with a new idempotency key, exactly like the integration
// workload output submission.
func (probe renewalProbe) firstCommands() []string {
	token := shellQuote(probe.Paths.TokenFile)
	ca := shellQuote(probe.Paths.CAFile)
	return []string{
		`for tool in curl sha256sum cut sleep grep head cat; do command -v "$tool" >/dev/null 2>&1 || { echo "q02: the workload image must provide $tool" >&2; exit 1; }; done`,
		`test -s ` + token,
		`FIRST_SHA=$(sha256sum ` + token + ` | cut -d' ' -f1)`,
		`test -n "$FIRST_SHA"`,
		`attempt=0`,
		`submitted=0`,
		`while [ "$attempt" -lt 20 ]; do attempt=$((attempt+1)); INSPECT_CODE=$(curl -sS -X GET -o /dev/null -w '%{http_code}' --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION"); if [ "$INSPECT_CODE" != "200" ]; then echo "q02: the mounted execution credential was refused with status $INSPECT_CODE" >&2; exit 1; fi; INSPECT=$(curl -sS -X GET --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION"); case "$INSPECT" in *'"operation":"execution.inspect"'*) ;; *) echo "q02: the execution inspection did not return a public result" >&2; exit 1 ;; esac; VERSION=$(printf '%s' "$INSPECT" | grep -o '"version":[0-9]*' | head -n 1 | cut -d: -f2); ACTION=$(printf '%s' "$INSPECT" | grep -o '"action_id":"[^"]*"' | head -n 1 | cut -d'"' -f4); test -n "$VERSION" || { echo "q02: the inspection carried no execution version" >&2; exit 1; }; test -n "$ACTION" || { echo "q02: the inspection carried no action id" >&2; exit 1; }; BODY=$(printf '{"input":{"outputs":{"fingerprint":"%s"}},"expected_version":%s,"idempotency_key":"q02-out-%s-%s"}' "$FIRST_SHA" "$VERSION" "$attempt" "$FIRST_SHA"); OUTPUT_CODE=$(curl -sS -X PUT -o /dev/null -w '%{http_code}' --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary "$BODY" "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION/actions/$ACTION/outputs"); case "$OUTPUT_CODE" in 200) submitted=1; break ;; 409) continue ;; *) echo "q02: the fingerprint output submission was refused with status $OUTPUT_CODE" >&2; exit 1 ;; esac; done`,
		`test "$submitted" = "1" || { echo "q02: the fingerprint output submission did not converge" >&2; exit 1; }`,
	}
}

// secondCommands read the settled fingerprint of the first action from the
// read-only output mount, wait past the first action's deadline, require the
// mounted credential file to have been replaced (renewal), and require the
// replacement to authenticate against the public API. Only sha256 fingerprints
// and public status codes are ever printed.
func (probe renewalProbe) secondCommands() []string {
	token := shellQuote(probe.Paths.TokenFile)
	ca := shellQuote(probe.Paths.CAFile)
	fingerprint := shellQuote(probe.Paths.SettledOutput)
	return []string{
		`test -s ` + fingerprint,
		`FIRST_SHA=$(cat ` + fingerprint + `)`,
		`test -n "$FIRST_SHA"`,
		`START_SHA=$(sha256sum ` + token + ` | cut -d' ' -f1)`,
		`test -n "$START_SHA"`,
		`if [ "$START_SHA" != "$FIRST_SHA" ]; then echo "q02: the consecutive action did not reuse the still-current mounted credential" >&2; exit 1; fi`,
		fmt.Sprintf(`sleep %d`, int(probe.DeadlineWait.Seconds())),
		`NOW_SHA=$(sha256sum ` + token + ` | cut -d' ' -f1)`,
		fmt.Sprintf(`attempt=0; while [ "$attempt" -lt %d ] && [ "$NOW_SHA" = "$FIRST_SHA" ]; do sleep %d; NOW_SHA=$(sha256sum %s | cut -d' ' -f1); attempt=$((attempt+1)); done`,
			probe.PollAttempts, int(probe.PollInterval.Seconds()), token),
		`if [ "$NOW_SHA" = "$FIRST_SHA" ]; then echo "q02: the mounted credential was not renewed after the first action deadline" >&2; exit 1; fi`,
		`NOW_CODE=$(curl -sS -X GET -o /dev/null -w '%{http_code}' --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION")`,
		`if [ "$NOW_CODE" != "200" ]; then echo "q02: the renewed mounted credential was refused with status $NOW_CODE" >&2; exit 1; fi`,
		`echo "q02-renewal: the mounted credential was renewed and authenticated after the first action deadline"`,
	}
}

// terminalExecutionState reports whether the state ends the execution.
func terminalExecutionState(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// waitExecutionTerminal inspects one execution until it reaches a terminal
// state or the bounded timeout expires.
func waitExecutionTerminal(ctx context.Context, client *API, token Secret, executionID string, interval, timeout time.Duration) (api.ExecutionInspectResult, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		result, err := client.Get(ctx, token, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "")
		if err != nil {
			return api.ExecutionInspectResult{}, fmt.Errorf("the execution inspection was refused: %v", err)
		}
		inspect, err := decodeStageResult[api.ExecutionInspectResult](result, "execution.inspect")
		if err != nil {
			return api.ExecutionInspectResult{}, err
		}
		if terminalExecutionState(inspect.State) {
			return inspect, nil
		}
		if !time.Now().Before(deadline) {
			return inspect, fmt.Errorf("the execution did not reach a terminal state within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return inspect, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// stageEvidence is one probe's allowlisted evidence record.
type stageEvidence struct {
	Name         string
	Probe        string
	Started      time.Time
	Finished     time.Time
	Outcome      string
	ExecutionID  string
	ResourceID   string
	ExtraSecrets []Secret
}

// writeStageEvidence writes one allowlisted record through the foundation
// writer. Configured and issued tokens are registered so a credential can never
// reach the file, and no rejected value is echoed.
func writeStageEvidence(t *testing.T, config Config, evidence stageEvidence) string {
	t.Helper()
	writer, err := NewEvidenceWriter(config.EvidenceDir)
	if err != nil {
		t.Fatalf("the configured evidence directory is unusable: %v", err)
	}
	writer.Secrets = config.tokens()
	writer.Secrets = append(writer.Secrets, evidence.ExtraSecrets...)
	outcome := evidence.Outcome
	if outcome == "" {
		outcome = "passed"
	}
	record := map[string]any{
		"probe":       evidence.Probe,
		"recorded_at": evidence.Finished.UTC(),
		"started_at":  evidence.Started.UTC(),
		"finished_at": evidence.Finished.UTC(),
		"duration_ms": evidence.Finished.Sub(evidence.Started).Milliseconds(),
		"outcome":     outcome,
	}
	if evidence.ExecutionID != "" {
		record["execution_id"] = evidence.ExecutionID
	}
	if evidence.ResourceID != "" {
		record["resource_id"] = evidence.ResourceID
	}
	if config.HarnessRevision != "" {
		record["harness_revision"] = config.HarnessRevision
	}
	if config.DeployedRevision != "" {
		record["deployed_revision"] = config.DeployedRevision
	}
	if digest := config.ImageDigest(); digest != "" {
		record["image_digest"] = digest
	}
	path, err := writer.Write(evidence.Name, record)
	if err != nil {
		t.Fatalf("write %s evidence: %v", evidence.Name, err)
	}
	return path
}

// shellQuote quotes one value for the generated POSIX shell script.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
