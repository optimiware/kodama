package testkit

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// These tests pin the retire-then-replace contract at the fake
// state boundary. The fake is not a permission gate that always says yes: a
// commit that would persist a retiring identity without affirmative
// nonexecution proof, retire somebody else's identity, downgrade a retiring
// identity, or replace it with an unproven identity must be refused with
// fault.InvalidInput and must leave the whole persisted transition untouched.

// testRetireProof is the affirmative nonexecution proof the runtime fake
// reports for a prepared identity that never received a start request.
const testRetireProof = "runtime fake: prepared identity never received a start request"

func testWorkloadID(actionID string) id.WorkloadID {
	return id.WorkloadID("wl-" + actionID)
}

// testIssuedAction is one running container action whose launch intent was
// persisted as issued before the start request.
func testIssuedAction(actionID string) kernel.Action {
	action := testContainerAction(actionID, "build", 0, kernel.ActionRunning, 0)
	action.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     action.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-" + actionID,
	}
	return action
}

// testRetiringAction is the persisted shape after a successful retiring
// commit: the identity is marked retiring and its never-started proof is
// stored in the same container row.
func testRetiringAction(actionID, proof string) kernel.Action {
	action := testIssuedAction(actionID)
	action.Container.Launch.State = kernel.LaunchRetiring
	action.Container.Observation = kernel.NeverStarted(proof)
	return action
}

func testRetiringCommit(executionID, actionID string, executionVersion, actionVersion uint64, workloadID id.WorkloadID, proof string) flow.Commit {
	commit := testCommit(executionID, actionID, executionVersion, actionVersion)
	commit.Launch = &kernel.LaunchIntent{
		WorkloadID:     workloadID,
		State:          kernel.LaunchRetiring,
		IdempotencyKey: "idem-" + actionID,
	}
	observation := kernel.NeverStarted(proof)
	commit.Observation = &observation
	return commit
}

func testReplacementLaunch(actionID string, workloadID id.WorkloadID) kernel.LaunchIntent {
	return kernel.LaunchIntent{
		WorkloadID:     workloadID,
		State:          kernel.LaunchNotIssued,
		IdempotencyKey: "idem-replacement-" + actionID,
	}
}

// testRetiringExecution seeds one execution and reaches the retiring state
// through the public State.Commit path, so every refusal test starts from a
// persisted precondition instead of a hand-built row.
func testRetiringExecution(t *testing.T, actions ...kernel.Action) (*Store, *State) {
	t.Helper()
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", actions...))
	state := store.Handle()
	if _, err := state.Commit(context.Background(), testRetiringCommit(
		"exec-1", "act-1", 0, 0, testWorkloadID("act-1"), testRetireProof,
	)); err != nil {
		t.Fatalf("retiring Commit() error = %v", err)
	}
	return store, state
}

func testActionByID(execution kernel.Execution, actionID id.ActionID) (kernel.Action, bool) {
	for _, action := range execution.Actions {
		if action.ID == actionID {
			return action, true
		}
	}
	return kernel.Action{}, false
}

// testAssertLaunchState asserts the persisted launch intent and the observation
// stored beside it. An empty wantProof means no evidence was ever persisted for
// the identity.
func testAssertLaunchState(t *testing.T, action kernel.Action, wantState kernel.LaunchState, wantWorkload id.WorkloadID, wantProof string) {
	t.Helper()
	if action.Container == nil {
		t.Fatalf("action %q lost its container state", action.ID)
	}
	container := action.Container
	if container.Launch.State != wantState {
		t.Fatalf("action %q launch state = %q, want %q", action.ID, container.Launch.State, wantState)
	}
	if container.Launch.WorkloadID != wantWorkload {
		t.Fatalf("action %q launch workload = %q, want %q", action.ID, container.Launch.WorkloadID, wantWorkload)
	}
	if container.WorkloadID != wantWorkload {
		t.Fatalf("action %q workload identity = %q, want %q", action.ID, container.WorkloadID, wantWorkload)
	}
	if wantProof == "" {
		if container.Observation.Kind != "" || container.Observation.ProofOfNonexecution() {
			t.Fatalf("action %q observation = %+v, want no persisted evidence", action.ID, container.Observation)
		}
		return
	}
	if container.Observation.Kind != kernel.ObservationNeverStarted || container.Observation.Proof != wantProof {
		t.Fatalf("action %q observation = %+v, want never-started proof %q", action.ID, container.Observation, wantProof)
	}
	if !container.Observation.ProofOfNonexecution() {
		t.Fatalf("action %q observation is not proof of nonexecution", action.ID)
	}
}

func testAssertRetiring(t *testing.T, execution kernel.Execution, actionID id.ActionID, wantWorkload id.WorkloadID, wantProof string) {
	t.Helper()
	action, ok := testActionByID(execution, actionID)
	if !ok {
		t.Fatalf("execution %q lost action %q", execution.ID, actionID)
	}
	testAssertLaunchState(t, action, kernel.LaunchRetiring, wantWorkload, wantProof)
}

// testAssertRetiringCommitRefused commits a payload that must be refused and
// proves the whole transition rolled back: the execution version, the primary
// action version, the launch intent, the observation and the successful commit
// counter are all unchanged.
func testAssertRetiringCommitRefused(t *testing.T, store *Store, state *State, commit flow.Commit, wantExecutionVersion, wantActionVersion uint64, wantCommits int) {
	t.Helper()
	if _, err := state.Commit(context.Background(), commit); err == nil {
		t.Fatalf("Commit() error = nil, want fault class %q", fault.InvalidInput)
	} else {
		testClass(t, err, fault.InvalidInput)
	}
	loaded, err := state.Load(context.Background(), commit.ExecutionID)
	if err != nil {
		t.Fatalf("Load() after refused commit error = %v", err)
	}
	if loaded.Version != wantExecutionVersion {
		t.Fatalf("execution version after refused commit = %d, want %d", loaded.Version, wantExecutionVersion)
	}
	action, ok := testActionByID(loaded, commit.ActionID)
	if !ok {
		t.Fatalf("execution %q lost action %q", loaded.ID, commit.ActionID)
	}
	if action.Version != wantActionVersion {
		t.Fatalf("action %q version after refused commit = %d, want %d", action.ID, action.Version, wantActionVersion)
	}
	if store.CommitCount() != wantCommits {
		t.Fatalf("successful commit count after refused commit = %d, want %d", store.CommitCount(), wantCommits)
	}
}

func TestStateRetiringCommitRequiresNeverStartedProofForCurrentIdentity(t *testing.T) {
	type retiringRefusal struct {
		name      string
		execution kernel.Execution
		commit    flow.Commit
		// wantProof is the evidence already persisted before the refused commit.
		wantProof string
		container bool
	}

	baseExecution := func() kernel.Execution {
		return testExecution("exec-1", "ws-1", testIssuedAction("act-1"))
	}
	baseCommit := func() flow.Commit {
		return testRetiringCommit("exec-1", "act-1", 0, 0, testWorkloadID("act-1"), testRetireProof)
	}

	var cases []retiringRefusal
	add := func(name string, execution kernel.Execution, commit flow.Commit, wantProof string) {
		cases = append(cases, retiringRefusal{
			name:      name,
			execution: execution,
			commit:    commit,
			wantProof: wantProof,
			container: true,
		})
	}

	// A retiring commit without the affirmative proof persisted in the same
	// payload is not a retirement; it must not be admitted.
	execution, commit := baseExecution(), baseCommit()
	commit.Observation = nil
	add("missing observation", execution, commit, "")

	for _, observation := range []kernel.Observation{
		kernel.RunningObservation(),
		kernel.UnknownObservation(),
		kernel.AbsentObservation(),
		{Kind: kernel.ObservationNeverStarted},
	} {
		execution, commit := baseExecution(), baseCommit()
		commit.Observation = &observation
		add("observation without proof "+string(observation.Kind), execution, commit, "")
	}

	// A retirement removes the current identity, never another one.
	execution, commit = baseExecution(), baseCommit()
	commit.Launch = &kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-other"),
		State:          kernel.LaunchRetiring,
		IdempotencyKey: "idem-act-1",
	}
	add("foreign workload identity", execution, commit, "")

	// A second retiring commit with the correct versions is a caller bug, not
	// a silent no-op.
	add(
		"already retiring",
		testExecution("exec-1", "ws-1", testRetiringAction("act-1", testRetireProof)),
		baseCommit(),
		testRetireProof,
	)

	// Retiring is a container launch state; a request action has no identity to
	// retire.
	cases = append(cases, retiringRefusal{
		name:      "request action",
		execution: testExecution("exec-1", "ws-1", testRequestAction("act-1", "ask", 0, kernel.ActionRunning, 0)),
		commit:    baseCommit(),
		container: false,
	})

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := NewStore()
			store.Seed(testCase.execution)
			state := store.Handle()

			testAssertRetiringCommitRefused(t, store, state, testCase.commit, 0, 0, 0)

			loaded, err := state.Load(context.Background(), "exec-1")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !testCase.container {
				action, ok := testActionByID(loaded, testCase.commit.ActionID)
				if !ok {
					t.Fatalf("execution %q lost action %q", loaded.ID, testCase.commit.ActionID)
				}
				if action.Container != nil {
					t.Fatalf("request action %q gained container state: %+v", action.ID, action.Container)
				}
				return
			}
			action, ok := testActionByID(loaded, testCase.commit.ActionID)
			if !ok {
				t.Fatalf("execution %q lost action %q", loaded.ID, testCase.commit.ActionID)
			}
			wantState := kernel.LaunchIssued
			if testCase.wantProof != "" {
				wantState = kernel.LaunchRetiring
			}
			testAssertLaunchState(t, action, wantState, testWorkloadID("act-1"), testCase.wantProof)
		})
	}
}

func TestStateRetiringCommitPersistsProofUntilReplacement(t *testing.T) {
	store := NewStore()
	store.Seed(testExecution("exec-1", "ws-1", testIssuedAction("act-1")))
	state := store.Handle()

	updated, err := state.Commit(context.Background(), testRetiringCommit(
		"exec-1", "act-1", 0, 0, testWorkloadID("act-1"), testRetireProof,
	))
	if err != nil {
		t.Fatalf("retiring Commit() error = %v", err)
	}
	if updated.Version != 1 || updated.Actions[0].Version != 1 {
		t.Fatalf("retiring commit versions = %d/%d, want 1/1", updated.Version, updated.Actions[0].Version)
	}
	testAssertRetiring(t, updated, "act-1", testWorkloadID("act-1"), testRetireProof)
	if updated.Actions[0].Container.Launch.IdempotencyKey != "idem-act-1" {
		t.Fatalf("retiring launch key = %q, want idem-act-1", updated.Actions[0].Container.Launch.IdempotencyKey)
	}

	// The proof and the retiring marker survive a controller restart because
	// they live in the store, not in the handle.
	restarted := store.Handle()
	loaded, err := restarted.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)

	// Cleanup-progress-only commit: no Launch and no Observation. Removal is
	// still owed, so the persisted proof must survive it.
	progress := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  testWorkloadID("act-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
			Attempts:    1,
			LastError:   "runtime unavailable",
		},
		At: testNow,
	}
	if _, err := restarted.Commit(context.Background(), progress); err != nil {
		t.Fatalf("cleanup-progress Commit() error = %v", err)
	}
	loaded, err = restarted.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() after cleanup progress error = %v", err)
	}
	if loaded.Version != 2 || loaded.Actions[0].Version != 1 {
		t.Fatalf("versions after cleanup progress = %d/%d, want 2/1", loaded.Version, loaded.Actions[0].Version)
	}
	if loaded.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", loaded.Cleanup.State)
	}
	testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)

	// Stop-evidence-only commit: still no Launch and no Observation.
	confirmed := kernel.ConfirmedStop("termination observed before retirement")
	stopOnly := testCommit("exec-1", "act-1", 2, 1)
	stopOnly.StopEvidence = &confirmed
	if _, err := restarted.Commit(context.Background(), stopOnly); err != nil {
		t.Fatalf("stop-evidence Commit() error = %v", err)
	}
	loaded, err = restarted.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() after stop evidence error = %v", err)
	}
	if loaded.Version != 3 || loaded.Actions[0].Version != 2 {
		t.Fatalf("versions after stop evidence = %d/%d, want 3/2", loaded.Version, loaded.Actions[0].Version)
	}
	if loaded.Actions[0].Stop.State != kernel.StopStateConfirmed {
		t.Fatalf("stop evidence = %+v, want confirmed", loaded.Actions[0].Stop)
	}
	testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)
}

func TestStateRetiringActionRefusesSameIdentityLaunch(t *testing.T) {
	for _, launchState := range []kernel.LaunchState{
		kernel.LaunchNotIssued,
		kernel.LaunchIssued,
		kernel.LaunchUnknown,
		kernel.LaunchRetiring,
	} {
		t.Run(string(launchState), func(t *testing.T) {
			store, state := testRetiringExecution(t, testIssuedAction("act-1"))

			commit := testCommit("exec-1", "act-1", 1, 1)
			commit.Launch = &kernel.LaunchIntent{
				WorkloadID:     testWorkloadID("act-1"),
				State:          launchState,
				IdempotencyKey: "idem-act-1",
			}
			if launchState == kernel.LaunchRetiring {
				observation := kernel.NeverStarted(testRetireProof)
				commit.Observation = &observation
			}

			testAssertRetiringCommitRefused(t, store, state, commit, 1, 1, 1)

			loaded, err := state.Load(context.Background(), "exec-1")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)
		})
	}
}

func TestStateRetiringActionOnlyAcceptsAReplacement(t *testing.T) {
	type replacementRefusal struct {
		name        string
		launch      kernel.LaunchIntent
		observation *kernel.Observation
	}

	replacementID := id.WorkloadID("wl-replacement")
	exited := kernel.ExitedObservation(0, kernel.ConfirmedStop("terminated"))
	running := kernel.RunningObservation()
	proof := kernel.NeverStarted(testRetireProof)

	cases := []replacementRefusal{
		{
			name:   "empty idempotency key",
			launch: kernel.LaunchIntent{WorkloadID: replacementID, State: kernel.LaunchNotIssued},
		},
		{
			name:   "issued replacement",
			launch: kernel.LaunchIntent{WorkloadID: replacementID, State: kernel.LaunchIssued, IdempotencyKey: "idem-replacement"},
		},
		{
			name:   "unknown replacement",
			launch: kernel.LaunchIntent{WorkloadID: replacementID, State: kernel.LaunchUnknown, IdempotencyKey: "idem-replacement"},
		},
		{
			name:   "retiring replacement",
			launch: kernel.LaunchIntent{WorkloadID: replacementID, State: kernel.LaunchRetiring, IdempotencyKey: "idem-replacement"},
		},
		{
			name:        "replacement with proof observation",
			launch:      testReplacementLaunch("act-1", replacementID),
			observation: &proof,
		},
		{
			name:        "replacement with running observation",
			launch:      testReplacementLaunch("act-1", replacementID),
			observation: &running,
		},
		{
			name:        "replacement with exited observation",
			launch:      testReplacementLaunch("act-1", replacementID),
			observation: &exited,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, state := testRetiringExecution(t, testIssuedAction("act-1"))

			commit := testCommit("exec-1", "act-1", 1, 1)
			launch := testCase.launch
			commit.Launch = &launch
			commit.Observation = testCase.observation

			testAssertRetiringCommitRefused(t, store, state, commit, 1, 1, 1)

			loaded, err := state.Load(context.Background(), "exec-1")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)
		})
	}
}

func TestStateRetiringActionRefusesConflictingObservation(t *testing.T) {
	cases := []struct {
		name        string
		observation kernel.Observation
	}{
		{name: "running", observation: kernel.RunningObservation()},
		{name: "unknown", observation: kernel.UnknownObservation()},
		{name: "absent", observation: kernel.AbsentObservation()},
		{name: "different proof", observation: kernel.NeverStarted("a different never-started proof")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, state := testRetiringExecution(t, testIssuedAction("act-1"))

			commit := testCommit("exec-1", "act-1", 1, 1)
			observation := testCase.observation
			commit.Observation = &observation

			testAssertRetiringCommitRefused(t, store, state, commit, 1, 1, 1)

			loaded, err := state.Load(context.Background(), "exec-1")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)
		})
	}
}

func TestStateRetiringActionAcceptsIdenticalProofRewrite(t *testing.T) {
	store, state := testRetiringExecution(t, testIssuedAction("act-1"))

	commit := testCommit("exec-1", "act-1", 1, 1)
	observation := kernel.NeverStarted(testRetireProof)
	commit.Observation = &observation

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("identical proof rewrite Commit() error = %v", err)
	}
	if updated.Version != 2 || updated.Actions[0].Version != 2 {
		t.Fatalf("versions after identical proof rewrite = %d/%d, want 2/2", updated.Version, updated.Actions[0].Version)
	}
	if store.CommitCount() != 2 {
		t.Fatalf("successful commit count = %d, want 2", store.CommitCount())
	}
	testAssertRetiring(t, updated, "act-1", testWorkloadID("act-1"), testRetireProof)

	loaded, err := store.Handle().Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	testAssertRetiring(t, loaded, "act-1", testWorkloadID("act-1"), testRetireProof)
}

func TestStateRetiringActionAcceptsReplacement(t *testing.T) {
	store, state := testRetiringExecution(t, testIssuedAction("act-1"))

	replacementID := id.WorkloadID("wl-replacement")
	commit := testCommit("exec-1", "act-1", 1, 1)
	launch := testReplacementLaunch("act-1", replacementID)
	commit.Launch = &launch
	observation := kernel.UnknownObservation()
	commit.Observation = &observation

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("replacement Commit() error = %v", err)
	}
	if updated.Version != 2 || updated.Actions[0].Version != 2 {
		t.Fatalf("versions after replacement = %d/%d, want 2/2", updated.Version, updated.Actions[0].Version)
	}
	action, ok := testActionByID(updated, "act-1")
	if !ok {
		t.Fatalf("execution %q lost action act-1", updated.ID)
	}
	if action.Container.WorkloadID != replacementID {
		t.Fatalf("replacement workload identity = %q, want %q", action.Container.WorkloadID, replacementID)
	}
	if action.Container.Launch.WorkloadID != replacementID {
		t.Fatalf("replacement launch workload = %q, want %q", action.Container.Launch.WorkloadID, replacementID)
	}
	if action.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("replacement launch state = %q, want %q", action.Container.Launch.State, kernel.LaunchNotIssued)
	}
	if action.Container.Launch.IdempotencyKey != "idem-replacement-act-1" {
		t.Fatalf("replacement launch key = %q", action.Container.Launch.IdempotencyKey)
	}
	if action.Container.Observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("replacement observation = %+v, want unknown", action.Container.Observation)
	}
	if action.Container.Observation.ProofOfNonexecution() {
		t.Fatal("replacement inherited a proof of nonexecution for the new identity")
	}

	loaded, err := store.Handle().Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("fresh handle Load() error = %v", err)
	}
	reloaded, ok := testActionByID(loaded, "act-1")
	if !ok {
		t.Fatalf("fresh handle lost action act-1")
	}
	if reloaded.Container.WorkloadID != replacementID || reloaded.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("fresh handle container = %q/%q, want %q/not_issued",
			reloaded.Container.WorkloadID, reloaded.Container.Launch.State, replacementID)
	}
}

func TestStateRetiringActionAcceptsReplacementWithoutObservation(t *testing.T) {
	_, state := testRetiringExecution(t, testIssuedAction("act-1"))

	replacementID := id.WorkloadID("wl-replacement")
	commit := testCommit("exec-1", "act-1", 1, 1)
	launch := testReplacementLaunch("act-1", replacementID)
	commit.Launch = &launch

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("replacement without observation Commit() error = %v", err)
	}
	action, ok := testActionByID(updated, "act-1")
	if !ok {
		t.Fatalf("execution %q lost action act-1", updated.ID)
	}
	if action.Container.WorkloadID != replacementID {
		t.Fatalf("replacement workload identity = %q, want %q", action.Container.WorkloadID, replacementID)
	}
	if action.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("replacement launch state = %q, want %q", action.Container.Launch.State, kernel.LaunchNotIssued)
	}
}

func TestStateRetiringActionAcceptsTerminalCommits(t *testing.T) {
	t.Run("execution-only cancel", func(t *testing.T) {
		_, state := testRetiringExecution(t,
			testIssuedAction("act-1"),
			testContainerAction("act-2", "next", 1, kernel.ActionPending, 0),
		)

		nextCurrent := -1
		cancel := flow.Commit{
			ExecutionID:              "exec-1",
			ExpectedExecutionVersion: 1,
			NextExecutionState:       kernel.ExecutionCancelled,
			NextCurrentAction:        &nextCurrent,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			PendingCancellations: []flow.ActionUpdate{{
				ActionID:        id.ActionID("act-2"),
				ExpectedVersion: 0,
				NextState:       kernel.ActionCancelled,
			}},
			At: testNow,
		}
		updated, err := state.Commit(context.Background(), cancel)
		if err != nil {
			t.Fatalf("execution-only cancel Commit() error = %v", err)
		}
		if updated.Version != 2 || updated.State != kernel.ExecutionCancelled || updated.CurrentAction != -1 {
			t.Fatalf("execution after cancel = version %d state %q current %d, want 2/cancelled/-1",
				updated.Version, updated.State, updated.CurrentAction)
		}
		testAssertRetiring(t, updated, "act-1", testWorkloadID("act-1"), testRetireProof)
		next, ok := testActionByID(updated, "act-2")
		if !ok {
			t.Fatal("execution lost the pending successor act-2")
		}
		if next.State != kernel.ActionCancelled || next.Version != 1 {
			t.Fatalf("pending successor = %q v%d, want cancelled v1", next.State, next.Version)
		}
	})

	t.Run("primary action cancel", func(t *testing.T) {
		_, state := testRetiringExecution(t, testIssuedAction("act-1"))

		commit := testCommit("exec-1", "act-1", 1, 1)
		commit.NextExecutionState = kernel.ExecutionCancelled
		commit.NextActionState = kernel.ActionCancelled
		nextCurrent := -1
		commit.NextCurrentAction = &nextCurrent

		updated, err := state.Commit(context.Background(), commit)
		if err != nil {
			t.Fatalf("cancel Commit() error = %v", err)
		}
		if updated.Version != 2 || updated.State != kernel.ExecutionCancelled {
			t.Fatalf("execution after cancel = version %d state %q, want 2/cancelled", updated.Version, updated.State)
		}
		action, ok := testActionByID(updated, "act-1")
		if !ok {
			t.Fatalf("execution %q lost action act-1", updated.ID)
		}
		if action.State != kernel.ActionCancelled || action.Version != 2 {
			t.Fatalf("cancelled action = %q v%d, want cancelled v2", action.State, action.Version)
		}
		testAssertRetiring(t, updated, "act-1", testWorkloadID("act-1"), testRetireProof)
	})

	t.Run("failure with stop and cleanup", func(t *testing.T) {
		store, state := testRetiringExecution(t, testIssuedAction("act-1"))

		commit := testCommit("exec-1", "act-1", 1, 1)
		commit.NextExecutionState = kernel.ExecutionFailed
		commit.NextActionState = kernel.ActionFailed
		nextCurrent := -1
		commit.NextCurrentAction = &nextCurrent
		commit.Failure = &kernel.Failure{
			Kind:   kernel.FailureTimeout,
			Reason: "deadline_reached",
			Detail: "the persisted action deadline elapsed while retiring",
		}
		commit.Stop = &flow.StopIntent{
			WorkloadID:  testWorkloadID("act-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
		}
		commit.Cleanup = &flow.CleanupIntent{
			WorkloadID:  testWorkloadID("act-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
			Reason:      "terminal cleanup after retirement",
		}
		commit.CleanupUpdate = &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  testWorkloadID("act-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
		}

		updated, err := state.Commit(context.Background(), commit)
		if err != nil {
			t.Fatalf("failure Commit() error = %v", err)
		}
		if updated.Version != 2 || updated.State != kernel.ExecutionFailed {
			t.Fatalf("execution after failure = version %d state %q, want 2/failed", updated.Version, updated.State)
		}
		if updated.Failure == nil || updated.Failure.Kind != kernel.FailureTimeout {
			t.Fatalf("failure = %+v, want kind %q", updated.Failure, kernel.FailureTimeout)
		}
		if updated.Cleanup.State != kernel.CleanupPending || updated.Cleanup.WorkloadID != testWorkloadID("act-1") {
			t.Fatalf("cleanup = %+v, want pending for %q", updated.Cleanup, testWorkloadID("act-1"))
		}
		testAssertRetiring(t, updated, "act-1", testWorkloadID("act-1"), testRetireProof)

		stops := store.PendingStops()
		if len(stops) != 1 || stops[0].WorkloadID != testWorkloadID("act-1") {
			t.Fatalf("pending stops = %+v, want one for %q", stops, testWorkloadID("act-1"))
		}
		cleanups := store.PendingCleanups()
		if len(cleanups) != 1 || cleanups[0].WorkloadID != testWorkloadID("act-1") {
			t.Fatalf("pending cleanups = %+v, want one for %q", cleanups, testWorkloadID("act-1"))
		}
	})
}
