package executionstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// startedRequestExecution starts a request-only execution and activates its
// action, returning the action identity.
func startedRequestExecution(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope, executionID id.ExecutionID) id.ActionID {
	t.Helper()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, executionID, "start-"+string(executionID), requestOnlyAction())); err != nil {
		t.Fatalf("Start %s: %v", executionID, err)
	}
	action := snapshotdoc.DeriveActionID(executionID, 0)
	if _, err := adapter.Activate(context.Background(), activationUnit(scope, executionID, action, "activate-"+string(executionID))); err != nil {
		t.Fatalf("Activate %s: %v", executionID, err)
	}
	return action
}

func TestResolvePassSettlesRequestAndPersistsEvidence(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)
	unit.Commit.Answer.Provenance = []byte{0x00, 0x01, 0xfe}
	result, err := adapter.Resolve(context.Background(), unit)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.ExecutionVersion != 3 || result.ActionVersion != 3 || result.Decision != request.DecisionPass || result.Replayed {
		t.Fatalf("result = %+v; want versions 3 and a fresh pass", result)
	}

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionSucceeded || aggregate.CurrentAction != -1 {
		t.Fatalf("execution = %+v; want succeeded with no current action", aggregate)
	}
	settled := aggregate.Actions[0]
	if settled.State != kernel.ActionSucceeded || settled.Outputs["decision"] != "yes" {
		t.Fatalf("action = %+v; want succeeded with the settled response", settled)
	}
	if settled.Request == nil || settled.Request.Answer == nil {
		t.Fatalf("request answer was not persisted: %+v", settled.Request)
	}
	if settled.Request.Answer.Decision != kernel.DecisionPass || settled.Request.Answer.Actor != "user-1" {
		t.Fatalf("answer = %+v", settled.Request.Answer)
	}
	if !settled.Request.Answer.At.Equal(testNow) {
		t.Fatalf("answer time = %s; want %s", settled.Request.Answer.At, testNow)
	}
	if string(settled.Request.Answer.Provenance) != string([]byte{0x00, 0x01, 0xfe}) {
		t.Fatalf("provenance = %v; want exact bytes", settled.Request.Answer.Provenance)
	}
	row, err := backend.Read(context.Background(), scope, "actions", string(action))
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if row.Values["answer_provenance"] == nil || row.Values["request_decision"] != "pass" {
		t.Fatalf("answer columns = %v", row.Values)
	}
}

func TestResolveFailRetainsAnswerWithoutPromotingOutputs(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionFail, kernel.Outputs{"decision": "no"}, "resolve-1", "e", testNow)
	if _, err := adapter.Resolve(context.Background(), unit); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %s; want failed", aggregate.State)
	}
	if aggregate.Failure == nil || aggregate.Failure.Kind != kernel.FailureRuntime {
		t.Fatalf("failure = %+v; want runtime", aggregate.Failure)
	}
	settled := aggregate.Actions[0]
	if settled.State != kernel.ActionFailed {
		t.Fatalf("action state = %s; want failed", settled.State)
	}
	if settled.Outputs != nil {
		t.Fatalf("failed answer promoted outputs: %+v", settled.Outputs)
	}
	if settled.Request == nil || settled.Request.Answer == nil || settled.Request.Answer.Outputs["decision"] != "no" {
		t.Fatalf("failed answer was not retained: %+v", settled.Request)
	}
}

func TestResolveReplayReturnsOriginalResultAndAppliesNothing(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")

	first, err := adapter.Resolve(context.Background(), firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// The retry happens after the deadline: a settled replay must still work.
	clock = testNow.Add(2 * time.Hour)
	replay := replayResolveUnit(scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow.Add(2*time.Hour))
	second, err := adapter.Resolve(context.Background(), replay)
	if err != nil {
		t.Fatalf("replay Resolve: %v", err)
	}
	if !second.Replayed || second.ExecutionVersion != first.ExecutionVersion || second.ActionVersion != first.ActionVersion || second.Decision != first.Decision {
		t.Fatalf("replay = %+v; want %+v replayed", second, first)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != first.ExecutionVersion {
		t.Fatalf("replay applied a second write: version %d", aggregate.Version)
	}
}

func TestResolveChangedReplayContentIsRefused(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	if _, err := adapter.Resolve(context.Background(), firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	changed := replayResolveUnit(scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "no"}, "resolve-1", "f", testNow)
	_, err := adapter.Resolve(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
}

func TestResolveLateAnswerIsRefusedByTheDeadlineGuard(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)
	clock = testNow.Add(60 * time.Second) // the persisted deadline: equality expires
	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.DeadlineExpired)

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 2 || aggregate.Actions[0].State != kernel.ActionRunning || aggregate.Actions[0].Request.Answer != nil {
		t.Fatalf("late answer changed state: %+v", aggregate)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-resolve-1"); ok {
		t.Fatal("late answer must not publish its event intent")
	}
}

func TestResolveStaleVersionConflictsWithoutPartialState(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)
	unit.ExpectedExecutionVersion = 9
	unit.Commit.ExpectedExecutionVersion = 9

	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("stale resolve changed the execution: %+v", aggregate)
	}
}

func TestResolveRollsBackEveryEffectOnCommitFailure(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)

	backend.FailNextCommit(fault.New(fault.DependencyUnavailable, "simulated commit failure"))
	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.DependencyUnavailable)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Actions[0].State != kernel.ActionRunning || aggregate.Actions[0].Request.Answer != nil {
		t.Fatalf("failed resolve changed the action: %+v", aggregate.Actions[0])
	}
	if _, err := adapter.Resolve(context.Background(), unit); err != nil {
		t.Fatalf("retry after failed commit: %v", err)
	}
}

func TestResolveRefusesCrossScopeUnit(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-1", "e", testNow)
	unit.Owner = otherScope()
	unit.Receipt.Key.ProjectID = otherScope().ProjectID
	unit.Audit = nil
	unit.Events = nil

	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("cross-scope resolve changed the execution: %+v", aggregate)
	}
}

func TestResolveConcurrentOneWinner(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	base := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-race-a", "e", testNow)
	second := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-race-b", "e", testNow)

	units := []request.ResolveUnit{base, second}
	errs := make([]error, len(units))
	var wait sync.WaitGroup
	var gate sync.WaitGroup
	gate.Add(1)
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			gate.Wait()
			_, errs[index] = adapter.Resolve(context.Background(), units[index])
		}(index)
	}
	gate.Done()
	wait.Wait()

	winners := 0
	for index := range errs {
		if errs[index] == nil {
			winners++
			continue
		}
		requireClass(t, errs[index], fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly 1", winners)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 3 || aggregate.Actions[0].Version != 3 {
		t.Fatalf("versions = %d/%d; want exactly one settlement", aggregate.Version, aggregate.Actions[0].Version)
	}
}

func TestResolveVersusCancelRaceHasOneWinner(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	resolve := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-race", "e", testNow)
	cancel := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionCancelled,
		ActionID:                 action,
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionCancelled,
		NextCurrentAction:        new(-1),
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		At:                       testNow,
	}
	cancelUnit := transitionUnit(scope, "exec-1", cancel, "cancel-race")

	var wait sync.WaitGroup
	var gate sync.WaitGroup
	errs := make([]error, 2)
	gate.Add(1)
	wait.Add(2)
	go func() {
		defer wait.Done()
		gate.Wait()
		_, errs[0] = adapter.Resolve(context.Background(), resolve)
	}()
	go func() {
		defer wait.Done()
		gate.Wait()
		_, errs[1] = adapter.Transition(context.Background(), cancelUnit)
	}()
	gate.Done()
	wait.Wait()

	winners := 0
	for index := range errs {
		if errs[index] == nil {
			winners++
			continue
		}
		requireClass(t, errs[index], fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly one of resolve/cancel", winners)
	}
}
