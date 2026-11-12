//go:build component

package postgrescomponent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestExecutionListOrderAndFiltersOnPostgreSQL proves the execution list pages
// in (created_at,id) order, filters by task/pipeline/state, and never skips or
// repeats a row across the cursor.
func TestExecutionListOrderAndFiltersOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	shard.SeedTask(ctx, scope, "task-1", "open", 1, "pipeline-1")
	shard.SeedTask(ctx, scope, "task-2", "open", 1, "pipeline-1")
	shard.SeedPipeline(ctx, scope, "pipeline-2", "other", []byte("actions: []\n"), 1, "[]")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	store := executionstore.New(storepg.New(runtime), storepg.New(runtime))
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: run\n    image: " + integrationImage + "\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	container := definition.Container{Image: integrationImage, Commands: []string{"echo", "ok"}}
	action := pipeline.AdmittedAction{Index: 0, Name: "run", Kind: definition.ActionKindContainer, Timeout: time.Minute, Container: &container}
	for index, item := range []struct{ id, task string }{{"exec-a", "task-1"}, {"exec-b", "task-1"}, {"exec-c", "task-2"}} {
		at := pgtest.FixtureTime.Add(time.Duration(index) * time.Minute)
		snapshotWithTask, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: run\n    image: " + integrationImage + "\n"), TaskID: id.TaskID(item.task)})
		if err != nil {
			t.Fatalf("NewSnapshot: %v", err)
		}
		if _, err := store.Start(ctx, execution.StartUnit{
			Owner: scope, PipelineID: "pipeline-1", ExpectedPipelineVersion: 1,
			Snapshot: snapshotWithTask, Actions: []pipeline.AdmittedAction{action},
			Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
			NewExecutionID: id.ExecutionID(item.id), At: at,
		}); err != nil {
			t.Fatalf("execution Start %s: %v%s", item.id, err, pgtest.CauseText(err))
		}
	}
	_ = snapshot
	// Two extra executions outside the task/pipeline/state filters prove each
	// predicate actually narrows the list.
	shard.SeedTask(ctx, scope, "task-3", "open", 1, "pipeline-2")
	for index, item := range []struct{ id, task, pipeline, state string }{
		{"exec-d", "task-3", "pipeline-2", "running"},
		{"exec-e", "task-3", "pipeline-2", "failed"},
	} {
		at := pgtest.FixtureTime.Add(time.Duration(10+index) * time.Minute)
		snapshotOther, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: run\n    image: " + integrationImage + "\n"), TaskID: id.TaskID(item.task)})
		if err != nil {
			t.Fatalf("NewSnapshot: %v", err)
		}
		if _, err := store.Start(ctx, execution.StartUnit{
			Owner: scope, PipelineID: pipeline.PipelineID(item.pipeline), ExpectedPipelineVersion: 1,
			Snapshot: snapshotOther, Actions: []pipeline.AdmittedAction{action},
			Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
			NewExecutionID: id.ExecutionID(item.id), At: at,
		}); err != nil {
			t.Fatalf("execution Start %s: %v%s", item.id, err, pgtest.CauseText(err))
		}
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(), `UPDATE executions SET state = $2 WHERE id = $1`, item.id, item.state); err != nil {
			t.Fatalf("set execution state: %v", err)
		}
	}

	first, err := store.List(ctx, scope, execution.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List page 1: %v%s", err, pgtest.CauseText(err))
	}
	if len(first.Items) != 2 || first.Items[0].ExecutionID != "exec-a" || first.Items[1].ExecutionID != "exec-b" || first.Next == "" {
		t.Fatalf("execution page 1 = %v next=%q; want exec-a,exec-b with a cursor", viewIDs(first.Items), first.Next)
	}
	second, err := store.List(ctx, scope, execution.ListFilter{Limit: 2, Cursor: first.Next})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(second.Items) != 2 || second.Items[0].ExecutionID != "exec-c" || second.Items[1].ExecutionID != "exec-d" || second.Next == "" {
		t.Fatalf("execution page 2 = %v next=%q; want exec-c,exec-d with a cursor", viewIDs(second.Items), second.Next)
	}
	third, err := store.List(ctx, scope, execution.ListFilter{Limit: 2, Cursor: second.Next})
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(third.Items) != 1 || third.Items[0].ExecutionID != "exec-e" || third.Next != "" {
		t.Fatalf("execution page 3 = %v next=%q; want only exec-e", viewIDs(third.Items), third.Next)
	}
	byTask, err := store.List(ctx, scope, execution.ListFilter{TaskID: "task-1", Limit: 10})
	if err != nil || fmt.Sprint(viewIDs(byTask.Items)) != "[exec-a exec-b]" {
		t.Fatalf("task filter = %v, err=%v; want exec-a,exec-b", viewIDs(byTask.Items), err)
	}
	byPipeline, err := store.List(ctx, scope, execution.ListFilter{PipelineID: "pipeline-1", Limit: 10})
	if err != nil || fmt.Sprint(viewIDs(byPipeline.Items)) != "[exec-a exec-b exec-c]" {
		t.Fatalf("pipeline filter = %v, err=%v; want exec-a,exec-b,exec-c", viewIDs(byPipeline.Items), err)
	}
	byState, err := store.List(ctx, scope, execution.ListFilter{State: kernel.ExecutionQueued, Limit: 10})
	if err != nil || fmt.Sprint(viewIDs(byState.Items)) != "[exec-a exec-b exec-c]" {
		t.Fatalf("state filter = %v, err=%v; want exec-a,exec-b,exec-c", viewIDs(byState.Items), err)
	}
}

func viewIDs(items []execution.View) []string {
	out := make([]string, len(items))
	for index, item := range items {
		out[index] = string(item.ExecutionID)
	}
	return out
}

// TestPipelinesListOrderOnPostgreSQL proves Pipelines.List orders by name,id.
func TestPipelinesListOrderOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	for id, name := range map[string]string{"pipeline-c": "gamma", "pipeline-a": "alpha", "pipeline-b": "beta"} {
		shard.SeedPipeline(ctx, scope, id, name, []byte("actions: []\n"), 1, "[]")
	}
	runtime := shard.Runtime(4)
	defer runtime.Close()
	adapter, err := work.NewPipelines(storepg.New(runtime), storepg.New(runtime), nil)
	if err != nil {
		t.Fatalf("NewPipelines: %v", err)
	}
	definitions, err := adapter.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v%s", err, pgtest.CauseText(err))
	}
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name
	}
	if fmt.Sprint(names) != "[alpha beta gamma]" {
		t.Fatalf("pipeline order = %v; want alpha,beta,gamma", names)
	}
}

// TestExecutionListExposesRequestProjectionOnPostgreSQL proves the list page
// carries the same request action projection as a direct inspection: the
// immutable subject and exact assignment on a request action, and an empty
// optional pipeline identity for a task-free inline execution. Neither field is
// dropped or fabricated while paging.
func TestExecutionListExposesRequestProjectionOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	store := executionstore.New(storepg.New(runtime), storepg.New(runtime))

	requestSnapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot request: %v", err)
	}
	declared := definition.Request{
		Assignment: "role/reviewer",
		Subject: definition.NewSubject(map[string]definition.SubjectValue{
			"ref": {Kind: definition.SubjectLiteral, Literal: "main"},
		}),
		Outputs: []id.OutputKey{"decision"},
	}
	if _, err := store.Start(ctx, execution.StartUnit{
		Owner: scope, PipelineID: "pipeline-1", ExpectedPipelineVersion: 1,
		Snapshot: requestSnapshot,
		Actions: []pipeline.AdmittedAction{{
			Index: 0, Name: "approve", Kind: definition.ActionKindRequest, Timeout: time.Minute, Request: &declared,
		}},
		Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: "exec-request", At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("Start request execution: %v%s", err, pgtest.CauseText(err))
	}
	inlineSnapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot inline: %v", err)
	}
	container := definition.Container{Image: integrationImage, Commands: []string{"true"}}
	if _, err := store.Start(ctx, execution.StartUnit{
		Owner: scope, PipelineID: "", ExpectedPipelineVersion: 0,
		Snapshot: inlineSnapshot,
		Actions: []pipeline.AdmittedAction{{
			Index: 0, Name: "run", Kind: definition.ActionKindContainer, Timeout: time.Minute, Container: &container,
		}},
		Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: "exec-inline", At: pgtest.FixtureTime.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Start inline execution: %v%s", err, pgtest.CauseText(err))
	}
	resolved, err := json.Marshal(definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectLiteral, Literal: "main"},
	}))
	if err != nil {
		t.Fatalf("marshal resolved subject: %v", err)
	}
	actionID := snapshotdoc.DeriveActionID("exec-request", 0)
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET state = 'running', deadline = $2, request_subject = $3 WHERE id = $1`,
		actionID, pgtest.FixtureTime, resolved); err != nil {
		t.Fatalf("activate request action: %v", err)
	}

	wantAssignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}
	refs := map[string]execution.View{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("execution list did not terminate")
		}
		page, err := store.List(ctx, scope, execution.ListFilter{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("List page %d: %v%s", pages, err, pgtest.CauseText(err))
		}
		if len(page.Items) != 1 {
			t.Fatalf("List page %d = %d items; want 1", pages, len(page.Items))
		}
		view := page.Items[0]
		refs[string(view.ExecutionID)] = view
		if view.ExecutionID == "exec-inline" {
			if view.PipelineID != "" || view.TaskID != "" {
				t.Fatalf("inline list view identity = pipeline %q task %q; want both empty", view.PipelineID, view.TaskID)
			}
		}
		if view.ExecutionID == "exec-request" {
			if view.PipelineID != "pipeline-1" {
				t.Fatalf("request list view pipeline_id = %q; want pipeline-1", view.PipelineID)
			}
			if len(view.Actions) != 1 {
				t.Fatalf("request list view actions = %d; want 1", len(view.Actions))
			}
			action := view.Actions[0]
			if action.Subject == nil || action.Subject.Fields()["ref"].Literal != "main" {
				t.Fatalf("request list action subject = %+v; want the immutable main field", action.Subject)
			}
			if action.Assignment != wantAssignment {
				t.Fatalf("request list action assignment = %+v; want %+v", action.Assignment, wantAssignment)
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(refs) != 2 {
		t.Fatalf("list pages covered %d executions; want 2", len(refs))
	}
	// Inspection and the list agree for both executions.
	for executionID, listed := range refs {
		inspected, err := store.Inspect(ctx, scope, id.ExecutionID(executionID))
		if err != nil {
			t.Fatalf("Inspect %s: %v%s", executionID, err, pgtest.CauseText(err))
		}
		if inspected.PipelineID != listed.PipelineID || inspected.TaskID != listed.TaskID {
			t.Fatalf("%s identity: inspect pipeline=%q task=%q list pipeline=%q task=%q",
				executionID, inspected.PipelineID, inspected.TaskID, listed.PipelineID, listed.TaskID)
		}
		if len(inspected.Actions) != len(listed.Actions) {
			t.Fatalf("%s action count: inspect=%d list=%d", executionID, len(inspected.Actions), len(listed.Actions))
		}
		for index := range inspected.Actions {
			a, b := inspected.Actions[index], listed.Actions[index]
			if a.Assignment != b.Assignment {
				t.Fatalf("%s action %d assignment: inspect=%+v list=%+v", executionID, index, a.Assignment, b.Assignment)
			}
			if (a.Subject == nil) != (b.Subject == nil) {
				t.Fatalf("%s action %d subject presence differs: inspect=%v list=%v", executionID, index, a.Subject, b.Subject)
			}
			if a.Subject != nil && !reflect.DeepEqual(a.Subject.Fields(), b.Subject.Fields()) {
				t.Fatalf("%s action %d subject: inspect=%+v list=%+v", executionID, index, a.Subject.Fields(), b.Subject.Fields())
			}
		}
	}
}
