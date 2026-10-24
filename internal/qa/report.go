package qa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Report is the structured result of one exploratory QA attempt. The QA agent
// writes it as the attempt's declared report artifact. Every field is written;
// a nullable field is an explicit null (the same rule as the runtime contract).
//
// A report is a claim, not an authority: tenant, project, attempt and the
// environment the attempt may inspect come from the attempt's trusted records,
// and Service.Record refuses a report whose subject, deployment, configuration
// or evidence disagree with them.
type Report struct {
	Format  string  `json:"format"`
	Subject Subject `json:"subject"`
	// Deployment identifies exactly what was inspected (DQ-10).
	Deployment   DeploymentRef `json:"deployment"`
	Summary      string        `json:"summary"`
	Observations []Observation `json:"observations"`
}

// Subject is the pinned acceptance context the exploration judged against.
type Subject struct {
	WorkItemID      domain.WorkItemID      `json:"workItemId"`
	ContentRevision domain.ContentRevision `json:"contentRevision"`
}

// DeploymentRef identifies the deployment and configuration that was inspected.
type DeploymentRef struct {
	ID              string               `json:"id"`
	EnvironmentID   domain.EnvironmentID `json:"environmentId"`
	ConfigDigest    runtimev1.Digest     `json:"configDigest"`
	CandidateCommit runtimev1.GitCommit  `json:"candidateCommit"`
	ImageDigest     runtimev1.Digest     `json:"imageDigest"`
}

// Observation is one finding: something the application did that the pinned
// acceptance context says it should not have (or failed to do).
type Observation struct {
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	// CriterionID names the pinned acceptance criterion that is violated, or null.
	CriterionID *string `json:"criterionId"`
	// Location is an application route path ("/orders/new"), never a URL with a
	// host, query or fragment.
	Location string   `json:"location"`
	Steps    []string `json:"steps"`
	Expected string   `json:"expected"`
	Actual   string   `json:"actual"`
	// Evidence is at least one artifact the QA attempt produced.
	Evidence []EvidenceRef `json:"evidence"`
}

// EvidenceRef references one artifact by identity and content digest.
type EvidenceRef struct {
	ArtifactID domain.ArtifactID `json:"artifactId"`
	Digest     runtimev1.Digest  `json:"digest"`
	Kind       EvidenceKind      `json:"kind"`
	Note       *string           `json:"note"`
}

// Validate checks the structure and the field rules. It does not (and cannot)
// check ownership: the use case verifies the report against trusted records.
func (r *Report) Validate() error {
	if r.Format != ReportFormat {
		return invalidf("format must be %q", ReportFormat)
	}
	if err := r.Subject.WorkItemID.Validate(); err != nil {
		return invalidf("subject: %v", err)
	}
	if err := r.Subject.ContentRevision.Validate(); err != nil {
		return invalidf("subject: %v", err)
	}
	if err := validDeploymentRef(r.Deployment); err != nil {
		return err
	}
	if err := text("summary", r.Summary, MaxSummaryChars, true); err != nil {
		return err
	}
	if r.Observations == nil {
		return invalidf("observations must be an array")
	}
	if len(r.Observations) > MaxObservations {
		return invalidf("a report has at most %d observations", MaxObservations)
	}
	seen := make(map[string]int, len(r.Observations))
	for i := range r.Observations {
		o := &r.Observations[i]
		if err := o.validate(); err != nil {
			return fmt.Errorf("observation %d: %w", i, err)
		}
		fp := o.Fingerprint()
		if j, dup := seen[fp]; dup {
			return invalidf("observation %d repeats the finding of observation %d", i, j)
		}
		seen[fp] = i
	}
	return nil
}

func validDeploymentRef(d DeploymentRef) error {
	if err := domain.WorkItemID(d.ID).Validate(); err != nil { // same opaque identifier syntax
		return invalidf("deployment id: must match %s", domain.IDPattern)
	}
	if err := d.EnvironmentID.Validate(); err != nil {
		return invalidf("deployment: %v", err)
	}
	if err := d.ConfigDigest.Validate(); err != nil {
		return invalidf("deployment configDigest: %v", err)
	}
	if err := d.CandidateCommit.Validate(); err != nil {
		return invalidf("deployment candidateCommit: %v", err)
	}
	if err := d.ImageDigest.Validate(); err != nil {
		return invalidf("deployment imageDigest: %v", err)
	}
	return nil
}

func (o *Observation) validate() error {
	if err := text("title", o.Title, MaxTitleChars, false); err != nil {
		return err
	}
	if !o.Severity.Valid() {
		return invalidf("severity must be critical, major, minor or trivial")
	}
	if o.CriterionID != nil && !criterionPattern.MatchString(*o.CriterionID) {
		return invalidf("criterionId must match %s", criterionPattern)
	}
	if len(o.Location) > MaxLocationChars || !locationPattern.MatchString(o.Location) || strings.Contains(o.Location, "//") {
		return invalidf("location must be an application route path without host, query or fragment")
	}
	if err := text("location", o.Location, MaxLocationChars, false); err != nil {
		return err
	}
	if len(o.Steps) < 1 || len(o.Steps) > MaxSteps {
		return invalidf("an observation has 1 to %d steps", MaxSteps)
	}
	for _, s := range o.Steps {
		if err := text("step", s, MaxStepChars, false); err != nil {
			return err
		}
	}
	if err := text("expected", o.Expected, MaxTextChars, true); err != nil {
		return err
	}
	if err := text("actual", o.Actual, MaxTextChars, true); err != nil {
		return err
	}
	if len(o.Evidence) < 1 || len(o.Evidence) > MaxEvidencePerFinding {
		return invalidf("an observation cites 1 to %d evidence artifacts", MaxEvidencePerFinding)
	}
	ids := make(map[domain.ArtifactID]struct{}, len(o.Evidence))
	for _, e := range o.Evidence {
		if err := e.validate(); err != nil {
			return err
		}
		if _, dup := ids[e.ArtifactID]; dup {
			return invalidf("evidence artifact %q is cited twice", e.ArtifactID)
		}
		ids[e.ArtifactID] = struct{}{}
	}
	return nil
}

func (e EvidenceRef) validate() error {
	if err := e.ArtifactID.Validate(); err != nil {
		return invalidf("evidence: %v", err)
	}
	if err := e.Digest.Validate(); err != nil {
		return invalidf("evidence digest: %v", err)
	}
	if !e.Kind.Valid() {
		return invalidf("evidence kind must be screenshot, trace or log")
	}
	if e.Note != nil {
		if err := text("evidence note", *e.Note, MaxNoteChars, false); err != nil {
			return err
		}
	}
	return nil
}

// Decode strictly decodes and validates report bytes: unknown, misspelled,
// differently cased, duplicate and omitted fields, non-integer numbers and
// trailing data are all errors (so a report cannot smuggle an "approved" field
// or an alternate target). Failures are ErrInvalid.
func Decode(data []byte) (Report, error) {
	r, err := runtimev1.DecodeRequest[Report](data)
	if err != nil {
		return Report{}, invalidf("%v", err)
	}
	return r, nil
}

// Canonical returns the canonical bytes of the report: sorted keys, no
// insignificant whitespace, explicit nulls and empty arrays, integers only (the
// runtime contract's canonicalization).
func (r Report) Canonical() ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return runtimev1.Canonicalize(raw)
}

// Digest is the SHA-256 of the canonical report, independent of formatting.
func (r Report) Digest() (runtimev1.Digest, error) {
	c, err := r.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Fingerprint identifies the finding independent of wording noise: the violated
// criterion, the normalized route and the normalized title. Severity, steps,
// expected/actual text and evidence do not change it, so the same finding
// observed again (a retry, another pass over the same deployment) maps to the
// same defect.
func (o Observation) Fingerprint() string {
	criterion := ""
	if o.CriterionID != nil {
		criterion = *o.CriterionID
	}
	raw, err := json.Marshal(struct {
		Format    string `json:"format"`
		Criterion string `json:"criterion"`
		Location  string `json:"location"`
		Title     string `json:"title"`
	}{"kodama/qa-finding/v1", criterion, normalizeLocation(o.Location), normalizeText(o.Title)})
	if err != nil {
		panic(fmt.Sprintf("qa: marshal fingerprint: %v", err)) // plain strings; cannot fail
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// PayloadDigest is the digest of the finding's substantive content: severity,
// normalized location, violated criterion, normalized title (case and spacing
// are noise, as for the fingerprint), steps, expected and actual
// behavior. It is deliberately not the Fingerprint: the fingerprint says which
// finding this is (and so which defect), the payload digest says what was
// claimed about it. Evidence is excluded, so a retry or a second pass that adds
// screenshots keeps the digest, while a changed severity, step or expectation
// changes it and cannot silently reuse the defect that the first claim created.
func (o Observation) PayloadDigest() string {
	criterion := ""
	if o.CriterionID != nil {
		criterion = *o.CriterionID
	}
	steps := o.Steps
	if steps == nil {
		steps = []string{}
	}
	raw, err := json.Marshal(struct {
		Format    string   `json:"format"`
		Severity  Severity `json:"severity"`
		Criterion string   `json:"criterion"`
		Location  string   `json:"location"`
		Title     string   `json:"title"`
		Steps     []string `json:"steps"`
		Expected  string   `json:"expected"`
		Actual    string   `json:"actual"`
	}{"kodama/qa-finding-payload/v1", o.Severity, criterion, normalizeLocation(o.Location), normalizeText(o.Title), steps, o.Expected, o.Actual})
	if err != nil {
		panic(fmt.Sprintf("qa: marshal finding payload: %v", err)) // plain strings; cannot fail
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		panic(fmt.Sprintf("qa: canonicalize finding payload: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func normalizeLocation(s string) string {
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
		if s == "" {
			s = "/"
		}
	}
	return s
}
