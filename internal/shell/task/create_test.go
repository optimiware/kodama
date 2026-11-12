package task_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func createCommand(actor authorize.Principal, title string) task.CreateCommand {
	return task.CreateCommand{
		Actor:       actor,
		Scope:       actor.Scope,
		Title:       title,
		Description: "Markdown details",
		Metadata:    []byte(`{"stage":"review","priority":"high"}`),
		PipelineID:  pipelineID,
		Inputs:      map[id.InputKey]string{"repository": "example/repo"},
		CallerKey:   "caller-create",
		Content:     receiptContent(`{"title":"` + title + `"}`),
	}
}

func TestCreateBuildsOpenTaskWithoutStarting(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Investigate failure")
	// Metadata is opaque userland state. These keys would look like a lifecycle
	// instruction to an implementation that interpreted them.
	command.Metadata = []byte(`{"stage":"done","kodama.state":"cancelled","priority":"high"}`)

	result, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "Create")

	if result.TaskID == "" {
		t.Fatal("create result has no task id")
	}
	if result.ExecutionID != "" {
		t.Fatalf("create result execution id = %q; want none", result.ExecutionID)
	}
	if result.Replayed {
		t.Fatal("first create reported a replay")
	}
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d; create must start nothing", env.admission.callCount())
	}
	if len(env.ports.starts) != 0 || len(env.ports.replies) != 0 {
		t.Fatalf("start/reply ports called during create")
	}

	unit := env.ports.lastCreate(t)
	if unit.Owner != actor.Scope {
		t.Fatalf("unit owner = %+v; want %+v", unit.Owner, actor.Scope)
	}
	if unit.TaskID != result.TaskID {
		t.Fatalf("unit task id = %q; want result %q", unit.TaskID, result.TaskID)
	}
	if unit.Title != "Investigate failure" || unit.Description != "Markdown details" {
		t.Fatalf("unit content = %q/%q", unit.Title, unit.Description)
	}
	if unit.Start != nil {
		t.Fatalf("plain create carries a start unit: %+v", unit.Start)
	}
	if unit.SelectedPipeline != pipelineID {
		t.Fatalf("unit selected pipeline = %q; want %q", unit.SelectedPipeline, pipelineID)
	}
	if unit.Inputs["repository"] != "example/repo" {
		t.Fatalf("unit inputs = %+v", unit.Inputs)
	}
	if unit.Initiator != actor.ID {
		t.Fatalf("unit initiator = %q; want %q", unit.Initiator, actor.ID)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", unit.At, testNow)
	}
	// Metadata is stored verbatim and never interpreted: the "done"/"cancelled"
	// values did not start or settle anything.
	if unit.Metadata["stage"] != "done" || unit.Metadata["kodama.state"] != "cancelled" || unit.Metadata["priority"] != "high" {
		t.Fatalf("unit metadata = %+v; want the caller's exact object", unit.Metadata)
	}
	if len(unit.Metadata) != 3 {
		t.Fatalf("unit metadata = %+v; want exactly three entries", unit.Metadata)
	}

	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.create", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.create", testNow)
	requireEvent(t, unit.Events, event.TypeTaskCreated, actor.Scope, string(result.TaskID), 1, testNow)
	requireNoEvent(t, unit.Events, event.TypeExecutionQueued)
	requirePublish(t, unit.PostCommit)
	requireNoDispatch(t, unit.PostCommit)

	membership := requireAuthority(t, unit.Authority, "task.create", authorize.CheckMembership)
	if membership.Scope != actor.Scope || membership.PrincipalID != actor.ID {
		t.Fatalf("membership read = %+v", membership)
	}
	requireNoAuthority(t, unit.Authority, authorize.CheckPipelineVersion)

	calls := env.authorizer.captured()
	if len(calls) != 1 || calls[0].Operation != "task.create" || calls[0].Scope != actor.Scope {
		t.Fatalf("authorizer calls = %+v; want one task.create", calls)
	}
}

func TestCreateAndStartCommitsOneAtomicUnit(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Run release")
	command.Start = true

	result, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "CreateAndStart")

	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	admitted := env.admission.requestsSnapshot()[0]
	if admitted.Definition.ID != pipelineID {
		t.Fatalf("admitted pipeline = %q; want %q", admitted.Definition.ID, pipelineID)
	}
	if admitted.Inputs["repository"] != "example/repo" {
		t.Fatalf("admitted inputs = %+v", admitted.Inputs)
	}
	if admitted.Task.TaskID == "" || admitted.Task.TaskID != result.TaskID {
		t.Fatalf("admitted task context id = %q; want %q", admitted.Task.TaskID, result.TaskID)
	}
	if string(admitted.Task.Metadata) != string(command.Metadata) {
		t.Fatalf("admitted metadata = %q; want the caller's exact bytes %q", admitted.Task.Metadata, command.Metadata)
	}

	if len(env.ports.starts) != 0 {
		t.Fatalf("StartPort called %d times; create-and-start must commit through CreatePort only", len(env.ports.starts))
	}
	if len(env.ports.creates) != 1 {
		t.Fatalf("create port calls = %d; want exactly one atomic unit", len(env.ports.creates))
	}
	unit := env.ports.lastCreate(t)
	if unit.TaskID != result.TaskID {
		t.Fatalf("unit task id = %q; want %q", unit.TaskID, result.TaskID)
	}
	if unit.Start == nil {
		t.Fatal("create-and-start unit has no start payload")
	}
	start := unit.Start
	if start.Owner != actor.Scope || start.TaskID != unit.TaskID {
		t.Fatalf("nested start identity = %+v/%q; want %+v/%q", start.Owner, start.TaskID, actor.Scope, unit.TaskID)
	}
	if start.ExpectedLifecycleVersion != 0 {
		t.Fatalf("nested start expected lifecycle = %d; want 0 for a new task", start.ExpectedLifecycleVersion)
	}
	if start.ExpectedPipelineVersion != 7 {
		t.Fatalf("nested start expected pipeline version = %d; want admitted 7", start.ExpectedPipelineVersion)
	}
	if start.Snapshot.TaskID != unit.TaskID {
		t.Fatalf("snapshot task id = %q; want %q", start.Snapshot.TaskID, unit.TaskID)
	}
	if len(start.Actions) != 1 || start.Actions[0].Name != "run" {
		t.Fatalf("nested start actions = %+v; want the admitted action", start.Actions)
	}
	if start.Initiator != actor.ID || start.ExecutionPrincipal == "" {
		t.Fatalf("nested start principals = %q/%q", start.Initiator, start.ExecutionPrincipal)
	}
	if start.NewExecutionID == "" || start.NewExecutionID != result.ExecutionID {
		t.Fatalf("nested start execution = %q; want result %q", start.NewExecutionID, result.ExecutionID)
	}
	// The single operation record belongs to CreateUnit. A second receipt,
	// audit or event set inside the nested start would double-commit the
	// operation.
	if start.Receipt.ID != "" || len(start.Audit) != 0 || len(start.Events) != 0 || len(start.Authority) != 0 || len(start.PostCommit) != 0 {
		t.Fatalf("nested start carries its own operation record: %+v", start)
	}

	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.create_and_start", command.CallerKey, command.Content, testNow)
	if unit.Receipt.ExecutionID != result.ExecutionID {
		t.Fatalf("receipt execution id = %q; want %q", unit.Receipt.ExecutionID, result.ExecutionID)
	}
	if unit.Receipt.SnapshotHash == nil || *unit.Receipt.SnapshotHash != start.Snapshot.YAMLSHA256 {
		t.Fatalf("receipt snapshot hash = %v; want the admitted snapshot hash", unit.Receipt.SnapshotHash)
	}
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.create_and_start", testNow)
	requireEvent(t, unit.Events, event.TypeTaskCreated, actor.Scope, string(unit.TaskID), 1, testNow)
	requireEvent(t, unit.Events, event.TypeExecutionQueued, actor.Scope, string(result.ExecutionID), 1, testNow)
	requirePublish(t, unit.PostCommit)
	requireDispatch(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.create_and_start", authorize.CheckMembership)
	version := requireAuthority(t, unit.Authority, "task.create_and_start", authorize.CheckPipelineVersion)
	if version.TargetID != string(pipelineID) || version.ExpectedVersion != 7 {
		t.Fatalf("pipeline version read = %+v; want %q at 7", version, pipelineID)
	}
}

func TestCreateRefusesInvalidMetadataBeforePorts(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	cases := map[string][]byte{
		"invalid json":   []byte(`{"stage":`),
		"non object":     []byte(`["stage"]`),
		"non string":     []byte(`{"stage":1}`),
		"duplicate key":  []byte(`{"stage":"a","stage":"b"}`),
		"trailing bytes": []byte(`{"stage":"a"}{"stage":"b"}`),
	}
	for name, metadata := range cases {
		command := createCommand(actor, "Task")
		command.Metadata = metadata
		_, err := env.service.Create(t.Context(), command)
		if err == nil {
			t.Fatalf("%s metadata was accepted", name)
		}
		requireFaultClass(t, err, fault.InvalidInput)
	}
	oversize := createCommand(actor, "Task")
	oversize.Metadata = []byte(`{"k":"` + strings.Repeat("a", (16<<10)) + `"}`)
	_, err := env.service.Create(t.Context(), oversize)
	requireFaultClass(t, err, fault.InvalidInput)

	empty := createCommand(actor, "")
	_, err = env.service.Create(t.Context(), empty)
	requireFaultClass(t, err, fault.InvalidInput)

	requireNoPortCalls(t, env.ports)
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d after invalid metadata", env.admission.callCount())
	}
}

func TestCreateAcceptsMetadataAtExactBound(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Task")
	// {"k":"<padding>"} is exactly 16 KiB.
	command.Metadata = []byte(`{"k":"` + strings.Repeat("a", (16<<10)-8) + `"}`)
	if len(command.Metadata) != 16<<10 {
		t.Fatalf("fixture metadata is %d bytes; want exactly %d", len(command.Metadata), 16<<10)
	}
	_, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "Create at metadata bound")
	unit := env.ports.lastCreate(t)
	if len(unit.Metadata["k"]) != (16<<10)-8 {
		t.Fatalf("stored metadata value has %d bytes", len(unit.Metadata["k"]))
	}
}

// TestCreateRetryConvergesOnOriginalTaskAndRefusesChangedContent models a
// commit whose response was lost: the port recorded the receipt and the task,
// then returned an error. An identical retry must return the original task id;
// changed content under the same caller key is refused.
func TestCreateRetryConvergesOnOriginalTaskAndRefusesChangedContent(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Investigate failure")

	type committed struct {
		result task.CreateResult
		hash   string
	}
	var mu sync.Mutex
	receipts := make(map[string]committed)
	applied := 0
	original := task.CreateResult{TaskID: "task-original", ContentVersion: 1, LifecycleVersion: 1}
	env.ports.setCreate(func(unit task.CreateUnit) (task.CreateResult, error) {
		mu.Lock()
		defer mu.Unlock()
		key := string(unit.Receipt.Key.TenantID) + "|" + string(unit.Receipt.Key.ProjectID) + "|" +
			string(unit.Receipt.Key.PrincipalID) + "|" + unit.Receipt.Key.Operation + "|" + unit.Receipt.Key.CallerKey
		if previous, ok := receipts[key]; ok {
			if previous.hash != unit.Receipt.ContentHash {
				return task.CreateResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
			}
			replayed := previous.result
			replayed.Replayed = true
			return replayed, nil
		}
		receipts[key] = committed{result: original, hash: unit.Receipt.ContentHash}
		applied++
		return task.CreateResult{}, fault.New(fault.DependencyUnavailable, "response lost after commit")
	})

	if _, err := env.service.Create(t.Context(), command); err == nil {
		t.Fatal("first create reported success despite the lost response")
	} else {
		requireFaultClass(t, err, fault.DependencyUnavailable)
	}
	if applied != 1 {
		t.Fatalf("applied creates = %d; want 1", applied)
	}

	result, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "Create retry")
	if result.TaskID != original.TaskID || !result.Replayed {
		t.Fatalf("retry result = %+v; want original task %q replayed", result, original.TaskID)
	}
	if applied != 1 {
		t.Fatalf("applied creates after retry = %d; want still 1", applied)
	}

	// Changed idempotency content under the same caller key is refused. The
	// receipt content is the caller-supplied request body, so the changed
	// request must carry different content bytes.
	changed := command
	changed.Title = "Different work"
	changed.Content = receiptContent(`{"title":"Different work"}`)
	_, err = env.service.Create(t.Context(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if applied != 1 {
		t.Fatalf("applied creates after changed content = %d; want still 1", applied)
	}
}

func TestCreateRefusesCrossScopeAndLostAuthority(t *testing.T) {
	env := newTestEnv(t)
	outsider := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	outsider.Scope = otherScope()
	command := createCommand(outsider, "Task")
	command.Scope = projectScope()

	_, err := env.service.Create(t.Context(), command)
	requireFaultClass(t, err, fault.Forbidden)
	if env.reader.getCallCount() != 0 {
		t.Fatalf("reader was called for a cross-scope create")
	}
	requireNoPortCalls(t, env.ports)

	env = newTestEnv(t)
	env.authorizer.authorize = func(_ context.Context, _ authorize.Principal, _ authorize.OperationName, _ tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}
	_, err = env.service.Create(t.Context(), createCommand(humanActor(engineerPrincipal, tenancy.RoleEngineer), "Task"))
	requireFaultClass(t, err, fault.Forbidden)
	requireNoPortCalls(t, env.ports)

	// A previously calculated allow is not durable permission: the current
	// membership read inside the unit must still hold.
	env = newTestEnv(t)
	env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		return allowFacts(reads, func(read authorize.AuthorityRead) bool {
			return read.Check == authorize.CheckMembership
		}), nil
	}
	_, err = env.service.Create(t.Context(), createCommand(humanActor(engineerPrincipal, tenancy.RoleEngineer), "Task"))
	requireFaultClass(t, err, fault.Forbidden)
	requireNoPortCalls(t, env.ports)
}
