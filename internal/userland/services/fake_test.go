package services_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// fakeClock is the injected time boundary. Every test controls it explicitly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func (clock *fakeClock) Set(instant time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = instant
}

// funcClock adapts a function to the injected time boundary, so a regression can
// supply a real time.Now() reading or a non-UTC location.
type funcClock func() time.Time

func (clock funcClock) Now() time.Time { return clock() }

// resolveRecord is one observed resolve invocation. rawInput is the exact byte
// payload the adapter submitted, so tests can prove redelivery identity.
type resolveRecord struct {
	operation string
	rawInput  json.RawMessage
	request   api.Request
	input     api.ExecutionRequestResolveRequest
}

// servedCodecs is the operation codec table the scripted public boundary
// decodes and encodes with. Using the real codecs makes the fake refuse any
// request that does not match the served public DTOs (including unknown
// fields), so a legacy flat inbox request cannot pass unnoticed.
var servedCodecs = func() map[string]api.OperationCodec {
	codecs := make(map[string]api.OperationCodec, len(api.OperationCodecs()))
	for _, codec := range api.OperationCodecs() {
		codecs[codec.Operation] = codec
	}
	return codecs
}()

// fakeHandler is a scripted public operation boundary. It never touches a
// process, network or database. Every request and result goes through the real
// public operation codecs, so the adapter is exercised against the served
// schema rather than a private fake vocabulary.
type fakeHandler struct {
	log *callLog

	mu            sync.Mutex
	inboxFunc     func(api.TaskInboxRequest) (api.TaskInboxResult, error)
	inspectFunc   func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error)
	resolveFunc   func(api.ExecutionRequestResolveRequest, api.Request, int) error
	inboxInputs   []api.TaskInboxRequest
	inspectInputs []api.ExecutionInspectRequest
	resolves      []resolveRecord
	// resultOperation overrides the operation identity of every encoded result
	// so a test can prove the adapter validates it. resultOperations overrides
	// the identity of one operation's result, leaving the other operations
	// intact. resultBodies replaces one operation's encoded body so a test can
	// prove the adapter strictly decodes the served result shape.
	resultOperation  string
	resultOperations map[string]string
	resultBodies     map[string]json.RawMessage
	// resolved receives one signal per accepted resolve invocation. It is a
	// barrier for the periodic Run tests, never a synchronization sleep.
	resolved chan struct{}
}

func (handler *fakeHandler) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	if err := invocation.Validate(); err != nil {
		return api.Result{}, err
	}
	codec, ok := servedCodecs[invocation.Operation]
	if !ok {
		return api.Result{}, &api.Error{Code: api.CodeInvalidInput, Message: "unknown operation " + invocation.Operation}
	}
	decoded, err := codec.DecodeRequest(invocation.Request.Input)
	if err != nil {
		return api.Result{}, err
	}
	switch input := decoded.(type) {
	case api.TaskInboxRequest:
		handler.mu.Lock()
		handler.inboxInputs = append(handler.inboxInputs, input)
		inboxFunc := handler.inboxFunc
		handler.mu.Unlock()
		cursor := ""
		if input.Page != nil {
			cursor = input.Page.Cursor
		}
		handler.log.add("inbox:" + cursor)
		if inboxFunc == nil {
			return handler.respond(invocation.Operation, api.TaskInboxResult{})
		}
		result, err := inboxFunc(input)
		if err != nil {
			return api.Result{}, err
		}
		return handler.respond(invocation.Operation, result)

	case api.ExecutionInspectRequest:
		handler.mu.Lock()
		handler.inspectInputs = append(handler.inspectInputs, input)
		inspectFunc := handler.inspectFunc
		handler.mu.Unlock()
		handler.log.add("inspect:" + input.ExecutionID)
		if inspectFunc == nil {
			return api.Result{}, &api.Error{Code: api.CodeNotFound, Message: "no current request"}
		}
		result, err := inspectFunc(input)
		if err != nil {
			return api.Result{}, err
		}
		return handler.respond(invocation.Operation, result)

	case api.ExecutionRequestResolveRequest:
		// The served edge refuses a missing idempotency key. The body is
		// action-only; the shared envelope expected_version remains the
		// expected execution version and is not repeated in the body.
		if invocation.Request.IdempotencyKey == "" {
			return api.Result{}, &api.Error{Code: api.CodeInvalidInput, Message: "idempotency key is required"}
		}
		record := resolveRecord{
			operation: invocation.Operation,
			rawInput:  append(json.RawMessage(nil), invocation.Request.Input...),
			request:   invocation.Request,
			input:     input,
		}
		handler.mu.Lock()
		index := len(handler.resolves)
		handler.resolves = append(handler.resolves, record)
		resolveFunc := handler.resolveFunc
		resolved := handler.resolved
		handler.mu.Unlock()
		handler.log.add("resolve:" + invocation.Request.IdempotencyKey)
		var failure error
		if resolveFunc != nil {
			failure = resolveFunc(input, invocation.Request, index)
		}
		if resolved != nil {
			select {
			case resolved <- struct{}{}:
			default:
			}
		}
		if failure != nil {
			return api.Result{}, failure
		}
		executionVersion := uint64(1)
		if invocation.Request.ExpectedVersion != nil {
			executionVersion = *invocation.Request.ExpectedVersion + 1
		}
		return handler.respond(invocation.Operation, api.ExecutionRequestResolveResult{
			ExecutionVersion: executionVersion,
			ActionVersion:    input.ExpectedActionVersion + 1,
		})
	}
	return api.Result{}, &api.Error{Code: api.CodeInvalidInput, Message: "unsupported operation " + invocation.Operation}
}

// respond encodes one result through its operation codec, optionally replacing
// the operation identity so identity validation is testable.
func (handler *fakeHandler) respond(operation string, value any) (api.Result, error) {
	codec, ok := servedCodecs[operation]
	if !ok {
		return api.Result{}, &api.Error{Code: api.CodeInvalidInput, Message: "unknown operation " + operation}
	}
	encoded, err := codec.EncodeResult(value)
	if err != nil {
		return api.Result{}, err
	}
	handler.mu.Lock()
	identity := handler.resultOperation
	if override, ok := handler.resultOperations[operation]; ok {
		identity = override
	}
	body := encoded
	if replacement, ok := handler.resultBodies[operation]; ok {
		body = append(json.RawMessage(nil), replacement...)
	}
	handler.mu.Unlock()
	if identity == "" {
		identity = operation
	}
	return api.Result{Operation: identity, Body: body}, nil
}

func (handler *fakeHandler) inboxCalls() []api.TaskInboxRequest {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]api.TaskInboxRequest(nil), handler.inboxInputs...)
}

func (handler *fakeHandler) inspectCalls() []api.ExecutionInspectRequest {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]api.ExecutionInspectRequest(nil), handler.inspectInputs...)
}

func (handler *fakeHandler) resolvesSnapshot() []resolveRecord {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]resolveRecord(nil), handler.resolves...)
}

func (handler *fakeHandler) resolveCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return len(handler.resolves)
}

// fakeEventSource serves retained notifications page by page. It may repeat or
// reorder pages; the adapter must not depend on delivery order.
type fakeEventSource struct {
	mu      sync.Mutex
	pages   map[string]services.NotificationPage
	cursors []string
	err     error
}

func (source *fakeEventSource) Poll(_ context.Context, cursor string) (services.NotificationPage, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.cursors = append(source.cursors, cursor)
	if source.err != nil {
		return services.NotificationPage{}, source.err
	}
	return source.pages[cursor], nil
}

func (source *fakeEventSource) polledCursors() []string {
	source.mu.Lock()
	defer source.mu.Unlock()
	return append([]string(nil), source.cursors...)
}

// fakeMergeEffects is the scripted merge/rebase boundary.
type fakeMergeEffects struct {
	log *callLog

	mu           sync.Mutex
	executeFunc  func(services.MergeOperation) (services.MergeObservation, error)
	observeFunc  func(services.MergeOperation) (services.MergeObservation, error)
	executions   []services.MergeOperation
	observations []services.MergeOperation
}

func (effects *fakeMergeEffects) Execute(_ context.Context, operation services.MergeOperation) (services.MergeObservation, error) {
	effects.mu.Lock()
	effects.executions = append(effects.executions, operation)
	executeFunc := effects.executeFunc
	effects.mu.Unlock()
	effects.log.add("execute:" + string(operation.OperationID))
	if executeFunc != nil {
		return executeFunc(operation)
	}
	return services.MergeObservation{State: services.EffectApplied, Base: operation.Base, Candidate: operation.Candidate}, nil
}

func (effects *fakeMergeEffects) Observe(_ context.Context, operation services.MergeOperation) (services.MergeObservation, error) {
	effects.mu.Lock()
	effects.observations = append(effects.observations, operation)
	observeFunc := effects.observeFunc
	effects.mu.Unlock()
	effects.log.add("observe:" + string(operation.OperationID))
	if observeFunc != nil {
		return observeFunc(operation)
	}
	return services.MergeObservation{State: services.EffectNotStarted}, nil
}

func (effects *fakeMergeEffects) executeCount() int {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	return len(effects.executions)
}

func (effects *fakeMergeEffects) observeCount() int {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	return len(effects.observations)
}

func (effects *fakeMergeEffects) executedOperations() []services.MergeOperation {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	return append([]services.MergeOperation(nil), effects.executions...)
}

// fakeCoordinationStore is an atomic in-memory implementation of the pinned
// durable-store semantics. It also injects persistence failures and records the
// transition order so tests can prove intent-before-effect and
// observation-before-return.
type fakeCoordinationStore struct {
	log *callLog

	mu         sync.Mutex
	records    map[string]services.CoordinationRecord
	creates    int
	failCreate error
	failState  services.EffectState
	failErr    error
}

func newFakeCoordinationStore(log *callLog) *fakeCoordinationStore {
	return &fakeCoordinationStore{log: log, records: make(map[string]services.CoordinationRecord)}
}

func coordinationSlot(tenant id.TenantID, key services.MergeKey) string {
	return string(tenant) + "\x00" + key.Repository + "\x00" + key.Ref
}

func (store *fakeCoordinationStore) Load(_ context.Context, tenant id.TenantID, key services.MergeKey) (services.CoordinationRecord, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return services.CoordinationRecord{}, false, err
	}
	if err := key.Validate(); err != nil {
		return services.CoordinationRecord{}, false, err
	}
	record, ok := store.records[coordinationSlot(tenant, key)]
	return record, ok, nil
}

func (store *fakeCoordinationStore) CreateClaim(_ context.Context, tenant id.TenantID, key services.MergeKey, owner string, operationID id.OperationID, expiresAt, now time.Time) (services.CoordinationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return services.CoordinationRecord{}, err
	}
	if err := key.Validate(); err != nil {
		return services.CoordinationRecord{}, err
	}
	if owner == "" {
		return services.CoordinationRecord{}, services.ErrClaimed
	}
	if store.failCreate != nil {
		return services.CoordinationRecord{}, store.failCreate
	}
	slot := coordinationSlot(tenant, key)
	previous := store.records[slot]
	if previous.TenantID != "" && !previous.Released && !previous.Expired(now) {
		return services.CoordinationRecord{}, services.ErrClaimed
	}
	next := services.CoordinationRecord{
		Key:         key,
		Owner:       owner,
		OperationID: operationID,
		Generation:  previous.Claim.Generation + 1,
		ExpiresAt:   expiresAt,
		TenantID:    tenant,
		State:       services.EffectNotStarted,
		UpdatedAt:   now,
	}
	if unsettledIntent(previous) {
		next.Claim.OperationID = previous.Claim.OperationID
		next.State = previous.State
		next.Request = previous.Request
		next.Result = previous.Result
		next.Reconciled = previous.Reconciled
	}
	store.records[slot] = next
	store.creates++
	store.log.add("create-claim")
	return next, nil
}

func (store *fakeCoordinationStore) Transition(_ context.Context, tenant id.TenantID, expected, next services.CoordinationRecord) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if next.TenantID != tenant ||
		next.Claim.Key != expected.Claim.Key ||
		next.Claim.Owner != expected.Claim.Owner ||
		next.Claim.OperationID != expected.Claim.OperationID ||
		next.Claim.Generation != expected.Claim.Generation {
		return services.ErrStaleClaim
	}
	slot := coordinationSlot(tenant, expected.Claim.Key)
	stored, ok := store.records[slot]
	if !ok || !fakeRecordsEqual(stored, expected) {
		return services.ErrStaleClaim
	}
	if store.failErr != nil && next.State == store.failState {
		store.log.add("transition-failed:" + string(next.State))
		return store.failErr
	}
	store.records[slot] = next
	store.log.add("transition:" + string(next.State))
	return nil
}

func (store *fakeCoordinationStore) record(t *testing.T, tenant id.TenantID, key services.MergeKey) (services.CoordinationRecord, bool) {
	t.Helper()
	record, ok, err := store.Load(context.Background(), tenant, key)
	if err != nil {
		t.Fatalf("fake store Load: %v", err)
	}
	return record, ok
}

// failPersisting makes every transition into state fail with err.
func (store *fakeCoordinationStore) failPersisting(state services.EffectState, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failState = state
	store.failErr = err
}

func (store *fakeCoordinationStore) clearFailures() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failState = ""
	store.failErr = nil
	store.failCreate = nil
}

func (store *fakeCoordinationStore) createCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.creates
}

// faultingStore delegates to a real store and injects a persistence failure for
// one target state. It models a process that observes an effect but cannot
// durably record the observation.
type faultingStore struct {
	inner services.CoordinationStore

	mu        sync.Mutex
	failState services.EffectState
	failErr   error
}

func (store *faultingStore) Load(ctx context.Context, tenant id.TenantID, key services.MergeKey) (services.CoordinationRecord, bool, error) {
	return store.inner.Load(ctx, tenant, key)
}

func (store *faultingStore) CreateClaim(ctx context.Context, tenant id.TenantID, key services.MergeKey, owner string, operationID id.OperationID, expiresAt, now time.Time) (services.CoordinationRecord, error) {
	return store.inner.CreateClaim(ctx, tenant, key, owner, operationID, expiresAt, now)
}

func (store *faultingStore) Transition(ctx context.Context, tenant id.TenantID, expected, next services.CoordinationRecord) error {
	store.mu.Lock()
	failState, failErr := store.failState, store.failErr
	store.mu.Unlock()
	if failErr != nil && next.State == failState {
		return failErr
	}
	return store.inner.Transition(ctx, tenant, expected, next)
}

// fakeRecordsEqual mirrors the durable store's instant-based comparison so the
// fake never rejects a record only because of a monotonic reading or location.
func fakeRecordsEqual(first, second services.CoordinationRecord) bool {
	return first.Key == second.Key &&
		first.Owner == second.Owner &&
		first.OperationID == second.OperationID &&
		first.Generation == second.Generation &&
		first.ExpiresAt.Equal(second.ExpiresAt) &&
		first.TenantID == second.TenantID &&
		first.State == second.State &&
		first.Request == second.Request &&
		first.Result == second.Result &&
		first.UpdatedAt.Equal(second.UpdatedAt) &&
		first.Released == second.Released &&
		first.Reconciled == second.Reconciled
}

// unsettledIntent reports whether a stored record holds an assignment that may
// have reached the outside world and must therefore keep its operation identity
// and result until it is settled.
func unsettledIntent(record services.CoordinationRecord) bool {
	if record.TenantID == "" || record.Released {
		return false
	}
	if record.State == services.EffectUncertain || record.State == services.EffectApplied {
		return true
	}
	return record.State == services.EffectNotStarted && record.Request.Key != (services.MergeKey{})
}

// Scan enumerates this tenant's pending and settled records, mirroring the file
// store's optional enumeration capability.
func (store *fakeCoordinationStore) Scan(_ context.Context, tenant id.TenantID) ([]services.CoordinationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	records := make([]services.CoordinationRecord, 0, len(store.records))
	for _, record := range store.records {
		if record.TenantID == tenant {
			records = append(records, record)
		}
	}
	return records, nil
}
