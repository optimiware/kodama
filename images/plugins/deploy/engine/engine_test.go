package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/state"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

type resolver struct{ write bool }

func (r resolver) ResolveBinding(_ context.Context, _ *http.Request, attempt, alias string) (state.Scope, state.Binding, error) {
	if attempt != "att_engine" || alias != "release" {
		return state.Scope{}, state.Binding{}, state.ErrDenied
	}
	return state.Scope{TenantID: domain.TenantID("t_engine"), ProjectID: domain.ProjectID("p_engine")},
		state.Binding{Namespace: "deploy", Prefix: "", Read: true, Write: r.write}, nil
}

type allowStateArtifacts struct{}

func (allowStateArtifacts) VerifyArtifact(context.Context, state.Scope, domain.ArtifactID, string) error {
	return nil
}

func newEngine(t *testing.T, d driver.Driver) (*Engine, func()) {
	t.Helper()
	mem := state.NewMemory(nil)
	h, err := state.NewHandler(mem, resolver{write: true})
	if err != nil {
		t.Fatal(err)
	}
	h.Artifacts = allowStateArtifacts{}
	srv := httptest.NewServer(h)
	client, err := pluginstate.New(srv.URL, "att_engine", "release", "")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return &Engine{State: client, Driver: d, Clock: func() time.Time { return time.Unix(1000, 0).UTC() }}, srv.Close
}

type fakeDriver struct {
	applies   int
	observes  int
	destroys  int
	destroyed bool
	obs       driver.Observation
	applyErr  error
	applyRes  driver.Result
}

func (f *fakeDriver) Identify(context.Context, driver.IdentityRequest) (driver.Identity, error) {
	return driver.Identity{Driver: "fake", DriverVersion: "1", ResourceKey: "fake/dummy", ArtifactKinds: []driver.ArtifactKind{driver.ArtifactFile}}, nil
}
func (f *fakeDriver) Apply(_ context.Context, _ driver.Request, cp driver.Checkpoint) (driver.Result, error) {
	f.applies++
	if cp != nil {
		if err := cp(context.Background(), "intent", json.RawMessage(`{"ok":true}`)); err != nil {
			return driver.Result{}, err
		}
	}
	return f.applyRes, f.applyErr
}
func (f *fakeDriver) Observe(context.Context, driver.Request) (driver.Observation, error) {
	f.observes++
	if f.destroyed {
		return driver.Observation{State: driver.Absent, ObservedAt: time.Unix(1000, 0).UTC()}, nil
	}
	if f.obs.State == "" {
		f.obs = driver.Observation{State: driver.Ready, ObservedAt: time.Unix(1000, 0).UTC(), ObservedRelease: "v1"}
	}
	return f.obs, nil
}
func (f *fakeDriver) Destroy(ctx context.Context, _ driver.Request, cp driver.Checkpoint) (driver.Result, error) {
	if cp != nil {
		if err := cp(ctx, "delete-intent", json.RawMessage(`{"ok":true}`)); err != nil {
			return driver.Result{Effect: "none"}, err
		}
	}
	f.destroys++
	f.destroyed = true
	return driver.Result{Effect: "confirmed"}, nil
}
func (f *fakeDriver) Rollback(context.Context, driver.Request, driver.Checkpoint) (driver.Result, error) {
	return driver.Result{Effect: "confirmed", ProviderVersion: "v0"}, nil
}

func testConfig() Config {
	return Config{
		ActionKey: "run:step", Resource: "dummy", Recipe: "fake", RecipeVersion: 1,
		Configuration:  json.RawMessage(`{"namespace":"deploy","release":"dummy"}`),
		DesiredRelease: "v1", Timeout: time.Minute,
	}
}

func TestApplySettlesAndReplaysWithoutSecondMutation(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed", ProviderRelease: "dummy"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	out, err := e.Apply(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if out.Operation.Status != "succeeded" || d.applies != 1 {
		t.Fatalf("first apply: %+v applies=%d", out, d.applies)
	}
	again, err := e.Apply(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if again.Operation.Status != "succeeded" || d.applies != 1 {
		t.Fatalf("replay applied again: %+v applies=%d", again, d.applies)
	}
}

func TestUncertainEffectRequiresReconcile(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "possible"}, applyErr: errors.New("timeout")}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	if _, err := e.Apply(context.Background(), testConfig()); err == nil {
		t.Fatal("expected uncertain error")
	}
	if d.applies != 1 {
		t.Fatalf("applies=%d", d.applies)
	}
	d.obs = driver.Observation{State: driver.Ready, ObservedAt: time.Unix(1001, 0).UTC(), ObservedRelease: "v1"}
	out, err := e.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if out.Operation.Status != "succeeded" || d.applies != 1 {
		t.Fatalf("reconcile: %+v applies=%d", out, d.applies)
	}
}

func TestCleanupKeepsHistoryAndMarksDeleted(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	if _, err := e.Apply(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ActionKey = "run:step:cleanup"
	cfg.OperationID = "cleanup-1"
	out, err := e.Cleanup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Resource.DeletedAt == nil || out.Resource.LastOperation != "cleanup-1" {
		t.Fatalf("cleanup state: %+v", out.Resource)
	}
}

func TestRollbackIsASeparateOperation(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	if _, err := e.Apply(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ActionKey = "run:step"
	out, err := e.Rollback(context.Background(), cfg, "dummy-v0")
	if err != nil {
		t.Fatal(err)
	}
	if out.Operation.Kind != "rollback" || out.Operation.Status != "succeeded" {
		t.Fatalf("rollback: %+v", out)
	}
}

func TestCreateRejectsUnconditionalWrite(t *testing.T) {
	mem := state.NewMemory(nil)
	ctx := context.Background()
	scope := state.Scope{TenantID: domain.TenantID("t"), ProjectID: domain.ProjectID("p")}
	if _, err := mem.Create(ctx, scope, "n", "k", "a", statev1.WriteRequest{Value: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Create(ctx, scope, "n", "k", "b", statev1.WriteRequest{Value: json.RawMessage(`{"x":2}`)}); !errors.Is(err, state.ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
}

type failingCheckpointState struct {
	State
}

func (s failingCheckpointState) Update(ctx context.Context, key, etag string, req statev1.WriteRequest) (statev1.Entry, string, error) {
	if bytes.Contains(req.Value, []byte("delete-intent")) {
		return statev1.Entry{}, "", errors.New("checkpoint unavailable")
	}
	return s.State.Update(ctx, key, etag, req)
}

func TestCleanupDoesNotDestroyWhenCheckpointFails(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	if _, err := e.Apply(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	e.State = failingCheckpointState{State: e.State}
	cfg := testConfig()
	cfg.ActionKey = "run:step:cleanup"
	cfg.OperationID = "cleanup-fail"
	out, err := e.Cleanup(context.Background(), cfg)
	if err == nil || !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("cleanup error = %v, want checkpoint failure", err)
	}
	if d.destroys != 0 {
		t.Fatalf("provider destroy ran %d times", d.destroys)
	}
	if out.Operation.Status != "failed" || out.Resource.DeletedAt != nil {
		t.Fatalf("outcome = %+v", out)
	}
}

type failReleaseState struct {
	State
	fail bool
}

func (s *failReleaseState) Create(ctx context.Context, key string, req statev1.WriteRequest) (statev1.Entry, string, error) {
	if s.fail && strings.HasPrefix(key, "releases/") {
		return statev1.Entry{}, "", errors.New("release history unavailable")
	}
	return s.State.Create(ctx, key, req)
}

func TestReconcilePublishesPendingReleaseAndKeepsPinnedArtifacts(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed", ProviderRelease: "dummy"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	cfg := testConfig()
	cfg.Artifacts = []driver.Artifact{{
		ID: "art_bundle", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Kind: driver.ArtifactFile, Path: "/tmp/bundle",
	}}
	failing := &failReleaseState{State: e.State, fail: true}
	e.State = failing
	if _, err := e.Apply(context.Background(), cfg); err == nil {
		t.Fatal("expected release publication failure")
	}
	resourceKey, _ := keyFor("resources", "fake/dummy")
	entry, _, err := e.State.Read(context.Background(), resourceKey)
	if err != nil {
		t.Fatal(err)
	}
	var resource resourceDoc
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		t.Fatal(err)
	}
	if resource.PendingRelease == nil || resource.ActiveOperation == "" {
		t.Fatalf("pending publication not retained: %+v", resource)
	}

	failing.fail = false
	out, err := e.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if out.Operation.Status != "succeeded" || len(out.Resource.ArtifactReferences) != 1 ||
		out.Resource.ArtifactReferences[0].ArtifactID != "art_bundle" || out.Resource.PendingRelease != nil {
		t.Fatalf("reconciled resource = %+v", out.Resource)
	}
	releaseKey, _ := keyFor("releases", "fake/dummy/v1")
	releaseEntry, _, err := e.State.Read(context.Background(), releaseKey)
	if err != nil {
		t.Fatal(err)
	}
	var release releaseDoc
	if err := json.Unmarshal(releaseEntry.Value, &release); err != nil {
		t.Fatal(err)
	}
	if len(release.Artifacts) != 1 || release.Artifacts[0].ID != "art_bundle" {
		t.Fatalf("release artifacts = %+v", release.Artifacts)
	}
}

func TestReconcileCleanupAfterApplyingUsesCleanupPath(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	if _, err := e.Apply(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ActionKey = "run:step:cleanup"
	cfg.OperationID = "cleanup-recover"
	if _, err := e.Cleanup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the provider deletion but before the cleanup
	// operation and resource were finalized.
	resourceKey, _ := keyFor("resources", "fake/dummy")
	entry, etag, err := e.State.Read(context.Background(), resourceKey)
	if err != nil {
		t.Fatal(err)
	}
	var resource resourceDoc
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		t.Fatal(err)
	}
	resource.DeletedAt = nil
	resource.ActiveOperation = "cleanup-recover"
	raw, _ := encode(resource)
	if _, _, err := e.State.Update(context.Background(), resourceKey, etag, statev1.WriteRequest{Value: raw}); err != nil {
		t.Fatal(err)
	}
	opKey, _ := keyFor("operations", "fake/dummy/cleanup-recover")
	op, err := e.readOperation(context.Background(), opKey)
	if err != nil {
		t.Fatal(err)
	}
	op.Status = "applying"
	if err := e.saveOperation(context.Background(), opKey, op); err != nil {
		t.Fatal(err)
	}

	out, err := e.Reconcile(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Resource.DeletedAt == nil || out.Operation.Status != "succeeded" {
		t.Fatalf("cleanup reconcile = %+v", out)
	}
}

func TestReleaseIDIsStableAndIgnoresLocalPaths(t *testing.T) {
	identity := driver.Identity{ResourceKey: "kubernetes/cluster/ns/app"}
	artifacts := []driver.Artifact{{ID: "bundle", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: driver.ArtifactFile, Path: "/tmp/one"}}
	first, err := ReleaseID(identity, artifacts, json.RawMessage(`{"release":"app"}`))
	if err != nil {
		t.Fatal(err)
	}
	artifacts[0].Path = "/var/other"
	second, err := ReleaseID(identity, artifacts, json.RawMessage(`{"release":"app"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("path changed release id: %s != %s", first, second)
	}
	artifacts[0].Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	third, _ := ReleaseID(identity, artifacts, json.RawMessage(`{"release":"app"}`))
	if third == first {
		t.Fatal("artifact change did not change release id")
	}
}

func TestPruneKeepsProtectedReleases(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	base := time.Unix(2000, 0).UTC()
	for i, release := range []string{"v1", "v2", "v3"} {
		e.Clock = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		cfg := testConfig()
		cfg.ActionKey = "run:" + release
		cfg.DesiredRelease = release
		if _, err := e.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := e.Prune(context.Background(), testConfig(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) == 0 {
		t.Fatalf("prune removed nothing: %+v", removed)
	}
	// The desired and last healthy release survive regardless of ordering.
	resourceKey, _ := keyFor("resources", "fake/dummy")
	entry, _, err := e.State.Read(context.Background(), resourceKey)
	if err != nil {
		t.Fatal(err)
	}
	var resource resourceDoc
	_ = json.Unmarshal(entry.Value, &resource)
	if resource.DesiredRelease != "v3" || resource.LastHealthyRelease != "v3" {
		t.Fatalf("resource = %+v", resource)
	}
}

func TestImportLegacyCreatesStateWithoutProviderMutation(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	cfg := testConfig()
	cfg.DesiredRelease = ""
	doc := LegacyImport{
		ResourceKey: "fake/dummy", Release: "legacy-v1", ObservedRelease: "legacy-v1",
		ProviderRelease: "legacy", ProviderVersion: "1",
		Resources:     []driver.Resource{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "deploy", Name: "dummy", UID: "uid-1"}},
		Configuration: json.RawMessage(`{"namespace":"deploy","release":"dummy"}`),
	}
	out, err := e.ImportLegacy(context.Background(), cfg, doc)
	if err != nil {
		t.Fatal(err)
	}
	if d.applies != 0 || out.Resource.DesiredRelease != "legacy-v1" || len(out.Resource.Resources) != 1 {
		t.Fatalf("import = %+v applies=%d", out, d.applies)
	}
	if _, err := e.ImportLegacy(context.Background(), cfg, doc); !errors.Is(err, ErrConflict) {
		t.Fatalf("second import = %v, want conflict", err)
	}
}

func TestCleanupExpiredDeletesOnlyExpiredResources(t *testing.T) {
	d := &fakeDriver{applyRes: driver.Result{Effect: "confirmed"}}
	e, closeFn := newEngine(t, d)
	defer closeFn()
	clock := time.Unix(3000, 0).UTC()
	e.Clock = func() time.Time { return clock }
	cfg := testConfig()
	cfg.ExpiresIn = time.Minute
	cfg.ActionKey = "run:expire"
	if _, err := e.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	out, err := e.CleanupExpired(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Resource.DeletedAt == nil {
		t.Fatalf("expired cleanup = %+v", out)
	}
}
