package workspace_test

import (
	"errors"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// Test_unit_workspace_repository_associations: WS-17, WS-A13. A work item that
// touches two internal repositories gets one task branch in each, recorded per
// repository with its base and accepted commit; the one-workspace rule, tenant
// scoping and the hosting rules hold.
func Test_unit_workspace_repository_associations(t *testing.T) {
	scope := wt.ScopeA
	t.Run("two repositories get one task branch each, in one workspace", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		appTip, docsTip := e.Seed(scope, wt.RepoApp), e.Seed(scope, wt.RepoDocs)
		w, assocs := e.Provision(scope, "wi_1", wt.RepoApp, wt.RepoDocs)

		if !w.Internal() || w.State != workspace.StateAvailable || w.RepositoryID != wt.RepoApp || w.WorkBranch != taskBranch("wi_1") || w.AcceptedCommit != appTip {
			t.Fatalf("workspace: %+v", w)
		}
		if len(assocs) != 2 || assocs[0].RepositoryID != wt.RepoApp || assocs[1].RepositoryID != wt.RepoDocs {
			t.Fatalf("associations: %+v", assocs)
		}
		for repo, tip := range map[workspace.RepositoryID]workspace.Commit{wt.RepoApp: appTip, wt.RepoDocs: docsTip} {
			a := assoc(t, assocs, repo)
			if a.TaskBranch != taskBranch("wi_1") || a.State != workspace.AssociationCreated || a.BaseCommit != tip || a.AcceptedCommit != tip || a.LastError != "" {
				t.Fatalf("%s: %+v", repo, a)
			}
			if got := e.Hosts.Ref(scope.TenantID, string(repo), taskBranch("wi_1")); got != tip {
				t.Fatalf("%s: the host's task branch is %q, want %s", repo, got, tip)
			}
		}
		// The default branches are untouched: a task branch is created beside them.
		if got := e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/heads/main"); got != appTip {
			t.Fatalf("the default branch moved: %s", got)
		}
		if n := len(e.Recorder.Committed(t, scope.TenantID, "workspace.state_changed")); n != 2 {
			t.Fatalf("state events: %d, want provisioning and available", n)
		}
		for action, want := range map[string]int{"workspace.provision_requested": 1, "workspace.task_branch_created": 2, "workspace.provisioned": 1} {
			if got := auditHas(t, e, w, action); got != want {
				t.Fatalf("audit %s: %d, want %d", action, got, want)
			}
		}
		// The workspace still belongs to the one work item.
		byItem, err := e.Store.GetByWorkItem(bg, scope, "wi_1")
		if err != nil || byItem.ID != w.ID {
			t.Fatalf("by work item: %+v %v", byItem, err)
		}
		// Recovery reports each repository's accepted commit (WS-08).
		rec, err := e.Svc.InternalRecovery(bg, wt.Engineer, scope, w.ID)
		if err != nil || len(rec) != 2 || !rec[0].UncommittedWorkMayBeLost || rec[0].LastAcceptedCommit != appTip {
			t.Fatalf("recovery: %+v %v", rec, err)
		}
		if _, err := e.Svc.RecoveryPoint(bg, wt.Engineer, scope, w.ID); !errors.Is(err, workspace.ErrWrongHosting) {
			t.Fatalf("the single recovery point of an internal workspace: %v", err)
		}
	})

	t.Run("a repository added later is associated beside the others", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		first, before := e.Provision(scope, "wi_1", wt.RepoApp)
		e.Seed(scope, "repo_extra")
		w, after, created, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", []workspace.RepositoryID{wt.RepoApp, "repo_extra"})
		if err != nil || created || w.ID != first.ID || len(after) != 2 {
			t.Fatalf("add: %+v %v created=%v %v", w, after, created, err)
		}
		if a := assoc(t, after, wt.RepoApp); *a != *before[0] {
			t.Fatalf("the first association changed: %+v -> %+v", before[0], a)
		}
		if a := assoc(t, after, "repo_extra"); a.State != workspace.AssociationCreated || a.TaskBranch != taskBranch("wi_1") {
			t.Fatalf("added: %+v", a)
		}
		if w.RepositoryID != wt.RepoApp {
			t.Fatalf("the primary repository changed to %s", w.RepositoryID)
		}
	})

	t.Run("repositories are checked before anything is reserved", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		e.Seed(wt.ScopeB, "repo_foreign")
		e.IRepos.Set(scope, workspace.InternalRepository{ID: "repo_ext", URL: "https://github.example.test/a/b.git", CheckoutSecretRef: "secret://x",
			DefaultBranch: "main", Hosting: gitv1.HostingExternal, Status: gitv1.StatusReady})
		for _, status := range []gitv1.RepositoryStatus{gitv1.StatusProvisioning, gitv1.StatusArchived} {
			e.IRepos.Set(scope, workspace.InternalRepository{ID: workspace.RepositoryID("repo_" + string(status)), URL: "https://git.example.test/x.git",
				CheckoutSecretRef: "kodama-git-read:x", DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: status})
		}
		e.IRepos.Set(scope, workspace.InternalRepository{ID: "repo_empty", URL: "https://git.example.test/e.git", CheckoutSecretRef: "kodama-git-read:e",
			DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady}) // no default branch yet
		e.Items.Add(scope, "wi_x", "story")
		e.Items.Add(scope, "epic_1", "epic")

		call := func(item domain.WorkItemID, actor workspace.Actor, repos ...workspace.RepositoryID) error {
			_, _, _, err := e.Svc.ProvisionInternal(bg, actor, scope, item, repos)
			return err
		}
		for name, tc := range map[string]struct {
			item  domain.WorkItemID
			actor workspace.Actor
			repos []workspace.RepositoryID
			want  error
		}{
			"foreign repository":    {"wi_x", wt.Engineer, []workspace.RepositoryID{wt.RepoApp, "repo_foreign"}, workspace.ErrNotFound},
			"unknown repository":    {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_nope"}, workspace.ErrNotFound},
			"external repository":   {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_ext"}, workspace.ErrWrongHosting},
			"provisioning repo":     {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_provisioning"}, workspace.ErrRepositoryNotReady},
			"archived repository":   {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_archived"}, workspace.ErrRepositoryNotReady},
			"empty repository":      {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_empty"}, workspace.ErrRepositoryNotReady},
			"no repository":         {"wi_x", wt.Engineer, nil, workspace.ErrInvalidValue},
			"repeated repository":   {"wi_x", wt.Engineer, []workspace.RepositoryID{wt.RepoApp, wt.RepoApp}, workspace.ErrInvalidValue},
			"malformed repository":  {"wi_x", wt.Engineer, []workspace.RepositoryID{"bad id"}, workspace.ErrInvalidValue},
			"epic":                  {"epic_1", wt.Engineer, []workspace.RepositoryID{wt.RepoApp}, workspace.ErrNotActionable},
			"unknown work item":     {"wi_nope", wt.Engineer, []workspace.RepositoryID{wt.RepoApp}, workspace.ErrNotFound},
			"unauthorized actor":    {"wi_x", workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}, []workspace.RepositoryID{wt.RepoApp}, workspace.ErrDenied},
			"malformed work item":   {"bad item", wt.Engineer, []workspace.RepositoryID{wt.RepoApp}, workspace.ErrNotFound},
			"repository then valid": {"wi_x", wt.Engineer, []workspace.RepositoryID{"repo_ext", wt.RepoApp}, workspace.ErrWrongHosting},
		} {
			if err := call(tc.item, tc.actor, tc.repos...); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		if _, err := e.Store.GetByWorkItem(bg, scope, "wi_x"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("a refused request reserved a workspace: %v", err)
		}
		if len(e.Hosts.CreateOps) != 0 {
			t.Fatalf("a refused request created branches: %v", e.Hosts.CreateOps)
		}
	})

	t.Run("a repository in another project or tenant is never visible", func(t *testing.T) {
		e := wt.NewInternalEnv(t)
		e.Seed(scope, wt.RepoApp)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp)
		for _, other := range []domain.Scope{wt.ScopeA2, wt.ScopeB} {
			if _, err := e.Svc.Repositories(bg, wt.Engineer, other, w.ID); err == nil {
				t.Fatalf("associations were read from %v", other)
			}
		}
		if _, err := e.Svc.Repositories(bg, workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}, scope, w.ID); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("unauthorized read: %v", err)
		}
	})

	t.Run("without the internal ports the operation fails closed", func(t *testing.T) {
		e := wt.NewMemServiceEnv(t)
		e.Items.Add(scope, "wi_1", "story")
		_, _, _, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", []workspace.RepositoryID{wt.RepoApp})
		if !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
		if _, err := e.Store.GetByWorkItem(bg, scope, "wi_1"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("a workspace was reserved: %v", err)
		}
	})
}

// Test_unit_workspace_task_branch_provisioned_idempotent: WS-17, WS-A13. Task
// branches are created once through the worker with an operation identifier
// stable per workspace and repository; a repeated, concurrent or interrupted
// provisioning creates nothing more, an unknown outcome is settled by observing
// the ref, and an independent branch is never overwritten.
func Test_unit_workspace_task_branch_provisioned_idempotent(t *testing.T) {
	scope := wt.ScopeA
	setup := func(t *testing.T) (*wt.InternalEnv, workspace.Commit, workspace.Commit) {
		e := wt.NewInternalEnv(t)
		return e, e.Seed(scope, wt.RepoApp), e.Seed(scope, wt.RepoDocs)
	}
	repos := []workspace.RepositoryID{wt.RepoApp, wt.RepoDocs}

	t.Run("repeating the provisioning calls the worker no more", func(t *testing.T) {
		e, _, _ := setup(t)
		w, _ := e.Provision(scope, "wi_1", repos...)
		ops := append([]string(nil), e.Hosts.CreateOps...)
		if len(ops) != 2 || ops[0] == ops[1] {
			t.Fatalf("one creation per repository with its own operation: %v", ops)
		}
		for _, repo := range repos {
			if got := workspace.BranchOperationID(w, repo); got != ops[0] && got != ops[1] {
				t.Fatalf("operation id of %s is not the derived stable one: %v", repo, ops)
			}
		}
		again, assocs, created, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", repos)
		if err != nil || created || again.ID != w.ID || again.Version != w.Version || len(assocs) != 2 {
			t.Fatalf("repeat: %+v created=%v %v", again, created, err)
		}
		if len(e.Hosts.CreateOps) != len(ops) {
			t.Fatalf("a repeated provisioning called the worker: %v", e.Hosts.CreateOps)
		}
		// Through the external-shaped entry point too: the same workspace, no call.
		viaProvision, createdExt, err := e.Svc.Provision(bg, wt.Engineer, scope, "wi_1")
		if err != nil || createdExt || viaProvision.ID != w.ID || len(e.Hosts.CreateOps) != len(ops) {
			t.Fatalf("Provision of an internal workspace: %+v %v", viaProvision, err)
		}
	})

	t.Run("concurrent provisioning yields one workspace and one creation per repository", func(t *testing.T) {
		e, _, _ := setup(t)
		e.Items.Add(scope, "wi_1", "story")
		const n = 8
		ids := make([]workspace.ID, n)
		created := make([]bool, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w, _, c, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", repos)
				errs[i], created[i] = err, c
				if err == nil {
					ids[i] = w.ID
				}
			}()
		}
		close(start)
		wg.Wait()
		reserved := 0
		for i := range n {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("caller %d: %v %q (first %q)", i, errs[i], ids[i], ids[0])
			}
			if created[i] {
				reserved++
			}
		}
		if reserved != 1 {
			t.Fatalf("%d callers reserved the workspace", reserved)
		}
		if got := distinct(e.Hosts.CreateOps); len(got) != 2 {
			t.Fatalf("concurrent callers used operations %v, want one per repository", got)
		}
		w, err := e.Store.GetByWorkItem(bg, scope, "wi_1")
		if err != nil || w.State != workspace.StateAvailable {
			t.Fatalf("%+v %v", w, err)
		}
		for _, a := range associations(t, e, w) {
			if a.State != workspace.AssociationCreated {
				t.Fatalf("%+v", a)
			}
		}
	})

	t.Run("a lost creation response is settled by observing the ref", func(t *testing.T) {
		e, appTip, _ := setup(t)
		e.Hosts.FailCreate(errors.New("connection reset"), true, 1) // applied, response lost
		w, assocs := e.Provision(scope, "wi_1", repos...)
		if w.State != workspace.StateAvailable || assoc(t, assocs, wt.RepoApp).State != workspace.AssociationCreated || assoc(t, assocs, wt.RepoApp).AcceptedCommit != appTip {
			t.Fatalf("%+v %+v", w, assocs)
		}
		if got := e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), taskBranch("wi_1")); got != appTip {
			t.Fatalf("task branch %s", got)
		}
		if n := len(distinct(e.Hosts.CreateOps)); n != 2 {
			t.Fatalf("operations: %v", e.Hosts.CreateOps)
		}
	})

	t.Run("a creation that failed is retried by the reconciler under the same operation", func(t *testing.T) {
		e, appTip, _ := setup(t)
		e.Hosts.FailCreate(errors.New("worker unavailable"), false, -1)
		w, assocs := e.Provision(scope, "wi_1", repos...)
		if w.State != workspace.StateProvisioning || w.LastError != workspace.ReasonProvisionFailed || w.AcceptedCommit != "" {
			t.Fatalf("workspace: %+v", w)
		}
		for _, a := range assocs {
			if a.State != workspace.AssociationIntent || a.LastError != workspace.ReasonProvisionFailed || a.BaseCommit == "" || a.AcceptedCommit != "" {
				t.Fatalf("association: %+v", a)
			}
		}
		e.Hosts.ReadErr = errors.New("worker unavailable")
		if _, err := e.Svc.Reconcile(bg); err != nil {
			t.Fatal(err)
		}
		if got := reload(t, e, w); got.State != workspace.StateProvisioning {
			t.Fatalf("reconciled during the outage: %+v", got)
		}
		before := len(e.Hosts.CreateOps)
		e.Hosts.FailCreate(nil, false, 0)
		e.Hosts.ReadErr = nil
		rep, err := e.Svc.Reconcile(bg)
		if err != nil || rep.Examined == 0 || rep.Failed != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		got := reload(t, e, w)
		if got.State != workspace.StateAvailable || got.AcceptedCommit != appTip || got.LastError != "" {
			t.Fatalf("after reconcile: %+v", got)
		}
		if after := distinct(e.Hosts.CreateOps); len(after) != 2 || len(e.Hosts.CreateOps) == before {
			t.Fatalf("the reconciler did not retry under the same operations: %v", e.Hosts.CreateOps)
		}
	})

	t.Run("a created branch that moved on is still recognised as ours from the journal", func(t *testing.T) {
		e, _, _ := setup(t)
		e.Hosts.FailCreate(errors.New("worker unavailable"), false, -1)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp)
		e.Hosts.FailCreate(nil, false, 0)
		// The earlier request had in fact been applied, and a human pushed after it.
		a := assoc(t, associations(t, e, w), wt.RepoApp)
		if _, err := e.Hosts.CreateBranch(bg, scope.TenantID, gitv1.CreateBranchRequest{RepositoryID: string(wt.RepoApp),
			OperationID: workspace.BranchOperationID(w, wt.RepoApp), Ref: a.TaskBranch, Commit: string(a.BaseCommit)}); err != nil {
			t.Fatal(err)
		}
		human := e.Inspector.Add("human work", a.BaseCommit)
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), a.TaskBranch, human)

		if err := e.Svc.ReconcileWorkspace(bg, scope, w.ID); err != nil {
			t.Fatal(err)
		}
		got := reload(t, e, w)
		after := assoc(t, associations(t, e, w), wt.RepoApp)
		if got.State != workspace.StateAvailable || after.State != workspace.AssociationCreated || after.AcceptedCommit != a.BaseCommit {
			t.Fatalf("%+v %+v", got, after)
		}
		if tip := e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), a.TaskBranch); tip != human {
			t.Fatalf("the human commit was overwritten: %s", tip)
		}
	})

	t.Run("an independent branch is never overwritten or adopted", func(t *testing.T) {
		e, _, _ := setup(t)
		other := e.Inspector.Add("someone else's branch")
		e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), taskBranch("wi_1"), other)
		w, assocs := e.Provision(scope, "wi_1", wt.RepoApp)
		a := assoc(t, assocs, wt.RepoApp)
		if w.State != workspace.StateProvisioning || w.LastError != workspace.ReasonBranchExists || a.State != workspace.AssociationIntent || a.LastError != workspace.ReasonBranchExists {
			t.Fatalf("%+v %+v", w, a)
		}
		if tip := e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), taskBranch("wi_1")); tip != other {
			t.Fatalf("the independent branch was overwritten: %s", tip)
		}
		// Repeating does not adopt it either.
		if again, _, _, err := e.Svc.ProvisionInternal(bg, wt.Engineer, scope, "wi_1", []workspace.RepositoryID{wt.RepoApp}); err != nil || again.State != workspace.StateProvisioning {
			t.Fatalf("%+v %v", again, err)
		}
	})

	t.Run("a repository that stops being ready records a reason and creates nothing", func(t *testing.T) {
		e, _, _ := setup(t)
		e.Hosts.FailCreate(errors.New("worker unavailable"), false, -1)
		w, _ := e.Provision(scope, "wi_1", wt.RepoApp)
		if w.State != workspace.StateProvisioning {
			t.Fatalf("%+v", w)
		}
		e.Hosts.FailCreate(nil, false, 0)
		repo, _ := e.IRepos.InternalRepository(bg, scope, wt.RepoApp)
		repo.Status = gitv1.StatusArchived
		e.IRepos.Set(scope, repo)
		before := len(e.Hosts.CreateOps)
		if err := e.Svc.ReconcileWorkspace(bg, scope, w.ID); err != nil {
			t.Fatal(err)
		}
		a := assoc(t, associations(t, e, w), wt.RepoApp)
		if got := reload(t, e, w); got.State != workspace.StateProvisioning || got.LastError != workspace.ReasonRepositoryUnready ||
			a.State != workspace.AssociationIntent || a.LastError != workspace.ReasonRepositoryUnready {
			t.Fatalf("%+v %+v", got, a)
		}
		if len(e.Hosts.CreateOps) != before {
			t.Fatalf("a branch was created in an archived repository: %v", e.Hosts.CreateOps)
		}
		// The repository is restored: the same workspace completes.
		repo.Status = gitv1.StatusReady
		e.IRepos.Set(scope, repo)
		if err := e.Svc.ReconcileWorkspace(bg, scope, w.ID); err != nil {
			t.Fatal(err)
		}
		if got := reload(t, e, w); got.State != workspace.StateAvailable || got.LastError != "" {
			t.Fatalf("%+v", got)
		}
	})
}
