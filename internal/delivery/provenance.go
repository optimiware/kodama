package delivery

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	repositoryURL = regexp.MustCompile(runtimev1.Patterns["RepositoryURL"])
	gitCommit     = regexp.MustCompile(runtimev1.Patterns["GitCommit"])
	toolImageRef  = regexp.MustCompile(runtimev1.Patterns["ImageRef"])
	digestSyntax  = regexp.MustCompile(runtimev1.Patterns["Digest"])
)

// MaxSourceCommits bounds one source set (the Git contract allows 32 sources).
const MaxSourceCommits = 32

// SourceCommit is one repository of a source set at one commit.
type SourceCommit struct {
	RepositoryID string `json:"repositoryId"`
	Commit       string `json:"commit"`
}

// SourceSet identifies the candidate of a multi-repository run (DQ-17): the
// canonical digest of the run's pinned source set and one commit for every
// repository of it, ordered by repository ID. It stands in place of a single
// repository URL and commit. The zero value is "no source set". A SourceSet is
// comparable: its commits are held in one canonical encoding, so two sets are
// equal exactly when their digests and commits are.
type SourceSet struct {
	Digest  string
	commits string
}

var repositoryID = regexp.MustCompile(domain.IDPattern)

// NewSourceSet validates and canonicalizes a source set: a digest, 1 to
// MaxSourceCommits commits, each repository once, ordered by repository ID.
func NewSourceSet(digest string, commits []SourceCommit) (SourceSet, error) {
	if !digestSyntax.MatchString(digest) {
		return SourceSet{}, invalidf("a source set has a digest sha256:<64 hex>")
	}
	if len(commits) < 1 || len(commits) > MaxSourceCommits {
		return SourceSet{}, invalidf("a source set has 1 to %d commits", MaxSourceCommits)
	}
	sorted := append([]SourceCommit(nil), commits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RepositoryID < sorted[j].RepositoryID })
	parts := make([]string, len(sorted))
	for i, c := range sorted {
		if !repositoryID.MatchString(c.RepositoryID) || !gitCommit.MatchString(c.Commit) {
			return SourceSet{}, invalidf("a source commit needs a repository ID and a full commit")
		}
		if i > 0 && sorted[i-1].RepositoryID == c.RepositoryID {
			return SourceSet{}, invalidf("a source set names a repository once")
		}
		parts[i] = c.RepositoryID + "@" + c.Commit
	}
	return SourceSet{Digest: digest, commits: strings.Join(parts, ",")}, nil
}

// IsZero reports "no source set".
func (s SourceSet) IsZero() bool { return s == SourceSet{} }

// Commits returns the per-repository commits ordered by repository ID.
func (s SourceSet) Commits() []SourceCommit {
	if s.commits == "" {
		return nil
	}
	parts := strings.Split(s.commits, ",")
	out := make([]SourceCommit, len(parts))
	for i, p := range parts {
		id, commit, _ := strings.Cut(p, "@")
		out[i] = SourceCommit{RepositoryID: id, Commit: commit}
	}
	return out
}

// Validate checks the set is exactly what NewSourceSet builds.
func (s SourceSet) Validate() error {
	if s.IsZero() {
		return nil
	}
	n, err := NewSourceSet(s.Digest, s.Commits())
	if err != nil {
		return err
	}
	if n != s {
		return invalidf("a source set is in canonical form")
	}
	return nil
}

type sourceSetJSON struct {
	Digest  string         `json:"digest"`
	Commits []SourceCommit `json:"commits"`
}

// MarshalJSON renders {digest, commits}.
func (s SourceSet) MarshalJSON() ([]byte, error) {
	return json.Marshal(sourceSetJSON{Digest: s.Digest, Commits: s.Commits()})
}

// UnmarshalJSON restores a set through NewSourceSet.
func (s *SourceSet) UnmarshalJSON(b []byte) error {
	var in sourceSetJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	n, err := NewSourceSet(in.Digest, in.Commits)
	if err != nil {
		return err
	}
	*s = n
	return nil
}

// Source is the candidate: the repository and exact commit a deployment is
// built from. For a run with a source set the set is added and the repository and
// commit are those of the set's primary source (the first writable source in
// repository order, else the first), so a reader that knows only a source sees
// one valid repository and commit; the set defines the candidate. A request names
// the set alone (the repository and commit are then empty: the service fills the
// primary from the attempt's pinned context). A single-repository source keeps its
// two members and its encoding exactly as before.
type Source struct {
	RepositoryURL string    `json:"repositoryUrl"`
	Commit        string    `json:"commit"`
	SourceSet     SourceSet `json:"sourceSet,omitzero"`
}

// Validate checks the syntax: a repository URL without credentials and a full
// commit; or a canonical source set with either neither of them (the request
// form) or the primary source's repository URL and a commit of the set (the
// record form).
func (s Source) Validate() error {
	if !s.SourceSet.IsZero() {
		if err := s.SourceSet.Validate(); err != nil {
			return err
		}
		if s.RepositoryURL == "" && s.Commit == "" {
			return nil
		}
		if !repositoryURL.MatchString(s.RepositoryURL) || !gitCommit.MatchString(s.Commit) {
			return invalidf("a source set's primary needs a repository URL without credentials and a full commit")
		}
		for _, c := range s.SourceSet.Commits() {
			if c.Commit == s.Commit {
				return nil
			}
		}
		return invalidf("the primary commit is not a commit of the source set")
	}
	if !repositoryURL.MatchString(s.RepositoryURL) || !gitCommit.MatchString(s.Commit) {
		return invalidf("a source needs a repository URL without credentials and a full commit")
	}
	return nil
}

// IsZero reports an unset source.
func (s Source) IsZero() bool { return s == Source{} }

// Image is the deployable output: a registered OCI artifact and its digest.
type Image struct {
	ArtifactID domain.ArtifactID `json:"artifactId"`
	Digest     runtimev1.Digest  `json:"digest"`
}

// Validate checks the syntax.
func (i Image) Validate() error {
	if err := i.ArtifactID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := i.Digest.Validate(); err != nil {
		return invalidf("an image digest is sha256:<64 hex>")
	}
	return nil
}

// Producer is the verified origin of an image: the attempt that produced and
// finalized it and the tool image taken from that attempt's immutable command
// context, never from caller text (DQ-07).
type Producer struct {
	Attempt   domain.AttemptID `json:"attemptId"`
	Run       domain.RunID     `json:"runId"`
	Step      string           `json:"stepId"`
	ToolImage string           `json:"toolImage"`
}

// Validate checks the syntax.
func (p Producer) Validate() error {
	if err := p.Attempt.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := p.Run.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if p.Step == "" || len(p.Step) > 128 || !toolImageRef.MatchString(p.ToolImage) {
		return invalidf("a producer needs a step and a digest-pinned tool image")
	}
	return nil
}

// ImageEvidence is the artifact owner's verified view of one artifact.
type ImageEvidence struct {
	ID         domain.ArtifactID
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Kind       runtimev1.ArtifactKind
	Role       runtimev1.ArtifactRole
	Usable     bool
	Digest     runtimev1.Digest
	Provenance Source
}

// ProducedOutput is the execution owner's view of the attempt that produced an
// artifact: whether it finished successfully and its accepted result named the
// artifact as a declared OCI output.
type ProducedOutput struct {
	Scope     domain.Scope
	Attempt   domain.AttemptID
	Run       domain.RunID
	Step      string
	ToolImage string
	// Accepted is true only for a succeeded attempt whose accepted result holds
	// the artifact as the value of a declared ociArtifact output.
	Accepted bool
}

// VerifyImage applies DQ-01 and DQ-07 to a requested image: it must be a usable
// OCI output artifact of the same tenant and project, with the requested
// digest, produced from exactly the deployment's source commit, and an
// accepted output of a succeeded producer. Every failure is the same class so a
// foreign, absent, incomplete or mismatched artifact cannot be told apart.
func VerifyImage(scope domain.Scope, source Source, img Image, ev ImageEvidence, out ProducedOutput) (Producer, error) {
	reject := func() (Producer, error) {
		return Producer{}, Fail(ErrProvenance, nil, "the image is not a verified output of this source")
	}
	if source.Validate() != nil || img.Validate() != nil {
		return reject()
	}
	switch {
	case ev.ID != img.ArtifactID || ev.Scope != scope:
		return reject()
	case ev.Kind != runtimev1.KindOCI || ev.Role != runtimev1.RoleOutput || !ev.Usable:
		return reject()
	case ev.Digest != img.Digest:
		return reject()
	case ev.Provenance != source:
		return reject()
	case out.Scope != scope || out.Attempt != ev.Attempt || !out.Accepted:
		return reject()
	}
	p := Producer{Attempt: out.Attempt, Run: out.Run, Step: out.Step, ToolImage: out.ToolImage}
	if p.Validate() != nil {
		return reject()
	}
	return p, nil
}
