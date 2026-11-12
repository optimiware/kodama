package work

// These tests drive the real task service onto the work adapters over the
// memory store. Hand-built commit units hid the service's authority-read
// shapes; these use the real service.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestRealTaskServiceCommitsThroughAdapters drives create, start, ask, reply,
// ask and cancel through the real task service. Every one of those operations
// must reach the store although the task_version read carries no target and,
// for edits, no expected version.
func TestRealTaskServiceCommitsThroughAdapters(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.service.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	if started.LifecycleVersion != 2 {
		t.Fatalf("start lifecycle version = %d; want 2", started.LifecycleVersion)
	}
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Which branch should I target?", Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		CallerKey: "task-ask-1", Content: integrationContent("task-ask-1"),
	}); err != nil {
		t.Fatalf("task ask: %v", err)
	}
	current, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read asked task: %v", err)
	}
	if current.Question == nil {
		t.Fatal("ask committed no outstanding question")
	}
	replied, err := env.service.Reply(ctx, task.ReplyCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version, Answer: "main",
		CallerKey: "task-reply", Content: integrationContent("task-reply"),
	})
	if err != nil {
		t.Fatalf("task reply: %v", err)
	}
	if replied.ExecutionID == "" {
		t.Fatal("reply committed no fresh execution")
	}
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 4,
		Question: "Anything else?", Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		CallerKey: "task-ask-2", Content: integrationContent("task-ask-2"),
	}); err != nil {
		t.Fatalf("second task ask: %v", err)
	}
	cancelled, err := env.service.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 5,
		CallerKey: "task-cancel", Content: integrationContent("task-cancel"),
	})
	if err != nil {
		t.Fatalf("task cancel: %v", err)
	}
	if cancelled.LifecycleVersion != 6 {
		t.Fatalf("cancel lifecycle version = %d; want 6", cancelled.LifecycleVersion)
	}
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if textValue(row, "state") != string(task.StateCancelled) {
		t.Fatalf("final task state = %q; want cancelled", textValue(row, "state"))
	}
}

// TestRealTaskServiceCreateAndStartCommits drives the nested create-and-start
// unit the real service builds.
func TestRealTaskServiceCreateAndStartCommits(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline, Start: true,
		CallerKey: "task-create-and-start", Content: integrationContent("task-create-and-start"),
	})
	if err != nil {
		t.Fatalf("task create-and-start: %v", err)
	}
	if created.ExecutionID == "" {
		t.Fatal("create-and-start committed no execution")
	}
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if versionValue(row, "lifecycle_version") != 2 {
		t.Fatalf("create-and-start lifecycle version = %d; want 2", versionValue(row, "lifecycle_version"))
	}
}

// TestRealTaskServiceEditsCommitThroughAdapters drives content, comment and
// explicit pipeline selection edits. Content and comment edits carry a
// task_version read with no target and a zero expected version.
func TestRealTaskServiceEditsCommitThroughAdapters(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 1, Title: "Fix it now", Description: "updated",
		CallerKey: "task-edit-content", Content: integrationContent("task-edit-content"),
	}); err != nil {
		t.Fatalf("task content edit: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 2, Body: "a comment",
		CallerKey: "task-edit-comment", Content: integrationContent("task-edit-comment"),
	}); err != nil {
		t.Fatalf("task comment edit: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1, PipelineID: testPipeline,
		CallerKey: "task-edit-select", Content: integrationContent("task-edit-select"),
	}); err != nil {
		t.Fatalf("task select-pipeline edit: %v", err)
	}
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if textValue(row, "title") != "Fix it now" {
		t.Fatalf("content edit title = %q; want Fix it now", textValue(row, "title"))
	}
	if versionValue(row, "content_version") != 3 || versionValue(row, "lifecycle_version") != 2 {
		t.Fatalf("edit versions = content %d lifecycle %d; want 3 and 2", versionValue(row, "content_version"), versionValue(row, "lifecycle_version"))
	}
}

// TestRealTaskServiceStaleLifecycleVersionConflicts proves a stale service-built
// unit is refused by the adapter's own guard with nothing persisted.
func TestRealTaskServiceStaleLifecycleVersionConflicts(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	before := env.fixture.commitCounts(t, env.scope)
	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "task-start-stale", Content: integrationContent("task-start-stale"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"task_id": string(created.TaskID)}); count != 0 {
		t.Fatalf("a refused stale start committed %d executions", count)
	}
	if versionValue(env.fixture.storedTask(t, env.scope, created.TaskID), "lifecycle_version") != 1 {
		t.Fatal("a refused stale start advanced the task lifecycle")
	}
}

// TestRealTaskServiceDemotedMembershipRefused proves the adapter re-evaluates
// the real service's membership read at the commit boundary: the pre-port fake
// authority allows, the demoted store row refuses.
func TestRealTaskServiceDemotedMembershipRefused(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	before := env.fixture.commitCounts(t, env.scope)
	env.fixture.setMembershipRoles(t, env.scope, "membership-1", 1, tenancy.RoleViewer)
	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-demoted", Content: integrationContent("task-start-demoted"),
	})
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"task_id": string(created.TaskID)}); count != 0 {
		t.Fatalf("a refused demoted start committed %d executions", count)
	}
}

// TestRealTaskServiceOperatorRetry proves the real service emits the
// conditional operator read and the real adapter commits the retry only for an
// open task whose current execution failed, atomically with the start.
func TestRealTaskServiceOperatorRetry(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	engineer := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: engineer, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.service.Start(ctx, task.StartCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	// The current execution fails, so the operator retry is allowed.
	env.executeDirect(t, env.scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            string(started.ExecutionID),
		ExpectVersion: 1,
		Values:        map[string]any{"state": string(kernel.ExecutionFailed)},
	}}})
	operator := env.registerHuman(t, "principal-operator", tenancy.RoleOperator)
	env.fixture.seedMembership(t, env.scope, "membership-operator", "principal-operator", 1, tenancy.RoleOperator)

	retried, err := env.service.Start(ctx, task.StartCommand{
		Actor: operator, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "task-operator-retry", Content: integrationContent("task-operator-retry"),
	})
	if err != nil {
		t.Fatalf("operator retry: %v", err)
	}
	if retried.ExecutionID == "" || retried.ExecutionID == started.ExecutionID {
		t.Fatalf("operator retry execution = %q; want a fresh execution", retried.ExecutionID)
	}
	if retried.LifecycleVersion != 3 {
		t.Fatalf("operator retry lifecycle = %d; want 3", retried.LifecycleVersion)
	}
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if textValue(row, "current_execution_id") != string(retried.ExecutionID) {
		t.Fatalf("task current execution = %q; want the retry %q", textValue(row, "current_execution_id"), retried.ExecutionID)
	}
}

// TestRealTaskServiceOperatorRetryRefusesRunning proves the commit boundary,
// not the grant stage, refuses an operator start on an open task whose current
// execution is still running.
func TestRealTaskServiceOperatorRetryRefusesRunning(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	engineer := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: engineer, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.service.Start(ctx, task.StartCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	operator := env.registerHuman(t, "principal-operator", tenancy.RoleOperator)
	env.fixture.seedMembership(t, env.scope, "membership-operator", "principal-operator", 1, tenancy.RoleOperator)
	before := env.fixture.commitCounts(t, env.scope)

	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: operator, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "task-operator-retry", Content: integrationContent("task-operator-retry"),
	})
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)
	if textValue(env.fixture.storedTask(t, env.scope, created.TaskID), "current_execution_id") != string(started.ExecutionID) {
		t.Fatal("a refused operator retry replaced the task's current execution")
	}
}

// TestRealTaskServiceOperatorRetryVersionMoved proves an execution version that
// moves between the service read and the commit is a version_conflict, with
// nothing committed.
func TestRealTaskServiceOperatorRetryVersionMoved(t *testing.T) {
	var mutator *mutatingExecutor
	env := newRealServiceEnvWithExecutor(t, func(inner store.Executor) store.Executor {
		mutator = &mutatingExecutor{inner: inner}
		return mutator
	})
	ctx := context.Background()
	engineer := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: engineer, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.service.Start(ctx, task.StartCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	env.executeDirect(t, env.scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            string(started.ExecutionID),
		ExpectVersion: 1,
		Values:        map[string]any{"state": string(kernel.ExecutionFailed)},
	}}})
	operator := env.registerHuman(t, "principal-operator", tenancy.RoleOperator)
	env.fixture.seedMembership(t, env.scope, "membership-operator", "principal-operator", 1, tenancy.RoleOperator)
	mutator.hook = func() {
		env.executeDirect(t, env.scope, store.Unit{Writes: []store.Write{{
			Table:         "executions",
			ID:            string(started.ExecutionID),
			ExpectVersion: 2,
			Values:        map[string]any{"state": string(kernel.ExecutionFailed)},
		}}})
	}

	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: operator, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "task-operator-retry", Content: integrationContent("task-operator-retry"),
	})
	mustClass(t, err, fault.VersionConflict)
}

// TestRealTaskServiceOperatorRetryReplaysLostResponse proves the conditional
// operator retry is idempotent. After the first retry commits, the task's
// current execution is the newly queued one, so a naive re-evaluation of the
// failed-task condition would refuse a lost-response retry before the receipt
// could replay. The adapter serves the replay first while still re-evaluating
// the operator's current membership: a revoked operator, a different caller key
// at the same state and changed content are all refused without a write.
func TestRealTaskServiceOperatorRetryReplaysLostResponse(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	engineer := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: engineer, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.service.Start(ctx, task.StartCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	env.executeDirect(t, env.scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            string(started.ExecutionID),
		ExpectVersion: 1,
		Values:        map[string]any{"state": string(kernel.ExecutionFailed)},
	}}})
	operator := env.registerHuman(t, "principal-operator", tenancy.RoleOperator)
	env.fixture.seedMembership(t, env.scope, "membership-operator", "principal-operator", 1, tenancy.RoleOperator)

	retryCommand := task.StartCommand{
		Actor: operator, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "task-operator-retry", Content: integrationContent("task-operator-retry"),
	}
	retried, err := env.service.Start(ctx, retryCommand)
	if err != nil {
		t.Fatalf("operator retry: %v", err)
	}
	if retried.Replayed {
		t.Fatal("the first operator retry must not be a replay")
	}

	// The lost-response retry observes the state the first commit produced: the
	// current execution is the new queued retry (version 1, not failed) and the
	// lifecycle advanced to the committed version. The same key+content must
	// return the original result without evaluating the failed-task gate again.
	replayCommand := retryCommand
	replayCommand.ExpectedLifecycleVersion = retried.LifecycleVersion
	before := env.fixture.commitCounts(t, env.scope)
	replayed, err := env.service.Start(ctx, replayCommand)
	if err != nil {
		t.Fatalf("operator retry replay: %v", err)
	}
	if !replayed.Replayed {
		t.Fatalf("retry replay = %+v; want Replayed", replayed)
	}
	if replayed.ExecutionID != retried.ExecutionID || replayed.LifecycleVersion != retried.LifecycleVersion {
		t.Fatalf("retry replay = %+v; want the original %+v", replayed, retried)
	}
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"task_id": string(created.TaskID)}); count != 2 {
		t.Fatalf("executions after replay = %d; want the original start and retry only", count)
	}

	// A different caller key at the same state is a fresh operator start and
	// must still fail closed: the current execution is queued, not failed.
	otherCommand := retryCommand
	otherCommand.CallerKey = "task-operator-retry-other"
	_, err = env.service.Start(ctx, otherCommand)
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)

	// A same-key replay with changed content is a content conflict, not a
	// forbidden fresh start.
	changedCommand := replayCommand
	changedCommand.Content = integrationContent("task-operator-retry-changed")
	_, err = env.service.Start(ctx, changedCommand)
	mustClass(t, err, fault.ChangedIdempotencyContent)
	env.fixture.requireUnchanged(t, env.scope, before)

	// The replay still re-confirms the operator's current membership: revoking
	// it refuses the replay with nothing written.
	env.setMembershipRows(t, env.scope, "membership-operator", 1, tenancy.RoleViewer)
	_, err = env.service.Start(ctx, replayCommand)
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)
}

// TestRealTaskServiceReplyRefusesDemotedOperatorInitiator proves the reply unit's
// initiator start read cannot be satisfied by the conditional operator grant.
// A reply always creates a fresh execution, so the failed-task retry condition
// can never apply to the initiator. The service's pre-admission check saw the
// initiator as an engineer; the commit-boundary re-evaluation sees an
// operator-only membership and must refuse forbidden with nothing written.
func TestRealTaskServiceReplyRefusesDemotedOperatorInitiator(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	engineer := env.registerHuman(t, "principal-engineer", tenancy.RoleEngineer)
	env.fixture.seedMembership(t, env.scope, "membership-engineer", "principal-engineer", 1, tenancy.RoleEngineer)
	reviewer := env.registerHuman(t, "principal-reviewer", tenancy.RoleReviewer)
	env.fixture.seedMembership(t, env.scope, "membership-reviewer", "principal-reviewer", 1, tenancy.RoleReviewer)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: engineer, Scope: env.scope, Title: "Fix the build", PipelineID: testPipeline,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	if _, err := env.service.Start(ctx, task.StartCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	}); err != nil {
		t.Fatalf("task start: %v", err)
	}
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor: engineer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Which base?", Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		CallerKey: "task-ask", Content: integrationContent("task-ask"),
	}); err != nil {
		t.Fatalf("task ask: %v", err)
	}
	current, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read waiting task: %v", err)
	}
	if current.Question == nil || current.Initiator != "principal-engineer" {
		t.Fatalf("waiting task = %+v; want a question with the engineer initiator", current)
	}
	// The initiator is demoted after the service's principal read and before the
	// reply commit. The conditional operator grant must not admit a fresh reply
	// execution.
	env.setMembershipRows(t, env.scope, "membership-engineer", 1, tenancy.RoleOperator)
	before := env.fixture.commitCounts(t, env.scope)

	_, err = env.service.Reply(ctx, task.ReplyCommand{
		Actor: reviewer, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version, Answer: "main",
		CallerKey: "task-reply", Content: integrationContent("task-reply"),
	})
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"task_id": string(created.TaskID)}); count != 1 {
		t.Fatalf("a refused reply committed %d executions; want the original start only", count)
	}
	after, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("re-read waiting task: %v", err)
	}
	if after.State != task.StateWaiting || after.Question == nil || after.Question.ID != current.Question.ID {
		t.Fatalf("a refused reply changed the waiting task: %+v", after)
	}
}

// racingExecutor commits a concurrent identical retry at the exact moment the
// conditional operator read re-reads the task row. It counts task reads so the
// pre-flight receipt lookup and the earlier history read see the original state.
type racingExecutor struct {
	inner        store.Executor
	store        *memory.Store
	scope        tenancy.Scope
	record       receipt.Record
	taskID       id.TaskID
	newExecution id.ExecutionID
	taskReads    int
	fired        bool
}

func (executor *racingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	return executor.inner.Execute(ctx, scope, unit)
}

func (executor *racingExecutor) Read(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	if table == "tasks" && identity == string(executor.taskID) {
		executor.taskReads++
		if executor.taskReads == 2 && !executor.fired {
			executor.fired = true
			record := executor.record
			if _, err := executor.store.Execute(context.Background(), executor.scope, store.Unit{
				Writes: []store.Write{{
					Table:  "tasks",
					ID:     string(executor.taskID),
					Expect: map[string]any{"lifecycle_version": int64(2)},
					Values: map[string]any{"current_execution_id": string(executor.newExecution)},
				}},
				Receipt: &record,
			}); err != nil {
				return store.Row{}, err
			}
		}
	}
	return executor.inner.Read(ctx, scope, table, identity)
}

// TestOperatorStartReplaysAConcurrentIdenticalRetry proves the conditional
// operator read cannot turn a concurrent duplicate into a forbidden refusal.
// A second identical retry can commit between the pre-flight receipt lookup and
// the failed-task re-read; when the conditional evaluation then fails because
// the task moved, the adapter re-checks the receipt and serves the replay
// instead of refusing the lost response.
func TestOperatorStartReplaysAConcurrentIdenticalRetry(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)

	record := fixture.receipt("task.start", "operator-race", contentHashA)
	resultJSON, err := json.Marshal(task.StartResult{ExecutionID: "execution-retry", LifecycleVersion: 3})
	if err != nil {
		t.Fatalf("marshal receipt result: %v", err)
	}
	record.Result = resultJSON
	racing := &racingExecutor{
		inner: fixture.store, store: fixture.store, scope: scope, record: record,
		taskID: testTask, newExecution: "execution-retry",
	}
	adapter, err := NewTask(racing, fixture.store)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	unit := fixture.startUnit(scope, testTask, "execution-retry", 2, 1, record)
	unit.Initiator = testPrincipal
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "task.start", testPrincipal, operatorRequires()),
		operatorStartRead(scope, testPrincipal, testExecution, 3),
	}

	replayed, err := adapter.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("concurrent identical retry: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionID != "execution-retry" || replayed.LifecycleVersion != 3 {
		t.Fatalf("concurrent retry result = %+v; want the original replay", replayed)
	}
	if !racing.fired {
		t.Fatal("the concurrent retry did not commit during the conditional re-read")
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-retry"); ok {
		t.Fatal("a replayed retry committed a second execution row")
	}
}
