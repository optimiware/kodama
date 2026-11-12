package git_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	git "github.com/optimiware/kodama/internal/userland/git"
)

func updateRefCommandMatches(matchRef string) func(git.Command) bool {
	return func(command git.Command) bool {
		if commandSubcommand(command) != "update-ref" {
			return false
		}
		args := command.Args
		if len(args) < 5 {
			return false
		}
		return args[3] == matchRef
	}
}

// TestSerializedSameRefUpdates pins service-owned serialization: two pushes to
// the same repository/ref never run their compare-and-swap concurrently, and the
// loser is refused for a stale base instead of overwriting the winner.
func TestSerializedSameRefUpdates(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)
	f.executor.setAncestor("shop/web", testBase, "cand-2", true)

	entered, release := f.executor.gateOn(updateRefCommandMatches(testRef))

	first := f.updateRequest()
	firstResult := make(chan error, 1)
	go func() {
		_, err := f.service.Update(context.Background(), first)
		firstResult <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first update never reached the compare-and-swap")
	}

	second := updateRequestFor("shop/web", testRef, testBase, "cand-2", "git-event-2")
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := f.service.Update(context.Background(), second)
		secondResult <- err
	}()
	<-secondStarted

	// While the first compare-and-swap is held, the second attempt must not
	// issue its own compare-and-swap.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if count := f.executor.updateRefCount(); count != 1 {
			t.Fatalf("concurrent update-ref commands = %d; want 1", count)
		}
		time.Sleep(2 * time.Millisecond)
	}
	release()

	if err := <-firstResult; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, git.ErrStaleBase) {
		t.Fatalf("second update error = %v; want ErrStaleBase", err)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("update-ref commands = %d; want one", f.executor.updateRefCount())
	}
	if got := f.executor.commit("shop/web", testRef); got != testCandidate {
		t.Fatalf("ref = %q; want the first candidate", got)
	}
	if state, ok := f.journal.state(first.SourceEventID); !ok || state != git.JournalReported {
		t.Fatalf("first journal state = %q,%v; want reported", state, ok)
	}
	if state, ok := f.journal.state(second.SourceEventID); !ok || state != git.JournalRefused {
		t.Fatalf("second journal state = %q,%v; want refused", state, ok)
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("reports = %d; want one", len(calls))
	}
}

// TestIndependentRefsAreNotSerialized pins that the serialization is scoped to
// the repository/ref, not global: a held update on main must not block a
// different ref.
func TestIndependentRefsAreNotSerialized(t *testing.T) {
	f := newFixture(t)
	f.verifier.script("exec-token", executionScript("exe-1"))
	f.verifier.script("eng-token", humanScript("prn-eng", "engineer"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entered, release := f.executor.gateOn(updateRefCommandMatches(testRef))
	mainRequest := f.updateRequest()
	mainRequest.ActorCredential = "exec-token"
	blocked := make(chan error, 1)
	go func() {
		_, err := f.service.Update(context.Background(), mainRequest)
		blocked <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("main update never reached the compare-and-swap")
	}

	independent := updateRequestFor("shop/web", "refs/heads/work/x", "", "work-1", "git-event-work")
	independent.ActorCredential = "eng-token"
	done := make(chan error, 1)
	go func() {
		_, err := f.service.Update(context.Background(), independent)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			release()
			t.Fatalf("independent ref update: %v", err)
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("an independent ref was blocked by the held main update")
	}
	if state, ok := f.journal.state(independent.SourceEventID); !ok || state != git.JournalReported {
		t.Fatalf("independent journal state = %q,%v; want reported", state, ok)
	}
	release()
	if err := <-blocked; err != nil {
		t.Fatalf("main update: %v", err)
	}
}

// TestConcurrentDuplicateDeliverySettlesOnce pins stable source-event
// serialization: two concurrent deliveries of the same receive event verify,
// push and report exactly once.
func TestConcurrentDuplicateDeliverySettlesOnce(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entered, release := f.executor.gateOn(updateRefCommandMatches(testRef))
	request := f.updateRequest()

	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := f.service.Update(context.Background(), request)
		results <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("first delivery never reached the compare-and-swap")
	}
	go func() {
		defer wait.Done()
		_, err := f.service.Update(context.Background(), request)
		results <- err
	}()

	// Let the duplicate reach the service while the first is held.
	time.Sleep(50 * time.Millisecond)
	release()
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("duplicate delivery: %v", err)
		}
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("verifications = %d; want one", len(calls))
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("update-ref commands = %d; want one", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("reports = %d; want one", len(calls))
	}
	if state, ok := f.journal.state(request.SourceEventID); !ok || state != git.JournalReported {
		t.Fatalf("journal state = %q,%v; want reported", state, ok)
	}
}

// TestConcurrentIndependentSourceEventsBothSettle pins that distinct source
// events on different refs both settle even when started together.
func TestConcurrentIndependentSourceEventsBothSettle(t *testing.T) {
	f := newFixture(t)
	f.verifier.script("eng-token", humanScript("prn-eng", "engineer"))
	f.verifier.script("svc-token", serviceScript("ci-pipeline"))

	requests := []git.UpdateRequest{
		updateRequestFor("shop/web", "refs/heads/work/a", "", "a-1", "git-event-a"),
		updateRequestFor("shop/web", "refs/heads/work/b", "", "b-1", "git-event-b"),
		updateRequestFor("shop/guarded", "refs/heads/svc", "", "svc-1", "git-event-svc"),
	}
	requests[0].ActorCredential = "eng-token"
	requests[1].ActorCredential = "eng-token"
	requests[2].ActorCredential = "svc-token"

	var wait sync.WaitGroup
	errs := make(chan error, len(requests))
	for _, request := range requests {
		wait.Add(1)
		go func(request git.UpdateRequest) {
			defer wait.Done()
			_, err := f.service.Update(context.Background(), request)
			errs <- err
		}(request)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	if f.executor.updateRefCount() != len(requests) {
		t.Fatalf("update-ref commands = %d; want %d", f.executor.updateRefCount(), len(requests))
	}
}
