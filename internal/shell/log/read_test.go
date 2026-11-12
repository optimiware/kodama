package log_test

// Behavioral tests for the execution.logs.read operation service. They are
// written before the Reader exists: the package must compile only once
// read.go/read_ports.go declare the service and its consumer-owned execution
// ownership port.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	readerTenant    = "tenant-a"
	readerProject   = "project-a"
	readerExecution = "exec-1"
	readerAction    = "action-1"
	readerCanary    = "s3cr3t-canary-value"
)

var (
	engineerPrincipal  = id.PrincipalID("principal-engineer")
	operatorPrincipal  = id.PrincipalID("principal-operator")
	viewerPrincipal    = id.PrincipalID("principal-viewer")
	adminPrincipal     = id.PrincipalID("principal-admin")
	reviewerPrincipal  = id.PrincipalID("principal-reviewer")
	executionPrincipal = id.PrincipalID("principal-execution")
)

func readerScope() tenancy.Scope {
	return testScope(readerTenant, readerProject)
}

func otherReaderScope() tenancy.Scope {
	return testScope("tenant-b", "project-b")
}

func readerHuman(principalID id.PrincipalID, role tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   readerScope(),
		Roles:   []tenancy.Role{role},
		Enabled: true,
		Version: 1,
	}
}

func readerService(principalID id.PrincipalID, role tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principalID,
		Kind:    identity.KindService,
		Scope:   readerScope(),
		Roles:   []tenancy.Role{role},
		Enabled: true,
		Version: 1,
	}
}

func readerExecutionActor(principalID id.PrincipalID, execution id.ExecutionID) authorize.Principal {
	return authorize.Principal{
		ID:          principalID,
		Kind:        identity.KindExecution,
		Scope:       readerScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: execution,
	}
}

func readerPrincipalFor(actor authorize.Principal) identity.Principal {
	return identity.Principal{
		ID:          actor.ID,
		Kind:        actor.Kind,
		Scope:       actor.Scope,
		Roles:       append([]tenancy.Role(nil), actor.Roles...),
		Enabled:     actor.Enabled,
		Version:     actor.Version,
		ExecutionID: actor.ExecutionID,
	}
}

func readerCommand(actor authorize.Principal) log.ReadCommand {
	return log.ReadCommand{
		Actor:       actor,
		Scope:       actor.Scope,
		ExecutionID: id.ExecutionID(readerExecution),
		ActionID:    id.ActionID(readerAction),
		Stream:      log.StreamStdout,
		Limit:       10,
	}
}

// readerAuthorizer is the injected matrix. It records every call so a test can
// prove the registry row was consulted and that a refused call never reached
// the sink.
type readerAuthorizer struct {
	mu        sync.Mutex
	calls     []readerAuthorizeCall
	authorize func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
}

type readerAuthorizeCall struct {
	Principal authorize.Principal
	Operation authorize.OperationName
	Scope     tenancy.Scope
}

func (authorizer *readerAuthorizer) Authorize(ctx context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	authorizer.mu.Lock()
	authorizer.calls = append(authorizer.calls, readerAuthorizeCall{Principal: principal, Operation: operation, Scope: scope})
	callback := authorizer.authorize
	authorizer.mu.Unlock()
	if callback != nil {
		return callback(ctx, principal, operation, scope)
	}
	return readerRegistryDecision(principal, operation, scope), nil
}

func (authorizer *readerAuthorizer) callCount() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return len(authorizer.calls)
}

func (authorizer *readerAuthorizer) captured() []readerAuthorizeCall {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]readerAuthorizeCall(nil), authorizer.calls...)
}

func readerRegistryDecision(principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) authorize.Decision {
	registered, ok := authorize.LookupOperation(operation)
	if !ok || !principal.Enabled || !readerScopeContains(principal.Scope, scope) {
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

func readerScopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID == "" || target.TenantID == "" || owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// readerAuthority answers current-authority reads. By default every fact holds.
type readerAuthority struct {
	mu    sync.Mutex
	calls [][]authorize.AuthorityRead
	read  func([]authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func (authority *readerAuthority) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	authority.mu.Lock()
	authority.calls = append(authority.calls, append([]authorize.AuthorityRead(nil), reads...))
	callback := authority.read
	authority.mu.Unlock()
	if callback != nil {
		return callback(reads)
	}
	return readerAllowFacts(reads), nil
}

func readerAllowFacts(reads []authorize.AuthorityRead) []authorize.AuthorityFact {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     true,
		})
	}
	return facts
}

func (authority *readerAuthority) callCount() int {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return len(authority.calls)
}

func (authority *readerAuthority) lastReads() []authorize.AuthorityRead {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if len(authority.calls) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), authority.calls[len(authority.calls)-1]...)
}

// readerPrincipals re-reads the current principal row. A missing row is a
// not_found fault so a stale credential cannot act.
type readerPrincipals struct {
	mu   sync.Mutex
	rows map[id.PrincipalID]identity.Principal
}

func (principals *readerPrincipals) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	principals.mu.Lock()
	defer principals.mu.Unlock()
	principal, ok := principals.rows[request.PrincipalID]
	if !ok {
		return identity.Principal{}, fault.New(fault.NotFound, "principal is not present in this scope")
	}
	return principal, nil
}

func (principals *readerPrincipals) put(principal identity.Principal) {
	principals.mu.Lock()
	defer principals.mu.Unlock()
	if principals.rows == nil {
		principals.rows = map[id.PrincipalID]identity.Principal{}
	}
	principals.rows[principal.ID] = principal
}

// readerOwnership is the consumer-owned execution/task association port.
type readerOwnership struct {
	mu    sync.Mutex
	rows  map[id.ExecutionID]log.ExecutionOwnership
	errs  map[id.ExecutionID]error
	calls []readerOwnershipCall
}

type readerOwnershipCall struct {
	Scope       tenancy.Scope
	ExecutionID id.ExecutionID
}

func (ownership *readerOwnership) Ownership(_ context.Context, scope tenancy.Scope, executionID id.ExecutionID) (log.ExecutionOwnership, error) {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	ownership.calls = append(ownership.calls, readerOwnershipCall{Scope: scope, ExecutionID: executionID})
	if err, ok := ownership.errs[executionID]; ok {
		return log.ExecutionOwnership{}, err
	}
	if binding, ok := ownership.rows[executionID]; ok {
		return binding, nil
	}
	return log.ExecutionOwnership{}, fault.New(fault.NotFound, "execution is not present in this scope")
}

func (ownership *readerOwnership) put(executionID id.ExecutionID, binding log.ExecutionOwnership) {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.rows == nil {
		ownership.rows = map[id.ExecutionID]log.ExecutionOwnership{}
	}
	ownership.rows[executionID] = binding
}

func (ownership *readerOwnership) putError(executionID id.ExecutionID, err error) {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.errs == nil {
		ownership.errs = map[id.ExecutionID]error{}
	}
	ownership.errs[executionID] = err
}

func (ownership *readerOwnership) callCount() int {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	return len(ownership.calls)
}

func (ownership *readerOwnership) captured() []readerOwnershipCall {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	return append([]readerOwnershipCall(nil), ownership.calls...)
}

// readerSink is a deterministic RetainedSink. It records every read request so
// a test can prove a refusal happened before any storage read.
type readerSink struct {
	mu       sync.Mutex
	requests []log.ReadRequest
	result   log.ReadResult
	err      error
}

func (sink *readerSink) Append(context.Context, tenancy.Scope, log.Chunk) (log.AppendResult, error) {
	return log.AppendResult{}, nil
}

func (sink *readerSink) Read(_ context.Context, request log.ReadRequest) (log.ReadResult, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.requests = append(sink.requests, request)
	chunks := make([]log.Chunk, 0, len(sink.result.Chunks))
	for _, chunk := range sink.result.Chunks {
		cloned := chunk
		cloned.Data = append([]byte(nil), chunk.Data...)
		chunks = append(chunks, cloned)
	}
	return log.ReadResult{Chunks: chunks, NextOffset: sink.result.NextOffset, Truncated: sink.result.Truncated}, sink.err
}

func (sink *readerSink) callCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.requests)
}

func (sink *readerSink) lastRequest() log.ReadRequest {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.requests) == 0 {
		return log.ReadRequest{}
	}
	return sink.requests[len(sink.requests)-1]
}

type readerEnv struct {
	authorizer *readerAuthorizer
	authority  *readerAuthority
	principals *readerPrincipals
	ownership  *readerOwnership
	sink       *readerSink
	clock      *fakeClock
	reader     *log.Reader
}

func newReaderEnv(t *testing.T) *readerEnv {
	t.Helper()
	return newReaderEnvWithSink(t, &readerSink{}, &fakeClock{now: testNow})
}

func newReaderEnvWithSink(t *testing.T, sink log.RetainedSink, clock *fakeClock) *readerEnv {
	t.Helper()
	env := &readerEnv{
		authorizer: &readerAuthorizer{},
		authority:  &readerAuthority{},
		principals: &readerPrincipals{},
		ownership:  &readerOwnership{},
		clock:      clock,
	}
	if recorder, ok := sink.(*readerSink); ok {
		env.sink = recorder
	}
	reader, err := log.NewReader(log.ReaderConfig{
		Authorizer: env.authorizer,
		Authority:  env.authority,
		Principals: env.principals,
		Ownership:  env.ownership,
		Sink:       sink,
		Clock:      clock,
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	env.reader = reader
	return env
}

// TestReaderAllowsRegistryRoles proves execution.logs.read is a pure read for
// every human and service role the declared registry grants, including the
// viewer role, and that the scope, execution, action and stream reach the sink.
func TestReaderAllowsRegistryRoles(t *testing.T) {
	cases := []struct {
		name  string
		actor authorize.Principal
	}{
		{name: "human engineer", actor: readerHuman(engineerPrincipal, tenancy.RoleEngineer)},
		{name: "human operator", actor: readerHuman(operatorPrincipal, tenancy.RoleOperator)},
		{name: "human viewer", actor: readerHuman(viewerPrincipal, tenancy.RoleViewer)},
		{name: "service engineer", actor: readerService(engineerPrincipal, tenancy.RoleEngineer)},
		{name: "service operator", actor: readerService(operatorPrincipal, tenancy.RoleOperator)},
		{name: "service viewer", actor: readerService(viewerPrincipal, tenancy.RoleViewer)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			env.principals.put(readerPrincipalFor(testCase.actor))
			env.sink.result = log.ReadResult{
				Chunks:     []log.Chunk{testChunk(readerExecution, readerAction, log.StreamStdout, 0, 1, "hello")},
				NextOffset: 0,
			}

			page, err := env.reader.Read(t.Context(), readerCommand(testCase.actor))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != "hello" {
				t.Fatalf("chunks = %+v; want one hello chunk", page.Chunks)
			}
			calls := env.authorizer.captured()
			if len(calls) != 1 || calls[0].Operation != "execution.logs.read" || calls[0].Scope != readerScope() {
				t.Fatalf("authorizer calls = %+v; want one execution.logs.read", calls)
			}
			reads := env.authority.lastReads()
			if len(reads) != 1 || reads[0].Check != authorize.CheckMembership || reads[0].Operation != "execution.logs.read" {
				t.Fatalf("authority reads = %+v; want one membership read", reads)
			}
			request := env.sink.lastRequest()
			// The service asks for one lookahead chunk so it can tell a full
			// page from the end of the stream without a second read.
			if request.Scope != readerScope() || request.ExecutionID != id.ExecutionID(readerExecution) ||
				request.ActionID != id.ActionID(readerAction) || request.Stream != log.StreamStdout || request.Limit != 11 {
				t.Fatalf("sink request = %+v; want the command target with one lookahead chunk", request)
			}
			if env.ownership.callCount() != 0 {
				t.Fatalf("ownership reads = %d; want none for a human/service read", env.ownership.callCount())
			}
		})
	}
}

// TestReaderRefusesUngrantedPrincipals proves a role outside the registry row,
// a disabled actor, a disabled current principal, a removed role and a
// cross-scope actor are all refused Forbidden before any sink read.
func TestReaderRefusesUngrantedPrincipals(t *testing.T) {
	cases := []struct {
		name  string
		actor authorize.Principal
		setup func(*readerEnv)
	}{
		{name: "human admin", actor: readerHuman(adminPrincipal, tenancy.RoleAdmin)},
		{name: "human reviewer", actor: readerHuman(reviewerPrincipal, tenancy.RoleReviewer)},
		{name: "service reviewer", actor: readerService(reviewerPrincipal, tenancy.RoleReviewer)},
		{
			name: "disabled actor",
			actor: func() authorize.Principal {
				actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
				actor.Enabled = false
				return actor
			}(),
		},
		{
			name:  "disabled current principal",
			actor: readerHuman(engineerPrincipal, tenancy.RoleEngineer),
			setup: func(env *readerEnv) {
				principal := readerPrincipalFor(readerHuman(engineerPrincipal, tenancy.RoleEngineer))
				principal.Enabled = false
				env.principals.put(principal)
			},
		},
		{
			name:  "current role removed",
			actor: readerHuman(engineerPrincipal, tenancy.RoleEngineer),
			setup: func(env *readerEnv) {
				env.principals.put(readerPrincipalFor(readerHuman(engineerPrincipal, tenancy.RoleReviewer)))
			},
		},
		{
			name: "actor scope does not contain the target",
			actor: func() authorize.Principal {
				actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
				actor.Scope = otherReaderScope()
				return actor
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			env.principals.put(readerPrincipalFor(testCase.actor))
			if testCase.setup != nil {
				testCase.setup(env)
			}
			command := readerCommand(testCase.actor)
			// The target stays in readerScope; a cross-scope actor must be
			// refused even though its own scope matches its command's default.
			command.Scope = readerScope()
			_, err := env.reader.Read(t.Context(), command)
			requireClass(t, err, fault.Forbidden)
			if env.sink.callCount() != 0 {
				t.Fatalf("sink reads = %d; want none before the refusal", env.sink.callCount())
			}
			if env.ownership.callCount() != 0 {
				t.Fatalf("ownership reads = %d; want none before the refusal", env.ownership.callCount())
			}
		})
	}
}

// TestReaderRequiresCurrentMembershipAuthority proves the commit-boundary
// membership read is authoritative even when the presented grant allowed.
func TestReaderRequiresCurrentMembershipAuthority(t *testing.T) {
	env := newReaderEnv(t)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))
	env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := readerAllowFacts(reads)
		facts[0].Allowed = false
		return facts, nil
	}

	_, err := env.reader.Read(t.Context(), readerCommand(actor))
	requireClass(t, err, fault.Forbidden)
	if env.sink.callCount() != 0 {
		t.Fatalf("sink reads = %d; want none after a denied membership fact", env.sink.callCount())
	}
}

// TestReaderExecutionPrincipalOwnTask proves an execution principal may read
// the log of its own task's current execution, that the ownership port is
// consulted with the exact scope and execution, and that execution authority
// never falls back to a membership read.
func TestReaderExecutionPrincipalOwnTask(t *testing.T) {
	env := newReaderEnv(t)
	actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
	env.principals.put(readerPrincipalFor(actor))
	env.ownership.put(id.ExecutionID(readerExecution), log.ExecutionOwnership{
		TaskID:           id.TaskID("task-1"),
		CurrentExecution: id.ExecutionID(readerExecution),
	})
	env.sink.result = log.ReadResult{Chunks: []log.Chunk{testChunk(readerExecution, readerAction, log.StreamStdout, 0, 1, "own")}}

	page, err := env.reader.Read(t.Context(), readerCommand(actor))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != "own" {
		t.Fatalf("chunks = %+v; want the own-execution chunk", page.Chunks)
	}
	if env.authorizer.callCount() != 0 {
		t.Fatalf("matrix calls = %d; an execution principal is decided by AuthorizeExecution", env.authorizer.callCount())
	}
	if env.authority.callCount() != 0 {
		t.Fatalf("authority reads = %d; an execution principal has no membership fact", env.authority.callCount())
	}
	calls := env.ownership.captured()
	if len(calls) != 1 || calls[0].Scope != readerScope() || calls[0].ExecutionID != id.ExecutionID(readerExecution) {
		t.Fatalf("ownership calls = %+v; want one scoped binding read", calls)
	}
	if env.sink.callCount() != 1 {
		t.Fatalf("sink reads = %d; want one", env.sink.callCount())
	}
}

// TestReaderRefusesForeignStaleOrCrossScopeExecution proves an execution
// principal bound to another task, a previous execution or an execution in
// another scope is refused Forbidden before any sink read.
func TestReaderRefusesForeignStaleOrCrossScopeExecution(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*readerEnv, id.ExecutionID)
	}{
		{
			name: "foreign execution of another task",
			setup: func(env *readerEnv, execution id.ExecutionID) {
				env.ownership.put(execution, log.ExecutionOwnership{
					TaskID:           id.TaskID("task-2"),
					CurrentExecution: id.ExecutionID("exec-foreign"),
				})
			},
		},
		{
			name: "stale previous execution",
			setup: func(env *readerEnv, execution id.ExecutionID) {
				env.ownership.put(execution, log.ExecutionOwnership{
					TaskID:           id.TaskID("task-1"),
					CurrentExecution: id.ExecutionID("exec-current"),
				})
			},
		},
		{
			name: "execution outside the scope",
			setup: func(env *readerEnv, execution id.ExecutionID) {
				env.ownership.putError(execution, fault.New(fault.NotFound, "execution is not present in this scope"))
			},
		},
		{
			name: "task-free execution",
			setup: func(env *readerEnv, execution id.ExecutionID) {
				env.ownership.put(execution, log.ExecutionOwnership{})
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
			env.principals.put(readerPrincipalFor(actor))
			testCase.setup(env, id.ExecutionID(readerExecution))

			_, err := env.reader.Read(t.Context(), readerCommand(actor))
			requireClass(t, err, fault.Forbidden)
			if env.sink.callCount() != 0 {
				t.Fatalf("sink reads = %d; want none before the binding refusal", env.sink.callCount())
			}
		})
	}
}

// TestReaderExecutionPrincipalCannotReadAStaleExecution proves the target must
// be the principal's own execution even when the ownership reader reports that
// execution's task still points at the principal's execution.
func TestReaderExecutionPrincipalCannotReadAStaleExecution(t *testing.T) {
	env := newReaderEnv(t)
	actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
	env.principals.put(readerPrincipalFor(actor))
	stale := id.ExecutionID("exec-stale")
	env.ownership.put(stale, log.ExecutionOwnership{
		TaskID:           id.TaskID("task-1"),
		CurrentExecution: id.ExecutionID(readerExecution),
	})
	command := readerCommand(actor)
	command.ExecutionID = stale

	_, err := env.reader.Read(t.Context(), command)
	requireClass(t, err, fault.Forbidden)
	if env.sink.callCount() != 0 {
		t.Fatalf("sink reads = %d; want none before the stale-target refusal", env.sink.callCount())
	}
	if env.ownership.callCount() != 0 {
		t.Fatalf("ownership reads = %d; want none for a foreign target", env.ownership.callCount())
	}
}

// TestReaderAllowsTenantScopedPrincipal proves the current principal's scope
// must contain the project target, not equal it.
func TestReaderAllowsTenantScopedPrincipal(t *testing.T) {
	env := newReaderEnv(t)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	actor.Scope = tenancy.Scope{TenantID: id.TenantID(readerTenant)}
	env.principals.put(readerPrincipalFor(actor))
	command := readerCommand(actor)
	command.Scope = readerScope()

	page, err := env.reader.Read(t.Context(), command)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 0 {
		t.Fatalf("chunks = %+v; want the fake sink's empty page", page.Chunks)
	}
	if request := env.sink.lastRequest(); request.Scope != readerScope() {
		t.Fatalf("sink scope = %+v; want the project target", request.Scope)
	}
}

// TestReaderRefusesMismatchedExecutionPrincipal proves a disabled execution
// principal, a mismatched execution identity and an ownership dependency
// failure cannot reach the sink.
func TestReaderRefusesMismatchedExecutionPrincipal(t *testing.T) {
	t.Run("disabled execution principal", func(t *testing.T) {
		env := newReaderEnv(t)
		actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
		principal := readerPrincipalFor(actor)
		principal.Enabled = false
		env.principals.put(principal)
		_, err := env.reader.Read(t.Context(), readerCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.sink.callCount() != 0 {
			t.Fatalf("sink reads = %d; want none", env.sink.callCount())
		}
	})

	t.Run("current execution id differs from the actor", func(t *testing.T) {
		env := newReaderEnv(t)
		actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
		principal := readerPrincipalFor(actor)
		principal.ExecutionID = id.ExecutionID("exec-other")
		env.principals.put(principal)
		_, err := env.reader.Read(t.Context(), readerCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.sink.callCount() != 0 {
			t.Fatalf("sink reads = %d; want none", env.sink.callCount())
		}
	})

	t.Run("ownership dependency failure", func(t *testing.T) {
		env := newReaderEnv(t)
		actor := readerExecutionActor(executionPrincipal, id.ExecutionID(readerExecution))
		env.principals.put(readerPrincipalFor(actor))
		env.ownership.putError(id.ExecutionID(readerExecution), fault.New(fault.DependencyUnavailable, "ownership store unavailable"))
		_, err := env.reader.Read(t.Context(), readerCommand(actor))
		requireClass(t, err, fault.DependencyUnavailable)
		if env.sink.callCount() != 0 {
			t.Fatalf("sink reads = %d; want none", env.sink.callCount())
		}
	})
}

// TestReaderLimitBounds proves the service bounds the requested page before the
// sink and fills an omitted limit with the declared default.
func TestReaderLimitBounds(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		wantLimit int
		wantClass fault.Class
	}{
		{name: "zero uses the default", limit: 0, wantLimit: log.DefaultReadLimit},
		{name: "one is accepted", limit: 1, wantLimit: 1},
		{name: "maximum is accepted", limit: log.MaxReadLimit, wantLimit: log.MaxReadLimit},
		{name: "above maximum", limit: log.MaxReadLimit + 1, wantClass: fault.InvalidInput},
		{name: "negative", limit: -1, wantClass: fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			command := readerCommand(actor)
			command.Limit = testCase.limit

			_, err := env.reader.Read(t.Context(), command)
			if testCase.wantClass != "" {
				requireClass(t, err, testCase.wantClass)
				if env.sink.callCount() != 0 {
					t.Fatalf("sink reads = %d; want none for an invalid limit", env.sink.callCount())
				}
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			// The public page bound is preserved; the sink sees one extra
			// lookahead chunk used only to detect the end of the stream.
			if request := env.sink.lastRequest(); request.Limit != testCase.wantLimit+1 {
				t.Fatalf("sink limit = %d; want %d", request.Limit, testCase.wantLimit+1)
			}
		})
	}
}

// TestReaderRefusesInvalidCursorBeforeSink proves a malformed cursor is an
// invalid input and never reaches storage.
func TestReaderRefusesInvalidCursorBeforeSink(t *testing.T) {
	for _, cursor := range []string{"not-a-cursor", "djE\nbm90LWEtbnVtYmVy"} {
		t.Run(cursor, func(t *testing.T) {
			env := newReaderEnv(t)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			command := readerCommand(actor)
			command.Cursor = cursor
			_, err := env.reader.Read(t.Context(), command)
			requireClass(t, err, fault.InvalidInput)
			if env.sink.callCount() != 0 {
				t.Fatalf("sink reads = %d; want none for an invalid cursor", env.sink.callCount())
			}
		})
	}
}

// TestReaderCursorPagingIsStable proves an opaque next cursor continues exactly
// after the last returned chunk without repeating or skipping it.
func TestReaderCursorPagingIsStable(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := readerScope()
	for _, offset := range []uint64{0, 10, 20} {
		if _, err := sink.Append(context.Background(), scope, testChunk(readerExecution, readerAction, log.StreamStdout, offset, offset+1, "chunk")); err != nil {
			t.Fatalf("Append %d: %v", offset, err)
		}
	}
	env := newReaderEnvWithSink(t, sink, clock)
	reader := env.reader
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))

	first, err := reader.Read(t.Context(), func() log.ReadCommand {
		command := readerCommand(actor)
		command.Limit = 2
		return command
	}())
	if err != nil {
		t.Fatalf("Read first page: %v", err)
	}
	if len(first.Chunks) != 2 || first.Chunks[0].Offset != 0 || first.Chunks[1].Offset != 10 {
		t.Fatalf("first page offsets = %+v; want 0 then 10", first.Chunks)
	}
	if first.NextCursor == "" {
		t.Fatal("first page returned no next cursor")
	}
	if strings.Contains(first.NextCursor, readerExecution) || strings.Contains(first.NextCursor, readerAction) {
		t.Fatalf("cursor %q leaks the requested identity", first.NextCursor)
	}

	secondCommand := readerCommand(actor)
	secondCommand.Limit = 2
	secondCommand.Cursor = first.NextCursor
	second, err := reader.Read(t.Context(), secondCommand)
	if err != nil {
		t.Fatalf("Read second page: %v", err)
	}
	if len(second.Chunks) != 1 || second.Chunks[0].Offset != 20 {
		t.Fatalf("second page offsets = %+v; want only 20", second.Chunks)
	}
	if second.NextCursor != "" {
		t.Fatalf("second page next cursor = %q; want empty at the end", second.NextCursor)
	}
}

// newPagedReaderEnv stores one stdout stream with the supplied offsets and
// returns a reader whose engineer principal is current. The chunk sequence is
// the append order, so two chunks may share an offset with distinct sequences.
func newPagedReaderEnv(t *testing.T, offsets []uint64) (*readerEnv, authorize.Principal) {
	t.Helper()
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	for index, offset := range offsets {
		chunk := testChunk(readerExecution, readerAction, log.StreamStdout, offset, uint64(index+1), fmt.Sprintf("chunk-%d-%d", offset, index+1))
		if _, err := sink.Append(context.Background(), readerScope(), chunk); err != nil {
			t.Fatalf("Append %d: %v", offset, err)
		}
	}
	env := newReaderEnvWithSink(t, sink, clock)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))
	return env, actor
}

// pageReaderStream pages one retained stream to exhaustion and returns the
// served chunk keys in order plus every non-empty cursor the reader emitted.
func pageReaderStream(t *testing.T, env *readerEnv, actor authorize.Principal, limit int) ([]string, []string) {
	t.Helper()
	command := readerCommand(actor)
	command.Limit = limit
	var served []string
	var cursors []string
	for page := range 64 {
		result, err := env.reader.Read(t.Context(), command)
		if err != nil {
			t.Fatalf("Read page %d: %v", page+1, err)
		}
		for _, chunk := range result.Chunks {
			served = append(served, fmt.Sprintf("%d:%d", chunk.Offset, chunk.Sequence))
		}
		if result.NextCursor == "" {
			return served, cursors
		}
		cursors = append(cursors, result.NextCursor)
		command.Cursor = result.NextCursor
	}
	t.Fatal("paging did not terminate")
	return nil, nil
}

func requireServedChunks(t *testing.T, served, want []string) {
	t.Helper()
	if len(served) != len(want) {
		t.Fatalf("served chunks = %v; want %v", served, want)
	}
	for index := range want {
		if served[index] != want[index] {
			t.Fatalf("served chunks = %v; want %v", served, want)
		}
	}
}

// TestReaderCursorPagesEveryChunkExactlyOnce proves a page size of one (and two
// or three) cannot collapse the cursor at offset zero and silently end the
// stream: every chunk is served exactly once, in order, and only the final page
// has no cursor.
func TestReaderCursorPagesEveryChunkExactlyOnce(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			env, actor := newPagedReaderEnv(t, []uint64{0, 10, 20})
			served, cursors := pageReaderStream(t, env, actor, limit)
			requireServedChunks(t, served, []string{"0:1", "10:2", "20:3"})
			wantPages := (3 + limit - 1) / limit
			if len(cursors) != wantPages-1 {
				t.Fatalf("non-empty cursors = %d; want %d for %d pages", len(cursors), wantPages-1, wantPages)
			}
		})
	}
}

// TestReaderCursorContinuesAfterSingleByteAtOffsetZero is the offset-zero
// boundary: the first chunk is one byte, so the next chunk starts at offset 1.
// A cursor rule that emitted no cursor when the sink's NextOffset equalled the
// request offset would make the client stop after the offset-zero chunk.
func TestReaderCursorContinuesAfterSingleByteAtOffsetZero(t *testing.T) {
	env, actor := newPagedReaderEnv(t, []uint64{0, 1, 2})
	firstCommand := readerCommand(actor)
	firstCommand.Limit = 1
	first, err := env.reader.Read(t.Context(), firstCommand)
	if err != nil {
		t.Fatalf("Read first page: %v", err)
	}
	if len(first.Chunks) != 1 || first.Chunks[0].Offset != 0 {
		t.Fatalf("first page chunks = %+v; want only offset 0", first.Chunks)
	}
	if first.NextCursor == "" {
		t.Fatal("the offset-zero page returned no cursor; the following chunks would be lost")
	}
	served, cursors := pageReaderStream(t, env, actor, 1)
	requireServedChunks(t, served, []string{"0:1", "1:2", "2:3"})
	if len(cursors) != 2 {
		t.Fatalf("non-empty cursors = %d; want 2", len(cursors))
	}
}

// TestReaderEndsWithEmptyCursorWhenTheStreamFitsOnePage pins the end-of-stream
// rule: the reader requests one lookahead chunk, so a stream with exactly the
// page limit ends with an empty cursor rather than a phantom continuation.
func TestReaderEndsWithEmptyCursorWhenTheStreamFitsOnePage(t *testing.T) {
	cases := []struct {
		limit   int
		offsets []uint64
	}{
		{limit: 2, offsets: []uint64{0, 10}},
		{limit: 3, offsets: []uint64{0, 10, 20}},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("limit %d", testCase.limit), func(t *testing.T) {
			env, actor := newPagedReaderEnv(t, testCase.offsets)
			command := readerCommand(actor)
			command.Limit = testCase.limit
			page, err := env.reader.Read(t.Context(), command)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(page.Chunks) != testCase.limit {
				t.Fatalf("chunks = %d; want %d", len(page.Chunks), testCase.limit)
			}
			if page.NextCursor != "" {
				t.Fatalf("next cursor = %q; want empty at the end of the stream", page.NextCursor)
			}
		})
	}
}

// TestReaderTamperedCursorStaysHarmless proves a well-formed but tampered or
// out-of-range cursor cannot panic, widen the scope or resurrect chunks: it
// only selects a later (offset, sequence) pair, and a cursor naming an
// offset-zero chunk still means strictly after that chunk.
func TestReaderTamperedCursorStaysHarmless(t *testing.T) {
	env, actor := newPagedReaderEnv(t, []uint64{0, 10, 20})
	encode := func(key string) string {
		t.Helper()
		cursor, err := api.EncodeCursor(key)
		if err != nil {
			t.Fatalf("EncodeCursor(%q): %v", key, err)
		}
		return cursor
	}
	cases := []struct {
		name   string
		cursor string
		want   []string
	}{
		{name: "after offset zero", cursor: encode("0:1"), want: []string{"10:2", "20:3"}},
		{name: "between stored offsets", cursor: encode("15:0"), want: []string{"20:3"}},
		{name: "past the end", cursor: encode("100:0"), want: nil},
		{name: "maximum pair", cursor: encode("18446744073709551615:18446744073709551615"), want: nil},
		{name: "future sequence at a stored offset", cursor: encode("10:99"), want: []string{"20:3"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			command := readerCommand(actor)
			command.Cursor = testCase.cursor
			page, err := env.reader.Read(t.Context(), command)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			var served []string
			for _, chunk := range page.Chunks {
				served = append(served, fmt.Sprintf("%d:%d", chunk.Offset, chunk.Sequence))
			}
			requireServedChunks(t, served, testCase.want)
			if page.NextCursor != "" {
				t.Fatalf("next cursor = %q; want empty", page.NextCursor)
			}
		})
	}
}

// TestReaderPagesSharedOffsetGroupsWithoutLoss proves a page may end inside a
// shared-offset group: the (offset, sequence) cursor resumes exactly after the
// last served chunk, so every chunk is served once, in order, and the stream
// still terminates.
func TestReaderPagesSharedOffsetGroupsWithoutLoss(t *testing.T) {
	env, actor := newPagedReaderEnv(t, []uint64{0, 10, 10, 20})
	served, cursors := pageReaderStream(t, env, actor, 2)
	requireServedChunks(t, served, []string{"0:1", "10:2", "10:3", "20:4"})
	if len(cursors) != 1 {
		t.Fatalf("non-empty cursors = %d; want 1", len(cursors))
	}
}

// TestReaderPagesSharedOffsetCases pins the shared-offset cases: a cursor that
// names only an offset would resume strictly after the whole group, so chunks
// that share the page's last offset would be silently lost. Every case must
// serve every chunk exactly once in order with no internal failure.
func TestReaderPagesSharedOffsetCases(t *testing.T) {
	cases := []struct {
		name    string
		offsets []uint64
		limit   int
		want    []string
	}{
		{name: "0,10,10 limit 1", offsets: []uint64{0, 10, 10}, limit: 1, want: []string{"0:1", "10:2", "10:3"}},
		{name: "0,10,10,10 limit 2", offsets: []uint64{0, 10, 10, 10}, limit: 2, want: []string{"0:1", "10:2", "10:3", "10:4"}},
		{name: "0,0,10 limit 1", offsets: []uint64{0, 0, 10}, limit: 1, want: []string{"0:1", "0:2", "10:3"}},
		{name: "0,0,10 limit 2", offsets: []uint64{0, 0, 10}, limit: 2, want: []string{"0:1", "0:2", "10:3"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env, actor := newPagedReaderEnv(t, testCase.offsets)
			served, _ := pageReaderStream(t, env, actor, testCase.limit)
			requireServedChunks(t, served, testCase.want)
		})
	}
}

// TestReaderPagesEmptyChunksAtSharedOffsets proves empty chunks are real
// chunks: they share an offset with a non-empty chunk but each has its own
// sequence and must be served exactly once.
func TestReaderPagesEmptyChunksAtSharedOffsets(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := readerScope()
	chunks := []log.Chunk{
		testChunk(readerExecution, readerAction, log.StreamStdout, 0, 1, ""),
		testChunk(readerExecution, readerAction, log.StreamStdout, 10, 2, "a"),
		testChunk(readerExecution, readerAction, log.StreamStdout, 10, 3, ""),
		testChunk(readerExecution, readerAction, log.StreamStdout, 10, 4, "b"),
		testChunk(readerExecution, readerAction, log.StreamStdout, 20, 5, ""),
	}
	for _, chunk := range chunks {
		if _, err := sink.Append(context.Background(), scope, chunk); err != nil {
			t.Fatalf("Append %d:%d: %v", chunk.Offset, chunk.Sequence, err)
		}
	}
	env := newReaderEnvWithSink(t, sink, clock)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))
	for _, limit := range []int{1, 2, 3} {
		served, _ := pageReaderStream(t, env, actor, limit)
		requireServedChunks(t, served, []string{"0:1", "10:2", "10:3", "10:4", "20:5"})
	}
}

// retainedOnlySink hides the optional pair-aware capability of FileSink so a
// test can prove the operation still pages correctly through the declared
// RetainedSink contract alone.
type retainedOnlySink struct {
	sink log.RetainedSink
}

func (sink *retainedOnlySink) Append(ctx context.Context, scope tenancy.Scope, chunk log.Chunk) (log.AppendResult, error) {
	return sink.sink.Append(ctx, scope, chunk)
}

func (sink *retainedOnlySink) Read(ctx context.Context, request log.ReadRequest) (log.ReadResult, error) {
	return sink.sink.Read(ctx, request)
}

// TestReaderPagesSharedOffsetsThroughOffsetOnlySink proves the shared-offset
// cases also work when the sink exposes only the declared offset-lower-bound
// read contract.
func TestReaderPagesSharedOffsetsThroughOffsetOnlySink(t *testing.T) {
	cases := []struct {
		name    string
		offsets []uint64
		limit   int
		want    []string
	}{
		{name: "0,10,10 limit 1", offsets: []uint64{0, 10, 10}, limit: 1, want: []string{"0:1", "10:2", "10:3"}},
		{name: "0,10,10,10 limit 2", offsets: []uint64{0, 10, 10, 10}, limit: 2, want: []string{"0:1", "10:2", "10:3", "10:4"}},
		{name: "0,0,10 limit 1", offsets: []uint64{0, 0, 10}, limit: 1, want: []string{"0:1", "0:2", "10:3"}},
		{name: "0,0,10 limit 2", offsets: []uint64{0, 0, 10}, limit: 2, want: []string{"0:1", "0:2", "10:3"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := &fakeClock{now: testNow}
			sink := newFileSink(t, defaultSinkConfig(t, clock))
			for index, offset := range testCase.offsets {
				chunk := testChunk(readerExecution, readerAction, log.StreamStdout, offset, uint64(index+1), fmt.Sprintf("chunk-%d-%d", offset, index+1))
				if _, err := sink.Append(context.Background(), readerScope(), chunk); err != nil {
					t.Fatalf("Append %d: %v", offset, err)
				}
			}
			env := newReaderEnvWithSink(t, &retainedOnlySink{sink: sink}, clock)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			served, _ := pageReaderStream(t, env, actor, testCase.limit)
			requireServedChunks(t, served, testCase.want)
		})
	}
}

// cappedSink hides the pair-aware capability and refuses a window above a
// fixed cap, so a test can exercise the offset-only fallback's window search.
type cappedSink struct {
	sink log.RetainedSink
	cap  int
}

func (sink *cappedSink) Append(ctx context.Context, scope tenancy.Scope, chunk log.Chunk) (log.AppendResult, error) {
	return sink.sink.Append(ctx, scope, chunk)
}

func (sink *cappedSink) Read(ctx context.Context, request log.ReadRequest) (log.ReadResult, error) {
	if request.Limit > sink.cap {
		return log.ReadResult{}, fault.New(fault.InvalidInput, "window exceeds the test cap")
	}
	return sink.sink.Read(ctx, request)
}

// TestReaderPagesOffsetOnlySinkNearItsWindowCap proves the fallback searches
// the accepted window range instead of failing when the next doubled window
// would exceed the sink's cap: the remaining chunks of a shared-offset group
// are still served exactly once.
func TestReaderPagesOffsetOnlySinkNearItsWindowCap(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	const shared = 9
	want := make([]string, 0, shared)
	for index := range shared {
		chunk := testChunk(readerExecution, readerAction, log.StreamStdout, 10, uint64(index+1), fmt.Sprintf("shared-%d", index+1))
		if _, err := sink.Append(context.Background(), readerScope(), chunk); err != nil {
			t.Fatalf("Append %d: %v", index+1, err)
		}
		want = append(want, fmt.Sprintf("10:%d", index+1))
	}
	env := newReaderEnvWithSink(t, &cappedSink{sink: sink, cap: 10}, clock)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))
	served, _ := pageReaderStream(t, env, actor, 7)
	requireServedChunks(t, served, want)
}

// TestReaderPagesGroupLargerThanSinkWindow proves a shared-offset group larger
// than the sink's maximum read window still pages through chunk by chunk.
func TestReaderPagesGroupLargerThanSinkWindow(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Limits.MaxReadChunks = 10
	sink := newFileSink(t, config)
	const shared = 25
	want := make([]string, 0, shared+1)
	for index := range shared {
		chunk := testChunk(readerExecution, readerAction, log.StreamStdout, 10, uint64(index+1), fmt.Sprintf("shared-%d", index+1))
		if _, err := sink.Append(context.Background(), readerScope(), chunk); err != nil {
			t.Fatalf("Append shared %d: %v", index+1, err)
		}
		want = append(want, fmt.Sprintf("10:%d", index+1))
	}
	last := testChunk(readerExecution, readerAction, log.StreamStdout, 20, shared+1, "after")
	if _, err := sink.Append(context.Background(), readerScope(), last); err != nil {
		t.Fatalf("Append after: %v", err)
	}
	want = append(want, fmt.Sprintf("20:%d", shared+1))
	env := newReaderEnvWithSink(t, sink, clock)
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))
	for _, limit := range []int{1, 5} {
		served, _ := pageReaderStream(t, env, actor, limit)
		requireServedChunks(t, served, want)
	}
}

// TestReaderPagesRandomStreamsExactlyOnce is a seeded property test: random
// streams with shared and large offset groups, empty chunks and a random sink
// read window are paged at every public small limit. Each chunk must be served
// exactly once in order and paging must terminate.
func TestReaderPagesRandomStreamsExactlyOnce(t *testing.T) {
	random := rand.New(rand.NewSource(20261104))
	for trial := range 30 {
		var offsets []uint64
		var data []string
		offset := uint64(0)
		groups := random.Intn(7)
		for range groups {
			size := 1 + random.Intn(5)
			if random.Intn(5) == 0 {
				// A group larger than the small sink windows exercised below.
				size = 8 + random.Intn(8)
			}
			for index := 0; index < size; index++ {
				offsets = append(offsets, offset)
				if random.Intn(4) == 0 {
					data = append(data, "")
				} else {
					data = append(data, fmt.Sprintf("chunk-%d-%d", offset, index))
				}
			}
			offset += uint64(1 + random.Intn(25))
		}
		want := make([]string, 0, len(offsets))
		for index, chunkOffset := range offsets {
			want = append(want, fmt.Sprintf("%d:%d", chunkOffset, index+1))
		}
		for limit := 1; limit <= 5; limit++ {
			window := 1 + random.Intn(10)
			clock := &fakeClock{now: testNow}
			config := defaultSinkConfig(t, clock)
			config.Limits.MaxReadChunks = window
			sink := newFileSink(t, config)
			for index, chunkOffset := range offsets {
				chunk := testChunk(readerExecution, readerAction, log.StreamStdout, chunkOffset, uint64(index+1), data[index])
				if _, err := sink.Append(context.Background(), readerScope(), chunk); err != nil {
					t.Fatalf("trial %d append %d:%d: %v", trial, chunkOffset, index+1, err)
				}
			}
			env := newReaderEnvWithSink(t, sink, clock)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			served, _ := pageReaderStream(t, env, actor, limit)
			requireServedChunks(t, served, want)
		}
	}
}

// TestReaderDoesNotServeExpiredRetention proves the operation never serves a
// chunk at or after its stored time plus retention.
func TestReaderDoesNotServeExpiredRetention(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	if _, err := sink.Append(context.Background(), readerScope(), testChunk(readerExecution, readerAction, log.StreamStdout, 10, 1, "expired")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	env := newReaderEnvWithSink(t, sink, clock)
	reader := env.reader
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))

	clock.Advance(log.DefaultRetention().RetainFor + time.Hour)
	page, err := reader.Read(t.Context(), readerCommand(actor))
	if err != nil {
		t.Fatalf("Read expired: %v", err)
	}
	if len(page.Chunks) != 0 {
		t.Fatalf("expired page = %+v; want no chunks", page.Chunks)
	}
	if !page.Truncated {
		t.Fatal("expired page did not report truncation")
	}
}

// TestReaderNeverServesCrossScopeContent proves a read in another tenant or
// project cannot observe a chunk stored under a different scope.
func TestReaderNeverServesCrossScopeContent(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	if _, err := sink.Append(context.Background(), readerScope(), testChunk(readerExecution, readerAction, log.StreamStdout, 0, 1, "tenant-a-only")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	env := newReaderEnvWithSink(t, sink, clock)
	reader := env.reader
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	actor.Scope = otherReaderScope()
	env.principals.put(readerPrincipalFor(actor))
	command := readerCommand(actor)
	command.Scope = otherReaderScope()

	page, err := reader.Read(t.Context(), command)
	if err != nil {
		t.Fatalf("Read other scope: %v", err)
	}
	if len(page.Chunks) != 0 {
		t.Fatalf("other-scope page = %+v; want no chunks", page.Chunks)
	}
}

// TestReaderPreservesRedactionEndToEnd proves a secret written through the
// retained sink is never served by the operation, even when it is split across
// two chunks.
func TestReaderPreservesRedactionEndToEnd(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Redact = []string{readerCanary}
	sink := newFileSink(t, config)
	scope := readerScope()
	first := "prefix s3cr3t-"
	second := "canary-value suffix"
	if _, err := sink.Append(context.Background(), scope, testChunk(readerExecution, readerAction, log.StreamStdout, 0, 1, first)); err != nil {
		t.Fatalf("Append first: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk(readerExecution, readerAction, log.StreamStdout, 100, 2, second)); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	env := newReaderEnvWithSink(t, sink, clock)
	reader := env.reader
	actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
	env.principals.put(readerPrincipalFor(actor))

	page, err := reader.Read(t.Context(), readerCommand(actor))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	output := concatChunks(page.Chunks)
	if strings.Contains(output, readerCanary) || strings.Contains(output, "s3cr3t-") {
		t.Fatalf("served output %q contains the configured secret", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Fatalf("served output %q did not preserve the redaction marker", output)
	}
}

// TestReaderSanitizesSinkFailures proves an adapter failure is mapped to a
// public fault whose rendered message never carries a storage path or the
// private cause.
func TestReaderSanitizesSinkFailures(t *testing.T) {
	const privatePath = "/private/retained-log/secret-canary"
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "fault with private cause",
			err:  fault.Wrap(fault.InternalFailure, "retained log read failed", errors.New("open "+privatePath+": permission denied")),
		},
		{
			name: "plain adapter error",
			err:  errors.New("open " + privatePath + ": permission denied"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			env.sink.err = testCase.err

			_, err := env.reader.Read(t.Context(), readerCommand(actor))
			requireClass(t, err, fault.InternalFailure)
			if strings.Contains(err.Error(), privatePath) {
				t.Fatalf("error %q leaks a storage path", err.Error())
			}
		})
	}
}

// TestReaderRefusesAChunkOutsideTheRequest proves a buggy adapter cannot widen
// the read to another execution, action or stream.
func TestReaderRefusesAChunkOutsideTheRequest(t *testing.T) {
	cases := []struct {
		name  string
		chunk log.Chunk
	}{
		{name: "another execution", chunk: testChunk("exec-other", readerAction, log.StreamStdout, 0, 1, "leak")},
		{name: "another action", chunk: testChunk(readerExecution, "action-other", log.StreamStdout, 0, 1, "leak")},
		{name: "another stream", chunk: testChunk(readerExecution, readerAction, log.StreamStderr, 0, 1, "leak")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newReaderEnv(t)
			actor := readerHuman(engineerPrincipal, tenancy.RoleEngineer)
			env.principals.put(readerPrincipalFor(actor))
			env.sink.result = log.ReadResult{Chunks: []log.Chunk{testCase.chunk}}
			_, err := env.reader.Read(t.Context(), readerCommand(actor))
			requireClass(t, err, fault.InternalFailure)
		})
	}
}

// TestNewReaderRequiresEveryDependency proves the service refuses an incomplete
// boundary instead of accepting a nil collaborator.
func TestNewReaderRequiresEveryDependency(t *testing.T) {
	base := log.ReaderConfig{
		Authorizer: &readerAuthorizer{},
		Authority:  &readerAuthority{},
		Principals: &readerPrincipals{},
		Ownership:  &readerOwnership{},
		Sink:       &readerSink{},
		Clock:      &fakeClock{now: testNow},
	}
	if _, err := log.NewReader(base); err != nil {
		t.Fatalf("NewReader with every dependency: %v", err)
	}
	cases := []struct {
		name string
		edit func(*log.ReaderConfig)
	}{
		{name: "no authorizer", edit: func(config *log.ReaderConfig) { config.Authorizer = nil }},
		{name: "no authority", edit: func(config *log.ReaderConfig) { config.Authority = nil }},
		{name: "no principals", edit: func(config *log.ReaderConfig) { config.Principals = nil }},
		{name: "no ownership", edit: func(config *log.ReaderConfig) { config.Ownership = nil }},
		{name: "no sink", edit: func(config *log.ReaderConfig) { config.Sink = nil }},
		{name: "no clock", edit: func(config *log.ReaderConfig) { config.Clock = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			testCase.edit(&config)
			_, err := log.NewReader(config)
			requireClass(t, err, fault.InternalFailure)
		})
	}
}

// Compile-time pins for the new declarations.
var (
	_ log.ExecutionOwnershipReader = (*readerOwnership)(nil)
	_ log.RetainedSink             = (*readerSink)(nil)
	_                              = log.NewReader
	_                              = log.DefaultReadLimit
	_                              = log.MaxReadLimit
	_                              = log.ReaderConfig{}
	_                              = log.ReadCommand{}
	_                              = log.ReadPage{}
)
