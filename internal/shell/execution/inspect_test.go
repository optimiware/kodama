package execution_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func pendingCleanupView(scope tenancy.Scope) execution.View {
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	return execution.View{
		Owner:         scope,
		ExecutionID:   id.ExecutionID("exec-1"),
		TaskID:        id.TaskID("task-1"),
		PipelineID:    "pipeline-1",
		State:         kernel.ExecutionFailed,
		Version:       7,
		CurrentAction: -1,
		WorkspaceID:   id.WorkspaceID("ws-1"),
		Cleanup: kernel.Cleanup{
			State:       kernel.CleanupPending,
			WorkloadID:  id.WorkloadID("workload-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
			Attempts:    2,
			LastError:   "remove failed",
		},
		Actions: []execution.ActionView{
			{
				ID:               id.ActionID("act-0"),
				Index:            0,
				Name:             id.ActionName("build"),
				Kind:             definition.ActionKindContainer,
				State:            kernel.ActionSucceeded,
				Version:          3,
				Timeout:          time.Hour,
				ActivatedAt:      testNow,
				Deadline:         testNow.Add(time.Hour),
				Outputs:          kernel.Outputs{"candidate": "abc"},
				StagedOutputs:    kernel.Outputs{},
				StagedOutputsSet: true,
				Stop:             kernel.PendingStop("stop acknowledgement is not confirmation"),
			},
			{
				ID:       id.ActionID("act-1"),
				Index:    1,
				Name:     id.ActionName("review"),
				Kind:     definition.ActionKindRequest,
				State:    kernel.ActionCancelled,
				Version:  2,
				Deadline: testNow.Add(2 * time.Hour),
				Subject:  &subject,
			},
		},
	}
}

func TestInspectReturnsScopedViewWithPendingCleanup(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	want := pendingCleanupView(scope)
	fixture.reader.inspectFn = func(_ context.Context, gotScope tenancy.Scope, executionID id.ExecutionID) (execution.View, error) {
		if gotScope != scope {
			t.Fatalf("reader scope = %+v; want %+v", gotScope, scope)
		}
		if executionID != want.ExecutionID {
			t.Fatalf("reader execution = %q; want %q", executionID, want.ExecutionID)
		}
		return want, nil
	}

	view, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer),
		Scope:       scope,
		ExecutionID: want.ExecutionID,
	})
	requireNoError(t, err, "Inspect")
	if view.Cleanup.State != kernel.CleanupPending || view.Cleanup.Attempts != 2 || view.Cleanup.LastError == "" {
		t.Fatalf("inspection lost pending cleanup evidence: %+v", view.Cleanup)
	}
	if len(view.Actions) != 2 {
		t.Fatalf("inspection actions = %d; want 2", len(view.Actions))
	}
	if view.Actions[0].Stop.IsConfirmed() {
		t.Fatal("inspection treated a pending stop acknowledgement as confirmed stop")
	}
	if view.Actions[1].Subject == nil || view.Actions[1].Subject.Len() == 0 {
		t.Fatal("inspection lost the immutable request subject")
	}
	if view.Owner != scope {
		t.Fatalf("view owner = %+v; want %+v", view.Owner, scope)
	}
}

func TestInspectRefusesForeignOwner(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	foreign := pendingCleanupView(projectScope("tenant-2", "project-9"))
	fixture.reader.inspectFn = func(context.Context, tenancy.Scope, id.ExecutionID) (execution.View, error) {
		return foreign, nil
	}
	_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer),
		Scope:       scope,
		ExecutionID: foreign.ExecutionID,
	})
	requireFaultClass(t, err, fault.NotFound)
}

func TestInspectRefusesUnauthorizedWithoutRead(t *testing.T) {
	fixture := newTestService(t)
	fixture.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}
	_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       humanActor("tenant-1", "project-1", "principal-1"),
		Scope:       projectScope("tenant-1", "project-1"),
		ExecutionID: id.ExecutionID("exec-1"),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.reader.inspectCount() != 0 {
		t.Fatalf("reader calls = %d; want zero after authorization refusal", fixture.reader.inspectCount())
	}
}

func TestInspectRefusesWhenAuthorityDeniesCurrentMembership(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer)
	fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := allowedFacts(reads)
		for index, read := range reads {
			if read.Check == authorize.CheckMembership {
				facts[index].Allowed = false
			}
		}
		return facts, nil
	}

	_, err := fixture.service.Inspect(context.Background(), execution.InspectCommand{
		Actor:       actor,
		Scope:       scope,
		ExecutionID: id.ExecutionID("exec-1"),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.authority.callCount() != 1 {
		t.Fatalf("authority calls = %d; want 1", fixture.authority.callCount())
	}
	read := requireAuthority(t, fixture.authority.captured()[0], "execution.inspect", authorize.CheckMembership)
	if read.Scope != scope || read.PrincipalID != actor.ID {
		t.Fatalf("authority read scope/principal = %+v/%q; want %+v/%q", read.Scope, read.PrincipalID, scope, actor.ID)
	}
	if fixture.reader.inspectCount() != 0 {
		t.Fatalf("reader calls = %d; want zero after current authority refusal", fixture.reader.inspectCount())
	}
}

func TestListPassesFilterAndReturnsPage(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	filter := execution.ListFilter{
		TaskID:     id.TaskID("task-1"),
		PipelineID: "pipeline-1",
		State:      kernel.ExecutionRunning,
		Limit:      25,
		Cursor:     "cursor-1",
	}
	want := execution.Page{Items: []execution.View{pendingCleanupView(scope)}, Next: "cursor-2"}
	fixture.reader.listFn = func(_ context.Context, gotScope tenancy.Scope, gotFilter execution.ListFilter) (execution.Page, error) {
		if gotScope != scope {
			t.Fatalf("list scope = %+v; want %+v", gotScope, scope)
		}
		if !reflect.DeepEqual(gotFilter, filter) {
			t.Fatalf("list filter = %+v; want %+v", gotFilter, filter)
		}
		return want, nil
	}
	page, err := fixture.service.List(context.Background(), execution.ListCommand{
		Actor:  humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer),
		Scope:  scope,
		Filter: filter,
	})
	requireNoError(t, err, "List")
	if page.Next != want.Next || len(page.Items) != 1 {
		t.Fatalf("page = %+v; want %+v", page, want)
	}
}

func TestListRefusesForeignScopeItem(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	foreign := pendingCleanupView(projectScope("tenant-2", "project-9"))
	fixture.reader.listFn = func(context.Context, tenancy.Scope, execution.ListFilter) (execution.Page, error) {
		return execution.Page{Items: []execution.View{foreign}}, nil
	}
	_, err := fixture.service.List(context.Background(), execution.ListCommand{
		Actor: humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer),
		Scope: scope,
	})
	requireFaultClass(t, err, fault.NotFound)
}

func TestListRefusesWhenAuthorityDeniesCurrentMembership(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer)
	fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := allowedFacts(reads)
		for index, read := range reads {
			if read.Check == authorize.CheckMembership {
				facts[index].Allowed = false
			}
		}
		return facts, nil
	}

	_, err := fixture.service.List(context.Background(), execution.ListCommand{
		Actor: actor,
		Scope: scope,
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.authority.callCount() != 1 {
		t.Fatalf("authority calls = %d; want 1", fixture.authority.callCount())
	}
	read := requireAuthority(t, fixture.authority.captured()[0], "execution.list", authorize.CheckMembership)
	if read.Scope != scope || read.PrincipalID != actor.ID {
		t.Fatalf("authority read scope/principal = %+v/%q; want %+v/%q", read.Scope, read.PrincipalID, scope, actor.ID)
	}
	if fixture.reader.listCount() != 0 {
		t.Fatalf("reader calls = %d; want zero after current authority refusal", fixture.reader.listCount())
	}
}

func TestInspectionViewCarriesNoCredentialMaterial(t *testing.T) {
	for _, value := range []any{execution.View{}, execution.ActionView{}} {
		typ := reflect.TypeOf(value)
		for field := range typ.Fields() {
			name := strings.ToLower(field.Name)
			for _, forbidden := range []string{"credential", "token", "nonce", "secret"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s carries forbidden field %q", typ.Name(), field.Name)
				}
			}
		}
	}

	scope := projectScope("tenant-1", "project-1")
	encoded, err := json.Marshal(pendingCleanupView(scope))
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"credential", "token", "nonce", "secret"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("inspection JSON leaks %q: %s", forbidden, encoded)
		}
	}
}
