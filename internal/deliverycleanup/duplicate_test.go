package deliverycleanup_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// expired is a world with one expired, settled deployment.
func expired(t *testing.T, p deliverycleanup.Policy) *world {
	t.Helper()
	w := newWorld(t, p)
	w.rec.add(scopeA, preview, time.Hour)
	w.m.Advance(2 * time.Hour)
	return w
}

// cleanup_duplicate_claim: however many schedulers, retries or restarts claim a
// deployment's cleanup, one run exists per round, a failed round is retried at
// most the bounded number of times, and an unknown outcome is never retried
// implicitly (DQ-09, PX-06, PX-07, PX-A05).
func TestCleanupDuplicateClaim(t *testing.T) {
	ctx := context.Background()

	t.Run("two schedulers converge on one run", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		second := w.scheduler(deliverycleanup.Policy{})
		first := one(t, w.sweep(w.sched))
		dup := one(t, w.sweep(second))
		wantOutcome(t, first, deliverycleanup.OutcomeAdmitted)
		// The run is queued, so the second scheduler sees it in flight: the
		// round is claimed once.
		wantOutcome(t, dup, deliverycleanup.OutcomeInFlight)
		if first.RunID != dup.RunID || len(w.cleanupRuns()) != 1 {
			t.Fatalf("runs %v %v / %d stored", first.RunID, dup.RunID, len(w.cleanupRuns()))
		}
	})

	t.Run("claims racing on the same round create one run", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		const claimants = 8
		outcomes := make(chan deliverycleanup.Outcome, claimants)
		var wg sync.WaitGroup
		for i := 0; i < claimants; i++ {
			s := w.scheduler(deliverycleanup.Policy{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				rep, err := s.Sweep(ctx, scopeA)
				if err != nil || len(rep.Items) != 1 {
					t.Errorf("sweep: %v %+v", err, rep.Items)
					return
				}
				outcomes <- rep.Items[0].Outcome
			}()
		}
		wg.Wait()
		close(outcomes)
		created := 0
		for o := range outcomes {
			switch o {
			case deliverycleanup.OutcomeAdmitted:
				created++
			case deliverycleanup.OutcomeDuplicate, deliverycleanup.OutcomeInFlight:
			default:
				t.Errorf("unexpected outcome %q", o)
			}
		}
		if created != 1 || len(w.cleanupRuns()) != 1 {
			t.Fatalf("%d claimants created a run, %d stored", created, len(w.cleanupRuns()))
		}
	})

	t.Run("an admission whose answer was lost is observed, not repeated", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		lossy := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) { c.Runs = lostAck{Runs: w.m.Svc} })
		wantOutcome(t, one(t, w.sweep(lossy)), deliverycleanup.OutcomeError)
		if len(w.cleanupRuns()) != 1 {
			t.Fatalf("%d runs after the lost answer", len(w.cleanupRuns()))
		}
		it := one(t, w.sweep(w.sched))
		wantOutcome(t, it, deliverycleanup.OutcomeInFlight)
		if it.RunID != execution.CleanupRunID(scopeA, preview, 1) || len(w.cleanupRuns()) != 1 {
			t.Fatalf("the retry created another run: %+v", it)
		}
	})

	t.Run("the same round admitted twice returns the stored run", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.sweep(w.sched)
		cmd := execution.CleanupCommand{Scope: scopeA, DeploymentID: preview, DefinitionID: w.def}
		calls := w.rec.authCalls
		run, created, err := w.m.Svc.AdmitCleanupRound(ctx, cmd, 1)
		if err != nil || created || run.ID != execution.CleanupRunID(scopeA, preview, 1) {
			t.Fatalf("repeat: %v %v %v", run, created, err)
		}
		if w.rec.authCalls != calls || len(w.cleanupRuns()) != 1 {
			t.Fatal("a repeat re-ran admission or created a run")
		}
	})

	t.Run("a failed round is retried after its backoff, once per round, up to the bound", func(t *testing.T) {
		p := deliverycleanup.Policy{RetryBackoff: time.Minute, MaxRounds: 3}
		w := expired(t, p)
		w.sweep(w.sched)
		w.failKnown()
		w.rec.report(scopeA, preview, deliverycleanup.RecordFailed)

		rounds, err := w.m.Svc.CleanupRounds(ctx, scopeA, preview, 5)
		if err != nil || len(rounds) != 1 || rounds[0].Disposition != execution.CleanupFailedKnown {
			t.Fatalf("rounds %+v %v", rounds, err)
		}
		// Within the backoff nothing is admitted; it is a visible exception.
		it := one(t, w.sweep(w.sched))
		wantOutcome(t, it, deliverycleanup.OutcomeBackoff)
		w.m.Advance(30 * time.Second)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeBackoff)
		w.m.Advance(31 * time.Second)

		// Due: a second round, claimed by two schedulers, exists once.
		second := w.scheduler(p)
		a := one(t, w.sweep(w.sched))
		b := one(t, w.sweep(second))
		wantOutcome(t, a, deliverycleanup.OutcomeAdmitted)
		wantOutcome(t, b, deliverycleanup.OutcomeInFlight)
		if a.Round != 2 || a.RunID != execution.CleanupRunID(scopeA, preview, 2) || len(w.cleanupRuns()) != 2 {
			t.Fatalf("second round %+v / %d runs", a, len(w.cleanupRuns()))
		}
		w.failKnown()
		// The backoff grows with the round: 2 minutes after the second failure.
		w.m.Advance(time.Minute + 30*time.Second)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeBackoff)
		w.m.Advance(31 * time.Second)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
		w.failKnown()

		// Three rounds are the bound: the failure stays a visible exception.
		w.m.Advance(24 * time.Hour)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeExhausted)
		if len(w.cleanupRuns()) != 3 {
			t.Fatalf("%d runs", len(w.cleanupRuns()))
		}
	})

	t.Run("an unknown outcome is never retried implicitly; reconciliation is explicit and single", func(t *testing.T) {
		p := deliverycleanup.Policy{RetryBackoff: time.Minute}
		w := expired(t, p)
		w.sweep(w.sched)
		w.failUnknown()

		rounds, err := w.m.Svc.CleanupRounds(ctx, scopeA, preview, 5)
		if err != nil || len(rounds) != 1 || rounds[0].Disposition != execution.CleanupOutcomeUnknown {
			t.Fatalf("rounds %+v %v", rounds, err)
		}
		// However much time passes, passes only report it.
		for i := 0; i < 3; i++ {
			w.m.Advance(24 * time.Hour)
			wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeUnknown)
		}
		if len(w.cleanupRuns()) != 1 {
			t.Fatalf("an unknown outcome was retried: %d runs", len(w.cleanupRuns()))
		}

		// The run is held open by the unaccounted effect: no second deleter may
		// start until execution's effect reconciliation settles it.
		if it, err := w.sched.Reconcile(ctx, scopeA, preview); !errors.Is(err, deliverycleanup.ErrInFlight) || it.Outcome != deliverycleanup.OutcomeUnknown {
			t.Fatalf("reconcile of an open unknown run: %+v %v", it, err)
		}
		w.m.Effects.Set(execution.VerdictSettled, nil)
		if _, err := w.m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		rounds, err = w.m.Svc.CleanupRounds(ctx, scopeA, preview, 5)
		if err != nil || len(rounds) != 1 || rounds[0].Status != execution.RunFailed || rounds[0].Disposition != execution.CleanupOutcomeUnknown {
			t.Fatalf("after reconciliation %+v %v", rounds, err)
		}
		w.m.Advance(24 * time.Hour)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeUnknown)

		// Only the explicit reconciliation admits the next round, once.
		it, err := w.sched.Reconcile(ctx, scopeA, preview)
		if err != nil {
			t.Fatal(err)
		}
		wantOutcome(t, it, deliverycleanup.OutcomeAdmitted)
		if it.Round != 2 || len(w.cleanupRuns()) != 2 {
			t.Fatalf("reconcile %+v / %d runs", it, len(w.cleanupRuns()))
		}
		if again, err := w.sched.Reconcile(ctx, scopeA, preview); !errors.Is(err, deliverycleanup.ErrInFlight) || again.Outcome != deliverycleanup.OutcomeInFlight {
			t.Fatalf("a repeat while the round runs: %+v %v", again, err)
		}
		if len(w.cleanupRuns()) != 2 {
			t.Fatal("a repeated reconcile created a run")
		}
		// The reconciling round observes and completes the cleanup.
		w.succeed()
		w.rec.report(scopeA, preview, deliverycleanup.RecordDone)
		if rep := w.sweep(w.sched); len(rep.Items) != 0 {
			t.Fatalf("a reconciled deployment is scheduled again: %+v", rep.Items)
		}
		if _, err := w.sched.Reconcile(ctx, scopeA, preview); !errors.Is(err, deliverycleanup.ErrNothingToReconcile) {
			t.Fatalf("reconcile after deletion: %v", err)
		}
	})

	t.Run("an unknown outcome reported by the cleanup script blocks the retry too", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{RetryBackoff: time.Minute})
		w.sweep(w.sched)
		w.failKnown()
		w.rec.report(scopeA, preview, deliverycleanup.RecordUnknown)
		w.m.Advance(time.Hour)
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeUnknown)
		if len(w.cleanupRuns()) != 1 {
			t.Fatal("a reported unknown outcome was retried")
		}
	})

	t.Run("a succeeded run the record never saw complete is an unknown outcome", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.sweep(w.sched)
		w.succeed()
		// A report that lands after the sweep read the record is not a conflict.
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeUnknown)
		if len(w.cleanupRuns()) != 1 {
			t.Fatal("a completed run was repeated")
		}
		it, err := w.sched.Reconcile(ctx, scopeA, preview)
		if err != nil || it.Round != 2 {
			t.Fatalf("reconcile %+v %v", it, err)
		}
	})

	t.Run("reconcile refuses what it must not touch", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		if _, err := w.sched.Reconcile(ctx, scopeA, preview); !errors.Is(err, deliverycleanup.ErrNothingToReconcile) {
			t.Fatalf("before any round: %v", err)
		}
		if _, err := w.sched.Reconcile(ctx, scopeB, preview); !errors.Is(err, deliverycleanup.ErrNotFound) {
			t.Fatalf("another scope: %v", err)
		}
		if _, err := w.sched.Reconcile(ctx, scopeA, "dep_arbitrary"); !errors.Is(err, deliverycleanup.ErrNotFound) {
			t.Fatalf("an arbitrary deployment: %v", err)
		}
		if len(w.cleanupRuns()) != 0 {
			t.Fatal("reconcile created a run it must not")
		}
	})

	t.Run("a requested cleanup scan resumes across pages", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{Batch: 2, ScanPages: 1})
		ids := []string{"dep_1", "dep_2", "dep_3", "dep_4", "dep_5"}
		for _, id := range ids {
			w.rec.add(scopeA, id, 0)
			w.rec.with(scopeA, id, func(d *recorded) {
				now := w.m.Clock.Now()
				d.RequestedAt, d.State = &now, deliverycleanup.RecordRequested
			})
		}
		seen := map[string]bool{}
		for pass := 0; pass < 4 && len(seen) < len(ids); pass++ {
			for _, it := range w.sweep(w.sched).Items {
				seen[it.Deployment] = true
			}
		}
		if len(seen) != len(ids) || len(w.cleanupRuns()) != len(ids) {
			t.Fatalf("scanned %v, %d runs", seen, len(w.cleanupRuns()))
		}
	})
}

// A page of records that stay blocked (still applying) must not starve the
// expired deployments behind it: the scan resumes where it stopped and wraps.
func TestExpiryDiscoveryIsFairAcrossBlockedRecords(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{Batch: 2, ScanPages: 1})
	for _, id := range []string{"dep_1", "dep_2"} {
		w.rec.add(scopeA, id, time.Hour).Settled = false
	}
	for _, id := range []string{"dep_3", "dep_4", "dep_5"} {
		w.rec.add(scopeA, id, time.Hour)
	}
	w.m.Advance(2 * time.Hour)

	first := w.sweep(w.sched)
	if first.Count(deliverycleanup.OutcomeNotSettled) != 2 || len(w.cleanupRuns()) != 0 {
		t.Fatalf("first page %+v", first.Items)
	}
	second := w.sweep(w.sched)
	if second.Count(deliverycleanup.OutcomeAdmitted) != 2 {
		t.Fatalf("the records behind the blocked page were not reached: %+v", second.Items)
	}
	third := w.sweep(w.sched)
	if third.Count(deliverycleanup.OutcomeAdmitted) != 1 || len(w.cleanupRuns()) != 3 {
		t.Fatalf("the tail was not reached: %+v", third.Items)
	}
	// The scan wraps: the blocked records are looked at again and, once settled, cleaned.
	for _, id := range []string{"dep_1", "dep_2"} {
		w.rec.with(scopeA, id, func(d *recorded) { d.Settled = true })
	}
	if rep := w.sweep(w.sched); rep.Count(deliverycleanup.OutcomeAdmitted) != 2 || len(w.cleanupRuns()) != 5 {
		t.Fatalf("after the wrap: %+v", rep.Items)
	}
}

// The run's first failure reason does not summarize its effects: a known command
// failure on one step recorded first, then another step's ambiguous attempt
// settled by a reconciliation without replay-safe evidence, ends the run with the
// known reason. The round is still an unknown outcome; no automatic retry.
func TestAmbiguousAttemptBehindAKnownFailureStaysUnknown(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deliverycleanup.Policy{RetryBackoff: time.Minute})
	two := w.m.Define(scopeA, pipelinetest.Doc(
		pipelinetest.Container("delete-a", "workspace: {access: none}", "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure]}"),
		pipelinetest.Container("delete-b", "workspace: {access: none}"),
	)).ID
	s := w.scheduler(deliverycleanup.Policy{RetryBackoff: time.Minute}, func(c *deliverycleanup.Config) { c.Definitions = definitions{scopeA: two} })
	w.rec.add(scopeA, preview, time.Hour)
	w.m.Advance(2 * time.Hour)
	wantOutcome(t, one(t, w.sweep(s)), deliverycleanup.OutcomeAdmitted)

	first, second := w.m.MustClaim(tenant), w.m.MustClaim(tenant)
	ambiguousIn, knownIn := first, second
	if first.StepID != "delete-a" {
		ambiguousIn, knownIn = second, first
	}
	// The ambiguous attempt fails without proof the command never ran; the other
	// step fails with a known command failure, which the run retains as its reason.
	if err := w.m.Observe(ambiguousIn, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
		t.Fatal(err)
	}
	w.m.Start(knownIn)
	if _, err := w.m.Fail(knownIn); err != nil {
		t.Fatal(err)
	}
	if err := w.m.Terminate(ambiguousIn, execution.LaunchUnknown); err != nil {
		t.Fatal(err)
	}
	if err := w.m.Terminate(knownIn, execution.LaunchStarted); err != nil {
		t.Fatal(err)
	}
	// Open and unreconciled: unknown, not retryable.
	rounds, err := w.m.Svc.CleanupRounds(ctx, scopeA, preview, 5)
	if err != nil || len(rounds) != 1 || rounds[0].Disposition != execution.CleanupOutcomeUnknown {
		t.Fatalf("before reconciliation %+v %v", rounds, err)
	}
	w.m.Effects.Set(execution.VerdictSettled, nil)
	if _, err := w.m.Svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rounds, err = w.m.Svc.CleanupRounds(ctx, scopeA, preview, 5)
	if err != nil || len(rounds) != 1 || rounds[0].Status != execution.RunFailed {
		t.Fatalf("the run did not end: %+v %v", rounds, err)
	}
	if rounds[0].Reason == execution.ReasonOutcomeUnknown {
		t.Fatalf("the regression needs the known reason to be the run's: %+v", rounds[0])
	}
	if rounds[0].Disposition != execution.CleanupOutcomeUnknown {
		t.Fatalf("an ambiguous attempt without replay-safe evidence was classified %s", rounds[0].Disposition)
	}
	w.m.Advance(24 * time.Hour)
	wantOutcome(t, one(t, w.sweep(s)), deliverycleanup.OutcomeUnknown)
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("an unknown outcome was retried automatically")
	}
}

// The listed candidate can predate a report: it says requested, the script has
// since reported an unknown outcome and the backoff has elapsed. A retry decision
// is made on the current record, so no round is admitted.
func TestRetryDecisionUsesTheCurrentRecord(t *testing.T) {
	w := expired(t, deliverycleanup.Policy{RetryBackoff: time.Minute})
	w.sweep(w.sched)
	w.failKnown()
	stale := w.rec.snapshot(scopeA, preview)
	if stale.State != deliverycleanup.RecordRequested {
		t.Fatalf("state %q", stale.State)
	}
	w.rec.report(scopeA, preview, deliverycleanup.RecordUnknown)
	w.m.Advance(time.Hour)
	listing := w.scheduler(deliverycleanup.Policy{RetryBackoff: time.Minute}, func(c *deliverycleanup.Config) {
		c.Records = staleRecords{Records: w.rec, stale: stale}
	})
	wantOutcome(t, one(t, w.sweep(listing)), deliverycleanup.OutcomeUnknown)
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("a stale listing admitted another round over an unknown outcome")
	}
	// A lookup failure fails closed too.
	w.rec.report(scopeA, preview, deliverycleanup.RecordFailed)
	broken := w.scheduler(deliverycleanup.Policy{RetryBackoff: time.Minute}, func(c *deliverycleanup.Config) {
		c.Records = staleRecords{Records: w.rec, stale: stale, getErr: errors.New("records down")}
	})
	wantOutcome(t, one(t, w.sweep(broken)), deliverycleanup.OutcomeError)
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("a failed record lookup admitted a round")
	}
	// With the current record failed and the backoff elapsed the retry is allowed.
	wantOutcome(t, one(t, w.sweep(listing)), deliverycleanup.OutcomeAdmitted)
}

func TestPolicyAndConstruction(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	base := deliverycleanup.Config{Scopes: scopes{scopeA}, Records: w.rec, Runs: w.m.Svc, Definitions: definitions{}}
	for name, mutate := range map[string]func(*deliverycleanup.Config){
		"scopes":      func(c *deliverycleanup.Config) { c.Scopes = nil },
		"records":     func(c *deliverycleanup.Config) { c.Records = nil },
		"runs":        func(c *deliverycleanup.Config) { c.Runs = nil },
		"definitions": func(c *deliverycleanup.Config) { c.Definitions = nil },
		"rounds":      func(c *deliverycleanup.Config) { c.Policy.MaxRounds = execution.MaxCleanupRounds + 1 },
		"batch":       func(c *deliverycleanup.Config) { c.Policy.Batch = 201 },
		"backoff":     func(c *deliverycleanup.Config) { c.Policy.RetryBackoff = -time.Second },
	} {
		c := base
		mutate(&c)
		if _, err := deliverycleanup.New(c); err == nil {
			t.Errorf("%s: construction accepted an invalid configuration", name)
		}
	}
	if _, err := deliverycleanup.New(base); err != nil {
		t.Fatal(err)
	}
	s, _ := deliverycleanup.New(base)
	if err := s.Loop(context.Background(), 0); err == nil {
		t.Fatal("a zero interval was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Loop(ctx, time.Hour); err != nil {
		t.Fatalf("a cancelled loop: %v", err)
	}
}
