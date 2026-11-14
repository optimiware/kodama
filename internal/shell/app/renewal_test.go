package app

// Workload credential renewal in the Shell composition.
//
// The composition issues an execution credential at container activation with a
// TTL bounded by the eight-hour maximum and the persisted action deadline
// (S05 §3 "Container activation"). A long-running action therefore needs a
// renewal loop owned by the composition root: while the execution is running and
// its current container action is before its deadline, a committed credential
// nearing expiry must be atomically replaced and the replacement delivered
// through the runtime credential-refresh seam without restarting the workload.
//
// Composition API under test:
//
//	Config.RenewBefore time.Duration
//	    The renewal margin. A running execution's committed credential is
//	    renewed when its committed expiry is at or before now+RenewBefore and
//	    the execution's current action deadline is still in the future. Zero
//	    selects a composition default; a negative value is refused at
//	    construction with fault.InvalidInput.
//
//	Config.RenewInterval time.Duration
//	    The renewal scan cadence. Zero selects a composition default; the
//	    tests below set it explicitly so no real time passes between scans.
//
// The loop itself is not a public symbol: App.start starts it as one owned
// background goroutine on the same lifecycle context as the other background
// loops, and App.Close cancels and joins it before it stops adapters. Renewal
// is not new dispatch: pausing the installation dispatch gate must not stop it,
// because a running workload's credential must stay current across a fenced
// restore.
//
// Behavior pinned here (S05 §3, KODAMA.md "Execution lifetime" and
// docs/specs/07-runtime-secrets.md):
//   - Renew only while the execution is running, holds a current committed
//     credential inside the margin, and its current action deadline is in the
//     future. Never mint for a request wait, terminal, cancelled or
//     past-deadline execution, and never extend past the action deadline or the
//     eight-hour maximum.
//   - Rotate the nonce/expiry atomically through token.Manager and deliver the
//     new token exactly once per renewal through the runtime RefreshCredential
//     seam. A failed delivery is retried with the same committed token: it
//     never re-mints and never restarts the workload.
//   - Reconstruct every fact from durable state, so a restarted composition
//     resumes renewal without relying on in-memory state.
//   - One execution's failure never stops another's renewal, and each renewal
//     reads and writes only the execution's own tenant/project scope (including
//     a second tenant's derivation key).
//   - Token and secret material never appears in logs, events, results or
//     errors.
//
// No test here starts a process, container, database or broker.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TestRenewalReissuesBeforeExpiry pins the renewal margin, the single atomic
// replacement and the delivery of the replacement token.
func TestRenewalReissuesBeforeExpiry(t *testing.T) {
	const renewBefore = 30 * time.Minute
	shared := newRenewalShared(t, renewBefore, 24*time.Hour)
	app := shared.start(t)
	handler := app.Handler()
	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "renew", 1)
	execution := id.ExecutionID(executionID)

	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Present() {
		t.Fatal("activation committed no execution credential")
	}
	if want := shared.clock.Now().Add(token.DefaultBounds.MaxTTL); !before.Expiry.Equal(want) {
		t.Fatalf("activation credential expiry = %v; want the eight-hour cap %v", before.Expiry, want)
	}

	// One hour before expiry is outside the thirty-minute margin: no renewal.
	shared.clock.Advance(7 * time.Hour)
	time.Sleep(100 * time.Millisecond)
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("renewal ran %d times while the credential was outside the %s margin", attempts, renewBefore)
	}

	// Fifteen minutes before expiry is inside the margin: exactly one renewal.
	shared.clock.Advance(45 * time.Minute)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.refreshAttemptsFor(execution) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 {
		t.Fatalf("renewal attempts = %d; want exactly one per renewal", len(attempts))
	}
	delivered := attempts[0].request
	if !attempts[0].delivered {
		t.Fatalf("renewal delivery failed: %v", attempts[0].err)
	}
	if delivered.Identity.ExecutionID != execution {
		t.Fatalf("renewal delivered execution = %q; want %q", delivered.Identity.ExecutionID, execution)
	}
	if delivered.Identity.WorkloadID == "" {
		t.Fatal("renewal delivered no workload identity")
	}
	if delivered.Credential == credential {
		t.Fatal("renewal delivered the unchanged credential")
	}
	if !strings.HasPrefix(delivered.Credential, token.ExecutionPrefix+executionID+".") {
		t.Fatalf("renewed credential is not a kodex token scoped to the execution")
	}

	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("renewal did not atomically replace the committed nonce")
	}
	wantExpiry := shared.clock.Now().Add(token.DefaultBounds.MaxTTL)
	if !after.Expiry.Equal(wantExpiry) {
		t.Fatalf("renewed credential expiry = %v; want now+8h %v", after.Expiry, wantExpiry)
	}

	// The renewed credential is current; the replaced one is refused because the
	// committed nonce it was derived from is gone.
	recorder, _, _ := rawExecutionInspect(t, handler, delivered.Credential, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the renewed credential did not authenticate: status=%d", recorder.Code)
	}
	oldRecorder, _, _ := rawExecutionInspect(t, handler, credential, executionID)
	if oldRecorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the replaced credential still authenticates after renewal")
	}
}

// TestRenewalNeverOutlivesDeadline pins the persisted action deadline and the
// maximum TTL as upper bounds on every renewal.
func TestRenewalNeverOutlivesDeadline(t *testing.T) {
	t.Run("a renewal is capped at the action deadline", func(t *testing.T) {
		// The action deadline is later than the maximum TTL, so activation
		// commits the eight-hour cap and the renewal is capped by the deadline.
		shared := newRenewalShared(t, 30*time.Minute, 9*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "deadline-cap", 1)
		execution := id.ExecutionID(executionID)
		deadline := renewalActionDeadline(t, shared.controller(), executionID)

		before := renewalDurableCredential(t, shared.controller(), executionID)
		if want := shared.clock.Now().Add(token.DefaultBounds.MaxTTL); !before.Expiry.Equal(want) {
			t.Fatalf("activation credential expiry = %v; want the eight-hour cap %v", before.Expiry, want)
		}
		if !deadline.After(before.Expiry) {
			t.Fatalf("action deadline = %v; want it later than the activation expiry %v", deadline, before.Expiry)
		}

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.refreshAttemptsFor(execution) >= 1
		})
		time.Sleep(100 * time.Millisecond)
		attempts := shared.runtime.attemptsFor(execution)
		if len(attempts) != 1 {
			t.Fatalf("renewal attempts = %d; want exactly one", len(attempts))
		}
		renewed := attempts[0].request.Credential
		if renewed == credential {
			t.Fatal("renewal delivered the unchanged credential")
		}
		after := renewalDurableCredential(t, shared.controller(), executionID)
		if after.Expiry.After(deadline) {
			t.Fatalf("renewal expiry = %v; it outlived the action deadline %v", after.Expiry, deadline)
		}
		if !after.Expiry.Equal(deadline) {
			t.Fatalf("renewal expiry = %v; want the action deadline cap %v", after.Expiry, deadline)
		}
		recorder, _, _ := rawExecutionInspect(t, handler, renewed, executionID)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the deadline-capped renewal did not authenticate: status=%d", recorder.Code)
		}

		// Keep the execution running past its deadline so only the renewal loop
		// is under test, then prove nothing extends the credential again.
		freezeRenewalDispatch(t, app)
		shared.clock.Advance(2 * time.Hour)
		time.Sleep(100 * time.Millisecond)
		if got := shared.runtime.refreshAttemptsFor(execution); got != 1 {
			t.Fatalf("renewal ran %d times at/after the action deadline; want none", got-1)
		}
		past := renewalDurableCredential(t, shared.controller(), executionID)
		if past.Expiry.After(deadline) {
			t.Fatalf("a past-deadline renewal extended the credential to %v beyond %v", past.Expiry, deadline)
		}
	})

	t.Run("a credential already capped at the deadline is never rotated", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 2*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "deadline-same", 1)
		execution := id.ExecutionID(executionID)
		deadline := renewalActionDeadline(t, shared.controller(), executionID)
		before := renewalDurableCredential(t, shared.controller(), executionID)
		if !before.Expiry.Equal(deadline) {
			t.Fatalf("activation credential expiry = %v; want the action deadline %v", before.Expiry, deadline)
		}
		// Keep the execution running so only the renewal loop is under test.
		freezeRenewalDispatch(t, app)

		// Inside the renewal margin but before the deadline, a rotation could
		// only grant the same deadline expiry D: no nonce and no delivery may be
		// spent on a pointless D-to-D rotation.
		shared.clock.Advance(105 * time.Minute)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal rotated %d times although it could not extend the deadline-capped expiry", attempts)
		}
		after := renewalDurableCredential(t, shared.controller(), executionID)
		if !after.Expiry.Equal(before.Expiry) || !bytes.Equal(after.Nonce, before.Nonce) {
			t.Fatal("a pointless renewal replaced the deadline-capped credential")
		}
	})

	t.Run("a capped lifetime below the minimum is refused", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 2*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "deadline-min", 1)
		execution := id.ExecutionID(executionID)
		before := renewalDurableCredential(t, shared.controller(), executionID)
		freezeRenewalDispatch(t, app)

		// Ten seconds remain before the deadline, below the fifteen-second
		// minimum lifetime, so no token may be minted.
		shared.clock.Advance(2*time.Hour - 10*time.Second)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal minted %d credentials shorter than the minimum lifetime", attempts)
		}
		after := renewalDurableCredential(t, shared.controller(), executionID)
		if !after.Expiry.Equal(before.Expiry) || !bytes.Equal(after.Nonce, before.Nonce) {
			t.Fatal("a refused renewal still replaced the committed credential")
		}
	})

	t.Run("an execution past its deadline is never renewed", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 2*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "deadline-past", 1)
		execution := id.ExecutionID(executionID)
		before := renewalDurableCredential(t, shared.controller(), executionID)
		freezeRenewalDispatch(t, app)

		shared.clock.Advance(3 * time.Hour)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal ran %d times for an execution past its action deadline", attempts)
		}
		after := renewalDurableCredential(t, shared.controller(), executionID)
		if !after.Expiry.Equal(before.Expiry) || !bytes.Equal(after.Nonce, before.Nonce) {
			t.Fatal("a past-deadline renewal replaced the committed credential")
		}
	})
}

// TestRenewalExtendsAfterCappedDeadline pins that a credential reused by a
// later action with a later deadline is renewed instead of staying at the first
// action's deadline. Consecutive container actions reuse the committed pair, so
// the renewal must be able to extend it when the persisted deadline moves out.
func TestRenewalExtendsAfterCappedDeadline(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 2*time.Hour)
	app := shared.start(t)
	handler := app.Handler()
	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "deadline-extend", 1)
	execution := id.ExecutionID(executionID)
	// Stop the reconciler so the test owns the action transition and no
	// concurrent observation advances the row under the fixture.
	freezeRenewalDispatch(t, app)

	deadline := renewalActionDeadline(t, shared.controller(), executionID)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Expiry.Equal(deadline) {
		t.Fatalf("activation credential expiry = %v; want the action deadline %v", before.Expiry, deadline)
	}

	// Inside the margin but still capped at the current action deadline: no
	// rotation, so no receipt is committed for generation D.
	shared.clock.Advance(105 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("renewal rotated %d times while it could not extend the capped expiry", attempts)
	}
	if got := renewalDurableCredential(t, shared.controller(), executionID); !bytes.Equal(got.Nonce, before.Nonce) {
		t.Fatal("a pointless renewal replaced the capped credential")
	}

	// The next container action activates later, so the running action's
	// persisted deadline moves out. The committed credential is reused with
	// expiry D and must now be renewed to the new deadline.
	extended := deadline.Add(3 * time.Hour)
	renewalExtendActionDeadline(t, shared.controller(), executionID, extended)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.refreshAttemptsFor(execution) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || !attempts[0].delivered {
		t.Fatalf("renewal attempts = %d; want one delivered renewal after the deadline extension", len(attempts))
	}
	renewed := attempts[0].request.Credential
	if renewed == credential {
		t.Fatal("the deadline extension delivered the unchanged credential")
	}
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("the deadline extension did not replace the committed nonce")
	}
	wantExpiry := shared.clock.Now().Add(token.DefaultBounds.MaxTTL)
	if extended.Before(wantExpiry) {
		wantExpiry = extended
	}
	if !after.Expiry.Equal(wantExpiry) {
		t.Fatalf("renewed expiry = %v; want min(now+8h, new deadline) %v", after.Expiry, wantExpiry)
	}
	recorder, _, _ := rawExecutionInspect(t, handler, renewed, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the extended renewal did not authenticate: status=%d", recorder.Code)
	}

	// A later generation must get its own rotation rather than replaying the
	// receipt of the generation it replaced.
	shared.clock.Advance(extended.Sub(shared.clock.Now()) - 10*time.Minute)
	next := extended.Add(5 * time.Hour)
	renewalExtendActionDeadline(t, shared.controller(), executionID, next)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.refreshAttemptsFor(execution) >= 2
	})
	time.Sleep(100 * time.Millisecond)
	attempts = shared.runtime.attemptsFor(execution)
	if len(attempts) != 2 || !attempts[1].delivered {
		t.Fatalf("later-generation renewal attempts = %d; want a second delivered renewal", len(attempts))
	}
	second := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(second.Nonce, after.Nonce) {
		t.Fatal("a later-generation renewal replayed the previous generation's receipt")
	}
	if !second.Expiry.Equal(next) {
		t.Fatalf("later-generation expiry = %v; want the new deadline %v", second.Expiry, next)
	}
}

// TestRenewalStaleCappedReceiptDoesNotMaskExtension pins recovery from a
// receipt left by a pointless D-to-D rotation. The durable receipt is
// keyed on the committed generation, so an expiry-only stale key can never mask
// the later extension.
func TestRenewalStaleCappedReceiptDoesNotMaskExtension(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 2*time.Hour)
	app := shared.start(t)
	handler := app.Handler()
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "stale-receipt", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)

	deadline := renewalActionDeadline(t, shared.controller(), executionID)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Expiry.Equal(deadline) {
		t.Fatalf("activation credential expiry = %v; want the action deadline %v", before.Expiry, deadline)
	}
	shared.clock.Advance(105 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("renewal rotated %d times while it could not extend the capped expiry", attempts)
	}

	// Simulate stale durable state: a committed D-to-D rotation whose receipt
	// is keyed on expiry D alone.
	renewalCommitStaleCappedReceipt(t, shared, app, executionID, deadline)
	stale := renewalDurableCredential(t, shared.controller(), executionID)
	if !stale.Expiry.Equal(deadline) || bytes.Equal(stale.Nonce, before.Nonce) {
		t.Fatal("the fixture did not commit a same-expiry generation")
	}

	// The next action's deadline is later. The renewal must mint and deliver a
	// new generation instead of replaying the stale generation-D receipt.
	extended := deadline.Add(3 * time.Hour)
	renewalExtendActionDeadline(t, shared.controller(), executionID, extended)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.refreshAttemptsFor(execution) >= 2
	})
	time.Sleep(100 * time.Millisecond)
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 2 || !attempts[1].delivered {
		t.Fatalf("renewal attempts = %d; a stale capped-generation receipt masked the extension", len(attempts))
	}
	renewed := attempts[1].request.Credential
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, stale.Nonce) {
		t.Fatal("the stale capped-generation receipt masked the later renewal")
	}
	if !after.Expiry.Equal(extended) {
		t.Fatalf("renewed expiry = %v; want the extended deadline %v", after.Expiry, extended)
	}
	recorder, _, _ := rawExecutionInspect(t, handler, renewed, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the renewal after a stale receipt did not authenticate: status=%d", recorder.Code)
	}
}

// TestRenewalPendingDeliveryIsNotRotatedAgain pins the pending-skip guard: a
// committed-but-undelivered renewal is retried with the same token and never
// rotated again, even when the renewal margin is wider than the maximum
// lifetime and the clock has advanced enough for a second rotation to extend
// the committed expiry.
func TestRenewalPendingDeliveryIsNotRotatedAgain(t *testing.T) {
	// A margin wider than the eight-hour maximum keeps every freshly committed
	// expiry inside the margin. The test drives each scan explicitly with a long
	// configured interval so the background loop cannot race it.
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	config := shared.config(t)
	config.RenewInterval = time.Hour
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	// Let the background loop perform its initial empty scan and fall asleep.
	time.Sleep(100 * time.Millisecond)

	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "pending", 1)
	execution := id.ExecutionID(executionID)
	// The test drives the renewal passes explicitly, so stop the reconciler
	// from concurrently advancing the execution row version under the manual
	// rotate's compare-and-commit.
	freezeRenewalDispatch(t, app)
	before := renewalDurableCredential(t, shared.controller(), executionID)

	// The freshly committed expiry is inside the nine-hour margin, but a
	// rotation at the same clock instant cannot extend it.
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("initial renewal scan: %v", err)
	}
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("renewal rotated %d times although it could not extend the expiry", attempts)
	}

	// Advance so the committed expiry is extendable, then fail the delivery
	// twice: the commit succeeds and the token becomes pending.
	shared.clock.Advance(time.Hour)
	shared.runtime.failNext(execution, 2)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with a failed delivery: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || attempts[0].delivered {
		t.Fatalf("renewal attempts = %d; want one failed delivery", len(attempts))
	}
	pendingToken := attempts[0].request.Credential
	if pendingToken == credential {
		t.Fatal("the pending renewal delivered the unchanged credential")
	}
	pendingCredential := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(pendingCredential.Nonce, before.Nonce) {
		t.Fatal("the failed delivery did not commit a renewal")
	}

	// Advance again. The retry still fails, and the pending guard must prevent a
	// second rotation of the committed generation even though the clock now lets
	// such a rotation extend the expiry.
	shared.clock.Advance(time.Hour)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with a pending delivery: %v", err)
	}
	attempts = shared.runtime.attemptsFor(execution)
	if len(attempts) != 2 {
		t.Fatalf("delivery attempts = %d; a pending delivery was rotated again instead of retried", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.delivered {
			t.Fatal("an injected delivery failure reported success")
		}
		if attempt.request.Credential != pendingToken {
			t.Fatal("a pending delivery retry minted a second token")
		}
	}
	afterRetry := renewalDurableCredential(t, shared.controller(), executionID)
	if !bytes.Equal(afterRetry.Nonce, pendingCredential.Nonce) {
		t.Fatal("a pending delivery was rotated again instead of retried")
	}

	// Complete the retry through the same delivery path. Exactly one generation
	// was committed and exactly one distinct token was delivered.
	shared.runtime.clearFailure(execution)
	app.renewal.retryPending(context.Background(), shared.clock.Now())
	attempts = shared.runtime.attemptsFor(execution)
	if len(attempts) != 3 || !attempts[2].delivered {
		t.Fatalf("delivery attempts = %d; want two failures then one successful retry", len(attempts))
	}
	if attempts[2].request.Credential != pendingToken {
		t.Fatal("the successful retry delivered a different token")
	}
	if delivered := shared.runtime.deliveredFor(execution); delivered != 1 {
		t.Fatalf("delivered tokens = %d; want exactly one", delivered)
	}
	final := renewalDurableCredential(t, shared.controller(), executionID)
	if !bytes.Equal(final.Nonce, pendingCredential.Nonce) {
		t.Fatal("the retry committed another generation")
	}
	recorder, _, _ := rawExecutionInspect(t, handler, pendingToken, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the retried delivery did not authenticate: status=%d", recorder.Code)
	}
}

// TestRenewalPendingDeliveryDropConditions pins every drop condition of the
// pending-delivery retry. retryPending is the only path that delivers a
// committed-but-undelivered renewal after the fact, so it must re-read durable
// state and never deliver a token that no longer names the current generation,
// belongs to a stopped execution, or has already expired.
//
// Each subtest starts from the same committed renewal with its delivery failing,
// changes only durable state, then drives the next pass with delivery allowed
// to succeed. A guard that reads stale memory instead of durable state would
// deliver the pending token here.
func TestRenewalPendingDeliveryDropConditions(t *testing.T) {
	t.Run("cancelled execution", func(t *testing.T) {
		fixture := newRenewalPendingDelivery(t)
		// Cancellation clears the credential pair and stops the execution in
		// the same commit.
		cancelExecution(t, fixture.app.Handler(), fixture.executionID)
		waitForExecutionState(t, fixture.app.Handler(), "dev-project-token", fixture.executionID, "cancelled")
		if renewalDurableCredential(t, fixture.shared.controller(), fixture.executionID).Present() {
			t.Fatal("a cancelled execution kept a committed credential")
		}

		if err := fixture.app.renewal.scan(context.Background()); err != nil {
			t.Fatalf("renewal scan after cancellation: %v", err)
		}
		renewalAssertPendingDropped(t, fixture, 0)
	})

	t.Run("cleared credential", func(t *testing.T) {
		fixture := newRenewalPendingDelivery(t)
		// A request action's activation revokes the workload credential while
		// the execution stays running, the exact durable state written here.
		renewalClearCommittedCredential(t, fixture.shared.controller(), fixture.executionID)
		if renewalDurableCredential(t, fixture.shared.controller(), fixture.executionID).Present() {
			t.Fatal("the cleared-credential fixture kept a committed credential")
		}

		if err := fixture.app.renewal.scan(context.Background()); err != nil {
			t.Fatalf("renewal scan after the credential was cleared: %v", err)
		}
		renewalAssertPendingDropped(t, fixture, 0)
	})

	t.Run("replaced committed generation", func(t *testing.T) {
		fixture := newRenewalPendingDelivery(t)
		// A later commit replaces the generation the pending token was minted
		// for. Delivering the stale token would overwrite the workload's valid
		// credential.
		replacementToken := renewalRotateCommittedReplacement(t, fixture.shared, fixture.app, fixture.executionID)
		if _, ok := fixture.app.renewal.pending[fixture.execution]; !ok {
			t.Fatal("the replacement generation unexpectedly dropped the pending entry")
		}
		attempts := fixture.shared.runtime.attemptsFor(fixture.execution)
		if len(attempts) != 2 || !attempts[1].delivered || attempts[1].request.Credential != replacementToken {
			t.Fatalf("replacement fixture attempts = %d; want the failed pending delivery then one delivered replacement", len(attempts))
		}

		if err := fixture.app.renewal.scan(context.Background()); err != nil {
			t.Fatalf("renewal scan after the generation was replaced: %v", err)
		}
		// The replacement generation stays current and authenticates.
		renewalAssertPendingDropped(t, fixture, 1)
		recorder, _, _ := rawExecutionInspect(t, fixture.app.Handler(), replacementToken, fixture.executionID)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the replacement generation did not authenticate: status=%d", recorder.Code)
		}
	})

	t.Run("expired pending token", func(t *testing.T) {
		fixture := newRenewalPendingDelivery(t)
		// Advance past the pending token's committed expiry but keep the
		// action deadline far in the future, so only the token state makes the
		// delivery ineligible.
		committed := renewalDurableCredential(t, fixture.shared.controller(), fixture.executionID)
		deadline := renewalActionDeadline(t, fixture.shared.controller(), fixture.executionID)
		shared := fixture.shared
		shared.clock.Advance(committed.Expiry.Sub(shared.clock.Now()) + time.Minute)
		if !shared.clock.Now().Before(deadline) {
			t.Fatalf("the fixture advanced the clock past the action deadline %v", deadline)
		}
		if state := renewalDurableCredential(t, shared.controller(), fixture.executionID).State(shared.clock.Now()); state != token.StateExpired {
			t.Fatalf("committed credential state = %q; want expired", state)
		}

		if err := fixture.app.renewal.scan(context.Background()); err != nil {
			t.Fatalf("renewal scan after the pending token expired: %v", err)
		}
		// The dead pending token is never delivered. The same pass may mint a
		// fresh generation for the still-running execution.
		renewalAssertPendingDropped(t, fixture, 1)
	})

	t.Run("non-running execution retaining a credential", func(t *testing.T) {
		fixture := newRenewalPendingDelivery(t)
		// No production transition stops an execution while keeping its
		// credential: cancel, failure and success all invalidate the pair in
		// the same commit. This fixture writes the inconsistent durable row
		// directly to prove the enabled-principal guard fails closed if one ever
		// appears.
		renewalRetainCredentialWhileStopped(t, fixture.shared.controller(), fixture.executionID)
		retained := renewalDurableCredential(t, fixture.shared.controller(), fixture.executionID)
		if !retained.Present() || retained.State(fixture.shared.clock.Now()) != token.StateCurrent {
			t.Fatal("the retained-credential fixture did not leave a present current pair")
		}

		if err := fixture.app.renewal.scan(context.Background()); err != nil {
			t.Fatalf("renewal scan after the execution stopped: %v", err)
		}
		renewalAssertPendingDropped(t, fixture, 0)
	})
}

// renewalAssertPendingDropped asserts the pending entry was deleted, the
// pending token was never delivered again, and exactly wantDelivered other
// tokens were delivered by the pass.
func renewalAssertPendingDropped(t *testing.T, fixture *renewalPendingDelivery, wantDelivered int) {
	t.Helper()
	if _, ok := fixture.app.renewal.pending[fixture.execution]; ok {
		t.Fatal("the pending renewal delivery was not dropped")
	}
	for index, attempt := range fixture.shared.runtime.attemptsFor(fixture.execution) {
		if attempt.request.Credential != fixture.pendingToken {
			continue
		}
		if index != 0 || attempt.delivered {
			t.Fatalf("the stale pending token was delivered at attempt %d (delivered=%v)", index, attempt.delivered)
		}
	}
	if delivered := fixture.shared.runtime.deliveredFor(fixture.execution); delivered != wantDelivered {
		t.Fatalf("delivered tokens = %d; want %d", delivered, wantDelivered)
	}
}

// TestRenewalSkipsNonRunning pins that renewal never revives an execution that
// no longer holds a live workload credential.
func TestRenewalSkipsNonRunning(t *testing.T) {
	t.Run("terminal execution", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour, "inspect")
		app := shared.start(t)
		handler := app.Handler()
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "terminal", 1)
		execution := id.ExecutionID(executionID)
		waitForExecutionState(t, handler, "dev-project-token", executionID, "succeeded")
		if renewalDurableCredential(t, shared.controller(), executionID).Present() {
			t.Fatal("a succeeded execution kept a committed credential")
		}

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal ran %d times for a terminal execution", attempts)
		}
	})

	t.Run("cancelled execution", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "cancelled", 1)
		execution := id.ExecutionID(executionID)
		cancelExecution(t, handler, executionID)
		waitForExecutionState(t, handler, "dev-project-token", executionID, "cancelled")
		if renewalDurableCredential(t, shared.controller(), executionID).Present() {
			t.Fatal("a cancelled execution kept a committed credential")
		}

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal ran %d times for a cancelled execution", attempts)
		}
	})

	t.Run("a request wait revoked the credential while the execution runs", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		applied := applyProjectPipeline(t, handler, "dev-project-token", "project-1",
			renewalRequestPipeline(), "request-wait-pipeline")
		started := startRenewalTask(t, handler, "dev-project-token", "project-1",
			applied.PipelineID, "request-wait-task", map[string]string{"repository": "repo-1"})
		execution := id.ExecutionID(started.ExecutionID)

		// Request activation revokes the credential atomically while the
		// execution stays running.
		waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "running")
		if shared.runtime.prepareCount() != 0 {
			t.Fatalf("a request action prepared %d workloads; want none", shared.runtime.prepareCount())
		}
		if renewalDurableCredential(t, shared.controller(), started.ExecutionID).Present() {
			t.Fatal("request activation kept a committed credential")
		}

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
			t.Fatalf("renewal minted %d credentials during a request wait", attempts)
		}
		if renewalDurableCredential(t, shared.controller(), started.ExecutionID).Present() {
			t.Fatal("renewal minted a credential during a request wait")
		}
	})
}

// TestRenewalRetriesDeliveryWithoutRestart pins bounded delivery retry with the
// same committed token: no second mint, no restart.
func TestRenewalRetriesDeliveryWithoutRestart(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	app := shared.start(t)
	handler := app.Handler()
	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "retry", 1)
	execution := id.ExecutionID(executionID)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	prepares := shared.runtime.prepareCount()
	// The composition starts the workload asynchronously after the preparation
	// that issued its credential, so the initial Start may still be pending when
	// the helper returns. Wait for it before taking the baseline; renewal must
	// never add another start.
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.startCount() >= 1
	})
	starts := shared.runtime.startCount()

	shared.runtime.failNext(execution, 2)
	shared.clock.Advance(7*time.Hour + 45*time.Minute)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.deliveredFor(execution) >= 1
	})
	time.Sleep(100 * time.Millisecond)

	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 3 {
		t.Fatalf("delivery attempts = %d; want two injected failures then one success", len(attempts))
	}
	tokens := make(map[string]bool, len(attempts))
	for _, attempt := range attempts {
		if attempt.request.Identity.ExecutionID != execution {
			t.Fatalf("retry delivered execution = %q; want %q", attempt.request.Identity.ExecutionID, execution)
		}
		tokens[attempt.request.Credential] = true
	}
	if len(tokens) != 1 {
		t.Fatalf("delivery retries carried %d distinct tokens; a failed delivery must not re-mint", len(tokens))
	}
	delivered := attempts[len(attempts)-1].request.Credential
	if delivered == credential {
		t.Fatal("retry delivered the unchanged credential")
	}

	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("delivery retries minted more than one committed credential")
	}
	if shared.runtime.startCount() != starts {
		t.Fatalf("renewal restarted the workload (%d starts; want %d)", shared.runtime.startCount(), starts)
	}
	if shared.runtime.prepareCount() != prepares {
		t.Fatalf("renewal prepared the workload again (%d preparations; want %d)", shared.runtime.prepareCount(), prepares)
	}

	recorder, _, _ := rawExecutionInspect(t, handler, delivered, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the retried delivery did not authenticate: status=%d", recorder.Code)
	}
}

// TestRenewalSurvivesRestart pins that renewal is reconstructed from durable
// state rather than from in-memory state.
func TestRenewalSurvivesRestart(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	appA := startComposedApp(t, shared.config(t))
	t.Cleanup(func() { closeApp(t, appA) })
	handlerA := appA.Handler()
	grantProjectEngineer(t, handlerA, "dev-admin-token")
	executionID, credential := renewalStartContainer(t, shared, handlerA, "dev-project-token", "project-1", "restart", 1)
	execution := id.ExecutionID(executionID)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Present() {
		t.Fatal("activation committed no execution credential")
	}
	closeApp(t, appA)

	appB := startComposedApp(t, shared.config(t))
	t.Cleanup(func() { closeApp(t, appB) })

	// A credential that is not near expiry must not be renewed by the restarted
	// composition.
	time.Sleep(100 * time.Millisecond)
	if attempts := shared.runtime.refreshAttemptsFor(execution); attempts != 0 {
		t.Fatalf("the restarted composition renewed %d credentials that were not near expiry", attempts)
	}

	shared.clock.Advance(7*time.Hour + 45*time.Minute)
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.refreshAttemptsFor(execution) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 {
		t.Fatalf("renewal attempts after restart = %d; want exactly one", len(attempts))
	}
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("the restarted composition did not renew from durable state")
	}
	renewed := attempts[0].request.Credential
	if renewed == credential {
		t.Fatal("the restarted composition delivered the unchanged credential")
	}
	recorder, _, _ := rawExecutionInspect(t, appB.Handler(), renewed, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the post-restart renewal did not authenticate: status=%d", recorder.Code)
	}
}

// TestRenewalIsolation pins per-execution and per-scope isolation: one failing
// delivery never blocks another execution, and each renewal uses only the
// execution's own scope and tenant key.
func TestRenewalIsolation(t *testing.T) {
	t.Run("one failing delivery does not block another execution", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
		app := shared.start(t)
		handler := app.Handler()
		firstID, firstCredential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "isolation-first", 1)
		secondID, secondCredential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "isolation-second", 2)
		if firstID == secondID {
			t.Fatal("the fixture started only one execution")
		}

		// Fail delivery for the lexicographically first execution so a scan that
		// aborts on the first failure never reaches the other.
		failed, healthy := firstID, secondID
		failedCredential, healthyCredential := firstCredential, secondCredential
		if healthy < failed {
			failed, healthy = healthy, failed
			failedCredential, healthyCredential = healthyCredential, failedCredential
		}
		failedExecution := id.ExecutionID(failed)
		healthyExecution := id.ExecutionID(healthy)
		shared.runtime.failAlways(failedExecution)

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.deliveredFor(healthyExecution) >= 1
		})
		// The engine's first running observation commits concurrently with the
		// first renewal scan, so one execution's first rotate can lose that
		// compare-and-commit and be retried on the next cadence. Wait for the
		// failing execution's first attempt rather than assuming it happened in
		// the same pass as the healthy delivery; the isolation guarantee under
		// test is that it is attempted and never blocks the other execution.
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.refreshAttemptsFor(failedExecution) >= 1
		})
		healthyAttempts := shared.runtime.attemptsFor(healthyExecution)
		if len(healthyAttempts) != 1 || !healthyAttempts[0].delivered {
			t.Fatalf("the healthy execution attempts = %d; want one delivered renewal", len(healthyAttempts))
		}
		healthyRequest := healthyAttempts[0].request
		if healthyRequest.Identity.ExecutionID != healthyExecution {
			t.Fatalf("the healthy renewal delivered execution = %q; want %q", healthyRequest.Identity.ExecutionID, healthyExecution)
		}
		if healthyRequest.Credential == healthyCredential {
			t.Fatal("the healthy execution delivered the unchanged credential")
		}
		if healthyRequest.Credential == failedCredential {
			t.Fatal("the healthy execution delivered another execution's credential")
		}

		// The failing execution recovers on retry without a restart.
		shared.runtime.clearFailure(failedExecution)
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.deliveredFor(failedExecution) >= 1
		})
		failedAttempts := shared.runtime.attemptsFor(failedExecution)
		failedRequest := failedAttempts[len(failedAttempts)-1].request
		if failedRequest.Identity.ExecutionID != failedExecution {
			t.Fatalf("the recovered renewal delivered execution = %q; want %q", failedRequest.Identity.ExecutionID, failedExecution)
		}
		if failedRequest.Credential == healthyRequest.Credential {
			t.Fatal("two executions were delivered the same credential")
		}
		// Both original workloads start asynchronously after their preparations.
		// Wait for the two expected starts, then assert renewal added none.
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.startCount() >= 2
		})
		if shared.runtime.startCount() != 2 {
			t.Fatalf("renewal started %d workloads; want only the two original starts", shared.runtime.startCount())
		}

		// Each renewed credential authenticates only its own execution.
		own, _, _ := rawExecutionInspect(t, handler, healthyRequest.Credential, healthy)
		if own.Code != api.HTTPStatusSuccess {
			t.Fatalf("the renewed credential did not authenticate its own execution: status=%d", own.Code)
		}
		cross, _, _ := rawExecutionInspect(t, handler, healthyRequest.Credential, failed)
		if cross.Code == api.HTTPStatusSuccess {
			t.Fatal("a credential of one execution addressed another execution")
		}
	})

	t.Run("a second tenant renews under its own key and scope", func(t *testing.T) {
		shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
		shared.dev.Tokens = append(shared.dev.Tokens, renewalTenantTwoToken(shared.clock))
		// The composition is installation-wide: an execution in another tenant
		// must be renewed with that tenant's own derivation key and scope, not
		// skipped and not read through the first tenant.
		renewalSeedTenant(t, shared, Bootstrap{
			TenantID:       "tenant-2",
			TenantName:     "Tenant Two",
			ProjectID:      "project-2",
			ProjectName:    "Project Two",
			AdminPrincipal: "admin-2",
			AdminRoles:     []string{"admin", "engineer"},
		})
		app := shared.start(t)
		handler := app.Handler()
		executionID, credential := renewalStartContainer(t, shared, handler, "dev-tenant2-token", "project-2", "tenant-two", 1)
		execution := id.ExecutionID(executionID)
		before := renewalDurableCredential(t, shared.controller(), executionID)
		if !before.Present() {
			t.Fatal("the second tenant's activation committed no execution credential")
		}

		shared.clock.Advance(7*time.Hour + 45*time.Minute)
		eventually(t, 5*time.Second, func() bool {
			return shared.runtime.refreshAttemptsFor(execution) >= 1
		})
		time.Sleep(100 * time.Millisecond)
		attempts := shared.runtime.attemptsFor(execution)
		if len(attempts) != 1 || !attempts[0].delivered {
			t.Fatalf("second-tenant renewal attempts = %d; want one delivered renewal", len(attempts))
		}
		after := renewalDurableCredential(t, shared.controller(), executionID)
		if bytes.Equal(after.Nonce, before.Nonce) {
			t.Fatal("the second tenant's execution was not renewed")
		}
		renewed := attempts[0].request.Credential
		if renewed == credential {
			t.Fatal("the second tenant's renewal delivered the unchanged credential")
		}
		recorder, _, _ := rawExecutionInspect(t, handler, renewed, executionID)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the second tenant's renewed credential did not authenticate: status=%d", recorder.Code)
		}
	})
}

// TestRenewalSecretFree pins that credential material never reaches logs,
// events, results, failures or errors, including on the delivery-retry path.
func TestRenewalSecretFree(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	app := shared.startRenewalManual(t)
	handler := app.Handler()
	executionID, credential := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "secret-free", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)

	// The retry deliverer returns raw adapter text directly, so the retryPending
	// logFault branch is pinned at composition level too. It is installed on a
	// detached loop over the same composition dependencies; the started loop's
	// deliverer field is never rewritten while its goroutine may read it.
	loop := newRenewalDetachedLoop(t, app.renewal, func(config *renewalLoopConfig) {
		config.Deliverer = &renewalRawErrorDeliverer{inner: config.Deliverer, failures: 1}
	})

	// Fail both the post-commit Rotate delivery and the next retryPending
	// delivery. Both adapter errors embed the token they tried to deliver, so a
	// logFault that rendered err.Error() would leak that pending token.
	shared.runtime.failNext(execution, 2)
	shared.clock.Advance(7*time.Hour + 45*time.Minute)
	ctx := context.Background()
	// Pass one commits the rotation and fails its post-commit delivery. Pass two
	// fails the pending retry inside the raw-error deliverer. Pass three fails
	// the pending retry in the runtime. Pass four delivers the same committed
	// token successfully.
	for pass := range 4 {
		if err := loop.scan(ctx); err != nil {
			t.Fatalf("renewal scan %d: %v", pass, err)
		}
	}
	if delivered := shared.runtime.deliveredFor(execution); delivered != 1 {
		t.Fatalf("delivered tokens = %d; want exactly one successful retry", delivered)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) < 3 {
		t.Fatalf("renewal attempts = %d; want a failed Rotate delivery, a failed retry and a successful retry", len(attempts))
	}
	renewed := attempts[len(attempts)-1].request.Credential
	if renewed == "" || renewed == credential {
		t.Fatal("the fixture did not capture a renewed credential")
	}
	// Every attempted token is checked, including the committed-but-pending
	// token embedded in the two injected delivery errors.
	materials := []string{credential}
	for _, attempt := range attempts {
		materials = append(materials, attempt.request.Credential)
	}

	if containsMaterial(shared.logs.String(), materials...) {
		t.Fatal("composition logs contain credential material")
	}
	eventRecorder, eventResult, _ := restCall(t, handler, http.MethodGet,
		"/api/v1/projects/project-1/events", "dev-project-token", nil, nil, "")
	if eventRecorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("event.read status=%d", eventRecorder.Code)
	}
	if containsMaterial(string(eventResult.Body), materials...) {
		t.Fatal("the event read rendered credential material")
	}
	_, inspectResult, _ := rawExecutionInspect(t, handler, "dev-project-token", executionID)
	if containsMaterial(string(inspectResult.Body), materials...) {
		t.Fatal("execution.inspect rendered credential material")
	}
	inspected := inspectExecution(t, handler, "dev-project-token", executionID)
	if inspected.Failure != nil && containsMaterial(inspected.Failure.Detail, materials...) {
		t.Fatal("the execution failure detail rendered credential material")
	}

	// Positive control: the renewed token we checked for is real.
	recorder, _, _ := rawExecutionInspect(t, handler, renewed, executionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the renewed credential did not authenticate: status=%d", recorder.Code)
	}
}

// TestRenewalCancellation pins that cancellation stops the renewal loop and
// that App.Close joins it even while a delivery is in flight.
func TestRenewalCancellation(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	app := startComposedApp(t, shared.config(t))
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "cancel-loop", 1)
	execution := id.ExecutionID(executionID)

	reached, release := shared.runtime.blockRefresh()
	defer release()
	shared.clock.Advance(7*time.Hour + 45*time.Minute)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal never attempted a delivery")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- app.Close(context.Background())
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the renewal loop blocked in a delivery")
	}

	attempts := shared.runtime.refreshAttemptsFor(execution)
	shared.clock.Advance(24 * time.Hour)
	time.Sleep(100 * time.Millisecond)
	if got := shared.runtime.refreshAttemptsFor(execution); got != attempts {
		t.Fatalf("renewal ran %d times after Close", got-attempts)
	}
	eventually(t, 2*time.Second, func() bool {
		return goruntime.NumGoroutine() <= baseline
	})
}

// TestRenewalCancellationJoinsInFlightDelivery pins that App.Close joins the
// renewal goroutine instead of returning while a renewal delivery is still in
// flight. The delivery gate ignores cancellation on purpose: a test double that
// returned on ctx.Done would let an unjoined renewal goroutine exit on its own
// once Close cancels the context, so such a double cannot distinguish a joined
// loop from a leaked one. Here only the test's release ends the delivery, so
// Close must wait for the loop itself, and the join also keeps Close from
// stopping adapters while a renewal pass is still inside Rotate/Deliver.
func TestRenewalCancellationJoinsInFlightDelivery(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	app := startComposedApp(t, shared.config(t))
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "cancel-join", 1)
	execution := id.ExecutionID(executionID)
	// Wait for the workload start so Close has a tracked adapter stop to defer;
	// this makes the adapter-ordering assertion below non-vacuous.
	eventually(t, 5*time.Second, func() bool {
		return shared.runtime.startCount() >= 1
	})

	reached, release := shared.runtime.blockRefreshUncancellable()
	defer release()
	shared.clock.Advance(7*time.Hour + 45*time.Minute)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal never attempted a delivery")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- app.Close(context.Background())
	}()

	// A joined Close cannot return while the delivery is blocked. Poll a
	// bounded window rather than sleeping a fixed time: the unjoined mutation
	// finishes Close in milliseconds, so the window only needs to outlast the
	// cancellation and teardown work, and it costs nothing but a short wait on
	// the correct implementation.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while a renewal delivery was still in flight; the renewal loop is not joined", err)
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Close joins the loops before it stops adapters, so an in-flight renewal
	// delivery must mean no workload has been stopped yet.
	if stopped := shared.runtime.stopCount(); stopped != 0 {
		t.Fatalf("Close stopped %d workload(s) while a renewal delivery was still in flight", stopped)
	}

	before := shared.runtime.refreshAttemptsFor(execution)
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the in-flight delivery was released")
	}
	after := shared.runtime.refreshAttemptsFor(execution)
	if after != before+1 {
		t.Fatalf("renewal attempts after the release = %d; want the one in-flight delivery (%d)", after, before+1)
	}

	// The joined loop is gone: a still-due credential and an advanced clock
	// cannot start another pass after Close.
	shared.clock.Advance(24 * time.Hour)
	time.Sleep(100 * time.Millisecond)
	if got := shared.runtime.refreshAttemptsFor(execution); got != after {
		t.Fatalf("renewal ran %d times after Close", got-after)
	}
	// The adapter stop that was deferred behind the join still ran.
	if stopped := shared.runtime.stopCount(); stopped == 0 {
		t.Fatal("Close never stopped the running workload")
	}
	eventually(t, 2*time.Second, func() bool {
		return goruntime.NumGoroutine() <= baseline
	})
}

// TestRenewalDeadlineIsPerExecution pins that the action-deadline read is
// scoped to the execution being renewed. Two running executions with different
// action deadlines are renewed in the same pass; each renewal must be capped by
// its own deadline and never by the other execution's.
func TestRenewalDeadlineIsPerExecution(t *testing.T) {
	cases := []struct {
		name           string
		firstDeadline  time.Duration
		secondDeadline time.Duration
	}{
		{"the first execution has the earlier deadline", 8*time.Hour + 30*time.Minute, 11 * time.Hour},
		{"the second execution has the earlier deadline", 11 * time.Hour, 8*time.Hour + 30*time.Minute},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
			app := shared.startRenewalManual(t)
			handler := app.Handler()
			firstID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "per-deadline-first", 1)
			secondID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "per-deadline-second", 2)
			freezeRenewalDispatch(t, app)

			start := shared.clock.Now()
			firstDeadline := start.Add(testCase.firstDeadline)
			secondDeadline := start.Add(testCase.secondDeadline)
			renewalExtendActionDeadline(t, shared.controller(), firstID, firstDeadline)
			renewalExtendActionDeadline(t, shared.controller(), secondID, secondDeadline)

			// One hour later both committed activation expiries are inside the
			// renewal margin and each deadline can extend its own credential.
			shared.clock.Advance(time.Hour)
			if err := app.renewal.scan(context.Background()); err != nil {
				t.Fatalf("renewal scan: %v", err)
			}
			now := shared.clock.Now()
			for _, execution := range []struct {
				id       string
				deadline time.Time
			}{
				{firstID, firstDeadline},
				{secondID, secondDeadline},
			} {
				identity := id.ExecutionID(execution.id)
				attempts := shared.runtime.attemptsFor(identity)
				if len(attempts) != 1 || !attempts[0].delivered {
					t.Fatalf("execution %q attempts = %d; want one delivered renewal", execution.id, len(attempts))
				}
				want := now.Add(token.DefaultBounds.MaxTTL)
				if execution.deadline.Before(want) {
					want = execution.deadline
				}
				got := renewalDurableCredential(t, shared.controller(), execution.id)
				if !got.Expiry.Equal(want) {
					t.Fatalf("execution %q renewed expiry = %v; want min(now+8h, its own deadline) %v", execution.id, got.Expiry, want)
				}
				if got.Expiry.After(execution.deadline) {
					t.Fatalf("execution %q renewed expiry %v outlived its own deadline %v", execution.id, got.Expiry, execution.deadline)
				}
			}
			firstExpiry := renewalDurableCredential(t, shared.controller(), firstID).Expiry
			secondExpiry := renewalDurableCredential(t, shared.controller(), secondID).Expiry
			if firstExpiry.Equal(secondExpiry) {
				t.Fatalf("both executions renewed to the same expiry %v; a cross-execution deadline capped one of them", firstExpiry)
			}
		})
	}
}

// TestRenewalDeadlineUsesTheRunningAction pins that the action-deadline read is
// restricted to the running action. A completed earlier action with a past
// deadline must not stop or cap the renewal of the later running action.
func TestRenewalDeadlineUsesTheRunningAction(t *testing.T) {
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	app := shared.startRenewalManual(t)
	handler := app.Handler()
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "completed-earlier", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)

	start := shared.clock.Now()
	runningDeadline := start.Add(8*time.Hour + 30*time.Minute)
	renewalExtendActionDeadline(t, shared.controller(), executionID, runningDeadline)
	completedActionID := renewalInsertCompletedAction(t, shared.controller(), executionID, start.Add(-time.Hour))

	shared.clock.Advance(time.Hour)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || !attempts[0].delivered {
		t.Fatalf("renewal attempts = %d; want one delivered renewal despite the completed earlier action", len(attempts))
	}
	got := renewalDurableCredential(t, shared.controller(), executionID)
	if !got.Expiry.Equal(runningDeadline) {
		t.Fatalf("renewed expiry = %v; want the running action deadline %v", got.Expiry, runningDeadline)
	}
	completed, err := shared.controller().Read(context.Background(), "actions", completedActionID)
	if err != nil {
		t.Fatalf("read the completed earlier action: %v", err)
	}
	if completed.Values["state"] != "succeeded" {
		t.Fatalf("completed earlier action state = %v; want succeeded", completed.Values["state"])
	}
}

// TestRenewalSkipsWithoutARunningAction pins that a running execution holding a
// live committed credential inside the margin is not renewed when it has no
// running action. That is the normal between-action state while dispatch is
// paused: there is no persisted action deadline to bound the new credential, so
// a renewal would rotate to an unbounded deadline. The fixture proves the
// credential is otherwise renewable by restoring the running action afterwards.
func TestRenewalSkipsWithoutARunningAction(t *testing.T) {
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	app := shared.startRenewalManual(t)
	handler := app.Handler()
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "no-running-action", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)

	deadline := renewalActionDeadline(t, shared.controller(), executionID)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Present() {
		t.Fatal("activation committed no execution credential")
	}
	receiptsBefore := renewalRenewalReceipts(t, shared, executionID)
	// Move the only running action to a completed state, exactly the durable
	// shape between two consecutive actions while dispatch is paused.
	actionID := renewalSetRunningActionState(t, shared.controller(), executionID, "succeeded")
	if !shared.clock.Now().Add(shared.renewBefore).After(before.Expiry) {
		t.Fatal("the fixture credential is outside the renewal margin")
	}

	shared.clock.Advance(time.Hour)
	if !shared.clock.Now().Before(deadline) {
		t.Fatalf("the fixture advanced the clock past the action deadline %v", deadline)
	}
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan without a running action: %v", err)
	}
	if attempts := shared.runtime.attemptsFor(execution); len(attempts) != 0 {
		t.Fatalf("renewal attempted %d deliveries without a running action", len(attempts))
	}
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if !bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("a renewal without a running action replaced the committed nonce")
	}
	if !after.Expiry.Equal(before.Expiry) {
		t.Fatalf("a renewal without a running action changed the committed expiry %v -> %v", before.Expiry, after.Expiry)
	}
	if got := renewalRenewalReceipts(t, shared, executionID); got != receiptsBefore {
		t.Fatalf("a renewal without a running action committed %d receipts; want %d", got-receiptsBefore, 0)
	}

	// Positive control: the same credential is renewable once its running action
	// (and so its action deadline) is visible again.
	renewalSetActionState(t, shared.controller(), executionID, actionID, "running")
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with the running action restored: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || !attempts[0].delivered {
		t.Fatalf("renewal attempts after restoring the running action = %d; want one delivered renewal", len(attempts))
	}
}

// TestRenewalScanPaginatesAllRunningExecutions pins the cross-scope execution
// page loop: the cursor must advance and every running execution must be
// renewed in one pass even when a page holds only one row.
func TestRenewalScanPaginatesAllRunningExecutions(t *testing.T) {
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	app := shared.startRenewalManual(t)
	handler := app.Handler()
	executions := make([]id.ExecutionID, 0, 3)
	for index := range 3 {
		executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1",
			"pagination-"+strconv.Itoa(index), index+1)
		executions = append(executions, id.ExecutionID(executionID))
	}
	freezeRenewalDispatch(t, app)
	// Clamp only the executions page. The production loop must follow the
	// returned cursor across three one-row pages in this single pass. The
	// clamped controller belongs to a detached loop: the started loop's
	// controller field is never rewritten while its goroutine may read it.
	loop := newRenewalDetachedLoop(t, app.renewal, func(config *renewalLoopConfig) {
		config.Controller = renewalPagedController{Controller: shared.store.Controller(), executionsLimit: 1}
	})

	shared.clock.Advance(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.scan(ctx); err != nil {
		t.Fatalf("renewal scan over three one-row pages: %v", err)
	}
	now := shared.clock.Now()
	for _, execution := range executions {
		attempts := shared.runtime.attemptsFor(execution)
		if len(attempts) != 1 || !attempts[0].delivered {
			t.Fatalf("execution %q attempts = %d; want one delivered renewal in the single scan pass", execution, len(attempts))
		}
		got := renewalDurableCredential(t, shared.controller(), string(execution))
		want := now.Add(token.DefaultBounds.MaxTTL)
		if !got.Expiry.Equal(want) {
			t.Fatalf("execution %q renewed expiry = %v; want %v", execution, got.Expiry, want)
		}
	}
}

// TestRenewalScanTerminatesOnInconsistentPagePairing pins both termination
// checks of the page loop. The page contract pairs HasMore with a
// non-empty Next, so each check alone is redundant on a conforming adapter; the
// loop still refuses to spin if an adapter ever violates that pairing.
func TestRenewalScanTerminatesOnInconsistentPagePairing(t *testing.T) {
	cases := []struct {
		name    string
		hasMore bool
		next    string
	}{
		{"more rows without a cursor", true, ""},
		{"a cursor without more rows", false, "cursor"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
			app := shared.startRenewalManual(t)
			controller := newRenewalInconsistentPageController(shared.store.Controller(), testCase.hasMore, testCase.next)
			// A detached loop carries the inconsistent controller. The started
			// loop is never mutated while its goroutine may read the field.
			loop := newRenewalDetachedLoop(t, app.renewal, func(config *renewalLoopConfig) {
				config.Controller = controller
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := loop.scan(ctx); err != nil {
				t.Fatalf("renewal scan did not terminate on an inconsistent page pairing: %v", err)
			}
			if controller.scans != 1 {
				t.Fatalf("renewal scanned %d pages; want one before terminating", controller.scans)
			}
		})
	}
}

// TestRenewalRotateVersionConflictLeavesCredentialAndRetries pins the failed
// Rotate commit that carries no Delivery. A concurrent writer that wins between
// the renewal scan and the compare-and-commit makes the commit a version
// conflict: the credential must stay untouched, nothing is delivered, no pending
// retry is remembered, the loop must not crash, and the next pass renews
// normally. Treating a nil Delivery as a post-commit delivery failure would
// dereference it and panic the loop goroutine.
func TestRenewalRotateVersionConflictLeavesCredentialAndRetries(t *testing.T) {
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	logs := &lockedLogBuffer{}
	app := shared.startRenewalManualConfigured(t, func(config *Config) {
		config.Adapters.Controller = newRenewalVersionBumpController(shared.store.Controller())
		config.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	handler := app.Handler()
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "rotate-conflict", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Present() {
		t.Fatal("activation committed no execution credential")
	}
	receiptsBefore := renewalRenewalReceipts(t, shared, executionID)

	// Advance into the renewal margin. The controller wrapper bumps the row
	// version after the executions page is read and before Rotate compares it,
	// so this pass loses the compare-and-commit deterministically.
	shared.clock.Advance(time.Hour)
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan with a lost compare-and-commit: %v", err)
	}
	if attempts := shared.runtime.attemptsFor(execution); len(attempts) != 0 {
		t.Fatalf("a failed Rotate commit delivered %d tokens; want none", len(attempts))
	}
	if _, remembered := app.renewal.pending[execution]; remembered {
		t.Fatal("a failed Rotate commit remembered a pending delivery")
	}
	after := renewalDurableCredential(t, shared.controller(), executionID)
	if !bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("a failed Rotate commit replaced the committed nonce")
	}
	if !after.Expiry.Equal(before.Expiry) {
		t.Fatalf("a failed Rotate commit changed the committed expiry %v -> %v", before.Expiry, after.Expiry)
	}
	if got := renewalRenewalReceipts(t, shared, executionID); got != receiptsBefore {
		t.Fatalf("a failed Rotate commit committed %d receipts; want %d", got-receiptsBefore, 0)
	}
	// The failed commit is reported by fault class, never by adapter text.
	if rendered := logs.String(); !strings.Contains(rendered, "credential renewal failed") ||
		!strings.Contains(rendered, string(fault.VersionConflict)) {
		t.Fatalf("the failed Rotate commit was not logged by fault class: %q", rendered)
	}

	// The next pass reads the advanced version and renews normally.
	if err := app.renewal.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan after the lost compare-and-commit: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || !attempts[0].delivered {
		t.Fatalf("renewal attempts after the lost compare-and-commit = %d; want one delivered renewal", len(attempts))
	}
	if _, remembered := app.renewal.pending[execution]; remembered {
		t.Fatal("the successful renewal left a pending delivery")
	}
	// The committed receipt records the version the rotation produced.
	row, err := shared.controller().Read(context.Background(), "executions", executionID)
	if err != nil {
		t.Fatalf("read the renewed execution: %v", err)
	}
	version, err := rowUint64(row, "version")
	if err != nil {
		t.Fatalf("read the renewed execution version: %v", err)
	}
	result := renewalRenewalReceiptResult(t, shared, executionID)
	if result.Version != version || result.State != token.StateCurrent {
		t.Fatalf("renewal receipt result = {state:%q version:%d}; want the committed {state:%q version:%d}",
			result.State, result.Version, token.StateCurrent, version)
	}
}

// TestRenewalScanRefusesAZeroClock pins that a scan never derives renewal
// eligibility from an unusable clock.
func TestRenewalScanRefusesAZeroClock(t *testing.T) {
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	app := shared.startRenewalManual(t)
	loop := newRenewalDetachedLoop(t, app.renewal, func(config *renewalLoopConfig) {
		config.Clock = renewalZeroClock{}
	})
	err := loop.scan(context.Background())
	if err == nil {
		t.Fatal("a renewal scan accepted a zero clock")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
		t.Fatalf("zero-clock refusal class = %v; want internal_failure", err)
	}
}

// TestRenewalRememberPendingFailsClosedWhenTheCredentialVanished pins that a
// committed-but-undelivered token is not remembered when the durable credential
// is no longer present by the time the pending fact is read. The credential
// reader returns a present pair for the Rotate read and an absent pair for the
// pending read, the durable state a concurrent clear leaves behind.
func TestRenewalRememberPendingFailsClosedWhenTheCredentialVanished(t *testing.T) {
	shared := newRenewalShared(t, 9*time.Hour, 24*time.Hour)
	app := shared.startRenewalManual(t)
	handler := app.Handler()
	executionID, _ := renewalStartContainer(t, shared, handler, "dev-project-token", "project-1", "vanish-before-remember", 1)
	execution := id.ExecutionID(executionID)
	freezeRenewalDispatch(t, app)
	before := renewalDurableCredential(t, shared.controller(), executionID)
	if !before.Present() {
		t.Fatal("activation committed no execution credential")
	}
	// rememberPending is the loop's only credential read on this path; the
	// manager reads through its own adapter.
	reader := &renewalVanishingCredentialReader{inner: app.renewal.credentials, target: execution, vanishOn: 1}
	loop := newRenewalDetachedLoop(t, app.renewal, func(config *renewalLoopConfig) {
		config.Credentials = reader
	})

	// Fail the post-commit delivery so rememberPending runs.
	shared.runtime.failNext(execution, 1)
	shared.clock.Advance(time.Hour)
	if err := loop.scan(context.Background()); err != nil {
		t.Fatalf("renewal scan: %v", err)
	}
	attempts := shared.runtime.attemptsFor(execution)
	if len(attempts) != 1 || attempts[0].delivered {
		t.Fatalf("renewal attempts = %d; want one failed delivery", len(attempts))
	}
	if after := renewalDurableCredential(t, shared.controller(), executionID); bytes.Equal(after.Nonce, before.Nonce) {
		t.Fatal("the fixture did not commit a rotation")
	}
	if _, remembered := loop.pending[execution]; remembered {
		t.Fatal("a pending delivery was remembered although the committed credential vanished")
	}
}

// TestRenewalPendingRetryIsolatesExecutions pins that retryPending continues
// past one execution's failed delivery. Two executions hold simultaneous
// pending renewals; one delivery always fails and the other must succeed in
// every pass regardless of randomized map iteration order.
func TestRenewalPendingRetryIsolatesExecutions(t *testing.T) {
	failed, healthy := newRenewalPendingPair(t)
	if failed.execution == healthy.execution {
		t.Fatal("the pair fixture produced one execution")
	}
	shared := failed.shared
	shared.runtime.failAlways(failed.execution)
	shared.runtime.clearFailure(healthy.execution)
	failedPending, ok := failed.app.renewal.pending[failed.execution]
	if !ok {
		t.Fatal("the failing execution has no pending renewal")
	}
	healthyPending, ok := healthy.app.renewal.pending[healthy.execution]
	if !ok {
		t.Fatal("the healthy execution has no pending renewal")
	}

	// Map iteration order is randomized for every range. A retry loop that
	// returned after the failing execution's error would deliver the healthy
	// token only in passes where the healthy entry happened to be visited
	// first. Reseed both entries and require the healthy token in every pass.
	// The reseed target is a detached loop: the started loop's pending map is
	// never rewritten while its goroutine may read it.
	loop := newRenewalDetachedLoop(t, failed.app.renewal, nil)
	const passes = 256
	for pass := range passes {
		loop.pending = map[id.ExecutionID]pendingRenewal{
			failed.execution:  failedPending,
			healthy.execution: healthyPending,
		}
		before := shared.runtime.deliveredFor(healthy.execution)
		loop.retryPending(context.Background(), shared.clock.Now())
		if got := shared.runtime.deliveredFor(healthy.execution); got != before+1 {
			t.Fatalf("pass %d delivered %d healthy tokens; a failed retry for another execution stopped the pass", pass, got-before)
		}
	}
	if delivered := shared.runtime.deliveredFor(failed.execution); delivered != 0 {
		t.Fatalf("the failing execution delivered %d tokens; want none", delivered)
	}
}

// TestRenewalPendingRetrySurvivesCredentialReadFailure pins both branches of a
// pending entry whose credential cannot be read: the entry is kept and the pass
// continues to the other entries, then the same entry is delivered once the
// durable reader recovers. A retry loop that returned on the read error would
// starve every later entry; one that deleted the entry would lose the committed
// token permanently.
func TestRenewalPendingRetrySurvivesCredentialReadFailure(t *testing.T) {
	failed, healthy := newRenewalPendingPair(t)
	if failed.execution == healthy.execution {
		t.Fatal("the pair fixture produced one execution")
	}
	shared := failed.shared
	failedPending, ok := failed.app.renewal.pending[failed.execution]
	if !ok {
		t.Fatal("the unreadable execution has no pending renewal")
	}
	healthyPending, ok := healthy.app.renewal.pending[healthy.execution]
	if !ok {
		t.Fatal("the healthy execution has no pending renewal")
	}
	reader := &renewalFailingCredentialReader{inner: failed.app.renewal.credentials, fail: failed.execution}
	loop := newRenewalDetachedLoop(t, failed.app.renewal, func(config *renewalLoopConfig) {
		config.Credentials = reader
	})

	// Map iteration order is randomized for every range. Reseed both entries and
	// require the healthy token in every pass: a retry loop that returned on the
	// unreadable execution's error would deliver it only when the healthy entry
	// happened to be visited first.
	const passes = 256
	for pass := range passes {
		loop.pending = map[id.ExecutionID]pendingRenewal{
			failed.execution:  failedPending,
			healthy.execution: healthyPending,
		}
		before := shared.runtime.deliveredFor(healthy.execution)
		loop.retryPending(context.Background(), shared.clock.Now())
		if got := shared.runtime.deliveredFor(healthy.execution); got != before+1 {
			t.Fatalf("pass %d delivered %d healthy tokens; an unreadable credential stopped the pass", pass, got-before)
		}
		if _, retained := loop.pending[failed.execution]; !retained {
			t.Fatalf("pass %d deleted the pending entry whose credential could not be read", pass)
		}
	}
	if delivered := shared.runtime.deliveredFor(failed.execution); delivered != 0 {
		t.Fatalf("the unreadable execution delivered %d tokens; want none while its credential is unreadable", delivered)
	}

	// Once the reader recovers, the retained entry is delivered exactly once and
	// the pass drains both entries.
	reader.fail = ""
	loop.pending = map[id.ExecutionID]pendingRenewal{
		failed.execution:  failedPending,
		healthy.execution: healthyPending,
	}
	loop.retryPending(context.Background(), shared.clock.Now())
	if delivered := shared.runtime.deliveredFor(failed.execution); delivered != 1 {
		t.Fatalf("the recovered execution delivered %d tokens; want exactly one", delivered)
	}
	if len(loop.pending) != 0 {
		t.Fatalf("the retry pass left %d pending entries after both deliveries succeeded", len(loop.pending))
	}

	// A failed retry delivery is logged by fault class and keeps the entry for a
	// later pass. The detached loop has no owning goroutine, so replacing its
	// logger cannot race a scan.
	logs := &lockedLogBuffer{}
	loop.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	shared.runtime.failAlways(healthy.execution)
	loop.pending = map[id.ExecutionID]pendingRenewal{healthy.execution: healthyPending}
	loop.retryPending(context.Background(), shared.clock.Now())
	if rendered := logs.String(); !strings.Contains(rendered, "credential renewal failed") {
		t.Fatalf("a failed retry delivery was not logged by fault class: %q", rendered)
	} else if containsMaterial(rendered, healthyPending.delivery.Token) {
		t.Fatal("the retry delivery failure log rendered the token")
	}
	if _, retained := loop.pending[healthy.execution]; !retained {
		t.Fatal("a failed retry delivery dropped the pending entry")
	}
}

// TestRenewalPendingRetryContinuesAfterStaleDrop pins that dropping one stale
// pending entry does not end the pass. A retry loop that returned after the
// delete would leave every later entry undelivered whenever the stale entry was
// visited first.
func TestRenewalPendingRetryContinuesAfterStaleDrop(t *testing.T) {
	stale, healthy := newRenewalPendingPair(t)
	if stale.execution == healthy.execution {
		t.Fatal("the pair fixture produced one execution")
	}
	shared := stale.shared
	// Clearing the committed credential makes the pending token unable to
	// authenticate, so the retry must drop it and continue.
	renewalClearCommittedCredential(t, shared.controller(), stale.executionID)
	stalePending, ok := stale.app.renewal.pending[stale.execution]
	if !ok {
		t.Fatal("the stale execution has no pending renewal")
	}
	healthyPending, ok := healthy.app.renewal.pending[healthy.execution]
	if !ok {
		t.Fatal("the healthy execution has no pending renewal")
	}
	loop := newRenewalDetachedLoop(t, stale.app.renewal, nil)

	// Map iteration order is randomized for every range. Reseed both entries and
	// require the healthy token in every pass.
	const passes = 256
	for pass := range passes {
		loop.pending = map[id.ExecutionID]pendingRenewal{
			stale.execution:   stalePending,
			healthy.execution: healthyPending,
		}
		before := shared.runtime.deliveredFor(healthy.execution)
		loop.retryPending(context.Background(), shared.clock.Now())
		if got := shared.runtime.deliveredFor(healthy.execution); got != before+1 {
			t.Fatalf("pass %d delivered %d healthy tokens; dropping a stale entry stopped the pass", pass, got-before)
		}
		if _, retained := loop.pending[stale.execution]; retained {
			t.Fatalf("pass %d kept a pending entry whose committed credential was cleared", pass)
		}
	}
	if delivered := shared.runtime.deliveredFor(stale.execution); delivered != 0 {
		t.Fatalf("the stale execution delivered %d tokens; want none", delivered)
	}
}

// TestRenewalLogFaultRendersOnlyTheFaultClass pins the logFault invariant for
// both branches: adapter error text is never rendered, only the fault class.
func TestRenewalLogFaultRendersOnlyTheFaultClass(t *testing.T) {
	var logs bytes.Buffer
	loop := &renewalLoop{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	const marker = "renewal-log-secret-marker-2f7d"
	raw := errors.New("adapter detail " + marker)
	classified := fault.New(fault.DependencyUnavailable, "adapter detail "+marker)
	loop.logFault("", raw)
	loop.logFault("execution-log-fault", classified)
	if strings.Contains(logs.String(), marker) {
		t.Fatal("logFault rendered adapter error text instead of only the fault class")
	}
}

// TestRenewalScanFailureLogIsSecretFree pins that a scan failure carrying
// adapter text in composition logs renders only the fault class.
func TestRenewalScanFailureLogIsSecretFree(t *testing.T) {
	const marker = "renewal-scan-secret-marker-91ab"
	shared := newRenewalShared(t, 30*time.Minute, 24*time.Hour)
	config := shared.config(t)
	config.RenewInterval = renewalInterval
	config.Adapters.Controller = renewalFailingScanController{
		Controller: shared.store.Controller(),
		marker:     marker,
	}
	logs := &lockedLogBuffer{}
	config.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	eventually(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "credential renewal scan failed")
	})
	if strings.Contains(logs.String(), marker) {
		t.Fatal("the renewal scan failure log rendered adapter error text")
	}
}

// TestRenewalWiring pins which compositions run the loop and the renewal-margin
// configuration boundary.
func TestRenewalWiring(t *testing.T) {
	t.Run("the test profile starts the renewal loop", func(t *testing.T) {
		config := fullTestProfileConfig(t)
		runtime := newRenewalRuntime()
		config.Adapters.Runtime = runtime
		config.Adapters.Workspace = runtime
		config.DefaultActionTimeout = 24 * time.Hour
		config.RenewBefore = 30 * time.Minute
		config.RenewInterval = renewalInterval
		clock, ok := config.Adapters.Clock.(*fakeClock)
		if !ok {
			t.Fatalf("test-profile clock has type %T; want *fakeClock", config.Adapters.Clock)
		}

		app, err := New(context.Background(), config)
		if err != nil {
			t.Fatalf("New ProfileTest: %v", err)
		}
		if err := app.start(context.Background()); err != nil {
			t.Fatalf("start ProfileTest: %v", err)
		}
		t.Cleanup(func() { closeApp(t, app) })
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyProjectPipeline(t, handler, "dev-project-token", "project-1",
			activationPipeline(activationAction{name: "inspect"}), "test-profile-renewal-pipeline")
		started := startRenewalTask(t, handler, "dev-project-token", "project-1",
			applied.PipelineID, "test-profile-renewal-task", nil)
		prepares := waitForActivationPrepares(t, runtime.recordingRuntime, 1)
		credential := ""
		for _, prepare := range prepares {
			if prepare.Identity.ExecutionID == id.ExecutionID(started.ExecutionID) {
				credential = prepare.Credential
			}
		}
		if credential == "" {
			t.Fatal("the test-profile execution received no credential")
		}

		clock.Advance(7*time.Hour + 45*time.Minute)
		execution := id.ExecutionID(started.ExecutionID)
		eventually(t, 5*time.Second, func() bool {
			return runtime.refreshAttemptsFor(execution) >= 1
		})
		attempts := runtime.attemptsFor(execution)
		if !attempts[0].delivered || attempts[0].request.Credential == credential {
			t.Fatal("the test profile did not deliver a renewed credential")
		}
		recorder, _, _ := rawExecutionInspect(t, handler, attempts[0].request.Credential, started.ExecutionID)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the test-profile renewal did not authenticate: status=%d", recorder.Code)
		}
	})

	t.Run("a composition with only static development tokens never refreshes", func(t *testing.T) {
		shared := newRenewalShared(t, 0, 24*time.Hour)
		app := shared.start(t)
		handler := app.Handler()

		shared.clock.Advance(12 * time.Hour)
		time.Sleep(100 * time.Millisecond)
		if attempts := shared.runtime.totalAttempts(); attempts != 0 {
			t.Fatalf("a composition with no workload credential refreshed %d times", attempts)
		}
		recorder, _, publicError := restCall(t, handler, http.MethodGet,
			"/api/v1/projects/project-1/events", "dev-project-token", nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("static development authentication regressed: status=%d error=%+v", recorder.Code, publicError)
		}
	})

	t.Run("an unusable renewal margin is refused", func(t *testing.T) {
		development := newTestEnvironment(t).config
		development.RenewBefore = -time.Minute
		err := development.resolved().validate()
		if err == nil {
			t.Fatal("the development composition accepted a negative renewal margin")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("negative renewal margin refusal class = %v; want invalid_input", err)
		}

		production := validProductionConfig(t)
		production.RenewBefore = -time.Minute
		err = production.resolved().validate()
		if err == nil {
			t.Fatal("the production composition accepted a negative renewal margin")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("production negative renewal margin refusal class = %v; want invalid_input", err)
		}
	})
}
