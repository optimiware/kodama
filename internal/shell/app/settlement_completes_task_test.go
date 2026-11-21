package app

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/memory"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

type recordingProgressSettler struct {
	calls []task.ProgressCommand
}

func (r *recordingProgressSettler) Progress(_ context.Context, cmd task.ProgressCommand) (task.ProgressStatus, error) {
	r.calls = append(r.calls, cmd)
	return task.ProgressStatus{State: task.StateDone, Applied: true}, nil
}

func TestTaskSettlementSkipsExecutionsWithCompletesTaskFalse(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	backend := memory.New()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	seedExecution := func(taskID id.TaskID, execID id.ExecutionID, completesTask *bool) {
		t.Helper()
		snap, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
			YAML:          []byte("actions:\n  - name: run\n    image: alpine\n    commands: [echo ok]\n"),
			TaskID:        taskID,
			Title:         string(taskID),
			Conversation:  []byte("[]"),
			CompletesTask: completesTask,
		})
		if err != nil {
			t.Fatalf("NewSnapshot: %v", err)
		}
		doc, err := snapshotdoc.Encode(snapshotdoc.Admitted{
			PipelineID:      "pipe-1",
			PipelineVersion: 1,
			Snapshot:        snap,
			Actions: []pipeline.AdmittedAction{{
				Index:   0,
				Name:    "run",
				Kind:    definition.ActionKindContainer,
				Timeout: time.Minute,
				Container: &definition.Container{
					Image:    "alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111",
					Commands: []string{"echo ok"},
				},
			}},
		})
		if err != nil {
			t.Fatalf("snapshotdoc.Encode: %v", err)
		}
		digest := snap.YAMLSHA256
		if _, err := backend.Execute(ctx, scope, store.Unit{
			Writes: []store.Write{
				{
					Table:  "tasks",
					ID:     string(taskID),
					Insert: true,
					Values: map[string]any{
						"content_version":      int64(1),
						"lifecycle_version":    int64(2),
						"state":                string(task.StateOpen),
						"initiator_id":         "user-1",
						"title":                string(taskID),
						"current_execution_id": string(execID),
						"created_at":           now,
					},
				},
				{
					Table:  "executions",
					ID:     string(execID),
					Insert: true,
					Values: map[string]any{
						"pipeline_id":  "pipe-1",
						"task_id":      string(taskID),
						"principal_id": "exec-p-" + string(execID),
						"state":        string(kernel.ExecutionSucceeded),
						"version":      int64(3),
						"yaml_sha256":  digest[:],
						"snapshot":     doc,
						"workspace_id": string(snapshotdoc.DeriveWorkspaceID(execID)),
						"created_at":   now,
					},
				},
			},
		}); err != nil {
			t.Fatalf("seed %s: %v", taskID, err)
		}
	}

	falseVal := false
	seedExecution("task-stay-open", "exec-stay-open", &falseVal)
	seedExecution("task-auto-done", "exec-auto-done", nil)

	recorder := &recordingProgressSettler{}
	settler := newTaskSettlement(backend.Controller(), backend.Controller(), backend, recorder)
	if err := settler.settle(ctx); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("progress calls = %+v; want exactly 1 (only task-auto-done)", recorder.calls)
	}
	if recorder.calls[0].TaskID != "task-auto-done" || recorder.calls[0].ExecutionID != "exec-auto-done" {
		t.Fatalf("progress call = %+v; want task-auto-done/exec-auto-done", recorder.calls[0])
	}
}
