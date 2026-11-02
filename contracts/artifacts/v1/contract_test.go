package artifactsv1

import (
	"bytes"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func Test_unit_release_manifest_canonical(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	dA := runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
	dB := runtimev1.Digest("sha256:" + strings.Repeat("b", 64))
	dC := runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
	mtTar := "application/x-tar"
	mtJSON := "application/json"
	repoA := "repo_api"
	repoB := "repo_web"
	clean := "clean"
	modified := "modified"
	toolImg := "registry.example.invalid/builder@sha256:" + strings.Repeat("1", 64)

	in1 := ReleaseManifestInput{
		Scope:        scope,
		Name:         "application",
		VersionLabel: "1.4.2",
		Artifacts: map[string]ManifestEntry{
			"web-bundle": {ArtifactID: "art_web", Digest: dB, Kind: runtimev1.KindFile, MediaType: &mtTar},
			"api-image":  {ArtifactID: "art_img", Digest: dA, Kind: runtimev1.KindOCI, MediaType: nil},
			"coverage":   {ArtifactID: "art_cov", Digest: dC, Kind: runtimev1.KindFile, MediaType: &mtJSON},
		},
		Sources: []SourceRef{
			{RepositoryID: &repoB, RepositoryURL: "https://git.example.invalid/acme/web.git", Commit: strings.Repeat("2", 40)},
			{RepositoryID: &repoA, RepositoryURL: "https://git.example.invalid/acme/api.git", Commit: strings.Repeat("1", 40)},
			// Duplicate source collapses deterministically.
			{RepositoryID: &repoA, RepositoryURL: "https://git.example.invalid/acme/api.git", Commit: strings.Repeat("1", 40)},
		},
		Producers: []ProducerRef{
			{ArtifactID: "art_cov", AttemptID: "att_03", RunID: "run_01", StepID: "test", ToolImage: &toolImg, WorkspaceState: &modified, ConsumedArtifacts: []ArtifactDigestRef{{ID: "art_web", Digest: dB}}},
			{ArtifactID: "art_img", AttemptID: "att_01", RunID: "run_01", StepID: "build-img", ToolImage: &toolImg, WorkspaceState: &clean, ConsumedArtifacts: nil},
			{ArtifactID: "art_web", AttemptID: "att_02", RunID: "run_01", StepID: "build-web", ToolImage: &toolImg, WorkspaceState: &clean, ConsumedArtifacts: nil},
		},
	}

	// Reordered sources and producers must produce identical canonical bytes and digest.
	in2 := ReleaseManifestInput{
		Scope:        scope,
		Name:         "application",
		VersionLabel: "1.4.2",
		Artifacts: map[string]ManifestEntry{
			"api-image":  {ArtifactID: "art_img", Digest: dA, Kind: runtimev1.KindOCI, MediaType: nil},
			"coverage":   {ArtifactID: "art_cov", Digest: dC, Kind: runtimev1.KindFile, MediaType: &mtJSON},
			"web-bundle": {ArtifactID: "art_web", Digest: dB, Kind: runtimev1.KindFile, MediaType: &mtTar},
		},
		Sources: []SourceRef{
			{RepositoryID: &repoA, RepositoryURL: "https://git.example.invalid/acme/api.git", Commit: strings.Repeat("1", 40)},
			{RepositoryID: &repoB, RepositoryURL: "https://git.example.invalid/acme/web.git", Commit: strings.Repeat("2", 40)},
		},
		Producers: []ProducerRef{
			{ArtifactID: "art_web", AttemptID: "att_02", RunID: "run_01", StepID: "build-web", ToolImage: &toolImg, WorkspaceState: &clean},
			{ArtifactID: "art_img", AttemptID: "att_01", RunID: "run_01", StepID: "build-img", ToolImage: &toolImg, WorkspaceState: &clean},
			{ArtifactID: "art_cov", AttemptID: "att_03", RunID: "run_01", StepID: "test", ToolImage: &toolImg, WorkspaceState: &modified, ConsumedArtifacts: []ArtifactDigestRef{{ID: "art_web", Digest: dB}}},
		},
	}

	m1, err := NewReleaseManifest(in1)
	if err != nil {
		t.Fatalf("NewReleaseManifest(in1): %v", err)
	}
	m2, err := NewReleaseManifest(in2)
	if err != nil {
		t.Fatalf("NewReleaseManifest(in2): %v", err)
	}
	if m1.Digest() != m2.Digest() || !bytes.Equal(m1.Canonical(), m2.Canonical()) {
		t.Fatalf("reordered equivalent manifests differ:\nd1=%s\nd2=%s", m1.Digest(), m2.Digest())
	}

	parsed, err := ParseReleaseManifest(m1.Canonical())
	if err != nil {
		t.Fatalf("ParseReleaseManifest: %v", err)
	}
	if parsed.Digest() != m1.Digest() {
		t.Fatalf("parsed digest %s != %s", parsed.Digest(), m1.Digest())
	}

	// Source-free release manifest has explicit empty sources [] and never fabricates a commit.
	sf, err := NewReleaseManifest(ReleaseManifestInput{
		Scope:        scope,
		Name:         "application",
		VersionLabel: "1.4.3-sourcefree",
		Artifacts: map[string]ManifestEntry{
			"bundle": {ArtifactID: "art_web", Digest: dB, Kind: runtimev1.KindFile, MediaType: &mtTar},
		},
		Sources: nil,
		Producers: []ProducerRef{
			{ArtifactID: "art_web", AttemptID: "att_02", RunID: "run_02", StepID: "package"},
		},
	})
	if err != nil {
		t.Fatalf("source-free manifest failed: %v", err)
	}
	if !strings.Contains(string(sf.Canonical()), `"sources":[]`) {
		t.Fatalf("expected explicit empty sources array in canonical JSON: %s", sf.Canonical())
	}

	// Conflicting digest for the same artifact ID under two entry names is rejected.
	badConflict := in2
	badConflict.Artifacts = map[string]ManifestEntry{
		"a": {ArtifactID: "art_img", Digest: dA, Kind: runtimev1.KindOCI},
		"b": {ArtifactID: "art_img", Digest: dB, Kind: runtimev1.KindOCI},
	}
	if _, err := NewReleaseManifest(badConflict); err == nil {
		t.Fatal("expected conflicting digests for same artifact ID to fail")
	}

	// Missing producer lineage for an entry is rejected.
	badMissingProducer := in2
	badMissingProducer.Producers = badMissingProducer.Producers[:1]
	if _, err := NewReleaseManifest(badMissingProducer); err == nil {
		t.Fatal("expected missing producer lineage to fail")
	}

	// Publication receipt validation: requires matching digest and completeGraph=true.
	receipt := PublicationReceipt{
		ID:             "pub_01",
		Scope:          scope,
		ArtifactID:     "art_img",
		ArtifactDigest: dA,
		Destination: DestinationRef{
			Kind:      DestinationOCI,
			Reference: "registry.prod.example.invalid/acme/inventory",
			Digest:    dA,
		},
		RunID:         "run_01",
		AttemptID:     "att_publish",
		StepID:        "publish",
		CompleteGraph: true,
		VerifiedAt:    "2026-10-30T12:00:00Z",
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	partial := receipt
	partial.CompleteGraph = false
	if err := partial.Validate(); err == nil {
		t.Fatal("partial OCI graph must not validate as a publication receipt")
	}
	mismatch := receipt
	mismatch.Destination.Digest = dB
	if err := mismatch.Validate(); err == nil {
		t.Fatal("mismatched destination digest must be rejected as a same-content receipt")
	}
}
