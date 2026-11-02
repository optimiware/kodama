package gitv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// RootPath is the only path that checks a repository out at /workspace itself.
// A source set holding it holds no other source.
const RootPath = "."

// ValidateSourcePath checks a source checkout path: relative to /workspace,
// clean (no empty, "." or ".." component, no leading or trailing slash, no
// backslash), made of components that begin with a letter, digit or underscore
// (so no ".git" or other hidden component), and at most MaxPathBytes. The single
// character "." is the root path.
func ValidateSourcePath(p string) error {
	if len(p) > MaxPathBytes {
		return fail("path", "longer than %d bytes", MaxPathBytes)
	}
	if err := matches("path", "SourcePath", p); err != nil {
		return err
	}
	if strings.Contains(p, "..") {
		return fail("path", "must not contain \"..\"")
	}
	return nil
}

func (s Source) Validate() error {
	if err := firstError(checkID("repositoryId", s.RepositoryID), checkLiveCommit("commit", s.Commit), ValidateSourcePath(s.Path),
		oneOf("access", string(s.Access), string(AccessRead), string(AccessWrite))); err != nil {
		return err
	}
	switch {
	case s.Access == AccessWrite && s.Branch == nil:
		return fail("branch", "a write source names the branch its writer updates")
	case s.Access == AccessRead && s.Branch != nil:
		return fail("branch", "a read source has no branch (null)")
	case s.Branch != nil:
		return ValidateBranchRef(*s.Branch)
	}
	return nil
}

// overlaps reports whether two paths are equal or one is a directory prefix of
// the other.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// Validate checks every source and the set: 1 to MaxSources sources ordered
// strictly by path (bytewise), paths unique and non-overlapping (neither is a
// directory prefix of another), "." only as the sole entry.
func (s SourceSet) Validate() error {
	if err := checkNonNil("sources", s.Sources); err != nil {
		return err
	}
	if len(s.Sources) < 1 || len(s.Sources) > MaxSources {
		return fail("sources", "must list 1 to %d sources", MaxSources)
	}
	for i, src := range s.Sources {
		if err := src.Validate(); err != nil {
			return wrap(indexed("sources", i), err)
		}
	}
	for i := range s.Sources {
		for j := i + 1; j < len(s.Sources); j++ {
			a, b := s.Sources[i].Path, s.Sources[j].Path
			if a == RootPath || b == RootPath {
				return fail(indexed("sources", j)+".path", "the root path \".\" must be the only source")
			}
			if overlaps(a, b) {
				return fail(indexed("sources", j)+".path", "%q overlaps %q: paths are unique and no path contains another", truncate(b, 64), truncate(a, 64))
			}
		}
	}
	for i := 1; i < len(s.Sources); i++ {
		if s.Sources[i-1].Path >= s.Sources[i].Path {
			return fail(indexed("sources", i)+".path", "sources must be ordered by path")
		}
	}
	return nil
}

// NewSourceSet orders sources by path and validates the result, so one set has
// one encoding and one digest.
func NewSourceSet(sources []Source) (SourceSet, error) {
	s := SourceSet{Sources: append([]Source{}, sources...)}
	sort.SliceStable(s.Sources, func(i, j int) bool { return s.Sources[i].Path < s.Sources[j].Path })
	if err := s.Validate(); err != nil {
		return SourceSet{}, err
	}
	return s, nil
}

// Digest is "sha256:" and the lowercase hex SHA-256 of the canonical JSON of the
// validated set (the runtime canonicalization of ResultHash). Evidence and gate
// subjects identify a multi-repository run by it.
func (s SourceSet) Digest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Find returns the source checked out at path.
func (s SourceSet) Find(path string) (Source, bool) {
	for _, src := range s.Sources {
		if src.Path == path {
			return src, true
		}
	}
	return Source{}, false
}
