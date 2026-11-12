//go:build qualification

package production

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// startAuthorityTLSServer starts one TLS fixture server with the test PKI. The
// API helper verifies the leaf through that CA and the controlled DNS SAN.
func startAuthorityTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, caPath
}

// writeMCPTestResult writes one successful JSON-RPC tools/call response.
func writeMCPTestResult(t *testing.T, writer http.ResponseWriter, id, operation string, body string) {
	t.Helper()
	encoded, err := api.EncodeMCPResult(api.Result{Operation: operation, Body: json.RawMessage(body)})
	if err != nil {
		t.Errorf("encode MCP result: %v", err)
		return
	}
	response, err := json.Marshal(api.MCPResponse{
		JSONRPC: api.MCPJSONRPC,
		ID:      json.RawMessage(`"` + id + `"`),
		Result:  encoded,
	})
	if err != nil {
		t.Errorf("encode MCP response: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	_, _ = writer.Write(response)
}

// writeMCPTestError writes one JSON-RPC error response carrying a public error
// body as its data. The number is the caller's, so a forged mismatch can be
// exercised.
func writeMCPTestError(t *testing.T, writer http.ResponseWriter, id string, number int, code string) {
	t.Helper()
	data, err := api.EncodeMCPErrorData(api.Error{Code: code, Message: "public refusal"})
	if err != nil {
		t.Errorf("encode MCP error data: %v", err)
		return
	}
	response, err := json.Marshal(api.MCPResponse{
		JSONRPC: api.MCPJSONRPC,
		ID:      json.RawMessage(`"` + id + `"`),
		Error:   &api.MCPError{Code: number, Message: "public refusal", Data: data},
	})
	if err != nil {
		t.Errorf("encode MCP error response: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	_, _ = writer.Write(response)
}

// TestHarnessStageGate proves the environment-only skip rule: an unconfigured
// stage skips with the input names, a required stage fails naming them, and a
// complete stage proceeds.
func TestHarnessStageGate(t *testing.T) {
	cases := []struct {
		name     string
		required bool
		missing  MissingInputs
		skip     bool
		fail     bool
	}{
		{"complete", true, nil, false, false},
		{"unconfigured skips", false, MissingInputs{InputAdminToken, InputEvidenceDir}, true, false},
		{"required stage fails", true, MissingInputs{InputAdminToken}, false, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skip, err := stageGate(testCase.required, testCase.missing)
			if testCase.fail {
				if err == nil {
					t.Fatal("a required stage with missing inputs did not fail")
				}
				if !strings.Contains(err.Error(), string(InputAdminToken)) {
					t.Fatalf("required-stage error %q does not name the missing input", err)
				}
			} else if err != nil {
				t.Fatalf("stage gate returned %v", err)
			}
			if skip != testCase.skip {
				t.Fatalf("stage gate skip = %v, want %v", skip, testCase.skip)
			}
		})
	}
}

// TestHarnessProbeID proves the generated probe identities and idempotency keys
// are bounded, prefixed and distinct.
func TestHarnessProbeID(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		id, err := probeID("q02-probe")
		if err != nil {
			t.Fatalf("probe identity: %v", err)
		}
		if !strings.HasPrefix(id, "q02-probe-") || !identityPattern.MatchString(id) {
			t.Fatalf("probe identity %q is not a bounded prefixed identity", id)
		}
		if seen[id] {
			t.Fatalf("probe identity %q repeated", id)
		}
		seen[id] = true

		key, err := probeIdempotencyKey("q02-key")
		if err != nil {
			t.Fatalf("idempotency key: %v", err)
		}
		if !strings.HasPrefix(key, "q02-key-") || len(key) > api.MaxIdempotencyKeyBytes {
			t.Fatalf("idempotency key %q is not bounded", key)
		}
		if seen[key] {
			t.Fatalf("idempotency key %q repeated", key)
		}
		seen[key] = true
	}
}

// TestHarnessMCPToolsCall proves the MCP builder emits the frozen JSON-RPC
// tools/call message and the helper sends it to the served MCP path over
// verified TLS, then decodes the api.Result result.
func TestHarnessMCPToolsCall(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotAuth   string
		gotBody   api.MCPRequest
	)
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read MCP request: %v", err)
			return
		}
		gotPath = request.URL.Path
		gotMethod = request.Method
		gotAuth = request.Header.Get("Authorization")
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("MCP request is not JSON-RPC: %v", err)
			return
		}
		writeMCPTestResult(t, writer, "q02-1", "execution.list", `{"executions":[]}`)
	}))

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	request := api.Request{
		Input:          json.RawMessage(`{"project_id":"tenant-a"}`),
		IdempotencyKey: "",
	}
	result, err := mcpCall(context.Background(), client, Secret("member-token-value"), "q02-1", "execution.list", request)
	if err != nil {
		t.Fatalf("MCP execution.list failed: %v", err)
	}
	if result.Operation != "execution.list" {
		t.Fatalf("MCP result operation = %q, want execution.list", result.Operation)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("MCP method = %q, want POST", gotMethod)
	}
	if gotPath != "/mcp" {
		t.Errorf("MCP path = %q, want the served /mcp path", gotPath)
	}
	if gotAuth != "Bearer member-token-value" {
		t.Errorf("MCP authorization = %q, want the supplied bearer token", gotAuth)
	}
	if gotBody.JSONRPC != api.MCPJSONRPC || gotBody.Method != api.MCPMethodToolsCall {
		t.Errorf("MCP envelope = %+v, want the frozen tools/call message", gotBody)
	}
	if string(gotBody.ID) != `"q02-1"` {
		t.Errorf("MCP id = %s, want \"q02-1\"", gotBody.ID)
	}
	var params api.MCPToolsCallParams
	if err := json.Unmarshal(gotBody.Params, &params); err != nil {
		t.Fatalf("MCP params are not a tools/call object: %v", err)
	}
	if params.Name != "execution.list" {
		t.Errorf("MCP tool name = %q, want execution.list", params.Name)
	}
	var arguments api.Request
	if err := json.Unmarshal(params.Arguments, &arguments); err != nil {
		t.Fatalf("MCP arguments are not the frozen envelope: %v", err)
	}
	if string(arguments.Input) != `{"project_id":"tenant-a"}` {
		t.Errorf("MCP arguments input = %s, want the operation input", arguments.Input)
	}
}

// TestHarnessMCPToolsCallMutationRequiresKey proves the MCP builder applies the
// frozen idempotency-key rule to mutations before any request.
func TestHarnessMCPToolsCallMutationRequiresKey(t *testing.T) {
	_, err := buildMCPToolsCall("q02-1", "service.create", api.Request{Input: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "idempotency") {
		t.Fatalf("MCP mutation without a key returned %v", err)
	}
	if _, err := buildMCPToolsCall("q02-1", "execution.list", api.Request{Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("MCP read without a key was refused: %v", err)
	}
	if _, err := buildMCPToolsCall("q02-1", "no.such_operation", api.Request{}); err == nil {
		t.Fatal("MCP builder accepted an unknown operation")
	}
}

// TestHarnessMCPErrorMapping proves a JSON-RPC refusal maps to the public code
// and status, that a forged number/code pair is refused, and that the presented
// token never appears in the local error.
func TestHarnessMCPErrorMapping(t *testing.T) {
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer garbage-token":
			writeMCPTestError(t, writer, "q02-1", -32001, api.CodeUnauthenticated)
		case "Bearer mismatch-token":
			writeMCPTestError(t, writer, "q02-1", -32602, api.CodeUnauthenticated)
		case "Bearer wrong-result-token":
			writeMCPTestResult(t, writer, "q02-1", "task.inbox", `{"items":[]}`)
		default:
			writeMCPTestError(t, writer, "q02-1", -32603, api.CodeInternalFailure)
		}
	}))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	request := api.Request{Input: json.RawMessage(`{"project_id":"tenant-a"}`)}

	_, err = mcpCall(context.Background(), client, Secret("garbage-token"), "q02-1", "execution.list", request)
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("MCP refusal returned %v, want a public API error", err)
	}
	if refusal.Status != http.StatusUnauthorized || refusal.Code != api.CodeUnauthenticated {
		t.Fatalf("MCP refusal = %+v, want 401 unauthenticated", refusal)
	}
	if strings.Contains(err.Error(), "garbage-token") {
		t.Fatal("the MCP refusal leaked the presented token")
	}

	if _, err := mcpCall(context.Background(), client, Secret("mismatch-token"), "q02-1", "execution.list", request); err == nil ||
		!strings.Contains(err.Error(), "number") {
		t.Fatalf("a forged MCP error number returned %v", err)
	}

	if _, err := mcpCall(context.Background(), client, Secret("wrong-result-token"), "q02-1", "execution.list", request); err == nil ||
		!strings.Contains(err.Error(), "execution.list") {
		t.Fatalf("an MCP result naming another operation returned %v", err)
	}
}

// TestHarnessAPIAlternateBinding proves PostAlternate uses the additive
// pipeline.start binding while Post keeps the canonical stored-pipeline route.
func TestHarnessAPIAlternateBinding(t *testing.T) {
	var paths []string
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var envelope api.Request
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("request body is not the public envelope: %v", err)
			return
		}
		if request.URL.Path == "/api/v1/projects/tenant-a/executions" {
			writePublicTestResult(t, writer, "pipeline.start", `{"execution_id":"exec-inline","version":2}`)
			return
		}
		writePublicTestResult(t, writer, "pipeline.start", `{"execution_id":"exec-stored","version":3}`)
	}))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	inline, err := client.PostAlternate(context.Background(), Secret("member-token-value"), "pipeline.start",
		api.PipelineStartRequest{ProjectID: "tenant-a", YAML: []byte("actions: []\n")}, "key-inline")
	if err != nil {
		t.Fatalf("alternate pipeline.start failed: %v", err)
	}
	if inline.Operation != "pipeline.start" {
		t.Fatalf("alternate result operation = %q", inline.Operation)
	}
	stored, err := client.Post(context.Background(), Secret("member-token-value"), "pipeline.start",
		api.PipelineStartRequest{ProjectID: "tenant-a", PipelineID: "pipe-1"}, "key-stored")
	if err != nil {
		t.Fatalf("canonical pipeline.start failed: %v", err)
	}
	if stored.Operation != "pipeline.start" {
		t.Fatalf("canonical result operation = %q", stored.Operation)
	}
	want := []string{
		"POST /api/v1/projects/tenant-a/executions",
		"POST /api/v1/projects/tenant-a/pipelines/pipe-1/start",
	}
	if len(paths) != len(want) {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
	for index := range want {
		if paths[index] != want[index] {
			t.Fatalf("request paths = %v, want %v", paths, want)
		}
	}
	if _, err := client.PostAlternate(context.Background(), Secret("member-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("PostAlternate accepted an operation with no alternate binding")
	}
}

// TestHarnessAPIVersionedMutations proves the versioned helpers place
// expected_version in the envelope, use the frozen method and refuse a zero
// expected version before any request.
func TestHarnessAPIVersionedMutations(t *testing.T) {
	var (
		requests  atomic.Int32
		gotMethod string
		gotPath   string
		gotBody   api.Request
	)
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		gotMethod = request.Method
		gotPath = request.URL.Path
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not the public envelope: %v", err)
			return
		}
		if request.Method == http.MethodDelete {
			writePublicTestResult(t, writer, "service.credential.revoke", `{"version":5,"invalidated":true}`)
			return
		}
		writePublicTestResult(t, writer, "service.credential.rotate", `{"state":"current","version":4}`)
	}))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	if _, err := client.PostVersioned(context.Background(), Secret("admin-token-value"), "service.credential.rotate",
		api.ServiceCredentialRotateRequest{ServiceID: "svc-1", TTLSeconds: 900}, "key-rotate", 3); err != nil {
		t.Fatalf("versioned rotate failed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/services/svc-1/credentials/rotate" {
		t.Fatalf("versioned rotate target = %s %s", gotMethod, gotPath)
	}
	if gotBody.ExpectedVersion == nil || *gotBody.ExpectedVersion != 3 {
		t.Fatalf("versioned rotate expected_version = %v, want 3", gotBody.ExpectedVersion)
	}
	if _, err := client.DeleteVersioned(context.Background(), Secret("admin-token-value"), "service.credential.revoke",
		api.ServiceCredentialRevokeRequest{ServiceID: "svc-1"}, "key-revoke", 4); err != nil {
		t.Fatalf("versioned revoke failed: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/services/svc-1/credentials" {
		t.Fatalf("versioned revoke target = %s %s", gotMethod, gotPath)
	}
	if gotBody.ExpectedVersion == nil || *gotBody.ExpectedVersion != 4 {
		t.Fatalf("versioned revoke expected_version = %v, want 4", gotBody.ExpectedVersion)
	}
	before := requests.Load()
	if _, err := client.PostVersioned(context.Background(), Secret("admin-token-value"), "service.disable",
		api.ServiceDisableRequest{ServiceID: "svc-1"}, "key-disable", 0); err == nil {
		t.Fatal("a zero expected version was accepted")
	}
	if requests.Load() != before {
		t.Fatal("a zero expected version reached the endpoint")
	}
}

// TestHarnessInlinePipelineDocument proves the inline probe documents are
// accepted by the real pipeline decoder with the intended short-first and
// long-second action timeouts.
func TestHarnessInlinePipelineDocument(t *testing.T) {
	image := "registry.example.com/q02/probe@sha256:" + strings.Repeat("a", 64)

	single, err := singleActionPipeline(image)
	if err != nil {
		t.Fatalf("build the single-action document: %v", err)
	}
	decoded, err := pipeline.Decode(single, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("the single-action document is not a valid pipeline: %v", err)
	}
	if len(decoded.Actions) != 1 || string(decoded.Actions[0].Name) != "probe" {
		t.Fatalf("single-action document actions = %+v", decoded.Actions)
	}
	if decoded.Actions[0].Container == nil || decoded.Actions[0].Timeout != 2*time.Minute {
		t.Fatalf("single-action document = %+v, want one container action with a two-minute timeout", decoded.Actions[0])
	}

	renewal, err := stageRenewalProbe(image).document()
	if err != nil {
		t.Fatalf("build the renewal document: %v", err)
	}
	decoded, err = pipeline.Decode(renewal, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("the renewal document is not a valid pipeline: %v", err)
	}
	if len(decoded.Actions) != 2 {
		t.Fatalf("renewal document has %d actions, want 2", len(decoded.Actions))
	}
	if string(decoded.Actions[0].Name) != "short-lived" || decoded.Actions[0].Timeout != 2*time.Minute {
		t.Fatalf("first renewal action = %+v, want a two-minute short action", decoded.Actions[0])
	}
	if decoded.Actions[0].Container == nil || len(decoded.Actions[0].Container.Outputs) != 1 ||
		decoded.Actions[0].Container.Outputs[0] != "fingerprint" {
		t.Fatalf("first renewal action does not declare the fingerprint output: %+v", decoded.Actions[0])
	}
	if string(decoded.Actions[1].Name) != "long-lived" || decoded.Actions[1].Timeout != 10*time.Minute {
		t.Fatalf("second renewal action = %+v, want a ten-minute long action", decoded.Actions[1])
	}
	for index, action := range decoded.Actions {
		if action.Container == nil || len(action.Container.Commands) == 0 {
			t.Fatalf("renewal action %d has no container commands", index)
		}
		for _, command := range action.Container.Commands {
			if strings.ContainsAny(command, "\n\r") {
				t.Fatalf("renewal command is not a single shell line: %q", command)
			}
		}
	}
}

// TestHarnessRenewalProbeTiming pins the live renewal probe's short-first,
// long-second, wait-past-the-first-deadline and bounded-poll properties.
func TestHarnessRenewalProbeTiming(t *testing.T) {
	probe := stageRenewalProbe("registry.example.com/q02/probe@sha256:" + strings.Repeat("a", 64))
	if err := probe.validateLive(); err != nil {
		t.Fatalf("the live renewal probe is not well formed: %v", err)
	}
	if probe.Paths.TokenFile != "/kodama/secrets/KODAMA_TOKEN" ||
		probe.Paths.CAFile != "/etc/kodama/ca/ca.crt" ||
		probe.Paths.SettledOutput != "/kodama/outputs/short-lived/fingerprint" {
		t.Fatalf("renewal paths = %+v, want the frozen workload mounts", probe.Paths)
	}
	if probe.FirstTimeout != renewalFirstTimeout {
		t.Fatalf("first action timeout = %s, want the pinned %s", probe.FirstTimeout, renewalFirstTimeout)
	}
	if probe.DeadlineWait != renewalDeadlineWait {
		t.Fatalf("deadline wait = %s, want the pinned %s", probe.DeadlineWait, renewalDeadlineWait)
	}
	if probe.DeadlineWait <= probe.FirstTimeout {
		t.Fatalf("deadline wait = %s, want it to pass the first action's timeout %s", probe.DeadlineWait, probe.FirstTimeout)
	}
	if probe.SecondTimeout != renewalSecondTimeout {
		t.Fatalf("second action timeout = %s, want the pinned %s", probe.SecondTimeout, renewalSecondTimeout)
	}
	if probe.PollAttempts != renewalPollAttempts || probe.PollInterval != renewalPollInterval {
		t.Fatalf("poll window = %d x %s, want the pinned %d x %s", probe.PollAttempts, probe.PollInterval, renewalPollAttempts, renewalPollInterval)
	}
	wait := time.Duration(probe.DeadlineWait) + time.Duration(probe.PollAttempts)*probe.PollInterval
	if wait >= renewalProbeTimeout {
		t.Fatalf("renewal waits total %s, want them inside the probe cap %s", wait, renewalProbeTimeout)
	}
	if probe.DeadlineWait != 135*time.Second {
		t.Fatalf("deadline wait = %s, want the pinned 2m15s", probe.DeadlineWait)
	}
	if probe.PollAttempts != 40 || probe.PollInterval != 5*time.Second {
		t.Fatalf("poll window = %d x %s, want the pinned 40 x 5s", probe.PollAttempts, probe.PollInterval)
	}
	second := joinCommands(probe.secondCommands())
	if !strings.Contains(second, "sleep 135") {
		t.Fatal("no generated command sleeps the pinned 135-second deadline wait")
	}
	first := joinCommands(probe.firstCommands())
	for _, tool := range []string{"curl", "sha256sum", "cut", "sleep"} {
		if !strings.Contains(first, tool) {
			t.Fatalf("the image preflight does not name %q", tool)
		}
	}
}

// fakeInspectBody is the execution.inspect result the fake workload API returns.
// The first action's action id is the first element, exactly as the inspection
// orders the admitted actions.
const fakeInspectBody = `{"operation":"execution.inspect","body":{"execution_id":"exec-one","state":"running","version":7,"current_action":0,"actions":[{"action_id":"action-one","index":0,"name":"short-lived","kind":"container","state":"running","version":2},{"action_id":"action-two","index":1,"name":"long-lived","kind":"container","state":"pending","version":1}],"cleanup":{"state":"not_applicable"}}}`

// TestHarnessRenewalProbeCommands runs the generated shell with a fake curl and
// a local token file: the first action must inspect its own execution and submit
// the fingerprint as its declared output; the second action must fail when the
// credential was not reused at activation or not replaced after the first
// deadline, must pass after a genuine replacement, and must fail when the
// replacement does not authenticate.
func TestHarnessRenewalProbeCommands(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if os.Getenv("Q02_REQUIRE_STAGE") == "1" || os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
			t.Fatalf("sha256sum is not available on the test host: %v", err)
		}
		t.Skip("sha256sum is not available on the test host")
	}
	dir := t.TempDir()
	paths := renewalPaths{
		TokenFile:     filepath.Join(dir, "KODAMA_TOKEN"),
		CAFile:        filepath.Join(dir, "ca.crt"),
		SettledOutput: filepath.Join(dir, "fingerprint"),
	}
	if err := os.WriteFile(paths.CAFile, []byte("test CA"), 0o600); err != nil {
		t.Fatalf("write the probe CA: %v", err)
	}
	probe := renewalProbe{
		Image:          "registry.example.com/q02/probe@sha256:" + strings.Repeat("a", 64),
		FirstTimeout:   time.Minute,
		SecondTimeout:  10 * time.Minute,
		DeadlineWait:   0,
		PollAttempts:   2,
		PollInterval:   time.Second,
		Paths:          paths,
		SkipValidation: true,
	}
	firstSHA := sha256Hex("token-one")
	writeToken := func(t *testing.T, token string) {
		t.Helper()
		if err := os.WriteFile(paths.TokenFile, []byte(token), 0o600); err != nil {
			t.Fatalf("write the mounted credential: %v", err)
		}
	}
	writeSettled := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(paths.SettledOutput, []byte(firstSHA+"\n"), 0o600); err != nil {
			t.Fatalf("write the settled fingerprint: %v", err)
		}
	}
	setupFake := func(t *testing.T, status string) string {
		t.Helper()
		installFakeExecutable(t, "curl", fakeCurlScript)
		t.Setenv("Q02_FAKE_TOKEN_FILE", paths.TokenFile)
		t.Setenv("Q02_FAKE_INSPECT_BODY", fakeInspectBody)
		statusPath := filepath.Join(dir, "status-"+status)
		mustWriteFile(t, statusPath, status)
		t.Setenv("Q02_FAKE_STATUS_FILE", statusPath)
		record := filepath.Join(dir, "record-"+status)
		t.Setenv("Q02_FAKE_RECORD_FILE", record)
		return record
	}

	t.Run("first action submits the fingerprint output", func(t *testing.T) {
		record := setupFake(t, "200")
		writeToken(t, "token-one")
		if err := runShell(t, joinCommands(probe.firstCommands())); err != nil {
			t.Fatalf("the first action refused its own mounted credential: %v", err)
		}
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the first action submitted no output: %v", err)
		}
		submitted := string(data)
		for _, want := range []string{
			"PUT ",
			"/api/v1/executions/exec-one/actions/action-one/outputs",
			`"fingerprint":"` + firstSHA + `"`,
			`"expected_version":7`,
		} {
			if !strings.Contains(submitted, want) {
				t.Fatalf("the submitted output %q does not contain %q", submitted, want)
			}
		}
	})

	t.Run("first action fails on a refused credential", func(t *testing.T) {
		setupFake(t, "200")
		t.Setenv("Q02_FAKE_TOKEN_FILE", filepath.Join(dir, "other-token"))
		mustWriteFile(t, filepath.Join(dir, "other-token"), "token-other")
		writeToken(t, "token-one")
		err := runShell(t, joinCommands(probe.firstCommands()))
		if err == nil || !strings.Contains(err.Error(), "refused with status 401") {
			t.Fatalf("the first action accepted a refused credential: %v", err)
		}
	})

	t.Run("second action observes the renewal", func(t *testing.T) {
		setupFake(t, "200")
		writeToken(t, "token-one")
		writeSettled(t)
		renewing := probe
		renewing.PollAttempts = 10
		renewing.PollInterval = time.Second
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(700 * time.Millisecond)
			_ = os.WriteFile(paths.TokenFile, []byte("token-two"), 0o600)
		}()
		err := runShell(t, joinCommands(renewing.secondCommands()))
		<-done
		if err != nil {
			t.Fatalf("the second action did not observe a genuine renewal: %v", err)
		}
	})

	t.Run("second action fails when activation did not reuse the credential", func(t *testing.T) {
		setupFake(t, "200")
		writeToken(t, "token-two")
		writeSettled(t)
		err := runShell(t, joinCommands(probe.secondCommands()))
		if err == nil || !strings.Contains(err.Error(), "did not reuse") {
			t.Fatalf("the second action accepted a changed credential at activation: %v", err)
		}
	})

	t.Run("second action fails without a renewal", func(t *testing.T) {
		setupFake(t, "200")
		writeToken(t, "token-one")
		writeSettled(t)
		err := runShell(t, joinCommands(probe.secondCommands()))
		if err == nil || !strings.Contains(err.Error(), "not renewed") {
			t.Fatalf("the second action accepted an unrenewed credential: %v", err)
		}
	})

	t.Run("second action fails when the renewed credential is refused", func(t *testing.T) {
		setupFake(t, "401")
		writeToken(t, "token-one")
		writeSettled(t)
		renewing := probe
		renewing.PollAttempts = 10
		renewing.PollInterval = time.Second
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(700 * time.Millisecond)
			_ = os.WriteFile(paths.TokenFile, []byte("token-two"), 0o600)
		}()
		err := runShell(t, joinCommands(renewing.secondCommands()))
		<-done
		if err == nil || !strings.Contains(err.Error(), "refused with status 401") {
			t.Fatalf("the second action accepted a refused renewed credential: %v", err)
		}
	})
}

// TestHarnessStageCredential proves an issued service token is wrapped as a
// Secret, must carry its frozen prefix, and is never echoed by a refusal.
func TestHarnessStageCredential(t *testing.T) {
	if _, err := stageCredential(nil, "kodsvc."); err == nil {
		t.Fatal("a credential result without a token was accepted")
	}
	wrong := "plain-token-value"
	if _, err := stageCredential(&wrong, "kodsvc."); err == nil || strings.Contains(err.Error(), wrong) {
		t.Fatalf("a non-service credential returned %v; it must refuse without echoing the value", err)
	}
	oversized := "kodsvc." + strings.Repeat("a", api.MaxCredentialBytes)
	if _, err := stageCredential(&oversized, "kodsvc."); err == nil || strings.Contains(err.Error(), oversized) {
		t.Fatal("an oversized credential was accepted or echoed")
	}
	good := "kodsvc.service-one.mac"
	secret, err := stageCredential(&good, "kodsvc.")
	if err != nil {
		t.Fatalf("a service credential was refused: %v", err)
	}
	if secret.Reveal() != good || secret.String() != "<redacted>" {
		t.Fatalf("the service credential is not redacted: %q", secret.String())
	}
}

// TestHarnessStageResultDecoding proves result decoding checks the operation
// and never echoes a malformed result body.
func TestHarnessStageResultDecoding(t *testing.T) {
	good := api.Result{Operation: "execution.list", Body: json.RawMessage(`{"executions":[]}`)}
	decoded, err := decodeStageResult[api.ExecutionListResult](good, "execution.list")
	if err != nil {
		t.Fatalf("a valid execution.list result was refused: %v", err)
	}
	if len(decoded.Executions) != 0 {
		t.Fatalf("decoded executions = %+v, want none", decoded.Executions)
	}
	if _, err := decodeStageResult[api.ExecutionListResult](api.Result{Operation: "task.inbox", Body: json.RawMessage(`{"items":[]}`)}, "execution.list"); err == nil {
		t.Fatal("a result naming another operation was accepted")
	}
	sentinel := `{"executions":"sentinel-body-value"}`
	_, err = decodeStageResult[api.ExecutionListResult](api.Result{Operation: "execution.list", Body: json.RawMessage(sentinel)}, "execution.list")
	if err == nil {
		t.Fatal("a malformed result body was accepted")
	}
	if strings.Contains(err.Error(), "sentinel-body-value") {
		t.Fatalf("the decoding error echoed the result body: %v", err)
	}
}

// TestHarnessTerminalState proves the terminal-state interpretation used by the
// live renewal probe.
func TestHarnessTerminalState(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "cancelled"} {
		if !terminalExecutionState(state) {
			t.Errorf("state %q is not terminal", state)
		}
	}
	for _, state := range []string{"queued", "running", "", "unknown"} {
		if terminalExecutionState(state) {
			t.Errorf("state %q is terminal", state)
		}
	}
}

// writePublicTestResult writes one successful public result body.
func writePublicTestResult(t *testing.T, writer http.ResponseWriter, operation, body string) {
	t.Helper()
	payload, err := json.Marshal(api.Result{Operation: operation, Body: json.RawMessage(body)})
	if err != nil {
		t.Errorf("encode public result: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	_, _ = writer.Write(payload)
}

// TestHarnessStageEvidence proves the evidence wrapper writes one allowlisted
// record naming the probe, identities and outcome, and never a credential.
func TestHarnessStageEvidence(t *testing.T) {
	config := Config{EvidenceDir: t.TempDir(), HarnessRevision: "be43abd", DeployedRevision: "be43abd"}
	started := time.Now().UTC()
	finished := started.Add(150 * time.Millisecond)
	writeStageEvidence(t, config, stageEvidence{
		Name:         "stage-authority-unit",
		Probe:        "TestHarnessStageEvidence",
		Started:      started,
		Finished:     finished,
		Outcome:      "passed",
		ExecutionID:  "exec-1",
		ResourceID:   "svc-1",
		ExtraSecrets: []Secret{"issued-service-token"},
	})
	entries, err := os.ReadDir(config.EvidenceDir)
	if err != nil {
		t.Fatalf("read the evidence directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence directory has %d files, want 1", len(entries))
	}
	path := filepath.Join(config.EvidenceDir, entries[0].Name())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the evidence file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the evidence file: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("evidence file is not JSON: %v", err)
	}
	if record["probe"] != "TestHarnessStageEvidence" || record["outcome"] != "passed" {
		t.Fatalf("evidence record = %v", record)
	}
	if record["execution_id"] != "exec-1" || record["resource_id"] != "svc-1" {
		t.Fatalf("evidence identities = %v", record)
	}
	if strings.Contains(string(data), "issued-service-token") {
		t.Fatal("evidence recorded a configured credential")
	}
}

// fakeCurlScript is a curl stand-in for the workload probe. It authenticates
// against the fake token file and enforces the frozen shapes: inspect is a GET
// with the envelope body, output submission is a PUT with an envelope body, and
// the paths must name the execution and action. A GET with -w is the status
// check; a GET without -w returns the canned inspection result. The output
// submission's method, path and body are appended to the record file.
const fakeCurlScript = `#!/bin/sh
token=$(cat "$Q02_FAKE_TOKEN_FILE")
status=$(cat "$Q02_FAKE_STATUS_FILE")
method=""
data=""
want_status=0
path=""
previous=""
for argument in "$@"; do
  [ "$previous" = "-X" ] && method="$argument"
  [ "$previous" = "--data-binary" ] && data="$argument"
  [ "$argument" = "-w" ] && want_status=1
  path="$argument"
  previous="$argument"
done
case " $* " in
  *"Authorization: Bearer $token"*) ;;
  *) printf '401'; exit 0 ;;
esac
case "$method" in
  PUT)
    case "$path" in
      */api/v1/executions/exec-one/actions/action-one/outputs) ;;
      *) printf '400'; exit 0 ;;
    esac
    test -n "$data" || { printf '400'; exit 0; }
    printf 'PUT %s %s
' "$path" "$data" >> "$Q02_FAKE_RECORD_FILE"
    printf '200'
    ;;
  GET)
    case "$path" in
      */api/v1/executions/exec-one) ;;
      *) printf '400'; exit 0 ;;
    esac
    test "$data" = "{}" || { printf '400'; exit 0; }
    if [ "$want_status" = "1" ]; then
      printf '%s' "$status"
    else
      printf '%s' "$Q02_FAKE_INSPECT_BODY"
    fi
    ;;
  *)
    printf '400'
    ;;
esac
exit 0
`

// joinCommands renders the exact fail-fast script the workload runs.
func joinCommands(commands []string) string { return strings.Join(commands, "\n") }

// runShell runs one generated workload script with the probe environment.
func runShell(t *testing.T, script string) error {
	t.Helper()
	return runShellURL(t, script, "https://kodama.test.invalid:8443")
}

// runShellURL runs one generated workload script against a chosen API base URL.
func runShellURL(t *testing.T, script, apiURL string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-ec", script)
	command.Env = append(os.Environ(),
		"KODAMA_URL="+apiURL,
		"KODAMA_EXECUTION=exec-one",
		"KODAMA_ACTION=action-one",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return &shellError{err: err, output: string(output)}
	}
	return nil
}

// shellError renders the generated script's own diagnostic without a token.
type shellError struct {
	err    error
	output string
}

// Error renders the exit status and the workload's own message.
func (failure *shellError) Error() string {
	return strings.TrimSpace(failure.output) + ": " + failure.err.Error()
}

// sha256Hex returns the lower-case hex sha256 of one value.
func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// mustWriteFile writes one fixture file.
func mustWriteFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", filepath.Base(path), err)
	}
}
