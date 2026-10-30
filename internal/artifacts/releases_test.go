package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

type testReleaseAuth struct {
	allowScope domain.Scope
	actor      artifacts.Actor
	inputs     map[string]artifactsv1.InputFetchRequest
	grant      artifacts.AttemptGrant
}

func (a *testReleaseAuth) AuthorizeReleaseRead(_ context.Context, _ artifacts.Caller, scope domain.Scope) error {
	if scope != a.allowScope {
		return artifacts.ErrNotFound
	}
	return nil
}

func (a *testReleaseAuth) AuthorizeReleaseWrite(_ context.Context, _ artifacts.Caller, scope domain.Scope) (artifacts.Actor, error) {
	if scope != a.allowScope {
		return artifacts.Actor{}, artifacts.ErrForbidden
	}
	return a.actor, nil
}

func (a *testReleaseAuth) AuthorizeInputFetch(_ context.Context, _ artifacts.Caller, attempt domain.AttemptID, req artifactsv1.InputFetchRequest) (domain.Scope, time.Time, error) {
	if attempt != a.grant.Attempt {
		return domain.Scope{}, time.Time{}, artifacts.ErrAuthorityMissing
	}
	allowed, ok := a.inputs[req.Input]
	if !ok || allowed.ArtifactID != req.ArtifactID || allowed.Digest != req.Digest {
		return domain.Scope{}, time.Time{}, artifacts.ErrForbidden
	}
	return a.allowScope, a.grant.ExpiresAt, nil
}

func (a *testReleaseAuth) AuthorizePublication(_ context.Context, _ artifacts.Caller, attempt domain.AttemptID, _ domain.ArtifactID, _ artifactsv1.DestinationRef) (artifacts.AttemptGrant, error) {
	if attempt != a.grant.Attempt {
		return artifacts.AttemptGrant{}, artifacts.ErrAuthorityMissing
	}
	return a.grant, nil
}

type testPubVerifier struct {
	err error
}

func (v *testPubVerifier) VerifyCopy(_ context.Context, _ domain.Scope, _ artifacts.Artifact, _ artifactsv1.DestinationRef) error {
	return v.err
}

func sealFileArtifact(t *testing.T, e *artifactstest.MemEnv, scope domain.Scope, attempt domain.AttemptID, name, mediaType string, data []byte) artifacts.Artifact {
	t.Helper()
	caller := e.Attempt(scope, attempt, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
	dg := artifacts.DigestOf(data)
	sz := int64(len(data))
	res, err := e.Service.Reserve(t.Context(), caller, attempt, artifacts.ReserveInput{
		Name: name, Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: dg, MediaType: &mediaType, Size: &sz,
	}, "req_res")
	if err != nil {
		t.Fatalf("Reserve(%s): %v", name, err)
	}
	stored, err := e.Store.Get(t.Context(), scope, res.ID)
	if err != nil {
		t.Fatalf("Store.Get(%s): %v", name, err)
	}
	if err := e.Objects.Put(t.Context(), artifacts.PutSpec{
		Key: stored.StagingKey(), Body: bytes.NewReader(data), Size: sz, Digest: dg, MediaType: mediaType,
	}); err != nil {
		t.Fatalf("Objects.Put(%s): %v", name, err)
	}
	if _, err := e.Service.Complete(t.Context(), caller, attempt, res.ID, artifacts.CompleteInput{
		Digest: dg, Size: sz,
	}, "req_cmp"); err != nil {
		t.Fatalf("Complete(%s): %v", name, err)
	}
	art, err := e.Store.Get(t.Context(), scope, res.ID)
	if err != nil {
		t.Fatalf("Store.Get completed(%s): %v", name, err)
	}
	return art
}

func Test_unit_release_identity_conflict(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	artA := sealFileArtifact(t, e, scope, "att_01", "bundle", "application/x-tar", []byte("bundle-v1-bytes"))
	artB := sealFileArtifact(t, e, scope, "att_02", "bundle", "application/x-tar", []byte("bundle-v2-bytes"))

	auth := &testReleaseAuth{
		allowScope: scope,
		actor:      artifacts.Actor{Type: "user", ID: "usr_eng"},
	}
	mt := "application/x-tar"
	in1 := artifactsv1.ReleaseManifestInput{
		Scope:        scope,
		Name:         "application",
		VersionLabel: "1.0.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{
			"bundle": {ArtifactID: artA.ID(), Digest: runtimev1.Digest(artA.Digest()), Kind: runtimev1.KindFile, MediaType: &mt},
		},
		Sources: []artifactsv1.SourceRef{
			{RepositoryURL: artifactstest.Source.RepositoryURL, Commit: artifactstest.Source.Commit},
		},
		Producers: []artifactsv1.ProducerRef{
			{ArtifactID: artA.ID(), AttemptID: "att_01", RunID: "run_1", StepID: "build"},
		},
	}

	rel1, created, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, in1, "req_1", e.Store, auth)
	if err != nil || !created {
		t.Fatalf("CreateRelease first call: created=%v err=%v", created, err)
	}

	// Identical repeat converges on the same release.
	rel2, created2, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, in1, "req_2", e.Store, auth)
	if err != nil || created2 || rel2.ID() != rel1.ID() || rel2.ManifestDigest() != rel1.ManifestDigest() {
		t.Fatalf("CreateRelease replay: created=%v id=%s err=%v", created2, rel2.ID(), err)
	}

	// Same (name, versionLabel) with different artifact content conflicts.
	inConflict := in1
	inConflict.Artifacts = map[string]artifactsv1.ManifestEntry{
		"bundle": {ArtifactID: artB.ID(), Digest: runtimev1.Digest(artB.Digest()), Kind: runtimev1.KindFile, MediaType: &mt},
	}
	inConflict.Producers = []artifactsv1.ProducerRef{
		{ArtifactID: artB.ID(), AttemptID: "att_02", RunID: "run_1", StepID: "build"},
	}
	if _, _, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, inConflict, "req_3", e.Store, auth); !errors.Is(err, artifacts.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for changed release manifest, got %v", err)
	}
}

func Test_unit_release_reference_retention(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	art := sealFileArtifact(t, e, scope, "att_01", "bundle", "application/x-tar", []byte("retained-release-bytes"))

	auth := &testReleaseAuth{
		allowScope: scope,
		actor:      artifacts.Actor{Type: "user", ID: "usr_eng"},
	}
	mt := "application/x-tar"
	rel, _, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, artifactsv1.ReleaseManifestInput{
		Scope:        scope,
		Name:         "application",
		VersionLabel: "1.0.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{
			"bundle": {ArtifactID: art.ID(), Digest: runtimev1.Digest(art.Digest()), Kind: runtimev1.KindFile, MediaType: &mt},
		},
		Producers: []artifactsv1.ProducerRef{
			{ArtifactID: art.ID(), AttemptID: "att_01", RunID: "run_1", StepID: "build"},
		},
	}, "req_rel", e.Store, auth)
	if err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}

	// Also pin the artifact for a deployment so retiring the release alone cannot delete it.
	if err := e.Service.Pin(t.Context(), scope, art.ID(), artifacts.PinOwner{Kind: "deployment", ID: "dep_01"}); err != nil {
		t.Fatalf("Pin deployment: %v", err)
	}

	// While pinned by the release, retention cannot remove the artifact.
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, art.ID(), e.Clock.Now(), artifacts.Actor{Type: "system", ID: "reconciler"}); !errors.Is(err, artifacts.ErrPinned) {
		t.Fatalf("expected ErrPinned while release is active, got %v", err)
	}

	// Retiring with wrong version fails with ErrVersionConflict.
	if _, err := e.Service.RetireRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, rel.ID(), 99, "req_ret_bad", e.Store, auth); !errors.Is(err, artifacts.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}

	// Retire the release: ResolveRelease now refuses it, GetRelease still reads it,
	// and the deployment pin still protects the artifact from retention deletion!
	retired, err := e.Service.RetireRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, rel.ID(), rel.Version(), "req_ret", e.Store, auth)
	if err != nil || !retired.Retired() || retired.Version() != 2 {
		t.Fatalf("RetireRelease: retired=%v version=%d err=%v", retired.Retired(), retired.Version(), err)
	}
	if _, err := e.Service.ResolveRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, "application", "1.0.0", e.Store, auth); !errors.Is(err, artifacts.ErrReleaseRetired) {
		t.Fatalf("expected ErrReleaseRetired from ResolveRelease, got %v", err)
	}
	if got, err := e.Service.GetRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, rel.ID(), e.Store, auth); err != nil || !got.Retired() {
		t.Fatalf("GetRelease on retired release failed: %v", err)
	}
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, art.ID(), e.Clock.Now(), artifacts.Actor{Type: "system", ID: "reconciler"}); !errors.Is(err, artifacts.ErrPinned) {
		t.Fatalf("expected deployment pin to keep protecting artifact after release retirement, got %v", err)
	}

	// Once the deployment also unpins, retention deletion succeeds.
	if err := e.Service.Unpin(t.Context(), scope, art.ID(), artifacts.PinOwner{Kind: "deployment", ID: "dep_01"}); err != nil {
		t.Fatalf("Unpin deployment: %v", err)
	}
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, art.ID(), e.Clock.Now(), artifacts.Actor{Type: "system", ID: "reconciler"}); err != nil {
		t.Fatalf("expected retention deletion to succeed after all pins released, got %v", err)
	}
}

func Test_unit_artifact_catalog_scope(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scopeA := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	scopeB := domain.Scope{TenantID: "t_acme", ProjectID: "p_other"}
	artA := sealFileArtifact(t, e, scopeA, "att_a", "report", "application/json", []byte(`{"coverage":92}`))
	_ = sealFileArtifact(t, e, scopeB, "att_b", "report", "application/json", []byte(`{"coverage":50}`))

	auth := &testReleaseAuth{
		allowScope: scopeA,
		actor:      artifacts.Actor{Type: "user", ID: "usr_eng"},
		grant: artifacts.AttemptGrant{
			Scope:     scopeA,
			Attempt:   "att_consumer",
			Run:       "run_2",
			Step:      "deploy",
			ExpiresAt: e.Clock.Now().Add(15 * time.Minute),
		},
		inputs: map[string]artifactsv1.InputFetchRequest{
			"report": {Input: "report", ArtifactID: artA.ID(), Digest: runtimev1.Digest(artA.Digest()), Download: true},
		},
	}

	// Scoped listing returns only scopeA's artifact; foreign scope is hidden as ErrNotFound.
	list, err := e.Service.ListArtifacts(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scopeA, "", 50, e.Store, auth)
	if err != nil || len(list) != 1 || list[0].ID() != artA.ID() {
		t.Fatalf("ListArtifacts(scopeA): len=%d err=%v", len(list), err)
	}
	if _, err := e.Service.ListArtifacts(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scopeB, "", 50, e.Store, auth); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign scopeB, got %v", err)
	}

	// Admitted input fetch succeeds for bound input and fails for undeclared/wrong digest.
	fetched, err := e.Service.FetchInput(t.Context(), artifacts.Caller{Subject: "att_consumer"}, "att_consumer", artifactsv1.InputFetchRequest{
		Input: "report", ArtifactID: artA.ID(), Digest: runtimev1.Digest(artA.Digest()), Download: true,
	}, auth)
	if err != nil || fetched.Download == nil || fetched.Download.URL == "" {
		t.Fatalf("FetchInput valid: %+v err=%v", fetched, err)
	}
	if _, err := e.Service.FetchInput(t.Context(), artifacts.Caller{Subject: "att_consumer"}, "att_consumer", artifactsv1.InputFetchRequest{
		Input: "undeclared", ArtifactID: artA.ID(), Digest: runtimev1.Digest(artA.Digest()), Download: true,
	}, auth); !errors.Is(err, artifacts.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for undeclared input, got %v", err)
	}
}

func Test_unit_publication_copy_identity(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	payload := []byte("published-chart-tgz-bytes")
	art := sealFileArtifact(t, e, scope, "att_build", "chart", "application/gzip", payload)

	// VerifyFileStream checks exact size and SHA-256 digest.
	if err := artifacts.VerifyFileStream(bytes.NewReader(payload), int64(len(payload)), art.Digest()); err != nil {
		t.Fatalf("VerifyFileStream valid: %v", err)
	}
	if err := artifacts.VerifyFileStream(bytes.NewReader([]byte("tampered-chart-tgz-bytes!")), int64(len(payload)), art.Digest()); !errors.Is(err, artifacts.ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch on tampered stream, got %v", err)
	}

	auth := &testReleaseAuth{
		allowScope: scope,
		actor:      artifacts.Actor{Type: "workload", ID: "att_publish"},
		grant: artifacts.AttemptGrant{
			Scope:     scope,
			Attempt:   "att_publish",
			Run:       "run_pub",
			Step:      "publish",
			ExpiresAt: e.Clock.Now().Add(15 * time.Minute),
		},
	}
	verifier := &testPubVerifier{}
	dest := artifactsv1.DestinationRef{
		Kind:      artifactsv1.DestinationS3,
		Reference: "s3://acme-prod-artifacts/charts/inventory-1.0.0.tgz",
		Digest:    runtimev1.Digest(art.Digest()),
	}

	rec1, created, err := e.Service.RecordPublication(t.Context(), artifacts.Caller{Subject: "att_publish"}, "att_publish", art.ID(), dest, "req_pub_1", e.Store, auth, verifier)
	if err != nil || !created {
		t.Fatalf("RecordPublication first: created=%v err=%v", created, err)
	}
	rec2, created2, err := e.Service.RecordPublication(t.Context(), artifacts.Caller{Subject: "att_publish"}, "att_publish", art.ID(), dest, "req_pub_2", e.Store, auth, verifier)
	if err != nil || created2 || rec2.ID != rec1.ID {
		t.Fatalf("RecordPublication replay: created=%v id=%s err=%v", created2, rec2.ID, err)
	}

	// Transformed digest at destination must be registered as a derived artifact, not a same-copy receipt.
	badDest := dest
	badDest.Digest = runtimev1.Digest("sha256:" + strings.Repeat("f", 64))
	if _, _, err := e.Service.RecordPublication(t.Context(), artifacts.Caller{Subject: "att_publish"}, "att_publish", art.ID(), badDest, "req_pub_3", e.Store, auth, verifier); !errors.Is(err, artifacts.ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch for transformed destination digest, got %v", err)
	}
}
