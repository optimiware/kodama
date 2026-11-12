//go:build integration

package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Fixture identities and credentials. The token values are deliberately
// distinctive so the secret scan can prove none of them is ever rendered.
const (
	tenantID  = "tenant-1"
	projectID = "project-1"
	otherID   = "project-2"

	adminToken    = "fixture-admin-token-4a1f9c"
	projectToken  = "fixture-project-token-7d2b3e"
	otherToken    = "fixture-other-project-token-9c4e5a"
	foreignToken  = "fixture-foreign-tenant-token-2b6d8f"
	expiredToken  = "fixture-expired-token-5e0a7c"
	serviceToken  = "fixture-service-token-1a2b3c"
	operatorToken = "fixture-operator-token-3c5d7e"
	// viewerToken is a project-scoped human holding only the viewer role.
	viewerToken = "fixture-viewer-token-6f7a8b"
	// tenantOperatorToken is the tenant-wide credential of the same human as
	// adminToken but holding only the operator role. It makes the admin-only
	// tenant boundary a role refusal rather than a scope refusal.
	tenantOperatorToken = "fixture-tenant-operator-token-2d4e6f"
	forgedToken         = "fixture-forged-token-8f1c2d"
	secretValue         = "fixture-secret-value-3a9d1e"
	adminPassword       = "fixture-actor-credential-6b4c2a"
)

// fakeClock is a deterministic authority clock whose Sleep still observes
// cancellation. Background loops sleep for real time so tests do not need to
// drive a scheduler; only the authority time is frozen.
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

// runtimeMode selects the fake runtime's observable behavior.
type runtimeMode int

const (
	// modeComplete reports a successful exit on the first observation.
	modeComplete runtimeMode = iota
	// modeStayRunning reports a running workload until Stop is called.
	modeStayRunning
	// modeUnknownLaunch reports an ambiguous start and never re-issues it.
	modeUnknownLaunch
)

// fakeRuntime is the injected Kernel runtime and workspace. It records every
// effect so a test can prove what the composition actually did.
type fakeRuntime struct {
	mode runtimeMode

	mu       sync.Mutex
	prepared int
	started  []id.WorkloadID
	stopped  []id.WorkloadID
	removed  []id.WorkloadID
	observed int
	running  map[id.WorkloadID]bool
}

var (
	_ kernelengine.Runtime   = (*fakeRuntime)(nil)
	_ kernelengine.Workspace = (*fakeRuntime)(nil)
)

func newFakeRuntime(mode runtimeMode) *fakeRuntime {
	return &fakeRuntime{mode: mode, running: make(map[id.WorkloadID]bool)}
}

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
	defer runtime.mu.Unlock()
	runtime.started = append(runtime.started, request.Identity.WorkloadID)
	runtime.running[request.Identity.WorkloadID] = true
	switch runtime.mode {
	case modeUnknownLaunch:
		return kernelengine.StartResult{Outcome: kernelengine.StartUnknown, WorkloadID: request.Identity.WorkloadID, Detail: "start outcome unknown"}, nil
	default:
		return kernelengine.StartResult{Outcome: kernelengine.StartAccepted, WorkloadID: request.Identity.WorkloadID, Detail: "started"}, nil
	}
}

func (runtime *fakeRuntime) Observe(_ context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.observed++
	if runtime.mode == modeComplete {
		return kernel.ExitedObservation(0, kernel.ConfirmedStop("process exited 0")), nil
	}
	if runtime.running[request.Identity.WorkloadID] {
		return kernel.RunningObservation(), nil
	}
	return kernel.ExitedObservation(0, kernel.ConfirmedStop("process exited after stop")), nil
}

func (runtime *fakeRuntime) Logs(context.Context, kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return kernelengine.LogBatch{}, nil
}

func (runtime *fakeRuntime) Stop(_ context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.stopped = append(runtime.stopped, request.Identity.WorkloadID)
	runtime.running[request.Identity.WorkloadID] = false
	return kernel.ConfirmedStop("stopped"), nil
}

func (runtime *fakeRuntime) Remove(_ context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.removed = append(runtime.removed, request.Identity.WorkloadID)
	delete(runtime.running, request.Identity.WorkloadID)
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

func (runtime *fakeRuntime) stoppedIDs() []id.WorkloadID {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]id.WorkloadID(nil), runtime.stopped...)
}

func (runtime *fakeRuntime) startedIDs() []id.WorkloadID {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]id.WorkloadID(nil), runtime.started...)
}

// release marks one controlled workload as exited successfully on the next
// observation. It is the deterministic barrier for a runtime that stays running
// until the test lets it finish.
func (runtime *fakeRuntime) release(workload id.WorkloadID) {
	runtime.mu.Lock()
	runtime.running[workload] = false
	runtime.mu.Unlock()
}

// fakePublisher is the injected event broker and token invalidation port. It
// can be switched unavailable to prove broker-independent progression and a
// durable outbox drain with the original event identities.
type fakePublisher struct {
	mu            sync.Mutex
	down          bool
	published     []event.Envelope
	invalidations []token.Invalidation
}

var (
	_ event.Publisher        = (*fakePublisher)(nil)
	_ token.InvalidationPort = (*fakePublisher)(nil)
)

func newFakePublisher() *fakePublisher { return &fakePublisher{} }

func (publisher *fakePublisher) Publish(ctx context.Context, envelope event.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.down {
		return fault.New(fault.DependencyUnavailable, "broker is unavailable")
	}
	publisher.published = append(publisher.published, envelope)
	return nil
}

func (publisher *fakePublisher) Invalidate(ctx context.Context, invalidation token.Invalidation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.down {
		return fault.New(fault.DependencyUnavailable, "broker is unavailable")
	}
	publisher.invalidations = append(publisher.invalidations, invalidation)
	return nil
}

func (publisher *fakePublisher) Close(context.Context) error { return nil }

func (publisher *fakePublisher) setDown(down bool) {
	publisher.mu.Lock()
	publisher.down = down
	publisher.mu.Unlock()
}

func (publisher *fakePublisher) publishedIDs() []id.EventID {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	identities := make([]id.EventID, 0, len(publisher.published))
	for _, envelope := range publisher.published {
		identities = append(identities, envelope.ID)
	}
	return identities
}

// safeBuffer is a concurrency-safe log sink for the background loops.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *safeBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *safeBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// settlementGate holds one captured settlement task page until the test
// releases it. It is the deterministic substitute for waiting on elapsed time:
// the settlement loop cannot observe newer state while the stale page is held.
type settlementGate struct {
	page     store.QueryPage
	captured chan struct{}
	release  chan struct{}
	once     sync.Once
}

// releaseGate lets the gated settlement page continue exactly once.
func (gate *settlementGate) releaseGate() {
	gate.once.Do(func() { close(gate.release) })
}

// taskCurrentExecution reports the current execution recorded in the captured
// page for one task. The value is read after the captured channel is closed, so
// it is the exact page snapshot the settlement loop later observes.
func (gate *settlementGate) taskCurrentExecution(taskID id.TaskID) string {
	for _, row := range gate.page.Rows {
		if row.Table != "tasks" {
			continue
		}
		if value, ok := row.Values["id"].(string); !ok || value != string(taskID) {
			continue
		}
		value, _ := row.Values["current_execution_id"].(string)
		return value
	}
	return ""
}

// settlementProbe observes and gates the composed settlement loop through the
// store seams it consumes. The succeeded-execution discovery scan and the
// open-task page query are specific to settlement, so decorating those two
// seams counts real settlement passes and can hold one of them without reaching
// into product code.
type settlementProbe struct {
	scanner    store.Scanner
	controller store.Controller

	mu      sync.Mutex
	watched id.ExecutionID
	taskID  id.TaskID
	passes  int
	gate    *settlementGate
}

var (
	_ store.Scanner    = (*settlementProbe)(nil)
	_ store.Controller = (*settlementProbe)(nil)
)

// newSettlementProbe decorates the store scanner and controller the composition
// consumes.
func newSettlementProbe(scanner store.Scanner, controller store.Controller) *settlementProbe {
	return &settlementProbe{scanner: scanner, controller: controller}
}

// watchExecution counts settlement discovery passes whose succeeded-execution
// page contains executionID. Every counted pass follows the task rows that were
// current at that moment and then reports their current execution to the task
// capability.
func (probe *settlementProbe) watchExecution(executionID id.ExecutionID) {
	probe.mu.Lock()
	probe.watched = executionID
	probe.mu.Unlock()
}

// armTaskGate holds the next settlement open-task page that contains taskID.
// The page snapshot is taken before the gate blocks, so the test can commit a
// newer task state while the settlement loop still observes the older page.
func (probe *settlementProbe) armTaskGate(taskID id.TaskID) *settlementGate {
	gate := &settlementGate{captured: make(chan struct{}), release: make(chan struct{})}
	probe.mu.Lock()
	probe.taskID = taskID
	probe.gate = gate
	probe.mu.Unlock()
	return gate
}

// passesFor reports how many settlement discovery passes saw executionID as a
// succeeded execution.
func (probe *settlementProbe) passesFor(executionID id.ExecutionID) int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.watched != executionID {
		return 0
	}
	return probe.passes
}

// Query passes every scoped read through and gates the settlement open-task
// page after its snapshot is materialized.
func (probe *settlementProbe) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	page, err := probe.scanner.Query(ctx, scope, query)
	if err != nil {
		return page, err
	}
	gate := probe.takeTaskGate(query, page)
	if gate == nil {
		return page, nil
	}
	gate.page = page
	close(gate.captured)
	select {
	case <-gate.release:
		return page, nil
	case <-ctx.Done():
		return store.QueryPage{}, ctx.Err()
	}
}

// takeTaskGate disarms and returns the gate when query is the settlement
// open-task page and page carries the watched task.
func (probe *settlementProbe) takeTaskGate(query store.ScanQuery, page store.QueryPage) *settlementGate {
	if query.Table != "tasks" || query.Equals["state"] != string(task.StateOpen) {
		return nil
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.gate == nil || probe.taskID == "" || !pageHasTask(page, probe.taskID) {
		return nil
	}
	gate := probe.gate
	probe.gate = nil
	return gate
}

// Scan passes every controller read through and counts succeeded-execution
// discovery pages that carry the watched execution.
func (probe *settlementProbe) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	page, err := probe.controller.Scan(ctx, scan)
	if err != nil {
		return page, err
	}
	if scan.Table != "executions" || scan.Equals["state"] != string(kernel.ExecutionSucceeded) {
		return page, nil
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.watched != "" && pageHasExecution(page, probe.watched) {
		probe.passes++
	}
	return page, nil
}

// Read passes one controller read through unchanged.
func (probe *settlementProbe) Read(ctx context.Context, table, identity string) (store.Row, error) {
	return probe.controller.Read(ctx, table, identity)
}

// Execute passes one controller unit through unchanged.
func (probe *settlementProbe) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	return probe.controller.Execute(ctx, executionID, unit)
}

// pageHasTask reports whether one page carries the task row.
func pageHasTask(page store.QueryPage, taskID id.TaskID) bool {
	for _, row := range page.Rows {
		if row.Table != "tasks" {
			continue
		}
		if value, ok := row.Values["id"].(string); ok && value == string(taskID) {
			return true
		}
	}
	return false
}

// pageHasExecution reports whether one page carries the execution row.
func pageHasExecution(page store.QueryPage, executionID id.ExecutionID) bool {
	for _, row := range page.Rows {
		if row.Table != "executions" {
			continue
		}
		if value, ok := row.Values["id"].(string); ok && value == string(executionID) {
			return true
		}
	}
	return false
}

// harness owns the deterministic composition input shared by every test and
// can start more than one App over the same memory store (restart evidence).
type harness struct {
	t          *testing.T
	clock      *fakeClock
	store      *memory.Store
	logs       *safeBuffer
	config     app.Config
	publisher  *fakePublisher
	settlement *settlementProbe
	started    []*runningEnv
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := newFakeClock()
	store := memory.NewWithClock(clock.Now)
	logs := &safeBuffer{}
	publisher := newFakePublisher()
	settlement := newSettlementProbe(store, store.Controller())
	allRoles := []tenancy.Role{
		tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator,
	}
	config := app.Config{
		Profile: app.ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   "http://127.0.0.1:0",
		},
		InstallationID:       "installation-composition",
		DataDir:              t.TempDir(),
		DefaultActionTimeout: 5 * time.Minute,
		LoopInterval:         2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
		Dev: &app.DevelopmentConfig{
			Tokens: []app.DevToken{
				{
					Credential: adminToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID},
						Roles:   []tenancy.Role{tenancy.RoleAdmin},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: projectToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   allRoles,
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: otherToken,
					Principal: identity.Principal{
						ID:      "admin-2",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: otherID},
						Roles:   allRoles,
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: foreignToken,
					Principal: identity.Principal{
						ID:      "admin-3",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-9"},
						Roles:   allRoles,
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: operatorToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   []tenancy.Role{tenancy.RoleOperator},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: viewerToken,
					Principal: identity.Principal{
						ID:      "viewer-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   []tenancy.Role{tenancy.RoleViewer},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: tenantOperatorToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID},
						Roles:   []tenancy.Role{tenancy.RoleOperator},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: serviceToken,
					Principal: identity.Principal{
						ID:      "service-1",
						Kind:    identity.KindService,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   []tenancy.Role{tenancy.RoleOperator},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(24 * time.Hour),
				},
				{
					Credential: expiredToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID},
						Roles:   []tenancy.Role{tenancy.RoleAdmin},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: clock.Now().Add(-time.Hour),
				},
			},
			Bootstrap: &app.Bootstrap{
				TenantID:       tenantID,
				TenantName:     "Tenant One",
				ProjectID:      projectID,
				ProjectName:    "Project One",
				AdminPrincipal: "admin-1",
				AdminRoles:     []string{string(tenancy.RoleAdmin)},
			},
		},
		Adapters: &app.Adapters{
			Store:      store,
			Scanner:    settlement,
			Controller: settlement,
			Publisher:  publisher,
			Clock:      clock,
		},
	}
	return &harness{t: t, clock: clock, store: store, logs: logs, config: config, publisher: publisher, settlement: settlement}
}

// start builds and serves one App over the shared store on a fresh loopback
// listener. Every started App is closed by the test cleanup.
func (harness *harness) start(runtime kernelengine.Runtime) *runningEnv {
	harness.t.Helper()
	config := harness.config
	config.Adapters.Runtime = runtime
	composed, err := app.New(context.Background(), config)
	if err != nil {
		harness.t.Fatalf("app.New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = composed.Close(context.Background())
		harness.t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- composed.Serve(ctx, listener) }()
	environment := &runningEnv{
		harness:  harness,
		app:      composed,
		listener: listener,
		baseURL:  "http://" + listener.Addr().String(),
		cancel:   cancel,
		served:   served,
	}
	harness.started = append(harness.started, environment)
	harness.t.Cleanup(func() { environment.close() })

	environment.eventually(5*time.Second, func() bool {
		response, err := environment.mcpRaw(api.MCPMethodToolsList, nil, "")
		return err == nil && response.Error == nil
	})
	return environment
}

// runningEnv is one served composition.
type runningEnv struct {
	harness  *harness
	app      *app.App
	listener net.Listener
	baseURL  string
	cancel   context.CancelFunc
	served   chan error
	once     sync.Once
}

// close stops the server and joins its goroutines. It is idempotent.
func (environment *runningEnv) close() {
	environment.once.Do(func() {
		environment.cancel()
		select {
		case err := <-environment.served:
			if err != nil {
				environment.harness.t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			environment.harness.t.Error("Serve did not return after cancellation")
		}
		if err := environment.app.Wait(); err != nil {
			environment.harness.t.Errorf("Wait: %v", err)
		}
	})
}

// transport builds the public REST client bound to one bearer credential.
//
// The credential expiry is derived from the real wall clock because
// client.HTTPTransport evaluates it with time.Now, not the composition's
// injected clock. The server-side fixture tokens below deliberately keep the
// deterministic fixture clock: the composed authenticator evaluates those with
// the injected clock, so their expiry stays deterministic (including the
// expired-token fixture). Mixing the two clocks made every public-client test
// fail once real time passed the fixture date.
func (environment *runningEnv) transport(token string) *client.HTTPTransport {
	return environment.transportWithClock(token, time.Now)
}

// transportWithClock builds the public REST client over one clock function.
// The same function both derives the credential expiry and is installed as the
// transport clock, so expiry and evaluation can never drift apart. Production
// tests pass time.Now; clock tests pass a fixed instant to prove the coupling
// independently of the real date.
func (environment *runningEnv) transportWithClock(token string, now func() time.Time) *client.HTTPTransport {
	return &client.HTTPTransport{
		BaseURL:      environment.baseURL,
		RoundTripper: http.DefaultTransport,
		Source: staticCredential{credential: client.Credential{
			Kind:      client.KindSession,
			Token:     token,
			ExpiresAt: now().Add(24 * time.Hour),
		}},
		Now:   now,
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
}

type staticCredential struct {
	credential client.Credential
}

func (source staticCredential) Credential(context.Context) (client.Credential, error) {
	return source.credential, nil
}

// call performs one raw REST envelope call. The suite uses the raw edge where it
// must inspect the exact status/error body (parity error classes, secret scan)
// and the public client transport where the scenario exercises client behavior.
func (environment *runningEnv) call(token, operation string, input any, expected *uint64, key string) (api.Result, api.Error) {
	environment.harness.t.Helper()
	route, ok := api.LookupRoute(operation)
	if !ok {
		environment.harness.t.Fatalf("unknown operation %q", operation)
	}
	path := expandPath(environment.harness.t, route, input)
	request := api.Request{ExpectedVersion: expected, IdempotencyKey: key, Input: encodeInput(environment.harness.t, input)}
	status, data := environment.restRaw(string(route.Method), path, token, request)
	if status == api.HTTPStatusSuccess {
		var result api.Result
		if err := json.Unmarshal(data, &result); err != nil {
			environment.harness.t.Fatalf("%s result: %v; body=%s", operation, err, string(data))
		}
		if err := result.Validate(); err != nil {
			environment.harness.t.Fatalf("%s result: %v", operation, err)
		}
		return result, api.Error{}
	}
	var publicError api.Error
	if err := json.Unmarshal(data, &publicError); err != nil {
		environment.harness.t.Fatalf("%s error body is not an api.Error: %v; body=%s", operation, err, string(data))
	}
	if want, ok := api.HTTPStatus(publicError.Code); !ok || want != status {
		environment.harness.t.Fatalf("%s error status=%d code=%q", operation, status, publicError.Code)
	}
	return api.Result{}, publicError
}

// invoke performs one REST call and maps a refusal to its public fault.
func (environment *runningEnv) invoke(token, operation string, input any, expected *uint64, key string) (api.Result, error) {
	environment.harness.t.Helper()
	result, publicError := environment.call(token, operation, input, expected, key)
	if publicError.Code == "" {
		return result, nil
	}
	class, ok := fault.ClassFor(fault.Code(publicError.Code))
	if !ok {
		environment.harness.t.Fatalf("%s returned unknown public code %q", operation, publicError.Code)
	}
	return api.Result{}, fault.New(class, publicError.Message)
}

// mustInvoke performs one REST call and fails the test on a refusal.
func (environment *runningEnv) mustInvoke(token, operation string, input any, expected *uint64, key string) api.Result {
	environment.harness.t.Helper()
	result, err := environment.invoke(token, operation, input, expected, key)
	if err != nil {
		environment.harness.t.Fatalf("%s over REST: %v", operation, err)
	}
	return result
}

// decode decodes one REST result body into target.
func (environment *runningEnv) decode(result api.Result, target any) {
	environment.harness.t.Helper()
	if err := json.Unmarshal(result.Body, target); err != nil {
		environment.harness.t.Fatalf("decode %s body: %v; body=%s", result.Operation, err, string(result.Body))
	}
}

// restRaw performs one raw REST envelope request and returns the status and the
// exact response bytes so a test can inspect public errors and scan secrets.
func (environment *runningEnv) restRaw(method, path, token string, request api.Request) (int, []byte) {
	environment.harness.t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		environment.harness.t.Fatalf("marshal request: %v", err)
	}
	httpRequest, err := http.NewRequest(method, environment.baseURL+path, bytes.NewReader(body))
	if err != nil {
		environment.harness.t.Fatalf("build request: %v", err)
	}
	httpRequest.Header.Set("Content-Type", api.MediaType)
	if token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		environment.harness.t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		environment.harness.t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, data
}

// restError performs one raw REST call and decodes its public error body.
func (environment *runningEnv) restError(token, operation string, input any, expected *uint64, key string) api.Error {
	environment.harness.t.Helper()
	route, ok := api.LookupRoute(operation)
	if !ok {
		environment.harness.t.Fatalf("unknown operation %q", operation)
	}
	path := expandPath(environment.harness.t, route, input)
	request := api.Request{ExpectedVersion: expected, IdempotencyKey: key, Input: encodeInput(environment.harness.t, input)}
	status, data := environment.restRaw(string(route.Method), path, token, request)
	var publicError api.Error
	if err := json.Unmarshal(data, &publicError); err != nil {
		environment.harness.t.Fatalf("%s error body is not an api.Error: %v; body=%s", operation, err, string(data))
	}
	if want, ok := api.HTTPStatus(publicError.Code); !ok || want != status {
		environment.harness.t.Fatalf("%s error status=%d code=%q", operation, status, publicError.Code)
	}
	return publicError
}

// expandPath substitutes the declared route parameters from the canonical DTO
// fields of the request input.
func expandPath(t *testing.T, route api.Route, input any) string {
	t.Helper()
	encoded := encodeInput(t, input)
	var fields map[string]any
	if len(encoded) != 0 {
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("route input is not an object: %v", err)
		}
	}
	segments := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	for index, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		value, ok := lookupField(fields, canonicalRouteField(route.Operation, name))
		if !ok || value == "" {
			t.Fatalf("route %s requires parameter %q", route.Operation, name)
		}
		segments[index] = value
	}
	return "/" + strings.Join(segments, "/")
}

// canonicalRouteField maps one route-template parameter to the canonical DTO
// field from the operation codec table.
func canonicalRouteField(operation, name string) string {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		for _, param := range codec.RouteParams {
			if param.Name == name {
				return param.Field
			}
		}
	}
	return name
}

// lookupField reads one dotted JSON field path.
func lookupField(fields map[string]any, path string) (string, bool) {
	var current any = fields
	for segment := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[segment]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}

// mcpRaw performs one raw MCP JSON-RPC request over HTTP.
func (environment *runningEnv) mcpRaw(method string, params any, token string) (api.MCPResponse, error) {
	body := map[string]any{"jsonrpc": api.MCPJSONRPC, "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return api.MCPResponse{}, err
	}
	request, err := http.NewRequest(http.MethodPost, environment.baseURL+app.DefaultMCPPath, bytes.NewReader(encoded))
	if err != nil {
		return api.MCPResponse{}, err
	}
	request.Header.Set("Content-Type", api.MediaType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return api.MCPResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return api.MCPResponse{}, err
	}
	var decoded api.MCPResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return api.MCPResponse{}, fmt.Errorf("decode MCP response: %w; body=%s", err, string(data))
	}
	return decoded, nil
}

// mcpCall performs one authorized MCP tools/call.
func (environment *runningEnv) mcpCall(token, operation string, input any, expected *uint64, key string) (api.Result, api.Error, error) {
	arguments := api.Request{
		ExpectedVersion: expected,
		IdempotencyKey:  key,
		Input:           encodeInput(environment.harness.t, input),
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return api.Result{}, api.Error{}, err
	}
	response, err := environment.mcpRaw(api.MCPMethodToolsCall, api.MCPToolsCallParams{
		Name:      operation,
		Arguments: encoded,
	}, token)
	if err != nil {
		return api.Result{}, api.Error{}, err
	}
	if response.Error != nil {
		var publicError api.Error
		if err := json.Unmarshal(response.Error.Data, &publicError); err != nil {
			return api.Result{}, api.Error{}, fmt.Errorf("decode MCP error data: %w; data=%s", err, string(response.Error.Data))
		}
		return api.Result{}, publicError, nil
	}
	var result api.Result
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return api.Result{}, api.Error{}, fmt.Errorf("decode MCP result: %w; result=%s", err, string(response.Result))
	}
	if err := result.Validate(); err != nil {
		return api.Result{}, api.Error{}, fmt.Errorf("invalid MCP result: %w", err)
	}
	return result, api.Error{}, nil
}

// mcpMustCall performs one MCP tools/call and fails the test on a refusal.
func (environment *runningEnv) mcpMustCall(token, operation string, input any, expected *uint64, key string) api.Result {
	environment.harness.t.Helper()
	result, publicError, err := environment.mcpCall(token, operation, input, expected, key)
	if err != nil {
		environment.harness.t.Fatalf("%s over MCP: %v", operation, err)
	}
	if publicError.Code != "" {
		environment.harness.t.Fatalf("%s over MCP refused: %s", operation, publicError.Code)
	}
	return result
}

// eventually polls a condition with a bounded deadline.
func (environment *runningEnv) eventually(timeout time.Duration, condition func() bool) {
	environment.harness.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	environment.harness.t.Fatalf("condition was not satisfied within %s", timeout)
}

// encodeInput marshals one operation input, normalizing nil to an empty object.
func encodeInput(t *testing.T, input any) json.RawMessage {
	t.Helper()
	if input == nil {
		return json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return encoded
}

// codeOf extracts the stable public code of one client fault.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	code, ok := fault.CodeOf(err)
	if !ok {
		t.Fatalf("error %v has no public code", err)
	}
	return string(code)
}

// grantEngineer grants the project roles the task/pipeline services require.
func (environment *runningEnv) grantEngineer(principal, project string) {
	environment.harness.t.Helper()
	environment.mustInvoke(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   project,
		PrincipalID: principal,
		Roles:       []string{"admin", "engineer", "reviewer", "operator"},
	}, nil, "membership-"+principal+"-"+project)
}

// applyPipeline applies the standard container pipeline and returns its id.
func (environment *runningEnv) applyPipeline(name string) string {
	environment.harness.t.Helper()
	result := environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: projectID,
		Name:      name,
		YAML:      []byte(smokePipeline),
	}, nil, "pipeline-apply-"+name)
	var applied api.PipelineApplyResult
	environment.decode(result, &applied)
	return applied.PipelineID
}

// smokePipeline is a small valid container pipeline the fake runtime runs.
const smokePipeline = `actions:
  - name: inspect
    image: registry.example.com/kodama/base@sha256:cae662172fd450bb0cd710a769079c05bfc5d8e35efa6576edc7d0377afdd4a2
    commands:
      - echo "integration composition work completed"
`
