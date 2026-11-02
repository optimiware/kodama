package qa

import (
	"fmt"
	"regexp"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The source set of an attempt (runtime context v3, ADR 0011).
//
// A QA attempt of a run that pinned a source set was admitted with one checkout
// per repository. The deployment it inspects was built from a candidate commit;
// the mapping must not describe a deployment built from a commit the attempt's
// own pinned sources do not hold, and the defect's provenance names the set that
// was inspected. The set comes only from the attempt's frozen context (read by
// the execution adapter, never from the report), so a report cannot choose it.
// A context without a source set (every v1 context) carries none, and nothing
// about the mapping changes for it.

var (
	sourceIDRE   = regexp.MustCompile(domain.IDPattern)
	sourcePathRE = regexp.MustCompile(`^(\.|[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*)$`)
)

// maxSources bounds a source set (the Git contract's bound).
const maxSources = 16

// Source is one repository of the pinned source set: the commit the run pinned
// at admission and the latest commit published to its write branch before this
// attempt (equal to Commit when nothing was).
type Source struct {
	RepositoryID string
	Path         string
	Commit       runtimev1.GitCommit
	HeadCommit   runtimev1.GitCommit
	// Access is the run-level access ("read" or "write").
	Access string
}

// Published is a commit an earlier attempt of the run published (the context's
// lineage): an intermediate commit of one of the set's repositories that is no
// longer any source's head.
type Published struct {
	RepositoryID string
	Commit       runtimev1.GitCommit
}

// SourceSet is the attempt's pinned source set in path order. Digest is the
// canonical digest the run pinned for it. Published lists what earlier attempts of
// the run published to the set's repositories.
type SourceSet struct {
	Digest    runtimev1.Digest
	Sources   []Source
	Published []Published
}

// Empty reports a context without a source set.
func (s SourceSet) Empty() bool { return len(s.Sources) == 0 && s.Digest == "" }

// Validate checks the set's shape. (The path and identifier syntax repeats the Git
// contract's: this package's boundary test allows it to import only the runtime v1
// contract and the shared identifiers, so it cannot reuse gitv1's validators.) The execution adapter reads a validated
// context, so this is a second guard against a malformed projection: an invalid
// set is a context error, never trusted.
func (s SourceSet) Validate() error {
	if s.Empty() {
		return nil
	}
	if len(s.Sources) == 0 || len(s.Sources) > maxSources {
		return fmt.Errorf("a source set lists 1 to %d sources", maxSources)
	}
	if err := s.Digest.Validate(); err != nil {
		return fmt.Errorf("source set digest: %w", err)
	}
	for i, src := range s.Sources {
		switch {
		case !sourceIDRE.MatchString(src.RepositoryID):
			return fmt.Errorf("source %d: invalid repository id", i)
		case !sourcePathRE.MatchString(src.Path) || strings.Contains(src.Path, ".."):
			return fmt.Errorf("source %d: invalid path", i)
		case src.Commit.Validate() != nil || src.HeadCommit.Validate() != nil:
			return fmt.Errorf("source %d: invalid commit", i)
		case src.Access != "read" && src.Access != "write":
			return fmt.Errorf("source %d: invalid access", i)
		case i > 0 && s.Sources[i-1].Path >= src.Path:
			return fmt.Errorf("sources must be in strict path order")
		}
	}
	repos := map[string]bool{}
	for _, src := range s.Sources {
		repos[src.RepositoryID] = true
	}
	for i, p := range s.Published {
		if !repos[p.RepositoryID] || p.Commit.Validate() != nil {
			return fmt.Errorf("published commit %d is not a commit of a repository of the set", i)
		}
	}
	return nil
}

// Holds reports whether the commit is one the set pins or has advanced to: a
// source's pinned commit or its head, or a commit an earlier attempt of the run
// published to one of the set's repositories (an intermediate commit, such as the
// one a deployment of an earlier candidate was built from). It deliberately holds
// nothing else: a commit that was merged elsewhere later (an integration into the
// default branch) is not in the attempt's context and is not recognized.
func (s SourceSet) Holds(c runtimev1.GitCommit) bool {
	for _, src := range s.Sources {
		if src.Commit == c || src.HeadCommit == c {
			return true
		}
	}
	for _, p := range s.Published {
		if p.Commit == c {
			return true
		}
	}
	return false
}
