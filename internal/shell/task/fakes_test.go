package task_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type sequentialIDs struct {
	mu     sync.Mutex
	counts map[string]int
}

func (source *sequentialIDs) next(kind string) string {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.counts == nil {
		source.counts = make(map[string]int)
	}
	source.counts[kind]++
	return fmt.Sprintf("%s-%d", kind, source.counts[kind])
}

func (source *sequentialIDs) NewTaskID() (id.TaskID, error) {
	return id.TaskID(source.next("task")), nil
}

func (source *sequentialIDs) NewQuestionID() (task.QuestionID, error) {
	return task.QuestionID(source.next("question")), nil
}

func (source *sequentialIDs) NewExecutionID() (id.ExecutionID, error) {
	return id.ExecutionID(source.next("execution")), nil
}

func (source *sequentialIDs) NewPrincipalID() (id.PrincipalID, error) {
	return id.PrincipalID(source.next("principal")), nil
}

func (source *sequentialIDs) NewEventID() (id.EventID, error) {
	return id.EventID(source.next("event")), nil
}

func (source *sequentialIDs) NewReceiptID() (id.ReceiptID, error) {
	return id.ReceiptID(source.next("receipt")), nil
}

// fakeAdmission records every admission request.
type fakeAdmission struct {
	mu       sync.Mutex
	requests []pipeline.AdmitRequest
	admit    func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
}

func (admission *fakeAdmission) Admit(_ context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	admission.mu.Lock()
	admission.requests = append(admission.requests, request)
	callback := admission.admit
	admission.mu.Unlock()
	if callback != nil {
		return callback(request)
	}
	return defaultAdmission(request)
}

func defaultAdmission(request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	selected := request.Definition.ID
	if selected == "" {
		selected = pipelineID
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         testPipelineYAML,
		Inputs:       request.Inputs,
		TaskID:       request.Task.TaskID,
		Conversation: request.Task.Conversation,
		References:   request.Task.References,
	})
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	container := definition.Container{Image: "registry.example.com/run@sha256:1111111111111111111111111111111111111111111111111111111111111111"}
	return pipeline.AdmissionResult{
		PipelineID:      selected,
		PipelineVersion: 7,
		Snapshot:        snapshot,
		Actions: []task.AdmittedAction{{
			Index:     0,
			Name:      id.ActionName("run"),
			Kind:      definition.ActionKindContainer,
			Timeout:   time.Minute,
			Container: &container,
		}},
	}, nil
}

func (admission *fakeAdmission) requestsSnapshot() []pipeline.AdmitRequest {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return append([]pipeline.AdmitRequest(nil), admission.requests...)
}

func (admission *fakeAdmission) callCount() int {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return len(admission.requests)
}

type fakePipelineReader struct {
	mu    sync.Mutex
	calls []pipeline.PipelineID
	get   func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error)
}

func (reader *fakePipelineReader) calledIDs() []pipeline.PipelineID {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]pipeline.PipelineID(nil), reader.calls...)
}

func (reader *fakePipelineReader) callCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.calls)
}

func (reader *fakePipelineReader) Get(ctx context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID) (pipeline.Definition, error) {
	reader.mu.Lock()
	reader.calls = append(reader.calls, pipelineID)
	callback := reader.get
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, scope, pipelineID)
	}
	if scope != projectScope() {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present in this scope")
	}
	return pipeline.Definition{ID: pipelineID, Name: "release", YAML: testPipelineYAML, Version: 3}, nil
}

type fakeExecutionReader struct {
	mu         sync.Mutex
	views      map[id.ExecutionID]task.ExecutionView
	aggregates map[id.ExecutionID]kernel.Execution
	calls      []id.ExecutionID
	loads      []id.ExecutionID
	loadScopes []tenancy.Scope
	get        func(context.Context, tenancy.Scope, id.ExecutionID) (task.ExecutionView, error)
	load       func(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)
}

var _ task.ExecutionAggregateReader = (*fakeExecutionReader)(nil)

func (reader *fakeExecutionReader) Get(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (task.ExecutionView, error) {
	reader.mu.Lock()
	reader.calls = append(reader.calls, executionID)
	callback := reader.get
	view, ok := reader.views[executionID]
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, scope, executionID)
	}
	if scope != projectScope() {
		return task.ExecutionView{}, fault.New(fault.NotFound, "execution is not present in this scope")
	}
	if !ok {
		return task.ExecutionView{}, fault.New(fault.NotFound, "execution is not present")
	}
	return view, nil
}

func (reader *fakeExecutionReader) put(view task.ExecutionView) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.views == nil {
		reader.views = make(map[id.ExecutionID]task.ExecutionView)
	}
	reader.views[view.ID] = view
	// Mechanical fixture bridge: a predecessor view also seeds the minimal
	// aggregate so existing task fixtures keep exercising the cancellation
	// path. Dedicated aggregate fixtures overwrite it with the full row.
	if reader.aggregates == nil {
		reader.aggregates = make(map[id.ExecutionID]kernel.Execution)
	}
	if _, ok := reader.aggregates[view.ID]; !ok {
		reader.aggregates[view.ID] = kernel.Execution{ID: view.ID, Version: view.Version, State: view.State}
	}
}

func (reader *fakeExecutionReader) putAggregate(aggregate kernel.Execution) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.aggregates == nil {
		reader.aggregates = make(map[id.ExecutionID]kernel.Execution)
	}
	reader.aggregates[aggregate.ID] = aggregate
}

func (reader *fakeExecutionReader) removeAggregate(executionID id.ExecutionID) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	delete(reader.aggregates, executionID)
}

func (reader *fakeExecutionReader) setLoad(fn func(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.load = fn
}

// Load implements task.ExecutionAggregateReader. The scoped read is the
// version-guarded aggregate; an out-of-scope or unknown execution is the
// uniform not_found.
func (reader *fakeExecutionReader) Load(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (kernel.Execution, error) {
	reader.mu.Lock()
	reader.loads = append(reader.loads, executionID)
	reader.loadScopes = append(reader.loadScopes, scope)
	callback := reader.load
	aggregate, ok := reader.aggregates[executionID]
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, scope, executionID)
	}
	if scope != projectScope() {
		return kernel.Execution{}, fault.New(fault.NotFound, "execution is not present in this scope")
	}
	if !ok {
		return kernel.Execution{}, fault.New(fault.NotFound, "execution is not present")
	}
	return aggregate, nil
}

func (reader *fakeExecutionReader) loadCalls() []id.ExecutionID {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]id.ExecutionID(nil), reader.loads...)
}

func (reader *fakeExecutionReader) loadedScopes() []tenancy.Scope {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]tenancy.Scope(nil), reader.loadScopes...)
}

func (reader *fakeExecutionReader) callCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.calls)
}

func (reader *fakeExecutionReader) calledIDs() []id.ExecutionID {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]id.ExecutionID(nil), reader.calls...)
}

type fakePrincipals struct {
	mu         sync.Mutex
	principals map[id.PrincipalID]identity.Principal
	calls      []id.PrincipalID
	read       func(context.Context, identity.PrincipalRequest) (identity.Principal, error)
}

func (reader *fakePrincipals) ReadPrincipal(ctx context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	reader.mu.Lock()
	reader.calls = append(reader.calls, request.PrincipalID)
	callback := reader.read
	principal, ok := reader.principals[request.PrincipalID]
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, request)
	}
	if !ok {
		return identity.Principal{}, fault.New(fault.NotFound, "principal is not present")
	}
	return principal, nil
}

func (reader *fakePrincipals) put(principal identity.Principal) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.principals == nil {
		reader.principals = make(map[id.PrincipalID]identity.Principal)
	}
	reader.principals[principal.ID] = principal
}

func (reader *fakePrincipals) calledIDs() []id.PrincipalID {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]id.PrincipalID(nil), reader.calls...)
}

func (reader *fakePrincipals) called(principalID id.PrincipalID) bool {
	return slices.Contains(reader.calledIDs(), principalID)
}

type fakeReader struct {
	mu          sync.Mutex
	tasks       map[id.TaskID]task.Task
	getCalls    []id.TaskID
	historyCall []task.HistoryQuery
	get         func(context.Context, tenancy.Scope, id.TaskID) (task.Task, error)
	history     func(context.Context, tenancy.Scope, id.TaskID, task.HistoryQuery) (task.HistoryPage, error)
}

func (reader *fakeReader) Get(ctx context.Context, scope tenancy.Scope, taskID id.TaskID) (task.Task, error) {
	reader.mu.Lock()
	reader.getCalls = append(reader.getCalls, taskID)
	callback := reader.get
	value, ok := reader.tasks[taskID]
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, scope, taskID)
	}
	if !ok {
		return task.Task{}, fault.New(fault.NotFound, "task is not present")
	}
	if value.Scope != scope {
		return task.Task{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	return cloneTask(value), nil
}

func (reader *fakeReader) History(ctx context.Context, scope tenancy.Scope, taskID id.TaskID, query task.HistoryQuery) (task.HistoryPage, error) {
	reader.mu.Lock()
	reader.historyCall = append(reader.historyCall, query)
	callback := reader.history
	value, ok := reader.tasks[taskID]
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, scope, taskID, query)
	}
	if !ok || value.Scope != scope {
		return task.HistoryPage{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	return task.HistoryPage{}, nil
}

func (reader *fakeReader) put(value task.Task) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.tasks == nil {
		reader.tasks = make(map[id.TaskID]task.Task)
	}
	reader.tasks[value.ID] = cloneTask(value)
}

func (reader *fakeReader) getCallCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.getCalls)
}

func (reader *fakeReader) historyQueries() []task.HistoryQuery {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]task.HistoryQuery(nil), reader.historyCall...)
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

func (authorizer *fakeAuthorizer) captured() []authorizeCall {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]authorizeCall(nil), authorizer.calls...)
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
		if principal.Enabled && registered.Requires.Execution != authorize.ExecNone {
			return authorize.DecisionAllow
		}
	}
	return authorize.DecisionDeny
}

// fakeAuthority answers every read in order. By default the current fact holds
// and reports the requested expected version; tests override read to model a
// removed role or a revoked initiator.
type fakeAuthority struct {
	mu    sync.Mutex
	calls [][]authorize.AuthorityRead
	read  func([]authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func (authority *fakeAuthority) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	authority.mu.Lock()
	authority.calls = append(authority.calls, append([]authorize.AuthorityRead(nil), reads...))
	callback := authority.read
	authority.mu.Unlock()
	if callback != nil {
		return callback(reads)
	}
	return allowFacts(reads, nil), nil
}

func allowFacts(reads []authorize.AuthorityRead, deny func(authorize.AuthorityRead) bool) []authorize.AuthorityFact {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		allowed := true
		if deny != nil {
			allowed = !deny(read)
		}
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     allowed,
		})
	}
	return facts
}

func (authority *fakeAuthority) lastReads() []authorize.AuthorityRead {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if len(authority.calls) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), authority.calls[len(authority.calls)-1]...)
}

// fakePorts implements every task commit port in one deterministic recorder.
type fakePorts struct {
	mu         sync.Mutex
	creates    []task.CreateUnit
	edits      []task.EditUnit
	starts     []task.StartUnit
	asks       []task.AskUnit
	replies    []task.ReplyUnit
	cancels    []task.CancelUnit
	progresses []task.ProgressUnit

	createFn   func(task.CreateUnit) (task.CreateResult, error)
	editFn     func(task.EditUnit) (task.EditResult, error)
	startFn    func(task.StartUnit) (task.StartResult, error)
	askFn      func(task.AskUnit) (task.AskResult, error)
	replyFn    func(task.ReplyUnit) (task.ReplyResult, error)
	cancelFn   func(task.CancelUnit) (task.CancelResult, error)
	progressFn func(task.ProgressUnit) (task.ProgressResult, error)
}

func (ports *fakePorts) Create(_ context.Context, unit task.CreateUnit) (task.CreateResult, error) {
	ports.mu.Lock()
	ports.creates = append(ports.creates, unit)
	callback := ports.createFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	executionID := id.ExecutionID("")
	if unit.Start != nil {
		executionID = unit.Start.NewExecutionID
	}
	return task.CreateResult{
		TaskID:           unit.TaskID,
		ExecutionID:      executionID,
		ContentVersion:   1,
		LifecycleVersion: 1,
	}, nil
}

func (ports *fakePorts) Edit(_ context.Context, unit task.EditUnit) (task.EditResult, error) {
	ports.mu.Lock()
	ports.edits = append(ports.edits, unit)
	callback := ports.editFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	result := task.EditResult{ContentVersion: unit.ExpectedContentVersion + 1}
	if unit.Kind == task.EditSelectPipeline {
		result.ContentVersion = 0
		result.LifecycleVersion = unit.ExpectedLifecycleVersion + 1
	}
	return result, nil
}

func (ports *fakePorts) Start(_ context.Context, unit task.StartUnit) (task.StartResult, error) {
	ports.mu.Lock()
	ports.starts = append(ports.starts, unit)
	callback := ports.startFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	return task.StartResult{ExecutionID: unit.NewExecutionID, LifecycleVersion: unit.ExpectedLifecycleVersion + 1}, nil
}

func (ports *fakePorts) Ask(_ context.Context, unit task.AskUnit) (task.AskResult, error) {
	ports.mu.Lock()
	ports.asks = append(ports.asks, unit)
	callback := ports.askFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	return task.AskResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1, QuestionVersion: 1}, nil
}

func (ports *fakePorts) Reply(_ context.Context, unit task.ReplyUnit) (task.ReplyResult, error) {
	ports.mu.Lock()
	ports.replies = append(ports.replies, unit)
	callback := ports.replyFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	return task.ReplyResult{
		ExecutionID:      unit.NewExecutionID,
		LifecycleVersion: unit.ExpectedLifecycleVersion + 1,
		QuestionVersion:  unit.ExpectedQuestionVersion + 1,
	}, nil
}

func (ports *fakePorts) Cancel(_ context.Context, unit task.CancelUnit) (task.CancelResult, error) {
	ports.mu.Lock()
	ports.cancels = append(ports.cancels, unit)
	callback := ports.cancelFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	return task.CancelResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1}, nil
}

func (ports *fakePorts) Apply(_ context.Context, unit task.ProgressUnit) (task.ProgressResult, error) {
	ports.mu.Lock()
	ports.progresses = append(ports.progresses, unit)
	callback := ports.progressFn
	ports.mu.Unlock()
	if callback != nil {
		return callback(unit)
	}
	return task.ProgressResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1}, nil
}

func (ports *fakePorts) totalCalls() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.creates) + len(ports.edits) + len(ports.starts) + len(ports.asks) +
		len(ports.replies) + len(ports.cancels) + len(ports.progresses)
}

func (ports *fakePorts) lastCreate(t *testing.T) task.CreateUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.creates) == 0 {
		t.Fatal("no create unit was committed")
	}
	return ports.creates[len(ports.creates)-1]
}

func (ports *fakePorts) lastEdit(t *testing.T) task.EditUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.edits) == 0 {
		t.Fatal("no edit unit was committed")
	}
	return ports.edits[len(ports.edits)-1]
}

func (ports *fakePorts) lastStart(t *testing.T) task.StartUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.starts) == 0 {
		t.Fatal("no start unit was committed")
	}
	return ports.starts[len(ports.starts)-1]
}

func (ports *fakePorts) lastAsk(t *testing.T) task.AskUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.asks) == 0 {
		t.Fatal("no ask unit was committed")
	}
	return ports.asks[len(ports.asks)-1]
}

func (ports *fakePorts) lastReply(t *testing.T) task.ReplyUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.replies) == 0 {
		t.Fatal("no reply unit was committed")
	}
	return ports.replies[len(ports.replies)-1]
}

func (ports *fakePorts) lastCancel(t *testing.T) task.CancelUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.cancels) == 0 {
		t.Fatal("no cancel unit was committed")
	}
	return ports.cancels[len(ports.cancels)-1]
}

func (ports *fakePorts) lastProgress(t *testing.T) task.ProgressUnit {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.progresses) == 0 {
		t.Fatal("no progress unit was committed")
	}
	return ports.progresses[len(ports.progresses)-1]
}

func (ports *fakePorts) setCreate(fn func(task.CreateUnit) (task.CreateResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.createFn = fn
}

func (ports *fakePorts) setEdit(fn func(task.EditUnit) (task.EditResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.editFn = fn
}

func (ports *fakePorts) setStart(fn func(task.StartUnit) (task.StartResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.startFn = fn
}

func (ports *fakePorts) setProgress(fn func(task.ProgressUnit) (task.ProgressResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.progressFn = fn
}

func (ports *fakePorts) setReply(fn func(task.ReplyUnit) (task.ReplyResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.replyFn = fn
}

func (ports *fakePorts) setAsk(fn func(task.AskUnit) (task.AskResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.askFn = fn
}

func (ports *fakePorts) setCancel(fn func(task.CancelUnit) (task.CancelResult, error)) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.cancelFn = fn
}

func cloneTask(value task.Task) task.Task {
	cloned := value
	if value.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(value.Metadata))
		maps.Copy(cloned.Metadata, value.Metadata)
	}
	if value.Inputs != nil {
		cloned.Inputs = make(map[id.InputKey]string, len(value.Inputs))
		maps.Copy(cloned.Inputs, value.Inputs)
	}
	cloned.Messages = append([]task.Message(nil), value.Messages...)
	cloned.ExecutionHistory = append([]id.ExecutionID(nil), value.ExecutionHistory...)
	if value.Question != nil {
		question := *value.Question
		question.References = append([]string(nil), value.Question.References...)
		cloned.Question = &question
	}
	return cloned
}

var _ = kernel.ExecutionQueued
