package executionstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-2"}
}

func newStore(t *testing.T) (*Store, *memory.Store) {
	t.Helper()
	backend := memory.New()
	return New(backend, backend), backend
}

func contentHash(fill string) string {
	return "sha256:" + strings.Repeat(fill, 64)
}

func testReceipt(scope tenancy.Scope, key, hash string) receipt.Record {
	return receipt.Record{
		ID:          id.ReceiptID("receipt-" + key),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "user-1", Operation: "execution.start", CallerKey: key},
		ContentHash: contentHash(hash),
		At:          testNow,
	}
}

func testAudit(scope tenancy.Scope, suffix string) event.Audit {
	return event.Audit{ID: id.EventID("audit-" + suffix), Scope: scope, Actor: "user-1", Operation: "execution.start", At: testNow}
}

func testEvent(scope tenancy.Scope, suffix, subject string) event.Envelope {
	return event.Envelope{
		ID:             id.EventID("event-" + suffix),
		Type:           event.TypeExecutionQueued,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: "user-1"},
		At:             testNow,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: 1,
	}
}

func testSnapshot(taskID id.TaskID, conversation []byte) pipeline.Snapshot {
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte("name: build\non: push\n"),
		Inputs:       map[id.InputKey]string{"target": "linux"},
		Requirements: []string{"process"},
		TaskID:       taskID,
		Conversation: conversation,
	})
	if err != nil {
		panic(err)
	}
	return snapshot
}

func containerAction() pipeline.AdmittedAction {
	return pipeline.AdmittedAction{
		Index:   0,
		Name:    "build",
		Kind:    definition.ActionKindContainer,
		Timeout: 90 * time.Second,
		Container: &definition.Container{
			Image:    "registry.example/build@sha256:" + strings.Repeat("a", 64),
			Commands: []string{"make build"},
			Outputs:  []id.OutputKey{"artifact"},
		},
	}
}

func requestAction() pipeline.AdmittedAction {
	return pipeline.AdmittedAction{
		Index:   1,
		Name:    "approve",
		Kind:    definition.ActionKindRequest,
		Timeout: 60 * time.Second,
		Request: &definition.Request{
			Assignment: "role/reviewer",
			Subject: definition.NewSubject(map[string]definition.SubjectValue{
				"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "build", Key: "artifact"}},
			}),
			Outputs: []id.OutputKey{"decision"},
		},
	}
}

func startUnit(scope tenancy.Scope, executionID id.ExecutionID, key string, actions ...pipeline.AdmittedAction) execution.StartUnit {
	return execution.StartUnit{
		Owner:                   scope,
		PipelineID:              pipelineIDFor(scope),
		ExpectedPipelineVersion: 3,
		Snapshot:                testSnapshot("", nil),
		Actions:                 actions,
		Initiator:               "user-1",
		ExecutionPrincipal:      "exec-principal-1",
		NewExecutionID:          executionID,
		At:                      testNow,
		Receipt:                 testReceipt(scope, key, "a"),
		Audit:                   []event.Audit{testAudit(scope, key)},
		Events:                  []event.Envelope{testEvent(scope, key, string(executionID))},
	}
}

// pipelineIDFor returns the distinct pipeline identity of one scope, so two
// scopes in one memory store cannot collide on the globally unique row id.
func pipelineIDFor(scope tenancy.Scope) pipeline.PipelineID {
	return pipeline.PipelineID("pipe-" + string(scope.ProjectID))
}

// seedPipeline inserts the admitted pipeline row a versioned start locks. It is
// idempotent so several executions can start from the same admitted pipeline.
func seedPipeline(t *testing.T, backend *memory.Store, scope tenancy.Scope, version uint64) {
	t.Helper()
	if _, ok := backend.Snapshot(scope, tablePipelines, string(pipelineIDFor(scope))); ok {
		return
	}
	digest := testSnapshot("", nil).YAMLSHA256
	_, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  tablePipelines,
			ID:     string(pipelineIDFor(scope)),
			Insert: true,
			Values: map[string]any{
				"name":        "pipe",
				"yaml":        []byte("name: build\n"),
				"yaml_sha256": append([]byte(nil), digest[:]...),
				"version":     int64(version),
				"created_at":  testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}
}

// startedExecution starts one execution and returns its action identities.
func startedExecution(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope, executionID id.ExecutionID, actions ...pipeline.AdmittedAction) []id.ActionID {
	t.Helper()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, executionID, string(executionID), actions...)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	identities := make([]id.ActionID, len(actions))
	for index := range actions {
		identities[index] = snapshotdoc.DeriveActionID(executionID, index)
	}
	return identities
}

func loadExecution(t *testing.T, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	aggregate, err := adapter.Load(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return aggregate
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault; got nil", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("fault class = %q (%v); want %q", class, err, want)
	}
}

func activationUnit(scope tenancy.Scope, executionID id.ExecutionID, action id.ActionID, key string) request.ActivationUnit {
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectLiteral, Literal: "artifact-1"},
	})
	return request.ActivationUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ActionID:                 action,
		ExpectedExecutionVersion: 1,
		ExpectedActionVersion:    1,
		Subject:                  subject,
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		Timeout:                  60 * time.Second,
		Deadline:                 testNow.Add(60 * time.Second),
		At:                       testNow,
		StopEvidence:             kernel.ConfirmedStop("workload stopped"),
		CredentialUpdate:         nil,
		Receipt:                  testReceipt(scope, key, "b"),
		Audit:                    []event.Audit{testAudit(scope, key)},
		Events:                   []event.Envelope{testEvent(scope, key, string(executionID))},
	}
}

// newStoreWithClock returns an adapter over a memory store whose transaction
// clock is pinned, so deadline-guard behavior is deterministic.
func newStoreWithClock(t *testing.T, clock func() time.Time) (*Store, *memory.Store) {
	t.Helper()
	backend := memory.NewWithClock(clock)
	return New(backend, backend), backend
}

// resolveReceipt builds a request-resolution receipt whose key matches the
// actor and idempotency key the ResolveUnit carries.
func resolveReceipt(scope tenancy.Scope, receiptID, callerKey, hash string) receipt.Record {
	return receipt.Record{
		ID:          id.ReceiptID(receiptID),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "user-1", Operation: "execution.request.resolve", CallerKey: callerKey},
		ContentHash: contentHash(hash),
		At:          testNow,
	}
}

func resolveEvent(scope tenancy.Scope, suffix, executionID string) event.Envelope {
	return event.Envelope{
		ID:             id.EventID("event-" + suffix),
		Type:           event.TypeRequestResolved,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: "user-1"},
		At:             testNow,
		Scope:          scope,
		SubjectID:      executionID,
		SubjectVersion: 3,
	}
}

func resolveAudit(scope tenancy.Scope, suffix string) event.Audit {
	return event.Audit{ID: id.EventID("audit-" + suffix), Scope: scope, Actor: "user-1", Operation: "execution.request.resolve", At: testNow}
}

// firstResolveUnit builds a first-settlement unit from the Kernel reduction of
// the current execution, exactly like the request service does.
func firstResolveUnit(t *testing.T, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, decision request.Decision, response kernel.Outputs, callerKey, hash string, at time.Time) request.ResolveUnit {
	t.Helper()
	current := loadExecution(t, adapter, scope, executionID)
	kernelDecision, ok := decision.KernelDecision()
	if !ok {
		t.Fatalf("unknown decision %q", decision)
	}
	_, commit, err := flow.Reduce(current, flow.AnswerFact{
		ExecutionID: executionID,
		ActionID:    actionID,
		Answer: kernel.Answer{
			Decision: kernelDecision,
			Outputs:  response,
			Actor:    "user-1",
			At:       at,
		},
		At: at,
	})
	if err != nil {
		t.Fatalf("reduce answer: %v", err)
	}
	if commit == nil {
		t.Fatal("answer reduction is empty; the action is already answered")
	}
	return request.ResolveUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ActionID:                 actionID,
		ExpectedExecutionVersion: current.Version,
		ExpectedActionVersion:    current.Actions[actionIndex(t, current, actionID)].Version,
		Decision:                 decision,
		Response:                 response,
		Actor:                    "user-1",
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		At:                       at,
		IdempotencyKey:           callerKey,
		Commit:                   *commit,
		Receipt:                  resolveReceipt(scope, "receipt-"+callerKey, callerKey, hash),
		Audit:                    []event.Audit{resolveAudit(scope, callerKey)},
		Events:                   []event.Envelope{resolveEvent(scope, callerKey, string(executionID))},
	}
}

// replayResolveUnit builds the zero-commit retry of a settled answer.
func replayResolveUnit(scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, decision request.Decision, response kernel.Outputs, callerKey, hash string, at time.Time) request.ResolveUnit {
	return request.ResolveUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ActionID:                 actionID,
		ExpectedExecutionVersion: 1,
		ExpectedActionVersion:    1,
		Decision:                 decision,
		Response:                 response,
		Actor:                    "user-1",
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		At:                       at,
		IdempotencyKey:           callerKey,
		Receipt:                  resolveReceipt(scope, "receipt-"+callerKey, callerKey, hash),
		Audit:                    []event.Audit{resolveAudit(scope, callerKey)},
		Events:                   []event.Envelope{resolveEvent(scope, callerKey, string(executionID))},
	}
}

func actionIndex(t *testing.T, execution kernel.Execution, actionID id.ActionID) int {
	t.Helper()
	for index, action := range execution.Actions {
		if action.ID == actionID {
			return index
		}
	}
	t.Fatalf("action %q is not part of the execution", actionID)
	return -1
}

// requestOnlyAction is a single request action at index zero, so a passing
// answer settles the whole execution.
func requestOnlyAction() pipeline.AdmittedAction {
	action := requestAction()
	action.Index = 0
	return action
}
