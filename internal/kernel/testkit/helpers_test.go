package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// This file contains test-only constructors. The production testkit API is
// deliberately exercised from the same package so the red state is a precise
// missing symbol rather than a missing import path.

var testNow = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func testExecution(executionID, workspaceID string, actions ...kernel.Action) kernel.Execution {
	return kernel.Execution{
		ID:          id.ExecutionID(executionID),
		Version:     0,
		State:       kernel.ExecutionQueued,
		WorkspaceID: id.WorkspaceID(workspaceID),
		Snapshot: definition.NewSnapshot(definition.SnapshotInput{
			Pipeline: definition.Pipeline{},
			YAML:     []byte("name: test\n"),
			Inputs:   map[string]string{"input": "value"},
		}),
		Actions:       append([]kernel.Action(nil), actions...),
		CurrentAction: -1,
	}
}

func testContainerAction(actionID, name string, index int, state kernel.ActionState, version uint64) kernel.Action {
	workloadID := id.WorkloadID("wl-" + actionID)
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindContainer,
		State:   state,
		Version: version,
		Timeout: time.Hour,
		Outputs: kernel.Outputs{},
		Container: &kernel.ContainerAction{
			Definition: definition.Container{
				Image:    "registry.example/app@sha256:abc",
				Commands: []string{"run"},
				Outputs:  []id.OutputKey{"candidate"},
			},
			WorkloadID:  workloadID,
			WorkspaceID: id.WorkspaceID("ws-1"),
			Launch: kernel.LaunchIntent{
				WorkloadID:     workloadID,
				State:          kernel.LaunchNotIssued,
				IdempotencyKey: "idem-" + actionID,
			},
		},
	}
}

func testRequestAction(actionID, name string, index int, state kernel.ActionState, version uint64) kernel.Action {
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindRequest,
		State:   state,
		Version: version,
		Timeout: time.Hour,
		Request: &kernel.RequestAction{
			Definition: definition.Request{
				Assignment: "assignee",
				Outputs:    []id.OutputKey{"decision"},
			},
		},
	}
}

func testCommit(executionID, actionID string, executionVersion, actionVersion uint64) flow.Commit {
	return flow.Commit{
		ExecutionID:              id.ExecutionID(executionID),
		ExpectedExecutionVersion: executionVersion,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 id.ActionID(actionID),
		ExpectedActionVersion:    actionVersion,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
}

func testIdentity(executionID, actionID, workloadID string) engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    id.ExecutionID(executionID),
		ActionID:       id.ActionID(actionID),
		WorkloadID:     id.WorkloadID(workloadID),
		IdempotencyKey: "idem-" + workloadID,
	}
}

func testPrepare(t *testing.T, runtime *Runtime, identity engine.EffectIdentity, workspaceID id.WorkspaceID) engine.Preparation {
	t.Helper()
	preparation, err := runtime.Prepare(context.Background(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		Container: definition.Container{
			Image:    "registry.example/app@sha256:abc",
			Commands: []string{"run"},
		},
	})
	if err != nil {
		t.Fatalf("Prepare(%s) error = %v", identity.WorkloadID, err)
	}
	return preparation
}

func testStart(t *testing.T, runtime *Runtime, identity engine.EffectIdentity, workspaceID id.WorkspaceID) engine.StartResult {
	t.Helper()
	result, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Start(%s) error = %v", identity.WorkloadID, err)
	}
	return result
}

func testRuntimeWithPreparedWorkload(t *testing.T) (*Runtime, engine.EffectIdentity, id.WorkspaceID) {
	t.Helper()
	runtime := NewRuntime()
	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")
	testPrepare(t, runtime, identity, workspaceID)
	return runtime, identity, workspaceID
}

func testRuntimeWithStartedWorkload(t *testing.T) (*Runtime, engine.EffectIdentity, id.WorkspaceID) {
	t.Helper()
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	result := testStart(t, runtime, identity, workspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("Start outcome = %q, want %q", result.Outcome, engine.StartAccepted)
	}
	return runtime, identity, workspaceID
}

func testClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = (%q, %v) for %v, want %q", class, ok, err, want)
	}
}
