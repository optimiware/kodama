package request_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, time.November, 4, 9, 0, 0, 0, time.UTC)

const testEngineActor = id.PrincipalID("engine-1")

func testEngineCaller(t *testing.T) request.EngineCaller {
	t.Helper()
	caller, err := request.NewEngineCaller(testEngineActor)
	if err != nil {
		t.Fatalf("NewEngineCaller: %v", err)
	}
	return caller
}

type fixedClock struct{ now time.Time }

func (clock *fixedClock) Now() time.Time { return clock.now }

type sequentialIDs struct {
	mu   sync.Mutex
	next int
}

func (source *sequentialIDs) NewEventID() (id.EventID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *sequentialIDs) NewReceiptID() (id.ReceiptID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

type fakeAuthorizer struct{}

func (*fakeAuthorizer) Authorize(_ context.Context, principal authorize.Principal, operation authorize.OperationName, _ tenancy.Scope) (authorize.Decision, error) {
	return registryDecision(principal, operation), nil
}

func registryDecision(principal authorize.Principal, operation authorize.OperationName) authorize.Decision {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.DecisionDeny
	}
	switch principal.Kind {
	case identity.KindHuman:
		if slices.ContainsFunc(registered.Requires.HumanRoles, principal.HasRole) {
			return authorize.DecisionAllow
		}
	case identity.KindService:
		if slices.ContainsFunc(registered.Requires.ServiceRoles, principal.HasRole) {
			return authorize.DecisionAllow
		}
	}
	return authorize.DecisionDeny
}

type fakeAuthorityReader struct {
	mu    sync.Mutex
	calls [][]authorize.AuthorityRead
	read  func(context.Context, []authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func (reader *fakeAuthorityReader) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	reader.mu.Lock()
	reader.calls = append(reader.calls, append([]authorize.AuthorityRead(nil), reads...))
	callback := reader.read
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, reads)
	}
	return allowedFacts(reads), nil
}

func (reader *fakeAuthorityReader) callCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.calls)
}

func allowedFacts(reads []authorize.AuthorityRead) []authorize.AuthorityFact {
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
	return facts
}

type fakeStore struct {
	store *testkit.Store

	mu      sync.Mutex
	owners  map[id.ExecutionID]tenancy.Scope
	commits int
}

func newFakeStore() *fakeStore {
	return &fakeStore{store: testkit.NewStore(), owners: make(map[id.ExecutionID]tenancy.Scope)}
}

func (store *fakeStore) Seed(scope tenancy.Scope, executions ...kernel.Execution) {
	store.mu.Lock()
	for _, aggregate := range executions {
		store.owners[aggregate.ID] = scope
	}
	store.mu.Unlock()
	store.store.Seed(executions...)
}

func (store *fakeStore) Load(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (kernel.Execution, error) {
	store.mu.Lock()
	owner, ok := store.owners[executionID]
	store.mu.Unlock()
	if !ok || owner != scope {
		return kernel.Execution{}, fault.New(fault.NotFound, fmt.Sprintf("execution %q is not visible in this scope", executionID))
	}
	return store.store.Handle().Load(ctx, executionID)
}

func (store *fakeStore) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	next, err := store.store.Handle().Commit(ctx, commit)
	if err != nil {
		return kernel.Execution{}, err
	}
	store.mu.Lock()
	store.commits++
	store.mu.Unlock()
	return next, nil
}

func (store *fakeStore) Execution(executionID id.ExecutionID) (kernel.Execution, bool) {
	return store.store.Execution(executionID)
}

func (store *fakeStore) commitCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.commits
}

type fakeActivationPort struct {
	mu    sync.Mutex
	calls []request.ActivationUnit
	fail  error
}

func (port *fakeActivationPort) Activate(ctx context.Context, unit request.ActivationUnit) (request.ActivationResult, error) {
	port.mu.Lock()
	port.calls = append(port.calls, unit)
	failure := port.fail
	port.fail = nil
	port.mu.Unlock()
	if failure != nil {
		return request.ActivationResult{}, failure
	}
	return request.ActivationResult{
		ExecutionVersion: unit.ExpectedExecutionVersion + 1,
		ActionVersion:    unit.ExpectedActionVersion + 1,
		Deadline:         unit.Deadline,
	}, nil
}

func (port *fakeActivationPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.calls)
}

func (port *fakeActivationPort) captured() []request.ActivationUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]request.ActivationUnit(nil), port.calls...)
}

type resolveReceipt struct {
	hash   string
	result request.ResolveResult
}

type fakeResolvePort struct {
	store *fakeStore

	mu          sync.Mutex
	calls       []request.ResolveUnit
	receipts    map[string]resolveReceipt
	validations int
	beforeApply func()
}

func newFakeResolvePort(store *fakeStore) *fakeResolvePort {
	return &fakeResolvePort{store: store, receipts: make(map[string]resolveReceipt)}
}

// Resolve mirrors the declared ResolvePort contract: look the receipt up FIRST,
// return the original result for equal content, refuse changed content, and
// only on a miss apply the first-settlement validation and persist the unit's
// Kernel commit. A replay unit therefore legitimately carries a zero Commit.
func (port *fakeResolvePort) Resolve(ctx context.Context, unit request.ResolveUnit) (request.ResolveResult, error) {
	port.mu.Lock()
	port.calls = append(port.calls, unit)
	recorded, found := port.receipts[unit.IdempotencyKey]
	beforeApply := port.beforeApply
	port.mu.Unlock()
	if found {
		if recorded.hash != unit.Receipt.ContentHash {
			return request.ResolveResult{}, fault.New(fault.ChangedIdempotencyContent, "request retry content changed")
		}
		recorded.result.Replayed = true
		return recorded.result, nil
	}

	if err := unit.Validate(); err != nil {
		return request.ResolveResult{}, err
	}
	port.mu.Lock()
	port.validations++
	port.mu.Unlock()

	current, err := port.store.Load(ctx, unit.Owner, unit.ExecutionID)
	if err != nil {
		return request.ResolveResult{}, err
	}
	if current.Version != unit.ExpectedExecutionVersion {
		return request.ResolveResult{}, fault.New(fault.VersionConflict, "execution version changed before resolution")
	}
	index := -1
	for position, action := range current.Actions {
		if action.ID == unit.ActionID {
			index = position
			break
		}
	}
	if index < 0 {
		return request.ResolveResult{}, fault.New(fault.InvalidInput, "request action is not part of the execution")
	}
	if current.Actions[index].Version != unit.ExpectedActionVersion {
		return request.ResolveResult{}, fault.New(fault.VersionConflict, "action version changed before resolution")
	}
	if beforeApply != nil {
		beforeApply()
	}
	applied, err := port.store.Commit(ctx, unit.Commit)
	if err != nil {
		return request.ResolveResult{}, err
	}
	actionVersion := unit.ExpectedActionVersion + 1
	for _, action := range applied.Actions {
		if action.ID == unit.ActionID {
			actionVersion = action.Version
			break
		}
	}
	result := request.ResolveResult{
		ExecutionVersion: applied.Version,
		ActionVersion:    actionVersion,
		Decision:         unit.Decision,
	}
	port.record(unit.IdempotencyKey, unit.Receipt.ContentHash, result)
	return result, nil
}

func (port *fakeResolvePort) record(key, hash string, result request.ResolveResult) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.receipts[key] = resolveReceipt{hash: hash, result: result}
}

func (port *fakeResolvePort) setBeforeApply(callback func()) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.beforeApply = callback
}

func (port *fakeResolvePort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.calls)
}

func (port *fakeResolvePort) validationCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.validations
}

func (port *fakeResolvePort) captured() []request.ResolveUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]request.ResolveUnit(nil), port.calls...)
}

// ---------------------------------------------------------------------------
// Fixtures and assertions
// ---------------------------------------------------------------------------

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func humanActor(tenant, project, principal string, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindHuman,
		Scope:   projectScope(tenant, project),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func serviceActor(tenant, project, principal string, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindService,
		Scope:   projectScope(tenant, project),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func containerAction(actionID, name string, index int, timeout time.Duration, outputs ...string) kernel.Action {
	declared := make([]id.OutputKey, len(outputs))
	for position, key := range outputs {
		declared[position] = id.OutputKey(key)
	}
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindContainer,
		State:   kernel.ActionPending,
		Version: 1,
		Timeout: timeout,
		Container: &kernel.ContainerAction{
			Definition: definition.Container{
				Image:    "registry.example.com/tool:1",
				Commands: []string{"run"},
				Outputs:  declared,
			},
		},
	}
}

func requestAction(actionID, name string, index int, timeout time.Duration, assignment string, subject definition.Subject, outputs ...string) kernel.Action {
	declared := make([]id.OutputKey, len(outputs))
	for position, key := range outputs {
		declared[position] = id.OutputKey(key)
	}
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindRequest,
		State:   kernel.ActionPending,
		Version: 1,
		Timeout: timeout,
		Request: &kernel.RequestAction{
			Definition: definition.Request{
				Assignment: assignment,
				Subject:    subject,
				Outputs:    declared,
			},
		},
	}
}

func literalSubject(fields map[string]string) definition.Subject {
	values := make(map[string]definition.SubjectValue, len(fields))
	for key, value := range fields {
		values[key] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: value}
	}
	return definition.NewSubject(values)
}

func baseExecution(actions ...kernel.Action) kernel.Execution {
	return kernel.Execution{
		ID:            id.ExecutionID("exec-1"),
		Version:       1,
		State:         kernel.ExecutionQueued,
		WorkspaceID:   id.WorkspaceID("ws-1"),
		Actions:       actions,
		CurrentAction: -1,
		Snapshot:      definition.NewSnapshot(definition.SnapshotInput{}),
		Cleanup:       kernel.Cleanup{State: kernel.CleanupNone},
	}
}

func runningRequestExecution(actions ...kernel.Action) kernel.Execution {
	exec := baseExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = testNow
	exec.Actions[0].Deadline = testNow.Add(exec.Actions[0].Timeout)
	// Activation persists the immutable resolved subject. A running request is
	// answerable only when that subject is present, so the fixture mirrors a
	// successfully activated action instead of the admitted declaration alone.
	if exec.Actions[0].Request != nil {
		subject := exec.Actions[0].Request.Definition.Subject
		exec.Actions[0].Request.Subject = &subject
	}
	return exec
}

func runningContainerExecution(actions ...kernel.Action) kernel.Execution {
	exec := baseExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = testNow
	exec.Actions[0].Deadline = testNow.Add(exec.Actions[0].Timeout)
	if exec.Actions[0].Container != nil {
		exec.Actions[0].Container.WorkloadID = id.WorkloadID("workload-1")
		exec.Actions[0].Container.WorkspaceID = exec.WorkspaceID
	}
	return exec
}

func receiptContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
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

func requireFaultClassIn(t *testing.T, err error, allowed ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected one of %v faults, got nil", allowed)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want one of %v", err, allowed)
	}
	if slices.Contains(allowed, got) {
		return
	}
	t.Fatalf("fault class = %s; want one of %v (%v)", got, allowed, err)
}

func requireNoError(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func requireAuthority(t *testing.T, reads []authorize.AuthorityRead, operation authorize.OperationName, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	registry, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("registry operation %q is not declared", operation)
	}
	for _, read := range reads {
		if read.Check != check {
			continue
		}
		if read.Operation != operation {
			t.Fatalf("authority read operation = %q; want %q", read.Operation, operation)
		}
		if !reflect.DeepEqual(read.Required, registry.Requires) {
			t.Fatalf("authority read required = %+v; want registry grant %+v", read.Required, registry.Requires)
		}
		return read
	}
	t.Fatalf("authority reads %v do not contain check %q", reads, check)
	return authorize.AuthorityRead{}
}

func findAuthority(reads []authorize.AuthorityRead, check authorize.AuthorityCheck) (authorize.AuthorityRead, bool) {
	for _, read := range reads {
		if read.Check == check {
			return read, true
		}
	}
	return authorize.AuthorityRead{}, false
}

// requireAssignmentRoleRead pins the in-commit assignment check: the read must
// ask for the exact assigned human role, not merely the operation's wider grant
// set, so a caller holding another granted role cannot answer.
func requireAssignmentRoleRead(t *testing.T, reads []authorize.AuthorityRead, role tenancy.Role) {
	t.Helper()
	for _, read := range reads {
		if read.Check != authorize.CheckMembership {
			continue
		}
		if read.Operation != "execution.request.resolve" {
			continue
		}
		if len(read.Required.HumanRoles) == 1 && read.Required.HumanRoles[0] == role && len(read.Required.ServiceRoles) == 0 {
			return
		}
	}
	t.Fatalf("authority reads %v do not require exactly the current human role %q", reads, role)
}

// requireServiceAssignmentRead pins the explicit service-ID assignment check.
func requireServiceAssignmentRead(t *testing.T, reads []authorize.AuthorityRead, serviceID string) {
	t.Helper()
	for _, read := range reads {
		if read.Check == authorize.CheckServiceEnabled && read.Operation == "execution.request.resolve" && read.TargetID == serviceID {
			return
		}
	}
	t.Fatalf("authority reads %v do not require enabled service %q", reads, serviceID)
}

func requireEvent(
	t *testing.T,
	events []event.Envelope,
	eventType event.Type,
	scope tenancy.Scope,
	subject string,
	actor id.PrincipalID,
	at time.Time,
) event.Envelope {
	t.Helper()
	for _, envelope := range events {
		if envelope.Type != eventType {
			continue
		}
		if envelope.Source.Kind != event.SourceShell {
			t.Fatalf("event source kind = %q; want %q", envelope.Source.Kind, event.SourceShell)
		}
		if envelope.Source.PrincipalID != actor {
			t.Fatalf("event source principal = %q; want %q", envelope.Source.PrincipalID, actor)
		}
		if !envelope.At.Equal(at) {
			t.Fatalf("event time = %v; want %v", envelope.At, at)
		}
		if envelope.Scope != scope {
			t.Fatalf("event scope = %+v; want %+v", envelope.Scope, scope)
		}
		if envelope.SubjectID != subject {
			t.Fatalf("event subject = %q; want %q", envelope.SubjectID, subject)
		}
		if err := envelope.Validate(); err != nil {
			t.Fatalf("event intent is not valid: %v", err)
		}
		return envelope
	}
	t.Fatalf("events %v do not contain type %q", events, eventType)
	return event.Envelope{}
}

func requirePostCommit(t *testing.T, effects []event.PostCommitEffect, kind event.PostCommitKind) event.PostCommitEffect {
	t.Helper()
	for _, effect := range effects {
		if effect.Kind == kind {
			return effect
		}
	}
	t.Fatalf("post-commit effects %v do not contain %q", effects, kind)
	return event.PostCommitEffect{}
}

type testService struct {
	service     *request.Service
	clock       *fixedClock
	ids         *sequentialIDs
	authorizer  *fakeAuthorizer
	authority   *fakeAuthorityReader
	store       *fakeStore
	activations *fakeActivationPort
	resolutions *fakeResolvePort
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	store := newFakeStore()
	clock := &fixedClock{now: testNow}
	ids := &sequentialIDs{}
	authorizer := &fakeAuthorizer{}
	authority := &fakeAuthorityReader{}
	activations := &fakeActivationPort{}
	resolutions := newFakeResolvePort(store)
	service, err := request.NewService(request.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   authority,
		State:       store,
		Activations: activations,
		Resolutions: resolutions,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}
	return &testService{
		service:     service,
		clock:       clock,
		ids:         ids,
		authorizer:  authorizer,
		authority:   authority,
		store:       store,
		activations: activations,
		resolutions: resolutions,
	}
}
