package containersdlc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

type e2eReleaseAuth struct {
	scope  domain.Scope
	inputs map[string]artifactsv1.InputFetchRequest
}

func (a *e2eReleaseAuth) AuthorizeReleaseRead(_ context.Context, _ artifacts.Caller, sc domain.Scope) error {
	if sc != a.scope {
		return artifacts.ErrNotFound
	}
	return nil
}

func (a *e2eReleaseAuth) AuthorizeReleaseWrite(_ context.Context, _ artifacts.Caller, sc domain.Scope) (artifacts.Actor, error) {
	if sc != a.scope {
		return artifacts.Actor{}, artifacts.ErrForbidden
	}
	return artifacts.Actor{Type: "user", ID: "usr_eng"}, nil
}

func (a *e2eReleaseAuth) AuthorizeInputFetch(_ context.Context, _ artifacts.Caller, _ domain.AttemptID, req artifactsv1.InputFetchRequest) (domain.Scope, time.Time, error) {
	allowed, ok := a.inputs[req.Input]
	if !ok || allowed.ArtifactID != req.ArtifactID || allowed.Digest != req.Digest {
		return domain.Scope{}, time.Time{}, artifacts.ErrForbidden
	}
	return a.scope, time.Now().UTC().Add(30 * time.Minute), nil
}

func (a *e2eReleaseAuth) AuthorizePublication(_ context.Context, _ artifacts.Caller, attempt domain.AttemptID, _ domain.ArtifactID, _ artifactsv1.DestinationRef) (artifacts.AttemptGrant, error) {
	return artifacts.AttemptGrant{
		Scope:     a.scope,
		Attempt:   attempt,
		Run:       "run_publish",
		Step:      "publish",
		ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}, nil
}

type serviceInputFetcher struct {
	svc     *artifacts.Service
	auth    artifacts.ReleaseAuthority
	attempt domain.AttemptID
	client  *http.Client
}

func (f serviceInputFetcher) FetchInput(ctx context.Context, req artifactsv1.InputFetchRequest) (artifactsv1.InputFetchResponse, error) {
	return f.svc.FetchInput(ctx, artifacts.Caller{Subject: string(f.attempt)}, f.attempt, req, f.auth)
}

func (f serviceInputFetcher) DownloadInput(ctx context.Context, target runtimev1.Upload) (io.ReadCloser, error) {
	return entrypoint.HTTPInputDownload(ctx, f.client, target)
}

func createUsableFile(t *testing.T, e *artifactstest.MemEnv, scope domain.Scope, attempt domain.AttemptID, name, mediaType string, content []byte) artifacts.Artifact {
	t.Helper()
	caller := e.Attempt(scope, attempt, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
	dg := artifacts.DigestOf(content)
	sz := int64(len(content))
	res, err := e.Service.Reserve(t.Context(), caller, attempt, artifacts.ReserveInput{
		Name: name, Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: dg, MediaType: &mediaType, Size: &sz,
	}, "req_res_"+name)
	if err != nil {
		t.Fatalf("Reserve %s: %v", name, err)
	}
	stored, _ := e.Store.Get(t.Context(), scope, res.ID)
	if err := e.Objects.Put(t.Context(), artifacts.PutSpec{
		Key: stored.StagingKey(), Body: bytes.NewReader(content), Size: sz, Digest: dg, MediaType: mediaType,
	}); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
	if _, err := e.Service.Complete(t.Context(), caller, attempt, res.ID, artifacts.CompleteInput{Digest: dg, Size: sz}, "req_cmp_"+name); err != nil {
		t.Fatalf("Complete %s: %v", name, err)
	}
	art, err := e.Store.Get(t.Context(), scope, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	return art
}

func Test_sdlc_release_roundtrip(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	// Retain build archive, Helm chart .tgz, and JSON coverage report.
	buildTar := createUsableFile(t, e, scope, "att_build", "build-archive", "application/x-tar", []byte("binary-and-static-assets-tar"))
	chartTgz := createUsableFile(t, e, scope, "att_chart", "helm-chart", "application/gzip", []byte("packaged-helm-chart-tgz"))
	covJSON := createUsableFile(t, e, scope, "att_test", "coverage", "application/json", []byte(`{"lines":94.2}`))

	mtTar := "application/x-tar"
	mtGzip := "application/gzip"
	mtJSON := "application/json"
	auth := &e2eReleaseAuth{scope: scope, inputs: map[string]artifactsv1.InputFetchRequest{}}

	rel, created, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, artifactsv1.ReleaseManifestInput{
		Scope:        scope,
		Name:         "inventory",
		VersionLabel: "1.0.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{
			"build":    {ArtifactID: buildTar.ID(), Digest: runtimev1.Digest(buildTar.Digest()), Kind: runtimev1.KindFile, MediaType: &mtTar},
			"chart":    {ArtifactID: chartTgz.ID(), Digest: runtimev1.Digest(chartTgz.Digest()), Kind: runtimev1.KindFile, MediaType: &mtGzip},
			"coverage": {ArtifactID: covJSON.ID(), Digest: runtimev1.Digest(covJSON.Digest()), Kind: runtimev1.KindFile, MediaType: &mtJSON},
		},
		Sources: []artifactsv1.SourceRef{
			{RepositoryURL: artifactstest.Source.RepositoryURL, Commit: artifactstest.Source.Commit},
		},
		Producers: []artifactsv1.ProducerRef{
			{ArtifactID: buildTar.ID(), AttemptID: "att_build", RunID: "run_1", StepID: "build"},
			{ArtifactID: chartTgz.ID(), AttemptID: "att_chart", RunID: "run_1", StepID: "package-chart"},
			{ArtifactID: covJSON.ID(), AttemptID: "att_test", RunID: "run_1", StepID: "test", ConsumedArtifacts: []artifactsv1.ArtifactDigestRef{{ID: buildTar.ID(), Digest: runtimev1.Digest(buildTar.Digest())}}},
		},
	}, "req_rel_100", e.Store, auth)
	if err != nil || !created {
		t.Fatalf("CreateRelease: created=%v err=%v", created, err)
	}

	// Later deployment run resolves release inventory@1.0.0 and fetches the exact bound chart and build archive.
	resolved, err := e.Service.ResolveRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, "inventory", "1.0.0", e.Store, auth)
	if err != nil || resolved.ID() != rel.ID() {
		t.Fatalf("ResolveRelease: %v", err)
	}
	entries := resolved.Manifest().Entries()
	auth.inputs["build"] = artifactsv1.InputFetchRequest{Input: "build", ArtifactID: entries["build"].ArtifactID, Digest: entries["build"].Digest}
	auth.inputs["chart"] = artifactsv1.InputFetchRequest{Input: "chart", ArtifactID: entries["chart"].ArtifactID, Digest: entries["chart"].Digest}

	wsDir := t.TempDir()
	fetcher := serviceInputFetcher{svc: e.Service, auth: auth, attempt: "att_deploy", client: http.DefaultClient}
	if _, err := entrypoint.FetchArtifactInput(t.Context(), fetcher, wsDir, "inputs/build.tar", auth.inputs["build"], false); err != nil {
		t.Fatalf("FetchArtifactInput build: %v", err)
	}
	if _, err := entrypoint.FetchArtifactInput(t.Context(), fetcher, wsDir, "inputs/chart.tgz", auth.inputs["chart"], false); err != nil {
		t.Fatalf("FetchArtifactInput chart: %v", err)
	}
	gotChart, err := os.ReadFile(filepath.Join(wsDir, "inputs/chart.tgz"))
	if err != nil || string(gotChart) != "packaged-helm-chart-tgz" {
		t.Fatalf("fetched chart mismatch: %q err=%v", gotChart, err)
	}
}

type ociGraphVerifierAdapter struct {
	v       *oci.Verifier
	attempt domain.AttemptID
}

func (a ociGraphVerifierAdapter) VerifyCopy(ctx context.Context, scope domain.Scope, _ artifacts.Artifact, dest artifactsv1.DestinationRef) error {
	return a.v.VerifyCompleteGraph(ctx, scope, a.attempt, dest)
}

func Test_sdlc_isolated_target_publication(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	// Set up an external production OCI registry that is reachable by the publish
	// step and production target, while Kodama's internal S3 store is never
	// contacted by production.
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgDigest := artifacts.DigestOf(cfgBytes)
	layerBytes := []byte("oci-image-layer-payload")
	layerDigest := artifacts.DigestOf(layerBytes)
	manifestMediaType := "application/vnd.oci.image.manifest.v1+json"
	manifestBytes := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`,
		manifestMediaType, cfgDigest, len(cfgBytes), layerDigest, len(layerBytes),
	))
	manifestDigest := artifacts.DigestOf(manifestBytes)

	prodRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/acme/inventory/manifests/" + string(manifestDigest):
			w.Header().Set("Content-Type", manifestMediaType)
			w.Header().Set("Docker-Content-Digest", string(manifestDigest))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(manifestBytes)
		case "/v2/acme/inventory/blobs/" + string(cfgDigest):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cfgBytes)
		case "/v2/acme/inventory/blobs/" + string(layerDigest):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(layerBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(prodRegistry.Close)
	u, _ := url.Parse(prodRegistry.URL)
	prodRepo := u.Host + "/acme/inventory"

	// Register the OCI image artifact in Kodama.
	e.Registry.Add(prodRepo, manifestDigest, artifactstest.Source)
	caller := e.Attempt(scope, "att_build_oci", artifacts.OutputDecl{Name: "image", Kind: artifacts.KindOCI})
	res, err := e.Service.Reserve(t.Context(), caller, "att_build_oci", artifacts.ReserveInput{
		Name: "image", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput, Digest: manifestDigest, Registry: &prodRepo,
	}, "req_oci")
	if err != nil {
		t.Fatalf("Reserve OCI: %v", err)
	}

	// Verify publication to the production registry with full OCI graph verification.
	ociVerifier, err := oci.NewVerifier(oci.Config{
		Policy:     fixedEndpointPolicy{base: prodRegistry.URL},
		Provenance: fixedProvenance{prov: artifactstest.Source},
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := &e2eReleaseAuth{scope: scope}
	dest := artifactsv1.DestinationRef{
		Kind:      artifactsv1.DestinationOCI,
		Reference: prodRepo,
		Digest:    runtimev1.Digest(manifestDigest),
	}
	receipt, created, err := e.Service.RecordPublication(
		t.Context(), artifacts.Caller{Subject: "att_publish"}, "att_publish", res.ID, dest, "req_pub",
		e.Store, auth, ociGraphVerifierAdapter{v: ociVerifier, attempt: "att_publish"},
	)
	if err != nil || !created || !receipt.CompleteGraph {
		t.Fatalf("RecordPublication OCI: created=%v receipt=%+v err=%v", created, receipt, err)
	}
}

type fixedEndpointPolicy struct{ base string }

func (p fixedEndpointPolicy) Authorize(_ context.Context, _ domain.Scope, _ domain.AttemptID, _ artifacts.RegistryRef) (oci.Endpoint, error) {
	return oci.Endpoint{BaseURL: p.base, Insecure: true}, nil
}

type fixedProvenance struct{ prov artifacts.Provenance }

func (p fixedProvenance) Resolve(_ context.Context, _ oci.ProvenanceQuery) (artifacts.Provenance, error) {
	return p.prov, nil
}

func Test_sdlc_release_rollback(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	auth := &e2eReleaseAuth{scope: scope}
	mt := "application/x-tar"

	// Create release 1.0.0 and 1.1.0.
	art100 := createUsableFile(t, e, scope, "att_b100", "bundle", mt, []byte("release-1.0.0-bytes"))
	art110 := createUsableFile(t, e, scope, "att_b110", "bundle", mt, []byte("release-1.1.0-bytes"))

	rel100, _, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, artifactsv1.ReleaseManifestInput{
		Scope: scope, Name: "inventory", VersionLabel: "1.0.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{"bundle": {ArtifactID: art100.ID(), Digest: runtimev1.Digest(art100.Digest()), Kind: runtimev1.KindFile, MediaType: &mt}},
		Producers: []artifactsv1.ProducerRef{{ArtifactID: art100.ID(), AttemptID: "att_b100", RunID: "run_100", StepID: "build"}},
	}, "req_100", e.Store, auth)
	if err != nil {
		t.Fatal(err)
	}
	rel110, _, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, artifactsv1.ReleaseManifestInput{
		Scope: scope, Name: "inventory", VersionLabel: "1.1.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{"bundle": {ArtifactID: art110.ID(), Digest: runtimev1.Digest(art110.Digest()), Kind: runtimev1.KindFile, MediaType: &mt}},
		Producers: []artifactsv1.ProducerRef{{ArtifactID: art110.ID(), AttemptID: "att_b110", RunID: "run_110", StepID: "build"}},
	}, "req_110", e.Store, auth)
	if err != nil {
		t.Fatal(err)
	}

	snap, err := deliveryv1.NewEnvironmentSnapshotV2(deliveryv1.EnvironmentSnapshotV2Input{
		Scope: scope, Environment: "env_prod", Name: "prod", Target: "cloudflare.workers",
		Config: map[string]string{"WORKER": "inventory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	actor := delivery.Actor{Type: delivery.ActorWorkload, ID: "att_dep_110"}

	// Deploy 1.1.0.
	dep110, err := delivery.NewGenericDeployment(deliveryv1.GenericDeploymentIntent{
		Scope: scope, ActionKey: "run_dep_110.deploy", RunID: "run_dep_110", StepID: "deploy", AttemptID: "att_dep_110",
		Environment: snap.Pin(),
		Release:     &deliveryv1.ReleasePin{ID: rel110.ID(), ManifestDigest: runtimev1.Digest(rel110.ManifestDigest())},
		Artifacts:   []artifactsv1.ArtifactDigestRef{{ID: art110.ID(), Digest: runtimev1.Digest(art110.Digest())}},
		Script:      deliveryv1.ScriptIdentity{Image: "registry.example.invalid/wrangler@sha256:" + strings.Repeat("1", 64), Command: []string{"wrangler", "deploy"}},
	}, actor, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = dep110.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{Status: "succeeded", Attribution: deliveryv1.AttributionToolObserved}, actor, "att_dep_110", now.Add(time.Second))

	// Rollback run selects prior release 1.0.0 without rebuilding, linking supersededDeploymentId=dep110.ID().
	superseded := string(dep110.ID())
	rbActor := delivery.Actor{Type: delivery.ActorWorkload, ID: "att_rb_100"}
	depRollback, err := delivery.NewGenericDeployment(deliveryv1.GenericDeploymentIntent{
		Scope: scope, ActionKey: "run_rb_100.deploy", RunID: "run_rb_100", StepID: "deploy", AttemptID: "att_rb_100",
		Environment:            snap.Pin(),
		Release:                &deliveryv1.ReleasePin{ID: rel100.ID(), ManifestDigest: runtimev1.Digest(rel100.ManifestDigest())},
		Artifacts:              []artifactsv1.ArtifactDigestRef{{ID: art100.ID(), Digest: runtimev1.Digest(art100.Digest())}},
		Script:                 deliveryv1.ScriptIdentity{Image: "registry.example.invalid/wrangler@sha256:" + strings.Repeat("1", 64), Command: []string{"wrangler", "deploy"}},
		SupersededDeploymentID: &superseded,
	}, rbActor, 0, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := depRollback.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{Status: "succeeded", Attribution: deliveryv1.AttributionToolObserved}, rbActor, "att_rb_100", now.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	// Historical dep110 outcome and history remain untouched.
	if dep110.Status() != delivery.StatusSucceeded || depRollback.Intent().Release.ID != rel100.ID() {
		t.Fatalf("unexpected rollback state: dep110=%s rb=%+v", dep110.Status(), depRollback.Record())
	}
}

func Test_sdlc_retention_recovery(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	auth := &e2eReleaseAuth{scope: scope}
	mt := "application/x-tar"

	art := createUsableFile(t, e, scope, "att_ret", "bundle", mt, []byte("protected-by-release-pin"))
	rel, _, err := e.Service.CreateRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, artifactsv1.ReleaseManifestInput{
		Scope: scope, Name: "inventory", VersionLabel: "2.0.0",
		Artifacts: map[string]artifactsv1.ManifestEntry{"bundle": {ArtifactID: art.ID(), Digest: runtimev1.Digest(art.Digest()), Kind: runtimev1.KindFile, MediaType: &mt}},
		Producers: []artifactsv1.ProducerRef{{ArtifactID: art.ID(), AttemptID: "att_ret", RunID: "run_200", StepID: "build"}},
	}, "req_200", e.Store, auth)
	if err != nil {
		t.Fatal(err)
	}

	// Retention deletion is blocked while the release is active.
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, art.ID(), e.Clock.Now(), artifacts.Actor{Type: "system", ID: "retention"}); !errors.Is(err, artifacts.ErrPinned) {
		t.Fatalf("expected ErrPinned while release is active, got %v", err)
	}

	// Retire the release and allow retention deletion; attempting to resolve or
	// consume the removed bytes afterwards fails visibly with ErrReleaseRetired / ErrIncomplete.
	if _, err := e.Service.RetireRelease(t.Context(), artifacts.Caller{Subject: "usr_eng"}, scope, rel.ID(), rel.Version(), "req_retire", e.Store, auth); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, art.ID(), e.Clock.Now(), artifacts.Actor{Type: "system", ID: "retention"}); err != nil {
		t.Fatalf("BeginRetentionDeletion after retirement: %v", err)
	}
	auth.inputs = map[string]artifactsv1.InputFetchRequest{
		"bundle": {Input: "bundle", ArtifactID: art.ID(), Digest: runtimev1.Digest(art.Digest()), Download: true},
	}
	if _, err := e.Service.FetchInput(t.Context(), artifacts.Caller{Subject: "att_dep"}, "att_dep", auth.inputs["bundle"], auth); !errors.Is(err, artifacts.ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete when fetching retention-removed artifact, got %v", err)
	}
}

func Test_sdlc_legacy_cutover(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	slice, err := app.AssembleContainerSDLC(app.ContainerSDLCInputs{
		Env:               app.Development,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  &e2eReleaseAuth{scope: scope},
		PublicationVerify: fixedCopyVerifier{},
		LegacyMode:        app.LegacyDeliveryRetired,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := slice.AdmitLegacyDelivery(); err == nil {
		t.Fatal("expected LegacyDeliveryRetired to reject legacy delivery admission")
	}
}

type fixedCopyVerifier struct{}

func (fixedCopyVerifier) VerifyCopy(_ context.Context, _ domain.Scope, _ artifacts.Artifact, _ artifactsv1.DestinationRef) error {
	return nil
}
