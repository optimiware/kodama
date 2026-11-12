//go:build integration

package services

// Durable merge serialization, restart recovery and observe-only release.
//
// The scenarios extend the single-coordinator real-Git proof
// (merge_coordinator_real_git_test.go) with the coordination properties:
//
//   - two coordinators over one durable FileCoordinationStore serialize the
//     same repository/ref: the live claim refuses the second coordinator and no
//     second effect runs;
//   - the effect is real native Git against a scratch bare repository, and the
//     surviving coordinator recovers after a restart by observing the remote
//     (not by repeating the merge) because the previous process stopped after
//     the push and before its observation was durable;
//   - the real served edge records exactly one answer and one request.resolved
//     event for the exact observed revisions;
//   - a cancelled uncertain assignment is not released by cancellation alone:
//     an inconclusive observation keeps it held, and only a conclusive
//     observation releases it without executing again or answering the
//     cancelled request.
//
// The only injected behaviour is the labelled process-death seam that performs
// the real effect and then loses the observation; recovery then uses the real
// durable effect record and remote ref.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	serviceadapter "github.com/optimiware/kodama/internal/userland/services"
	servicesapp "github.com/optimiware/kodama/internal/userland/services/app"
	"github.com/optimiware/kodama/test/support/factory"
)

// mergeSerializationClock is the coordinators' injected time boundary. Both
// coordinators share it so a restart can advance past the crashed claim's lease.
type mergeSerializationClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *mergeSerializationClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *mergeSerializationClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(duration)
}

// mergeSerializationCounts is shared by every effects instance in one scenario
// so a repeated execute or a missing observation is visible after a restart.
type mergeSerializationCounts struct {
	executes atomic.Int32
	observes atomic.Int32
}

func (counts *mergeSerializationCounts) executeCount() int { return int(counts.executes.Load()) }
func (counts *mergeSerializationCounts) observeCount() int { return int(counts.observes.Load()) }

// mergeSerializationEffects wraps the concrete Git effects with the labelled
// process-death and observation faults. It never fabricates a successful
// observation: each Execute and Observe delegates to the real Git boundary.
type mergeSerializationEffects struct {
	real    serviceadapter.MergeEffects
	counts  *mergeSerializationCounts
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once

	// crashAfterExecute models a coordinator process that pushed the real
	// result and then died before persisting its observation.
	crashAfterExecute bool
	// observe overrides the real observation when set. It is the explicit
	// inconclusive-observation seam.
	observe func(serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error)
}

func (effects *mergeSerializationEffects) Execute(ctx context.Context, operation serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error) {
	effects.counts.executes.Add(1)
	if effects.entered != nil {
		effects.once.Do(func() { close(effects.entered) })
	}
	if effects.release != nil {
		select {
		case <-effects.release:
		case <-ctx.Done():
			return serviceadapter.MergeObservation{State: serviceadapter.EffectUncertain}, ctx.Err()
		}
	}
	observation, err := effects.real.Execute(ctx, operation)
	if err != nil {
		return observation, err
	}
	if effects.crashAfterExecute && observation.State == serviceadapter.EffectApplied {
		return serviceadapter.MergeObservation{State: serviceadapter.EffectUncertain},
			errors.New("fault injection: coordinator stopped after the push before recording its observation")
	}
	return observation, nil
}

func (effects *mergeSerializationEffects) Observe(ctx context.Context, operation serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error) {
	effects.counts.observes.Add(1)
	if effects.observe != nil {
		return effects.observe(operation)
	}
	return effects.real.Observe(ctx, operation)
}

// newMergeSerializationEffects builds one fresh concrete Git effects instance
// over the shared durable effect directory, like a restarted process would.
func newMergeSerializationEffects(t *testing.T, gitRoot, effectDirectory string) *serviceadapter.GitMergeEffects {
	t.Helper()
	executor, err := servicesapp.NewOSGitExecutor(servicesapp.OSGitExecutorConfig{})
	if err != nil {
		t.Fatalf("NewOSGitExecutor: %v", err)
	}
	effects, err := serviceadapter.NewGitMergeEffects(serviceadapter.GitMergeEffectsConfig{
		RemoteBaseURL:  "file://" + gitRoot,
		StateDirectory: effectDirectory,
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	return effects
}

// newMergeSerializationCoordinator opens the shared durable coordination store
// for one coordinator process identity.
func newMergeSerializationCoordinator(t *testing.T, directory, owner string, effects serviceadapter.MergeEffects, clock serviceadapter.Clock, operationID string) *serviceadapter.Coordinator {
	t.Helper()
	store, err := serviceadapter.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	coordinator, err := serviceadapter.NewCoordinator(serviceadapter.CoordinatorConfig{
		TenantID: id.TenantID(factory.TenantID),
		Owner:    owner,
		Lease:    time.Minute,
		Store:    store,
		Effects:  effects,
		Clock:    clock,
		NewOperationID: func() (id.OperationID, error) {
			return id.OperationID(operationID), nil
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return coordinator
}

// loadMergeCoordinationRecord reads one durable record from the shared store.
func loadMergeCoordinationRecord(t *testing.T, directory string, key serviceadapter.MergeKey) serviceadapter.CoordinationRecord {
	t.Helper()
	store, err := serviceadapter.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	record, ok, err := store.Load(t.Context(), id.TenantID(factory.TenantID), key)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !ok {
		t.Fatal("durable merge coordination record is missing")
	}
	return record
}

// TestMergeCoordinatorRealGitSerialization proves two coordinators over one
// durable store cannot run the same merge twice, and that a coordinator
// restarted after a post-push crash recovers by observing the real remote and
// answers the request exactly once.
func TestMergeCoordinatorRealGitSerialization(t *testing.T) {
	harness := factory.Start(t)
	root := t.TempDir()
	base, candidate := mergeRealSeedRepository(t, root)

	access := createResponderService(t, harness, mergeRealServiceID)
	applied := mergeRealApplyPipeline(t, harness, "merge-real-serialization", base, candidate)
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-real-serialization")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "merge")

	clock := &mergeSerializationClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	directory := t.TempDir()
	effectDirectory := t.TempDir()
	counts := &mergeSerializationCounts{}

	release := make(chan struct{})
	entered := make(chan struct{})
	firstEffects := &mergeSerializationEffects{
		real:              newMergeSerializationEffects(t, root, effectDirectory),
		counts:            counts,
		entered:           entered,
		release:           release,
		crashAfterExecute: true,
	}
	first := newMergeSerializationCoordinator(t, directory, "merge-coordinator-a", firstEffects, clock, "merge-op-serialization")
	liveSecond := newMergeSerializationCoordinator(t, directory, "merge-coordinator-b", newMergeSerializationEffects(t, root, effectDirectory), clock, "merge-op-unused")

	key := serviceadapter.MergeKey{Repository: mergeRealRepository, Ref: mergeRealRef}
	request := serviceadapter.MergeRequest{
		Key: key,
		Assignment: serviceadapter.MergeAssignment{
			ExecutionID: id.ExecutionID(started.ExecutionID),
			ActionID:    id.ActionID(actionByName(t, waiting, "merge").ActionID),
		},
		Base:      base,
		Candidate: candidate,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("MergeRequest.Validate: %v", err)
	}

	firstClaim, err := first.Claim(t.Context(), key)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if firstClaim.Generation != 1 {
		t.Fatalf("first claim generation = %d; want 1", firstClaim.Generation)
	}

	// The first coordinator is inside its real effect while the second still
	// holds the same durable store: the live claim refuses it and nothing runs.
	result := make(chan error, 1)
	go func() { _, mergeErr := first.Merge(t.Context(), request); result <- mergeErr }()
	waitSignal(t, entered, "the first coordinator's effect to start")

	paused, err := liveSecond.Merge(t.Context(), request)
	if !errors.Is(err, serviceadapter.ErrClaimed) {
		t.Fatalf("live second coordinator Merge error = %v; want ErrClaimed", err)
	}
	if paused.State != "" {
		t.Fatalf("refused second coordinator returned result %+v; want no effect", paused)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions while the first claim is live = %d; want only the first", got)
	}

	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, serviceadapter.ErrUncertain) {
			t.Fatalf("crashed first coordinator error = %v; want ErrUncertain", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("first coordinator did not finish its real effect")
	}

	uncertain := loadMergeCoordinationRecord(t, directory, key)
	if uncertain.Released || uncertain.State != serviceadapter.EffectUncertain {
		t.Fatalf("record after the post-push crash = %+v; want a held uncertain record", uncertain)
	}
	if uncertain.Claim.OperationID != firstClaim.OperationID {
		t.Fatalf("persisted operation identity = %q; want the first claim identity %q", uncertain.Claim.OperationID, firstClaim.OperationID)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions after the crash = %d; want exactly one", got)
	}
	bare := filepath.Join(root, mergeRealRepository)
	if remote := mergeRealRunGit(t, bare, "rev-parse", mergeRealRef); remote != candidate {
		t.Fatalf("remote %s after the real effect = %s; want the candidate %s", mergeRealRef, remote, candidate)
	}

	// Restart: the crashed lease has elapsed, so a fresh coordinator process
	// claims the same generated record and must observe rather than execute.
	clock.Advance(2 * time.Minute)
	restartedEffects := &mergeSerializationEffects{
		real:   newMergeSerializationEffects(t, root, effectDirectory),
		counts: counts,
	}
	restarted := newMergeSerializationCoordinator(t, directory, "merge-coordinator-b", restartedEffects, clock, "merge-op-unused")
	handler := newResponderHandler(harness, access.token)
	adapter := newIntegrationMergeAdapter(t, access, handler, emptyNotificationSource{}, restarted)
	if err := adapter.RunOnce(t.Context()); err != nil {
		t.Fatalf("restarted merge RunOnce: %v", err)
	}

	settled := waitExecutionSettled(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded (failure=%+v)", settled.State, settled.Failure)
	}
	action := actionByName(t, settled, "merge")
	if action.Decision != "pass" || action.Actor != mergeRealServiceID {
		t.Fatalf("merge answer = %+v; want the observing service's pass result", action)
	}
	if action.Outputs["base"] != base || action.Outputs["candidate"] != candidate || action.Outputs["output"] != candidate {
		t.Fatalf("merge outputs = %v; want the exact observed revisions", action.Outputs)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one answer", got)
	}
	if resolves := handler.resolveAttempts(); resolves != 1 {
		t.Fatalf("resolve submissions = %d; want exactly one answer", resolves)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions across the restart = %d; want no repeated merge", got)
	}
	if got := counts.observeCount(); got < 1 {
		t.Fatalf("observations across the restart = %d; the recovery did not observe the remote", got)
	}
	if remote := mergeRealRunGit(t, bare, "rev-parse", mergeRealRef); remote != candidate {
		t.Fatalf("remote %s after recovery = %s; want the single observed candidate %s", mergeRealRef, remote, candidate)
	}
	record := loadMergeCoordinationRecord(t, directory, key)
	if !record.Released || record.State != serviceadapter.EffectApplied {
		t.Fatalf("record after the answer = %+v; want a released applied assignment", record)
	}
	if record.Claim.OperationID != firstClaim.OperationID {
		t.Fatalf("operation identity after restart = %q; want the preserved %q", record.Claim.OperationID, firstClaim.OperationID)
	}
	if record.Claim.Generation != 2 {
		t.Fatalf("generation after restart = %d; want the restarted claim generation 2", record.Claim.Generation)
	}
}

// TestMergeUncertainReleaseNeedsObservation proves a cancelled uncertain
// assignment is released only after a conclusive observation, never by
// cancellation or lease expiry alone.
func TestMergeUncertainReleaseNeedsObservation(t *testing.T) {
	harness := factory.Start(t)
	root := t.TempDir()
	base, candidate := mergeRealSeedRepository(t, root)

	access := createResponderService(t, harness, mergeRealServiceID)
	applied := mergeRealApplyPipeline(t, harness, "merge-real-cancelled-release", base, candidate)
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-real-cancelled-release")
	waitRequestRunning(t, harness, started.ExecutionID, "merge")

	clock := &mergeSerializationClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	directory := t.TempDir()
	effectDirectory := t.TempDir()
	counts := &mergeSerializationCounts{}

	key := serviceadapter.MergeKey{Repository: mergeRealRepository, Ref: mergeRealRef}

	// The first process performs the real push and dies before recording the
	// observation, leaving the real remote advanced and the record uncertain.
	firstEffects := &mergeSerializationEffects{
		real:              newMergeSerializationEffects(t, root, effectDirectory),
		counts:            counts,
		crashAfterExecute: true,
	}
	first := newMergeSerializationCoordinator(t, directory, "merge-coordinator-cancelled", firstEffects, clock, "merge-op-cancelled")
	firstAdapter := newIntegrationMergeAdapter(t, access, newResponderHandler(harness, access.token), emptyNotificationSource{}, first)
	if err := firstAdapter.RunOnce(t.Context()); err == nil {
		t.Fatal("the first adapter reported success although its observation was lost")
	}

	uncertain := loadMergeCoordinationRecord(t, directory, key)
	if uncertain.Released || uncertain.State != serviceadapter.EffectUncertain {
		t.Fatalf("record after the lost observation = %+v; want a held uncertain record", uncertain)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions before cancellation = %d; want exactly one", got)
	}
	// The pre-dispatch safety observation is the one Reconcile the coordinator
	// performs before a fresh intent is executed.
	if got := counts.observeCount(); got != 1 {
		t.Fatalf("observations before cancellation = %d; want the single pre-dispatch observation", got)
	}
	bare := filepath.Join(root, mergeRealRepository)
	if remote := mergeRealRunGit(t, bare, "rev-parse", mergeRealRef); remote != candidate {
		t.Fatalf("remote %s = %s; want the real effect's candidate %s", mergeRealRef, remote, candidate)
	}

	// Cancel the request. Cancellation ends the request but is not evidence
	// about the external merge, so the assignment must stay held.
	current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	cancelResult, publicError := harness.REST(harness.ProjectToken, "execution.cancel", api.ExecutionCancelRequest{
		ExecutionID: started.ExecutionID,
	}, &current.Version, "cancel-merge-uncertain-release")
	if publicError.Code != "" {
		t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var cancelled api.ExecutionCancelResult
	harness.Decode(cancelResult, &cancelled)
	if cancelled.ExecutionVersion == 0 {
		t.Fatal("execution.cancel returned no committed version")
	}
	cancelledView := waitExecutionSettled(t, harness, started.ExecutionID)
	if cancelledView.State != "cancelled" {
		t.Fatalf("execution state = %q; want cancelled", cancelledView.State)
	}
	cancelledAction := actionByName(t, cancelledView, "merge")
	if cancelledAction.State != "cancelled" || cancelledAction.Decision != "" {
		t.Fatalf("cancelled request action = %+v; want an unanswered cancelled action", cancelledAction)
	}
	held := loadMergeCoordinationRecord(t, directory, key)
	if held.Released || held.State != serviceadapter.EffectUncertain {
		t.Fatalf("record immediately after cancellation = %+v; want still held and uncertain", held)
	}
	firstPending, err := first.Pending(t.Context())
	if err != nil {
		t.Fatalf("Pending after cancellation: %v", err)
	}
	if len(firstPending) != 1 {
		t.Fatalf("pending assignments after cancellation = %+v; want the held assignment", firstPending)
	}

	// An inconclusive observation is not evidence: the assignment stays held.
	inconclusive := &mergeSerializationEffects{
		real:   newMergeSerializationEffects(t, root, effectDirectory),
		counts: counts,
		observe: func(serviceadapter.MergeOperation) (serviceadapter.MergeObservation, error) {
			return serviceadapter.MergeObservation{State: serviceadapter.EffectUncertain}, nil
		},
	}
	heldCoordinator := newMergeSerializationCoordinator(t, directory, "merge-coordinator-observation-1", inconclusive, clock, "merge-op-unused")
	heldAdapter := newIntegrationMergeAdapter(t, access, newResponderHandler(harness, access.token), emptyNotificationSource{}, heldCoordinator)
	if err := heldAdapter.RunOnce(t.Context()); err != nil {
		t.Fatalf("inconclusive observation RunOnce: %v", err)
	}
	if got := counts.observeCount(); got != 2 {
		t.Fatalf("observations after the inconclusive pass = %d; want the pre-dispatch observation plus one release observation", got)
	}
	afterInconclusive := loadMergeCoordinationRecord(t, directory, key)
	if afterInconclusive.Released || afterInconclusive.State != serviceadapter.EffectUncertain {
		t.Fatalf("record after an inconclusive observation = %+v; want it still held", afterInconclusive)
	}
	stillPending, err := heldCoordinator.Pending(t.Context())
	if err != nil {
		t.Fatalf("Pending after inconclusive observation: %v", err)
	}
	if len(stillPending) != 1 {
		t.Fatalf("pending after an inconclusive observation = %+v; want the assignment held", stillPending)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions after the inconclusive pass = %d; want no repeat", got)
	}

	// The conclusive real observation releases the held assignment. The
	// cancelled request is not answered and the merge is not repeated.
	conclusive := &mergeSerializationEffects{
		real:   newMergeSerializationEffects(t, root, effectDirectory),
		counts: counts,
	}
	releaseHandler := newResponderHandler(harness, access.token)
	releaseCoordinator := newMergeSerializationCoordinator(t, directory, "merge-coordinator-observation-2", conclusive, clock, "merge-op-unused")
	releaseAdapter := newIntegrationMergeAdapter(t, access, releaseHandler, emptyNotificationSource{}, releaseCoordinator)
	if err := releaseAdapter.RunOnce(t.Context()); err != nil {
		t.Fatalf("conclusive observation RunOnce: %v", err)
	}

	released := loadMergeCoordinationRecord(t, directory, key)
	if !released.Released {
		t.Fatalf("record after the conclusive observation = %+v; want released", released)
	}
	if released.State != serviceadapter.EffectApplied || released.Result.ObservedOutput != candidate {
		t.Fatalf("released record = %+v; want the observed applied candidate", released)
	}
	if got := counts.executeCount(); got != 1 {
		t.Fatalf("executions after release = %d; want no repeated merge", got)
	}
	if got := counts.observeCount(); got != 3 {
		t.Fatalf("observations after release = %d; want the pre-dispatch observation plus one per release pass", got)
	}
	if resolves := releaseHandler.resolveAttempts(); resolves != 0 {
		t.Fatalf("resolve submissions after release = %d; a cancelled request must not be answered", resolves)
	}
	pending, err := releaseCoordinator.Pending(t.Context())
	if err != nil {
		t.Fatalf("Pending after release: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after release = %+v; want none", pending)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 0 {
		t.Fatalf("committed request.resolved events = %d; a cancelled request has no answer", got)
	}
	final := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if final.State != "cancelled" {
		t.Fatalf("execution state after release = %q; want cancelled", final.State)
	}
	if finalAction := actionByName(t, final, "merge"); finalAction.Decision != "" {
		t.Fatalf("cancelled request gained an answer: %+v", finalAction)
	}
	if remote := mergeRealRunGit(t, bare, "rev-parse", mergeRealRef); remote != candidate {
		t.Fatalf("remote %s after release = %s; want the observed candidate %s", mergeRealRef, remote, candidate)
	}
}
