package work

// These tests drive the real task service with an EXECUTION principal onto the
// work adapters. An execution principal holds no membership row: its grant is
// the registry's execution permission and its enablement is a running execution
// row in scope. Before the adapter's membership guard understood that shape,
// every execution-built task.create/edit/comment/ask unit was refused at the
// commit boundary.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// executionRow reads one committed execution row.
func (env *realServiceEnv) executionRow(t *testing.T, executionID id.ExecutionID) store.Row {
	t.Helper()
	row, ok := env.fixture.store.Snapshot(env.scope, "executions", string(executionID))
	if !ok {
		t.Fatalf("execution %q is not committed", executionID)
	}
	return row
}

// markExecutionRunning advances one committed execution to running at its
// current version, the state the registry's execution permission requires.
func (env *realServiceEnv) markExecutionRunning(t *testing.T, executionID id.ExecutionID) {
	t.Helper()
	row := env.executionRow(t, executionID)
	if _, err := env.fixture.store.Execute(context.Background(), env.scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            string(executionID),
			ExpectVersion: versionValue(row, "version"),
			Values:        map[string]any{"state": executionStateRunning},
		}},
	}); err != nil {
		t.Fatalf("mark execution %q running: %v", executionID, err)
	}
}

// registerExecutionActor registers the committed execution's principal and
// returns the authenticated execution actor that presents its credential.
func (env *realServiceEnv) registerExecutionActor(t *testing.T, executionID id.ExecutionID) authorize.Principal {
	t.Helper()
	row := env.executionRow(t, executionID)
	principalID := id.PrincipalID(textValue(row, "principal_id"))
	if principalID == "" {
		t.Fatalf("execution %q has no principal", executionID)
	}
	principal := identity.Principal{
		ID:          principalID,
		Kind:        identity.KindExecution,
		Scope:       env.scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: executionID,
	}
	env.principals.principals[principalID] = principal
	return principal
}

// TestRealTaskServiceExecutionPrincipalCommitsOwnTask proves every task unit an
// execution principal may build through the real service commits: task.create
// under its project_task_create permission and task.edit, task.comment and
// task.ask on its own current task under its own-task permission.
func TestRealTaskServiceExecutionPrincipalCommitsOwnTask(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	human := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)

	started, err := env.service.Create(ctx, task.CreateCommand{
		Actor: human, Scope: env.scope, Title: "Execution-owned task", PipelineID: testPipeline, Start: true,
		CallerKey: "execution-own-setup", Content: integrationContent("execution-own-setup"),
	})
	if err != nil {
		t.Fatalf("setup create-and-start: %v", err)
	}
	env.markExecutionRunning(t, started.ExecutionID)
	executionActor := env.registerExecutionActor(t, started.ExecutionID)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: executionActor, Scope: env.scope, Title: "Created by the running execution",
		CallerKey: "execution-create", Content: integrationContent("execution-create"),
	})
	if err != nil {
		t.Fatalf("task create by execution: %v", err)
	}
	if created.TaskID == "" {
		t.Fatal("execution create committed no task id")
	}

	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: executionActor, Scope: env.scope, TaskID: started.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 1, Title: "Edited by its execution",
		CallerKey: "execution-edit", Content: integrationContent("execution-edit"),
	}); err != nil {
		t.Fatalf("task content edit by execution: %v", err)
	}

	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: executionActor, Scope: env.scope, TaskID: started.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 2, Body: "a note from the execution",
		CallerKey: "execution-comment", Content: integrationContent("execution-comment"),
	}); err != nil {
		t.Fatalf("task comment edit by execution: %v", err)
	}

	asked, err := env.service.Ask(ctx, task.AskCommand{
		Actor: executionActor, Scope: env.scope, TaskID: started.TaskID, ExpectedLifecycleVersion: 2,
		Question:   "Which branch should I target?",
		Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		CallerKey:  "execution-ask", Content: integrationContent("execution-ask"),
	})
	if err != nil {
		t.Fatalf("task ask by execution: %v", err)
	}
	if asked.LifecycleVersion != 3 {
		t.Fatalf("ask lifecycle version = %d; want 3", asked.LifecycleVersion)
	}

	row := env.fixture.storedTask(t, env.scope, started.TaskID)
	if title := textValue(row, "title"); title != "Edited by its execution" {
		t.Fatalf("content edit title = %q; want Edited by its execution", title)
	}
	if versionValue(row, "content_version") != 3 || versionValue(row, "lifecycle_version") != 3 {
		t.Fatalf("task versions = content %d lifecycle %d; want 3 and 3",
			versionValue(row, "content_version"), versionValue(row, "lifecycle_version"))
	}
	if state := textValue(row, "state"); state != string(task.StateWaiting) {
		t.Fatalf("task state = %q; want waiting", state)
	}
	if row.Values["question_id"] == nil {
		t.Fatal("ask by execution committed no outstanding question")
	}
	if state := textValue(env.executionRow(t, started.ExecutionID), "state"); state != "cancelled" {
		t.Fatalf("asked execution state = %q; want cancelled", state)
	}
}

// TestRealTaskServiceExecutionPrincipalForeignOrStaleRefused proves the service
// and the commit boundary still refuse an execution that is not the task's
// current execution and an execution row that is not running.
func TestRealTaskServiceExecutionPrincipalForeignOrStaleRefused(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	human := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)

	createStarted := func(t *testing.T, label string) task.CreateResult {
		t.Helper()
		result, err := env.service.Create(ctx, task.CreateCommand{
			Actor: human, Scope: env.scope, Title: "Task " + label, PipelineID: testPipeline, Start: true,
			CallerKey: label, Content: integrationContent(label),
		})
		if err != nil {
			t.Fatalf("setup create-and-start %q: %v", label, err)
		}
		return result
	}

	t.Run("foreign execution", func(t *testing.T) {
		own := createStarted(t, "foreign-own")
		env.markExecutionRunning(t, own.ExecutionID)
		foreign := createStarted(t, "foreign-other")
		env.markExecutionRunning(t, foreign.ExecutionID)
		foreignActor := env.registerExecutionActor(t, foreign.ExecutionID)

		before := env.fixture.commitCounts(t, env.scope)
		_, err := env.service.Edit(ctx, task.EditCommand{
			Actor: foreignActor, Scope: env.scope, TaskID: own.TaskID, Kind: task.EditContent,
			ExpectedContentVersion: 1, Title: "hijacked",
			CallerKey: "foreign-edit", Content: integrationContent("foreign-edit"),
		})
		mustClass(t, err, fault.Forbidden)
		env.fixture.requireUnchanged(t, env.scope, before)
		if title := textValue(env.fixture.storedTask(t, env.scope, own.TaskID), "title"); title == "hijacked" {
			t.Fatal("a foreign execution edited another task")
		}
	})

	t.Run("stale execution row", func(t *testing.T) {
		stale := createStarted(t, "stale-own")
		// The task's current execution is the actor's execution, but the row is
		// still queued: the commit-boundary membership fact requires running.
		staleActor := env.registerExecutionActor(t, stale.ExecutionID)

		before := env.fixture.commitCounts(t, env.scope)
		_, err := env.service.Edit(ctx, task.EditCommand{
			Actor: staleActor, Scope: env.scope, TaskID: stale.TaskID, Kind: task.EditContent,
			ExpectedContentVersion: 1, Title: "stale write",
			CallerKey: "stale-edit", Content: integrationContent("stale-edit"),
		})
		mustClass(t, err, fault.Forbidden)
		env.fixture.requireUnchanged(t, env.scope, before)
		if title := textValue(env.fixture.storedTask(t, env.scope, stale.TaskID), "title"); title == "stale write" {
			t.Fatal("a non-running execution edited the task")
		}
	})
}
