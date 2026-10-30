package adapters

import (
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

func TestPrimaryOfASourceSet(t *testing.T) {
	src := func(id string, access gitv1.SourceAccess) runtimev3.SourceV3 {
		return runtimev3.SourceV3{Source: gitv1.Source{RepositoryID: id, Access: access}}
	}
	r, w := gitv1.AccessRead, gitv1.AccessWrite
	for name, tc := range map[string]struct {
		in   []runtimev3.SourceV3
		want int
	}{
		"the writable source, not the smallest ID":       {[]runtimev3.SourceV3{src("repo_a", r), src("repo_b", w)}, 1},
		"the smallest ID of several writable sources":    {[]runtimev3.SourceV3{src("repo_c", w), src("repo_a", w), src("repo_b", w)}, 1},
		"the smallest ID when none is writable":          {[]runtimev3.SourceV3{src("repo_c", r), src("repo_a", r)}, 1},
		"a writable source in path order beyond the end": {[]runtimev3.SourceV3{src("repo_b", r), src("repo_c", r), src("repo_d", w)}, 2},
		"one source": {[]runtimev3.SourceV3{src("repo_a", r)}, 0},
	} {
		if got := primaryOf(tc.in); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}
