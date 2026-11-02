package workspace_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// The workspace module is gitpush's Reservations provider; gitpush's own
// service is the module's push-record and writer-gate source. A compile-time
// proof that the shapes agree.
var (
	_ gitpush.Reservations  = (*workspace.Service)(nil)
	_ workspace.PushRecords = (*gitpush.Service)(nil)
	_ workspace.WriterGate  = (*gitpush.Service)(nil)
)

// Test_unit_workspace_single_writer_per_item: WS-03, WS-A02, WS-18. One writer
// reservation per work item, whatever the number of repositories; the reserved
// attempt, and only it, holds the task-branch authority gitpush asks about.
func Test_unit_workspace_single_writer_per_item(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, *workspace.Workspace) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		e.Seed(scope, wt.RepoDocs)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp, wt.RepoDocs)
		return e, w
	}
	holds := func(t *testing.T, e *wt.InternalEnv, item, attempt string) bool {
		t.Helper()
		ok, err := e.Svc.HoldsWriter(bg, scope, item, attempt)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Run("competing attempts yield one reservation", func(t *testing.T) {
		e, w := setup(t)
		const n = 6
		ids := make([]string, n)
		for i := range n {
			ids[i] = "att_c" + string(rune('a'+i))
			e.Attempts.Register(scope, workspaceAttempt(ids[i]), w.ID, workspace.AccessWrite)
		}
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: workspaceAttempt(ids[i])})
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				if !holds(t, e, "wi_1", ids[i]) {
					t.Fatalf("the winner %s does not hold the writer", ids[i])
				}
			case !errors.Is(err, workspace.ErrWriterBusy):
				t.Fatalf("loser %s: %v", ids[i], err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d reservations: %v", winners, errs)
		}
		held := 0
		for _, id := range ids {
			if holds(t, e, "wi_1", id) {
				held++
			}
		}
		if held != 1 {
			t.Fatalf("HoldsWriter is true for %d attempts", held)
		}
		cur := reload(t, e, w)
		if cur.Writer == nil || cur.AcceptedCommit != w.AcceptedCommit {
			t.Fatalf("%+v", cur)
		}
	})

	t.Run("the slot is per work item, repeated by the holder and freed by settlement", func(t *testing.T) {
		e, w := setup(t)
		e.Seed(scope, "repo_other")
		other, _ := e.Provision(scope, "wi_2", "repo_other")
		first := e.InternalWriter(w, "att_1")
		if first.Writer == nil || first.Writer.AttemptID != "att_1" {
			t.Fatalf("%+v", first.Writer)
		}
		// Another work item's workspace has its own slot.
		e.InternalWriter(other, "att_other")
		if !holds(t, e, "wi_1", "att_1") || !holds(t, e, "wi_2", "att_other") || holds(t, e, "wi_1", "att_other") || holds(t, e, "wi_2", "att_1") {
			t.Fatal("reservations crossed work items")
		}
		// The holder repeating its request is idempotent; another attempt is busy.
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1"}); err != nil {
			t.Fatalf("repeat: %v", err)
		}
		e.Attempts.Register(scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_2"}); !errors.Is(err, workspace.ErrWriterBusy) {
			t.Fatalf("second writer: %v", err)
		}
		if holds(t, e, "wi_1", "att_2") {
			t.Fatal("the busy attempt holds the writer")
		}
		// Settlement frees the slot (no single published commit is verified: the
		// commits were accepted one by one from their push records).
		if _, err := e.Svc.SettleWriter(bg, workspace.SettleWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1",
			Outcome: workspace.OutcomeSucceeded, SourceCommit: workspace.Commit(w.AcceptedCommit)}); err != nil {
			t.Fatal(err)
		}
		if holds(t, e, "wi_1", "att_1") {
			t.Fatal("a settled writer still holds the reservation")
		}
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err != nil {
			t.Fatalf("after settlement: %v", err)
		}
		if !holds(t, e, "wi_1", "att_2") || holds(t, e, "wi_1", "att_1") {
			t.Fatal("the slot did not move to the second attempt")
		}
	})

	t.Run("only a live attempt with write access of this workspace is admitted", func(t *testing.T) {
		e, w := setup(t)
		reserve := func(id string) error {
			_, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: workspaceAttempt(id)})
			return err
		}
		e.Attempts.Register(scope, "att_read", w.ID, workspace.AccessRead)
		if err := reserve("att_read"); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("read attempt: %v", err)
		}
		e.Attempts.Register(scope, "att_dead", w.ID, workspace.AccessWrite)
		e.Attempts.Revoke("att_dead")
		if err := reserve("att_dead"); !errors.Is(err, workspace.ErrAttemptRevoked) {
			t.Fatalf("revoked attempt: %v", err)
		}
		e.Attempts.Register(scope, "att_elsewhere", "ws_other", workspace.AccessWrite)
		if err := reserve("att_elsewhere"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("another workspace's attempt: %v", err)
		}
		if err := reserve("att_unknown"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("unknown attempt: %v", err)
		}
		if cur := reload(t, e, w); cur.Writer != nil {
			t.Fatalf("a refused attempt holds the slot: %+v", cur.Writer)
		}
		for _, id := range []string{"att_read", "att_dead", "att_elsewhere"} {
			if holds(t, e, "wi_1", id) {
				t.Fatalf("%s holds the writer", id)
			}
		}
	})

	t.Run("an archived or provisioning workspace admits no writer", func(t *testing.T) {
		e, w := setup(t)
		e.Attempts.Register(scope, "att_1", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.Archive(bg, wt.Engineer, scope, w.ID, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1"}); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("archived: %v", err)
		}
		if holds(t, e, "wi_1", "att_1") {
			t.Fatal("archived workspace holds a writer")
		}
	})

	t.Run("a human fast-forward is adopted at admission and a rewritten branch refuses it", func(t *testing.T) {
		e, w := setup(t)
		a := assoc(t, associations(t, e, w), wt.RepoApp)
		human := e.Inspector.Add("human work", a.AcceptedCommit)
		e.HumanPush(scope, wt.RepoApp, a.TaskBranch, a.AcceptedCommit, human)
		e.Attempts.Register(scope, "att_1", w.ID, workspace.AccessWrite)
		got, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1"})
		if err != nil || got.AcceptedCommit != human {
			t.Fatalf("admission: %+v %v", got, err)
		}
		if after := assoc(t, associations(t, e, w), wt.RepoApp); after.AcceptedCommit != human {
			t.Fatalf("the accepted commit of the association is %s", after.AcceptedCommit)
		}
		if n := checkpointEventCount(t, e, w); n != 1 {
			t.Fatalf("the adoption emitted %d checkpoint events", n)
		}
		evs := e.Recorder.Committed(t, scope.TenantID, "workspace.checkpoint_created")
		if len(evs) != 1 || !jsonHas(evs[0].Data, `"humanSynchronization":true`) || !jsonHas(evs[0].Data, `"repository":"repo_app"`) {
			t.Fatalf("event: %s", evs[0].Data)
		}
		settle(t, e, w, "att_1", workspace.OutcomeFailed)

		// A force-pushed task branch is not adopted: admission is refused.
		rewritten := e.Inspector.Add("rewritten history")
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, rewritten)
		e.Attempts.Register(scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_2"}); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("diverged: %v", err)
		}
		after := assoc(t, associations(t, e, w), wt.RepoApp)
		if after.AcceptedCommit != human || after.LastError != workspace.ReasonBranchDiverged {
			t.Fatalf("%+v", after)
		}
		if holds(t, e, "wi_1", "att_2") {
			t.Fatal("a writer was admitted on a diverged branch")
		}
		// Sync reports it as the external one does.
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("sync: %v", err)
		}
		// Once the branch is restored the reason clears and admission succeeds.
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, human)
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); err != nil {
			t.Fatalf("sync after repair: %v", err)
		}
		if after := assoc(t, associations(t, e, w), wt.RepoApp); after.LastError != "" {
			t.Fatalf("%+v", after)
		}
		// A deleted task branch is recorded, never recreated.
		e.Hosts.DeleteRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch)
		if _, err := e.Svc.Sync(bg, wt.Engineer, scope, w.ID, 0); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("missing branch: %v", err)
		}
		if after := assoc(t, associations(t, e, w), wt.RepoApp); after.LastError != workspace.ReasonBranchMissing || e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), a.TaskBranch) != "" {
			t.Fatalf("%+v", after)
		}
	})

	t.Run("the transaction-joined mutation refuses the other hosting", func(t *testing.T) {
		e, w := setup(t)
		in := workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1", ExpectedTip: w.AcceptedCommit, Actor: wt.ExecutionService}
		if _, err := e.Store.Update(bg, scope, w.ID, workspace.Load{}, workspace.ReserveWriterMutation(in, e.Clock.Now)); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Fatalf("the publisher reservation on an internal workspace: %v", err)
		}
		ext := e.Ready(wt.ScopeA2, "wi_ext")
		in2 := workspace.ReserveWriterInput{Scope: ext.Scope, WorkspaceID: ext.ID, AttemptID: "att_x", Actor: wt.ExecutionService}
		if _, err := e.Store.Update(bg, ext.Scope, ext.ID, workspace.Load{}, workspace.ReserveInternalWriterMutation(in2, ext.Version, e.Clock.Now)); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Fatalf("the internal reservation on an external workspace: %v", err)
		}
		e.Attempts.Register(scope, "att_1", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(bg, wt.Engineer, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_1"}); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Fatalf("Service.ReserveWriter on an internal workspace: %v", err)
		}
	})

	t.Run("unknown or foreign work items hold nothing", func(t *testing.T) {
		e, w := setup(t)
		e.InternalWriter(w, "att_1")
		for _, tc := range []struct{ item, attempt string }{{"wi_none", "att_1"}, {"bad item", "att_1"}, {"wi_1", "bad attempt"}} {
			if holds(t, e, tc.item, tc.attempt) {
				t.Fatalf("%+v holds the writer", tc)
			}
		}
		if ok, err := e.Svc.HoldsWriter(bg, wt.ScopeB, "wi_1", "att_1"); err != nil || ok {
			t.Fatalf("another tenant's question: %v %v", ok, err)
		}
	})
}

// Test_unit_workspace_unknown_push_blocks_reassignment: WS-11, GR-09, WS-A15. An
// unknown push outcome on any task branch of the workspace blocks the next
// writer assignment until it is resolved, and the check fails closed.
func Test_unit_workspace_unknown_push_blocks_reassignment(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, *workspace.Workspace) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		e.Seed(scope, wt.RepoDocs)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp, wt.RepoDocs)
		e.InternalWriter(w, "att_1")
		settle(t, e, w, "att_1", workspace.OutcomeFailed)
		e.Attempts.Register(scope, "att_2", w.ID, workspace.AccessWrite)
		return e, w
	}
	reserve := func(e *wt.InternalEnv, w *workspace.Workspace, attempt string) error {
		_, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: workspaceAttempt(attempt)})
		return err
	}

	t.Run("an unknown entry blocks reassignment until it is resolved", func(t *testing.T) {
		e, w := setup(t)
		e.Gate.Block(scope, string(wt.RepoApp), taskBranch("wi_1"))
		if err := reserve(e, w, "att_2"); !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("got %v", err)
		}
		if ok, _ := e.Svc.HoldsWriter(bg, scope, "wi_1", "att_2"); ok || reload(t, e, w).Writer != nil {
			t.Fatal("the blocked attempt holds the slot")
		}
		if err := e.Svc.CheckWriterGate(bg, scope, w.ID); !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("CheckWriterGate: %v", err)
		}
		// The gate was asked for the task branch of the repository, not for another ref.
		if !contains(e.Gate.Asked, string(wt.RepoApp)+"|"+taskBranch("wi_1")) {
			t.Fatalf("asked %v", e.Gate.Asked)
		}
		e.Gate.Resolve(scope, string(wt.RepoApp), taskBranch("wi_1"))
		if err := reserve(e, w, "att_2"); err != nil {
			t.Fatalf("after resolution: %v", err)
		}
	})

	t.Run("an unknown entry in any associated repository blocks the one writer", func(t *testing.T) {
		e, w := setup(t)
		e.Gate.Block(scope, string(wt.RepoDocs), taskBranch("wi_1"))
		if err := reserve(e, w, "att_2"); !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("got %v", err)
		}
		// Another work item's branches are not affected.
		e.Seed(scope, "repo_other")
		other, _ := e.Provision(scope, "wi_2", "repo_other")
		e.Attempts.Register(scope, "att_o", other.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: other.ID, AttemptID: "att_o"}); err != nil {
			t.Fatalf("an unrelated workspace was blocked: %v", err)
		}
	})

	t.Run("the check fails closed", func(t *testing.T) {
		e, w := setup(t)
		e.Gate.Err = errors.New("push store down")
		if err := reserve(e, w, "att_2"); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("gate outage: %v", err)
		}
		if reload(t, e, w).Writer != nil {
			t.Fatal("a writer was assigned without the gate's answer")
		}
		// A service without a gate admits no internal writer at all.
		noGate, err := workspace.NewService(workspace.Dependencies{Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos,
			Attempts: e.Attempts, Bundles: e.Bundles, Secrets: e.Secrets, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(),
			Hosts: e.Hosts, InternalRepos: e.IRepos, Inspector: e.Inspector, Clock: e.Clock.Now}, wt.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := noGate.ReserveInternalWriter(bg, wt.ExecutionService, workspace.ReserveWriterInput{Scope: scope, WorkspaceID: w.ID, AttemptID: "att_2"}); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("no gate: %v", err)
		}
	})

	t.Run("the holder repeating its request is not a reassignment", func(t *testing.T) {
		e, w := setup(t)
		if err := reserve(e, w, "att_2"); err != nil {
			t.Fatal(err)
		}
		asked := len(e.Gate.Asked)
		e.Gate.Block(scope, string(wt.RepoApp), taskBranch("wi_1")) // an unknown outcome arises while att_2 is writing
		if err := reserve(e, w, "att_2"); err != nil {
			t.Fatalf("the holder's repeat: %v", err)
		}
		if len(e.Gate.Asked) != asked {
			t.Fatal("the gate was asked for the holder's own repeat")
		}
		// But releasing it and assigning another attempt is blocked.
		settle(t, e, w, "att_2", workspace.OutcomeFailed)
		e.Attempts.Register(scope, "att_3", w.ID, workspace.AccessWrite)
		if err := reserve(e, w, "att_3"); !errors.Is(err, workspace.ErrWriterBlocked) {
			t.Fatalf("reassignment: %v", err)
		}
	})

	t.Run("an unsettled push keeps the commit output pending, not accepted", func(t *testing.T) {
		e, w := setup(t)
		if err := reserve(e, w, "att_2"); err != nil {
			t.Fatal(err)
		}
		a := assoc(t, associations(t, e, w), wt.RepoApp)
		c1 := e.Inspector.Add("feature", a.AcceptedCommit)
		push := e.Push(scope, wt.RepoApp, "att_2", "run_1", wt.PushEntry{Ref: a.TaskBranch, Old: a.AcceptedCommit, New: c1, State: "unknown", Kind: "fast_forward"})
		if _, err := accept(e, w, "att_2", wt.RepoApp, a.TaskBranch, c1, push); !errors.Is(err, workspace.ErrPushPending) {
			t.Fatalf("got %v", err)
		}
		if got := assoc(t, associations(t, e, w), wt.RepoApp); got.AcceptedCommit != a.AcceptedCommit {
			t.Fatalf("an unknown push moved the accepted commit: %+v", got)
		}
	})
}
