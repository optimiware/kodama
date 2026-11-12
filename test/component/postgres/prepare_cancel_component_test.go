//go:build component

package postgrescomponent

// The Prepare-cancellation never-started witness must be emitted by the
// real Kernel engine and persisted by the real execution store, not hand-built
// into a golden commit. The runtime below deliberately reports absence and an
// unconfirmed Stop for the prepared identity, so the only way cleanup can finish
// is the engine's own durable NeverStarted proof. A fresh controller/store
// reader then converges from that persisted row alone.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// prepareCancellationRuntime is a barrier-controlled Kernel runtime. Prepare
// materializes nothing durable and blocks until the test has committed the
// cancellation. Start records abuse but never runs. Observe reports absence and
// Stop acknowledges without termination evidence: neither is nonexecution or
// stop proof.
type prepareCancellationRuntime struct {
	mu sync.Mutex

	target     id.WorkloadID
	preparedAt chan struct{}
	resume     chan struct{}
	prepared   sync.Once

	prepares int
	starts   int
	observes int
	stops    int
	removes  int
}

func newPrepareCancellationRuntime() *prepareCancellationRuntime {
	return &prepareCancellationRuntime{
		preparedAt: make(chan struct{}),
		resume:     make(chan struct{}),
	}
}

func (r *prepareCancellationRuntime) Capabilities(context.Context) (kernelengine.Capabilities, error) {
	return kernelengine.Capabilities{}, nil
}

func (r *prepareCancellationRuntime) Prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	r.mu.Lock()
	r.prepares++
	r.target = request.Identity.WorkloadID
	r.mu.Unlock()
	r.prepared.Do(func() { close(r.preparedAt) })
	select {
	case <-r.resume:
	case <-ctx.Done():
		return kernelengine.Preparation{}, ctx.Err()
	}
	return kernelengine.Preparation{WorkspaceID: request.WorkspaceID, Detail: "materialized"}, nil
}

func (r *prepareCancellationRuntime) Start(_ context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return kernelengine.StartResult{Outcome: kernelengine.StartAccepted, WorkloadID: request.Identity.WorkloadID}, nil
}

func (r *prepareCancellationRuntime) Observe(context.Context, kernelengine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	r.observes++
	r.mu.Unlock()
	return kernel.AbsentObservation(), nil
}

func (r *prepareCancellationRuntime) Logs(context.Context, kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return kernelengine.LogBatch{}, nil
}

func (r *prepareCancellationRuntime) Stop(context.Context, kernelengine.StopRequest) (kernel.StopEvidence, error) {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	return kernel.PendingStop("stop unconfirmed: no workload pods observed"), nil
}

func (r *prepareCancellationRuntime) Remove(_ context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	r.mu.Lock()
	r.removes++
	r.mu.Unlock()
	return kernelengine.Removal{Removed: true, Detail: "removed partial preparation resources"}, nil
}

func (r *prepareCancellationRuntime) RefreshCredential(context.Context, kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

func (r *prepareCancellationRuntime) releasePrepare() { close(r.resume) }

func (r *prepareCancellationRuntime) counts() (prepare, start, observe, stop, remove int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepares, r.starts, r.observes, r.stops, r.removes
}

// prepareCancellationWorkspace tracks retention and release without any host
// filesystem.
type prepareCancellationWorkspace struct {
	mu       sync.Mutex
	released map[id.WorkspaceID]bool
}

func newPrepareCancellationWorkspace() *prepareCancellationWorkspace {
	return &prepareCancellationWorkspace{released: make(map[id.WorkspaceID]bool)}
}

func (w *prepareCancellationWorkspace) Status(context.Context, id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	return kernelengine.WorkspaceObservation{Available: true}, nil
}

func (w *prepareCancellationWorkspace) Retain(context.Context, id.WorkspaceID) error { return nil }

func (w *prepareCancellationWorkspace) Release(_ context.Context, workspaceID id.WorkspaceID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.released[workspaceID] = true
	return nil
}

func (w *prepareCancellationWorkspace) isReleased(workspaceID id.WorkspaceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.released[workspaceID]
}

type prepareCancellationClock struct{ at time.Time }

func (clock prepareCancellationClock) Now() time.Time { return clock.at }

// prepareWitnessCrashState applies a commit durably and then reports a
// storage-style failure for the witness commit, so the test can construct a
// fresh engine over the exact persisted row.
type prepareWitnessCrashState struct {
	kernelengine.State
	mu      sync.Mutex
	armed   bool
	crashed bool
	err     error
}

func (s *prepareWitnessCrashState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	updated, err := s.State.Commit(ctx, commit)
	if err != nil {
		return updated, err
	}
	witness := commit.Launch == nil && commit.Failure == nil &&
		commit.Observation != nil &&
		commit.Observation.Kind == kernel.ObservationNeverStarted &&
		commit.Observation.Proof != ""
	s.mu.Lock()
	armed := s.armed && witness
	if armed {
		s.armed = false
		s.crashed = true
	}
	s.mu.Unlock()
	if armed {
		return updated, s.err
	}
	return updated, nil
}

func (s *prepareWitnessCrashState) didCrash() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crashed
}

// TestPrepareCancellationProofPersistsOnPostgreSQL drives the real engine over
// the real controller store. The never-started witness is emitted by the exact
// Prepare invocation, persisted as a version-guarded row update, and a fresh
// engine/store reader completes cleanup from it while the runtime keeps
// reporting absence and an unconfirmed stop.
func TestPrepareCancellationProofPersistsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	executionID := startedExecution(t, env)
	workspaceID := snapshotdoc.DeriveWorkspaceID(executionID)
	runtime := newPrepareCancellationRuntime()
	workspace := newPrepareCancellationWorkspace()
	clock := prepareCancellationClock{at: pgtest.FixtureTime}

	state := executionstore.NewEngine(adapterController(t, env.shard))
	crashing := &prepareWitnessCrashState{
		State: state,
		armed: true,
		err:   errors.New("injected controller crash after the witness commit"),
	}
	reconciler := kernelengine.New(crashing, runtime, workspace, clock)

	done := make(chan error, 1)
	go func() {
		_, err := reconciler.Reconcile(ctx, executionID)
		done <- err
	}()
	waitPrepareReached(t, runtime, ctx)
	if err := reconciler.Cancel(ctx, executionID, flow.CancelRequested); err != nil {
		t.Fatalf("Cancel: %v%s", err, pgtest.CauseText(err))
	}
	runtime.releasePrepare()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "injected controller crash after the witness commit") {
		t.Fatalf("Reconcile error = %v, want the injected post-commit crash", err)
	}
	if !crashing.didCrash() {
		t.Fatal("the engine never emitted the never-started witness")
	}

	// A fresh store reader sees the proof and the preserved cancellation.
	reader := executionstore.NewEngine(adapterController(t, env.shard))
	persisted, err := reader.Load(ctx, executionID)
	if err != nil {
		t.Fatalf("fresh Load: %v%s", err, pgtest.CauseText(err))
	}
	if persisted.State != kernel.ExecutionCancelled {
		t.Fatalf("persisted state = %q, want cancelled", persisted.State)
	}
	if persisted.Failure != nil {
		t.Fatalf("persisted cancellation gained a failure: %+v", persisted.Failure)
	}
	action := persisted.Actions[0]
	if action.State != kernel.ActionCancelled {
		t.Fatalf("persisted action state = %q, want cancelled", action.State)
	}
	if action.Container == nil || action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("persisted launch = %+v, want the issued attempt preserved", action.Container)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("persisted observation = %+v, want never-started proof", action.Container.Observation)
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("the persisted witness fabricated confirmed stop evidence")
	}
	if persisted.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("persisted cleanup = %+v, want pending before the fresh engine runs", persisted.Cleanup)
	}
	if workspace.isReleased(workspaceID) {
		t.Fatal("the crashed pass released the workspace")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 0 {
		t.Fatalf("crashed pass effects = start:%d observe:%d stop:%d remove:%d, want none",
			starts, observes, stops, removes)
	}

	// A fresh engine over a fresh store reader converges from the persisted
	// proof alone. The runtime still reports absence and an unconfirmed stop.
	fresh := kernelengine.New(executionstore.NewEngine(adapterController(t, env.shard)), runtime, workspace, clock)
	completed, err := fresh.Reconcile(ctx, executionID)
	if err != nil {
		t.Fatalf("fresh Reconcile: %v%s", err, pgtest.CauseText(err))
	}
	if completed.State != kernel.ExecutionCancelled || completed.Failure != nil {
		t.Fatalf("fresh terminal outcome = %q/%+v, want the preserved cancellation",
			completed.State, completed.Failure)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("fresh cleanup = %+v, want complete", completed.Cleanup)
	}
	if completed.Cleanup.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("fresh cleanup workload = %q, want %q",
			completed.Cleanup.WorkloadID, action.Container.WorkloadID)
	}
	if !workspace.isReleased(workspaceID) {
		t.Fatal("fresh engine did not release the workspace")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("fresh effects = start:%d observe:%d stop:%d remove:%d, want 0/0/0/1",
			starts, observes, stops, removes)
	}

	// The proof remains durable after cleanup completed.
	after, err := executionstore.NewEngine(adapterController(t, env.shard)).Load(ctx, executionID)
	if err != nil {
		t.Fatalf("Load after cleanup: %v%s", err, pgtest.CauseText(err))
	}
	if !after.Actions[0].Container.Observation.ProofOfNonexecution() {
		t.Fatalf("completed cleanup lost the never-started proof: %+v",
			after.Actions[0].Container.Observation)
	}
}

func waitPrepareReached(t *testing.T, runtime *prepareCancellationRuntime, ctx context.Context) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-runtime.preparedAt:
	case <-bounded.Done():
		t.Fatalf("Prepare was not reached: %v", bounded.Err())
	}
}
