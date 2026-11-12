package execution_test

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
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// testNow is the injected authoritative time for every operation command.
var testNow = time.Date(2026, time.November, 4, 9, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

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

func (source *sequentialIDs) NewExecutionID() (id.ExecutionID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.ExecutionID(fmt.Sprintf("execution-%d", source.next)), nil
}

func unexpectedCall(name string) error {
	return fault.New(fault.InternalFailure, "unexpected port call "+name)
}

type authorizeCall struct {
	Principal authorize.Principal
	Operation authorize.OperationName
	Scope     tenancy.Scope
}

type fakeAuthorizer struct {
	mu        sync.Mutex
	calls     []authorizeCall
	authorize func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
}

func (authorizer *fakeAuthorizer) Authorize(ctx context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	authorizer.mu.Lock()
	authorizer.calls = append(authorizer.calls, authorizeCall{Principal: principal, Operation: operation, Scope: scope})
	callback := authorizer.authorize
	authorizer.mu.Unlock()
	if callback != nil {
		return callback(ctx, principal, operation, scope)
	}
	return registryDecision(principal, operation), nil
}

func (authorizer *fakeAuthorizer) callCount() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return len(authorizer.calls)
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
	case identity.KindExecution:
		// Execution authority is a separate closed vocabulary. The fake models
		// the admitted-permission branch only, so an execution principal is
		// allowed exactly when the operation declares an execution permission.
		if registered.Requires.Execution != authorize.ExecNone {
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
	if reader == nil {
		return allowedFacts(reads), nil
	}
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

func (reader *fakeAuthorityReader) captured() [][]authorize.AuthorityRead {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([][]authorize.AuthorityRead(nil), reader.calls...)
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

// fakeStore is one scoped execution aggregate plus one atomic compare-and-commit
// boundary. It uses the Kernel testkit store for the projection, so every
// transition the Shell builds is applied exactly like the pure reducer defines.
// Failure injection happens before or after the atomic write.
type fakeStore struct {
	store *testkit.Store

	mu           sync.Mutex
	owners       map[id.ExecutionID]tenancy.Scope
	commits      int
	beforeCommit func()
	failBefore   error
	failAfter    error
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
	store.mu.Lock()
	before := store.beforeCommit
	failBefore := store.failBefore
	store.failBefore = nil
	store.mu.Unlock()
	if before != nil {
		before()
	}
	if failBefore != nil {
		return kernel.Execution{}, failBefore
	}
	next, err := store.store.Handle().Commit(ctx, commit)
	if err != nil {
		return kernel.Execution{}, err
	}
	store.mu.Lock()
	store.commits++
	failAfter := store.failAfter
	store.failAfter = nil
	store.mu.Unlock()
	if failAfter != nil {
		return next, failAfter
	}
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

func (store *fakeStore) setBeforeCommit(callback func()) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.beforeCommit = callback
}

func (store *fakeStore) failNextBefore(err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failBefore = err
}

func (store *fakeStore) failNextAfter(err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failAfter = err
}

type fakeTransitionPort struct {
	store *fakeStore

	mu    sync.Mutex
	calls []execution.TransitionUnit
}

func (port *fakeTransitionPort) Transition(ctx context.Context, unit execution.TransitionUnit) (execution.TransitionResult, error) {
	port.mu.Lock()
	port.calls = append(port.calls, cloneTransitionUnit(unit))
	port.mu.Unlock()

	next, err := port.store.Commit(ctx, unit.Commit)
	if err != nil {
		return execution.TransitionResult{}, err
	}
	result := execution.TransitionResult{
		ExecutionVersion: next.Version,
		NextExecution:    next.State,
	}
	if unit.Commit.ActionID != "" {
		for _, action := range next.Actions {
			if action.ID == unit.Commit.ActionID {
				result.ActionVersion = action.Version
				result.NextAction = action.State
				break
			}
		}
	}
	return result, nil
}

func (port *fakeTransitionPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.calls)
}

func (port *fakeTransitionPort) captured() []execution.TransitionUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]execution.TransitionUnit(nil), port.calls...)
}

func cloneTransitionUnit(unit execution.TransitionUnit) execution.TransitionUnit {
	clone := unit
	clone.Audit = append([]event.Audit(nil), unit.Audit...)
	clone.Events = append([]event.Envelope(nil), unit.Events...)
	clone.Authority = append([]authorize.AuthorityRead(nil), unit.Authority...)
	clone.PostCommit = append([]event.PostCommitEffect(nil), unit.PostCommit...)
	return clone
}

type fakeStartPort struct {
	mu       sync.Mutex
	calls    []execution.StartUnit
	byKey    map[string]execution.StartResult
	hashes   map[string]string
	failNext bool
}

func newFakeStartPort() *fakeStartPort {
	return &fakeStartPort{byKey: make(map[string]execution.StartResult), hashes: make(map[string]string)}
}

func (port *fakeStartPort) Start(ctx context.Context, unit execution.StartUnit) (execution.StartResult, error) {
	port.mu.Lock()
	key := receiptKeyString(unit.Receipt.Key)
	if recorded, ok := port.byKey[key]; ok {
		hash := port.hashes[key]
		port.mu.Unlock()
		if hash != unit.Receipt.ContentHash {
			return execution.StartResult{}, fault.New(fault.ChangedIdempotencyContent, "start retry content changed")
		}
		recorded.Replayed = true
		return recorded, nil
	}
	port.calls = append(port.calls, unit)
	result := execution.StartResult{ExecutionID: unit.NewExecutionID, PipelineVersion: unit.ExpectedPipelineVersion, ExecutionVersion: 1}
	port.byKey[key] = result
	port.hashes[key] = unit.Receipt.ContentHash
	fail := port.failNext
	port.failNext = false
	port.mu.Unlock()
	if fail {
		return result, fault.New(fault.DependencyUnavailable, "start response was lost after commit")
	}
	return result, nil
}

func (port *fakeStartPort) failNextCallAfterCommit() {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.failNext = true
}

func (port *fakeStartPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.calls)
}

func (port *fakeStartPort) captured() []execution.StartUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]execution.StartUnit(nil), port.calls...)
}

type fakeReader struct {
	mu         sync.Mutex
	inspectFn  func(context.Context, tenancy.Scope, id.ExecutionID) (execution.View, error)
	listFn     func(context.Context, tenancy.Scope, execution.ListFilter) (execution.Page, error)
	inspectIDs []id.ExecutionID
	listCalls  []execution.ListFilter
}

func (reader *fakeReader) Inspect(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (execution.View, error) {
	reader.mu.Lock()
	reader.inspectIDs = append(reader.inspectIDs, executionID)
	callback := reader.inspectFn
	reader.mu.Unlock()
	if callback == nil {
		return execution.View{}, unexpectedCall("Reader.Inspect")
	}
	return callback(ctx, scope, executionID)
}

func (reader *fakeReader) List(ctx context.Context, scope tenancy.Scope, filter execution.ListFilter) (execution.Page, error) {
	reader.mu.Lock()
	reader.listCalls = append(reader.listCalls, filter)
	callback := reader.listFn
	reader.mu.Unlock()
	if callback == nil {
		return execution.Page{}, unexpectedCall("Reader.List")
	}
	return callback(ctx, scope, filter)
}

func (reader *fakeReader) inspectCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.inspectIDs)
}

func (reader *fakeReader) listCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.listCalls)
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

// executionActor is the authenticated credential of one running container.
// Execution authority is its own closed vocabulary; it never carries roles.
func executionActor(tenant, project, principal string, executionID id.ExecutionID) authorize.Principal {
	return authorize.Principal{
		ID:          id.PrincipalID(principal),
		Kind:        identity.KindExecution,
		Scope:       projectScope(tenant, project),
		Enabled:     true,
		ExecutionID: executionID,
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

func runningContainerExecution(actions ...kernel.Action) kernel.Execution {
	exec := baseExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = testNow
	exec.Actions[0].Deadline = testNow.Add(exec.Actions[0].Timeout)
	exec.Actions[0].Container.WorkloadID = id.WorkloadID("workload-1")
	exec.Actions[0].Container.WorkspaceID = exec.WorkspaceID
	exec.Actions[0].Container.Launch = kernel.LaunchIntent{WorkloadID: id.WorkloadID("workload-1"), State: kernel.LaunchIssued}
	exec.Actions[0].Container.Observation = kernel.RunningObservation()
	return exec
}

func runningRequestExecution(actions ...kernel.Action) kernel.Execution {
	exec := baseExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = testNow
	exec.Actions[0].Deadline = testNow.Add(exec.Actions[0].Timeout)
	return exec
}

func receiptContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
}

func receiptKeyString(key receipt.Key) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", key.TenantID, key.ProjectID, key.PrincipalID, key.Operation, key.CallerKey)
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

func requireNoAuthority(t *testing.T, reads []authorize.AuthorityRead, check authorize.AuthorityCheck) {
	t.Helper()
	for _, read := range reads {
		if read.Check == check {
			t.Fatalf("authority reads unexpectedly contain %q: %v", check, reads)
		}
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

func requireAudit(t *testing.T, audits []event.Audit, scope tenancy.Scope, actor id.PrincipalID, operation string, at time.Time) {
	t.Helper()
	if len(audits) != 1 {
		t.Fatalf("audit records = %d; want exactly one", len(audits))
	}
	audit := audits[0]
	if audit.ID == "" {
		t.Fatal("audit record has no identity")
	}
	if audit.Scope != scope {
		t.Fatalf("audit scope = %+v; want %+v", audit.Scope, scope)
	}
	if audit.Actor != actor {
		t.Fatalf("audit actor = %q; want %q", audit.Actor, actor)
	}
	if audit.Operation != operation {
		t.Fatalf("audit operation = %q; want %q", audit.Operation, operation)
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

func pipelineAdmission(t *testing.T, yaml []byte, version uint64, actions ...pipeline.AdmittedAction) pipeline.AdmissionResult {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml})
	if err != nil {
		t.Fatalf("pipeline.NewSnapshot: %v", err)
	}
	return pipeline.AdmissionResult{
		PipelineID:      pipeline.PipelineID("pipeline-1"),
		PipelineVersion: version,
		Snapshot:        snapshot,
		Actions:         actions,
	}
}

type testService struct {
	service     *execution.Service
	clock       fixedClock
	ids         *sequentialIDs
	authorizer  *fakeAuthorizer
	authority   *fakeAuthorityReader
	store       *fakeStore
	reader      *fakeReader
	transitions *fakeTransitionPort
	starts      *fakeStartPort
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	store := newFakeStore()
	reader := &fakeReader{}
	transitions := &fakeTransitionPort{store: store}
	starts := newFakeStartPort()
	authorizer := &fakeAuthorizer{}
	authority := &fakeAuthorityReader{}
	clock := fixedClock{now: testNow}
	ids := &sequentialIDs{}
	service, err := execution.NewService(execution.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   authority,
		Reader:      reader,
		State:       store,
		Transitions: transitions,
		Starts:      starts,
	})
	if err != nil {
		t.Fatalf("execution.NewService: %v", err)
	}
	return &testService{
		service:     service,
		clock:       clock,
		ids:         ids,
		authorizer:  authorizer,
		authority:   authority,
		store:       store,
		reader:      reader,
		transitions: transitions,
		starts:      starts,
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

func admittedContainerAction(index int, name string, timeout time.Duration) pipeline.AdmittedAction {
	return pipeline.AdmittedAction{
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindContainer,
		Timeout: timeout,
		Container: &definition.Container{
			Image:    "registry.example.com/tool:1",
			Commands: []string{"run"},
		},
	}
}
