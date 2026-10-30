package gitv1

import (
	"strings"
	"testing"
)

// Test_unit_git_contract_branch_namespace: the branch and ref namespace every
// consumer shares, including the branches of a run without a work item.
func Test_unit_git_contract_branch_namespace(t *testing.T) {
	t.Run("builders", func(t *testing.T) {
		for name, tc := range map[string]struct {
			got  func() (string, error)
			want string
		}{
			"task branch":     {func() (string, error) { return TaskBranch("wi_1042") }, "refs/heads/kodama/work/wi_1042"},
			"agent branch":    {func() (string, error) { return AgentBranch("wi_1042", "run_01", "impl") }, "refs/heads/kodama/agents/wi_1042/run_01/impl"},
			"agent pattern":   {func() (string, error) { return AgentBranchPattern("wi_1042", "run_01") }, "refs/heads/kodama/agents/wi_1042/run_01/*"},
			"run branch":      {func() (string, error) { return RunBranch("run_01", "provision") }, "refs/heads/kodama/runs/run_01/provision"},
			"run pattern":     {func() (string, error) { return RunBranchPattern("run_01") }, "refs/heads/kodama/runs/run_01/*"},
			"retained ref":    {func() (string, error) { return RetainedRef("run_01", 12) }, "refs/kodama/runs/run_01/12"},
			"label with dots": {func() (string, error) { return RunBranch("run_01", "v1.2-final") }, "refs/heads/kodama/runs/run_01/v1.2-final"},
		} {
			got, err := tc.got()
			if err != nil || got != tc.want {
				t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
			}
			if err == nil {
				if verr := ValidateRefName(strings.TrimSuffix(got, "/*")); verr != nil {
					t.Errorf("%s: %q is not a valid ref name: %v", name, got, verr)
				}
			}
		}
		for name, fn := range map[string]func() (string, error){
			"task with slash":        func() (string, error) { return TaskBranch("wi/1") },
			"task empty":             func() (string, error) { return TaskBranch("") },
			"agent label with slash": func() (string, error) { return AgentBranch("wi_1", "run_01", "a/b") },
			"agent label traversal":  func() (string, error) { return AgentBranch("wi_1", "run_01", "..") },
			"agent label dotdot":     func() (string, error) { return AgentBranch("wi_1", "run_01", "a..b") },
			"agent label lock":       func() (string, error) { return AgentBranch("wi_1", "run_01", "x.lock") },
			"agent label empty":      func() (string, error) { return AgentBranch("wi_1", "run_01", "") },
			"run branch label":       func() (string, error) { return RunBranch("run_01", "-x") },
			"run id with slash":      func() (string, error) { return RunBranch("run/01", "x") },
			"retained zero":          func() (string, error) { return RetainedRef("run_01", 0) },
			"retained negative":      func() (string, error) { return RetainedRef("run_01", -1) },
			"retained run id":        func() (string, error) { return RetainedRef("", 1) },
		} {
			if got, err := fn(); err == nil {
				t.Errorf("%s accepted as %q", name, got)
			}
		}
	})

	t.Run("retained refs round trip", func(t *testing.T) {
		for _, n := range []int64{1, 9, 10, 12345, MaxSafeInteger} {
			ref, err := RetainedRef("run_01", n)
			if err != nil {
				t.Fatal(err)
			}
			run, got, err := ParseRetainedRef(ref)
			if err != nil || run != "run_01" || got != n {
				t.Errorf("%s: %q %d %v", ref, run, got, err)
			}
		}
		for _, bad := range []string{"refs/kodama/runs/run_01", "refs/kodama/runs/run_01/", "refs/kodama/runs/run_01/0", "refs/kodama/runs/run_01/01", "refs/kodama/runs/run_01/1/2",
			"refs/kodama/runs/run_01/x", "refs/kodama/runs/run_01/-1", "refs/kodama/runs/run_01/9007199254740992", "refs/heads/kodama/runs/run_01/1", "refs/kodama/runs//1", "refs/kodama/other/1"} {
			if _, _, err := ParseRetainedRef(bad); err == nil {
				t.Errorf("%q accepted", bad)
			}
		}
	})

	t.Run("classification", func(t *testing.T) {
		for _, tc := range []struct {
			def, ref string
			want     Ref
		}{
			{"main", "refs/heads/main", Ref{Class: ClassDefault}},
			{"trunk", "refs/heads/trunk", Ref{Class: ClassDefault}},
			{"trunk", "refs/heads/main", Ref{Class: ClassBranch}},
			{"release/1", "refs/heads/release/1", Ref{Class: ClassDefault}},
			{"main", "refs/heads/kodama/work/wi_1042", Ref{Class: ClassTask, WorkItemID: "wi_1042"}},
			{"main", "refs/heads/kodama/agents/wi_1042/run_01/impl", Ref{Class: ClassAgent, WorkItemID: "wi_1042", RunID: "run_01", Label: "impl"}},
			{"main", "refs/heads/kodama/runs/run_01/provision", Ref{Class: ClassRun, RunID: "run_01", Label: "provision"}},
			{"main", "refs/heads/feature/x", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/work/wi/1", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/work", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/agents/wi_1042/impl", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/agents/wi_1042/run_01/a/b", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/runs/run_01", Ref{Class: ClassBranch}},
			{"main", "refs/heads/kodama/other/x", Ref{Class: ClassBranch}},
			{"main", "refs/tags/v1", Ref{Class: ClassTag}},
			{"main", "refs/tags/kodama/work/wi_1", Ref{Class: ClassTag}},
			{"main", "refs/kodama/runs/run_01/1", Ref{Class: ClassServer}},
			{"main", "refs/kodama/anything", Ref{Class: ClassServer}},
			{"main", "refs/notes/commits", Ref{Class: ClassOther}},
			{"main", "refs/changes/01/1", Ref{Class: ClassOther}},
		} {
			got, err := ClassifyRef(tc.def, tc.ref)
			if err != nil || got != tc.want {
				t.Errorf("ClassifyRef(%q, %q) = %+v, %v; want %+v", tc.def, tc.ref, got, err, tc.want)
			}
		}
		for _, tc := range [][2]string{{"main", "main"}, {"main", "refs/heads/a..b"}, {"main", "HEAD"}, {"refs/heads/main", "refs/heads/main"}, {"kodama", "refs/heads/kodama"}, {"main", ""}} {
			if got, err := ClassifyRef(tc[0], tc[1]); err == nil {
				t.Errorf("ClassifyRef(%q, %q) = %+v", tc[0], tc[1], got)
			}
		}
	})

	t.Run("built refs classify as built", func(t *testing.T) {
		agent, _ := AgentBranch("wi_1", "run_02", "x")
		task, _ := TaskBranch("wi_1")
		run, _ := RunBranch("run_02", "x")
		ret, _ := RetainedRef("run_02", 3)
		for ref, class := range map[string]RefClass{agent: ClassAgent, task: ClassTask, run: ClassRun, ret: ClassServer} {
			if got, err := ClassifyRef("main", ref); err != nil || got.Class != class {
				t.Errorf("%s: %+v %v", ref, got, err)
			}
		}
		pattern, _ := AgentBranchPattern("wi_1", "run_02")
		if !MatchRefPattern(pattern, agent) {
			t.Error("the agent pattern does not select the run's agent branch")
		}
		other, _ := AgentBranch("wi_1", "run_03", "x")
		if MatchRefPattern(pattern, other) {
			t.Error("the agent pattern selects another run's branch")
		}
		runPattern, _ := RunBranchPattern("run_02")
		if !MatchRefPattern(runPattern, run) || MatchRefPattern(runPattern, agent) {
			t.Error("run pattern")
		}
	})

	t.Run("ref names", func(t *testing.T) {
		for _, ok := range []string{"refs/heads/main", "refs/tags/v1.0", "refs/kodama/runs/run_01/1", "refs/heads/a/b/c", "refs/notes/commits"} {
			if err := ValidateRefName(ok); err != nil {
				t.Errorf("%q: %v", ok, err)
			}
		}
		for _, bad := range []string{"", "refs", "refs/", "refs/heads", "heads/main", "refs/heads/", "refs/heads//a", "refs/heads/a/", "refs/heads/a..b", "refs/heads/.a",
			"refs/heads/a.lock", "refs/heads/a.lock/b", "refs/heads/a.", "refs/heads/a b", "refs/heads/a~", "refs/heads/a^", "refs/heads/a:b", "refs/heads/a?", "refs/heads/a*",
			"refs/heads/a[", "refs/heads/a\\b", "refs/heads/a@{1}", "refs/heads/-a", "refs/heads/a\x7f", "refs/heads/é", "refs/heads/" + strings.Repeat("a", 250)} {
			if err := ValidateRefName(bad); err == nil {
				t.Errorf("%q accepted", bad)
			}
		}
		if err := ValidateBranchRef("refs/tags/v1"); err == nil {
			t.Error("a tag accepted as a branch ref")
		}
	})
}
