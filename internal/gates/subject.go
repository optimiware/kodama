package gates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// MaxEvidence bounds the evidence references of one subject.
const MaxEvidence = 256

// EvidenceRef identifies one artifact by ID and the digest of its verified
// content.
type EvidenceRef struct {
	ID     domain.ArtifactID `json:"id"`
	Digest string            `json:"digest"`
}

// SubjectInput is the server-side material of a subject. Nothing in it comes
// from a decision request.
type SubjectInput struct {
	Scope           domain.Scope
	WorkItemID      domain.WorkItemID
	ContentRevision domain.ContentRevision
	// CandidateCommit is the commit of a single-repository candidate. A candidate
	// of a run with a source set sets SourceSet instead (exactly one of them).
	CandidateCommit string
	SourceSet       *SourceSetSubject
	DefinitionID    domain.DefinitionID
	Evidence        []EvidenceRef
	// DeploymentID and DeploymentConfigDigest are both empty for a source-only
	// gate and both set for a gate that reviews a deployment.
	DeploymentID           string
	DeploymentConfigDigest string
}

// Subject is the single subject shape of delivery and quality gates: the
// source commit plus content revision, definition, evidence and deployment
// configuration. It is immutable once built; the canonical bytes and digest
// are what a reviewer's decision is bound to.
type Subject struct {
	scope      domain.Scope
	workItem   domain.WorkItemID
	revision   domain.ContentRevision
	candidate  string
	set        *SourceSetSubject
	definition domain.DefinitionID
	evidence   []EvidenceRef
	deployment *string
	config     *string
	canonical  []byte
	digest     string
}

// NewSubject validates, normalizes and canonicalizes a subject. Evidence is
// sorted by (id, digest); an identical duplicate collapses and one artifact
// with two digests is refused. Absent deployment fields stay explicit nulls and
// absent evidence stays an empty array.
func NewSubject(in SubjectInput) (Subject, error) {
	if err := in.Scope.Validate(); err != nil {
		return Subject{}, invalidf("%v", err)
	}
	if err := in.WorkItemID.Validate(); err != nil {
		return Subject{}, invalidf("%v", err)
	}
	if err := in.ContentRevision.Validate(); err != nil {
		return Subject{}, invalidf("%v", err)
	}
	if (in.CandidateCommit == "") == (in.SourceSet == nil) {
		return Subject{}, invalidf("a subject names one candidate commit or one source set")
	}
	if in.SourceSet == nil {
		if err := validCommit("candidate commit", in.CandidateCommit); err != nil {
			return Subject{}, err
		}
	}
	if err := in.DefinitionID.Validate(); err != nil {
		return Subject{}, invalidf("%v", err)
	}
	if (in.DeploymentID == "") != (in.DeploymentConfigDigest == "") {
		return Subject{}, invalidf("a deployment and its configuration digest are set together")
	}
	s := Subject{scope: in.Scope, workItem: in.WorkItemID, revision: in.ContentRevision, candidate: in.CandidateCommit, definition: in.DefinitionID}
	if in.SourceSet != nil {
		// Re-validate: a stored or built set is canonical (ordered, once each).
		set, err := NewSourceSetSubject(in.SourceSet.Digest, in.SourceSet.Sources)
		if err != nil {
			return Subject{}, err
		}
		s.set = &set
	}
	if in.DeploymentID != "" {
		if err := validID("deployment id", in.DeploymentID); err != nil {
			return Subject{}, err
		}
		if err := validDigest("deployment configuration digest", in.DeploymentConfigDigest); err != nil {
			return Subject{}, err
		}
		d, c := in.DeploymentID, in.DeploymentConfigDigest
		s.deployment, s.config = &d, &c
	}
	ev, err := normalizeEvidence(in.Evidence)
	if err != nil {
		return Subject{}, err
	}
	s.evidence = ev
	raw, err := json.Marshal(s.wire())
	if err != nil {
		return Subject{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return Subject{}, invalidf("subject is not canonicalizable: %v", err)
	}
	sum := sha256.Sum256(canonical)
	s.canonical, s.digest = canonical, "sha256:"+hex.EncodeToString(sum[:])
	return s, nil
}

func normalizeEvidence(in []EvidenceRef) ([]EvidenceRef, error) {
	out := make([]EvidenceRef, 0, len(in))
	byID := map[domain.ArtifactID]string{}
	for _, e := range in {
		if err := e.ID.Validate(); err != nil {
			return nil, invalidf("%v", err)
		}
		if err := validDigest("evidence digest", e.Digest); err != nil {
			return nil, err
		}
		if d, ok := byID[e.ID]; ok {
			if d != e.Digest {
				return nil, invalidf("evidence %s is listed with two different digests", e.ID)
			}
			continue
		}
		byID[e.ID] = e.Digest
		out = append(out, e)
	}
	if len(out) > MaxEvidence {
		return nil, invalidf("a subject lists at most %d evidence references", MaxEvidence)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Digest < out[j].Digest
	})
	return out, nil
}

type subjectWire struct {
	TenantID        string `json:"tenantId"`
	ProjectID       string `json:"projectId"`
	WorkItemID      string `json:"workItemId"`
	ContentRevision int64  `json:"contentRevision"`
	// CandidateCommit and SourceSet are alternatives. A single-repository subject
	// keeps its candidateCommit member and has no sourceSet member (its canonical
	// bytes are those of every earlier subject); a source-set subject has no
	// candidateCommit member.
	CandidateCommit        *string       `json:"candidateCommit,omitempty"`
	SourceSet              *sourceSetW   `json:"sourceSet,omitempty"`
	DefinitionID           string        `json:"definitionId"`
	Evidence               []EvidenceRef `json:"evidence"`
	DeploymentID           *string       `json:"deploymentId"`
	DeploymentConfigDigest *string       `json:"deploymentConfigDigest"`
}

type sourceSetW struct {
	Digest  string    `json:"digest"`
	Sources []sourceW `json:"sources"`
}

type sourceW struct {
	RepositoryID string `json:"repositoryId"`
	Commit       string `json:"commit"`
	Base         *baseW `json:"base"`
}

type baseW struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

func (s SourceSetSubject) wire() *sourceSetW {
	w := &sourceSetW{Digest: s.Digest, Sources: make([]sourceW, len(s.Sources))}
	for i, x := range s.Sources {
		w.Sources[i] = sourceW{RepositoryID: x.RepositoryID, Commit: x.Commit}
		if x.Base != nil {
			w.Sources[i].Base = &baseW{Ref: x.Base.Ref, Commit: x.Base.Commit}
		}
	}
	return w
}

func (w sourceSetW) subject() SourceSetSubject {
	out := SourceSetSubject{Digest: w.Digest, Sources: make([]SubjectSource, len(w.Sources))}
	for i, x := range w.Sources {
		out.Sources[i] = SubjectSource{RepositoryID: x.RepositoryID, Commit: x.Commit}
		if x.Base != nil {
			out.Sources[i].Base = &Base{Ref: x.Base.Ref, Commit: x.Base.Commit}
		}
	}
	return out
}

func (s Subject) wire() subjectWire {
	ev := s.evidence
	if ev == nil {
		ev = []EvidenceRef{}
	}
	w := subjectWire{
		TenantID: string(s.scope.TenantID), ProjectID: string(s.scope.ProjectID), WorkItemID: string(s.workItem),
		ContentRevision: int64(s.revision), DefinitionID: string(s.definition),
		Evidence: ev, DeploymentID: s.deployment, DeploymentConfigDigest: s.config,
	}
	if s.set != nil {
		w.SourceSet = s.set.wire()
	} else {
		c := s.candidate
		w.CandidateCommit = &c
	}
	return w
}

// ParseSubject restores a stored subject. The bytes must be exactly the
// canonical form of a valid subject: unknown members, reordering, missing nulls
// and noncanonical encodings are rejected, so a stored subject can never
// disagree with its digest.
func ParseSubject(canonical []byte) (Subject, error) {
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var w subjectWire
	if err := dec.Decode(&w); err != nil {
		return Subject{}, invalidf("subject: %v", err)
	}
	in := SubjectInput{
		Scope:      domain.Scope{TenantID: domain.TenantID(w.TenantID), ProjectID: domain.ProjectID(w.ProjectID)},
		WorkItemID: domain.WorkItemID(w.WorkItemID), ContentRevision: domain.ContentRevision(w.ContentRevision),
		DefinitionID: domain.DefinitionID(w.DefinitionID), Evidence: w.Evidence,
	}
	if w.CandidateCommit != nil {
		in.CandidateCommit = *w.CandidateCommit
	}
	if w.SourceSet != nil {
		set := w.SourceSet.subject()
		in.SourceSet = &set
	}
	if w.DeploymentID != nil {
		in.DeploymentID = *w.DeploymentID
	}
	if w.DeploymentConfigDigest != nil {
		in.DeploymentConfigDigest = *w.DeploymentConfigDigest
	}
	s, err := NewSubject(in)
	if err != nil {
		return Subject{}, err
	}
	if !bytes.Equal(s.canonical, canonical) {
		return Subject{}, invalidf("subject is not in canonical form")
	}
	return s, nil
}

// Canonical returns the canonical JSON bytes: UTF-8, lexicographically ordered
// keys, evidence sorted by (id, digest), explicit nulls, integers only.
func (s Subject) Canonical() []byte { return append([]byte(nil), s.canonical...) }

// Digest is the SHA-256 of the canonical bytes, as sha256:<hex>.
func (s Subject) Digest() string { return s.digest }

func (s Subject) Scope() domain.Scope                     { return s.scope }
func (s Subject) WorkItemID() domain.WorkItemID           { return s.workItem }
func (s Subject) ContentRevision() domain.ContentRevision { return s.revision }
func (s Subject) CandidateCommit() string                 { return s.candidate }
func (s Subject) DefinitionID() domain.DefinitionID       { return s.definition }

// PresentedCandidate is the one commit a client that does not read source sets
// sees: the candidate commit of a single-repository subject, or the commit of the
// primary source of a source-set subject (SourceSetSubject.Primary). The subject's
// identity and digest are never defined by it.
func (s Subject) PresentedCandidate() string {
	if s.set != nil {
		return s.set.Primary().Commit
	}
	return s.candidate
}

// SourceSet returns the source-set candidate of the subject, or false for a
// single-repository subject (which has CandidateCommit instead).
func (s Subject) SourceSet() (SourceSetSubject, bool) {
	if s.set == nil {
		return SourceSetSubject{}, false
	}
	return s.set.clone(), true
}

// Evidence returns a copy of the sorted evidence references.
func (s Subject) Evidence() []EvidenceRef { return append([]EvidenceRef{}, s.evidence...) }

// Deployment returns the deployment ID and configuration digest, or empty
// strings for a source-only subject.
func (s Subject) Deployment() (id, configDigest string) {
	if s.deployment == nil {
		return "", ""
	}
	return *s.deployment, *s.config
}

// IsZero reports whether the subject was never built.
func (s Subject) IsZero() bool { return s.digest == "" }
