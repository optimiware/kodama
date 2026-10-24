package composition_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
)

const retryInfra = "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure]}"

// barrier makes the first crossing of a fault point wait until released, with
// the crossing transaction's locks held. The transaction identity of the
// waiter is whatever opened it; later crossings pass.
type barrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *Env) holdAt(point string) *barrier {
	b := &barrier{entered: make(chan struct{}), release: make(chan struct{})}
	e.Faults.Hook(point, func() {
		b.once.Do(func() {
			close(b.entered)
			<-b.release
		})
	})
	return b
}

func (b *barrier) waitEntered(t testing.TB) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the transaction never reached the barrier")
	}
}

func (b *barrier) open() { close(b.release) }

func notDone(t testing.TB, what string, done <-chan error, wait time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s finished while the lock holder was paused: %v", what, err)
	case <-time.After(wait):
	}
}

func finished(t testing.TB, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish: a lock cycle or a stuck lock", what)
		return nil
	}
}

func noDeadlock(t testing.TB, what string, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001") {
		t.Fatalf("%s: lock-order inversion (%s): %v", what, pg.Code, err)
	}
}

// TestCompositionLockOrder: SD-04, SD-10, PX-07, PX-11, WS-11. The documented
// transaction-wide lock order (quota, work, run rows, grants, secrets, workspace,
// events) holds across the composed peers: an item-scoped cancellation locks
// every run before any lower-order lock and completes grants and secrets for
// the whole batch before any workspace or event write; writer finalization,
// secret release, cancellation, reconciliation and guarded results competing at
// barriers finish without inversion; an unknown effect stays blocked across
// service reconstruction until affirmative reconciliation. Real PostgreSQL;
// executor and effect evidence are controlled doubles.
func TestCompositionLockOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("an item-scoped cancellation locks every run first and finishes grants and secrets before any event", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		var attempts []*Attempt
		for range 3 {
			attempts = append(attempts, e.Run(p, Job("build", readRun, labelJob, secretDoc)))
		}
		var checked bool
		e.Faults.Hook(PointGrantRevoke, func() {
			if checked {
				return
			}
			checked = true
			// The first revocation is in progress: every affected run row is
			// already locked by the batch, though no other run was touched yet.
			for _, a := range attempts {
				_, err := e.X.Migration.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, string(a.Run.ID))
				var pg *pgconn.PgError
				if !errors.As(err, &pg) || pg.Code != "55P03" {
					t.Errorf("run %s was not locked before the first lower-order effect: %v", a.Run.ID, err)
				}
			}
		})
		e.Faults.Reset()
		if err := e.ExecPlain.RequestCancellation(ctx, execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2}); err != nil {
			t.Fatal(err)
		}
		e.Faults.Hook(PointGrantRevoke, nil)
		if !checked {
			t.Fatal("the barrier never ran")
		}
		hits := e.Faults.Trace()
		xid := onePositiveXid(t, e.Faults.Hits(PointGrantRevoke))
		var lastLower, firstEvent int
		for _, h := range hits {
			if h.Xid != xid {
				continue
			}
			switch {
			case h.Point == PointGrantRevoke || h.Point == PointSecretRelease:
				lastLower = h.Seq
			case h.Point == PointWorkspaceSettle:
				t.Fatalf("a writer settlement in a reader batch: %+v", hits)
			default:
				if firstEvent == 0 {
					firstEvent = h.Seq
				}
			}
		}
		if len(e.Faults.Hits(PointGrantRevoke)) != 3 || firstEvent == 0 || lastLower > firstEvent {
			t.Fatalf("run %d's grants/secrets (seq %d) came after the first event (seq %d): %+v", 3, lastLower, firstEvent, hits)
		}
	})

	t.Run("writer finalization paused at its secret release blocks competing updates without a cycle", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		a := e.Run(p, Job("build", writeRun, labelJob, secretDoc))
		res := Completed(outputsOf("v1"))
		commit := runtimev1.GitCommit(e.Source())
		res.SourceCommit = &commit

		hold := e.holdAt(PointSecretRelease)
		commitDone := make(chan error, 1)
		go func() { _, err := e.CommitPlain(a, res); commitDone <- err }()
		hold.waitEntered(t) // the finalizing transaction holds the run row and has taken its grants

		cancelDone, itemDone, reconcileDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
		go func() { cancelDone <- e.CancelErr(a) }()
		go func() {
			itemDone <- e.ExecPlain.RequestCancellation(ctx, execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2})
		}()
		go func() { _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); reconcileDone <- err }()
		notDone(t, "the cancellation", cancelDone, 400*time.Millisecond)
		notDone(t, "the item cancellation", itemDone, 100*time.Millisecond)
		hold.open()

		if err := finished(t, "the result", commitDone); err != nil {
			t.Fatal(err)
		}
		for what, ch := range map[string]chan error{"cancellation": cancelDone, "item cancellation": itemDone, "reconciliation": reconcileDone} {
			err := finished(t, what, ch)
			noDeadlock(t, what, err)
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		at := e.Persisted(a)
		if at.Status != execution.AttemptSucceeded || at.ResultHash == "" {
			t.Fatalf("the cancellations rewrote the accepted result: %s", at.Status)
		}
		got := e.Snap(ScopeA.TenantID)
		if got.HeldReservations != 0 || got.ReleasedReservations != 1 || got.WritersHeld != 0 || got.PinsLive != 0 || e.GrantRevoked(a) {
			t.Fatalf("%+v revoked=%v", got, e.GrantRevoked(a))
		}
		if e.transitions(a) != 1 {
			t.Fatalf("%d acceptances", e.transitions(a))
		}
	})

	t.Run("a cancellation paused at its revocation orders a guarded result behind it", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		hold := e.holdAt(PointGrantRevoke)
		cancelDone := make(chan error, 1)
		go func() { cancelDone <- e.CancelErr(a) }()
		hold.waitEntered(t)
		submitDone := make(chan error, 1)
		go func() { _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); submitDone <- err }()
		notDone(t, "the result", submitDone, 400*time.Millisecond)
		hold.open()
		if err := finished(t, "the cancellation", cancelDone); err != nil {
			t.Fatal(err)
		}
		if err := finished(t, "the result", submitDone); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("the result queued behind a cancellation: %v", err)
		}
		if at := e.Persisted(a); at.ResultHash != "" || at.Status != execution.AttemptCancelled || !e.GrantRevoked(a) {
			t.Fatalf("%s %q", at.Status, at.ResultHash)
		}
	})

	t.Run("racing results, cancellations and reconciliation never invert locks and leave one outcome", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		for round := range 6 {
			p := e.NewProject(ScopeA)
			a := e.Run(p, Job("build", readRun, labelJob, secretDoc))
			req := Request(t, Completed(outputsOf("v1")))
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, 4)
			run := func(fn func() error) {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; errs <- fn() }()
			}
			run(func() error { _, err := e.Submit(a, req); return err })
			run(func() error { return e.CancelErr(a) })
			run(func() error {
				return e.ExecPlain.RequestCancellation(ctx, execution.WorkCancellation{Scope: p.Scope, WorkItemID: p.Item, Version: 2})
			})
			run(func() error { _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); return err })
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				noDeadlock(t, "racing operation", err)
				if err != nil && !errors.Is(err, results.ErrAttemptRevoked) && !errors.Is(err, results.ErrUnavailable) && !errors.Is(err, execution.ErrUnavailable) {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			if _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
				t.Fatal(err)
			}
			at := e.Persisted(a)
			switch at.Status {
			case execution.AttemptSucceeded:
				if at.ResultHash != string(req.ResultHash) || e.GrantRevoked(a) {
					t.Fatalf("round %d: accepted result with revoked grant or no hash", round)
				}
			case execution.AttemptCancelled:
				if at.ResultHash != "" || !e.GrantRevoked(a) {
					t.Fatalf("round %d: cancelled attempt with a result or a live grant", round)
				}
			default:
				t.Fatalf("round %d: attempt %s", round, at.Status)
			}
			if h := e.Count(`SELECT count(*) FROM execution_reservations WHERE attempt_id = $1 AND released_at IS NULL`, string(a.ID())); h != 0 {
				t.Fatalf("round %d: the reservation was not released", round)
			}
		}
	})

	t.Run("an unknown effect stays blocked across service reconstruction until affirmative reconciliation", func(t *testing.T) {
		e := New(t, Options{})
		p := e.NewProject(ScopeA)
		a := e.Claimed(p, Job("deploy", writeRun, retryInfra))
		in := a.Intent
		// The runtime cannot confirm the workload stopped on its own.
		e.X.Manual.SetStop(execution.StopEvidence{})
		// The attempt fails ambiguously and its workload is confirmed gone with no
		// launch evidence: nothing says whether the command ran.
		if err := e.X.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
			t.Fatal(err)
		}
		if err := e.X.Terminate(in, execution.LaunchUnknown); err != nil {
			t.Fatal(err)
		}
		// No accepted result: the failure revoked the grant in its transaction.
		if !e.GrantRevoked(a) {
			t.Fatal("a failure without an accepted result left the grant usable")
		}
		blocked := func(svc *execution.Service, when string) {
			t.Helper()
			e.X.Clock.Advance(time.Hour)
			if _, err := svc.ClaimRunnable(ctx, ScopeA.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("%s: a blocked step was claimed: %v", when, err)
			}
			got := e.Snap(ScopeA.TenantID)
			if got.Attempts != 1 || got.HeldReservations != 1 || got.WritersHeld != 1 {
				t.Fatalf("%s: the barrier was released: %+v", when, got)
			}
		}
		blocked(e.ExecPlain, "before reconstruction")
		restarted := e.X.NewService(executiontestOptions(e, true))
		if _, err := restarted.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		blocked(restarted, "after reconstruction without an effect adapter")
		e.X.Effects.Set(execution.VerdictStillUnknown, nil)
		withEffects := e.X.NewService(executiontestOptions(e, false))
		if _, err := withEffects.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		blocked(withEffects, "after an inconclusive reconciliation")

		e.X.Effects.Set(execution.VerdictReplaySafe, nil)
		if _, err := withEffects.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		got := e.Snap(ScopeA.TenantID)
		if got.HeldReservations != 0 || got.ReleasedReservations != 1 || got.WritersHeld != 0 {
			t.Fatalf("affirmative evidence did not settle once: %+v", got)
		}
		if at := e.Persisted(a); at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutcomeUnknown {
			t.Fatalf("the unknown attempt was rewritten to %s/%s", at.Status, at.Reason)
		}
	})
}
