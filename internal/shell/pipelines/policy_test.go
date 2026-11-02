package pipelines

import "testing"

// Test_unit_pipelines_policy pins the ownership decision both adapters share.
func Test_unit_pipelines_policy(t *testing.T) {
	git := func(repo string) Source { return Source{Kind: SourceGit, RepositoryID: repo} }
	api := Source{Kind: SourceAPI}
	held := func(src Source, commit string, disabled bool) *Definition {
		return &Definition{Source: src, Commit: commit, Disabled: disabled}
	}
	for _, tc := range []struct {
		name     string
		existing *Definition
		sub      Submission
		same     bool
		want     Action
	}{
		{"free name, api", nil, Submission{Source: api}, false, ActionCreate},
		{"free name, git", nil, Submission{Source: git("r1"), Commit: "c1"}, false, ActionCreate},
		{"api over api", held(api, "", false), Submission{Source: api}, true, ActionDuplicate},
		{"api over git", held(git("r1"), "c1", false), Submission{Source: api}, false, ActionConflict},
		{"git over api", held(api, "", false), Submission{Source: git("r1"), Commit: "c1"}, false, ActionConflict},
		{"git over other repository", held(git("r1"), "c1", false), Submission{Source: git("r2"), Commit: "c1"}, true, ActionConflict},
		{"same repository, same commit and document", held(git("r1"), "c1", false), Submission{Source: git("r1"), Commit: "c1"}, true, ActionUnchanged},
		{"same repository, new commit", held(git("r1"), "c1", false), Submission{Source: git("r1"), Commit: "c2"}, true, ActionUpdate},
		{"same repository, new document", held(git("r1"), "c1", false), Submission{Source: git("r1"), Commit: "c1"}, false, ActionUpdate},
		{"owner re-enables its disabled definition", held(git("r1"), "c1", true), Submission{Source: git("r1"), Commit: "c1"}, true, ActionUpdate},
		{"a disabled definition still reserves the name", held(git("r1"), "c1", true), Submission{Source: git("r2"), Commit: "c1"}, true, ActionConflict},
	} {
		if got := decide(tc.existing, tc.sub, tc.same); got != tc.want {
			t.Errorf("%s: decide = %d, want %d", tc.name, got, tc.want)
		}
	}
}
