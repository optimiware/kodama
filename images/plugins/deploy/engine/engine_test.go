package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func newEngine(t *testing.T, d driver.Driver) (*Engine, func()) {
	t.Helper()
	mem := state.NewMemory(nil)
	h, err := state.NewHandler(mem, resolver{write: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	client, err := pluginstate.New(srv.URL, "att_engine", "release", "")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return &Engine{State: client, Driver: d, Clock: func() time.Time { return time.Unix(1000, 0).UTC() }}, srv.Close
}

type fakeDriver struct {
	applies  int
	observes int
	obs      driver.Observation
	applyErr error
	applyRes driver.Result
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
	if f.obs.State == "" {
		f.obs = driver.Observation{State: driver.Ready, ObservedAt: time.Unix(1000, 0).UTC(), ObservedRelease: "v1"}
	}
	return f.obs, nil
}
func (f *fakeDriver) Destroy(context.Context, driver.Request, driver.Checkpoint) (driver.Result, error) {
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
