// Package publiccontract pins the public and Userland contract.
//
// These tests are compile-time consumers plus deterministic table/vector
// checks. They never start a process, container, database, broker, Git server,
// agent, session, service or cloud call, and they never assemble a real Shell
// capability. Every collaborator is a fake declared in the test package.
//
// # Pinned public API
//
// The declarations below are the minimal complete public API. Names,
// signatures, JSON keys, vocabularies, bounds and refusal classes are fixed by
// these tests. The domain's separate resource-operation vocabulary
// (`git.ref.update`) is accepted by the verification/report schemas while the
// 51-operation router registry is not. Git reads are local Git RBAC
// decisions, the approval subject names the repository `resource`, and the
// public lifecycle name is `action.requested`.
//
//	api (public DTOs/schemas; imports only the standard library)
//	  Version = "v1"; MediaType = "application/json"
//	  Bounds: MaxRequestBytes 1 MiB, MaxResponseBytes 8 MiB,
//	          MaxEventDataBytes 64 KiB, MaxIdempotencyKeyBytes 128,
//	          MaxCursorBytes 512, DefaultPageLimit 50, MaxPageLimit 200,
//	          MaxModelBodyBytes 1 MiB, MaxModelMessages 128,
//	          MaxCompletionTokens 32768, MaxSubjectFields 64,
//	          MaxSubjectValueBytes 4096, MaxCredentialBytes 4096
//	  Method: GET, POST, PUT, PATCH, DELETE; (Method).Valid()
//	  Route{Operation, Method, Path, MCPTool, InputSchema, ResultSchema}
//	  Routes() []Route; LookupRoute(operation) (Route, bool)
//	  Match(method Method, path string) (Route, map[string]string, bool)
//	  Request{Input json.RawMessage, ExpectedVersion *uint64,
//	          IdempotencyKey string} with JSON keys input,
//	          expected_version, idempotency_key
//	  DecodeRequest([]byte) (Request, error); (Request).Validate() error
//	  Result{Operation, Body json.RawMessage, Replayed bool} with JSON keys
//	          operation, body, replayed; DecodeResult/Validate
//	  Invocation{Operation string, Request Request}; (Invocation).Validate()
//	  Handler interface { Invoke(context.Context, Invocation) (Result, error) }
//	  Error{Code, Message, RequestID}; (Error).Error()/Validate();
//	          ErrorCodes() []string; IsErrorCode(string) bool
//	  HTTPStatus(code string) (int, bool); HTTPStatusSuccess = 200
//	  Page{Limit int, Cursor string}; PageInfo{NextCursor string}
//	  (Page).Validate()/EffectiveLimit(); EncodeCursor/DecodeCursor
//	  Event{ID, Type, ReportType string, Source EventSource, At time.Time,
//	        Scope EventScope, SubjectID string, SubjectVersion uint64,
//	        CorrelationID, CausationID string, Data []byte}
//	  EventSource{Kind, PrincipalID}; EventScope{TenantID, ProjectID}
//	  (Event).Validate(); EventTypes() []string; EventSources() []string;
//	  CanonicalEventType(string) (string, bool); ReportTypeValid(string) bool
//	  SubjectField{Name, Kind, Value string}; Subject{Fields []SubjectField}
//	  (Subject).Validate()
//	  ResourceOperation string; ResourceOperationGitRefUpdate = "git.ref.update";
//	          ResourceOperations() []ResourceOperation; (ResourceOperation).Valid()
//	  VerifyRequest{ActorCredential, ResourceID, Operation string,
//	          Subject Subject, Approval string}; DecodeVerifyRequest/Validate
//	  VerifyResponse{Principal VerifiedPrincipal, Roles []string,
//	          Approval bool, VerificationID string}; VerifiedPrincipal{ID, Kind}
//	  ReportRequest{ResourceID, SourceEventID, EventType, Operation,
//	          VerificationID string, Subject Subject}; DecodeReportRequest/Validate
//	  ReportResponse{VerificationID, EventID string, Replayed bool}
//	  ChatMessage{Role, Content}; ChatCompletionRequest{Model, Messages,
//	          MaxTokens, Temperature *float64, Stream}; ChatUsage; ChatChoice;
//	          ChatCompletionResponse; DecodeChatCompletionRequest; Validate;
//	          ModelSubsetVersion = "openai-chat-completions.v1" (non-streaming)
//	  MCPRequest{JSONRPC, ID, Method, Params}; DecodeMCPRequest/Validate
//	  MCPMethodInitialize/MCPMethodToolsList/MCPMethodToolsCall
//	  MCPInitializeParams{ProtocolVersion, Capabilities, ClientInfo};
//	          MCPInitializeResult{ProtocolVersion, Capabilities, ServerInfo};
//	          MCPImplementation{Name, Version}
//	  MCPToolsCallParams{Name, Arguments}; MCPTool{Name, Description,
//	          InputSchema}; MCPToolsListParams{}; MCPToolsListResult{Tools};
//	          MCPError{Code, Message, Data};
//	          MCPResponse{JSONRPC, ID, Result, Error}; MCPErrorCode(code) (int, bool)
//	  EncodeMCPResult/DecodeMCPResult; EncodeMCPErrorData/DecodeMCPErrorData
//	  Output{Name, Reference string}; (Output).Validate()
//	  Code constants for all 11 public codes.
//
//	client (Userland REST client declarations; api/id/fault only)
//	  CredentialKind: session, execution, service; Credential{Kind, Token,
//	          ExpiresAt}; Validate/Usable/String
//	  CredentialSource interface { Credential(context.Context) (Credential, error) }
//	  Bearer(ctx, source, now) (string, error)
//	  RetryPolicy{MaxAttempts, BaseDelay, MaxDelay}; Validate
//	  RetryAllowed(code fault.Code, mutation, hasIdempotencyKey bool) bool
//	  RetryRequest(api.Request) api.Request
//	  Transport interface { Invoke(context.Context, api.Invocation) (api.Result, error) }
//
//	internal/userland/git (independent Git service contract)
//	  Permission R/RW/RW+/-; UpdateKind read/create/fast_forward/rewind/delete
//	  PrincipalRef; Identity{Roles, Execution, ServiceID}
//	  AccessRequest{Identity, Ref, Update}; ReadRequest{Identity}; Rule;
//	          ApprovalRule; RepositoryPolicy; ParseConfig([]byte) (Config, error);
//	          (Config).Policy(name) (RepositoryPolicy, bool);
//	          (RepositoryPolicy).Authorize(AccessRequest) (Decision, error);
//	          (RepositoryPolicy).AuthorizeRead(ReadRequest) (Decision, error);
//	          (RepositoryPolicy).ApprovalFor(ref) (string, bool)
//	  JournalVersion 1; JournalState intended/accepted/reported/refused/uncertain;
//	          JournalEntry{Version, Repository, Ref, ExpectedBase, Candidate,
//	          SourceEventID, VerificationID, State, At}; Validate;
//	          Reconcile(RefState) (RecoveryOutcome, error); Reportable();
//	          ReportRequest() (api.ReportRequest, error)
//	  Seams: CommandExecutor, ReadAuthorizer, RefObserver, JournalStore,
//	          VerificationClient, RetentionQuery, Clock
//
//	internal/userland/session (durable checkpoint reference)
//	  RefPrefix "kodama-checkpoint/"; RefVersion "v1"; MaxRefBytes 512
//	  Ref string; ParseRef; (Ref).Version()
//	  Reason missing/version_mismatch/corrupt/context_missing; RestoreError
//	  Message{Role, Content}; Record{Version, TaskID, ExecutionID, Messages,
//	          CreatedAt}; Validate; Store interface { Save; Restore }
//
//	internal/userland/skill (Markdown knowledge)
//	  Name; ParseName; MaxBodyBytes 1 MiB; Skill{Name, Path, Body}; Validate;
//	  Loader interface { Load; List }
//
//	internal/userland/agent (native agent provider contract)
//	  Message; ModelRequest/ModelResponse; ToolRequest/ToolResult; Usage;
//	          ModelPort; ToolPort; Clock
//
//	internal/userland/services (independent service adapters)
//	  ServiceKind webhook/email/timer/responder/merge_coordinator;
//	  ServiceAdapter interface { Kind() ServiceKind; Run(context.Context) error }
//	  EffectState not_started/applied/uncertain; (EffectState).Releasable()
//	  MergeKey{Repository, Ref}; Claim{Key, Owner, ExpiresAt};
//	  MergeCoordinator interface { Claim; Release }; MergeResult; Validate
//
//	internal/userland/cli (kodactl, sessions and the Git credential helper)
//	  OutputFormat table/json; Config{ServerURL, CredentialFile, Output,
//	          TrustedOrigins}; ParseConfig/Validate
//	  Origin{Scheme, Host, Path}; ParseOrigin; (Origin).String()/Matches()
//	  Session{Source client.CredentialKind, TokenPath, ExpiresAt}; Validate
//	  HelperRequest{Protocol, URL}; Validate
//	  HelperResponse{Username, Password}; String
//	  HelperDecision(config, request, credential, now) (HelperResponse, error)
//
// # Behavior rules pinned here
//
//   - Routes enumerate exactly the 51 registry operations, one REST
//     method/path and one MCP tool per operation, with the registry schema
//     names.
//     The six product-fixed endpoints are exact.
//   - Unknown operations, paths, methods, fields, unsupported MCP methods and
//     oversized bodies are refused with a stable public code.
//   - Public error codes are exactly the domain fault codes; MCP error codes
//     are a deterministic, distinct mapping of the same codes.
//   - A resource binding accepts `git.ref.update`; no public router operation
//     name is a resource operation, and `git.ref.update` is never a router
//     operation. Reads are local Git RBAC decisions and there is no
//     `git.ref.read` resource operation.
//   - The Git approval subject uses `operation`, `resource` (the repository),
//     `ref`, `expected_base` and `candidate`; `repository` is not an alias.
//   - The canonical public lifecycle name is `action.requested`; the domain's
//     `request.activated` is an internal alias the public edge canonicalizes.
//   - A verified report carries the binding-declared report type under the
//     trusted `resource.verified` envelope type; a lifecycle type is never a
//     report type and a report never carries actor authority or a credential.
//   - Cursors are opaque versioned base64url tokens without scope or secrets;
//     page limits are bounded and stable.
//   - The OpenAI-compatible model subset and the MCP subset are explicit and
//     refuse unsupported fields.
//   - Artifact references are ordinary strings; there is no artifact entity,
//     route or operation.
package publiccontract

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// wantRoute is the frozen public route inventory. Paths use `{name}` parameter
// segments; the product-fixed endpoints in S08 are reproduced exactly.
type wantRoute struct {
	operation string
	method    api.Method
	path      string
}

var wantRoutes = []wantRoute{
	{"tenant.create", api.MethodPost, "/api/v1/tenants"},
	{"tenant.rotate_key", api.MethodPost, "/api/v1/tenants/{tenant}/rotate-key"},
	{"project.create", api.MethodPost, "/api/v1/tenants/{tenant}/projects"},
	{"project.update", api.MethodPatch, "/api/v1/projects/{project}"},
	{"membership.grant", api.MethodPut, "/api/v1/projects/{project}/memberships/{principal}"},
	{"membership.revoke", api.MethodDelete, "/api/v1/projects/{project}/memberships/{principal}"},
	{"membership.list", api.MethodGet, "/api/v1/projects/{project}/memberships"},

	{"resource.binding.register", api.MethodPost, "/api/v1/projects/{project}/bindings"},
	{"resource.binding.update", api.MethodPatch, "/api/v1/bindings/{binding}"},
	{"resource.binding.delete", api.MethodDelete, "/api/v1/bindings/{binding}"},
	{"resource.binding.read", api.MethodGet, "/api/v1/bindings/{binding}"},

	{"pipeline.apply", api.MethodPut, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.read", api.MethodGet, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.update", api.MethodPatch, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.delete", api.MethodDelete, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.start", api.MethodPost, "/api/v1/projects/{project}/pipelines/{pipeline}/start"},

	{"secret.metadata.read", api.MethodGet, "/api/v1/projects/{project}/secrets/{secret}"},
	{"secret.write", api.MethodPut, "/api/v1/projects/{project}/secrets/{secret}"},
	{"secret.delete", api.MethodDelete, "/api/v1/projects/{project}/secrets/{secret}"},

	{"service.create", api.MethodPost, "/api/v1/projects/{project}/services"},
	{"service.update", api.MethodPatch, "/api/v1/services/{service}"},
	{"service.enable", api.MethodPost, "/api/v1/services/{service}/enable"},
	{"service.disable", api.MethodPost, "/api/v1/services/{service}/disable"},
	{"service.credential.issue", api.MethodPost, "/api/v1/services/{service}/credentials"},
	{"service.credential.rotate", api.MethodPost, "/api/v1/services/{service}/credentials/rotate"},
	{"service.credential.revoke", api.MethodDelete, "/api/v1/services/{service}/credentials"},

	{"task.create", api.MethodPost, "/api/v1/projects/{project}/tasks"},
	{"task.read", api.MethodGet, "/api/v1/tasks/{task}"},
	{"task.edit", api.MethodPatch, "/api/v1/tasks/{task}"},
	{"task.comment", api.MethodPost, "/api/v1/tasks/{task}/comments"},
	{"task.select_pipeline", api.MethodPut, "/api/v1/tasks/{task}/pipeline"},
	{"task.start", api.MethodPost, "/api/v1/tasks/{task}/start"},
	{"task.create_and_start", api.MethodPost, "/api/v1/projects/{project}/tasks/create-and-start"},
	{"task.ask", api.MethodPost, "/api/v1/tasks/{task}/ask"},
	{"task.reply", api.MethodPost, "/api/v1/tasks/{task}/reply"},
	{"task.cancel", api.MethodPost, "/api/v1/tasks/{task}/cancel"},
	{"task.history", api.MethodGet, "/api/v1/tasks/{task}/history"},
	{"task.inbox", api.MethodGet, "/api/v1/projects/{project}/inbox"},

	{"execution.inspect", api.MethodGet, "/api/v1/executions/{execution}"},
	{"execution.list", api.MethodGet, "/api/v1/projects/{project}/executions"},
	{"execution.cancel", api.MethodPost, "/api/v1/executions/{execution}/cancel"},
	{"execution.action.outputs", api.MethodPut, "/api/v1/executions/{execution}/actions/{action}/outputs"},
	{"execution.request.resolve", api.MethodPost, "/api/v1/executions/{execution}/actions/{action}/resolve"},
	{"execution.logs.read", api.MethodGet, "/api/v1/executions/{execution}/logs"},

	{"actor.verify", api.MethodPost, "/api/v1/verify"},
	{"resource.report", api.MethodPost, "/api/v1/reports"},

	{"event.publish", api.MethodPost, "/api/v1/projects/{project}/events"},
	{"event.read", api.MethodGet, "/api/v1/projects/{project}/events"},
	{"event.subscribe", api.MethodPost, "/api/v1/projects/{project}/events/subscribe"},

	{"model.invoke", api.MethodPost, "/api/v1/projects/{project}/models/{model}/invoke"},
	{"tool.invoke", api.MethodPost, "/api/v1/projects/{project}/tools/{tool}/invoke"},
}

// fixedRoute is one product-fixed endpoint from S08. These paths may not change
// even if the rest of the inventory is reorganized.
type fixedRoute struct {
	operation string
	method    api.Method
	path      string
}

var fixedRoutes = []fixedRoute{
	{"execution.action.outputs", api.MethodPut, "/api/v1/executions/{execution}/actions/{action}/outputs"},
	{"execution.request.resolve", api.MethodPost, "/api/v1/executions/{execution}/actions/{action}/resolve"},
	{"task.ask", api.MethodPost, "/api/v1/tasks/{task}/ask"},
	{"task.reply", api.MethodPost, "/api/v1/tasks/{task}/reply"},
	{"actor.verify", api.MethodPost, "/api/v1/verify"},
	{"execution.inspect", api.MethodGet, "/api/v1/executions/{execution}"},
}

// TestRouteInventoryAndOperationParity pins the complete REST/MCP inventory
// against the operation registry: one route per registry operation, no
// invented operation, and the registry schema names.
func TestRouteInventoryAndOperationParity(t *testing.T) {
	operations := authorize.Operations()
	if len(operations) != 51 {
		t.Fatalf("the operation registry has %d operations; the public contract pins 51", len(operations))
	}
	if len(wantRoutes) != len(operations) {
		t.Fatalf("pinned route table has %d rows; the registry has %d operations", len(wantRoutes), len(operations))
	}

	routes := api.Routes()
	if len(routes) != len(operations) {
		t.Fatalf("api.Routes() has %d rows; want %d", len(routes), len(operations))
	}
	byOperation := make(map[string]api.Route, len(routes))
	for _, route := range routes {
		if _, duplicate := byOperation[route.Operation]; duplicate {
			t.Fatalf("api.Routes() repeats operation %q", route.Operation)
		}
		byOperation[route.Operation] = route
	}

	wantByOperation := make(map[string]wantRoute, len(wantRoutes))
	for _, row := range wantRoutes {
		wantByOperation[row.operation] = row
	}

	for _, operation := range operations {
		name := string(operation.Name)
		want, ok := wantByOperation[name]
		if !ok {
			t.Errorf("pinned route table is missing registry operation %q", name)
			continue
		}
		route, ok := byOperation[name]
		if !ok {
			t.Errorf("api.Routes() is missing registry operation %q", name)
			continue
		}
		if route.Method != want.method || route.Path != want.path {
			t.Errorf("%s route = %s %s; want %s %s", name, route.Method, route.Path, want.method, want.path)
		}
		if route.InputSchema != operation.InputSchema {
			t.Errorf("%s input schema = %q; want %q", name, route.InputSchema, operation.InputSchema)
		}
		if route.ResultSchema != operation.ResultSchema {
			t.Errorf("%s result schema = %q; want %q", name, route.ResultSchema, operation.ResultSchema)
		}
		if route.MCPTool != name {
			t.Errorf("%s MCP tool = %q; want the operation name", name, route.MCPTool)
		}
	}

	for name := range byOperation {
		if _, ok := wantByOperation[name]; !ok {
			t.Errorf("api.Routes() invents operation %q outside the operation registry", name)
		}
	}

	if _, ok := api.LookupRoute("task.create"); !ok {
		t.Fatal("LookupRoute refused a registered operation")
	}
	for _, unknown := range []string{"", "task.unknown", "git.ref.update", "artifact.register"} {
		if route, ok := api.LookupRoute(unknown); ok {
			t.Errorf("LookupRoute(%q) = %+v; want refusal", unknown, route)
		}
	}
}

// TestRoutePathsAndFixedEndpoints pins path syntax, parameter names, uniqueness
// and the product-fixed endpoints.
func TestRoutePathsAndFixedEndpoints(t *testing.T) {
	allowedParams := map[string]bool{
		"tenant": true, "project": true, "principal": true, "binding": true,
		"pipeline": true, "secret": true, "service": true, "task": true,
		"execution": true, "action": true, "model": true, "tool": true,
	}
	seen := make(map[string]bool, len(wantRoutes))
	for _, route := range api.Routes() {
		key := string(route.Method) + " " + route.Path
		if seen[key] {
			t.Errorf("duplicate method/path route %s", key)
		}
		seen[key] = true

		if !strings.HasPrefix(route.Path, "/api/v1/") {
			t.Errorf("%s path %q does not start with /api/v1/", route.Operation, route.Path)
		}
		if strings.HasSuffix(route.Path, "/") {
			t.Errorf("%s path %q has a trailing slash", route.Operation, route.Path)
		}
		if strings.ContainsAny(route.Path, "?#* ") {
			t.Errorf("%s path %q contains a query, fragment, glob or space", route.Operation, route.Path)
		}
		if !route.Method.Valid() {
			t.Errorf("%s method %q is not a frozen method", route.Operation, route.Method)
		}
		for _, name := range pathParams(t, route.Path) {
			if !allowedParams[name] {
				t.Errorf("%s path %q uses unknown parameter {%s}", route.Operation, route.Path, name)
			}
			if strings.Count(route.Path, "{"+name+"}") != 1 {
				t.Errorf("%s path %q repeats parameter {%s}", route.Operation, route.Path, name)
			}
		}
	}

	for _, want := range fixedRoutes {
		route, ok := api.LookupRoute(want.operation)
		if !ok {
			t.Errorf("fixed operation %q is missing", want.operation)
			continue
		}
		if route.Method != want.method || route.Path != want.path {
			t.Errorf("fixed endpoint %s = %s %s; want %s %s",
				want.operation, route.Method, route.Path, want.method, want.path)
		}
	}

	for _, method := range []api.Method{api.MethodGet, api.MethodPost, api.MethodPut, api.MethodPatch, api.MethodDelete} {
		if !method.Valid() {
			t.Errorf("method %q is not valid", method)
		}
	}
	if api.Method("TRACE").Valid() {
		t.Error("Method(TRACE).Valid() = true; want false")
	}
}

// TestRouteMatch pins the matcher: concrete paths resolve to the frozen route,
// parameters are extracted, and unknown, trailing-slash or query-bearing paths
// refuse.
func TestRouteMatch(t *testing.T) {
	for _, want := range wantRoutes {
		path, params := concretePath(want.path)
		route, got, ok := api.Match(want.method, path)
		if !ok {
			t.Errorf("Match(%s %s) refused a frozen route", want.method, path)
			continue
		}
		if route.Operation != want.operation {
			t.Errorf("Match(%s %s) = %q; want %q", want.method, path, route.Operation, want.operation)
		}
		if !reflect.DeepEqual(got, params) {
			t.Errorf("Match(%s %s) params = %v; want %v", want.method, path, got, params)
		}
	}

	cases := []struct {
		method api.Method
		path   string
	}{
		{api.MethodGet, "/api/v1/tasks/t-1/"},
		{api.MethodGet, "/api/v1/tasks/t-1?limit=1"},
		{api.MethodGet, "/api/v1/tasks/t-1/history/extra"},
		{api.MethodGet, "/api/v1/unknown"},
		{api.Method("TRACE"), "/api/v1/tasks/t-1"},
		{api.MethodDelete, "/api/v1/tasks/t-1"},
		{api.MethodGet, ""},
		{api.MethodGet, "/v1/tasks/t-1"},
		{api.MethodGet, "api/v1/tasks/t-1"},
	}
	for _, testCase := range cases {
		if route, params, ok := api.Match(testCase.method, testCase.path); ok {
			t.Errorf("Match(%s %q) = %q %v; want refusal", testCase.method, testCase.path, route.Operation, params)
		}
	}

	// A literal segment is not captured as a parameter of a different route.
	route, params, ok := api.Match(api.MethodPost, "/api/v1/projects/proj-1/tasks/create-and-start")
	if !ok || route.Operation != "task.create_and_start" {
		t.Fatalf("Match(create-and-start) = %q %v %v; want task.create_and_start", route.Operation, params, ok)
	}
	if !reflect.DeepEqual(params, map[string]string{"project": "proj-1"}) {
		t.Fatalf("create-and-start params = %v; want only project", params)
	}
}

// TestPublicErrorCodesMatchDomain pins the public code vocabulary to the domain
// fault table and the error body representation.
func TestPublicErrorCodesMatchDomain(t *testing.T) {
	domainCodes := fault.Codes()
	publicCodes := api.ErrorCodes()
	if len(publicCodes) != len(domainCodes) || len(publicCodes) != 11 {
		t.Fatalf("public codes = %v (%d); want the 11 domain fault codes %v", publicCodes, len(publicCodes), domainCodes)
	}
	domain := make(map[string]bool, len(domainCodes))
	for _, code := range domainCodes {
		domain[string(code)] = true
	}
	seen := make(map[string]bool, len(publicCodes))
	for _, code := range publicCodes {
		if !domain[code] {
			t.Errorf("public code %q is not a domain fault code", code)
		}
		if seen[code] {
			t.Errorf("public code %q is repeated", code)
		}
		seen[code] = true
		if !api.IsErrorCode(code) {
			t.Errorf("IsErrorCode(%q) = false", code)
		}
	}
	for _, code := range domainCodes {
		if !seen[string(code)] {
			t.Errorf("public codes are missing domain code %q", code)
		}
	}
	for _, unknown := range []string{"", "teapot", "Invalid_Input"} {
		if api.IsErrorCode(unknown) {
			t.Errorf("IsErrorCode(%q) = true; want false", unknown)
		}
	}

	body, err := json.Marshal(api.Error{Code: api.CodeInvalidInput, Message: "bad request", RequestID: "req-1"})
	if err != nil {
		t.Fatalf("marshal error body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if decoded["code"] != "invalid_input" || decoded["message"] != "bad request" || decoded["request_id"] != "req-1" {
		t.Fatalf("error body keys = %v; want code/message/request_id", decoded)
	}

	if err := (api.Error{Code: api.CodeInvalidInput, Message: "bad"}).Validate(); err != nil {
		t.Errorf("valid error body refused: %v", err)
	}
	if err := (api.Error{Code: "teapot", Message: "bad"}).Validate(); err == nil {
		t.Error("Validate accepted an unknown code")
	}
	if err := (api.Error{Code: api.CodeForbidden, Message: ""}).Validate(); err == nil {
		t.Error("Validate accepted an empty message")
	}
	rendered := (api.Error{Code: api.CodeForbidden, Message: "denied"}).Error()
	if !strings.Contains(rendered, "forbidden") || !strings.Contains(rendered, "denied") {
		t.Errorf("Error() = %q; want code and message", rendered)
	}
}

// TestMCPOperationMapping pins the MCP JSON-RPC subset: every operation is one
// tools/call with the operation name, the tool catalog is advertised by
// tools/list, unsupported methods refuse, and MCP error codes deterministically
// map the public codes.
func TestMCPOperationMapping(t *testing.T) {
	if api.MCPMethodToolsCall != "tools/call" || api.MCPMethodInitialize != "initialize" {
		t.Fatalf("MCP methods = %q/%q; want tools/call and initialize", api.MCPMethodToolsCall, api.MCPMethodInitialize)
	}
	if api.MCPMethodToolsList != "tools/list" {
		t.Fatalf("MCP tools/list method = %q; want tools/list", api.MCPMethodToolsList)
	}
	if api.MCPProtocolVersion == "" {
		t.Fatal("MCPProtocolVersion is empty")
	}

	for _, operation := range authorize.OperationNames() {
		name := string(operation)
		params, err := json.Marshal(api.MCPToolsCallParams{Name: name, Arguments: json.RawMessage(`{"input":{}}`)})
		if err != nil {
			t.Fatalf("marshal tools/call params for %s: %v", name, err)
		}
		request, err := api.DecodeMCPRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(params) + `}`))
		if err != nil {
			t.Fatalf("DecodeMCPRequest(%s): %v", name, err)
		}
		if err := request.Validate(); err != nil {
			t.Errorf("%s tools/call refused: %v", name, err)
		}
	}

	initialize, err := api.DecodeMCPRequest([]byte(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kodama-test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("DecodeMCPRequest(initialize): %v", err)
	}
	if err := initialize.Validate(); err != nil {
		t.Errorf("initialize refused: %v", err)
	}

	// tools/list is the authorized MCP tool-listing method: it accepts an
	// absent or empty parameter object and never dispatches an operation.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	} {
		toolsList, err := api.DecodeMCPRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeMCPRequest(%s): %v", body, err)
		}
		if err := toolsList.Validate(); err != nil {
			t.Errorf("tools/list refused: %v", err)
		}
	}

	refusals := []string{
		`{"jsonrpc":"1.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kodama-test","version":"1.0.0"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/list","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"next"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.unknown"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git.ref.update"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","arguments":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","extra":true}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","unknown":true}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":{},"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":[],"method":"tools/list"}`,
	}
	for _, body := range refusals {
		request, err := api.DecodeMCPRequest([]byte(body))
		if err == nil {
			err = request.Validate()
		}
		if err == nil {
			t.Errorf("DecodeMCPRequest(%s) was accepted; want refusal", body)
		}
	}

	wantMCPCodes := map[string]int{
		api.CodeInvalidInput:              -32602,
		api.CodeUnauthenticated:           -32001,
		api.CodeForbidden:                 -32003,
		api.CodeNotFound:                  -32004,
		api.CodeVersionConflict:           -32009,
		api.CodeChangedIdempotencyContent: -32010,
		api.CodeDeadlineExpired:           -32008,
		api.CodeUnsupportedCapability:     -32012,
		api.CodeDependencyUnavailable:     -32013,
		api.CodeUncertainExternalEffect:   -32014,
		api.CodeInternalFailure:           -32603,
	}
	used := make(map[int]string, len(wantMCPCodes))
	for code, want := range wantMCPCodes {
		got, ok := api.MCPErrorCode(code)
		if !ok {
			t.Errorf("MCPErrorCode(%q) is unmapped", code)
			continue
		}
		if got != want {
			t.Errorf("MCPErrorCode(%q) = %d; want %d", code, got, want)
		}
		if got >= 0 {
			t.Errorf("MCPErrorCode(%q) = %d; want a negative JSON-RPC code", code, got)
		}
		if other, duplicate := used[got]; duplicate {
			t.Errorf("MCPErrorCode(%q) reuses %d already used by %q", code, got, other)
		}
		used[got] = code
	}
	if _, ok := api.MCPErrorCode("teapot"); ok {
		t.Error("MCPErrorCode accepted an unknown code")
	}

	validResponses := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32003,"message":"forbidden"}}`,
	}
	for _, body := range validResponses {
		var response api.MCPResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatalf("unmarshal MCP response %s: %v", body, err)
		}
		if err := response.Validate(); err != nil {
			t.Errorf("MCP response %s refused: %v", body, err)
		}
	}
	invalidResponses := []string{
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32603,"message":"both"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":200,"message":"positive"}}`,
	}
	for _, body := range invalidResponses {
		var response api.MCPResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			continue
		}
		if err := response.Validate(); err == nil {
			t.Errorf("MCP response %s was accepted; want refusal", body)
		}
	}
}

// TestMCPHandshakeAndResult pins the initialize parameter/result declarations,
// the supported protocol version, the tools/call result encoding and the public
// error data encoding.
func TestMCPHandshakeAndResult(t *testing.T) {
	initializeBody := `{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{` +
		`"protocolVersion":"` + api.MCPProtocolVersion + `",` +
		`"capabilities":{"roots":{}},` +
		`"clientInfo":{"name":"kodama-cli","version":"1.0.0"}}}`
	request, err := api.DecodeMCPRequest([]byte(initializeBody))
	if err != nil {
		t.Fatalf("conforming initialize refused: %v", err)
	}
	var params api.MCPInitializeParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("initialize params do not decode: %v", err)
	}
	if params.ProtocolVersion != api.MCPProtocolVersion {
		t.Fatalf("initialize protocol version = %q; want %q", params.ProtocolVersion, api.MCPProtocolVersion)
	}
	if params.ClientInfo.Name != "kodama-cli" || params.ClientInfo.Version != "1.0.0" {
		t.Fatalf("initialize clientInfo = %+v; want kodama-cli/1.0.0", params.ClientInfo)
	}

	initializeRefusals := []string{
		// Unsupported protocol version.
		`{"jsonrpc":"2.0","id":"init-2","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"kodama-cli","version":"1.0.0"}}}`,
		// Missing client identity.
		`{"jsonrpc":"2.0","id":"init-3","method":"initialize","params":{"protocolVersion":"` + api.MCPProtocolVersion + `","capabilities":{}}}`,
		// Non-object capabilities.
		`{"jsonrpc":"2.0","id":"init-4","method":"initialize","params":{"protocolVersion":"` + api.MCPProtocolVersion + `","capabilities":[],"clientInfo":{"name":"kodama-cli","version":"1.0.0"}}}`,
	}
	for _, body := range initializeRefusals {
		if _, err := api.DecodeMCPRequest([]byte(body)); err == nil {
			t.Errorf("initialize %s was accepted; want refusal", body)
		}
	}

	initializeResult := api.MCPInitializeResult{
		ProtocolVersion: api.MCPProtocolVersion,
		Capabilities:    json.RawMessage(`{"tools":{}}`),
		ServerInfo:      api.MCPImplementation{Name: "kodama", Version: "1.0.0"},
	}
	if err := initializeResult.Validate(); err != nil {
		t.Fatalf("initialize result refused: %v", err)
	}
	encodedInitialize, err := json.Marshal(initializeResult)
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	var roundTrippedInitialize api.MCPInitializeResult
	if err := json.Unmarshal(encodedInitialize, &roundTrippedInitialize); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if err := roundTrippedInitialize.Validate(); err != nil {
		t.Fatalf("round-tripped initialize result refused: %v", err)
	}
	if !reflect.DeepEqual(roundTrippedInitialize, initializeResult) {
		t.Fatalf("initialize result round trip = %+v; want %+v", roundTrippedInitialize, initializeResult)
	}

	toolsResult := api.Result{
		Operation: "task.read",
		Body:      json.RawMessage(`{"id":"task-1"}`),
		Replayed:  true,
	}
	encodedResult, err := api.EncodeMCPResult(toolsResult)
	if err != nil {
		t.Fatalf("EncodeMCPResult: %v", err)
	}
	successResponse := api.MCPResponse{JSONRPC: api.MCPJSONRPC, ID: json.RawMessage(`7`), Result: encodedResult}
	if err := successResponse.Validate(); err != nil {
		t.Fatalf("tools/call success response refused: %v", err)
	}
	decodedResult, err := api.DecodeMCPResult(successResponse.Result)
	if err != nil {
		t.Fatalf("DecodeMCPResult: %v", err)
	}
	if !reflect.DeepEqual(decodedResult, toolsResult) {
		t.Fatalf("tools/call result round trip = %+v; want %+v", decodedResult, toolsResult)
	}

	publicError := api.Error{Code: api.CodeForbidden, Message: "denied", RequestID: "req-1"}
	encodedErrorData, err := api.EncodeMCPErrorData(publicError)
	if err != nil {
		t.Fatalf("EncodeMCPErrorData: %v", err)
	}
	errorNumber, ok := api.MCPErrorCode(publicError.Code)
	if !ok {
		t.Fatalf("public code %q has no MCP error number", publicError.Code)
	}
	errorResponse := api.MCPResponse{
		JSONRPC: api.MCPJSONRPC,
		ID:      json.RawMessage(`8`),
		Error:   &api.MCPError{Code: errorNumber, Message: publicError.Message, Data: encodedErrorData},
	}
	if err := errorResponse.Validate(); err != nil {
		t.Fatalf("tools/call error response refused: %v", err)
	}
	decodedError, err := api.DecodeMCPErrorData(errorResponse.Error.Data)
	if err != nil {
		t.Fatalf("DecodeMCPErrorData: %v", err)
	}
	if decodedError.Code != api.CodeForbidden {
		t.Fatalf("error data code = %q; want %q", decodedError.Code, api.CodeForbidden)
	}
}

// TestMCPResponseRequiresExactlyOneOutcome pins the response envelope: exactly
// one of result and error, a negative error code and a non-empty message.
func TestMCPResponseRequiresExactlyOneOutcome(t *testing.T) {
	id := json.RawMessage(`1`)
	both := api.MCPResponse{
		JSONRPC: api.MCPJSONRPC, ID: id,
		Result: json.RawMessage(`{}`),
		Error:  &api.MCPError{Code: -32602, Message: "bad"},
	}
	if err := both.Validate(); err == nil {
		t.Error("MCP response carrying both a result and an error was accepted")
	}
	neither := api.MCPResponse{JSONRPC: api.MCPJSONRPC, ID: id}
	if err := neither.Validate(); err == nil {
		t.Error("MCP response carrying neither a result nor an error was accepted")
	}
	positive := api.MCPResponse{JSONRPC: api.MCPJSONRPC, ID: id, Error: &api.MCPError{Code: 1, Message: "bad"}}
	if err := positive.Validate(); err == nil {
		t.Error("MCP response with a positive error code was accepted")
	}
	emptyMessage := api.MCPResponse{JSONRPC: api.MCPJSONRPC, ID: id, Error: &api.MCPError{Code: -32602}}
	if err := emptyMessage.Validate(); err == nil {
		t.Error("MCP response with an empty error message was accepted")
	}
}

// recordingHandler is the compile-only fake schema consumer: both public edges
// decode into api.Invocation and call one api.Handler.
type recordingHandler struct {
	calls  []api.Invocation
	result api.Result
	err    error
}

func (handler *recordingHandler) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	handler.calls = append(handler.calls, invocation)
	return handler.result, handler.err
}

var _ api.Handler = (*recordingHandler)(nil)

// TestSchemaConsumersCompileWithFakeHandler pins the shared handler boundary:
// one invocation type, a registered operation, and a validated result.
func TestSchemaConsumersCompileWithFakeHandler(t *testing.T) {
	handler := &recordingHandler{result: api.Result{Operation: "task.read", Body: json.RawMessage(`{"id":"task-1"}`)}}
	invocation := api.Invocation{
		Operation: "task.read",
		Request:   api.Request{Input: json.RawMessage(`{"id":"task-1"}`)},
	}
	if err := invocation.Validate(); err != nil {
		t.Fatalf("valid invocation refused: %v", err)
	}
	result, err := handler.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("fake handler: %v", err)
	}
	if len(handler.calls) != 1 || handler.calls[0].Operation != "task.read" {
		t.Fatalf("handler calls = %+v; want one task.read", handler.calls)
	}
	if result.Operation != "task.read" {
		t.Fatalf("result operation = %q; want task.read", result.Operation)
	}
	if err := result.Validate(); err != nil {
		t.Errorf("valid result refused: %v", err)
	}

	if err := (api.Invocation{Operation: "task.unknown"}).Validate(); err == nil {
		t.Error("Invocation.Validate accepted an unknown operation")
	}
	if err := (api.Invocation{Operation: "task.read", Request: api.Request{ExpectedVersion: uint64Pointer(0)}}).Validate(); err == nil {
		t.Error("Invocation.Validate accepted expected_version 0")
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

// pathParams extracts `{name}` parameters from a route template in order.
func pathParams(t *testing.T, path string) []string {
	t.Helper()
	var params []string
	for index := 0; index < len(path); index++ {
		if path[index] != '{' {
			if path[index] == '}' {
				t.Fatalf("path %q has an unmatched }", path)
			}
			continue
		}
		end := strings.IndexByte(path[index:], '}')
		if end < 0 {
			t.Fatalf("path %q has an unmatched {", path)
		}
		params = append(params, path[index+1:index+end])
		index += end
	}
	return params
}

// concretePath substitutes deterministic parameter values into a route
// template and returns the expected extracted map.
func concretePath(template string) (string, map[string]string) {
	var builder strings.Builder
	params := make(map[string]string)
	for index := 0; index < len(template); index++ {
		if template[index] != '{' {
			builder.WriteByte(template[index])
			continue
		}
		end := strings.IndexByte(template[index:], '}')
		name := template[index+1 : index+end]
		value := name + "-1"
		params[name] = value
		builder.WriteString(value)
		index += end
	}
	return builder.String(), params
}
