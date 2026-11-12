package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestStartBuildsTaskFreeAdmissionUnit(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer)
	admission := pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 3,
		admittedContainerAction(0, "build", time.Minute))

	command := execution.StartCommand{
		Actor:              actor,
		Scope:              scope,
		Admission:          admission,
		Initiator:          id.PrincipalID("principal-1"),
		ExecutionPrincipal: id.PrincipalID("principal-execution"),
		CallerKey:          "start-key-1",
		Content:            receiptContent(`{"pipeline_id":"pipeline-1","version":3}`),
	}
	result, err := fixture.service.Start(context.Background(), command)
	requireNoError(t, err, "Start")
	if result.Replayed {
		t.Fatal("fresh start reported a replay")
	}
	if result.PipelineVersion != 3 {
		t.Fatalf("result pipeline version = %d; want 3", result.PipelineVersion)
	}
	if result.ExecutionVersion != 1 {
		t.Fatalf("result execution version = %d; want 1", result.ExecutionVersion)
	}

	if fixture.starts.callCount() != 1 {
		t.Fatalf("start port calls = %d; want exactly one", fixture.starts.callCount())
	}
	unit := fixture.starts.captured()[0]
	if unit.Owner != scope {
		t.Fatalf("start owner = %+v; want %+v", unit.Owner, scope)
	}
	if unit.PipelineID != admission.PipelineID {
		t.Fatalf("start pipeline = %q; want %q", unit.PipelineID, admission.PipelineID)
	}
	if unit.ExpectedPipelineVersion != admission.PipelineVersion {
		t.Fatalf("start pipeline version = %d; want %d", unit.ExpectedPipelineVersion, admission.PipelineVersion)
	}
	if unit.Snapshot.YAMLSHA256 != admission.Snapshot.YAMLSHA256 {
		t.Fatal("start snapshot hash differs from the immutable admission snapshot")
	}
	if len(unit.Actions) != 1 || unit.Actions[0].Name != "build" {
		t.Fatalf("start actions = %+v; want the admitted ordered action rows", unit.Actions)
	}
	if unit.Initiator != id.PrincipalID("principal-1") || unit.ExecutionPrincipal != id.PrincipalID("principal-execution") {
		t.Fatalf("start principals = %q/%q", unit.Initiator, unit.ExecutionPrincipal)
	}
	if unit.NewExecutionID == "" {
		t.Fatal("start unit has no new execution identity")
	}
	if result.ExecutionID != unit.NewExecutionID {
		t.Fatalf("result execution = %q; want %q", result.ExecutionID, unit.NewExecutionID)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("start time = %v; want %v", unit.At, testNow)
	}

	requireReceipt(t, unit.Receipt, scope, actor.ID, "pipeline.start", command.CallerKey, command.Content, testNow)
	if unit.Receipt.ExecutionID != unit.NewExecutionID {
		t.Fatalf("receipt execution = %q; want %q", unit.Receipt.ExecutionID, unit.NewExecutionID)
	}
	if unit.Receipt.SnapshotHash == nil || *unit.Receipt.SnapshotHash != admission.Snapshot.YAMLSHA256 {
		t.Fatal("receipt does not pin the admitted snapshot hash")
	}
	requireAudit(t, unit.Audit, scope, actor.ID, "pipeline.start", testNow)

	envelope := requireEvent(t, unit.Events, event.TypeExecutionQueued, scope, string(unit.NewExecutionID), actor.ID, testNow)
	if envelope.SubjectVersion != 1 {
		t.Fatalf("queued execution event version = %d; want 1", envelope.SubjectVersion)
	}

	requireAuthority(t, unit.Authority, "pipeline.start", authorize.CheckMembership)
	requireAuthority(t, unit.Authority, "pipeline.start", authorize.CheckPipelineVersion)
	requireNoAuthority(t, unit.Authority, authorize.CheckTaskVersion)

	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)
	requirePostCommit(t, unit.PostCommit, event.PostDispatch)
}

func TestStartRefusesUnauthorizedWithoutCommit(t *testing.T) {
	fixture := newTestService(t)
	fixture.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}
	_, err := fixture.service.Start(context.Background(), execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     projectScope("tenant-1", "project-1"),
		Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 1),
		CallerKey: "start-key-1",
		Content:   receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.starts.callCount() != 0 {
		t.Fatalf("start port calls = %d; want zero after authorization refusal", fixture.starts.callCount())
	}
}

func TestStartRequiresProjectScope(t *testing.T) {
	fixture := newTestService(t)
	_, err := fixture.service.Start(context.Background(), execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     tenancy.Scope{TenantID: id.TenantID("tenant-1")},
		Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 1),
		CallerKey: "start-key-1",
		Content:   receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.starts.callCount() != 0 {
		t.Fatalf("start port calls = %d; want zero", fixture.starts.callCount())
	}
}

func TestStartRejectsIncompleteAdmission(t *testing.T) {
	fixture := newTestService(t)
	_, err := fixture.service.Start(context.Background(), execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     projectScope("tenant-1", "project-1"),
		Admission: pipeline.AdmissionResult{},
		CallerKey: "start-key-1",
		Content:   receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.starts.callCount() != 0 {
		t.Fatalf("start port calls = %d; want zero", fixture.starts.callCount())
	}
}

func TestStartLostResponseAfterCommitReturnsOriginalExecution(t *testing.T) {
	fixture := newTestService(t)
	command := execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     projectScope("tenant-1", "project-1"),
		Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 2),
		CallerKey: "start-key-1",
		Content:   receiptContent(`{"pipeline_id":"pipeline-1","version":2}`),
	}
	fixture.starts.failNextCallAfterCommit()
	if _, err := fixture.service.Start(context.Background(), command); err == nil {
		t.Fatal("first start unexpectedly succeeded; want a lost response after commit")
	}
	committed := fixture.starts.captured()
	if len(committed) != 1 {
		t.Fatalf("committed start units = %d; want one", len(committed))
	}

	result, err := fixture.service.Start(context.Background(), command)
	requireNoError(t, err, "retried Start")
	if !result.Replayed {
		t.Fatal("identical start retry did not report a replay")
	}
	if result.ExecutionID != committed[0].NewExecutionID {
		t.Fatalf("retry execution = %q; want the committed %q", result.ExecutionID, committed[0].NewExecutionID)
	}
	if fixture.starts.callCount() != 1 {
		t.Fatalf("committed start units = %d; want one (no second effect)", fixture.starts.callCount())
	}
}

func TestStartRefusesChangedContentForSameReceipt(t *testing.T) {
	fixture := newTestService(t)
	command := execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     projectScope("tenant-1", "project-1"),
		Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 2),
		CallerKey: "start-key-1",
		Content:   receiptContent(`{"pipeline_id":"pipeline-1","version":2}`),
	}
	if _, err := fixture.service.Start(context.Background(), command); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	command.Content = receiptContent(`{"pipeline_id":"pipeline-1","version":3}`)
	_, err := fixture.service.Start(context.Background(), command)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
}

func TestStartUnitDoesNotCarryTaskFields(t *testing.T) {
	// A task-free start must not smuggle a task version or task id into the
	// admission commit. The compile-time shape already excludes the fields;
	// this behavioral check pins the authority reads and the empty task id in
	// the admitted snapshot.
	fixture := newTestService(t)
	admission := pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 1)
	if admission.Snapshot.TaskID != "" {
		t.Fatalf("task-free admission snapshot carries task id %q", admission.Snapshot.TaskID)
	}
	_, err := fixture.service.Start(context.Background(), execution.StartCommand{
		Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:     projectScope("tenant-1", "project-1"),
		Admission: admission,
		CallerKey: "start-key-1",
		Content:   receiptContent(`{}`),
	})
	requireNoError(t, err, "Start")
	unit := fixture.starts.captured()[0]
	requireNoAuthority(t, unit.Authority, authorize.CheckTaskVersion)
}
