package gates

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Source sets in gates (ADR 0011, DQ-17, GR-12). A candidate that spans several
// repositories is identified by the run's source set: its canonical digest and,
// for every repository, the commit under review. A repository the run may write
// also names its integration base, the default-branch tip the approval was given
// over. A single-repository subject keeps candidateCommit and its canonical
// bytes exactly as before.

// MaxSources bounds the repositories of one subject (the Git contract's bound).
const MaxSources = 32

var (
	refPattern = regexp.MustCompile(`^refs/heads/[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)
	// repoIDPattern is the opaque identifier syntax of a repository.
	repoIDPattern = regexp.MustCompile(domain.IDPattern)
)

// Tip is the current commit of a repository's default branch.
type Tip struct {
	// Ref is the full ref name (refs/heads/<default branch>).
	Ref    string
	Commit string
}

// Base is the integration base of one write source: the default branch and the
// tip the approval was given over.
type Base struct {
	Ref    string
	Commit string
}

// SubjectSource is one repository of a source-set candidate: the commit under
// review and, for a source the run may write, the integration base. A source the
// run only reads has no base.
type SubjectSource struct {
	RepositoryID string
	Commit       string
	Base         *Base
}

// SourceSetSubject is the source-set part of a subject: the digest of the run's
// pinned set and every repository with its candidate, ordered by repository ID.
type SourceSetSubject struct {
	Digest  string
	Sources []SubjectSource
}

// NewSourceSetSubject validates and orders a source-set subject: a digest, 1 to
// MaxSources sources, each repository once, a full candidate commit each and a
// well-formed base where one is named.
func NewSourceSetSubject(digest string, sources []SubjectSource) (SourceSetSubject, error) {
	if err := validDigest("source set digest", digest); err != nil {
		return SourceSetSubject{}, err
	}
	if len(sources) < 1 || len(sources) > MaxSources {
		return SourceSetSubject{}, invalidf("a source set subject names 1 to %d repositories", MaxSources)
	}
	out := make([]SubjectSource, len(sources))
	for i, s := range sources {
		out[i] = s
		if s.Base != nil {
			b := *s.Base
			out[i].Base = &b
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepositoryID < out[j].RepositoryID })
	for i, s := range out {
		if !repoIDPattern.MatchString(s.RepositoryID) {
			return SourceSetSubject{}, invalidf("a source needs a repository ID")
		}
		if i > 0 && out[i-1].RepositoryID == s.RepositoryID {
			return SourceSetSubject{}, invalidf("a source set subject names a repository once")
		}
		if err := validCommit("source candidate commit", s.Commit); err != nil {
			return SourceSetSubject{}, err
		}
		if s.Base != nil {
			if !refPattern.MatchString(s.Base.Ref) || len(s.Base.Ref) > 255 {
				return SourceSetSubject{}, invalidf("an integration base names a branch")
			}
			if err := validCommit("integration base commit", s.Base.Commit); err != nil {
				return SourceSetSubject{}, err
			}
		}
	}
	return SourceSetSubject{Digest: digest, Sources: out}, nil
}

// Primary is the source a single-commit view of the candidate presents: the first
// writable source in canonical (repository ID) order, else the first source. It is
// a presentation for clients that read only candidateCommit; the identity of the
// subject is the whole set.
func (s SourceSetSubject) Primary() SubjectSource {
	for _, x := range s.Sources {
		if x.Base != nil {
			return x
		}
	}
	return s.Sources[0]
}

// Source returns the entry of a repository.
func (s SourceSetSubject) Source(repository string) (SubjectSource, bool) {
	for _, x := range s.Sources {
		if x.RepositoryID == repository {
			return x, true
		}
	}
	return SubjectSource{}, false
}

func (s SourceSetSubject) clone() SourceSetSubject {
	out := SourceSetSubject{Digest: s.Digest, Sources: make([]SubjectSource, len(s.Sources))}
	for i, x := range s.Sources {
		out.Sources[i] = x
		if x.Base != nil {
			b := *x.Base
			out.Sources[i].Base = &b
		}
	}
	return out
}

// SetSource is one repository of the candidate the run currently has.
type SetSource struct {
	RepositoryID string
	// Commit is the commit under review: the pinned commit advanced by what the
	// writers before the gate published to the source's own branch.
	Commit string
	// Write is true for a source the run may write: its default branch is an
	// integration target.
	Write bool
}

// SetInputs is the source-set candidate of a gate step as the run's immutable
// records resolve it now: the pinned set's digest and the commit of each
// repository, ordered by repository ID. It is a pure function of the run.
type SetInputs struct {
	Digest  string
	Sources []SetSource
}

// Writes lists the repositories the run may write, in repository order.
func (s SetInputs) Writes() []string {
	var out []string
	for _, x := range s.Sources {
		if x.Write {
			out = append(out, x.RepositoryID)
		}
	}
	return out
}

// Commits is the candidate commit of every repository, by repository ID.
func (s SetInputs) Commits() map[string]string {
	out := make(map[string]string, len(s.Sources))
	for _, x := range s.Sources {
		out[x.RepositoryID] = x.Commit
	}
	return out
}

func (s SetInputs) equal(o SetInputs) bool {
	if s.Digest != o.Digest || len(s.Sources) != len(o.Sources) {
		return false
	}
	for i := range s.Sources {
		if s.Sources[i] != o.Sources[i] {
			return false
		}
	}
	return true
}

// matches reports whether the subject names exactly this candidate: the same
// set, the same commit of every repository and a base for exactly the sources
// the run may write.
func (s SourceSetSubject) matches(in SetInputs) bool {
	if s.Digest != in.Digest || len(s.Sources) != len(in.Sources) {
		return false
	}
	for i, x := range s.Sources {
		if x.RepositoryID != in.Sources[i].RepositoryID || x.Commit != in.Sources[i].Commit || (x.Base != nil) != in.Sources[i].Write {
			return false
		}
	}
	return true
}

// IsSourceSetRun reports whether the run executes a source set and so is gated
// by source set subjects.
func IsSourceSetRun(agg *execution.Aggregate) bool {
	return agg != nil && agg.Run != nil && agg.Run.Sources != nil && !agg.Run.Sources.Empty()
}

// ResolveInputs resolves the subject material of a gate step. For a run without
// a source set it is exactly Aggregate.GateInputs (the returned set is nil), so
// every earlier subject is built from the same facts as before. For a work run
// with a source set it resolves the same step-local material (work item, content
// revision, definition, evidence and deployment references, the producing
// writer) and the source-set candidate: the pinned commit of each repository
// advanced by the commits the successful attempts before the gate published to
// that source's own branch. Execution's GateInputs needs one source commit,
// which a source-set run does not have, so the shared parts are resolved over a
// view of the aggregate whose source commit stands in; nothing of it reaches the
// result, and the aggregate itself is never changed.
func ResolveInputs(agg *execution.Aggregate, stepID string) (execution.GateInputs, *SetInputs, error) {
	if !IsSourceSetRun(agg) {
		in, err := agg.GateInputs(stepID)
		return in, nil, err
	}
	run := agg.Run
	if run.Purpose != execution.PurposeWork || run.WorkItemID == "" {
		return execution.GateInputs{}, nil, fmt.Errorf("%w: a gate needs a work run of a work item", execution.ErrInvalid)
	}
	placeholder := run.Sources.Entries[0].Commit
	view := *agg
	vr := *run
	vr.SourceCommit = placeholder
	view.Run = &vr
	view.Steps = make([]*execution.StepRun, len(agg.Steps))
	for i, st := range agg.Steps {
		c := *st
		if c.Spec.IsWriter() && c.Status == execution.StepSucceeded && c.OutputCommit == "" {
			c.OutputCommit = placeholder
		}
		view.Steps[i] = &c
	}
	in, err := view.GateInputs(stepID)
	if err != nil {
		return execution.GateInputs{}, nil, err
	}
	in.Candidate.Commit = ""

	// The steps before the gate, in definition order: the commits their accepted
	// attempts published advance the sources' heads.
	ancestors := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		st := agg.Step(id)
		if st == nil {
			return
		}
		for _, n := range st.Spec.Needs {
			if !ancestors[n] {
				ancestors[n] = true
				walk(n)
			}
		}
	}
	walk(stepID)
	var before []*execution.StepRun
	for _, st := range agg.Steps {
		if ancestors[st.ID] {
			before = append(before, st)
		}
	}
	sort.Slice(before, func(i, j int) bool { return before[i].Ordinal < before[j].Ordinal })

	heads := make(map[string]string, len(run.Sources.Entries))
	for _, e := range run.Sources.Entries {
		heads[e.RepositoryID] = e.Commit
	}
	for _, st := range before {
		for _, at := range agg.AttemptsOf(st.ID) {
			if at.Status != execution.AttemptSucceeded {
				continue
			}
			applyPublished(run.Sources.Entries, heads, at.Commits)
		}
	}
	set := &SetInputs{Digest: run.Sources.Digest}
	for _, e := range run.Sources.Entries {
		set.Sources = append(set.Sources, SetSource{RepositoryID: e.RepositoryID, Commit: heads[e.RepositoryID], Write: e.Access == "write"})
	}
	sort.Slice(set.Sources, func(i, j int) bool { return set.Sources[i].RepositoryID < set.Sources[j].RepositoryID })
	return in, set, nil
}

// applyPublished advances the heads by the commits an attempt reported: only a
// commit on a source's own write branch moves its head, as in the run's own
// lineage (an agent branch push or a read source never does).
func applyPublished(entries []execution.RunSource, heads map[string]string, outs []execution.CommitOutput) {
	for _, out := range outs {
		for _, e := range entries {
			if e.Branch != nil && e.RepositoryID == out.RepositoryID && *e.Branch == out.Ref {
				heads[e.RepositoryID] = out.Commit
			}
		}
	}
}

// ---- ports -----------------------------------------------------------------------

// Bases reads the current tip of repositories' default branches (the integration
// bases). It reaches the Git service, so it is never called inside a SQL
// transaction: a gate is requested, decided and its integration approval read
// with the tips observed first, and Git's own expected-old check at the push
// stays the final protection (GR-12).
//
// DefaultTips answers one entry per requested repository of the scope's project.
// An unknown, foreign, archived or not internally hosted repository is
// ErrNotFound; an outage is any other error and never an answer.
type Bases interface {
	DefaultTips(ctx context.Context, scope domain.Scope, repositories []string) (map[string]Tip, error)
}

// tips observes the default-branch tips of the repositories. A missing port is
// ErrNotConfigured, a missing or malformed answer is ErrEvidence and an outage
// is ErrUnavailable: nothing is ever invented.
func (s *Service) tips(ctx context.Context, scope domain.Scope, repositories []string) (map[string]Tip, error) {
	if len(repositories) == 0 {
		return map[string]Tip{}, nil
	}
	if s.d.Bases == nil {
		return nil, fmt.Errorf("%w: a base reader is required for a source set with a write source", ErrNotConfigured)
	}
	got, err := s.d.Bases.DefaultTips(ctx, scope, repositories)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%w: a repository of the source set has no integration base", ErrEvidence)
	case err != nil && IsBusinessError(err):
		return nil, err
	case err != nil:
		return nil, Unavailablef(err, "the default branch tips are unavailable")
	}
	out := make(map[string]Tip, len(repositories))
	for _, r := range repositories {
		t, ok := got[r]
		if !ok || !refPattern.MatchString(t.Ref) || validCommit("default branch tip", t.Commit) != nil {
			return nil, fmt.Errorf("%w: a repository of the source set has no integration base", ErrEvidence)
		}
		out[r] = t
	}
	return out, nil
}
