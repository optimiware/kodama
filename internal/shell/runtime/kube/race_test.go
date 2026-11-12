// Cancellation and concurrency schedules: explicit barriers, no wall-clock
// sleeps, and one stable workload under concurrent retries.
package kube

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// TestStartCancelledBeforeCallHasNoEffect proves a cancelled context never
// reaches the external effect.
func TestStartCancelledBeforeCallHasNoEffect(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := adapter.Start(ctx, engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Start succeeded with a cancelled context")
	}
	if result.Outcome == engine.StartAccepted {
		t.Fatal("cancelled Start reported an accepted outcome")
	}
	if fake.callCount("CreateJob") != 0 {
		t.Fatal("cancelled Start reached the API")
	}
	if fake.jobCount() != 0 {
		t.Fatal("cancelled Start created a job")
	}
}

// TestStartCancellationAtBarrierStaysUnknown proves cancelling a request that
// may already have reached the API stays unknown rather than proven-not-started.
func TestStartCancellationAtBarrierStaysUnknown(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	gate := fake.newGate("CreateJob")
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		result engine.StartResult
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		result, err := adapter.Start(ctx, engine.StartRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		results <- outcome{result: result, err: err}
	}()

	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("Start never reached the CreateJob barrier")
	}
	cancel()
	close(gate.release)

	got := <-results
	if got.err == nil {
		t.Fatal("cancelled Start hid the cancellation")
	}
	if got.result.Outcome != engine.StartUnknown {
		t.Fatalf("cancelled-at-barrier outcome = %q; want %q", got.result.Outcome, engine.StartUnknown)
	}
	if got.result.Outcome == engine.StartProvenNotStarted {
		t.Fatal("cancelled-at-barrier outcome collapsed into proven-not-started")
	}
}

// TestStartConcurrentSameIdentityCreatesOneJob proves concurrent retries of the
// same persisted identity converge on the single stable Job.
func TestStartConcurrentSameIdentityCreatesOneJob(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	const workers = 4
	start := make(chan struct{})
	results := make([]engine.StartResult, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = adapter.Start(context.Background(), engine.StartRequest{
				Identity:    request.Identity,
				WorkspaceID: request.WorkspaceID,
			})
		}(index)
	}
	close(start)
	wait.Wait()

	for index := range workers {
		if errs[index] != nil {
			t.Fatalf("worker %d Start returned error: %v", index, errs[index])
		}
		if results[index].Outcome != engine.StartAccepted {
			t.Fatalf("worker %d outcome = %q; want accepted", index, results[index].Outcome)
		}
	}
	if fake.jobCount() != 1 {
		t.Fatalf("concurrent starts created %d jobs; want exactly 1", fake.jobCount())
	}
}

// TestStopCancellationStaysUnconfirmed proves a cancelled stop request never
// fabricates a stop barrier.
func TestStopCancellationStaysUnconfirmed(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	gate := fake.newGate("SuspendJob")
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		evidence kernel.StopEvidence
		err      error
	}
	results := make(chan outcome, 1)
	go func() {
		evidence, err := adapter.Stop(ctx, engine.StopRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		results <- outcome{evidence: evidence, err: err}
	}()

	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("Stop never reached the SuspendJob barrier")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never reached the SuspendJob barrier")
	}
	cancel()
	close(gate.release)

	got := <-results
	if got.err == nil {
		t.Fatal("cancelled Stop hid the cancellation")
	}
	if got.evidence.IsConfirmed() {
		t.Fatal("cancelled Stop reported a confirmed stop")
	}
}

// TestRefreshAndObserveConcurrently proves credential refresh and observation
// are safe to interleave and never restart the workload.
func TestRefreshAndObserveConcurrently(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	fake.addPod(testRunningPod(request.Identity))

	const workers = 4
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			refresh, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
				Identity:   request.Identity,
				Credential: "tok-rotated",
			})
			if err == nil && !refresh.Delivered {
				err = errNotDelivered
			}
			errs <- err
		}()
		go func() {
			defer wait.Done()
			<-start
			observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
				Identity:    request.Identity,
				WorkspaceID: request.WorkspaceID,
			})
			if err == nil && observation.Kind != kernel.ObservationRunning {
				err = errNotRunning
			}
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation failed: %v", err)
		}
	}

	if fake.jobCount() != 1 {
		t.Fatalf("concurrent operations left %d jobs; want 1", fake.jobCount())
	}
	if fake.callCount("CreateJob") != 1 {
		t.Fatal("concurrent operations replayed the start")
	}
}

var (
	errNotDelivered = &concurrentError{"refresh was not delivered"}
	errNotRunning   = &concurrentError{"observation was not running"}
)

type concurrentError struct{ message string }

func (e *concurrentError) Error() string { return e.message }
