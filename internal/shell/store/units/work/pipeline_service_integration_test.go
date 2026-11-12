package work

// These tests drive the real pipeline management service and the real trigger
// processor onto the work adapters over the memory store.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// TestRealPipelineManagementCommitsThroughAdapters drives apply-new, update,
// delete and a push-trigger apply. A new apply's pipeline_version read carries
// no target and no expected version; the push apply must satisfy the Admin read.
func TestRealPipelineManagementCommitsThroughAdapters(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply-new", Content: integrationContent("pipeline-apply-new"),
	})
	if err != nil {
		t.Fatalf("pipeline apply new: %v", err)
	}
	if applied.Version != 1 {
		t.Fatalf("applied version = %d; want 1", applied.Version)
	}
	updated, err := env.management.Update(ctx, pipeline.UpdateCommand{
		Actor: actor, Scope: env.scope, PipelineID: applied.PipelineID, Name: "build",
		YAML: integrationPipelineYAML("echo v2"), ExpectedVersion: 1,
		CallerKey: "pipeline-update", Content: integrationContent("pipeline-update"),
	})
	if err != nil {
		t.Fatalf("pipeline update: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated version = %d; want 2", updated.Version)
	}
	deleted, err := env.management.Delete(ctx, pipeline.DeleteCommand{
		Actor: actor, Scope: env.scope, PipelineID: applied.PipelineID, ExpectedVersion: 2,
		CallerKey: "pipeline-delete", Content: integrationContent("pipeline-delete"),
	})
	if err != nil {
		t.Fatalf("pipeline delete: %v", err)
	}
	if deleted.Version != 3 {
		t.Fatalf("deleted version = %d; want 3", deleted.Version)
	}
	if _, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(applied.PipelineID)); ok {
		t.Fatal("pipeline delete left the row behind")
	}
	pushed, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "push", YAML: integrationPushYAML(),
		CallerKey: "pipeline-apply-push", Content: integrationContent("pipeline-apply-push"),
	})
	if err != nil {
		t.Fatalf("push pipeline apply: %v", err)
	}
	row, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(pushed.PipelineID))
	if !ok {
		t.Fatal("push pipeline apply committed no row")
	}
	if len(bytesValue(row, "push_triggers")) == 0 {
		t.Fatal("push pipeline apply persisted no push projection")
	}
}

// TestRealPipelineManagementStaleVersionConflicts proves a stale service-built
// update is refused with nothing persisted.
func TestRealPipelineManagementStaleVersionConflicts(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply-new", Content: integrationContent("pipeline-apply-new"),
	})
	if err != nil {
		t.Fatalf("pipeline apply new: %v", err)
	}
	before := env.fixture.commitCounts(t, env.scope)
	_, err = env.management.Update(ctx, pipeline.UpdateCommand{
		Actor: actor, Scope: env.scope, PipelineID: applied.PipelineID, Name: "build",
		YAML: integrationPipelineYAML("echo stale"), ExpectedVersion: 2,
		CallerKey: "pipeline-update-stale", Content: integrationContent("pipeline-update-stale"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.fixture.requireUnchanged(t, env.scope, before)
	row, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(applied.PipelineID))
	if !ok || versionValue(row, "version") != 1 {
		t.Fatalf("a refused stale update changed the pipeline: %+v", row.Values)
	}
	if string(bytesValue(row, "yaml")) == string(integrationPipelineYAML("echo stale")) {
		t.Fatal("a refused stale update persisted the stale yaml")
	}
}

// TestRealTriggerProcessorCommitsThroughAdapters drives the real trigger
// processor through its real Select and Start adapters. Its automatic-service
// reads include service_enabled and binding_version shapes. The repository is
// bound to a Git service that is not the pipeline's automatic-start service,
// and the event is a human push report in the real ingress shape.
func TestRealTriggerProcessorCommitsThroughAdapters(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedServiceWithRoles(t, env.scope, "service-1", serviceStateEnabled, 1, tenancy.RoleEngineer)
	env.fixture.seedServiceWithRoles(t, env.scope, integrationGitService, serviceStateEnabled, 1, tenancy.RoleViewer)
	env.fixture.seedBinding(t, env.scope, "binding-1", "repo-1", integrationGitService, 1)
	if _, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "push", YAML: integrationPushYAML(),
		CallerKey: "pipeline-apply-push", Content: integrationContent("pipeline-apply-push"),
	}); err != nil {
		t.Fatalf("push pipeline apply: %v", err)
	}
	completion := &fakeCompletionPort{}
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         fakePendingEvents{rows: []event.PendingEvent{{Sequence: 1, Event: integrationPushEvent(env.scope)}}},
		Candidates:     env.fixture.pipelines,
		Selections:     env.fixture.triggers,
		Select:         env.fixture.triggers,
		Admission:      integrationAdmission{pipelines: env.fixture.pipelines, scope: env.scope},
		Start:          env.fixture.triggers,
		Completion:     completion,
		Clock:          integrationClock{now: testNow},
		IDs:            env.ids,
		BatchSize:      10,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("trigger.NewProcessor: %v", err)
	}
	result, err := processor.Process(ctx)
	if err != nil {
		t.Fatalf("trigger process: %v", err)
	}
	if len(result.Started) != 1 {
		t.Fatalf("trigger started %d executions; want 1", len(result.Started))
	}
	if _, ok := env.fixture.storedRow(t, env.scope, "executions", string(result.Started[0])); !ok {
		t.Fatal("trigger start committed no execution")
	}
	if len(completion.completed) != 1 {
		t.Fatalf("trigger completed %d events; want 1", len(completion.completed))
	}
}

// TestRealTriggerProcessorCompletesAfterOrdinaryStartAndUnmatchedReport is the
// regression for the trigger scanner mistaking a completed ordinary
// pipeline.start receipt for an unfinished push selection. It drives the real
// processor onto the real Select and Start adapters with one ordinary inline
// start receipt, an earlier human report whose ref no trigger declares, a
// matching human report and a later execution-originated report. Both accepted
// reports must complete, exactly one execution must start, a replay must start
// none, and processing must reach the later event. Completion is asserted
// explicitly because a first matching push can start before the final
// Unfinished scan and hide the poison.
func TestRealTriggerProcessorCompletesAfterOrdinaryStartAndUnmatchedReport(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedServiceWithRoles(t, env.scope, "service-1", serviceStateEnabled, 1, tenancy.RoleEngineer)
	env.fixture.seedServiceWithRoles(t, env.scope, integrationGitService, serviceStateEnabled, 1, tenancy.RoleViewer)
	env.fixture.seedBinding(t, env.scope, "binding-1", "repo-1", integrationGitService, 1)
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "push", YAML: integrationPushYAML(),
		CallerKey: "pipeline-apply-push", Content: integrationContent("pipeline-apply-push"),
	})
	if err != nil {
		t.Fatalf("push pipeline apply: %v", err)
	}
	// The ordinary start's completed receipt is the exact production shape.
	env.fixture.seedManualStart(t, env.scope, "manual-inline-processor", "", 0)

	unmatched := integrationPushEvent(env.scope)
	unmatched.ID = "event-unmatched"
	unmatched.Data = []byte(`{"candidate":"1111111","expected_base":"0000000","operation":"git.ref.update",` +
		`"ref":"refs/heads/not-subscribed","resource":"repo-1"}`)
	matching := integrationPushEvent(env.scope)
	after := integrationPushEvent(env.scope)
	after.ID = "event-after"
	after.CorrelationID = "execution-origin"
	completion := &fakeCompletionPort{}
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events: fakePendingEvents{rows: []event.PendingEvent{
			{Sequence: 1, Event: unmatched},
			{Sequence: 2, Event: matching},
			{Sequence: 3, Event: after},
		}},
		Candidates:     env.fixture.pipelines,
		Selections:     env.fixture.triggers,
		Select:         env.fixture.triggers,
		Admission:      integrationAdmission{pipelines: env.fixture.pipelines, scope: env.scope},
		Start:          env.fixture.triggers,
		Completion:     completion,
		Clock:          integrationClock{now: testNow},
		IDs:            env.ids,
		BatchSize:      10,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("trigger.NewProcessor: %v", err)
	}
	result, err := processor.Process(ctx)
	if err != nil {
		t.Fatalf("trigger process: %v", err)
	}
	if len(result.Started) != 1 {
		t.Fatalf("trigger started %d executions; want 1", len(result.Started))
	}
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"pipeline_id": string(applied.PipelineID)}); count != 1 {
		t.Fatalf("push pipeline executions = %d; want exactly 1", count)
	}
	if _, ok := env.fixture.storedRow(t, env.scope, "executions", string(result.Started[0])); !ok {
		t.Fatal("trigger start committed no execution")
	}
	// Both accepted reports completed and processing reached the later event.
	// The first matching push would start even while its event stayed deferred,
	// so the completed set is the load-bearing assertion.
	wantCompleted := []event.Sequence{1, 2, 3}
	if len(result.Completed) != len(wantCompleted) {
		t.Fatalf("completed events = %v; want %v", result.Completed, wantCompleted)
	}
	for index, sequence := range wantCompleted {
		if result.Completed[index] != sequence {
			t.Fatalf("completed events = %v; want %v", result.Completed, wantCompleted)
		}
	}
	if len(completion.completed) != len(wantCompleted) {
		t.Fatalf("completion markers = %d; want %d", len(completion.completed), len(wantCompleted))
	}
	// A replay rescans from zero: the completed selection and the
	// execution-originated exclusion start nothing new.
	replay, err := processor.Process(ctx)
	if err != nil {
		t.Fatalf("trigger replay: %v", err)
	}
	if len(replay.Started) != 1 || replay.Started[0] != result.Started[0] {
		t.Fatalf("replay started = %v; want the original execution %q", replay.Started, result.Started[0])
	}
	if count := env.fixture.countRows(t, env.scope, "executions", map[string]any{"pipeline_id": string(applied.PipelineID)}); count != 1 {
		t.Fatalf("a replay committed %d push executions; want 1", count)
	}
}

// The tests below cover pipeline deletion: a pipeline that tasks and
// executions name can be deleted, the history rows keep their pipeline_id, and
// a deleted pipeline admits nothing new.

// applyHistoryPipeline applies one pipeline through the real management
// service and returns its identity at version 1.
func applyHistoryPipeline(t *testing.T, env *realServiceEnv) pipeline.PipelineID {
	t.Helper()
	applied, err := env.management.Apply(context.Background(), pipeline.ApplyCommand{
		Actor: env.actor(), Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	if applied.Version != 1 {
		t.Fatalf("applied version = %d; want 1", applied.Version)
	}
	return applied.PipelineID
}

// deleteHistoryPipeline deletes the pipeline at version 1 through the real
// management service.
func deleteHistoryPipeline(t *testing.T, env *realServiceEnv, pipelineID pipeline.PipelineID) {
	t.Helper()
	deleted, err := env.management.Delete(context.Background(), pipeline.DeleteCommand{
		Actor: env.actor(), Scope: env.scope, PipelineID: pipelineID, ExpectedVersion: 1,
		CallerKey: "pipeline-delete", Content: integrationContent("pipeline-delete"),
	})
	if err != nil {
		t.Fatalf("pipeline delete of a pipeline with history: %v", err)
	}
	if deleted.Replayed {
		t.Fatal("first pipeline delete reported a replay")
	}
	if _, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(pipelineID)); ok {
		t.Fatal("pipeline delete left the row behind")
	}
}

// historyRowCounts counts every row family a refused task operation could
// leave behind.
type historyRowCounts struct {
	commits    commitCounts
	tasks      int
	messages   int
	executions int
	actions    int
}

func (env *realServiceEnv) historyRowCounts(t *testing.T) historyRowCounts {
	t.Helper()
	return historyRowCounts{
		commits:    env.fixture.commitCounts(t, env.scope),
		tasks:      env.fixture.countRows(t, env.scope, "tasks", nil),
		messages:   env.fixture.countRows(t, env.scope, "task_messages", nil),
		executions: env.fixture.countRows(t, env.scope, "executions", nil),
		actions:    env.fixture.countRows(t, env.scope, "actions", nil),
	}
}

// storedExecution reads one committed execution row.
func (env *realServiceEnv) storedExecution(t *testing.T, executionID id.ExecutionID) store.Row {
	t.Helper()
	row, ok := env.fixture.storedRow(t, env.scope, "executions", string(executionID))
	if !ok {
		t.Fatalf("execution %s is missing", executionID)
	}
	return row
}

func (env *realServiceEnv) requireNoHistoryChange(t *testing.T, before historyRowCounts, label string) {
	t.Helper()
	if after := env.historyRowCounts(t); after != before {
		t.Fatalf("%s changed rows: before %+v, after %+v", label, before, after)
	}
}

// TestRealPipelineDeleteKeepsTaskAndExecutionHistory deletes a pipeline that a
// waiting task, an open task, a cancelled execution and a queued execution all
// name. The delete succeeds, every history row keeps its pipeline_id, the
// queued execution still loads from its immutable snapshot, every read still
// works, and start and reply against the deleted selection are refused
// not_found with nothing written.
func TestRealPipelineDeleteKeepsTaskAndExecutionHistory(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	pipelineID := applyHistoryPipeline(t, env)

	// A started task that then asks: its first execution is cancelled and the
	// task waits for a reply that would admit from the same pipeline.
	waiting, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Waiting", PipelineID: pipelineID, Start: true,
		CallerKey: "create-waiting", Content: integrationContent("create-waiting"),
	})
	if err != nil {
		t.Fatalf("create-and-start: %v", err)
	}
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: waiting.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Which branch?", Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		CallerKey: "ask-waiting", Content: integrationContent("ask-waiting"),
	}); err != nil {
		t.Fatalf("task ask: %v", err)
	}
	// A task with a queued execution admitted from the pipeline.
	running, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Running", PipelineID: pipelineID, Start: true,
		CallerKey: "create-running", Content: integrationContent("create-running"),
	})
	if err != nil {
		t.Fatalf("create-and-start: %v", err)
	}
	// A task that only selects the pipeline and never started.
	selected, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Selected", PipelineID: pipelineID,
		CallerKey: "create-selected", Content: integrationContent("create-selected"),
	})
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	executions := executionstore.New(env.fixture.store, env.fixture.store)
	snapshotBefore := bytesValue(env.storedExecution(t, running.ExecutionID), "snapshot")

	deleteHistoryPipeline(t, env, pipelineID)

	for _, taskID := range []id.TaskID{waiting.TaskID, running.TaskID, selected.TaskID} {
		row := env.fixture.storedTask(t, env.scope, taskID)
		if textValue(row, "pipeline_id") != string(pipelineID) {
			t.Fatalf("task %s pipeline_id = %q; want the deleted pipeline %q", taskID, textValue(row, "pipeline_id"), pipelineID)
		}
	}
	for _, executionID := range []id.ExecutionID{waiting.ExecutionID, running.ExecutionID} {
		row := env.storedExecution(t, executionID)
		if textValue(row, "pipeline_id") != string(pipelineID) {
			t.Fatalf("execution %s pipeline_id = %q; want the deleted pipeline %q", executionID, textValue(row, "pipeline_id"), pipelineID)
		}
	}

	// The admitted execution is self-contained: the aggregate, its snapshot and
	// its actions load without the pipeline row.
	queued := env.storedExecution(t, running.ExecutionID)
	if textValue(queued, "state") != string(kernel.ExecutionQueued) {
		t.Fatalf("admitted execution state = %q; want it still queued", textValue(queued, "state"))
	}
	if string(bytesValue(queued, "snapshot")) != string(snapshotBefore) {
		t.Fatal("the pipeline delete changed the admitted snapshot")
	}
	admitted, err := snapshotdoc.Decode(bytesValue(queued, "snapshot"))
	if err != nil {
		t.Fatalf("decode admitted snapshot: %v", err)
	}
	if string(admitted.Snapshot.YAMLBytes()) != string(integrationPipelineYAML("echo v1")) {
		t.Fatalf("admitted snapshot yaml = %q; want the deleted pipeline's bytes", admitted.Snapshot.YAMLBytes())
	}
	aggregate, err := executions.Load(ctx, env.scope, running.ExecutionID)
	if err != nil {
		t.Fatalf("load admitted execution after the delete: %v", err)
	}
	if aggregate.State != kernel.ExecutionQueued || len(aggregate.Actions) != 1 {
		t.Fatalf("admitted aggregate = state %q with %d actions; want queued with its one action", aggregate.State, len(aggregate.Actions))
	}

	// Every read of the history still works.
	view, err := executions.Inspect(ctx, env.scope, running.ExecutionID)
	if err != nil {
		t.Fatalf("execution inspect after the delete: %v", err)
	}
	if view.PipelineID != pipelineID || view.TaskID != running.TaskID {
		t.Fatalf("inspected execution = pipeline %q task %q; want %q/%q", view.PipelineID, view.TaskID, pipelineID, running.TaskID)
	}
	page, err := executions.List(ctx, env.scope, execution.ListFilter{PipelineID: pipelineID})
	if err != nil {
		t.Fatalf("execution list after the delete: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("executions listed for the deleted pipeline = %d; want 2", len(page.Items))
	}
	read, err := env.fixture.task.Get(ctx, env.scope, selected.TaskID)
	if err != nil {
		t.Fatalf("task read after the delete: %v", err)
	}
	if read.SelectedPipeline != pipelineID {
		t.Fatalf("read task selected pipeline = %q; want %q", read.SelectedPipeline, pipelineID)
	}
	history, err := env.fixture.task.History(ctx, env.scope, waiting.TaskID, task.HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatalf("task history after the delete: %v", err)
	}
	if len(history.Entries) == 0 {
		t.Fatal("task history after the delete is empty")
	}
	assigned, err := env.fixture.inbox.Query(ctx, inbox.Query{Scope: env.scope, Principal: env.principals.principals[testPrincipal], Limit: 10})
	if err != nil {
		t.Fatalf("inbox query after the delete: %v", err)
	}
	foundWaiting := false
	for _, item := range assigned.Items {
		if item.Kind == inbox.KindWaitingTask && item.TaskID == waiting.TaskID {
			foundWaiting = true
		}
	}
	if !foundWaiting {
		t.Fatalf("inbox after the delete lost the waiting task: %+v", assigned.Items)
	}

	// Nothing new is admitted from the deleted pipeline, and a refusal writes
	// nothing.
	before := env.historyRowCounts(t)
	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: selected.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "start-selected", Content: integrationContent("start-selected"),
	})
	mustClass(t, err, fault.NotFound)
	env.requireNoHistoryChange(t, before, "a refused start")

	current, err := env.fixture.task.Get(ctx, env.scope, waiting.TaskID)
	if err != nil || current.Question == nil {
		t.Fatalf("read waiting task: question %v, err %v", current.Question, err)
	}
	_, err = env.service.Reply(ctx, task.ReplyCommand{
		Actor: actor, Scope: env.scope, TaskID: waiting.TaskID, ExpectedLifecycleVersion: current.LifecycleVersion,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version, Answer: "main",
		CallerKey: "reply-waiting", Content: integrationContent("reply-waiting"),
	})
	mustClass(t, err, fault.NotFound)
	env.requireNoHistoryChange(t, before, "a refused reply")
	if row := env.fixture.storedTask(t, env.scope, waiting.TaskID); textValue(row, "state") != string(task.StateWaiting) {
		t.Fatalf("waiting task state after the refused reply = %q; want it still waiting", textValue(row, "state"))
	}

	_, err = env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Again", PipelineID: pipelineID, Start: true,
		CallerKey: "create-and-start-deleted", Content: integrationContent("create-and-start-deleted"),
	})
	mustClass(t, err, fault.NotFound)
	env.requireNoHistoryChange(t, before, "a refused create-and-start")
}

// TestRealTaskStartRacingPipelineDeleteWritesNothing interleaves a committed
// pipeline delete between the start's admission read and its commit. The
// start's guarded pipeline lock finds no row and the whole unit is refused.
func TestRealTaskStartRacingPipelineDeleteWritesNothing(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	pipelineID := applyHistoryPipeline(t, env)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Racing", PipelineID: pipelineID,
		CallerKey: "create-racing", Content: integrationContent("create-racing"),
	})
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	var before historyRowCounts
	env.ports.setHook(func() {
		deleteHistoryPipeline(t, env, pipelineID)
		before = env.historyRowCounts(t)
	})
	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "start-racing", Content: integrationContent("start-racing"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.requireNoHistoryChange(t, before, "a start that lost the race with the delete")
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if versionValue(row, "lifecycle_version") != 1 || textValue(row, "current_execution_id") != "" {
		t.Fatalf("task after the refused start = lifecycle %d current %q; want it untouched",
			versionValue(row, "lifecycle_version"), textValue(row, "current_execution_id"))
	}
}

// TestRealTaskCreateRefusesUnknownPipeline proves the plain create guard: a
// selection that is unknown, or that exists only in another project, is
// not_found and leaves no task, receipt, audit or event row.
func TestRealTaskCreateRefusesUnknownPipeline(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, otherScope(), "pipeline-foreign", "build", integrationPipelineYAML("echo v1"), 1)
	before := env.historyRowCounts(t)

	for _, selection := range []pipeline.PipelineID{"pipeline-unknown", "pipeline-foreign"} {
		_, err := env.service.Create(ctx, task.CreateCommand{
			Actor: actor, Scope: env.scope, Title: "Refused", PipelineID: selection,
			CallerKey: "create-" + string(selection), Content: integrationContent("create-" + string(selection)),
		})
		mustClass(t, err, fault.NotFound)
		env.requireNoHistoryChange(t, before, "a refused create selecting "+string(selection))
	}

	// A create without a selection, and one selecting a known pipeline, commit.
	pipelineID := applyHistoryPipeline(t, env)
	if _, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "No selection",
		CallerKey: "create-plain", Content: integrationContent("create-plain"),
	}); err != nil {
		t.Fatalf("create without a pipeline: %v", err)
	}
	known, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Known", PipelineID: pipelineID,
		CallerKey: "create-known", Content: integrationContent("create-known"),
	})
	if err != nil {
		t.Fatalf("create selecting a known pipeline: %v", err)
	}
	if row := env.fixture.storedTask(t, env.scope, known.TaskID); textValue(row, "pipeline_id") != string(pipelineID) {
		t.Fatalf("created task pipeline_id = %q; want %q", textValue(row, "pipeline_id"), pipelineID)
	}
}

// TestRealTaskCreateReplaysAfterPipelineDelete proves a lost-response retry of
// a committed create still converges on its receipt after the selected
// pipeline was deleted, while a fresh create naming it is refused.
func TestRealTaskCreateReplaysAfterPipelineDelete(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	pipelineID := applyHistoryPipeline(t, env)
	command := task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Lost response", PipelineID: pipelineID,
		CallerKey: "create-lost", Content: integrationContent("create-lost"),
	}
	first, err := env.service.Create(ctx, command)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	deleteHistoryPipeline(t, env, pipelineID)
	before := env.historyRowCounts(t)

	retried, err := env.service.Create(ctx, command)
	if err != nil {
		t.Fatalf("retried create after the pipeline delete: %v", err)
	}
	if !retried.Replayed || retried.TaskID != first.TaskID {
		t.Fatalf("retried create = %+v; want the replayed original task %q", retried, first.TaskID)
	}
	env.requireNoHistoryChange(t, before, "a replayed create")

	changed := command
	changed.Content = integrationContent("create-lost-changed")
	_, err = env.service.Create(ctx, changed)
	mustClass(t, err, fault.ChangedIdempotencyContent)

	fresh := command
	fresh.CallerKey = "create-fresh"
	fresh.Content = integrationContent("create-fresh")
	_, err = env.service.Create(ctx, fresh)
	mustClass(t, err, fault.NotFound)
	env.requireNoHistoryChange(t, before, "a refused fresh create")
}

// TestRealTaskCreateRacingPipelineDeleteWritesNothing interleaves a committed
// pipeline delete between the create's scoped pipeline read and its commit.
// The unit pins the selected pipeline row, so the create is refused whole.
func TestRealTaskCreateRacingPipelineDeleteWritesNothing(t *testing.T) {
	executor := &mutatingExecutor{}
	env := newRealServiceEnvWithExecutor(t, func(inner store.Executor) store.Executor {
		executor.inner = inner
		return executor
	})
	ctx := context.Background()
	actor := env.actor()
	pipelineID := applyHistoryPipeline(t, env)
	var before historyRowCounts
	executor.hook = func() {
		deleteHistoryPipeline(t, env, pipelineID)
		before = env.historyRowCounts(t)
	}
	_, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Racing", PipelineID: pipelineID,
		CallerKey: "create-racing", Content: integrationContent("create-racing"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.requireNoHistoryChange(t, before, "a create that lost the race with the delete")
}

// TestTaskCreatePinsSelectedPipelineRow drives the create port directly: a
// plain creation unit naming an absent pipeline is refused before any write.
func TestTaskCreatePinsSelectedPipelineRow(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	unit := task.CreateUnit{
		Owner:            scope,
		TaskID:           testTask,
		Title:            "Fix the build",
		SelectedPipeline: testPipeline,
		Initiator:        testPrincipal,
		At:               testNow,
		Receipt:          fixture.receiptFor(scope, "task.create", "create-pinned", contentHashA),
	}
	_, err := fixture.task.Create(context.Background(), unit)
	mustClass(t, err, fault.VersionConflict)
	if _, ok := fixture.storedRow(t, scope, "tasks", testTask); ok {
		t.Fatal("a refused create left a task row")
	}
	if count := fixture.countRows(t, scope, "receipts", nil); count != 0 {
		t.Fatalf("a refused create left %d receipts", count)
	}

	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	created, err := fixture.task.Create(context.Background(), unit)
	if err != nil {
		t.Fatalf("create selecting a present pipeline: %v", err)
	}
	if created.Replayed || created.TaskID != testTask {
		t.Fatalf("create result = %+v; want the fresh task", created)
	}
	if row := fixture.storedTask(t, scope, testTask); textValue(row, "pipeline_id") != testPipeline {
		t.Fatalf("created task pipeline_id = %q; want %q", textValue(row, "pipeline_id"), testPipeline)
	}
}

// hookedPipelineChanges runs one test hook after the management service read
// the pipeline by name and built its unit, immediately before the unit commits.
type hookedPipelineChanges struct {
	inner pipeline.ChangePort
	hook  func()
}

func (changes *hookedPipelineChanges) Change(ctx context.Context, unit pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
	if changes.hook != nil {
		hook := changes.hook
		changes.hook = nil
		hook()
	}
	return changes.inner.Change(ctx, unit)
}

// TestRealPipelineManagementRefusesADuplicateName proves the memory store
// refuses a second pipeline with a taken name in one project with the same
// version_conflict the PostgreSQL pipelines_scope_name constraint returns.
//
// A sequential Apply of an existing name never reaches the constraint: the
// service reads the pipeline by name and replaces it. The two shapes that do
// reach it are an Update that renames a pipeline onto another's name and an
// Apply that read the name absent and lost the race to another Apply.
func TestRealPipelineManagementRefusesADuplicateName(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	apply := func(management *pipeline.Management, name, callerKey string) (pipeline.ChangeResult, error) {
		return management.Apply(ctx, pipeline.ApplyCommand{
			Actor: actor, Scope: env.scope, Name: name, YAML: integrationPipelineYAML("echo " + callerKey),
			CallerKey: callerKey, Content: integrationContent(callerKey),
		})
	}
	build, err := apply(env.management, "build", "apply-build")
	if err != nil {
		t.Fatalf("apply build: %v", err)
	}
	replaced, err := apply(env.management, "build", "apply-build-again")
	if err != nil {
		t.Fatalf("sequential apply of an existing name: %v", err)
	}
	if replaced.PipelineID != build.PipelineID || replaced.Version != 2 {
		t.Fatalf("sequential apply = %+v; want pipeline %q replaced at version 2", replaced, build.PipelineID)
	}
	deploy, err := apply(env.management, "deploy", "apply-deploy")
	if err != nil {
		t.Fatalf("apply deploy: %v", err)
	}

	before := env.fixture.commitCounts(t, env.scope)
	_, err = env.management.Update(ctx, pipeline.UpdateCommand{
		Actor: actor, Scope: env.scope, PipelineID: deploy.PipelineID, Name: "build",
		YAML: integrationPipelineYAML("echo renamed"), ExpectedVersion: 1,
		CallerKey: "rename-deploy", Content: integrationContent("rename-deploy"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.fixture.requireUnchanged(t, env.scope, before)
	row, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(deploy.PipelineID))
	if !ok || textValue(row, "name") != "deploy" || versionValue(row, "version") != 1 {
		t.Fatalf("a refused rename changed the pipeline: %+v", row.Values)
	}

	// The losing Apply read "release" absent, so its unit inserts a new row
	// after the winner committed the same name.
	changes := &hookedPipelineChanges{inner: env.fixture.pipelines}
	racing, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      integrationClock{now: testNow},
		IDs:        env.ids,
		Authorizer: allowAuthorizer{},
		Authority:  confirmAuthority{},
		Changes:    changes,
		Reader:     env.fixture.pipelines,
	})
	if err != nil {
		t.Fatalf("pipeline.NewManagement: %v", err)
	}
	var winner pipeline.ChangeResult
	changes.hook = func() {
		var err error
		winner, err = apply(env.management, "release", "apply-release-winner")
		if err != nil {
			t.Errorf("winning apply: %v", err)
		}
		before = env.fixture.commitCounts(t, env.scope)
	}
	_, err = apply(racing, "release", "apply-release-loser")
	mustClass(t, err, fault.VersionConflict)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "pipelines", map[string]any{"name": "release"}); count != 1 {
		t.Fatalf("pipelines named release = %d; want only the winner", count)
	}
	if _, ok := env.fixture.storedRow(t, env.scope, "pipelines", string(winner.PipelineID)); !ok {
		t.Fatal("the winning apply's pipeline is missing")
	}

	// The same name in another project is a different key.
	env.fixture.seedPipeline(t, otherScope(), "pipeline-foreign", "build", integrationPipelineYAML("echo v1"), 1)
}
