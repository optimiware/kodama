package inbox_test

// Inbox API pinned by these tests.
//
// The inbox is a query over active request actions, waiting tasks and failed
// open tasks: it is not an independently mutable request entity, so
// authoritative state lives on the execution/action and task rows. The package
// declares Kind, Query, Item, Page and QueryPort, and this read-only service
// seam on top of them:
//
//	type ServiceConfig struct {
//	    Authorizer authorize.Authorizer
//	    Authority  authorize.AuthorityReader
//	    Queries    QueryPort
//	}
//
//	type QueryCommand struct {
//	    Actor  authorize.Principal
//	    Scope  tenancy.Scope
//	    Kinds  []Kind
//	    Cursor string
//	    Limit  int
//	}
//
//	const MaxPageSize = api.MaxPageLimit
//
//	type Service struct{ /* ... */ }
//	func NewService(ServiceConfig) (*Service, error)
//	func (*Service) Query(context.Context, QueryCommand) (Page, error)
//
// Pinned rules (S06 §5):
//
//   - NewService refuses a nil Authorizer, Authority or Queries with
//     internal_failure, matching the sibling task service. The service has no
//     event, receipt or mutation dependency: the inbox owns no mutable
//     entity.
//   - Query validates before any authorizer or port call. Scope must be a
//     valid project scope (an empty project id is invalid_input). Actor.ID
//     must be a valid principal id and Actor.Kind a declared kind, every
//     supplied Kind must be declared, and Limit must be 0 (adapter default) or
//     1..MaxPageSize. Violations are invalid_input.
//   - Actor.Scope must equal the command Scope; a mismatch is forbidden
//     before the authorizer is called.
//   - The service authorizes exactly task.inbox for the actor and scope; a
//     deny is forbidden. A previously calculated allow is not durable
//     permission: the service then reads the registry's current checks for
//     task.inbox (CheckMembership, CheckPrincipalEnabled, in that order)
//     through Authority with Operation "task.inbox", the command scope and
//     the actor principal id. Any fact that is not Allowed is forbidden. The
//     port is never called after a refusal.
//   - An execution principal has no inbox grant (ExecNone) and is refused
//     with forbidden.
//   - The service calls Queries.Query exactly once with Query{Scope: command
//     scope, Principal: the actor, Kinds: a copy of the supplied kinds in
//     order (empty when none), Cursor: the command cursor verbatim, Limit:
//     the command limit verbatim}. It returns the Page unchanged, including
//     NextCursor; it never interprets, rewrites, reorders or truncates a
//     cursor or item.
//   - A port error is returned unchanged (same fault class, errors.Is) and
//     nothing is cached. The next Query reads authoritative state again, so a
//     resolved item disappears without any event delivery, claim or
//     acknowledgement.
//   - There is no claim/acknowledge lifecycle and no mutation seam: Service
//     exposes only Query, QueryPort only Query, and the command/query/page/
//     item values carry no state, claim, acknowledgement or persistence
//     field.

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, time.November, 4, 9, 0, 0, 0, time.UTC)

// Compile-time pins for the added declarations and signatures.
var (
	_ inbox.QueryPort = (*fakeQueryPort)(nil)

	_ func(inbox.ServiceConfig) (*inbox.Service, error)                             = inbox.NewService
	_ func(*inbox.Service, context.Context, inbox.QueryCommand) (inbox.Page, error) = (*inbox.Service).Query
	_                                                                               = inbox.MaxPageSize
)

// Pinned struct shapes. Keyed composite literals make a missing or renamed
// field a compile-time failure here instead of a silent behavioral gap.
var (
	_ = inbox.ServiceConfig{
		Authorizer: nil,
		Authority:  nil,
		Principals: nil,
		Queries:    nil,
	}
	_ = inbox.QueryCommand{
		Actor:  authorize.Principal{},
		Scope:  tenancy.Scope{},
		Kinds:  []inbox.Kind{},
		Cursor: "",
		Limit:  0,
	}
)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-b")}
}

func humanActor(principal id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func serviceActor(principal id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func executionActor(principal id.PrincipalID, execution id.ExecutionID) authorize.Principal {
	return authorize.Principal{
		ID:          principal,
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: execution,
	}
}

func roleAssignment(t *testing.T, role tenancy.Role) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("role/" + string(role))
	if err != nil {
		t.Fatalf("ParseAssignment role: %v", err)
	}
	return assignment
}

func serviceAssignment(t *testing.T, service string) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("service/" + service)
	if err != nil {
		t.Fatalf("ParseAssignment service: %v", err)
	}
	return assignment
}

func requireClass(t *testing.T, err error, want fault.Class) {
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

type testEnv struct {
	service    *inbox.Service
	config     inbox.ServiceConfig
	authorizer *fakeAuthorizer
	authority  *fakeAuthority
	principals *fakePrincipals
	port       *fakeQueryPort
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	authorizer := &fakeAuthorizer{}
	env := &testEnv{
		authorizer: authorizer,
		authority:  &fakeAuthority{},
		principals: &fakePrincipals{authorizer: authorizer},
		port:       &fakeQueryPort{},
	}
	env.config = inbox.ServiceConfig{
		Authorizer: env.authorizer,
		Authority:  env.authority,
		Principals: env.principals,
		Queries:    env.port,
	}
	service, err := inbox.NewService(env.config)
	requireNoError(t, err, "NewService")
	env.service = service
	return env
}

func queryCommand(actor authorize.Principal) inbox.QueryCommand {
	return inbox.QueryCommand{Actor: actor, Scope: testScope(), Limit: 10}
}

// TestNewServiceRequiresDependencies proves the service cannot be built
// without the authorizer, the authority reader, the principal reader and the
// query port.
func TestNewServiceRequiresDependencies(t *testing.T) {
	t.Parallel()

	cases := map[string]inbox.ServiceConfig{
		"missing authorizer": {
			Authority:  &fakeAuthority{},
			Principals: &fakePrincipals{},
			Queries:    &fakeQueryPort{},
		},
		"missing authority": {
			Authorizer: &fakeAuthorizer{},
			Principals: &fakePrincipals{},
			Queries:    &fakeQueryPort{},
		},
		"missing principals": {
			Authorizer: &fakeAuthorizer{},
			Authority:  &fakeAuthority{},
			Queries:    &fakeQueryPort{},
		},
		"missing queries": {
			Authorizer: &fakeAuthorizer{},
			Authority:  &fakeAuthority{},
			Principals: &fakePrincipals{},
		},
	}
	for name, config := range cases {
		config := config
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service, err := inbox.NewService(config)
			requireClass(t, err, fault.InternalFailure)
			if service != nil {
				t.Fatal("NewService returned a service with missing dependencies")
			}
		})
	}

	service, err := inbox.NewService(inbox.ServiceConfig{
		Authorizer: &fakeAuthorizer{},
		Authority:  &fakeAuthority{},
		Principals: &fakePrincipals{},
		Queries:    &fakeQueryPort{},
	})
	requireNoError(t, err, "NewService complete")
	if service == nil {
		t.Fatal("NewService returned nil for a complete config")
	}
}

// TestQueryPassesScopedQueryThrough proves the service authorizes task.inbox,
// re-reads the two current checks and passes one exact scoped query to the
// port without changing the authoritative page.
func TestQueryPassesScopedQueryThrough(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
	command := queryCommand(actor)
	command.Kinds = []inbox.Kind{inbox.KindRequest, inbox.KindWaitingTask}
	command.Cursor = "cursor-1"
	command.Limit = 25

	page := inbox.Page{
		Items: []inbox.Item{
			{
				Kind:        inbox.KindRequest,
				TaskID:      id.TaskID("task-1"),
				ExecutionID: id.ExecutionID("exec-1"),
				ActionID:    id.ActionID("action-1"),
				Assignment:  roleAssignment(t, tenancy.RoleEngineer),
				Version:     4,
				At:          testNow,
			},
			{
				Kind:       inbox.KindWaitingTask,
				TaskID:     id.TaskID("task-2"),
				Assignment: serviceAssignment(t, "service-a"),
				Version:    2,
				At:         testNow.Add(time.Minute),
			},
		},
		NextCursor: "cursor-2",
	}
	env.port.query = func(inbox.Query) (inbox.Page, error) {
		return page, nil
	}

	got, err := env.service.Query(t.Context(), command)
	requireNoError(t, err, "Query")
	if !reflect.DeepEqual(got, page) {
		t.Fatalf("page = %+v; want the authoritative page unchanged %+v", got, page)
	}

	calls := env.authorizer.captured()
	if len(calls) != 1 {
		t.Fatalf("authorizer calls = %d; want exactly one", len(calls))
	}
	if calls[0].Operation != authorize.OperationName("task.inbox") {
		t.Fatalf("authorized operation = %q; want task.inbox", calls[0].Operation)
	}
	if calls[0].Scope != testScope() {
		t.Fatalf("authorized scope = %+v; want %+v", calls[0].Scope, testScope())
	}
	if calls[0].Principal.ID != actor.ID {
		t.Fatalf("authorized principal = %q; want %q", calls[0].Principal.ID, actor.ID)
	}

	reads := env.authority.lastReads()
	wantChecks := []authorize.AuthorityCheck{authorize.CheckMembership, authorize.CheckPrincipalEnabled}
	if len(reads) != len(wantChecks) {
		t.Fatalf("authority reads = %+v; want %v", reads, wantChecks)
	}
	for index, check := range wantChecks {
		read := reads[index]
		if read.Check != check {
			t.Fatalf("authority read %d check = %q; want %q", index, read.Check, check)
		}
		if read.Operation != authorize.OperationName("task.inbox") {
			t.Fatalf("authority read %d operation = %q; want task.inbox", index, read.Operation)
		}
		if read.Scope != testScope() {
			t.Fatalf("authority read %d scope = %+v; want %+v", index, read.Scope, testScope())
		}
		if read.PrincipalID != actor.ID {
			t.Fatalf("authority read %d principal = %q; want %q", index, read.PrincipalID, actor.ID)
		}
		if read.TargetID != "" || read.ExpectedVersion != 0 {
			t.Fatalf("authority read %d = %+v; want no target or version", index, read)
		}
	}

	queries := env.port.captured()
	if len(queries) != 1 {
		t.Fatalf("port queries = %d; want exactly one", len(queries))
	}
	query := queries[0]
	if query.Scope != testScope() {
		t.Fatalf("query scope = %+v; want %+v", query.Scope, testScope())
	}
	if !reflect.DeepEqual(query.Principal, actor) {
		t.Fatalf("query principal = %+v; want the verified actor %+v", query.Principal, actor)
	}
	if !reflect.DeepEqual(query.Kinds, command.Kinds) {
		t.Fatalf("query kinds = %v; want %v", query.Kinds, command.Kinds)
	}
	if query.Cursor != "cursor-1" {
		t.Fatalf("query cursor = %q; want %q", query.Cursor, "cursor-1")
	}
	if query.Limit != 25 {
		t.Fatalf("query limit = %d; want 25", query.Limit)
	}

	// The service owns a copy of the kinds; a caller mutating its slice after
	// the call cannot change the query already handed to the adapter.
	command.Kinds[0] = inbox.KindFailedTask
	if queries[0].Kinds[0] != inbox.KindRequest {
		t.Fatal("service aliased the caller's kinds slice")
	}
}

// TestQueryRejectsInvalidInput proves validation happens before the authorizer
// and the port, and that an empty kind list is passed as an empty list.
func TestQueryRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	valid := queryCommand(humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer))

	invalidID := valid.Actor
	invalidID.ID = id.PrincipalID("")
	invalidKind := valid.Actor
	invalidKind.Kind = identity.PrincipalKind("robot")

	cases := map[string]inbox.QueryCommand{
		"zero scope": func() inbox.QueryCommand {
			command := valid
			command.Scope = tenancy.Scope{}
			return command
		}(),
		"tenant scope without project": func() inbox.QueryCommand {
			command := valid
			command.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-a")}
			return command
		}(),
		"invalid principal id": func() inbox.QueryCommand {
			command := valid
			command.Actor = invalidID
			return command
		}(),
		"invalid principal kind": func() inbox.QueryCommand {
			command := valid
			command.Actor = invalidKind
			return command
		}(),
		"unknown kind": func() inbox.QueryCommand {
			command := valid
			command.Kinds = []inbox.Kind{inbox.KindRequest, inbox.Kind("bogus")}
			return command
		}(),
		"negative limit": func() inbox.QueryCommand {
			command := valid
			command.Limit = -1
			return command
		}(),
		"limit over maximum": func() inbox.QueryCommand {
			command := valid
			command.Limit = inbox.MaxPageSize + 1
			return command
		}(),
	}
	for name, command := range cases {
		command := command
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			_, err := env.service.Query(t.Context(), command)
			requireClass(t, err, fault.InvalidInput)
			if env.authorizer.callCount() != 0 {
				t.Fatal("authorizer was called for an invalid command")
			}
			if env.port.callCount() != 0 {
				t.Fatal("port was called for an invalid command")
			}
		})
	}

	t.Run("empty kinds pass through as empty", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		command := valid
		command.Kinds = nil
		_, err := env.service.Query(t.Context(), command)
		requireNoError(t, err, "Query")
		queries := env.port.captured()
		if len(queries) != 1 || len(queries[0].Kinds) != 0 {
			t.Fatalf("query kinds = %v; want empty", queries)
		}
	})

	t.Run("maximum limit accepted", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		command := valid
		command.Limit = inbox.MaxPageSize
		_, err := env.service.Query(t.Context(), command)
		requireNoError(t, err, "Query")
		queries := env.port.captured()
		if len(queries) != 1 || queries[0].Limit != inbox.MaxPageSize {
			t.Fatalf("query limit = %+v; want %d", queries, inbox.MaxPageSize)
		}
	})
}

// TestQueryRefusesCrossScopeAndDeniedActor proves the query is scoped to the
// verified principal and refused before any read when the actor is foreign,
// denied, or an execution principal with no inbox grant.
func TestQueryRefusesCrossScopeAndDeniedActor(t *testing.T) {
	t.Parallel()

	t.Run("actor scope mismatch", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
		actor.Scope = otherScope()
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.authorizer.callCount() != 0 {
			t.Fatal("authorizer was called for a cross-scope command")
		}
		if env.port.callCount() != 0 {
			t.Fatal("port was called for a cross-scope command")
		}
	})

	t.Run("authorizer denies", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		env.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
			return authorize.DecisionDeny, nil
		}
		actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 0 {
			t.Fatal("port was called for a denied command")
		}
	})

	t.Run("execution principal has no inbox grant", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		actor := executionActor(id.PrincipalID("principal-exec"), id.ExecutionID("exec-1"))
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 0 {
			t.Fatal("port was called for an execution principal")
		}
	})
}

// TestQueryReReadsCurrentRoleAndEnablement proves a role removed or a
// principal disabled after the grant check refuses the query and that a
// previous allow is never cached across calls.
func TestQueryReReadsCurrentRoleAndEnablement(t *testing.T) {
	t.Parallel()

	t.Run("membership removed", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			return allowFacts(reads, func(read authorize.AuthorityRead) bool {
				return read.Check == authorize.CheckMembership
			}), nil
		}
		actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		reads := env.authority.lastReads()
		if len(reads) != 2 {
			t.Fatalf("authority reads = %+v; want both current checks", reads)
		}
		if env.port.callCount() != 0 {
			t.Fatal("port was called after membership removal")
		}
	})

	t.Run("principal disabled", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			return allowFacts(reads, func(read authorize.AuthorityRead) bool {
				return read.Check == authorize.CheckPrincipalEnabled
			}), nil
		}
		actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 0 {
			t.Fatal("port was called for a disabled principal")
		}
	})

	t.Run("role removal after a successful page", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		removed := false
		env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			return allowFacts(reads, func(read authorize.AuthorityRead) bool {
				return removed && read.Check == authorize.CheckMembership
			}), nil
		}
		actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
		command := queryCommand(actor)

		_, err := env.service.Query(t.Context(), command)
		requireNoError(t, err, "first Query")
		if env.port.callCount() != 1 {
			t.Fatalf("port calls = %d; want 1", env.port.callCount())
		}

		removed = true
		_, err = env.service.Query(t.Context(), command)
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 1 {
			t.Fatal("port was called again after the role was removed")
		}
	})
}

// TestQueryServiceSeesOnlyExactAssignment proves the service identity is
// passed through unchanged so the adapter can apply the exact service id: a
// service sees only its own explicitly service-assigned item, a human sees
// only role-assigned items, and a different service sees nothing.
func TestQueryServiceSeesOnlyExactAssignment(t *testing.T) {
	t.Parallel()

	roleItem := inbox.Item{
		Kind:       inbox.KindWaitingTask,
		TaskID:     id.TaskID("task-role"),
		Assignment: roleAssignment(t, tenancy.RoleReviewer),
		Version:    3,
		At:         testNow,
	}
	serviceItem := inbox.Item{
		Kind:       inbox.KindWaitingTask,
		TaskID:     id.TaskID("task-service-a"),
		Assignment: serviceAssignment(t, "service-a"),
		Version:    4,
		At:         testNow.Add(time.Minute),
	}
	authoritative := []inbox.Item{roleItem, serviceItem}

	env := newTestEnv(t)
	env.port.query = func(query inbox.Query) (inbox.Page, error) {
		page := inbox.Page{}
		for _, item := range authoritative {
			switch item.Assignment.Kind {
			case request.AssignmentService:
				if query.Principal.Kind == identity.KindService &&
					string(item.Assignment.ServiceID) == string(query.Principal.ID) {
					page.Items = append(page.Items, item)
				}
			case request.AssignmentRole:
				if query.Principal.Kind == identity.KindHuman && query.Principal.HasRole(item.Assignment.Role) {
					page.Items = append(page.Items, item)
				}
			}
		}
		return page, nil
	}

	t.Run("explicit service assignment", func(t *testing.T) {
		actor := serviceActor(id.PrincipalID("service-a"), tenancy.RoleReviewer)
		got, err := env.service.Query(t.Context(), queryCommand(actor))
		requireNoError(t, err, "Query service")
		if len(got.Items) != 1 || got.Items[0].TaskID != serviceItem.TaskID {
			t.Fatalf("service page = %+v; want only %q", got.Items, serviceItem.TaskID)
		}
		if got.Items[0].Assignment.Kind != request.AssignmentService ||
			string(got.Items[0].Assignment.ServiceID) != "service-a" {
			t.Fatalf("service item assignment = %+v; want service/service-a", got.Items[0].Assignment)
		}
		queries := env.port.captured()
		last := queries[len(queries)-1]
		if last.Principal.Kind != identity.KindService || last.Principal.ID != actor.ID {
			t.Fatalf("query principal = %+v; want the exact service principal", last.Principal)
		}
		if !reflect.DeepEqual(last.Principal.Roles, actor.Roles) {
			t.Fatalf("query principal roles = %v; want %v", last.Principal.Roles, actor.Roles)
		}
	})

	t.Run("different service sees nothing", func(t *testing.T) {
		actor := serviceActor(id.PrincipalID("service-b"), tenancy.RoleReviewer)
		got, err := env.service.Query(t.Context(), queryCommand(actor))
		requireNoError(t, err, "Query other service")
		if len(got.Items) != 0 {
			t.Fatalf("service-b page = %+v; want no explicitly assigned item", got.Items)
		}
	})

	t.Run("human role assignment", func(t *testing.T) {
		actor := humanActor(id.PrincipalID("principal-reviewer"), tenancy.RoleReviewer)
		got, err := env.service.Query(t.Context(), queryCommand(actor))
		requireNoError(t, err, "Query human")
		if len(got.Items) != 1 || got.Items[0].TaskID != roleItem.TaskID {
			t.Fatalf("human page = %+v; want only %q", got.Items, roleItem.TaskID)
		}
		queries := env.port.captured()
		last := queries[len(queries)-1]
		if last.Principal.Kind != identity.KindHuman {
			t.Fatalf("query principal kind = %q; want human", last.Principal.Kind)
		}
	})
}

// TestQueryListsOperatorFailures proves a failed open task is listed for an
// operator through the failed_task kind with its authoritative version, while
// the service itself invents no item and leaves role filtering to the adapter.
func TestQueryListsOperatorFailures(t *testing.T) {
	t.Parallel()

	failed := inbox.Item{
		Kind:       inbox.KindFailedTask,
		TaskID:     id.TaskID("task-failed"),
		Assignment: roleAssignment(t, tenancy.RoleOperator),
		Version:    7,
		At:         testNow,
	}
	env := newTestEnv(t)
	env.port.query = func(query inbox.Query) (inbox.Page, error) {
		if query.Principal.HasRole(tenancy.RoleOperator) {
			return inbox.Page{Items: []inbox.Item{failed}}, nil
		}
		return inbox.Page{}, nil
	}

	operator := humanActor(id.PrincipalID("principal-operator"), tenancy.RoleOperator)
	command := queryCommand(operator)
	command.Kinds = []inbox.Kind{inbox.KindFailedTask}
	got, err := env.service.Query(t.Context(), command)
	requireNoError(t, err, "Query operator")
	if len(got.Items) != 1 {
		t.Fatalf("operator page = %+v; want the failed task", got.Items)
	}
	item := got.Items[0]
	if item.Kind != inbox.KindFailedTask || item.TaskID != failed.TaskID {
		t.Fatalf("item = %+v; want failed_task %q", item, failed.TaskID)
	}
	if item.Version != failed.Version || !item.At.Equal(failed.At) {
		t.Fatalf("item version/time = %d/%v; want %d/%v", item.Version, item.At, failed.Version, failed.At)
	}
	queries := env.port.captured()
	if len(queries) != 1 || !reflect.DeepEqual(queries[0].Kinds, command.Kinds) {
		t.Fatalf("query kinds = %+v; want %v", queries, command.Kinds)
	}

	viewer := humanActor(id.PrincipalID("principal-viewer"), tenancy.RoleViewer)
	viewerCommand := queryCommand(viewer)
	viewerCommand.Kinds = []inbox.Kind{inbox.KindFailedTask}
	viewerPage, err := env.service.Query(t.Context(), viewerCommand)
	requireNoError(t, err, "Query viewer")
	if len(viewerPage.Items) != 0 {
		t.Fatalf("viewer page = %+v; want no operator failures", viewerPage.Items)
	}
}

// TestQueryKeepsOpaqueCursorStable proves the service passes the cursor
// through byte-identically, returns NextCursor unchanged and never reorders or
// truncates a page.
func TestQueryKeepsOpaqueCursorStable(t *testing.T) {
	t.Parallel()

	opaque := "v1:tenant-a/project-a:+/=:waiting_task:task-1"
	first := inbox.Page{
		Items: []inbox.Item{
			{Kind: inbox.KindWaitingTask, TaskID: id.TaskID("task-1"), Version: 1, At: testNow},
			{Kind: inbox.KindRequest, TaskID: id.TaskID("task-2"), Version: 1, At: testNow.Add(time.Second)},
		},
		NextCursor: opaque,
	}
	second := inbox.Page{
		Items:      []inbox.Item{{Kind: inbox.KindFailedTask, TaskID: id.TaskID("task-3"), Version: 2, At: testNow}},
		NextCursor: "",
	}

	env := newTestEnv(t)
	pages := []inbox.Page{first, second}
	env.port.query = func(query inbox.Query) (inbox.Page, error) {
		if query.Cursor == "" {
			return pages[0], nil
		}
		return pages[1], nil
	}

	actor := humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer)
	command := queryCommand(actor)
	command.Limit = 0

	page, err := env.service.Query(t.Context(), command)
	requireNoError(t, err, "first page")
	if !reflect.DeepEqual(page, first) {
		t.Fatalf("first page = %+v; want %+v", page, first)
	}
	if page.NextCursor != opaque {
		t.Fatalf("next cursor = %q; want the opaque cursor unchanged %q", page.NextCursor, opaque)
	}

	command.Cursor = page.NextCursor
	page, err = env.service.Query(t.Context(), command)
	requireNoError(t, err, "second page")
	if !reflect.DeepEqual(page, second) {
		t.Fatalf("second page = %+v; want %+v", page, second)
	}
	if page.NextCursor != "" {
		t.Fatalf("final next cursor = %q; want empty", page.NextCursor)
	}

	queries := env.port.captured()
	if len(queries) != 2 {
		t.Fatalf("port queries = %d; want 2", len(queries))
	}
	if queries[0].Cursor != "" {
		t.Fatalf("first query cursor = %q; want empty", queries[0].Cursor)
	}
	if queries[1].Cursor != opaque {
		t.Fatalf("second query cursor = %q; want %q verbatim", queries[1].Cursor, opaque)
	}
	if queries[0].Limit != 0 || queries[1].Limit != 0 {
		t.Fatalf("query limits = %d/%d; want 0 passthrough", queries[0].Limit, queries[1].Limit)
	}
}

// TestQueryRecoversWithoutEventDelivery proves a failed read is not cached and
// that authoritative state changes are observed on the next query without any
// event, receipt or post-commit delivery.
func TestQueryRecoversWithoutEventDelivery(t *testing.T) {
	t.Parallel()

	item := inbox.Item{
		Kind:       inbox.KindRequest,
		TaskID:     id.TaskID("task-1"),
		ActionID:   id.ActionID("action-1"),
		Assignment: roleAssignment(t, tenancy.RoleOperator),
		Version:    2,
		At:         testNow,
	}

	env := newTestEnv(t)
	fail := true
	resolved := false
	env.port.query = func(inbox.Query) (inbox.Page, error) {
		if fail {
			return inbox.Page{}, fault.New(fault.DependencyUnavailable, "inbox index unavailable")
		}
		if resolved {
			return inbox.Page{}, nil
		}
		return inbox.Page{Items: []inbox.Item{item}}, nil
	}

	actor := humanActor(id.PrincipalID("principal-operator"), tenancy.RoleOperator)
	command := queryCommand(actor)

	_, err := env.service.Query(t.Context(), command)
	requireClass(t, err, fault.DependencyUnavailable)

	fail = false
	page, err := env.service.Query(t.Context(), command)
	requireNoError(t, err, "recovered Query")
	if len(page.Items) != 1 || page.Items[0].TaskID != item.TaskID {
		t.Fatalf("recovered page = %+v; want %q", page.Items, item.TaskID)
	}

	// The underlying request is resolved with no event delivered to the inbox:
	// the next authoritative read simply no longer returns it.
	resolved = true
	page, err = env.service.Query(t.Context(), command)
	requireNoError(t, err, "resolved Query")
	if len(page.Items) != 0 {
		t.Fatalf("page after resolution = %+v; want empty", page.Items)
	}
	if env.port.callCount() != 3 {
		t.Fatalf("port calls = %d; want one per query (no caching)", env.port.callCount())
	}

	// The service has no publication seam to deliver or replay an event.
	for index := 0; index < reflect.TypeOf(inbox.ServiceConfig{}).NumField(); index++ {
		name := strings.ToLower(reflect.TypeOf(inbox.ServiceConfig{}).Field(index).Name)
		for _, marker := range []string{"event", "publish", "postcommit", "broker", "receipt", "outbox"} {
			if strings.Contains(name, marker) {
				t.Fatalf("ServiceConfig field %q exposes a delivery seam", name)
			}
		}
	}
}

// TestNoClaimAcknowledgeOrMutationSeam proves the inbox service is a pure
// query: only Query is exposed, and no command/query/page/item value carries a
// claim, acknowledgement, state or persistence field.
func TestNoClaimAcknowledgeOrMutationSeam(t *testing.T) {
	t.Parallel()

	assertMethodNames(t, reflect.TypeOf((*inbox.Service)(nil)), []string{"Query"})
	assertMethodNames(t, reflect.TypeOf((*inbox.QueryPort)(nil)).Elem(), []string{"Query"})

	assertFieldNames(t, reflect.TypeOf(inbox.ServiceConfig{}), []string{"Authorizer", "Authority", "Principals", "Queries"})
	assertFieldNames(t, reflect.TypeOf(inbox.QueryCommand{}), []string{"Actor", "Scope", "Kinds", "Cursor", "Limit"})
	assertFieldNames(t, reflect.TypeOf(inbox.Query{}), []string{"Scope", "Principal", "Kinds", "Cursor", "Limit"})
	assertFieldNames(t, reflect.TypeOf(inbox.Page{}), []string{"Items", "NextCursor"})
	assertFieldNames(t, reflect.TypeOf(inbox.Item{}), []string{"Kind", "TaskID", "ExecutionID", "ActionID", "Assignment", "Subject", "Outputs", "QuestionID", "Question", "References", "Version", "LifecycleVersion", "At"})

	markers := []string{"claim", "ack", "state", "status", "seen", "persist", "save", "update", "delete", "resolve", "mutate"}
	types := []reflect.Type{
		reflect.TypeOf(inbox.ServiceConfig{}),
		reflect.TypeOf(inbox.QueryCommand{}),
		reflect.TypeOf(inbox.Query{}),
		reflect.TypeOf(inbox.Page{}),
		reflect.TypeOf(inbox.Item{}),
	}
	for _, typ := range types {
		for index := 0; index < typ.NumField(); index++ {
			name := strings.ToLower(typ.Field(index).Name)
			for _, marker := range markers {
				if strings.Contains(name, marker) {
					t.Fatalf("%s.%s exposes the mutation marker %q", typ.Name(), typ.Field(index).Name, marker)
				}
			}
		}
	}
}

func assertMethodNames(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	got := make([]string, 0, typ.NumMethod())
	for index := 0; index < typ.NumMethod(); index++ {
		got = append(got, typ.Method(index).Name)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("%s methods = %v, want exactly %v", typ.Name(), got, sorted)
	}
}

func assertFieldNames(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	got := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		got = append(got, typ.Field(index).Name)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("%s fields = %v, want exactly %v", typ.Name(), got, sorted)
	}
}

// TestInboxPageBoundMatchesThePublicContract proves the inbox service enforces
// the declared public page bound (api.MaxPageLimit = 200), not a smaller
// package-local one. The handler must not be able to ask for less than the
// contract allows.
func TestInboxPageBoundMatchesThePublicContract(t *testing.T) {
	if inbox.MaxPageSize != api.MaxPageLimit {
		t.Fatalf("inbox.MaxPageSize = %d; want the public api.MaxPageLimit %d", inbox.MaxPageSize, api.MaxPageLimit)
	}
}

// TestQueryServesEveryAcceptedPageLimit proves the service forwards every limit
// the public page contract accepts (100, 101 and 200) unchanged, refuses 201
// before the authorizer or port is called, and treats 0 as the adapter default.
func TestQueryServesEveryAcceptedPageLimit(t *testing.T) {
	valid := inbox.QueryCommand{
		Actor: humanActor(id.PrincipalID("principal-engineer"), tenancy.RoleEngineer),
		Scope: testScope(),
	}

	for _, limit := range []int{100, 101, api.MaxPageLimit} {
		env := newTestEnv(t)
		command := valid
		command.Limit = limit
		page, err := env.service.Query(t.Context(), command)
		requireNoError(t, err, "Query")
		if len(page.Items) != 0 {
			t.Fatalf("limit %d page = %+v; want the port's empty page unchanged", limit, page)
		}
		queries := env.port.captured()
		if len(queries) != 1 || queries[0].Limit != limit {
			t.Fatalf("limit %d forwarded %+v; want the requested limit verbatim", limit, queries)
		}
	}

	for _, limit := range []int{api.MaxPageLimit + 1, 201} {
		env := newTestEnv(t)
		command := valid
		command.Limit = limit
		_, err := env.service.Query(t.Context(), command)
		requireClass(t, err, fault.InvalidInput)
		if env.authorizer.callCount() != 0 || env.port.callCount() != 0 {
			t.Fatalf("limit %d was refused after an authorizer or port call", limit)
		}
	}

	env := newTestEnv(t)
	command := valid
	command.Limit = 0
	_, err := env.service.Query(t.Context(), command)
	requireNoError(t, err, "Query")
	queries := env.port.captured()
	if len(queries) != 1 || queries[0].Limit != 0 {
		t.Fatalf("zero limit forwarded %+v; want 0 so the adapter applies its default", queries)
	}
}

// TestQueryReReadsEffectiveRolesAndAllowsTenantScopedActor proves that
// inbox.Service re-reads current effective roles via Principals.ReadPrincipal
// so a demoted/revoked reviewer role no longer receives role/reviewer items,
// and a tenant-scoped human actor can query a project inbox in its own tenant.
func TestQueryReReadsEffectiveRolesAndAllowsTenantScopedActor(t *testing.T) {
	t.Parallel()

	reviewerItem := inbox.Item{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("exec-review"),
		ActionID:    id.ActionID("action-review"),
		Assignment:  roleAssignment(t, tenancy.RoleReviewer),
		Version:     1,
		At:          testNow,
	}

	t.Run("demoted reviewer no longer receives role/reviewer items", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		currentRoles := []tenancy.Role{tenancy.RoleReviewer, tenancy.RoleViewer}
		env.principals.read = func(_ context.Context, req identity.PrincipalRequest) (identity.Principal, error) {
			return identity.Principal{
				ID:      req.PrincipalID,
				Kind:    identity.KindHuman,
				Scope:   req.Scope,
				Roles:   append([]tenancy.Role(nil), currentRoles...),
				Enabled: true,
				Version: 2,
			}, nil
		}
		env.port.query = func(query inbox.Query) (inbox.Page, error) {
			if query.Principal.HasRole(tenancy.RoleReviewer) {
				return inbox.Page{Items: []inbox.Item{reviewerItem}}, nil
			}
			return inbox.Page{}, nil
		}

		// Stale authentication snapshot still carries Reviewer + Viewer.
		staleActor := humanActor(id.PrincipalID("principal-demoted"), tenancy.RoleReviewer, tenancy.RoleViewer)
		before, err := env.service.Query(t.Context(), queryCommand(staleActor))
		requireNoError(t, err, "Query before demotion")
		if len(before.Items) != 1 {
			t.Fatalf("items before demotion = %+v; want 1", before.Items)
		}

		// Demote Reviewer -> Viewer only: CheckMembership still allows task.inbox
		// via Viewer, and QueryPort must receive the fresh [Viewer] role set.
		currentRoles = []tenancy.Role{tenancy.RoleViewer}
		after, err := env.service.Query(t.Context(), queryCommand(staleActor))
		requireNoError(t, err, "Query after demotion")
		if len(after.Items) != 0 {
			t.Fatalf("demoted actor still received role/reviewer items: %+v", after.Items)
		}
		captured := env.port.captured()
		last := captured[len(captured)-1]
		if !reflect.DeepEqual(last.Principal.Roles, []tenancy.Role{tenancy.RoleViewer}) {
			t.Fatalf("forwarded roles = %v; want [viewer]", last.Principal.Roles)
		}
	})

	t.Run("tenant-scoped actor in same tenant succeeds while foreign project is refused", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		tenantActor := authorize.Principal{
			ID:      id.PrincipalID("principal-tenant-reviewer"),
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: id.TenantID("tenant-a")},
			Roles:   []tenancy.Role{tenancy.RoleReviewer},
			Enabled: true,
			Version: 1,
		}
		_, err := env.service.Query(t.Context(), queryCommand(tenantActor))
		requireNoError(t, err, "tenant-scoped actor Query")
		captured := env.port.captured()
		if len(captured) != 1 || captured[0].Principal.Scope != testScope() {
			t.Fatalf("captured query = %+v; want project-scoped refreshed principal", captured)
		}

		foreignProjectActor := humanActor(id.PrincipalID("principal-other-project"), tenancy.RoleReviewer)
		foreignProjectActor.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-b")}
		_, err = env.service.Query(t.Context(), queryCommand(foreignProjectActor))
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 1 {
			t.Fatal("foreign project actor reached QueryPort")
		}
	})

	t.Run("missing current principal maps to forbidden", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t)
		env.principals.read = func(context.Context, identity.PrincipalRequest) (identity.Principal, error) {
			return identity.Principal{}, fault.New(fault.NotFound, "no principal")
		}
		actor := humanActor(id.PrincipalID("principal-gone"), tenancy.RoleEngineer)
		_, err := env.service.Query(t.Context(), queryCommand(actor))
		requireClass(t, err, fault.Forbidden)
		if env.port.callCount() != 0 {
			t.Fatal("missing current principal reached QueryPort")
		}
	})
}
