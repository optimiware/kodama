package task_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, time.November, 4, 9, 0, 0, 0, time.UTC)

const (
	taskID       = id.TaskID("task-1")
	questionID   = task.QuestionID("question-1")
	pipelineID   = pipeline.PipelineID("pipeline-1")
	executionOne = id.ExecutionID("exec-1")
	executionTwo = id.ExecutionID("exec-2")

	engineerPrincipal = id.PrincipalID("principal-engineer")
	reviewerPrincipal = id.PrincipalID("principal-reviewer")
	operatorPrincipal = id.PrincipalID("principal-operator")
	viewerPrincipal   = id.PrincipalID("principal-viewer")
	serviceAPrincipal = id.PrincipalID("principal-service-a")
	serviceBPrincipal = id.PrincipalID("principal-service-b")
)

var testPipelineYAML = []byte("actions:\n  - name: run\n    image: registry.example.com/run:latest\n")

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-b")}
}

func humanActor(principal id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    identity.KindHuman,
		Scope:   projectScope(),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func serviceActor(principal id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    identity.KindService,
		Scope:   projectScope(),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func executionActor(principal id.PrincipalID, execution id.ExecutionID) authorize.Principal {
	return authorize.Principal{
		ID:          principal,
		Kind:        identity.KindExecution,
		Scope:       projectScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: execution,
	}
}

func roleAssignment(t *testing.T, role tenancy.Role) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("role/" + string(role))
	if err != nil {
		t.Fatalf("ParseAssignment role: %v", err)
	}
	return assignment
}

func serviceAssignment(t *testing.T, service string) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("service/" + service)
	if err != nil {
		t.Fatalf("ParseAssignment service: %v", err)
	}
	return assignment
}

func receiptContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
}

func baseTask() task.Task {
	return task.Task{
		ID:               taskID,
		Scope:            projectScope(),
		Title:            "Investigate failure",
		Description:      "Markdown details",
		Metadata:         map[string]string{"stage": "review", "priority": "high"},
		ContentVersion:   3,
		LifecycleVersion: 4,
		State:            task.StateOpen,
		SelectedPipeline: pipelineID,
		Inputs:           map[id.InputKey]string{"repository": "example/repo"},
		Initiator:        engineerPrincipal,
		ExecutionHistory: []id.ExecutionID{executionOne},
		CurrentExecution: executionOne,
	}
}

func waitingTask(t *testing.T) task.Task {
	t.Helper()
	value := baseTask()
	value.State = task.StateWaiting
	value.Question = &task.Question{
		ID:         questionID,
		Body:       "Which candidate should land?",
		Assignment: roleAssignment(t, tenancy.RoleReviewer),
		References: []string{"git:abc123"},
		Version:    1,
		AskedBy:    engineerPrincipal,
		At:         testNow,
	}
	return value
}

func doneTask() task.Task {
	value := baseTask()
	value.State = task.StateDone
	return value
}

func cancelledTask() task.Task {
	value := baseTask()
	value.State = task.StateCancelled
	return value
}

func openExecution(version uint64) task.ExecutionView {
	return task.ExecutionView{ID: executionOne, Version: version, State: kernel.ExecutionRunning}
}

func confirmedStop() kernel.StopEvidence {
	return kernel.ConfirmedStop("container exited")
}

func pendingStop() kernel.StopEvidence {
	return kernel.PendingStop("container stop unconfirmed")
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault, got nil", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want class %s", err, want)
	}
	if got != want {
		t.Fatalf("fault class = %s; want %s (%v)", got, want, err)
	}
}

func requireFaultClassOneOf(t *testing.T, err error, wants ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected one of %v faults, got nil", wants)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want one of %v", err, wants)
	}
	if slices.Contains(wants, got) {
		return
	}
	t.Fatalf("fault class = %s; want one of %v (%v)", got, wants, err)
}

func requireNoError(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func requireNoEvents(t *testing.T, events []event.Envelope) {
	t.Helper()
	if len(events) != 0 {
		t.Fatalf("events = %v; want none", eventTypes(events))
	}
}

func requireNoPostCommit(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	if len(effects) != 0 {
		t.Fatalf("post-commit effects = %+v; want none", effects)
	}
}

func requireReceipt(
	t *testing.T,
	record receipt.Record,
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation, callerKey string,
	content receipt.Content,
	at time.Time,
) {
	t.Helper()
	if record.ID == "" {
		t.Fatal("receipt has no identity")
	}
	if record.Key.TenantID != scope.TenantID || record.Key.ProjectID != scope.ProjectID {
		t.Fatalf("receipt scope = %+v; want %+v", record.Key, scope)
	}
	if record.Key.PrincipalID != principal {
		t.Fatalf("receipt principal = %q; want %q", record.Key.PrincipalID, principal)
	}
	if record.Key.Operation != operation {
		t.Fatalf("receipt operation = %q; want %q", record.Key.Operation, operation)
	}
	if record.Key.CallerKey != callerKey {
		t.Fatalf("receipt caller key = %q; want %q", record.Key.CallerKey, callerKey)
	}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		t.Fatalf("receipt.Fingerprint: %v", err)
	}
	if record.ContentHash != fingerprint {
		t.Fatalf("receipt content hash = %q; want %q", record.ContentHash, fingerprint)
	}
	if !record.At.Equal(at) {
		t.Fatalf("receipt time = %v; want %v", record.At, at)
	}
}

func requireAudit(
	t *testing.T,
	audits []event.Audit,
	scope tenancy.Scope,
	actor id.PrincipalID,
	operation string,
	at time.Time,
) {
	t.Helper()
	if len(audits) != 1 {
		t.Fatalf("audit records = %d; want exactly one", len(audits))
	}
	audit := audits[0]
	if audit.ID == "" {
		t.Fatal("audit record has no identity")
	}
	if audit.Scope != scope || audit.Actor != actor || audit.Operation != operation {
		t.Fatalf("audit = %+v; want scope %+v actor %q operation %q", audit, scope, actor, operation)
	}
	if !audit.At.Equal(at) {
		t.Fatalf("audit time = %v; want %v", audit.At, at)
	}
}

func requireEvent(
	t *testing.T,
	events []event.Envelope,
	eventType event.Type,
	scope tenancy.Scope,
	subjectID string,
	subjectVersion uint64,
	at time.Time,
) event.Envelope {
	t.Helper()
	for _, envelope := range events {
		if envelope.Type != eventType {
			continue
		}
		if envelope.Scope != scope {
			t.Fatalf("event %q scope = %+v; want %+v", eventType, envelope.Scope, scope)
		}
		if envelope.SubjectID != subjectID || envelope.SubjectVersion != subjectVersion {
			t.Fatalf("event %q subject = %q@%d; want %q@%d", eventType, envelope.SubjectID, envelope.SubjectVersion, subjectID, subjectVersion)
		}
		if !envelope.At.Equal(at) {
			t.Fatalf("event %q time = %v; want %v", eventType, envelope.At, at)
		}
		return envelope
	}
	t.Fatalf("events %v do not contain %q", eventTypes(events), eventType)
	return event.Envelope{}
}

func requireNoEvent(t *testing.T, events []event.Envelope, eventType event.Type) {
	t.Helper()
	for _, envelope := range events {
		if envelope.Type == eventType {
			t.Fatalf("events unexpectedly contain %q", eventType)
		}
	}
}

func eventTypes(events []event.Envelope) []event.Type {
	types := make([]event.Type, 0, len(events))
	for _, envelope := range events {
		types = append(types, envelope.Type)
	}
	return types
}

func requirePublish(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	for _, effect := range effects {
		if effect.Kind == event.PostPublishEvent {
			return
		}
	}
	t.Fatalf("post-commit effects %+v do not publish", effects)
}

func requireDispatch(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	for _, effect := range effects {
		if effect.Kind == event.PostDispatch {
			return
		}
	}
	t.Fatalf("post-commit effects %+v do not dispatch", effects)
}

func requireNoDispatch(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	for _, effect := range effects {
		if effect.Kind == event.PostDispatch {
			t.Fatalf("post-commit effects unexpectedly dispatch: %+v", effects)
		}
	}
}

func requireAuthority(t *testing.T, reads []authorize.AuthorityRead, operation authorize.OperationName, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("registry operation %q is not declared", operation)
	}
	for _, read := range reads {
		if read.Check != check || read.Operation != operation {
			continue
		}
		if !reflect.DeepEqual(read.Required, registered.Requires) {
			t.Fatalf("authority read required = %+v; want registry grant %+v", read.Required, registered.Requires)
		}
		return read
	}
	t.Fatalf("authority reads %+v do not contain check %q for %q", reads, check, operation)
	return authorize.AuthorityRead{}
}

func requireNoAuthority(t *testing.T, reads []authorize.AuthorityRead, check authorize.AuthorityCheck) {
	t.Helper()
	for _, read := range reads {
		if read.Check == check {
			t.Fatalf("authority reads unexpectedly contain %q: %+v", check, reads)
		}
	}
}

func requireNoPortCalls(t *testing.T, ports *fakePorts) {
	t.Helper()
	if calls := ports.totalCalls(); calls != 0 {
		t.Fatalf("commit ports called %d times; want 0", calls)
	}
}
