package executionstore

// This file wires the real execution and request services onto the store
// adapters over the memory store. Hand-built commit units hid a class of
// defect: the Kernel reconciler activates a first-position request on a queued
// execution through Engine.Commit, and a Shell activation port emits authority
// reads the adapter must accept exactly. The services and the Kernel engine are
// the source of truth for every shape the adapters persist.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// integrationClock is the injected service and engine clock.
type integrationClock struct{ now time.Time }

func (clock integrationClock) Now() time.Time { return clock.now }

// integrationIDs supplies deterministic unique identities to both services.
type integrationIDs struct {
	mu        sync.Mutex
	execution int
	event     int
	receipt   int
}

func (ids *integrationIDs) NewExecutionID() (id.ExecutionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.execution++
	return id.ExecutionID(fmt.Sprintf("execution-%d", ids.execution)), nil
}

func (ids *integrationIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.event++
	return id.EventID(fmt.Sprintf("event-%d", ids.event)), nil
}

func (ids *integrationIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipt)), nil
}

// confirmAuthority answers exactly the reads the request service asks, with the
// expected version, so the commit reaches the adapter and its real
// in-transaction mapping. The adapter re-reads every fact from the store.
type confirmAuthority struct{}

func (confirmAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     true,
		}
	}
	return facts, nil
}

// unusedRuntime is the Kernel workload port for request-only flows. Any call is
// a test bug: a request owns no workload.
type unusedRuntime struct{}

func (unusedRuntime) Capabilities(context.Context) (engine.Capabilities, error) {
	return engine.Capabilities{}, nil
}

func (unusedRuntime) Prepare(context.Context, engine.PrepareRequest) (engine.Preparation, error) {
	return engine.Preparation{}, fault.New(fault.UnsupportedCapability, "request flow must not prepare a workload")
}

func (unusedRuntime) Start(context.Context, engine.StartRequest) (engine.StartResult, error) {
	return engine.StartResult{}, fault.New(fault.UnsupportedCapability, "request flow must not start a workload")
}

func (unusedRuntime) Observe(context.Context, engine.ObserveRequest) (kernel.Observation, error) {
	return kernel.Observation{}, fault.New(fault.UnsupportedCapability, "request flow must not observe a workload")
}

func (unusedRuntime) Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error) {
	return engine.LogBatch{}, fault.New(fault.UnsupportedCapability, "request flow must not read logs")
}

func (unusedRuntime) Stop(context.Context, engine.StopRequest) (kernel.StopEvidence, error) {
	return kernel.StopEvidence{}, fault.New(fault.UnsupportedCapability, "request flow must not stop a workload")
}

func (unusedRuntime) Remove(context.Context, engine.RemoveRequest) (engine.Removal, error) {
	return engine.Removal{}, fault.New(fault.UnsupportedCapability, "request flow must not remove a workload")
}

func (unusedRuntime) RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error) {
	return engine.CredentialRefresh{}, fault.New(fault.UnsupportedCapability, "request flow must not refresh a credential")
}

// retainedWorkspace reports the retained execution workspace as available.
type retainedWorkspace struct{}

func (retainedWorkspace) Status(_ context.Context, workspaceID id.WorkspaceID) (engine.WorkspaceObservation, error) {
	return engine.WorkspaceObservation{WorkspaceID: workspaceID, Available: true}, nil
}

func (retainedWorkspace) Retain(context.Context, id.WorkspaceID) error  { return nil }
func (retainedWorkspace) Release(context.Context, id.WorkspaceID) error { return nil }

// humanActor is one enabled human principal with the supplied roles.
func humanActor(scope tenancy.Scope, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      "user-1",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

// approvalAction is one request action whose resolved subject carries the
// approval operation and resource beside an effect field.
func approvalAction(index int, name string) pipeline.AdmittedAction {
	return pipeline.AdmittedAction{
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindRequest,
		Timeout: 60 * time.Second,
		Request: &definition.Request{
			Assignment: "role/reviewer",
			Subject: definition.NewSubject(map[string]definition.SubjectValue{
				"operation": {Kind: definition.SubjectLiteral, Literal: "deploy"},
				"resource":  {Kind: definition.SubjectLiteral, Literal: "res-1"},
				"artifact":  {Kind: definition.SubjectLiteral, Literal: "artifact-1"},
			}),
			Outputs: []id.OutputKey{"decision"},
		},
	}
}

// newRealExecutionService wires the real execution service onto the adapter.
func newRealExecutionService(t *testing.T, adapter *Store, ids *integrationIDs) *execution.Service {
	t.Helper()
	service, err := execution.NewService(execution.Config{
		Clock:       integrationClock{now: testNow},
		IDs:         ids,
		Authorizer:  authorize.NewMatrix(),
		Authority:   confirmAuthority{},
		Reader:      adapter,
		State:       adapter,
		Transitions: adapter,
		Starts:      adapter,
	})
	if err != nil {
		t.Fatalf("execution.NewService: %v", err)
	}
	return service
}

// newRealRequestService wires the real request service onto the adapter.
func newRealRequestService(t *testing.T, adapter *Store, ids *integrationIDs) *request.Service {
	t.Helper()
	service, err := request.NewService(request.Config{
		Clock:       integrationClock{now: testNow},
		IDs:         ids,
		Authorizer:  authorize.NewMatrix(),
		Authority:   confirmAuthority{},
		State:       adapter,
		Activations: adapter,
		Resolutions: adapter,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}
	return service
}

// startThroughService starts one execution through the real service.
func startThroughService(t *testing.T, service *execution.Service, scope tenancy.Scope, actions []pipeline.AdmittedAction, callerKey string) execution.StartResult {
	t.Helper()
	result, err := service.Start(context.Background(), execution.StartCommand{
		Actor: humanActor(scope, tenancy.RoleEngineer, tenancy.RoleReviewer),
		Scope: scope,
		Admission: pipeline.AdmissionResult{
			PipelineID:      pipelineIDFor(scope),
			PipelineVersion: 3,
			Snapshot:        testSnapshot("", nil),
			Actions:         actions,
		},
		Initiator:          "user-1",
		ExecutionPrincipal: "exec-principal-1",
		CallerKey:          callerKey,
		Content:            receipt.Content{MediaType: "application/json", Bytes: []byte(`{"start":"` + callerKey + `"}`)},
	})
	if err != nil {
		t.Fatalf("execution.Service.Start: %v", err)
	}
	return result
}

// decodeAssignment decodes one stored request_assignment column.
func decodeAssignment(t *testing.T, value any) request.Assignment {
	t.Helper()
	encoded, ok := value.([]byte)
	if !ok || len(encoded) == 0 {
		t.Fatalf("request_assignment column is not decodable bytes: %v", value)
	}
	var assignment request.Assignment
	if err := json.Unmarshal(encoded, &assignment); err != nil {
		t.Fatalf("request_assignment does not decode: %v", err)
	}
	if !assignment.Valid() {
		t.Fatalf("request_assignment is not a valid assignment: %+v", assignment)
	}
	return assignment
}

// TestRealServicesStartActivateResolveAndReadEvidence is the end-to-end
// regression: the real execution service starts a queued request-only
// execution, the real Kernel reconciler activates the request through
// Engine.Commit, the real request service settles a pass, and the platform
// evidence reader decodes the assignment, subject and decision columns.
func TestRealServicesStartActivateResolveAndReadEvidence(t *testing.T) {
	ctx := context.Background()
	backend := memory.NewWithClock(func() time.Time { return testNow })
	adapter := New(backend, backend)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}, 1)
	ids := &integrationIDs{}
	execService := newRealExecutionService(t, adapter, ids)
	requestService := newRealRequestService(t, adapter, ids)

	start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{approvalAction(0, "approve")}, "start-1")
	actionID := snapshotdoc.DeriveActionID(start.ExecutionID, 0)

	// The insert itself must carry the immutable assignment, before any
	// activation: an engine-activated row is never written by Store.Activate.
	row, err := backend.Read(ctx, scope, "actions", string(actionID))
	if err != nil {
		t.Fatalf("read started action: %v", err)
	}
	wantAssignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}
	if assignment := decodeAssignment(t, row.Values["request_assignment"]); assignment != wantAssignment {
		t.Fatalf("started assignment = %+v; want %+v", assignment, wantAssignment)
	}
	if row.Values["request_subject"] != nil {
		t.Fatalf("pending request already carries a subject: %v", row.Values["request_subject"])
	}

	// The real reconciler activates the first pending request on the queued
	// execution exactly as the Kernel does.
	reconciler := engine.New(NewEngine(backend.Controller()), unusedRuntime{}, retainedWorkspace{}, integrationClock{now: testNow})
	activated, err := reconciler.Reconcile(ctx, start.ExecutionID)
	if err != nil {
		t.Fatalf("engine.Reconcile: %v", err)
	}
	if activated.State != kernel.ExecutionRunning || activated.CurrentAction != 0 {
		t.Fatalf("activated execution = %+v; want running with current action 0", activated)
	}
	action := activated.Actions[0]
	if action.State != kernel.ActionRunning || action.Request == nil || action.Request.Subject == nil {
		t.Fatalf("activated action = %+v; want a running request with a subject", action)
	}
	if !action.Deadline.Equal(testNow.Add(60 * time.Second)) {
		t.Fatalf("deadline = %s; want %s", action.Deadline, testNow.Add(60*time.Second))
	}

	row, err = backend.Read(ctx, scope, "actions", string(actionID))
	if err != nil {
		t.Fatalf("read activated action: %v", err)
	}
	if assignment := decodeAssignment(t, row.Values["request_assignment"]); assignment != wantAssignment {
		t.Fatalf("activated assignment = %+v; want %+v", assignment, wantAssignment)
	}
	if len(row.Values["request_subject"].([]byte)) == 0 {
		t.Fatal("engine activation did not persist the resolved subject")
	}

	// The real request service settles the pass.
	resolved, err := requestService.Resolve(ctx, request.ResolveCommand{
		Actor:                    humanActor(scope, tenancy.RoleReviewer),
		Scope:                    scope,
		ExecutionID:              start.ExecutionID,
		ActionID:                 actionID,
		ExpectedExecutionVersion: activated.Version,
		ExpectedActionVersion:    action.Version,
		Decision:                 request.DecisionPass,
		Response:                 kernel.Outputs{"decision": "yes"},
	})
	if err != nil {
		t.Fatalf("request.Service.Resolve: %v", err)
	}
	if resolved.Decision != request.DecisionPass || resolved.Replayed {
		t.Fatalf("resolve result = %+v; want a fresh pass", resolved)
	}

	// The platform evidence reader decodes every column it needs.
	reader, err := platform.NewEvidenceReader(backend)
	if err != nil {
		t.Fatalf("platform.NewEvidenceReader: %v", err)
	}
	evidence, err := reader.ReadEvidence(ctx, verify.EvidenceQuery{
		Scope:       scope,
		ExecutionID: start.ExecutionID,
		Assignment:  wantAssignment,
		Operation:   "deploy",
		ResourceID:  "res-1",
	})
	if err != nil {
		t.Fatalf("EvidenceReader.ReadEvidence: %v", err)
	}
	if len(evidence) != 1 {
		t.Fatalf("evidence = %d; want 1", len(evidence))
	}
	fact := evidence[0]
	if fact.ActionID != actionID || fact.Decision != request.DecisionPass {
		t.Fatalf("evidence = %+v; want action %q with a pass", fact, actionID)
	}
	if fact.Assignment != wantAssignment {
		t.Fatalf("evidence assignment = %+v; want %+v", fact.Assignment, wantAssignment)
	}
	if fact.Operation != "deploy" || fact.ResourceID != "res-1" {
		t.Fatalf("evidence approval = %q/%q; want deploy/res-1", fact.Operation, fact.ResourceID)
	}
	if !fact.ResolvedAt.Equal(testNow) {
		t.Fatalf("evidence resolved_at = %s; want %s", fact.ResolvedAt, testNow)
	}
	if field, ok := fact.Subject.Field("artifact"); !ok || field.Value != "artifact-1" {
		t.Fatalf("evidence subject = %+v; want artifact-1", fact.Subject)
	}
}

// TestRealRequestServiceActivatesRunningSuccessorThroughAdapter proves the
// adapter accepts exactly what the Shell activation port emits for a running
// execution: the reducer-derived subject, assignment and deadline.
func TestRealRequestServiceActivatesRunningSuccessorThroughAdapter(t *testing.T) {
	ctx := context.Background()
	backend := memory.NewWithClock(func() time.Time { return testNow })
	adapter := New(backend, backend)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}, 1)
	ids := &integrationIDs{}
	execService := newRealExecutionService(t, adapter, ids)
	requestService := newRealRequestService(t, adapter, ids)

	start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{
		approvalAction(0, "approve"),
		approvalAction(1, "confirm"),
	}, "start-1")
	firstAction := snapshotdoc.DeriveActionID(start.ExecutionID, 0)
	secondAction := snapshotdoc.DeriveActionID(start.ExecutionID, 1)

	reconciler := engine.New(NewEngine(backend.Controller()), unusedRuntime{}, retainedWorkspace{}, integrationClock{now: testNow})
	activated, err := reconciler.Reconcile(ctx, start.ExecutionID)
	if err != nil {
		t.Fatalf("engine.Reconcile: %v", err)
	}
	if _, err := requestService.Resolve(ctx, request.ResolveCommand{
		Actor:                    humanActor(scope, tenancy.RoleReviewer),
		Scope:                    scope,
		ExecutionID:              start.ExecutionID,
		ActionID:                 firstAction,
		ExpectedExecutionVersion: activated.Version,
		ExpectedActionVersion:    activated.Actions[0].Version,
		Decision:                 request.DecisionPass,
		Response:                 kernel.Outputs{"decision": "yes"},
	}); err != nil {
		t.Fatalf("resolve first request: %v", err)
	}

	current := loadExecution(t, adapter, scope, start.ExecutionID)
	if current.State != kernel.ExecutionRunning || current.CurrentAction != -1 || current.Actions[1].State != kernel.ActionPending {
		t.Fatalf("execution = %+v; want running with a pending successor", current)
	}
	_, activation, err := flow.Reduce(current, flow.ActivateFact{
		ExecutionID: start.ExecutionID,
		ActionID:    secondAction,
		At:          testNow,
	})
	if err != nil || activation == nil || activation.Subject == nil {
		t.Fatalf("reduce successor activation: %v %+v", err, activation)
	}
	caller, err := request.NewEngineCaller("engine-actor")
	if err != nil {
		t.Fatalf("NewEngineCaller: %v", err)
	}
	subject := *activation.Subject
	result, err := requestService.Activate(ctx, request.ActivateCommand{
		Caller:                   caller,
		Scope:                    scope,
		ExecutionID:              start.ExecutionID,
		ActionID:                 secondAction,
		ExpectedExecutionVersion: current.Version,
		ExpectedActionVersion:    current.Actions[1].Version,
		Subject:                  &subject,
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		Timeout:                  60 * time.Second,
		CallerKey:                "activate-1",
		Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"activate":1}`)},
	})
	if err != nil {
		t.Fatalf("request.Service.Activate: %v", err)
	}
	if result.ExecutionVersion != current.Version+1 || result.ActionVersion != current.Actions[1].Version+1 {
		t.Fatalf("activation result = %+v; want versions %d/%d", result, current.Version+1, current.Actions[1].Version+1)
	}
	if !result.Deadline.Equal(testNow.Add(60 * time.Second)) {
		t.Fatalf("activation deadline = %s; want %s", result.Deadline, testNow.Add(60*time.Second))
	}
	row, err := backend.Read(ctx, scope, "actions", string(secondAction))
	if err != nil {
		t.Fatalf("read activated successor: %v", err)
	}
	if assignment := decodeAssignment(t, row.Values["request_assignment"]); assignment != (request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}) {
		t.Fatalf("successor assignment = %+v", assignment)
	}
	if row.Values["request_subject"] == nil || row.Values["state"] != string(kernel.ActionRunning) {
		t.Fatalf("successor row = %v; want running with a subject", row.Values)
	}
}

// TestRealExecutionServiceStagesOutputsThroughAdapter drives the real
// StageOutputs operation over a Kernel-activated container action.
func TestRealExecutionServiceStagesOutputsThroughAdapter(t *testing.T) {
	ctx := context.Background()
	backend := memory.NewWithClock(func() time.Time { return testNow })
	adapter := New(backend, backend)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
	ids := &integrationIDs{}
	execService := newRealExecutionService(t, adapter, ids)

	start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{containerAction()}, "start-1")
	actionID := snapshotdoc.DeriveActionID(start.ExecutionID, 0)

	// Apply exactly the commit the reconciler produces for a container
	// activation; the workload effect itself is outside this adapter.
	current := loadExecution(t, adapter, scope, start.ExecutionID)
	_, commit, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: start.ExecutionID, ActionID: actionID, At: testNow})
	if err != nil || commit == nil {
		t.Fatalf("reduce activation: %v", err)
	}
	if _, err := NewEngine(backend.Controller()).Commit(ctx, *commit); err != nil {
		t.Fatalf("engine commit activation: %v", err)
	}
	running := loadExecution(t, adapter, scope, start.ExecutionID)

	actor := authorize.Principal{
		ID:          "exec-principal-1",
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		ExecutionID: start.ExecutionID,
	}
	result, err := execService.StageOutputs(ctx, execution.OutputsCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              start.ExecutionID,
		ActionID:                 actionID,
		ExpectedExecutionVersion: running.Version,
		Outputs:                  kernel.Outputs{"artifact": "binary"},
		CallerKey:                "outputs-1",
	})
	if err != nil {
		t.Fatalf("execution.Service.StageOutputs: %v", err)
	}
	if result.NextExecution != kernel.ExecutionRunning || result.NextAction != kernel.ActionRunning {
		t.Fatalf("stage result = %+v; want running action", result)
	}
	row, err := backend.Read(ctx, scope, "actions", string(actionID))
	if err != nil {
		t.Fatalf("read staged action: %v", err)
	}
	if row.Values["staged_outputs_set"] != true {
		t.Fatalf("staged_outputs_set = %v; want true", row.Values["staged_outputs_set"])
	}
	staged := loadExecution(t, adapter, scope, start.ExecutionID)
	if staged.Actions[0].StagedOutputs["artifact"] != "binary" || staged.Actions[0].Outputs != nil {
		t.Fatalf("staged action = %+v; want staged-only outputs", staged.Actions[0])
	}
}

// TestRealExecutionServiceCancelsThroughAdapter drives the real Cancel
// operation, including its pending cancellation, cleanup and credential clear.
func TestRealExecutionServiceCancelsThroughAdapter(t *testing.T) {
	ctx := context.Background()
	backend := memory.NewWithClock(func() time.Time { return testNow })
	adapter := New(backend, backend)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, 1)
	ids := &integrationIDs{}
	execService := newRealExecutionService(t, adapter, ids)

	start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{containerAction()}, "start-1")
	actionID := snapshotdoc.DeriveActionID(start.ExecutionID, 0)

	result, err := execService.Cancel(ctx, execution.CancelCommand{
		Actor:                    humanActor(scope, tenancy.RoleEngineer, tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              start.ExecutionID,
		ExpectedExecutionVersion: 1,
		CallerKey:                "cancel-1",
		Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"cancel":1}`)},
	})
	if err != nil {
		t.Fatalf("execution.Service.Cancel: %v", err)
	}
	// A queued execution has no running primary action, so the cancellation is
	// execution-scoped and the pending action is settled by its cancellation
	// update; the action row is asserted below.
	if result.NextExecution != kernel.ExecutionCancelled || result.NextAction != "" {
		t.Fatalf("cancel result = %+v; want a cancelled execution with no primary action", result)
	}
	cancelled := loadExecution(t, adapter, scope, start.ExecutionID)
	if cancelled.State != kernel.ExecutionCancelled || cancelled.CurrentAction != -1 {
		t.Fatalf("cancelled execution = %+v", cancelled)
	}
	if cancelled.Actions[0].ID != actionID || cancelled.Actions[0].State != kernel.ActionCancelled {
		t.Fatalf("cancelled action = %+v", cancelled.Actions[0])
	}
	row, err := backend.Read(ctx, scope, "cleanup", cleanupID(start.ExecutionID))
	if err != nil {
		t.Fatalf("read cleanup row: %v", err)
	}
	if row.Values["state"] != string(kernel.CleanupPending) {
		t.Fatalf("cleanup state = %v; want pending", row.Values["state"])
	}
}
