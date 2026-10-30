package workspace_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// Test_unit_workspace_external_publisher_unchanged: WS-19, WS-A15. With the
// internally hosted model fully wired, an externally hosted workspace still
// provisions, reserves, publishes through the bundle publisher with
// expected-tip compare-and-swap, pins and recovers exactly as before, never
// touches the internal host, and the two models refuse each other's operations.
func Test_unit_workspace_external_publisher_unchanged(t *testing.T) {
	scope := wt.ScopeA

	t.Run("an external workspace publishes through the publisher and never touches the internal host", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		w := e.Ready(scope, "wi_ext")
		if w.Internal() || w.Hosting != workspace.HostingExternal || w.RepositoryID != "repo_t088" || w.AcceptedCommit != e.Repo.Initial || w.PublisherSecretRef == workspace.NoPublisherRef {
			t.Fatalf("workspace: %+v", w)
		}
		if assocs := associations(t, e, w); len(assocs) != 0 {
			t.Fatalf("an external workspace has associations: %+v", assocs)
		}

		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", wt.File{Path: "a.txt", Content: "a"})
		res, err := e.Svc.ProposeSource(bg, in)
		if err != nil || res.Status != workspace.ProposeAccepted || res.AcceptedCommit != in.Commit {
			t.Fatalf("propose: %+v %v", res, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit {
			t.Fatalf("tip %q, want %q", tip, in.Commit)
		}
		settle(t, e, w, "att_1", workspace.OutcomeSucceeded)
		cur := reload(t, e, w)
		if cur.AcceptedCommit != in.Commit || cur.Writer != nil || cur.Pending != nil {
			t.Fatalf("after publication: %+v", cur)
		}
		if got, err := e.Svc.SelectWriterOutput(bg, wt.Engineer, scope, w.ID, "att_1"); err != nil || got != in.Commit {
			t.Fatalf("writer output: %s %v", got, err)
		}
		pin, err := e.Svc.PinWriterOutput(bg, wt.Engineer, scope, w.ID, "att_1", "run_1")
		if err != nil || pin.Commit != in.Commit || pin.RefName[:len("refs/heads/kodama/retained/")] != "refs/heads/kodama/retained/" {
			t.Fatalf("pin: %+v %v", pin, err)
		}
		if rec, err := e.Svc.RecoveryPoint(bg, wt.Engineer, scope, w.ID); err != nil || rec.LastPublishedCommit != in.Commit || !rec.UncommittedWorkMayBeLost {
			t.Fatalf("recovery: %+v %v", rec, err)
		}
		if synced, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil || synced.AcceptedCommit != in.Commit {
			t.Fatalf("sync: %+v %v", synced, err)
		}
		// Neither the worker nor the push store, the gate or the inspector was asked.
		if len(e.Hosts.CreateOps) != 0 || len(e.Hosts.PinOps) != 0 || len(e.Gate.Asked) != 0 || len(e.Inspector.Requests) != 0 {
			t.Fatalf("the internal model was used: %v %v %v %d", e.Hosts.CreateOps, e.Hosts.PinOps, e.Gate.Asked, len(e.Inspector.Requests))
		}
		if checkpointEventCount(t, e, w) != 1 {
			t.Fatalf("checkpoint events: %d, want the publication's", checkpointEventCount(t, e, w))
		}
		// An external workspace never holds an internal writer reservation.
		if ok, err := e.Svc.HoldsWriter(bg, scope, "wi_ext", "att_1"); err != nil || ok {
			t.Fatalf("HoldsWriter on an external workspace: %v %v", ok, err)
		}
	})

	t.Run("a lost publication acknowledgment is still settled by the reconciler", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		w := e.Ready(scope, "wi_ext")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", wt.File{Path: "a.txt", Content: "a"})
		if _, err := e.Svc.ProposeSource(bg, in); err != nil {
			t.Fatal(err)
		}
		// The same proposal again observes the recorded receipt: no second publication.
		again, err := e.Svc.ProposeSource(bg, in)
		if err != nil || again.Status != workspace.ProposeAccepted {
			t.Fatalf("repeat: %+v %v", again, err)
		}
		if rep, err := e.Svc.Reconcile(bg); err != nil || rep.Failed != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
	})

	t.Run("the two models refuse each other's operations", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		ext := e.Ready(scope, "wi_ext")
		e.Seed(scope, wt.RepoApp)
		internal, _ := e.Provision(scope, "wi_int", wt.RepoApp)
		e.Attempts.Register(scope, "att_ext", ext.ID, workspace.AccessWrite)
		e.Attempts.Register(scope, "att_int", internal.ID, workspace.AccessWrite)

		// Internal operations on the external workspace.
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: ext.ID, AttemptID: "att_ext"}); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("internal writer on external: %v", err)
		}
		if _, _, _, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_ext", []workspace.RepositoryID{wt.RepoApp}); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("internal provisioning of an external item: %v", err)
		}
		pushID := e.AppliedPush(scope, wt.RepoApp, "att_ext", "run_1", taskBranch("wi_ext"), e.Repo.Initial, e.Repo.Initial)
		if _, err := accept(e, ext, "att_ext", wt.RepoApp, taskBranch("wi_ext"), e.Repo.Initial, pushID); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("checkpoint on external: %v", err)
		}

		// External operations on the internal workspace.
		in := e.Proposal(ext, "att_ext", e.Repo.Initial, "x", wt.File{Path: "a", Content: "a"})
		in.AttemptID = "att_int"
		if _, err := e.Svc.ProposeSource(bg, in); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("bundle proposal on internal: %v", err)
		}
		e.Attempts.Register(scope, "att_w", internal.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(bg, wt.Engineer, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: internal.ID, AttemptID: "att_w"}); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("publisher writer on internal: %v", err)
		}
		if _, err := e.Svc.PinCommit(bg, wt.Engineer, workspace.PinInput{Scope: scope, WorkspaceID: internal.ID, Commit: internal.AcceptedCommit, RunID: "run_1"}); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("external pin on internal: %v", err)
		}
		if _, err := e.Svc.RecoveryPoint(bg, wt.Engineer, scope, internal.ID); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Errorf("single recovery point of internal: %v", err)
		}
		if _, err := e.Svc.PinWriterOutput(bg, wt.Engineer, scope, internal.ID, "att_int", "run_1"); err == nil {
			t.Error("a writer output was pinned through the external receipt model")
		}
		if cur := reload(t, e, internal); cur.Writer != nil || cur.Pending != nil {
			t.Fatalf("a refused external operation changed the internal workspace: %+v", cur)
		}
	})

	t.Run("archive and restore work for both models", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		ext := e.Ready(scope, "wi_ext")
		e.Seed(scope, wt.RepoApp)
		internal, _ := e.Provision(scope, "wi_int", wt.RepoApp)
		for _, w := range []*workspace.Workspace{ext, internal} {
			archived, err := e.Svc.Archive(bg, wt.Engineer, scope, w.ID, 0)
			if err != nil || archived.State != workspace.StateArchived {
				t.Fatalf("archive %s: %+v %v", w.ID, archived, err)
			}
			restored, err := e.Svc.Restore(bg, wt.Engineer, scope, w.ID, 0)
			if err != nil || restored.State != workspace.StateAvailable {
				t.Fatalf("restore %s: %+v %v", w.ID, restored, err)
			}
		}
		// An archived internal workspace takes no new repository.
		if _, err := e.Svc.Archive(bg, wt.Engineer, scope, internal.ID, 0); err != nil {
			t.Fatal(err)
		}
		e.Seed(scope, wt.RepoDocs)
		if _, _, _, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_int", []workspace.RepositoryID{wt.RepoApp, wt.RepoDocs}); err != nil {
			// An archived workspace is returned as is: nothing is added to it.
			t.Fatalf("provisioning an archived workspace: %v", err)
		}
		if assocs := associations(t, e, internal); len(assocs) != 1 {
			t.Fatalf("a repository was added to an archived workspace: %+v", assocs)
		}
	})
}
