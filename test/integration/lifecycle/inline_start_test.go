//go:build integration

package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// inlineRESTStart posts one inline-YAML pipeline.start to the task-free
// alternate REST binding (/api/v1/projects/{project}/executions). The shared
// factory edge helper only expands canonical routes, so this file carries its
// own raw request path and leaves factory routing untouched.
func inlineRESTStart(t *testing.T, harness *factory.Harness, token string, document []byte, inputs map[string]string, key string) api.PipelineStartResult {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{YAML: document, Inputs: inputs})
	if err != nil {
		t.Fatalf("marshal inline start input: %v", err)
	}
	envelope, err := json.Marshal(api.Request{Input: input, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("marshal inline start envelope: %v", err)
	}
	path := "/api/v1/projects/" + factory.ProjectID + "/executions"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.BaseURL+path, bytes.NewReader(envelope))
	if err != nil {
		t.Fatalf("build inline start request: %v", err)
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("inline start request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read inline start response: %v", err)
	}
	if response.StatusCode != api.HTTPStatusSuccess {
		var publicError api.Error
		if err := json.Unmarshal(data, &publicError); err != nil {
			t.Fatalf("inline start error body is not an api.Error: %v; body=%s", err, string(data))
		}
		t.Fatalf("inline start refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var result api.Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode inline start result: %v; body=%s", err, string(data))
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("inline start result: %v", err)
	}
	var started api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil {
		t.Fatalf("decode inline start body: %v; body=%s", err, string(result.Body))
	}
	if err := started.Validate(); err != nil {
		t.Fatalf("inline start body: %v", err)
	}
	return started
}

// inlineScopedCount runs one count query through the runtime role inside the
// fixture project's transaction-local scope.
func inlineScopedCount(t *testing.T, harness *factory.Harness, query string, args ...any) int64 {
	t.Helper()
	rows, err := pgtest.ScopedQuery(t.Context(), harness.Postgres.Runtime(1), harness.Scope, 1, query, args...)
	if err != nil {
		t.Fatalf("scoped count: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("scoped count returned %d rows; want 1", len(rows))
	}
	var value int64
	value, err = strconv.ParseInt(rows[0][0], 10, 64)
	if err != nil {
		t.Fatalf("scan scoped count: %v", err)
	}
	return value
}

// TestInlineTaskFreeStart proves a real-stack inline-YAML start on the
// task-free executions route persists the exact document and hash, creates no
// pipeline or task row, reports the execution version and replays to the same
// execution over MCP.
func TestInlineTaskFreeStart(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "inline",
			Image:    harness.Image.Reference,
			Commands: []string{"true"},
		}},
	})

	first := inlineRESTStart(t, harness, harness.ProjectToken, document, nil, "start-inline")
	if first.ExecutionID == "" {
		t.Fatal("inline start returned no execution id")
	}
	if first.Version != 1 {
		t.Fatalf("inline start version = %d; want the execution version 1", first.Version)
	}

	admittedYAML, admittedSHA, err := harness.ExecutionRow(ctx, first.ExecutionID)
	if err != nil {
		t.Fatalf("read admitted execution row: %v", err)
	}
	if !bytes.Equal(admittedYAML, document) {
		t.Fatalf("admitted yaml differs from the posted bytes:\nposted=%s\nstored=%s", document, admittedYAML)
	}
	wantSHA := sha256.Sum256(document)
	if !bytes.Equal(admittedSHA, wantSHA[:]) {
		t.Fatalf("admitted yaml_sha256 = %x; want %x", admittedSHA, wantSHA[:])
	}

	if count := inlineScopedCount(t, harness, "SELECT count(*) FROM pipelines"); count != 0 {
		t.Fatalf("inline start created %d pipeline rows; want none", count)
	}
	if count := inlineScopedCount(t, harness, "SELECT count(*) FROM executions WHERE id = $1 AND pipeline_id IS NULL AND task_id IS NULL", first.ExecutionID); count != 1 {
		t.Fatalf("inline execution row pipeline/task null count = %d; want 1", count)
	}
	tasks, err := harness.TaskCount(ctx)
	if err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("inline start created %d task rows; want none", tasks)
	}

	// A task-free inline execution has no pipeline or task identity, so the
	// inspection projection omits pipeline_id instead of fabricating one, while
	// execution.list reports the same empty identity.
	inspected := harness.MustREST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: first.ExecutionID,
	}, nil, "")
	var view api.ExecutionInspectResult
	harness.Decode(inspected, &view)
	if view.ExecutionID != first.ExecutionID {
		t.Fatalf("inline inspect execution = %q; want %q", view.ExecutionID, first.ExecutionID)
	}
	if view.PipelineID != "" || view.TaskID != "" {
		t.Fatalf("inline inspect identity = pipeline %q task %q; want both empty", view.PipelineID, view.TaskID)
	}
	if bytes.Contains(inspected.Body, []byte(`"pipeline_id"`)) {
		t.Fatalf("inline execution.inspect carries a fabricated pipeline_id: %s", inspected.Body)
	}
	listed := harness.MustREST(harness.ProjectToken, "execution.list", api.ExecutionListRequest{
		ProjectID: factory.ProjectID,
	}, nil, "")
	var list api.ExecutionListResult
	harness.Decode(listed, &list)
	if len(list.Executions) != 1 || list.Executions[0].ExecutionID != first.ExecutionID {
		t.Fatalf("inline execution.list = %+v; want only %q", list.Executions, first.ExecutionID)
	}
	if list.Executions[0].PipelineID != "" {
		t.Fatalf("inline execution.list pipeline_id = %q; want the empty optional identity", list.Executions[0].PipelineID)
	}

	replayed := harness.MustMCP(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID: factory.ProjectID,
		YAML:      document,
	}, nil, "start-inline")
	var replayedResult api.PipelineStartResult
	harness.Decode(replayed, &replayedResult)
	if replayedResult.ExecutionID != first.ExecutionID {
		t.Fatalf("MCP replay execution = %q; want %q", replayedResult.ExecutionID, first.ExecutionID)
	}
	if replayedResult.Version != first.Version {
		t.Fatalf("MCP replay version = %d; want %d", replayedResult.Version, first.Version)
	}

	receipts, err := harness.ReceiptCount(ctx, "start-inline")
	if err != nil {
		t.Fatalf("count inline start receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("inline start receipt count = %d; want 1", receipts)
	}
}
