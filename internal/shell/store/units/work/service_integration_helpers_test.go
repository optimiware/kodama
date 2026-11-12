package work

// This file wires the real capability services onto the work adapters over the
// memory store. Hand-built commit units hid a class of defect: the strict
// authority mapper accepted shapes the services never emit and refused the
// shapes they do emit. The services are the source of truth for every authority
// read the adapters must map.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

const integrationImage = "registry.example.com/run@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// integrationClock is the injected service clock.
type integrationClock struct{ now time.Time }

func (clock integrationClock) Now() time.Time { return clock.now }

// integrationIDs supplies deterministic unique identities to both services.
type integrationIDs struct {
	mu        sync.Mutex
	task      int
	question  int
	execution int
	principal int
	event     int
	receipt   int
}

func (ids *integrationIDs) NewTaskID() (id.TaskID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.task++
	return id.TaskID(fmt.Sprintf("task-%d", ids.task)), nil
}

func (ids *integrationIDs) NewQuestionID() (task.QuestionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.question++
	return task.QuestionID(fmt.Sprintf("question-%d", ids.question)), nil
}

func (ids *integrationIDs) NewExecutionID() (id.ExecutionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.execution++
	return id.ExecutionID(fmt.Sprintf("execution-%d", ids.execution)), nil
}

func (ids *integrationIDs) NewPrincipalID() (id.PrincipalID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.principal++
	return id.PrincipalID(fmt.Sprintf("execution-principal-%d", ids.principal)), nil
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

// allowAuthorizer is the grant-stage fake; the adapter's current-authority
// mapping is what these tests exercise.
type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionAllow, nil
}

// confirmAuthority is the pre-port authority-read fake. It answers exactly the
// reads the service asks, with the expected version, so the commit reaches the
// adapter and its real in-transaction mapping.
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

// integrationPrincipals re-reads the acting principal, human or execution. The
// task service re-reads the current principal of every actor, so an execution
// principal's running execution identity must resolve here too.
type integrationPrincipals struct {
	principals map[id.PrincipalID]identity.Principal
}

func (reader integrationPrincipals) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	principal, ok := reader.principals[request.PrincipalID]
	if !ok || request.Scope != principal.Scope {
		return identity.Principal{}, fault.New(fault.NotFound, "principal is not present in this scope")
	}
	return principal, nil
}

// storeExecutionReader reads the committed execution fact the task service uses
// as its predecessor evidence. It is the real store-backed reader so the
// service sees the persisted stop barrier, which is what gates replacement
// dispatch after a cancelled predecessor.
type storeExecutionReader struct{ reader *readers.ExecutionReader }

func newStoreExecutionReader(t *testing.T, store *memory.Store) storeExecutionReader {
	t.Helper()
	reader, err := readers.NewExecutionReader(store, store)
	if err != nil {
		t.Fatalf("readers.NewExecutionReader: %v", err)
	}
	return storeExecutionReader{reader: reader}
}

func (reader storeExecutionReader) Get(ctx context.Context, scope tenancy.Scope, identity id.ExecutionID) (task.ExecutionView, error) {
	return reader.reader.Get(ctx, scope, identity)
}

// recordingPorts wraps the task ports so a test can inspect exactly the unit
// the real service built and, when needed, interleave another commit between
// the service's read and the adapter's commit.
type recordingPorts struct {
	mu      sync.Mutex
	asks    []task.AskUnit
	cancels []task.CancelUnit
	replies []task.ReplyUnit
	hook    func()
}

func (ports *recordingPorts) setHook(hook func()) {
	ports.mu.Lock()
	ports.hook = hook
	ports.mu.Unlock()
}

func (ports *recordingPorts) enter() func() {
	ports.mu.Lock()
	hook := ports.hook
	ports.hook = nil
	ports.mu.Unlock()
	return hook
}

func (ports *recordingPorts) lastAsk() task.AskUnit {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.asks) == 0 {
		return task.AskUnit{}
	}
	return ports.asks[len(ports.asks)-1]
}

func (ports *recordingPorts) lastCancel() task.CancelUnit {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.cancels) == 0 {
		return task.CancelUnit{}
	}
	return ports.cancels[len(ports.cancels)-1]
}

func (ports *recordingPorts) lastReply() task.ReplyUnit {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.replies) == 0 {
		return task.ReplyUnit{}
	}
	return ports.replies[len(ports.replies)-1]
}

type recordingAskPort struct {
	inner task.AskPort
	ports *recordingPorts
}

func (port recordingAskPort) Ask(ctx context.Context, unit task.AskUnit) (task.AskResult, error) {
	port.ports.mu.Lock()
	port.ports.asks = append(port.ports.asks, unit)
	port.ports.mu.Unlock()
	if hook := port.ports.enter(); hook != nil {
		hook()
	}
	return port.inner.Ask(ctx, unit)
}

type recordingCancelPort struct {
	inner task.CancelPort
	ports *recordingPorts
}

func (port recordingCancelPort) Cancel(ctx context.Context, unit task.CancelUnit) (task.CancelResult, error) {
	port.ports.mu.Lock()
	port.ports.cancels = append(port.ports.cancels, unit)
	port.ports.mu.Unlock()
	if hook := port.ports.enter(); hook != nil {
		hook()
	}
	return port.inner.Cancel(ctx, unit)
}

type recordingReplyPort struct {
	inner task.ReplyPort
	ports *recordingPorts
}

func (port recordingReplyPort) Reply(ctx context.Context, unit task.ReplyUnit) (task.ReplyResult, error) {
	port.ports.mu.Lock()
	port.ports.replies = append(port.ports.replies, unit)
	port.ports.mu.Unlock()
	if hook := port.ports.enter(); hook != nil {
		hook()
	}
	return port.inner.Reply(ctx, unit)
}

type recordingStartPort struct {
	inner task.StartPort
	ports *recordingPorts
}

func (port recordingStartPort) Start(ctx context.Context, unit task.StartUnit) (task.StartResult, error) {
	if hook := port.ports.enter(); hook != nil {
		hook()
	}
	return port.inner.Start(ctx, unit)
}

// integrationAdmission resolves the current pipeline the real service selected.
// It is the only non-store dependency with real admission policy removed.
type integrationAdmission struct {
	pipelines *Pipelines
	scope     tenancy.Scope
}

func (admission integrationAdmission) Admit(ctx context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
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
	container := definition.Container{Image: integrationImage, Commands: []string{"echo", "ok"}}
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

// realServiceEnv owns one store with the real task service, the real pipeline
// management service and every work adapter under test.
type realServiceEnv struct {
	fixture    *fixture
	scope      tenancy.Scope
	service    *task.Service
	management *pipeline.Management
	ids        *integrationIDs
	principals integrationPrincipals
	ports      *recordingPorts
}

func newRealServiceEnv(t *testing.T) *realServiceEnv {
	t.Helper()
	return newRealServiceEnvWith(t, nil)
}

// newRealServiceEnvWith builds the same real-service environment with an
// optional admission factory (for a request-only flow).
func newRealServiceEnvWith(t *testing.T, admission func(*fixture, tenancy.Scope) task.Admission) *realServiceEnv {
	t.Helper()
	return newRealServiceEnvConfigured(t, admission, nil)
}

// newRealServiceEnvWithExecutor builds the same real-service environment with a
// wrapped task adapter executor. The wrapper lets a test interleave a store
// commit after the port's authority read built its pins and before the unit
// commits, which is exactly the confirmation-to-commit window the union pins
// must guard.
func newRealServiceEnvWithExecutor(t *testing.T, wrap func(store.Executor) store.Executor) *realServiceEnv {
	t.Helper()
	return newRealServiceEnvConfigured(t, nil, wrap)
}

// newRealServiceEnvConfigured builds the shared environment from the optional
// admission and executor wrappers.
func newRealServiceEnvConfigured(t *testing.T, admission func(*fixture, tenancy.Scope) task.Admission, wrap func(store.Executor) store.Executor) *realServiceEnv {
	t.Helper()
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedMembership(t, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer, tenancy.RoleAdmin)
	principal := identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
	principals := integrationPrincipals{principals: map[id.PrincipalID]identity.Principal{principal.ID: principal}}
	ids := &integrationIDs{}
	aggregates := executionstore.New(fixture.store, fixture.store)
	ports := &recordingPorts{}
	admitted := task.Admission(integrationAdmission{pipelines: fixture.pipelines, scope: scope})
	if admission != nil {
		admitted = admission(fixture, scope)
	}
	taskPorts := fixture.task
	if wrap != nil {
		wrapped, err := NewTask(wrap(fixture.store), fixture.store)
		if err != nil {
			t.Fatalf("NewTask with wrapped executor: %v", err)
		}
		taskPorts = wrapped
	}
	service, err := task.NewService(task.ServiceConfig{
		Clock:          integrationClock{now: testNow},
		IDs:            ids,
		Authorizer:     allowAuthorizer{},
		Authority:      confirmAuthority{},
		Principals:     principals,
		Pipelines:      fixture.pipelines,
		Reader:         fixture.task,
		Executions:     newStoreExecutionReader(t, fixture.store),
		Aggregates:     aggregates,
		Admission:      admitted,
		Creates:        taskPorts,
		Edits:          taskPorts,
		Starts:         recordingStartPort{inner: taskPorts, ports: ports},
		Asks:           recordingAskPort{inner: taskPorts, ports: ports},
		Replies:        recordingReplyPort{inner: taskPorts, ports: ports},
		Cancels:        recordingCancelPort{inner: taskPorts, ports: ports},
		Progress:       taskPorts,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("task.NewService: %v", err)
	}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      integrationClock{now: testNow},
		IDs:        ids,
		Authorizer: allowAuthorizer{},
		Authority:  confirmAuthority{},
		Changes:    fixture.pipelines,
		Reader:     fixture.pipelines,
	})
	if err != nil {
		t.Fatalf("pipeline.NewManagement: %v", err)
	}
	return &realServiceEnv{fixture: fixture, scope: scope, service: service, management: management, ids: ids, principals: principals, ports: ports}
}

func (env *realServiceEnv) actor() authorize.Principal {
	return authorize.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   env.scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
}

// registerHuman registers one human credential the real service re-reads through
// its PrincipalReader. The stored memberships are seeded separately so a test
// controls the tenant-wide and exact-project rows independently.
func (env *realServiceEnv) registerHuman(t *testing.T, principalID id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	t.Helper()
	principal := identity.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   env.scope,
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
	env.principals.principals[principalID] = principal
	return authorize.Principal(principal)
}

// mutatingExecutor runs a test hook immediately before it delegates one Execute
// call to the underlying executor. The port has already read the current
// authority and built its pins at that point, so the hook commits in exactly
// the confirmation-to-commit window the pins must guard.
type mutatingExecutor struct {
	inner store.Executor
	hook  func()
}

func (executor *mutatingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if executor.hook != nil {
		hook := executor.hook
		executor.hook = nil
		hook()
	}
	return executor.inner.Execute(ctx, scope, unit)
}

func (executor *mutatingExecutor) Read(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	return executor.inner.Read(ctx, scope, table, identity)
}

// executeDirect commits one raw store unit directly through the memory store,
// bypassing the wrapped executor so a mutation hook does not recurse.
func (env *realServiceEnv) executeDirect(t *testing.T, scope tenancy.Scope, unit store.Unit) {
	t.Helper()
	if _, err := env.fixture.store.Execute(context.Background(), scope, unit); err != nil {
		t.Fatalf("direct store commit: %v", err)
	}
}

// setMembershipRows commits a role update at the next version for one membership
// row through the raw store.
func (env *realServiceEnv) setMembershipRows(t *testing.T, scope tenancy.Scope, identity string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := jsonColumn(roles)
	if err != nil {
		t.Fatalf("encode membership roles: %v", err)
	}
	env.executeDirect(t, scope, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            identity,
		ExpectVersion: version,
		Values:        map[string]any{"roles": encoded},
	}}})
}

// recreateMembershipRow deletes one membership row and grants a fresh row at the
// supplied identity and version, the ABA shape an identity pin must refuse even
// when the recreated version equals the observed one.
func (env *realServiceEnv) recreateMembershipRow(t *testing.T, scope tenancy.Scope, principal, oldIdentity string, version uint64, newIdentity string, roles ...tenancy.Role) {
	t.Helper()
	env.executeDirect(t, scope, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            oldIdentity,
		Delete:        true,
		ExpectVersion: version,
	}}})
	env.fixture.seedMembership(t, scope, newIdentity, principal, version, roles...)
}

func integrationContent(label string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"` + label + `"}`)}
}

func integrationPipelineYAML(command string) []byte {
	return []byte("actions:\n  - name: build\n    image: " + integrationImage + "\n    commands:\n      - " + command + "\n")
}

func integrationPushYAML() []byte {
	return []byte("repository: repo-1\nservice: service-1\non:\n  push:\n    - refs/heads/main\n" +
		"inputs: [repository, ref, commit, previous_commit]\n" +
		"actions:\n  - name: test\n    image: " + integrationImage + "\n    commands:\n      - echo ok\n")
}

// fakePendingEvents is the processor's committed event table.
type fakePendingEvents struct{ rows []event.PendingEvent }

func (events fakePendingEvents) Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error) {
	return events.rows, nil
}

// fakeCompletionPort records completions; the authority proof is in Select and
// Start, which are the real adapters.
type fakeCompletionPort struct{ completed []trigger.CompletionUnit }

func (port *fakeCompletionPort) Complete(_ context.Context, unit trigger.CompletionUnit) error {
	port.completed = append(port.completed, unit)
	return nil
}

// integrationPushEvent is a human push report in the exact shape the
// verified-report ingress commits: the enforcing Git service is the source,
// the causation is the cited verification, there is no execution correlation,
// and the data is the verified Git subject.
func integrationPushEvent(scope tenancy.Scope) event.Envelope {
	return event.Envelope{
		ID:          "event-push-1",
		Type:        event.TypeVerifiedReport,
		ReportType:  trigger.ReportTypeGitRefUpdated,
		Source:      event.Source{Kind: event.SourceService, PrincipalID: integrationGitService},
		At:          testNow,
		Scope:       scope,
		SubjectID:   "repo-1",
		CausationID: "verification-push-1",
		Data: []byte(`{"candidate":"2222222","expected_base":"1111111","operation":"git.ref.update",` +
			`"ref":"refs/heads/main","resource":"repo-1"}`),
	}
}

// integrationGitService is the enforcing (reporting) Git service of repo-1. It
// differs from the pipeline's automatic-start service, service-1.
const integrationGitService = "git-service-1"
