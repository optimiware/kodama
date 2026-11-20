package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
)

type modelUsageStubTransport struct {
	result api.Result
}

func (stub modelUsageStubTransport) Invoke(context.Context, api.Invocation) (api.Result, error) {
	return stub.result, nil
}

func TestInspectRendersModelUsageInTableAndJSON(t *testing.T) {
	withUsage := api.ExecutionInspectResult{
		ExecutionID:   "exec-usage-1",
		State:         "succeeded",
		Version:       4,
		CurrentAction: -1,
		Actions: []api.ExecutionAction{{
			ActionID: "act-1",
			Index:    0,
			Name:     "code",
			Kind:     "container",
			State:    "succeeded",
			Version:  2,
		}},
		ModelUsage: &api.ExecutionModelUsage{
			Requests:     3,
			InputTokens:  1200,
			OutputTokens: 350,
			TotalTokens:  1550,
			ByModel: []api.ExecutionModelUsageBreakdown{
				{
					Provider:     "anthropic",
					Model:        "claude-sonnet-4-6",
					Requests:     2,
					InputTokens:  900,
					OutputTokens: 250,
					TotalTokens:  1150,
				},
				{
					Provider:     "opencode",
					Model:        "zen-coder-1",
					Requests:     1,
					InputTokens:  300,
					OutputTokens: 100,
					TotalTokens:  400,
				},
			},
		},
		Cleanup: api.ExecutionCleanup{State: "complete"},
	}
	rawWithUsage, err := json.Marshal(withUsage)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("table output renders model_usage summary and breakdown", func(t *testing.T) {
		var out, errOut bytes.Buffer
		transport := modelUsageStubTransport{result: api.Result{Operation: "execution.inspect", Body: rawWithUsage}}
		code := Run(context.Background(),
			Config{ServerURL: "https://kodema.example", Output: OutputTable},
			[]string{"inspect", "exec-usage-1"},
			Ports{Client: transport, Out: &out, Err: &errOut})
		if code != ExitSuccess {
			t.Fatalf("inspect exit=%d err=%s", code, errOut.String())
		}
		rendered := out.String()
		for _, want := range []string{
			"model_usage",
			"requests=3 input_tokens=1200 output_tokens=350 total_tokens=1550",
			"PROVIDER",
			"MODEL",
			"anthropic",
			"claude-sonnet-4-6",
			"opencode",
			"zen-coder-1",
		} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("table output missing %q:\n%s", want, rendered)
			}
		}
	})

	t.Run("json output preserves model_usage object", func(t *testing.T) {
		var out, errOut bytes.Buffer
		transport := modelUsageStubTransport{result: api.Result{Operation: "execution.inspect", Body: rawWithUsage}}
		code := Run(context.Background(),
			Config{ServerURL: "https://kodema.example", Output: OutputJSON},
			[]string{"inspect", "exec-usage-1"},
			Ports{Client: transport, Out: &out, Err: &errOut})
		if code != ExitSuccess {
			t.Fatalf("inspect --output json exit=%d err=%s", code, errOut.String())
		}
		var decoded api.ExecutionInspectResult
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("unmarshal inspect JSON: %v\n%s", err, out.String())
		}
		if decoded.ModelUsage == nil || decoded.ModelUsage.TotalTokens != 1550 || len(decoded.ModelUsage.ByModel) != 2 {
			t.Fatalf("decoded model_usage = %+v", decoded.ModelUsage)
		}
	})

	t.Run("table output omits model_usage when execution made no model calls", func(t *testing.T) {
		noUsage := withUsage
		noUsage.ModelUsage = nil
		rawNoUsage, err := json.Marshal(noUsage)
		if err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		transport := modelUsageStubTransport{result: api.Result{Operation: "execution.inspect", Body: rawNoUsage}}
		code := Run(context.Background(),
			Config{ServerURL: "https://kodema.example", Output: OutputTable},
			[]string{"inspect", "exec-usage-1"},
			Ports{Client: transport, Out: &out, Err: &errOut})
		if code != ExitSuccess {
			t.Fatalf("inspect exit=%d err=%s", code, errOut.String())
		}
		if strings.Contains(out.String(), "model_usage") || strings.Contains(out.String(), "INPUT_TOKENS") {
			t.Fatalf("table output unexpectedly rendered model_usage when absent:\n%s", out.String())
		}
	})
}
