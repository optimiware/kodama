package app

// Shared deterministic fixtures for the workload credential renewal tests. No
// test here starts a process, container, database or broker: the clock, store,
// runtime, key provider and broker are fakes or in-memory implementations owned
// by the test.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// renewalInterval is the deterministic scan cadence the renewal fixtures
// configure. The fake clock still decides whether a credential is due, so a
// short real interval only bounds how quickly the test observes a scan.
const renewalInterval = 2 * time.Millisecond

// renewalAttempt is one observed RefreshCredential call and its outcome.
type renewalAttempt struct {
	request   kernelengine.CredentialRequest
	err       error
	delivered bool
}

// renewalRuntime is the fake Kernel runtime/workspace adapter used by the
// renewal tests. It embeds the recording runtime (Prepare, Start,
// Observe, Stop, Remove, Workspace) and replaces only the credential-refresh
// seam so a test can observe the exact delivery, inject delivery failures and
// hold a delivery open to prove cancellation and join.
type renewalRuntime struct {
	*recordingRuntime

	mu           sync.Mutex
	attempts     []renewalAttempt
	failures     map[id.ExecutionID]int
	always       map[id.ExecutionID]bool
	gate         chan struct{}
	reached      chan struct{}
	ignoreCancel bool
}

var (
	_ kernelengine.Runtime   = (*renewalRuntime)(nil)
	_ kernelengine.Workspace = (*renewalRuntime)(nil)
)

func newRenewalRuntime(exitNames ...id.ActionName) *renewalRuntime {
	return &renewalRuntime{
		recordingRuntime: newRecordingRuntime(exitNames...),
		failures:         make(map[id.ExecutionID]int),
		always:           make(map[id.ExecutionID]bool),
	}
}

// RefreshCredential records the delivery and returns injected evidence. It never
// renders or persists the token.
func (runtime *renewalRuntime) RefreshCredential(ctx context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	runtime.mu.Lock()
	remaining := runtime.failures[request.Identity.ExecutionID]
	if remaining > 0 {
		runtime.failures[request.Identity.ExecutionID] = remaining - 1
	}
	always := runtime.always[request.Identity.ExecutionID]
	gate, reached := runtime.gate, runtime.reached
	ignoreCancel := runtime.ignoreCancel
	runtime.mu.Unlock()

	if reached != nil {
		select {
		case reached <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		if ignoreCancel {
			// A gate that ignores cancellation models an adapter call that is
			// already inside an effect when the composition context ends.
			<-gate
		} else {
			select {
			case <-ctx.Done():
				runtime.record(request, fault.Wrap(fault.DeadlineExpired, "credential refresh was canceled", ctx.Err()), false)
				return kernelengine.CredentialRefresh{}, ctx.Err()
			case <-gate:
			}
		}
	}
	if always || remaining > 0 {
		// The injected adapter error deliberately embeds the token it was asked
		// to deliver. The composition must render only the fault class, never
		// adapter error text, so a test that captures composition logs can
		// detect a raw err.Error() leak even though the real bridge wraps the
		// adapter cause.
		injected := fault.New(fault.UncertainExternalEffect, "injected credential refresh failure for token "+request.Credential)
		runtime.record(request, injected, false)
		return kernelengine.CredentialRefresh{Delivered: false}, injected
	}
	runtime.record(request, nil, true)
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

func (runtime *renewalRuntime) record(request kernelengine.CredentialRequest, err error, delivered bool) {
	runtime.mu.Lock()
	runtime.attempts = append(runtime.attempts, renewalAttempt{request: request, err: err, delivered: delivered})
	runtime.mu.Unlock()
}

// failNext makes the next count deliveries for one execution fail.
func (runtime *renewalRuntime) failNext(executionID id.ExecutionID, count int) {
	runtime.mu.Lock()
	runtime.failures[executionID] = count
	runtime.mu.Unlock()
}

// failAlways makes every delivery for one execution fail until clearFailure.
func (runtime *renewalRuntime) failAlways(executionID id.ExecutionID) {
	runtime.mu.Lock()
	runtime.always[executionID] = true
	runtime.mu.Unlock()
}

// clearFailure stops injecting delivery failures for one execution.
func (runtime *renewalRuntime) clearFailure(executionID id.ExecutionID) {
	runtime.mu.Lock()
	delete(runtime.always, executionID)
	delete(runtime.failures, executionID)
	runtime.mu.Unlock()
}

// blockRefresh holds the next delivery open until the gate is released or the
// caller context is canceled. The returned reached channel receives once a
// delivery has entered the blocked call.
func (runtime *renewalRuntime) blockRefresh() (reached <-chan struct{}, release func()) {
	gate := make(chan struct{})
	signal := make(chan struct{}, 1)
	runtime.mu.Lock()
	runtime.gate = gate
	runtime.reached = signal
	runtime.mu.Unlock()
	var once sync.Once
	return signal, func() { once.Do(func() { close(gate) }) }
}

// blockRefreshUncancellable holds the next delivery open until the gate is
// released, even when the delivery context is canceled. blockRefresh cannot pin
// the App.Close join: a delivery that returns on ctx.Done lets an unjoined
// renewal goroutine exit on its own once Close cancels the context, so the join
// and a leaked goroutine look identical. This gate only releases on the test's
// signal, so Close must wait for the loop itself.
func (runtime *renewalRuntime) blockRefreshUncancellable() (reached <-chan struct{}, release func()) {
	gate := make(chan struct{})
	signal := make(chan struct{}, 1)
	runtime.mu.Lock()
	runtime.gate = gate
	runtime.reached = signal
	runtime.ignoreCancel = true
	runtime.mu.Unlock()
	var once sync.Once
	return signal, func() { once.Do(func() { close(gate) }) }
}

func (runtime *renewalRuntime) attemptsFor(executionID id.ExecutionID) []renewalAttempt {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	var selected []renewalAttempt
	for _, attempt := range runtime.attempts {
		if attempt.request.Identity.ExecutionID == executionID {
			selected = append(selected, attempt)
		}
	}
	return selected
}

func (runtime *renewalRuntime) refreshAttemptsFor(executionID id.ExecutionID) int {
	return len(runtime.attemptsFor(executionID))
}

func (runtime *renewalRuntime) deliveredFor(executionID id.ExecutionID) int {
	delivered := 0
	for _, attempt := range runtime.attemptsFor(executionID) {
		if attempt.delivered {
			delivered++
		}
	}
	return delivered
}

func (runtime *renewalRuntime) totalAttempts() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.attempts)
}

// renewalShared is one durable development composition input shared by every
// renewal test. Only the composition is rebuilt between appA and appB.
type renewalShared struct {
	composedShared
	cipher        secretCipher
	runtime       *renewalRuntime
	renewBefore   time.Duration
	actionTimeout time.Duration
}

func newRenewalShared(t *testing.T, renewBefore, actionTimeout time.Duration, exitNames ...id.ActionName) *renewalShared {
	t.Helper()
	shared := newComposedShared(t)
	cipher, err := keys.NewCipher(shared.provider, rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewCipher: %v", err)
	}
	return &renewalShared{
		composedShared: shared,
		cipher:         cipher,
		runtime:        newRenewalRuntime(exitNames...),
		renewBefore:    renewBefore,
		actionTimeout:  actionTimeout,
	}
}

// config builds one development composition over the shared durable state.
func (shared *renewalShared) config(t *testing.T) Config {
	t.Helper()
	config := shared.developmentConfig(t, shared.runtime)
	config.DefaultActionTimeout = shared.actionTimeout
	config.RenewBefore = shared.renewBefore
	config.RenewInterval = renewalInterval
	return config
}

// start constructs and starts one composition over the shared state, grants the
// project roles the fixtures need and closes the composition at test cleanup.
func (shared *renewalShared) start(t *testing.T) *App {
	t.Helper()
	app := startComposedApp(t, shared.config(t))
	grantProjectEngineer(t, app.Handler(), "dev-admin-token")
	t.Cleanup(func() { closeApp(t, app) })
	return app
}

func (shared *renewalShared) controller() store.Controller {
	return shared.store.Controller()
}

// startRenewalTask creates and starts one task in a project.
func startRenewalTask(t *testing.T, handler http.Handler, token, projectID, pipelineID, key string, inputs map[string]string) api.TaskCreateAndStartResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/projects/"+projectID+"/tasks/create-and-start", token,
		api.TaskCreateAndStartRequest{Title: "renewal", PipelineID: pipelineID, Inputs: inputs}, nil, key)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskCreateAndStartResult](t, result)
}

// renewalStartContainer starts one single-container execution that stays
// running and returns its execution identity and the credential delivered into
// Prepare.
func renewalStartContainer(t *testing.T, shared *renewalShared, handler http.Handler, token, projectID, key string, wantPrepares int) (string, string) {
	t.Helper()
	applied := applyProjectPipeline(t, handler, token, projectID,
		activationPipeline(activationAction{name: "inspect"}), key+"-pipeline")
	started := startRenewalTask(t, handler, token, projectID, applied.PipelineID, key+"-task", nil)
	prepares := waitForActivationPrepares(t, shared.runtime.recordingRuntime, wantPrepares)
	for _, prepare := range prepares {
		if prepare.Identity.ExecutionID != id.ExecutionID(started.ExecutionID) {
			continue
		}
		if prepare.Credential == "" {
			t.Fatalf("execution %q received an empty credential", started.ExecutionID)
		}
		return started.ExecutionID, prepare.Credential
	}
	t.Fatalf("no preparation was recorded for execution %q", started.ExecutionID)
	return "", ""
}

// renewalRequestPipeline declares one request action that needs one input. Its
// activation revokes the execution credential while the execution keeps
// running.
func renewalRequestPipeline() []byte {
	return []byte("inputs: [repository]\nactions:\n  - name: review\n    request: role/reviewer\n    subject:\n      resource:\n        from: inputs.repository\n")
}

// renewalSeedTenant seeds a second tenant, its project and its tenant-wide
// administrator through the same explicit first-admin seed the composition uses
// for the bootstrap tenant. The public edge has no installation-level tenant
// administrator, so a second tenant's first membership cannot be created
// through an operation.
func renewalSeedTenant(t *testing.T, shared *renewalShared, bootstrap Bootstrap) {
	t.Helper()
	if err := seedFirstAdmin(context.Background(), shared.store, shared.store, shared.ids,
		shared.cipher, "installation-1", secret.MinKeyVersion, &bootstrap, shared.clock); err != nil {
		t.Fatalf("seed second tenant: %v", err)
	}
}

// renewalTenantTwoToken is the explicit development credential of a second
// tenant's project administrator.
func renewalTenantTwoToken(clock *fakeClock) DevToken {
	return DevToken{
		Credential: "dev-tenant2-token",
		Principal: identity.Principal{
			ID:      "admin-2",
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"},
			Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		},
		ExpiresAt: clock.Now().Add(24 * time.Hour),
	}
}

// renewalDurableCredential reads the committed nonce/expiry pair of one
// execution from durable state.
func renewalDurableCredential(t *testing.T, controller store.Controller, executionID string) token.Credential {
	t.Helper()
	row, err := controller.Read(context.Background(), "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	credential := token.Credential{}
	if value := row.Values["token_nonce"]; value != nil {
		nonce, ok := value.([]byte)
		if !ok {
			t.Fatalf("execution %q token_nonce has type %T; want []byte", executionID, value)
		}
		credential.Nonce = append([]byte(nil), nonce...)
	}
	if value := row.Values["token_expires_at"]; value != nil {
		expiry, ok := value.(time.Time)
		if !ok {
			t.Fatalf("execution %q token_expires_at has type %T; want time.Time", executionID, value)
		}
		credential.Expiry = expiry
	}
	return credential
}

// renewalActionDeadline reads the persisted absolute deadline of the execution's
// running action.
func renewalActionDeadline(t *testing.T, controller store.Controller, executionID string) time.Time {
	t.Helper()
	page, err := controller.Scan(context.Background(), store.ControllerScan{
		Table: "actions",
		Equals: map[string]any{
			"execution_id": executionID,
			"state":        "running",
		},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   1})
	if err != nil {
		t.Fatalf("scan running action of execution %q: %v", executionID, err)
	}
	if len(page.Rows) == 0 {
		t.Fatalf("execution %q has no running action", executionID)
	}
	deadline, ok := page.Rows[0].Values["deadline"].(time.Time)
	if !ok {
		t.Fatalf("running action of execution %q has no persisted deadline", executionID)
	}
	return deadline
}

// renewalSetRunningActionState rewrites the durable state of the execution's
// single running action and returns its identity. It is the controller seam a
// test uses to reach a durable action state without a workflow transition;
// dispatch must already be frozen so the reconciler is not concurrently
// advancing the same row.
func renewalSetRunningActionState(t *testing.T, controller store.Controller, executionID, state string) string {
	t.Helper()
	return renewalRewriteRunningAction(t, controller, executionID, map[string]any{"state": state})
}

// renewalRewriteRunningAction writes values onto the execution's single running
// action at its observed version and returns the action identity.
func renewalRewriteRunningAction(t *testing.T, controller store.Controller, executionID string, values map[string]any) string {
	t.Helper()
	ctx := context.Background()
	page, err := controller.Scan(ctx, store.ControllerScan{
		Table: "actions",
		Equals: map[string]any{
			"execution_id": executionID,
			"state":        "running",
		},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   1})
	if err != nil {
		t.Fatalf("scan running action of execution %q: %v", executionID, err)
	}
	if len(page.Rows) == 0 {
		t.Fatalf("execution %q has no running action", executionID)
	}
	row := page.Rows[0]
	actionID, err := rowText(row, "id")
	if err != nil {
		t.Fatalf("read running action id: %v", err)
	}
	version, err := rowUint64(row, "version")
	if err != nil {
		t.Fatalf("read running action version: %v", err)
	}
	if _, err := controller.Execute(ctx, executionID, store.Unit{
		Writes: []store.Write{{
			Table:         "actions",
			ID:            actionID,
			ExpectVersion: version,
			Values:        values,
		}},
	}); err != nil {
		t.Fatalf("rewrite running action %q of execution %q with %v: %v", actionID, executionID, values, err)
	}
	return actionID
}

// renewalSetActionState rewrites the durable state of one known action row.
func renewalSetActionState(t *testing.T, controller store.Controller, executionID, actionID, state string) {
	t.Helper()
	ctx := context.Background()
	row, err := controller.Read(ctx, "actions", actionID)
	if err != nil {
		t.Fatalf("read action %q: %v", actionID, err)
	}
	version, err := rowUint64(row, "version")
	if err != nil {
		t.Fatalf("read action %q version: %v", actionID, err)
	}
	if _, err := controller.Execute(ctx, executionID, store.Unit{
		Writes: []store.Write{{
			Table:         "actions",
			ID:            actionID,
			ExpectVersion: version,
			Values:        map[string]any{"state": state},
		}},
	}); err != nil {
		t.Fatalf("move action %q to %q: %v", actionID, state, err)
	}
}

// renewalRenewalReceipts counts the durable renewal receipts of one execution.
// A receipt is the only durable record of a committed rotation, so a pass that
// must not renew must leave this count unchanged. Receipts are read through the
// scoped scanner, the authority that owns them.
func renewalRenewalReceipts(t *testing.T, shared *renewalShared, executionID string) int {
	t.Helper()
	ctx := context.Background()
	row, err := shared.controller().Read(ctx, "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	scope, _, _, _, err := renewalExecutionFact(row)
	if err != nil {
		t.Fatalf("read execution %q scope: %v", executionID, err)
	}
	page, err := shared.store.Query(ctx, scope, store.ScanQuery{
		Table:   "receipts",
		Equals:  map[string]any{"execution_id": executionID},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		t.Fatalf("scan receipts of execution %q: %v", executionID, err)
	}
	count := 0
	for _, row := range page.Rows {
		operation, err := rowText(row, "operation")
		if err != nil {
			t.Fatalf("read receipt operation: %v", err)
		}
		if operation == renewalCredentialOperation {
			count++
		}
	}
	return count
}

// renewalFailingCredentialReader fails ReadCredential for one execution. It is
// installed on a detached loop (or before a composition starts), so no
// synchronization with a live scan is needed. fail may be cleared to model the
// durable reader recovering.
type renewalFailingCredentialReader struct {
	inner token.CredentialReader
	fail  id.ExecutionID
}

var _ token.CredentialReader = (*renewalFailingCredentialReader)(nil)

func (reader *renewalFailingCredentialReader) ReadCredential(ctx context.Context, request token.CredentialRequest) (token.CredentialFact, error) {
	if reader.fail != "" && request.ExecutionID == reader.fail {
		return token.CredentialFact{}, fault.New(fault.DependencyUnavailable, "injected credential read failure")
	}
	return reader.inner.ReadCredential(ctx, request)
}

// renewalVersionBumpController bumps the durable version of every row in the
// first non-empty renewal executions page it returns, after the page snapshot
// was taken and before the caller rotates against it. That is exactly the race
// a concurrent writer wins between the renewal scan and the compare-and-commit,
// so a Rotate version conflict is deterministic. The exact renewal query shape
// is matched, so no other composition reader is affected, and the bump happens
// once.
type renewalVersionBumpController struct {
	inner store.Controller
	once  sync.Once
}

var _ store.Controller = (*renewalVersionBumpController)(nil)

func newRenewalVersionBumpController(inner store.Controller) *renewalVersionBumpController {
	return &renewalVersionBumpController{inner: inner}
}

func (controller *renewalVersionBumpController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	page, err := controller.inner.Scan(ctx, scan)
	if err != nil {
		return store.QueryPage{}, err
	}
	if scan.Table != "executions" || scan.Equals["state"] != "running" ||
		len(scan.OrderBy) != 1 || scan.OrderBy[0].Column != "id" ||
		scan.Limit != renewalScanLimit || len(page.Rows) == 0 {
		return page, nil
	}
	controller.once.Do(func() {
		for _, row := range page.Rows {
			executionID, rowErr := rowText(row, "id")
			if rowErr != nil {
				continue
			}
			version, versionErr := rowUint64(row, "version")
			if versionErr != nil {
				continue
			}
			// Rewriting one real column is a normal optimistic write, so the
			// stored version advances exactly as a concurrent workflow commit
			// would advance it.
			_, _ = controller.inner.Execute(ctx, executionID, store.Unit{
				Writes: []store.Write{{
					Table:         "executions",
					ID:            executionID,
					ExpectVersion: version,
					Values:        map[string]any{"state": "running"},
				}},
			})
		}
	})
	return page, nil
}

func (controller *renewalVersionBumpController) Read(ctx context.Context, table, id string) (store.Row, error) {
	return controller.inner.Read(ctx, table, id)
}

func (controller *renewalVersionBumpController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	return controller.inner.Execute(ctx, executionID, unit)
}

// renewalExtendActionDeadline rewrites the persisted deadline of the
// execution's running action, as a consecutive container action would when it
// activates at a later time. Dispatch must already be frozen so the reconciler
// is not concurrently advancing the same row.
func renewalExtendActionDeadline(t *testing.T, controller store.Controller, executionID string, deadline time.Time) {
	t.Helper()
	renewalRewriteRunningAction(t, controller, executionID, map[string]any{"deadline": deadline})
}

// renewalCommitStaleCappedReceipt commits the durable receipt of a capped
// renewal that rotates a deadline-capped credential from D to D. That receipt
// is keyed on the pre-renewal expiry alone, so the loop's per-generation key
// must not replay it.
func renewalCommitStaleCappedReceipt(t *testing.T, shared *renewalShared, app *App, executionID string, deadline time.Time) {
	t.Helper()
	ctx := context.Background()
	row, err := shared.controller().Read(ctx, "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	scope, principalID, version, credential, err := renewalExecutionFact(row)
	if err != nil {
		t.Fatalf("read execution renewal fact: %v", err)
	}
	if !credential.Present() || !credential.Expiry.Equal(deadline) {
		t.Fatalf("stale-receipt fixture credential expiry = %v; want the capped deadline %v", credential.Expiry, deadline)
	}
	generation := strconv.FormatInt(credential.Expiry.UnixNano(), 10)
	fingerprint, err := receipt.Fingerprint(receipt.Content{
		MediaType: "application/octet-stream",
		Bytes:     []byte(executionID + "\x00" + generation),
	})
	if err != nil {
		t.Fatalf("fingerprint the stale receipt: %v", err)
	}
	receiptID, err := shared.ids.NewReceiptID()
	if err != nil {
		t.Fatalf("allocate the stale receipt id: %v", err)
	}
	result, err := json.Marshal(token.IssueResult{State: token.StateCurrent, Version: version + 1})
	if err != nil {
		t.Fatalf("encode the stale receipt result: %v", err)
	}
	now := shared.clock.Now()
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principalID,
			Operation:   renewalCredentialOperation,
			CallerKey:   "renewal:" + executionID + ":" + generation,
		},
		ContentHash: fingerprint,
		Result:      result,
		ExecutionID: id.ExecutionID(executionID),
		At:          now,
	}
	rotated, err := app.renewal.manager.Rotate(ctx, token.IssueRequest{
		Scope:           scope,
		PrincipalID:     principalID,
		ExecutionID:     id.ExecutionID(executionID),
		TTL:             token.DefaultBounds.MaxTTL,
		Deadline:        deadline,
		ExpectedVersion: version,
		Receipt:         record,
	})
	if err != nil {
		t.Fatalf("commit the stale capped renewal: %v", err)
	}
	if rotated.Replayed || rotated.Token == "" {
		t.Fatal("the stale capped renewal fixture did not commit a fresh token")
	}
}

// renewalPendingDelivery is one committed renewal whose post-commit delivery
// failed, so the loop holds that exact token in app.renewal.pending for a later
// retry. The fixture has already cleared the injected delivery failure: every
// later pass must decide from durable state alone.
type renewalPendingDelivery struct {
	shared       *renewalShared
	app          *App
	executionID  string
	execution    id.ExecutionID
	pendingToken string
}

// newRenewalPendingDelivery commits one renewal with its delivery failing and
// returns the committed-but-undelivered token still pending. The test drives
// each renewal pass explicitly: a margin wider than the maximum TTL keeps a
// fresh expiry inside the margin, and the hour-long configured interval keeps
// the background loop asleep between explicit scans.
func newRenewalPendingDelivery(t *testing.T) *renewalPendingDelivery {
	t.Helper()
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	config := shared.config(t)
	config.RenewInterval = time.Hour
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	// Let the background loop perform its initial empty scan and fall asleep.
	time.Sleep(100 * time.Millisecond)

	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "pending-drop", 1)
	execution := id.ExecutionID(executionID)
	// The test drives the renewal passes explicitly, so stop the reconciler
	// from concurrently advancing the execution row version under a manual
	// rotate's compare-and-commit.
	freezeRenewalDispatch(t, app)
	before := renewalDurableCredential(t, shared.controller(), executionID)

	// A rotation at the activation instant cannot extend the committed expiry.
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("initial renewal scan: %v", err)
	}
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("renewal rotated %d times although it could not extend the expiry", attempts)
	}

	// Advance so the committed expiry is extendable, then fail exactly one
	// delivery: the commit succeeds and the token becomes pending.
	shared.clock.Advance(time.Hour)
	shared.runtime.failNext(execution, 1)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with a failed delivery: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || attempts[0].delivered {
		t.Fatalf("renewal attempts = %d; want one failed delivery", len(attempts))
	}
	pendingToken := attempts[0].request.Credential
	if pendingToken == credential {
		t.Fatal("the pending renewal delivered the unchanged credential")
	}
	committed := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(committed.Nonce, before.Nonce) {
		t.Fatal("the failed delivery did not commit a renewal")
	}
	if _, ok := app.renewal.pending[execution]; !ok {
		t.Fatal("the failed delivery was not remembered as pending")
	}

	// Allow every later delivery to succeed so a drop decision can only come
	// from durable state, never from an injected failure.
	shared.runtime.clearFailure(execution)
	return &renewalPendingDelivery{
		shared:       shared,
		app:          app,
		executionID:  executionID,
		execution:    execution,
		pendingToken: pendingToken,
	}
}

// renewalRewriteExecutionRow rewrites selected columns of the execution row
// through the controller authority, the same durable seam the Shell's
// execution units use. It is only a fixture for durable states a test must
// arrange without a full workflow transition.
func renewalRewriteExecutionRow(t *testing.T, controller store.Controller, executionID string, values map[string]any) {
	t.Helper()
	ctx := context.Background()
	row, err := controller.Read(ctx, "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	version, err := rowUint64(row, "version")
	if err != nil {
		t.Fatalf("read execution %q version: %v", executionID, err)
	}
	if _, err := controller.Execute(ctx, executionID, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            executionID,
			ExpectVersion: version,
			Values:        values,
		}},
	}); err != nil {
		t.Fatalf("rewrite execution %q: %v", executionID, err)
	}
}

// renewalClearCommittedCredential leaves the running execution without a
// credential pair, the exact durable state a request action's activation
// commits when it revokes the workload credential.
func renewalClearCommittedCredential(t *testing.T, controller store.Controller, executionID string) {
	t.Helper()
	renewalRewriteExecutionRow(t, controller, executionID, map[string]any{
		"token_nonce":      nil,
		"token_expires_at": nil,
	})
}

// renewalRetainCredentialWhileStopped writes a stopped execution state without
// clearing the credential pair. No production transition produces that row:
// every cancel, failure and success commit invalidates the credential in the
// same unit. The fixture exercises the pending retry's enabled-principal guard
// as defense-in-depth against an inconsistent durable row.
func renewalRetainCredentialWhileStopped(t *testing.T, controller store.Controller, executionID string) {
	t.Helper()
	renewalRewriteExecutionRow(t, controller, executionID, map[string]any{"state": "cancelled"})
}

// renewalRotateCommittedReplacement commits a distinct replacement generation
// through the same token manager a renewal uses, with its own receipt key so
// the commit cannot replay the pending renewal's receipt. It returns the token
// of the new committed generation.
func renewalRotateCommittedReplacement(t *testing.T, shared *renewalShared, app *App, executionID string) string {
	t.Helper()
	ctx := context.Background()
	row, err := shared.controller().Read(ctx, "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	scope, principalID, version, credential, err := renewalExecutionFact(row)
	if err != nil {
		t.Fatalf("read execution renewal fact: %v", err)
	}
	generation := strconv.FormatInt(credential.Expiry.UnixNano(), 10)
	fingerprint, err := receipt.Fingerprint(receipt.Content{
		MediaType: "application/octet-stream",
		Bytes:     []byte(executionID + "\x00replacement\x00" + generation + "\x00" + string(credential.Nonce)),
	})
	if err != nil {
		t.Fatalf("fingerprint the replacement receipt: %v", err)
	}
	receiptID, err := shared.ids.NewReceiptID()
	if err != nil {
		t.Fatalf("allocate the replacement receipt id: %v", err)
	}
	result, err := json.Marshal(token.IssueResult{State: token.StateCurrent, Version: version + 1})
	if err != nil {
		t.Fatalf("encode the replacement receipt result: %v", err)
	}
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principalID,
			Operation:   renewalCredentialOperation,
			CallerKey:   "replacement:" + executionID + ":" + generation,
		},
		ContentHash: fingerprint,
		Result:      result,
		ExecutionID: id.ExecutionID(executionID),
		At:          shared.clock.Now(),
	}
	rotated, err := app.renewal.manager.Rotate(ctx, token.IssueRequest{
		Scope:           scope,
		PrincipalID:     principalID,
		ExecutionID:     id.ExecutionID(executionID),
		TTL:             token.DefaultBounds.MaxTTL,
		Deadline:        renewalActionDeadline(t, shared.controller(), executionID),
		ExpectedVersion: version,
		Receipt:         record,
	})
	if err != nil {
		t.Fatalf("commit the replacement generation: %v", err)
	}
	if rotated.Replayed || rotated.Token == "" {
		t.Fatal("the replacement fixture did not commit a fresh token")
	}
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, credential.Nonce) {
		t.Fatal("the replacement fixture did not replace the committed nonce")
	}
	return rotated.Token
}

// freezeRenewalDispatch stops the Kernel reconciler so a test can keep an
// execution running past its action deadline and observe only the renewal loop.
// It waits for an already in-flight scan to finish before returning.
func freezeRenewalDispatch(t *testing.T, app *App) {
	t.Helper()
	if app == nil || app.gate == nil {
		t.Fatal("composition has no dispatch gate")
	}
	app.gate.enabled.Store(false)
	time.Sleep(50 * time.Millisecond)
}

// lockedLogBuffer is a concurrency-safe slog sink for tests that must read
// composition logs while a background loop is still writing them.
type lockedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (sink *lockedLogBuffer) Write(payload []byte) (int, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.buffer.Write(payload)
}

func (sink *lockedLogBuffer) String() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.buffer.String()
}

// renewalRawErrorDeliverer fails a bounded number of retry deliveries with an
// error whose rendered text carries the token. It models an adapter that
// returns raw request detail, so a logFault that rendered err.Error() would
// leak the token even though the production bridge wraps causes.
type renewalRawErrorDeliverer struct {
	mu       sync.Mutex
	inner    token.Deliverer
	failures int
}

var _ token.Deliverer = (*renewalRawErrorDeliverer)(nil)

func (deliverer *renewalRawErrorDeliverer) Deliver(ctx context.Context, delivery token.Delivery) error {
	deliverer.mu.Lock()
	fail := deliverer.failures > 0
	if fail {
		deliverer.failures--
	}
	deliverer.mu.Unlock()
	if fail {
		return fault.New(fault.UncertainExternalEffect, "injected retry delivery failure for token "+delivery.Token)
	}
	return deliverer.inner.Deliver(ctx, delivery)
}

// renewalVanishingCredentialReader delegates ReadCredential but returns an
// absent credential on one chosen call for one execution, modelling a durable
// credential cleared between the renewal commit and the pending-delivery read.
// The counter is only touched by a detached loop, so it needs no lock.
type renewalVanishingCredentialReader struct {
	inner    token.CredentialReader
	target   id.ExecutionID
	vanishOn int
	calls    int
}

var _ token.CredentialReader = (*renewalVanishingCredentialReader)(nil)

func (reader *renewalVanishingCredentialReader) ReadCredential(ctx context.Context, request token.CredentialRequest) (token.CredentialFact, error) {
	if request.ExecutionID == reader.target {
		reader.calls++
		if reader.calls == reader.vanishOn {
			fact, err := reader.inner.ReadCredential(ctx, request)
			if err != nil {
				return token.CredentialFact{}, err
			}
			fact.Credential = token.Credential{}
			return fact, nil
		}
	}
	return reader.inner.ReadCredential(ctx, request)
}

// renewalRenewalReceiptResult decodes the durable result of the execution's one
// renewal receipt. A committed rotation records the version it produced.
func renewalRenewalReceiptResult(t *testing.T, shared *renewalShared, executionID string) token.IssueResult {
	t.Helper()
	ctx := context.Background()
	row, err := shared.controller().Read(ctx, "executions", executionID)
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	scope, _, _, _, err := renewalExecutionFact(row)
	if err != nil {
		t.Fatalf("read execution %q scope: %v", executionID, err)
	}
	page, err := shared.store.Query(ctx, scope, store.ScanQuery{
		Table:   "receipts",
		Equals:  map[string]any{"execution_id": executionID},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		t.Fatalf("scan receipts of execution %q: %v", executionID, err)
	}
	for _, row := range page.Rows {
		operation, err := rowText(row, "operation")
		if err != nil {
			t.Fatalf("read receipt operation: %v", err)
		}
		if operation != renewalCredentialOperation {
			continue
		}
		raw, ok := row.Values["result"].([]byte)
		if !ok {
			t.Fatalf("renewal receipt result has type %T; want []byte", row.Values["result"])
		}
		var result token.IssueResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode renewal receipt result: %v", err)
		}
		return result
	}
	t.Fatalf("execution %q has no renewal receipt", executionID)
	return token.IssueResult{}
}

// renewalZeroClock is a Clock whose Now is the zero time. It pins the loop's
// refusal to scan against an unusable clock.
type renewalZeroClock struct{}

func (renewalZeroClock) Now() time.Time                             { return time.Time{} }
func (renewalZeroClock) Sleep(context.Context, time.Duration) error { return nil }

// startRenewalManual starts one composition whose renewal loop sleeps for an
// hour between passes, so a test can drive each renewal pass explicitly. The
// initial empty scan runs and falls asleep before the test starts an execution.
func (shared *renewalShared) startRenewalManual(t *testing.T) *App {
	t.Helper()
	return shared.startRenewalManualConfigured(t, nil)
}

// startRenewalManualConfigured starts the same manual composition and applies
// configure to the composition input before construction. A test that needs a
// wrapped controller, credential reader or deliverer installs it here, before
// the renewal goroutine exists, instead of writing a live loop field that the
// background scan reads without any happens-before edge.
func (shared *renewalShared) startRenewalManualConfigured(t *testing.T, configure func(*Config)) *App {
	t.Helper()
	config := shared.config(t)
	config.RenewInterval = time.Hour
	if configure != nil {
		configure(&config)
	}
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	grantProjectEngineer(t, app.Handler(), "dev-admin-token")
	time.Sleep(100 * time.Millisecond)
	return app
}

// newRenewalDetachedLoop builds a renewal loop over the same composition
// dependencies as the running app's loop, applies configure, and never starts
// it. No background goroutine owns the detached loop, so a test may install a
// test double, swap its controller or reseed its pending map without racing a
// live scan. The loop still uses the composition's real manager, credential
// reader, deliverer and clock, so its decisions are the production ones.
func newRenewalDetachedLoop(t *testing.T, source *renewalLoop, configure func(*renewalLoopConfig)) *renewalLoop {
	t.Helper()
	if source == nil {
		t.Fatal("detached renewal loop has no source loop")
	}
	config := renewalLoopConfig{
		Controller:  source.controller,
		Credentials: source.credentials,
		Manager:     source.manager,
		Deliverer:   source.deliverer,
		Clock:       source.clock,
		IDs:         source.ids,
		RenewBefore: source.renewBefore,
		Interval:    source.interval,
		Logger:      source.logger,
	}
	if configure != nil {
		configure(&config)
	}
	loop, err := newRenewalLoop(config)
	if err != nil {
		t.Fatalf("build a detached renewal loop: %v", err)
	}
	return loop
}

// renewalPagedController wraps the real controller and clamps one executions
// page to a small size. It lets a test exercise the renewal scan cursor with a
// handful of executions instead of renewalScanLimit+1; every other table and
// every write delegates unchanged.
type renewalPagedController struct {
	store.Controller
	executionsLimit int
}

var _ store.Controller = renewalPagedController{}

func (controller renewalPagedController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if scan.Table == "executions" && scan.Limit > controller.executionsLimit {
		scan.Limit = controller.executionsLimit
	}
	return controller.Controller.Scan(ctx, scan)
}

// renewalInconsistentPageController returns one fixed executions page on every
// scan, ignoring the cursor, so a test can pin both termination checks of the
// renewal page loop against an adapter that violates the documented
// HasMore/Next pairing.
type renewalInconsistentPageController struct {
	store.Controller
	page  store.QueryPage
	scans int
}

var _ store.Controller = (*renewalInconsistentPageController)(nil)

func newRenewalInconsistentPageController(inner store.Controller, hasMore bool, next string) *renewalInconsistentPageController {
	return &renewalInconsistentPageController{
		Controller: inner,
		page:       store.QueryPage{HasMore: hasMore, Next: next},
	}
}

func (controller *renewalInconsistentPageController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if scan.Table == "executions" {
		if err := ctx.Err(); err != nil {
			return store.QueryPage{}, err
		}
		controller.scans++
		return controller.page, nil
	}
	return controller.Controller.Scan(ctx, scan)
}

// renewalFailingScanController injects one controller scan failure whose error
// text carries a marker. It matches only the renewal loop's cross-scope
// running-executions page (state equality plus id ordering), so the Kernel's
// unrelated recovery scans and workspace activation keep working. The renewal
// loop must log only the fault class, never the adapter text.
type renewalFailingScanController struct {
	store.Controller
	marker string
}

var _ store.Controller = renewalFailingScanController{}

func (controller renewalFailingScanController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if scan.Table == "executions" && scan.Equals["state"] != nil &&
		len(scan.OrderBy) > 0 && scan.OrderBy[0].Column == "id" {
		return store.QueryPage{}, fault.New(fault.DependencyUnavailable, "injected scan failure "+controller.marker)
	}
	return controller.Controller.Scan(ctx, scan)
}

// renewalInsertCompletedAction writes a completed action with a lower index and
// a past deadline for the execution. It is the durable shape a consecutive
// action pipeline leaves behind while a later action is still running. The
// direct write is the same fixture seam renewalExtendActionDeadline uses.
func renewalInsertCompletedAction(t *testing.T, controller store.Controller, executionID string, deadline time.Time) string {
	t.Helper()
	ctx := context.Background()
	page, err := controller.Scan(ctx, store.ControllerScan{
		Table: "actions",
		Equals: map[string]any{
			"execution_id": executionID,
			"state":        "running",
		},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   1})
	if err != nil {
		t.Fatalf("scan running action of execution %q: %v", executionID, err)
	}
	if len(page.Rows) == 0 {
		t.Fatalf("execution %q has no running action", executionID)
	}
	runningIndex, ok := page.Rows[0].Values["index"].(int64)
	if !ok {
		t.Fatalf("running action of execution %q has no integer index", executionID)
	}
	actionID := "completed-earlier-action"
	values := map[string]any{
		"execution_id":       executionID,
		"index":              runningIndex - 1,
		"name":               "completed-earlier",
		"kind":               "container",
		"state":              "succeeded",
		"version":            int64(1),
		"timeout_ms":         int64(time.Minute / time.Millisecond),
		"staged_outputs_set": false,
		"launch_state":       "not_issued",
		"deadline":           deadline,
	}
	if _, err := controller.Execute(ctx, executionID, store.Unit{
		Writes: []store.Write{{
			Table:  "actions",
			ID:     actionID,
			Insert: true,
			Values: values,
		}},
	}); err != nil {
		t.Fatalf("insert a completed earlier action for execution %q: %v", executionID, err)
	}
	return actionID
}

// newRenewalPendingPair commits two simultaneous pending renewals in one
// composition and one scan pass. Both executions hold a committed-but-
// undelivered token in app.renewal.pending, so a retry pass must isolate their
// delivery outcomes.
func newRenewalPendingPair(t *testing.T) (*renewalPendingDelivery, *renewalPendingDelivery) {
	t.Helper()
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	config := shared.config(t)
	config.RenewInterval = time.Hour
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	// Let the background loop perform its initial empty scan and fall asleep.
	time.Sleep(100 * time.Millisecond)

	firstID, firstCredential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "pair-first", 1)
	secondID, secondCredential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "pair-second", 2)
	if firstID == secondID {
		t.Fatal("the fixture started only one execution")
	}
	firstExecution := id.ExecutionID(firstID)
	secondExecution := id.ExecutionID(secondID)
	// The test drives the renewal passes explicitly, so stop the reconciler
	// from concurrently advancing either execution row under the rotate.
	freezeRenewalDispatch(t, app)
	firstBefore := renewalDurableCredential(t, shared.controller(), firstID)
	secondBefore := renewalDurableCredential(t, shared.controller(), secondID)

	// A rotation at the activation instant cannot extend the committed expiry.
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("initial renewal scan: %v", err)
	}
	if attempts := shared.runtime.totalAttempts(); attempts != 0 {
		t.Fatalf("renewal rotated %d times although it could not extend either expiry", attempts)
	}

	// Advance so both committed expiries are extendable, then fail both
	// deliveries in one pass: both commits succeed and both tokens become
	// pending.
	shared.clock.Advance(time.Hour)
	shared.runtime.failNext(firstExecution, 1)
	shared.runtime.failNext(secondExecution, 1)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with two failed deliveries: %v", err)
	}
	for _, execution := range []id.ExecutionID{firstExecution, secondExecution} {
		attempts := shared.runtime.attemptsFor(execution)
		if len(attempts) != 1 || attempts[0].delivered {
			t.Fatalf("execution %q renewal attempts = %d; want one failed delivery", execution, len(attempts))
		}
		if _, ok := app.renewal.pending[execution]; !ok {
			t.Fatalf("execution %q failed delivery was not remembered as pending", execution)
		}
	}
	firstPending := shared.runtime.attemptsFor(firstExecution)[0].request.Credential
	secondPending := shared.runtime.attemptsFor(secondExecution)[0].request.Credential
	if firstPending == firstCredential || secondPending == secondCredential {
		t.Fatal("a pending renewal delivered the unchanged credential")
	}
	if bytes.Equal(renewalDurableCredential(t, shared.controller(), firstID).Nonce, firstBefore.Nonce) {
		t.Fatal("the first failed delivery did not commit a renewal")
	}
	if bytes.Equal(renewalDurableCredential(t, shared.controller(), secondID).Nonce, secondBefore.Nonce) {
		t.Fatal("the second failed delivery did not commit a renewal")
	}

	// Allow every later delivery to succeed so a retry decision can only come
	// from durable state, never from an injected failure.
	shared.runtime.clearFailure(firstExecution)
	shared.runtime.clearFailure(secondExecution)
	return &renewalPendingDelivery{
		shared:       shared,
		app:          app,
		executionID:  firstID,
		execution:    firstExecution,
		pendingToken: firstPending,
	}, &renewalPendingDelivery{
		shared:       shared,
		app:          app,
		executionID:  secondID,
		execution:    secondExecution,
		pendingToken: secondPending,
	}
}

// TestRenewalRowHelpersRejectUnexpectedShapes pins the defensive column readers
// the renewal loop uses for durable facts. A malformed adapter row must be a
// fault, never a silently zeroed nonce, expiry, version or deadline, and a
// returned byte column must never alias the stored slice.
func TestRenewalRowHelpersRejectUnexpectedShapes(t *testing.T) {
	if _, err := rowBytes(store.Row{Values: map[string]any{"b": "not-bytes"}}, "b"); err == nil {
		t.Fatal("rowBytes accepted a non-byte column")
	}
	source := []byte{1, 2, 3}
	got, err := rowBytes(store.Row{Values: map[string]any{"b": source}}, "b")
	if err != nil {
		t.Fatalf("rowBytes: %v", err)
	}
	got[0] = 9
	if source[0] != 1 {
		t.Fatal("rowBytes aliased the stored byte column")
	}
	if value, err := rowBytes(store.Row{Values: map[string]any{}}, "b"); err != nil || value != nil {
		t.Fatalf("rowBytes absent column = (%v, %v); want (nil, nil)", value, err)
	}

	if _, err := rowTime(store.Row{Values: map[string]any{"t": "not-time"}}, "t"); err == nil {
		t.Fatal("rowTime accepted a non-time column")
	}
	if value, err := rowTime(store.Row{Values: map[string]any{}}, "t"); err != nil || !value.IsZero() {
		t.Fatalf("rowTime absent column = (%v, %v); want (zero, nil)", value, err)
	}

	cases := []struct {
		name    string
		value   any
		want    uint64
		wantErr bool
	}{
		{"uint64", uint64(7), 7, false},
		{"uint32", uint32(7), 7, false},
		{"int64", int64(7), 7, false},
		{"int", 7, 7, false},
		{"negative int64", int64(-1), 0, true},
		{"negative int", -1, 0, true},
		{"unexpected type", "7", 0, true},
	}
	for _, testCase := range cases {
		t.Run("rowUint64/"+testCase.name, func(t *testing.T) {
			got, err := rowUint64(store.Row{Values: map[string]any{"v": testCase.value}}, "v")
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("rowUint64 accepted %T", testCase.value)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("rowUint64(%T) = (%d, %v); want (%d, nil)", testCase.value, got, err, testCase.want)
			}
		})
	}
}
