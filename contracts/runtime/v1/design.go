package runtimev1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// Design conformance (AR-14, DQ-14, SD-08). The pinned baseline travels in the
// immutable attempt Context; the candidate-bound report is an ordinary declared
// artifact whose payload this package defines. Neither is a separate resource.
// See docs/implementation/design-conformance.md.

// Codes returned by CheckConformance and baseline validation. They are not
// runtime wire codes (no HTTP status); T-034 maps them onto its gate responses.
const (
	CodeBaselineDigestMismatch       domain.ErrorCode = "baseline_digest_mismatch"
	CodeConformanceCandidateMismatch domain.ErrorCode = "conformance_candidate_mismatch"
	CodeConformanceBaselineMismatch  domain.ErrorCode = "conformance_baseline_mismatch"
	CodeConformanceIncomplete        domain.ErrorCode = "conformance_incomplete"
	CodeConformanceBlocked           domain.ErrorCode = "conformance_blocked"
)

type (
	CheckOutcome string
	Disposition  string
)

const (
	CheckPassed CheckOutcome = "passed"
	CheckFailed CheckOutcome = "failed"
	CheckNotRun CheckOutcome = "not_run"
	Conforms    Disposition  = "conforms"
	DispBlocked Disposition  = "blocked"
)

const (
	maxBaselineRequirements = 256
	maxBaselineDecisions    = 128
	maxBaselineSources      = 64
	maxBaselineChecks       = 64
	maxReportDeviations     = 64
)

// SourceRef identifies one design source at an immutable revision. Prompts must
// never resolve main, latest or an unversioned blueprint again mid-run, so the
// revision is a Git object ID, never a branch or tag. The repository URL carries
// no credentials; the path is relative and cannot escape the repository.
type SourceRef struct {
	Repository string    `json:"repository"`
	Revision   GitCommit `json:"revision"`
	Path       string    `json:"path"`
}

// DesignBaseline is the immutable, reference-only baseline of one attempt.
// Digest is SHA-256 over the canonical JSON of the four reference lists below
// (never over Digest itself, and never over a later candidate or report).
// Every list is strictly sorted and duplicate-free, so one baseline has exactly
// one canonical form and one digest: requirements, decisions and required
// checks by string, sources by (repository, path, revision).
type DesignBaseline struct {
	Digest         Digest      `json:"digest"`
	Requirements   []string    `json:"requirements"`
	Decisions      []string    `json:"decisions"`
	Sources        []SourceRef `json:"sources"`
	RequiredChecks []string    `json:"requiredChecks"`
}

// baselineReferences is the exact object the baseline digest covers.
type baselineReferences struct {
	Requirements   []string    `json:"requirements"`
	Decisions      []string    `json:"decisions"`
	Sources        []SourceRef `json:"sources"`
	RequiredChecks []string    `json:"requiredChecks"`
}

func (b DesignBaseline) references() baselineReferences {
	return baselineReferences{b.Requirements, b.Decisions, b.Sources, b.RequiredChecks}
}

func (r baselineReferences) validate() error {
	if err := stringList("requirements", r.Requirements, "RequirementID", 1, maxBaselineRequirements); err != nil {
		return err
	}
	if err := stringList("decisions", r.Decisions, "DecisionID", 0, maxBaselineDecisions); err != nil {
		return err
	}
	if err := stringList("requiredChecks", r.RequiredChecks, "CheckID", 0, maxBaselineChecks); err != nil {
		return err
	}
	if r.Sources == nil || len(r.Sources) < 1 || len(r.Sources) > maxBaselineSources {
		return fail("sources", "must be an array of 1-%d sources", maxBaselineSources)
	}
	for i, s := range r.Sources {
		if err := firstError(matches("sources.repository", "RepositoryURL", s.Repository), wrap("sources.revision", s.Revision.Validate()), matches("sources.path", "SourcePath", s.Path)); err != nil {
			return err
		}
		if i > 0 && !sourceLess(r.Sources[i-1], s) {
			return fail("sources", "must be strictly sorted by repository, path, revision without duplicates")
		}
	}
	return nil
}

func sourceLess(a, b SourceRef) bool {
	if a.Repository != b.Repository {
		return a.Repository < b.Repository
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Revision < b.Revision
}

// stringList validates a strictly sorted, duplicate-free list of strings.
func stringList(path string, list []string, pattern string, min, max int) error {
	if list == nil || len(list) < min || len(list) > max {
		return fail(path, "must be an array of %d-%d items", min, max)
	}
	for i, s := range list {
		if err := matches(path, pattern, s); err != nil {
			return err
		}
		if i > 0 && list[i-1] >= s {
			return fail(path, "must be strictly sorted without duplicates")
		}
	}
	return nil
}

func digestOf(canonical []byte) Digest {
	sum := sha256.Sum256(canonical)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func canonicalOf(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// BaselineDigest validates the baseline's references and returns the digest
// they must carry. The baseline's own Digest field is ignored.
func BaselineDigest(b DesignBaseline) (Digest, error) {
	refs := b.references()
	if err := refs.validate(); err != nil {
		return "", err
	}
	canonical, err := canonicalOf(refs)
	if err != nil {
		return "", err
	}
	return digestOf(canonical), nil
}

// Validate checks the references and that Digest matches them. A copied digest
// next to edited references is rejected.
func (b DesignBaseline) Validate() error {
	if err := wrap("digest", b.Digest.Validate()); err != nil {
		return err
	}
	want, err := BaselineDigest(b)
	if err != nil {
		return err
	}
	if want != b.Digest {
		return &ContractError{Code: CodeBaselineDigestMismatch, Message: "digest does not match the baseline references"}
	}
	return nil
}

// NewDesignBaseline sorts the references into canonical order, then computes
// the digest. Producers (the control plane) use it to build run context.
func NewDesignBaseline(requirements, decisions []string, sources []SourceRef, requiredChecks []string) (DesignBaseline, error) {
	b := DesignBaseline{
		Requirements:   sortedCopy(requirements),
		Decisions:      sortedCopy(decisions),
		Sources:        append([]SourceRef{}, sources...),
		RequiredChecks: sortedCopy(requiredChecks),
	}
	sort.Slice(b.Sources, func(i, j int) bool { return sourceLess(b.Sources[i], b.Sources[j]) })
	digest, err := BaselineDigest(b)
	if err != nil {
		return DesignBaseline{}, err
	}
	b.Digest = digest
	return b, nil
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// ConformanceCheck is one named check's outcome. A passed check must cite
// evidence, so a prose-only success claim cannot satisfy it.
type ConformanceCheck struct {
	ID                  string              `json:"id"`
	Outcome             CheckOutcome        `json:"outcome"`
	EvidenceArtifactIDs []domain.ArtifactID `json:"evidenceArtifactIds"`
}

// Deviation is an unresolved material conflict with the design baseline.
type Deviation struct {
	Requirement string `json:"requirement"`
	Reason      string `json:"reason"`
}

// ConformanceReport is the candidate-bound payload of the declared conformance
// artifact. Disposition is derived, not chosen: "conforms" exactly when there
// are no unresolved deviations and every listed check passed; otherwise
// "blocked". Checks are sorted by ID, requirements and evidence IDs strictly
// sorted, deviations by (requirement, reason).
type ConformanceReport struct {
	BaselineDigest       Digest             `json:"baselineDigest"`
	CandidateCommit      GitCommit          `json:"candidateCommit"`
	Requirements         []string           `json:"requirements"`
	Checks               []ConformanceCheck `json:"checks"`
	UnresolvedDeviations []Deviation        `json:"unresolvedDeviations"`
	Disposition          Disposition        `json:"disposition"`
}

func (r ConformanceReport) Validate() error {
	if err := firstError(wrap("baselineDigest", r.BaselineDigest.Validate()), wrap("candidateCommit", r.CandidateCommit.Validate()), stringList("requirements", r.Requirements, "RequirementID", 1, maxBaselineRequirements)); err != nil {
		return err
	}
	if r.Checks == nil || len(r.Checks) < 1 || len(r.Checks) > maxBaselineChecks {
		return fail("checks", "must be an array of 1-%d checks", maxBaselineChecks)
	}
	allPassed := true
	for i, c := range r.Checks {
		if err := matches("checks.id", "CheckID", c.ID); err != nil {
			return err
		}
		if i > 0 && r.Checks[i-1].ID >= c.ID {
			return fail("checks", "must be strictly sorted by id without duplicates")
		}
		if c.Outcome != CheckPassed && c.Outcome != CheckFailed && c.Outcome != CheckNotRun {
			return fail("checks."+c.ID+".outcome", "unknown outcome %q", c.Outcome)
		}
		if c.EvidenceArtifactIDs == nil || len(c.EvidenceArtifactIDs) > maxDiagnostics {
			return fail("checks."+c.ID+".evidenceArtifactIds", "must be an array of at most %d IDs", maxDiagnostics)
		}
		if c.Outcome == CheckPassed && len(c.EvidenceArtifactIDs) == 0 {
			return fail("checks."+c.ID+".evidenceArtifactIds", "a passed check must cite evidence")
		}
		for j, id := range c.EvidenceArtifactIDs {
			if err := wrap("checks."+c.ID+".evidenceArtifactIds", id.Validate()); err != nil {
				return err
			}
			if j > 0 && c.EvidenceArtifactIDs[j-1] >= id {
				return fail("checks."+c.ID+".evidenceArtifactIds", "must be strictly sorted without duplicates")
			}
		}
		allPassed = allPassed && c.Outcome == CheckPassed
	}
	if r.UnresolvedDeviations == nil || len(r.UnresolvedDeviations) > maxReportDeviations {
		return fail("unresolvedDeviations", "must be an array of at most %d deviations", maxReportDeviations)
	}
	for i, d := range r.UnresolvedDeviations {
		if err := matches("unresolvedDeviations.requirement", "RequirementID", d.Requirement); err != nil {
			return err
		}
		if strings.TrimSpace(d.Reason) == "" || len(d.Reason) > 2048 {
			return fail("unresolvedDeviations.reason", "must be a non-blank string of at most 2048 bytes")
		}
		if i > 0 {
			p := r.UnresolvedDeviations[i-1]
			if p.Requirement > d.Requirement || (p.Requirement == d.Requirement && p.Reason >= d.Reason) {
				return fail("unresolvedDeviations", "must be strictly sorted by requirement, reason without duplicates")
			}
		}
	}
	want := DispBlocked
	if allPassed && len(r.UnresolvedDeviations) == 0 {
		want = Conforms
	}
	if r.Disposition != Conforms && r.Disposition != DispBlocked {
		return fail("disposition", "must be conforms or blocked")
	}
	if r.Disposition != want {
		return fail("disposition", "must be %s for these checks and deviations", want)
	}
	return nil
}

// ReportDigest is SHA-256 over the canonical form of the whole report, using
// the same canonicalization as result hashing and gate subjects.
func ReportDigest(r ConformanceReport) (Digest, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	canonical, err := canonicalOf(r)
	if err != nil {
		return "", err
	}
	return digestOf(canonical), nil
}

// CheckConformance applies what a schema cannot: that a report assesses this
// candidate and this baseline, covers every required check, and is not
// blocked. It never trusts a self-report as proof of correctness; it only
// establishes that the evidence is current and complete enough to put in front
// of a reviewer. Evidence artifact ownership and provenance are verified by the
// gate (T-034) against the artifact store.
//
// Errors, in order: invalid_request (structure), conformance_candidate_mismatch,
// conformance_baseline_mismatch, conformance_incomplete (a required check is
// absent), conformance_blocked (unresolved deviation or a check not passed).
func CheckConformance(r ConformanceReport, b DesignBaseline, candidate GitCommit) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return &ContractError{Code: domain.CodeInvalidRequest, Message: err.Error()}
	}
	if r.CandidateCommit != candidate {
		return &ContractError{Code: CodeConformanceCandidateMismatch, Message: "the report assesses a different candidate"}
	}
	if r.BaselineDigest != b.Digest {
		return &ContractError{Code: CodeConformanceBaselineMismatch, Message: "the report assesses a different design baseline"}
	}
	have := map[string]bool{}
	for _, c := range r.Checks {
		have[c.ID] = true
	}
	for _, id := range b.RequiredChecks {
		if !have[id] {
			return &ContractError{Code: CodeConformanceIncomplete, Message: "required check " + id + " is missing"}
		}
	}
	if r.Disposition != Conforms {
		return &ContractError{Code: CodeConformanceBlocked, Message: "the report records unresolved deviations or checks that did not pass"}
	}
	return nil
}
