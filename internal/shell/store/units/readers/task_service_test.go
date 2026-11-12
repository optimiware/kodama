package readers_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// serviceClock is the task service's injected authoritative clock.
type serviceClock struct{}

func (serviceClock) Now() time.Time { return testNow }

// serviceIDs allocates deterministic unique identities.
type serviceIDs struct {
	mu        sync.Mutex
	task      int
	question  int
	execution int
	principal int
	event     int
	receipt   int
}

func (ids *serviceIDs) NewTaskID() (id.TaskID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.task++
	return id.TaskID(fmt.Sprintf("task-%d", ids.task)), nil
}

func (ids *serviceIDs) NewQuestionID() (task.QuestionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.question++
	return task.QuestionID(fmt.Sprintf("question-%d", ids.question)), nil
}

func (ids *serviceIDs) NewExecutionID() (id.ExecutionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.execution++
	return id.ExecutionID(fmt.Sprintf("execution-%d", ids.execution)), nil
}

func (ids *serviceIDs) NewPrincipalID() (id.PrincipalID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.principal++
	return id.PrincipalID(fmt.Sprintf("execution-principal-%d", ids.principal)), nil
}

func (ids *serviceIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.event++
	return id.EventID(fmt.Sprintf("event-%d", ids.event)), nil
}

func (ids *serviceIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipt)), nil
}

// allowAuthorizer is the grant-stage fake; current authority is still read
// through the real identity adapter.
type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionAllow, nil
}

// pipelineAdmission resolves the stored pipeline and builds the admitted
// snapshot; it is the only non-store dependency with real admission policy
// removed.
type pipelineAdmission struct {
	pipelines *work.Pipelines
	scope     tenancy.Scope
}

func (admission pipelineAdmission) Admit(ctx context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	current, err := admission.pipelines.Get(ctx, admission.scope, request.Definition.ID)
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         current.YAML,
		Inputs:       request.Inputs,
		TaskID:       request.Task.TaskID,
		Conversation: request.Task.Conversation,
	})
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	container := definition.Container{Image: integrationImg, Commands: []string{"echo", "ok"}}
	return pipeline.AdmissionResult{
		PipelineID:      current.ID,
		PipelineVersion: current.Version,
		Snapshot:        snapshot,
		Actions: []pipeline.AdmittedAction{{
			Index:     0,
			Name:      id.ActionName("run"),
			Kind:      definition.ActionKindContainer,
			Timeout:   time.Minute,
			Container: &container,
		}},
	}, nil
}

// recordingStarts delegates to the real work adapter and records the commit
// unit so the test can observe the post-commit dispatch intent.
type recordingStarts struct {
	inner task.StartPort
	mu    sync.Mutex
	units []task.StartUnit
}

func (recorder *recordingStarts) Start(ctx context.Context, unit task.StartUnit) (task.StartResult, error) {
	recorder.mu.Lock()
	recorder.units = append(recorder.units, unit)
	recorder.mu.Unlock()
	return recorder.inner.Start(ctx, unit)
}

func (recorder *recordingStarts) last(t *testing.T) task.StartUnit {
	t.Helper()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.units) == 0 {
		t.Fatal("no start unit was committed")
	}
	return recorder.units[len(recorder.units)-1]
}

// taskServiceEnv wires the real task service onto the work, identity and
// execution adapters and the reader under test over one memory store.
type taskServiceEnv struct {
	store    *memory.Store
	service  *task.Service
	exec     *executionstore.Store
	starts   *recordingStarts
	scope    tenancy.Scope
	actor    authorize.Principal
	execID   id.ExecutionID
	taskID   id.TaskID
	pipeline pipeline.PipelineID
}

func newTaskServiceEnv(t *testing.T) *taskServiceEnv {
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
	execReader, err := readers.NewExecutionReader(backend, backend)
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	seedPipeline(t, backend, scope, testPipeline, "build", []byte("actions:\n  - name: run\n"), 1)
	seedMembership(t, backend, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer)
	starts := &recordingStarts{inner: tasks}
	execStore := executionstore.New(backend, backend)
	service, err := task.NewService(task.ServiceConfig{
		Clock:          serviceClock{},
		IDs:            &serviceIDs{},
		Authorizer:     allowAuthorizer{},
		Authority:      identityAdapter,
		Principals:     identityAdapter,
		Pipelines:      pipes,
		Reader:         tasks,
		Executions:     execReader,
		Aggregates:     execStore,
		Admission:      pipelineAdmission{pipelines: pipes, scope: scope},
		Creates:        tasks,
		Edits:          tasks,
		Starts:         starts,
		Asks:           tasks,
		Replies:        tasks,
		Cancels:        tasks,
		Progress:       tasks,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("task.NewService: %v", err)
	}
	return &taskServiceEnv{
		store:    backend,
		service:  service,
		exec:     execStore,
		starts:   starts,
		scope:    scope,
		actor:    authorize.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}},
		pipeline: testPipeline,
	}
}

// createAndStart commits one open task with a queued execution through the real
// service.
func (env *taskServiceEnv) createAndStart(t *testing.T) {
	t.Helper()
	created, err := env.service.Create(context.Background(), task.CreateCommand{
		Actor:      env.actor,
		Scope:      env.scope,
		Title:      "Fix the build",
		PipelineID: env.pipeline,
		Start:      true,
		CallerKey:  "task-create",
		Content:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"create":"task"}`)},
	})
	if err != nil {
		t.Fatalf("task create-and-start: %v", err)
	}
	env.taskID = created.TaskID
	env.execID = created.ExecutionID
}

// writeCleanup commits one cleanup row and the execution state the same Kernel
// commit persists through the real execution adapter.
func (env *taskServiceEnv) writeCleanup(t *testing.T, version uint64, state kernel.CleanupState, next kernel.ExecutionState) uint64 {
	t.Helper()
	result, err := env.exec.Transition(context.Background(), execution.TransitionUnit{
		Owner:                    env.scope,
		ExecutionID:              env.execID,
		ExpectedExecutionVersion: version,
		Commit: flow.Commit{
			ExecutionID:              env.execID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       next,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate: &flow.CleanupUpdate{
				State:       state,
				WorkloadID:  "workload-1",
				WorkspaceID: snapshotdoc.DeriveWorkspaceID(env.execID),
				Attempts:    1,
			},
			At: testNow,
		},
	})
	if err != nil {
		t.Fatalf("commit cleanup %q: %v", state, err)
	}
	return result.ExecutionVersion
}

// writeStop commits one persisted stop barrier on the execution's first action
// and the execution state the same Kernel commit persists.
func (env *taskServiceEnv) writeStop(t *testing.T, version, actionVersion uint64, evidence kernel.StopEvidence, next kernel.ExecutionState) uint64 {
	t.Helper()
	return writeStopEvidence(t, env.exec, env.scope, env.execID, version, actionVersion, evidence, next)
}

// hasDispatch reports whether the unit carries the post-commit dispatch intent.
func hasDispatch(effects []event.PostCommitEffect) bool {
	for _, effect := range effects {
		if effect.Kind == event.PostDispatch {
			return true
		}
	}
	return false
}

// TestRealTaskServiceDispatchWaitsForPredecessorStopEvidence drives the real
// task service onto the execution reader. A second start must dispatch only
// after the predecessor's persisted stop evidence is confirmed.
func TestRealTaskServiceDispatchWaitsForPredecessorStopEvidence(t *testing.T) {
	cases := map[string]struct {
		cleanup      kernel.CleanupState
		next         kernel.ExecutionState
		wantDispatch bool
	}{
		"predecessor cleanup pending":  {cleanup: kernel.CleanupPending, next: kernel.ExecutionRunning, wantDispatch: false},
		"predecessor cleanup complete": {cleanup: kernel.CleanupComplete, next: kernel.ExecutionSucceeded, wantDispatch: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTaskServiceEnv(t)
			env.createAndStart(t)
			env.writeCleanup(t, 1, testCase.cleanup, testCase.next)
			_, err := env.service.Start(context.Background(), task.StartCommand{
				Actor:                    env.actor,
				Scope:                    env.scope,
				TaskID:                   env.taskID,
				ExpectedLifecycleVersion: 2,
				CallerKey:                "task-start-2",
				Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"start":"second"}`)},
			})
			if err != nil {
				t.Fatalf("second task start: %v", err)
			}
			unit := env.starts.last(t)
			if hasDispatch(unit.PostCommit) != testCase.wantDispatch {
				t.Fatalf("dispatch = %v; want %v for %+v stop evidence", hasDispatch(unit.PostCommit), testCase.wantDispatch, testCase.cleanup)
			}
		})
	}
}

// TestRealTaskServiceDoesNotDispatchOverALivePredecessor proves the reader, not
// the task service, is the double-run guard: a RUNNING predecessor between
// actions already carries a confirmed per-action stop barrier, yet it may still
// start its next action, so a second start must not dispatch a replacement. The
// same confirmed barrier on a terminal execution still dispatches.
func TestRealTaskServiceDoesNotDispatchOverALivePredecessor(t *testing.T) {
	cases := map[string]struct {
		next         kernel.ExecutionState
		wantDispatch bool
	}{
		"predecessor running with a completed action": {next: kernel.ExecutionRunning, wantDispatch: false},
		"predecessor succeeded":                       {next: kernel.ExecutionSucceeded, wantDispatch: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTaskServiceEnv(t)
			env.createAndStart(t)
			env.writeStop(t, 1, 1, kernel.ConfirmedStop("runtime confirmed termination"), testCase.next)
			_, err := env.service.Start(context.Background(), task.StartCommand{
				Actor:                    env.actor,
				Scope:                    env.scope,
				TaskID:                   env.taskID,
				ExpectedLifecycleVersion: 2,
				CallerKey:                "task-start-2",
				Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"start":"second"}`)},
			})
			if err != nil {
				t.Fatalf("second task start: %v", err)
			}
			unit := env.starts.last(t)
			if hasDispatch(unit.PostCommit) != testCase.wantDispatch {
				t.Fatalf("dispatch = %v; want %v for a %s predecessor with confirmed stop evidence", hasDispatch(unit.PostCommit), testCase.wantDispatch, testCase.next)
			}
		})
	}
}
