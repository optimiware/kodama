package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestRunCallForwardsInlinePipelineStart pins that the CLI's generic call
// surface already carries an inline pipeline.start selection unchanged: it
// neither injects a pipeline id nor imposes the 1 MiB default envelope. No
// dedicated inline-start command exists.
func TestRunCallForwardsInlinePipelineStart(t *testing.T) {
	document := bytes.Repeat([]byte("a"), api.MaxInlineYAMLBytes)
	document[0] = 'p'
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID: "shop",
		YAML:      document,
		Inputs:    map[string]string{"branch": "main"},
	})
	if err != nil {
		t.Fatalf("marshal inline pipeline.start input: %v", err)
	}
	if len(input) <= api.MaxRequestBytes {
		t.Fatalf("test input = %d bytes; want above the 1 MiB default", len(input))
	}

	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{
			Operation: invocation.Operation,
			Body:      json.RawMessage(`{"execution_id":"exec-1","version":2}`),
		}, nil
	}}
	ports, out, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{
		"call", "pipeline.start",
		"--input=" + string(input),
		"--idempotency-key=inline-start-key-1",
	}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if !strings.Contains(out.String(), `"execution_id":"exec-1"`) {
		t.Fatalf("stdout = %q; want the admitted execution result", out.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 1 {
		t.Fatalf("invocations = %d; want 1", len(recorded))
	}
	if recorded[0].Operation != "pipeline.start" {
		t.Fatalf("operation = %q", recorded[0].Operation)
	}
	if string(recorded[0].Request.Input) != string(input) {
		t.Fatal("CLI rewrote the inline pipeline.start input")
	}
	if recorded[0].Request.ExpectedVersion != nil {
		t.Fatalf("expected version = %v; want none", recorded[0].Request.ExpectedVersion)
	}
	if recorded[0].Request.IdempotencyKey != "inline-start-key-1" {
		t.Fatalf("idempotency key = %q", recorded[0].Request.IdempotencyKey)
	}
}

// TestRunDetachForwardsInlinePipelineStart pins that `kodema run --detach`
// forwards an inline pipeline YAML directly to pipeline.start, prints the
// execution id, and returns 0 without polling execution.inspect.
func TestRunDetachForwardsInlinePipelineStart(t *testing.T) {
	const path = "/repo/service-request.yaml"
	document := []byte("actions:\n  - name: qa\n    request: service/uc09-qa-bot\n")
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{
			Operation: invocation.Operation,
			Body:      json.RawMessage(`{"execution_id":"exec-inline-42","version":1}`),
		}, nil
	}}
	ports, out, errOut := testPorts(transport)
	ports.Files.(*fakeFS).files[path] = document
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"run", "--detach", path, "--input", "env=staging"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if out.String() != "exec-inline-42\n" {
		t.Fatalf("stdout = %q; want execution id line", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 1 || recorded[0].Operation != "pipeline.start" {
		t.Fatalf("recorded = %+v; want single pipeline.start", recorded)
	}
	var req api.PipelineStartRequest
	if err := json.Unmarshal(recorded[0].Request.Input, &req); err != nil {
		t.Fatalf("unmarshal pipeline.start input: %v", err)
	}
	if req.ProjectID != "shop" || req.PipelineID != "" || string(req.YAML) != string(document) || req.Inputs["env"] != "staging" {
		t.Fatalf("pipeline.start request = %+v", req)
	}
	if recorded[0].Request.IdempotencyKey == "" {
		t.Fatal("missing auto-generated idempotency key")
	}
}
