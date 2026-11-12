//go:build integration

package services

// MCP proxy authority: the optional external tool proxy on the real factory
// composition.
//
// The test drives the real served edge, the real authorizer and current
// authority reader, the real configured mcphttp driver and the real project
// secret store. Only the upstream tool is the deterministic bounded loopback
// MCP fixture in test/support/factory. Every observation is a durable row, an
// upstream request, a retained log or a public diagnostic; nothing fabricates a
// tool result or an authority grant.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const mcpProxyActionTimeout = "10m"

// mcpProxyPipelineDoc is one container pipeline that keeps its action running
// so a real execution credential exists while the test presents it.
type mcpProxyPipelineDoc struct {
	Actions []mcpProxyActionDoc `yaml:"actions"`
}

// mcpProxyActionDoc is one container action of the hold pipeline.
type mcpProxyActionDoc struct {
	Name     string   `yaml:"name"`
	Image    string   `yaml:"image"`
	Commands []string `yaml:"commands"`
	Timeout  string   `yaml:"timeout"`
}

// mcpProxyArguments is the exact configured tool argument value.
const mcpProxyArguments = `{"query":"authority","limit":2}`

// mcpProxyExecutionArguments is a distinct bounded argument value for the
// execution call. It is deliberately a different LENGTH from
// mcpProxyArguments (32 vs 31 bytes), because the usage row records only
// request_bytes; equal lengths would let a swapped attribution pass. Each
// usage row's measured request is therefore matched to the call that produced
// it instead of relying on row order.
const mcpProxyExecutionArguments = `{"query":"authority","limit":10}`

// TestFactoryMCPProxyAuthority proves the configured external MCP proxy obeys
// the current caller authority on the real composition:
//
//   - an engineer service and a running container execution may invoke only the
//     configured public alias;
//   - a service without the engineer role, an execution for an unconfigured
//     alias and the configured upstream name are refused before any upstream
//     request;
//   - a task wait revokes the running execution credential, so the same
//     credential can no longer invoke the tool and the upstream sees no replay;
//   - the configured upstream credential is used only for the upstream header,
//     never in request bodies, retained logs, usage details or public
//     diagnostics.
//
// Mutation reasoning: dropping the current-authority re-read lets the revoked
// execution credential reach the upstream; dropping the configured allow-list
// lets the denied alias or the internal upstream name through; forwarding the
// caller credential instead of the configured mapping changes the fixture's
// observed Authorization header; recording a resolved value in a usage detail,
// log or diagnostic leaks it to the caller.
func TestFactoryMCPProxyAuthority(t *testing.T) {
	fixture := factory.NewToolFixture(t)
	harness := factory.Start(t, factory.WithToolFixture(fixture))

	engineerCredential := serviceCredentialWithRoles(t, harness, "mcp-engineer", []string{"engineer"})
	operatorCredential := serviceCredentialWithRoles(t, harness, "mcp-operator", []string{"operator"})
	arguments := json.RawMessage(mcpProxyArguments)
	executionArguments := json.RawMessage(mcpProxyExecutionArguments)

	// Service authority: the engineer service invokes the configured alias over
	// the public MCP edge. The fixture must observe exactly one authenticated
	// tools/call with the configured upstream name and the exact arguments.
	serviceResult := harness.MustMCP(engineerCredential, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: factory.ProjectID,
		Tool:      factory.ToolAlias,
		Arguments: arguments,
	}, nil, "mcp-service-allowed")
	var serviceTool api.ToolInvokeResult
	harness.Decode(serviceResult, &serviceTool)
	if serviceTool.IsError || serviceTool.Content == "" {
		t.Fatalf("engineer service tool result = %+v; want a settled non-error result", serviceTool)
	}
	fixture.AssertAuthenticated(t)
	if got := fixture.CallCount(); got != 1 {
		t.Fatalf("upstream calls after the service invocation = %d; want 1", got)
	}
	calls := fixture.Calls()
	if calls[0].Tool != factory.ToolUpstreamName {
		t.Fatalf("service upstream tool = %q; want the configured upstream name %q", calls[0].Tool, factory.ToolUpstreamName)
	}
	if string(calls[0].Arguments) != mcpProxyArguments {
		t.Fatalf("service upstream arguments = %s; want the exact configured arguments %s", calls[0].Arguments, mcpProxyArguments)
	}
	if !strings.Contains(serviceTool.Content, factory.ToolAnswer) {
		t.Fatalf("service tool content = %q; want the bounded fixture answer", serviceTool.Content)
	}
	mcpProxyAssertCredentialFree(t, "service tool content", serviceTool.Content,
		engineerCredential, operatorCredential, harness.ProjectToken)

	// Service authority is current membership and role: an operator service has
	// no engineer grant, so the call is refused before any upstream request.
	_, operatorRefusal := harness.REST(operatorCredential, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: factory.ProjectID,
		Tool:      factory.ToolAlias,
		Arguments: arguments,
	}, nil, "mcp-service-operator")
	if operatorRefusal.Code != api.CodeForbidden {
		t.Fatalf("operator service refusal = %q (%s); want %q", operatorRefusal.Code, operatorRefusal.Message, api.CodeForbidden)
	}
	mcpProxyAssertCredentialFree(t, "operator refusal diagnostic", operatorRefusal.Message,
		engineerCredential, operatorCredential, harness.ProjectToken)
	fixture.AssertCredentialAbsent(t, operatorRefusal.Message)
	if got := fixture.CallCount(); got != 1 {
		t.Fatalf("upstream calls after the operator refusal = %d; want 1", got)
	}

	// The public allow-list is exact: a plausible but unconfigured alias and
	// the internal upstream name are both not_found and never reach the
	// configured upstream.
	for _, denied := range []string{factory.ToolDeniedAlias, factory.ToolUpstreamName} {
		_, publicError := harness.REST(engineerCredential, "tool.invoke", api.ToolInvokeRequest{
			ProjectID: factory.ProjectID,
			Tool:      denied,
			Arguments: arguments,
		}, nil, "")
		if publicError.Code != api.CodeNotFound {
			t.Fatalf("denied tool %q refusal = %q (%s); want %q", denied, publicError.Code, publicError.Message, api.CodeNotFound)
		}
		mcpProxyAssertCredentialFree(t, "denied tool diagnostic", publicError.Message,
			engineerCredential, operatorCredential, harness.ProjectToken)
		fixture.AssertCredentialAbsent(t, publicError.Message)
	}
	if got := fixture.CallCount(); got != 1 {
		t.Fatalf("upstream calls after denied aliases = %d; want 1", got)
	}

	// Current execution authority: start a real task-backed execution whose
	// container action keeps running, then present its materialized credential.
	applied := mcpProxyApplyHoldPipeline(t, harness, "mcp-proxy-authority")
	created := harness.MustREST(harness.ProjectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "mcp proxy execution authority",
		PipelineID: applied.PipelineID,
	}, nil, "mcp-proxy-task-create")
	var started api.TaskCreateAndStartResult
	harness.Decode(created, &started)
	waitRequestRunning(t, harness, started.ExecutionID, "hold")
	executionCredential := mcpProxyWaitExecutionCredential(t, harness, started.ExecutionID, "hold")

	// Positive control: the execution credential authenticates while the
	// execution is running; the tool call is authorized by the running action
	// and its deadline, not by the human token that started the task.
	selfResult, selfError := harness.REST(executionCredential, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: started.ExecutionID,
	}, nil, "")
	if selfError.Code != "" {
		t.Fatalf("running execution credential was refused: %s (%s)", selfError.Code, selfError.Message)
	}
	var selfInspect api.ExecutionInspectResult
	harness.Decode(selfResult, &selfInspect)
	if selfInspect.ExecutionID != started.ExecutionID || selfInspect.State != "running" {
		t.Fatalf("execution credential self-inspect = %+v; want the running execution %q", selfInspect, started.ExecutionID)
	}

	executionResult, executionError := harness.REST(executionCredential, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: factory.ProjectID,
		Tool:      factory.ToolAlias,
		Arguments: executionArguments,
	}, nil, "mcp-execution-allowed")
	if executionError.Code != "" {
		t.Fatalf("running execution tool refusal = %q (%s); want success", executionError.Code, executionError.Message)
	}
	var executionTool api.ToolInvokeResult
	harness.Decode(executionResult, &executionTool)
	if executionTool.IsError || executionTool.Content == "" {
		t.Fatalf("running execution tool result = %+v; want a settled non-error result", executionTool)
	}
	mcpProxyAssertCredentialFree(t, "execution tool content", executionTool.Content,
		engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
	if got := fixture.CallCount(); got != 2 {
		t.Fatalf("upstream calls after the execution invocation = %d; want 2", got)
	}
	fixture.AssertAuthenticated(t)

	// The same current execution authority is still bounded by the configured
	// allow-list.
	if _, publicError := harness.REST(executionCredential, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: factory.ProjectID,
		Tool:      factory.ToolDeniedAlias,
		Arguments: arguments,
	}, nil, ""); publicError.Code != api.CodeNotFound {
		t.Fatalf("execution denied-tool refusal = %q (%s); want %q", publicError.Code, publicError.Message, api.CodeNotFound)
	}
	if got := fixture.CallCount(); got != 2 {
		t.Fatalf("upstream calls after the execution denied alias = %d; want 2", got)
	}

	// Wait revocation: the handoff cancels the execution, clears the stored
	// credential and leaves the task waiting. The old credential must fail
	// authentication, and the upstream must observe no replay.
	nonceBefore, err := harness.ExecutionCredentialNoncePresent(t.Context(), started.ExecutionID)
	if err != nil {
		t.Fatalf("read execution credential nonce before the wait: %v", err)
	}
	if !nonceBefore {
		t.Fatal("positive control: the running execution has no stored credential nonce")
	}
	mcpProxyAskTaskCurrent(t, harness, started.TaskID, "mcp-proxy-wait", "role/reviewer")
	nonceAfter, err := harness.ExecutionCredentialNoncePresent(t.Context(), started.ExecutionID)
	if err != nil {
		t.Fatalf("read execution credential nonce after the wait: %v", err)
	}
	if nonceAfter {
		t.Fatal("task.ask left the execution credential nonce stored; the wait did not revoke the credential")
	}

	revokedResult, revokedError := harness.REST(executionCredential, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: factory.ProjectID,
		Tool:      factory.ToolAlias,
		Arguments: arguments,
	}, nil, "")
	if revokedError.Code != api.CodeUnauthenticated {
		t.Fatalf("revoked execution credential tool refusal = %q (%s); want %q", revokedError.Code, revokedError.Message, api.CodeUnauthenticated)
	}
	mcpProxyAssertCredentialFree(t, "revoked tool diagnostic", revokedError.Message,
		engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
	fixture.AssertCredentialAbsent(t, revokedError.Message)
	if got := fixture.CallCount(); got != 2 {
		t.Fatalf("upstream calls after the revoked credential = %d; want 2 (no replay)", got)
	}
	if len(revokedResult.Body) != 0 {
		t.Fatal("a revoked execution credential received a tool result")
	}

	// The cancelled execution is terminal and its retained logs carry no
	// credential. Reading them through the public edge keeps the evidence path
	// honest.
	cancelled := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("execution state after the wait = %q; want cancelled", cancelled.State)
	}
	executionLogs := mcpProxyExecutionLogs(t, harness, harness.ProjectToken, cancelled)
	mcpProxyAssertCredentialFree(t, "retained execution logs", executionLogs,
		engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
	mcpProxyAssertCredentialFree(t, "composition logs", harness.Logs(),
		engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
	fixture.AssertCredentialAbsent(t, executionLogs, harness.Logs())

	// Two allowed calls committed exactly two attributable usage rows; every
	// refused call committed none. The harness clock is fixed, so the two rows
	// can share the same timestamp and their (at, id) order is not meaningful:
	// attribution is matched by actor and tied to a call by its measured request.
	usage, err := harness.ToolUsageRows(t.Context())
	if err != nil {
		t.Fatalf("read tool usage rows: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("tool usage rows = %d; want 2 (one per allowed call)", len(usage))
	}
	upstreamCalls := fixture.Calls()
	if len(upstreamCalls) != 2 {
		t.Fatalf("upstream observed %d requests; want exactly the two allowed calls", len(upstreamCalls))
	}
	for index, call := range upstreamCalls {
		if call.Tool != factory.ToolUpstreamName {
			t.Fatalf("upstream request %d tool = %q; want the configured upstream name %q", index, call.Tool, factory.ToolUpstreamName)
		}
	}
	if string(upstreamCalls[1].Arguments) != mcpProxyExecutionArguments {
		t.Fatalf("execution upstream arguments = %s; want the exact execution arguments %s", upstreamCalls[1].Arguments, mcpProxyExecutionArguments)
	}
	executionPrincipalID, err := harness.ExecutionPrincipal(t.Context(), started.ExecutionID)
	if err != nil {
		t.Fatalf("read execution principal: %v", err)
	}
	const servicePrincipal = "mcp-engineer"
	if executionPrincipalID == servicePrincipal {
		t.Fatalf("execution principal = %q; want a capability identity distinct from the service principal", executionPrincipalID)
	}
	// Each allowed call is attributed to its own caller: one row to the engineer
	// service and one to the execution capability identity. A row for the
	// execution call attributed to the service or to the initiating project
	// token would otherwise pass a membership-only check. The rows record only
	// request_bytes, so the two calls must differ in length for attribution to
	// be provable.
	if len(arguments) == len(executionArguments) {
		t.Fatalf("the service and execution calls must have different request lengths so their usage rows are attributable: both %d bytes", len(arguments))
	}
	wantRequestBytes := map[string]int{
		servicePrincipal:     len(arguments),
		executionPrincipalID: len(executionArguments),
	}
	attribution := make(map[string]int, 2)
	for index, row := range usage {
		want, ok := wantRequestBytes[row.Actor]
		if !ok {
			t.Fatalf("tool usage row %d actor = %q; want the engineer service principal %q or the execution principal %q",
				index, row.Actor, servicePrincipal, executionPrincipalID)
		}
		attribution[row.Actor]++
		var detail struct {
			Server          string `json:"server"`
			Tool            string `json:"tool"`
			Outcome         string `json:"outcome"`
			RequestBytes    int    `json:"request_bytes"`
			ResponseBytes   int    `json:"response_bytes"`
			ContentRetained bool   `json:"content_retained"`
		}
		if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
			t.Fatalf("tool usage row %d detail is not JSON: %v", index, err)
		}
		if detail.Server != "docs" || detail.Tool != factory.ToolUpstreamName || detail.Outcome != "ok" {
			t.Fatalf("tool usage row %d detail = %+v; want docs/%s/ok", index, detail, factory.ToolUpstreamName)
		}
		// The row measures the exact arguments of the call it is attributed to,
		// so the two rows cannot have been swapped or duplicated.
		if detail.RequestBytes != want || detail.ResponseBytes <= 0 {
			t.Fatalf("tool usage row %d actor %q sizes = %d/%d; want request %d and a bounded response",
				index, row.Actor, detail.RequestBytes, detail.ResponseBytes, want)
		}
		if detail.ContentRetained {
			t.Fatalf("tool usage row %d retained raw content; retention is off by default", index)
		}
		mcpProxyAssertCredentialFree(t, "tool usage detail", row.Detail,
			engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
		fixture.AssertCredentialAbsent(t, row.Detail)
	}
	if attribution[servicePrincipal] != 1 || attribution[executionPrincipalID] != 1 {
		t.Fatalf("tool usage attribution = %d service and %d execution rows; want exactly one of each",
			attribution[servicePrincipal], attribution[executionPrincipalID])
	}

	// Every observed upstream request body is free of the configured credential
	// and of every caller credential; the credential appears only in the
	// configured Authorization header (AssertAuthenticated above).
	for _, call := range upstreamCalls {
		mcpProxyAssertCredentialFree(t, "upstream request body", string(call.Body),
			engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
		fixture.AssertCredentialAbsent(t, string(call.Body))
		for name, values := range call.Header {
			if strings.EqualFold(name, "Authorization") {
				continue
			}
			mcpProxyAssertCredentialFree(t, "upstream request header", strings.Join(values, " "),
				engineerCredential, operatorCredential, executionCredential, harness.ProjectToken)
			fixture.AssertCredentialAbsent(t, values...)
		}
	}

	if refusals := fixture.Refusals(); len(refusals) != 0 {
		t.Fatalf("tool fixture refused %d request(s); want none: %v", len(refusals), refusals)
	}

	// Cancel the waiting task so the fixture leaves no open task.
	mcpProxyCancelTaskCurrent(t, harness, started.TaskID, "mcp-proxy-cancel")
}

// mcpProxyApplyHoldPipeline applies a pipeline with one long-running container
// action so the execution keeps a current credential while it is inspected.
func mcpProxyApplyHoldPipeline(t *testing.T, harness *factory.Harness, name string) api.PipelineApplyResult {
	t.Helper()
	document, err := yaml.Marshal(mcpProxyPipelineDoc{Actions: []mcpProxyActionDoc{{
		Name:     "hold",
		Image:    harness.Image.Reference,
		Commands: []string{`printf 'mcp-proxy-hold\n' > /workspace/marker` + "\nsleep 300"},
		Timeout:  mcpProxyActionTimeout,
	}}})
	if err != nil {
		t.Fatalf("marshal hold pipeline: %v", err)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// mcpProxyWaitExecutionCredential waits until activation has materialized the
// execution credential for the running action.
func mcpProxyWaitExecutionCredential(t *testing.T, harness *factory.Harness, executionID, actionName string) string {
	t.Helper()
	var credential string
	harness.Eventually(t, 2*time.Minute, func() bool {
		value, err := harness.ExecutionCredential(t.Context(), executionID, actionName)
		if err != nil {
			return false
		}
		credential = value
		return value != ""
	})
	if credential == "" {
		t.Fatal("the composition materialized no execution credential")
	}
	return credential
}

// mcpProxyAskTaskCurrent asks from the latest task lifecycle version. A
// concurrent activation commit can refuse a legitimate ask with
// version_conflict; the refusal commits nothing, so a fresh read and the same
// idempotency key converge.
func mcpProxyAskTaskCurrent(t *testing.T, harness *factory.Harness, taskID, key, assignment string) api.TaskAskResult {
	t.Helper()
	for range 20 {
		readResult := harness.MustREST(harness.ProjectToken, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, "")
		var current api.TaskReadResult
		harness.Decode(readResult, &current)
		result, publicError := harness.REST(harness.ProjectToken, "task.ask", api.TaskAskRequest{
			TaskID:     taskID,
			Question:   "Which durable context should the next attempt restore?",
			Assignment: assignment,
		}, &current.LifecycleVersion, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.ask refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var asked api.TaskAskResult
		harness.Decode(result, &asked)
		return asked
	}
	t.Fatal("task.ask did not converge on a current task version")
	return api.TaskAskResult{}
}

// mcpProxyCancelTaskCurrent cancels a waiting task from its latest lifecycle
// version.
func mcpProxyCancelTaskCurrent(t *testing.T, harness *factory.Harness, taskID, key string) api.TaskCancelResult {
	t.Helper()
	for range 20 {
		readResult := harness.MustREST(harness.ProjectToken, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, "")
		var current api.TaskReadResult
		harness.Decode(readResult, &current)
		result, publicError := harness.REST(harness.ProjectToken, "task.cancel", api.TaskCancelRequest{TaskID: taskID}, &current.LifecycleVersion, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var cancelled api.TaskCancelResult
		harness.Decode(result, &cancelled)
		return cancelled
	}
	t.Fatal("task.cancel did not converge on a current task version")
	return api.TaskCancelResult{}
}

// mcpProxyExecutionLogs concatenates the retained stdout and stderr chunks of
// every action of one execution.
func mcpProxyExecutionLogs(t *testing.T, harness *factory.Harness, token string, execution api.ExecutionInspectResult) string {
	t.Helper()
	var builder strings.Builder
	for _, action := range execution.Actions {
		for _, stream := range []string{"stdout", "stderr"} {
			result := harness.MustREST(token, "execution.logs.read", api.ExecutionLogsReadRequest{
				ExecutionID: execution.ExecutionID,
				ActionID:    action.ActionID,
				Stream:      stream,
			}, nil, "")
			var logs api.ExecutionLogsReadResult
			harness.Decode(result, &logs)
			for _, chunk := range logs.Chunks {
				builder.Write(chunk.Data)
			}
		}
	}
	return builder.String()
}

// mcpProxyAssertCredentialFree fails without rendering any credential when the
// evidence carries one of the caller credentials.
func mcpProxyAssertCredentialFree(t *testing.T, label, evidence string, credentials ...string) {
	t.Helper()
	for _, credential := range credentials {
		if credential != "" && strings.Contains(evidence, credential) {
			t.Fatalf("%s contains a caller credential", label)
		}
	}
}
