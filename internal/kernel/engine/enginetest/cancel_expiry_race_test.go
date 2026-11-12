package enginetest

// Engine-level cancel-versus-expiry schedules. The reducer pair is
// pinned by flow's TestTerminalRacesPreserveTheFirstWinner and the "Cancel
// wins while Reconcile's expiry commit is held" order by
// TestReconcileVersionConflictRereadsAndPreservesWinner. This file adds the
// opposite deterministic order through the engine's own Cancel, and schedules
// in which Cancel and Reconcile really run concurrently.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// raceKind is one shape of the cancel-versus-expiry subject: the action that
// is active when the deadline is reached, followed by a pending successor of
// the other kind.
type raceKind struct {
	name   string
	suffix string
	active definition.ActionKind
}

func raceKinds() []raceKind {
	return []raceKind{
		{"running container", "container", definition.ActionKindContainer},
		{"waiting request", "request", definition.ActionKindRequest},
	}
}

// newRaceHarness drives a fresh execution through a real engine activation of
// its first action and then moves the authoritative clock onto that action's
// persisted deadline, so the next Reconcile expires it. The same identifiers
// are used for every harness of one kind, which makes final states comparable
// across schedules. The returned workload is empty for a waiting request.
func newRaceHarness(t *testing.T, kind raceKind) (*harness, id.WorkloadID) {
	t.Helper()
	subject := literalSubject(map[string]string{"operation": "review"})
	var actions []kernel.Action
	switch kind.active {
	case definition.ActionKindContainer:
		actions = []kernel.Action{
			containerAction("act-first", "first", 0, time.Hour),
			requestAction("act-second", "second", 1, time.Hour, subject, "decision"),
		}
	default:
		actions = []kernel.Action{
			requestAction("act-first", "first", 0, time.Hour, subject, "decision"),
			containerAction("act-second", "second", 1, time.Hour),
		}
	}
	h := newHarness(t, newExecution("exec-cancel-expiry-"+kind.suffix, "ws-cancel-expiry-"+kind.suffix, actions...))
	running := h.reconcileUntil("first action running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "first")
		return ok && action.State == kernel.ActionRunning
	})
	first := mustAction(t, running, "first")
	var workload id.WorkloadID
	if first.Container != nil {
		workload = first.Container.WorkloadID
		assertWorkloadStarted(t, h.fixture.Runtime, workload)
	}
	h.fixture.Clock.Set(first.Deadline)
	return h, workload
}

func isCancelCommit(commit flow.Commit) bool {
	return commit.NextExecutionState == kernel.ExecutionCancelled
}

func isExpiryCommit(commit flow.Commit) bool {
	return commit.NextExecutionState == kernel.ExecutionFailed &&
		commit.Failure != nil && commit.Failure.Kind == kernel.FailureTimeout
}

// assertOneTerminalCommit walks the applied commits in store order and fails
// unless exactly one of them moved the execution into a terminal state and no
// later commit changed that state or its failure.
func assertOneTerminalCommit(t *testing.T, log *commitLog) {
	t.Helper()
	transitions := 0
	var terminal *kernel.Execution
	for _, entry := range log.commits() {
		if terminal == nil {
			if entry.updated.State.Terminal() {
				transitions++
				settled := entry.updated
				terminal = &settled
			}
			continue
		}
		if entry.updated.State != terminal.State || !reflect.DeepEqual(entry.updated.Failure, terminal.Failure) {
			t.Fatalf("commit to version %d rewrote the terminal outcome %q/%+v as %q/%+v",
				entry.updated.Version, terminal.State, terminal.Failure, entry.updated.State, entry.updated.Failure)
		}
	}
	if transitions != 1 {
		t.Fatalf("terminal commits = %d, want exactly one", transitions)
	}
}

// assertObligationsIssuedOnce fails unless the terminal transition recorded
// exactly one cleanup intent and, for a workload, exactly one stop intent.
func assertObligationsIssuedOnce(t *testing.T, log *commitLog, workload id.WorkloadID) {
	t.Helper()
	wantStops := 0
	if workload != "" {
		wantStops = 1
	}
	stops := log.count(func(commit flow.Commit) bool { return commit.Stop != nil })
	cleanups := log.count(func(commit flow.Commit) bool { return commit.Cleanup != nil })
	if stops != wantStops || cleanups != 1 {
		t.Fatalf("committed stop/cleanup intents = %d/%d, want %d/1", stops, cleanups, wantStops)
	}
	for _, entry := range log.commits() {
		if entry.commit.Stop != nil && entry.commit.Stop.WorkloadID != workload {
			t.Fatalf("stop intent names workload %q, want %q", entry.commit.Stop.WorkloadID, workload)
		}
	}
}

// ---------------------------------------------------------------------------
// Expiry wins while Cancel is held at its commit boundary
// ---------------------------------------------------------------------------

// TestCancelHeldAtCommitBoundaryLosesToExpiry pins S02 §3 for the opposite
// order: the engine's Cancel has loaded the running execution
// and is held at its commit boundary when Reconcile commits the deadline
// expiry. The stale cancellation is refused with a real version_conflict, a
// re-read Cancel is refused as a conflicting settlement, the timeout outcome is
// never overwritten and the stop/cleanup obligations are recorded once.
func TestCancelHeldAtCommitBoundaryLosesToExpiry(t *testing.T) {
	for _, kind := range raceKinds() {
		t.Run(kind.name, func(t *testing.T) {
			h, workload := newRaceHarness(t, kind)
			log := newCommitLog(h.fixture.State)
			gate := newGatedState(log)
			gate.gate = isCancelCommit
			h.engine = engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

			cancelled := make(chan error, 1)
			go func() {
				cancelled <- h.engine.Cancel(h.t.Context(), h.executionID, flow.CancelRequested)
			}()
			waitGate(t, gate, h.t.Context())

			// The expiry commits between Cancel's load and its commit.
			expired := h.reconcile()
			if expired.State != kernel.ExecutionFailed || expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
				t.Fatalf("outcome while Cancel is held = %q/%+v, want failed/timeout", expired.State, expired.Failure)
			}
			commits := h.fixture.Store.CommitCount()

			gate.releaseGate()
			mustClass(t, <-cancelled, fault.VersionConflict)
			if log.conflictCount() != 1 {
				t.Fatalf("version conflicts = %d, want exactly the refused cancellation", log.conflictCount())
			}
			// The caller re-reads: the cancellation now conflicts with settled
			// history and is refused without a commit.
			mustClass(t, h.engine.Cancel(h.t.Context(), h.executionID, flow.CancelRequested), fault.ChangedIdempotencyContent)

			final := h.load()
			if !reflect.DeepEqual(final, expired) {
				t.Fatalf("losing cancellation changed the expiry outcome:\n got %+v\nwant %+v", final, expired)
			}
			if got := h.fixture.Store.CommitCount(); got != commits {
				t.Fatalf("losing cancellation committed %d times, want none", got-commits)
			}
			if final.Failure.Reason != "deadline_reached" {
				t.Fatalf("failure reason = %q, want deadline_reached", final.Failure.Reason)
			}
			if got := mustAction(t, final, "first").State; got != kernel.ActionFailed {
				t.Fatalf("expired action state = %q, want failed", got)
			}
			if got := mustAction(t, final, "second").State; got != kernel.ActionCancelled {
				t.Fatalf("pending successor state = %q, want cancelled", got)
			}
			assertNoPendingActions(t, final)
			if !h.fixture.Store.CredentialInvalidated(h.executionID) {
				t.Fatal("expiry did not revoke execution authority")
			}
			assertOneTerminalCommit(t, log)
			assertObligationsIssuedOnce(t, log, workload)
			if got := log.count(isCancelCommit); got != 0 {
				t.Fatalf("applied cancellation commits = %d, want none", got)
			}

			if workload == "" {
				if h.runtime.stopCount() != 0 || final.Cleanup.State != kernel.CleanupComplete {
					t.Fatalf("request expiry: stops=%d cleanup=%q, want no stop and complete cleanup",
						h.runtime.stopCount(), final.Cleanup.State)
				}
				return
			}
			stops := h.fixture.Store.PendingStops()
			if len(stops) != 1 || stops[0].WorkloadID != workload {
				t.Fatalf("pending stop intents = %+v, want exactly one for %q", stops, workload)
			}
			if final.Cleanup.State != kernel.CleanupPending || h.fixture.Workspace.IsReleased(h.workspaceID) {
				t.Fatalf("cleanup before a confirmed stop = %q released=%v, want pending and retained",
					final.Cleanup.State, h.fixture.Workspace.IsReleased(h.workspaceID))
			}
			h.confirmRuntimeStop(identityFor(h.executionID, mustAction(t, final, "first")))
			completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
				return candidate.Cleanup.State == kernel.CleanupComplete
			})
			if completed.State != kernel.ExecutionFailed || !reflect.DeepEqual(completed.Failure, expired.Failure) {
				t.Fatalf("cleanup changed the expiry outcome to %q/%+v", completed.State, completed.Failure)
			}
			if !h.fixture.Workspace.IsReleased(h.workspaceID) || h.runtime.effectsFor(workload).remove != 1 {
				t.Fatalf("cleanup after the confirmed stop: released=%v removals=%d, want released and one removal",
					h.fixture.Workspace.IsReleased(h.workspaceID), h.runtime.effectsFor(workload).remove)
			}
			assertOneTerminalCommit(t, log)
			assertObligationsIssuedOnce(t, log, workload)
		})
	}
}

// ---------------------------------------------------------------------------
// Truly concurrent Cancel and Reconcile
// ---------------------------------------------------------------------------

// commitRendezvous holds the first cancellation commit and the first expiry
// commit at the commit boundary. ready closes once both have arrived; release
// then lets them reach the store together. Both transitions were reduced from
// the same loaded revision, so the store decides the winner and the loser
// always sees a real version_conflict. cancelFirst only picks which held commit
// is woken first, so repeated rounds do not favour one winner.
type commitRendezvous struct {
	engine.State

	mu      sync.Mutex
	arrived int
	ready   chan struct{}
	cancel  chan struct{}
	expiry  chan struct{}
}

func newCommitRendezvous(state engine.State) *commitRendezvous {
	return &commitRendezvous{
		State:  state,
		ready:  make(chan struct{}),
		cancel: make(chan struct{}),
		expiry: make(chan struct{}),
	}
}

// hold returns the channel a commit must wait on, or nil when it is not the
// first cancellation or expiry commit.
func (s *commitRendezvous) hold(commit flow.Commit) <-chan struct{} {
	var held chan struct{}
	switch {
	case isCancelCommit(commit):
		held = s.cancel
	case isExpiryCommit(commit):
		held = s.expiry
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-held:
		return nil
	default:
	}
	s.arrived++
	if s.arrived == 2 {
		close(s.ready)
	}
	return held
}

func (s *commitRendezvous) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if held := s.hold(commit); held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return kernel.Execution{}, ctx.Err()
		}
	}
	return s.State.Commit(ctx, commit)
}

func (s *commitRendezvous) release(cancelFirst bool) {
	if cancelFirst {
		close(s.cancel)
		close(s.expiry)
		return
	}
	close(s.expiry)
	close(s.cancel)
}

// sequentialRaceOutcome returns the durable state one sequential order of
// Cancel and Reconcile produces. The concurrent schedules must be
// indistinguishable from one of these two.
func sequentialRaceOutcome(t *testing.T, kind raceKind, cancelFirst bool) kernel.Execution {
	t.Helper()
	h, _ := newRaceHarness(t, kind)
	if cancelFirst {
		h.cancel(flow.CancelRequested)
		h.reconcile()
	} else {
		h.reconcile()
		mustClass(t, h.engine.Cancel(h.t.Context(), h.executionID, flow.CancelRequested), fault.ChangedIdempotencyContent)
	}
	return h.load()
}

// TestConcurrentCancelAndExpiryConvergeOnOneTerminalWinner pins S02 §3 under
// real concurrency: Cancel and an expiring Reconcile run on two goroutines
// released together, once free-running and once held until both sit at the
// commit boundary. Whichever wins, there is exactly one terminal commit, the
// loser reports the conflict or converges, the stop/cleanup obligations are
// recorded once, and the durable state equals the state of the corresponding
// sequential order and is unchanged by a restart and a further pass.
func TestConcurrentCancelAndExpiryConvergeOnOneTerminalWinner(t *testing.T) {
	const rounds = 16
	schedules := []struct {
		name       string
		atBoundary bool
	}{
		{"released together", false},
		{"released together at the commit boundary", true},
	}
	for _, kind := range raceKinds() {
		t.Run(kind.name, func(t *testing.T) {
			wantCancel := sequentialRaceOutcome(t, kind, true)
			wantExpiry := sequentialRaceOutcome(t, kind, false)
			if wantCancel.State != kernel.ExecutionCancelled || wantExpiry.State != kernel.ExecutionFailed {
				t.Fatalf("sequential outcomes = %q/%q, want cancelled/failed", wantCancel.State, wantExpiry.State)
			}
			for _, schedule := range schedules {
				t.Run(schedule.name, func(t *testing.T) {
					for round := range rounds {
						h, workload := newRaceHarness(t, kind)
						log := newCommitLog(h.fixture.State)
						var state engine.State = log
						var rendezvous *commitRendezvous
						if schedule.atBoundary {
							rendezvous = newCommitRendezvous(log)
							state = rendezvous
						}
						h.engine = engine.New(state, h.runtime, h.fixture.Workspace, h.fixture.Clock)

						ctx, stop := context.WithTimeout(h.t.Context(), 10*time.Second)
						start := testkit.NewBarrier()
						var wg sync.WaitGroup
						var cancelErr, reconcileErr error
						wg.Add(2)
						go func() {
							defer wg.Done()
							if cancelErr = start.Wait(ctx); cancelErr == nil {
								cancelErr = h.engine.Cancel(ctx, h.executionID, flow.CancelRequested)
							}
						}()
						go func() {
							defer wg.Done()
							if reconcileErr = start.Wait(ctx); reconcileErr == nil {
								_, reconcileErr = h.engine.Reconcile(ctx, h.executionID)
							}
						}()
						start.Release()
						if rendezvous != nil {
							// Both commits are held at the boundary before
							// either may reach the store.
							select {
							case <-rendezvous.ready:
								rendezvous.release(round%2 == 0)
							case <-ctx.Done():
							}
						}
						wg.Wait()
						stop()

						// Reconcile re-reads a conflict; Cancel reports one.
						if reconcileErr != nil {
							t.Fatalf("round %d: Reconcile error = %v", round, reconcileErr)
						}
						want := wantCancel
						if cancelErr != nil {
							want = wantExpiry
							class, ok := fault.ClassOf(cancelErr)
							refused := ok && class == fault.ChangedIdempotencyContent && !schedule.atBoundary
							if !ok || (class != fault.VersionConflict && !refused) {
								t.Fatalf("round %d: losing Cancel error = %v (class %q, %v), want a conflict", round, cancelErr, class, ok)
							}
						}
						final := h.load()
						if !reflect.DeepEqual(final, want) {
							t.Fatalf("round %d: Cancel error %v but durable state is not the %q outcome:\n got %+v\nwant %+v",
								round, cancelErr, want.State, final, want)
						}
						if schedule.atBoundary && log.conflictCount() != 1 {
							t.Fatalf("round %d: version conflicts = %d, want exactly one loser", round, log.conflictCount())
						}
						assertOneTerminalCommit(t, log)
						assertObligationsIssuedOnce(t, log, workload)
						if got := h.runtime.stopCount(); (workload == "" && got != 0) || (workload != "" && got != 1) {
							t.Fatalf("round %d: Stop requests = %d for workload %q", round, got, workload)
						}

						// A restarted controller and a repeated pass re-read the
						// same outcome.
						commits := h.fixture.Store.CommitCount()
						h.restart()
						h.reconcile()
						if reread := h.load(); !reflect.DeepEqual(reread, final) {
							t.Fatalf("round %d: re-read after restart changed the outcome:\n got %+v\nwant %+v", round, reread, final)
						}
						if got := h.fixture.Store.CommitCount(); got != commits {
							t.Fatalf("round %d: re-read committed %d times, want none", round, got-commits)
						}
					}
				})
			}
		})
	}
}
