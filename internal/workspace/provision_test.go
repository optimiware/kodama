package workspace_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// Test_duplicate_provisioning: WS-01, WS-02 and WS-A01. Concurrent provisioning
// and a lost branch-creation acknowledgment yield one workspace and one branch
// at a recorded commit; epic, foreign and unauthorized requests are denied.
func Test_duplicate_provisioning(t *testing.T) {
	t.Run("concurrent provisioning yields one workspace and one branch", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Items.Add(wt.ScopeA, "wi_1", "story")
		const n = 8
		ids := make([]workspace.ID, n)
		created := make([]bool, n)
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w, c, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1")
				if err != nil {
					errs <- err
					return
				}
				ids[i], created[i] = w.ID, c
				if w.State != workspace.StateAvailable {
					errs <- errors.New("not available: " + string(w.State) + " " + w.LastError)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		creators := 0
		for i := range n {
			if ids[i] != ids[0] {
				t.Fatalf("two workspaces: %s and %s", ids[0], ids[i])
			}
			if created[i] {
				creators++
			}
		}
		if creators != 1 {
			t.Fatalf("%d callers created the workspace", creators)
		}
		if got := e.CountSQL(`SELECT count(*) FROM workspaces WHERE work_item_id = 'wi_1'`); got != 1 {
			t.Fatalf("%d workspace rows", got)
		}
		w, err := e.Svc.Get(context.Background(), wt.Engineer, wt.ScopeA, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		if w.AcceptedCommit != workspace.Commit(e.Repo.Initial) || w.InitialCommit != w.AcceptedCommit {
			t.Fatalf("recorded commit %s / %s, remote base %s", w.InitialCommit, w.AcceptedCommit, e.Repo.Initial)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("branch %s at %s", w.WorkBranch, tip)
		}
		// One state_changed event per transition: requested, then available.
		if got := e.CountTenant(wt.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'workspace.state_changed'`); got != 2 {
			t.Fatalf("%d state events", got)
		}
		if got := e.CountSQL(`SELECT count(*) FROM audit WHERE resource_type = 'workspace' AND action = 'workspace.provisioned'`); got != 1 {
			t.Fatalf("%d provisioned audit rows", got)
		}
	})

	t.Run("a branch conflict for another item is not a provisioning replay", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Items.Add(wt.ScopeA, "wi_2", "story")
		candidate, err := workspace.New(workspace.NewParams{
			ID: "ws_branch_conflict", Scope: w.Scope, WorkItemID: "wi_2",
			RepositoryID: w.RepositoryID, RepositoryURL: w.RepositoryURL,
			CheckoutSecretRef: w.CheckoutSecretRef, PublisherSecretRef: w.PublisherSecretRef,
		}, e.Clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		// Exercise the persistence boundary's secondary uniqueness constraint:
		// a conflicting branch must not return a different item's workspace.
		candidate.WorkBranch = w.WorkBranch
		got, created, err := e.PG.Insert(context.Background(), candidate, workspace.Effects{})
		if err == nil || created || got != nil {
			t.Fatalf("a foreign-item branch collision became a replay: %+v %v %v", got, created, err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspaces`); n != 1 {
			t.Fatalf("%d workspaces after the rejected branch collision", n)
		}
	})

	t.Run("a lost branch-creation acknowledgment is reconciled without overwriting", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		faults.Publisher.CreateAckLost = 1
		e.Items.Add(wt.ScopeA, "wi_1", "story")
		w, _, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1")
		if err != nil {
			t.Fatal(err)
		}
		if w.State != workspace.StateAvailable || w.AcceptedCommit != workspace.Commit(e.Repo.Initial) {
			t.Fatalf("%+v", w)
		}
		if got := e.CountSQL(`SELECT count(*) FROM workspaces`); got != 1 {
			t.Fatalf("%d workspaces", got)
		}
	})

	t.Run("an independently existing branch is never overwritten", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Items.Add(wt.ScopeA, "wi_1", "story")
		// An unrelated root commit already sits on the work branch.
		orphan := orphanCommit(t, e)
		e.Repo.SetRef(t, "refs/heads/kodama/work/wi_1", orphan)
		w, _, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1")
		if err != nil {
			t.Fatal(err)
		}
		if w.State != workspace.StateProvisioning || w.LastError != workspace.ReasonBranchExists {
			t.Fatalf("state %s error %q", w.State, w.LastError)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != orphan {
			t.Fatalf("the independent branch was overwritten: %s", tip)
		}
		// The provisioning error is preserved and retried: once the conflicting
		// branch is gone the same workspace becomes available.
		e.Repo.DeleteRef(t, w.WorkBranch)
		if err := e.Svc.ReconcileWorkspace(context.Background(), wt.ScopeA, w.ID); err != nil {
			t.Fatal(err)
		}
		got, err := e.Svc.Get(context.Background(), wt.Engineer, wt.ScopeA, w.ID)
		if err != nil || got.State != workspace.StateAvailable || got.LastError != "" {
			t.Fatalf("after retry: %+v %v", got, err)
		}
	})

	t.Run("a human commit on top of a created branch is adopted, not overwritten", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		e.Items.Add(wt.ScopeA, "wi_1", "story")
		// Creation succeeds remotely but its acknowledgment is lost, and a human
		// pushes before the retry.
		faults.Publisher.CreateAckLost = 1
		faults.Store.FailNextUpdates(0)
		w, _, _ := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1")
		human := e.Repo.HumanPush(t, w.WorkBranch, "human work", gitfx.File{Path: "h.txt", Content: "h"})
		again, created, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1")
		if err != nil || created || again.ID != w.ID {
			t.Fatalf("%v %v", created, err)
		}
		if e.Repo.Tip(t, w.WorkBranch) != human {
			t.Fatal("human work was lost")
		}
		synced, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil || synced.AcceptedCommit != workspace.Commit(human) {
			t.Fatalf("sync: %v %+v", err, synced)
		}
	})

	t.Run("epic, foreign and unauthorized requests are denied", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Items.Add(wt.ScopeA, "epic_1", "epic")
		if _, _, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "epic_1"); !errors.Is(err, workspace.ErrNotActionable) {
			t.Fatalf("epic: %v", err)
		}
		// A work item that belongs to another tenant is indistinguishable from a missing one.
		e.Items.Add(wt.ScopeB, "wi_b", "story")
		if _, _, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_b"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("foreign work item: %v", err)
		}
		e.Items.Add(wt.ScopeA, "wi_1", "story")
		stranger := workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}
		if _, _, err := e.Svc.Provision(context.Background(), stranger, wt.ScopeA, "wi_1"); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("ungranted actor: %v", err)
		}
		otherTenant := workspace.Actor{Type: workspace.ActorUser, ID: "usr_other_tenant"}
		if _, _, err := e.Svc.Provision(context.Background(), otherTenant, wt.ScopeA, "wi_1"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("other tenant's user: %v", err)
		}
		if got := e.CountSQL(`SELECT count(*) FROM workspaces`); got != 0 {
			t.Fatalf("%d workspaces created by denied requests", got)
		}
		// Repository configuration outage leaves nothing half-created.
		e.Repos.Err = errors.New("config store down")
		if _, _, err := e.Svc.Provision(context.Background(), wt.Engineer, wt.ScopeA, "wi_1"); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("configuration outage: %v", err)
		}
		if got := e.CountSQL(`SELECT count(*) FROM workspaces`); got != 0 {
			t.Fatalf("%d workspaces after an outage", got)
		}
	})

	t.Run("missing peer adapters fail closed", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		for name, mutate := range map[string]func(*workspace.Dependencies){
			"authorizer": func(d *workspace.Dependencies) { d.Authorizer = nil },
			"work items": func(d *workspace.Dependencies) { d.WorkItems = nil },
			"attempts":   func(d *workspace.Dependencies) { d.Attempts = nil },
			"bundles":    func(d *workspace.Dependencies) { d.Bundles = nil },
			"publisher":  func(d *workspace.Dependencies) { d.Publisher = nil },
			"store":      func(d *workspace.Dependencies) { d.Store = nil },
		} {
			_, err := workspace.NewService(depsWith(e, mutate), wt.DefaultOptions())
			if !errors.Is(err, workspace.ErrNotConfigured) {
				t.Errorf("%s: %v", name, err)
			}
		}
		o := wt.DefaultOptions()
		o.GitDeadline = 0
		if _, err := workspace.NewService(depsWith(e, nil), o); !errors.Is(err, workspace.ErrInvalidConfiguration) {
			t.Errorf("zero deadline: %v", err)
		}
	})
}

var _ domain.Scope

func depsWith(e *wt.Env, mutate func(*workspace.Dependencies)) workspace.Dependencies {
	d := workspace.Dependencies{
		Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos,
		Attempts: e.Attempts, Bundles: e.Bundles, Secrets: e.Secrets, Refs: e.Reader, Source: e.Reader, Publisher: e.Publisher,
	}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

// orphanCommit creates an unrelated root commit inside the fixture repository.
func orphanCommit(t *testing.T, e *wt.Env) string {
	t.Helper()
	return e.Repo.OrphanCommit(t, "unrelated")
}
