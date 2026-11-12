package flow_test

// Retire-then-replace (S02 §5) is the only permitted relaunch
// protocol. Affirmative proof of nonexecution retires the current workload
// identity in one commit, removal is confirmed separately, and only a `retired`
// fact allocates the deterministic replacement. These tests pin the pure
// reducer part of that protocol; they never start a process, read a wall clock
// or perform I/O.
//
// The reducer outcomes are the usual ones: a nil commit with a nil error is a
// converged no-op, a non-nil error refuses with the input unchanged, and a
// non-nil commit is one atomic payload whose post-commit projection the reducer
// also returns.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

const (
	// retireProofA is affirmative evidence that the first attempt never ran.
	retireProofA = "runtime proved the first start request created no object"
	// retireProofB is the same evidence for the replacement attempt.
	retireProofB = "runtime proved the replacement start request created no object"
)

// runningContainerWithLaunch builds the running two-action fixture whose current
// container action carries an explicit launch state and observation. It is the
// durable shape a restart load can produce at any point of the launch protocol.
func runningContainerWithLaunch(state kernel.LaunchState, observation kernel.Observation) kernel.Execution {
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	container := *exec.Actions[0].Container
	container.Launch.State = state
	container.Observation = observation
	exec.Actions[0].Container = &container
	return exec
}

// retiringExecution is the persisted post-retirement state: the identity is
// still A with its durable never-started proof, and no replacement exists yet.
func retiringExecution(proof string) kernel.Execution {
	return runningContainerWithLaunch(kernel.LaunchRetiring, kernel.NeverStarted(proof))
}

// terminalRetiringExecution seeds the terminal race: the execution settled
// while the identity was retiring, so a late `retired` fact must never create a
// replacement identity.
func terminalRetiringExecution(state kernel.ExecutionState, actionState kernel.ActionState) kernel.Execution {
	exec := retiringExecution(retireProofA)
	exec.State = state
	exec.CurrentAction = -1
	exec.Actions[0].State = actionState
	exec.Actions[0].Version = 3
	if state == kernel.ExecutionFailed {
		exec.Failure = &kernel.Failure{
			Kind:   kernel.FailureRuntime,
			Reason: "workload_exited",
			Detail: "workload exited with status 1",
		}
	}
	return exec
}

// assertRetiringLaunch asserts the action still owns exactly the retiring
// identity and its persisted proof, with no replacement allocated.
func assertRetiringLaunch(t *testing.T, action kernel.Action, workload id.WorkloadID, key, proof string) {
	t.Helper()
	if action.Container == nil {
		t.Fatalf("action %q has no container value", action.ID)
	}
	if action.Container.WorkloadID != workload {
		t.Fatalf("action workload = %q, want retiring identity %q", action.Container.WorkloadID, workload)
	}
	if action.Container.Launch.WorkloadID != workload {
		t.Fatalf("launch workload = %q, want retiring identity %q", action.Container.Launch.WorkloadID, workload)
	}
	if action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("launch state = %q, want retiring", action.Container.Launch.State)
	}
	if action.Container.Launch.IdempotencyKey != key {
		t.Fatalf("launch idempotency key = %q, want %q", action.Container.Launch.IdempotencyKey, key)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retiring identity lost its never-started proof: %+v", action.Container.Observation)
	}
	if action.Container.Observation.Proof != proof {
		t.Fatalf("retiring proof = %q, want %q", action.Container.Observation.Proof, proof)
	}
}

// assertNoLaunchEffects asserts a retirement commit carries no stop, cleanup,
// credential, output, settlement, failure, deadline or current-index side
// effect; retirement only marks the identity and retains the proof.
func assertNoLaunchEffects(t *testing.T, commit *flow.Commit) {
	t.Helper()
	if commit.Stop != nil || commit.StopEvidence != nil {
		t.Fatalf("commit carried stop intent/evidence: %+v / %+v", commit.Stop, commit.StopEvidence)
	}
	if commit.Cleanup != nil || commit.CleanupUpdate != nil {
		t.Fatalf("commit carried cleanup intent/update: %+v / %+v", commit.Cleanup, commit.CleanupUpdate)
	}
	if commit.InvalidateCredential {
		t.Fatal("commit invalidated the credential")
	}
	if commit.Output.Action != flow.OutputNone || commit.Output.Values != nil {
		t.Fatalf("commit changed outputs: %+v", commit.Output)
	}
	if commit.Subject != nil || commit.Answer != nil || commit.Failure != nil {
		t.Fatalf("commit carried subject/answer/failure: %+v / %+v / %+v", commit.Subject, commit.Answer, commit.Failure)
	}
	if !commit.Deadline.IsZero() {
		t.Fatalf("commit changed the persisted deadline to %v", commit.Deadline)
	}
	if commit.NextCurrentAction != nil {
		t.Fatalf("commit changed the current action index to %d", *commit.NextCurrentAction)
	}
	if len(commit.PendingCancellations) != 0 {
		t.Fatalf("commit cancelled pending actions: %+v", commit.PendingCancellations)
	}
}

// ---------------------------------------------------------------------------
// Never-started proof retires the live identity; it never relaunches it.
// ---------------------------------------------------------------------------

func TestNeverStartedOnLiveIdentityBeginsRetirement(t *testing.T) {
	at := baseTime

	for _, launchState := range []kernel.LaunchState{
		kernel.LaunchIssued,
		kernel.LaunchUnknown,
		kernel.LaunchNotIssued,
	} {
		t.Run(string(launchState), func(t *testing.T) {
			exec := runningContainerWithLaunch(launchState, kernel.Observation{})
			workloadA := exec.Actions[0].Container.WorkloadID
			keyA := exec.Actions[0].Container.Launch.IdempotencyKey
			if workloadA == "" || keyA == "" {
				t.Fatalf("fixture has no persisted identity: workload=%q key=%q", workloadA, keyA)
			}

			next, commit := mustReduce(t, exec, flow.ObserveFact{
				ExecutionID: exec.ID,
				ActionID:    "act-0",
				Observation: kernel.NeverStarted(retireProofA),
				At:          at.Add(time.Minute),
			})

			// The commit is a retirement, not a relaunch: the same identity and
			// key, the retiring marker, and the proof retained verbatim.
			want := kernel.LaunchIntent{
				WorkloadID:     workloadA,
				State:          kernel.LaunchRetiring,
				IdempotencyKey: keyA,
			}
			if commit.Launch == nil {
				t.Fatal("never-started proof did not persist a retiring launch intent")
			}
			if *commit.Launch != want {
				t.Fatalf("launch intent = %+v, want retirement of the same identity %+v", *commit.Launch, want)
			}
			if commit.Observation == nil {
				t.Fatal("retirement commit dropped the never-started observation")
			}
			if commit.Observation.Kind != kernel.ObservationNeverStarted || commit.Observation.Proof != retireProofA {
				t.Fatalf("retirement observation = %+v, want the retained proof %q", *commit.Observation, retireProofA)
			}
			if commit.NextExecutionState != kernel.ExecutionRunning {
				t.Fatalf("next execution state = %q, want running", commit.NextExecutionState)
			}
			if commit.NextActionState != kernel.ActionRunning {
				t.Fatalf("next action state = %q, want running", commit.NextActionState)
			}
			if commit.ExpectedExecutionVersion != exec.Version {
				t.Fatalf("expected execution version = %d, want %d", commit.ExpectedExecutionVersion, exec.Version)
			}
			if commit.ExpectedActionVersion != exec.Actions[0].Version {
				t.Fatalf("expected action version = %d, want %d", commit.ExpectedActionVersion, exec.Actions[0].Version)
			}
			assertNoLaunchEffects(t, commit)

			if next.State != kernel.ExecutionRunning || next.CurrentAction != 0 {
				t.Fatalf("projected execution = %q current=%d, want running current=0", next.State, next.CurrentAction)
			}
			if next.Actions[0].State != kernel.ActionRunning {
				t.Fatalf("projected action state = %q, want running", next.Actions[0].State)
			}
			assertRetiringLaunch(t, next.Actions[0], workloadA, keyA, retireProofA)
			if next.Actions[1].State != kernel.ActionPending {
				t.Fatalf("pending successor state = %q, want pending", next.Actions[1].State)
			}
			assertSingleActiveAction(t, next)
		})
	}
}

// ---------------------------------------------------------------------------
// A retiring identity is launch-terminal; only the identical proof converges.
// ---------------------------------------------------------------------------

func TestRetiringIdentityOnlyAcceptsTheIdenticalProof(t *testing.T) {
	at := baseTime
	exec := retiringExecution(retireProofA)
	workloadA := exec.Actions[0].Container.WorkloadID

	t.Run("identical proof converges without a commit", func(t *testing.T) {
		mustNoop(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: kernel.NeverStarted(retireProofA),
			At:          at.Add(time.Minute),
		})
		// The fact time is metadata for a converged duplicate; a later retry of
		// the same proof still writes nothing.
		mustNoop(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: kernel.NeverStarted(retireProofA),
			At:          at.Add(time.Hour),
		})
	})

	t.Run("every other observation is refused as conflicting content", func(t *testing.T) {
		cases := []struct {
			name        string
			observation kernel.Observation
		}{
			{"different proof", kernel.NeverStarted("runtime proved a different attempt never started")},
			{"running", kernel.RunningObservation()},
			{"absent", kernel.AbsentObservation()},
			{"unknown", kernel.UnknownObservation()},
			{"exited", kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				mustRefuse(t, exec, flow.ObserveFact{
					ExecutionID: exec.ID,
					ActionID:    "act-0",
					Observation: tc.observation,
					At:          at.Add(time.Minute),
				}, fault.ChangedIdempotencyContent)
			})
		}
	})

	// The fixture itself still describes a retiring identity; a refusal never
	// allocates the replacement.
	assertRetiringLaunch(t, exec.Actions[0], workloadA, exec.Actions[0].Container.Launch.IdempotencyKey, retireProofA)
}

// ---------------------------------------------------------------------------
// Only a `retired` fact allocates the deterministic replacement identity.
// ---------------------------------------------------------------------------

func TestRetiredFactCommitsDeterministicReplacement(t *testing.T) {
	at := baseTime
	exec := retiringExecution(retireProofA)
	workloadA := exec.Actions[0].Container.WorkloadID
	keyA := exec.Actions[0].Container.Launch.IdempotencyKey
	deadline := exec.Actions[0].Deadline
	activatedAt := exec.Actions[0].ActivatedAt

	fact := flow.RetiredFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		WorkloadID:  workloadA,
		At:          at.Add(time.Minute),
	}
	next, commit := mustReduce(t, exec, fact)

	if commit.Launch == nil {
		t.Fatal("retired fact did not allocate a replacement launch intent")
	}
	replacement := *commit.Launch
	if replacement.State != kernel.LaunchNotIssued {
		t.Fatalf("replacement launch state = %q, want not_issued", replacement.State)
	}
	if replacement.WorkloadID == workloadA {
		t.Fatalf("replacement reused the retired workload identity %q", workloadA)
	}
	if replacement.IdempotencyKey == keyA {
		t.Fatalf("replacement reused the retired idempotency key %q", keyA)
	}
	if !strings.HasPrefix(string(replacement.WorkloadID), "wl-") {
		t.Fatalf("replacement workload identity = %q, want the wl- identity form", replacement.WorkloadID)
	}
	if !strings.HasPrefix(replacement.IdempotencyKey, "start-") {
		t.Fatalf("replacement idempotency key = %q, want the start- key form", replacement.IdempotencyKey)
	}

	// The derivation is deterministic for one revision: reducing the same
	// loaded state twice yields the identical replacement identity and key.
	_, repeated := mustReduce(t, exec, fact)
	if repeated.Launch == nil {
		t.Fatal("repeated reduction of the same revision allocated no replacement")
	}
	if *repeated.Launch != replacement {
		t.Fatalf("repeated reduction allocated %+v, want the identical %+v", *repeated.Launch, replacement)
	}

	// The replacement owns no runtime evidence; the retired proof stays with A.
	if commit.Observation == nil {
		t.Fatal("replacement commit did not record the new identity's observation")
	}
	if commit.Observation.Kind != kernel.ObservationUnknown || commit.Observation.Proof != "" {
		t.Fatalf("replacement observation = %+v, want unknown without a proof", *commit.Observation)
	}

	// The action stays the single running action; only the identity changes.
	if commit.NextExecutionState != kernel.ExecutionRunning {
		t.Fatalf("next execution state = %q, want running", commit.NextExecutionState)
	}
	if commit.NextActionState != kernel.ActionRunning {
		t.Fatalf("next action state = %q, want running", commit.NextActionState)
	}
	if commit.ExpectedExecutionVersion != exec.Version {
		t.Fatalf("expected execution version = %d, want %d", commit.ExpectedExecutionVersion, exec.Version)
	}
	if commit.ExpectedActionVersion != exec.Actions[0].Version {
		t.Fatalf("expected action version = %d, want %d", commit.ExpectedActionVersion, exec.Actions[0].Version)
	}
	// The replacement never rewrites the persisted deadline or activation time.
	if !commit.Deadline.IsZero() {
		t.Fatalf("replacement commit changed the deadline to %v", commit.Deadline)
	}
	assertNoLaunchEffects(t, commit)

	if next.State != kernel.ExecutionRunning || next.CurrentAction != 0 {
		t.Fatalf("projected execution = %q current=%d, want running current=0", next.State, next.CurrentAction)
	}
	if next.Version != exec.Version+1 {
		t.Fatalf("projected execution version = %d, want %d", next.Version, exec.Version+1)
	}
	if next.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("projected action state = %q, want running", next.Actions[0].State)
	}
	if next.Actions[0].Version != exec.Actions[0].Version+1 {
		t.Fatalf("projected action version = %d, want %d", next.Actions[0].Version, exec.Actions[0].Version+1)
	}
	if !next.Actions[0].Deadline.Equal(deadline) {
		t.Fatalf("projected deadline = %v, want unchanged %v", next.Actions[0].Deadline, deadline)
	}
	if !next.Actions[0].ActivatedAt.Equal(activatedAt) {
		t.Fatalf("projected activation time = %v, want unchanged %v", next.Actions[0].ActivatedAt, activatedAt)
	}

	container := next.Actions[0].Container
	if container == nil {
		t.Fatal("projected action lost its container value")
	}
	if container.WorkloadID != replacement.WorkloadID {
		t.Fatalf("projected workload = %q, want replacement %q", container.WorkloadID, replacement.WorkloadID)
	}
	if container.Launch != replacement {
		t.Fatalf("projected launch = %+v, want %+v", container.Launch, replacement)
	}
	if container.Observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("projected observation = %+v, want unknown", container.Observation)
	}
	if container.Observation.ProofOfNonexecution() {
		t.Fatal("replacement identity inherited the retired attempt's proof")
	}
	if next.Actions[1].State != kernel.ActionPending {
		t.Fatalf("pending successor state = %q, want pending", next.Actions[1].State)
	}
	assertSingleActiveAction(t, next)
}

// ---------------------------------------------------------------------------
// A `retired` fact is only valid for the exact retiring target.
// ---------------------------------------------------------------------------

func TestRetiredFactRefusesIllegalAndStaleTargets(t *testing.T) {
	at := baseTime
	workloadA := id.WorkloadID("wl-0")

	t.Run("wrong execution id", func(t *testing.T) {
		exec := retiringExecution(retireProofA)
		mustRefuse(t, exec, flow.RetiredFact{
			ExecutionID: "other-exec",
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("unknown action id", func(t *testing.T) {
		exec := retiringExecution(retireProofA)
		mustRefuse(t, exec, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "missing",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("action is not a container", func(t *testing.T) {
		exec := runningRequestExecution(requestAction(
			"act-0", "review", 0, time.Hour,
			literalSubject(map[string]string{"operation": "review"}), "decision",
		))
		mustRefuse(t, exec, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("launch state is not retiring", func(t *testing.T) {
		for _, launchState := range []kernel.LaunchState{
			kernel.LaunchIssued,
			kernel.LaunchUnknown,
			kernel.LaunchNotIssued,
		} {
			exec := runningContainerWithLaunch(launchState, kernel.NeverStarted(retireProofA))
			mustRefuse(t, exec, flow.RetiredFact{
				ExecutionID: exec.ID,
				ActionID:    "act-0",
				WorkloadID:  workloadA,
				At:          at,
			}, fault.InvalidInput)
		}
	})

	t.Run("foreign workload identity", func(t *testing.T) {
		exec := retiringExecution(retireProofA)
		mustRefuse(t, exec, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  "wl-foreign",
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("retiring without a persisted proof", func(t *testing.T) {
		// A malformed aggregate: the retiring marker exists but no affirmative
		// proof was persisted, so removal was never justified.
		for _, observation := range []kernel.Observation{
			kernel.UnknownObservation(),
			kernel.NeverStarted("   "),
		} {
			exec := runningContainerWithLaunch(kernel.LaunchRetiring, observation)
			mustRefuse(t, exec, flow.RetiredFact{
				ExecutionID: exec.ID,
				ActionID:    "act-0",
				WorkloadID:  workloadA,
				At:          at,
			}, fault.InvalidInput)
		}
	})

	t.Run("queued execution", func(t *testing.T) {
		exec := newExecution(containerAction("act-0", "build", 0, time.Hour))
		container := *exec.Actions[0].Container
		container.WorkloadID = workloadA
		container.Launch = kernel.LaunchIntent{
			WorkloadID:     workloadA,
			State:          kernel.LaunchRetiring,
			IdempotencyKey: "start-wl-0",
		}
		container.Observation = kernel.NeverStarted(retireProofA)
		exec.Actions[0].Container = &container
		mustRefuse(t, exec, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("action is not the running current action", func(t *testing.T) {
		// A settled earlier action keeps a stale retiring marker while a later
		// action runs: the fact targets the wrong action.
		stale := retiringExecution(retireProofA)
		stale.CurrentAction = 1
		stale.Actions[0].State = kernel.ActionSucceeded
		stale.Actions[1].State = kernel.ActionRunning
		stale.Actions[1].Version = 2
		mustRefuse(t, stale, flow.RetiredFact{
			ExecutionID: stale.ID,
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)

		// A pending, non-current action is never a retirement target.
		pending := retiringExecution(retireProofA)
		mustRefuse(t, pending, flow.RetiredFact{
			ExecutionID: pending.ID,
			ActionID:    "act-1",
			WorkloadID:  workloadA,
			At:          at,
		}, fault.InvalidInput)
	})

	t.Run("terminal execution never creates a replacement", func(t *testing.T) {
		cases := []struct {
			name        string
			execution   kernel.ExecutionState
			actionState kernel.ActionState
		}{
			{"succeeded", kernel.ExecutionSucceeded, kernel.ActionSucceeded},
			{"failed", kernel.ExecutionFailed, kernel.ActionFailed},
			{"cancelled", kernel.ExecutionCancelled, kernel.ActionCancelled},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				exec := terminalRetiringExecution(tc.execution, tc.actionState)
				// mustRefuse also proves the returned execution is unchanged, so a
				// terminal refusal can never allocate a replacement identity.
				mustRefuse(t, exec, flow.RetiredFact{
					ExecutionID: exec.ID,
					ActionID:    "act-0",
					WorkloadID:  workloadA,
					At:          at,
				}, fault.ChangedIdempotencyContent)
				if exec.Actions[0].Container == nil {
					t.Fatal("terminal fixture lost its container value")
				}
				assertRetiringLaunch(t, exec.Actions[0], workloadA, "start-wl-0", retireProofA)
			})
		}
	})

	t.Run("already replaced identity", func(t *testing.T) {
		exec := retiringExecution(retireProofA)
		replaced, commit := mustReduce(t, exec, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at.Add(time.Minute),
		})
		if commit.Launch == nil {
			t.Fatal("retired fact allocated no replacement")
		}
		replacement := commit.Launch.WorkloadID

		// The current identity is now B with not_issued; neither the retired A
		// nor a repeated retirement of B may allocate another identity.
		mustRefuse(t, replaced, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  replacement,
			At:          at.Add(2 * time.Minute),
		}, fault.InvalidInput)
		mustRefuse(t, replaced, flow.RetiredFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			WorkloadID:  workloadA,
			At:          at.Add(2 * time.Minute),
		}, fault.InvalidInput)
	})
}

// ---------------------------------------------------------------------------
// Terminal settlement during retirement keeps A and its proof; no replacement.
// ---------------------------------------------------------------------------

func TestTerminalSettlementDuringRetirementKeepsTheRetiringIdentity(t *testing.T) {
	at := baseTime
	workloadA := id.WorkloadID("wl-0")
	keyA := "start-wl-0"

	retiring := func() kernel.Execution {
		exec := retiringExecution(retireProofA)
		exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
		exec.Actions[0].StagedOutputsSet = true
		return exec
	}

	t.Run("cancel", func(t *testing.T) {
		exec := retiring()
		next, commit := mustReduce(t, exec, flow.CancelFact{
			ExecutionID: exec.ID,
			Reason:      flow.CancelRequested,
			At:          at.Add(time.Minute),
		})
		if commit.Launch != nil {
			t.Fatalf("cancel committed a replacement launch: %+v", commit.Launch)
		}
		if next.State != kernel.ExecutionCancelled {
			t.Fatalf("execution state = %q, want cancelled", next.State)
		}
		if next.Actions[0].State != kernel.ActionCancelled {
			t.Fatalf("action state = %q, want cancelled", next.Actions[0].State)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("cancel retained staged outputs")
		}
		if commit.Stop != nil {
			t.Fatalf("cancel emitted a stop intent during retirement: %+v", commit.Stop)
		}
		assertCleanup(t, exec, commit, workloadA)
		assertRetiringLaunch(t, next.Actions[0], workloadA, keyA, retireProofA)
		assertSingleActiveAction(t, next)
	})

	t.Run("expire", func(t *testing.T) {
		exec := retiring()
		deadline := exec.Actions[0].Deadline
		next, commit := mustReduce(t, exec, flow.ExpireFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			At:          deadline,
		})
		if commit.Launch != nil {
			t.Fatalf("expiry committed a replacement launch: %+v", commit.Launch)
		}
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if next.Actions[0].State != kernel.ActionFailed {
			t.Fatalf("action state = %q, want failed", next.Actions[0].State)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("expiry retained staged outputs")
		}
		assertFailure(t, commit, kernel.FailureTimeout)
		if commit.Stop != nil {
			t.Fatalf("expiry emitted a stop intent during retirement: %+v", commit.Stop)
		}
		assertCleanup(t, exec, commit, workloadA)
		assertRetiringLaunch(t, next.Actions[0], workloadA, keyA, retireProofA)
		assertSingleActiveAction(t, next)
	})

	t.Run("workspace lost", func(t *testing.T) {
		exec := retiring()
		next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{
			ExecutionID: exec.ID,
			WorkspaceID: exec.WorkspaceID,
			At:          at.Add(time.Minute),
		})
		if commit.Launch != nil {
			t.Fatalf("workspace loss committed a replacement launch: %+v", commit.Launch)
		}
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if next.Actions[0].State != kernel.ActionFailed {
			t.Fatalf("action state = %q, want failed", next.Actions[0].State)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("workspace loss retained staged outputs")
		}
		assertFailure(t, commit, kernel.FailureWorkspaceLost)
		if commit.Stop != nil {
			t.Fatalf("workspace loss emitted a stop intent during retirement: %+v", commit.Stop)
		}
		assertCleanup(t, exec, commit, workloadA)
		assertRetiringLaunch(t, next.Actions[0], workloadA, keyA, retireProofA)
		assertSingleActiveAction(t, next)
	})
}

func TestTerminalSettlementStopIntentOnlyForLiveUnconfirmedLaunch(t *testing.T) {
	at := baseTime
	workloadA := id.WorkloadID("wl-0")

	cases := []struct {
		name          string
		launchState   kernel.LaunchState
		observation   kernel.Observation
		stopConfirmed bool
		wantStop      bool
	}{
		{
			name:        "issued emits stop",
			launchState: kernel.LaunchIssued,
			observation: kernel.RunningObservation(),
			wantStop:    true,
		},
		{
			name:        "unknown emits stop",
			launchState: kernel.LaunchUnknown,
			observation: kernel.UnknownObservation(),
			wantStop:    true,
		},
		{
			name:        "not_issued omits stop",
			launchState: kernel.LaunchNotIssued,
			observation: kernel.UnknownObservation(),
			wantStop:    false,
		},
		{
			name:        "retiring omits stop",
			launchState: kernel.LaunchRetiring,
			observation: kernel.NeverStarted(retireProofA),
			wantStop:    false,
		},
		{
			name:          "confirmed stop omits duplicate stop",
			launchState:   kernel.LaunchIssued,
			observation:   kernel.RunningObservation(),
			stopConfirmed: true,
			wantStop:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed := func() kernel.Execution {
				exec := runningContainerWithLaunch(tc.launchState, tc.observation)
				if tc.stopConfirmed {
					exec.Actions[0].Stop = kernel.ConfirmedStop("termination observed")
				}
				return exec
			}

			facts := []struct {
				name string
				fact func(kernel.Execution) flow.Fact
			}{
				{
					name: "cancel",
					fact: func(exec kernel.Execution) flow.Fact {
						return flow.CancelFact{
							ExecutionID: exec.ID,
							Reason:      flow.CancelRequested,
							At:          at.Add(time.Minute),
						}
					},
				},
				{
					name: "expire",
					fact: func(exec kernel.Execution) flow.Fact {
						return flow.ExpireFact{
							ExecutionID: exec.ID,
							ActionID:    "act-0",
							At:          exec.Actions[0].Deadline,
						}
					},
				},
				{
					name: "workspace_lost",
					fact: func(exec kernel.Execution) flow.Fact {
						return flow.WorkspaceLostFact{
							ExecutionID: exec.ID,
							WorkspaceID: exec.WorkspaceID,
							At:          at.Add(time.Minute),
						}
					},
				},
			}

			for _, f := range facts {
				t.Run(f.name, func(t *testing.T) {
					exec := seed()
					_, commit := mustReduce(t, exec, f.fact(exec))
					if tc.wantStop {
						assertStopIntent(t, commit, workloadA, exec.WorkspaceID)
					} else if commit.Stop != nil {
						t.Fatalf("commit.Stop = %+v, want nil", commit.Stop)
					}
					assertCleanup(t, exec, commit, workloadA)
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The full retire-then-replace chain, applied through the commit projection.
// ---------------------------------------------------------------------------

func TestRetireThenReplaceIdentityChain(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	workloadA := exec.Actions[0].Container.WorkloadID
	keyA := exec.Actions[0].Container.Launch.IdempotencyKey
	assertSingleActiveAction(t, exec)

	// Step 1: affirmative proof retires A without allocating anything.
	retiring, retireCommit := mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.NeverStarted(retireProofA),
		At:          at.Add(time.Minute),
	})
	appliedRetire := applyCommit(exec, retireCommit)
	if !reflect.DeepEqual(retiring, appliedRetire) {
		t.Fatalf("retirement projection diverges from its commit\n got: %#v\nwant: %#v", retiring, appliedRetire)
	}
	if appliedRetire.Version != exec.Version+1 {
		t.Fatalf("retirement execution version = %d, want %d", appliedRetire.Version, exec.Version+1)
	}
	if appliedRetire.Actions[0].Version != exec.Actions[0].Version+1 {
		t.Fatalf("retirement action version = %d, want %d", appliedRetire.Actions[0].Version, exec.Actions[0].Version+1)
	}
	assertRetiringLaunch(t, appliedRetire.Actions[0], workloadA, keyA, retireProofA)
	assertSingleActiveAction(t, appliedRetire)

	// Step 2: confirmed removal replaces A with the deterministic B.
	replaced, replaceCommit := mustReduce(t, appliedRetire, flow.RetiredFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		WorkloadID:  workloadA,
		At:          at.Add(2 * time.Minute),
	})
	appliedReplace := applyCommit(appliedRetire, replaceCommit)
	if !reflect.DeepEqual(replaced, appliedReplace) {
		t.Fatalf("replacement projection diverges from its commit\n got: %#v\nwant: %#v", replaced, appliedReplace)
	}
	if replaceCommit.Launch == nil {
		t.Fatal("retired fact allocated no replacement identity")
	}
	workloadB := replaceCommit.Launch.WorkloadID
	keyB := replaceCommit.Launch.IdempotencyKey
	if workloadB == workloadA {
		t.Fatalf("replacement reused the retired identity %q", workloadA)
	}
	if keyB == keyA {
		t.Fatalf("replacement reused the retired idempotency key %q", keyA)
	}
	if appliedReplace.Version != appliedRetire.Version+1 {
		t.Fatalf("replacement execution version = %d, want %d", appliedReplace.Version, appliedRetire.Version+1)
	}
	if appliedReplace.Actions[0].Version != appliedRetire.Actions[0].Version+1 {
		t.Fatalf("replacement action version = %d, want %d", appliedReplace.Actions[0].Version, appliedRetire.Actions[0].Version+1)
	}
	if appliedReplace.Actions[0].Container.WorkloadID != workloadB {
		t.Fatalf("replacement workload = %q, want %q", appliedReplace.Actions[0].Container.WorkloadID, workloadB)
	}
	if appliedReplace.Actions[0].Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("replacement launch state = %q, want not_issued", appliedReplace.Actions[0].Container.Launch.State)
	}
	if appliedReplace.Actions[1].State != kernel.ActionPending {
		t.Fatalf("pending successor state after replacement = %q, want pending", appliedReplace.Actions[1].State)
	}
	assertSingleActiveAction(t, appliedReplace)

	// Step 3: the replacement attempt is itself proven never started and retires.
	retiringB, retireBCommit := mustReduce(t, appliedReplace, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.NeverStarted(retireProofB),
		At:          at.Add(3 * time.Minute),
	})
	appliedRetireB := applyCommit(appliedReplace, retireBCommit)
	if !reflect.DeepEqual(retiringB, appliedRetireB) {
		t.Fatalf("second retirement projection diverges from its commit\n got: %#v\nwant: %#v", retiringB, appliedRetireB)
	}
	if appliedRetireB.Version != appliedReplace.Version+1 {
		t.Fatalf("second retirement execution version = %d, want %d", appliedRetireB.Version, appliedReplace.Version+1)
	}
	if appliedRetireB.Actions[0].Version != appliedReplace.Actions[0].Version+1 {
		t.Fatalf("second retirement action version = %d, want %d", appliedRetireB.Actions[0].Version, appliedReplace.Actions[0].Version+1)
	}
	assertRetiringLaunch(t, appliedRetireB.Actions[0], workloadB, keyB, retireProofB)
	assertSingleActiveAction(t, appliedRetireB)

	// Step 4: confirmed removal of B yields a distinct C.
	replacedAgain, replaceBCommit := mustReduce(t, appliedRetireB, flow.RetiredFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		WorkloadID:  workloadB,
		At:          at.Add(4 * time.Minute),
	})
	appliedReplaceB := applyCommit(appliedRetireB, replaceBCommit)
	if !reflect.DeepEqual(replacedAgain, appliedReplaceB) {
		t.Fatalf("second replacement projection diverges from its commit\n got: %#v\nwant: %#v", replacedAgain, appliedReplaceB)
	}
	if replaceBCommit.Launch == nil {
		t.Fatal("second retired fact allocated no replacement identity")
	}
	workloadC := replaceBCommit.Launch.WorkloadID
	keyC := replaceBCommit.Launch.IdempotencyKey
	if workloadC == workloadA || workloadC == workloadB {
		t.Fatalf("second replacement identity %q reused an earlier identity (A=%q B=%q)", workloadC, workloadA, workloadB)
	}
	if keyC == keyA || keyC == keyB {
		t.Fatalf("second replacement key %q reused an earlier key (A=%q B=%q)", keyC, keyA, keyB)
	}
	if appliedReplaceB.Version != appliedRetireB.Version+1 {
		t.Fatalf("second replacement execution version = %d, want %d", appliedReplaceB.Version, appliedRetireB.Version+1)
	}
	if appliedReplaceB.Actions[0].Version != appliedRetireB.Actions[0].Version+1 {
		t.Fatalf("second replacement action version = %d, want %d", appliedReplaceB.Actions[0].Version, appliedRetireB.Actions[0].Version+1)
	}
	if appliedReplaceB.Actions[0].Container.WorkloadID != workloadC {
		t.Fatalf("second replacement workload = %q, want %q", appliedReplaceB.Actions[0].Container.WorkloadID, workloadC)
	}
	if appliedReplaceB.Actions[0].Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("second replacement launch state = %q, want not_issued", appliedReplaceB.Actions[0].Container.Launch.State)
	}
	if appliedReplaceB.Actions[1].State != kernel.ActionPending {
		t.Fatalf("pending successor state after second replacement = %q, want pending", appliedReplaceB.Actions[1].State)
	}
	assertSingleActiveAction(t, appliedReplaceB)

	// Every intermediate state kept exactly one active action and the same
	// current action index; only the owned identity moved.
	for name, state := range map[string]kernel.Execution{
		"initial":         exec,
		"retiring A":      appliedRetire,
		"replaced with B": appliedReplace,
		"retiring B":      appliedRetireB,
		"replaced with C": appliedReplaceB,
	} {
		if state.CurrentAction != 0 {
			t.Fatalf("%s: CurrentAction = %d, want 0", name, state.CurrentAction)
		}
		if state.State != kernel.ExecutionRunning {
			t.Fatalf("%s: execution state = %q, want running", name, state.State)
		}
	}
}
