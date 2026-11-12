package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fakeClock is the deterministic-ish test clock. Sleep honors cancellation.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func (clock *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fakeRuntime implements the Kernel runtime and workspace ports without any
// host effect. Every started workload exits successfully.
type fakeRuntime struct {
	mu       sync.Mutex
	started  []id.WorkloadID
	stopped  []id.WorkloadID
	removed  []id.WorkloadID
	prepared int
}

var (
	_ kernelengine.Runtime   = (*fakeRuntime)(nil)
	_ kernelengine.Workspace = (*fakeRuntime)(nil)
)

func (runtime *fakeRuntime) Capabilities(context.Context) (kernelengine.Capabilities, error) {
	return kernelengine.Capabilities{Names: []string{"process"}}, nil
}

func (runtime *fakeRuntime) Prepare(_ context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	runtime.mu.Lock()
	runtime.prepared++
	runtime.mu.Unlock()
	return kernelengine.Preparation{WorkspaceID: request.WorkspaceID, Detail: "prepared"}, nil
}

func (runtime *fakeRuntime) Start(_ context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	runtime.mu.Lock()
	runtime.started = append(runtime.started, request.Identity.WorkloadID)
	runtime.mu.Unlock()
	return kernelengine.StartResult{Outcome: kernelengine.StartAccepted, WorkloadID: request.Identity.WorkloadID, Detail: "started"}, nil
}

func (runtime *fakeRuntime) Observe(context.Context, kernelengine.ObserveRequest) (kernel.Observation, error) {
	return kernel.ExitedObservation(0, kernel.ConfirmedStop("process exited 0")), nil
}

func (runtime *fakeRuntime) Logs(context.Context, kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return kernelengine.LogBatch{}, nil
}

func (runtime *fakeRuntime) Stop(_ context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	runtime.mu.Lock()
	runtime.stopped = append(runtime.stopped, request.Identity.WorkloadID)
	runtime.mu.Unlock()
	return kernel.ConfirmedStop("stopped"), nil
}

func (runtime *fakeRuntime) Remove(_ context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	runtime.mu.Lock()
	runtime.removed = append(runtime.removed, request.Identity.WorkloadID)
	runtime.mu.Unlock()
	return kernelengine.Removal{Removed: true}, nil
}

func (runtime *fakeRuntime) RefreshCredential(context.Context, kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

func (runtime *fakeRuntime) Status(context.Context, id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	return kernelengine.WorkspaceObservation{Available: true}, nil
}

func (runtime *fakeRuntime) Retain(context.Context, id.WorkspaceID) error  { return nil }
func (runtime *fakeRuntime) Release(context.Context, id.WorkspaceID) error { return nil }

func (runtime *fakeRuntime) startCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.started)
}

func (runtime *fakeRuntime) stopCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.stopped)
}

func (runtime *fakeRuntime) removeCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.removed)
}

// testEnvironment is the shared deterministic development composition input.
type testEnvironment struct {
	clock   *fakeClock
	runtime *fakeRuntime
	logs    *bytes.Buffer
	config  Config
}

func newTestEnvironment(t *testing.T) *testEnvironment {
	t.Helper()
	clock := newFakeClock()
	runtime := &fakeRuntime{}
	logs := &bytes.Buffer{}
	config := Config{
		Profile: ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   "http://127.0.0.1:8080",
		},
		InstallationID:       "installation-1",
		DataDir:              t.TempDir(),
		DefaultActionTimeout: time.Minute,
		LoopInterval:         2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
		Dev: &DevelopmentConfig{
			Tokens: []DevToken{
				{
					Credential: "dev-admin-token",
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: "tenant-1"},
						Roles:   []tenancy.Role{tenancy.RoleAdmin},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: "dev-project-token",
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"},
						Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
			},
			Bootstrap: &Bootstrap{
				TenantID:       "tenant-1",
				TenantName:     "Tenant One",
				ProjectID:      "project-1",
				ProjectName:    "Project One",
				AdminPrincipal: "admin-1",
				AdminRoles:     []string{string(tenancy.RoleAdmin)},
			},
		},
		Adapters: &Adapters{
			Runtime: runtime,
			Clock:   clock,
		},
	}
	return &testEnvironment{clock: clock, runtime: runtime, logs: logs, config: config}
}

// fullTestProfileConfig builds a fully injected ProfileTest composition over
// the deterministic development environment. The production access token
// verifier is replaced by the static development authenticator; every adapter
// the test profile requires is explicit.
func fullTestProfileConfig(t *testing.T) Config {
	t.Helper()
	base := newTestEnvironment(t)
	clock := base.clock
	runtime := base.runtime
	store := memory.NewWithClock(clock.Now)
	broker, err := newInProcessBroker(clock)
	if err != nil {
		t.Fatalf("newInProcessBroker: %v", err)
	}
	provider, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewEphemeral: %v", err)
	}
	validity, err := readers.NewCredentialValidity(store)
	if err != nil {
		t.Fatalf("readers.NewCredentialValidity: %v", err)
	}
	sink, err := shelllog.NewFileSink(shelllog.SinkConfig{Root: t.TempDir(), Clock: clock})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	authenticator, err := NewDevAuthenticator(clock, base.config.Dev.Tokens)
	if err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	config := base.config
	config.Profile = ProfileTest
	config.Adapters = &Adapters{
		Store:              store,
		Scanner:            store,
		Controller:         store.Controller(),
		Outbox:             store,
		Credentials:        store,
		Runtime:            runtime,
		Workspace:          runtime,
		Keys:               provider,
		Publisher:          broker,
		Invalidator:        broker,
		Clock:              clock,
		IDs:                bridges.NewSecureIDs(),
		Principal:          authenticator,
		CredentialValidity: validity,
		Registry:           devRegistry{},
		LogSink:            sink,
	}
	return config
}

// startApp constructs and starts the app, registering cleanup.
func (environment *testEnvironment) startApp(t *testing.T) *App {
	t.Helper()
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return app
}

// closeApp closes one app with a bounded shutdown context and fails the test
// on error.
func closeApp(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// newTestCipher builds the real envelope cipher over ephemeral keys.
func newTestCipher(t *testing.T) secret.EnvelopeCipher {
	t.Helper()
	provider, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewEphemeral: %v", err)
	}
	cipher, err := keys.NewCipher(provider, rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewCipher: %v", err)
	}
	return cipher
}

// inspectExecution reads one execution through the REST edge.
func inspectExecution(t *testing.T, handler http.Handler, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodGet,
		"/api/v1/executions/"+executionID, token, nil, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("execution.inspect status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.ExecutionInspectResult](t, result)
}

// applyProjectPipeline applies one pipeline named build in a project through
// the REST edge.
func applyProjectPipeline(t *testing.T, handler http.Handler, token, projectID string, pipeline []byte, key string) api.PipelineApplyResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/"+projectID+"/pipelines/build", token,
		api.PipelineApplyRequest{Name: "build", YAML: pipeline}, nil, key)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.PipelineApplyResult](t, result)
}

// restCall performs one REST envelope call.
func restCall(t *testing.T, handler http.Handler, method, path, token string, input any, expectedVersion *uint64, idempotencyKey string) (*httptest.ResponseRecorder, api.Result, api.Error) {
	t.Helper()
	request := api.Request{ExpectedVersion: expectedVersion, IdempotencyKey: idempotencyKey, Input: json.RawMessage(`{}`)}
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		request.Input = encoded
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	httpRequest := httptest.NewRequest(method, path, bytes.NewReader(body))
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httpRequest)

	var result api.Result
	var publicError api.Error
	if recorder.Code == api.HTTPStatusSuccess {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode result: %v; body=%s", err, recorder.Body.String())
		}
	} else {
		if err := json.Unmarshal(recorder.Body.Bytes(), &publicError); err != nil {
			t.Fatalf("decode error: %v; body=%s", err, recorder.Body.String())
		}
	}
	return recorder, result, publicError
}

func decodeBody[T any](t *testing.T, result api.Result) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.Body, &value); err != nil {
		t.Fatalf("decode result body: %v; body=%s", err, string(result.Body))
	}
	return value
}

func grantProjectEngineer(t *testing.T, handler http.Handler, tenantToken string) {
	t.Helper()
	recorder, _, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/memberships/admin-1", tenantToken,
		api.MembershipGrantRequest{Roles: []string{"admin", "engineer"}}, nil, "membership-bootstrap")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("membership.grant status=%d error=%+v", recorder.Code, publicError)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition was not satisfied within %s", timeout)
}
