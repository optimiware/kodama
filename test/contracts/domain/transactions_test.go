package domaincontract

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
)

func sampleScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")}
}

func sampleReceipt() receipt.Record {
	return receipt.Record{
		ID: id.ReceiptID("receipt-1"),
		Key: receipt.Key{
			TenantID:    id.TenantID("tenant-1"),
			ProjectID:   id.ProjectID("project-1"),
			PrincipalID: id.PrincipalID("human-1"),
			Operation:   "task.start",
			CallerKey:   "caller-key-1",
		},
		ExecutionID: id.ExecutionID("exec-1"),
	}
}

func sampleAudit(now time.Time) []event.Audit {
	return []event.Audit{{
		ID:        id.EventID("audit-1"),
		Scope:     sampleScope(),
		Actor:     id.PrincipalID("human-1"),
		Operation: "task.start",
		At:        now,
		Detail:    "committed",
	}}
}

func sampleEvents(now time.Time) []event.Envelope {
	return []event.Envelope{{
		ID:             id.EventID("event-1"),
		Type:           event.TypeTaskCreated,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID("human-1")},
		At:             now,
		Scope:          sampleScope(),
		SubjectID:      "task-1",
		SubjectVersion: 1,
	}}
}

func sampleReportEvents(now time.Time) []event.Envelope {
	return []event.Envelope{{
		ID:             id.EventID("event-report-1"),
		Type:           event.TypeVerifiedReport,
		ReportType:     event.ReportType("git.ref.updated"),
		Source:         event.Source{Kind: event.SourceService, PrincipalID: id.PrincipalID("service-1")},
		At:             now,
		Scope:          sampleScope(),
		SubjectID:      "resource-1",
		SubjectVersion: 3,
	}}
}

func sampleAuthority() []authorize.AuthorityRead {
	return []authorize.AuthorityRead{{
		Check:           authorize.CheckMembership,
		Scope:           sampleScope(),
		PrincipalID:     id.PrincipalID("human-1"),
		TargetID:        "task-1",
		ExpectedVersion: 1,
	}}
}

func samplePostCommit() []event.PostCommitEffect {
	return []event.PostCommitEffect{{Kind: event.PostDispatch, Target: "exec-1"}}
}

func sampleSnapshot(t *testing.T) pipeline.Snapshot {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	return snapshot
}

func sampleCredential(now time.Time) token.Credential {
	return token.Credential{Nonce: make([]byte, 32), Expiry: now.Add(time.Hour)}
}

// TestCommitUnitsCarryCompletePayload pins the whole-transaction payloads. Each
// unit carries owner scope, expected versions, current-authority reads, the
// state/credential/receipt/audit/event writes and post-commit effects. The
// reflection check is intentionally explicit: a missing field is a contract
// regression, not an implementation detail.
func TestCommitUnitsCarryCompletePayload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	snapshot := sampleSnapshot(t)
	credential := sampleCredential(now)
	assignment := mustAssignment(t, "role/reviewer")
	receiptRecord := sampleReceipt()
	audit := sampleAudit(now)
	events := sampleEvents(now)
	authority := sampleAuthority()
	postCommit := samplePostCommit()

	executionUnit := execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              id.ExecutionID("exec-1"),
		ExpectedExecutionVersion: 4,
		ExpectedActionVersion:    2,
		Commit: flow.Commit{
			ExecutionID:              id.ExecutionID("exec-1"),
			ExpectedExecutionVersion: 4,
			NextExecutionState:       kernel.ExecutionRunning,
			ActionID:                 id.ActionID("action-1"),
			ExpectedActionVersion:    2,
			NextActionState:          kernel.ActionRunning,
			At:                       now,
			Deadline:                 now.Add(time.Hour),
		},
		CredentialUpdate: &credential,
		Receipt:          receiptRecord,
		Audit:            audit,
		Events:           events,
		Authority:        authority,
		PostCommit:       postCommit,
	}
	if executionUnit.Owner != scope || executionUnit.ExpectedExecutionVersion != 4 ||
		executionUnit.Commit.ExecutionID != "exec-1" || executionUnit.CredentialUpdate == nil {
		t.Fatalf("execution transition unit lost payload: %+v", executionUnit)
	}

	activationUnit := request.ActivationUnit{
		Owner:                    scope,
		ExecutionID:              id.ExecutionID("exec-1"),
		ActionID:                 id.ActionID("action-1"),
		ExpectedExecutionVersion: 4,
		ExpectedActionVersion:    2,
		Subject:                  definition.Subject{},
		Assignment:               assignment,
		Deadline:                 now.Add(30 * time.Minute),
		StopEvidence:             kernel.ConfirmedStop("container exited"),
		CredentialUpdate:         &credential,
		Receipt:                  receiptRecord,
		Audit:                    audit,
		Events:                   events,
		Authority:                authority,
		PostCommit:               postCommit,
	}
	if !activationUnit.StopEvidence.IsConfirmed() || activationUnit.Assignment.Kind != request.AssignmentRole {
		t.Fatalf("activation unit lost stop evidence or assignment: %+v", activationUnit)
	}

	resolveUnit := request.ResolveUnit{
		Owner:                    scope,
		ExecutionID:              id.ExecutionID("exec-1"),
		ActionID:                 id.ActionID("action-1"),
		ExpectedExecutionVersion: 5,
		ExpectedActionVersion:    3,
		Decision:                 request.DecisionPass,
		Response:                 kernel.Outputs{"approved": "true"},
		Actor:                    id.PrincipalID("reviewer-1"),
		Assignment:               assignment,
		At:                       now,
		IdempotencyKey:           "resolve-key-1",
		Commit: flow.Commit{
			ExecutionID:              id.ExecutionID("exec-1"),
			ExpectedExecutionVersion: 5,
			ActionID:                 id.ActionID("action-1"),
			ExpectedActionVersion:    3,
			NextExecutionState:       kernel.ExecutionRunning,
			NextActionState:          kernel.ActionSucceeded,
			Answer:                   &kernel.Answer{Decision: kernel.DecisionPass, Actor: id.PrincipalID("reviewer-1"), At: now},
			At:                       now,
		},
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	if resolveUnit.Decision != request.DecisionPass || resolveUnit.Response["approved"] != "true" {
		t.Fatalf("resolve unit lost decision or response: %+v", resolveUnit)
	}

	startUnit := task.StartUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		ExpectedLifecycleVersion: 7,
		ExpectedPipelineVersion:  3,
		Snapshot:                 snapshot,
		NewExecutionID:           id.ExecutionID("exec-1"),
		Receipt:                  receiptRecord,
		Audit:                    audit,
		Events:                   events,
		Authority:                authority,
		PostCommit:               postCommit,
	}
	if startUnit.ExpectedLifecycleVersion != 7 || startUnit.Snapshot.HashHex() == "" {
		t.Fatalf("task start unit lost version or snapshot: %+v", startUnit)
	}

	askUnit := task.AskUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		ExpectedLifecycleVersion: 7,
		ExpectedExecutionVersion: 2,
		QuestionID:               task.QuestionID("question-1"),
		Question:                 "Which candidate?",
		Assignment:               assignment,
		References:               []string{"git:abc123"},
		CredentialUpdate:         &credential,
		Receipt:                  receiptRecord,
		Audit:                    audit,
		Events:                   events,
		Authority:                authority,
		PostCommit:               postCommit,
	}
	if askUnit.QuestionID != "question-1" || len(askUnit.References) != 1 {
		t.Fatalf("task ask unit lost question or references: %+v", askUnit)
	}

	replyUnit := task.ReplyUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		ExpectedLifecycleVersion: 7,
		QuestionID:               task.QuestionID("question-1"),
		ExpectedQuestionVersion:  1,
		Assignment:               assignment,
		Answer:                   "use abc123",
		Initiator:                id.PrincipalID("human-1"),
		ExpectedPipelineVersion:  3,
		Snapshot:                 snapshot,
		NewExecutionID:           id.ExecutionID("exec-2"),
		Receipt:                  receiptRecord,
		Audit:                    audit,
		Events:                   events,
		Authority:                authority,
		PostCommit:               postCommit,
	}
	if replyUnit.ExpectedQuestionVersion != 1 || replyUnit.NewExecutionID != "exec-2" {
		t.Fatalf("task reply unit lost question version or execution id: %+v", replyUnit)
	}

	cancelUnit := task.CancelUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		ExpectedLifecycleVersion: 7,
		ExpectedExecutionVersion: 2,
		CredentialUpdate:         &credential,
		Receipt:                  receiptRecord,
		Audit:                    audit,
		Events:                   events,
		Authority:                authority,
		PostCommit:               postCommit,
	}
	if cancelUnit.ExpectedLifecycleVersion != 7 {
		t.Fatalf("task cancel unit lost lifecycle version: %+v", cancelUnit)
	}

	tokenUnit := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     id.PrincipalID("exec-1"),
		ExecutionID:     id.ExecutionID("exec-1"),
		ServiceID:       "",
		Kind:            token.MutationRotate,
		ExpectedVersion: 6,
		Credential:      credential,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	if tokenUnit.Kind != token.MutationRotate || !tokenUnit.Credential.Present() {
		t.Fatalf("token mutation unit lost kind or credential: %+v", tokenUnit)
	}

	bindingUnit := resource.BindingUnit{
		Owner:           scope,
		BindingID:       resource.BindingID("binding-1"),
		ExpectedVersion: 2,
		Binding: resource.Binding{
			ID:               resource.BindingID("binding-1"),
			ResourceID:       resource.ResourceID("resource-1"),
			Scope:            scope,
			EnforcingService: "service-1",
			TrustedOrigins:   []string{"https://git.example.com"},
			Operations:       []resource.Operation{resource.OperationGitRefUpdate},
			ReportableEvents: []string{"git.ref.updated"},
			Version:          3,
			Metadata:         map[string]string{"kind": "repository"},
		},
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	if bindingUnit.Binding.EnforcingService != "service-1" || bindingUnit.ExpectedVersion != 2 {
		t.Fatalf("binding unit lost enforcing service or expected version: %+v", bindingUnit)
	}

	reportUnit := verify.ReportUnit{
		Owner:            scope,
		ReportingService: "service-1",
		BindingID:        resource.BindingID("binding-1"),
		SourceEventID:    "source-event-1",
		EventType:        event.ReportType("git.ref.updated"),
		VerificationID:   verify.VerificationID("verification-1"),
		ResourceID:       resource.ResourceID("resource-1"),
		Operation:        "git.ref.update",
		Subject:          verify.Subject{},
		Receipt:          receiptRecord,
		Audit:            audit,
		Events:           sampleReportEvents(now),
		Authority:        authority,
		PostCommit:       postCommit,
	}
	if reportUnit.ReportingService != "service-1" || reportUnit.SourceEventID != "source-event-1" {
		t.Fatalf("report unit lost service or source event id: %+v", reportUnit)
	}
	if reportUnit.EventType != event.ReportType("git.ref.updated") {
		t.Fatalf("report unit lost its binding-declared event type: %+v", reportUnit)
	}
	if err := reportUnit.ValidateEventType(bindingUnit.Binding); err != nil {
		t.Fatalf("declared report event type refused: %v", err)
	}
	undeclared := reportUnit
	undeclared.EventType = event.ReportType("git.ref.deleted")
	if err := undeclared.ValidateEventType(bindingUnit.Binding); err == nil {
		t.Fatal("undeclared report event type accepted")
	}
	missingAccepted := reportUnit
	missingAccepted.Events = events
	if err := missingAccepted.ValidateEventType(bindingUnit.Binding); err == nil {
		t.Fatal("report unit without an accepted resource.verified event accepted")
	}
	mismatchedAccepted := reportUnit
	mismatchedAccepted.Events = sampleReportEvents(now)
	mismatchedAccepted.Events[0].ReportType = event.ReportType("git.ref.deleted")
	if err := mismatchedAccepted.ValidateEventType(bindingUnit.Binding); err == nil {
		t.Fatal("accepted event with a different report type accepted")
	}

	selectUnit := trigger.SelectUnit{
		Owner:                   scope,
		PipelineID:              pipeline.PipelineID("pipeline-1"),
		EventID:                 id.EventID("event-1"),
		ExpectedPipelineVersion: 4,
		ExpectedBindingVersion:  2,
		Selection: trigger.Selection{
			PipelineID:       pipeline.PipelineID("pipeline-1"),
			EventID:          id.EventID("event-1"),
			YAML:             []byte("actions: []\n"),
			ServiceID:        "service-1",
			Inputs:           map[id.InputKey]string{"ref": "refs/heads/main"},
			EventContentHash: [32]byte{1},
		},
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	if selectUnit.Selection.ServiceID != "service-1" || selectUnit.ExpectedBindingVersion != 2 {
		t.Fatalf("push selection unit lost service or binding version: %+v", selectUnit)
	}

	units := []struct {
		name   string
		value  any
		fields []string
	}{
		{
			name:  "execution.TransitionUnit",
			value: executionUnit,
			fields: []string{
				"Owner", "ExecutionID", "ExpectedExecutionVersion", "ExpectedActionVersion",
				"Commit", "CredentialUpdate", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "request.ActivationUnit",
			value: activationUnit,
			fields: []string{
				"Owner", "ExecutionID", "ActionID", "ExpectedExecutionVersion", "ExpectedActionVersion",
				"Commit", "Subject", "Assignment", "Deadline", "StopEvidence", "CredentialUpdate",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "request.ResolveUnit",
			value: resolveUnit,
			fields: []string{
				"Owner", "ExecutionID", "ActionID", "ExpectedExecutionVersion", "ExpectedActionVersion",
				"Decision", "Response", "Actor", "Assignment", "At", "IdempotencyKey", "Commit",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "task.StartUnit",
			value: startUnit,
			fields: []string{
				"Owner", "TaskID", "ExpectedLifecycleVersion", "ExpectedPipelineVersion",
				"Snapshot", "NewExecutionID", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "task.AskUnit",
			value: askUnit,
			fields: []string{
				"Owner", "TaskID", "ExpectedLifecycleVersion", "ExpectedExecutionVersion",
				"QuestionID", "Question", "Assignment", "References", "CredentialUpdate",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "task.ReplyUnit",
			value: replyUnit,
			fields: []string{
				"Owner", "TaskID", "ExpectedLifecycleVersion", "QuestionID", "ExpectedQuestionVersion",
				"Assignment", "Answer", "Initiator", "ExpectedPipelineVersion", "Snapshot",
				"NewExecutionID", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "task.CancelUnit",
			value: cancelUnit,
			fields: []string{
				"Owner", "TaskID", "ExpectedLifecycleVersion", "ExpectedExecutionVersion",
				"CredentialUpdate", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "token.MutationUnit",
			value: tokenUnit,
			fields: []string{
				"Owner", "PrincipalID", "ExecutionID", "ServiceID", "Kind", "ExpectedVersion",
				"Credential", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "resource.BindingUnit",
			value: bindingUnit,
			fields: []string{
				"Owner", "BindingID", "ExpectedVersion", "Binding",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "verify.ReportUnit",
			value: reportUnit,
			fields: []string{
				"Owner", "ReportingService", "BindingID", "SourceEventID", "EventType",
				"VerificationID", "ResourceID", "Operation", "Subject",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "trigger.SelectUnit",
			value: selectUnit,
			fields: []string{
				"Owner", "PipelineID", "EventID", "ExpectedPipelineVersion", "ExpectedBindingVersion",
				"Selection", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
	}
	for _, unit := range units {
		typ := reflect.TypeOf(unit.value)
		for _, field := range unit.fields {
			if _, ok := typ.FieldByName(field); !ok {
				t.Errorf("%s is missing required field %q", unit.name, field)
			}
		}
	}
}

// TestInboxQueryAndPage pins the scoped inbox shape: current principal, scope,
// kinds, stable cursor/limit and the active items with assignment/version.
func TestInboxQueryAndPage(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	query := inbox.Query{
		Scope:     sampleScope(),
		Principal: identityPrincipal(),
		Kinds:     []inbox.Kind{inbox.KindRequest, inbox.KindWaitingTask, inbox.KindFailedTask},
		Cursor:    "cursor-1",
		Limit:     50,
	}
	if query.Limit != 50 || query.Principal.ID != "human-1" {
		t.Fatalf("inbox query lost principal or limit: %+v", query)
	}
	page := inbox.Page{
		Items: []inbox.Item{{
			Kind:        inbox.KindRequest,
			TaskID:      id.TaskID("task-1"),
			ExecutionID: id.ExecutionID("exec-1"),
			ActionID:    id.ActionID("action-1"),
			Assignment:  mustAssignment(t, "role/reviewer"),
			Version:     3,
			At:          now,
		}},
		NextCursor: "cursor-2",
	}
	if len(page.Items) != 1 || page.Items[0].Assignment.Kind != request.AssignmentRole {
		t.Fatalf("inbox page lost item assignment: %+v", page)
	}
}

// Compile-time proof that each complete commit unit is accepted by exactly one
// consumer-owned port method. This is the no-generic-repository rule in
// executable form.
func TestCommitUnitPortSignatures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	ctx := context.Background()
	var transition execution.TransitionPort = fakeTransition{}
	if _, err := transition.Transition(ctx, execution.TransitionUnit{}); err != nil {
		t.Fatal(err)
	}
	var activation request.ActivationPort = fakeActivation{}
	if _, err := activation.Activate(ctx, request.ActivationUnit{Deadline: now}); err != nil {
		t.Fatal(err)
	}
	var resolve request.ResolvePort = fakeResolve{}
	if _, err := resolve.Resolve(ctx, request.ResolveUnit{At: now}); err != nil {
		t.Fatal(err)
	}
	var start task.StartPort = fakeTaskStart{}
	if _, err := start.Start(ctx, task.StartUnit{}); err != nil {
		t.Fatal(err)
	}
	var ask task.AskPort = fakeTaskAsk{}
	if _, err := ask.Ask(ctx, task.AskUnit{}); err != nil {
		t.Fatal(err)
	}
	var reply task.ReplyPort = fakeTaskReply{}
	if _, err := reply.Reply(ctx, task.ReplyUnit{}); err != nil {
		t.Fatal(err)
	}
	var cancel task.CancelPort = fakeTaskCancel{}
	if _, err := cancel.Cancel(ctx, task.CancelUnit{}); err != nil {
		t.Fatal(err)
	}
	var mutation token.MutationPort = fakeTokenMutation{}
	if _, err := mutation.Mutate(ctx, token.MutationUnit{}); err != nil {
		t.Fatal(err)
	}
	var binding resource.BindingPort = fakeBinding{}
	if _, err := binding.Apply(ctx, resource.BindingUnit{}); err != nil {
		t.Fatal(err)
	}
	var report verify.ReportPort = fakeReport{}
	if _, err := report.Report(ctx, verify.ReportUnit{}); err != nil {
		t.Fatal(err)
	}
	var selector trigger.SelectPort = fakeTrigger{}
	if _, err := selector.Select(ctx, trigger.SelectUnit{}); err != nil {
		t.Fatal(err)
	}
	var query inbox.QueryPort = fakeInbox{}
	if _, err := query.Query(ctx, inbox.Query{}); err != nil {
		t.Fatal(err)
	}
}
