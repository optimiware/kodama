//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// parityOperations is the operation matrix every surface must serve with one
// schema, one authorization rule and one typed error mapping.
var parityOperations = []string{
	"pipeline.apply",
	"pipeline.update",
	"pipeline.read",
	"pipeline.start",
	"execution.inspect",
	"execution.list",
	"task.create",
	"task.read",
	"event.publish",
	"event.read",
}

// TestE2E_Parity proves the CLI, the REST edge and the MCP edge against one
// real installation: the same operations produce equivalent result bodies, the
// same expected-version and idempotency-key envelope is honoured by every
// surface, an absent role is refused with the same public error on every
// surface, and no surface renders credential material.
//
// The REST edge is the harness's public-envelope HTTP client. The MCP edge is
// the JSON-RPC tools/call subset with the identical api.Request argument
// envelope. The CLI edge is the real cli.Run entrypoint (the same dispatch
// cmd/kodactl reaches through cli.RunWithDeps) driving the real
// client.HTTPTransport over the installation's verified TLS. The CLI's
// login/session acquisition is deliberately outside this probe: the
// installation's access token is presented through an injected credential
// source so a stage port-forward with an explicit TLS server name works on
// every edge. Every captured byte is scanned for credential material.
//
// The probe is deliberately not parallel: its event.read step compares the
// page after the project's event tail across the three edges, so a sibling
// scenario publishing into the same project would change that page.
func TestE2E_Parity(t *testing.T) {
	instance := startLive(t)
	probe := newParityProbe(t, instance)
	started := time.Now().UTC()

	// The redaction scanner is asserted live before it is trusted: a configured
	// credential must be detected, not just absent from real output.
	if err := harness.ScanCredentials("prefix "+instance.token.Reveal()+" suffix", instance.token); err == nil {
		t.Fatal("the credential-redaction scan did not detect a configured credential (positive control)")
	}

	tools := probe.requireToolCatalog()

	name := "w43b-parity-" + instance.runID
	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "w43b-parity",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: []string{"echo w43b-parity-ok", "echo w43b-parity-marker"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the parity pipeline: %v", err)
	}

	// pipeline.apply through the CLI, then pipeline.read through every surface.
	applied := decodeBody[api.PipelineApplyResult](t, probe.cliResult(
		instance.token,
		"pipeline.apply",
		api.PipelineApplyRequest{ProjectID: instance.projectID, Name: name, YAML: document},
		instance.key("parity-apply"),
		nil,
	), "cli pipeline.apply")
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("cli pipeline.apply returned %+v", applied)
	}
	pipelineID := applied.PipelineID

	readAcross := func(wantVersion uint64, wantYAML []byte, label string) api.PipelineReadResult {
		t.Helper()
		input := api.PipelineReadRequest{ProjectID: instance.projectID, PipelineID: pipelineID}
		rest := probe.restResult(instance.token, api.MethodGet, "pipeline.read", input, "", nil)
		mcp := probe.mcpResult(instance.token, "pipeline.read", input, "", nil)
		cliBody := probe.cliResult(instance.token, "pipeline.read", input, "", nil)
		requireSameBody(t, label+" pipeline.read", rest.Body, mcp.Body, cliBody)
		read := decodeBody[api.PipelineReadResult](t, rest.Body, label+" pipeline.read")
		if read.PipelineID != pipelineID {
			t.Fatalf("%s pipeline.read id = %q; want %q", label, read.PipelineID, pipelineID)
		}
		if read.Name != name {
			t.Fatalf("%s pipeline.read name = %q; want %q", label, read.Name, name)
		}
		if read.Version != wantVersion {
			t.Fatalf("%s pipeline.read version = %d; want %d", label, read.Version, wantVersion)
		}
		if string(read.YAML) != string(wantYAML) {
			t.Fatalf("%s pipeline.read returned different exact bytes", label)
		}
		return read
	}
	readAcross(applied.Version, document, "after apply")

	// The same caller key with changed content is refused identically on every
	// surface: one receipt ledger, no per-edge alternate policy.
	changedDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "w43b-parity",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: []string{"echo w43b-parity-changed"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the changed pipeline: %v", err)
	}
	changedApply := api.PipelineApplyRequest{ProjectID: instance.projectID, Name: name, YAML: changedDocument}
	requireSameFailure(t, "changed pipeline.apply content",
		namedFailure{"REST", probe.restFailure(instance.token, api.MethodPut, "pipeline.apply", changedApply, instance.key("parity-apply"), nil)},
		namedFailure{"MCP", probe.mcpFailure(instance.token, "pipeline.apply", changedApply, instance.key("parity-apply"), nil)},
		namedFailure{"CLI", probe.cliFailure(instance.token, "pipeline.apply", changedApply, instance.key("parity-apply"), nil)},
	)

	// A successful update through MCP carries the expected version from the MCP
	// argument envelope, not from any edge-local state.
	updatedDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "w43b-parity",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: []string{"echo w43b-parity-ok", "echo w43b-parity-mcp-update"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the MCP-updated pipeline: %v", err)
	}
	mcpUpdate := api.PipelineUpdateRequest{ProjectID: instance.projectID, PipelineID: pipelineID, Name: name, YAML: updatedDocument}
	mcpUpdated := decodeBody[api.PipelineUpdateResult](t, probe.mcpResult(
		instance.token, "pipeline.update", mcpUpdate, instance.key("parity-mcp-update"), &applied.Version,
	).Body, "mcp pipeline.update")
	if mcpUpdated.Version <= applied.Version {
		t.Fatalf("mcp pipeline.update version = %d; want above %d", mcpUpdated.Version, applied.Version)
	}
	readAcross(mcpUpdated.Version, updatedDocument, "after mcp update")

	// A successful update through the CLI carries --expected-version through the
	// same shared envelope.
	cliDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "w43b-parity",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: []string{"echo w43b-parity-ok", "echo w43b-parity-cli-update"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the CLI-updated pipeline: %v", err)
	}
	cliUpdate := api.PipelineUpdateRequest{ProjectID: instance.projectID, PipelineID: pipelineID, Name: name, YAML: cliDocument}
	cliUpdated := decodeBody[api.PipelineUpdateResult](t, probe.cliResult(
		instance.token, "pipeline.update", cliUpdate, instance.key("parity-cli-update"), &mcpUpdated.Version,
	), "cli pipeline.update")
	if cliUpdated.Version <= mcpUpdated.Version {
		t.Fatalf("cli pipeline.update version = %d; want above %d", cliUpdated.Version, mcpUpdated.Version)
	}
	readAcross(cliUpdated.Version, cliDocument, "after cli update")

	// A stale expected version is the same typed refusal on every surface. Each
	// edge uses its own key so the receipt ledger never answers this probe with
	// changed_idempotency_content instead of the version conflict.
	staleUpdate := api.PipelineUpdateRequest{ProjectID: instance.projectID, PipelineID: pipelineID, Name: name, YAML: updatedDocument}
	stale := requireSameFailure(t, "stale pipeline.update expected version",
		namedFailure{"REST", probe.restFailure(instance.token, api.MethodPatch, "pipeline.update", staleUpdate, instance.key("parity-stale-rest"), &applied.Version)},
		namedFailure{"MCP", probe.mcpFailure(instance.token, "pipeline.update", staleUpdate, instance.key("parity-stale-mcp"), &applied.Version)},
		namedFailure{"CLI", probe.cliFailure(instance.token, "pipeline.update", staleUpdate, instance.key("parity-stale-cli"), &applied.Version)},
	)
	if stale.code != api.CodeVersionConflict {
		t.Fatalf("stale pipeline.update code = %q; want %q", stale.code, api.CodeVersionConflict)
	}

	// pipeline.start with one caller key returns one execution through every
	// surface: the key and expected pipeline version cross the edges unchanged.
	startInput := api.PipelineStartRequest{ProjectID: instance.projectID, PipelineID: pipelineID}
	startedBody := probe.restResult(instance.token, api.MethodPost, "pipeline.start", startInput, instance.key("parity-start"), &cliUpdated.Version).Body
	startedExecution := decodeBody[api.PipelineStartResult](t, startedBody, "rest pipeline.start")
	if startedExecution.ExecutionID == "" {
		t.Fatal("rest pipeline.start returned no execution")
	}
	mcpStartBody := probe.mcpResult(instance.token, "pipeline.start", startInput, instance.key("parity-start"), &cliUpdated.Version).Body
	cliStartBody := probe.cliResult(instance.token, "pipeline.start", startInput, instance.key("parity-start"), &cliUpdated.Version)
	requireSameBody(t, "pipeline.start replay", startedBody, mcpStartBody, cliStartBody)
	executionID := startedExecution.ExecutionID

	// execution.inspect is the same recorded view through every surface.
	view := instance.waitComplete(t, executionID)
	status, err := harness.Interpret(view)
	if err != nil {
		t.Fatalf("execution %s inspection is invalid: %v", executionID, err)
	}
	if !status.Succeeded || !status.CleanupComplete {
		t.Fatalf("execution %s state = %q cleanup = %q; want succeeded with complete cleanup", executionID, status.State, status.CleanupState)
	}
	inspectInput := api.ExecutionInspectRequest{ExecutionID: executionID}
	restInspect := probe.restResult(instance.token, api.MethodGet, "execution.inspect", inspectInput, "", nil)
	mcpInspect := probe.mcpResult(instance.token, "execution.inspect", inspectInput, "", nil)
	cliInspect := probe.cliResult(instance.token, "execution.inspect", inspectInput, "", nil)
	requireSameBody(t, "execution.inspect", restInspect.Body, mcpInspect.Body, cliInspect)
	inspected := decodeBody[api.ExecutionInspectResult](t, restInspect.Body, "execution.inspect")
	if inspected.ExecutionID != executionID {
		t.Fatalf("execution.inspect id = %q; want %q", inspected.ExecutionID, executionID)
	}
	action := actionByName(t, inspected, "w43b-parity")
	if action.State != "succeeded" {
		t.Fatalf("parity action state = %q; want succeeded", action.State)
	}

	// The workload really ran the admitted (CLI-updated) definition, and its
	// retained log carries no credential.
	log := instance.readLogs(t, executionID, action.ActionID, "stdout")
	probe.scan("workload", "execution.logs.read", string(log))
	requireLogContains(t, "w43b-parity", log, "w43b-parity-ok\n")
	requireLogContains(t, "w43b-parity", log, "w43b-parity-cli-update\n")

	// task.create through REST, task.read through every surface.
	taskCreated := decodeBody[api.TaskCreateResult](t, probe.restResult(
		instance.token, api.MethodPost, "task.create",
		api.TaskCreateRequest{ProjectID: instance.projectID, Title: "w43b parity " + instance.runID},
		instance.key("parity-task"), nil,
	).Body, "rest task.create")
	if taskCreated.TaskID == "" {
		t.Fatal("rest task.create returned no task")
	}
	taskInput := api.TaskReadRequest{TaskID: taskCreated.TaskID}
	restTask := probe.restResult(instance.token, api.MethodGet, "task.read", taskInput, "", nil)
	mcpTask := probe.mcpResult(instance.token, "task.read", taskInput, "", nil)
	cliTask := probe.cliResult(instance.token, "task.read", taskInput, "", nil)
	requireSameBody(t, "task.read", restTask.Body, mcpTask.Body, cliTask)
	taskRead := decodeBody[api.TaskReadResult](t, restTask.Body, "task.read")
	if taskRead.TaskID != taskCreated.TaskID {
		t.Fatalf("task.read id = %q; want %q", taskRead.TaskID, taskCreated.TaskID)
	}

	// event.publish replays one committed observation across the edges, then
	// event.read with the same cursor returns the identical page.
	tail := probe.eventTail()
	observation := api.EventPublishRequest{
		ProjectID: instance.projectID,
		Type:      "observation.custom",
		SubjectID: "w43b-parity-" + instance.runID,
		Data:      []byte(fmt.Sprintf(`{"w43b":%q}`, instance.runID)),
	}
	published := decodeBody[api.EventPublishResult](t, probe.restResult(
		instance.token, api.MethodPost, "event.publish", observation, instance.key("parity-event"), nil,
	).Body, "rest event.publish")
	if published.EventID == "" || published.ReceiptID == "" {
		t.Fatalf("rest event.publish returned %+v", published)
	}
	for _, edge := range []struct {
		name   string
		result api.EventPublishResult
	}{
		{"MCP", decodeBody[api.EventPublishResult](t, probe.mcpResult(
			instance.token, "event.publish", observation, instance.key("parity-event"), nil,
		).Body, "mcp event.publish")},
		{"CLI", decodeBody[api.EventPublishResult](t, probe.cliResult(
			instance.token, "event.publish", observation, instance.key("parity-event"), nil,
		), "cli event.publish")},
	} {
		if edge.result.EventID != published.EventID || edge.result.ReceiptID != published.ReceiptID {
			t.Fatalf("%s event.publish replay returned event %q receipt %q; want %q/%q",
				edge.name, edge.result.EventID, edge.result.ReceiptID, published.EventID, published.ReceiptID)
		}
		if !edge.result.Replayed {
			t.Fatalf("%s event.publish replay did not report replayed", edge.name)
		}
	}
	eventInput := api.EventReadRequest{
		ProjectID: instance.projectID,
		Page:      &api.Page{Cursor: tail},
		Types:     []string{"observation.custom"},
	}
	restEvent := probe.restResult(instance.token, api.MethodGet, "event.read", eventInput, "", nil)
	mcpEvent := probe.mcpResult(instance.token, "event.read", eventInput, "", nil)
	cliEvent := probe.cliResult(instance.token, "event.read", eventInput, "", nil)
	requireSameBody(t, "event.read", restEvent.Body, mcpEvent.Body, cliEvent)
	page := decodeBody[api.EventReadResult](t, restEvent.Body, "event.read")
	if len(page.Events) != 1 {
		t.Fatalf("event.read after the parity cursor returned %d events; want exactly the published one", len(page.Events))
	}
	if page.Events[0].ID != published.EventID {
		t.Fatalf("event.read event id = %q; want %q", page.Events[0].ID, published.EventID)
	}
	if page.Events[0].SubjectID != observation.SubjectID {
		t.Fatalf("event.read subject = %q; want %q", page.Events[0].SubjectID, observation.SubjectID)
	}

	// An unknown pipeline is the same not_found refusal on every surface.
	missing := api.PipelineReadRequest{ProjectID: instance.projectID, PipelineID: "w43b-parity-missing"}
	notFound := requireSameFailure(t, "missing pipeline.read",
		namedFailure{"REST", probe.restFailure(instance.token, api.MethodGet, "pipeline.read", missing, "", nil)},
		namedFailure{"MCP", probe.mcpFailure(instance.token, "pipeline.read", missing, "", nil)},
		namedFailure{"CLI", probe.cliFailure(instance.token, "pipeline.read", missing, "", nil)},
	)
	if notFound.code != api.CodeNotFound {
		t.Fatalf("missing pipeline.read code = %q; want %q", notFound.code, api.CodeNotFound)
	}

	// An authenticated human without the role is refused identically, and the
	// same principal still succeeds on an operation its role carries: the
	// refusal is authorization, not a broken credential or a CLI-only policy.
	operator, ok := instance.tokens[harness.PersonaOperator]
	if !ok {
		t.Fatal("the operator persona credential is not configured")
	}
	forbiddenRead := api.PipelineReadRequest{ProjectID: instance.projectID, PipelineID: pipelineID}
	forbidden := requireSameFailure(t, "operator pipeline.read",
		namedFailure{"REST", probe.restFailure(operator, api.MethodGet, "pipeline.read", forbiddenRead, "", nil)},
		namedFailure{"MCP", probe.mcpFailure(operator, "pipeline.read", forbiddenRead, "", nil)},
		namedFailure{"CLI", probe.cliFailure(operator, "pipeline.read", forbiddenRead, "", nil)},
	)
	if forbidden.code != api.CodeForbidden {
		t.Fatalf("operator pipeline.read code = %q; want %q", forbidden.code, api.CodeForbidden)
	}
	// The operator's role carries execution.list. One bounded, pipeline-scoped
	// page is served byte-identically by every edge, and it must contain the
	// execution under test: an empty page, another project's rows or a
	// differently paged answer would otherwise be indistinguishable from parity.
	allowedRead := api.ExecutionListRequest{
		ProjectID:  instance.projectID,
		PipelineID: pipelineID,
		Page:       &api.Page{Limit: api.MaxPageLimit},
	}
	restList := probe.restResult(operator, api.MethodGet, "execution.list", allowedRead, "", nil)
	mcpList := probe.mcpResult(operator, "execution.list", allowedRead, "", nil)
	cliList := probe.cliResult(operator, "execution.list", allowedRead, "", nil)
	requireSameBody(t, "operator execution.list", restList.Body, mcpList.Body, cliList)
	listed := decodeBody[api.ExecutionListResult](t, restList.Body, "operator execution.list")
	if len(listed.Executions) > api.MaxPageLimit {
		t.Fatalf("operator execution.list returned %d executions above the %d-entry page bound", len(listed.Executions), api.MaxPageLimit)
	}
	summary, summaryBytes := executionEntry(t, restList.Body, executionID, "operator execution.list")
	if summary.PipelineID != pipelineID {
		t.Fatalf("operator execution.list pipeline = %q; want %q", summary.PipelineID, pipelineID)
	}
	if summary.State != "succeeded" {
		t.Fatalf("operator execution.list state = %q; want succeeded", summary.State)
	}
	if summary.Version == 0 {
		t.Fatal("operator execution.list returned a zero execution version")
	}
	// The identical body above already fixes the continuation cursor; decode it
	// too so a page/cursor divergence is named rather than implicit.
	if listed.NextCursor != "" {
		t.Fatalf("operator execution.list returned continuation %q for the single-execution pipeline page", listed.NextCursor)
	}
	for _, edge := range []struct {
		name string
		body []byte
	}{
		{"MCP", mcpList.Body},
		{"CLI", cliList},
	} {
		_, edgeSummary := executionEntry(t, edge.body, executionID, "operator execution.list via "+edge.name)
		if !bytes.Equal(summaryBytes, edgeSummary) {
			t.Fatalf("operator execution.list via %s returned different bytes for execution %q", edge.name, executionID)
		}
	}

	// An unusable credential is the same unauthenticated refusal everywhere.
	bogus := harness.Secret("w43b-not-a-credential")
	unauthenticated := requireSameFailure(t, "unauthenticated execution.list",
		namedFailure{"REST", probe.restFailure(bogus, api.MethodGet, "execution.list", allowedRead, "", nil)},
		namedFailure{"MCP", probe.mcpFailure(bogus, "execution.list", allowedRead, "", nil)},
		namedFailure{"CLI", probe.cliFailure(bogus, "execution.list", allowedRead, "", nil)},
	)
	if unauthenticated.code != api.CodeUnauthenticated {
		t.Fatalf("unauthenticated execution.list code = %q; want %q", unauthenticated.code, api.CodeUnauthenticated)
	}

	finished := time.Now().UTC()
	instance.recordScenario(t, "SurfaceParity", "TestE2E_Parity", &inspected, started, finished, map[string]any{
		"task_id":  taskCreated.TaskID,
		"event_id": published.EventID,
	})
	t.Logf("parity matrix: %d operations x CLI/REST/MCP (pipeline %s, %d catalog tools, %d credential scans); execution %s",
		len(parityOperations), pipelineID, len(tools), probe.scanned, executionID)
}

// parityProbe drives the three edges of one installation.
type parityProbe struct {
	t            *testing.T
	instance     *live
	ctx          context.Context
	mcp          *harness.MCP
	cliTransport http.RoundTripper
	secrets      []harness.Secret
	scanned      int
}

// newParityProbe builds the MCP and CLI transports over the installation's one
// verified TLS policy.
func newParityProbe(t *testing.T, instance *live) *parityProbe {
	t.Helper()
	mcp, err := harness.NewMCP(instance.config)
	if err != nil {
		t.Fatalf("build the MCP edge helper: %v", err)
	}
	transport, err := harness.NewVerifiedTransport(instance.config)
	if err != nil {
		t.Fatalf("build the CLI edge transport: %v", err)
	}
	secrets := append(instance.config.PersonaSecrets(), instance.token)
	return &parityProbe{
		t:            t,
		instance:     instance,
		ctx:          t.Context(),
		mcp:          mcp,
		cliTransport: transport,
		secrets:      secrets,
	}
}

// requireToolCatalog asserts the MCP edge advertises every parity operation
// under its declared name with the derived input schema.
func (probe *parityProbe) requireToolCatalog() []api.MCPTool {
	probe.t.Helper()
	tools, wire, err := probe.mcp.Tools(probe.ctx)
	for _, exchange := range wire {
		probe.scan("MCP", "initialize/tools-list", string(exchange))
	}
	if err != nil {
		probe.t.Fatalf("MCP initialize/tools-list: %v", err)
	}
	byName := make(map[string]api.MCPTool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	for _, operation := range parityOperations {
		route, ok := api.LookupRoute(operation)
		if !ok {
			probe.t.Fatalf("parity operation %q has no declared route", operation)
		}
		tool, ok := byName[route.MCPTool]
		if !ok {
			probe.t.Fatalf("the MCP tools/list catalog does not advertise %q", route.MCPTool)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			probe.t.Fatalf("MCP tool %q inputSchema is not a JSON object: %v", route.MCPTool, err)
		}
	}
	return tools
}

// scan asserts one captured edge output carries no credential material.
func (probe *parityProbe) scan(edge, label, text string) {
	probe.t.Helper()
	if err := harness.ScanCredentials(text, probe.secrets...); err != nil {
		probe.t.Fatalf("%s %s leaked credential material: %v", edge, label, err)
	}
	probe.scanned++
}

// restResult performs one REST operation that must succeed.
func (probe *parityProbe) restResult(token harness.Secret, method api.Method, operation string, input any, key string, version *uint64) api.Result {
	probe.t.Helper()
	result, err := probe.instance.api.Invoke(probe.ctx, harness.Call{
		Method:          method,
		Token:           token,
		Operation:       operation,
		Input:           input,
		IdempotencyKey:  key,
		ExpectedVersion: version,
	})
	if err != nil {
		probe.scan("REST", operation+" refusal", err.Error())
		probe.t.Fatalf("REST %s was refused: %v", operation, err)
	}
	probe.scan("REST", operation, string(result.Body))
	if result.Operation != operation {
		probe.t.Fatalf("REST %s named operation %q", operation, result.Operation)
	}
	return result
}

// restFailure performs one REST operation that must be refused.
func (probe *parityProbe) restFailure(token harness.Secret, method api.Method, operation string, input any, key string, version *uint64) publicFailure {
	probe.t.Helper()
	_, err := probe.instance.api.Invoke(probe.ctx, harness.Call{
		Method:          method,
		Token:           token,
		Operation:       operation,
		Input:           input,
		IdempotencyKey:  key,
		ExpectedVersion: version,
	})
	var refusal *harness.APIError
	if !errors.As(err, &refusal) {
		probe.t.Fatalf("REST %s = %v; want a public refusal", operation, err)
	}
	probe.scan("REST", operation+" refusal", string(refusal.Raw)+refusal.Message)
	return publicFailure{code: refusal.Code, message: refusal.Message}
}

// mcpResult performs one MCP tools/call that must succeed.
func (probe *parityProbe) mcpResult(token harness.Secret, operation string, input any, key string, version *uint64) api.Result {
	probe.t.Helper()
	result, raw, err := probe.mcp.Call(probe.ctx, harness.MCPCall{
		Token:           token,
		Operation:       operation,
		Input:           input,
		IdempotencyKey:  key,
		ExpectedVersion: version,
	})
	probe.scan("MCP", operation, string(raw))
	if err != nil {
		probe.t.Fatalf("MCP %s was refused: %v", operation, err)
	}
	return result
}

// mcpFailure performs one MCP tools/call that must be refused.
func (probe *parityProbe) mcpFailure(token harness.Secret, operation string, input any, key string, version *uint64) publicFailure {
	probe.t.Helper()
	_, raw, err := probe.mcp.Call(probe.ctx, harness.MCPCall{
		Token:           token,
		Operation:       operation,
		Input:           input,
		IdempotencyKey:  key,
		ExpectedVersion: version,
	})
	probe.scan("MCP", operation+" refusal", string(raw))
	var refusal *harness.MCPRefusal
	if !errors.As(err, &refusal) {
		probe.t.Fatalf("MCP %s = %v; want a public refusal", operation, err)
	}
	return publicFailure{code: refusal.Public.Code, message: refusal.Public.Message}
}

// cliResult drives one CLI command that must succeed and returns the rendered
// result body. JSON output is the exact result body bytes plus one newline.
func (probe *parityProbe) cliResult(token harness.Secret, operation string, input any, key string, version *uint64) []byte {
	probe.t.Helper()
	outcome := probe.cliInvoke(token, operation, input, key, version)
	probe.scan("CLI", operation+" stdout", string(outcome.stdout))
	probe.scan("CLI", operation+" stderr", string(outcome.stderr))
	if outcome.code != cli.ExitSuccess {
		probe.t.Fatalf("CLI %s exit = %d; stderr=%s", operation, outcome.code, strings.TrimSpace(string(outcome.stderr)))
	}
	body := bytes.TrimSuffix(outcome.stdout, []byte("\n"))
	if len(body) == 0 {
		probe.t.Fatalf("CLI %s wrote no result body", operation)
	}
	return body
}

// cliFailure drives one CLI command that must be refused and parses the typed
// error the CLI renders.
func (probe *parityProbe) cliFailure(token harness.Secret, operation string, input any, key string, version *uint64) publicFailure {
	probe.t.Helper()
	outcome := probe.cliInvoke(token, operation, input, key, version)
	probe.scan("CLI", operation+" stdout", string(outcome.stdout))
	probe.scan("CLI", operation+" stderr", string(outcome.stderr))
	if outcome.code != cli.ExitFailure {
		probe.t.Fatalf("CLI %s exit = %d; want %d; stderr=%s", operation, outcome.code, cli.ExitFailure, strings.TrimSpace(string(outcome.stderr)))
	}
	text := strings.TrimSpace(string(outcome.stderr))
	text = strings.TrimPrefix(text, "error: ")
	code, message, ok := strings.Cut(text, ": ")
	if !ok || code == "" {
		probe.t.Fatalf("CLI %s stderr %q carries no typed public error", operation, strings.TrimSpace(string(outcome.stderr)))
	}
	return publicFailure{code: code, message: message}
}

// cliInvoke runs one real cli.Run command over the real client transport.
func (probe *parityProbe) cliInvoke(token harness.Secret, operation string, input any, key string, version *uint64) cliOutcome {
	probe.t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		probe.t.Fatalf("encode the CLI %s input: %v", operation, err)
	}
	args := []string{"call", operation, "--input=" + string(encoded)}
	if version != nil {
		args = append(args, fmt.Sprintf("--expected-version=%d", *version))
	}
	if key != "" {
		args = append(args, "--idempotency-key="+key)
	}
	transport := &client.HTTPTransport{
		BaseURL:      probe.instance.config.APIURL,
		RoundTripper: probe.cliTransport,
		Source:       staticCredentialSource{token: token, expiry: time.Now().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second},
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(probe.ctx, cli.Config{
		ServerURL: probe.instance.config.APIURL,
		Output:    cli.OutputJSON,
	}, args, cli.Ports{
		Client: transport,
		Out:    &stdout,
		Err:    &stderr,
	})
	return cliOutcome{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: code}
}

// eventTail walks the committed event stream to its end and returns the cursor
// of the last committed sequence, so a later read is exactly the events this
// probe publishes.
func (probe *parityProbe) eventTail() string {
	probe.t.Helper()
	cursor := ""
	for range 128 {
		result := probe.restResult(probe.instance.token, api.MethodGet, "event.read", api.EventReadRequest{
			ProjectID: probe.instance.projectID,
			Page:      &api.Page{Limit: api.MaxPageLimit, Cursor: cursor},
		}, "", nil)
		decoded := decodeBody[api.EventReadResult](probe.t, result.Body, "event.read tail")
		if decoded.Next == "" {
			probe.t.Fatal("event.read returned an empty continuation cursor")
		}
		if !decoded.HasMore {
			return decoded.Next
		}
		cursor = decoded.Next
	}
	probe.t.Fatal("event.read did not reach the end of the committed stream within 128 pages")
	return ""
}

// staticCredentialSource presents one already-acquired installation credential
// through the client port. The CLI login/session flow is out of scope
// for a parity probe.
type staticCredentialSource struct {
	token  harness.Secret
	expiry time.Time
}

var _ client.CredentialSource = staticCredentialSource{}

// Credential returns the current installation credential.
func (source staticCredentialSource) Credential(ctx context.Context) (client.Credential, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return client.Credential{}, err
		}
	}
	if !source.token.IsSet() {
		return client.Credential{}, errors.New("no credential is configured")
	}
	return client.Credential{Kind: client.KindSession, Token: source.token.Reveal(), ExpiresAt: source.expiry}, nil
}

// cliOutcome is one completed CLI command.
type cliOutcome struct {
	stdout []byte
	stderr []byte
	code   int
}

// publicFailure is one typed public refusal independent of the edge.
type publicFailure struct {
	code    string
	message string
}

// namedFailure labels one edge's refusal.
type namedFailure struct {
	edge    string
	failure publicFailure
}

// requireSameFailure asserts every edge produced the same public code and
// message, and returns it.
func requireSameFailure(t *testing.T, label string, failures ...namedFailure) publicFailure {
	t.Helper()
	if len(failures) == 0 {
		t.Fatalf("%s: no edge outcome", label)
	}
	want := failures[0]
	for _, got := range failures[1:] {
		if got.failure.code != want.failure.code || got.failure.message != want.failure.message {
			t.Fatalf("%s: %s = %q/%q; want the %s refusal %q/%q",
				label, got.edge, got.failure.code, got.failure.message,
				want.edge, want.failure.code, want.failure.message)
		}
	}
	return want.failure
}

// requireSameBody asserts every edge returned exactly the same result body.
func requireSameBody(t *testing.T, label string, bodies ...[]byte) {
	t.Helper()
	if len(bodies) == 0 {
		t.Fatalf("%s: no edge body", label)
	}
	for index, body := range bodies[1:] {
		if !bytes.Equal(bodies[0], body) {
			t.Fatalf("%s: edge %d returned different bytes\nfirst=%s\nthis =%s",
				label, index+1, compactJSON(bodies[0]), compactJSON(body))
		}
	}
}

// compactJSON renders bounded diagnostic JSON without ever echoing credential
// material (result bodies carry state, not credentials).
func compactJSON(body []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return fmt.Sprintf("<%d bytes>", len(body))
	}
	if compact.Len() > 2048 {
		return compact.String()[:2048] + "…"
	}
	return compact.String()
}

// decodeBody decodes one typed result body.
func decodeBody[T any](t *testing.T, body []byte, label string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("%s body is not %T: %v", label, value, err)
	}
	return value
}

// executionEntry returns the summary for one execution from an execution.list
// body together with its exact JSON bytes, so a probe can prove the entry
// itself, not only the surrounding page, is identical across edges.
func executionEntry(t *testing.T, body []byte, executionID, label string) (api.ExecutionSummary, json.RawMessage) {
	t.Helper()
	var page struct {
		Executions []json.RawMessage `json:"executions"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("%s body is not an execution list: %v", label, err)
	}
	var found json.RawMessage
	count := 0
	for _, raw := range page.Executions {
		var candidate api.ExecutionSummary
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("%s carries an invalid execution summary: %v", label, err)
		}
		if candidate.ExecutionID == executionID {
			found = append(json.RawMessage(nil), raw...)
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s listed execution %q %d times; want exactly once", label, executionID, count)
	}
	var summary api.ExecutionSummary
	if err := json.Unmarshal(found, &summary); err != nil {
		t.Fatalf("%s execution %q is not a valid summary: %v", label, executionID, err)
	}
	return summary, found
}
