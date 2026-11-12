package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// TestIntentSaveFailureLeavesNoEffect proves checkpoint-before-effect: when the
// durable intent cannot be saved, the tool is never invoked.
func TestIntentSaveFailureLeavesNoEffect(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.write", `{"path":"a"}`)}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
	}}
	tool := &fakeTool{}
	checkpoints := newFakeCheckpoints()
	checkpoints.failAt = map[int]error{0: fault.New(fault.DependencyUnavailable, "checkpoint store unavailable")}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	})
	if err == nil {
		t.Fatal("checkpoint save failure was not reported")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v/%v; want dependency_unavailable", class, ok)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tool calls = %d; want 0 (no effect without durable intent)", tool.callCount())
	}
	if checkpoints.saveCount() != 0 {
		t.Fatalf("checkpoint saves = %d; want 0", checkpoints.saveCount())
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("status = %q; want failed", outcome.Status)
	}
}

// TestCompletionSaveFailureIsUncertain proves a lost completion record stops
// the run as uncertain and is never replayed from the surviving intent.
func TestCompletionSaveFailureIsUncertain(t *testing.T) {
	call := toolCall("repo.write", `{"path":"a"}`)
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{call}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{"repo.write": {Name: "repo.write", Content: "written"}}}
	checkpoints := newFakeCheckpoints()
	checkpoints.failAt = map[int]error{1: fault.New(fault.DependencyUnavailable, "checkpoint store unavailable")}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	})
	if err != nil {
		t.Fatalf("uncertain settlement escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeUncertain {
		t.Fatalf("status = %q; want uncertain", outcome.Status)
	}
	if len(outcome.Uncertain) != 1 {
		t.Fatalf("uncertain effects = %v; want one", outcome.Uncertain)
	}
	if tool.callCount() != 1 || model.callCount() != 1 {
		t.Fatalf("model/tool calls = %d/%d; want 1/1", model.callCount(), tool.callCount())
	}
	if checkpoints.saveCount() != 1 {
		t.Fatalf("checkpoint saves = %d; want only the intent", checkpoints.saveCount())
	}
	intent := checkpoints.checkpointAt(0)
	if len(intent.InFlight) != 1 || len(intent.Completed) != 0 {
		t.Fatalf("intent effects = in-flight %v completed %v", intent.InFlight, intent.Completed)
	}
	if outcome.CheckpointRef != checkpoints.refAt(0) {
		t.Fatalf("outcome ref = %q; want the intent ref %q", outcome.CheckpointRef, checkpoints.refAt(0))
	}

	// Resuming from the intent must not replay the effect; it stops for
	// reconciliation because the intent is still in flight.
	checkpoints.failAt = nil
	resumedModel := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{call}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	resumed := newTestRuntime(t, agent.RuntimeConfig{
		Model: resumedModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	resumedOutcome, resumedErr := resumed.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "write"}},
		Tools:      []string{"repo.write"},
		RestoreRef: checkpoints.refAt(0),
	})
	if resumedErr != nil {
		t.Fatalf("resume: %v", resumedErr)
	}
	if resumedOutcome.Status != agent.OutcomeUncertain {
		t.Fatalf("resumed status = %q; want uncertain", resumedOutcome.Status)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls after resume = %d; want 1 (never replayed)", tool.callCount())
	}
	if resumedModel.callCount() != 0 {
		t.Fatalf("resume called the model %d times before reconciling", resumedModel.callCount())
	}
}

// TestLostToolResponseStopsForReconciliation covers an effect that ran but whose
// response was lost: it is recorded uncertain and never retried.
func TestLostToolResponseStopsForReconciliation(t *testing.T) {
	call := toolCall("repo.write", `{"path":"a"}`)
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{call}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{errAt: map[int]error{0: fault.New(fault.UncertainExternalEffect, "tool response lost")}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	})
	if err != nil {
		t.Fatalf("lost response escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeUncertain || len(outcome.Uncertain) != 1 {
		t.Fatalf("outcome = %+v; want uncertain with one effect", outcome)
	}
	if tool.callCount() != 1 || model.callCount() != 1 {
		t.Fatalf("model/tool calls = %d/%d; want 1/1", model.callCount(), tool.callCount())
	}
	last := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	if len(last.Uncertain) != 1 || len(last.InFlight) != 0 || len(last.Completed) != 0 {
		t.Fatalf("checkpoint effects = uncertain %v in-flight %v completed %v", last.Uncertain, last.InFlight, last.Completed)
	}

	// A resume from the uncertain checkpoint stops without calling model or tool.
	resumedModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("done", 1, 1)}}
	resumed := newTestRuntime(t, agent.RuntimeConfig{
		Model: resumedModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	resumedOutcome, resumedErr := resumed.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "write"}},
		Tools:      []string{"repo.write"},
		RestoreRef: checkpoints.refAt(checkpoints.saveCount() - 1),
	})
	if resumedErr != nil {
		t.Fatalf("resume: %v", resumedErr)
	}
	if resumedOutcome.Status != agent.OutcomeUncertain {
		t.Fatalf("resumed status = %q; want uncertain", resumedOutcome.Status)
	}
	if tool.callCount() != 1 || resumedModel.callCount() != 0 {
		t.Fatalf("resume model/tool calls = %d/%d; want 0/1", resumedModel.callCount(), tool.callCount())
	}
}

// TestCompletedEffectNotRepeatedAfterCrash proves a durably completed effect is
// skipped when a fresh execution restores the checkpoint.
func TestCompletedEffectNotRepeatedAfterCrash(t *testing.T) {
	call := toolCall("repo.write", `{"path":"a"}`)
	firstModel := &fakeModel{
		responses: []agent.ModelResponse{
			{ToolCalls: []agent.ToolRequest{call}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
			stopResponse("done", 1, 1),
		},
		errAt: map[int]error{1: fault.New(fault.DependencyUnavailable, "provider connection lost")},
	}
	tool := &fakeTool{results: map[string]agent.ToolResult{"repo.write": {Name: "repo.write", Content: "written"}}}
	checkpoints := newFakeCheckpoints()
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: firstModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	if _, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	}); err == nil {
		t.Fatal("simulated provider crash did not surface")
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls after first run = %d; want 1", tool.callCount())
	}
	if checkpoints.saveCount() != 2 {
		t.Fatalf("checkpoint saves = %d; want intent and completion", checkpoints.saveCount())
	}
	completed := checkpoints.checkpointAt(1)
	if len(completed.Completed) != 1 || len(completed.InFlight) != 0 {
		t.Fatalf("completed checkpoint = %+v", completed)
	}
	effectID := completed.Completed[0]

	// The fresh execution sees the same tool call again. It must skip the
	// completed effect rather than repeat it.
	secondModel := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{call}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "write"}},
		Tools:      []string{"repo.write"},
		RestoreRef: checkpoints.refAt(1),
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if outcome.Status != agent.OutcomeSucceeded || outcome.Summary != "done" {
		t.Fatalf("resumed outcome = %+v", outcome)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls after resume = %d; want 1 (completed effect not repeated)", tool.callCount())
	}
	if secondModel.callCount() != 2 {
		t.Fatalf("resumed model calls = %d; want 2", secondModel.callCount())
	}
	if len(outcome.Completed) != 1 || outcome.Completed[0] != effectID {
		t.Fatalf("completed identity changed across restore: %v vs %q", outcome.Completed, effectID)
	}
	final := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	if final.ExecutionID != "exec-2" {
		t.Fatalf("restored checkpoint execution = %q; want the fresh exec-2", final.ExecutionID)
	}
	if len(final.Completed) != 1 {
		t.Fatalf("final completed effects = %v; want one stable identity", final.Completed)
	}
}
