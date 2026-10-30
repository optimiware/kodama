package gates_test

import (
	"errors"
	"strings"
	"testing"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

func Test_unit_artifact_gate_provenance(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	dBuild := runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
	dCov := runtimev1.Digest("sha256:" + strings.Repeat("b", 64))
	dRel := runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
	commit1 := strings.Repeat("1", 40)
	commit2 := strings.Repeat("2", 40)
	src1 := []artifactsv1.SourceRef{{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: commit1}}

	// 1. Artifact/release gate allows generated outputs produced in "modified"
	//    shared scratch without requiring a Git commit, and remains valid even
	//    if later steps write to scratch (because the sealed artifact/release
	//    digests do not change).
	relPin := deliveryv1.ReleasePin{ID: "rel_142", ManifestDigest: dRel}
	relSubj, err := deliveryv1.NewGateSubjectV2(deliveryv1.GateSubjectV2Input{
		Scope:        scope,
		Release:      &relPin,
		Artifacts:    []artifactsv1.ArtifactDigestRef{{ID: "art_build", Digest: dBuild}},
		DefinitionID: "pd_release",
		Evidence:     []artifactsv1.ArtifactDigestRef{{ID: "art_cov", Digest: dCov}},
	})
	if err != nil {
		t.Fatalf("NewGateSubjectV2 release: %v", err)
	}
	curRel := gates.CurrentApplicabilityV2{
		CurrentRelease:   &relPin,
		CurrentArtifacts: []artifactsv1.ArtifactDigestRef{{ID: "art_build", Digest: dBuild}},
		EvidenceObservations: map[domain.ArtifactID]gates.ProvenanceObservation{
			"art_cov": {ArtifactID: "art_cov", Digest: dCov, Sources: src1, WorkspaceState: "modified"},
		},
	}
	if err := gates.CheckApplicabilityV2(relSubj, curRel, false); err != nil {
		t.Fatalf("expected artifact/release gate with generated-file evidence to pass: %v", err)
	}

	// 2. Artifact-only gate cannot be used to claim Git branch integration authority.
	if err := gates.CheckApplicabilityV2(relSubj, curRel, true); !errors.Is(err, gates.ErrStaleSubject) {
		t.Fatalf("expected ErrStaleSubject when using artifact-only gate for Git integration, got %v", err)
	}

	// 3. Changing the selected release manifest digest invalidates the approval.
	changedRel := curRel
	changedPin := deliveryv1.ReleasePin{ID: "rel_142", ManifestDigest: runtimev1.Digest("sha256:" + strings.Repeat("9", 64))}
	changedRel.CurrentRelease = &changedPin
	if err := gates.CheckApplicabilityV2(relSubj, changedRel, false); !errors.Is(err, gates.ErrStaleSubject) {
		t.Fatalf("expected ErrStaleSubject when release manifest digest changes, got %v", err)
	}

	// 4. Source-candidate gate rejects modified/dirty scratch masquerading as a
	//    clean candidate commit, and rejects changed source commits.
	srcSubj, err := deliveryv1.NewGateSubjectV2(deliveryv1.GateSubjectV2Input{
		Scope:        scope,
		Sources:      src1,
		DefinitionID: "pd_source",
		Evidence:     []artifactsv1.ArtifactDigestRef{{ID: "art_cov", Digest: dCov}},
	})
	if err != nil {
		t.Fatalf("NewGateSubjectV2 source: %v", err)
	}
	dirtySourceObs := gates.CurrentApplicabilityV2{
		CurrentSources: src1,
		EvidenceObservations: map[domain.ArtifactID]gates.ProvenanceObservation{
			"art_cov": {ArtifactID: "art_cov", Digest: dCov, Sources: src1, WorkspaceState: "modified"},
		},
	}
	if err := gates.CheckApplicabilityV2(srcSubj, dirtySourceObs, true); !errors.Is(err, gates.ErrStaleSubject) {
		t.Fatalf("expected modified workspace evidence to be rejected for clean source candidate gate, got %v", err)
	}

	cleanSourceObs := gates.CurrentApplicabilityV2{
		CurrentSources: src1,
		EvidenceObservations: map[domain.ArtifactID]gates.ProvenanceObservation{
			"art_cov": {ArtifactID: "art_cov", Digest: dCov, Sources: src1, WorkspaceState: "clean"},
		},
	}
	if err := gates.CheckApplicabilityV2(srcSubj, cleanSourceObs, true); err != nil {
		t.Fatalf("expected clean source candidate gate to pass: %v", err)
	}

	movedSourceObs := cleanSourceObs
	movedSourceObs.CurrentSources = []artifactsv1.SourceRef{{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: commit2}}
	if err := gates.CheckApplicabilityV2(srcSubj, movedSourceObs, true); !errors.Is(err, gates.ErrStaleSubject) {
		t.Fatalf("expected moved candidate commit to invalidate source gate, got %v", err)
	}
}
