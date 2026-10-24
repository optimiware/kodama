package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/workspace"
)

// storeSubject is one gitwebhook.Store under test with what the contract needs
// to seed around it.
type storeSubject struct {
	// Store is a handle over the shared persistence; Restart returns a new one
	// over the same persistence, as after a process restart.
	Store   gw.Store
	Restart func() gw.Store
	// ScopeA and ScopeB are scopes of different tenants.
	ScopeA, ScopeB domain.Scope
	// NewTrigger returns a valid, enabled trigger in scope with its references
	// (definition, work item) seeded where the implementation requires it.
	NewTrigger func(t testing.TB, scope domain.Scope) gw.Trigger
	// NewRun returns the identifier of a run that exists in scope.
	NewRun func(t testing.TB, scope domain.Scope) domain.RunID
	// Audits counts the audit records of the trigger.
	Audits func(t testing.TB, scope domain.Scope, trigger gw.TriggerID) int
}

type openStore func(t testing.TB) storeSubject

var clock = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func audit(action string, t gw.Trigger, delivery string) gw.AuditRecord {
	return gw.AuditRecord{Action: action, Scope: t.Scope, TriggerID: t.ID, DeliveryID: delivery, Principal: "", At: clock, Metadata: map[string]string{"ref": "refs/heads/main"}}
}

func claimOf(t gw.Trigger, delivery, digest string) gw.DeliveryRecord {
	return gw.DeliveryRecord{
		Key: gw.DeliveryKey{TriggerID: t.ID, DeliveryID: delivery}, Scope: t.Scope, BodyDigest: digest, Ref: "refs/heads/main",
		Commit: workspace.Commit("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), ReceivedAt: clock,
	}
}

// storeContract is the behavior of every gitwebhook.Store the delivery protocol
// depends on: atomic claims, write-once outcomes, the admission fence and lease,
// optimistic trigger updates and audit written with the change it describes.
func storeContract(t *testing.T, open openStore) {
	ctx := context.Background()

	t.Run("a trigger is stored with its owner and replaced only at its version", func(t *testing.T) {
		s := open(t)
		tr := s.NewTrigger(t, s.ScopeA)
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a reused address = %v, want ErrConflict", err)
		}
		got, err := s.Store.Trigger(ctx, tr.ID)
		if err != nil || got.Scope != s.ScopeA || got.ID != tr.ID || got.RepositoryID != tr.RepositoryID || got.DefinitionID != tr.DefinitionID ||
			got.SecretRef != tr.SecretRef || !got.Enabled || got.Version != 1 || fmt.Sprint(got.Branches) != fmt.Sprint(tr.Branches) {
			t.Fatalf("trigger = %+v, %v", got, err)
		}
		if _, err := s.Store.Trigger(ctx, "trg-unknown"); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("unknown trigger = %v", err)
		}
		next := got
		next.Enabled = false
		next.UpdatedAt = clock.Add(time.Minute)
		if _, err := s.Store.UpdateTrigger(ctx, next, 7, audit("git_trigger.disabled", next, "")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale version = %v, want ErrConflict", err)
		}
		foreign := next
		foreign.Scope = s.ScopeB
		if _, err := s.Store.UpdateTrigger(ctx, foreign, 1, audit("git_trigger.disabled", foreign, "")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("another tenant's update = %v, want ErrNotFound", err)
		}
		upd, err := s.Store.UpdateTrigger(ctx, next, 1, audit("git_trigger.disabled", next, ""))
		if err != nil || upd.Version != 2 || upd.Enabled {
			t.Fatalf("update = %+v, %v", upd, err)
		}
		if again, err := s.Store.Trigger(ctx, tr.ID); err != nil || again.Enabled || again.Version != 2 {
			t.Fatalf("after update = %+v, %v", again, err)
		}
		if _, err := s.Store.UpdateTrigger(ctx, next, 1, audit("git_trigger.disabled", next, "")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("the replaced version = %v, want ErrConflict", err)
		}
		if n := s.Audits(t, s.ScopeA, tr.ID); n != 2 {
			t.Fatalf("%d audit records, want one per committed change (2)", n)
		}
		if n := s.Audits(t, s.ScopeB, tr.ID); n != 0 {
			t.Fatalf("another tenant sees %d audit records", n)
		}
	})

	t.Run("two concurrent identical deliveries produce one claim", func(t *testing.T) {
		s := open(t)
		tr := s.NewTrigger(t, s.ScopeA)
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
			t.Fatal(err)
		}
		const n = 12
		recs := make([]gw.DeliveryRecord, n)
		created := make([]bool, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				recs[i], created[i], errs[i] = s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "digest-1"))
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("claim %d: %v", i, errs[i])
			}
			if created[i] {
				won++
			}
			if recs[i].Key != recs[0].Key || recs[i].BodyDigest != "digest-1" || recs[i].Done || recs[i].Scope != s.ScopeA {
				t.Fatalf("claim %d returned %+v", i, recs[i])
			}
		}
		if won != 1 {
			t.Fatalf("%d callers created the claim, want exactly one", won)
		}
	})

	t.Run("a replay returns the stored outcome and the first finish wins", func(t *testing.T) {
		s := open(t)
		tr := s.NewTrigger(t, s.ScopeA)
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
			t.Fatal(err)
		}
		key := gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: "dlv-1"}
		if _, created, err := s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "digest-1")); err != nil || !created {
			t.Fatalf("claim: %v %v", created, err)
		}
		done, err := s.Store.FinishDelivery(ctx, key, gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonBranchFiltered}, audit("git_hook.ignored", tr, "dlv-1"))
		if err != nil || !done.Done || done.Outcome != gw.OutcomeIgnored || done.Reason != gw.ReasonBranchFiltered {
			t.Fatalf("finish = %+v, %v", done, err)
		}
		// A second, different finish returns the first and writes no audit.
		other, err := s.Store.FinishDelivery(ctx, key, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonInstallerDenied}, audit("git_hook.rejected", tr, "dlv-1"))
		if err != nil || other.Outcome != gw.OutcomeIgnored || other.Reason != gw.ReasonBranchFiltered {
			t.Fatalf("second finish = %+v, %v; the first must win", other, err)
		}
		for _, handle := range []gw.Store{s.Store, s.Restart()} {
			rec, created, err := handle.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "digest-1"))
			if err != nil || created || !rec.Done || rec.Outcome != gw.OutcomeIgnored || rec.Reason != gw.ReasonBranchFiltered || rec.CompletedAt.IsZero() {
				t.Fatalf("replay = %+v created=%v, %v", rec, created, err)
			}
		}
		if rec, _, _ := s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "digest-2")); rec.BodyDigest != "digest-1" {
			t.Fatalf("a replay with another body must still return the stored record, got %+v", rec)
		}
		if n := s.Audits(t, s.ScopeA, tr.ID); n != 2 {
			t.Fatalf("%d audit records, want install + the first finish (2)", n)
		}
	})

	t.Run("deliveries are keyed by trigger and delivery identifier", func(t *testing.T) {
		s := open(t)
		a, b := s.NewTrigger(t, s.ScopeA), s.NewTrigger(t, s.ScopeB)
		for _, tr := range []gw.Trigger{a, b} {
			if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range []gw.DeliveryRecord{claimOf(a, "dlv-1", "x"), claimOf(a, "dlv-2", "x"), claimOf(b, "dlv-1", "x")} {
			if _, created, err := s.Store.ClaimDelivery(ctx, c); err != nil || !created {
				t.Fatalf("claim %+v: %v %v", c.Key, created, err)
			}
		}
		if _, err := s.Store.FinishDelivery(ctx, gw.DeliveryKey{TriggerID: a.ID, DeliveryID: "dlv-1"}, gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonBranchFiltered}, audit("git_hook.ignored", a, "dlv-1")); err != nil {
			t.Fatal(err)
		}
		for _, other := range []gw.DeliveryRecord{claimOf(a, "dlv-2", "x"), claimOf(b, "dlv-1", "x")} {
			rec, created, err := s.Store.ClaimDelivery(ctx, other)
			if err != nil || created || rec.Done {
				t.Fatalf("an unrelated delivery %+v was affected: %+v %v %v", other.Key, rec, created, err)
			}
		}
	})

	t.Run("the admission fence and lease keep success and refusal exclusive", func(t *testing.T) {
		s := open(t)
		tr := s.NewTrigger(t, s.ScopeA)
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
			t.Fatal(err)
		}
		fin := func(key gw.DeliveryKey, f gw.Finish) error {
			_, err := s.Store.FinishDelivery(ctx, key, f, audit("git_hook.rejected", tr, key.DeliveryID))
			return err
		}
		claim := func(id string) gw.DeliveryKey {
			if _, _, err := s.Store.ClaimDelivery(ctx, claimOf(tr, id, "d")); err != nil {
				t.Fatal(err)
			}
			return gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: id}
		}
		k := claim("dlv-1")
		if _, _, err := s.Store.AcquireAdmission(ctx, k, "A", clock, time.Minute); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a lease before admission began = %v, want ErrConflict", err)
		}
		if err := fin(k, gw.Finish{Outcome: gw.OutcomeStarted, RunID: "run-x", Admission: true}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("an admission finish before admission began = %v, want ErrConflict", err)
		}
		rec, err := s.Store.BeginAdmission(ctx, k)
		if err != nil || !rec.Admitting {
			t.Fatalf("begin = %+v, %v", rec, err)
		}
		if again, err := s.Store.BeginAdmission(ctx, k); err != nil || !again.Admitting {
			t.Fatalf("begin is idempotent: %+v, %v", again, err)
		}
		// Once admitting, a pre-admission result is fenced out.
		if err := fin(k, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonInstallerDenied}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a pre-admission rejection of an admitting delivery = %v, want ErrConflict", err)
		}
		a, ok, err := s.Store.AcquireAdmission(ctx, k, "A", clock, time.Minute)
		if err != nil || !ok || a.LeaseHolder != "A" || !a.LeaseUntil.Equal(clock.Add(time.Minute)) {
			t.Fatalf("A acquires: %+v %v %v", a, ok, err)
		}
		if _, ok, err := s.Store.AcquireAdmission(ctx, k, "B", clock.Add(time.Second), time.Minute); err != nil || ok {
			t.Fatalf("B must wait for A's lease: ok=%v err=%v", ok, err)
		}
		if _, ok, err := s.Store.AcquireAdmission(ctx, k, "A", clock.Add(time.Second), time.Minute); err != nil || !ok {
			t.Fatalf("A re-acquires its own lease: ok=%v err=%v", ok, err)
		}
		if err := s.Store.ReleaseAdmission(ctx, k, "B"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Store.AcquireAdmission(ctx, k, "B", clock.Add(2*time.Second), time.Minute); ok {
			t.Fatal("a release by a non-holder must not drop A's lease")
		}
		// A's lease expires: B takes over, and A (stale) can no longer reject.
		b, ok, err := s.Store.AcquireAdmission(ctx, k, "B", clock.Add(2*time.Minute), time.Minute)
		if err != nil || !ok || b.LeaseHolder != "B" {
			t.Fatalf("B acquires after expiry: %+v %v %v", b, ok, err)
		}
		if err := fin(k, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Admission: true, Holder: "A"}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale holder's rejection = %v, want ErrConflict", err)
		}
		if err := fin(k, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Admission: true}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a holderless admission rejection = %v, want ErrConflict", err)
		}
		if err := fin(k, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Admission: true, Holder: "B"}); err != nil {
			t.Fatalf("the holder's rejection: %v", err)
		}
		done, _, err := s.Store.AcquireAdmission(ctx, k, "C", clock.Add(10*time.Minute), time.Minute)
		if err != nil || !done.Done || done.Outcome != gw.OutcomeRejected || done.LeaseHolder != "" {
			t.Fatalf("a finished delivery is returned unchanged: %+v %v", done, err)
		}

		// A started finish needs no lease; the run is the outcome.
		k2 := claim("dlv-2")
		if _, err := s.Store.BeginAdmission(ctx, k2); err != nil {
			t.Fatal(err)
		}
		run := s.NewRun(t, s.ScopeA)
		if err := fin(k2, gw.Finish{Outcome: gw.OutcomeStarted, RunID: run, Admission: true}); err != nil {
			t.Fatalf("started: %v", err)
		}
		got, _, _ := s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-2", "d"))
		if !got.Done || got.Outcome != gw.OutcomeStarted || got.RunID != run {
			t.Fatalf("started delivery = %+v", got)
		}
		// Neither outcome can be turned into the other.
		if err := fin(k2, gw.Finish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Admission: true, Holder: "B"}); err != nil {
			t.Fatalf("a later finish returns the first, unchanged: %v", err)
		}
		if got, _, _ := s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-2", "d")); got.Outcome != gw.OutcomeStarted || got.RunID != run {
			t.Fatalf("a started delivery was rewritten: %+v", got)
		}
	})

	t.Run("a released lease can be taken at once and unknown deliveries are not found", func(t *testing.T) {
		s := open(t)
		tr := s.NewTrigger(t, s.ScopeA)
		if err := s.Store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
			t.Fatal(err)
		}
		k := gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: "dlv-1"}
		if _, _, err := s.Store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "d")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.BeginAdmission(ctx, k); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Store.AcquireAdmission(ctx, k, "A", clock, time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.Store.ReleaseAdmission(ctx, k, "A"); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.Store.AcquireAdmission(ctx, k, "B", clock, time.Hour); err != nil || !ok || got.LeaseHolder != "B" {
			t.Fatalf("B after a release: %+v %v %v", got, ok, err)
		}
		missing := gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: "dlv-none"}
		if _, err := s.Store.BeginAdmission(ctx, missing); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unclaimed delivery = %v", err)
		}
		if _, err := s.Store.BeginAdmission(ctx, gw.DeliveryKey{TriggerID: "trg-none", DeliveryID: "d"}); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unknown trigger = %v", err)
		}
	})
}
