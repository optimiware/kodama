//go:build integration

package services

// The merge-coordinator adapter on the real factory composition.
//
// The served public edge is real (PostgreSQL, NATS, the real operation
// handlers); these scenarios isolate adapter convergence with a scripted
// merge/rebase effect boundary. The concrete effects and the `kodama services`
// entry are proven separately by
// TestMergeCoordinatorRealGitMergesAssignedCandidate.
//
// The scenarios prove:
//
//   - a startup scan answers an old service assignment while the real broker is
//     unreachable, with exactly one recorded effect and one settlement whose
//     declared outputs are the observed work-ref result;
//  - a resolve whose committed response was lost converges after a restart from
//     the durable assignment: no repeated effect, no replacement answer;
//  - a human-role assignment stays with human authority: the merge service runs
//     no effect for it and its credential cannot resolve it.
//
// The merge service never approves, reviews or pushes. Its answer is an
// observed work-ref result (refs/heads/work/...), and the approval-guarded ref
// remains with the human/execution authority that owns it.

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	serviceadapter "github.com/optimiware/kodama/internal/userland/services"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	mergeAdapterServiceID  = "merge-adapter-svc"
	mergeAdapterRepository = "shop/web"
	mergeAdapterWorkRef    = "refs/heads/work/merge-integration"
	mergeAdapterOutput     = "refs/heads/work/merge-integration: merged candidate"
)

// mergePipelineAction is one request action for the merge service.
type mergePipelineAction struct {
	Name    string         `yaml:"name"`
	Request string         `yaml:"request"`
	Subject map[string]any `yaml:"subject,omitempty"`
	Outputs []string       `yaml:"outputs,omitempty"`
	Timeout string         `yaml:"timeout,omitempty"`
}

// mergePipelineDocument is the exact authored pipeline document.
type mergePipelineDocument struct {
	Actions []mergePipelineAction `yaml:"actions"`
}

// applyMergePipeline applies one merge request action assigned to assignment.
func applyMergePipeline(t *testing.T, harness *factory.Harness, name, assignment string) api.PipelineApplyResult {
	t.Helper()
	document, err := yaml.Marshal(mergePipelineDocument{Actions: []mergePipelineAction{{
		Name:    "merge",
		Request: assignment,
		Subject: map[string]any{
			serviceadapter.MergeFieldRepository: mergeAdapterRepository,
			serviceadapter.MergeFieldRef:        mergeAdapterWorkRef,
			serviceadapter.MergeFieldBase:       "base-1111111",
			serviceadapter.MergeFieldCandidate:  "candidate-2222222",
		},
		Outputs: []string{
			serviceadapter.MergeOutputBase,
			serviceadapter.MergeOutputCandidate,
			serviceadapter.MergeOutputResult,
		},
		Timeout: responderRequestTimeout,
	}}})
	if err != nil {
		t.Fatalf("marshal merge pipeline: %v", err)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// integrationMergeEffects is the fake merge/rebase boundary. It records every
// execution and settles applied observations that echo the exact revisions.
type integrationMergeEffects struct {
	mu         sync.Mutex
	executed   []serviceadapter.MergeOperation
	observed   []serviceadapter.MergeOperation
	executeErr error
	observe    func(serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error)
}

func (effects *integrationMergeEffects) Execute(_ context.Context, operation serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error) {
	effects.mu.Lock()
	effects.executed = append(effects.executed, operation)
	executeErr := effects.executeErr
	effects.mu.Unlock()
	if executeErr != nil {
		return serviceadapter.MergeObservation{State: serviceadapter.EffectUncertain, Base: operation.Base}, executeErr
	}
	return serviceadapter.MergeObservation{
		State:     serviceadapter.EffectApplied,
		Base:      operation.Base,
		Candidate: operation.Candidate,
		Output:    mergeAdapterOutput,
	}, nil
}

func (effects *integrationMergeEffects) Observe(_ context.Context, operation serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error) {
	effects.mu.Lock()
	effects.observed = append(effects.observed, operation)
	observe := effects.observe
	effects.mu.Unlock()
	if observe != nil {
		return observe(operation)
	}
	return serviceadapter.MergeObservation{State: serviceadapter.EffectNotStarted}, nil
}

func (effects *integrationMergeEffects) executeCount() int {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	return len(effects.executed)
}

func (effects *integrationMergeEffects) executions() []serviceadapter.MergeOperation {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	return append([]serviceadapter.MergeOperation(nil), effects.executed...)
}

// integrationOperationIDs returns deterministic, valid operation identities.
func integrationOperationIDs(prefix string) func() (id.OperationID, error) {
	var mu sync.Mutex
	next := 0
	return func() (id.OperationID, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return id.OperationID(fmt.Sprintf("%s-%d", prefix, next)), nil
	}
}

// newIntegrationCoordinator opens one durable file store over directory and
// returns the serializing coordinator.
func newIntegrationCoordinator(t *testing.T, directory string, effects serviceadapter.MergeEffects) *serviceadapter.Coordinator {
	t.Helper()
	store, err := serviceadapter.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	coordinator, err := serviceadapter.NewCoordinator(serviceadapter.CoordinatorConfig{
		TenantID:       id.TenantID(factory.TenantID),
		Owner:          "merge-adapter-integration",
		Lease:          time.Minute,
		Store:          store,
		Effects:        effects,
		Clock:          responderClock{},
		NewOperationID: integrationOperationIDs("merge-op"),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return coordinator
}

// newIntegrationMergeAdapter builds the merge adapter over one injected public
// handler, hint source and durable coordinator.
func newIntegrationMergeAdapter(t *testing.T, access serviceAccess, handler api.Handler, events serviceadapter.EventSource, coordinator serviceadapter.MergeRunner) *serviceadapter.Adapter {
	t.Helper()
	adapter, err := serviceadapter.NewAdapter(serviceadapter.AdapterConfig{
		Kind:      serviceadapter.KindMergeCoordinator,
		ServiceID: id.PrincipalID(access.id),
		TenantID:  id.TenantID(factory.TenantID),
		ProjectID: id.ProjectID(factory.ProjectID),
		Handler:   handler,
		Events:    events,
		Clock:     responderClock{},
		Merges:    coordinator,
	})
	if err != nil {
		t.Fatalf("services.NewAdapter: %v", err)
	}
	return adapter
}

// assertMergeAnswer proves the request carries exactly the observed work-ref
// result, attributed to the service principal, with no approval field.
func assertMergeAnswer(t *testing.T, execution api.ExecutionInspectResult, serviceID string) api.ExecutionAction {
	t.Helper()
	action := actionByName(t, execution, "merge")
	if action.Decision != "pass" {
		t.Fatalf("recorded decision = %q; want pass (action=%+v)", action.Decision, action)
	}
	if action.Actor != serviceID {
		t.Fatalf("recorded actor = %q; want the service principal %q", action.Actor, serviceID)
	}
	if action.AnsweredAt == nil || action.AnsweredAt.IsZero() {
		t.Fatalf("merge answer has no answered_at: %+v", action)
	}
	want := map[string]string{
		serviceadapter.MergeOutputBase:      "base-1111111",
		serviceadapter.MergeOutputCandidate: "candidate-2222222",
		serviceadapter.MergeOutputResult:    mergeAdapterOutput,
	}
	if !reflect.DeepEqual(action.Outputs, want) {
		t.Fatalf("recorded outputs = %v; want the observed work-ref result %v", action.Outputs, want)
	}
	if _, approved := action.Outputs["approval"]; approved {
		t.Fatal("the merge answer carries an approval")
	}
	return action
}

// TestMergeAdapterStartupResolvesObservedWorkRef creates the request before the
// service starts, stops the real broker and fails notification delivery, then
// proves the startup scan commits exactly one effect and one answer.
func TestMergeAdapterStartupResolvesObservedWorkRef(t *testing.T) {
	harness := factory.Start(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	access := createResponderService(t, harness, mergeAdapterServiceID)
	applied := applyMergePipeline(t, harness, "merge-startup", "service/"+mergeAdapterServiceID)
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-startup")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "merge")
	action := actionByName(t, waiting, "merge")

	inbox := inboxRequests(t, harness, access.token)
	item, found := findRequestInbox(inbox, started.ExecutionID, action.ActionID)
	if !found {
		t.Fatalf("the old merge request is not in the service inbox: %+v", inbox.Items)
	}
	if item.At.IsZero() || !item.At.After(time.Now()) {
		t.Fatalf("merge inbox item has no future deadline: %+v", item)
	}
	if !reflect.DeepEqual(item.Outputs, []string{serviceadapter.MergeOutputBase, serviceadapter.MergeOutputCandidate, serviceadapter.MergeOutputResult}) {
		t.Fatalf("declared outputs = %v", item.Outputs)
	}

	harness.NATS.Stop(t)
	requireBrokerUnreachable(t, harness)

	effects := &integrationMergeEffects{}
	coordinator := newIntegrationCoordinator(t, t.TempDir(), effects)
	handler := newResponderHandler(harness, access.token)
	adapter := newIntegrationMergeAdapter(t, access, handler, faultNotificationSource{}, coordinator)

	runErr := make(chan error, 1)
	go func() { runErr <- adapter.Run(ctx) }()

	settled := waitExecutionState(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded (failure=%+v)", settled.State, settled.Failure)
	}
	assertMergeAnswer(t, settled, mergeAdapterServiceID)
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one answer", got)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("merge effects = %d; want exactly one", effects.executeCount())
	}
	executions := effects.executions()
	if executions[0].Key != (serviceadapter.MergeKey{Repository: mergeAdapterRepository, Ref: mergeAdapterWorkRef}) ||
		executions[0].Base != "base-1111111" || executions[0].Candidate != "candidate-2222222" {
		t.Fatalf("effect operation = %+v; want the exact subject mapping", executions[0])
	}
	if scans := handler.count(serviceadapter.OperationInbox); scans < 1 {
		t.Fatalf("inbox scans = %d; the startup scan did not run", scans)
	}
	if inspections := handler.count(serviceadapter.OperationInspect); inspections < 1 {
		t.Fatalf("inspections = %d; the current request was not revalidated", inspections)
	}
	if resolves := handler.resolveAttempts(); resolves != 1 {
		t.Fatalf("resolve submissions = %d; want exactly one", resolves)
	}
	// The answer did not depend on the broker returning.
	requireBrokerUnreachable(t, harness)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("merge startup run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("merge adapter did not stop after cancellation")
	}
	harness.NATS.Start(t)
}

// TestMergeAdapterLostResolveResponseConverges loses the committed resolve
// response, then restarts the adapter over the same durable store. The request
// is no longer in the inbox, so only the durable assignment can converge it:
// exactly one effect and one committed answer survive.
func TestMergeAdapterLostResolveResponseConverges(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	access := createResponderService(t, harness, mergeAdapterServiceID)
	applied := applyMergePipeline(t, harness, "merge-lost-response", "service/"+mergeAdapterServiceID)
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-lost-response")
	waitRequestRunning(t, harness, started.ExecutionID, "merge")

	directory := t.TempDir()
	effects := &integrationMergeEffects{}
	firstCoordinator := newIntegrationCoordinator(t, directory, effects)
	firstHandler := newResponderHandler(harness, access.token)
	firstHandler.loseFirstResolve = true
	first := newIntegrationMergeAdapter(t, access, firstHandler, emptyNotificationSource{}, firstCoordinator)
	if err := first.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce reported success although the resolve response was lost")
	}
	if effects.executeCount() != 1 {
		t.Fatalf("merge effects = %d; want the one committed effect", effects.executeCount())
	}

	settled := waitExecutionSettled(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded (failure=%+v)", settled.State, settled.Failure)
	}
	assertMergeAnswer(t, settled, mergeAdapterServiceID)
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one", got)
	}
	pending, err := firstCoordinator.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending assignments = %+v; want the unresolved assignment", pending)
	}

	// A fresh process over the same durable store. The request is gone from the
	// inbox, so convergence must come from the pending assignment.
	restartedCoordinator := newIntegrationCoordinator(t, directory, effects)
	restartedHandler := newResponderHandler(harness, access.token)
	restarted := newIntegrationMergeAdapter(t, access, restartedHandler, emptyNotificationSource{}, restartedCoordinator)
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatalf("restarted RunOnce: %v", err)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("merge effects after restart = %d; want no repeated effect", effects.executeCount())
	}
	if resolves := restartedHandler.resolveAttempts(); resolves != 0 {
		t.Fatalf("restarted resolve submissions = %d; want none for the committed answer", resolves)
	}
	if updates := restartedHandler.count(serviceadapter.OperationInspect); updates < 1 {
		t.Fatalf("restarted inspections = %d; the pending assignment was not converged", updates)
	}
	pending, err = restartedCoordinator.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending after restart: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after restart = %+v; want the assignment settled", pending)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events after restart = %d; want exactly one", got)
	}
	after := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	assertMergeAnswer(t, after, mergeAdapterServiceID)
}

// TestMergeServiceCannotAnswerHumanRoleRequest proves the service-authorized
// work-ref result does not widen authority: a role/reviewer assignment causes no
// effect, and the service credential cannot resolve it.
func TestMergeServiceCannotAnswerHumanRoleRequest(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	access := createResponderService(t, harness, mergeAdapterServiceID)
	applied := applyMergePipeline(t, harness, "merge-human-role", "role/reviewer")
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-human-role")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "merge")
	action := actionByName(t, waiting, "merge")

	effects := &integrationMergeEffects{}
	coordinator := newIntegrationCoordinator(t, t.TempDir(), effects)
	handler := newResponderHandler(harness, access.token)
	adapter := newIntegrationMergeAdapter(t, access, handler, emptyNotificationSource{}, coordinator)
	if err := adapter.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if effects.executeCount() != 0 {
		t.Fatalf("merge effects = %d; a human-role request must cause none", effects.executeCount())
	}
	if resolves := handler.resolveAttempts(); resolves != 0 {
		t.Fatalf("resolve submissions = %d; the service answered a human-role request", resolves)
	}

	// A direct service attempt on the same role request is refused by the served
	// edge, and the request remains running and unanswered.
	current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if _, publicError := resolveRequest(t, harness, access.token, current.ExecutionID, action.ActionID, current.Version, action.Version, "pass", nil, "merge-human-role-refusal"); publicError.Code != api.CodeForbidden {
		t.Fatalf("service resolve of a human-role request = %q; want forbidden", publicError.Code)
	}
	current = inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if current.State != "running" {
		t.Fatalf("execution state = %q; want the unanswered request still running", current.State)
	}
	if actionByName(t, current, "merge").Decision != "" {
		t.Fatal("the human-role request was answered")
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 0 {
		t.Fatalf("committed request.resolved events = %d; want none", got)
	}

	// Cancel the execution so the request settles without an answer.
	result, publicError := harness.REST(harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: started.ExecutionID,
	}, &current.Version, "cancel-merge-human-role")
	if publicError.Code != "" {
		t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var cancelled api.ExecutionCancelResult
	harness.Decode(result, &cancelled)
	if cancelled.ExecutionVersion == 0 {
		t.Fatal("execution.cancel returned no committed version")
	}
}
