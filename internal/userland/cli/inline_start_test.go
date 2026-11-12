package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
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
