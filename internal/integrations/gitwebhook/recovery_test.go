package gitwebhook_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/workspace"
)

func keyOf(f *gitwebhooktest.Fixture, id string) gw.DeliveryKey {
	return gw.DeliveryKey{TriggerID: f.Trigger.ID, DeliveryID: id}
}

func countAudits(f *gitwebhooktest.Fixture, action string) (n int) {
	for _, a := range f.Store.Audits() {
		if a.Action == action {
			n++
		}
	}
	return n
}

// Ignored and refused deliveries are claimed and recorded like any other, so
// their identifiers are deduplicated whatever the outcome.
func duplicateOutcomes(t *testing.T) {
	t.Run("an ignored delivery is recorded and a changed body conflicts", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		topic := gitwebhooktest.Marshal(t, f.Payload("refs/heads/topic", gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", topic))
		if err != nil || r.Outcome != gw.OutcomeIgnored || r.Duplicate {
			t.Fatalf("first: %+v, %v", r, err)
		}
		rec, ok := f.Store.Delivery(keyOf(f, "d1"))
		if !ok || !rec.Done || rec.Outcome != gw.OutcomeIgnored || rec.Reason != gw.ReasonBranchFiltered {
			t.Fatalf("ignored outcome not persisted: %+v", rec)
		}
		// The same identifier with a body that WOULD match must not start a run.
		r, err = f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA)))
		if !errors.Is(err, gw.ErrDeliveryConflict) || r.RunID != "" {
			t.Fatalf("changed body: %+v, %v", r, err)
		}
		assertNoWork(t, f)
		// Replaying the identical body reports the recorded result.
		r, err = f.Service.Receive(t.Context(), f.Sign("d1", topic))
		if err != nil || !r.Duplicate || r.Outcome != gw.OutcomeIgnored || r.Reason != gw.ReasonBranchFiltered {
			t.Fatalf("replay: %+v, %v", r, err)
		}
		if countAudits(f, "git_hook.ignored") != 1 {
			t.Fatalf("ignored outcome audited %d times", countAudits(f, "git_hook.ignored"))
		}
	})

	t.Run("a foreign-repository refusal reserves its identifier", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		foreign := f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA))
		foreign.Repository = "repo-other"
		body := gitwebhooktest.Marshal(t, foreign)
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", body)); !errors.Is(err, gw.ErrForeignRepository) {
			t.Fatal(err)
		}
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))); !errors.Is(err, gw.ErrDeliveryConflict) {
			t.Fatalf("changed body after refusal: %v", err)
		}
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", body))
		if !errors.Is(err, gw.ErrForeignRepository) || !errors.Is(err, gw.ErrRejected) || !r.Duplicate || r.Reason != gw.ReasonForeignRepository {
			t.Fatalf("replay: %+v, %v", r, err)
		}
		assertNoWork(t, f)
		if countAudits(f, "git_hook.rejected") != 2 { // the refusal + the conflict
			t.Fatalf("audits: %+v", f.Store.Audits())
		}
	})

	t.Run("a finished delivery replays through repository changes and outages", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		first, err := f.Service.Receive(t.Context(), n)
		if err != nil || first.RunID == "" {
			t.Fatal(first, err)
		}
		f.Repos.Err = errors.New("configuration store down")
		r, err := f.Service.Receive(t.Context(), n)
		if err != nil || !r.Duplicate || r.RunID != first.RunID {
			t.Fatalf("during repository outage: %+v, %v", r, err)
		}
		f.Repos.Err = nil
		f.Repos.Set(f.Scope, workspace.RepositoryConfig{ID: "repo-replacement", URL: "https://git.example.test/x.git", CheckoutSecretRef: gitwebhooktest.CheckoutRef})
		r, err = f.Service.Receive(t.Context(), n)
		if err != nil || !r.Duplicate || r.RunID != first.RunID {
			t.Fatalf("after repository rebound: %+v, %v", r, err)
		}
		if f.Runs.Created() != 1 || f.Git.CallCount() != 1 {
			t.Fatalf("replay repeated work: runs=%d git=%d", f.Runs.Created(), f.Git.CallCount())
		}
	})

	t.Run("a refusal recorded while pending is replayed, not re-decided", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Repos.Err = errors.New("configuration store down")
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if rec, ok := f.Store.Delivery(keyOf(f, "d1")); !ok || rec.Done {
			t.Fatalf("an outage must leave the claim pending: %+v", rec)
		}
		f.Repos.Err = nil
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.Outcome != gw.OutcomeStarted || r.Duplicate {
			t.Fatalf("resume: %+v, %v", r, err)
		}
	})
}

// A delivery whose admission may have succeeded recovers the actual run; later
// revocation or branch movement can neither lose it nor turn it into a refusal.
func TestAdmissionRecovery(t *testing.T) {
	disturb := func(f *gitwebhooktest.Fixture) {
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		f.Git.SetBranch(gitwebhooktest.MainRef, workspace.BranchInfo{Tip: gitwebhooktest.CommitC, Exists: true, Contains: false})
	}
	for name, lose := range map[string]func(f *gitwebhooktest.Fixture){
		"response lost after admission": func(f *gitwebhooktest.Fixture) { f.Runs.LoseResponses = 1 },
		"finish lost after admission":   func(f *gitwebhooktest.Fixture) { f.Store.FailFinishes = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			f := gitwebhooktest.NewFixture(t)
			n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
			lose(f)
			if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
				t.Fatalf("got %v", err)
			}
			rec, _ := f.Store.Delivery(keyOf(f, "d1"))
			if rec.Done || !rec.Admitting || f.Runs.Created() != 1 {
				t.Fatalf("expected an admitting delivery with one run: %+v runs=%d", rec, f.Runs.Created())
			}
			disturb(f)
			gitCalls, started := f.Git.CallCount(), f.Runs.Started()
			r, err := f.Service.Receive(t.Context(), n)
			if err != nil || r.Outcome != gw.OutcomeStarted || r.RunID != "run-a" {
				t.Fatalf("recovery: %+v, %v", r, err)
			}
			if f.Runs.Created() != 1 || f.Runs.Started() != started || f.Git.CallCount() != gitCalls {
				t.Fatalf("recovery re-decided: runs=%d started=%d->%d git=%d->%d", f.Runs.Created(), started, f.Runs.Started(), gitCalls, f.Git.CallCount())
			}
			rec, _ = f.Store.Delivery(keyOf(f, "d1"))
			if !rec.Done || rec.Outcome != gw.OutcomeStarted || rec.RunID != "run-a" {
				t.Fatalf("final record: %+v", rec)
			}
			if countAudits(f, "git_hook.run_started") != 1 || countAudits(f, "git_hook.rejected") != 0 {
				t.Fatalf("audit: %+v", f.Store.Audits())
			}
			// And it stays that way.
			if r, err = f.Service.Receive(t.Context(), n); err != nil || !r.Duplicate || r.RunID != "run-a" {
				t.Fatalf("replay: %+v, %v", r, err)
			}
		})
	}

	t.Run("a lookup outage during recovery stays retryable", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Store.FailFinishes = 1
		_, _ = f.Service.Receive(t.Context(), n)
		f.Runs.FindErr = errors.New("admission store down")
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if rec, _ := f.Store.Delivery(keyOf(f, "d1")); rec.Done {
			t.Fatal("an outage finished the delivery")
		}
		f.Runs.FindErr = nil
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" || f.Runs.Created() != 1 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})

	t.Run("an admitted-but-unrecorded delivery whose admission then refuses is a recorded refusal", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.SetError(errors.New("down"))
		_, _ = f.Service.Receive(t.Context(), n)
		f.Runs.SetError(fmt.Errorf("%w: installer lost run authority", gw.ErrRunRefused))
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused || f.Runs.Created() != 0 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})

	t.Run("concurrent conflicting observations cannot reject an admitted run", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))

		bAtAuthorize := make(chan struct{}) // B has claimed and is about to check authority
		releaseB := make(chan struct{})     // let B evaluate (after revocation)
		aInAdmission := make(chan struct{}) // A has begun admission and is inside StartRun
		releaseA := make(chan struct{})     // let A's StartRun proceed
		bEvaluated := make(chan struct{})   // B's (denying) decision is fixed
		f.Auth.Gate = func(call int) {
			if call == 1 { // B
				close(bAtAuthorize)
				<-releaseB
			}
		}
		f.Auth.After = func(call int) {
			if call == 1 {
				close(bEvaluated)
			}
		}
		f.Runs.Gate = func(call int) {
			if call == 1 { // A
				close(aInAdmission)
				<-releaseA
			}
		}

		type result struct {
			r   gw.Receipt
			err error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		run := func() {
			defer wg.Done()
			r, err := f.Service.Receive(t.Context(), n)
			results <- result{r, err}
		}
		wg.Add(1)
		go run() // B: first to check authority, parked there
		<-bAtAuthorize
		wg.Add(1)
		go run() // A: passes every check and begins admission
		<-aInAdmission

		// Observations change: B will now see a revoked installer.
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		close(releaseB)
		<-bEvaluated
		// B now holds a denying decision while A is mid-admission: B's rejection
		// must lose to A's admission. Let A finish.
		close(releaseA)
		wg.Wait()
		close(results)

		// The processor that lost the race is either told to retry (the lease is
		// A's) or, if A had already finished, shown A's result. Never a rejection.
		var ids []domain.RunID
		for res := range results {
			switch {
			case res.err == nil:
				ids = append(ids, res.r.RunID)
			case errors.Is(res.err, gw.ErrAdmissionBusy) && errors.Is(res.err, gw.ErrUnavailable):
			default:
				t.Fatalf("a processor failed: %+v, %v", res.r, res.err)
			}
		}
		if len(ids) == 0 {
			t.Fatal("no processor admitted the run")
		}
		for _, id := range ids {
			if id != "run-a" {
				t.Fatalf("run ids %v", ids)
			}
		}
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" || !r.Duplicate {
			t.Fatalf("retry of the loser: %+v, %v", r, err)
		}
		rec, _ := f.Store.Delivery(keyOf(f, "d1"))
		if !rec.Done || rec.Outcome != gw.OutcomeStarted || rec.RunID != "run-a" || f.Runs.Created() != 1 {
			t.Fatalf("final: %+v runs=%d", rec, f.Runs.Created())
		}
		if countAudits(f, "git_hook.rejected") != 0 || countAudits(f, "git_hook.run_started") != 1 {
			t.Fatalf("audit: %+v", f.Store.Audits())
		}
	})

	t.Run("a rejection recorded first wins and admission never starts", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrRejected) {
			t.Fatal(err)
		}
		f.Auth.Grant(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || !r.Duplicate || f.Runs.Created() != 0 || f.Runs.Started() != 0 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
}

// Success and refusal are mutually exclusive durable admission outcomes: while
// one processor may still be creating the run, no other can record a refusal.
func TestAdmissionLeaseExcludesRefusal(t *testing.T) {
	t.Run("a refusal cannot be recorded while another processor is inside StartRun", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		inStart, releaseA := make(chan struct{}), make(chan struct{})
		f.Runs.Gate = func(call int) {
			if call == 1 { // A
				close(inStart)
				<-releaseA
			}
		}
		type result struct {
			r   gw.Receipt
			err error
		}
		aDone := make(chan result, 1)
		go func() {
			r, err := f.Service.Receive(t.Context(), n)
			aDone <- result{r, err}
		}()
		<-inStart

		// B observes a permanent refusal from admission.
		f.Runs.SetError(fmt.Errorf("%w: definition currently refused", gw.ErrRunRefused))
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrAdmissionBusy) || r.Outcome != "" {
			t.Fatalf("B must be fenced out: %+v, %v", r, err)
		}
		if rec, _ := f.Store.Delivery(keyOf(f, "d1")); rec.Done {
			t.Fatalf("B recorded a terminal outcome: %+v", rec)
		}
		// The refusal clears, and A proceeds and succeeds.
		f.Runs.SetError(nil)
		close(releaseA)
		a := <-aDone
		if a.err != nil || a.r.Outcome != gw.OutcomeStarted || a.r.RunID != "run-a" {
			t.Fatalf("A: %+v, %v", a.r, a.err)
		}
		rec, _ := f.Store.Delivery(keyOf(f, "d1"))
		if !rec.Done || rec.Outcome != gw.OutcomeStarted || rec.RunID != "run-a" || f.Runs.Created() != 1 {
			t.Fatalf("final: %+v runs=%d", rec, f.Runs.Created())
		}
		if countAudits(f, "git_hook.rejected") != 0 || countAudits(f, "git_hook.run_started") != 1 {
			t.Fatalf("audit: %+v", f.Store.Audits())
		}
	})

	t.Run("a holder that vanished only delays others until its lease expires", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		inStart, releaseA := make(chan struct{}), make(chan struct{})
		f.Runs.Gate = func(call int) {
			if call == 1 {
				close(inStart)
				<-releaseA
			}
		}
		aDone := make(chan error, 1)
		go func() { _, err := f.Service.Receive(t.Context(), n); aDone <- err }()
		<-inStart
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", n.Body)); !errors.Is(err, gw.ErrAdmissionBusy) {
			t.Fatalf("during the lease: %v", err)
		}
		f.Clock.Advance(gw.AdmissionLease + gw.MaxClockSkew)
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", n.Body))
		if err != nil || r.RunID != "run-a" {
			t.Fatalf("after expiry: %+v, %v", r, err)
		}
		close(releaseA)
		if err := <-aDone; err != nil {
			t.Fatalf("A after being fenced by expiry must converge on the recorded run: %v", err)
		}
		if f.Runs.Created() != 1 {
			t.Fatalf("%d runs", f.Runs.Created())
		}
	})

	t.Run("an outage releases the lease at once", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.SetError(errors.New("down"))
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) || errors.Is(err, gw.ErrAdmissionBusy) {
			t.Fatalf("got %v", err)
		}
		f.Runs.SetError(nil)
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" {
			t.Fatalf("retry: %+v, %v", r, err)
		}
	})
}

// Recovery of an Admitting delivery that has no run still validates the
// trigger's repository binding before creating one.
func TestAdmissionRecoveryRevalidatesRepository(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
	f.Runs.SetError(errors.New("admission database down"))
	if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if rec, _ := f.Store.Delivery(keyOf(f, "d1")); !rec.Admitting || rec.Done || f.Runs.Created() != 0 {
		t.Fatalf("expected an admitting delivery without a run: %+v", rec)
	}
	attempts := f.Runs.Started()

	f.Repos.Set(f.Scope, workspace.RepositoryConfig{
		ID: "repo-replacement", URL: "https://git.example.test/other.git", CheckoutSecretRef: gitwebhooktest.CheckoutRef,
		PublisherSecretRef: gitwebhooktest.PublisherRef, BaseRef: "main",
	})
	f.Runs.SetError(nil)
	r, err := f.Service.Receive(t.Context(), n)
	if !errors.Is(err, gw.ErrForeignRepository) || !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonRepositoryRebound {
		t.Fatalf("got %+v, %v", r, err)
	}
	if f.Runs.Created() != 0 || f.Runs.Started() != attempts {
		t.Fatalf("a run was admitted for a rebound project: runs=%d attempts %d->%d", f.Runs.Created(), attempts, f.Runs.Started())
	}
	if rec, _ := f.Store.Delivery(keyOf(f, "d1")); !rec.Done || rec.Outcome != gw.OutcomeRejected {
		t.Fatalf("final: %+v", rec)
	}

	t.Run("an existing run is still recovered receipt-first after a rebound", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.LoseResponses = 1
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatal(err)
		}
		f.Repos.Set(f.Scope, workspace.RepositoryConfig{ID: "repo-replacement", URL: "https://git.example.test/o.git", CheckoutSecretRef: gitwebhooktest.CheckoutRef})
		r, err := f.Service.Receive(t.Context(), n)
		if err != nil || r.RunID != "run-a" || f.Runs.Created() != 1 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
	t.Run("a repository outage during recovery stays retryable", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.SetError(errors.New("down"))
		_, _ = f.Service.Receive(t.Context(), n)
		f.Runs.SetError(nil)
		f.Repos.Err = errors.New("configuration store down")
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) || f.Runs.Created() != 0 {
			t.Fatalf("got %v runs=%d", err, f.Runs.Created())
		}
		f.Repos.Err = nil
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" {
			t.Fatalf("retry: %+v, %v", r, err)
		}
	})
}

// The refusal lives in admission's own ledger, so a holder that stalled before
// its StartRun, lost its lease and resumes after a replacement recorded the
// refusal cannot create a run.
func TestStaleHolderCannotCreateRunAfterRefusal(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))

	parked, releaseA := make(chan struct{}), make(chan struct{})
	f.Repos.Gate = func(call int) {
		// Call 1 is the fixture's installation, call 2 the pre-admission binding
		// check and call 3 A's lookup under its lease, before its StartRun
		// timeout has begun.
		if call == 3 {
			close(parked)
			<-releaseA
		}
	}
	type result struct {
		r   gw.Receipt
		err error
	}
	aDone := make(chan result, 1)
	go func() {
		r, err := f.Service.Receive(t.Context(), n)
		aDone <- result{r, err}
	}()
	<-parked
	if f.Runs.Started() != 0 {
		t.Fatal("A reached StartRun")
	}

	// A's lease expires; B replaces it and admission permanently refuses.
	f.Clock.Advance(gw.AdmissionLease + time.Second)
	f.Runs.SetError(fmt.Errorf("%w: definition refused", gw.ErrRunRefused))
	r, err := f.Service.Receive(t.Context(), f.Sign("d1", n.Body))
	if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused {
		t.Fatalf("B: %+v, %v", r, err)
	}

	// Conditions recover; the old holder resumes.
	f.Runs.SetError(nil)
	close(releaseA)
	a := <-aDone
	if !errors.Is(a.err, gw.ErrRejected) || a.r.RunID != "" {
		t.Fatalf("A must see the recorded refusal: %+v, %v", a.r, a.err)
	}
	if f.Runs.Created() != 0 {
		t.Fatalf("a rejected delivery acquired %d unrecorded run(s)", f.Runs.Created())
	}
	rec, _ := f.Store.Delivery(keyOf(f, "d1"))
	if !rec.Done || rec.Outcome != gw.OutcomeRejected || rec.RunID != "" {
		t.Fatalf("final: %+v", rec)
	}
	if countAudits(f, "git_hook.run_started") != 0 || countAudits(f, "git_hook.rejected") != 1 {
		t.Fatalf("audit: %+v", f.Store.Audits())
	}
	// And the decision is stable under replay.
	if r, err = f.Service.Receive(t.Context(), f.Sign("d1", n.Body)); !errors.Is(err, gw.ErrRejected) || !r.Duplicate {
		t.Fatalf("replay: %+v, %v", r, err)
	}
}

func TestRefusalLedger(t *testing.T) {
	t.Run("the first refusal reason stands when the finish is lost", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		// Reach the admission phase with the original binding, then rebind.
		f.Runs.SetError(errors.New("down"))
		_, _ = f.Service.Receive(t.Context(), n)
		f.Repos.Set(f.Scope, workspace.RepositoryConfig{ID: "repo-replacement", URL: "https://git.example.test/o.git", CheckoutSecretRef: gitwebhooktest.CheckoutRef})
		f.Store.FailFinishes = 1
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("lost finish: %v", err)
		}
		// The binding is restored, but the durable decision is already made.
		f.Repos.Set(f.Scope, workspace.RepositoryConfig{ID: gitwebhooktest.RepositoryName, URL: gitwebhooktest.RepositoryURL, CheckoutSecretRef: gitwebhooktest.CheckoutRef})
		f.Runs.SetError(nil)
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonRepositoryRebound || f.Runs.Created() != 0 {
			t.Fatalf("got %+v, %v runs=%d", r, err, f.Runs.Created())
		}
	})

	t.Run("a ledger outage is retryable and records nothing", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.SetError(fmt.Errorf("%w: refused", gw.ErrRunRefused))
		f.Runs.DecideErr = errors.New("ledger down")
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if rec, _ := f.Store.Delivery(keyOf(f, "d1")); rec.Done {
			t.Fatal("a terminal outcome was recorded without the ledger")
		}
		f.Runs.DecideErr, f.Runs.Err = nil, nil
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" {
			t.Fatalf("retry: %+v, %v", r, err)
		}
	})
}
