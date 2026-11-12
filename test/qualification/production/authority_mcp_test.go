//go:build qualification

package production

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestStageAuthorityMCPReads is the live MCP authority probe: the admin token
// (the only onboarded project member) drives the same representative read set
// and the same inline execution start through the MCP tools/call JSON-RPC
// surface, with the frozen envelope and the
// idempotency-key rule, while no token and a garbage token are refused as
// unauthenticated.
func TestStageAuthorityMCPReads(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config,
		InputAPIURL, InputCAPEM, InputTLSServerName,
		InputAdminToken, InputProjectID, InputImage, InputEvidenceDir,
	)
	client := newStageClient(t, config)
	ctx := context.Background()
	started := time.Now().UTC()

	listResult, err := mcpCall(ctx, client, config.AdminToken, "q02-list", "execution.list",
		api.Request{Input: mustMarshal(t, api.ExecutionListRequest{ProjectID: config.ProjectID})})
	if err != nil {
		t.Fatalf("the authenticated MCP execution.list was refused: %v", err)
	}
	if _, err := decodeStageResult[api.ExecutionListResult](listResult, "execution.list"); err != nil {
		t.Fatalf("the authenticated MCP execution.list returned an invalid result: %v", err)
	}

	document, err := singleActionPipeline(config.Image)
	if err != nil {
		t.Fatalf("build the inline probe document: %v", err)
	}
	startKey, err := probeIdempotencyKey("q02-mcp-start")
	if err != nil {
		t.Fatalf("generate the inline start key: %v", err)
	}
	executionID, err := mcpStartInlineExecution(ctx, client, config.AdminToken, config.ProjectID, document, startKey)
	if err != nil {
		t.Fatalf("the MCP inline pipeline.start was refused: %v", err)
	}

	inspectResult, err := mcpCall(ctx, client, config.AdminToken, "q02-inspect", "execution.inspect",
		api.Request{Input: mustMarshal(t, api.ExecutionInspectRequest{ExecutionID: executionID})})
	if err != nil {
		t.Fatalf("the authenticated MCP execution.inspect was refused: %v", err)
	}
	inspect, err := decodeStageResult[api.ExecutionInspectResult](inspectResult, "execution.inspect")
	if err != nil {
		t.Fatalf("the authenticated MCP execution.inspect returned an invalid result: %v", err)
	}
	if inspect.ExecutionID != executionID {
		t.Fatalf("MCP execution.inspect returned execution %q, want the freshly started %q", inspect.ExecutionID, executionID)
	}

	inboxResult, err := mcpCall(ctx, client, config.AdminToken, "q02-inbox", "task.inbox",
		api.Request{Input: mustMarshal(t, api.TaskInboxRequest{ProjectID: config.ProjectID})})
	if err != nil {
		t.Fatalf("the authenticated MCP task.inbox was refused: %v", err)
	}
	if _, err := decodeStageResult[api.TaskInboxResult](inboxResult, "task.inbox"); err != nil {
		t.Fatalf("the authenticated MCP task.inbox returned an invalid result: %v", err)
	}

	reads := []struct {
		name      string
		operation string
		input     any
	}{
		{"execution.list", "execution.list", api.ExecutionListRequest{ProjectID: config.ProjectID}},
		{"execution.inspect", "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}},
		{"task.inbox", "task.inbox", api.TaskInboxRequest{ProjectID: config.ProjectID}},
	}
	for _, read := range reads {
		for _, token := range []Secret{"", "garbage-token"} {
			_, err := mcpCall(ctx, client, token, "q02-refused", read.operation,
				api.Request{Input: mustMarshal(t, read.input)})
			expectPublicRefusal(t, err, 401, api.CodeUnauthenticated)
		}
	}

	finished := time.Now().UTC()
	writeStageEvidence(t, config, stageEvidence{
		Name:        "stage-authority-mcp",
		Probe:       "TestStageAuthorityMCPReads",
		Started:     started,
		Finished:    finished,
		ExecutionID: executionID,
	})
}

// mustMarshal encodes one probe request input.
func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := encodeInput(value)
	if err != nil {
		t.Fatalf("encode a probe request input: %v", err)
	}
	return encoded
}
