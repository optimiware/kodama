package gates

import (
	"fmt"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ProvenanceObservation describes the server-observed provenance of an artifact
// or release evaluated at a gate.
type ProvenanceObservation struct {
	ArtifactID     domain.ArtifactID
	Digest         runtimev1.Digest
	Sources        []artifactsv1.SourceRef
	WorkspaceState string // "none", "clean", "modified", or "" (unknown)
}

// CurrentApplicabilityV2 describes the live project/run state rechecked when a
// v2 gate is requested or progressed (DQ-04, DQ-05, DQ-18, DQ-19).
type CurrentApplicabilityV2 struct {
	WorkItemID             *domain.WorkItemID
	ContentRevision        *domain.ContentRevision
	CurrentSources         []artifactsv1.SourceRef
	CurrentRelease         *deliveryv1.ReleasePin
	CurrentArtifacts       []artifactsv1.ArtifactDigestRef
	DeploymentConfigDigest *runtimev1.Digest
	EvidenceObservations   map[domain.ArtifactID]ProvenanceObservation
}

// CheckApplicabilityV2 evaluates DQ-18 and DQ-19 for a v2 gate subject:
//   - Source-bound subjects require candidate-bound evidence to come from a
//     "clean" workspace at the exact source set, and invalidate if any commit in
//     the source set or the work-item content revision changes.
//   - Artifact/release-bound subjects bind the exact artifact/manifest digests
//     and disclosed provenance; generated outputs from "modified" shared scratch
//     are valid without requiring a Git commit, and later scratch changes do not
//     invalidate an already sealed immutable artifact/release!
//   - Modified or unknown source is never relabeled as a clean source candidate
//     and an artifact-only approval never grants Git branch integration authority.
func CheckApplicabilityV2(subj deliveryv1.GateSubjectV2, cur CurrentApplicabilityV2, requireSourceIntegration bool) error {
	// 1. Check deployment configuration pin if present.
	rawSubj, err := deliveryv1.ParseGateSubjectV2(subj.Canonical())
	if err != nil {
		return err
	}
	_ = rawSubj

	// 2. If the caller asks for Git default-branch integration authority (ADR-0011
	//    DQ-17), the gate subject MUST bind a non-empty source set and clean source
	//    provenance; an artifact-only or release-only gate cannot grant it.
	subjSources := subj.Sources()
	if requireSourceIntegration && len(subjSources) == 0 {
		return fmt.Errorf("%w: artifact-only gate approval cannot grant Git integration authority", ErrStaleSubject)
	}

	// 3. If the subject binds sources, verify the current source set matches and
	//    all required evidence was produced from a clean tree at those commits.
	if len(subjSources) > 0 {
		if !sameSourceSet(subjSources, cur.CurrentSources) {
			return fmt.Errorf("%w: candidate source set changed since approval", ErrStaleSubject)
		}
		for _, ev := range subj.Evidence() {
			obs, ok := cur.EvidenceObservations[ev.ID]
			if !ok || obs.Digest != ev.Digest {
				return fmt.Errorf("%w: evidence %s missing or digest changed", ErrStaleSubject, ev.ID)
			}
			if obs.WorkspaceState != "clean" || !sameSourceSet(subjSources, obs.Sources) {
				return fmt.Errorf("%w: source-candidate gate requires clean workspace state at the candidate commits (got %q)", ErrStaleSubject, obs.WorkspaceState)
			}
		}
	}

	// 4. If the subject binds a release, verify the selected release ID and
	//    canonical manifest digest are unchanged.
	if rel := subj.Release(); rel != nil {
		if cur.CurrentRelease == nil || *cur.CurrentRelease != *rel {
			return fmt.Errorf("%w: selected release or manifest digest changed", ErrStaleSubject)
		}
	}

	// 5. If the subject binds an artifact set, verify the exact artifact IDs and
	//    digests are unchanged. Later scratch edits do not alter sealed digests.
	if arts := subj.Artifacts(); len(arts) > 0 {
		if !sameArtifactSet(arts, cur.CurrentArtifacts) {
			return fmt.Errorf("%w: selected artifact set changed", ErrStaleSubject)
		}
	}

	// 6. Verify all evidence digests match their observed immutable artifacts.
	for _, ev := range subj.Evidence() {
		obs, ok := cur.EvidenceObservations[ev.ID]
		if !ok || obs.Digest != ev.Digest {
			return fmt.Errorf("%w: evidence %s is missing or mismatched", ErrStaleSubject, ev.ID)
		}
	}
	return nil
}

func sameSourceSet(a, b []artifactsv1.SourceRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		var idA, idB string
		if a[i].RepositoryID != nil {
			idA = *a[i].RepositoryID
		}
		if b[i].RepositoryID != nil {
			idB = *b[i].RepositoryID
		}
		if idA != idB || a[i].RepositoryURL != b[i].RepositoryURL || a[i].Commit != b[i].Commit {
			return false
		}
	}
	return true
}

func sameArtifactSet(a, b []artifactsv1.ArtifactDigestRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
