package app

// The concurrent first-claim race. Two different verified subjects present two
// valid access tokens for the same configured tenant and reach the store seam
// together. The tenant's unique insert is the transactional one-shot guard:
// exactly one unit commits and the loser is a version_conflict with no
// membership, audit or event of its own.
//
// Run with -race: the shared memory store, envelope cipher, clock and the two
// independent verifiers/ID sources are all exercised concurrently.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestBootstrapAdminConcurrentFirstClaim proves the unique tenant insert is the
// transactional one-shot guard: two different verified subjects racing the same
// tenant produce exactly one winner and one version-conflict loser, and the
// winner's identity is the only membership granted.
func TestBootstrapAdminConcurrentFirstClaim(t *testing.T) {
	clock := newFakeClock()
	memoryStore := memory.NewWithClock(clock.Now)
	cipher := newTestCipher(t)

	firstPrincipal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, "auth0|first-contender")
	secondPrincipal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, "auth0|second-contender")
	firstVerifier := &bootstrapRecordingVerifier{principal: firstPrincipal}
	secondVerifier := &bootstrapRecordingVerifier{principal: secondPrincipal}
	firstIDs := &bootstrapTestIDs{rowID: "row-first", eventIDs: []id.EventID{"event-audit-first", "event-config-first"}}
	secondIDs := &bootstrapTestIDs{rowID: "row-second", eventIDs: []id.EventID{"event-audit-second", "event-config-second"}}

	// One shared barrier holds both contenders at the store seam so the race is
	// genuinely contested: both verified identities reach Execute before either
	// may commit.
	release := make(chan struct{})
	ready := new(sync.WaitGroup)
	ready.Add(2)
	first := newBootstrapForTest(t, &barrierBootstrapExecutor{inner: memoryStore, ready: ready, release: release}, cipher, firstVerifier, firstIDs, clock,
		bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "first-contender-token")))
	second := newBootstrapForTest(t, &barrierBootstrapExecutor{inner: memoryStore, ready: ready, release: release}, cipher, secondVerifier, secondIDs, clock,
		bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "second-contender-token")))

	type outcome struct {
		principal id.PrincipalID
		result    BootstrapAdminResult
		err       error
	}
	// The barrier is only reachable when both contenders get past verification
	// and sealing. A changed bootstrap associated identity makes both fail
	// before the store seam, so an unbounded wait here would hang until the
	// package timeout. Bound the wait and fail with the reason instead.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	run := func(principal id.PrincipalID, bootstrap *BootstrapAdmin) {
		<-start
		result, err := bootstrap.Run(ctx)
		outcomes <- outcome{principal: principal, result: result, err: err}
	}
	go run(firstPrincipal.ID, first)
	go run(secondPrincipal.ID, second)
	close(start)
	contendersReady := make(chan struct{})
	go func() {
		ready.Wait()
		close(contendersReady)
	}()
	select {
	case <-contendersReady:
	case <-ctx.Done():
		t.Fatal("both contenders never reached the store barrier; a changed bootstrap identity or a broken barrier would hang the race (fail-fast deadline reached)")
	}
	close(release)

	successes := make([]outcome, 0, 2)
	conflicts := make([]outcome, 0, 2)
	for range 2 {
		got := <-outcomes
		if got.err == nil {
			successes = append(successes, got)
			continue
		}
		if class, ok := fault.ClassOf(got.err); !ok || class != fault.VersionConflict {
			t.Fatalf("losing contender error = %v (class=%v, ok=%v); want version_conflict", got.err, class, ok)
		}
		conflicts = append(conflicts, got)
	}
	if len(successes) != 1 || len(conflicts) != 1 {
		t.Fatalf("race outcomes: %d successes, %d conflicts; want exactly one each", len(successes), len(conflicts))
	}
	winner := successes[0]
	loser := conflicts[0]
	if winner.result.PrincipalID != winner.principal {
		t.Fatalf("winner result principal = %q; want %q", winner.result.PrincipalID, winner.principal)
	}
	scope := tenancy.Scope{TenantID: bootstrapTestTenant}
	if rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, winner.principal); len(rows) != 1 {
		t.Fatalf("winner memberships = %d; want exactly 1", len(rows))
	}
	if rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, loser.principal); len(rows) != 0 {
		t.Fatalf("loser memberships = %d; want none", len(rows))
	}
	if audits := memoryStore.Audit(scope); len(audits) != 1 || audits[0].Actor != winner.principal {
		t.Fatalf("audit rows = %+v; want exactly the winner's audit", audits)
	}
	if events := memoryStore.Events(scope); len(events) != 1 || events[0].Envelope.Source.PrincipalID != winner.principal {
		t.Fatalf("event rows = %+v; want exactly the winner's event", events)
	}
}

// barrierBootstrapExecutor blocks the first Execute of every contender until
// the test releases the shared barrier. It is deterministic synchronization,
// never a sleep.
type barrierBootstrapExecutor struct {
	inner   store.Executor
	ready   *sync.WaitGroup
	release chan struct{}
}

func (executor *barrierBootstrapExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.ready.Done()
	select {
	case <-executor.release:
	case <-ctx.Done():
		return store.UnitResult{}, ctx.Err()
	}
	return executor.inner.Execute(ctx, scope, unit)
}

func (executor *barrierBootstrapExecutor) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error) {
	return executor.inner.Read(ctx, scope, table, id)
}
