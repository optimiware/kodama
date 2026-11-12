package app

// Shared deterministic fixtures for the activation-materialization and
// workload-bearer tests. No test here starts a process, container, database
// or broker: every runtime, clock, store and key provider is a fake or an
// in-memory implementation owned by the test.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	// activationSecretName is declared by the activation pipelines and written
	// through the public secret.write operation.
	activationSecretName id.SecretName = "DEPLOY_TOKEN"
	// activationOtherName is a second valid name used to prove an allow-list
	// for one name never widens to another.
	activationOtherName id.SecretName = "OTHER_TOKEN"
	// activationSecretValue is distinctive so any accidental log, event,
	// receipt or error rendering is detectable.
	activationSecretValue = "activation-secret-value-9c1f"
)

// activationRules builds trusted workload-consumer allow rules. A project rule
// authorizes exactly one declared name in project-1; project=false builds a
// tenant-wide rule instead.
func activationRules(project bool, names ...id.SecretName) []secret.NameRule {
	scope := tenancy.Scope{TenantID: "tenant-1"}
	if project {
		scope.ProjectID = "project-1"
	}
	return []secret.NameRule{{Scope: scope, Names: append([]id.SecretName(nil), names...)}}
}

// activationAction describes one action of a generated pipeline.
type activationAction struct {
	name    string
	secrets []id.SecretName
	outputs []id.OutputKey
}

// activationPipeline renders the admitted YAML for a deterministic pipeline.
// Every action pins the same digest-resolved image the development registry
// accepts.
func activationPipeline(actions ...activationAction) []byte {
	var builder strings.Builder
	builder.WriteString("actions:\n")
	for _, action := range actions {
		builder.WriteString("  - name: " + action.name + "\n")
		builder.WriteString("    image: " + smokeImage + "\n")
		builder.WriteString("    commands:\n")
		builder.WriteString("      - echo " + action.name + "\n")
		if len(action.secrets) > 0 {
			names := make([]string, len(action.secrets))
			for index, name := range action.secrets {
				names[index] = string(name)
			}
			builder.WriteString("    secrets: [" + strings.Join(names, ", ") + "]\n")
		}
		if len(action.outputs) > 0 {
			keys := make([]string, len(action.outputs))
			for index, key := range action.outputs {
				keys[index] = string(key)
			}
			builder.WriteString("    outputs: [" + strings.Join(keys, ", ") + "]\n")
		}
	}
	return []byte(builder.String())
}

// recordingRuntime is the fake Kernel runtime/workspace adapter. It records the
// exact PrepareRequest the composition hands the adapter, counts post-commit
// RefreshCredential calls and lets a test decide which named action exits.
//
// Observation is deterministic: an action whose name is in exitNames reports a
// confirmed exit once releases are enabled; every other action stays running so
// its execution credential remains current. Exits are paused by pauseExits so a
// restart test can close the first composition before the first action settles.
type recordingRuntime struct {
	*fakeRuntime

	mu            sync.Mutex
	prepares      []kernelengine.PrepareRequest
	delegated     int
	refreshes     int
	exitNames     map[id.ActionName]bool
	exitActions   map[id.ActionID]bool
	prepareErrors map[id.ActionName]error
	prepareGate   chan struct{}
	prepareReach  chan struct{}
	released      bool
}

var (
	_ kernelengine.Runtime   = (*recordingRuntime)(nil)
	_ kernelengine.Workspace = (*recordingRuntime)(nil)
)

func newRecordingRuntime(exitNames ...id.ActionName) *recordingRuntime {
	runtime := &recordingRuntime{
		fakeRuntime:   &fakeRuntime{},
		exitNames:     make(map[id.ActionName]bool, len(exitNames)),
		exitActions:   make(map[id.ActionID]bool),
		prepareErrors: make(map[id.ActionName]error),
		released:      true,
	}
	for _, name := range exitNames {
		runtime.exitNames[name] = true
	}
	return runtime
}

func (runtime *recordingRuntime) Prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	runtime.mu.Lock()
	runtime.prepares = append(runtime.prepares, request)
	runtime.delegated++
	runtime.exitActions[request.Identity.ActionID] = runtime.exitNames[request.ActionName]
	prepareErr := runtime.prepareErrors[request.ActionName]
	gate := runtime.prepareGate
	reach := runtime.prepareReach
	runtime.mu.Unlock()
	if reach != nil {
		select {
		case reach <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if prepareErr != nil {
		return kernelengine.Preparation{}, prepareErr
	}
	return runtime.fakeRuntime.Prepare(ctx, request)
}

// failPrepare makes the adapter refuse the preparation of one named action after
// the composition has already materialized its secrets and credential.
func (runtime *recordingRuntime) failPrepare(name id.ActionName, err error) {
	runtime.mu.Lock()
	runtime.prepareErrors[name] = err
	runtime.mu.Unlock()
}

// holdPrepare blocks the adapter's next preparation after it has recorded the
// materialized request (including the committed credential). The returned
// channel receives once that preparation has been reached; release unblocks it.
func (runtime *recordingRuntime) holdPrepare() (reached <-chan struct{}, release func()) {
	gate := make(chan struct{})
	reach := make(chan struct{}, 1)
	runtime.mu.Lock()
	runtime.prepareGate = gate
	runtime.prepareReach = reach
	runtime.mu.Unlock()
	var once sync.Once
	return reach, func() { once.Do(func() { close(gate) }) }
}

func (runtime *recordingRuntime) Observe(_ context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	runtime.mu.Lock()
	exits := runtime.released && runtime.exitActions[request.Identity.ActionID]
	runtime.mu.Unlock()
	if exits {
		return kernel.ExitedObservation(0, kernel.ConfirmedStop("workload exited")), nil
	}
	return kernel.RunningObservation(), nil
}

func (runtime *recordingRuntime) RefreshCredential(_ context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	runtime.mu.Lock()
	runtime.refreshes++
	runtime.mu.Unlock()
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

// pauseExits keeps every action running until releaseExits is called.
func (runtime *recordingRuntime) pauseExits() {
	runtime.mu.Lock()
	runtime.released = false
	runtime.mu.Unlock()
}

// releaseExits lets the configured named actions report a confirmed exit.
func (runtime *recordingRuntime) releaseExits() {
	runtime.mu.Lock()
	runtime.released = true
	runtime.mu.Unlock()
}

func (runtime *recordingRuntime) prepareSnapshot() []kernelengine.PrepareRequest {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]kernelengine.PrepareRequest(nil), runtime.prepares...)
}

func (runtime *recordingRuntime) prepareCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.prepares)
}

func (runtime *recordingRuntime) delegatedCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.delegated
}

func (runtime *recordingRuntime) refreshCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.refreshes
}

// activationFixture is one development composition over the recording runtime.
type activationFixture struct {
	environment *testEnvironment
	runtime     *recordingRuntime
}

func newActivationFixture(t *testing.T, rules []secret.NameRule, exitNames ...id.ActionName) *activationFixture {
	t.Helper()
	base := newTestEnvironment(t)
	runtime := newRecordingRuntime(exitNames...)
	base.config.Adapters.Runtime = runtime
	base.config.Adapters.Workspace = runtime
	base.config.SecretRules = rules
	// A 24h action deadline keeps the action alive across the token-expiry
	// scenario while the credential itself stays capped by the 8h TTL.
	base.config.DefaultActionTimeout = 24 * time.Hour
	return &activationFixture{environment: base, runtime: runtime}
}

// applyActivationPipeline applies one generated pipeline in project-1 through
// the REST edge.
func applyActivationPipeline(t *testing.T, handler http.Handler, token string, pipeline []byte, key string) api.PipelineApplyResult {
	t.Helper()
	return applyProjectPipeline(t, handler, token, "project-1", pipeline, key)
}

// startActivationTask creates and starts one task for the applied pipeline.
func startActivationTask(t *testing.T, handler http.Handler, token, pipelineID, key string) api.TaskCreateAndStartResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", token,
		api.TaskCreateAndStartRequest{Title: "activation", PipelineID: pipelineID}, nil, key)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskCreateAndStartResult](t, result)
}

// writeActivationSecret stores one project secret through the public edge.
func writeActivationSecret(t *testing.T, handler http.Handler, token string, name id.SecretName, value, key string) {
	t.Helper()
	recorder, _, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/secrets/"+string(name), token,
		api.SecretWriteRequest{Value: value}, nil, key)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("secret.write status=%d error=%+v", recorder.Code, publicError)
	}
}

// rawExecutionInspect performs one inspect and returns the recorder untouched so
// a refusal body can be inspected.
func rawExecutionInspect(t *testing.T, handler http.Handler, token, executionID string) (*httptest.ResponseRecorder, api.Result, api.Error) {
	t.Helper()
	return restCall(t, handler, http.MethodGet, "/api/v1/executions/"+executionID, token, nil, nil, "")
}

// waitForActivationPrepares blocks until the runtime adapter has seen count
// Prepare calls and returns the recorded requests.
func waitForActivationPrepares(t *testing.T, runtime *recordingRuntime, count int) []kernelengine.PrepareRequest {
	t.Helper()
	eventually(t, 5*time.Second, func() bool { return runtime.prepareCount() >= count })
	prepares := runtime.prepareSnapshot()
	if len(prepares) < count {
		t.Fatalf("recorded %d preparations; want at least %d", len(prepares), count)
	}
	return prepares
}

// waitForExecutionState blocks until the execution reaches the wanted state.
func waitForExecutionState(t *testing.T, handler http.Handler, token, executionID, state string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	eventually(t, 5*time.Second, func() bool {
		recorder, result, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+executionID, token, nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			return false
		}
		var inspected api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &inspected); err != nil {
			return false
		}
		last = inspected
		return inspected.State == state
	})
	return last
}

// waitForStableExecutionVersion waits until two consecutive reads agree on the
// execution version. The Kernel commits one running observation shortly after a
// start; after that the version is stable for a still-running workload, so an
// output submission can carry an expected version without racing the loop.
func waitForStableExecutionVersion(t *testing.T, handler http.Handler, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var (
		last     api.ExecutionInspectResult
		previous uint64
	)
	eventually(t, 5*time.Second, func() bool {
		recorder, result, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+executionID, token, nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			return false
		}
		var inspected api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &inspected); err != nil {
			return false
		}
		stable := previous != 0 && inspected.Version == previous
		previous = inspected.Version
		last = inspected
		return stable
	})
	return last
}

// startComposedApp constructs and starts one composition without registering a
// cleanup, so a restart test can close it deterministically.
func startComposedApp(t *testing.T, config Config) *App {
	t.Helper()
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return app
}

// composedShared is the durable, cross-composition state a restart test
// shares: the same store, key provider, clock and broker. Only the composition
// (and therefore its token manager) is rebuilt.
type composedShared struct {
	clock    *fakeClock
	store    *memory.Store
	broker   *inProcessBroker
	provider keys.Provider
	ids      *bridges.IDs
	validity event.CredentialValidity
	dataDir  string
	logs     *bytes.Buffer
	dev      *DevelopmentConfig
}

func newComposedShared(t *testing.T) composedShared {
	t.Helper()
	base := newTestEnvironment(t)
	clock := base.clock
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
	return composedShared{
		clock:    clock,
		store:    store,
		broker:   broker,
		provider: provider,
		ids:      bridges.NewSecureIDs(),
		validity: validity,
		dataDir:  t.TempDir(),
		logs:     base.logs,
		dev:      base.config.Dev,
	}
}

// developmentConfig builds a development composition over the shared durable
// state. The one runtime adapter serves as both runtime and workspace; the
// caller sets the action timeout and whatever else its scenario needs.
func (shared composedShared) developmentConfig(t *testing.T, runtime interface {
	kernelengine.Runtime
	kernelengine.Workspace
}) Config {
	t.Helper()
	sink, err := shelllog.NewFileSink(shelllog.SinkConfig{Root: t.TempDir(), Clock: shared.clock})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	return Config{
		Profile: ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   "http://127.0.0.1:8080",
		},
		InstallationID: "installation-1",
		DataDir:        shared.dataDir,
		LoopInterval:   2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(shared.logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
		Dev: shared.dev,
		Adapters: &Adapters{
			Store:              shared.store,
			Scanner:            shared.store,
			Controller:         shared.store.Controller(),
			Outbox:             shared.store,
			Credentials:        shared.store,
			Runtime:            runtime,
			Workspace:          runtime,
			Keys:               shared.provider,
			Publisher:          shared.broker,
			Invalidator:        shared.broker,
			Clock:              shared.clock,
			IDs:                shared.ids,
			CredentialValidity: shared.validity,
			Registry:           devRegistry{},
			LogSink:            sink,
		},
	}
}

// activationShared is the shared state of an activation restart test plus the
// recording runtime adapter that survives the restart.
type activationShared struct {
	composedShared
	runtime *recordingRuntime
}

func newActivationShared(t *testing.T, exitNames ...id.ActionName) *activationShared {
	t.Helper()
	return &activationShared{
		composedShared: newComposedShared(t),
		runtime:        newRecordingRuntime(exitNames...),
	}
}

// config builds a development composition over the shared durable state.
func (shared *activationShared) config(t *testing.T, rules []secret.NameRule) Config {
	t.Helper()
	config := shared.developmentConfig(t, shared.runtime)
	config.DefaultActionTimeout = 24 * time.Hour
	config.SecretRules = rules
	return config
}

// projectPrincipal is the development token principal with the project roles a
// pipeline and task operation requires. It is reused by fixtures that need a
// static development credential for a project-scoped operation.
func projectPrincipal(clock *fakeClock) DevToken {
	return DevToken{
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
	}
}

// containsMaterial reports whether any rendered value contains one of the
// secret or token strings. Every captured artifact is checked with it.
func containsMaterial(rendered string, materials ...string) bool {
	for _, material := range materials {
		if material != "" && strings.Contains(rendered, material) {
			return true
		}
	}
	return false
}
