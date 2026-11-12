// Package enginetest exercises the durable reconciliation engine through the
// Kernel ports only. It lives in a subpackage of internal/kernel/engine
// because the architecture import checker treats an in-directory external
// test package as part of the engine package, and the testkit legitimately
// imports engine.
//
// Engine API under test:
//
//	func New(state State, runtime Runtime, workspace Workspace, clock Clock) *Engine
//	func (e *Engine) Reconcile(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error)
//	func (e *Engine) Scan(ctx context.Context) error
//	func (e *Engine) SubmitOutputs(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, outputs kernel.Outputs) error
//	func (e *Engine) SettleRequest(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, answer kernel.Answer) error
//	func (e *Engine) Cancel(ctx context.Context, executionID id.ExecutionID, reason flow.CancelReason) error
//	func (e *Engine) SetCleanupPolicy(policy CleanupPolicy)
//
// Reconcile loads the persisted execution, observes any outstanding external
// effect before deciding to retry it, reduces one explicit fact with the
// injected authoritative clock, compare-and-commits the complete transition and
// then performs only the committed effects. It may drive several steps in one
// call; tests use reconcileUntil to tolerate either one-step or
// run-to-quiescence implementations. A crash between commit and effect is
// recovered by a later Reconcile/Scan; a stale transition is never replayed and
// a launch is never retried on unknown evidence.
//
// Scan asks the state port for due and unfinished work and reconciles each id.
// SubmitOutputs and SettleRequest apply the output/answer rules through
// the reducer and compare-and-commit boundary. Cancel commits the cancellation
// transition; stop and cleanup effects remain recoverable through
// Reconcile/Scan.
package enginetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// testNow is the deterministic authoritative time for every fixture.
var testNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// ---------------------------------------------------------------------------
// Durable fixtures
// ---------------------------------------------------------------------------

func newExecution(executionID, workspaceID string, actions ...kernel.Action) kernel.Execution {
	return newExecutionWithInputs(executionID, workspaceID, map[string]string{"repository": "example/repo"}, actions...)
}

func newExecutionWithInputs(executionID, workspaceID string, inputs map[string]string, actions ...kernel.Action) kernel.Execution {
	return kernel.Execution{
		ID:            id.ExecutionID(executionID),
		State:         kernel.ExecutionQueued,
		WorkspaceID:   id.WorkspaceID(workspaceID),
		Snapshot:      snapshotFor(actions, inputs),
		Actions:       append([]kernel.Action(nil), actions...),
		CurrentAction: -1,
	}
}

func snapshotFor(actions []kernel.Action, inputs map[string]string) *definition.Snapshot {
	pipeline := definition.Pipeline{Inputs: []id.InputKey{"repository"}}
	for _, action := range actions {
		declared := definition.Action{Name: action.Name, Timeout: action.Timeout}
		if action.Container != nil {
			container := action.Container.Definition
			declared.Container = &container
		}
		if action.Request != nil {
			request := action.Request.Definition
			declared.Request = &request
		}
		pipeline.Actions = append(pipeline.Actions, declared)
	}
	return definition.NewSnapshot(definition.SnapshotInput{
		Pipeline: pipeline,
		YAML:     []byte("name: test\n"),
		Inputs:   inputs,
	})
}

func containerAction(actionID, name string, index int, timeout time.Duration, outputs ...id.OutputKey) kernel.Action {
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindContainer,
		State:   kernel.ActionPending,
		Timeout: timeout,
		Container: &kernel.ContainerAction{
			Definition: definition.Container{
				Image:    "registry.example/app@sha256:abc",
				Commands: []string{"run"},
				Outputs:  outputs,
			},
		},
	}
}

func requestAction(actionID, name string, index int, timeout time.Duration, subject definition.Subject, outputs ...id.OutputKey) kernel.Action {
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindRequest,
		State:   kernel.ActionPending,
		Timeout: timeout,
		Request: &kernel.RequestAction{
			Definition: definition.Request{
				Assignment: "assignee",
				Subject:    subject,
				Outputs:    outputs,
			},
		},
	}
}

func literalSubject(fields map[string]string) definition.Subject {
	values := make(map[string]definition.SubjectValue, len(fields))
	for name, value := range fields {
		values[name] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: value}
	}
	return definition.NewSubject(values)
}

func inputReference(key string) definition.SubjectValue {
	return definition.SubjectValue{
		Kind:      definition.SubjectReference,
		Reference: definition.Reference{Input: true, Key: key},
	}
}

func actionReference(action, key string) definition.SubjectValue {
	return definition.SubjectValue{
		Kind:      definition.SubjectReference,
		Reference: definition.Reference{Action: action, Key: key},
	}
}

// ---------------------------------------------------------------------------
// Recording runtime
// ---------------------------------------------------------------------------

// recordingRuntime delegates every effect to the testkit fake while recording the
// calls the engine makes. It never fabricates success: the fake still owns the
// uncertainty, stop and cleanup guards.
type recordingRuntime struct {
	*testkit.Runtime

	mu          sync.Mutex
	prepares    []engine.PrepareRequest
	starts      []engine.StartRequest
	observes    []engine.ObserveRequest
	stops       []engine.StopRequest
	removes     []engine.RemoveRequest
	credentials []engine.CredentialRequest
	removeErr   error

	// beforePrepare and afterPrepare let a test order a concurrent fact around
	// the external preparation step. They are set before the driving goroutine
	// starts, so no additional synchronization is needed.
	beforePrepare func(context.Context)
	afterPrepare  func(context.Context)
}

func (r *recordingRuntime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	r.mu.Lock()
	r.prepares = append(r.prepares, request)
	before := r.beforePrepare
	after := r.afterPrepare
	r.mu.Unlock()
	if before != nil {
		before(ctx)
	}
	preparation, err := r.Runtime.Prepare(ctx, request)
	if err == nil && after != nil {
		after(ctx)
	}
	return preparation, err
}

func (r *recordingRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	r.mu.Lock()
	r.starts = append(r.starts, request)
	r.mu.Unlock()
	return r.Runtime.Start(ctx, request)
}

func (r *recordingRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	r.observes = append(r.observes, request)
	r.mu.Unlock()
	observation, err := r.Runtime.Observe(ctx, request)
	return observation, translateNotPrepared(err)
}

func (r *recordingRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	r.mu.Lock()
	r.stops = append(r.stops, request)
	r.mu.Unlock()
	evidence, err := r.Runtime.Stop(ctx, request)
	return evidence, translateNotPrepared(err)
}

func (r *recordingRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.mu.Lock()
	r.removes = append(r.removes, request)
	removeErr := r.removeErr
	r.mu.Unlock()
	if removeErr != nil {
		return engine.Removal{}, removeErr
	}
	removal, err := r.Runtime.Remove(ctx, request)
	return removal, translateNotPrepared(err)
}

// translateNotPrepared maps the fake's testkit sentinel onto the engine's
// consumer-facing sentinel. Production adapters return either
// engine.ErrNotPrepared or a fault.NotFound, so the engine never imports the
// testkit.
func translateNotPrepared(err error) error {
	if errors.Is(err, testkit.ErrNotPrepared) {
		return engine.ErrNotPrepared
	}
	return err
}

func (r *recordingRuntime) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	r.mu.Lock()
	r.credentials = append(r.credentials, request)
	r.mu.Unlock()
	return r.Runtime.RefreshCredential(ctx, request)
}

func (r *recordingRuntime) prepareCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prepares)
}

func (r *recordingRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *recordingRuntime) observeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.observes)
}

func (r *recordingRuntime) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stops)
}

func (r *recordingRuntime) removeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.removes)
}

func (r *recordingRuntime) credentialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.credentials)
}

func (r *recordingRuntime) lastPrepare() engine.PrepareRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prepares) == 0 {
		return engine.PrepareRequest{}
	}
	return r.prepares[len(r.prepares)-1]
}

func (r *recordingRuntime) startRequests() []engine.StartRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.StartRequest(nil), r.starts...)
}

func (r *recordingRuntime) removedWorkloads() []id.WorkloadID {
	r.mu.Lock()
	defer r.mu.Unlock()
	workloads := make([]id.WorkloadID, 0, len(r.removes))
	for _, removal := range r.removes {
		workloads = append(workloads, removal.Identity.WorkloadID)
	}
	return workloads
}

func (r *recordingRuntime) setRemoveError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeErr = err
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t           *testing.T
	fixture     *testkit.Fixture
	runtime     *recordingRuntime
	engine      *engine.Engine
	executionID id.ExecutionID
	workspaceID id.WorkspaceID
}

func newHarness(t *testing.T, execution kernel.Execution) *harness {
	t.Helper()
	fixture := testkit.NewFixture(testNow)
	fixture.Store.Seed(execution)
	runtime := &recordingRuntime{Runtime: fixture.Runtime}
	h := &harness{
		t:           t,
		fixture:     fixture,
		runtime:     runtime,
		executionID: execution.ID,
		workspaceID: execution.WorkspaceID,
	}
	h.engine = engine.New(fixture.State, runtime, fixture.Workspace, fixture.Clock)
	return h
}

func (h *harness) load() kernel.Execution {
	h.t.Helper()
	return h.loadID(h.executionID)
}

func (h *harness) loadID(executionID id.ExecutionID) kernel.Execution {
	h.t.Helper()
	execution, err := h.fixture.State.Load(h.t.Context(), executionID)
	if err != nil {
		h.t.Fatalf("Load(%s) error = %v", executionID, err)
	}
	return execution
}

func (h *harness) reconcile() kernel.Execution {
	h.t.Helper()
	execution, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		h.t.Fatalf("Reconcile(%s) error = %v", h.executionID, err)
	}
	return execution
}

func (h *harness) reconcileUntil(description string, max int, predicate func(kernel.Execution) bool) kernel.Execution {
	h.t.Helper()
	for range max {
		execution := h.reconcile()
		if predicate(execution) {
			return execution
		}
	}
	last := h.load()
	h.t.Fatalf("execution %s did not reach %s after %d reconciles: %s", h.executionID, description, max, actionSummary(last))
	return last
}

func (h *harness) restart() {
	h.t.Helper()
	state := h.fixture.Restart()
	h.engine = engine.New(state, h.runtime, h.fixture.Workspace, h.fixture.Clock)
}

func (h *harness) cancel(reason flow.CancelReason) {
	h.t.Helper()
	if err := h.engine.Cancel(h.t.Context(), h.executionID, reason); err != nil {
		h.t.Fatalf("Cancel(%s) error = %v", h.executionID, err)
	}
}

func (h *harness) retainWorkspace() {
	h.t.Helper()
	if err := h.fixture.Workspace.Retain(h.t.Context(), h.workspaceID); err != nil {
		h.t.Fatalf("Retain(%s) error = %v", h.workspaceID, err)
	}
}

// prepareOnly materializes a workload directly through the testkit fake without
// starting it, reconstructing a prepared-but-never-started identity.
func (h *harness) prepareOnly(identity engine.EffectIdentity, container definition.Container) {
	h.t.Helper()
	if _, err := h.fixture.Runtime.Prepare(h.t.Context(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: h.workspaceID,
		Container:   container,
	}); err != nil {
		h.t.Fatalf("Prepare(%s) error = %v", identity.WorkloadID, err)
	}
}

// prepareAndStart materializes a workload directly through the testkit fake. Tests
// use it to reconstruct the persisted state a crashed controller would find;
// the recording wrapper is intentionally bypassed so its call counts only
// describe engine activity.
func (h *harness) prepareAndStart(identity engine.EffectIdentity, container definition.Container) {
	h.t.Helper()
	if _, err := h.fixture.Runtime.Prepare(h.t.Context(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: h.workspaceID,
		Container:   container,
	}); err != nil {
		h.t.Fatalf("Prepare(%s) error = %v", identity.WorkloadID, err)
	}
	result, err := h.fixture.Runtime.Start(h.t.Context(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: h.workspaceID,
	})
	if err != nil {
		h.t.Fatalf("Start(%s) error = %v", identity.WorkloadID, err)
	}
	if result.Outcome != engine.StartAccepted {
		h.t.Fatalf("Start(%s) outcome = %q, want accepted", identity.WorkloadID, result.Outcome)
	}
}

// confirmRuntimeStop stages affirmative termination evidence on the testkit fake
// and records it. A pending stop acknowledgment is never promoted.
func (h *harness) confirmRuntimeStop(identity engine.EffectIdentity) {
	h.t.Helper()
	h.fixture.Runtime.ConfirmNextStop("test: termination observed")
	evidence, err := h.fixture.Runtime.Stop(h.t.Context(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: h.workspaceID,
	})
	if err != nil {
		h.t.Fatalf("Stop(%s) error = %v", identity.WorkloadID, err)
	}
	if !evidence.IsConfirmed() {
		h.t.Fatalf("Stop(%s) evidence = %+v, want confirmed", identity.WorkloadID, evidence)
	}
}

func identityFor(executionID id.ExecutionID, action kernel.Action) engine.EffectIdentity {
	identity := engine.EffectIdentity{ExecutionID: executionID, ActionID: action.ID}
	if action.Container != nil {
		identity.WorkloadID = action.Container.WorkloadID
		identity.IdempotencyKey = action.Container.Launch.IdempotencyKey
	}
	return identity
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

func mustAction(t *testing.T, execution kernel.Execution, name id.ActionName) kernel.Action {
	t.Helper()
	action, ok := actionByName(execution, name)
	if !ok {
		t.Fatalf("execution %s has no action named %q: %s", execution.ID, name, actionSummary(execution))
	}
	return action
}

func actionByName(execution kernel.Execution, name id.ActionName) (kernel.Action, bool) {
	for _, action := range execution.Actions {
		if action.Name == name {
			return action, true
		}
	}
	return kernel.Action{}, false
}

func mustClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = (%q, %v) for %v, want %q", class, ok, err, want)
	}
}

func assertSingleActiveAction(t *testing.T, execution kernel.Execution) {
	t.Helper()
	running := 0
	for _, action := range execution.Actions {
		if action.State == kernel.ActionRunning {
			running++
		}
	}
	if running > 1 {
		t.Fatalf("execution has %d running actions: %s", running, actionSummary(execution))
	}
	if execution.CurrentAction >= 0 {
		if execution.CurrentAction >= len(execution.Actions) {
			t.Fatalf("CurrentAction = %d is outside the action list: %s", execution.CurrentAction, actionSummary(execution))
		}
		if execution.Actions[execution.CurrentAction].State != kernel.ActionRunning {
			t.Fatalf("current action %q is %q, want running: %s",
				execution.Actions[execution.CurrentAction].Name,
				execution.Actions[execution.CurrentAction].State,
				actionSummary(execution))
		}
	}
}

func assertNoPendingActions(t *testing.T, execution kernel.Execution) {
	t.Helper()
	for _, action := range execution.Actions {
		if action.State == kernel.ActionPending {
			t.Fatalf("action %q is still pending: %s", action.Name, actionSummary(execution))
		}
	}
}

func assertWorkloadStarted(t *testing.T, runtime *testkit.Runtime, workloadID id.WorkloadID) {
	t.Helper()
	workload, ok := runtime.Workload(workloadID)
	if !ok {
		t.Fatalf("runtime has no workload %q", workloadID)
	}
	if !workload.Started {
		t.Fatalf("workload %q was not started: %+v", workloadID, workload)
	}
}

func actionSummary(execution kernel.Execution) string {
	parts := make([]string, 0, len(execution.Actions))
	for _, action := range execution.Actions {
		parts = append(parts, fmt.Sprintf("%s=%s/v%d", action.Name, action.State, action.Version))
	}
	return fmt.Sprintf("state=%s current=%d cleanup=%s [%s]",
		execution.State, execution.CurrentAction, execution.Cleanup.State, strings.Join(parts, " "))
}

func containsWorkload(workloads []id.WorkloadID, target id.WorkloadID) bool {
	return slices.Contains(workloads, target)
}

func countWorkload(workloads []id.WorkloadID, target id.WorkloadID) int {
	count := 0
	for _, workload := range workloads {
		if workload == target {
			count++
		}
	}
	return count
}
