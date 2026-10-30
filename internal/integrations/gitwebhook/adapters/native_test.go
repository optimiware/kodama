package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/repository"
	"github.com/optimiware/kodama/internal/work"
)

var nativeScope = domain.Scope{TenantID: "tenant-a", ProjectID: "project-a"}

type admitStub struct {
	caller execution.Caller
	cmd    execution.SourceRunCommand
	run    *execution.Run
	err    error
}

func (a *admitStub) AdmitSourceRun(_ context.Context, c execution.Caller, cmd execution.SourceRunCommand) (*execution.Run, error) {
	a.caller, a.cmd = c, cmd
	return a.run, a.err
}

type readerStub struct {
	agg *execution.Aggregate
	err error
}

func (r readerStub) Get(context.Context, domain.Scope, domain.RunID) (*execution.Aggregate, error) {
	return r.agg, r.err
}

func pushRun(id domain.RunID, repo, commit string) *execution.Run {
	return &execution.Run{Scope: nativeScope, ID: id, Sources: &execution.RunSources{Entries: []execution.RunSource{{RepositoryID: repo, Commit: commit, Path: "."}}}}
}

func pushReq() gw.SourceRunRequest {
	return gw.SourceRunRequest{
		Scope: nativeScope, RunID: "run_1", DefinitionID: "pd_ci", Principal: "usr_1", RequestID: "evt_1", ExpectedCommit: "2222222222222222222222222222222222222222",
		Origin: gw.RunOrigin{PushID: "push_1", RepositoryID: "repo_1", Ref: "refs/heads/main"},
	}
}

func TestNativeRunsPushOrigin(t *testing.T) {
	ctx := context.Background()
	stub := &admitStub{run: pushRun("run_1", "repo_1", "2222222222222222222222222222222222222222")}
	runs, err := adapters.NewNativeRuns(stub, readerStub{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runs.Admit(ctx, pushReq())
	if err != nil || ref.ID != "run_1" {
		t.Fatalf("%+v %v", ref, err)
	}
	c := stub.cmd
	if c.Origin != (execution.Origin{Kind: execution.OriginPush, PushID: "push_1", RepositoryID: "repo_1", Ref: "refs/heads/main"}) ||
		c.RunID != "run_1" || c.DefinitionID != "pd_ci" || c.Scope != nativeScope || c.Repository != "" || c.ContentRevision != 0 || len(c.Pins) != 0 {
		t.Fatalf("a push run is admitted with its origin and stable run ID, without work item, repository, revision or pins: %+v", c)
	}
	if err := c.Origin.Validate(); err != nil {
		t.Fatalf("the origin satisfies execution's own rule: %v", err)
	}
	if stub.caller.PrincipalID != "usr_1" || stub.caller.RequestID != "evt_1" {
		t.Fatalf("caller: %+v", stub.caller)
	}

	t.Run("the pinned commit must be the pushed one", func(t *testing.T) {
		for name, run := range map[string]*execution.Run{
			"another commit":    pushRun("run_1", "repo_1", "3333333333333333333333333333333333333333"),
			"another repo":      pushRun("run_1", "repo_9", "2222222222222222222222222222222222222222"),
			"no source set":     {Scope: nativeScope, ID: "run_1"},
			"another run":       pushRun("run_2", "repo_1", "2222222222222222222222222222222222222222"),
			"another scope run": {Scope: domain.Scope{TenantID: "tenant-b", ProjectID: "project-a"}, ID: "run_1"},
		} {
			runs, _ := adapters.NewNativeRuns(&admitStub{run: run}, readerStub{})
			if _, err := runs.Admit(ctx, pushReq()); !errors.Is(err, gw.ErrUnavailable) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("a malformed request never reaches admission", func(t *testing.T) {
		stub := &admitStub{}
		runs, _ := adapters.NewNativeRuns(stub, readerStub{})
		bad := pushReq()
		bad.Origin.WorkItemID = "wi_1"
		for name, req := range map[string]gw.SourceRunRequest{
			"push with a work item": bad,
			"no commit":             func() gw.SourceRunRequest { r := pushReq(); r.ExpectedCommit = ""; return r }(),
			"no origin":             func() gw.SourceRunRequest { r := pushReq(); r.Origin = gw.RunOrigin{}; return r }(),
		} {
			if _, err := runs.Admit(ctx, req); !errors.Is(err, gw.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if stub.cmd.RunID != "" {
			t.Fatalf("admission was reached: %+v", stub.cmd)
		}
	})
}

func TestNativeRunsWorkItemOrigin(t *testing.T) {
	stub := &admitStub{run: &execution.Run{Scope: nativeScope, ID: "run_w"}}
	runs, _ := adapters.NewNativeRuns(stub, readerStub{})
	ref, err := runs.Admit(context.Background(), gw.SourceRunRequest{
		Scope: nativeScope, RunID: "run_w", DefinitionID: "pd_task", Principal: "usr_2", RequestID: "evt_2", ContentRevision: 4, Origin: gw.RunOrigin{WorkItemID: "wi_1"},
	})
	if err != nil || ref.ID != "run_w" {
		t.Fatalf("%+v %v", ref, err)
	}
	if c := stub.cmd; c.Origin != (execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: "wi_1"}) || c.ContentRevision != 4 || c.DefinitionID != "pd_task" || c.RunID != "run_w" {
		t.Fatalf("%+v", c)
	}
}

func TestNativeRunsClassifiesAdmission(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		permanent bool
	}{
		"admission refused": {execution.Refusedf("source_set_push_repository"), true},
		"denied":            {execution.ErrDenied, true},
		"hidden":            {execution.ErrHidden, true},
		"not actionable":    {execution.ErrNotActionable, true},
		"cancelled":         {execution.ErrWorkCancelled, true},
		"quota":             {execution.ErrQuotaExceeded, false},
		"busy":              {execution.ErrBusy, false},
		"stale":             {execution.ErrStale, false},
		"not configured":    {execution.ErrNotConfigured, false},
		"infrastructure":    {errors.New("connection reset"), false},
	} {
		runs, _ := adapters.NewNativeRuns(&admitStub{err: tc.err}, readerStub{})
		_, err := runs.Admit(context.Background(), pushReq())
		if errors.Is(err, gw.ErrRunRefused) != tc.permanent || errors.Is(err, gw.ErrUnavailable) == tc.permanent {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNativeRunsRefusalCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"denied":              {execution.ErrDenied, "run_authority_denied"},
		"hidden":              {execution.ErrHidden, "run_authority_denied"},
		"not actionable":      {execution.ErrNotActionable, "work_item_not_actionable"},
		"cancelled":           {execution.ErrWorkCancelled, "work_item_cancelled"},
		"purpose":             {execution.ErrPurposeDenied, "run_purpose_denied"},
		"invalid":             {execution.Invalidf("secret detail"), "admission_invalid"},
		"not found":           {execution.ErrNotFound, "admission_target_not_found"},
		"admission code":      {execution.Refusedf("source_set_push_repository: the pushed repository is not a source of the run"), "source_set_push_repository"},
		"admission no code":   {execution.Refusedf("The definition pd_private is refused"), gw.ReasonAdmissionRefused},
		"wrapped denial":      {fmt.Errorf("authorize usr_secret: %w", execution.ErrDenied), "run_authority_denied"},
		"wrapped with detail": {fmt.Errorf("%w: usr_secret", execution.ErrDenied), "run_authority_denied"},
	} {
		runs, _ := adapters.NewNativeRuns(&admitStub{err: tc.err}, readerStub{})
		_, err := runs.Admit(context.Background(), pushReq())
		if !errors.Is(err, gw.ErrRunRefused) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := gw.RefusalCode(err); got != tc.code {
			t.Errorf("%s: code %q, want %q", name, got, tc.code)
		}
	}
	// An outage never carries a code.
	runs, _ := adapters.NewNativeRuns(&admitStub{err: execution.ErrQuotaExceeded}, readerStub{})
	_, err := runs.Admit(context.Background(), pushReq())
	if errors.Is(err, gw.ErrRunRefused) || gw.RefusalCode(err) != gw.ReasonAdmissionRefused {
		t.Fatalf("outage: %v", err)
	}
}

func TestNativeRunsFind(t *testing.T) {
	ctx := context.Background()
	run := func(r readerStub) (bool, error) {
		runs, _ := adapters.NewNativeRuns(&admitStub{}, r)
		return runs.Find(ctx, nativeScope, "run_1")
	}
	if ok, err := run(readerStub{agg: &execution.Aggregate{Run: &execution.Run{}}}); err != nil || !ok {
		t.Fatalf("existing: %v %v", ok, err)
	}
	if ok, err := run(readerStub{err: execution.ErrNotFound}); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if _, err := run(readerStub{err: errors.New("down")}); !errors.Is(err, gw.ErrUnavailable) {
		t.Fatalf("outage: %v", err)
	}
	runs, _ := adapters.NewNativeRuns(&admitStub{}, readerStub{})
	if _, err := runs.Find(ctx, domain.Scope{}, "run_1"); !errors.Is(err, gw.ErrInvalid) {
		t.Fatalf("invalid scope: %v", err)
	}
	if _, err := adapters.NewNativeRuns(nil, readerStub{}); err == nil {
		t.Fatal("run admission is required")
	}
}

type reloadStub struct {
	rec gitv1.PushRecord
	err error
}

func (r reloadStub) ReloadByReceive(context.Context, domain.Scope, string, string) (gitv1.PushRecord, error) {
	return r.rec, r.err
}

func TestPushReader(t *testing.T) {
	ctx := context.Background()
	read := func(err error) error {
		p, _ := adapters.NewPushReader(reloadStub{rec: gitv1.PushRecord{PushID: "p"}, err: err})
		_, e := p.Reload(ctx, nativeScope, "repo_1", "p")
		return e
	}
	if err := read(nil); err != nil {
		t.Fatal(err)
	}
	if err := read(&gitv1.ContractError{Code: domain.CodeNotFound, Message: "not found"}); !errors.Is(err, gw.ErrNotFound) {
		t.Fatalf("not_found: %v", err)
	}
	if err := read(errors.New("db down")); !errors.Is(err, gw.ErrUnavailable) {
		t.Fatalf("outage: %v", err)
	}
	if _, err := adapters.NewPushReader(nil); err == nil {
		t.Fatal("reload is required")
	}
}

type repoStub struct {
	r   *repository.Repository
	err error
}

func (s repoStub) Get(context.Context, domain.Scope, repository.ID) (*repository.Repository, error) {
	return s.r, s.err
}

func TestNativeRepositories(t *testing.T) {
	ctx := context.Background()
	repo := &repository.Repository{ID: "repo_1", Scope: nativeScope, Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady}
	a, _ := adapters.NewNativeRepositories(repoStub{r: repo})
	f, err := a.Repository(ctx, nativeScope, "repo_1")
	if err != nil || f.ID != "repo_1" || f.Hosting != gitv1.HostingInternal || f.Status != gitv1.StatusReady {
		t.Fatalf("%+v %v", f, err)
	}
	for name, s := range map[string]repoStub{
		"not found":     {err: repository.ErrNotFound},
		"foreign scope": {r: &repository.Repository{ID: "repo_1", Scope: domain.Scope{TenantID: "tenant-b", ProjectID: "project-a"}}},
	} {
		a, _ := adapters.NewNativeRepositories(s)
		if _, err := a.Repository(ctx, nativeScope, "repo_1"); !errors.Is(err, gw.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	a, _ = adapters.NewNativeRepositories(repoStub{err: errors.New("down")})
	if _, err := a.Repository(ctx, nativeScope, "repo_1"); !errors.Is(err, gw.ErrUnavailable) {
		t.Fatalf("outage: %v", err)
	}
}

type itemStub struct {
	it  work.Item
	err error
}

func (s itemStub) Get(context.Context, domain.Scope, domain.WorkItemID) (work.Item, error) {
	return s.it, s.err
}

func TestWorkReader(t *testing.T) {
	ctx := context.Background()
	item := work.Item{Scope: nativeScope, ID: "wi_1", Kind: work.KindTask, State: work.StateReady, ContentRevision: 3, BlockedReason: "waiting"}
	w, _ := adapters.NewWorkReader(itemStub{it: item})
	f, err := w.Facts(ctx, nativeScope, "wi_1")
	if err != nil || f.State != "ready" || f.ContentRevision != 3 || !f.Actionable || f.Cancelled || f.BlockedReason != "waiting" {
		t.Fatalf("%+v %v", f, err)
	}
	item.Kind, item.State = work.KindEpic, work.StateCancelled
	w, _ = adapters.NewWorkReader(itemStub{it: item})
	if f, _ := w.Facts(ctx, nativeScope, "wi_1"); f.Actionable || !f.Cancelled {
		t.Fatalf("epic, cancelled: %+v", f)
	}
	w, _ = adapters.NewWorkReader(itemStub{err: work.ErrNotFound})
	if _, err := w.Facts(ctx, nativeScope, "wi_1"); !errors.Is(err, gw.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	w, _ = adapters.NewWorkReader(itemStub{it: work.Item{Scope: domain.Scope{TenantID: "tenant-b", ProjectID: "project-a"}}})
	if _, err := w.Facts(ctx, nativeScope, "wi_1"); !errors.Is(err, gw.ErrNotFound) {
		t.Fatalf("foreign: %v", err)
	}
	w, _ = adapters.NewWorkReader(itemStub{err: errors.New("down")})
	if _, err := w.Facts(ctx, nativeScope, "wi_1"); !errors.Is(err, gw.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}
