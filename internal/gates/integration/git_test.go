package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	gateadapters "github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

type gitCreds struct{ read, write string }

func (c gitCreds) Resolve(_ context.Context, _ domain.Scope, ref string) (gitad.Credential, error) {
	switch ref {
	case wt.CheckoutRef:
		return gitad.Credential{Username: gitfx.Username, Password: c.read}, nil
	case wt.PublisherRef:
		return gitad.Credential{Username: gitfx.Username, Password: c.write}, nil
	}
	return gitad.Credential{}, errors.New("unknown secret reference")
}

type tenantsOf struct{ id domain.TenantID }

func (t tenantsOf) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{t.id}, nil
}

// TestChangedSpecSourceConfigGit covers the source half of DQ-A01/DQ-A03
// against real HTTPS Git: the candidate of a gate must be in the work item's
// retained workspace history. A commit the repository does not hold, or one
// outside the work branch's history, is wrong evidence and no gate is created;
// the run's own pinned commit and a later accepted checkpoint are accepted.
func TestChangedSpecSourceConfigGit(t *testing.T) {
	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "t034-repo")
	scope := gatestest.ScopeA
	gopts := gitad.Options{CAFile: srv.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20, ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
	c := gitCreds{read: srv.ReadToken, write: srv.WriteToken}
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
	repos.Set(scope, workspace.RepositoryConfig{ID: "repo_t034", URL: repo.URL, CheckoutSecretRef: wt.CheckoutRef, PublisherSecretRef: wt.PublisherRef, BaseRef: repo.BaseRef})

	var wsSvc *workspace.Service
	e := gatestest.New(t, gatestest.Options{Sources: func(e *gatestest.Env) execution.Sources {
		var err error
		wsSvc, err = workspace.NewService(workspace.Dependencies{
			Store: e.X.Workspaces, Tenants: tenantsOf{scope.TenantID}, Authorizer: authz, WorkItems: items, Repositories: repos,
			Attempts: wt.NewAttempts(), Bundles: wt.NewBundles(), Secrets: wt.NewSecrets(), Refs: reader, Source: reader, Publisher: publisher,
			Clock: e.X.Clock.Now,
		}, workspace.Options{GitDeadline: 30 * time.Second, ArchiveWait: 3 * time.Second, ArchivePoll: 20 * time.Millisecond, DriverStaleAfter: time.Minute, RedriveAfter: time.Hour, ReconcileBatch: 10})
		if err != nil {
			t.Fatal(err)
		}
		src, err := adapters.NewSources(e.X.Workspaces, reader, wsSvc)
		if err != nil {
			t.Fatal(err)
		}
		return src
	}})
	e.GrantAll()
	ctx := context.Background()

	item := e.X.WorkItem(scope, e.X.ID("wi"), "# Plan\nReview over real Git.")
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
	// A commit on another branch is not part of the work item's history.
	other, otherWork := repo.Commit(t, repo.Initial, "elsewhere", gitfx.File{Path: "b.txt", Content: "two\n"})
	repo.Git(t, "fetch", "-q", otherWork, other+":refs/heads/other-branch")

	p := executiontest.Project{Scope: scope, Item: item, Workspace: w.ID}
	def := e.Define(scope, pipelinetest.Container("test", readRun, reportOutput), gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"))

	// A run pinned to the work branch's accepted checkpoint gets a gate.
	run, err := e.AdmitAt(p, def, moved)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	cancelAtEnd(t, e, run)
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("ok")})
	if rep := reconcile(t, e, scope.TenantID); rep.Requested != 1 {
		t.Fatalf("a candidate in the workspace history was refused: %+v", rep)
	}
	g := gateOf(t, e, run, "review")
	if g.Subject.CandidateCommit() != moved {
		t.Fatalf("candidate %s", g.Subject.CandidateCommit())
	}

	// The original provisioned commit is also in the history.
	run2, err := e.AdmitAt(p, def, runCommit)
	if err != nil {
		t.Fatalf("admit original commit: %v", err)
	}
	cancelAtEnd(t, e, run2)

	// Admission itself refuses commits outside the history, and the gate's own
	// verification refuses a candidate the history no longer holds.
	if _, err := e.AdmitAt(p, def, other); err == nil {
		t.Fatalf("a commit of another branch was admitted")
	}
	verify, err := gateadapters.NewCandidates(mustSources(t, e, repo, reader, wsSvc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Verify(ctx, scope, item, other); !errors.Is(err, gates.ErrEvidence) {
		t.Fatalf("a commit outside the work branch history: %v", err)
	}
	if _, err := verify.Verify(ctx, scope, item, "0123456789abcdef0123456789abcdef01234567"); !errors.Is(err, gates.ErrEvidence) {
		t.Fatalf("a commit the repository does not hold: %v", err)
	}
	src, err := verify.Verify(ctx, scope, item, moved)
	if err != nil {
		t.Fatalf("a commit in the history: %v", err)
	}
	// The verified repository is carried to the gate, not discarded.
	if src.Repository != repo.URL {
		t.Fatalf("verified repository %q, want %q", src.Repository, repo.URL)
	}

	// Evidence whose provenance names another repository than the verified
	// one is wrong evidence even at the verified commit.
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("from elsewhere"),
		Provenance: &gatestest.Provenance{Repository: "https://git.example.test/acme/elsewhere.git", Commit: runCommit}})
	if rep := reconcile(t, e, scope.TenantID); rep.Requested != 0 || rep.Blocked != 1 {
		t.Fatalf("evidence of another repository: %+v", rep)
	}
	if _, err := e.Store.ByStep(ctx, scope, run2.ID, "review"); !errors.Is(err, gates.ErrNotFound) {
		t.Fatalf("a gate exists over evidence of another repository: %v", err)
	}
}

func mustSources(t *testing.T, e *gatestest.Env, repo *gitfx.Repo, reader *gitad.Reader, svc *workspace.Service) execution.Sources {
	t.Helper()
	src, err := adapters.NewSources(e.X.Workspaces, reader, svc)
	if err != nil {
		t.Fatal(err)
	}
	return src
}
