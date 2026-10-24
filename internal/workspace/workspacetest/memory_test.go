package workspacetest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
	"github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// TestUnitMemoryWorkspaceContract runs the shared workspace persistence
// contract on the memory adapter: no database.
func TestUnitMemoryWorkspaceContract(t *testing.T) {
	workspacetest.StoreContract(t, workspacetest.OpenMemStore)
}

// TestUnitMemoryWorkspaceGitContract runs the shared Git contract on the
// temporary bare repositories.
func TestUnitMemoryWorkspaceGitContract(t *testing.T) {
	workspacetest.GitContract(t, workspacetest.OpenLocalGit)
}

func TestUnitMemoryWorkspaceIsDevelopmentOnly(t *testing.T) {
	e := workspacetest.NewMemStoreEnv(t)
	g := workspacetest.LocalGitFor(t)
	// The ports the application injects carry the marker, not only the parent.
	for _, v := range []any{e.Mem, g, g.Reader(), g.Preparer(), g.Publisher()} {
		d, ok := v.(interface{ DevelopmentOnly() bool })
		if !ok || !d.DevelopmentOnly() {
			t.Fatalf("%T is not flagged development-only", v)
		}
	}
	if _, err := workspacetest.NewMemStore(nil, e.Events, workspacetest.MemOptions{}); !errors.Is(err, workspace.ErrNotConfigured) {
		t.Fatalf("a store without a kernel: %v", err)
	}
	if _, err := workspacetest.NewMemStore(e.DB, nil, workspacetest.MemOptions{}); !errors.Is(err, workspace.ErrNotConfigured) {
		t.Fatalf("a store without an event appender: %v", err)
	}
}

// A peer that joins the caller's Tx through UpdateInTx rolls back with it: the
// reservation, its event and the peer's own write are one unit (SD-A12).
func TestUnitMemoryWorkspaceUpdateInTxJoinsTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	e := workspacetest.NewMemStoreEnv(t)
	w, err := workspace.New(workspace.NewParams{ID: "ws_join", Scope: workspacetest.ScopeA, WorkItemID: "wi_join", RepositoryID: "repo_t088",
		RepositoryURL: "https://git.example.test/acme/app.git", CheckoutSecretRef: "secret://c", PublisherSecretRef: "secret://p"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Mem.Insert(ctx, w, workspace.Effects{}); err != nil {
		t.Fatal(err)
	}
	commit := workspace.Commit(strings.Repeat("a", 40))
	provision := func(a *workspace.Aggregate) (workspace.Effects, error) {
		return workspace.Effects{}, a.Workspace.CompleteProvisioning(commit, time.Now())
	}
	boom := errors.New("the caller's own write failed")
	err = e.DB.Update(ctx, workspacetest.ScopeA.TenantID, func(tx memdb.Tx) error {
		if _, err := e.Mem.UpdateInTx(ctx, tx, workspacetest.ScopeA, "ws_join", workspace.Load{}, provision); err != nil {
			return err
		}
		if err := eventstest.Note(tx, "attempt:att_join"); err != nil {
			return err
		}
		// The caller sees the uncommitted workspace through the same Tx.
		got, err := e.Mem.UpdateInTx(ctx, tx, workspacetest.ScopeA, "ws_join", workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) { return workspace.Effects{}, nil })
		if err != nil || got.Workspace.State != workspace.StateAvailable {
			return errors.Join(errors.New("the caller does not see its own update"), err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("update: %v", err)
	}
	if got, _ := e.Mem.Get(ctx, workspacetest.ScopeA, "ws_join"); got.State != workspace.StateProvisioning || got.Version != 1 {
		t.Fatalf("the peer's update survived the caller's rollback: %+v", got)
	}
	if notes, _ := e.Events.Notes(ctx, workspacetest.ScopeA.TenantID); len(notes) != 0 {
		t.Fatalf("the caller's write survived: %v", notes)
	}
	// A Tx of another tenant reaches nothing.
	err = e.DB.Update(ctx, workspacetest.ScopeB.TenantID, func(tx memdb.Tx) error {
		_, err := e.Mem.UpdateInTx(ctx, tx, workspacetest.ScopeA, "ws_join", workspace.Load{}, provision)
		return err
	})
	if !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("a Tx of tenant B updated tenant A's workspace: %v", err)
	}
}

func TestUnitMemoryLocalGitConfinesAddresses(t *testing.T) {
	ctx := context.Background()
	g := workspacetest.LocalGitFor(t)
	repo := g.NewRepo(t, "repo")
	outside := workspacetest.LocalGitFor(t).NewRepo(t, "repo") // a repository under another root
	for name, url := range map[string]string{
		"another root": outside.Path, "relative": "repo.git", "empty": "", "nested": filepath.Join(g.Root(), "x", "repo.git"),
		"not a repository name": filepath.Join(g.Root(), "repo"),
	} {
		ref := repo.Ref(workspacetest.ScopeA)
		ref.URL = url
		if _, _, err := g.Reader().ResolveRef(ctx, ref, repo.BaseRef); err == nil {
			t.Errorf("%s: the reader addressed %q", name, url)
		}
		if err := g.Publisher().DeleteRef(ctx, ref, repo.BaseRef, repo.Initial); err == nil {
			t.Errorf("%s: the publisher addressed %q", name, url)
		}
	}
	if tip := repo.Tip(t, repo.BaseRef); tip != repo.Initial {
		t.Fatalf("a refused address changed the repository: %q", tip)
	}
}

// The adapter is built and released without a testing.TB, as the assembled
// application does.
func TestUnitMemoryLocalGitLifecycle(t *testing.T) {
	ctx := context.Background()
	g, err := workspacetest.NewLocalGit()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := g.CreateRepo(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateRepo(ctx, "app"); !errors.Is(err, workspace.ErrBranchExists) {
		t.Fatalf("a second repository of the same name: %v", err)
	}
	for _, bad := range []string{"", "a/b", "..", "-x", "a.b"} {
		if _, err := g.CreateRepo(ctx, bad); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Errorf("repository name %q: %v", bad, err)
		}
	}
	c, ok, err := g.Reader().ResolveRef(ctx, repo.Ref(workspacetest.ScopeA), repo.BaseRef)
	if err != nil || !ok || c != repo.Initial {
		t.Fatalf("resolve: %q %v %v", c, ok, err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close is idempotent: %v", err)
	}
	if _, err := os.Stat(g.Root()); !os.IsNotExist(err) {
		t.Fatalf("Close left the temporary root: %v", err)
	}
	// An existing root is served, and Close leaves it in place.
	root := t.TempDir()
	at, err := workspacetest.NewLocalGitAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := at.CreateRepo(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if err := at.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "app.git")); err != nil {
		t.Fatalf("Close removed a root it does not own: %v", err)
	}
	if _, err := workspacetest.NewLocalGitAt(filepath.Join(root, "missing")); !errors.Is(err, workspace.ErrInvalidConfiguration) {
		t.Fatalf("a root that does not exist: %v", err)
	}
}

// The real workspace.Service publishes an uploaded bundle through the local
// preparer and publisher: provision, reserve the writer, verify and stage the
// bundle, compare-and-swap the branch, record the checkpoint. A bundle that
// fails verification publishes nothing.
func TestUnitMemoryWorkspaceServicePublication(t *testing.T) {
	ctx := context.Background()
	e := workspacetest.NewMemServiceEnv(t)
	w := e.Ready(workspacetest.ScopeA, "wi_1")
	if tip := e.Repo.Tip(t, w.WorkBranch); tip != w.AcceptedCommit || tip != e.Repo.Initial {
		t.Fatalf("the work branch is at %q, accepted %q", tip, w.AcceptedCommit)
	}
	e.Writer(w, "att_1")
	in := e.Proposal(w, "att_1", e.Repo.Initial, "change", workspacetest.File{Path: "a.txt", Content: "a"})
	res, err := e.Svc.ProposeSource(ctx, in)
	if err != nil || res.Status != workspace.ProposeAccepted || res.AcceptedCommit != in.Commit {
		t.Fatalf("propose: %+v %v", res, err)
	}
	if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit {
		t.Fatalf("tip %q, want %q", tip, in.Commit)
	}
	if tip := e.Repo.Tip(t, e.Repo.BaseRef); tip != e.Repo.Initial {
		t.Fatalf("the base branch moved to %q", tip)
	}
	again, err := e.Svc.ProposeSource(ctx, in)
	if err != nil || again.Status != workspace.ProposeAccepted || again.Commit != in.Commit {
		t.Fatalf("an identical repeat: %+v %v", again, err)
	}
	cur, err := e.Svc.Get(ctx, workspacetest.Engineer, w.Scope, w.ID)
	if err != nil || cur.AcceptedCommit != in.Commit || cur.Pending != nil {
		t.Fatalf("workspace after publication: %+v %v", cur, err)
	}
	ids, _ := e.Events.EventIDs(ctx, w.Scope.TenantID)
	if len(ids) != 3 { // requested, provisioned and the checkpoint
		t.Fatalf("events: %v", ids)
	}

	t.Run("an injected secret never enters source", func(t *testing.T) {
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_2")
		e.Writer(w, "att_2")
		e.Secrets.Set("att_2", "super-secret-token-value")
		in := e.Proposal(w, "att_2", e.Repo.Initial, "leak", workspacetest.File{Path: "env.txt", Content: "TOKEN=super-secret-token-value"})
		if _, err := e.Svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("propose: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("a refused bundle moved the branch to %q", tip)
		}
	})

	t.Run("a bundle that is not the registered artifact is refused", func(t *testing.T) {
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_3")
		e.Writer(w, "att_3")
		in := e.Proposal(w, "att_3", e.Repo.Initial, "change", workspacetest.File{Path: "a.txt", Content: "a"})
		_, other := e.Repo.BundleT(t, e.Repo.Initial, "other", workspacetest.File{Path: "b.txt", Content: "b"})
		e.Bundles.AddWith(workspace.Bundle{ID: in.BundleArtifactID, Scope: w.Scope, AttemptID: "att_3", Role: "bundle", Complete: true,
			Digest: workspacetest.Digest(bytes.Repeat([]byte("x"), 8)), Size: int64(len(other))}, other)
		_, err := e.Svc.ProposeSource(ctx, in)
		var be *workspace.BundleError
		if !errors.As(err, &be) {
			t.Fatalf("a bundle whose digest is not the artifact's: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("tip %q", tip)
		}
	})

	t.Run("a human push that wins the race is never overwritten", func(t *testing.T) {
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_4")
		e.Writer(w, "att_4")
		in := e.Proposal(w, "att_4", e.Repo.Initial, "change", workspacetest.File{Path: "a.txt", Content: "a"})
		human := e.Repo.HumanPush(t, w.WorkBranch, "human", workspacetest.File{Path: "h.txt", Content: "h"})
		_, err := e.Svc.ProposeSource(ctx, in)
		var conflict *workspace.ConflictError
		if !errors.Is(err, workspace.ErrPublicationConflict) || !errors.As(err, &conflict) {
			t.Fatalf("propose: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != human {
			t.Fatalf("the human commit was lost: %q", tip)
		}
	})
}
