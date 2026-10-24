package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// Test_retention_pin_delete_race: concurrent pin and removal of the same commit
// cannot lose a referenced commit, and a missing deletion policy denies removal.
func Test_retention_pin_delete_race(t *testing.T) {
	ctx := context.Background()

	t.Run("concurrent pin and removal never lose a referenced commit", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Policy.Allow = true

		const n = 6
		commits := make([]string, n)
		old := make([]*workspace.RetainedRef, n)
		for i := range n {
			commits[i] = e.Repo.HumanPush(t, w.WorkBranch, fmt.Sprintf("c%d", i), gitfx.File{Path: "f.txt", Content: fmt.Sprint(i)})
		}
		if _, err := e.Svc.Sync(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(commits[i]), RunID: domain.RunID(fmt.Sprintf("run_old_%d", i))})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Svc.ReleasePin(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID); err != nil {
				t.Fatal(err)
			}
			old[i] = pin
		}

		fresh := make([]*workspace.RetainedRef, n)
		var wg sync.WaitGroup
		errCh := make(chan error, 4*n)
		for i := range n {
			wg.Add(2)
			go func() { // a new reader pins the same commit ...
				defer wg.Done()
				for attempt := 0; attempt < 200; attempt++ {
					pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(commits[i]), RunID: domain.RunID(fmt.Sprintf("run_new_%d", i))})
					switch {
					case err == nil:
						fresh[i] = pin
						return
					case errors.Is(err, workspace.ErrRetentionBusy):
						continue
					default:
						errCh <- fmt.Errorf("pin %d: %w", i, err)
						return
					}
				}
				errCh <- fmt.Errorf("pin %d never succeeded", i)
			}()
			go func() { // ... while the previous reference is being removed.
				defer wg.Done()
				for attempt := 0; attempt < 200; attempt++ {
					err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, old[i].ID, "cleanup")
					switch {
					case err == nil:
						return
					case errors.Is(err, workspace.ErrRemovalDeferred):
						continue
					default:
						errCh <- fmt.Errorf("removal %d: %w", i, err)
						return
					}
				}
				errCh <- fmt.Errorf("removal %d never completed", i)
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}

		// The branch is rewritten away and Git collects garbage: only references protect now.
		e.Repo.SetRef(t, w.WorkBranch, e.Repo.Initial)
		e.Repo.GC(t)
		for i := range n {
			if fresh[i] == nil || fresh[i].Status != workspace.PinConfirmed {
				t.Fatalf("pin %d: %+v", i, fresh[i])
			}
			if got := e.Repo.Tip(t, fresh[i].RefName); got != commits[i] {
				t.Fatalf("pin %d: reference holds %q, want %s", i, got, commits[i])
			}
			if !e.Repo.HasCommit(t, commits[i]) {
				t.Fatalf("referenced commit %d was lost", i)
			}
			if got := e.Repo.Tip(t, old[i].RefName); got != "" {
				t.Fatalf("released reference %d should have been removed: %s", i, got)
			}
		}
	})

	t.Run("a removal that starts first makes a concurrent pin of the commit wait", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Policy.Allow = true
		h := e.Repo.HumanPush(t, w.WorkBranch, "c", gitfx.File{Path: "f.txt", Content: "c"})
		if _, err := e.Svc.Sync(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); err != nil {
			t.Fatal(err)
		}
		old, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(h), RunID: "run_1"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = e.Svc.ReleasePin(ctx, wt.Engineer, wt.ScopeA, w.ID, old.ID)
		// Leave the removal half done: accepted, deletion never acknowledged.
		agg, err := e.Store.Update(ctx, w.Scope, w.ID, workspace.Load{Pins: true}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			p := a.Pins[0]
			if err := p.BeginRemoval(e.Clock.Now()); err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Pins: []*workspace.RetainedRef{p}}, nil
		})
		if err != nil || agg.Pins[0].Status != workspace.PinRemoving {
			t.Fatalf("%v", err)
		}
		if _, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(h), RunID: "run_2"}); !errors.Is(err, workspace.ErrRetentionBusy) {
			t.Fatalf("pin during removal: %v", err)
		}
		// Reconciliation finishes the removal; the pin then succeeds.
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(h), RunID: "run_2"}); err != nil {
			t.Fatalf("pin after removal: %v", err)
		}
	})

	t.Run("a missing deletion policy denies removal", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(e.Repo.Initial), RunID: "run_1"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = e.Svc.ReleasePin(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID)
		noPolicy := e.NewService(func(d *workspace.Dependencies, _ *workspace.Options) { d.Deletion = nil })
		if err := noPolicy.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); !errors.Is(err, workspace.ErrRemovalDenied) {
			t.Fatalf("no policy: %v", err)
		}
		e.Policy.Err = errors.New("policy service down")
		e.Policy.Allow = true
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); !errors.Is(err, workspace.ErrRemovalDenied) {
			t.Fatalf("policy outage must deny: %v", err)
		}
		if e.Repo.Tip(t, pin.RefName) != e.Repo.Initial {
			t.Fatal("reference removed without a policy decision")
		}
	})
}
