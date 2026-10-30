package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/shell/store"
)

func runInfo(id, pipeline, task string, repoID string) store.RunGitInfo {
	return store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: id, PipelineName: pipeline, TaskID: task, Status: "running",
		Sources: []byte(`[{"repositoryId":"` + repoID + `","commit":"` + c1 + `","path":".","access":"write","branch":"refs/heads/kodama/work/` + task + `"}]`)}
}

// seeded returns a repository with a default branch (the seed was recorded).
func (e *env) seeded(name string, policy map[string]any) (map[string]any, string) {
	e.t.Helper()
	repo := e.createRepo("tok-eng", "p_erp", name, map[string]any{"policy": policy})
	id := str(repo["id"])
	main := store.ReceiveUpdate{Ref: "refs/heads/main", Old: z0, New: c1, Kind: "create"}
	if _, err := e.mem.CommitReceive(e.t.Context(), store.ReceiveCommit{TenantID: tenantID, RepositoryID: id, ReceiveID: "rcv_seed_" + name,
		ActorType: "user", ActorID: "u_eng", Updates: []store.ReceiveUpdate{main}}); err != nil {
		e.t.Fatal(err)
	}
	return repo, id
}

func update(ref, old, new string) gitv1.RefUpdate {
	return gitv1.RefUpdate{Ref: ref, Old: old, New: new}
}

func facts(ff map[string]bool, updates ...gitv1.RefUpdate) []gitv1.GitFact {
	out := make([]gitv1.GitFact, len(updates))
	for i, u := range updates {
		out[i] = gitv1.GitFact{Ref: u.Ref}
		if !gitv1.IsZeroObjectID(u.Old) && !gitv1.IsZeroObjectID(u.New) {
			v, ok := ff[u.Ref]
			if !ok {
				v = true
			}
			out[i].FastForward = &v
		}
	}
	return out
}

// Test_unit_kernel_git_default_branch_protected: after seeding, the default
// branch advances only by integration or, where the repository opted in, by an
// engineer's fast-forward; every ref class has its own rule and any denial names
// a stable reason.
func Test_unit_kernel_git_default_branch_protected(t *testing.T) {
	e := newEnv(t)
	const main = "refs/heads/main"
	closed, closedID := e.seeded("closed", map[string]any{"integrationPipeline": "integrate"})
	open, openID := e.seeded("open", map[string]any{"directPushToDefault": true})

	check := func(t *testing.T, grant, repoID string, u gitv1.RefUpdate, ff bool, want string) {
		t.Helper()
		resp, err := e.authorize(grant, repoID, []gitv1.RefUpdate{u}, facts(map[string]bool{u.Ref: ff}, u))
		if err != nil {
			t.Fatalf("%s %s->%s: %v", u.Ref, u.Old, u.New, err)
		}
		if got := reasons(resp)[0]; got != want {
			t.Errorf("%s %.4s->%.4s ff=%v: %s, want %s", u.Ref, u.Old, u.New, ff, got, want)
		}
	}

	t.Run("engineers", func(t *testing.T) {
		closedGrant, _ := e.mustCredential("tok-eng", closed, false)
		openGrant, _ := e.mustCredential("tok-eng", open, false)
		for name, c := range map[string]struct {
			grant, repo string
			u           gitv1.RefUpdate
			ff          bool
			want        string
		}{
			"default branch is protected by default": {closedGrant, closedID, update(main, c1, c2), true, gitv1.ReasonRefProtected},
			"default branch delete":                  {closedGrant, closedID, update(main, c1, z0), true, gitv1.ReasonRefProtected},
			"opted in: fast-forward":                 {openGrant, openID, update(main, c1, c2), true, "allow"},
			"opted in: rewrite":                      {openGrant, openID, update(main, c1, c3), false, gitv1.ReasonNotFastForward},
			"opted in: delete":                       {openGrant, openID, update(main, c1, z0), true, gitv1.ReasonRefProtected},
			"opted in: create of the default branch": {openGrant, openID, update(main, z0, c2), true, gitv1.ReasonRefProtected},
			"task branch":                            {closedGrant, closedID, update("refs/heads/kodama/work/tsk_1", z0, c2), true, "allow"},
			"run branch":                             {closedGrant, closedID, update("refs/heads/kodama/runs/run_9/x", z0, c2), true, "allow"},
			"agent branch":                           {closedGrant, closedID, update("refs/heads/kodama/agents/run_9/impl", z0, c2), true, "allow"},
			"other branch":                           {closedGrant, closedID, update("refs/heads/feature/x", z0, c2), true, "allow"},
			"other branch rewrite":                   {closedGrant, closedID, update("refs/heads/feature/x", c1, c3), false, "allow"},
			"tag":                                    {closedGrant, closedID, update("refs/tags/v1", z0, c2), true, "allow"},
			"server ref":                             {closedGrant, closedID, update("refs/kodama/runs/run_9/1", z0, c2), true, gitv1.ReasonRefProtected},
			"server ref of an opted-in repository":   {openGrant, openID, update("refs/kodama/runs/run_9/1", c1, c2), true, gitv1.ReasonRefProtected},
			"notes":                                  {closedGrant, closedID, update("refs/notes/commits", z0, c2), true, gitv1.ReasonRefProtected},
			"another repository with this repository's grant": {closedGrant, openID, update("refs/heads/feature/x", z0, c2), true, ""},
		} {
			if c.want == "" { // a grant of one repository names no other
				if _, err := e.authorize(c.grant, c.repo, []gitv1.RefUpdate{c.u}, facts(nil, c.u)); err == nil {
					t.Errorf("%s: a grant authorized another repository", name)
				}
				continue
			}
			t.Run(name, func(t *testing.T) { check(t, c.grant, c.repo, c.u, c.ff, c.want) })
		}
	})

	t.Run("a read-only person pushes nothing", func(t *testing.T) {
		grant, _ := e.mustCredential("tok-viewer", closed, false)
		for _, ref := range []string{main, "refs/heads/feature/x", "refs/tags/v1", "refs/heads/kodama/work/tsk_1"} {
			check(t, grant, closedID, update(ref, z0, c2), true, gitv1.ReasonGrantScope)
		}
	})

	t.Run("a whole receive carries one verdict per update", func(t *testing.T) {
		grant, _ := e.mustCredential("tok-eng", closed, false)
		ups := []gitv1.RefUpdate{update("refs/heads/feature/a", z0, c2), update(main, c1, c2), update("refs/tags/v2", z0, c2)}
		resp, err := e.authorize(grant, closedID, ups, facts(nil, ups...))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(reasons(resp), ","); got != "allow,ref_protected,allow" {
			t.Fatalf("verdicts = %s: the denied update is named, the worker fails the receive as a whole", got)
		}
		if resp.Allowed() {
			t.Fatal("a receive with a denied update is allowed")
		}
		if resp.PushID == "" || resp.Deadline == "" {
			t.Fatalf("response = %+v", resp)
		}
	})

	t.Run("runs", func(t *testing.T) {
		e.mem.SetRun(runInfo("run_1", "build", "tsk_1", closedID))
		issued, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", closedID)
		if err != nil {
			t.Fatal(err)
		}
		g := issued.GrantID
		for name, c := range map[string]struct {
			u    gitv1.RefUpdate
			want string
		}{
			"default branch":        {update(main, c1, c2), gitv1.ReasonRefProtected},
			"its task branch":       {update("refs/heads/kodama/work/tsk_1", z0, c2), "allow"},
			"another task's branch": {update("refs/heads/kodama/work/tsk_2", z0, c2), gitv1.ReasonGrantScope},
			"its run branch":        {update("refs/heads/kodama/runs/run_1/x", z0, c2), "allow"},
			"its agent branch":      {update("refs/heads/kodama/agents/run_1/impl", z0, c2), "allow"},
			"another run's branch":  {update("refs/heads/kodama/runs/run_2/x", z0, c2), gitv1.ReasonGrantScope},
			"another run's agent":   {update("refs/heads/kodama/agents/run_2/impl", z0, c2), gitv1.ReasonGrantScope},
			"an ungranted branch":   {update("refs/heads/feature/x", z0, c2), gitv1.ReasonGrantScope},
			"a tag":                 {update("refs/tags/v1", z0, c2), gitv1.ReasonGrantScope},
			"a retained server ref": {update("refs/kodama/runs/run_1/1", z0, c2), gitv1.ReasonRefProtected},
		} {
			t.Run(name, func(t *testing.T) { check(t, g, closedID, c.u, true, c.want) })
		}
	})

	t.Run("integration", func(t *testing.T) {
		e.mem.SetRun(runInfo("run_i", "integrate", "tsk_1", closedID))
		// The credential is issued only to the repository's integration pipeline.
		e.mem.SetRun(runInfo("run_b", "build", "tsk_1", closedID))
		if _, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_b", closedID, c1, c2); err == nil {
			t.Fatal("an integration credential was issued to another pipeline")
		}
		if _, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", openID, c1, c2); err == nil {
			t.Fatal("an integration credential was issued for a repository without an integration pipeline")
		}
		// The run's task must be approved: no task, an unknown task and a task that
		// is not approved are all refused at issuance.
		e.mem.SetRun(runInfo("run_nt", "integrate", "", closedID))
		if _, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_nt", closedID, c1, c2); !errors.Is(err, store.ErrTaskNotApproved) {
			t.Fatalf("a run without a task was issued an integration credential: %v", err)
		}
		e.mem.SetRun(runInfo("run_un", "integrate", "tsk_ghost", closedID))
		if _, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_un", closedID, c1, c2); !errors.Is(err, store.ErrTaskNotApproved) {
			t.Fatalf("a run whose task cannot be found was issued an integration credential: %v", err)
		}
		e.mem.SetTaskApproval(tenantID, projectID, "tsk_1", false)
		if _, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", closedID, c1, c2); !errors.Is(err, store.ErrTaskNotApproved) {
			t.Fatalf("an unapproved task was issued an integration credential: %v", err)
		}
		e.mem.SetTaskApproval(tenantID, projectID, "tsk_1", true)
		issued, err := e.git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", closedID, c1, c2)
		if err != nil {
			t.Fatal(err)
		}
		g := issued.GrantID
		for name, c := range map[string]struct {
			u    gitv1.RefUpdate
			ff   bool
			want string
		}{
			"exactly from to to": {update(main, c1, c2), true, "allow"},
			"another commit":     {update(main, c1, c3), true, gitv1.ReasonIntegrationMismatch},
			"the base moved":     {update(main, c3, c2), true, gitv1.ReasonIntegrationMismatch},
			"not a fast-forward": {update(main, c1, c2), false, gitv1.ReasonNotFastForward},
			"a create":           {update(main, z0, c2), true, gitv1.ReasonIntegrationMismatch},
			"another branch":     {update("refs/heads/feature/x", z0, c2), true, gitv1.ReasonGrantScope},
			"a tag":              {update("refs/tags/v1", z0, c2), true, gitv1.ReasonGrantScope},
			"a server ref":       {update("refs/kodama/runs/run_i/1", z0, c2), true, gitv1.ReasonRefProtected},
		} {
			t.Run(name, func(t *testing.T) { check(t, g, closedID, c.u, c.ff, c.want) })
		}
		// Approval is read again at every receive: withdrawn after issuance, or the
		// task gone, the credential authorizes nothing; approved again, it works.
		e.mem.SetTaskApproval(tenantID, projectID, "tsk_1", false)
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		e.mem.RemoveTask(tenantID, projectID, "tsk_1")
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		e.mem.SetTaskApproval(tenantID, projectID, "tsk_1", true)
		check(t, g, closedID, update(main, c1, c2), true, "allow")
		// The authority is the repository's current policy: changing the integration pipeline withdraws it.
		if r := e.do(http.MethodPatch, reposPath+"/"+closedID, "tok-eng", map[string]any{"policy": map[string]any{"directPushToDefault": false, "integrationPipeline": "release"}}, "If-Match", "2"); r.status != http.StatusOK {
			t.Fatalf("policy change = %d %s", r.status, r.raw)
		}
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		// A cancelled run keeps nothing.
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_i", PipelineName: "integrate", Status: "running", Cancelling: true})
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonRunInactive)
	})
}
