//go:build integration

package services

// Delivery recipe: a plain task-free container pipeline runs on the real
// factory with the optional services disabled.
//
// The composition used here has no Git service, model gateway, external MCP
// proxy, agent runtime or service principal, and the pipeline declares no
// repository, service or on.push binding and no request action. It still runs a
// real workload to a real success and records its output through the served
// execution record, which is the "plain pipelines without gateways/agents"
// requirement. The optional capability recipes have their own tests, so this
// test deliberately does not pretend to exercise them.

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestDeliveryPlainPipelineWithoutOptionalServices proves a plain pipeline
// succeeds end to end with no service binding, request action or optional
// capability engaged.
func TestDeliveryPlainPipelineWithoutOptionalServices(t *testing.T) {
	harness := factory.Start(t)

	applied := deliveryApply(t, harness, "delivery-plain", deliveryPipelineDoc{
		Env: map[string]string{"LOG_LEVEL": "info"},
		Actions: []deliveryActionDoc{{
			Name:    "inspect",
			Image:   harness.Image.Reference,
			Timeout: deliveryActionTimeout,
			Outputs: []string{"done"},
			Commands: []string{
				`test ! -e /kodama/task.json`,
				`test -z "${KODAMA_TASK:-}"`,
				`test "$LOG_LEVEL" = info`,
				`printf 'plain\n' > /workspace/marker`,
				`DONE="plain:$(cat /workspace/marker)"`,
				deliverySubmitScript("done", "DONE"),
			},
		}},
	})
	started := deliveryStart(t, harness, applied.PipelineID, nil, "start-delivery-plain")
	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)

	if len(execution.Actions) != 1 {
		t.Fatalf("plain execution has %d actions; want the single container action", len(execution.Actions))
	}
	action := actionByName(t, execution, "inspect")
	if action.Kind != "container" {
		t.Fatalf("plain action kind = %q; want container", action.Kind)
	}
	if action.State != "succeeded" {
		t.Fatalf("plain action state = %q; want succeeded", action.State)
	}
	if got := action.Outputs["done"]; got != "plain:plain" {
		t.Fatalf("plain output = %q; want plain:plain", got)
	}

	// The plain path creates no request or service work: no request-resolved or
	// service-changed events exist for this execution.
	for _, types := range [][]string{{"request.resolved"}, {"service.changed"}} {
		events := readProjectEvents(t, harness, types...)
		for _, event := range events.Events {
			if event.SubjectID == started.ExecutionID {
				t.Fatalf("plain execution published unexpected %q event %+v", event.Type, event)
			}
		}
	}

	// Positive control: the optional capability operations are addressable but
	// fail closed on this composition. They must answer not_found rather than
	// succeed, hang or mask the disabled capability.
	refusals := []struct {
		operation string
		input     any
	}{
		{
			operation: "model.invoke",
			input: api.ModelInvokeRequest{
				ProjectID: factory.ProjectID,
				Model:     "absent-model",
				Messages:  api.Messages{{Role: "user", Content: "hello"}},
			},
		},
		{
			operation: "tool.invoke",
			input: api.ToolInvokeRequest{
				ProjectID: factory.ProjectID,
				Tool:      "absent-tool",
				Arguments: json.RawMessage(`{}`),
			},
		},
	}
	for _, refusal := range refusals {
		_, publicError := harness.REST(harness.ProjectToken, refusal.operation, refusal.input, nil, "")
		if publicError.Code != api.CodeNotFound {
			t.Fatalf("%s refusal = %q (%s); want %q because the optional capability is disabled",
				refusal.operation, publicError.Code, publicError.Message, api.CodeNotFound)
		}
	}
}
