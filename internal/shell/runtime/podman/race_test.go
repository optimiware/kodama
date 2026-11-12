// Race and cancellation behavior: a start cancelled at a barrier stays
// unknown, concurrent duplicate starts never create a second container, a
// removal fence is never crossed by an in-flight start, a stop acknowledgment
// never confirms during a start race, and concurrent credential refreshes leave
// one whole token.
package podman

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func TestStartCancelledBeforeCallHasNoEffect(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := fx.adapter.Start(ctx, startRequest(fx))
	if err == nil {
		t.Fatal("Start hid a cancelled context")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("cancelled Start created %d containers", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("cancelled Start started %d containers", calls)
	}
}

func TestStartCancelledAtCreateBarrierStaysUnknown(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.libpod.newGate("CreateContainer")

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result engine.StartResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := fx.adapter.Start(ctx, startRequest(fx))
		done <- outcome{result: result, err: err}
	}()
	waitFor(t, gate.entered, "CreateContainer at the barrier")
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("cancelled Start returned no error")
		}
		if got.result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", got.result.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	if count := fx.libpod.containerCount(); count != 0 {
		t.Fatalf("cancelled create left %d containers", count)
	}

	fx.libpod.clearGate("CreateContainer")
	retry := fx.start(t)
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q, want accepted", retry.Outcome)
	}
	if count := fx.libpod.containerCount(); count != 1 {
		t.Fatalf("containers after retry = %d, want 1", count)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 2 {
		t.Fatalf("CreateContainer calls = %d, want one cancelled and one successful", calls)
	}
}

func TestConcurrentStartsCreateOneContainer(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	const workers = 8
	start := make(chan struct{})
	results := make(chan engine.StartResult, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			result, err := fx.adapter.Start(context.Background(), startRequest(fx))
			results <- result
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)

	accepted := 0
	for result := range results {
		if result.Outcome == engine.StartProvenNotStarted {
			t.Fatalf("concurrent start claimed proven-not-started: %+v", result)
		}
		if result.Outcome == engine.StartAccepted {
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatal("no concurrent start was accepted")
	}
	if count := fx.libpod.containerCount(); count != 1 {
		t.Fatalf("concurrent starts created %d containers, want 1", count)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls < 1 {
		t.Fatalf("CreateContainer calls = %d, want at least one", calls)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Start returned error: %v", err)
		}
	}
}

func TestStartAndRemoveDoNotCrossTheFence(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.libpod.newGate("CreateContainer")

	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	started := make(chan startOutcome, 1)
	go func() {
		result, err := fx.adapter.Start(context.Background(), startRequest(fx))
		started <- startOutcome{result: result, err: err}
	}()
	waitFor(t, gate.entered, "CreateContainer at the barrier")

	type removeOutcome struct {
		removal engine.Removal
		err     error
	}
	removed := make(chan removeOutcome, 1)
	go func() {
		removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
		removed <- removeOutcome{removal: removal, err: err}
	}()

	close(gate.release)

	var startResult startOutcome
	select {
	case startResult = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
	var removeResult removeOutcome
	select {
	case removeResult = <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove did not return")
	}
	if startResult.err != nil {
		t.Fatalf("Start returned error: %v", startResult.err)
	}
	if startResult.result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q, want accepted", startResult.result.Outcome)
	}

	if removeResult.err != nil {
		t.Fatalf("Remove returned error: %v", removeResult.err)
	}
	if removeResult.removal.Removed {
		// Remove won the race before the create effect. The fence must then
		// prevent an orphan container and any later start.
		if count := fx.libpod.containerCount(); count != 0 {
			t.Fatalf("Remove reported success but left %d orphan containers", count)
		}
		if hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove reported success but left the materialization behind")
		}
		if _, err := fx.adapter.Start(context.Background(), startRequest(fx)); err == nil {
			t.Fatal("Start crossed the durable removal fence")
		}
		return
	}

	// Remove lost the race to the accepted start. It must retain the live
	// container and materialization rather than reporting a false success.
	if _, ok := fx.libpod.container(fx.containerName); !ok {
		t.Fatal("Remove reported a retained removal but the container disappeared")
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove retained the container but deleted its materialization")
	}
}

func TestStopDuringStartNeverConfirms(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.libpod.setStopEffect(false)
	gate := fx.libpod.newGate("CreateContainer")

	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	started := make(chan startOutcome, 1)
	go func() {
		result, err := fx.adapter.Start(context.Background(), startRequest(fx))
		started <- startOutcome{result: result, err: err}
	}()
	waitFor(t, gate.entered, "CreateContainer at the barrier")

	type stopOutcome struct {
		evidence kernel.StopEvidence
		err      error
	}
	stopped := make(chan stopOutcome, 1)
	go func() {
		evidence, err := fx.adapter.Stop(context.Background(), stopRequest(fx))
		stopped <- stopOutcome{evidence: evidence, err: err}
	}()

	close(gate.release)

	select {
	case got := <-started:
		if got.err != nil {
			t.Fatalf("Start returned error: %v", got.err)
		}
		if got.result.Outcome != engine.StartAccepted {
			t.Fatalf("start outcome = %q, want accepted", got.result.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
	select {
	case got := <-stopped:
		if got.err != nil {
			t.Fatalf("Stop returned error: %v", got.err)
		}
		if got.evidence.IsConfirmed() {
			t.Fatal("a stop racing with a start confirmed the stop from an acknowledgment")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestConcurrentRefreshKeepsTokenWhole(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)

	const workers = 8
	tokens := make([]string, workers)
	for index := range tokens {
		tokens[index] = "tok-" + strings.Repeat("x", index+1)
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	delivered := make(chan bool, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range workers {
		go func() {
			defer wait.Done()
			<-start
			refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
				Identity:   fx.request.Identity,
				Credential: tokens[index],
			})
			errs <- err
			delivered <- refresh.Delivered
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	close(delivered)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent refresh returned error: %v", err)
		}
	}
	for ok := range delivered {
		if !ok {
			t.Fatal("concurrent refresh did not report delivery")
		}
	}
	final := string(readHostFile(t, fx.credentialPath()))
	found := slices.Contains(tokens, final)
	if !found {
		t.Fatalf("final token %q is torn or not one of the delivered values", final)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("concurrent refresh created %d containers", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("concurrent refresh restarted %d containers", calls)
	}
}
