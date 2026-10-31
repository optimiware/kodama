package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/api"
	gitauth "github.com/optimiware/kodama/internal/shell/git"
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
		runs := &runRecords{}
		git, err := api.NewGit(api.GitConfig{Store: e.mem, Runs: runs, Auth: tokens, Workers: e.workers, Signer: e.signer,
			Identities: e.verifier, Now: e.clock})
		if err != nil {
			t.Fatal(err)
		}
		plain := e.git
		e.git = git
		t.Cleanup(func() { e.git = plain })
		e.workers.ancestor = true // c1 is an ancestor of c2: the update fast-forwards

		issue := func(run, repo string) (api.IssuedCredential, error) {
			return git.IssueIntegrationCredential(t.Context(), tenantID, projectID, run, "integrate", repo, c1, c2)
		}
		wantRefusal := func(t *testing.T, err error, want gitauth.Code) {
			t.Helper()
			if code, ok := gitauth.IsRefusal(err); !ok || code != want {
				t.Fatalf("error = %v, want refusal %s", err, want)
			}
		}

		e.mem.SetRun(runInfo("run_i", "integrate", "tsk_1", closedID))
		runs.set(integrationRecord(t, "run_i", c2, "approved"))

		// Without a configured authority nothing is issued.
		if _, err := plain.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", "integrate", closedID, c1, c2); !errors.Is(err, api.ErrIntegrationUnavailable) {
			t.Fatalf("issued without an authority: %v", err)
		}
		// The credential is issued only to the repository's integration pipeline.
		e.mem.SetRun(runInfo("run_b", "build", "tsk_1", closedID))
		runs.set(integrationRecord(t, "run_b", c2, "approved"))
		_, err = issue("run_b", closedID)
		wantRefusal(t, err, gitauth.CodeWrongPipeline)
		_, err = issue("run_i", openID)
		wantRefusal(t, err, gitauth.CodeWrongPipeline)
		// A run with no durable record is unknown, whatever its task says.
		e.mem.SetRun(runInfo("run_nr", "integrate", "tsk_approved", closedID))
		_, err = issue("run_nr", closedID)
		wantRefusal(t, err, gitauth.CodeUnknown)
		// An earlier step holds no authority.
		if _, err := git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", "implement", closedID, c1, c2); err == nil {
			t.Fatal("an earlier step was issued an integration credential")
		} else {
			wantRefusal(t, err, gitauth.CodeStepIneligible)
		}
		// A wait that rejected, or approved another commit, authorizes nothing.
		runs.set(integrationRecord(t, "run_i", c2, "rejected"))
		_, err = issue("run_i", closedID)
		wantRefusal(t, err, gitauth.CodeNotApproved)
		runs.set(integrationRecord(t, "run_i", c3, "approved"))
		_, err = issue("run_i", closedID)
		wantRefusal(t, err, gitauth.CodeCandidateMismatch)
		// A wait still open is not approval, and a task that says approved changes nothing.
		open := integrationRecord(t, "run_i", c2, "")
		runs.set(open)
		_, err = issue("run_i", closedID)
		wantRefusal(t, err, gitauth.CodeStepIneligible)
		// A moved branch or a non-fast-forward is refused at issuance.
		runs.set(integrationRecord(t, "run_i", c2, "approved"))
		e.workers.ancestor = false
		_, err = issue("run_i", closedID)
		wantRefusal(t, err, gitauth.CodeNotFastForward)
		e.workers.ancestor = true
		if _, err := git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_i", "integrate", closedID, c3, c2); err == nil {
			t.Fatal("an expected-old that is not the ref's value was issued")
		} else {
			wantRefusal(t, err, gitauth.CodeExpectedOldChanged)
		}

		issued, err := issue("run_i", closedID)
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
		// Authority is decided again at every receive from the run's durable record:
		// a wait decided for another commit withdraws it, the step settling ends it,
		// and a restored record allows it again.
		runs.set(integrationRecord(t, "run_i", c3, "approved"))
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		runs.set(integrationRecord(t, "run_i", c2, "rejected"))
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		settled := integrationRecord(t, "run_i", c2, "approved")
		if err := settled.Run.FinishStep("integrate", rt.StepSucceeded, rt.ReasonNone, rt.Result{Code: rt.ResultSucceeded}); err != nil {
			t.Fatal(err)
		}
		runs.set(settled)
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonIntegrationMismatch)
		cancelling := integrationRecord(t, "run_i", c2, "approved")
		cancelling.Run.RequestCancel()
		runs.set(cancelling)
		check(t, g, closedID, update(main, c1, c2), true, gitv1.ReasonRunInactive)
		runs.set(integrationRecord(t, "run_i", c2, "approved"))
		check(t, g, closedID, update(main, c1, c2), true, "allow")
		// A task row, an approving status and a bare event are not inputs: the
		// SQL run keeps its task and nothing about it matters.
		e.mem.SetRun(runInfo("run_i", "integrate", "tsk_approved_everywhere", closedID))
		check(t, g, closedID, update(main, c1, c2), true, "allow")
		e.mem.SetRun(runInfo("run_i", "integrate", "tsk_1", closedID))
		// Without an authority the credential authorizes nothing.
		e.git = plain
		resp, err := e.authorize(g, closedID, []gitv1.RefUpdate{update(main, c1, c2)}, facts(nil, update(main, c1, c2)))
		e.git = git
		if err != nil || reasons(resp)[0] != gitv1.ReasonIntegrationMismatch {
			t.Fatalf("an integration credential honored without an authority: %v %v", reasons(resp), err)
		}
		// A record that cannot be read fails the receive: never an approval.
		runs.fail = errors.New("state store unavailable")
		if _, err := e.authorize(g, closedID, []gitv1.RefUpdate{update(main, c1, c2)}, facts(nil, update(main, c1, c2))); contractCode(err) != domain.CodeUnavailable {
			t.Fatalf("an unreadable run record: %v", err)
		}
		runs.fail = nil
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

// runRecords is a runtime record reader the tests change between receives.
type runRecords struct {
	mu   sync.Mutex
	recs map[domain.RunID]rt.RunRecord
	fail error
}

func (r *runRecords) set(rec rt.RunRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recs == nil {
		r.recs = map[domain.RunID]rt.RunRecord{}
	}
	r.recs[rec.Run.ID] = rec
}

func (r *runRecords) Load(_ context.Context, scope domain.Scope, id domain.RunID) (rt.RunRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return rt.RunRecord{}, r.fail
	}
	rec, ok := r.recs[id]
	if !ok || rec.Run.Scope != scope {
		return rt.RunRecord{}, &domain.Error{Code: domain.CodeNotFound, Message: "run"}
	}
	return rec, nil
}

// integrationRecord is the durable record of run id: implement succeeded, the
// review wait opened for candidate and decided with outcome (empty: left open),
// and the protected integrate step running once the wait is decided.
func integrationRecord(t *testing.T, id domain.RunID, candidate, outcome string) rt.RunRecord {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	spec := rt.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "implement.outputs.commit"}, Input: "approval",
		Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour, OnExpiry: rt.ExpiryFail}
	plan := rt.Plan{Digest: "sha256:p", Steps: []rt.PlanStep{
		{Key: "implement", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "agent:1", Commands: []string{"agent"}}},
		{Key: "review", Kind: rt.StepWait, DependsOn: []domain.StepID{"implement"}, Wait: &spec},
		{Key: "integrate", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, Authority: rt.AuthorityIntegrate,
			Requires: []rt.Requirement{{Wait: "review", Outcome: "approved"}},
			Workload: &rt.WorkloadSpec{Image: "integrator:1", Commands: []string{"integrate-candidate"}}},
	}}
	scope := domain.Scope{TenantID: tenantID, ProjectID: projectID}
	run, err := rt.NewRun(scope, id, "pl_1", plan, 3)
	must(err)
	must(run.AdvanceStep("implement", rt.StepRunning, rt.ReasonNone))
	must(run.FinishStep("implement", rt.StepSucceeded, rt.ReasonNone, rt.Result{Code: rt.ResultSucceeded}))
	must(run.AdvanceStep("review", rt.StepWaiting, rt.ReasonNone))
	opened := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	w, err := rt.NewWait(scope, "wait_1", id, "review", spec)
	must(err)
	must(w.Open(opened, map[string]string{"candidate": candidate}, ""))
	if outcome != "" {
		_, err = w.Resolve(rt.SignalRequest{Identity: rt.SignalIdentity{Scope: scope, Caller: "user-bob", Key: "k1"},
			WaitID: w.ID, Outcome: outcome, Actor: "user:bob"}, opened.Add(time.Minute))
		must(err)
		must(run.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
		must(run.AdvanceStep("integrate", rt.StepRunning, rt.ReasonNone))
	}
	rec := rt.RunRecord{Run: run, Plan: plan, Waits: []rt.Wait{w}}
	must(rec.Validate())
	return rec
}
