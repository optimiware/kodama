package composition_test

import (
	"context"
	"errors"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
)

// Test_execution_peer_rollback: SD-04, SD-09-SD-11, PX-05, PX-11, WS-11
// (acceptance `execution_peer_rollback`). Admission, the writer claim and
// settlement run their peers (secret pins, workspace writer, capacity, events)
// in the one transaction execution opened: an injected failure at any seam, or
// at commit, rolls back execution state, pins, retained references, writer and
// capacity reservations, history, audit and outbox together; a retry consumes
// no extra ordinal and releases each resource once. Real PostgreSQL through the
// runtime-enabled store; no NATS, S3 or Git.
func Test_execution_peer_rollback(t *testing.T) {
	ctx := context.Background()

	t.Run("admission rolls back pins, retained references, history, audit and outbox", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		p := e.NewProject(ScopeA)
		def := e.Define(p.Scope, Job("build", readRun, labelJob, secretDoc))
		before := e.Snap(ScopeA.TenantID)
		for _, point := range []string{PointSecretPin, PointEventAppend + ":" + eventsv1.TypeRunCreated, PointEventAppend} {
			e.Faults.Fail(point)
			if _, err := e.AdmitRun(p, def); !errors.Is(err, errInjected) {
				t.Fatalf("%s: %v", point, err)
			}
			e.Faults.Clear()
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s: a failed admission left state behind:\n before %+v\n after  %+v", point, before, got)
			}
		}
		e.Faults.Commit(2)
		if _, err := e.AdmitRun(p, def); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("commit failure: %v", err)
		}
		e.Faults.Clear()
		if got := e.Snap(ScopeA.TenantID); got != before {
			t.Fatalf("a failed commit left state behind:\n before %+v\n after  %+v", before, got)
		}

		e.Faults.Reset()
		run, err := e.AdmitRun(p, def)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Snap(ScopeA.TenantID); got.Runs != 1 || got.PinsLive != 1 {
			t.Fatalf("the retried admission: %+v", got)
		}
		// The pin, the retention intent on the workspace and the run event joined
		// one transaction (a later confirmation transaction is a different one).
		var joined []Hit
		for _, h := range e.Faults.Trace() {
			if h.Point == PointSecretPin || h.Point == PointWorkspaceReserve || h.Point == PointEventAppend+":"+eventsv1.TypeRunCreated {
				joined = append(joined, h)
			}
		}
		if len(joined) != 3 {
			t.Fatalf("expected pin, workspace and event hits: %+v", e.Faults.Trace())
		}
		onePositiveXid(t, joined)
		if run.Status != execution.RunQueued {
			t.Fatal(run.Status)
		}
	})

	t.Run("a writer claim rolls back the attempt, capacity and writer slot and consumes no ordinal", func(t *testing.T) {
		e := New(t, Options{})
		p := e.NewProject(ScopeA)
		def := e.Define(p.Scope, Job("build", writeRun, labelJob))
		if _, err := e.AdmitRun(p, def); err != nil {
			t.Fatal(err)
		}
		before := e.Snap(ScopeA.TenantID)
		for _, point := range []string{PointWorkspaceReserve, PointEventAppend + ":" + eventsv1.TypeStepAttemptStateChange, PointEventAppend} {
			e.Faults.Fail(point)
			if _, err := e.ExecPlain.ClaimRunnable(ctx, ScopeA.TenantID); !errors.Is(err, errInjected) {
				t.Fatalf("%s: %v", point, err)
			}
			e.Faults.Clear()
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s: a failed claim left state behind:\n before %+v\n after  %+v", point, before, got)
			}
		}
		e.Faults.Commit(2)
		if _, err := e.ExecPlain.ClaimRunnable(ctx, ScopeA.TenantID); err == nil {
			t.Fatal("a failed commit claimed")
		}
		e.Faults.Clear()
		if got := e.Snap(ScopeA.TenantID); got != before {
			t.Fatalf("a failed commit left state behind:\n before %+v\n after  %+v", before, got)
		}

		e.Faults.Reset()
		in, err := e.ExecPlain.ClaimRunnable(ctx, ScopeA.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		if in.Ordinal != 1 {
			t.Fatalf("ordinal %d: the failed claims consumed one", in.Ordinal)
		}
		hits := e.Faults.Trace()
		onePositiveXid(t, hits)
		if got := e.Snap(ScopeA.TenantID); got.HeldReservations != 1 || got.WritersHeld != 1 {
			t.Fatalf("the retried claim: %+v", got)
		}
	})

	t.Run("settlement rolls back secret release, writer settlement and capacity release together", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		a := e.Run(e.NewProject(ScopeA), Job("build", writeRun, labelJob, secretDoc))
		before := e.Snap(ScopeA.TenantID)
		if before.HeldReservations != 1 || before.WritersHeld != 1 || before.PinsLive != 1 {
			t.Fatalf("setup: %+v", before)
		}
		res := Completed(outputsOf("v1"))
		commit := runtimev1.GitCommit(e.Source())
		res.SourceCommit = &commit
		for _, point := range []string{PointSecretRelease, PointWorkspaceSettle, PointEventAppend, "commit"} {
			if point == "commit" {
				e.Faults.Commit(2)
			} else {
				e.Faults.Fail(point)
			}
			if _, err := e.CommitPlain(a, res); err == nil {
				t.Fatalf("%s: the failure was swallowed", point)
			}
			e.Faults.Clear()
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("%s: a failed settlement left state behind:\n before %+v\n after  %+v", point, before, got)
			}
			if at := e.Persisted(a); at.Status != execution.AttemptRunning || at.ResultHash != "" {
				t.Fatalf("%s: attempt %s %q", point, at.Status, at.ResultHash)
			}
		}

		e.Faults.Reset()
		ack, err := e.CommitPlain(a, res)
		if err != nil || ack.AttemptStatus != execution.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, err)
		}
		got := e.Snap(ScopeA.TenantID)
		if got.HeldReservations != 0 || got.ReleasedReservations != 1 || got.WritersHeld != 0 || got.PinsLive != 0 || got.PinsReleased != 1 {
			t.Fatalf("the retried settlement did not release each resource once: %+v", got)
		}
		// One transaction; secret release (level 4) before the workspace (5)
		// before events (6), whatever order the entities queued them in.
		hits := e.Faults.Trace()
		onePositiveXid(t, hits)
		assertOrdered(t, hits, PointSecretRelease, PointWorkspaceSettle, PointEventAppend)
		if len(e.Faults.Hits(PointSecretRelease)) != 1 || len(e.Faults.Hits(PointWorkspaceSettle)) != 1 {
			t.Fatalf("peer effects ran more than once: %v", points(hits))
		}
		// An accepted result keeps the grant's receipt exception.
		if e.GrantRevoked(a) {
			t.Fatal("an accepted result revoked the grant that serves its receipt")
		}
	})

	t.Run("a failed writer settles in its termination transaction and a failure there is recovered by reconciliation", func(t *testing.T) {
		e := New(t, Options{})
		e.X.Secret(ScopeA, "token", "s3cr3t")
		a := e.Run(e.NewProject(ScopeA), Job("build", writeRun, labelJob, secretDoc))
		e.Faults.Fail(PointWorkspaceSettle)
		ack, err := e.CommitPlain(a, Failed())
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("the result is accepted in its own transaction: %+v %v", ack, err)
		}
		e.Faults.Clear()
		got := e.Snap(ScopeA.TenantID)
		// The settlement transaction rolled back whole: the writer and the
		// capacity reservation are retained, the termination is unconfirmed.
		if got.WritersHeld != 1 || got.HeldReservations != 1 || got.Terminated != 0 {
			t.Fatalf("a failed settlement was partly applied: %+v", got)
		}
		if got.PinsLive != 0 {
			t.Fatalf("the accepted failure released no secrets: %+v", got)
		}
		e.Faults.Reset()
		if _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		got = e.Snap(ScopeA.TenantID)
		if got.WritersHeld != 0 || got.HeldReservations != 0 || got.ReleasedReservations != 1 || got.Terminated != 1 {
			t.Fatalf("reconciliation did not settle once: %+v", got)
		}
		assertOrdered(t, e.Faults.Trace(), PointSecretRelease, PointWorkspaceSettle, PointEventAppend)
	})
}
