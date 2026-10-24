package memory_test

import (
	"context"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/execution"
)

// TestUnitTransitionOutbox proves PX-05 on the memory store: every transition
// commits with its history, audit and catalog event; the outbox deduplicates;
// and reconciliation tolerates a missing, duplicated or reordered cancellation
// wakeup.
func TestUnitTransitionOutbox(t *testing.T) {
	ctx := context.Background()
	tenant := scopeA.TenantID

	t.Run("history, audit and events commit with each transition", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		in := m.MustClaim(tenant)
		m.Start(in)
		if _, err := m.Succeed(in, nil, ""); err != nil {
			t.Fatal(err)
		}
		hist, err := m.Mem.History(ctx, scopeA, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := m.Mem.AuditRows(ctx, scopeA)
		if err != nil || len(audit) != len(hist) {
			t.Fatalf("%d history rows, %d audit rows (%v)", len(hist), len(audit), err)
		}
		var runs, attempts int
		for _, h := range hist {
			switch h.Subject {
			case execution.SubjectRun:
				runs++
			case execution.SubjectAttempt:
				attempts++
			}
		}
		evs, err := m.World.Events(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		var created, runChanged, attemptChanged int
		for _, e := range evs {
			if ids[e.ID] {
				t.Fatalf("duplicate event %s", e.ID)
			}
			ids[e.ID] = true
			if e.TenantID != tenant || e.ProjectID != scopeA.ProjectID {
				t.Fatalf("event of %s/%s", e.TenantID, e.ProjectID)
			}
			switch e.Type {
			case eventsv1.TypeRunCreated:
				created++
			case eventsv1.TypeRunStateChanged:
				runChanged++
			case eventsv1.TypeStepAttemptStateChange:
				attemptChanged++
			}
		}
		if created != 1 || created+runChanged != runs || attemptChanged != attempts {
			t.Fatalf("events %d created + %d run + %d attempt for %d run and %d attempt transitions", created, runChanged, attemptChanged, runs, attempts)
		}
		if got := m.Get(run).Run; got.Status != execution.RunSucceeded || hist[len(hist)-1].RunVersion != got.Version {
			t.Fatalf("run %s at version %d, last history at %d", got.Status, got.Version, hist[len(hist)-1].RunVersion)
		}
	})

	t.Run("a lost cancellation wakeup is repaired by the periodic scan", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		in := m.MustClaim(tenant)
		m.Start(in)
		m.CancelWork(scopeA, p.Item) // the event never reached the consumer
		if got := m.Get(run).Run; got.CancelRequestedAt != nil {
			t.Fatal("cancellation arrived without a wakeup")
		}
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := m.Get(run)
		if agg.Run.CancelRequestedAt == nil || agg.Run.CancelReason != execution.ReasonWorkCancelled || agg.Control(in.AttemptID).Live() {
			t.Fatalf("the scan did not cancel the run: %s", agg)
		}
	})

	t.Run("duplicate and reordered cancellation wakeups are idempotent", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		in := m.MustClaim(tenant)
		m.Start(in)
		for _, version := range []int{3, 2, 3, 1} { // newest first, then stale and repeated deliveries
			if err := m.Svc.RequestCancellation(ctx, execution.WorkCancellation{Scope: scopeA, WorkItemID: p.Item, Version: versionOf(version)}); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := m.World.Events(ctx, tenant)
		for i := 0; i < 3; i++ {
			if _, err := m.Svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
		}
		after, _ := m.World.Events(ctx, tenant)
		if len(after) != len(before) {
			t.Fatalf("reconciling a settled cancellation published %d events", len(after)-len(before))
		}
		agg := m.Get(run)
		if agg.Run.CancelRequestedAt == nil || len(agg.Attempts) != 1 {
			t.Fatalf("run %s attempts %d", agg.Run.Status, len(agg.Attempts))
		}
	})
}
