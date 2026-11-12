package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

var _ engine.State = (*State)(nil)

func TestStateCommitPersistsOneAtomicTransition(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	commit := testCommit("exec-1", "act-1", 0, 0)
	commit.Deadline = testNow.Add(time.Hour)
	commit.Launch = &kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-1"),
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-1",
	}
	commit.InvalidateCredential = true

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if updated.Version != 1 {
		t.Fatalf("execution version = %d, want 1", updated.Version)
	}
	if updated.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want running", updated.State)
	}
	if len(updated.Actions) != 1 {
		t.Fatalf("action count = %d, want 1", len(updated.Actions))
	}
	action := updated.Actions[0]
	if action.Version != 1 || action.State != kernel.ActionRunning {
		t.Fatalf("action version/state = %d/%q, want 1/running", action.Version, action.State)
	}
	if !action.Deadline.Equal(commit.Deadline) {
		t.Fatalf("action deadline = %s, want %s", action.Deadline, commit.Deadline)
	}
	if !action.ActivatedAt.Equal(commit.At) {
		t.Fatalf("action activation = %s, want %s", action.ActivatedAt, commit.At)
	}
	if action.Container == nil || action.Container.Launch != *commit.Launch {
		t.Fatalf("launch intent = %+v, want %+v", action.Container, *commit.Launch)
	}
	if action.Container.WorkloadID != commit.Launch.WorkloadID {
		t.Fatalf("workload id = %q, want %q", action.Container.WorkloadID, commit.Launch.WorkloadID)
	}
	if !store.CredentialInvalidated("exec-1") {
		t.Fatal("credential invalidation intent was not persisted")
	}
	if store.CommitCount() != 1 {
		t.Fatalf("successful commit count = %d, want 1", store.CommitCount())
	}

	restarted := store.Handle()
	loaded, err := restarted.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	if loaded.Version != 1 || loaded.State != kernel.ExecutionRunning {
		t.Fatalf("fresh handle execution = version %d state %q, want 1/running", loaded.Version, loaded.State)
	}
	if !loaded.Actions[0].Deadline.Equal(commit.Deadline) {
		t.Fatalf("fresh handle deadline = %s, want %s", loaded.Actions[0].Deadline, commit.Deadline)
	}
	if loaded.Actions[0].Container.Launch.WorkloadID != "wl-1" {
		t.Fatalf("fresh handle workload id = %q, want wl-1", loaded.Actions[0].Container.Launch.WorkloadID)
	}
}

func TestStateVersionConflictRollsBackWholeCommit(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
		testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
	))
	state := store.Handle()

	commit := testCommit("exec-1", "act-a", 0, 0)
	commit.NextActionState = kernel.ActionSucceeded
	commit.PendingCancellations = []flow.ActionUpdate{{
		ActionID:        id.ActionID("act-b"),
		ExpectedVersion: 1,
		NextState:       kernel.ActionCancelled,
	}}

	_, err := state.Commit(context.Background(), commit)
	testClass(t, err, fault.VersionConflict)

	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() after conflict error = %v", err)
	}
	if loaded.Version != 0 || loaded.State != kernel.ExecutionQueued {
		t.Fatalf("execution after conflict = version %d state %q, want 0/queued", loaded.Version, loaded.State)
	}
	if loaded.Actions[0].State != kernel.ActionPending || loaded.Actions[0].Version != 0 {
		t.Fatalf("primary action after conflict = %q v%d, want pending v0", loaded.Actions[0].State, loaded.Actions[0].Version)
	}
	if loaded.Actions[1].State != kernel.ActionPending || loaded.Actions[1].Version != 0 {
		t.Fatalf("pending action after conflict = %q v%d, want pending v0", loaded.Actions[1].State, loaded.Actions[1].Version)
	}
	if store.CommitCount() != 0 {
		t.Fatalf("successful commits after conflict = %d, want 0", store.CommitCount())
	}

	stale := testCommit("exec-1", "act-a", 0, 5)
	_, err = state.Commit(context.Background(), stale)
	testClass(t, err, fault.VersionConflict)
}

func TestStateInjectedCommitFailureRollsBack(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	store.FailNextCommit(errors.New("state storage unavailable"))

	commit := testCommit("exec-1", "act-1", 0, 0)
	commit.Deadline = testNow.Add(time.Hour)
	if _, err := state.Commit(context.Background(), commit); err == nil {
		t.Fatal("Commit() error = nil, want injected failure")
	}

	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Version != 0 || loaded.State != kernel.ExecutionQueued {
		t.Fatalf("execution after injected failure = version %d state %q, want 0/queued", loaded.Version, loaded.State)
	}
	if loaded.Actions[0].State != kernel.ActionPending || loaded.Actions[0].Version != 0 {
		t.Fatalf("action after injected failure = %q v%d, want pending v0", loaded.Actions[0].State, loaded.Actions[0].Version)
	}
	if store.CommitCount() != 0 {
		t.Fatalf("successful commits after injected failure = %d, want 0", store.CommitCount())
	}

	if _, err := state.Commit(context.Background(), commit); err != nil {
		t.Fatalf("Commit() after one-shot failure error = %v, want nil", err)
	}
	if store.CommitCount() != 1 {
		t.Fatalf("successful commits after retry = %d, want 1", store.CommitCount())
	}
}

func TestStateConcurrentCommitsHaveOneWinner(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	type outcome struct {
		execution kernel.Execution
		err       error
	}

	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, nextState := range []kernel.ExecutionState{kernel.ExecutionRunning, kernel.ExecutionCancelled} {
		commit := testCommit("exec-1", "act-1", 0, 0)
		commit.NextExecutionState = nextState
		go func(commit flow.Commit) {
			<-start
			execution, err := state.Commit(context.Background(), commit)
			results <- outcome{execution: execution, err: err}
		}(commit)
	}
	close(start)

	var winner kernel.ExecutionState
	successes := 0
	conflicts := 0
	for range 2 {
		outcome := <-results
		switch {
		case outcome.err == nil:
			successes++
			winner = outcome.execution.State
		case func() bool {
			class, ok := fault.ClassOf(outcome.err)
			return ok && class == fault.VersionConflict
		}():
			conflicts++
		default:
			t.Fatalf("unexpected concurrent commit error = %v", outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent commit outcomes = %d success, %d conflict; want one each", successes, conflicts)
	}

	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Version != 1 {
		t.Fatalf("execution version after race = %d, want 1", loaded.Version)
	}
	if loaded.State != winner {
		t.Fatalf("execution state = %q, want winning state %q", loaded.State, winner)
	}
}

func TestStateLoadReturnsDefensiveCopies(t *testing.T) {
	action := testContainerAction("act-1", "build", 0, kernel.ActionRunning, 1)
	action.Outputs = kernel.Outputs{"candidate": "settled"}
	action.StagedOutputs = kernel.Outputs{"candidate": "staged"}
	action.StagedOutputsSet = true
	action.Container.Observation = kernel.ExitedObservation(1, kernel.ConfirmedStop("observed"))
	execution := testExecution("exec-1", "ws-1", action)

	store := NewStore()
	store.Seed(execution)
	execution.Actions[0].Outputs["candidate"] = "mutated-after-seed"

	state := store.Handle()
	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loaded.Actions[0].Outputs["candidate"] = "tampered"
	loaded.Actions[0].StagedOutputs["candidate"] = "tampered"
	loaded.Actions[0].Container.Definition.Commands[0] = "tampered"
	loaded.Actions[0].Container.Launch.WorkloadID = "tampered"
	loaded.Actions[0].Container.Observation.Kind = kernel.ObservationUnknown
	*loaded.Actions[0].Container.Observation.ExitCode = 99
	loaded.Actions[0].Container.Observation.Stop.State = kernel.StopStateUnconfirmed
	loaded.Actions[0].State = kernel.ActionFailed
	loaded.State = kernel.ExecutionFailed

	reloaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	action = reloaded.Actions[0]
	if action.Outputs["candidate"] != "settled" {
		t.Fatalf("settled output = %q, want settled", action.Outputs["candidate"])
	}
	if action.StagedOutputs["candidate"] != "staged" {
		t.Fatalf("staged output = %q, want staged", action.StagedOutputs["candidate"])
	}
	if action.Container.Definition.Commands[0] != "run" {
		t.Fatalf("command = %q, want run", action.Container.Definition.Commands[0])
	}
	if action.Container.Launch.WorkloadID != "wl-act-1" {
		t.Fatalf("workload id = %q, want wl-act-1", action.Container.Launch.WorkloadID)
	}
	if action.Container.Observation.Kind != kernel.ObservationExited {
		t.Fatalf("observation kind = %q, want exited", action.Container.Observation.Kind)
	}
	code, ok := action.Container.Observation.ExitStatus()
	if !ok || code != 1 {
		t.Fatalf("exit status = (%d, %v), want (1, true)", code, ok)
	}
	if !action.Container.Observation.StopConfirmed() {
		t.Fatal("stop evidence was not copied deeply")
	}
	if action.State != kernel.ActionRunning {
		t.Fatalf("action state = %q, want running", action.State)
	}
	if reloaded.State != kernel.ExecutionQueued {
		t.Fatalf("execution state = %q, want queued", reloaded.State)
	}
}

func TestStateCommitCopiesCallerValues(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-c", "build", 0, kernel.ActionPending, 0),
		testRequestAction("act-r", "review", 1, kernel.ActionPending, 0),
	))
	state := store.Handle()

	observation := kernel.ExitedObservation(7, kernel.ConfirmedStop("observed"))
	launch := &kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-act-c"),
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-c",
	}
	stopEvidence := kernel.PendingStop("pending barrier")
	containerCommit := testCommit("exec-1", "act-c", 0, 0)
	containerCommit.Observation = &observation
	containerCommit.Launch = launch
	containerCommit.StopEvidence = &stopEvidence
	if _, err := state.Commit(context.Background(), containerCommit); err != nil {
		t.Fatalf("container Commit() error = %v", err)
	}

	*observation.ExitCode = 99
	observation.Stop.State = kernel.StopStateUnconfirmed
	launch.IdempotencyKey = "tampered"
	stopEvidence.Detail = "tampered"

	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"field": {Kind: definition.SubjectLiteral, Literal: "value"},
	})
	answer := &kernel.Answer{
		Decision:   kernel.DecisionPass,
		Outputs:    kernel.Outputs{"decision": "ok"},
		Actor:      id.PrincipalID("principal-1"),
		At:         testNow,
		Provenance: []byte("proof"),
	}
	requestCommit := testCommit("exec-1", "act-r", 1, 0)
	requestCommit.Subject = &subject
	requestCommit.Answer = answer
	if _, err := state.Commit(context.Background(), requestCommit); err != nil {
		t.Fatalf("request Commit() error = %v", err)
	}

	answer.Outputs["decision"] = "tampered"
	answer.Provenance[0] = 'X'

	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	container := loaded.Actions[0].Container
	if container.Launch.IdempotencyKey != "idem-c" {
		t.Fatalf("launch idempotency key = %q, want idem-c", container.Launch.IdempotencyKey)
	}
	code, ok := container.Observation.ExitStatus()
	if !ok || code != 7 {
		t.Fatalf("observation exit = (%d, %v), want (7, true)", code, ok)
	}
	if !container.Observation.StopConfirmed() {
		t.Fatal("stop evidence was mutated through the caller's pointer")
	}
	if loaded.Actions[0].Stop.Detail != "pending barrier" {
		t.Fatalf("stop detail = %q, want pending barrier", loaded.Actions[0].Stop.Detail)
	}
	request := loaded.Actions[1].Request
	if request == nil || request.Answer == nil {
		t.Fatal("request answer was not persisted")
	}
	if request.Answer.Outputs["decision"] != "ok" {
		t.Fatalf("answer output = %q, want ok", request.Answer.Outputs["decision"])
	}
	if string(request.Answer.Provenance) != "proof" {
		t.Fatalf("answer provenance = %q, want proof", request.Answer.Provenance)
	}
	if request.Subject == nil || request.Subject.Len() != 1 {
		t.Fatal("request subject was not persisted")
	}
}

func TestStateMissingExecutionIsNotFound(t *testing.T) {
	store := NewStore()
	state := store.Handle()

	if _, err := state.Load(context.Background(), "missing"); err != nil {
		testClass(t, err, fault.NotFound)
	} else {
		t.Fatal("Load() missing execution error = nil, want not_found")
	}

	_, err := state.Commit(context.Background(), testCommit("missing", "act-1", 0, 0))
	testClass(t, err, fault.NotFound)
	if _, ok := store.Execution("missing"); ok {
		t.Fatal("Commit() silently created an execution that was never admitted")
	}
}

func TestStateRetainsDistinctObservationEvidence(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
		testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
		testContainerAction("act-c", "c", 2, kernel.ActionPending, 0),
	))
	state := store.Handle()

	observations := []kernel.Observation{
		kernel.UnknownObservation(),
		kernel.AbsentObservation(),
		kernel.NeverStarted("runtime confirmed no object was created"),
	}
	for index, observation := range observations {
		actionID := string("act-" + string(rune('a'+index)))
		commit := testCommit("exec-1", actionID, uint64(index), 0)
		commit.Observation = &observation
		if _, err := state.Commit(context.Background(), commit); err != nil {
			t.Fatalf("Commit(%s) error = %v", actionID, err)
		}
	}

	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	unknown := loaded.Actions[0].Container.Observation
	absent := loaded.Actions[1].Container.Observation
	neverStarted := loaded.Actions[2].Container.Observation

	if unknown.Kind != kernel.ObservationUnknown || unknown.ProofOfNonexecution() || unknown.StopConfirmed() {
		t.Fatalf("unknown observation = %+v", unknown)
	}
	if absent.Kind != kernel.ObservationAbsent || absent.ProofOfNonexecution() || absent.StopConfirmed() {
		t.Fatalf("absent observation = %+v", absent)
	}
	if neverStarted.Kind != kernel.ObservationNeverStarted || !neverStarted.ProofOfNonexecution() {
		t.Fatalf("never-started observation = %+v", neverStarted)
	}
	if neverStarted.Proof != "runtime confirmed no object was created" {
		t.Fatalf("never-started proof = %q", neverStarted.Proof)
	}
}

func TestStateAppliesOutputSettlementDistinctly(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	commit := testCommit("exec-1", "act-1", 0, 0)
	if _, err := state.Commit(context.Background(), commit); err != nil {
		t.Fatalf("activation Commit() error = %v", err)
	}

	stageNull := testCommit("exec-1", "act-1", 1, 1)
	stageNull.Output = flow.OutputChange{Action: flow.OutputStage, Values: nil}
	if _, err := state.Commit(context.Background(), stageNull); err != nil {
		t.Fatalf("null stage Commit() error = %v", err)
	}
	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() after null stage error = %v", err)
	}
	if !loaded.Actions[0].StagedOutputsSet {
		t.Fatal("null stage did not set the staging presence marker")
	}
	if loaded.Actions[0].StagedOutputs != nil {
		t.Fatalf("null stage values = %#v, want nil", loaded.Actions[0].StagedOutputs)
	}

	stageValues := testCommit("exec-1", "act-1", 2, 2)
	stageValues.Output = flow.OutputChange{
		Action: flow.OutputStage,
		Values: kernel.Outputs{"candidate": "value"},
	}
	if _, err := state.Commit(context.Background(), stageValues); err != nil {
		t.Fatalf("value stage Commit() error = %v", err)
	}
	loaded, _ = state.Load(context.Background(), "exec-1")
	if !loaded.Actions[0].StagedOutputsSet || loaded.Actions[0].StagedOutputs["candidate"] != "value" {
		t.Fatalf("staged values = %#v marker=%v", loaded.Actions[0].StagedOutputs, loaded.Actions[0].StagedOutputsSet)
	}

	promote := testCommit("exec-1", "act-1", 3, 3)
	promote.Output = flow.OutputChange{
		Action: flow.OutputPromote,
		Values: kernel.Outputs{"candidate": "value"},
	}
	if _, err := state.Commit(context.Background(), promote); err != nil {
		t.Fatalf("promote Commit() error = %v", err)
	}
	loaded, _ = state.Load(context.Background(), "exec-1")
	if loaded.Actions[0].Outputs["candidate"] != "value" {
		t.Fatalf("settled output = %q, want value", loaded.Actions[0].Outputs["candidate"])
	}
	if loaded.Actions[0].StagedOutputs != nil || loaded.Actions[0].StagedOutputsSet {
		t.Fatalf("promotion left staging = %#v marker=%v", loaded.Actions[0].StagedOutputs, loaded.Actions[0].StagedOutputsSet)
	}

	stageAgain := testCommit("exec-1", "act-1", 4, 4)
	stageAgain.Output = flow.OutputChange{
		Action: flow.OutputStage,
		Values: kernel.Outputs{"candidate": "discarded"},
	}
	if _, err := state.Commit(context.Background(), stageAgain); err != nil {
		t.Fatalf("second stage Commit() error = %v", err)
	}
	discard := testCommit("exec-1", "act-1", 5, 5)
	discard.Output = flow.OutputChange{Action: flow.OutputDiscard}
	if _, err := state.Commit(context.Background(), discard); err != nil {
		t.Fatalf("discard Commit() error = %v", err)
	}
	loaded, _ = state.Load(context.Background(), "exec-1")
	if loaded.Actions[0].StagedOutputs != nil || loaded.Actions[0].StagedOutputsSet {
		t.Fatalf("discard left staging = %#v marker=%v", loaded.Actions[0].StagedOutputs, loaded.Actions[0].StagedOutputsSet)
	}
	if loaded.Actions[0].Outputs["candidate"] != "value" {
		t.Fatalf("discard changed settled output = %q, want value", loaded.Actions[0].Outputs["candidate"])
	}

	none := testCommit("exec-1", "act-1", 6, 6)
	none.Output = flow.OutputChange{Action: flow.OutputNone}
	if _, err := state.Commit(context.Background(), none); err != nil {
		t.Fatalf("none Commit() error = %v", err)
	}
	loaded, _ = state.Load(context.Background(), "exec-1")
	if loaded.Actions[0].Outputs["candidate"] != "value" || loaded.Actions[0].StagedOutputsSet {
		t.Fatalf("OutputNone changed outputs = %#v marker=%v", loaded.Actions[0].Outputs, loaded.Actions[0].StagedOutputsSet)
	}
}

func TestStatePersistsCleanupProgress(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionRunning, 1)))
	state := store.Handle()

	pending := testCommit("exec-1", "act-1", 0, 1)
	pending.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkloadID:  id.WorkloadID("wl-1"),
		WorkspaceID: id.WorkspaceID("ws-1"),
		Attempts:    2,
		LastError:   "runtime unavailable",
	}
	if _, err := state.Commit(context.Background(), pending); err != nil {
		t.Fatalf("pending cleanup Commit() error = %v", err)
	}

	restarted := store.Handle()
	loaded, err := restarted.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	if loaded.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", loaded.Cleanup.State)
	}
	if loaded.Cleanup.WorkloadID != "wl-1" || loaded.Cleanup.WorkspaceID != "ws-1" {
		t.Fatalf("cleanup resources = %q/%q", loaded.Cleanup.WorkloadID, loaded.Cleanup.WorkspaceID)
	}
	if loaded.Cleanup.Attempts != 2 || loaded.Cleanup.LastError != "runtime unavailable" {
		t.Fatalf("cleanup attribution = %d/%q", loaded.Cleanup.Attempts, loaded.Cleanup.LastError)
	}

	complete := testCommit("exec-1", "act-1", 1, 2)
	complete.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupComplete,
		WorkloadID:  id.WorkloadID("wl-1"),
		WorkspaceID: id.WorkspaceID("ws-1"),
		Attempts:    3,
	}
	if _, err := state.Commit(context.Background(), complete); err != nil {
		t.Fatalf("complete cleanup Commit() error = %v", err)
	}
	loaded, _ = state.Load(context.Background(), "exec-1")
	if loaded.Cleanup.State != kernel.CleanupComplete || loaded.Cleanup.Attempts != 3 {
		t.Fatalf("completed cleanup = %+v", loaded.Cleanup)
	}
}

func TestStateScanDueUsesExplicitTimeAndStableOrder(t *testing.T) {
	dueEarly := testContainerAction("act-d", "d", 0, kernel.ActionRunning, 1)
	dueEarly.Deadline = testNow.Add(-time.Hour)
	dueNow := testContainerAction("act-a", "a", 0, kernel.ActionRunning, 1)
	dueNow.Deadline = testNow
	later := testContainerAction("act-b", "b", 0, kernel.ActionRunning, 1)
	later.Deadline = testNow.Add(time.Hour)
	pending := testContainerAction("act-c", "c", 0, kernel.ActionPending, 0)
	pending.Deadline = testNow

	store := NewStore()
	store.Seed(
		testExecution("exec-b", "ws-b", later),
		testExecution("exec-d", "ws-d", dueEarly),
		testExecution("exec-a", "ws-a", dueNow),
		testExecution("exec-c", "ws-c", pending),
	)
	state := store.Handle()

	ids, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: testNow})
	if err != nil {
		t.Fatalf("Scan(due) error = %v", err)
	}
	want := []id.ExecutionID{"exec-a", "exec-d"}
	if !equalExecutionIDs(ids, want) {
		t.Fatalf("Scan(due at %s) = %v, want %v", testNow, ids, want)
	}

	ids, err = state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: testNow.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Scan(due early) error = %v", err)
	}
	if !equalExecutionIDs(ids, []id.ExecutionID{"exec-d"}) {
		t.Fatalf("Scan(due early) = %v, want [exec-d]", ids)
	}

	ids, err = state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: testNow.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("Scan(due later) error = %v", err)
	}
	if !equalExecutionIDs(ids, []id.ExecutionID{"exec-a", "exec-d"}) {
		t.Fatalf("Scan(due later) = %v, want [exec-a exec-d]", ids)
	}

	ids, err = state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: testNow, Limit: 1})
	if err != nil {
		t.Fatalf("Scan(due limited) error = %v", err)
	}
	if !equalExecutionIDs(ids, []id.ExecutionID{"exec-a"}) {
		t.Fatalf("Scan(due limited) = %v, want [exec-a]", ids)
	}
}

func TestStateScanUnfinishedIncludesIntentsAndCleanup(t *testing.T) {
	clean := testExecution("exec-clean", "ws-clean", testContainerAction("act-clean", "clean", 0, kernel.ActionSucceeded, 1))
	clean.State = kernel.ExecutionSucceeded
	clean.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}

	cleanup := testExecution("exec-cleanup", "ws-cleanup", testContainerAction("act-cleanup", "cleanup", 0, kernel.ActionRunning, 1))
	cleanup.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  id.WorkloadID("wl-cleanup"),
		WorkspaceID: id.WorkspaceID("ws-cleanup"),
		Attempts:    1,
		LastError:   "remove failed",
	}

	intent := testExecution("exec-intent", "ws-intent", testContainerAction("act-intent", "intent", 0, kernel.ActionRunning, 1))

	store := NewStore()
	store.Seed(clean, cleanup, intent)
	state := store.Handle()

	stopCommit := testCommit("exec-intent", "act-intent", 0, 1)
	stopCommit.Stop = &flow.StopIntent{
		WorkloadID:  id.WorkloadID("wl-intent"),
		WorkspaceID: id.WorkspaceID("ws-intent"),
	}
	stopCommit.Cleanup = &flow.CleanupIntent{
		WorkloadID:  id.WorkloadID("wl-intent"),
		WorkspaceID: id.WorkspaceID("ws-intent"),
		Reason:      "cancel",
	}
	if _, err := state.Commit(context.Background(), stopCommit); err != nil {
		t.Fatalf("intent Commit() error = %v", err)
	}

	if stops := store.PendingStops(); len(stops) != 1 || stops[0].WorkloadID != "wl-intent" {
		t.Fatalf("pending stops = %+v, want one for wl-intent", stops)
	}
	if cleanups := store.PendingCleanups(); len(cleanups) != 1 || cleanups[0].Reason != "cancel" {
		t.Fatalf("pending cleanups = %+v, want one with reason cancel", cleanups)
	}

	ids, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow})
	if err != nil {
		t.Fatalf("Scan(unfinished) error = %v", err)
	}
	if !equalExecutionIDs(ids, []id.ExecutionID{"exec-cleanup", "exec-intent"}) {
		t.Fatalf("Scan(unfinished) = %v, want [exec-cleanup exec-intent]", ids)
	}

	complete := testCommit("exec-cleanup", "act-cleanup", 0, 1)
	complete.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupComplete,
		WorkloadID:  id.WorkloadID("wl-cleanup"),
		WorkspaceID: id.WorkspaceID("ws-cleanup"),
		Attempts:    2,
	}
	if _, err := state.Commit(context.Background(), complete); err != nil {
		t.Fatalf("complete cleanup Commit() error = %v", err)
	}

	ids, err = state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow})
	if err != nil {
		t.Fatalf("Scan(unfinished) after complete error = %v", err)
	}
	if !equalExecutionIDs(ids, []id.ExecutionID{"exec-intent"}) {
		t.Fatalf("Scan(unfinished) after complete = %v, want [exec-intent]", ids)
	}
}

func TestStateScanUnfinishedIncludesUnactivatedWork(t *testing.T) {
	// K-RECOVER "after settlement/before next activation": the settled action
	// is done, the successor is still pending and no action is active. The
	// pending action has no persisted deadline, so only the unfinished scan can
	// resume the execution.
	settled := testContainerAction("act-settled", "build", 0, kernel.ActionSucceeded, 2)
	settled.Stop = kernel.ConfirmedStop("workload exited and stop was confirmed")
	window := testExecution("exec-window", "ws-window", settled,
		testContainerAction("act-next", "test", 1, kernel.ActionPending, 0))
	window.State = kernel.ExecutionRunning
	window.CurrentAction = -1
	window.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}

	// K-RECOVER "before intent": the execution was admitted but never
	// activated, so it is queued with a pending action and no active action.
	queued := testExecution("exec-queued", "ws-queued",
		testContainerAction("act-first", "first", 0, kernel.ActionPending, 0))

	done := testExecution("exec-done", "ws-done",
		testContainerAction("act-done", "done", 0, kernel.ActionSucceeded, 1))
	done.State = kernel.ExecutionSucceeded
	done.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}

	failed := testExecution("exec-failed", "ws-failed",
		testContainerAction("act-failed", "failed", 0, kernel.ActionFailed, 1))
	failed.State = kernel.ExecutionFailed
	failed.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}

	cancelled := testExecution("exec-cancelled", "ws-cancelled",
		testContainerAction("act-cancelled", "cancelled", 0, kernel.ActionCancelled, 1))
	cancelled.State = kernel.ExecutionCancelled
	cancelled.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}

	cleaned := testExecution("exec-cleaned", "ws-cleaned",
		testContainerAction("act-cleaned", "cleaned", 0, kernel.ActionSucceeded, 1))
	cleaned.State = kernel.ExecutionSucceeded
	cleaned.Cleanup = kernel.Cleanup{State: kernel.CleanupComplete}

	store := NewStore()
	store.Seed(window, queued, done, failed, cancelled, cleaned)
	state := store.Handle()

	ids, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow})
	if err != nil {
		t.Fatalf("Scan(unfinished) error = %v", err)
	}
	want := []id.ExecutionID{"exec-queued", "exec-window"}
	if !equalExecutionIDs(ids, want) {
		t.Fatalf("Scan(unfinished) = %v, want %v", ids, want)
	}

	// A pending successor has no persisted deadline, so the due scan alone
	// cannot recover the settlement window.
	due, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: testNow.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("Scan(due) error = %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("Scan(due) = %v, want none; the unfinished scan must recover the window", due)
	}
}

func TestStateContextCancellation(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := state.Load(ctx, "exec-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := state.Commit(ctx, testCommit("exec-1", "act-1", 0, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := state.Scan(ctx, engine.ScanRequest{Kind: engine.ScanDue, Now: testNow}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan(canceled) error = %v, want context.Canceled", err)
	}
	if store.CommitCount() != 0 {
		t.Fatalf("successful commits after canceled calls = %d, want 0", store.CommitCount())
	}
}

func TestStateStoresAreIndependent(t *testing.T) {
	first := NewStore()
	second := NewStore()
	first.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))

	if _, ok := second.Execution("exec-1"); ok {
		t.Fatal("second store saw the first store's execution")
	}
	if _, err := second.Handle().Load(context.Background(), "exec-1"); err == nil {
		t.Fatal("second store Load() error = nil, want not_found")
	}

	firstState := first.Handle()
	secondState := second.Handle()
	if _, err := firstState.Commit(context.Background(), testCommit("exec-1", "act-1", 0, 0)); err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	if _, err := secondState.Load(context.Background(), "exec-1"); err == nil {
		t.Fatal("commit in first store appeared in second store")
	}
}

func TestExecutionOnlyCommitCancelsQueuedWork(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
		testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
	))
	state := store.Handle()

	nextCurrent := -1
	commit := testExecutionOnlyCommit("exec-1", 0)
	commit.NextCurrentAction = &nextCurrent
	commit.Failure = &kernel.Failure{
		Kind:   kernel.FailureWorkspaceLost,
		Reason: "workspace_missing",
		Detail: "retained workspace disappeared",
	}
	commit.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkspaceID: id.WorkspaceID("ws-1"),
	}
	commit.InvalidateCredential = true
	commit.PendingCancellations = []flow.ActionUpdate{
		{ActionID: id.ActionID("act-a"), ExpectedVersion: 0, NextState: kernel.ActionCancelled},
		{ActionID: id.ActionID("act-b"), ExpectedVersion: 0, NextState: kernel.ActionCancelled},
	}

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("execution-only Commit() error = %v", err)
	}
	if updated.Version != 1 || updated.State != kernel.ExecutionFailed || updated.CurrentAction != -1 {
		t.Fatalf("updated execution = version %d state %q current %d, want 1/failed/-1",
			updated.Version, updated.State, updated.CurrentAction)
	}
	if store.CommitCount() != 1 {
		t.Fatalf("successful commit count = %d, want 1", store.CommitCount())
	}

	loaded, err := store.Handle().Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	if loaded.Version != 1 || loaded.State != kernel.ExecutionFailed {
		t.Fatalf("fresh handle execution = version %d state %q, want 1/failed", loaded.Version, loaded.State)
	}
	if loaded.CurrentAction != -1 {
		t.Fatalf("fresh handle current action = %d, want -1", loaded.CurrentAction)
	}
	for index, wantID := range []id.ActionID{"act-a", "act-b"} {
		action := loaded.Actions[index]
		if action.ID != wantID || action.State != kernel.ActionCancelled || action.Version != 1 {
			t.Fatalf("action %d = id %q state %q version %d, want %q/cancelled/1",
				index, action.ID, action.State, action.Version, wantID)
		}
	}
	if loaded.Failure == nil || loaded.Failure.Kind != kernel.FailureWorkspaceLost {
		t.Fatalf("failure = %+v, want kind %q", loaded.Failure, kernel.FailureWorkspaceLost)
	}
	if loaded.Cleanup.State != kernel.CleanupPending || loaded.Cleanup.WorkspaceID != "ws-1" {
		t.Fatalf("cleanup = %+v, want pending for ws-1", loaded.Cleanup)
	}
	if !store.CredentialInvalidated("exec-1") {
		t.Fatal("execution-only commit did not persist credential invalidation")
	}
	if store.CommitCount() != 1 {
		t.Fatalf("successful commit count after reload = %d, want 1", store.CommitCount())
	}
}

func TestExecutionOnlyCommitCancelsQueuedExecution(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
		testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
	))
	state := store.Handle()

	nextCurrent := -1
	commit := testExecutionOnlyCommit("exec-1", 0)
	commit.NextExecutionState = kernel.ExecutionCancelled
	commit.NextCurrentAction = &nextCurrent
	commit.Cleanup = &flow.CleanupIntent{
		WorkspaceID: id.WorkspaceID("ws-1"),
		Reason:      "cancelled",
	}
	commit.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkspaceID: id.WorkspaceID("ws-1"),
	}
	commit.InvalidateCredential = true
	commit.PendingCancellations = []flow.ActionUpdate{
		{ActionID: id.ActionID("act-a"), ExpectedVersion: 0, NextState: kernel.ActionCancelled},
		{ActionID: id.ActionID("act-b"), ExpectedVersion: 0, NextState: kernel.ActionCancelled},
	}

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("execution-only cancel Commit() error = %v", err)
	}
	if updated.Version != 1 || updated.State != kernel.ExecutionCancelled || updated.CurrentAction != -1 {
		t.Fatalf("updated execution = version %d state %q current %d, want 1/cancelled/-1",
			updated.Version, updated.State, updated.CurrentAction)
	}

	loaded, err := store.Handle().Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	if loaded.State != kernel.ExecutionCancelled || loaded.CurrentAction != -1 || loaded.Failure != nil {
		t.Fatalf("fresh handle execution = state %q current %d failure %+v, want cancelled/-1/nil",
			loaded.State, loaded.CurrentAction, loaded.Failure)
	}
	for index, wantID := range []id.ActionID{"act-a", "act-b"} {
		action := loaded.Actions[index]
		if action.ID != wantID || action.State != kernel.ActionCancelled || action.Version != 1 {
			t.Fatalf("action %d = id %q state %q version %d, want %q/cancelled/1",
				index, action.ID, action.State, action.Version, wantID)
		}
	}
	if loaded.Cleanup.State != kernel.CleanupPending || loaded.Cleanup.WorkspaceID != "ws-1" {
		t.Fatalf("cleanup = %+v, want pending for ws-1", loaded.Cleanup)
	}
	if !store.CredentialInvalidated("exec-1") {
		t.Fatal("execution-only cancel did not persist credential invalidation")
	}
	cleanups := store.PendingCleanups()
	if len(cleanups) != 1 || cleanups[0].Reason != "cancelled" || cleanups[0].WorkspaceID != "ws-1" {
		t.Fatalf("pending cleanups = %+v, want one cancelled cleanup for ws-1", cleanups)
	}
	if store.CommitCount() != 1 {
		t.Fatalf("successful commit count = %d, want 1", store.CommitCount())
	}
}

func TestExecutionOnlyCommitRejectsPrimaryOnlyEvidence(t *testing.T) {
	observation := kernel.UnknownObservation()
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"field": {Kind: definition.SubjectLiteral, Literal: "value"},
	})
	answer := &kernel.Answer{Decision: kernel.DecisionPass, Outputs: kernel.Outputs{}}
	launch := &kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-act-a"),
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-act-a",
	}
	stopEvidence := kernel.PendingStop("pending barrier")

	cases := []struct {
		name  string
		apply func(*flow.Commit)
	}{
		{"observation", func(commit *flow.Commit) { commit.Observation = &observation }},
		{"answer", func(commit *flow.Commit) { commit.Answer = answer }},
		{"subject", func(commit *flow.Commit) { commit.Subject = &subject }},
		{"launch", func(commit *flow.Commit) { commit.Launch = launch }},
		{"stop_evidence", func(commit *flow.Commit) { commit.StopEvidence = &stopEvidence }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-a", "a", 0, kernel.ActionPending, 0)))
			state := store.Handle()

			commit := testExecutionOnlyCommit("exec-1", 0)
			tc.apply(&commit)

			_, err := state.Commit(context.Background(), commit)
			testClass(t, err, fault.InvalidInput)
			if !strings.Contains(err.Error(), "requires a primary action") {
				t.Fatalf("Commit() error = %v, want a primary-action requirement", err)
			}
			testAssertQueuedUnchanged(t, store, state, "act-a")
		})
	}
}

func TestExecutionOnlyCommitRejectsActionStateWithoutPrimaryAction(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-a", "a", 0, kernel.ActionPending, 0)))
	state := store.Handle()

	commit := testExecutionOnlyCommit("exec-1", 0)
	commit.NextActionState = kernel.ActionRunning

	_, err := state.Commit(context.Background(), commit)
	testClass(t, err, fault.InvalidInput)
	if !strings.Contains(err.Error(), "requires a primary action") {
		t.Fatalf("Commit() error = %v, want a primary-action requirement", err)
	}
	testAssertQueuedUnchanged(t, store, state, "act-a")
}

func TestExecutionOnlyCommitRejectsNonZeroActionVersionOrOutputStage(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*flow.Commit)
	}{
		{"non_zero_action_version", func(commit *flow.Commit) { commit.ExpectedActionVersion = 1 }},
		{"output_stage", func(commit *flow.Commit) {
			commit.Output = flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"candidate": "value"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			store.Seed(testExecution(
				"exec-1",
				"ws-1",
				testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
				testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
			))
			state := store.Handle()

			commit := testExecutionOnlyCommit("exec-1", 0)
			tc.apply(&commit)

			_, err := state.Commit(context.Background(), commit)
			testClass(t, err, fault.InvalidInput)
			testAssertQueuedUnchanged(t, store, state, "act-a", "act-b")
		})
	}
}

func TestExecutionOnlyCommitStalePendingCancellationRollsBack(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-a", "a", 0, kernel.ActionPending, 0),
		testContainerAction("act-b", "b", 1, kernel.ActionPending, 0),
	))
	state := store.Handle()

	nextCurrent := -1
	commit := testExecutionOnlyCommit("exec-1", 0)
	commit.NextCurrentAction = &nextCurrent
	commit.Failure = &kernel.Failure{Kind: kernel.FailureCancelled, Reason: "cancelled"}
	commit.CleanupUpdate = &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkspaceID: id.WorkspaceID("ws-1"),
	}
	commit.InvalidateCredential = true
	commit.PendingCancellations = []flow.ActionUpdate{
		{ActionID: id.ActionID("act-a"), ExpectedVersion: 0, NextState: kernel.ActionCancelled},
		{ActionID: id.ActionID("act-b"), ExpectedVersion: 1, NextState: kernel.ActionCancelled},
	}

	_, err := state.Commit(context.Background(), commit)
	testClass(t, err, fault.VersionConflict)
	if !strings.Contains(err.Error(), "pending action") || !strings.Contains(err.Error(), "act-b") {
		t.Fatalf("Commit() error = %v, want a stale pending-cancellation conflict for act-b", err)
	}
	testAssertQueuedUnchanged(t, store, state, "act-a", "act-b")
}

func testExecutionOnlyCommit(executionID string, executionVersion uint64) flow.Commit {
	return flow.Commit{
		ExecutionID:              id.ExecutionID(executionID),
		ExpectedExecutionVersion: executionVersion,
		NextExecutionState:       kernel.ExecutionFailed,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		At:                       testNow,
	}
}

func testAssertQueuedUnchanged(t *testing.T, store *Store, state *State, actionIDs ...id.ActionID) {
	t.Helper()
	loaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() after rejected commit error = %v", err)
	}
	if loaded.Version != 0 || loaded.State != kernel.ExecutionQueued || loaded.CurrentAction != -1 {
		t.Fatalf("execution after rejected commit = version %d state %q current %d, want 0/queued/-1",
			loaded.Version, loaded.State, loaded.CurrentAction)
	}
	if len(loaded.Actions) != len(actionIDs) {
		t.Fatalf("action count after rejected commit = %d, want %d", len(loaded.Actions), len(actionIDs))
	}
	for index, actionID := range actionIDs {
		action := loaded.Actions[index]
		if action.ID != actionID || action.State != kernel.ActionPending || action.Version != 0 {
			t.Fatalf("action %d after rejected commit = id %q state %q version %d, want %q/pending/0",
				index, action.ID, action.State, action.Version, actionID)
		}
	}
	if loaded.Failure != nil {
		t.Fatalf("failure after rejected commit = %+v, want nil", loaded.Failure)
	}
	if loaded.Cleanup != (kernel.Cleanup{}) {
		t.Fatalf("cleanup after rejected commit = %+v, want zero", loaded.Cleanup)
	}
	if store.CommitCount() != 0 {
		t.Fatalf("successful commits after rejected commit = %d, want 0", store.CommitCount())
	}
	if store.CredentialInvalidated("exec-1") {
		t.Fatal("rejected commit persisted credential invalidation")
	}
}

func equalExecutionIDs(got, want []id.ExecutionID) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
