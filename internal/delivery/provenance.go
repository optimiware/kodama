package delivery

import (
	"regexp"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	repositoryURL = regexp.MustCompile(runtimev1.Patterns["RepositoryURL"])
	gitCommit     = regexp.MustCompile(runtimev1.Patterns["GitCommit"])
	toolImageRef  = regexp.MustCompile(runtimev1.Patterns["ImageRef"])
)

// Source is the candidate: the repository and exact commit a deployment is
// built from.
type Source struct {
	RepositoryURL string `json:"repositoryUrl"`
	Commit        string `json:"commit"`
}

// Validate checks the syntax.
func (s Source) Validate() error {
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
	// the artifact as the value of a declared output of AcceptedKind.
	Accepted     bool
	AcceptedKind ArtifactKind
}

// VerifyImage applies DQ-01 and DQ-07 to a requested image: it must be a usable
// OCI output artifact of the same tenant and project, with the requested
// digest, produced from exactly the deployment's source commit, and an
// accepted output of a succeeded producer. Every failure is the same class so a
// foreign, absent, incomplete or mismatched artifact cannot be told apart.
func VerifyImage(scope domain.Scope, source Source, img Image, ev ImageEvidence, out ProducedOutput) (Producer, error) {
	return VerifyArtifact(scope, source, FromImage(img), ev, out)
}

// VerifyArtifact applies the same provenance rules to either artifact kind.
func VerifyArtifact(scope domain.Scope, source Source, artifact DeployableArtifact, ev ImageEvidence, out ProducedOutput) (Producer, error) {
	reject := func() (Producer, error) {
		return Producer{}, Fail(ErrProvenance, nil, "the artifact is not a verified output of this source")
	}
	if source.Validate() != nil || artifact.Validate() != nil {
		return reject()
	}
	want := runtimev1.KindOCI
	if artifact.Kind == ArtifactBundle {
		want = runtimev1.KindFile
	}
	switch {
	case ev.ID != artifact.ArtifactID || ev.Scope != scope:
		return reject()
	case ev.Kind != want || ev.Role != runtimev1.RoleOutput || !ev.Usable:
		return reject()
	case ev.Digest != artifact.Digest:
		return reject()
	case ev.Provenance != source:
		return reject()
	case out.Scope != scope || out.Attempt != ev.Attempt || !out.Accepted:
		return reject()
	case out.AcceptedKind != "" && out.AcceptedKind != artifact.Kind:
		return reject()
	}
	p := Producer{Attempt: out.Attempt, Run: out.Run, Step: out.Step, ToolImage: out.ToolImage}
	if p.Validate() != nil {
		return reject()
	}
	return p, nil
}
