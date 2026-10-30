package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// Checkouts and candidates of an agent attempt under runtime context v3
// (AR-11, AR-15, ADR 0011, docs/specs/git-repositories.md).
//
// An agent attempt of a source-set run works in one checkout per source at
// /workspace/<path> ("." is the workspace itself). Its candidate is not one
// commit but the state of every checkout: for each source the commit it ends on
// and, for a changed source, the branch that commit is on. The wrapper commits
// and the step entrypoint publishes (it pushes a checked-out branch only when
// the attempt's own grant selects it); neither the wrapper nor the harness ever
// holds a Git credential or pushes. These types are the pure half of that
// division of labor: which branch a source's commit goes on, and what the
// candidate is. They never touch Git, the network or a grant.

// CheckoutMode says where a source's changes are committed.
type CheckoutMode string

const (
	// ModeHead commits on the checkout's HEAD. It is the writer's own write source:
	// the entrypoint commits and pushes it to the source's task branch with a lease
	// on the expected tip, and the grant of the writer selects exactly that branch.
	ModeHead CheckoutMode = "head"
	// ModeAgentBranch commits on a run-scoped branch the wrapper creates and checks
	// out (refs/heads/kodama/agents/<workItemId>/<runId>/<label>, or
	// refs/heads/kodama/runs/<runId>/<label> for a run without a work item). Every
	// attempt's grant selects its run's namespace, so the entrypoint can push it;
	// a reader's view of the task branch is never one.
	ModeAgentBranch CheckoutMode = "agent_branch"
)

// Checkout is the wrapper's plan for one source of the set.
type Checkout struct {
	RepositoryID string
	// Path is the source's checkout path relative to the workspace mount ("." for the
	// workspace itself).
	Path string
	// Access is the source's run-level access.
	Access gitv1.SourceAccess
	Mode   CheckoutMode
	// Ref is the branch the commit is on once committed: the source's task branch
	// for ModeHead, the agent branch for ModeAgentBranch.
	Ref string
	// Pinned is the commit the run pinned for the source; Head is the latest commit
	// published to its own write branch by this run (equal to Pinned before any).
	Pinned runtimev1.GitCommit
	Head   runtimev1.GitCommit
}

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// BranchLabel derives the branch label of a step: its ID with every character a
// ref component does not take replaced by "-", beginning with a letter or digit
// and at most 48 bytes (leaving room for a per-source suffix). The result is
// deterministic, so a retried or redriven attempt names the same branch.
func BranchLabel(stepID string) string {
	l := strings.Trim(labelUnsafe.ReplaceAllString(stepID, "-"), "-._")
	if len(l) > 48 {
		l = strings.TrimRight(l[:48], "-._")
	}
	if l == "" {
		l = "agent"
	}
	return l
}

// pathLabel derives the label of a later source of a repository already used by
// an earlier one: the step label (cut to leave room) and a short hash of the
// source's path. The hash, not the path text, keeps two paths that differ only in
// characters a label cannot hold (a/b and a-b) or beyond the length bound apart.
func pathLabel(label, path string) string {
	if len(label) > 40 {
		label = strings.TrimRight(label[:40], "-._")
	}
	sum := sha256.Sum256([]byte(path))
	return label + "-" + hex.EncodeToString(sum[:6])
}

// PlanCheckouts plans every source of an attempt's context in path order. The
// branch of an agent branch checkout is derived from the run (never from a
// request or the harness) and must be selected by the attempt's own Git grant
// as the context records it; a source whose branch the grant cannot select makes
// the whole plan fail, so the wrapper never commits work the entrypoint could not
// publish. A source-free run, or a context without Git delivery, has no plan (an
// agent needs a repository).
//
// Two sources of one repository would otherwise name the same agent branch; each
// later one gets a suffix with a short hash of its path, and the plan fails on any
// remaining duplicate (repository, ref), so refs are unique within a repository.
func PlanCheckouts(c runtimev3.ContextV3) ([]Checkout, error) {
	if len(c.Workspace.Sources) == 0 || c.Git == nil {
		return nil, errors.New("an agent step needs a source-backed run with Git delivery")
	}
	if c.Workspace.Access == runtimev3.AccessNone {
		return nil, errors.New("an agent step needs workspace access")
	}
	label := BranchLabel(c.StepID)
	writer := c.Workspace.Access == runtimev3.AccessWrite
	used := map[string]bool{}
	refs := map[string]bool{}
	var plan []Checkout
	for _, s := range c.Workspace.Sources {
		co := Checkout{RepositoryID: s.RepositoryID, Path: s.Path, Access: s.Access, Pinned: runtimev1.GitCommit(s.Commit), Head: s.HeadCommit}
		if writer && s.Access == gitv1.AccessWrite && s.Branch != nil {
			co.Mode, co.Ref = ModeHead, *s.Branch
		} else {
			l := label
			if used[s.RepositoryID] {
				l = pathLabel(label, s.Path)
			}
			ref, err := agentBranchOf(c, l)
			if err != nil {
				return nil, fmt.Errorf("source %q: %w", s.Path, err)
			}
			co.Mode, co.Ref = ModeAgentBranch, ref
		}
		used[s.RepositoryID] = true
		if !grantSelects(c, co.RepositoryID, co.Ref) {
			return nil, fmt.Errorf("source %q: the attempt's Git grant does not select %s", s.Path, co.Ref)
		}
		// One repository, one ref, one source: two checkouts that would commit to the
		// same branch of the same repository would publish one over the other.
		key := co.RepositoryID + "\x00" + co.Ref
		if refs[key] {
			return nil, fmt.Errorf("source %q: another source of the repository already commits to %s", s.Path, co.Ref)
		}
		refs[key] = true
		plan = append(plan, co)
	}
	return plan, nil
}

func agentBranchOf(c runtimev3.ContextV3, label string) (string, error) {
	if c.Origin.Kind == runtimev3.OriginWorkItem && c.Origin.WorkItemID != nil {
		return gitv1.AgentBranch(string(*c.Origin.WorkItemID), string(c.RunID), label)
	}
	return gitv1.RunBranch(string(c.RunID), label)
}

// grantSelects reports whether the attempt's own grant, as the context records
// it, allows updating ref in the repository.
func grantSelects(c runtimev3.ContextV3, repositoryID, ref string) bool {
	if c.Git == nil {
		return false
	}
	for _, r := range c.Git.Repositories {
		if r.RepositoryID != repositoryID {
			continue
		}
		for _, p := range r.Update {
			if gitv1.MatchRefPattern(p, ref) {
				return true
			}
		}
	}
	return false
}

// CandidateFormat identifies the canonical form a candidate digest covers.
const CandidateFormat = "kodama/agent-candidate/v1"

// CandidateSource is one checkout's part of a candidate: the commit it ends on.
// Ref is the branch the commit is on for a changed source and empty for a source
// the attempt did not change (its commit is the head it started from).
type CandidateSource struct {
	RepositoryID string              `json:"repositoryId"`
	Path         string              `json:"path"`
	Ref          string              `json:"ref"`
	Commit       runtimev1.GitCommit `json:"commit"`
	Changed      bool                `json:"changed"`
}

// CandidateSet is the candidate of an attempt in a source-set run: one entry per
// source in path order. It identifies a multi-checkout change by the commits of
// all its parts, so a change in one repository is never described by another's
// commit.
type CandidateSet struct {
	Sources []CandidateSource `json:"sources"`
}

// ErrCandidate is the class of an invalid or unpublished candidate.
var ErrCandidate = errors.New("agents: invalid candidate")

func candidatef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCandidate}, args...)...)
}

// NewCandidateSet builds the candidate of the planned checkouts, given the
// commit every checkout started on and the commit it ends on (both by path,
// measured from the checkouts, never from the harness's account). A checkout is
// Changed exactly when its final commit is not the one it started on, and then
// carries the branch the plan chose for it. Every plan entry needs both commits.
// Entries are ordered by path.
func NewCandidateSet(plan []Checkout, starts, ends map[string]runtimev1.GitCommit) (CandidateSet, error) {
	if len(plan) == 0 {
		return CandidateSet{}, candidatef("no sources")
	}
	set := CandidateSet{Sources: make([]CandidateSource, 0, len(plan))}
	for _, co := range plan {
		start, sok := starts[co.Path]
		end, eok := ends[co.Path]
		if !sok || !eok {
			return CandidateSet{}, candidatef("source %q has no start or final commit", co.Path)
		}
		if err := firstErr(start.Validate(), end.Validate()); err != nil {
			return CandidateSet{}, candidatef("source %q: %v", co.Path, err)
		}
		cs := CandidateSource{RepositoryID: co.RepositoryID, Path: co.Path, Commit: end}
		if end != start {
			cs.Changed, cs.Ref = true, co.Ref
		}
		set.Sources = append(set.Sources, cs)
	}
	sort.SliceStable(set.Sources, func(i, j int) bool { return set.Sources[i].Path < set.Sources[j].Path })
	return set, set.Validate()
}

// Validate checks the structure: sources in strict path order, one ref per
// repository, a branch exactly for a changed source, and only refs of the Kodama
// task, agent or run classes (never the default branch, a tag, a foreign ref or a
// server ref).
func (c CandidateSet) Validate() error {
	if len(c.Sources) == 0 {
		return candidatef("no sources")
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if err := gitvID(s.RepositoryID); err != nil {
			return candidatef("source %d: %v", i, err)
		}
		if err := gitv1.ValidateSourcePath(s.Path); err != nil {
			return candidatef("source %d: %v", i, err)
		}
		if i > 0 && c.Sources[i-1].Path >= s.Path {
			return candidatef("sources must be in strict path order")
		}
		if err := s.Commit.Validate(); err != nil {
			return candidatef("source %d: %v", i, err)
		}
		switch {
		case s.Changed && s.Ref == "":
			return candidatef("source %q changed without a branch", s.Path)
		case !s.Changed && s.Ref != "":
			return candidatef("source %q did not change but names a branch", s.Path)
		case !s.Changed:
			continue
		}
		if class, err := gitv1.ClassifyRef("main", s.Ref); err != nil || (class.Class != gitv1.ClassTask && class.Class != gitv1.ClassAgent && class.Class != gitv1.ClassRun) {
			return candidatef("source %q names a ref outside the task, agent and run branches", s.Path)
		}
		key := s.RepositoryID + "\x00" + s.Ref
		if seen[key] {
			return candidatef("two sources update %s of one repository", s.Ref)
		}
		seen[key] = true
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func gitvID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("repository id %q is not an identifier", id)
	}
	return nil
}

// Digest identifies the candidate: the SHA-256 of the canonical form of its
// validated sources, so a different commit, branch or source changes it.
func (c CandidateSet) Digest() (runtimev1.Digest, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Format  string            `json:"format"`
		Sources []CandidateSource `json:"sources"`
	}{CandidateFormat, c.Sources})
	if err != nil {
		return "", err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Primary is the source whose commit the conformance report names: the first
// changed source in path order, or the first source when nothing changed. The
// conformance contract binds one commit; the other parts are bound through
// Digest and the result's commit outputs (see README.md, open limit).
func (c CandidateSet) Primary() CandidateSource {
	for _, s := range c.Sources {
		if s.Changed {
			return s
		}
	}
	return c.Sources[0]
}

// Changed lists the changed sources in path order.
func (c CandidateSet) Changed() []CandidateSource {
	var out []CandidateSource
	for _, s := range c.Sources {
		if s.Changed {
			out = append(out, s)
		}
	}
	return out
}

// CheckPublished verifies that a result's commit outputs publish exactly the
// changed parts of the candidate: every changed source's (repository, ref,
// commit) is one output, and no output is outside the candidate. A candidate
// with an unpublished or substituted part has not been delivered, and a prior
// attempt's or another source's push is not this candidate's.
func (c CandidateSet) CheckPublished(outputs []runtimev3.CommitOutput) error {
	if err := c.Validate(); err != nil {
		return err
	}
	type key struct{ repo, ref string }
	have := map[key]runtimev1.GitCommit{}
	for _, o := range outputs {
		have[key{o.RepositoryID, o.Ref}] = o.Commit
	}
	want := map[key]bool{}
	for _, s := range c.Changed() {
		k := key{s.RepositoryID, s.Ref}
		want[k] = true
		got, ok := have[k]
		if !ok {
			return candidatef("source %q was not published to %s", s.Path, s.Ref)
		}
		if got != s.Commit {
			return candidatef("source %q was published at another commit than the candidate's", s.Path)
		}
	}
	for k := range have {
		if !want[k] {
			return candidatef("a published ref is not part of the candidate")
		}
	}
	return nil
}
