package adapters_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/workspace"
)

type fakePreparer struct {
	caller execution.Caller
	cmd    execution.AdmitCommand
	calls  int
	err    error
}

func (p *fakePreparer) Prepare(_ context.Context, c execution.Caller, cmd execution.AdmitCommand) (*execution.AdmissionPlan, error) {
	p.calls++
	p.caller, p.cmd = c, cmd
	if p.err != nil {
		return nil, p.err
	}
	return &execution.AdmissionPlan{
		Aggregate: &execution.Aggregate{Run: &execution.Run{Scope: cmd.Scope, ID: "run_p", WorkspaceID: "ws1"}},
		Source:    execution.SourceInfo{WorkspaceID: "ws1", RepositoryURL: gitwebhooktest.RepositoryURL},
	}, nil
}

// fakeWorkspaces serves workspaces by identifier.
type fakeWorkspaces struct {
	byID map[workspace.ID]*workspace.Workspace
	err  error
}

func (w fakeWorkspaces) Get(_ context.Context, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error) {
	if w.err != nil {
		return nil, w.err
	}
	ws, ok := w.byID[id]
	if !ok || ws.Scope != scope {
		return nil, workspace.ErrNotFound
	}
	return ws, nil
}

func workspaceOf(f *gitwebhooktest.Fixture, repo workspace.RepositoryID, url string) fakeWorkspaces {
	return fakeWorkspaces{byID: map[workspace.ID]*workspace.Workspace{
		"ws1": {ID: "ws1", Scope: f.Scope, WorkItemID: "wi1", RepositoryID: repo, RepositoryURL: url},
	}}
}

type fakeWork struct {
	snap execution.WorkSnapshot
	err  error
}

func (w fakeWork) Snapshot(context.Context, domain.Scope, domain.WorkItemID) (execution.WorkSnapshot, error) {
	return w.snap, w.err
}

func plannerRequest(f *gitwebhooktest.Fixture) gw.RunRequest {
	return gw.RunRequest{
		Scope: f.Scope, DefinitionID: gitwebhooktest.DefinitionName, WorkItemID: "wi1", Commit: gitwebhooktest.CommitA, Ref: gitwebhooktest.MainRef,
		TriggerID: f.Trigger.ID, DeliveryID: "d1", IdempotencyKey: "git-hook:" + string(f.Trigger.ID) + ":d1",
		RequestedBy: gitwebhooktest.Installer, RepositoryID: gitwebhooktest.RepositoryName,
	}
}

func TestExecutionPlanner(t *testing.T) {
	ctx := context.Background()
	f := gitwebhooktest.NewFixture(t)
	wsOK := workspaceOf(f, gitwebhooktest.RepositoryName, gitwebhooktest.RepositoryURL)
	buildWS := func(prep *fakePreparer, work fakeWork, ws fakeWorkspaces) *adapters.ExecutionPlanner {
		p, err := adapters.NewExecutionPlanner(adapters.PlannerConfig{Preparer: prep, Work: work, Repositories: f.Repos, Workspaces: ws})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	build := func(prep *fakePreparer, work fakeWork) *adapters.ExecutionPlanner { return buildWS(prep, work, wsOK) }
	okWork := fakeWork{snap: execution.WorkSnapshot{ID: "wi1", Actionable: true, ContentRevision: 7}}

	t.Run("maps the verified push onto an exact-commit admission by the installer", func(t *testing.T) {
		prep := &fakePreparer{}
		plan, err := build(prep, okWork).Plan(ctx, plannerRequest(f))
		if err != nil || plan == nil {
			t.Fatalf("plan = %v, %v", plan, err)
		}
		if prep.caller.PrincipalID != gitwebhooktest.Installer || prep.cmd.SourceCommit != string(gitwebhooktest.CommitA) ||
			prep.cmd.ContentRevision != 7 || prep.cmd.WorkItemID != "wi1" || prep.cmd.DefinitionID != gitwebhooktest.DefinitionName || prep.cmd.Scope != f.Scope {
			t.Fatalf("admission was asked %+v by %+v", prep.cmd, prep.caller)
		}
		if prep.caller.RequestID == "" || len(prep.caller.RequestID) > 256 {
			t.Fatalf("request id %q", prep.caller.RequestID)
		}
	})

	t.Run("the request identifier is bounded", func(t *testing.T) {
		prep := &fakePreparer{}
		req := plannerRequest(f)
		req.IdempotencyKey = "git-hook:" + strings.Repeat("a", 500)
		if _, err := build(prep, okWork).Plan(ctx, req); err != nil || len(prep.caller.RequestID) != 256 {
			t.Fatalf("%v, %d", err, len(prep.caller.RequestID))
		}
	})

	t.Run("permanent refusals are not retried and call no admission", func(t *testing.T) {
		for name, c := range map[string]struct {
			mut  func(*gw.RunRequest)
			work fakeWork
			prep *fakePreparer
		}{
			"no work item":           {mut: func(r *gw.RunRequest) { r.WorkItemID = "" }, work: okWork, prep: &fakePreparer{}},
			"another repository":     {mut: func(r *gw.RunRequest) { r.RepositoryID = "repo-other" }, work: okWork, prep: &fakePreparer{}},
			"unknown work item":      {work: fakeWork{err: execution.ErrNotFound}, prep: &fakePreparer{}},
			"definition refused":     {work: okWork, prep: &fakePreparer{err: execution.Refusedf("capability")}},
			"installer lost the run": {work: okWork, prep: &fakePreparer{err: execution.ErrDenied}},
			"work item cancelled":    {work: okWork, prep: &fakePreparer{err: execution.ErrWorkCancelled}},
			"work item not ready":    {work: okWork, prep: &fakePreparer{err: execution.ErrNotActionable}},
		} {
			req := plannerRequest(f)
			if c.mut != nil {
				c.mut(&req)
			}
			_, err := build(c.prep, c.work).Plan(ctx, req)
			if !errors.Is(err, gw.ErrRunRefused) || errors.Is(err, gw.ErrUnavailable) {
				t.Errorf("%s: %v, want a permanent ErrRunRefused", name, err)
			}
			if (name == "no work item" || name == "another repository") && c.prep.calls != 0 {
				t.Errorf("%s reached admission", name)
			}
		}
	})

	t.Run("a workspace of another repository is refused before admission", func(t *testing.T) {
		const other = workspace.RepositoryID("repo-a")
		for name, ws := range map[string]fakeWorkspaces{
			"another repository id":  workspaceOf(f, other, gitwebhooktest.RepositoryURL),
			"another repository url": workspaceOf(f, gitwebhooktest.RepositoryName, "https://git.example.test/org/a.git"),
			"missing workspace":      {byID: map[workspace.ID]*workspace.Workspace{}},
		} {
			plan, err := buildWS(&fakePreparer{}, okWork, ws).Plan(ctx, plannerRequest(f))
			if !errors.Is(err, gw.ErrRunRefused) || errors.Is(err, gw.ErrUnavailable) || plan != nil {
				t.Errorf("%s: %v %v, want a permanent ErrRunRefused and no plan", name, plan, err)
			}
		}
		if _, err := buildWS(&fakePreparer{}, okWork, fakeWorkspaces{err: errors.New("db down")}).Plan(ctx, plannerRequest(f)); !errors.Is(err, gw.ErrUnavailable) {
			t.Errorf("workspace outage = %v, want ErrUnavailable", err)
		}
	})

	t.Run("a project without a repository binding refuses", func(t *testing.T) {
		req := plannerRequest(f)
		req.Scope = f.Other
		_, err := build(&fakePreparer{}, okWork).Plan(ctx, req)
		if !errors.Is(err, gw.ErrRunRefused) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("outages stay retryable", func(t *testing.T) {
		for name, err := range map[string]error{
			"conflict":     execution.ErrConflict,
			"quota":        execution.ErrQuotaExceeded,
			"busy":         execution.ErrBusy,
			"unavailable":  execution.Unavailablef(nil, "workspace"),
			"unclassified": errors.New("connection reset"),
		} {
			_, got := build(&fakePreparer{err: err}, okWork).Plan(ctx, plannerRequest(f))
			if !errors.Is(got, gw.ErrUnavailable) || errors.Is(got, gw.ErrRunRefused) {
				t.Errorf("%s: %v, want ErrUnavailable", name, got)
			}
		}
		_, got := build(&fakePreparer{}, fakeWork{err: errors.New("db down")}).Plan(ctx, plannerRequest(f))
		if !errors.Is(got, gw.ErrUnavailable) {
			t.Errorf("work lookup outage: %v", got)
		}
		f.Repos.Err = errors.New("repositories down")
		_, got = build(&fakePreparer{}, okWork).Plan(ctx, plannerRequest(f))
		f.Repos.Err = nil
		if errors.Is(got, gw.ErrRunRefused) || got == nil {
			t.Errorf("repository lookup outage: %v", got)
		}
	})

	t.Run("a missing collaborator fails construction", func(t *testing.T) {
		for name, cfg := range map[string]adapters.PlannerConfig{
			"no preparer":     {Work: okWork, Repositories: f.Repos, Workspaces: wsOK},
			"no work":         {Preparer: &fakePreparer{}, Repositories: f.Repos, Workspaces: wsOK},
			"no repositories": {Preparer: &fakePreparer{}, Work: okWork, Workspaces: wsOK},
			"no workspaces":   {Preparer: &fakePreparer{}, Work: okWork, Repositories: f.Repos},
		} {
			if _, err := adapters.NewExecutionPlanner(cfg); err == nil {
				t.Errorf("%s: construction succeeded", name)
			}
		}
		if _, err := adapters.NewRuns(nil, nil); err == nil {
			t.Error("NewRuns accepted nothing")
		}
	})

	var _ workspace.Repositories = f.Repos
}

type defReader struct {
	err error
	got []domain.DefinitionID
}

func (d *defReader) Get(_ context.Context, _ domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	d.got = append(d.got, id)
	return pipeline.Definition{}, d.err
}

func TestDefinitions(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{TenantID: "t1", ProjectID: "p1"}
	for name, c := range map[string]struct {
		err  error
		want error // nil: registered
	}{
		"registered": {},
		"missing":    {err: pipeline.ErrNotFound, want: gw.ErrNotFound},
		"hidden":     {err: pipeline.ErrHidden, want: gw.ErrNotFound},
		"outage":     {err: errors.New("db down"), want: gw.ErrUnavailable},
	} {
		d, err := adapters.NewDefinitions(&defReader{err: c.err})
		if err != nil {
			t.Fatal(err)
		}
		got := d.Registered(ctx, scope, "def1")
		if c.want == nil && got != nil || c.want != nil && !errors.Is(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
		if name == "outage" && errors.Is(got, gw.ErrNotFound) {
			t.Error("an outage must not read as a missing definition")
		}
	}
	r := &defReader{}
	d, _ := adapters.NewDefinitions(r)
	if err := d.Registered(ctx, domain.Scope{}, "def1"); !errors.Is(err, gw.ErrNotFound) || len(r.got) != 0 {
		t.Fatalf("an invalid scope must not reach the reader: %v", err)
	}
	if err := d.Registered(ctx, scope, "bad id!"); !errors.Is(err, gw.ErrNotFound) || len(r.got) != 0 {
		t.Fatalf("an invalid identifier must not reach the reader: %v", err)
	}
	if _, err := adapters.NewDefinitions(nil); err == nil {
		t.Fatal("a nil reader must fail construction")
	}
}
