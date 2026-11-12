//go:build integration

package lifecycle

// Real-stack race schedules.
//
// Coverage map (docs/specs/06-shell-lifecycles.md S-REQUEST/S-HANDOFF):
//   - output submission versus runtime-confirmed exit-zero settlement;
//   - request resolution versus execution cancel;
//   - request resolution versus the persisted one-minute deadline;
//   - task ask versus task cancel at the same task lifecycle version.
//
// Every ordering is forced, never won by luck. A test-owned PostgreSQL row hold
// blocks the exact row a serialized commit must lock; the test observes through
// pg_stat_activity that the intended product statement is queued behind that
// hold (pg_blocking_pids), and only then releases it. The loser is issued with
// the pre-winner versions, so its refusal is deterministic. Both orders
// of each race run as explicit subtests and each order pins its own assertions.
//
// The request deadline race also proves the split clock the product
// deliberately has: the harness TestClock is frozen, so the application-level
// "before the deadline" read passes, while PostgreSQL statement_timestamp()
// really crosses the persisted deadline during a held commit. The persisted
// request deadline is two minutes so the real hold stays comfortably above the
// >= 60s requirement even when several real-stack copies run concurrently. That
// is a real wait; the test polls the database clock instead of sleeping
// blindly.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	raceWorkloadTokenPath    = "/workspace/race-token"
	raceWorkloadReleasePath  = "/workspace/race-release"
	raceIntegrateReleasePath = "/workspace/race-integrate-release"
	raceHoldTimeout          = 30 * time.Second
)

// raceIntegrateGate keeps the winner's integrate action running and quiescent
// until the test releases it, so the stale cancel is decided by the
// expected-version guard instead of a concurrent engine commit.
const raceIntegrateGate = `while [ ! -e ` + raceIntegrateReleasePath + ` ]; do sleep 1; done`

// raceBlockingCommands copies the execution credential into the retained
// workspace, then blocks until the test releases the workload. The credential
// copy is test evidence read back through podman exec; it is not logged, is not
// a receipt/event field and disappears with the retained workspace.
func raceBlockingCommands() []string {
	return []string{
		`cat /kodama/secrets/KODAMA_TOKEN > ` + raceWorkloadTokenPath,
		`while [ ! -e ` + raceWorkloadReleasePath + ` ]; do sleep 1; done`,
		`exit 0`,
	}
}

// applyRaceBlockingPipeline authors one container action that blocks on a
// workspace file until the test releases it.
func applyRaceBlockingPipeline(t *testing.T, harness *factory.Harness, name string) api.PipelineApplyResult {
	t.Helper()
	return applyPipeline(t, harness, name, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "work",
			Image:    harness.Image.Reference,
			Timeout:  containerTimeout,
			Outputs:  []string{"artifact"},
			Commands: raceBlockingCommands(),
		}},
	}))
}

// startBlockingExecution starts one blocking pipeline and returns the execution
// identity, the running workload container and the execution credential read
// from the workload workspace.
func startBlockingExecution(t *testing.T, harness *factory.Harness, name, key string) (string, string, string) {
	t.Helper()
	applied := applyRaceBlockingPipeline(t, harness, name)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, key)
	container, token := waitBlockingWorkload(t, harness, started.ExecutionID)
	return started.ExecutionID, container, token
}

// waitBlockingWorkload is the workload readiness barrier: the action is running,
// its container is running, and the credential copy exists in the workspace.
func waitBlockingWorkload(t *testing.T, harness *factory.Harness, executionID string) (string, string) {
	t.Helper()
	harness.Eventually(t, 2*time.Minute, func() bool {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		if current.State != "running" {
			return false
		}
		for _, action := range current.Actions {
			if action.Name == "work" && action.State == "running" {
				return true
			}
		}
		return false
	})
	var container string
	harness.Eventually(t, time.Minute, func() bool {
		found, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		container = found
		return true
	})
	var token string
	harness.Eventually(t, time.Minute, func() bool {
		value, err := harness.ReadContainerFile(t.Context(), container, raceWorkloadTokenPath)
		if err != nil || strings.TrimSpace(value) == "" {
			return false
		}
		token = strings.TrimSpace(value)
		return true
	})
	return container, token
}

// currentWorkAction re-inspects an execution and returns the running "work"
// action with the versions the next commit must carry.
func currentWorkAction(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	current := inspectExecution(t, harness, harness.ProjectToken, executionID)
	action := actionByName(t, current, "work")
	if action.State != "running" {
		t.Fatalf("work action state = %q; want running", action.State)
	}
	return current
}

// raceContentionTimeout bounds a barrier that waits for a product commit to
// queue behind a hold. It is an observation window, not a race deadline: the
// test never decides an ordering by how long it waited.
const raceContentionTimeout = 2 * time.Minute

// waitWorkloadObservation waits until the running container action already
// carries a persisted runtime observation. The first Running observation is the
// last engine commit before the exit observation, so after this barrier the
// engine issues no further action-row write until the workload exits. A race
// hold taken after it therefore cannot be polluted by an unrelated engine
// commit that would be served ahead of the intended winner.
func waitWorkloadObservation(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	harness.Eventually(t, 2*time.Minute, func() bool {
		row, err := harness.ActionRow(t.Context(), executionID, "work")
		return err == nil && row.ObservationSet
	})
}

// requireNoBlockedStatements proves the hold is quiet before the winner is
// issued. Without this, a background engine commit already queued behind the
// hold would be served first and the asserted winner/loser order would be a
// guess.
func requireNoBlockedStatements(t *testing.T, harness *factory.Harness, hold *factory.RowHold, table, label string) {
	t.Helper()
	observed, err := harness.BlockedStatements(t.Context(), hold, table)
	if err != nil {
		t.Fatalf("%s: read blocked statements before the race: %v", label, err)
	}
	if len(observed) != 0 {
		t.Fatalf("%s: %d product backend(s) were already queued behind the hold before the winner was issued: %+v",
			label, len(observed), observed)
	}
}

// requireContendingWaiters proves two distinct product backends are queued
// behind the same hold, the winner was observed first, and both statements are
// the scoped FOR UPDATE row lock the store issues inside its commit
// transaction. That is the pin which distinguishes a store refusal from a
// service pre-check refusal: the loser had already passed every service read
// and was waiting on the database lock when the hold was released.
func requireContendingWaiters(t *testing.T, waiters []factory.BlockedStatement, hold *factory.RowHold, winner factory.BlockedStatement, table, winnerLabel, loserLabel string) {
	t.Helper()
	if len(waiters) != 2 {
		t.Fatalf("%s/%s: observed %d contending waiters on %s; want exactly 2: %+v",
			winnerLabel, loserLabel, len(waiters), table, waiters)
	}
	if waiters[0].PID != winner.PID {
		t.Fatalf("%s/%s: first queued backend pid=%d, want the already-observed winner pid=%d: %+v",
			winnerLabel, loserLabel, waiters[0].PID, winner.PID, waiters)
	}
	if waiters[1].PID == winner.PID {
		t.Fatalf("%s/%s: the second waiter is the winner backend pid=%d; want a distinct loser backend: %+v",
			winnerLabel, loserLabel, waiters[1].PID, waiters)
	}
	if !slices.Contains(waiters[0].Blockers, hold.PID()) {
		t.Fatalf("%s/%s: winner blockers=%v do not include the hold pid=%d; want the winner directly queued behind the hold",
			winnerLabel, loserLabel, waiters[0].Blockers, hold.PID())
	}
	if !slices.Contains(waiters[1].Blockers, waiters[0].PID) && !slices.Contains(waiters[1].Blockers, hold.PID()) {
		t.Fatalf("%s/%s: loser blockers=%v include neither the winner pid=%d nor the hold pid=%d; want a queued FIFO waiter",
			winnerLabel, loserLabel, waiters[1].Blockers, waiters[0].PID, hold.PID())
	}
	if waiters[1].QueryStart.Before(waiters[0].QueryStart) {
		t.Fatalf("%s/%s: loser queued at %s before the winner at %s; want winner-first FIFO order: %+v",
			winnerLabel, loserLabel, waiters[1].QueryStart.UTC(), waiters[0].QueryStart.UTC(), waiters)
	}
	requireQueuedRowLock(t, waiters[0], table, winnerLabel)
	requireQueuedRowLock(t, waiters[1], table, loserLabel)
	t.Logf("%s then %s queued behind the same hold: winner pid=%d blockers=%v query_start=%s; loser pid=%d blockers=%v query_start=%s; loser query=%q",
		winnerLabel, loserLabel, waiters[0].PID, waiters[0].Blockers, waiters[0].QueryStart.UTC(),
		waiters[1].PID, waiters[1].Blockers, waiters[1].QueryStart.UTC(), waiters[1].Query)
}

// requireStoreVersionConflict asserts a queued loser was refused by the store
// guard after it had already passed the service pre-check.
func requireStoreVersionConflict(t *testing.T, outcome raceInvocation, label string) {
	t.Helper()
	if outcome.err != nil {
		t.Fatalf("%s transport: %v", label, outcome.err)
	}
	t.Logf("%s refused by the store: code=%q message=%q", label, outcome.publicError.Code, outcome.publicError.Message)
	if outcome.publicError.Code != api.CodeVersionConflict {
		t.Fatalf("%s code = %q (%s); want %q from the store guard",
			label, outcome.publicError.Code, outcome.publicError.Message, api.CodeVersionConflict)
	}
}

// awaitContention waits for want waiters behind the hold, but fails with the
// loser's public result if the loser returns before it ever queues. A loser
// refused by a service pre-check never reaches the store transaction, so this
// distinguishes missing contention from a slow barrier instead of timing out
// with no explanation.
func awaitContention(t *testing.T, ctx context.Context, harness *factory.Harness, hold *factory.RowHold, table string, want int, loser <-chan raceInvocation, loserLabel string) []factory.BlockedStatement {
	t.Helper()
	type barrierResult struct {
		waiters []factory.BlockedStatement
		err     error
	}
	done := make(chan barrierResult, 1)
	go func() {
		waiters, err := harness.WaitForBlockedStatements(ctx, hold, table, want, raceContentionTimeout)
		done <- barrierResult{waiters: waiters, err: err}
	}()
	// A nil channel is never ready, so a schedule whose second party is the
	// engine (and has no caller channel) still waits for the barrier only.
	var loserCase <-chan raceInvocation
	if loser != nil {
		loserCase = loser
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("%s did not contend: %v", loserLabel, result.err)
		}
		return result.waiters
	case outcome := <-loserCase:
		if outcome.err != nil {
			t.Fatalf("%s returned before contending: transport %v", loserLabel, outcome.err)
		}
		t.Fatalf("%s returned before contending: code=%q message=%q; want a queued store transaction",
			loserLabel, outcome.publicError.Code, outcome.publicError.Message)
		return nil
	}
}

// stageOutputsCurrent submits declared outputs from the workload credential at
// the current execution version. A version_conflict commits nothing, so a
// retry from a fresh inspect is safe (the workload does the same).
func stageOutputsCurrent(t *testing.T, harness *factory.Harness, token, executionID string, outputs map[string]string, key string) api.ExecutionActionOutputsResult {
	t.Helper()
	for range 20 {
		current := currentWorkAction(t, harness, executionID)
		action := actionByName(t, current, "work")
		result, publicError, err := harness.Invoke(t.Context(), token, "execution.action.outputs", api.ExecutionActionOutputsRequest{
			ExecutionID: executionID,
			ActionID:    action.ActionID,
			Outputs:     outputs,
		}, &current.Version, key)
		if err != nil {
			t.Fatalf("execution.action.outputs transport: %v", err)
		}
		switch publicError.Code {
		case "":
			var submitted api.ExecutionActionOutputsResult
			harness.Decode(result, &submitted)
			return submitted
		case api.CodeVersionConflict:
			continue
		default:
			t.Fatalf("execution.action.outputs refused: %s (%s)", publicError.Code, publicError.Message)
		}
	}
	t.Fatalf("output submission did not converge on a current execution version")
	return api.ExecutionActionOutputsResult{}
}

// TestRaceOutputSettlement races a workload output submission against the
// runtime-confirmed exit-zero settlement in both explicit orders.
func TestRaceOutputSettlement(t *testing.T) {
	harness := factory.Start(t)

	t.Run("sequential outputs first are staged then promoted by settlement", func(t *testing.T) {
		ctx := t.Context()
		const stagedValue = "staged-before-settlement"
		executionID, container, token := startBlockingExecution(t, harness, "race-outputs-first", "start-race-outputs-first")

		submitted := stageOutputsCurrent(t, harness, token, executionID, map[string]string{"artifact": stagedValue}, "race-outputs-first-submit")
		if submitted.ExecutionVersion == 0 || submitted.ActionVersion == 0 {
			t.Fatalf("output submission result = %+v; want positive versions", submitted)
		}

		row, err := harness.ActionRow(ctx, executionID, "work")
		if err != nil {
			t.Fatalf("read work action row after staging: %v", err)
		}
		if !row.StagedOutputsSet || !strings.Contains(row.StagedOutputs, stagedValue) {
			t.Fatalf("staged outputs = %q (set=%v); want the submitted value staged", row.StagedOutputs, row.StagedOutputsSet)
		}
		if row.Outputs != "" {
			t.Fatalf("promoted outputs = %q before settlement; want none", row.Outputs)
		}
		// Positive control: the running workload really carries credential
		// material, so the post-settlement "cleared" read is not vacuous.
		requireExecutionCredentialMaterial(t, harness, executionID, false, "running workload before settlement")

		// Only now does the runtime observe exit zero and settle the action.
		if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
			t.Fatalf("release workload: %v", err)
		}
		finished := waitExecution(t, harness, harness.ProjectToken, executionID)
		requireSucceeded(t, finished)
		work := actionByName(t, finished, "work")
		if work.Outputs["artifact"] != stagedValue {
			t.Fatalf("settled outputs = %+v; want artifact=%q", work.Outputs, stagedValue)
		}
		settled, err := harness.ActionRow(ctx, executionID, "work")
		if err != nil {
			t.Fatalf("read work action row after settlement: %v", err)
		}
		if settled.State != "succeeded" {
			t.Fatalf("work action row state = %q; want succeeded", settled.State)
		}
		if !strings.Contains(settled.Outputs, stagedValue) || settled.StagedOutputsSet || settled.StagedOutputs != "" {
			t.Fatalf("settled row outputs=%q staged=%q (set=%v); want promoted and cleared",
				settled.Outputs, settled.StagedOutputs, settled.StagedOutputsSet)
		}

		receipts, err := harness.ReceiptCount(ctx, "race-outputs-first-submit")
		if err != nil {
			t.Fatalf("count output receipts: %v", err)
		}
		if receipts != 1 {
			t.Fatalf("output submission receipt count = %d; want exactly 1", receipts)
		}
		requireEvent(t, readRequestEvents(t, harness, "action.transition"), "action.transition", work.ActionID)
		requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
		requireExecutionCredentialMaterial(t, harness, executionID, true, "settled execution")
		assertExecutionResourcesReleased(t, harness, executionID)
	})

	t.Run("sequential settlement first revokes the credential and refuses the late submission", func(t *testing.T) {
		ctx := t.Context()
		executionID, container, token := startBlockingExecution(t, harness, "race-settlement-first", "start-race-settlement-first")
		before := currentWorkAction(t, harness, executionID)
		beforeWork := actionByName(t, before, "work")
		requireExecutionCredentialMaterial(t, harness, executionID, false, "running workload before settlement")

		if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
			t.Fatalf("release workload: %v", err)
		}
		finished := waitExecution(t, harness, harness.ProjectToken, executionID)
		requireSucceeded(t, finished)

		// The terminal settlement revokes the execution credential in the same
		// commit, so the late submission cannot even authenticate. It never
		// reaches the version guard, which is why the refusal is
		// unauthenticated rather than version_conflict.
		_, publicError, err := harness.Invoke(ctx, token, "execution.action.outputs", api.ExecutionActionOutputsRequest{
			ExecutionID: executionID,
			ActionID:    beforeWork.ActionID,
			Outputs:     map[string]string{"artifact": "late-after-settlement"},
		}, &before.Version, "race-settlement-first-late")
		if err != nil {
			t.Fatalf("late output submission transport: %v", err)
		}
		if publicError.Code != api.CodeUnauthenticated {
			t.Fatalf("late output submission code = %q; want %q", publicError.Code, api.CodeUnauthenticated)
		}

		row, err := harness.ActionRow(ctx, executionID, "work")
		if err != nil {
			t.Fatalf("read work action row: %v", err)
		}
		if row.Outputs != "" || row.StagedOutputsSet || row.StagedOutputs != "" {
			t.Fatalf("late submission wrote outputs=%q staged=%q (set=%v); want no loser write",
				row.Outputs, row.StagedOutputs, row.StagedOutputsSet)
		}
		receipts, err := harness.ReceiptCount(ctx, "race-settlement-first-late")
		if err != nil {
			t.Fatalf("count late submission receipts: %v", err)
		}
		if receipts != 0 {
			t.Fatalf("late submission committed %d receipts; want 0", receipts)
		}
		after := inspectExecution(t, harness, harness.ProjectToken, executionID)
		if after.State != "succeeded" {
			t.Fatalf("late submission changed the execution state to %q", after.State)
		}
		requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
		requireExecutionCredentialMaterial(t, harness, executionID, true, "settled execution")
		assertExecutionResourcesReleased(t, harness, executionID)
	})

	t.Run("contested settlement first refuses the in-flight authenticated submission at the store guard", func(t *testing.T) {
		testSettlementFirstContested(t, harness)
	})
	t.Run("contested outputs first stages then promotes exactly once", func(t *testing.T) {
		testOutputsFirstContested(t, harness)
	})
}

// testSettlementFirstContested holds the execution/action rows, releases the
// workload so the runtime-confirmed settlement commit queues behind the hold,
// then submits declared outputs from the still-valid execution credential at
// the same pre-settlement versions. The submission passes every service read
// and queues second behind the settlement; releasing the hold makes the
// settlement win and the submission lose at the store guard.
func testSettlementFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	const loserValue = "late-contested-after-settlement"
	executionID, container, token := startBlockingExecution(t, harness, "race-settlement-first-contested", "start-race-settlement-first-contested")
	waitWorkloadObservation(t, harness, executionID)
	before := currentWorkAction(t, harness, executionID)
	beforeWork := actionByName(t, before, "work")
	requireExecutionCredentialMaterial(t, harness, executionID, false, "running workload before settlement")

	hold, err := harness.HoldExecutionRows(ctx, executionID, beforeWork.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "executions", "settlement-first contested")

	// The runtime-confirmed exit-zero settlement is the winner and must queue
	// first. Only after it is observed blocked does the submission run.
	if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release workload: %v", err)
	}
	settlementBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceContentionTimeout)
	if err != nil {
		t.Fatalf("runtime settlement did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, settlementBlocked, "executions", "settlement winner")
	t.Logf("settlement commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		settlementBlocked.PID, settlementBlocked.WaitEventType, settlementBlocked.WaitEvent, settlementBlocked.Query)

	// The credential is still live and the versions are still the pre-winner
	// versions because the settlement has not committed. The submission passes
	// authentication and every service pre-check, then queues its own commit.
	submission := invokeAsync(t, harness, token, "execution.action.outputs", api.ExecutionActionOutputsRequest{
		ExecutionID: executionID,
		ActionID:    beforeWork.ActionID,
		Outputs:     map[string]string{"artifact": loserValue},
	}, &before.Version, "race-settlement-first-contested-late")

	waiters := awaitContention(t, ctx, harness, hold, "executions", 2, submission, "outputs loser")
	requireContendingWaiters(t, waiters, hold, settlementBlocked, "executions", "settlement winner", "outputs loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release settlement-first contested hold: %v", err)
	}

	loser := awaitInvocation(t, submission, 2*time.Minute, "late output submission")
	requireStoreVersionConflict(t, loser, "late output submission")

	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	requireSucceeded(t, finished)
	row, err := harness.ActionRow(ctx, executionID, "work")
	if err != nil {
		t.Fatalf("read work action row: %v", err)
	}
	if row.Outputs != "" || row.StagedOutputsSet || row.StagedOutputs != "" {
		t.Fatalf("loser submission wrote outputs=%q staged=%q (set=%v); want no loser write",
			row.Outputs, row.StagedOutputs, row.StagedOutputsSet)
	}
	receipts, err := harness.ReceiptCount(ctx, "race-settlement-first-contested-late")
	if err != nil {
		t.Fatalf("count late submission receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("late submission committed %d receipts; want 0", receipts)
	}
	after := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if after.State != "succeeded" {
		t.Fatalf("late submission changed the execution state to %q", after.State)
	}
	requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
	requireExecutionCredentialMaterial(t, harness, executionID, true, "settled execution")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// testOutputsFirstContested holds the execution/action rows and queues the
// output submission first, then releases the workload so the settlement commit
// queues second. The submission stages first; the settlement loses that
// version, re-reads and promotes the staged values exactly once.
func testOutputsFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	const stagedValue = "contested-staged-once"
	executionID, container, token := startBlockingExecution(t, harness, "race-outputs-first-contested", "start-race-outputs-first-contested")
	waitWorkloadObservation(t, harness, executionID)
	before := currentWorkAction(t, harness, executionID)
	beforeWork := actionByName(t, before, "work")

	hold, err := harness.HoldExecutionRows(ctx, executionID, beforeWork.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "executions", "outputs-first contested")

	submission := invokeAsync(t, harness, token, "execution.action.outputs", api.ExecutionActionOutputsRequest{
		ExecutionID: executionID,
		ActionID:    beforeWork.ActionID,
		Outputs:     map[string]string{"artifact": stagedValue},
	}, &before.Version, "race-outputs-first-contested-submit")
	submissionBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceContentionTimeout)
	if err != nil {
		t.Fatalf("output submission did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, submissionBlocked, "executions", "outputs winner")
	t.Logf("output submission queued behind the hold: pid=%d wait=%s/%s query=%q",
		submissionBlocked.PID, submissionBlocked.WaitEventType, submissionBlocked.WaitEvent, submissionBlocked.Query)

	// The submission is the first committer. Only now does the runtime observe
	// the exit and queue the settlement behind it.
	if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release workload: %v", err)
	}
	waiters := awaitContention(t, ctx, harness, hold, "executions", 2, nil, "settlement loser")
	requireContendingWaiters(t, waiters, hold, submissionBlocked, "executions", "outputs winner", "settlement loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release outputs-first contested hold: %v", err)
	}

	winner := awaitInvocation(t, submission, 2*time.Minute, "output submission winner")
	requireInvocationSuccess(t, winner, "output submission winner")
	var submitted api.ExecutionActionOutputsResult
	harness.Decode(winner.result, &submitted)
	if submitted.ExecutionVersion == 0 || submitted.ActionVersion == 0 {
		t.Fatalf("output submission result = %+v; want positive versions", submitted)
	}

	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	requireSucceeded(t, finished)
	work := actionByName(t, finished, "work")
	if work.Outputs["artifact"] != stagedValue {
		t.Fatalf("settled outputs = %+v; want artifact=%q promoted exactly once", work.Outputs, stagedValue)
	}
	settled, err := harness.ActionRow(ctx, executionID, "work")
	if err != nil {
		t.Fatalf("read work action row after settlement: %v", err)
	}
	if settled.State != "succeeded" {
		t.Fatalf("work action row state = %q; want succeeded", settled.State)
	}
	if !strings.Contains(settled.Outputs, stagedValue) || settled.StagedOutputsSet || settled.StagedOutputs != "" {
		t.Fatalf("settled row outputs=%q staged=%q (set=%v); want promoted and cleared",
			settled.Outputs, settled.StagedOutputs, settled.StagedOutputsSet)
	}
	receipts, err := harness.ReceiptCount(ctx, "race-outputs-first-contested-submit")
	if err != nil {
		t.Fatalf("count output submission receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("output submission receipt count = %d; want exactly 1", receipts)
	}
	requireEvent(t, readRequestEvents(t, harness, "action.transition"), "action.transition", work.ActionID)
	requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
	requireExecutionCredentialMaterial(t, harness, executionID, true, "settled execution")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// TestRaceResolve races request resolution against execution cancel, and a
// resolution admitted before its deadline against the real database clock. Each
// resolve/cancel order runs twice: the sequential order keeps the original
// stale-loser coverage, and the contested order proves both commits queue at
// the same database row before either can commit.
func TestRaceResolve(t *testing.T) {
	t.Run("resolve versus cancel", func(t *testing.T) {
		harness := factory.Start(t)
		t.Run("sequential resolve first wins and cancel is stale", func(t *testing.T) {
			testResolveFirst(t, harness)
		})
		t.Run("sequential cancel first wins and resolve is stale", func(t *testing.T) {
			testCancelFirst(t, harness)
		})
		t.Run("contested resolve first wins at the store guard", func(t *testing.T) {
			testResolveFirstContested(t, harness)
		})
		t.Run("contested cancel first wins at the store guard", func(t *testing.T) {
			testCancelFirstContested(t, harness)
		})
	})

	t.Run("resolve versus deadline", func(t *testing.T) {
		t.Run("resolve before the deadline preserves its answer", func(t *testing.T) {
			testResolveBeforeDeadline(t, factory.Start(t))
		})
		t.Run("resolve held past the database deadline is deadline_expired", func(t *testing.T) {
			testResolveHeldPastDeadline(t, factory.Start(t))
		})
	})
}

// startRequestWait authors the prepare -> approve -> integrate pipeline, starts
// it and waits for the reviewer-assigned request action to be the running action.
func startRequestWait(t *testing.T, harness *factory.Harness, name, timeout, key string, integrateExtra []string) (string, api.ExecutionInspectResult, api.ExecutionAction) {
	t.Helper()
	applied := applyRequestPipeline(t, harness, name, timeout, nil, integrateExtra)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, key)
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	return started.ExecutionID, waiting, actionByName(t, waiting, "approve")
}

// testResolveFirst holds the execution and action rows, queues the resolve
// behind the hold, proves the waiter exists, releases the hold so the resolve
// commits first, then issues a cancel carrying the pre-resolve version.
func testResolveFirst(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	executionID, _, approve := startRequestWait(t, harness, "race-resolve-first", requestWaitTimeout, "start-race-resolve-first", []string{raceIntegrateGate})

	hold, err := harness.HoldExecutionRows(ctx, executionID, approve.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	// The hold freezes every version advance, so the versions read now stay the
	// expected versions until the hold is released.
	before := inspectExecution(t, harness, harness.ProjectToken, executionID)
	beforeApprove := actionByName(t, before, "approve")
	call := invokeAsync(t, harness, harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              beforeApprove.ActionID,
		ExpectedActionVersion: beforeApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &before.Version, "race-resolve-first")
	blocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceHoldTimeout)
	if err != nil {
		t.Fatalf("resolve did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, blocked, "executions", "resolve")
	t.Logf("resolve commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		blocked.PID, blocked.WaitEventType, blocked.WaitEvent, blocked.Query)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release resolve hold: %v", err)
	}

	outcome := awaitInvocation(t, call, 2*time.Minute, "resolve")
	requireInvocationSuccess(t, outcome, "resolve")
	var resolved api.ExecutionRequestResolveResult
	harness.Decode(outcome.result, &resolved)
	if resolved.ExecutionVersion <= before.Version || resolved.ActionVersion <= beforeApprove.Version {
		t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
			resolved, before.Version, beforeApprove.Version)
	}

	// The winner's answer advances the pipeline to the integrate container
	// action. Wait until that workload is running and gated, so the stale cancel
	// is evaluated against a quiescent running execution: the refusal must come
	// from the expected-version guard, not from a concurrent engine commit.
	waitRequestRunning(t, harness, executionID, "integrate")
	var integrateContainer string
	harness.Eventually(t, time.Minute, func() bool {
		found, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		integrateContainer = found
		return true
	})

	// The cancel carries the versions from before the resolve committed. It
	// must lose without writing anything.
	_, publicError, err := harness.Invoke(ctx, harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: executionID,
	}, &before.Version, "race-resolve-first-cancel")
	if err != nil {
		t.Fatalf("loser cancel transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("loser cancel code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	afterCancel := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if afterCancel.State != "running" || actionByName(t, afterCancel, "integrate").State != "running" {
		t.Fatalf("loser cancel changed the execution/integrate action to %q/%q; want running/running",
			afterCancel.State, actionByName(t, afterCancel, "integrate").State)
	}

	// Release the gated winner workload and let it settle.
	if err := harness.TouchContainerFile(ctx, integrateContainer, raceIntegrateReleasePath); err != nil {
		t.Fatalf("release integrate workload: %v", err)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	requireSucceeded(t, finished)
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.Decision != "pass" || finalApprove.Actor != factory.ReviewerPrincipal || finalApprove.AnsweredAt == nil {
		t.Fatalf("resolved answer = %+v; want pass by the reviewer with an answered time", finalApprove)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 1, "resolve")
	raceReceiptCount(t, harness, "race-resolve-first-cancel", 0, "loser cancel")
	requireEvent(t, readRequestEvents(t, harness, "request.resolved"), "request.resolved", executionID)
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 1)
	requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// testCancelFirst holds the execution and action rows, queues the cancel behind
// the hold, proves the waiter exists, releases the hold so the cancel commits
// first, then issues a resolve carrying the pre-cancel versions.
func testCancelFirst(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	executionID, _, approve := startRequestWait(t, harness, "race-cancel-first", requestWaitTimeout, "start-race-cancel-first", nil)

	hold, err := harness.HoldExecutionRows(ctx, executionID, approve.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	before := inspectExecution(t, harness, harness.ProjectToken, executionID)
	beforeApprove := actionByName(t, before, "approve")
	call := invokeAsync(t, harness, harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: executionID,
	}, &before.Version, "race-cancel-first")
	blocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceHoldTimeout)
	if err != nil {
		t.Fatalf("cancel did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, blocked, "executions", "cancel")
	t.Logf("cancel commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		blocked.PID, blocked.WaitEventType, blocked.WaitEvent, blocked.Query)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release cancel hold: %v", err)
	}
	outcome := awaitInvocation(t, call, 2*time.Minute, "cancel")
	requireInvocationSuccess(t, outcome, "cancel")
	var cancelled api.ExecutionCancelResult
	harness.Decode(outcome.result, &cancelled)
	if cancelled.ExecutionVersion <= before.Version {
		t.Fatalf("cancel version = %d; want > %d", cancelled.ExecutionVersion, before.Version)
	}

	// The resolve carries the versions from before the cancel committed.
	_, publicError, err := harness.Invoke(ctx, harness.ProjectToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              beforeApprove.ActionID,
		ExpectedActionVersion: beforeApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &before.Version, "race-cancel-first-resolve")
	if err != nil {
		t.Fatalf("loser resolve transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("loser resolve code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}

	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	if finished.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", finished.State)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "cancelled" || finalApprove.Decision != "" || finalApprove.Actor != "" || finalApprove.AnsweredAt != nil {
		t.Fatalf("cancelled request retained an answer: %+v", finalApprove)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 0, "loser resolve")
	raceReceiptCount(t, harness, "race-cancel-first", 1, "cancel")
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 0)
	requireSingleTerminalEvent(t, harness, executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// testResolveFirstContested holds the execution/action rows, queues the resolve
// winner behind the hold, then issues the cancel loser with the same pre-winner
// versions. Both requests pass their service reads while the winner is queued
// and uncommitted, both queue on the store's scoped row lock, and the hold is
// released only after two distinct backends are observed. The resolve wins and
// the cancel is refused by the store's expected-version guard.
func testResolveFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	executionID, _, approve := startRequestWait(t, harness, "race-resolve-first-contested", requestWaitTimeout, "start-race-resolve-first-contested", []string{raceIntegrateGate})

	hold, err := harness.HoldExecutionRows(ctx, executionID, approve.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "executions", "resolve-first contested")
	before := inspectExecution(t, harness, harness.ProjectToken, executionID)
	beforeApprove := actionByName(t, before, "approve")
	winner := invokeAsync(t, harness, harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              beforeApprove.ActionID,
		ExpectedActionVersion: beforeApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &before.Version, "race-resolve-first-contested")
	winnerBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceContentionTimeout)
	if err != nil {
		t.Fatalf("resolve winner did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, winnerBlocked, "executions", "resolve winner")

	// Same pre-winner expected version: the loser's service reads run while the
	// winner is queued and uncommitted, so the service pre-check passes and the
	// loser queues its own store transaction behind the winner.
	loser := invokeAsync(t, harness, harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: executionID,
	}, &before.Version, "race-resolve-first-contested-cancel")
	waiters := awaitContention(t, ctx, harness, hold, "executions", 2, loser, "cancel loser")
	requireContendingWaiters(t, waiters, hold, winnerBlocked, "executions", "resolve winner", "cancel loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release resolve-first contested hold: %v", err)
	}
	winnerOutcome := awaitInvocation(t, winner, 2*time.Minute, "resolve winner")
	requireInvocationSuccess(t, winnerOutcome, "resolve winner")
	var resolved api.ExecutionRequestResolveResult
	harness.Decode(winnerOutcome.result, &resolved)
	if resolved.ExecutionVersion <= before.Version || resolved.ActionVersion <= beforeApprove.Version {
		t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
			resolved, before.Version, beforeApprove.Version)
	}
	loserOutcome := awaitInvocation(t, loser, 2*time.Minute, "cancel loser")
	requireStoreVersionConflict(t, loserOutcome, "cancel loser")

	// The winner's answer advances the pipeline to the gated integrate
	// workload. Release it and assert the same final state as the sequential
	// order.
	waitRequestRunning(t, harness, executionID, "integrate")
	var integrateContainer string
	harness.Eventually(t, time.Minute, func() bool {
		found, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		integrateContainer = found
		return true
	})
	afterCancel := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if afterCancel.State != "running" || actionByName(t, afterCancel, "integrate").State != "running" {
		t.Fatalf("loser cancel changed the execution/integrate action to %q/%q; want running/running",
			afterCancel.State, actionByName(t, afterCancel, "integrate").State)
	}
	if err := harness.TouchContainerFile(ctx, integrateContainer, raceIntegrateReleasePath); err != nil {
		t.Fatalf("release integrate workload: %v", err)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	requireSucceeded(t, finished)
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.Decision != "pass" || finalApprove.Actor != factory.ReviewerPrincipal || finalApprove.AnsweredAt == nil {
		t.Fatalf("resolved answer = %+v; want pass by the reviewer with an answered time", finalApprove)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 1, "resolve")
	raceReceiptCount(t, harness, "race-resolve-first-contested-cancel", 0, "loser cancel")
	requireEvent(t, readRequestEvents(t, harness, "request.resolved"), "request.resolved", executionID)
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 1)
	requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// testCancelFirstContested holds the execution/action rows, queues the cancel
// winner behind the hold, then issues the resolve loser with the same
// pre-winner versions. Both requests pass their service reads while the winner
// is queued and uncommitted, both queue on the store's scoped row lock, and the
// hold is released only after two distinct backends are observed. The cancel
// wins and the resolve is refused by the store's expected-version guard.
func testCancelFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	executionID, _, approve := startRequestWait(t, harness, "race-cancel-first-contested", requestWaitTimeout, "start-race-cancel-first-contested", nil)

	hold, err := harness.HoldExecutionRows(ctx, executionID, approve.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "executions", "cancel-first contested")
	before := inspectExecution(t, harness, harness.ProjectToken, executionID)
	beforeApprove := actionByName(t, before, "approve")
	winner := invokeAsync(t, harness, harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: executionID,
	}, &before.Version, "race-cancel-first-contested")
	winnerBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceContentionTimeout)
	if err != nil {
		t.Fatalf("cancel winner did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, winnerBlocked, "executions", "cancel winner")

	loser := invokeAsync(t, harness, harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              beforeApprove.ActionID,
		ExpectedActionVersion: beforeApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &before.Version, "race-cancel-first-contested-resolve")
	waiters := awaitContention(t, ctx, harness, hold, "executions", 2, loser, "resolve loser")
	requireContendingWaiters(t, waiters, hold, winnerBlocked, "executions", "cancel winner", "resolve loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release cancel-first contested hold: %v", err)
	}
	winnerOutcome := awaitInvocation(t, winner, 2*time.Minute, "cancel winner")
	requireInvocationSuccess(t, winnerOutcome, "cancel winner")
	var cancelled api.ExecutionCancelResult
	harness.Decode(winnerOutcome.result, &cancelled)
	if cancelled.ExecutionVersion <= before.Version {
		t.Fatalf("cancel version = %d; want > %d", cancelled.ExecutionVersion, before.Version)
	}
	loserOutcome := awaitInvocation(t, loser, 2*time.Minute, "resolve loser")
	requireStoreVersionConflict(t, loserOutcome, "resolve loser")

	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	if finished.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", finished.State)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "cancelled" || finalApprove.Decision != "" || finalApprove.Actor != "" || finalApprove.AnsweredAt != nil {
		t.Fatalf("cancelled request retained an answer: %+v", finalApprove)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 0, "loser resolve")
	raceReceiptCount(t, harness, "race-cancel-first-contested", 1, "cancel")
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 0)
	requireSingleTerminalEvent(t, harness, executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// testResolveBeforeDeadline resolves promptly and then advances the harness
// clock past the deadline. A committed answer is final: the later clock advance
// must not turn the execution into a timeout.
func testResolveBeforeDeadline(t *testing.T, harness *factory.Harness) {
	executionID, waiting, approve := startRequestWait(t, harness, "race-resolve-before-deadline", "1m", "start-race-resolve-before-deadline", nil)
	if approve.Deadline == nil {
		t.Fatalf("request has no persisted deadline: %+v", approve)
	}
	requestResolveCurrent(t, harness, harness.ReviewerToken, executionID, "approve",
		"pass", map[string]string{"approved": requestApproval}, "race-resolve-before-deadline")

	harness.Clock.Advance(61 * time.Second)

	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	requireSucceeded(t, finished)
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.Decision != "pass" || finalApprove.Actor != factory.ReviewerPrincipal || finalApprove.AnsweredAt == nil {
		t.Fatalf("committed answer = %+v; a later clock advance must not discard it", finalApprove)
	}
	if finished.ExecutionID != executionID || waiting.Version == 0 {
		t.Fatalf("resolve-first execution identity/version = %q/%d", finished.ExecutionID, waiting.Version)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 1, "resolve")
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 1)
	requireSingleTerminalEvent(t, harness, executionID, "execution.succeeded")
}

// testResolveHeldPastDeadline admits a resolve before the application deadline,
// queues its commit behind a row hold until PostgreSQL statement_timestamp()
// really crosses the persisted two-minute deadline, then releases it. The store
// deadline guard must refuse it with deadline_expired and commit nothing; the
// controller scan then settles the timeout with no answer.
func testResolveHeldPastDeadline(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	executionID, _, approve := startRequestWait(t, harness, "race-resolve-held-deadline", "2m", "start-race-resolve-held-deadline", nil)
	if approve.Deadline == nil {
		t.Fatalf("request has no persisted deadline: %+v", approve)
	}

	hold, err := harness.HoldExecutionRows(ctx, executionID, approve.ActionID)
	if err != nil {
		t.Fatalf("hold execution/action rows: %v", err)
	}
	before := inspectExecution(t, harness, harness.ProjectToken, executionID)
	beforeApprove := actionByName(t, before, "approve")
	call := invokeAsync(t, harness, harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              beforeApprove.ActionID,
		ExpectedActionVersion: beforeApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &before.Version, "race-resolve-held-deadline")
	deadlineBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceHoldTimeout)
	if err != nil {
		t.Fatalf("resolve did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, deadlineBlocked, "executions", "late resolve")
	t.Logf("late resolve commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		deadlineBlocked.PID, deadlineBlocked.WaitEventType, deadlineBlocked.WaitEvent, deadlineBlocked.Query)
	// The premise of the race is that the commit is already queued while the
	// real database clock is still before the persisted deadline. If setup were
	// slow enough that the clock had already crossed, releasing the hold would
	// still expire the answer but would not prove the held-commit crossing, so
	// refuse the vacuous ordering explicitly.
	queuedAt, err := harness.DatabaseTime(ctx)
	if err != nil {
		t.Fatalf("read database clock while the resolve is queued: %v", err)
	}
	if !queuedAt.Before(*approve.Deadline) {
		t.Fatalf("resolve queued at database time %s, at or after the persisted deadline %s; the held commit cannot prove the crossing",
			queuedAt.UTC(), approve.Deadline.UTC())
	}
	t.Logf("resolve queued at database time %s, before the persisted deadline %s", queuedAt.UTC(), approve.Deadline.UTC())

	// The real database clock must cross the persisted deadline while the
	// commit is still queued. The harness TestClock stays frozen, so this is the
	// store's transaction-clock guard, not an application-clock expiry.
	crossed, err := harness.WaitForDatabaseTimeAfter(ctx, approve.Deadline.Add(time.Second), 3*time.Minute)
	if err != nil {
		t.Fatalf("database clock did not cross the request deadline: %v", err)
	}
	t.Logf("database clock crossed the deadline at %s", crossed.UTC())
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release deadline hold: %v", err)
	}

	outcome := awaitInvocation(t, call, 2*time.Minute, "late resolve")
	if outcome.err != nil {
		t.Fatalf("late resolve transport: %v", outcome.err)
	}
	if outcome.publicError.Code != api.CodeDeadlineExpired {
		t.Fatalf("late resolve code = %q; want %q", outcome.publicError.Code, api.CodeDeadlineExpired)
	}
	afterRefusal := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if afterRefusal.State != "running" {
		t.Fatalf("refused resolve changed the execution state to %q", afterRefusal.State)
	}
	if afterRefusal.Version != before.Version {
		t.Fatalf("refused resolve advanced the execution version to %d; want %d", afterRefusal.Version, before.Version)
	}
	refusedApprove := actionByName(t, afterRefusal, "approve")
	if refusedApprove.Decision != "" || refusedApprove.Actor != "" || refusedApprove.AnsweredAt != nil {
		t.Fatalf("refused resolve recorded an answer: %+v", refusedApprove)
	}
	raceReceiptCount(t, harness, executionID+"/"+approve.ActionID, 0, "refused resolve")

	// Only now does the application clock pass the deadline, so the controller
	// scan settles the timeout without an answer. Derive the advance from the
	// persisted deadline so widening the request timeout cannot silently leave
	// the frozen application clock before it.
	harness.Clock.Advance(approve.Deadline.Sub(harness.Clock.Now()) + time.Second)
	finished := waitExecution(t, harness, harness.ProjectToken, executionID)
	if finished.State != "failed" {
		t.Fatalf("expired execution state = %q; want failed", finished.State)
	}
	if finished.Failure == nil || finished.Failure.Kind != "timeout" {
		t.Fatalf("expired execution failure = %+v; want kind timeout", finished.Failure)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "failed" || finalApprove.Decision != "" || finalApprove.Actor != "" || finalApprove.AnsweredAt != nil {
		t.Fatalf("expired request retained an answer: %+v", finalApprove)
	}
	requireSubjectEventCount(t, harness, "request.resolved", executionID, 0)
	requireSingleTerminalEvent(t, harness, executionID, "execution.failed")
	assertExecutionResourcesReleased(t, harness, executionID)
}

// TestRaceAskCancel races task.ask against task.cancel at the same task
// lifecycle version, in both explicit orders. Each order runs twice: the
// sequential order keeps the original stale-loser coverage, and the contested
// order proves both commits queue at the same database row before either can
// commit. The contested orders use different principals for the two operations
// so their authority membership pins cannot serialize against each other before
// the task row lock the race targets.
func TestRaceAskCancel(t *testing.T) {
	harness := factory.Start(t)

	t.Run("sequential ask first leaves a waiting question and a cancelled predecessor", func(t *testing.T) {
		testAskFirst(t, harness)
	})
	t.Run("sequential cancel first leaves a cancelled task with no question", func(t *testing.T) {
		testCancelTaskFirst(t, harness)
	})
	t.Run("contested ask first wins at the store guard", func(t *testing.T) {
		testAskFirstContested(t, harness)
	})
	t.Run("contested cancel first wins at the store guard", func(t *testing.T) {
		testCancelTaskFirstContested(t, harness)
	})
}

// raceTask is one open task whose current execution is a running blocking
// workload.
type raceTask struct {
	taskID      string
	pipelineID  string
	executionID string
	container   string
	token       string
}

// startRaceTask creates and starts one task over the blocking pipeline and
// waits until its execution is running with a readable credential.
func startRaceTask(t *testing.T, harness *factory.Harness, name, key string) raceTask {
	t.Helper()
	applied := applyRaceBlockingPipeline(t, harness, name)
	created := harness.MustREST(harness.ProjectToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      name,
		PipelineID: applied.PipelineID,
	}, nil, "create-"+key)
	var task api.TaskCreateResult
	harness.Decode(created, &task)
	started := harness.MustREST(harness.ProjectToken, "task.start", api.TaskStartRequest{
		TaskID: task.TaskID,
	}, &task.LifecycleVersion, "start-"+key)
	var execution api.TaskStartResult
	harness.Decode(started, &execution)
	container, token := waitBlockingWorkload(t, harness, execution.ExecutionID)
	// Prove the task is readable before the race starts; the race tests take
	// their expected lifecycle version from a fresh read under the hold.
	raceReadTask(t, harness, task.TaskID)
	return raceTask{
		taskID:      task.TaskID,
		pipelineID:  applied.PipelineID,
		executionID: execution.ExecutionID,
		container:   container,
		token:       token,
	}
}

// raceReadTask reads one task through the public API with the project token.
func raceReadTask(t *testing.T, harness *factory.Harness, taskID string) api.TaskReadResult {
	t.Helper()
	return readTask(t, harness, harness.ProjectToken, taskID)
}

// testAskFirst holds the task row, queues the ask behind the hold, releases it
// so the ask commits first, then issues a cancel with the pre-ask version.
func testAskFirst(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	task := startRaceTask(t, harness, "race-ask-first", "race-ask-first")
	requireExecutionCredentialMaterial(t, harness, task.executionID, false, "running predecessor before ask")

	hold, err := harness.HoldTaskRow(ctx, task.taskID)
	if err != nil {
		t.Fatalf("hold task row: %v", err)
	}
	before := raceReadTask(t, harness, task.taskID)
	call := invokeAsync(t, harness, harness.ProjectToken, "task.ask", api.TaskAskRequest{
		TaskID:     task.taskID,
		Question:   "Which artifact should be promoted?",
		Assignment: "role/reviewer",
	}, &before.LifecycleVersion, "race-ask-first")
	askBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "tasks", raceHoldTimeout)
	if err != nil {
		t.Fatalf("ask did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, askBlocked, "tasks", "ask")
	t.Logf("ask commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		askBlocked.PID, askBlocked.WaitEventType, askBlocked.WaitEvent, askBlocked.Query)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release ask hold: %v", err)
	}
	outcome := awaitInvocation(t, call, 2*time.Minute, "ask")
	requireInvocationSuccess(t, outcome, "ask")
	var asked api.TaskAskResult
	harness.Decode(outcome.result, &asked)
	if asked.LifecycleVersion <= before.LifecycleVersion || asked.QuestionVersion == 0 {
		t.Fatalf("ask result = %+v; want an advanced lifecycle and a question version", asked)
	}

	// The loser cancel carries the pre-ask lifecycle version.
	_, publicError, err := harness.Invoke(ctx, harness.ProjectToken, "task.cancel", api.TaskCancelRequest{
		TaskID: task.taskID,
	}, &before.LifecycleVersion, "race-ask-first-cancel")
	if err != nil {
		t.Fatalf("loser cancel transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("loser cancel code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}

	waiting := raceReadTask(t, harness, task.taskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question: %+v", waiting)
	}
	if waiting.Question.Assignment != "role/reviewer" || waiting.Question.Version == 0 {
		t.Fatalf("outstanding question = %+v; want role/reviewer with a version", waiting.Question)
	}

	finished := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if finished.State != "cancelled" {
		t.Fatalf("predecessor execution state = %q; want cancelled", finished.State)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "cancelled predecessor")
	raceReceiptCount(t, harness, "race-ask-first", 1, "ask")
	raceReceiptCount(t, harness, "race-ask-first-cancel", 0, "loser cancel")
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 1)
	requireSubjectEventCount(t, harness, "task.cancelled", task.taskID, 0)
	requireSingleTerminalEvent(t, harness, task.executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, task.executionID)
}

// testCancelTaskFirst holds the task row, queues the cancel behind the hold,
// releases it so the cancel commits first, then issues an ask with the
// pre-cancel version.
func testCancelTaskFirst(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	task := startRaceTask(t, harness, "race-cancel-task-first", "race-cancel-task-first")
	requireExecutionCredentialMaterial(t, harness, task.executionID, false, "running predecessor before cancel")

	hold, err := harness.HoldTaskRow(ctx, task.taskID)
	if err != nil {
		t.Fatalf("hold task row: %v", err)
	}
	before := raceReadTask(t, harness, task.taskID)
	call := invokeAsync(t, harness, harness.ProjectToken, "task.cancel", api.TaskCancelRequest{
		TaskID: task.taskID,
	}, &before.LifecycleVersion, "race-cancel-task-first")
	cancelBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "tasks", raceHoldTimeout)
	if err != nil {
		t.Fatalf("cancel did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, cancelBlocked, "tasks", "cancel")
	t.Logf("cancel commit queued behind the hold: pid=%d wait=%s/%s query=%q",
		cancelBlocked.PID, cancelBlocked.WaitEventType, cancelBlocked.WaitEvent, cancelBlocked.Query)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release cancel hold: %v", err)
	}
	outcome := awaitInvocation(t, call, 2*time.Minute, "cancel")
	requireInvocationSuccess(t, outcome, "cancel")
	var cancelled api.TaskCancelResult
	harness.Decode(outcome.result, &cancelled)
	if cancelled.LifecycleVersion <= before.LifecycleVersion {
		t.Fatalf("cancel lifecycle version = %d; want > %d", cancelled.LifecycleVersion, before.LifecycleVersion)
	}

	_, publicError, err := harness.Invoke(ctx, harness.ProjectToken, "task.ask", api.TaskAskRequest{
		TaskID:     task.taskID,
		Question:   "Which artifact should be promoted?",
		Assignment: "role/reviewer",
	}, &before.LifecycleVersion, "race-cancel-task-first-ask")
	if err != nil {
		t.Fatalf("loser ask transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("loser ask code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}

	closed := raceReadTask(t, harness, task.taskID)
	if closed.State != "cancelled" {
		t.Fatalf("task state after cancel = %q; want cancelled", closed.State)
	}
	if closed.Question != nil {
		t.Fatalf("cancelled task exposed a question: %+v", closed.Question)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if finished.State != "cancelled" {
		t.Fatalf("predecessor execution state = %q; want cancelled", finished.State)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "cancelled predecessor")
	raceReceiptCount(t, harness, "race-cancel-task-first", 1, "cancel")
	raceReceiptCount(t, harness, "race-cancel-task-first-ask", 0, "loser ask")
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 0)
	requireSubjectEventCount(t, harness, "task.cancelled", task.taskID, 1)
	requireSingleTerminalEvent(t, harness, task.executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, task.executionID)
}

// testAskFirstContested holds the task row, queues the ask winner behind the
// hold, then issues the cancel loser with the same pre-ask lifecycle version.
// Both requests pass their service reads while the ask is queued and
// uncommitted, both queue on the store's scoped task row lock, and the hold is
// released only after two distinct backends are observed. The ask wins and the
// cancel is refused by the store's lifecycle-version guard.
func testAskFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	task := startRaceTask(t, harness, "race-ask-first-contested", "race-ask-first-contested")
	waitWorkloadObservation(t, harness, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, false, "running predecessor before ask")

	hold, err := harness.HoldTaskRow(ctx, task.taskID)
	if err != nil {
		t.Fatalf("hold task row: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "tasks", "ask-first contested")
	before := raceReadTask(t, harness, task.taskID)
	winner := invokeAsync(t, harness, harness.ProjectToken, "task.ask", api.TaskAskRequest{
		TaskID:     task.taskID,
		Question:   "Which artifact should be promoted?",
		Assignment: "role/reviewer",
	}, &before.LifecycleVersion, "race-ask-first-contested")
	winnerBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "tasks", raceContentionTimeout)
	if err != nil {
		t.Fatalf("ask winner did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, winnerBlocked, "tasks", "ask winner")

	// A different principal performs the loser cancel, so its authority
	// membership pins are distinct rows and cannot serialize it ahead of the
	// task row the race targets.
	loser := invokeAsync(t, harness, harness.OperatorToken, "task.cancel", api.TaskCancelRequest{
		TaskID: task.taskID,
	}, &before.LifecycleVersion, "race-ask-first-contested-cancel")
	waiters := awaitContention(t, ctx, harness, hold, "tasks", 2, loser, "cancel loser")
	requireContendingWaiters(t, waiters, hold, winnerBlocked, "tasks", "ask winner", "cancel loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release ask-first contested hold: %v", err)
	}
	winnerOutcome := awaitInvocation(t, winner, 2*time.Minute, "ask winner")
	requireInvocationSuccess(t, winnerOutcome, "ask winner")
	var asked api.TaskAskResult
	harness.Decode(winnerOutcome.result, &asked)
	if asked.LifecycleVersion <= before.LifecycleVersion || asked.QuestionVersion == 0 {
		t.Fatalf("ask result = %+v; want an advanced lifecycle and a question version", asked)
	}
	loserOutcome := awaitInvocation(t, loser, 2*time.Minute, "cancel loser")
	requireStoreVersionConflict(t, loserOutcome, "cancel loser")

	waiting := raceReadTask(t, harness, task.taskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question: %+v", waiting)
	}
	if waiting.Question.Assignment != "role/reviewer" || waiting.Question.Version == 0 {
		t.Fatalf("outstanding question = %+v; want role/reviewer with a version", waiting.Question)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if finished.State != "cancelled" {
		t.Fatalf("predecessor execution state = %q; want cancelled", finished.State)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "cancelled predecessor")
	raceReceiptCount(t, harness, "race-ask-first-contested", 1, "ask")
	raceReceiptCount(t, harness, "race-ask-first-contested-cancel", 0, "loser cancel")
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 1)
	requireSubjectEventCount(t, harness, "task.cancelled", task.taskID, 0)
	requireSingleTerminalEvent(t, harness, task.executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, task.executionID)
}

// testCancelTaskFirstContested holds the task row, queues the cancel winner
// behind the hold, then issues the ask loser with the same pre-cancel lifecycle
// version. Both requests pass their service reads while the cancel is queued
// and uncommitted, both queue on the store's scoped task row lock, and the hold
// is released only after two distinct backends are observed. The cancel wins
// and the ask is refused by the store's lifecycle-version guard.
func testCancelTaskFirstContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	task := startRaceTask(t, harness, "race-cancel-task-first-contested", "race-cancel-task-first-contested")
	waitWorkloadObservation(t, harness, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, false, "running predecessor before cancel")

	hold, err := harness.HoldTaskRow(ctx, task.taskID)
	if err != nil {
		t.Fatalf("hold task row: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "tasks", "cancel-first contested")
	before := raceReadTask(t, harness, task.taskID)
	winner := invokeAsync(t, harness, harness.OperatorToken, "task.cancel", api.TaskCancelRequest{
		TaskID: task.taskID,
	}, &before.LifecycleVersion, "race-cancel-task-first-contested")
	winnerBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "tasks", raceContentionTimeout)
	if err != nil {
		t.Fatalf("cancel winner did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, winnerBlocked, "tasks", "cancel winner")

	loser := invokeAsync(t, harness, harness.ProjectToken, "task.ask", api.TaskAskRequest{
		TaskID:     task.taskID,
		Question:   "Which artifact should be promoted?",
		Assignment: "role/reviewer",
	}, &before.LifecycleVersion, "race-cancel-task-first-contested-ask")
	waiters := awaitContention(t, ctx, harness, hold, "tasks", 2, loser, "ask loser")
	requireContendingWaiters(t, waiters, hold, winnerBlocked, "tasks", "cancel winner", "ask loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release cancel-first contested hold: %v", err)
	}
	winnerOutcome := awaitInvocation(t, winner, 2*time.Minute, "cancel winner")
	requireInvocationSuccess(t, winnerOutcome, "cancel winner")
	var cancelled api.TaskCancelResult
	harness.Decode(winnerOutcome.result, &cancelled)
	if cancelled.LifecycleVersion <= before.LifecycleVersion {
		t.Fatalf("cancel lifecycle version = %d; want > %d", cancelled.LifecycleVersion, before.LifecycleVersion)
	}
	loserOutcome := awaitInvocation(t, loser, 2*time.Minute, "ask loser")
	requireStoreVersionConflict(t, loserOutcome, "ask loser")

	closed := raceReadTask(t, harness, task.taskID)
	if closed.State != "cancelled" {
		t.Fatalf("task state after cancel = %q; want cancelled", closed.State)
	}
	if closed.Question != nil {
		t.Fatalf("cancelled task exposed a question: %+v", closed.Question)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if finished.State != "cancelled" {
		t.Fatalf("predecessor execution state = %q; want cancelled", finished.State)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "cancelled predecessor")
	raceReceiptCount(t, harness, "race-cancel-task-first-contested", 1, "cancel")
	raceReceiptCount(t, harness, "race-cancel-task-first-contested-ask", 0, "loser ask")
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 0)
	requireSubjectEventCount(t, harness, "task.cancelled", task.taskID, 1)
	requireSingleTerminalEvent(t, harness, task.executionID, "execution.cancelled")
	assertExecutionResourcesReleased(t, harness, task.executionID)
}

// requireRevokedExecutionCredential proves the predecessor execution credential
// no longer authenticates after the handoff/cancel revoked it. The token is
// never rendered.
func requireRevokedExecutionCredential(t *testing.T, harness *factory.Harness, token, executionID string) {
	t.Helper()
	if strings.TrimSpace(token) == "" {
		t.Fatalf("execution credential evidence is empty")
	}
	_, publicError, err := harness.Invoke(t.Context(), token, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	if err != nil {
		t.Fatalf("revoked credential probe transport: %v", err)
	}
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("revoked execution credential code = %q; want %q", publicError.Code, api.CodeUnauthenticated)
	}
}

// requireQueuedRowLock proves the barrier observed the product's own scoped row
// lock, not merely any statement that mentions the table. The store locks a
// held row with the tenant/project scope predicates and FOR UPDATE, so the
// observation is the exact serialization statement the race relies on.
func requireQueuedRowLock(t *testing.T, blocked factory.BlockedStatement, table, label string) {
	t.Helper()
	query := strings.ToLower(blocked.Query)
	if blocked.PID == 0 {
		t.Fatalf("%s blocked statement = %+v; want a queued %s row lock", label, blocked, table)
	}
	for _, fragment := range []string{"from " + table, "tenant_id", "project_id", "for update"} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("%s blocked statement query = %q; want a scoped %s row lock containing %q",
				label, blocked.Query, table, fragment)
		}
	}
}

// requireExecutionCredentialMaterial asserts whether the execution row still
// carries credential material. The nonce is never read or rendered: the helper
// only distinguishes the atomic revocation write from the terminal state that
// disables the credential by itself. A "not cleared" call is the positive
// control for the matching "cleared" call.
func requireExecutionCredentialMaterial(t *testing.T, harness *factory.Harness, executionID string, cleared bool, label string) {
	t.Helper()
	got, err := harness.ExecutionCredentialCleared(t.Context(), executionID)
	if err != nil {
		t.Fatalf("read %s credential material: %v", label, err)
	}
	if got == cleared {
		return
	}
	if cleared {
		t.Fatalf("%s still carries credential material; want it cleared in the terminal commit", label)
	}
	t.Fatalf("%s has no credential material; want a live credential before the race", label)
}

// raceInvocation is one public call made from a test goroutine.
type raceInvocation struct {
	result      api.Result
	publicError api.Error
	err         error
}

// invokeAsync starts one public call in a goroutine so the test can hold its
// commit and observe the queued statement. It never calls testing.T.Fatal from
// the goroutine.
func invokeAsync(t *testing.T, harness *factory.Harness, token, operation string, input any, expected *uint64, key string) <-chan raceInvocation {
	t.Helper()
	channel := make(chan raceInvocation, 1)
	go func() {
		result, publicError, err := harness.Invoke(context.Background(), token, operation, input, expected, key)
		channel <- raceInvocation{result: result, publicError: publicError, err: err}
	}()
	return channel
}

// awaitInvocation waits for one queued call to finish.
func awaitInvocation(t *testing.T, channel <-chan raceInvocation, timeout time.Duration, label string) raceInvocation {
	t.Helper()
	select {
	case outcome := <-channel:
		return outcome
	case <-time.After(timeout):
		t.Fatalf("%s did not finish within %s", label, timeout)
		return raceInvocation{}
	}
}

// requireInvocationSuccess fails when a queued call was refused or failed at the
// transport.
func requireInvocationSuccess(t *testing.T, outcome raceInvocation, label string) {
	t.Helper()
	if outcome.err != nil {
		t.Fatalf("%s transport: %v", label, outcome.err)
	}
	if outcome.publicError.Code != "" {
		t.Fatalf("%s refused: %s (%s)", label, outcome.publicError.Code, outcome.publicError.Message)
	}
}

// raceReceiptCount asserts the exact receipt count for one caller key. It is
// file-local because the outage suite already owns requireReceiptCount with a
// different signature; this one also carries a failure label.
func raceReceiptCount(t *testing.T, harness *factory.Harness, key string, want int64, label string) {
	t.Helper()
	count, err := harness.ReceiptCount(t.Context(), key)
	if err != nil {
		t.Fatalf("count %s receipts: %v", label, err)
	}
	if count != want {
		t.Fatalf("%s receipt count = %d; want %d", label, count, want)
	}
}

// requireSubjectEventCount asserts how many committed events of one type name
// one subject.
func requireSubjectEventCount(t *testing.T, harness *factory.Harness, eventType, subject string, want int) {
	t.Helper()
	events := readRequestEvents(t, harness, eventType)
	count := 0
	for _, event := range events.Events {
		if event.Type == eventType && event.SubjectID == subject {
			count++
		}
	}
	if count != want {
		t.Fatalf("committed %s events for subject %s = %d; want %d; events=%+v",
			eventType, subject, count, want, events.Events)
	}
}

// requireSingleTerminalEvent asserts exactly one terminal execution outcome was
// published and that it is want.
func requireSingleTerminalEvent(t *testing.T, harness *factory.Harness, executionID, want string) {
	t.Helper()
	events := readRequestEvents(t, harness, "execution.succeeded", "execution.failed", "execution.cancelled")
	count := 0
	got := ""
	for _, event := range events.Events {
		if event.SubjectID != executionID {
			continue
		}
		switch event.Type {
		case "execution.succeeded", "execution.failed", "execution.cancelled":
			count++
			got = event.Type
		}
	}
	if count != 1 || got != want {
		t.Fatalf("terminal events for execution %s = %d (last %q); want exactly one %s; events=%+v",
			executionID, count, got, want, events.Events)
	}
}
