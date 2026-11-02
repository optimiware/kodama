package pipeline

import (
	"regexp"
	"strconv"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// Source declarations of a kodama/v1alpha3 document (ADR 0011). The document
// names repositories and checkout paths; it never selects a commit or a branch,
// and it never resolves a name: run admission maps each name to a repository of
// the run's project, pins one commit per source and builds the run's
// gitv1.SourceSet. The path rules are the Git contract's own
// (gitv1.ValidateSourcePath), so a declaration the document accepts can become
// a valid SourceSet entry.

// maxSources is the Git contract's bound on one run's source set.
const maxSources = gitv1.MaxSources

var sourceRepositoryRE = regexp.MustCompile(gitv1.Patterns["RepositoryName"])

// sourcesPath is the document path of source i.
func sourcesPath(i int) string { return "$.spec.sources[" + strconv.Itoa(i) + "]" }

// pathsOverlap reports whether two checkout paths are equal or one is a
// directory prefix of the other.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// checkSourcesShape enforces the rules of the source declarations that need no
// other part of the document: bounds, names, paths, accesses, uniqueness and
// non-overlap. It is a syntax-stage rule set (the schema states what it can and
// this states the rest) and runs on every document built in code too. Absent
// sources (nil) are the single default source and are never an error.
func checkSourcesShape(sources []Source, d *diagnostics) {
	if sources == nil {
		return
	}
	if len(sources) < 1 || len(sources) > maxSources {
		d.add(CodeLimitExceeded, "$.spec.sources", "a pipeline declares between 1 and %d sources", maxSources)
		return
	}
	byRepository := map[string]int{}
	for i, s := range sources {
		p := sourcesPath(i)
		if !sourceRepositoryRE.MatchString(s.Repository) {
			d.add(CodeInvalidFormat, p+".repository", "the repository is a lowercase name of letters, digits and hyphens, at most 63 characters")
		} else if _, dup := byRepository[s.Repository]; dup {
			d.add(CodeDuplicateSourceRepository, p+".repository", "the repository is already declared by another source")
		} else {
			byRepository[s.Repository] = i
		}
		if err := gitv1.ValidateSourcePath(s.Path); err != nil {
			d.add(CodeInvalidSourcePath, p+".path", "%s", v2Message(CodeInvalidSourcePath))
		}
		switch s.Access {
		case SourceRead, SourceWrite:
		default:
			d.add(CodeInvalidValue, p+".access", "source access must be read or write")
		}
	}
	// Overlap is judged pairwise on the paths that are well formed, in order, so
	// one finding names the later of two conflicting sources.
	for j := 1; j < len(sources); j++ {
		b := sources[j].Path
		if gitv1.ValidateSourcePath(b) != nil {
			continue
		}
		for i := 0; i < j; i++ {
			a := sources[i].Path
			if gitv1.ValidateSourcePath(a) != nil {
				continue
			}
			if a == RootSourcePath || b == RootSourcePath || pathsOverlap(a, b) {
				d.add(CodeSourcePathOverlap, sourcesPath(j)+".path", "source paths are unique, no path contains another, and . is the only source when it is used")
				break
			}
		}
	}
}

// checkSourceWriters relates declared sources to the steps that write the
// workspace. A write step needs a write source to publish to, and a write source
// needs a write step, otherwise the declaration promises authority nobody can
// use. The doc must be normalized. A document without sources states nothing
// here: its implicit single source follows the steps.
func checkSourceWriters(doc Document, d *diagnostics) {
	if doc.Spec.Sources == nil {
		return
	}
	firstWriter, firstWrite := -1, -1
	for i, s := range doc.Spec.Steps {
		if s.IsWriter() {
			firstWriter = i
			break
		}
	}
	for i, s := range doc.Spec.Sources {
		if s.Access == SourceWrite {
			firstWrite = i
			break
		}
	}
	switch {
	case firstWriter >= 0 && firstWrite < 0:
		d.add(CodeSourceWriteUndeclared, stepPath(firstWriter)+".workspace.access", "a step writes the workspace but no source is declared with write access")
	case firstWriter < 0 && firstWrite >= 0:
		d.add(CodeSourceWriteUnused, sourcesPath(firstWrite)+".access", "a source is declared with write access but no step writes the workspace")
	}
}

// EffectiveSources returns the source declarations of a document as run
// admission applies them. A document that declares sources returns them in
// declaration order with implicit false. A document without sources (every
// kodama/v1alpha1 and kodama/v1alpha2 document, and a kodama/v1alpha3 document
// that omits them) has exactly one implicit source: the project's default
// repository, checked out at "." with Repository empty (the caller supplies the
// default), and with write access exactly when a step writes the workspace. The
// canonical form never carries this default, so the digest is the same for every
// project. The document should be validated.
func EffectiveSources(doc Document) (sources []Source, implicit bool) {
	if len(doc.Spec.Sources) > 0 {
		return append([]Source{}, doc.Spec.Sources...), false
	}
	access := SourceRead
	for _, s := range doc.Spec.Steps {
		if s.IsWriter() {
			access = SourceWrite
			break
		}
	}
	return []Source{{Path: RootSourcePath, Access: access}}, true
}
