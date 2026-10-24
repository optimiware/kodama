package composition_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// Test_cancel_grant_atomicity: SD-10, SD-11, PX-07, PX-10, PX-12, IT-03, IT-06,
// WS-11 (acceptance `cancel_grant_atomicity`). Direct, item-scoped, caller-owned
// transaction, consumer-bound (workpg.CancellationBinding over
// adapters.TxCancellationRequester, driven through the event store's Process)
// and periodic cancellation revoke the attempt's runtime grants in the
// transaction that records the cancellation, with the secret release; the
// writer slot and capacity are released by the later settlement, never by the
// intent. A revoker, secret, event or commit failure rolls all of it back and
// leaves the token usable; ordered races with result commit and grant admission
// are decided by whichever transaction wins; repeating a cancellation is
// idempotent. Real PostgreSQL; no NATS, S3 or Git (source-publication
// interleavings are TestGuardedSourceRecovery).
func Test_cancel_grant_atomicity(t *testing.T) {
	ctx := context.Background()

	t.Run("direct cancellation revokes the grant with the intent, in lock order, once", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		a := e.Run(e.NewProject(ScopeA), Job("build", writeRun, labelJob, secretDoc))
		if e.GrantRevoked(a) {
			t.Fatal("setup")
		}
		e.Faults.Reset()
		if err := e.CancelErr(a); err != nil {
			t.Fatal(err)
		}
		got := e.Snap(ScopeA.TenantID)
		if got.GrantsRevoked != 1 || got.ControlsRevoked != 1 || got.PinsLive != 0 || got.WritersHeld != 0 || got.HeldReservations != 0 {
			t.Fatalf("cancellation did not revoke, release and settle: %+v", got)
		}
		if at := e.Persisted(a); at.Status != execution.AttemptCancelled {
			t.Fatal(at.Status)
		}
		// The grant revocation, secret release and the intent's events share one
		// transaction, in the documented order; the settlement is its own later one.
		hits := e.Faults.Trace()
		assertOrdered(t, hits, PointGrantRevoke, PointSecretRelease, PointWorkspaceSettle, PointEventAppend)
		intent := e.Faults.Hits(PointGrantRevoke)
		if len(intent) != 1 {
			t.Fatalf("grants revoked %d times", len(intent))
		}
		if rel := e.Faults.Hits(PointSecretRelease); len(rel) == 0 || rel[0].Xid != intent[0].Xid {
			t.Fatalf("the secret release is not in the revoking transaction: %+v %+v", rel, intent)
		}
		// Repeating the cancellation changes nothing and revokes nothing again.
		before := e.Snap(ScopeA.TenantID)
		e.Faults.Reset()
		if err := e.CancelErr(a); err != nil {
			t.Fatal(err)
		}
		if again := e.Snap(ScopeA.TenantID); again != before {
			t.Fatalf("a repeated cancellation changed state:\n before %+v\n after  %+v", before, again)
		}
		if n := len(e.Faults.Hits(PointGrantRevoke)); n != 0 {
			t.Fatalf("a repeated cancellation revoked %d grants", n)
		}
	})

	t.Run("a revoker, secret, event or commit failure rolls the cancellation back and the token still works", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		a := e.Run(e.NewProject(ScopeA), Job("build", writeRun, labelJob, secretDoc))
		before := e.Snap(ScopeA.TenantID)
		for _, point := range []string{PointGrantRevoke, PointSecretRelease, PointEventAppend, "commit"} {
			if point == "commit" {
				e.Faults.Commit(2)
			} else {
				e.Faults.Fail(point)
			}
			if err := e.CancelErr(a); err == nil {
				t.Fatalf("%s: the failure was swallowed", point)
			}
			e.Faults.Clear()
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s: a failed cancellation left state behind:\n before %+v\n after  %+v", point, before, got)
			}
		}
		if e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptRunning {
			t.Fatal("authority changed although no cancellation committed")
		}
		// Authority is intact: the attempt's own session still accepts a result.
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); err != nil {
			t.Fatalf("the token of an attempt whose cancellation failed: %v", err)
		}
	})

	t.Run("item-scoped cancellation revokes every run's grants in one batch and is idempotent", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		var attempts []*Attempt
		for range 3 {
			attempts = append(attempts, e.Run(p, Job("build", readRun, labelJob, secretDoc)))
		}
		e.Faults.Reset()
		c := execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2}
		if err := e.ExecPlain.RequestCancellation(ctx, c); err != nil {
			t.Fatal(err)
		}
		for _, a := range attempts {
			if !e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptCancelled {
				t.Fatalf("attempt %s: revoked %v status %s", a.ID(), e.GrantRevoked(a), e.Persisted(a).Status)
			}
		}
		got := e.Snap(ScopeA.TenantID)
		// The request revokes and releases; the workloads' termination (and with
		// it the capacity release) is the reconciler's, outside any transaction.
		if got.CancelIntents != 1 || got.GrantsRevoked != 3 || got.PinsLive != 0 || got.HeldReservations != 3 {
			t.Fatalf("%+v", got)
		}
		// One transaction owns the whole batch.
		onePositiveXid(t, e.Faults.Hits(PointGrantRevoke))
		if _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		if got := e.Snap(ScopeA.TenantID); got.HeldReservations != 0 || got.ReleasedReservations != 3 {
			t.Fatalf("%+v", got)
		}
		before := e.Snap(ScopeA.TenantID)
		if err := e.ExecPlain.RequestCancellation(ctx, c); err != nil {
			t.Fatal(err)
		}
		if again := e.Snap(ScopeA.TenantID); again != before {
			t.Fatalf("a repeated item cancellation changed state:\n before %+v\n after  %+v", before, again)
		}
	})

	t.Run("a failure at a later run of the batch rolls back the earlier runs' grants, pins and history", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		var attempts []*Attempt
		for range 3 {
			attempts = append(attempts, e.Run(p, Job("build", readRun, labelJob, secretDoc)))
		}
		before := e.Snap(ScopeA.TenantID)
		c := execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2}
		for _, f := range []struct {
			point string
			nth   int
		}{{PointGrantRevoke, 3}, {PointSecretRelease, 3}, {PointEventAppend, 2}} {
			e.Faults.Reset()
			e.Faults.FailNth(f.point, f.nth)
			err := e.ExecPlain.RequestCancellation(ctx, c)
			e.Faults.Clear()
			if err == nil {
				t.Fatalf("%s #%d: the failure was swallowed", f.point, f.nth)
			}
			// The earlier runs were already mutated when the failure arrived.
			if n := len(e.Faults.Hits(f.point)); n < f.nth {
				t.Fatalf("%s: the failure came after %d crossings, want %d", f.point, n, f.nth)
			}
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s #%d: a failed batch left state behind:\n before %+v\n after  %+v", f.point, f.nth, before, got)
			}
		}
		for _, a := range attempts {
			if e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptRunning {
				t.Fatalf("attempt %s lost authority although the batch never committed", a.ID())
			}
		}
		// The retry applies the whole batch once.
		e.Faults.Reset()
		if err := e.ExecPlain.RequestCancellation(ctx, c); err != nil {
			t.Fatal(err)
		}
		if got := e.Snap(ScopeA.TenantID); got.GrantsRevoked != 3 || got.CancelIntents != 1 || got.PinsLive != 0 {
			t.Fatalf("%+v", got)
		}
		onePositiveXid(t, e.Faults.Hits(PointGrantRevoke))
	})

	t.Run("a caller-owned transaction commits or rolls back the cancellation request with its own", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		a := e.Run(p, Job("build", writeRun, labelJob, secretDoc))
		before := e.Snap(ScopeA.TenantID)
		c := execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2}

		// The inbox transaction fails after the cancellation applied: everything,
		// including the intent row, rolls back with it.
		err := e.inTenant(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := e.RT.RequestCancellationInTx(ctx, tx, c); err != nil {
				return err
			}
			var revoked int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM runtime_grants WHERE revoked_at IS NOT NULL`).Scan(&revoked); err != nil || revoked != 1 {
				t.Errorf("the revocation was not visible inside the owning transaction: %d %v", revoked, err)
			}
			return errInjected
		})
		if !errors.Is(err, errInjected) {
			t.Fatal(err)
		}
		if got := e.Snap(ScopeA.TenantID); got != before {
			t.Fatalf("a rolled-back inbox transaction left state behind:\n before %+v\n after  %+v", before, got)
		}

		e.Faults.Reset()
		if err := e.inTenant(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return e.RT.RequestCancellationInTx(ctx, tx, c)
		}); err != nil {
			t.Fatal(err)
		}
		if !e.GrantRevoked(a) || e.Snap(ScopeA.TenantID).CancelIntents != 1 {
			t.Fatal("the committed inbox transaction did not revoke the grant and record the intent")
		}
		onePositiveXid(t, e.Faults.Hits(PointGrantRevoke))
	})

	t.Run("the consumer-bound cancellation commits with the inbox result and a failure leaves nothing but the retry record", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		a := e.Run(p, Job("build", writeRun, labelJob, secretDoc))

		// The actual composition: the work module's consumer binding over the
		// execution adapter's transaction-bound requester, registered with the real
		// PostgreSQL event consumer and driven through Store.Process.
		binding, err := workpg.CancellationBinding(workpg.CancellationConsumerName, execadapters.TxCancellationRequester(e.RT))
		if err != nil {
			t.Fatal(err)
		}
		opts := eventstest.DefaultOptions()
		opts.Retry.MaxAttempts = 10 // three injected failures must stay retries, not a quarantine
		consumer, err := eventspg.NewStore(e.X.Runtime, opts, binding)
		if err != nil {
			t.Fatal(err)
		}
		process := func(env eventsv1.Envelope) events.Outcome {
			t.Helper()
			payload, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			out, err := consumer.Process(ctx, events.Delivery{Tenant: ScopeA.TenantID, Consumer: workpg.CancellationConsumerName, Payload: payload})
			if err != nil {
				t.Fatalf("process %s: %v", env.ID, err)
			}
			return out
		}
		cancelled := func(id string, version int64) eventsv1.Envelope {
			env := eventstest.WorkItemTransition(string(ScopeA.TenantID), string(ScopeA.ProjectID), id, string(p.Item), version)
			env.OccurredAt = time.Now().UTC()
			data, err := json.Marshal(eventsv1.WorkItemTransitioned{From: "active", To: "cancelled", TransitionID: "tr_" + id})
			if err != nil {
				t.Fatal(err)
			}
			env.Data = data
			// The delivered event has an origin, as one the outbox published has.
			if err := e.inTenant(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := e.X.Events.Append(ctx, tx, env)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return env
		}
		inbox := func(id string) (state string, attempts int, xmin int64) {
			t.Helper()
			// The inbox is tenant-owned with forced RLS: read it as the application does.
			err := e.inTenant(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT state, attempts, xmin::text::bigint FROM event_inbox WHERE consumer = $1 AND event_id = $2`,
					workpg.CancellationConsumerName, id).Scan(&state, &attempts, &xmin)
			})
			if err != nil {
				t.Fatal(err)
			}
			return state, attempts, xmin
		}

		// A failure after the cancellation applied (at each peer step) rolls back
		// every effect of the handler, leaving only the consumer's retry record.
		first := cancelled("evt_t062_cancel_1", 2)
		before := e.Snap(ScopeA.TenantID)
		beforeInbox := e.X.CountTenant(ScopeA.TenantID, `SELECT count(*) FROM event_inbox`)
		for n, point := range []string{PointGrantRevoke, PointSecretRelease, PointEventAppend} {
			e.Faults.Fail(point)
			out := process(first)
			e.Faults.Clear()
			if out.Kind != events.OutcomeRetry {
				t.Fatalf("%s: a failed handler must be retried, got %+v", point, out)
			}
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s: a failed consumer left cancellation effects behind:\n before %+v\n after  %+v", point, before, got)
			}
			if state, attempts, _ := inbox(string(first.ID)); state != "pending" || attempts != n+1 {
				t.Fatalf("%s: the inbox entry must be pending with the failed attempt recorded, got %s/%d", point, state, attempts)
			}
			if e.X.CountTenant(ScopeA.TenantID, `SELECT count(*) FROM event_inbox`) != beforeInbox+1 {
				t.Fatalf("%s: more than the one retry record", point)
			}
			time.Sleep(2 * opts.Retry.Max) // past the persisted retry schedule
		}
		if e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptRunning {
			t.Fatal("authority changed although no consumer transaction committed")
		}

		// The redelivery commits the cancellation and the inbox completion together.
		e.Faults.Reset()
		if out := process(first); out.Kind != events.OutcomeCompleted {
			t.Fatalf("%+v", out)
		}
		got := e.Snap(ScopeA.TenantID)
		// The consumer's transaction records the intent, revokes the grant and
		// releases the secret pins. The workload's termination, and with it the
		// writer slot and capacity, is a later settlement: neither is released
		// by the intent.
		if got.CancelIntents != 1 || got.GrantsRevoked != 1 || got.PinsLive != 0 || !e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptCancelled {
			t.Fatalf("the committed consumer transaction did not record the intent, revoke and release: %+v", got)
		}
		if got.WritersHeld != 1 || got.HeldReservations != 1 {
			t.Fatalf("the intent must not release the writer or capacity before the workload is confirmed gone: %+v", got)
		}
		if _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		if settled := e.Snap(ScopeA.TenantID); settled.WritersHeld != 0 || settled.HeldReservations != 0 || settled.ReleasedReservations != 1 {
			t.Fatalf("the later settlement did not release the writer and capacity: %+v", settled)
		}
		state, _, xmin := inbox(string(first.ID))
		if state != "completed" {
			t.Fatal(state)
		}
		revoke := onePositiveXid(t, e.Faults.Hits(PointGrantRevoke))
		if revoke&0xffffffff != xmin {
			t.Fatalf("the revocation (xid %d) did not run in the inbox completion's transaction (xmin %d)", revoke, xmin)
		}

		// Duplicate and reordered deliveries are idempotent: nothing changes, the
		// older version never lowers the recorded one, and nothing is revoked twice.
		after := e.Snap(ScopeA.TenantID)
		e.Faults.Reset()
		if out := process(first); out.Kind != events.OutcomeDuplicate {
			t.Fatalf("a duplicate delivery: %+v", out)
		}
		newer := cancelled("evt_t062_cancel_3", 3)
		process(newer)
		process(cancelled("evt_t062_cancel_2", 2)) // late, lower version
		if again := e.Snap(ScopeA.TenantID); again.CancelIntents != after.CancelIntents || again.GrantsRevoked != after.GrantsRevoked ||
			again.Runs != after.Runs || again.Attempts != after.Attempts || again.PinsReleased != after.PinsReleased {
			t.Fatalf("a duplicate or reordered delivery changed state:\n before %+v\n after  %+v", after, again)
		}
		if v := e.Count(`SELECT version FROM work_cancellation_requests WHERE work_item_id = $1`, string(p.Item)); v != 3 {
			t.Fatalf("the recorded cancellation version is %d, want the highest, 3", v)
		}
		if n := len(e.Faults.Hits(PointGrantRevoke)); n != 0 {
			t.Fatalf("a repeated delivery revoked %d grants", n)
		}
	})

	t.Run("the periodic requester revokes grants the lost event never reached", func(t *testing.T) {
		e := New(t, Options{})
		p := e.NewProject(ScopeA)
		a := e.Run(p, Job("build", readRun, labelJob))
		e.X.CancelWork(p.Scope, p.Item) // the work module cancelled the item; its event was lost
		if e.GrantRevoked(a) {
			t.Fatal("setup: nothing has reached execution yet")
		}
		n, err := e.ExecPlain.ReconcileCancellations(ctx, ScopeA.TenantID)
		if err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		if !e.GrantRevoked(a) || e.Persisted(a).Status != execution.AttemptCancelled {
			t.Fatal("the periodic scan did not revoke")
		}
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a callback after the periodic cancellation: %v", err)
		}
	})

	t.Run("a result that commits first is accepted and its receipt survives a later cancellation", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		req := Request(t, Completed(outputsOf("v1")))
		paused, release := e.Gate.Arm()
		var wg sync.WaitGroup
		var ack error
		wg.Add(1)
		go func() { defer wg.Done(); _, ack = e.Submit(a, req) }()
		paused()
		cancelled := make(chan error, 1)
		go func() { cancelled <- e.CancelErr(a) }()
		select {
		case err := <-cancelled:
			t.Fatalf("the cancellation did not wait for the result's run lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		release()
		wg.Wait()
		if ack != nil {
			t.Fatal(ack)
		}
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		at := e.Persisted(a)
		if at.Status != execution.AttemptSucceeded || at.ResultHash != string(req.ResultHash) {
			t.Fatalf("%s %q", at.Status, at.ResultHash)
		}
		if e.GrantRevoked(a) {
			t.Fatal("a cancellation revoked the grant of an accepted result")
		}
		if again, err := e.Submit(a, req); err != nil || again.ResultHash != req.ResultHash {
			t.Fatalf("the receipt after the late cancellation: %+v %v", again, err)
		}
	})

	t.Run("a cancellation that commits first wins, and a result then cannot be accepted", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		if err := e.CancelErr(a); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		if at := e.Persisted(a); at.ResultHash != "" || at.Status != execution.AttemptCancelled || !e.GrantRevoked(a) {
			t.Fatalf("%s %q revoked %v", at.Status, at.ResultHash, e.GrantRevoked(a))
		}
	})

	t.Run("grant admission and cancellation: the winning transaction governs", func(t *testing.T) {
		e := New(t, Options{})
		// Cancellation first: the executor's grant registration is refused and
		// leaves no grant.
		late := e.Unadmitted(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		if err := e.CancelErr(late); err != nil {
			t.Fatal(err)
		}
		if err := e.Register(late); err == nil {
			t.Fatal("a grant was registered for a cancelled attempt")
		} else if !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		if n := e.Count(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1`, string(late.ID())); n != 0 {
			t.Fatalf("%d grants", n)
		}
		// Registration first: the cancellation revokes the grant it created.
		early := e.Unadmitted(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		if err := e.Register(early); err != nil {
			t.Fatal(err)
		}
		if err := e.CancelErr(early); err != nil {
			t.Fatal(err)
		}
		if !e.GrantRevoked(early) {
			t.Fatal("the cancellation left the registered grant usable")
		}
	})
}
