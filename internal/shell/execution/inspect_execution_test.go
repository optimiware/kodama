package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// These tests pin the registry row for execution.inspect:
//
//	execution.inspect | execution | project | ... | own_task_content | membership | none | read
//
// A running execution authenticates as an execution principal carrying its own
// admitted execution permission. The human/service matrix refuses such a
// principal ("execution principal requires its admitted permission"), so the
// capability must decide it through authorize.AuthorizeExecution, exactly as
// execution.action.outputs and execution.logs.read already do. It may inspect
// only its own current execution; every other principal kind and every foreign
// or non-current target stays refused.

// useRegistryMatrix makes the service consult the real declared matrix for
// human/service principals. The default fake models the admitted-execution
// branch permissively, which would hide the very boundary these tests pin.
func useRegistryMatrix(fixture *testService) {
	fixture.authorizer.authorize = authorize.NewMatrix().Authorize
}

// runningAggregate is one running execution with a running container action.
func runningAggregate(executionID id.ExecutionID) kernel.Execution {
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	aggregate.ID = executionID
	return aggregate
}

// terminalAggregate is one terminal execution whose credential is no longer
// current authority.
func terminalAggregate(executionID id.ExecutionID) kernel.Execution {
	aggregate := runningAggregate(executionID)
	aggregate.State = kernel.ExecutionFailed
	aggregate.Version = 9
	aggregate.CurrentAction = -1
	aggregate.Actions[0].State = kernel.ActionFailed
	aggregate.Actions[0].Version = 5
	return aggregate
}

// runningInspectView is the exact own-execution view a running workload needs:
// the execution identity/version and the current action identity/version that
// execution.action.outputs requires.
func runningInspectView(scope tenancy.Scope, aggregate kernel.Execution) execution.View {
	return execution.View{
		Owner:         scope,
		ExecutionID:   aggregate.ID,
		PipelineID:    "pipeline-1",
		State:         aggregate.State,
		Version:       aggregate.Version,
		CurrentAction: aggregate.CurrentAction,
		WorkspaceID:   aggregate.WorkspaceID,
		Actions: []execution.ActionView{{
			ID:          aggregate.Actions[0].ID,
			Index:       aggregate.Actions[0].Index,
			Name:        aggregate.Actions[0].Name,
			Kind:        aggregate.Actions[0].Kind,
			State:       aggregate.Actions[0].State,
			Version:     aggregate.Actions[0].Version,
			Timeout:     aggregate.Actions[0].Timeout,
			ActivatedAt: aggregate.Actions[0].ActivatedAt,
			Deadline:    aggregate.Actions[0].Deadline,
			Outputs:     kernel.Outputs{"candidate": "abc"},
		}},
		Cleanup: kernel.Cleanup{State: kernel.CleanupNone},
	}
}

func disabled(actor authorize.Principal) authorize.Principal {
	actor.Enabled = false
	return actor
}

// TestInspectExecutionPrincipalReadsItsOwnCurrentExecution proves a running
// workload can learn its persisted execution/action identity and version. The
// declared matrix is injected, so a human/service-only decision path is visibly
// refused instead of being masked by the fake.
func TestInspectExecutionPrincipalReadsItsOwnCurrentExecution(t *testing.T) {
	fixture := newTestService(t)
	useRegistryMatrix(fixture)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningAggregate(id.ExecutionID("exec-1"))
	fixture.store.Seed(scope, aggregate)
	view := runningInspectView(scope, aggregate)

	var gotScope tenancy.Scope
	var gotExecution id.ExecutionID
	fixture.reader.inspectFn = func(_ context.Context, got tenancy.Scope, executionID id.ExecutionID) (execution.View, error) {
		gotScope = got
		gotExecution = executionID
		return view, nil
	}

	actor := executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID)
	actor.Version = aggregate.Version
	got, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       actor,
		Scope:       scope,
		ExecutionID: aggregate.ID,
	})
	requireNoError(t, err, "Inspect own execution")
	if gotScope != scope || gotExecution != aggregate.ID {
		t.Fatalf("reader saw scope/execution %+v/%q; want %+v/%q", gotScope, gotExecution, scope, aggregate.ID)
	}
	if fixture.authorizer.callCount() != 0 {
		t.Fatalf("human/service matrix calls = %d; an execution principal must be decided by authorize.AuthorizeExecution",
			fixture.authorizer.callCount())
	}
	if got.ExecutionID != aggregate.ID || got.Version != aggregate.Version || got.CurrentAction != aggregate.CurrentAction {
		t.Fatalf("view identity = %q version %d action %d; want %q version %d action %d",
			got.ExecutionID, got.Version, got.CurrentAction,
			aggregate.ID, aggregate.Version, aggregate.CurrentAction)
	}
	if got.State != kernel.ExecutionRunning {
		t.Fatalf("view state = %q; want running", got.State)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("view actions = %d; want the one running action", len(got.Actions))
	}
	action := got.Actions[0]
	wantAction := aggregate.Actions[0]
	if action.ID != wantAction.ID || action.Index != wantAction.Index || action.Name != wantAction.Name {
		t.Fatalf("action identity = %q/%d/%q; want %q/%d/%q",
			action.ID, action.Index, action.Name, wantAction.ID, wantAction.Index, wantAction.Name)
	}
	if action.Version != wantAction.Version || action.State != kernel.ActionRunning {
		t.Fatalf("action version/state = %d/%q; want %d/running", action.Version, action.State, wantAction.Version)
	}
	if !action.Deadline.Equal(wantAction.Deadline) {
		t.Fatalf("action deadline = %v; want %v", action.Deadline, wantAction.Deadline)
	}
	if action.Outputs["candidate"] != "abc" {
		t.Fatalf("action outputs = %+v; want the recorded candidate", action.Outputs)
	}
}

// TestInspectExecutionPrincipalRefusesForeignExecution pins that the admitted
// permission never addresses another execution.
func TestInspectExecutionPrincipalRefusesForeignExecution(t *testing.T) {
	fixture := newTestService(t)
	useRegistryMatrix(fixture)
	scope := projectScope("tenant-1", "project-1")
	target := runningAggregate(id.ExecutionID("exec-1"))
	own := runningAggregate(id.ExecutionID("exec-other"))
	fixture.store.Seed(scope, target, own)

	actor := executionActor("tenant-1", "project-1", "principal-exec", own.ID)
	actor.Version = own.Version
	_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       actor,
		Scope:       scope,
		ExecutionID: target.ID,
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.reader.inspectCount() != 0 {
		t.Fatalf("reader calls = %d; a foreign execution must be refused before any read", fixture.reader.inspectCount())
	}
}

// TestInspectExecutionPrincipalRefusesCrossScope pins that own-execution
// authority never crosses a tenant or project boundary.
func TestInspectExecutionPrincipalRefusesCrossScope(t *testing.T) {
	cases := []struct {
		name        string
		actorTenant string
		actorProj   string
		target      tenancy.Scope
	}{
		{"cross tenant", "tenant-2", "project-9", projectScope("tenant-1", "project-1")},
		{"cross project", "tenant-1", "project-2", projectScope("tenant-1", "project-1")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			useRegistryMatrix(fixture)
			aggregate := runningAggregate(id.ExecutionID("exec-1"))
			fixture.store.Seed(testCase.target, aggregate)

			actor := executionActor(testCase.actorTenant, testCase.actorProj, "principal-exec", aggregate.ID)
			actor.Version = aggregate.Version
			_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
				Actor:       actor,
				Scope:       testCase.target,
				ExecutionID: aggregate.ID,
			})
			requireFaultClass(t, err, fault.Forbidden)
			if fixture.reader.inspectCount() != 0 {
				t.Fatalf("reader calls = %d; a cross-scope execution must be refused before any read", fixture.reader.inspectCount())
			}
		})
	}
}

// TestInspectExecutionPrincipalRefusesNonCurrentAuthority pins that a terminal
// execution or an expired/revoked credential is not current authority even
// when the principal value still names the same execution. Enabled is the
// current execution-state fact (the platform reader derives it from the running
// state; authentication re-reads it), and a stale Version alone is never
// authority.
func TestInspectExecutionPrincipalRefusesNonCurrentAuthority(t *testing.T) {
	cases := []struct {
		name      string
		aggregate kernel.Execution
		version   uint64
	}{
		{"terminal execution", terminalAggregate(id.ExecutionID("exec-1")), 9},
		{"revoked credential", runningAggregate(id.ExecutionID("exec-1")), 1},
		{"expired credential with stale version", runningAggregate(id.ExecutionID("exec-1")), 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			useRegistryMatrix(fixture)
			scope := projectScope("tenant-1", "project-1")
			aggregate := testCase.aggregate
			if testCase.name == "expired credential with stale version" {
				// The execution advanced after the credential was admitted;
				// the stale principal version is not durable permission.
				aggregate.Version = 9
			}
			fixture.store.Seed(scope, aggregate)

			actor := disabled(executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID))
			actor.Version = testCase.version
			_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
				Actor:       actor,
				Scope:       scope,
				ExecutionID: aggregate.ID,
			})
			requireFaultClass(t, err, fault.Forbidden)
			if fixture.reader.inspectCount() != 0 {
				t.Fatalf("reader calls = %d; non-current authority must be refused before any read", fixture.reader.inspectCount())
			}
		})
	}
}

// TestInspectRefusesNonExecutionAndMalformedPrincipals pins that only a
// well-formed execution principal bound to its own execution reaches the
// admitted-execution branch. Every other principal kind keeps its own fault.
func TestInspectRefusesNonExecutionAndMalformedPrincipals(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	cases := []struct {
		name  string
		actor authorize.Principal
		want  fault.Class
	}{
		{
			name:  "human without inspect role",
			actor: humanActor("tenant-1", "project-1", "principal-human", tenancy.RoleReviewer),
			want:  fault.Forbidden,
		},
		{
			name:  "service without inspect role",
			actor: serviceActor("tenant-1", "project-1", "service-1", tenancy.RoleReviewer),
			want:  fault.Forbidden,
		},
		{
			name: "unknown principal kind",
			actor: authorize.Principal{
				ID:      id.PrincipalID("principal-1"),
				Kind:    identity.PrincipalKind("robot"),
				Scope:   scope,
				Enabled: true,
			},
			want: fault.InvalidInput,
		},
		{
			name: "execution principal without execution id",
			actor: authorize.Principal{
				ID:      id.PrincipalID("principal-exec"),
				Kind:    identity.KindExecution,
				Scope:   scope,
				Enabled: true,
			},
			want: fault.InvalidInput,
		},
		{
			name: "missing principal id",
			actor: authorize.Principal{
				Kind:        identity.KindExecution,
				Scope:       scope,
				Enabled:     true,
				ExecutionID: id.ExecutionID("exec-1"),
			},
			want: fault.Unauthenticated,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			useRegistryMatrix(fixture)
			_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
				Actor:       testCase.actor,
				Scope:       scope,
				ExecutionID: id.ExecutionID("exec-1"),
			})
			requireFaultClass(t, err, testCase.want)
			if fixture.reader.inspectCount() != 0 {
				t.Fatalf("reader calls = %d; a refused principal must not reach the reader", fixture.reader.inspectCount())
			}
		})
	}
}

// TestInspectHumanAndServiceBehaviorUnchanged pins the registry rows: a human
// viewer/operator and a service engineer still inspect, and a reviewer-only
// human/service or a disabled human still does not.
func TestInspectHumanAndServiceBehaviorUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		actor authorize.Principal
		allow bool
	}{
		{"human viewer", humanActor("tenant-1", "project-1", "principal-human", tenancy.RoleViewer), true},
		{"human operator", humanActor("tenant-1", "project-1", "principal-operator", tenancy.RoleOperator), true},
		{"service engineer", serviceActor("tenant-1", "project-1", "service-1", tenancy.RoleEngineer), true},
		{"human reviewer is not an inspect role", humanActor("tenant-1", "project-1", "principal-reviewer", tenancy.RoleReviewer), false},
		{"service reviewer is not an inspect role", serviceActor("tenant-1", "project-1", "service-2", tenancy.RoleReviewer), false},
		{"disabled human viewer", disabled(humanActor("tenant-1", "project-1", "principal-disabled", tenancy.RoleViewer)), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			useRegistryMatrix(fixture)
			scope := projectScope("tenant-1", "project-1")
			aggregate := runningAggregate(id.ExecutionID("exec-1"))
			fixture.store.Seed(scope, aggregate)
			view := runningInspectView(scope, aggregate)
			calls := 0
			fixture.reader.inspectFn = func(context.Context, tenancy.Scope, id.ExecutionID) (execution.View, error) {
				calls++
				return view, nil
			}

			got, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
				Actor:       testCase.actor,
				Scope:       scope,
				ExecutionID: aggregate.ID,
			})
			if !testCase.allow {
				requireFaultClass(t, err, fault.Forbidden)
				if calls != 0 {
					t.Fatalf("reader calls = %d; a denied principal must not reach the reader", calls)
				}
				return
			}
			requireNoError(t, err, "Inspect")
			if calls != 1 {
				t.Fatalf("reader calls = %d; want one", calls)
			}
			if got.ExecutionID != aggregate.ID {
				t.Fatalf("view execution = %q; want %q", got.ExecutionID, aggregate.ID)
			}
		})
	}
}

// TestInspectFixDoesNotWidenListForExecutionPrincipals pins that only
// execution.inspect gained the admitted-execution branch. execution.list
// declares no execution permission, so an execution principal stays refused
// before the reader.
func TestInspectFixDoesNotWidenListForExecutionPrincipals(t *testing.T) {
	fixture := newTestService(t)
	useRegistryMatrix(fixture)
	scope := projectScope("tenant-1", "project-1")
	actor := executionActor("tenant-1", "project-1", "principal-exec", id.ExecutionID("exec-1"))
	_, err := fixture.service.List(context.Background(), execution.ListCommand{
		Actor: actor,
		Scope: scope,
	})
	requireFaultClassIn(t, err, fault.InvalidInput, fault.Forbidden)
	if fixture.reader.listCount() != 0 {
		t.Fatalf("list reader calls = %d; a refused execution principal must not reach the reader", fixture.reader.listCount())
	}
}
