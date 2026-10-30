package qa

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// findingPlan is one observation mapped to its defect draft.
type findingPlan struct {
	draft DefectDraft
	// pins are the artifacts to protect for the defect: the report and every
	// cited evidence artifact.
	pins []domain.ArtifactID
}

// plan maps one verified observation to the defect work item it becomes: an
// ordinary defect with a specification stating the finding and the provenance
// of what was inspected, one acceptance criterion (the expected behavior), and
// artifact evidence links (the report, then each cited artifact).
func plan(cmd RecordCommand, f AttemptFacts, r Report, o Observation, dep DeploymentFacts, report EvidenceMeta, ref ReportRef) findingPlan {
	key := DefectKey(cmd.Scope, r.Subject.WorkItemID, r.Deployment.CandidateCommit, o.Fingerprint())
	id := DefectIDFor(key)
	links := []EvidenceLink{{ArtifactID: report.ID, Note: "QA report"}}
	pins := []domain.ArtifactID{report.ID}
	for _, e := range o.Evidence {
		note := string(e.Kind)
		if e.Note != nil {
			note += ": " + *e.Note
		}
		links = append(links, EvidenceLink{ArtifactID: e.ArtifactID, Note: note})
		pins = append(pins, e.ArtifactID)
	}
	return findingPlan{
		draft: DefectDraft{
			Scope: cmd.Scope, ID: id, Key: key, Payload: o.PayloadDigest(), Attempt: cmd.Attempt,
			Title:     "[QA] " + o.Title,
			Spec:      renderSpec(f, r, o, dep, key, o.PayloadDigest(), ref),
			Criteria:  []Criterion{{ID: "qa-1", Text: "Expected behavior: " + o.Expected}},
			Evidence:  links,
			RequestID: cmd.Caller.RequestID,
		},
		pins: pins,
	}
}

// renderSpec writes the defect specification. The provenance section names what
// was inspected (DQ-10) and which pinned acceptance context judged it; every
// value in it was verified against trusted records before this point.
func renderSpec(f AttemptFacts, r Report, o Observation, dep DeploymentFacts, key, payload string, ref ReportRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Observation\n\n%s\n\n", o.Title)
	fmt.Fprintf(&b, "- Severity: %s\n- Location: `%s`\n", o.Severity, o.Location)
	if o.CriterionID != nil {
		fmt.Fprintf(&b, "- Violated acceptance criterion: `%s`%s\n", *o.CriterionID, criterionText(f, *o.CriterionID))
	}
	b.WriteString("\n### Steps\n\n")
	for i, s := range o.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	fmt.Fprintf(&b, "\n### Expected\n\n%s\n\n### Actual\n\n%s\n\n", o.Expected, o.Actual)
	b.WriteString("## Provenance\n\n")
	b.WriteString("Found by AI exploratory QA. This defect is evidence for a human reviewer; it does not approve or reject anything.\n\n")
	fmt.Fprintf(&b, "- QA attempt: `%s` (run `%s`, step `%s`)\n", f.Attempt, f.Run, f.Step)
	fmt.Fprintf(&b, "- Work item under test: `%s` at content revision %d\n", r.Subject.WorkItemID, r.Subject.ContentRevision)
	fmt.Fprintf(&b, "- Deployment inspected: `%s` (environment `%s`)\n", dep.ID, dep.Environment)
	fmt.Fprintf(&b, "- Configuration digest: `%s`\n- Candidate commit: `%s`\n- Image digest: `%s`\n", dep.ConfigDigest, dep.Candidate, dep.Image)
	if !f.Sources.Empty() {
		// A source-set attempt names every checkout it was pinned to (DQ-10); an
		// older context has none and renders exactly as before.
		fmt.Fprintf(&b, "- Source set: `%s`\n", f.Sources.Digest)
		for _, src := range f.Sources.Sources {
			fmt.Fprintf(&b, "  - `%s` at `%s` (repository `%s`, %s, pinned `%s`)\n", src.Path, src.HeadCommit, src.RepositoryID, src.Access, src.Commit)
		}
	}
	fmt.Fprintf(&b, "- QA report: artifact `%s` (`%s`), canonical digest `%s`\n", ref.ArtifactID, ref.ArtifactDigest, ref.Digest)
	fmt.Fprintf(&b, "- Deduplication key: `%s`\n", key)
	b.WriteString(PayloadMarker(payload) + "\n")
	return b.String()
}

func criterionText(f AttemptFacts, id string) string {
	if f.Context.WorkItem == nil {
		return ""
	}
	for _, c := range f.Context.WorkItem.AcceptanceCriteria {
		if c.ID == id {
			return " — " + c.Text
		}
	}
	return ""
}

// file verifies the identity, pins the evidence and files (or observes) the defect of one finding.
// Pins come first and are idempotent: a defect link never points at an artifact
// that retention could remove, and a crash between the two steps leaves only a
// pin that the retry reuses.
func (s *Service) file(ctx context.Context, p findingPlan, res *DefectResult) error {
	// A changed finding under an existing identity is refused before any pin: the
	// evidence of a conflicting claim is not retained on the first claim's defect.
	if err := s.cfg.Defects.Verify(ctx, p.draft); err != nil {
		return passThrough(err, "the defect identity could not be checked")
	}
	for _, id := range p.pins {
		if err := s.cfg.Artifacts.Pin(ctx, p.draft.Scope, id, string(p.draft.ID)); err != nil {
			if errors.Is(err, ErrNotFound) {
				return failf(ErrEvidence, "an artifact is no longer retained")
			}
			return passThrough(err, "an artifact could not be pinned")
		}
	}
	d, created, err := s.cfg.Defects.Open(ctx, p.draft)
	if err != nil {
		return passThrough(err, "the defect could not be filed")
	}
	if d.ID != p.draft.ID {
		return failf(ErrConflict, "the defect identity is held by another record")
	}
	res.Created, res.State = created, d.State
	if created {
		res.EvidenceAdded = len(p.draft.Evidence)
		return nil
	}
	if d.Terminal {
		return nil
	}
	d, added, err := s.cfg.Defects.Attach(ctx, p.draft.Scope, p.draft.Attempt, p.draft.RequestID, p.draft.ID, p.draft.Evidence)
	if err != nil {
		return passThrough(err, "the defect evidence could not be attached")
	}
	res.EvidenceAdded, res.State = added, d.State
	return nil
}
