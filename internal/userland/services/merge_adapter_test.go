package services_test

// The merge-coordinator service adapter.
//
// A merge-coordinator assignment is a request action whose immutable typed
// subject names the exact repository/ref/base/candidate and whose declared
// outputs name the observations the answer must carry. The adapter revalidates
// the current request, performs the serialized effect through the consumer-owned
// merge runner, maps only observed values onto the declared outputs, resolves the
// request as the service principal and settles the durable assignment after the
// request is resolved.
//
// Every refusal below must leave the effect boundary untouched: a foreign
// assignment, a human role, a cancelled or stale request and an expired deadline
// are decided before the runner is called.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

var testMergeService = id.PrincipalID("merge-svc")

// mergeTestKey is the repository/ref of the valid merge assignment below.
func mergeTestKey() services.MergeKey {
	return services.MergeKey{Repository: "shop/web", Ref: "refs/heads/work/merge-1"}
}

// mergeSubjectFields returns the exact typed subject of one merge assignment.
func mergeSubjectFields(repository, ref, base, candidate string) []api.SubjectField {
	return []api.SubjectField{
		{Name: services.MergeFieldRepository, Kind: "string", Value: repository},
		{Name: services.MergeFieldRef, Kind: "string", Value: ref},
		{Name: services.MergeFieldBase, Kind: "string", Value: base},
		{Name: services.MergeFieldCandidate, Kind: "string", Value: candidate},
	}
}

// mergeSubject wraps subject fields as the served immutable subject.
func mergeSubject(fields []api.SubjectField) *api.Subject {
	return &api.Subject{Fields: fields}
}

// validMergeInboxItem is one running merge request assigned to the test service.
func validMergeInboxItem() api.InboxItem {
	return api.InboxItem{
		Kind:        "request",
		ExecutionID: "exec-1",
		ActionID:    "action-1",
		Assignment:  "service/" + string(testMergeService),
		Subject:     mergeSubject(mergeSubjectFields("shop/web", "refs/heads/work/merge-1", "base-1111111", "candidate-2222222")),
		Outputs:     []string{services.MergeOutputBase, services.MergeOutputCandidate, services.MergeOutputResult},
		Version:     1,
		At:          testNow.Add(time.Hour),
	}
}

// mergeInspect is the served current-execution observation of one request.
func mergeInspect(executionID, actionID string, actionVersion uint64, state, decision string) api.ExecutionInspectResult {
	action := api.ExecutionAction{
		ActionID: actionID,
		Index:    0,
		Name:     "merge",
		Kind:     "request",
		State:    state,
		Version:  actionVersion,
	}
	if decision != "" {
		action.Decision = decision
		action.Actor = string(testMergeService)
		answered := testNow.Add(time.Second)
		action.AnsweredAt = &answered
	}
	return api.ExecutionInspectResult{
		ExecutionID:   executionID,
		PipelineID:    "pipeline-1",
		State:         "running",
		Version:       1,
		CurrentAction: 0,
		Actions:       []api.ExecutionAction{action},
		Cleanup:       api.ExecutionCleanup{State: "none"},
	}
}

// mergeAdapterConfig is the merge-coordinator adapter configuration.
func mergeAdapterConfig(handler api.Handler, events services.EventSource, clock services.Clock, runner services.MergeRunner) services.AdapterConfig {
	return services.AdapterConfig{
		Kind:      services.KindMergeCoordinator,
		ServiceID: testMergeService,
		TenantID:  testTenant,
		ProjectID: testProject,
		Handler:   handler,
		Events:    events,
		Clock:     clock,
		Merges:    runner,
	}
}

// newMergeTestAdapter builds the adapter under test.
func newMergeTestAdapter(t *testing.T, handler api.Handler, events services.EventSource, clock services.Clock, runner services.MergeRunner) *services.Adapter {
	t.Helper()
	adapter, err := services.NewAdapter(mergeAdapterConfig(handler, events, clock, runner))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return adapter
}

// mergeTestFixture wires the fake public boundary, the durable store and the
// scripted merge effects.
type mergeTestFixture struct {
	handler     *fakeHandler
	store       *fakeCoordinationStore
	effects     *fakeMergeEffects
	clock       *fakeClock
	coordinator *services.Coordinator
	adapter     *services.Adapter
}

func newMergeTestFixture(t *testing.T, inbox func(api.TaskInboxRequest) (api.TaskInboxResult, error), inspect func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error)) *mergeTestFixture {
	t.Helper()
	log := &callLog{}
	handler := &fakeHandler{log: log}
	handler.inboxFunc = inbox
	if inspect == nil {
		item := validMergeInboxItem()
		inspect = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
			return mergeInspect(item.ExecutionID, item.ActionID, item.Version, "running", ""), nil
		}
	}
	handler.inspectFunc = inspect
	store := newFakeCoordinationStore(log)
	effects := &fakeMergeEffects{log: log}
	effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "refs/heads/work/merge-1 merged into candidate-2222222"), nil
	}
	clock := &fakeClock{now: testNow}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	adapter := newMergeTestAdapter(t, handler, &fakeEventSource{}, clock, coordinator)
	return &mergeTestFixture{handler: handler, store: store, effects: effects, clock: clock, coordinator: coordinator, adapter: adapter}
}

func singleMergeItem(item api.InboxItem) func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
	return func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{Items: []api.InboxItem{item}}, nil
	}
}

func TestMergeAdapterAnswersObservedResult(t *testing.T) {
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)

	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the single serialized effect", fixture.effects.executeCount())
	}
	operations := fixture.effects.executedOperations()
	want := services.MergeOperation{
		Key:         services.MergeKey{Repository: "shop/web", Ref: "refs/heads/work/merge-1"},
		OperationID: "op-1",
		Base:        "base-1111111",
		Candidate:   "candidate-2222222",
	}
	if len(operations) != 1 || operations[0] != want {
		t.Fatalf("executed operations = %+v; want %+v", operations, want)
	}

	resolves := fixture.handler.resolvesSnapshot()
	if len(resolves) != 1 {
		t.Fatalf("resolve submissions = %d; want exactly one answer", len(resolves))
	}
	resolved := resolves[0]
	if resolved.input.Decision != services.DecisionPass {
		t.Fatalf("decision = %q; want pass", resolved.input.Decision)
	}
	wantOutputs := map[string]string{
		services.MergeOutputBase:      "base-1111111",
		services.MergeOutputCandidate: "candidate-2222222",
		services.MergeOutputResult:    "refs/heads/work/merge-1 merged into candidate-2222222",
	}
	if !reflect.DeepEqual(resolved.input.Outputs, wantOutputs) {
		t.Fatalf("resolved outputs = %v; want exactly the observed declared values %v", resolved.input.Outputs, wantOutputs)
	}
	if resolved.request.ExpectedVersion == nil || *resolved.request.ExpectedVersion != 1 {
		t.Fatalf("resolve envelope version = %v; want the observed execution version 1", resolved.request.ExpectedVersion)
	}

	record, ok := fixture.store.record(t, testTenant, mergeTestKey())
	if !ok {
		t.Fatal("the merge assignment has no durable record")
	}
	if record.State != services.EffectApplied || !record.Released {
		t.Fatalf("record = %+v; want a released applied assignment after the resolution", record)
	}
	if record.Request.Assignment.ExecutionID != "exec-1" || record.Request.Assignment.ActionID != "action-1" {
		t.Fatalf("record assignment = %+v; want the exact execution/action identity", record.Request.Assignment)
	}
}

func TestMergeAdapterRefusesMissingDependency(t *testing.T) {
	handler := &fakeHandler{log: &callLog{}}
	clock := &fakeClock{now: testNow}
	if _, err := services.NewAdapter(mergeAdapterConfig(handler, &fakeEventSource{}, clock, nil)); err == nil {
		t.Fatal("NewAdapter accepted a merge coordinator with no merge dependency")
	}

	// Configured success is the positive control: the same config with a
	// runner is accepted.
	coordinator := newCoordinator(t, newFakeCoordinationStore(&callLog{}), &fakeMergeEffects{log: &callLog{}}, clock, "merge-worker-1")
	if _, err := services.NewAdapter(mergeAdapterConfig(handler, &fakeEventSource{}, clock, coordinator)); err != nil {
		t.Fatalf("NewAdapter refused a complete merge configuration: %v", err)
	}
}

// TestMergeAdapterRefusesMalformedMapping proves a malformed typed subject is
// refused before any effect and any answer.
func TestMergeAdapterRefusesMalformedMapping(t *testing.T) {
	valid := mergeSubjectFields("shop/web", "refs/heads/work/merge-1", "base-1111111", "candidate-2222222")
	cases := map[string]*api.Subject{
		"missing repository": mergeSubject(valid[1:]),
		"missing candidate":  mergeSubject(valid[:3]),
		"extra field": mergeSubject(append(append([]api.SubjectField(nil), valid...),
			api.SubjectField{Name: "approval", Kind: "string", Value: "role/reviewer"})),
		"wrong kind": mergeSubject([]api.SubjectField{
			{Name: services.MergeFieldRepository, Kind: "string", Value: "shop/web"},
			{Name: services.MergeFieldRef, Kind: "number", Value: "1"},
			{Name: services.MergeFieldBase, Kind: "string", Value: "base-1111111"},
			{Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-2222222"},
		}),
		"empty value": mergeSubject([]api.SubjectField{
			{Name: services.MergeFieldRepository, Kind: "string", Value: "shop/web"},
			{Name: services.MergeFieldRef, Kind: "string", Value: "refs/heads/work/merge-1"},
			{Name: services.MergeFieldBase, Kind: "string", Value: ""},
			{Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-2222222"},
		}),
		"invalid repository": mergeSubject(mergeSubjectFields("../escape", "refs/heads/work/merge-1", "base-1111111", "candidate-2222222")),
		"invalid ref":        mergeSubject(mergeSubjectFields("shop/web", "heads/main", "base-1111111", "candidate-2222222")),
	}
	for name, subject := range cases {
		t.Run(name, func(t *testing.T) {
			item := validMergeInboxItem()
			item.Subject = subject
			fixture := newMergeTestFixture(t, singleMergeItem(item), nil)
			if err := fixture.adapter.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce reported success for a malformed merge subject")
			}
			if fixture.effects.executeCount() != 0 {
				t.Fatalf("execute calls = %d; a malformed subject caused an effect", fixture.effects.executeCount())
			}
			if fixture.handler.resolveCount() != 0 {
				t.Fatalf("resolve submissions = %d; a malformed subject answered a request", fixture.handler.resolveCount())
			}
		})
	}
}

// TestMergeAdapterRefusesMalformedDeclaredOutputs proves an unknown, duplicate
// or absent declared output set is refused before any effect.
func TestMergeAdapterRefusesMalformedDeclaredOutputs(t *testing.T) {
	cases := map[string][]string{
		"no declared output":  nil,
		"unknown output":      {services.MergeOutputBase, "approval"},
		"duplicate output":    {services.MergeOutputBase, services.MergeOutputBase},
		"protected-ref name":  {"refs/heads/main"},
		"empty output name":   {""},
		"undeclared-response": {services.MergeOutputCandidate, "reviewer"},
	}
	for name, outputs := range cases {
		t.Run(name, func(t *testing.T) {
			item := validMergeInboxItem()
			item.Outputs = outputs
			fixture := newMergeTestFixture(t, singleMergeItem(item), nil)
			if err := fixture.adapter.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce reported success for a malformed output declaration")
			}
			if fixture.effects.executeCount() != 0 {
				t.Fatalf("execute calls = %d; a malformed output declaration caused an effect", fixture.effects.executeCount())
			}
			if fixture.handler.resolveCount() != 0 {
				t.Fatalf("resolve submissions = %d; a malformed declaration answered a request", fixture.handler.resolveCount())
			}
		})
	}
}

// TestMergeAdapterZeroEffectForForeignHumanCancelledStaleAndExpired covers every
// request the adapter must not act on.
func TestMergeAdapterZeroEffectForForeignHumanCancelledStaleAndExpired(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*api.InboxItem)
		inspect func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error)
		wantErr bool
	}{
		"foreign service": {
			mutate: func(item *api.InboxItem) { item.Assignment = "service/other-merge-svc" },
		},
		"human role": {
			mutate: func(item *api.InboxItem) { item.Assignment = "role/reviewer" },
		},
		"expired deadline": {
			mutate: func(item *api.InboxItem) { item.At = testNow.Add(-time.Second) },
		},
		"cancelled action": {
			inspect: func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return mergeInspect("exec-1", "action-1", 1, "cancelled", ""), nil
			},
		},
		"settled action": {
			inspect: func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return mergeInspect("exec-1", "action-1", 1, "succeeded", "pass"), nil
			},
		},
		"stale action version": {
			mutate: func(item *api.InboxItem) { item.Version = 2 },
			inspect: func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return mergeInspect("exec-1", "action-1", 1, "running", ""), nil
			},
			wantErr: true,
		},
		"moved execution": {
			inspect: func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return mergeInspect("exec-other", "action-1", 1, "running", ""), nil
			},
		},
		"foreign action": {
			inspect: func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
				return mergeInspect("exec-1", "action-other", 1, "running", ""), nil
			},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			item := validMergeInboxItem()
			if testCase.mutate != nil {
				testCase.mutate(&item)
			}
			fixture := newMergeTestFixture(t, singleMergeItem(item), testCase.inspect)
			err := fixture.adapter.RunOnce(context.Background())
			if testCase.wantErr && !errors.Is(err, services.ErrStaleVersion) {
				t.Fatalf("RunOnce error = %v; want ErrStaleVersion for the stale request", err)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("RunOnce error = %v; want a clean skip", err)
			}
			if fixture.effects.executeCount() != 0 {
				t.Fatalf("execute calls = %d; the request must cause zero effects", fixture.effects.executeCount())
			}
			if fixture.handler.resolveCount() != 0 {
				t.Fatalf("resolve submissions = %d; the request must not be answered", fixture.handler.resolveCount())
			}
			if creates := fixture.store.createCount(); creates != 0 {
				t.Fatalf("claim creations = %d; the request reached the merge store", creates)
			}
		})
	}
}

// TestMergeAdapterSettlesOnlyAfterResolution proves the durable assignment is
// still held when the outgoing resolve is in flight; it is released only after
// the answer committed.
func TestMergeAdapterSettlesOnlyAfterResolution(t *testing.T) {
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)
	heldWhileResolving := false
	fixture.handler.resolveFunc = func(api.ExecutionRequestResolveRequest, api.Request, int) error {
		record, ok := fixture.store.record(t, testTenant, mergeTestKey())
		heldWhileResolving = ok && record.State == services.EffectApplied && !record.Released
		return nil
	}
	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !heldWhileResolving {
		t.Fatal("the assignment was released before the request resolution")
	}
	record, ok := fixture.store.record(t, testTenant, mergeTestKey())
	if !ok || !record.Released {
		t.Fatalf("record = %+v; want released after the resolution", record)
	}
}

// TestMergeAdapterLostResolveResponseConverges proves an answer whose response
// was lost settles from durable state: a restarted adapter with an empty inbox
// inspects the assignment's current action, sees the committed decision and
// settles without repeating the effect or the answer.
func TestMergeAdapterLostResolveResponseConverges(t *testing.T) {
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)
	fixture.handler.resolveFunc = func(api.ExecutionRequestResolveRequest, api.Request, int) error {
		return errors.New("transport lost the committed resolve response")
	}
	if err := fixture.adapter.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce reported success although the resolve response was lost")
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the single committed effect", fixture.effects.executeCount())
	}
	held, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if held.Released {
		t.Fatal("the assignment was released without a confirmed resolution")
	}

	// The answer did commit server-side; the request leaves the inbox. A fresh
	// adapter process over the same durable store converges from the pending
	// assignment.
	settledInspect := func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return mergeInspect("exec-1", "action-1", 2, "succeeded", "pass"), nil
	}
	restartedHandler := &fakeHandler{log: &callLog{}}
	restartedHandler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{}, nil
	}
	restartedHandler.inspectFunc = settledInspect
	restarted := newMergeTestAdapter(t, restartedHandler, &fakeEventSource{}, fixture.clock, fixture.coordinator)
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restarted RunOnce: %v", err)
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls after restart = %d; want no repeated effect", fixture.effects.executeCount())
	}
	if restartedHandler.resolveCount() != 0 {
		t.Fatalf("restarted resolve submissions = %d; the committed answer must not be replaced", restartedHandler.resolveCount())
	}
	released, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if !released.Released || released.State != services.EffectApplied {
		t.Fatalf("record after restart = %+v; want the settled assignment released", released)
	}
}

// TestMergeAdapterKeepsPendingAssignmentWhileRequestRuns proves reconciliation
// never settles an assignment whose current action is still running.
func TestMergeAdapterKeepsPendingAssignmentWhileRequestRuns(t *testing.T) {
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)
	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Simulate a resolve whose response was lost before the request settled:
	// put the record back into the held state by clearing only the release.
	record, _ := fixture.store.record(t, testTenant, mergeTestKey())
	held := record
	held.Released = false
	if err := fixture.store.Transition(context.Background(), testTenant, record, held); err != nil {
		t.Fatalf("reinstate held record: %v", err)
	}

	runningHandler := &fakeHandler{log: &callLog{}}
	runningHandler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{}, nil
	}
	runningHandler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return mergeInspect("exec-1", "action-1", 1, "running", ""), nil
	}
	restarted := newMergeTestAdapter(t, runningHandler, &fakeEventSource{}, fixture.clock, fixture.coordinator)
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restarted RunOnce: %v", err)
	}
	stillHeld, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if stillHeld.Released {
		t.Fatal("a running request's assignment was released")
	}
	if runningHandler.resolveCount() != 0 {
		t.Fatal("the restarted adapter answered a request from the inbox it never scanned")
	}
}

// TestMergeAdapterUncertainEffectIsNotAnswered proves an unknown outcome never
// becomes a service answer.
func TestMergeAdapterUncertainEffectIsNotAnswered(t *testing.T) {
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)
	fixture.effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, errors.New("connection reset during the merge effect")
	}
	if err := fixture.adapter.RunOnce(context.Background()); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("RunOnce error = %v; want ErrUncertain", err)
	}
	if fixture.handler.resolveCount() != 0 {
		t.Fatalf("resolve submissions = %d; an uncertain effect was answered", fixture.handler.resolveCount())
	}
	record, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if record.Released {
		t.Fatal("an uncertain effect was released")
	}
}
