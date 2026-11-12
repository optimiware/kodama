package enginetest

// Engine-driven coverage of the K-RECOVER window "after settlement / before
// next activation". The reducer settles a confirmed success in one
// commit (the action succeeds, staged outputs are promoted and the current
// index is cleared) and the engine activates the successor in a second,
// separate commit. The window between those two commits therefore exists, and
// the test below crashes the controller exactly inside it.

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

// ---------------------------------------------------------------------------
// Commit log
// ---------------------------------------------------------------------------

// appliedCommit is one commit the state port applied and the execution it
// produced.
type appliedCommit struct {
	commit  flow.Commit
	updated kernel.Execution
}

// commitLog is an engine.State that records every commit the underlying state
// port applied and counts the real version conflicts it refused. A test asserts
// on what became durable (how many settlement, activation or terminal commits
// exist) instead of inferring it from the final state. rebind swaps in the
// fresh State handle of a restarted controller, so one log spans the restart.
type commitLog struct {
	mu        sync.Mutex
	state     engine.State
	applied   []appliedCommit
	conflicts int

	// afterCommit runs on the committing goroutine after each applied commit.
	// It is set before the engine runs.
	afterCommit func(flow.Commit, kernel.Execution)
}

func newCommitLog(state engine.State) *commitLog {
	return &commitLog{state: state}
}

func (l *commitLog) inner() engine.State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *commitLog) rebind(state engine.State) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = state
}

func (l *commitLog) Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	return l.inner().Load(ctx, executionID)
}

func (l *commitLog) Scan(ctx context.Context, request engine.ScanRequest) ([]id.ExecutionID, error) {
	return l.inner().Scan(ctx, request)
}

func (l *commitLog) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	updated, err := l.inner().Commit(ctx, commit)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			l.mu.Lock()
			l.conflicts++
			l.mu.Unlock()
		}
		return updated, err
	}
	l.mu.Lock()
	l.applied = append(l.applied, appliedCommit{commit: commit, updated: updated})
	after := l.afterCommit
	l.mu.Unlock()
	if after != nil {
		after(commit, updated)
	}
	return updated, nil
}

// commits returns the applied commits ordered by the execution version they
// produced, which is the order the store serialized them in.
func (l *commitLog) commits() []appliedCommit {
	l.mu.Lock()
	defer l.mu.Unlock()
	ordered := append([]appliedCommit(nil), l.applied...)
	slices.SortStableFunc(ordered, func(left, right appliedCommit) int {
		switch {
		case left.updated.Version < right.updated.Version:
			return -1
		case left.updated.Version > right.updated.Version:
			return 1
		default:
			return 0
		}
	})
	return ordered
}

func (l *commitLog) count(match func(flow.Commit) bool) int {
	count := 0
	for _, entry := range l.commits() {
		if match(entry.commit) {
			count++
		}
	}
	return count
}

func (l *commitLog) conflictCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conflicts
}

// isActivationCommit reports the commit that makes one pending action the
// running current action: it persists the deadline and the current index.
func isActivationCommit(commit flow.Commit, actionID id.ActionID) bool {
	return commit.ActionID == actionID &&
		commit.NextActionState == kernel.ActionRunning &&
		commit.NextCurrentAction != nil && *commit.NextCurrentAction >= 0 &&
		!commit.Deadline.IsZero()
}

// isSuccessCommit reports the commit that settles one action as succeeded.
func isSuccessCommit(commit flow.Commit, actionID id.ActionID) bool {
	return commit.ActionID == actionID && commit.NextActionState == kernel.ActionSucceeded
}

// ---------------------------------------------------------------------------
// Crash seam and effect counts
// ---------------------------------------------------------------------------

// activationCrashState arms the store's one-shot commit failure at the moment
// the engine submits the first activation commit of one action. The store then
// rejects exactly that commit and applies nothing, which is what a controller
// that dies between the settlement commit and the successor activation leaves
// behind. atCrash observes the durable state at that boundary.
type activationCrashState struct {
	engine.State

	store    *testkit.Store
	actionID id.ActionID
	failure  error
	atCrash  func()

	mu      sync.Mutex
	crashes int
}

func (s *activationCrashState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if isActivationCommit(commit, s.actionID) {
		s.mu.Lock()
		first := s.crashes == 0
		s.crashes++
		s.mu.Unlock()
		if first {
			if s.atCrash != nil {
				s.atCrash()
			}
			s.store.FailNextCommit(s.failure)
		}
	}
	return s.State.Commit(ctx, commit)
}

func (s *activationCrashState) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crashes
}

// effectCounts is how often the engine issued each runtime effect for one
// workload identity.
type effectCounts struct {
	prepare, start, observe, stop, remove int
}

func (r *recordingRuntime) effectsFor(workloadID id.WorkloadID) effectCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	var counts effectCounts
	for _, request := range r.prepares {
		if request.Identity.WorkloadID == workloadID {
			counts.prepare++
		}
	}
	for _, request := range r.starts {
		if request.Identity.WorkloadID == workloadID {
			counts.start++
		}
	}
	for _, request := range r.observes {
		if request.Identity.WorkloadID == workloadID {
			counts.observe++
		}
	}
	for _, request := range r.stops {
		if request.Identity.WorkloadID == workloadID {
			counts.stop++
		}
	}
	for _, request := range r.removes {
		if request.Identity.WorkloadID == workloadID {
			counts.remove++
		}
	}
	return counts
}

// restartRecorded restarts the controller and keeps the commit log across the
// restart: the fresh engine runs over the fresh State handle.
func restartRecorded(h *harness, log *commitLog) {
	h.t.Helper()
	h.restart()
	log.rebind(h.fixture.State)
	h.engine = engine.New(log, h.runtime, h.fixture.Workspace, h.fixture.Clock)
}

// ---------------------------------------------------------------------------
// K-RECOVER: after settlement / before next activation, engine-driven
// ---------------------------------------------------------------------------

// TestEngineDrivenCrashAfterSettlementBeforeNextActivation pins the fourth
// K-RECOVER window without seeding it. The engine starts a container, observes
// its confirmed exit and commits the settlement; the controller then dies at
// the successor's activation commit, which is a separate commit. The settlement
// is durable and unchanged, the outputs are promoted exactly once, a restarted
// controller activates the successor exactly once from persisted state, the
// settled workload is never prepared, started, observed or stopped again, the
// workspace stays retained and at most one action is ever active.
func TestEngineDrivenCrashAfterSettlementBeforeNextActivation(t *testing.T) {
	successors := []struct {
		name      string
		suffix    string
		successor kernel.Action
	}{
		{"request successor", "request", requestAction("act-next", "next", 1, time.Hour, definitionSubjectForBuild(), "decision")},
		{"container successor", "container", containerAction("act-next", "next", 1, time.Hour)},
	}
	for _, tc := range successors {
		t.Run(tc.name, func(t *testing.T) {
			build := containerAction("act-build", "build", 0, time.Hour, "candidate")
			h := newHarness(t, newExecution("exec-settle-crash-"+tc.suffix, "ws-settle-crash-"+tc.suffix, build, tc.successor))

			injected := errors.New("injected crash at the successor activation commit")
			crash := &activationCrashState{
				State:    h.fixture.State,
				store:    h.fixture.Store,
				actionID: "act-next",
				failure:  injected,
			}
			log := newCommitLog(crash)
			log.afterCommit = func(_ flow.Commit, updated kernel.Execution) {
				assertSingleActiveAction(t, updated)
			}
			h.engine = engine.New(log, h.runtime, h.fixture.Workspace, h.fixture.Clock)

			// A real engine start, a staged output and a confirmed exit.
			running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
				action, ok := actionByName(candidate, "build")
				return ok && action.State == kernel.ActionRunning
			})
			buildIdentity := identityFor(h.executionID, mustAction(t, running, "build"))
			buildWorkload := buildIdentity.WorkloadID
			assertWorkloadStarted(t, h.fixture.Runtime, buildWorkload)
			if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
				t.Fatalf("SubmitOutputs(build) error = %v", err)
			}
			h.confirmRuntimeStop(buildIdentity)
			h.fixture.Runtime.QueueObservation(buildWorkload, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))

			// The controller dies between the settlement commit and the
			// successor activation commit.
			var atBoundary kernel.Execution
			crash.atCrash = func() { atBoundary = h.load() }
			if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); !errors.Is(err, injected) {
				t.Fatalf("Reconcile error = %v, want the injected activation-commit crash", err)
			}
			if crash.attempts() != 1 {
				t.Fatalf("activation commit attempts before the restart = %d, want 1", crash.attempts())
			}

			crashed := h.load()
			if !reflect.DeepEqual(crashed, atBoundary) {
				t.Fatalf("failed activation commit changed durable state:\n got %+v\nwant %+v", crashed, atBoundary)
			}
			if crashed.State != kernel.ExecutionRunning || crashed.CurrentAction != -1 {
				t.Fatalf("state in the crash window = %s", actionSummary(crashed))
			}
			settled := mustAction(t, crashed, "build")
			if settled.State != kernel.ActionSucceeded {
				t.Fatalf("build state in the crash window = %q, want succeeded", settled.State)
			}
			if len(settled.Outputs) != 1 || settled.Outputs["candidate"] != "abc" {
				t.Fatalf("settled outputs = %+v, want candidate=abc", settled.Outputs)
			}
			if settled.StagedOutputsSet || settled.StagedOutputs != nil {
				t.Fatalf("settlement kept staging: set=%v values=%+v", settled.StagedOutputsSet, settled.StagedOutputs)
			}
			if !settled.Stop.IsConfirmed() {
				t.Fatalf("settled stop = %+v, want confirmed", settled.Stop)
			}
			pending := mustAction(t, crashed, "next")
			if pending.State != kernel.ActionPending || pending.Version != 0 || !pending.Deadline.IsZero() || !pending.ActivatedAt.IsZero() {
				t.Fatalf("successor in the crash window = %+v, want untouched and pending", pending)
			}
			if pending.Container != nil && (pending.Container.WorkloadID != "" || pending.Container.Launch != (kernel.LaunchIntent{})) {
				t.Fatalf("crashed activation left a launch intent: %+v", pending.Container)
			}
			if pending.Request != nil && pending.Request.Subject != nil {
				t.Fatalf("crashed activation left an answerable subject: %+v", pending.Request.Subject.Fields())
			}
			if got := log.count(func(commit flow.Commit) bool { return isSuccessCommit(commit, "act-build") }); got != 1 {
				t.Fatalf("settlement commits = %d, want 1", got)
			}
			if got := log.count(func(commit flow.Commit) bool { return isActivationCommit(commit, "act-next") }); got != 0 {
				t.Fatalf("applied successor activations before the restart = %d, want 0", got)
			}
			buildEffects := h.runtime.effectsFor(buildWorkload)
			if buildEffects.prepare != 1 || buildEffects.start != 1 || buildEffects.stop != 0 || buildEffects.remove != 0 {
				t.Fatalf("build effects in the crash window = %+v, want one prepare and one start only", buildEffects)
			}
			if h.runtime.prepareCount() != 1 || h.runtime.startCount() != 1 {
				t.Fatalf("crashed activation dispatched work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
			}
			if !h.fixture.Workspace.IsRetained(h.workspaceID) {
				t.Fatal("crash window lost workspace retention")
			}
			unfinished, err := h.fixture.State.Scan(h.t.Context(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: h.fixture.Clock.Now()})
			if err != nil {
				t.Fatalf("Scan(unfinished) error = %v", err)
			}
			if !slices.Contains(unfinished, h.executionID) {
				t.Fatalf("unfinished scan = %v, want the stranded execution %q", unfinished, h.executionID)
			}

			// A fresh controller recovers the successor from persisted state.
			restartRecorded(h, log)
			if err := h.engine.Scan(h.t.Context()); err != nil {
				t.Fatalf("Scan() after restart error = %v", err)
			}
			resumed := h.load()
			assertSingleActiveAction(t, resumed)
			next := mustAction(t, resumed, "next")
			if next.State != kernel.ActionRunning || resumed.CurrentAction != 1 {
				t.Fatalf("successor after restart = %s, want running", actionSummary(resumed))
			}
			switch next.Kind {
			case definition.ActionKindRequest:
				if next.Request == nil || next.Request.Subject == nil {
					t.Fatal("resumed request has no resolved subject")
				}
				if got := next.Request.Subject.Fields()["candidate"].Literal; got != "abc" {
					t.Fatalf("resumed subject candidate = %q, want the promoted abc", got)
				}
				if h.runtime.prepareCount() != 1 || h.runtime.startCount() != 1 {
					t.Fatalf("resumed request dispatched a workload: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
				}
			case definition.ActionKindContainer:
				nextWorkload := next.Container.WorkloadID
				if nextWorkload == "" || nextWorkload == buildWorkload {
					t.Fatalf("successor workload = %q, want a fresh identity (build is %q)", nextWorkload, buildWorkload)
				}
				if effects := h.runtime.effectsFor(nextWorkload); effects.prepare != 1 || effects.start != 1 {
					t.Fatalf("successor effects = %+v, want exactly one prepare and one start", effects)
				}
				assertWorkloadStarted(t, h.fixture.Runtime, nextWorkload)
				prepare := h.runtime.lastPrepare()
				if prepare.WorkspaceID != h.workspaceID || prepare.Outputs["build"]["candidate"] != "abc" {
					t.Fatalf("successor prepare = workspace %q outputs %+v, want the retained workspace and candidate=abc",
						prepare.WorkspaceID, prepare.Outputs)
				}
			}

			// Further passes neither re-activate the successor nor touch the
			// settled action.
			commits := h.fixture.Store.CommitCount()
			for range 3 {
				assertSingleActiveAction(t, h.reconcile())
			}
			if err := h.engine.Scan(h.t.Context()); err != nil {
				t.Fatalf("repeated Scan() error = %v", err)
			}
			if got := h.fixture.Store.CommitCount(); got != commits {
				t.Fatalf("repeated passes committed %d more times, want none", got-commits)
			}
			assertSettledOnce := func(stage string, execution kernel.Execution) {
				t.Helper()
				if got := mustAction(t, execution, "build"); !reflect.DeepEqual(got, settled) {
					t.Fatalf("%s changed the settled build action:\n got %+v\nwant %+v", stage, got, settled)
				}
				if got := log.count(func(commit flow.Commit) bool { return isSuccessCommit(commit, "act-build") }); got != 1 {
					t.Fatalf("%s: settlement commits = %d, want 1", stage, got)
				}
				if got := log.count(func(commit flow.Commit) bool {
					return commit.ActionID == "act-build" && commit.Output.Action == flow.OutputPromote
				}); got != 1 {
					t.Fatalf("%s: build output promotions = %d, want exactly 1", stage, got)
				}
				if got := log.count(func(commit flow.Commit) bool { return isActivationCommit(commit, "act-next") }); got != 1 {
					t.Fatalf("%s: successor activations = %d, want exactly 1", stage, got)
				}
				effects := h.runtime.effectsFor(buildWorkload)
				if effects.prepare != 1 || effects.start != 1 || effects.stop != 0 || effects.observe != buildEffects.observe {
					t.Fatalf("%s: settled build workload effects = %+v, want no prepare/start/observe/stop after settlement (%+v)",
						stage, effects, buildEffects)
				}
				workload, ok := h.fixture.Runtime.Workload(buildWorkload)
				if !ok || workload.StartCount != 1 {
					t.Fatalf("%s: settled build workload = %+v, want exactly one start", stage, workload)
				}
			}
			assertSettledOnce("recovery", h.load())
			if h.runtime.effectsFor(buildWorkload).remove != 0 {
				t.Fatal("recovery removed the settled workload before the execution was terminal")
			}
			if !h.fixture.Workspace.IsRetained(h.workspaceID) || h.fixture.Workspace.RetainCount() != 1 || h.fixture.Workspace.ReleaseCount() != 0 {
				t.Fatalf("workspace after recovery: retained=%v retains=%d releases=%d, want retained once and never released",
					h.fixture.Workspace.IsRetained(h.workspaceID), h.fixture.Workspace.RetainCount(), h.fixture.Workspace.ReleaseCount())
			}

			// The recovered execution still completes and cleans up once.
			switch next.Kind {
			case definition.ActionKindRequest:
				answer := kernel.Answer{
					Decision: kernel.DecisionPass,
					Outputs:  kernel.Outputs{"decision": "approved"},
					Actor:    id.PrincipalID("principal-1"),
					At:       h.fixture.Clock.Now(),
				}
				if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-next", answer); err != nil {
					t.Fatalf("SettleRequest(pass) error = %v", err)
				}
			case definition.ActionKindContainer:
				h.confirmRuntimeStop(identityFor(h.executionID, next))
				h.fixture.Runtime.QueueObservation(next.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("next exited")))
			}
			final := h.reconcileUntil("terminal success and cleanup complete", 8, func(candidate kernel.Execution) bool {
				return candidate.State == kernel.ExecutionSucceeded && candidate.Cleanup.State == kernel.CleanupComplete
			})
			assertNoPendingActions(t, final)
			assertSettledOnce("completion", final)
			if got := h.runtime.effectsFor(buildWorkload).remove; got != 1 {
				t.Fatalf("settled workload removals = %d, want exactly one at terminal cleanup", got)
			}
			if !h.fixture.Workspace.IsReleased(h.workspaceID) || h.fixture.Workspace.ReleaseCount() != 1 {
				t.Fatalf("workspace releases = %d, want exactly one after terminal cleanup", h.fixture.Workspace.ReleaseCount())
			}
		})
	}
}
