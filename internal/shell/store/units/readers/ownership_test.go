package readers_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// retainedSink is a deterministic retained-log sink that records the scoped
// read request and serves one chunk.
type retainedSink struct {
	mu    sync.Mutex
	reads []log.ReadRequest
}

func (sink *retainedSink) Append(context.Context, tenancy.Scope, log.Chunk) (log.AppendResult, error) {
	return log.AppendResult{}, nil
}

func (sink *retainedSink) Read(_ context.Context, request log.ReadRequest) (log.ReadResult, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.reads = append(sink.reads, request)
	return log.ReadResult{
		Chunks: []log.Chunk{{
			ExecutionID: request.ExecutionID,
			ActionID:    request.ActionID,
			Stream:      request.Stream,
			Offset:      0,
			Sequence:    1,
			Data:        []byte("redacted output"),
		}},
		NextOffset: 0,
	}, nil
}

func (sink *retainedSink) count() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.reads)
}

// logFixture owns the real log reader over the real identity adapter and the
// ownership reader under test.
type logFixture struct {
	store  *memory.Store
	tasks  *work.Task
	pipes  *work.Pipelines
	exec   *executionstore.Store
	reader *log.Reader
	sink   *retainedSink
	scope  tenancy.Scope
	taskID id.TaskID
	execID id.ExecutionID
}

func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	scope := testScope()
	backend := newStore()
	tasks, err := work.NewTask(backend, backend)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	pipes, err := work.NewPipelines(backend, backend, nil)
	if err != nil {
		t.Fatalf("NewPipelines: %v", err)
	}
	identityAdapter, err := identitystore.New(identitystore.Config{Executor: backend, Scanner: backend, Clock: serviceClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	ownership, err := readers.NewOwnershipReader(backend)
	if err != nil {
		t.Fatalf("NewOwnershipReader: %v", err)
	}
	seedPipeline(t, backend, scope, testPipeline, "build", []byte("actions:\n  - name: run\n"), 1)
	receipts := &receiptCounter{}
	taskID, execID := startedTask(t, tasks, pipes, receipts, scope)
	sink := &retainedSink{}
	reader, err := log.NewReader(log.ReaderConfig{
		Authorizer: allowAuthorizer{},
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Ownership:  ownership,
		Sink:       sink,
		Clock:      serviceClock{},
	})
	if err != nil {
		t.Fatalf("log.NewReader: %v", err)
	}
	return &logFixture{
		store:  backend,
		tasks:  tasks,
		pipes:  pipes,
		exec:   executionstore.New(backend, backend),
		reader: reader,
		sink:   sink,
		scope:  scope,
		taskID: taskID,
		execID: execID,
	}
}

// run makes the queued execution a running one so the identity adapter resolves
// an enabled execution principal.
func (fixture *logFixture) run(t *testing.T) {
	t.Helper()
	_, err := fixture.exec.Transition(context.Background(), execution.TransitionUnit{
		Owner:                    fixture.scope,
		ExecutionID:              fixture.execID,
		ExpectedExecutionVersion: 1,
		Commit: flow.Commit{
			ExecutionID:              fixture.execID,
			ExpectedExecutionVersion: 1,
			NextExecutionState:       kernel.ExecutionRunning,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			At:                       testNow,
		},
	})
	if err != nil {
		t.Fatalf("run execution: %v", err)
	}
}

// executionActor is the execution principal of the fixture execution.
func (fixture *logFixture) executionActor() authorize.Principal {
	return authorize.Principal{
		ID:          id.PrincipalID("execution-principal-1"),
		Kind:        identity.KindExecution,
		Scope:       fixture.scope,
		Enabled:     true,
		ExecutionID: fixture.execID,
	}
}

// readCommand targets the fixture execution's first action.
func (fixture *logFixture) readCommand() log.ReadCommand {
	return log.ReadCommand{
		Actor:       fixture.executionActor(),
		Scope:       fixture.scope,
		ExecutionID: fixture.execID,
		ActionID:    id.ActionID("act-1"),
		Stream:      log.StreamStdout,
	}
}

// TestOwnershipReaderBindsExecutionToItsTask proves the reader returns the
// owning task and that task's current execution.
func TestOwnershipReaderBindsExecutionToItsTask(t *testing.T) {
	fixture := newLogFixture(t)
	ownership, err := readers.NewOwnershipReader(fixture.store)
	if err != nil {
		t.Fatalf("NewOwnershipReader: %v", err)
	}
	binding, err := ownership.Ownership(context.Background(), fixture.scope, fixture.execID)
	if err != nil {
		t.Fatalf("Ownership: %v", err)
	}
	if binding.TaskID != fixture.taskID || binding.CurrentExecution != fixture.execID {
		t.Fatalf("binding = %+v; want task %q current %q", binding, fixture.taskID, fixture.execID)
	}
}

// TestOwnershipReaderScopeIsolationAndAbsence proves a cross-scope or unknown
// execution is not_found, and a task-free execution has no association.
func TestOwnershipReaderScopeIsolationAndAbsence(t *testing.T) {
	fixture := newLogFixture(t)
	ownership, err := readers.NewOwnershipReader(fixture.store)
	if err != nil {
		t.Fatalf("NewOwnershipReader: %v", err)
	}
	if _, err := ownership.Ownership(context.Background(), otherScope(), fixture.execID); !isClass(err, fault.NotFound) {
		t.Fatalf("cross-scope ownership error = %v; want not_found", err)
	}
	if _, err := ownership.Ownership(context.Background(), fixture.scope, "execution-missing"); !isClass(err, fault.NotFound) {
		t.Fatalf("absent ownership error = %v; want not_found", err)
	}
	// A task-free execution row carries no task association.
	_, err = fixture.store.Execute(context.Background(), fixture.scope, store.Unit{
		Writes: []store.Write{{
			Table:  "executions",
			ID:     "execution-task-free",
			Insert: true,
			Values: map[string]any{
				"pipeline_id":  nil,
				"task_id":      nil,
				"principal_id": "execution-principal-2",
				"state":        string(kernel.ExecutionRunning),
				"version":      int64(1),
				"yaml_sha256":  []byte("sha256"),
				"snapshot":     []byte(`{}`),
				"workspace_id": "ws-task-free",
				"created_at":   testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("insert task-free execution: %v", err)
	}
	if _, err := ownership.Ownership(context.Background(), fixture.scope, "execution-task-free"); !isClass(err, fault.NotFound) {
		t.Fatalf("task-free ownership error = %v; want not_found", err)
	}
}

// TestRealLogReaderReadsOwnTaskThroughOwnershipReader drives the real log
// service onto the ownership reader: the current execution of its own task may
// read, a stale execution and a foreign scope are refused before the sink.
func TestRealLogReaderReadsOwnTaskThroughOwnershipReader(t *testing.T) {
	fixture := newLogFixture(t)
	fixture.run(t)
	if _, err := fixture.reader.Read(context.Background(), fixture.readCommand()); err != nil {
		t.Fatalf("own-task log read: %v", err)
	}
	if fixture.sink.count() != 1 {
		t.Fatalf("sink reads = %d; want one", fixture.sink.count())
	}

	// A second start makes the first execution stale; its principal may no
	// longer read the task's retained content.
	startSecondExecution(t, fixture)
	if _, err := fixture.reader.Read(context.Background(), fixture.readCommand()); !isClass(err, fault.Forbidden) {
		t.Fatalf("stale execution log read error = %v; want forbidden", err)
	}
	if fixture.sink.count() != 1 {
		t.Fatalf("sink reads after stale refusal = %d; want still one", fixture.sink.count())
	}
}

// startSecondExecution commits a second queued execution through the real work
// adapter, moving the task's current execution.
func startSecondExecution(t *testing.T, fixture *logFixture) {
	t.Helper()
	definitionValue, err := fixture.pipes.Get(context.Background(), fixture.scope, testPipeline)
	if err != nil {
		t.Fatalf("read pipeline: %v", err)
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: definitionValue.YAML, TaskID: fixture.taskID})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	container := definition.Container{Image: integrationImg, Commands: []string{"echo", "ok"}}
	record := receipt.Record{
		ID:          "receipt-second-start",
		Key:         receipt.Key{TenantID: fixture.scope.TenantID, ProjectID: fixture.scope.ProjectID, PrincipalID: testPrincipal, Operation: "task.start", CallerKey: "second-start"},
		ContentHash: contentHashA,
		At:          testNow,
	}
	if _, err := fixture.tasks.Start(context.Background(), task.StartUnit{
		Owner:                    fixture.scope,
		TaskID:                   fixture.taskID,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: 2,
		ExpectedPipelineVersion:  1,
		Snapshot:                 snapshot,
		Actions: []pipeline.AdmittedAction{{
			Index:     0,
			Name:      id.ActionName("run"),
			Kind:      definition.ActionKindContainer,
			Timeout:   time.Minute,
			Container: &container,
		}},
		Initiator:          testPrincipal,
		ExecutionPrincipal: id.PrincipalID("execution-principal-1"),
		NewExecutionID:     "execution-2",
		At:                 testNow,
		Receipt:            record,
	}); err != nil {
		t.Fatalf("start second execution: %v", err)
	}
}
