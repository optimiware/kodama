package workspace_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

func reserveFor(e *wt.InternalEnv, w *workspace.Workspace, attempt string) error {
	_, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: workspaceAttempt(attempt)})
	return err
}

// Test_unit_workspace_sync_adopts_only_explained_human_work: a fast-forward of
// a task branch is adopted as human work only when durable push evidence shows
// every step was an engineer's; a workload push that was refused, never
// reported or is still in flight is never adopted by synchronization or by
// writer admission (WS-09, WS-19, GR-09).
func Test_unit_workspace_sync_adopts_only_explained_human_work(t *testing.T) {
	scope := wt.ScopeA
	const own = "own-injected-secret-1234"

	t.Run("a refused secret push is not adopted by sync or reassignment", func(t *testing.T) {
		f := newWriter(t)
		f.e.Secrets.Set("att_1", own)
		secret := f.e.Inspector.Add("leaks "+own, f.app.AcceptedCommit)
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, secret)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, secret, push); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("accept: %v", err)
		}
		// While the writer still holds the slot its push is in progress: pending, nothing recorded.
		if got, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrPushPending) || got == nil || got.AcceptedCommit != f.app.AcceptedCommit {
			t.Fatalf("sync while the writer holds: %+v %v", got, err)
		}
		if after := assoc(t, associations(t, f.e, f.w), wt.RepoApp); after.LastError != "" {
			t.Fatalf("a writer's own push was recorded as a failure: %+v", after)
		}
		settle(t, f.e, f.w, "att_1", workspace.OutcomeFailed)
		got, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0)
		if !errors.Is(err, workspace.ErrCheckpointRequired) {
			t.Fatalf("sync adopted or ignored the refused push: %v", err)
		}
		if got == nil || got.AcceptedCommit != f.app.AcceptedCommit {
			t.Fatalf("workspace after sync: %+v", got)
		}
		after := assoc(t, associations(t, f.e, f.w), wt.RepoApp)
		if after.AcceptedCommit != f.app.AcceptedCommit || after.LastError != workspace.ReasonUnacceptedPush {
			t.Fatalf("%+v", after)
		}
		if checkpointEventCount(t, f.e, f.w) != 0 {
			t.Fatal("a checkpoint event was emitted for the refused push")
		}
		// Reassignment refuses too; nothing is admitted on the unaccepted tip.
		f.e.Attempts.Register(scope, "att_2", f.w.ID, workspace.AccessWrite)
		if err := reserveFor(f.e, f.w, "att_2"); !errors.Is(err, workspace.ErrCheckpointRequired) {
			t.Fatalf("reassignment: %v", err)
		}
		if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_2"); ok || reload(t, f.e, f.w).Writer != nil {
			t.Fatal("a writer was admitted on the unaccepted tip")
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("accepted commit %s", got)
		}
		// An engineer resets the branch to the accepted commit: the reason clears, admission succeeds.
		f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), f.ref, f.app.AcceptedCommit)
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); err != nil {
			t.Fatalf("sync after the reset: %v", err)
		}
		if err := reserveFor(f.e, f.w, "att_2"); err != nil {
			t.Fatalf("admission after the reset: %v", err)
		}
		if after := assoc(t, associations(t, f.e, f.w), wt.RepoApp); after.LastError != "" {
			t.Fatalf("%+v", after)
		}
	})

	t.Run("a human push on top of an unaccepted workload push is not adopted", func(t *testing.T) {
		f := newWriter(t)
		w1 := f.e.Inspector.Add("workload", f.app.AcceptedCommit)
		pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, w1) // never reported as a commit output
		h := f.e.Inspector.Add("human on top", w1)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, w1, h)
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrPushPending) {
			t.Fatalf("sync while the writer holds: %v", err)
		}
		settle(t, f.e, f.w, "att_1", workspace.OutcomeFailed) // the attempt that made it is gone
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrCheckpointRequired) {
			t.Fatalf("sync: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("adopted %s", got)
		}
	})

	t.Run("a fast-forward with no push record is not adopted", func(t *testing.T) {
		f := newWriter(t)
		c := f.e.Inspector.Add("unexplained", f.app.AcceptedCommit)
		f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), f.ref, c)
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrCheckpointRequired) {
			t.Fatalf("sync: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("adopted %s", got)
		}
	})

	t.Run("an unconfirmed push is pending: sync and admission refuse", func(t *testing.T) {
		for _, state := range []gitv1.EntryState{gitv1.EntryAuthorized, gitv1.EntryCommitting, gitv1.EntryUnknown} {
			f := newWriter(t)
			c := f.e.Inspector.Add("in flight", f.app.AcceptedCommit)
			f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), f.ref, c)
			f.e.Push(scope, wt.RepoApp, "att_1", "run_1", wt.PushEntry{Ref: f.ref, Old: f.app.AcceptedCommit, New: c, State: state, Kind: gitv1.KindFastForward})
			if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrPushPending) {
				t.Fatalf("%s: sync: %v", state, err)
			}
			if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
				t.Fatalf("%s: adopted %s", state, got)
			}
			if after := assoc(t, associations(t, f.e, f.w), wt.RepoApp); after.LastError != "" {
				t.Fatalf("%s: a pending push was recorded as a failure: %+v", state, after)
			}
			settle(t, f.e, f.w, "att_1", workspace.OutcomeFailed)
			f.e.Attempts.Register(scope, "att_2", f.w.ID, workspace.AccessWrite)
			err := reserveFor(f.e, f.w, "att_2")
			if !errors.Is(err, workspace.ErrWriterBlocked) || (state != gitv1.EntryUnknown && !errors.Is(err, workspace.ErrPushPending)) {
				t.Fatalf("%s: reassignment: %v", state, err)
			}
			if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_2"); ok {
				t.Fatalf("%s: admitted", state)
			}
		}
	})

	t.Run("human pushes chained from the accepted commit are adopted once", func(t *testing.T) {
		f := newWriter(t)
		h1 := f.e.Inspector.Add("human 1", f.app.AcceptedCommit)
		h2 := f.e.Inspector.Add("human 2", h1)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, f.app.AcceptedCommit, h1)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, h1, h2)
		got, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0)
		if err != nil || got.AcceptedCommit != h2 || f.accepted(t, wt.RepoApp) != h2 {
			t.Fatalf("sync: %+v %v", got, err)
		}
		if checkpointEventCount(t, f.e, f.w) != 1 {
			t.Fatalf("events %d", checkpointEventCount(t, f.e, f.w))
		}
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); err != nil || checkpointEventCount(t, f.e, f.w) != 1 {
			t.Fatalf("a repeated sync is not a no-op: %v", err)
		}
	})

	t.Run("a human push on top of an accepted checkpoint is adopted", func(t *testing.T) {
		f := newWriter(t)
		c := f.e.Inspector.Add("workload", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); err != nil {
			t.Fatal(err)
		}
		h := f.e.Inspector.Add("human", c)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, c, h)
		if got, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); err != nil || got.AcceptedCommit != h {
			t.Fatalf("sync: %+v %v", got, err)
		}
	})

	t.Run("an outage of the push records fails closed", func(t *testing.T) {
		f := newWriter(t)
		h := f.e.Inspector.Add("human", f.app.AcceptedCommit)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, f.app.AcceptedCommit, h)
		f.e.Pushes.RefErr = errors.New("push store down")
		if _, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("sync: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("adopted %s without evidence", got)
		}
		f.e.Pushes.RefErr = nil
		if got, err := f.e.Svc.Sync(bg, wt.Engineer, scope, f.w.ID, 0); err != nil || got.AcceptedCommit != h {
			t.Fatalf("sync after the outage: %+v %v", got, err)
		}
	})
}

// Test_unit_workspace_archive_closes_writer_authority: closing admission ends
// the writer's push authority, and archive does not complete while a push of a
// created task branch is unsettled.
func Test_unit_workspace_archive_closes_writer_authority(t *testing.T) {
	scope := wt.ScopeA

	t.Run("an archived workspace's writer no longer holds it", func(t *testing.T) {
		f := newWriter(t)
		if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_1"); !ok {
			t.Fatal("the writer does not hold before archive")
		}
		got, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0)
		if err != nil || got.State != workspace.StateArchived {
			t.Fatalf("archive: %+v %v", got, err)
		}
		if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_1"); ok {
			t.Fatal("the writer of an archived workspace still holds the reservation")
		}
		// Restore reopens admission: the same writer's reservation is live again.
		if _, err := f.e.Svc.Restore(bg, wt.Engineer, scope, f.w.ID, 0); err != nil {
			t.Fatal(err)
		}
		if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_1"); !ok {
			t.Fatal("the reservation did not survive restore")
		}
	})

	t.Run("archive waits for an unsettled push, and the writer cannot push meanwhile", func(t *testing.T) {
		f := newWriter(t)
		c := f.e.Inspector.Add("in flight", f.app.AcceptedCommit)
		push := f.e.Push(scope, wt.RepoApp, "att_1", "run_1", wt.PushEntry{Ref: f.ref, Old: f.app.AcceptedCommit, New: c, State: gitv1.EntryCommitting, Kind: gitv1.KindFastForward})
		got, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0)
		if !errors.Is(err, workspace.ErrArchiveUncertain) || got == nil || got.State != workspace.StateAvailable || got.ArchiveRequestedAt == nil {
			t.Fatalf("archive over an unsettled push: %+v %v", got, err)
		}
		if ok, _ := f.e.Svc.HoldsWriter(bg, scope, "wi_1", "att_1"); ok {
			t.Fatal("admission is closed but the writer still holds")
		}
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, push); err == nil {
			t.Fatal("a checkpoint was accepted after admission closed")
		}
		// The reconciler does not complete it either.
		if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil {
			t.Fatal(err)
		}
		if cur := reload(t, f.e, f.w); cur.State != workspace.StateAvailable {
			t.Fatalf("archived with an unsettled push: %s", cur.State)
		}
		// An unknown outcome waits for the operator's resolution.
		f.e.Pushes.SetEntryState(scope, string(wt.RepoApp), push, f.ref, gitv1.EntryUnknown)
		if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil || reload(t, f.e, f.w).State != workspace.StateAvailable {
			t.Fatalf("archived over an unknown push: %v", err)
		}
		f.e.Pushes.SetEntryState(scope, string(wt.RepoApp), push, f.ref, gitv1.EntryRefused)
		if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil {
			t.Fatal(err)
		}
		if cur := reload(t, f.e, f.w); cur.State != workspace.StateArchived {
			t.Fatalf("not archived after the push settled: %s", cur.State)
		}
	})

	t.Run("an unsettled push of another repository's branch also holds the archive", func(t *testing.T) {
		f := newWriter(t)
		d := f.e.Inspector.Add("docs", f.doc.AcceptedCommit)
		f.e.Push(scope, wt.RepoDocs, "att_1", "run_1", wt.PushEntry{Ref: f.ref, Old: f.doc.AcceptedCommit, New: d, State: gitv1.EntryAuthorized, Kind: gitv1.KindFastForward})
		if _, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrArchiveUncertain) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the push records failing keeps the archive requested", func(t *testing.T) {
		f := newWriter(t)
		f.e.Pushes.RefErr = errors.New("push store down")
		if _, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if cur := reload(t, f.e, f.w); cur.State != workspace.StateAvailable || cur.ArchiveRequestedAt == nil {
			t.Fatalf("%+v", cur)
		}
		f.e.Pushes.RefErr = nil
		if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil || reload(t, f.e, f.w).State != workspace.StateArchived {
			t.Fatalf("reconcile: %v", err)
		}
	})
}

// Test_unit_workspace_admission_is_bound_to_the_workspace_version: a writer is
// never admitted against a gate answer that predates another writer's push.
func Test_unit_workspace_admission_is_bound_to_the_workspace_version(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, *workspace.Workspace) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		e.Seed(scope, wt.RepoDocs)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp, wt.RepoDocs)
		e.Attempts.Register(scope, "att_a", w.ID, workspace.AccessWrite)
		e.Attempts.Register(scope, "att_b", w.ID, workspace.AccessWrite)
		return e, w
	}
	// interleave is B's whole life between A's last answer and A's reservation:
	// B is admitted, pushes something that is not settled, and is released. It
	// runs once, after the answer for the docs repository, which A asks last.
	interleave := func(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace, state gitv1.EntryState, disarm func()) func(repo, ref string) {
		return func(repo, _ string) {
			if repo != string(wt.RepoDocs) {
				return
			}
			disarm()
			if err := reserveFor(e, w, "att_b"); err != nil {
				t.Errorf("B admission: %v", err)
				return
			}
			a := assoc(t, associations(t, e, w), wt.RepoApp)
			c := e.Inspector.Add("b's push", a.AcceptedCommit)
			e.Push(scope, wt.RepoApp, "att_b", "run_b", wt.PushEntry{Ref: a.TaskBranch, Old: a.AcceptedCommit, New: c, State: state, Kind: gitv1.KindFastForward})
			settle(t, e, w, "att_b", workspace.OutcomeFailed)
		}
	}

	t.Run("an unknown push between the gate and the reservation refuses A", func(t *testing.T) {
		e, w := setup(t)
		e.Gate.AfterAsk = interleave(t, e, w, gitv1.EntryUnknown, func() { e.Gate.AfterAsk = nil })
		err := reserveFor(e, w, "att_a")
		if !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("A was admitted against a stale gate: %v", err)
		}
		if cur := reload(t, e, w); cur.Writer != nil {
			t.Fatalf("writer %+v", cur.Writer)
		}
		if ok, _ := e.Svc.HoldsWriter(bg, scope, "wi_1", "att_a"); ok {
			t.Fatal("A holds the reservation")
		}
	})

	t.Run("an in-flight push between the question and the reservation refuses A", func(t *testing.T) {
		e, w := setup(t)
		e.Pushes.AfterUnsettled = interleave(t, e, w, gitv1.EntryCommitting, func() { e.Pushes.AfterUnsettled = nil })
		err := reserveFor(e, w, "att_a")
		if !errors.Is(err, workspace.ErrPushPending) || !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("A was admitted against a stale answer: %v", err)
		}
		if cur := reload(t, e, w); cur.Writer != nil {
			t.Fatalf("writer %+v", cur.Writer)
		}
	})

	t.Run("a harmless interleaving is repeated and then admits A", func(t *testing.T) {
		e, w := setup(t)
		e.Gate.AfterAsk = func(string, string) {
			e.Gate.AfterAsk = nil
			if err := reserveFor(e, w, "att_b"); err != nil {
				t.Errorf("B: %v", err)
			}
			settle(t, e, w, "att_b", workspace.OutcomeFailed)
		}
		if err := reserveFor(e, w, "att_a"); err != nil {
			t.Fatalf("A: %v", err)
		}
		if ok, _ := e.Svc.HoldsWriter(bg, scope, "wi_1", "att_a"); !ok {
			t.Fatal("A does not hold")
		}
	})

	t.Run("the joined mutation refuses a stale or unbound version", func(t *testing.T) {
		e, w := setup(t)
		observed, err := e.Svc.CheckWriterGateAt(bg, scope, w.ID)
		if err != nil || observed != w.Version {
			t.Fatalf("gate at: %d %v", observed, err)
		}
		in := workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_a", Actor: wt.ExecutionService}
		if _, err := e.Store.Update(bg, scope, w.ID, workspace.Load{}, workspace.ReserveInternalWriterMutation(in, 0, e.Clock.Now)); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("unbound: %v", err)
		}
		if err := reserveFor(e, w, "att_b"); err != nil { // the workspace changes after the gate
			t.Fatal(err)
		}
		settle(t, e, w, "att_b", workspace.OutcomeFailed)
		if _, err := e.Store.Update(bg, scope, w.ID, workspace.Load{}, workspace.ReserveInternalWriterMutation(in, observed, e.Clock.Now)); !errors.Is(err, workspace.ErrStale) {
			t.Fatalf("stale: %v", err)
		}
		cur := reload(t, e, w)
		if cur.Writer != nil {
			t.Fatalf("writer %+v", cur.Writer)
		}
		fresh, err := e.Svc.CheckWriterGateAt(bg, scope, w.ID)
		if err != nil || fresh != cur.Version {
			t.Fatalf("%d %v", fresh, err)
		}
		if _, err := e.Store.Update(bg, scope, w.ID, workspace.Load{}, workspace.ReserveInternalWriterMutation(in, fresh, e.Clock.Now)); err != nil {
			t.Fatalf("fresh: %v", err)
		}
	})

	t.Run("a service without push records by ref admits no internal writer", func(t *testing.T) {
		e, w := setup(t)
		svc, err := workspace.NewService(workspace.Dependencies{Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos,
			Attempts: e.Attempts, Bundles: e.Bundles, Secrets: e.Secrets, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(),
			Hosts: e.Hosts, InternalRepos: e.IRepos, Gate: e.Gate, Inspector: e.Inspector, Clock: e.Clock.Now}, wt.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_a"}); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})
}

// Test_unit_workspace_concurrent_provisioning_adds_every_repository: the
// request that loses the insert of the workspace still gets its own
// repositories associated and created.
func Test_unit_workspace_concurrent_provisioning_adds_every_repository(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) *wt.InternalEnv {
		e := wt.NewInternalEnv(t)
		for _, r := range []workspace.RepositoryID{wt.RepoApp, wt.RepoDocs, "repo_other"} {
			e.Seed(scope, r)
		}
		e.Items.Add(scope, "wi_1", "story")
		return e
	}
	provision := func(e *wt.InternalEnv, repos ...workspace.RepositoryID) (*workspace.Workspace, []*workspace.RepositoryAssociation, bool, error) {
		return e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", repos)
	}

	t.Run("the loser of the insert adds its disjoint repository", func(t *testing.T) {
		e := setup(t)
		var winner *workspace.Workspace
		e.Hosts.BeforeRead = func(repo, ref string) {
			if repo != string(wt.RepoDocs) {
				e.Hosts.BeforeRead = func(string, string) {}
				return
			}
			e.Hosts.BeforeRead = nil
			var err error
			if winner, _, _, err = provision(e, wt.RepoApp); err != nil {
				t.Errorf("winner: %v", err)
			}
		}
		w, assocs, created, err := provision(e, wt.RepoDocs)
		if err != nil || created || winner == nil || w.ID != winner.ID {
			t.Fatalf("loser: %+v created=%v %v (winner %+v)", w, created, err, winner)
		}
		for _, r := range []workspace.RepositoryID{wt.RepoApp, wt.RepoDocs} {
			if a := assoc(t, assocs, r); a.State != workspace.AssociationCreated {
				t.Fatalf("%s: %+v", r, a)
			}
		}
		if got := reload(t, e, w); got.State != workspace.StateAvailable || got.RepositoryID != wt.RepoApp {
			t.Fatalf("%+v", got)
		}
		for _, r := range []workspace.RepositoryID{wt.RepoApp, wt.RepoDocs} {
			if e.Hosts.Ref(scope.TenantID, string(r), taskBranch("wi_1")) == "" {
				t.Fatalf("no task branch in %s", r)
			}
		}
	})

	t.Run("the loser of the insert with overlapping repositories creates nothing twice", func(t *testing.T) {
		e := setup(t)
		e.Hosts.BeforeRead = func(repo, ref string) {
			if repo != string(wt.RepoApp) {
				e.Hosts.BeforeRead = func(string, string) {}
				return
			}
			e.Hosts.BeforeRead = nil
			if _, _, _, err := provision(e, wt.RepoApp, wt.RepoDocs); err != nil {
				t.Errorf("winner: %v", err)
			}
		}
		w, assocs, created, err := provision(e, wt.RepoApp, "repo_other")
		if err != nil || created || len(assocs) != 3 {
			t.Fatalf("loser: %+v created=%v %d %v", w, created, len(assocs), err)
		}
		if got := distinct(e.Hosts.CreateOps); len(got) != 3 || len(e.Hosts.CreateOps) != 3 {
			t.Fatalf("branch operations: %v", e.Hosts.CreateOps)
		}
	})

	t.Run("many concurrent requests with overlapping and disjoint sets all succeed", func(t *testing.T) {
		e := setup(t)
		sets := [][]workspace.RepositoryID{{wt.RepoApp, wt.RepoDocs}, {wt.RepoDocs, "repo_other"}, {"repo_other"}, {wt.RepoApp}, {wt.RepoDocs}, {"repo_other", wt.RepoApp}}
		var wg sync.WaitGroup
		start := make(chan struct{})
		createdBy := make([]bool, len(sets))
		errs := make([]error, len(sets))
		outs := make([][]*workspace.RepositoryAssociation, len(sets))
		for i, set := range sets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, outs[i], createdBy[i], errs[i] = provision(e, set...)
			}()
		}
		close(start)
		wg.Wait()
		n := 0
		for i, err := range errs {
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			if createdBy[i] {
				n++
			}
			for _, r := range sets[i] {
				if a := workspace.FindAssociation(outs[i], r); a == nil || a.State != workspace.AssociationCreated {
					t.Fatalf("request %d: %s is %+v", i, r, a)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d requests created the workspace", n)
		}
		w, _ := e.Store.GetByWorkItem(bg, scope, "wi_1")
		list := associations(t, e, w)
		if len(list) != 3 || w.State != workspace.StateAvailable {
			t.Fatalf("%d associations, state %s", len(list), w.State)
		}
		if got := distinct(e.Hosts.CreateOps); len(got) != 3 {
			t.Fatalf("branch operations: %v", e.Hosts.CreateOps)
		}
	})
}

// Test_unit_workspace_sync_applies_in_one_guarded_mutation: what Sync observed
// is applied only if the workspace is still available, If-Match still holds and
// the accepted commits are still the ones it observed against.
func Test_unit_workspace_sync_applies_in_one_guarded_mutation(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, *workspace.Workspace, *workspace.RepositoryAssociation, workspace.Commit) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		e.Seed(scope, wt.RepoDocs)
		w, assocs := e.Provision(scope, "wi_1", wt.RepoApp, wt.RepoDocs)
		a := assoc(t, assocs, wt.RepoApp)
		h := e.Inspector.Add("human", a.AcceptedCommit)
		e.HumanPush(scope, wt.RepoApp, a.TaskBranch, a.AcceptedCommit, h)
		return e, w, a, h
	}

	t.Run("an archive that completes during the observation wins", func(t *testing.T) {
		e, w, a, _ := setup(t)
		e.Hosts.BeforeRead = func(string, string) {
			if _, err := e.Svc.Archive(bg, wt.Engineer, scope, w.ID, 0); err != nil {
				t.Errorf("archive: %v", err)
			}
		}
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("sync: %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.AcceptedCommit != a.AcceptedCommit {
			t.Fatalf("an archived workspace adopted %s", got.AcceptedCommit)
		}
		if checkpointEventCount(t, e, w) != 0 {
			t.Fatal("an event was emitted after the archive")
		}
	})

	t.Run("another update after the observation fails If-Match and adopts nothing", func(t *testing.T) {
		e, w, a, _ := setup(t)
		e.Seed(scope, "repo_other")
		e.Hosts.BeforeRead = func(string, string) { // a repository is added to the workspace: its version moves
			if _, _, _, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", []workspace.RepositoryID{wt.RepoApp, "repo_other"}); err != nil {
				t.Errorf("provision: %v", err)
			}
		}
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, w.Version); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatalf("sync: %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.AcceptedCommit != a.AcceptedCommit {
			t.Fatalf("adopted %s past a stale If-Match", got.AcceptedCommit)
		}
	})

	t.Run("an accepted commit that moved after the observation is observed again", func(t *testing.T) {
		e, w, a, h := setup(t)
		// A sync adopts h concurrently, after this one read the association.
		e.Hosts.BeforeRead = func(string, string) {
			if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil {
				t.Errorf("concurrent sync: %v", err)
			}
		}
		if got, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil || got == nil {
			t.Fatalf("sync: %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.AcceptedCommit != h || got.LastError != "" {
			t.Fatalf("%+v (base %s)", got, a.AcceptedCommit)
		}
		if n := checkpointEventCount(t, e, w); n != 1 {
			t.Fatalf("the adoption was emitted %d times", n)
		}
	})
}

// Test_unit_workspace_pending_archive_accepts_only_the_reserved_writer: a push
// authorized before the archive request and applied during its wait is still
// accepted from the reserved writer; nobody else's, and nothing once the
// archive completed.
func Test_unit_workspace_pending_archive_accepts_only_the_reserved_writer(t *testing.T) {
	scope := wt.ScopeA
	f := newWriter(t)
	c := f.e.Inspector.Add("authorized before the archive", f.app.AcceptedCommit)
	push := f.e.Push(scope, wt.RepoApp, "att_1", "run_1", wt.PushEntry{Ref: f.ref, Old: f.app.AcceptedCommit, New: c, State: gitv1.EntryCommitting, Kind: gitv1.KindFastForward})
	if _, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0); !errors.Is(err, workspace.ErrArchiveUncertain) {
		t.Fatalf("archive: %v", err)
	}
	if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, push); !errors.Is(err, workspace.ErrPushPending) {
		t.Fatalf("an in-flight push during the archive wait: %v", err)
	}
	// The push applies during the wait.
	f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), f.ref, c)
	f.e.Pushes.SetEntryState(scope, string(wt.RepoApp), push, f.ref, gitv1.EntryApplied)

	// Another attempt of the workspace, even with write access, is refused.
	f.e.Attempts.Register(scope, "att_2", f.w.ID, workspace.AccessWrite)
	if _, err := accept(f.e, f.w, "att_2", wt.RepoApp, f.ref, c, push); !errors.Is(err, workspace.ErrAdmissionClosed) {
		t.Fatalf("another attempt: %v", err)
	}
	if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
		t.Fatalf("moved to %s", got)
	}
	// The reserved writer's applied push is accepted and pinned.
	res, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, push)
	if err != nil || !res.Advanced || f.accepted(t, wt.RepoApp) != c {
		t.Fatalf("the writer's push during the archive wait: %+v %v", res, err)
	}
	if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil {
		t.Fatal(err)
	}
	if cur := reload(t, f.e, f.w); cur.State != workspace.StateArchived || cur.AcceptedCommit != c {
		t.Fatalf("%+v", cur)
	}
	// Nothing is accepted once the archive completed, not even a replay by the writer's other push.
	d := f.e.Inspector.Add("too late", c)
	late := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, c, d)
	if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, d, late); !errors.Is(err, workspace.ErrAdmissionClosed) {
		t.Fatalf("after the archive completed: %v", err)
	}
}

// Test_unit_workspace_push_record_outage_is_retryable: a failure of the push
// records is unavailable (retryable), never a writer block.
func Test_unit_workspace_push_record_outage_is_retryable(t *testing.T) {
	scope := wt.ScopeA
	e := wt.NewInternalEnv(t)
	e.Seed(scope, wt.RepoApp)
	w, _ := e.Provision(scope, "wi_1", wt.RepoApp)
	e.Attempts.Register(scope, "att_a", w.ID, workspace.AccessWrite)
	e.Pushes.RefErr = errors.New("push store down")
	for name, err := range map[string]error{
		"reserve":   reserveFor(e, w, "att_a"),
		"gate":      e.Svc.CheckWriterGate(bg, scope, w.ID),
		"reserveOf": second2(e.Svc.CheckWriterGateAt(bg, scope, w.ID)),
	} {
		if !errors.Is(err, workspace.ErrUnavailable) || errors.Is(err, workspace.ErrWriterBlocked) || errors.Is(err, workspace.ErrPushPending) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A pending push is a block.
	e.Pushes.RefErr = nil
	a := assoc(t, associations(t, e, w), wt.RepoApp)
	c := e.Inspector.Add("x", a.AcceptedCommit)
	e.Push(scope, wt.RepoApp, "att_old", "run_old", wt.PushEntry{Ref: a.TaskBranch, Old: a.AcceptedCommit, New: c, State: gitv1.EntryAuthorized, Kind: gitv1.KindFastForward})
	if err := reserveFor(e, w, "att_a"); !errors.Is(err, workspace.ErrWriterBlocked) || !errors.Is(err, workspace.ErrPushPending) {
		t.Fatalf("pending: %v", err)
	}
}

func second2[T any](_ T, err error) error { return err }

// Test_unit_workspace_sync_chain_walk_fails_closed pins the edges of the
// provenance walk: each of these is refused, never adopted.
func Test_unit_workspace_sync_chain_walk_fails_closed(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, *workspace.Workspace, *workspace.RepositoryAssociation) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		w, assocs := e.Provision(scope, "wi_1", wt.RepoApp)
		return e, w, assoc(t, assocs, wt.RepoApp)
	}
	record := func(e *wt.InternalEnv, id string, actor gitv1.Actor, entries ...gitv1.PushEntry) {
		rec := gitv1.PushRecord{PushID: id, RepositoryID: string(wt.RepoApp), ReceiveID: "rcv_" + id, GrantID: "grt_x", Actor: actor,
			Entries: entries, CreatedAt: gitv1.Timestamp("2026-10-19T10:00:00Z")}
		rec.Status = gitv1.DeriveStatus(entries)
		e.Pushes.Put(scope, rec)
	}
	user := gitv1.Actor{Type: gitv1.ActorUser, ID: string(wt.Engineer.ID)}
	entry := func(ref string, old, new workspace.Commit, kind gitv1.UpdateKind) gitv1.PushEntry {
		return gitv1.PushEntry{Ref: ref, Old: string(old), New: string(new), Kind: kind, State: gitv1.EntryApplied}
	}
	refuses := func(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace, a *workspace.RepositoryAssociation) {
		t.Helper()
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); !errors.Is(err, workspace.ErrCheckpointRequired) {
			t.Fatalf("sync: %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.AcceptedCommit != a.AcceptedCommit || got.LastError != workspace.ReasonUnacceptedPush {
			t.Fatalf("%+v", got)
		}
		if checkpointEventCount(t, e, w) != 0 {
			t.Fatal("an event was emitted")
		}
	}

	t.Run("a chain longer than the records read is never adopted", func(t *testing.T) {
		e, w, a := setup(t)
		prev := a.AcceptedCommit
		for range 300 {
			next := e.Inspector.Add("step", prev)
			e.HumanPush(scope, wt.RepoApp, a.TaskBranch, prev, next)
			prev = next
		}
		refuses(t, e, w, a)
	})

	t.Run("a chain just within the records read is adopted", func(t *testing.T) {
		e, w, a := setup(t)
		prev := a.AcceptedCommit
		for range 200 {
			next := e.Inspector.Add("step", prev)
			e.HumanPush(scope, wt.RepoApp, a.TaskBranch, prev, next)
			prev = next
		}
		if got, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil || got.AcceptedCommit != prev {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("an unknown actor type explains nothing", func(t *testing.T) {
		e, w, a := setup(t)
		c := e.Inspector.Add("by a robot", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, c)
		record(e, "push_robot", gitv1.Actor{Type: "robot", ID: "r1"}, entry(a.TaskBranch, a.AcceptedCommit, c, gitv1.KindFastForward))
		refuses(t, e, w, a)
	})

	t.Run("a recreated branch (all-zero old) is not a fast-forward chain", func(t *testing.T) {
		e, w, a := setup(t)
		c := e.Inspector.Add("recreated", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, c)
		record(e, "push_recreate", user, entry(a.TaskBranch, workspace.Commit(strings.Repeat("0", 40)), c, gitv1.KindCreate))
		refuses(t, e, w, a)
	})

	t.Run("a force-push over an unaccepted workload push walks through the old tip", func(t *testing.T) {
		e, w, a := setup(t)
		w1 := e.Inspector.Add("workload", a.AcceptedCommit)
		pushed(e, w, "att_gone", "run_1", wt.RepoApp, a.AcceptedCommit, w1)
		h := e.Inspector.Add("clean rewrite", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, h)
		record(e, "push_force", user, entry(a.TaskBranch, w1, h, gitv1.KindRewrite))
		refuses(t, e, w, a)
		// Only an exact reset to the accepted commit clears it.
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, a.AcceptedCommit)
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil {
			t.Fatalf("after the reset: %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.LastError != "" {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("a force-push whose old value no record explains is not adopted", func(t *testing.T) {
		e, w, a := setup(t)
		ghost := e.Inspector.Add("never recorded", a.AcceptedCommit)
		h := e.Inspector.Add("rewrite", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, h)
		record(e, "push_force2", user, entry(a.TaskBranch, ghost, h, gitv1.KindRewrite))
		refuses(t, e, w, a)
	})

	t.Run("a record with several entries counts only its applied entry of the task branch", func(t *testing.T) {
		e, w, a := setup(t)
		h := e.Inspector.Add("human", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, h)
		other := gitv1.PushEntry{Ref: "refs/heads/other", Old: strings.Repeat("0", 40), New: string(h), Kind: gitv1.KindCreate, State: gitv1.EntryApplied}
		refusedSame := entry(a.TaskBranch, a.AcceptedCommit, e.Inspector.Add("refused", a.AcceptedCommit), gitv1.KindFastForward)
		refusedSame.State, refusedSame.Reason = gitv1.EntryRefused, ptr("no_commit_intent")
		record(e, "push_multi", user, other, entry(a.TaskBranch, a.AcceptedCommit, h, gitv1.KindFastForward), refusedSame)
		if got, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil || got.AcceptedCommit != h {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("a record whose only entry for the task branch is not applied explains nothing", func(t *testing.T) {
		e, w, a := setup(t)
		h := e.Inspector.Add("elsewhere", a.AcceptedCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, h)
		refused := entry(a.TaskBranch, a.AcceptedCommit, h, gitv1.KindFastForward)
		refused.State, refused.Reason = gitv1.EntryRefused, ptr("no_commit_intent")
		other := gitv1.PushEntry{Ref: "refs/heads/other", Old: strings.Repeat("0", 40), New: string(h), Kind: gitv1.KindCreate, State: gitv1.EntryApplied}
		record(e, "push_mixed", user, other, refused)
		refuses(t, e, w, a)
	})
}

func ptr[T any](v T) *T { return &v }
