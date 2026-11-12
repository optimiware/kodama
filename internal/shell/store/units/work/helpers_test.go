package work

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const (
	testTenant    = "tenant-1"
	testProject   = "project-1"
	otherProject  = "project-2"
	testPrincipal = "principal-1"
	testPipeline  = "pipeline-1"
	testTask      = "task-1"
	testExecution = "execution-1"
	testEvent     = "event-1"
	contentHashA  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	contentHashB  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: otherProject}
}

// fixture owns one ephemeral memory store and every adapter under test.
type fixture struct {
	store     *memory.Store
	task      *Task
	inbox     *Inbox
	pipelines *Pipelines
	triggers  *Triggers

	receipts int
	events   int
	audits   int
	pipes    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := memory.New()
	fixture := &fixture{store: store}
	var err error
	fixture.task, err = NewTask(store, store)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	fixture.inbox, err = NewInbox(store, store)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	fixture.triggers, err = NewTriggers(store, store, store.Controller(), func() time.Time { return testNow }, func() (id.EventID, error) { return "event-refusal-1", nil })
	if err != nil {
		t.Fatalf("NewTriggers: %v", err)
	}
	fixture.pipelines, err = NewPipelines(store, store, func() (pipeline.PipelineID, error) {
		fixture.pipes++
		return pipeline.PipelineID("pipeline-new-" + strconv.Itoa(fixture.pipes)), nil
	})
	if err != nil {
		t.Fatalf("NewPipelines: %v", err)
	}
	return fixture
}

// receipt builds one valid unique receipt record.
func (f *fixture) receipt(operation, caller, contentHash string) receipt.Record {
	f.receipts++
	return receipt.Record{
		ID: id.ReceiptID("receipt-" + strconv.Itoa(f.receipts)),
		Key: receipt.Key{
			TenantID:    testTenant,
			ProjectID:   testProject,
			PrincipalID: testPrincipal,
			Operation:   operation,
			CallerKey:   caller,
		},
		ContentHash: contentHash,
		At:          testNow,
	}
}

// receiptFor builds one valid unique receipt scoped to the supplied scope.
func (f *fixture) receiptFor(scope tenancy.Scope, operation, caller, contentHash string) receipt.Record {
	record := f.receipt(operation, caller, contentHash)
	record.Key.TenantID = scope.TenantID
	record.Key.ProjectID = scope.ProjectID
	return record
}

// audit builds one valid unique audit row.
func (f *fixture) audit(scope tenancy.Scope, operation string) event.Audit {
	f.audits++
	return event.Audit{
		ID:        id.EventID("audit-" + strconv.Itoa(f.audits)),
		Scope:     scope,
		Actor:     testPrincipal,
		Operation: operation,
		At:        testNow,
	}
}

// event builds one valid unique event envelope.
func (f *fixture) event(scope tenancy.Scope, subject string, version uint64) event.Envelope {
	f.events++
	return event.Envelope{
		ID:             id.EventID("event-" + strconv.Itoa(f.events)),
		Type:           event.TypeExecutionQueued,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: testPrincipal},
		At:             testNow,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: version,
	}
}

// seedPipeline inserts one pipelines row directly through the neutral seam.
func (f *fixture) seedPipeline(t *testing.T, scope tenancy.Scope, identity pipeline.PipelineID, name string, yaml []byte, version uint64) {
	t.Helper()
	digest := sha256.Sum256(yaml)
	_, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "pipelines",
			ID:     string(identity),
			Insert: true,
			Values: map[string]any{
				"name":        name,
				"yaml":        yaml,
				"yaml_sha256": digest[:],
				"version":     int64(version),
				"created_at":  testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}
}

// seedBinding inserts one resource_bindings row directly through the seam.
func (f *fixture) seedBinding(t *testing.T, scope tenancy.Scope, bindingID, repository, serviceID string, version uint64) {
	t.Helper()
	origins, err := jsonColumn([]string{"https://git.example"})
	if err != nil {
		t.Fatalf("encode origins: %v", err)
	}
	operations, err := jsonColumn([]resource.Operation{resource.OperationGitRefUpdate})
	if err != nil {
		t.Fatalf("encode operations: %v", err)
	}
	reportable, err := jsonColumn([]string{"git.ref.updated"})
	if err != nil {
		t.Fatalf("encode reportable events: %v", err)
	}
	metadata, err := jsonColumn(map[string]string{"owner": "team"})
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	_, err = f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "resource_bindings",
			ID:     bindingID,
			Insert: true,
			Values: map[string]any{
				"resource_id":       repository,
				"service_id":        serviceID,
				"origins":           origins,
				"operations":        operations,
				"reportable_events": reportable,
				"metadata":          metadata,
				"version":           int64(version),
				"created_at":        testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

// seedService inserts one services row directly through the seam.
func (f *fixture) seedService(t *testing.T, scope tenancy.Scope, identity, state string, version uint64) {
	t.Helper()
	roles, err := jsonColumn([]string{})
	if err != nil {
		t.Fatalf("encode roles: %v", err)
	}
	_, err = f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "services",
			ID:     identity,
			Insert: true,
			Values: map[string]any{
				"name":             identity,
				"state":            state,
				"roles":            roles,
				"token_nonce":      nil,
				"token_expires_at": nil,
				"version":          int64(version),
				"created_at":       testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// seedExecution inserts one executions row directly through the neutral seam.
func (f *fixture) seedExecution(t *testing.T, scope tenancy.Scope, executionID id.ExecutionID, taskID id.TaskID, state string, version uint64) {
	t.Helper()
	f.seedExecutionForPrincipalAt(t, scope, executionID, taskID, testPrincipal, state, version, testNow)
}

// seedExecutionForPrincipal inserts one executions row owned by the supplied
// principal, so an execution principal's row is distinct from a human's.
func (f *fixture) seedExecutionForPrincipal(t *testing.T, scope tenancy.Scope, executionID id.ExecutionID, taskID id.TaskID, principal string, state string, version uint64) {
	t.Helper()
	f.seedExecutionForPrincipalAt(t, scope, executionID, taskID, principal, state, version, testNow)
}

// seedExecutionForPrincipalAt inserts one executions row with an explicit
// created_at so a timestamp projection (the failed_task At) is deterministic.
func (f *fixture) seedExecutionForPrincipalAt(t *testing.T, scope tenancy.Scope, executionID id.ExecutionID, taskID id.TaskID, principal string, state string, version uint64, at time.Time) {
	t.Helper()
	digest := sha256.Sum256([]byte("yaml"))
	_, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "executions",
			ID:     string(executionID),
			Insert: true,
			Values: map[string]any{
				"pipeline_id":      string(testPipeline),
				"task_id":          nullableText(string(taskID)),
				"principal_id":     principal,
				"state":            state,
				"version":          int64(version),
				"yaml_sha256":      digest[:],
				"snapshot":         []byte(`{"yaml":""}`),
				"token_nonce":      nil,
				"token_expires_at": nil,
				"workspace_id":     nil,
				"created_at":       at,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}
}

// snapshot builds one immutable admission snapshot with exact YAML bytes.
func testSnapshot(yaml []byte) pipeline.Snapshot {
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:     yaml,
		Inputs:   map[id.InputKey]string{"ref": "main"},
		TaskID:   testTask,
		Metadata: map[string]string{"owner": "team"},
	})
	if err != nil {
		panic(err)
	}
	return snapshot
}

// richSnapshot builds one admitted snapshot with env, task bytes and references.
func richSnapshot(yaml, conversation []byte) pipeline.Snapshot {
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         yaml,
		Inputs:       map[id.InputKey]string{"ref": "main"},
		Env:          []definition.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}},
		SecretNames:  []id.SecretName{"REGISTRY_TOKEN"},
		Requirements: []string{"linux/amd64"},
		TaskID:       testTask,
		Conversation: conversation,
		References:   []string{"pipeline:1"},
		Metadata:     map[string]string{"owner": "team"},
	})
	if err != nil {
		panic(err)
	}
	return snapshot
}

// testAction is one valid admitted container action.
func testAction(index int) task.AdmittedAction {
	return task.AdmittedAction{
		Index:   index,
		Name:    id.ActionName("build-" + strconv.Itoa(index)),
		Kind:    definition.ActionKindContainer,
		Timeout: time.Minute,
		Container: &definition.Container{
			Image:    "registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Commands: []string{"go", "test", "./..."},
		},
	}
}

// testRequestAction is one valid admitted request action.
func testRequestAction(index int) task.AdmittedAction {
	return task.AdmittedAction{
		Index:   index,
		Name:    id.ActionName("approve-" + strconv.Itoa(index)),
		Kind:    definition.ActionKindRequest,
		Timeout: time.Minute,
		Request: &definition.Request{
			Assignment: "role/reviewer",
			Subject:    definition.NewSubject(map[string]definition.SubjectValue{"ref": {Kind: definition.SubjectLiteral, Literal: "main"}}),
			Outputs:    []id.OutputKey{"decision"},
		},
	}
}

// createTask commits one plain task creation.
func (f *fixture) createTask(t *testing.T, scope tenancy.Scope, identity id.TaskID) task.CreateResult {
	t.Helper()
	result, err := f.task.Create(context.Background(), task.CreateUnit{
		Owner:       scope,
		TaskID:      identity,
		Title:       "Fix the build",
		Description: "make it green",
		Metadata:    map[string]string{"team": "core"},
		Inputs:      map[id.InputKey]string{"ref": "main"},
		Initiator:   testPrincipal,
		At:          testNow,
		Receipt:     f.receiptFor(scope, "task.create", "create-"+string(identity), contentHashA),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return result
}

// startTask commits one start against a seeded pipeline at version 1.
func (f *fixture) startTask(t *testing.T, scope tenancy.Scope, identity id.TaskID, expectedLifecycle uint64, executionID id.ExecutionID) task.StartResult {
	t.Helper()
	unit := f.startUnit(scope, identity, executionID, expectedLifecycle, 1, f.receipt("task.start", "start-"+string(identity)+"-"+string(executionID), contentHashA))
	result, err := f.task.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	return result
}

// startUnit builds one complete start commit unit.
func (f *fixture) startUnit(scope tenancy.Scope, identity id.TaskID, executionID id.ExecutionID, expectedLifecycle, expectedPipeline uint64, record receipt.Record) task.StartUnit {
	return task.StartUnit{
		Owner:                    scope,
		TaskID:                   identity,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: expectedLifecycle,
		ExpectedPipelineVersion:  expectedPipeline,
		Snapshot:                 testSnapshot([]byte("kind: Pipeline\nversion: 1\n")),
		Actions:                  []task.AdmittedAction{testAction(0), testAction(1)},
		Initiator:                testPrincipal,
		ExecutionPrincipal:       "execution-principal-1",
		NewExecutionID:           executionID,
		At:                       testNow,
		Receipt:                  record,
		Audit:                    []event.Audit{f.audit(scope, "task.start")},
		Events:                   []event.Envelope{f.event(scope, string(executionID), 1)},
	}
}

// cancellationCommit reads the task's current execution aggregate and reduces
// the Kernel cancel fact the unit must carry. A terminal predecessor returns a
// nil commit: its row is preserved, not rewritten.
func (f *fixture) cancellationCommit(t *testing.T, scope tenancy.Scope, identity id.TaskID) (kernel.Execution, *flow.Commit) {
	t.Helper()
	row := f.storedTask(t, scope, identity)
	pointer := id.ExecutionID(textValue(row, "current_execution_id"))
	if pointer == "" {
		return kernel.Execution{}, nil
	}
	aggregate, err := executionstore.New(f.store, f.store).Load(context.Background(), scope, pointer)
	if err != nil {
		t.Fatalf("load current execution %q: %v", pointer, err)
	}
	if aggregate.State.Terminal() {
		return aggregate, nil
	}
	_, commit, err := flow.Reduce(aggregate, flow.CancelFact{
		ExecutionID: aggregate.ID,
		Reason:      flow.CancelRequested,
		At:          testNow,
	})
	if err != nil {
		t.Fatalf("reduce cancel fact: %v", err)
	}
	if commit == nil {
		t.Fatalf("cancel reduction for %q returned no commit", aggregate.ID)
	}
	return aggregate, commit
}

// askUnit builds one complete ask commit unit carrying the Kernel cancel
// reduction of the task's current execution.
func (f *fixture) askUnit(t *testing.T, scope tenancy.Scope, identity id.TaskID, expectedLifecycle uint64, record receipt.Record) task.AskUnit {
	t.Helper()
	aggregate, commit := f.cancellationCommit(t, scope, identity)
	return task.AskUnit{
		Owner:                    scope,
		TaskID:                   identity,
		ExpectedLifecycleVersion: expectedLifecycle,
		ExpectedExecutionVersion: aggregate.Version,
		QuestionID:               "question-1",
		Question:                 "Which branch should I target?",
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator},
		References:               []string{"pipeline:1"},
		Commit:                   commit,
		Receipt:                  record,
		Audit:                    []event.Audit{f.audit(scope, "task.ask")},
		Events:                   []event.Envelope{f.event(scope, string(identity), expectedLifecycle+1)},
	}
}

// replyUnit builds one complete reply commit unit.
func (f *fixture) replyUnit(scope tenancy.Scope, identity id.TaskID, executionID id.ExecutionID, expectedLifecycle, expectedQuestion, expectedPipeline uint64, record receipt.Record) task.ReplyUnit {
	return task.ReplyUnit{
		Owner:                    scope,
		TaskID:                   identity,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: expectedLifecycle,
		QuestionID:               "question-1",
		ExpectedQuestionVersion:  expectedQuestion,
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator},
		Answer:                   "Target main.",
		Initiator:                testPrincipal,
		ExpectedPipelineVersion:  expectedPipeline,
		Snapshot:                 testSnapshot([]byte("kind: Pipeline\nversion: 2\n")),
		Actions:                  []task.AdmittedAction{testAction(0)},
		ExecutionPrincipal:       "execution-principal-2",
		NewExecutionID:           executionID,
		At:                       testNow.Add(time.Minute),
		Receipt:                  record,
		Audit:                    []event.Audit{f.audit(scope, "task.reply")},
		Events:                   []event.Envelope{f.event(scope, string(executionID), 1)},
	}
}

// cancelUnit builds one complete cancel commit unit carrying the Kernel cancel
// reduction of the task's current execution.
func (f *fixture) cancelUnit(t *testing.T, scope tenancy.Scope, identity id.TaskID, expectedLifecycle uint64, record receipt.Record) task.CancelUnit {
	t.Helper()
	aggregate, commit := f.cancellationCommit(t, scope, identity)
	return task.CancelUnit{
		Owner:                    scope,
		TaskID:                   identity,
		ExpectedLifecycleVersion: expectedLifecycle,
		ExpectedExecutionVersion: aggregate.Version,
		Commit:                   commit,
		Receipt:                  record,
		Audit:                    []event.Audit{f.audit(scope, "task.cancel")},
		Events:                   []event.Envelope{f.event(scope, string(identity), expectedLifecycle+1)},
	}
}

// storedTask reads one committed task row through the neutral seam.
func (f *fixture) storedTask(t *testing.T, scope tenancy.Scope, identity id.TaskID) store.Row {
	t.Helper()
	row, ok := f.store.Snapshot(scope, "tasks", string(identity))
	if !ok {
		t.Fatalf("task %q is not committed", identity)
	}
	return row
}

// storedRow reads one committed row through the neutral seam.
func (f *fixture) storedRow(t *testing.T, scope tenancy.Scope, table, identity string) (store.Row, bool) {
	t.Helper()
	return f.store.Snapshot(scope, table, identity)
}

// scanRow reads one committed row by an equality set through the scanner. It
// is used for publication tables (receipts) that Snapshot does not expose.
func (f *fixture) scanRow(t *testing.T, scope tenancy.Scope, table string, equals map[string]any) (store.Row, bool) {
	t.Helper()
	page, err := f.store.Query(context.Background(), scope, store.ScanQuery{
		Table:   table,
		Equals:  equals,
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	if len(page.Rows) == 0 {
		return store.Row{}, false
	}
	return page.Rows[0], true
}

// countRows counts committed rows matching one equality set.
func (f *fixture) countRows(t *testing.T, scope tenancy.Scope, table string, equals map[string]any) int {
	t.Helper()
	total := 0
	cursor := ""
	for {
		page, err := f.store.Query(context.Background(), scope, store.ScanQuery{
			Table:   table,
			Equals:  equals,
			OrderBy: []store.Order{{Column: "id"}},
			After:   cursor,
			Limit:   store.MaxQueryLimit,
		})
		if err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		total += len(page.Rows)
		if !page.HasMore {
			return total
		}
		cursor = page.Next
	}
}

// mustClass asserts one fault class.
func mustClass(t *testing.T, err error, class fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error; got nil", class)
	}
	actual, ok := fault.ClassOf(err)
	if !ok || actual != class {
		t.Fatalf("expected fault class %s; got %v", class, err)
	}
}
