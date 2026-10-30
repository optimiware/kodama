package agents_test

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/agents"
)

const (
	commitBack  runtimev1.GitCommit = "1111111111111111111111111111111111111111"
	commitFront runtimev1.GitCommit = "2222222222222222222222222222222222222222"
	newBack     runtimev1.GitCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newFront    runtimev1.GitCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	taskBranch  = "refs/heads/kodama/work/wi_1042"
	agentBranch = "refs/heads/kodama/agents/wi_1042/run_01/generate"
	// readerBranch is the agent branch of the reader fixtures (their step is "test").
	readerBranch = "refs/heads/kodama/agents/wi_1042/run_01/test"
)

// v3Fixture reads a published runtime v3 context fixture (valid by the contract's
// own tests) so these tests exercise real, validated contexts.
func v3Fixture(t testing.TB, name string) runtimev3.ContextV3 {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/runtime/v3/fixtures/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			ctx, err := runtimev3.DecodeRequest[runtimev3.ContextV3](c.Payload)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return ctx
		}
	}
	t.Fatalf("no fixture %q", name)
	return runtimev3.ContextV3{}
}

func byPath(plan []agents.Checkout) map[string]agents.Checkout {
	m := map[string]agents.Checkout{}
	for _, c := range plan {
		m[c.Path] = c
	}
	return m
}

// Test_unit_agent_multi_checkout_candidate: an agent run with several write and
// read sources has one checkout per source, and its candidate is the set of the
// per-source commits and branches, not one commit (AR-11, AR-15, ADR 0011).
func Test_unit_agent_multi_checkout_candidate(t *testing.T) {
	t.Run("the writer commits its write source for the task branch and every other source on an agent branch", func(t *testing.T) {
		plan, err := agents.PlanCheckouts(v3Fixture(t, "context_v3_work_item_writer_two_repositories"))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan) != 2 || plan[0].Path != "backend" || plan[1].Path != "frontend" {
			t.Fatalf("plan is in path order: %+v", plan)
		}
		back, front := plan[0], plan[1]
		if back.Mode != agents.ModeHead || back.Ref != taskBranch || back.RepositoryID != "repo_01" || back.Pinned != commitBack {
			t.Fatalf("backend: %+v", back)
		}
		if front.Mode != agents.ModeAgentBranch || front.Ref != agentBranch || front.RepositoryID != "repo_02" {
			t.Fatalf("a read source never goes to the task branch: %+v", front)
		}
	})

	t.Run("a reader never plans the task branch, even for a write source", func(t *testing.T) {
		plan, err := agents.PlanCheckouts(v3Fixture(t, "context_v3_work_item_reader_first_attempt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, co := range plan {
			if co.Mode != agents.ModeAgentBranch || co.Ref != readerBranch {
				t.Fatalf("a reader's checkouts are agent branches: %+v", co)
			}
			if strings.HasPrefix(co.Ref, "refs/heads/kodama/work/") {
				t.Fatalf("the reader was planned onto the task branch: %+v", co)
			}
		}
	})

	t.Run("a run without a work item uses its run branches", func(t *testing.T) {
		plan, err := agents.PlanCheckouts(v3Fixture(t, "context_v3_push_origin_reader_root_source"))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan) != 1 || plan[0].Path != "." || plan[0].Ref != "refs/heads/kodama/runs/run_01/verify" || plan[0].Mode != agents.ModeAgentBranch {
			t.Fatalf("%+v", plan)
		}
		w, err := agents.PlanCheckouts(v3Fixture(t, "context_v3_push_origin_writer_run_branch"))
		if err != nil {
			t.Fatal(err)
		}
		if w[0].Mode != agents.ModeHead || w[0].Ref != "refs/heads/kodama/runs/run_01/out" {
			t.Fatalf("%+v", w)
		}
	})

	t.Run("the plan is refused whenever the attempt's grant could not publish a branch", func(t *testing.T) {
		c := v3Fixture(t, "context_v3_work_item_writer_two_repositories")
		c.Git.Repositories[1].Update = []string{"refs/heads/kodama/agents/wi_1042/run_01/other"} // frontend: not the planned label
		if _, err := agents.PlanCheckouts(c); err == nil {
			t.Fatal("a plan the entrypoint could not publish was accepted")
		}
		c = v3Fixture(t, "context_v3_work_item_writer_two_repositories")
		c.Git.Repositories[0].Update = c.Git.Repositories[0].Update[:1] // the writer's grant lost the task branch
		if _, err := agents.PlanCheckouts(c); err == nil {
			t.Fatal("the writer was planned onto a branch its grant does not select")
		}
		c = v3Fixture(t, "context_v3_work_item_writer_two_repositories")
		c.Git = nil
		if _, err := agents.PlanCheckouts(c); err == nil {
			t.Fatal("a context without Git delivery was planned")
		}
		c = v3Fixture(t, "context_v3_cleanup_run_manual_origin_source_free_without_git")
		if _, err := agents.PlanCheckouts(c); err == nil {
			t.Fatal("a source-free run was planned for an agent")
		}
	})

	t.Run("two sources of one repository get distinct agent branches", func(t *testing.T) {
		c := v3Fixture(t, "context_v3_work_item_reader_first_attempt")
		front := c.Workspace.Sources[1]
		front.Path, front.RepositoryID = "docs", c.Workspace.Sources[0].RepositoryID
		c.Workspace.Sources = []runtimev3.SourceV3{c.Workspace.Sources[0], front}
		plan, err := agents.PlanCheckouts(c)
		if err != nil {
			t.Fatal(err)
		}
		if plan[0].Ref == plan[1].Ref || plan[0].Ref != readerBranch || !strings.HasPrefix(plan[1].Ref, readerBranch+"-") {
			t.Fatalf("refs inside one repository must differ: %q %q", plan[0].Ref, plan[1].Ref)
		}
	})

	t.Run("sources of one repository never collide, whatever their paths or the step ID", func(t *testing.T) {
		for name, step := range map[string]string{"short": "test", "at the bound": strings.Repeat("s", 48), "beyond the bound": strings.Repeat("s", 90)} {
			c := v3Fixture(t, "context_v3_work_item_reader_first_attempt")
			c.StepID = step
			base := c.Workspace.Sources[0]
			var srcs []runtimev3.SourceV3
			// Paths that differ only where a label cannot: a/b vs a-b vs a_b, and a long pair
			// that agrees up to the length bound.
			for _, p := range []string{"a", "a-b", "a/b", "a_b", "a.b", strings.Repeat("p", 70) + "1", strings.Repeat("p", 70) + "2"} {
				s := base
				s.Path = p
				srcs = append(srcs, s)
			}
			sort.Slice(srcs, func(i, j int) bool { return srcs[i].Path < srcs[j].Path })
			c.Workspace.Sources = srcs
			// Paths nest ("a" and "a/b"); the planner does not judge the set (the contract
			// does), it must only never hand out one ref twice.
			plan, err := agents.PlanCheckouts(c)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			seen := map[string]string{}
			for _, co := range plan {
				if prev, dup := seen[co.Ref]; dup {
					t.Fatalf("%s: %q and %q share %s", name, prev, co.Path, co.Ref)
				}
				seen[co.Ref] = co.Path
				if len(co.Ref) > 255 || gitv1.ValidateBranchRef(co.Ref) != nil {
					t.Fatalf("%s: %q is not a valid branch ref", name, co.Ref)
				}
			}
		}
	})

	t.Run("a plan that would commit two sources to one ref of one repository is refused", func(t *testing.T) {
		c := v3Fixture(t, "context_v3_work_item_writer_two_repositories")
		second := c.Workspace.Sources[0]
		second.Path = "backend2"
		c.Workspace.Sources = []runtimev3.SourceV3{c.Workspace.Sources[0], second} // two write sources, one task branch
		if _, err := agents.PlanCheckouts(c); err == nil {
			t.Fatal("two sources of one repository were planned onto the same task branch")
		}
	})

	t.Run("branch labels are deterministic ref components", func(t *testing.T) {
		for in, want := range map[string]string{
			"generate": "generate", "Step 1/impl": "Step-1-impl", "..": "agent", "": "agent", "-x-": "x",
			strings.Repeat("a", 80): strings.Repeat("a", 48),
		} {
			if got := agents.BranchLabel(in); got != want {
				t.Errorf("BranchLabel(%q) = %q, want %q", in, got, want)
			}
		}
	})

	plan, err := agents.PlanCheckouts(v3Fixture(t, "context_v3_work_item_writer_two_repositories"))
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]runtimev1.GitCommit{"backend": commitBack, "frontend": commitFront}
	both := map[string]runtimev1.GitCommit{"backend": newBack, "frontend": newFront}

	t.Run("the candidate is the commits of every source, not one commit", func(t *testing.T) {
		c, err := agents.NewCandidateSet(plan, starts, both)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Sources) != 2 || len(c.Changed()) != 2 {
			t.Fatalf("%+v", c)
		}
		if c.Sources[0] != (agents.CandidateSource{RepositoryID: "repo_01", Path: "backend", Ref: taskBranch, Commit: newBack, Changed: true}) ||
			c.Sources[1] != (agents.CandidateSource{RepositoryID: "repo_02", Path: "frontend", Ref: agentBranch, Commit: newFront, Changed: true}) {
			t.Fatalf("%+v", c.Sources)
		}
		d, err := c.Digest()
		if err != nil || !strings.HasPrefix(string(d), "sha256:") || d.Validate() != nil {
			t.Fatalf("%q %v", d, err)
		}
		if c.Primary().Path != "backend" {
			t.Fatalf("the report names the first changed source: %+v", c.Primary())
		}

		// Any part changes the identity: the other repository's commit does not stand in.
		onlyBack, _ := agents.NewCandidateSet(plan, starts, map[string]runtimev1.GitCommit{"backend": newBack, "frontend": commitFront})
		onlyFront, _ := agents.NewCandidateSet(plan, starts, map[string]runtimev1.GitCommit{"backend": commitBack, "frontend": newFront})
		seen := map[runtimev1.Digest]string{d: "both"}
		for name, s := range map[string]agents.CandidateSet{"back": onlyBack, "front": onlyFront} {
			dg, err := s.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := seen[dg]; dup {
				t.Fatalf("%s has the digest of %s", name, prev)
			}
			seen[dg] = name
		}
		if len(onlyBack.Changed()) != 1 || onlyBack.Changed()[0].Path != "backend" || onlyFront.Primary().Path != "frontend" {
			t.Fatalf("changed/primary: %+v %+v", onlyBack, onlyFront)
		}
		again, _ := agents.NewCandidateSet(plan, starts, both)
		if d2, _ := again.Digest(); d2 != d {
			t.Fatal("the digest is not deterministic")
		}
	})

	t.Run("an unchanged source keeps its commit and names no branch", func(t *testing.T) {
		c, err := agents.NewCandidateSet(plan, starts, starts)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Changed()) != 0 || c.Primary().Path != "backend" || c.Sources[1].Ref != "" || c.Sources[1].Commit != commitFront {
			t.Fatalf("%+v", c)
		}
		if err := c.CheckPublished(nil); err != nil {
			t.Fatalf("an unchanged candidate publishes nothing: %v", err)
		}
	})

	t.Run("a candidate is published only when its result outputs are exactly its changed parts", func(t *testing.T) {
		c, _ := agents.NewCandidateSet(plan, starts, both)
		good := []runtimev3.CommitOutput{
			{RepositoryID: "repo_01", Ref: taskBranch, Commit: newBack, PushID: "pu_1"},
			{RepositoryID: "repo_02", Ref: agentBranch, Commit: newFront, PushID: "pu_2"},
		}
		if err := c.CheckPublished(good); err != nil {
			t.Fatal(err)
		}
		for name, outs := range map[string][]runtimev3.CommitOutput{
			"nothing published":      nil,
			"one part missing":       good[:1],
			"a part at another head": {good[0], {RepositoryID: "repo_02", Ref: agentBranch, Commit: commitFront, PushID: "pu_2"}},
			"a part on another ref":  {good[0], {RepositoryID: "repo_02", Ref: "refs/heads/kodama/agents/wi_1042/run_01/other", Commit: newFront, PushID: "pu_2"}},
			"a part in another repo": {good[0], {RepositoryID: "repo_01", Ref: agentBranch, Commit: newFront, PushID: "pu_2"}},
			"an extra push":          append(append([]runtimev3.CommitOutput{}, good...), runtimev3.CommitOutput{RepositoryID: "repo_02", Ref: "refs/heads/kodama/agents/wi_1042/run_01/x", Commit: newFront, PushID: "pu_3"}),
		} {
			if err := c.CheckPublished(outs); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("a candidate cannot name the default branch, a tag, a server ref or a foreign ref", func(t *testing.T) {
		for _, ref := range []string{"refs/heads/main", "refs/heads/release", "refs/tags/v1", "refs/kodama/runs/run_01/1", "refs/heads/kodama/agents/wi_1042/run_01", "refs/heads/kodama/other", ""} {
			c := agents.CandidateSet{Sources: []agents.CandidateSource{{RepositoryID: "repo_01", Path: "backend", Ref: ref, Commit: newBack, Changed: true}}}
			if err := c.Validate(); err == nil {
				t.Errorf("%q accepted", ref)
			}
		}
		dup := agents.CandidateSet{Sources: []agents.CandidateSource{
			{RepositoryID: "repo_01", Path: "a", Ref: agentBranch, Commit: newBack, Changed: true},
			{RepositoryID: "repo_01", Path: "b", Ref: agentBranch, Commit: newFront, Changed: true},
		}}
		if dup.Validate() == nil {
			t.Error("two sources updating one ref were accepted")
		}
		for name, c := range map[string]agents.CandidateSet{
			"empty":              {},
			"unordered":          {Sources: []agents.CandidateSource{{RepositoryID: "r", Path: "b", Commit: newBack}, {RepositoryID: "r", Path: "a", Commit: newBack}}},
			"unchanged with ref": {Sources: []agents.CandidateSource{{RepositoryID: "r", Path: "a", Ref: agentBranch, Commit: newBack}}},
			"bad commit":         {Sources: []agents.CandidateSource{{RepositoryID: "r", Path: "a", Commit: "HEAD"}}},
			"bad path":           {Sources: []agents.CandidateSource{{RepositoryID: "r", Path: "../a", Commit: newBack}}},
			"bad repository":     {Sources: []agents.CandidateSource{{RepositoryID: "r e", Path: "a", Commit: newBack}}},
		} {
			if _, err := c.Digest(); err == nil {
				t.Errorf("%s: a digest was produced", name)
			}
		}
	})

	t.Run("building a candidate needs both commits of every planned source", func(t *testing.T) {
		if _, err := agents.NewCandidateSet(plan, starts, map[string]runtimev1.GitCommit{"backend": newBack}); err == nil {
			t.Error("a missing final commit was accepted")
		}
		if _, err := agents.NewCandidateSet(plan, map[string]runtimev1.GitCommit{"backend": commitBack}, both); err == nil {
			t.Error("a missing starting commit was accepted")
		}
		if _, err := agents.NewCandidateSet(plan, starts, map[string]runtimev1.GitCommit{"backend": "main", "frontend": newFront}); err == nil {
			t.Error("a non-commit was accepted")
		}
		if _, err := agents.NewCandidateSet(nil, starts, both); err == nil {
			t.Error("an empty plan was accepted")
		}
	})
}
