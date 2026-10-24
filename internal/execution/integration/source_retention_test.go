package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

type creds struct{ read, write string }

func (c creds) Resolve(_ context.Context, _ domain.Scope, ref string) (gitad.Credential, error) {
	switch ref {
	case wt.CheckoutRef:
		return gitad.Credential{Username: gitfx.Username, Password: c.read}, nil
	case wt.PublisherRef:
		return gitad.Credential{Username: gitfx.Username, Password: c.write}, nil
	}
	return gitad.Credential{}, errors.New("unknown secret reference")
}

// flakySources fails retention a controlled number of times, as a crash or a
// Git outage between the run's commit and the confirmation of its reference.
type flakySources struct {
	execution.Sources
	mu       sync.Mutex
	failures int
	calls    int
}

func (f *flakySources) Retain(ctx context.Context, scope domain.Scope, ws string, run domain.RunID, commit string) (string, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return "", execution.Unavailablef(errors.New("connection reset"), "retained reference creation failed")
	}
	return f.Sources.Retain(ctx, scope, ws, run, commit)
}

// Test_source_retention_recovery: PX-14, PX-02, WS-11. Missing retention
// confirmation blocks dispatch; a crash and retry preserves the same retained
// commit even though the branch moved on, against real PostgreSQL and HTTPS Git.
func Test_source_retention_recovery(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()

	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "t020-repo")
	scope := executiontest.ScopeA
	gopts := gitad.Options{CAFile: srv.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20, ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
	c := creds{read: srv.ReadToken, write: srv.WriteToken}
	reader, err := gitad.NewReader(gopts, c)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := gitad.NewPublisher(gopts, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	t.Cleanup(publisher.Close)

	authz, items, repos := wt.NewAuthz(), wt.NewItems(), wt.NewRepos()
	engineer := workspace.Actor{Type: workspace.ActorUser, ID: "usr_engineer"}
	system := workspace.Actor{Type: workspace.ActorService, ID: "execution"}
	for _, a := range []string{engineer.ID, system.ID} {
		authz.Grant(a, scope)
	}
	repos.Set(scope, workspace.RepositoryConfig{ID: "repo_t020", URL: repo.URL, CheckoutSecretRef: wt.CheckoutRef, PublisherSecretRef: wt.PublisherRef, BaseRef: repo.BaseRef})
	wsSvc, err := workspace.NewService(workspace.Dependencies{
		Store: e.Workspaces, Tenants: tenants{scope.TenantID}, Authorizer: authz, WorkItems: items, Repositories: repos,
		Attempts: wt.NewAttempts(), Bundles: wt.NewBundles(), Secrets: wt.NewSecrets(), Refs: reader, Source: reader, Publisher: publisher,
		Clock: e.Clock.Now,
	}, workspace.Options{GitDeadline: 30 * time.Second, ArchiveWait: 3 * time.Second, ArchivePoll: 20 * time.Millisecond, DriverStaleAfter: time.Minute, RedriveAfter: time.Hour, ReconcileBatch: 10})
	if err != nil {
		t.Fatal(err)
	}
	real, err := adapters.NewSources(e.Workspaces, reader, wsSvc)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakySources{Sources: real, failures: 1}
	svc := e.NewService(executiontest.Options{Sources: flaky})

	// A workspace over the real repository, with the branch moved past the run's
	// commit by an engineer.
	item := e.WorkItem(scope, e.ID("wi"), "# Plan\nRetain the source.")
	items.Add(scope, item, "story")
	w, _, err := wsSvc.Provision(ctx, engineer, scope, item)
	if err != nil || w.State != workspace.StateAvailable {
		t.Fatalf("provision: %v %+v", err, w)
	}
	runCommit := string(w.AcceptedCommit)
	moved := repo.HumanPush(t, w.WorkBranch, "engineer work", gitfx.File{Path: "a.txt", Content: "one\n"})
	if _, err := wsSvc.Sync(ctx, engineer, scope, w.ID, 0); err != nil {
		t.Fatal(err)
	}
	def := e.Define(scope, pipelinetest.Doc(pipelinetest.Container("build", "workspace: {access: read, from: run}")))
	caller := execution.Caller{PrincipalID: executiontest.Engineer}
	cmd := execution.AdmitCommand{Scope: scope, WorkItemID: item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(executiontest.Project{Scope: scope, Item: item}), SourceCommit: runCommit}

	// The first confirmation fails after the run's intent committed.
	run, err := svc.Admit(ctx, caller, cmd)
	if !errors.Is(err, execution.ErrUnavailable) {
		t.Fatalf("admit with a failing retention: %v", err)
	}
	var id domain.RunID
	e.Scalar(&id, `SELECT id FROM runs WHERE work_item_id = $1`, string(item))
	got := e.Get(&execution.Run{Scope: scope, ID: id}).Run
	if got.SourceRetention != execution.RetentionPending || got.SourceCommit != runCommit {
		t.Fatalf("run %s commit %s (admit returned %v)", got.SourceRetention, got.SourceCommit, run)
	}
	var status string
	e.Scalar(&status, `SELECT status FROM workspace_retained_refs WHERE run_id = $1`, string(id))
	if status != string(workspace.PinIntent) {
		t.Fatalf("retained reference status %s: the intent must be durable before confirmation", status)
	}

	// No dispatch until the reference is confirmed, however long it waits.
	e.Clock.Advance(time.Minute)
	if _, err := svc.ClaimRunnable(ctx, scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("a run with an unconfirmed source was claimed: %v", err)
	}

	// The branch moves again while the confirmation is outstanding.
	again := repo.HumanPush(t, w.WorkBranch, "more engineer work", gitfx.File{Path: "b.txt", Content: "two\n"})
	if again == moved || repo.Tip(t, w.WorkBranch) != again {
		t.Fatal("the branch did not move")
	}

	// Recovery: a restarted controller's reconciliation confirms the SAME retained
	// commit instead of creating another run, and dispatch becomes possible.
	restarted := e.NewService(executiontest.Options{Sources: flaky})
	rep, err := restarted.Reconcile(ctx)
	if err != nil || rep.Retained != 1 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	got = e.Get(&execution.Run{Scope: scope, ID: id}).Run
	if got.SourceRetention != execution.RetentionConfirmed || got.SourceCommit != runCommit {
		t.Fatalf("after recovery: %s commit %s", got.SourceRetention, got.SourceCommit)
	}
	var ref, pinCommit string
	e.Scalar(&ref, `SELECT ref_name FROM workspace_retained_refs WHERE run_id = $1`, string(id))
	e.Scalar(&pinCommit, `SELECT commit_oid FROM workspace_retained_refs WHERE run_id = $1`, string(id))
	if pinCommit != runCommit || repo.Tip(t, ref) != runCommit {
		t.Fatalf("retained ref %s points at %s (pin says %s), want the run's commit %s though the branch is at %s", ref, repo.Tip(t, ref), pinCommit, runCommit, again)
	}
	if n := e.Count(`SELECT count(*) FROM workspace_retained_refs WHERE run_id = $1`, string(id)); n != 1 {
		t.Fatalf("%d retained references for one run", n)
	}
	if n := e.Count(`SELECT count(*) FROM runs`); n != 1 {
		t.Fatalf("%d runs: the retry created another", n)
	}
	if id != "" && got.RetainedRefID == "" {
		t.Fatal("the confirmed run does not name its reference")
	}

	in, err := restarted.ClaimRunnable(ctx, scope.TenantID)
	if err != nil {
		t.Fatalf("claim after confirmation: %v", err)
	}
	agg := e.Get(&execution.Run{Scope: scope, ID: id})
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](agg.Attempt(in.AttemptID).Context)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Workspace.Source == nil || string(pinned.Workspace.Source.Commit) != runCommit || pinned.Workspace.Source.RepositoryURL != repo.URL {
		t.Fatalf("the attempt's source %+v is not the run's commit %s: branch movement must not change a read input", pinned.Workspace.Source, runCommit)
	}
	// Repeating the confirmation is idempotent (a restart after the fact).
	if _, err := restarted.ConfirmSource(ctx, scope, id); err != nil {
		t.Fatal(err)
	}
	if n := e.Count(`SELECT count(*) FROM workspace_retained_refs WHERE run_id = $1`, string(id)); n != 1 {
		t.Fatalf("%d retained references after a repeat", n)
	}
}

type tenants struct{ id domain.TenantID }

func (t tenants) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{t.id}, nil
}
