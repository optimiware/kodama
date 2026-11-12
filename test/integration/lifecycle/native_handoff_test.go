//go:build integration

package lifecycle

// Real-stack coverage for the native-agent handoff. The unit owner regression
// proves the payload matches the public codec; this file proves the same
// payload reaches the served task API and leaves the task waiting with a
// durable checkpoint reference. The model is a deterministic owner-regression
// fixture because the default public-model path cannot generate tool calls; the
// claim is the served task wire, not live provider behavior.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/agent/app"
	"github.com/optimiware/kodama/test/support/factory"
)

// nativeHandoffModel is the deterministic ModelPort fixture for the handoff.
type nativeHandoffModel struct {
	calls      int
	question   string
	assignment string
}

func (model *nativeHandoffModel) Invoke(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	model.calls++
	arguments, err := json.Marshal(map[string]string{
		"question":   model.question,
		"assignment": model.assignment,
	})
	if err != nil {
		return agent.ModelResponse{}, err
	}
	return agent.ModelResponse{
		ToolCalls:    []agent.ToolRequest{{Name: "task.ask", Arguments: arguments}},
		Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		FinishReason: "tool_calls",
	}, nil
}

// nativeHandoffTools proves the handoff never reaches the tool port.
type nativeHandoffTools struct{}

func (nativeHandoffTools) Invoke(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
	return agent.ToolResult{}, errors.New("task.ask must not reach the tool port")
}

// waitHandoffObservation waits until one running container action already
// carries a persisted runtime observation. The first running observation is the
// last engine commit before the exit observation, so after this barrier the
// engine issues no further action or execution version write until the workload
// exits. A handoff read after it cannot be refused because activation or
// preparation moved the version under the app.
func waitHandoffObservation(t *testing.T, harness *factory.Harness, executionID, actionName string) {
	t.Helper()
	harness.Eventually(t, 2*time.Minute, func() bool {
		row, err := harness.ActionRow(t.Context(), executionID, actionName)
		return err == nil && row.ObservationSet
	})
}

// TestNativeAgentHandoffServedTaskAPI runs the native agent composition against
// the real served task API: the app reads the current task lifecycle version,
// saves a durable checkpoint, asks with that version and a durable reference,
// and the served task becomes waiting with the ask cancelled execution.
func TestNativeAgentHandoffServedTaskAPI(t *testing.T) {
	harness := factory.Start(t)
	applied := applyPipeline(t, harness, "native-handoff", holdPipeline(t, harness))
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "native agent handoff",
		PipelineID: applied.PipelineID,
	}, "native-handoff-create")
	waitExecutionRunning(t, harness, created.ExecutionID)
	// Activation is several commits, not one: the activation commit makes the
	// execution observable as running, then preparation commits the
	// launch-attempt marker and, inside Prepare, the first execution
	// credential, and the first runtime observation of the running container
	// lands after Start. Each advances the execution row version. The native
	// app reads the task version once and asks with the execution version it
	// observes, so a commit that lands between its read and its ask refuses the
	// handoff with a version_conflict the app cannot correct. The persisted
	// observation of the running action is the last engine commit before the
	// exit observation, so waiting for it keeps this test on the served handoff
	// wire instead of racing activation. It is a barrier, never a fixed delay.
	waitHandoffObservation(t, harness, created.ExecutionID, "hold")
	before := readTask(t, harness, harness.ProjectToken, created.TaskID)

	model := &nativeHandoffModel{
		question:   "Which branch should the next attempt use?",
		assignment: "role/reviewer",
	}
	var out bytes.Buffer
	err := app.Run(t.Context(), app.Config{
		Run: agent.RunInput{
			TaskID:      created.TaskID,
			ExecutionID: created.ExecutionID,
			Model:       "owner-regression-fixture",
			Messages:    []agent.Message{{Role: "user", Content: "continue the task"}},
			Tools:       []string{"task.ask"},
		},
		ProjectID:      factory.ProjectID,
		CheckpointRoot: t.TempDir(),
		Out:            &out,
		Transport:      harness.Client(harness.ProjectToken),
		Model:          model,
		Tools:          nativeHandoffTools{},
	})
	if err != nil {
		t.Fatalf("native agent run: %v", err)
	}
	var outcome agent.Outcome
	if err := json.Unmarshal(out.Bytes(), &outcome); err != nil {
		t.Fatalf("decode agent outcome: %v; body=%s", err, out.String())
	}
	if outcome.Status != agent.OutcomeWaiting {
		t.Fatalf("outcome status = %q; want waiting", outcome.Status)
	}
	if outcome.CheckpointRef == "" || !agent.ValidCheckpointRef(outcome.CheckpointRef) {
		t.Fatalf("outcome checkpoint reference = %q; want a durable reference", outcome.CheckpointRef)
	}

	waiting := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after native ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question: %+v", waiting)
	}
	if waiting.Question.Body != model.question {
		t.Fatalf("question body = %q; want %q", waiting.Question.Body, model.question)
	}
	if waiting.Question.Assignment != model.assignment {
		t.Fatalf("question assignment = %q; want %q", waiting.Question.Assignment, model.assignment)
	}
	if len(waiting.Question.References) != 1 {
		t.Fatalf("question references = %v; want one durable checkpoint reference", waiting.Question.References)
	}
	if !agent.ValidCheckpointRef(waiting.Question.References[0]) {
		t.Fatalf("question reference %q is not a durable checkpoint reference", waiting.Question.References[0])
	}
	if waiting.Question.References[0] != outcome.CheckpointRef {
		t.Fatalf("served reference = %q; outcome checkpoint = %q",
			waiting.Question.References[0], outcome.CheckpointRef)
	}
	if waiting.LifecycleVersion <= before.LifecycleVersion {
		t.Fatalf("task lifecycle version = %d; want > %d", waiting.LifecycleVersion, before.LifecycleVersion)
	}

	cancelled := inspectExecution(t, harness, harness.ProjectToken, created.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("execution state after native ask = %q; want cancelled", cancelled.State)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d; want one handoff decision", model.calls)
	}
}
