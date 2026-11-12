package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// adapterFixture wires a deterministic adapter around a scripted served public
// boundary and event source. The boundary decodes and encodes with the real
// operation codecs, so the fixture cannot accidentally accept a private wire
// vocabulary.
type adapterFixture struct {
	adapter *services.Adapter
	handler *fakeHandler
	events  *fakeEventSource
	clock   *fakeClock
	log     *callLog
}

func newAdapterFixture(t *testing.T, kind services.ServiceKind, requests ...servedRequest) *adapterFixture {
	t.Helper()
	return newAdapterFixtureForService(t, kind, testService, requests...)
}

// newAdapterFixtureForService is newAdapterFixture with an explicit service
// identity, so a scope test can use a responder service and its near-misses.
func newAdapterFixtureForService(t *testing.T, kind services.ServiceKind, serviceID id.PrincipalID, requests ...servedRequest) *adapterFixture {
	t.Helper()
	log := &callLog{}
	handler := &fakeHandler{log: log}
	handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		items := make([]api.InboxItem, 0, len(requests))
		for _, request := range requests {
			items = append(items, request.inboxItem())
		}
		return api.TaskInboxResult{Items: items}, nil
	}
	handler.inspectFunc = func(input api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		for _, request := range requests {
			if request.executionID == input.ExecutionID {
				return request.inspectResult(), nil
			}
		}
		return api.ExecutionInspectResult{}, &api.Error{Code: api.CodeNotFound, Message: "no current request"}
	}
	events := &fakeEventSource{}
	clock := &fakeClock{now: testNow}
	config := validAdapterConfig(handler, events, clock)
	config.ServiceID = serviceID
	config.Kind = kind
	if kind == services.KindTimer {
		config.Schedule = time.Minute
	}
	adapter, err := services.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return &adapterFixture{adapter: adapter, handler: handler, events: events, clock: clock, log: log}
}

func (fixture *adapterFixture) run(t *testing.T) error {
	t.Helper()
	return fixture.adapter.RunOnce(context.Background())
}

func inboxCursor(input api.TaskInboxRequest) string {
	if input.Page == nil {
		return ""
	}
	return input.Page.Cursor
}

// waitForResolves is a channel barrier: it fails the test if the expected
// number of resolve invocations does not arrive within the timeout. It never
// sleeps to synchronize.
func waitForResolves(t *testing.T, handler *fakeHandler, count int) {
	t.Helper()
	for index := range count {
		select {
		case <-handler.resolved:
		case <-time.After(2 * time.Second):
			t.Fatalf("observed %d/%d resolve invocations before the barrier timed out", index, count)
		}
	}
}

// runBounded runs one adapter pass under a deadline. A cursor cycle that never
// terminates fails the pass instead of hanging the test.
func runBounded(t *testing.T, adapter *services.Adapter) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- adapter.RunOnce(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("RunOnce did not terminate before the deadline: %w", ctx.Err())
	}
}

// TestServedBoundaryAcceptsServedDTOs is fixture sanity evidence: the scripted
// boundary accepts a correctly shaped served task.inbox request and returns a
// result the real codec decodes. It proves the red adapter tests fail on the
// missing implementation, not on a broken fake.
func TestServedBoundaryAcceptsServedDTOs(t *testing.T) {
	request := validServedRequest()
	handler := &fakeHandler{log: &callLog{}}
	handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{Items: []api.InboxItem{request.inboxItem()}}, nil
	}
	input, err := json.Marshal(api.TaskInboxRequest{
		ProjectID: string(testProject),
		Kinds:     []string{"request"},
		Page:      &api.Page{Limit: api.DefaultPageLimit},
	})
	if err != nil {
		t.Fatalf("marshal served inbox request: %v", err)
	}
	result, err := handler.Invoke(context.Background(), api.Invocation{
		Operation: services.OperationInbox,
		Request:   api.Request{Input: input},
	})
	if err != nil {
		t.Fatalf("served boundary refused a valid task.inbox request: %v", err)
	}
	codec, ok := servedCodecs[services.OperationInbox]
	if !ok {
		t.Fatal("task.inbox codec is missing")
	}
	decoded, err := codec.DecodeResult(result.Body)
	if err != nil {
		t.Fatalf("decode served inbox result: %v", err)
	}
	inbox, ok := decoded.(api.TaskInboxResult)
	if !ok {
		t.Fatalf("decoded inbox result has type %T; want api.TaskInboxResult", decoded)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].ExecutionID != request.executionID {
		t.Fatalf("served inbox items = %+v; want the fixture request", inbox.Items)
	}
}

func TestAdapterStartupResolvesRetainedRequest(t *testing.T) {
	request := validServedRequest()
	fixture := newAdapterFixture(t, services.KindEmail, request)

	// No notification at all: the startup current-request scan must still find
	// the retained request through the served task.inbox operation.
	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want 1", len(resolves))
	}
	resolve := resolves[0]
	if resolve.operation != services.OperationResolve {
		t.Fatalf("operation = %q; want %q", resolve.operation, services.OperationResolve)
	}
	if resolve.request.ExpectedVersion == nil || *resolve.request.ExpectedVersion != request.executionVersion {
		t.Fatalf("envelope expected version = %v; want the execution version %d", resolve.request.ExpectedVersion, request.executionVersion)
	}
	if resolve.input.ExecutionID != request.executionID || resolve.input.ActionID != request.actionID {
		t.Fatalf("resolve identities = %+v; want the scanned request", resolve.input)
	}
	if resolve.input.ExpectedActionVersion != request.actionVersion {
		t.Fatalf("resolve action version = %d; want action %d",
			resolve.input.ExpectedActionVersion, request.actionVersion)
	}
	if resolve.input.Decision != services.DecisionPass {
		t.Fatalf("decision = %q; want %q", resolve.input.Decision, services.DecisionPass)
	}
	// The served inbox declares output names, but this adapter owns no response
	// policy: it never copies a payload out of inbox metadata and submits an
	// empty output map until an explicit service-supplied response is specified.
	if len(resolve.input.Outputs) != 0 {
		t.Fatalf("outputs = %v; want no automatic response outputs", resolve.input.Outputs)
	}
	assertLegalIdempotencyKey(t, resolve.request.IdempotencyKey, *resolve.request.ExpectedVersion)

	inbox := fixture.handler.inboxCalls()
	if len(inbox) != 1 {
		t.Fatalf("inbox calls = %d; want 1", len(inbox))
	}
	if inbox[0].ProjectID != string(testProject) {
		t.Fatalf("inbox project = %q; want %q", inbox[0].ProjectID, testProject)
	}
	if !reflect.DeepEqual(inbox[0].Kinds, []string{"request"}) {
		t.Fatalf("inbox kinds = %v; want [request]", inbox[0].Kinds)
	}
	if inbox[0].Page == nil || inbox[0].Page.Limit <= 0 {
		t.Fatalf("inbox page = %+v; want a positive page limit", inbox[0].Page)
	}
	if cursors := fixture.events.polledCursors(); len(cursors) == 0 || cursors[0] != "" {
		t.Fatalf("event poll cursors = %v; want a poll from the empty cursor", cursors)
	}
	inspects := fixture.handler.inspectCalls()
	if len(inspects) != 1 {
		t.Fatalf("inspect calls = %d; want 1", len(inspects))
	}
	if inspects[0].ExecutionID != request.executionID {
		t.Fatalf("inspect input = %+v; want the scanned execution identity", inspects[0])
	}
}

// TestAdapterStopsRepeatingInboxCursors proves a repeated page cursor bounds the
// scan: the pass terminates under a deadline and every candidate is answered
// exactly once.
func TestAdapterStopsRepeatingInboxCursors(t *testing.T) {
	t.Run("cursor repeats immediately", func(t *testing.T) {
		first := validServedRequest()
		first.executionID = "exec-1"
		first.actionID = "action-1"
		second := validServedRequest()
		second.executionID = "exec-2"
		second.actionID = "action-2"

		fixture := newAdapterFixture(t, services.KindEmail, first, second)
		calls := 0
		fixture.handler.inboxFunc = func(input api.TaskInboxRequest) (api.TaskInboxResult, error) {
			calls++
			if calls > 10 {
				return api.TaskInboxResult{}, fmt.Errorf("the scan did not stop a repeating cursor")
			}
			switch inboxCursor(input) {
			case "":
				return api.TaskInboxResult{Items: []api.InboxItem{first.inboxItem()}, NextCursor: "page-a"}, nil
			case "page-a":
				return api.TaskInboxResult{Items: []api.InboxItem{second.inboxItem()}, NextCursor: "page-a"}, nil
			}
			return api.TaskInboxResult{}, fmt.Errorf("unexpected cursor %q", inboxCursor(input))
		}

		if err := runBounded(t, fixture.adapter); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		assertResolvedOnce(t, fixture.handler, first.actionID, second.actionID)
	})

	t.Run("cursor cycles a b a", func(t *testing.T) {
		first := validServedRequest()
		first.executionID = "exec-1"
		first.actionID = "action-1"
		second := validServedRequest()
		second.executionID = "exec-2"
		second.actionID = "action-2"
		third := validServedRequest()
		third.executionID = "exec-3"
		third.actionID = "action-3"

		fixture := newAdapterFixture(t, services.KindEmail, first, second, third)
		calls := 0
		fixture.handler.inboxFunc = func(input api.TaskInboxRequest) (api.TaskInboxResult, error) {
			calls++
			if calls > 10 {
				return api.TaskInboxResult{}, fmt.Errorf("the scan did not stop a cursor cycle")
			}
			switch inboxCursor(input) {
			case "":
				return api.TaskInboxResult{Items: []api.InboxItem{first.inboxItem()}, NextCursor: "page-a"}, nil
			case "page-a":
				return api.TaskInboxResult{Items: []api.InboxItem{second.inboxItem()}, NextCursor: "page-b"}, nil
			case "page-b":
				return api.TaskInboxResult{Items: []api.InboxItem{third.inboxItem()}, NextCursor: "page-a"}, nil
			}
			return api.TaskInboxResult{}, fmt.Errorf("unexpected cursor %q", inboxCursor(input))
		}

		if err := runBounded(t, fixture.adapter); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		assertResolvedOnce(t, fixture.handler, first.actionID, second.actionID, third.actionID)
	})
}

// assertResolvedOnce proves every named action was answered exactly once.
func assertResolvedOnce(t *testing.T, handler *fakeHandler, actionIDs ...string) {
	t.Helper()
	resolves := handler.resolvesSnapshot()
	if len(resolves) != len(actionIDs) {
		t.Fatalf("resolve calls = %d; want %d", len(resolves), len(actionIDs))
	}
	counts := make(map[string]int, len(actionIDs))
	for _, resolve := range resolves {
		counts[resolve.input.ActionID]++
	}
	for _, actionID := range actionIDs {
		if counts[actionID] != 1 {
			t.Fatalf("action %q resolved %d times; want exactly once (all: %v)", actionID, counts[actionID], counts)
		}
	}
}

func TestAdapterFollowsInboxCursorAcrossPages(t *testing.T) {
	first := validServedRequest()
	second := validServedRequest()
	second.executionID = "exec-2"
	second.actionID = "action-2"

	fixture := newAdapterFixture(t, services.KindEmail, first, second)
	fixture.handler.inboxFunc = func(input api.TaskInboxRequest) (api.TaskInboxResult, error) {
		switch inboxCursor(input) {
		case "":
			return api.TaskInboxResult{Items: []api.InboxItem{first.inboxItem()}, NextCursor: "page-2"}, nil
		case "page-2":
			return api.TaskInboxResult{Items: []api.InboxItem{second.inboxItem()}}, nil
		}
		return api.TaskInboxResult{}, fmt.Errorf("unexpected cursor %q", inboxCursor(input))
	}

	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 2 {
		t.Fatalf("resolve calls = %d; want 2", len(resolves))
	}
	if resolves[0].input.ActionID != first.actionID || resolves[1].input.ActionID != second.actionID {
		t.Fatalf("resolve order = %q,%q; want %q,%q",
			resolves[0].input.ActionID, resolves[1].input.ActionID, first.actionID, second.actionID)
	}
	inbox := fixture.handler.inboxCalls()
	if len(inbox) != 2 || inboxCursor(inbox[0]) != "" || inboxCursor(inbox[1]) != "page-2" {
		t.Fatalf("inbox cursors = %+v; want [\"\" \"page-2\"]", inbox)
	}
	if inbox[0].Page == nil || inbox[0].Page.Limit <= 0 || inbox[1].Page == nil || inbox[1].Page.Limit <= 0 {
		t.Fatalf("inbox pages = %+v,%+v; want positive limits", inbox[0].Page, inbox[1].Page)
	}
}

func TestAdapterIgnoresForeignNotificationsAndAssignments(t *testing.T) {
	ours := validServedRequest()
	fixture := newAdapterFixture(t, services.KindEmail, ours)
	fixture.events.pages = map[string]services.NotificationPage{
		"": {
			Notifications: []services.Notification{
				{EventID: "event-tenant-b", Type: "assignment", TenantID: "tenant-b", ProjectID: testProject, ServiceID: testService, At: testNow},
				{EventID: "event-webhook", Type: "assignment", TenantID: testTenant, ProjectID: testProject, ServiceID: "webhook-svc", At: testNow},
				{EventID: "event-ours", Type: "assignment", TenantID: testTenant, ProjectID: testProject, ServiceID: testService, At: testNow},
			},
			Next: api.PageInfo{NextCursor: "page-2"},
		},
		"page-2": {
			Notifications: []services.Notification{
				{EventID: "event-project-b", Type: "assignment", TenantID: testTenant, ProjectID: "project-b", ServiceID: testService, At: testNow},
			},
		},
	}

	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Notifications are hints only: they never cause a resolve. The single
	// served inbox item is the sole authority for the answer.
	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want exactly the adapter's own served assignment", len(resolves))
	}
	if resolves[0].input.ActionID != ours.actionID {
		t.Fatalf("resolved action = %q; want %q", resolves[0].input.ActionID, ours.actionID)
	}
	if inspects := fixture.handler.inspectCalls(); len(inspects) != 1 {
		t.Fatalf("inspect calls = %d; want 1", len(inspects))
	}
	cursors := fixture.events.polledCursors()
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "page-2" {
		t.Fatalf("event poll cursors = %v; want [\"\" \"page-2\"]", cursors)
	}
}

func TestAdapterRevalidatesCurrentRequestVersion(t *testing.T) {
	t.Run("current versions used", func(t *testing.T) {
		scanned := validServedRequest()
		scanned.executionVersion = 2
		scanned.actionVersion = 1
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		current := scanned
		current.executionVersion = 5
		current.actionVersion = 3
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		resolves := fixture.handler.resolvesSnapshot()
		if len(resolves) != 1 {
			t.Fatalf("resolves = %+v; want one resolve", resolves)
		}
		if resolves[0].input.ExpectedActionVersion != 3 {
			t.Fatalf("resolve action version = %d; want action 3", resolves[0].input.ExpectedActionVersion)
		}
		if resolves[0].request.ExpectedVersion == nil || *resolves[0].request.ExpectedVersion != 5 {
			t.Fatalf("envelope expected version = %v; want the current execution version 5", resolves[0].request.ExpectedVersion)
		}
	})

	t.Run("lower current action version refused", func(t *testing.T) {
		scanned := validServedRequest()
		scanned.actionVersion = 2
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		current := scanned
		current.actionVersion = 1
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); !errors.Is(err, services.ErrStaleVersion) {
			t.Fatalf("RunOnce error = %v; want ErrStaleVersion", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a stale current action version was resolved")
		}
	})

	t.Run("not found skips", func(t *testing.T) {
		scanned := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return api.ExecutionInspectResult{}, &api.Error{Code: api.CodeNotFound, Message: "no current request"}
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a vanished request was resolved")
		}
	})

	t.Run("unrelated action id skips", func(t *testing.T) {
		scanned := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		current := scanned
		current.actionID = "action-other"
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a request whose action id is unrelated was resolved")
		}
	})

	t.Run("action id prefix extension skips", func(t *testing.T) {
		scanned := validServedRequest()
		scanned.actionID = "action-1"
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		current := scanned
		current.actionID = "action-10"
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a prefix-extension action id was treated as the scanned action")
		}
	})

	t.Run("different execution id skips", func(t *testing.T) {
		scanned := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		current := scanned
		current.executionID = "exec-2"
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a request whose inspected execution id differs was resolved")
		}
	})

	t.Run("running action with a decision skips", func(t *testing.T) {
		scanned := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		answeredAt := testNow.Add(time.Second)
		current := scanned
		current.state = "running"
		current.decision = services.DecisionPass
		current.actor = string(testService)
		current.answeredAt = &answeredAt
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("a running action that already carried a decision was answered again")
		}
	})

	for _, state := range []string{"failed", "cancelled"} {
		t.Run(state+" action without a decision skips", func(t *testing.T) {
			scanned := validServedRequest()
			fixture := newAdapterFixture(t, services.KindEmail, scanned)
			current := scanned
			current.state = state
			fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return current.inspectResult(), nil
			}
			if err := fixture.run(t); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fixture.handler.resolveCount() != 0 {
				t.Fatalf("a %s action without a decision was resolved", state)
			}
		})
	}

	t.Run("settled request skips", func(t *testing.T) {
		scanned := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, scanned)
		answeredAt := testNow.Add(time.Second)
		current := scanned
		current.state = "succeeded"
		current.decision = services.DecisionPass
		current.actor = string(testService)
		current.answeredAt = &answeredAt
		current.executionVersion = 4
		current.actionVersion = 2
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 0 {
			t.Fatal("an already-answered request was answered again")
		}
	})
}

func TestAdapterIdempotencyKeyIsStableAcrossRedelivery(t *testing.T) {
	for _, kind := range []services.ServiceKind{services.KindEmail, services.KindWebhook, services.KindResponder} {
		t.Run(string(kind)+" served scope", func(t *testing.T) {
			request := validServedRequest()
			fixture := newAdapterFixture(t, kind, request)

			if err := fixture.run(t); err != nil {
				t.Fatalf("first RunOnce: %v", err)
			}
			if err := fixture.run(t); err != nil {
				t.Fatalf("second RunOnce: %v", err)
			}

			resolves := fixture.handler.resolvesSnapshot()
			if len(resolves) != 2 {
				t.Fatalf("resolve calls = %d; want 2", len(resolves))
			}
			if resolves[0].request.IdempotencyKey != resolves[1].request.IdempotencyKey {
				t.Fatalf("redelivery keys = %q,%q; want a stable key",
					resolves[0].request.IdempotencyKey, resolves[1].request.IdempotencyKey)
			}
			if string(resolves[0].rawInput) != string(resolves[1].rawInput) {
				t.Fatalf("redelivered resolve inputs differ:\n%s\n%s", resolves[0].rawInput, resolves[1].rawInput)
			}
			if resolves[0].operation != services.OperationResolve {
				t.Fatalf("operation = %q; want %q", resolves[0].operation, services.OperationResolve)
			}
			assertLegalIdempotencyKey(t, resolves[0].request.IdempotencyKey, *resolves[0].request.ExpectedVersion)
		})
	}

	t.Run("newer current action version changes the key", func(t *testing.T) {
		request := validServedRequest()
		fixture := newAdapterFixture(t, services.KindEmail, request)

		if err := fixture.run(t); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		first := fixture.handler.resolvesSnapshot()
		if len(first) != 1 {
			t.Fatalf("resolve calls = %d; want 1", len(first))
		}
		key := first[0].request.IdempotencyKey

		current := request
		current.actionVersion = 2
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return current.inspectResult(), nil
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		second := fixture.handler.resolvesSnapshot()
		if len(second) != 2 {
			t.Fatalf("resolve calls = %d; want 2", len(second))
		}
		if second[1].request.IdempotencyKey == key {
			t.Fatalf("action version 2 reused the version 1 key %q", key)
		}
		if second[1].input.ExpectedActionVersion != 2 {
			t.Fatalf("action version 2 resolve expected %d; want 2", second[1].input.ExpectedActionVersion)
		}
	})

	t.Run("timer schedule window", func(t *testing.T) {
		first := validServedRequest()
		second := validServedRequest()
		second.executionID = "exec-2"
		second.actionID = "action-2"
		fixture := newAdapterFixture(t, services.KindTimer, first, second)

		keysByAction := func(resolves []resolveRecord) map[id.ActionID]string {
			t.Helper()
			keys := make(map[id.ActionID]string, len(resolves))
			for _, resolve := range resolves {
				keys[id.ActionID(resolve.input.ActionID)] = resolve.request.IdempotencyKey
			}
			return keys
		}

		if err := fixture.run(t); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		resolves := fixture.handler.resolvesSnapshot()
		if len(resolves) != 2 {
			t.Fatalf("resolve calls = %d; want both timer assignments", len(resolves))
		}
		firstWindow := keysByAction(resolves)
		if firstWindow[id.ActionID(first.actionID)] == firstWindow[id.ActionID(second.actionID)] {
			t.Fatalf("two timer assignments in one window shared key %q", firstWindow[id.ActionID(first.actionID)])
		}
		for action, key := range firstWindow {
			assertLegalIdempotencyKey(t, key, 1)
			if key == "" {
				t.Fatalf("timer assignment %q produced an empty key", action)
			}
		}

		// Redelivery in the same window reuses each assignment's own key.
		fixture.clock.Advance(20 * time.Second)
		if err := fixture.run(t); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		resolves = fixture.handler.resolvesSnapshot()
		if len(resolves) != 4 {
			t.Fatalf("resolve calls = %d; want the same-window redelivery", len(resolves))
		}
		for _, resolve := range resolves[2:] {
			if resolve.request.IdempotencyKey != firstWindow[id.ActionID(resolve.input.ActionID)] {
				t.Fatalf("same-window key = %q; want the stable %q",
					resolve.request.IdempotencyKey, firstWindow[id.ActionID(resolve.input.ActionID)])
			}
		}

		// The next window changes both keys.
		fixture.clock.Advance(60 * time.Second)
		if err := fixture.run(t); err != nil {
			t.Fatalf("third RunOnce: %v", err)
		}
		resolves = fixture.handler.resolvesSnapshot()
		if len(resolves) != 6 {
			t.Fatalf("resolve calls = %d; want the next-window resolves", len(resolves))
		}
		for _, resolve := range resolves[4:] {
			if resolve.request.IdempotencyKey == firstWindow[id.ActionID(resolve.input.ActionID)] {
				t.Fatalf("the next timer window reused key %q", resolve.request.IdempotencyKey)
			}
		}
	})
}

// TestAdapterIdempotencyKeyScopesEveryIdentityAndVersion proves the derived key
// separates every kind's action identity, execution identity and both observed
// versions. Each pair differs in exactly one scope term.
func TestAdapterIdempotencyKeyScopesEveryIdentityAndVersion(t *testing.T) {
	for _, kind := range []services.ServiceKind{services.KindEmail, services.KindWebhook, services.KindResponder, services.KindTimer} {
		t.Run(string(kind), func(t *testing.T) {
			base := validServedRequest()

			sameExecutionOtherAction := base
			sameExecutionOtherAction.actionID = "action-2"

			otherExecutionSameAction := base
			otherExecutionSameAction.executionID = "exec-2"

			newExecutionVersion := base
			newExecutionVersion.executionVersion = 9

			newActionVersion := base
			newActionVersion.actionVersion = 9

			cases := []struct {
				name    string
				scanned servedRequest
				current servedRequest
			}{
				{name: "base", scanned: base, current: base},
				{name: "same execution different action", scanned: sameExecutionOtherAction, current: sameExecutionOtherAction},
				{name: "different execution same action", scanned: otherExecutionSameAction, current: otherExecutionSameAction},
				{name: "execution version change", scanned: base, current: newExecutionVersion},
				{name: "action version change", scanned: base, current: newActionVersion},
			}

			keys := make(map[string]string, len(cases))
			for _, testCase := range cases {
				keys[testCase.name] = captureResolveKey(t, kind, testCase.scanned, testCase.current)
			}
			names := make([]string, 0, len(cases))
			for _, testCase := range cases {
				names = append(names, testCase.name)
			}
			for i, firstName := range names {
				if keys[firstName] == "" {
					t.Fatalf("%s produced an empty key", firstName)
				}
				for _, secondName := range names[i+1:] {
					if keys[firstName] == keys[secondName] {
						t.Fatalf("%q and %q shared idempotency key %q", firstName, secondName, keys[firstName])
					}
				}
			}
		})
	}
}

// captureResolveKey runs one adapter pass for a single scanned request and
// returns the idempotency key it submitted.
func captureResolveKey(t *testing.T, kind services.ServiceKind, scanned, current servedRequest) string {
	t.Helper()
	fixture := newAdapterFixture(t, kind, scanned)
	fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return current.inspectResult(), nil
	}
	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want 1 for %s/%s", len(resolves), scanned.executionID, scanned.actionID)
	}
	return resolves[0].request.IdempotencyKey
}

func TestAdapterIdempotencyKeyBoundedForLongScope(t *testing.T) {
	long := strings.Repeat("a", 120)
	request := validServedRequest()
	request.executionID = long
	request.actionID = long

	handler := &fakeHandler{log: &callLog{}}
	handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{Items: []api.InboxItem{request.inboxItem()}}, nil
	}
	handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return request.inspectResult(), nil
	}
	config := validAdapterConfig(handler, &fakeEventSource{}, &fakeClock{now: testNow})
	config.TenantID = id.TenantID(long)
	config.ProjectID = id.ProjectID(long)
	adapter, err := services.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if err := adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	resolves := handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want 1", len(resolves))
	}
	key := resolves[0].request.IdempotencyKey
	if len(key) > api.MaxIdempotencyKeyBytes {
		t.Fatalf("idempotency key is %d bytes; api allows %d", len(key), api.MaxIdempotencyKeyBytes)
	}
	assertLegalIdempotencyKey(t, key, *resolves[0].request.ExpectedVersion)
}

func TestAdapterMapsPublicRefusals(t *testing.T) {
	boom := errors.New("transient dependency failure")
	for name, resolveErr := range map[string]error{
		"version conflict": &api.Error{Code: api.CodeVersionConflict, Message: "stale version"},
		"changed content":  &api.Error{Code: api.CodeChangedIdempotencyContent, Message: "content changed"},
		"transient":        boom,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newAdapterFixture(t, services.KindEmail, validServedRequest())
			fixture.handler.resolveFunc = func(api.ExecutionRequestResolveRequest, api.Request, int) error { return resolveErr }
			err := fixture.run(t)
			switch name {
			case "version conflict":
				if !errors.Is(err, services.ErrStaleVersion) {
					t.Fatalf("error = %v; want ErrStaleVersion", err)
				}
			case "changed content":
				if !errors.Is(err, services.ErrChangedContent) {
					t.Fatalf("error = %v; want ErrChangedContent", err)
				}
			default:
				if !errors.Is(err, boom) {
					t.Fatalf("error = %v; want the unchanged %v", err, boom)
				}
			}
		})
	}
}

func TestAdapterProcessesEveryCandidateAndReturnsFirstError(t *testing.T) {
	first := validServedRequest()
	first.executionID = "exec-1"
	first.actionID = "action-1"
	second := validServedRequest()
	second.executionID = "exec-2"
	second.actionID = "action-2"
	third := validServedRequest()
	third.executionID = "exec-3"
	third.actionID = "action-3"

	fixture := newAdapterFixture(t, services.KindEmail, first, second, third)
	boom := errors.New("first failure")
	fixture.handler.resolveFunc = func(input api.ExecutionRequestResolveRequest, _ api.Request, _ int) error {
		switch input.ActionID {
		case first.actionID:
			return boom
		case second.actionID:
			return &api.Error{Code: api.CodeVersionConflict, Message: "stale"}
		}
		return nil
	}
	if err := fixture.run(t); !errors.Is(err, boom) {
		t.Fatalf("RunOnce error = %v; want the first failure", err)
	}
	if count := fixture.handler.resolveCount(); count != 3 {
		t.Fatalf("resolve calls = %d; want every candidate attempted", count)
	}

	// A later pass retries with identical operation, content and key.
	fixture.handler.resolveFunc = nil
	if err := fixture.run(t); err != nil {
		t.Fatalf("retry RunOnce: %v", err)
	}
	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 6 {
		t.Fatalf("resolve calls = %d; want 6", len(resolves))
	}
	byAction := make(map[string]resolveRecord, 3)
	for _, resolve := range resolves[:3] {
		byAction[resolve.input.ActionID] = resolve
	}
	for _, resolve := range resolves[3:] {
		original, ok := byAction[resolve.input.ActionID]
		if !ok {
			t.Fatalf("retry resolved unexpected action %q", resolve.input.ActionID)
		}
		if resolve.request.IdempotencyKey != original.request.IdempotencyKey {
			t.Fatalf("retry key %q != original %q for %q",
				resolve.request.IdempotencyKey, original.request.IdempotencyKey, resolve.input.ActionID)
		}
		if string(resolve.rawInput) != string(original.rawInput) {
			t.Fatalf("retry input changed for %q:\n%s\n%s", resolve.input.ActionID, original.rawInput, resolve.rawInput)
		}
	}
}

func TestAdapterKeepsScanningWhenEventSourceFails(t *testing.T) {
	boom := errors.New("event source unavailable")
	request := validServedRequest()

	t.Run("run once still scans", func(t *testing.T) {
		fixture := newAdapterFixture(t, services.KindEmail, request)
		fixture.events.err = boom
		if err := fixture.run(t); !errors.Is(err, boom) {
			t.Fatalf("RunOnce error = %v; want the event source failure %v", err, boom)
		}
		if fixture.handler.resolveCount() != 1 {
			t.Fatalf("resolve calls = %d; want the retained request resolved despite the poll failure", fixture.handler.resolveCount())
		}
	})

	t.Run("run keeps scanning", func(t *testing.T) {
		fixture := newAdapterFixture(t, services.KindEmail, request)
		fixture.events.err = boom
		fixture.handler.resolved = make(chan struct{}, 8)
		config := validAdapterConfig(fixture.handler, fixture.events, fixture.clock)
		config.RescanInterval = 5 * time.Millisecond
		adapter, err := services.NewAdapter(config)
		if err != nil {
			t.Fatalf("NewAdapter: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- adapter.Run(ctx) }()

		waitForResolves(t, fixture.handler, 2)
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v after cancellation; want nil despite the event-source failure", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run exited on the event-source failure")
		}
		if fixture.handler.resolveCount() < 2 {
			t.Fatalf("resolve calls = %d; want a startup and at least one periodic scan", fixture.handler.resolveCount())
		}
	})
}

func TestAdapterRunScansAtStartupAndPeriodically(t *testing.T) {
	fixture := newAdapterFixture(t, services.KindEmail, validServedRequest())
	fixture.handler.resolved = make(chan struct{}, 8)
	config := validAdapterConfig(fixture.handler, fixture.events, fixture.clock)
	config.RescanInterval = 5 * time.Millisecond
	adapter, err := services.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.Run(ctx) }()

	waitForResolves(t, fixture.handler, 2)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) < 2 {
		t.Fatalf("resolve calls = %d; want a startup pass and at least one periodic pass", len(resolves))
	}
	for index, resolve := range resolves {
		if resolve.request.IdempotencyKey != resolves[0].request.IdempotencyKey {
			t.Fatalf("resolve %d key = %q; want the stable %q",
				index, resolve.request.IdempotencyKey, resolves[0].request.IdempotencyKey)
		}
	}
}

// TestResponderUsesServedInboxCodec proves the responder discovers and decodes
// the real served task.inbox request/result pair. The scripted boundary uses
// the real operation codecs, so the legacy flat tenant_id/page request is
// refused as an unknown field and the legacy "assignments" result decodes to
// zero items.
func TestResponderUsesServedInboxCodec(t *testing.T) {
	request := validServedRequest()
	fixture := newAdapterFixture(t, services.KindResponder, request)

	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fixture.handler.resolveCount() != 1 {
		t.Fatalf("resolve calls = %d; want one served request resolved", fixture.handler.resolveCount())
	}
	inbox := fixture.handler.inboxCalls()
	if len(inbox) != 1 {
		t.Fatalf("inbox calls = %d; want 1", len(inbox))
	}
	if inbox[0].ProjectID != string(testProject) {
		t.Fatalf("inbox project = %q; want the served project_id", inbox[0].ProjectID)
	}
	if !reflect.DeepEqual(inbox[0].Kinds, []string{"request"}) {
		t.Fatalf("inbox kinds = %v; want the served request kind only", inbox[0].Kinds)
	}
	if inbox[0].Page == nil || inbox[0].Page.Cursor != "" {
		t.Fatalf("inbox page = %+v; want the first served page", inbox[0].Page)
	}

	t.Run("mismatched result operation refused", func(t *testing.T) {
		mismatch := newAdapterFixture(t, services.KindResponder, request)
		mismatch.handler.resultOperation = services.OperationInspect
		if err := mismatch.run(t); err == nil {
			t.Fatal("RunOnce accepted a task.inbox result that claimed another operation")
		}
		if mismatch.handler.resolveCount() != 0 {
			t.Fatal("a mismatched result operation still produced an answer")
		}
	})

	t.Run("mismatched inspect result operation refused", func(t *testing.T) {
		mismatch := newAdapterFixture(t, services.KindResponder, request)
		mismatch.handler.resultOperations = map[string]string{services.OperationInspect: services.OperationInbox}
		if err := mismatch.run(t); err == nil {
			t.Fatal("RunOnce accepted an execution.inspect result that claimed another operation")
		}
		if mismatch.handler.resolveCount() != 0 {
			t.Fatal("a mismatched inspect result operation still produced an answer")
		}
	})

	t.Run("mismatched resolve result operation refused", func(t *testing.T) {
		mismatch := newAdapterFixture(t, services.KindResponder, request)
		mismatch.handler.resultOperations = map[string]string{services.OperationResolve: services.OperationInbox}
		if err := mismatch.run(t); err == nil {
			t.Fatal("RunOnce accepted an execution.request.resolve result that claimed another operation")
		}
		if mismatch.handler.resolveCount() != 1 {
			t.Fatalf("resolve calls = %d; want the refused resolve attempt", mismatch.handler.resolveCount())
		}
	})
}

// TestResponderRejectsMalformedInboxResult proves the adapter strictly decodes
// the served inbox result: an unknown field, the legacy "assignments" shape and
// a non-positive item version fail before any inspect or resolve.
func TestResponderRejectsMalformedInboxResult(t *testing.T) {
	request := validServedRequest()
	zeroVersion := fmt.Sprintf(
		`{"items":[{"kind":"request","execution_id":%q,"action_id":%q,"assignment":%q,"version":0,"at":%q}]}`,
		request.executionID, request.actionID, request.assignment, request.at.Format(time.RFC3339),
	)
	cases := map[string]string{
		"unknown field":            `{"items":[],"unexpected_field":true}`,
		"legacy assignments shape": `{"assignments":null}`,
		"zero item version":        zeroVersion,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newAdapterFixture(t, services.KindResponder, request)
			fixture.handler.resultBodies = map[string]json.RawMessage{
				services.OperationInbox: json.RawMessage(body),
			}
			if err := fixture.run(t); err == nil {
				t.Fatalf("RunOnce accepted a malformed task.inbox result: %s", body)
			}
			if inspects := fixture.handler.inspectCalls(); len(inspects) != 0 {
				t.Fatalf("inspect calls = %d; want none before the inbox result decodes", len(inspects))
			}
			if fixture.handler.resolveCount() != 0 {
				t.Fatal("a malformed inbox result still produced an answer")
			}
		})
	}
}

// TestResponderUsesBothCurrentVersions proves the action-only resolve still
// carries the current execution version in the shared envelope and the current
// action version in the body. The scanned inbox item deliberately carries a
// stale action version, so an implementation that reads the action version from
// the scanned item cannot pass; the body never repeats an execution version.
func TestResponderUsesBothCurrentVersions(t *testing.T) {
	request := validServedRequest()
	request.executionVersion = 4
	request.actionVersion = 7
	fixture := newAdapterFixture(t, services.KindResponder, request)
	fixture.handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		item := request.inboxItem()
		item.Version = 2 // the scanned action version is stale
		return api.TaskInboxResult{Items: []api.InboxItem{item}}, nil
	}

	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want 1", len(resolves))
	}
	resolve := resolves[0]
	if resolve.input.ExpectedActionVersion != 7 {
		t.Fatalf("expected_action_version = %d; want the current action version 7", resolve.input.ExpectedActionVersion)
	}
	if resolve.request.ExpectedVersion == nil || *resolve.request.ExpectedVersion != 4 {
		t.Fatalf("envelope expected_version = %v; want the execution version 4", resolve.request.ExpectedVersion)
	}
	var body map[string]any
	if err := json.Unmarshal(resolve.rawInput, &body); err != nil {
		t.Fatalf("decode resolve input: %v", err)
	}
	if _, present := body["expected_execution_version"]; present {
		t.Fatal("action-only resolve body still carries expected_execution_version")
	}
	if len(resolve.input.Outputs) != 0 {
		t.Fatalf("outputs = %v; want no automatic response outputs", resolve.input.Outputs)
	}
}

// TestResponderStartupRescanWithoutEvents proves the startup scan discovers a
// retained request with an empty notification source, and a later rescan after
// settlement creates no replacement answer.
func TestResponderStartupRescanWithoutEvents(t *testing.T) {
	request := validServedRequest()
	fixture := newAdapterFixture(t, services.KindResponder, request)

	if err := fixture.run(t); err != nil {
		t.Fatalf("startup RunOnce: %v", err)
	}
	if fixture.handler.resolveCount() != 1 {
		t.Fatalf("resolve calls = %d; want the startup scan to answer once", fixture.handler.resolveCount())
	}

	// The served request is settled after the answer, so a rescan sees no
	// current request even though the notification source remains empty.
	fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return api.ExecutionInspectResult{}, &api.Error{Code: api.CodeNotFound, Message: "no current request"}
	}
	if err := fixture.run(t); err != nil {
		t.Fatalf("rescan RunOnce: %v", err)
	}
	if fixture.handler.resolveCount() != 1 {
		t.Fatalf("resolve calls = %d; want no replacement answer on rescan", fixture.handler.resolveCount())
	}
	if inbox := fixture.handler.inboxCalls(); len(inbox) != 2 {
		t.Fatalf("inbox scans = %d; want the startup scan and one rescan", len(inbox))
	}
	cursors := fixture.events.polledCursors()
	if len(cursors) < 2 || cursors[0] != "" {
		t.Fatalf("event poll cursors = %v; want a poll from the empty cursor on every pass", cursors)
	}
}

// TestResponderRejectsOtherServiceAndHumanAssignment proves the adapter filters
// the served inbox by exact service assignment before revalidation. A foreign
// service, a near-miss service id and a human role are never inspected or
// answered.
func TestResponderRejectsOtherServiceAndHumanAssignment(t *testing.T) {
	ours := validServedRequest()

	foreignService := validServedRequest()
	foreignService.executionID = "exec-2"
	foreignService.actionID = "action-2"
	foreignService.assignment = "service/webhook-svc"

	nearMiss := validServedRequest()
	nearMiss.executionID = "exec-3"
	nearMiss.actionID = "action-3"
	nearMiss.assignment = "service/email-svc-other"

	humanRole := validServedRequest()
	humanRole.executionID = "exec-4"
	humanRole.actionID = "action-4"
	humanRole.assignment = "role/reviewer"

	fixture := newAdapterFixture(t, services.KindResponder, ours, foreignService, nearMiss, humanRole)
	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want exactly the adapter's own service assignment", len(resolves))
	}
	if resolves[0].input.ExecutionID != ours.executionID || resolves[0].input.ActionID != ours.actionID {
		t.Fatalf("resolved request = %s/%s; want %s/%s",
			resolves[0].input.ExecutionID, resolves[0].input.ActionID, ours.executionID, ours.actionID)
	}
	if inspects := fixture.handler.inspectCalls(); len(inspects) != 1 {
		t.Fatalf("inspect calls = %d; want 1 (foreign and human rows are filtered before inspection)", len(inspects))
	}
	if inspects := fixture.handler.inspectCalls(); len(inspects) == 1 && inspects[0].ExecutionID != ours.executionID {
		t.Fatalf("inspected execution = %q; want %q", inspects[0].ExecutionID, ours.executionID)
	}
}

// TestResponderScopeIsExactServiceRequest proves the scope filter is an exact,
// case-sensitive request assignment to the adapter's own service. Case
// variants, suffix near-misses and a waiting_task item assigned to the same
// service are never inspected or resolved.
func TestResponderScopeIsExactServiceRequest(t *testing.T) {
	ours := validServedRequest()
	ours.assignment = "service/responder-svc"

	caseVariant := validServedRequest()
	caseVariant.executionID = "exec-case"
	caseVariant.actionID = "action-case"
	caseVariant.assignment = "service/Responder-SVC"

	upperID := validServedRequest()
	upperID.executionID = "exec-upper"
	upperID.actionID = "action-upper"
	upperID.assignment = "service/responder-SVc"

	digitSuffix := validServedRequest()
	digitSuffix.executionID = "exec-digit"
	digitSuffix.actionID = "action-digit"
	digitSuffix.assignment = "service/responder-svc2"

	shortSuffix := validServedRequest()
	shortSuffix.executionID = "exec-short"
	shortSuffix.actionID = "action-short"
	shortSuffix.assignment = "service/responder-sv"

	waiting := api.InboxItem{
		Kind:             "waiting_task",
		TaskID:           "task-1",
		Assignment:       "service/responder-svc",
		QuestionID:       "question-1",
		Question:         "proceed?",
		Version:          1,
		LifecycleVersion: 1,
		At:               testNow,
	}

	fixture := newAdapterFixtureForService(t, services.KindResponder, id.PrincipalID("responder-svc"), ours)
	fixture.handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{Items: []api.InboxItem{
			ours.inboxItem(),
			caseVariant.inboxItem(),
			upperID.inboxItem(),
			digitSuffix.inboxItem(),
			shortSuffix.inboxItem(),
			waiting,
		}}, nil
	}

	if err := fixture.run(t); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve calls = %d; want exactly the adapter's own exact service request", len(resolves))
	}
	if resolves[0].input.ExecutionID != ours.executionID || resolves[0].input.ActionID != ours.actionID {
		t.Fatalf("resolved request = %s/%s; want %s/%s",
			resolves[0].input.ExecutionID, resolves[0].input.ActionID, ours.executionID, ours.actionID)
	}
	inspects := fixture.handler.inspectCalls()
	if len(inspects) != 1 || inspects[0].ExecutionID != ours.executionID {
		t.Fatalf("inspect calls = %+v; want only the exact service request %q", inspects, ours.executionID)
	}
}

// TestResponderLostResolveResponseConverges proves a committed answer whose
// response was lost is not answered again: the next scan revalidates the
// settled action and creates no replacement answer.
func TestResponderLostResolveResponseConverges(t *testing.T) {
	lost := errors.New("simulated lost resolve response")

	t.Run("settled action is not answered again", func(t *testing.T) {
		request := validServedRequest()
		fixture := newAdapterFixture(t, services.KindResponder, request)
		answeredAt := testNow.Add(time.Second)
		settled := false
		fixture.handler.resolveFunc = func(api.ExecutionRequestResolveRequest, api.Request, int) error {
			settled = true
			return lost
		}
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			if !settled {
				return request.inspectResult(), nil
			}
			current := request
			current.state = "succeeded"
			current.decision = services.DecisionPass
			current.actor = string(testService)
			current.answeredAt = &answeredAt
			current.executionVersion++
			current.actionVersion++
			return current.inspectResult(), nil
		}

		if err := fixture.run(t); !errors.Is(err, lost) {
			t.Fatalf("first RunOnce error = %v; want the simulated lost response", err)
		}
		if fixture.handler.resolveCount() != 1 {
			t.Fatalf("resolve calls after the lost response = %d; want 1", fixture.handler.resolveCount())
		}

		if err := fixture.run(t); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 1 {
			t.Fatalf("resolve calls = %d; want no replacement answer after convergence", fixture.handler.resolveCount())
		}
	})

	t.Run("vanished request is not answered again", func(t *testing.T) {
		request := validServedRequest()
		fixture := newAdapterFixture(t, services.KindResponder, request)
		settled := false
		fixture.handler.resolveFunc = func(api.ExecutionRequestResolveRequest, api.Request, int) error {
			settled = true
			return lost
		}
		fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			if settled {
				return api.ExecutionInspectResult{}, &api.Error{Code: api.CodeNotFound, Message: "no current request"}
			}
			return request.inspectResult(), nil
		}

		if err := fixture.run(t); !errors.Is(err, lost) {
			t.Fatalf("first RunOnce error = %v; want the simulated lost response", err)
		}
		if err := fixture.run(t); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		if fixture.handler.resolveCount() != 1 {
			t.Fatalf("resolve calls = %d; want no replacement answer for a vanished request", fixture.handler.resolveCount())
		}
	})
}

// assertLegalIdempotencyKey proves a derived key is a legal public idempotency
// key, so the public edge never refuses the adapter's own request.
func assertLegalIdempotencyKey(t *testing.T, key string, version uint64) {
	t.Helper()
	if key == "" {
		t.Fatal("idempotency key is empty")
	}
	expected := version
	envelope := api.Request{
		Input:           json.RawMessage(`{}`),
		ExpectedVersion: &expected,
		IdempotencyKey:  key,
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("idempotency key %q is not a legal public key: %v", key, err)
	}
}
