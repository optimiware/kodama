package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/workspace"
)

type fakeWorkspaceService struct {
	provisioned []workspace.RepositoryID
	syncErr     error
	gateErr     error
	version     domain.Version
	checkpoint  workspace.CheckpointInput
	acceptErr   error
	order       []string
}

func (f *fakeWorkspaceService) ProvisionInternal(_ context.Context, _ workspace.Actor, _ domain.Scope, _ domain.WorkItemID, repos []workspace.RepositoryID) (*workspace.Workspace, []*workspace.RepositoryAssociation, bool, error) {
	f.provisioned = repos
	return &workspace.Workspace{ID: "ws_one"}, nil, true, nil
}

func (f *fakeWorkspaceService) Sync(context.Context, workspace.Actor, domain.Scope, workspace.ID, domain.Version) (*workspace.Workspace, error) {
	f.order = append(f.order, "sync")
	return &workspace.Workspace{}, f.syncErr
}

func (f *fakeWorkspaceService) CheckWriterGateAt(context.Context, domain.Scope, workspace.ID) (domain.Version, error) {
	f.order = append(f.order, "gate")
	return f.version, f.gateErr
}

func (f *fakeWorkspaceService) AcceptCheckpoint(_ context.Context, a workspace.Actor, in workspace.CheckpointInput) (workspace.CheckpointResult, error) {
	f.checkpoint = in
	if a != (workspace.Actor{Type: workspace.ActorService, ID: "execution"}) {
		return workspace.CheckpointResult{}, errors.New("the execution service actor is required")
	}
	return workspace.CheckpointResult{}, f.acceptErr
}

type viewStore struct {
	workspace.Store
	assocs []*workspace.RepositoryAssociation
}

func (v viewStore) View(context.Context, domain.Scope, workspace.ID, workspace.Load) (*workspace.Aggregate, error) {
	return &workspace.Aggregate{Workspace: &workspace.Workspace{}, Associations: v.assocs}, nil
}

// Test_internal_workspaces_adapter: the adapter maps the workspace module's
// outcomes onto what the claim and the result path distinguish (T-148, WS-11,
// WS-17-WS-19): wait, repeat the observation, refuse, or retry.
func Test_internal_workspaces_adapter(t *testing.T) {
	ctx := context.Background()
	svc := &fakeWorkspaceService{version: 7}
	store := viewStore{assocs: []*workspace.RepositoryAssociation{
		{RepositoryID: "repo_a", State: workspace.AssociationCreated, AcceptedCommit: "1111111111111111111111111111111111111111"},
		{RepositoryID: "repo_b", State: workspace.AssociationIntent, AcceptedCommit: ""},
	}}
	a, err := adapters.NewInternalWorkspaces(store, svc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.NewInternalWorkspaces(nil, svc); err == nil {
		t.Fatal("a store is required")
	}

	ws, err := a.Provision(ctx, scope, "wi_1", []string{"repo_a"})
	if err != nil || ws.ID != "ws_one" || len(svc.provisioned) != 1 || svc.provisioned[0] != "repo_a" {
		t.Fatalf("%+v %v %v", ws, err, svc.provisioned)
	}

	adm, err := a.AdmitWriter(ctx, scope, "ws_one")
	if err != nil || adm.Version != 7 || len(adm.Accepted) != 1 || adm.Accepted["repo_a"] != "1111111111111111111111111111111111111111" {
		t.Fatalf("only created task branches have an accepted commit: %+v %v", adm, err)
	}
	if len(svc.order) != 2 || svc.order[0] != "sync" || svc.order[1] != "gate" {
		t.Fatalf("the branches are synchronized before the gate: %v", svc.order)
	}

	for name, tc := range map[string]struct {
		gate error
		want error
	}{
		"an unknown push outcome":     {workspace.ErrWriterBlocked, execution.ErrBusy},
		"an unsettled push":           {workspace.ErrPushPending, execution.ErrBusy},
		"another writer":              {workspace.ErrWriterBusy, execution.ErrBusy},
		"an unaccepted workload push": {workspace.ErrCheckpointRequired, execution.ErrBusy},
		"a racing change":             {workspace.ErrStale, execution.ErrStale},
		"a dependency outage":         {errors.New("down"), execution.ErrUnavailable},
	} {
		svc.gateErr = tc.gate
		if _, err := a.AdmitWriter(ctx, scope, "ws_one"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	svc.gateErr, svc.syncErr = nil, workspace.ErrDiverged
	if _, err := a.AdmitWriter(ctx, scope, "ws_one"); !errors.Is(err, execution.ErrBusy) {
		t.Errorf("a rewritten branch refuses admission: %v", err)
	}

	out := execution.CommitOutput{RepositoryID: "repo_a", Ref: "refs/heads/kodama/work/wi_1", Commit: "2222222222222222222222222222222222222222", PushID: "push_1"}
	if err := a.AcceptCheckpoint(ctx, scope, "att_1", out); err != nil {
		t.Fatal(err)
	}
	if svc.checkpoint.AttemptID != "att_1" || svc.checkpoint.PushID != "push_1" || svc.checkpoint.Commit != "2222222222222222222222222222222222222222" || svc.checkpoint.Scope != scope {
		t.Fatalf("%+v", svc.checkpoint)
	}
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"the push is not settled": {workspace.ErrPushPending, execution.ErrBusy},
		"a secret in the commit":  {workspace.ErrSecretInSource, execution.ErrAdmissionRefused},
		"no applied push":         {workspace.ErrPushNotApplied, execution.ErrAdmissionRefused},
		"another writer":          {workspace.ErrNotWriter, execution.ErrAdmissionRefused},
		"an outage":               {errors.New("down"), execution.ErrUnavailable},
	} {
		svc.acceptErr = tc.err
		if err := a.AcceptCheckpoint(ctx, scope, "att_1", out); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
