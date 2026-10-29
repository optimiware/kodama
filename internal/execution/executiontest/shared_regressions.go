package executiontest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

func beginReq() runtimev2.BeginRequest {
	return runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: CommitA, TrackedClean: true}}
}

func (r *rig) begin(in *execution.AttemptSpec) (execution.BeginAck, error) {
	return r.s.Svc.RecordBegin(context.Background(), SharedScope, in.AttemptID, beginReq())
}

func (r *rig) cancel() {
	r.t.Helper()
	if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
		r.t.Fatal(err)
	}
}

// drain runs reconciliation passes, advancing the clock past the backoff, until
// the run is terminal or the passes are exhausted.
func (r *rig) drain(svc *execution.Service, n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		r.tick(10 * time.Second)
		r.passOn(svc)
	}
}

func (r *rig) starts() int { return len(r.s.Decorator.Starts()) }

// sharedRegressions are the deterministic negative schedules found by the review
// of the first candidate: each one fails on the earlier behavior.
func sharedRegressions(t *testing.T, open func(testing.TB) SharedSubject) {
	t.Helper()
	svcOnly := SharedSpecDoc(false, SharedStep("job"))
	withDB := SharedSpecDoc(true, SharedStep("job"))
	twoStep := SharedSpecDoc(false, SharedStep("a"), SharedStep("b", "needs: [a]"))
	newRig := func(t *testing.T) *rig { t.Helper(); return &rig{t: t, s: open(t)} }
	ctx := context.Background()

	t.Run("a collision never fabricates settlement of a bound or possibly created resource", func(t *testing.T) {
		t.Run("a bound running service whose instance stops being ours", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.up()
			r.s.Resources.Collide("service.db", true)
			r.tick(2 * time.Second)
			r.pass()
			if got := r.get().Run.FailureReason; got != execution.Reason(execution.ReasonResourceCollision) {
				t.Fatalf("failure %q", got)
			}
			r.drain(r.s.Svc, 6)
			agg := r.get()
			db := agg.Resource("service.db")
			if agg.Run.Status.Terminal() || db.Stopped() || db.Removed() || db.Reason != execution.ReasonResourceCollision {
				t.Fatalf("the collision is ambiguity, not absence: %s stopped=%v removed=%v reason=%q", agg.Run.Status, db.Stopped(), db.Removed(), db.Reason)
			}
			if r.s.RunReservations(SharedScope.TenantID) != 1 || !r.s.Resources.Exists("service.db") || callIndex(r.s.Resources.Calls(), "remove:service.db") >= 0 {
				t.Fatalf("the reservation, the object or the call log: %v", r.s.Resources.Calls())
			}
			if v := agg.SharedView(); v == nil || v.CleanupReason == "" {
				t.Fatal("the stuck cleanup must stay visible")
			}
			// An affirmative settlement ends it.
			r.s.Resources.Collide("service.db", false)
			r.drain(r.s.Svc, 8)
			if agg := r.get(); agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() || r.s.RunReservations(SharedScope.TenantID) != 0 {
				t.Fatalf("after affirmative settlement: %s", agg.Run.Status)
			}
		})
		t.Run("a never-owned foreign object is reported and untouched while our record settles", func(t *testing.T) {
			r := newRig(t)
			r.admit(svcOnly)
			r.start()
			specs, err := execution.BuildResourceSpecs(r.get().Run, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, sp := range specs {
				if sp.Key == "network" {
					foreign := sp
					foreign.Backend = "someone-else"
					r.s.Resources.Plant(foreign)
				}
			}
			r.pass()
			uid := r.s.Resources.UID("network")
			r.drain(r.s.Svc, 8)
			agg := r.get()
			if agg.Run.FailureReason != execution.Reason(execution.ReasonResourceCollision) || agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() {
				t.Fatalf("never-owned: %s %s", agg.Run.Status, agg.Run.FailureReason)
			}
			if !r.s.Resources.Exists("network") || r.s.Resources.UID("network") != uid {
				t.Fatal("the foreign object was touched")
			}
		})
	})

	t.Run("begin rechecks live command authority before every acknowledgment, replays included", func(t *testing.T) {
		t.Run("cancellation after a recorded begin", func(t *testing.T) {
			r := newRig(t)
			r.admit(twoStep)
			r.up()
			a := r.mustClaim()
			r.started(a)
			if _, err := r.begin(a); err != nil {
				t.Fatal(err)
			}
			r.cancel()
			if _, err := r.begin(a); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("an identical replay after cancellation must not authorize the command: %v", err)
			}
			if !r.get().Began(a.AttemptID) || !r.get().StepBegan("a") {
				t.Fatal("the recorded begin must stay for the retry and effect barriers")
			}
		})
		t.Run("the total deadline after a lost acknowledgment", func(t *testing.T) {
			r := newRig(t)
			r.admit(twoStep)
			r.up()
			a := r.mustClaim()
			r.started(a)
			if _, err := r.begin(a); err != nil {
				t.Fatal(err)
			}
			r.tick(2 * time.Hour) // the acknowledgment was lost; the replay arrives after the deadline
			if _, err := r.begin(a); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("a replay after the deadline: %v", err)
			}
			agg := r.get()
			if agg.Run.FailureReason != execution.Reason(execution.ReasonRunDeadlineExceeded) || !agg.Began(a.AttemptID) {
				t.Fatalf("the failure must be persisted and the begin kept: %+v", agg.Run.FailureReason)
			}
			if _, err := r.begin(a); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("still revoked: %v", err)
			}
		})
		t.Run("a conflicting body is still a conflict while live", func(t *testing.T) {
			r := newRig(t)
			r.admit(twoStep)
			r.up()
			a := r.mustClaim()
			if _, err := r.begin(a); err != nil {
				t.Fatal(err)
			}
			other := runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: CommitA, TrackedClean: false}}
			if _, err := r.s.Svc.RecordBegin(ctx, SharedScope, a.AttemptID, other); !errors.Is(err, execution.ErrBeginConflict) {
				t.Fatalf("%v", err)
			}
		})
	})

	t.Run("a delayed older ready answer never overwrites a newer unknown observation", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.tick(2 * time.Second)
		entered, release := r.s.Resources.HoldNextObserve("service.db")
		other := r.s.Restart()
		done := make(chan struct{})
		var lateErr error
		go func() {
			defer close(done)
			_, lateErr = r.s.Svc.ReconcileRun(ctx, r.ref()) // observes ready, then waits before delivering it
		}()
		<-entered
		r.s.Resources.FailObserve("service.db", errors.New("api outage"))
		r.passOn(other) // commits the newer unknown observation
		before := r.res("service.db")
		if before.Phase != execution.PhaseUnknown || before.UnknownSince == nil {
			t.Fatalf("the newer observation must be unknown: %+v", before)
		}
		release() // the older ready answer is delivered late
		<-done
		if lateErr != nil {
			t.Fatalf("the stale answer must be discarded, not applied and rejected: %v", lateErr)
		}
		after := r.res("service.db")
		if after.Phase != execution.PhaseUnknown || after.UnknownSince == nil || after.AppliedGen != before.AppliedGen {
			t.Fatalf("the old ready overwrote the newer unknown: phase=%s unknownSince=%v gen %d -> %d", after.Phase, after.UnknownSince, before.AppliedGen, after.AppliedGen)
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("dispatch must remain blocked: %v", err)
		}
	})

	t.Run("the holder is released only on current, fresh, UID-bound evidence, never on an older answer", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		a := r.mustClaim()
		r.started(a)
		r.settledSucceed(a, "")
		r.tick(2 * time.Second)
		// The settled attempt's release is about to be observed as released; that
		// answer is delayed while a newer observation reports the volume in use.
		entered, release := r.s.Resources.HoldNextObserve("workspace")
		other := r.s.Restart()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = r.s.Svc.ReconcileRun(ctx, r.ref())
		}()
		<-entered
		r.s.Resources.Hold(true)
		r.passOn(other)
		if r.get().ActiveHolder() == nil {
			t.Fatal("the holder was released while the volume was in use")
		}
		release()
		<-done
		if r.get().ActiveHolder() == nil {
			t.Fatal("an older released answer released the holder after a newer unreleased one")
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("handoff to the next attempt: %v", err)
		}
		r.s.Resources.Hold(false)
		r.tick(2 * time.Second)
		r.passes(2)
		if r.get().ActiveHolder() != nil {
			t.Fatal("current release evidence must release the holder")
		}
	})

	t.Run("a late success after the total deadline is evidence only, without an intervening reconcile", func(t *testing.T) {
		longStep := "{id: job, kind: container, image: " + SharedImage + ", command: [/bin/true], timeoutSeconds: 3500}"
		doc := SharedSpecDoc(false, longStep)
		t.Run("late success", func(t *testing.T) {
			r := newRig(t)
			r.admit(doc)
			r.up()
			r.tick(200 * time.Second)
			r.passes(1)
			a := r.mustClaim()
			r.started(a)
			r.tick(3450 * time.Second) // past the run deadline, inside the step's own window
			if _, err := r.s.Drive.Succeed(a, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("a late success must be refused: %v", err)
			}
			agg := r.get()
			if agg.Run.FailureReason != execution.Reason(execution.ReasonRunDeadlineExceeded) || agg.Step("job").Status == execution.StepSucceeded {
				t.Fatalf("the failure must be persisted by the result transaction: %+v %s", agg.Run.FailureReason, agg.Step("job").Status)
			}
			if at := agg.Attempt(a.AttemptID); at.Status != execution.AttemptCancelled {
				t.Fatalf("the attempt is %s", at.Status)
			}
			if n := r.s.EvidenceCount(a.AttemptID, "late_result"); n == 0 {
				t.Fatal("the late result must be kept as evidence")
			}
		})
		t.Run("an accepted result stays replayable after the deadline", func(t *testing.T) {
			r := newRig(t)
			r.admit(twoStep)
			r.up()
			a := r.mustClaim()
			r.started(a)
			first, err := r.s.Drive.Succeed(a, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			r.tick(2 * time.Hour)
			again, err := r.s.Drive.Succeed(a, nil, "")
			if err != nil || !again.Replayed || again.ResultHash != first.ResultHash {
				t.Fatalf("the accepted result's replay exception: %+v %v", again, err)
			}
		})
	})

	t.Run("an unconfirmed launch is redriven only while every launch check still holds", func(t *testing.T) {
		cases := map[string]struct {
			doc  string
			mut  func(r *rig)
			want int
		}{
			"all checks hold": {svcOnly, func(r *rig) {}, 1},
			"companion capability lost": {svcOnly, func(r *rig) {
				r.s.Resources.SetCapabilities(FullResourceSet &^ execution.ResourceCapabilitySet(execution.CapRunNetworkIsolation))
			}, 0},
			"pins no longer verify": {svcOnly, func(r *rig) { r.s.CorruptPolicy(SharedScope, r.run.ID) }, 0},
			"readiness unknown":     {withDB, func(r *rig) { r.s.Resources.FailObserve("service.db", errors.New("outage")) }, 0},
			"another backend":       {svcOnly, func(r *rig) { r.s.Resources.SetBackend("other") }, 0},
			"runtime guarantees lost": {svcOnly, func(r *rig) {
				r.s.Manual.SetCapabilities(execution.Capabilities{Backend: SharedBackend, Guarantees: execution.OCIImage | execution.RunResourcesMarker})
			}, 0},
		}
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				r := newRig(t)
				r.admit(c.doc)
				r.up()
				a := r.mustClaim() // the start acknowledgment is lost: no Start happened
				_ = a
				if r.starts() != 0 {
					t.Fatal("fixture")
				}
				r.tick(3 * time.Second) // the claim lease lapsed
				c.mut(r)
				_, _ = r.s.Svc.ReconcileRun(ctx, r.ref())
				if got := r.starts(); got != c.want {
					t.Fatalf("Runtime.Start called %d times, want %d", got, c.want)
				}
			})
		}
	})

	t.Run("a policy that no longer verifies fails the run and still settles its resources", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.s.CorruptPolicy(SharedScope, r.run.ID)
		restarted := r.s.Restart()
		ensures := r.s.Resources.Count("ensure:service.db")
		r.drain(restarted, 10)
		agg := r.get()
		if agg.Run.FailureReason != execution.Reason(execution.ReasonPolicyUnavailable) || agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() {
			t.Fatalf("policy loss: %s %s removed=%v", agg.Run.Status, agg.Run.FailureReason, agg.ResourcesRemoved())
		}
		if len(r.s.Resources.LiveKeys()) != 0 || r.s.Resources.Count("ensure:service.db") != ensures || r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatalf("live=%v", r.s.Resources.LiveKeys())
		}
	})

	t.Run("a controller that cannot own the run model or backend leaves it untouched", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		calls := len(r.s.Resources.Calls())
		_, _ = r.s.Closed().ReconcileRun(ctx, r.ref()) // no run_shared composition
		r.s.Resources.SetBackend("other")
		r.s.Svc.ReconcileRun(ctx, r.ref())
		r.s.Restart().ReconcileRun(ctx, r.ref())
		if _, err := r.s.Svc.Cancel(ctx, sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		r.tick(10 * time.Second)
		r.s.Svc.ReconcileRun(ctx, r.ref())
		if got := len(r.s.Resources.Calls()); got != calls {
			t.Fatalf("a foreign backend or unsupported model made %d calls", got-calls)
		}
		if len(r.s.Resources.LiveKeys()) == 0 || r.get().Run.Status.Terminal() {
			t.Fatal("resources of a run this controller cannot own must stay untouched")
		}
	})

	t.Run("a begun attempt of a retryable class fails the run promptly and stops the services while it settles", func(t *testing.T) {
		retry := SharedSpecDoc(true, SharedStep("job", "retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure]}"))
		r := newRig(t)
		r.admit(retry)
		r.up()
		a := r.mustClaim()
		r.started(a)
		if _, err := r.begin(a); err != nil {
			t.Fatal(err)
		}
		r.s.Manual.SetStop(execution.StopEvidence{Stopped: false, Launch: execution.LaunchUnknown}) // the workload is not confirmed gone
		if err := r.s.Drive.Observe(a, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
			t.Fatal(err)
		}
		if r.get().Run.FailureIntentAt == nil {
			t.Fatal("the failure intent of a begun, nonretryable attempt must be recorded now, not after settlement")
		}
		r.drain(r.s.Svc, 4)
		agg := r.get()
		if db := agg.Resource("service.db"); !db.Removed() {
			t.Fatalf("the service must be stopped promptly while the uncertain attempt settles: stopped=%v removed=%v", db.Stopped(), db.Removed())
		}
		if agg.Run.Status.Terminal() || agg.Resource("workspace").Removed() {
			t.Fatal("the uncertain attempt still holds the run and its scratch")
		}
	})

	t.Run("an unsolicited verified stop is loss, except a preparation that already recorded its receipt", func(t *testing.T) {
		t.Run("a ready service stopped on its own", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.up()
			r.s.Resources.SetPhase("service.db", execution.PhaseStopped)
			r.tick(2 * time.Second)
			r.pass()
			if got := r.get().Run.FailureReason; got != execution.Reason(execution.ReasonServiceLost) {
				t.Fatalf("%q", got)
			}
		})
		t.Run("a preparation that stopped without a receipt", func(t *testing.T) {
			r := newRig(t)
			r.admit(svcOnly)
			r.s.Resources.AutoReady = true
			r.start()
			r.passes(2)
			r.s.Resources.SetPhase("preparation", execution.PhaseStopped)
			r.tick(2 * time.Second)
			r.pass()
			if got := r.get().Run.FailureReason; got != execution.Reason(execution.ReasonPreparationFailed) {
				t.Fatalf("%q", got)
			}
		})
	})

	t.Run("a shared run's active-attempt slot is reserved with its capacity and never starves it", func(t *testing.T) {
		legacy := "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: legacy}\nspec:\n  steps:\n    - {id: job, kind: container, image: " +
			SharedImage + ", command: [/bin/true], timeoutSeconds: 60, workspace: {access: read, from: run}}\n"
		oneSlot := func(r *rig) {
			lim := SharedLimit()
			lim.TenantConcurrency, lim.ProjectConcurrency = 1, 1
			r.s.Limits.Set(lim)
		}
		admitLegacy := func(r *rig) *execution.Run {
			def := r.s.Seed.Define(SharedScope, legacy)
			run, err := r.s.Svc.Admit(ctx, sharedCaller, execution.AdmitCommand{
				Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
			})
			if err != nil {
				t.Fatal(err)
			}
			return run
		}
		t.Run("a legacy run cannot take the slot of a shared run holding services", func(t *testing.T) {
			r := newRig(t)
			oneSlot(r)
			r.admit(twoStep)
			shared := r.run
			r.up()
			legacyRun := admitLegacy(r)
			a := r.mustClaim()
			if a.RunID != shared.ID {
				t.Fatal("the shared attempt runs first")
			}
			r.started(a)
			r.settledSucceed(a, "")
			r.tick(2 * time.Second)
			r.passes(3)
			// Between a and b the run still owns its slot: the waiting legacy run cannot take it.
			b, err := r.claim()
			if err != nil || b == nil || b.RunID != shared.ID || b.StepID != "b" {
				t.Fatalf("the shared run's next attempt must not wait for quota: %+v %v", b, err)
			}
			r.started(b)
			r.settledSucceed(b, "")
			if got := r.getRun(legacyRun).Steps[0].AttemptsStarted; got != 0 {
				t.Fatalf("the legacy run started %d attempts while the shared run held the slot", got)
			}
			r.drain(r.s.Svc, 8)
			if agg := r.get(); agg.Run.Status != execution.RunSucceeded {
				t.Fatalf("shared run %s", agg.Run.Status)
			}
			in, err := r.claim()
			if err != nil || in == nil || in.RunID != legacyRun.ID {
				t.Fatalf("after the verified release the legacy run takes the slot: %+v %v", in, err)
			}
		})
		t.Run("a shared run does not start while another run holds the only slot, and starts after its release", func(t *testing.T) {
			r := newRig(t)
			oneSlot(r)
			legacyRun := admitLegacy(r)
			la, err := r.claim()
			if err != nil || la == nil || la.RunID != legacyRun.ID {
				t.Fatalf("legacy claim %+v %v", la, err)
			}
			r.admit(svcOnly)
			if _, err := r.claim(); err != nil && !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatal(err)
			}
			agg := r.get()
			if agg.Run.Status != execution.RunQueued || len(agg.Resources) != 0 || r.s.RunReservations(SharedScope.TenantID) != 0 {
				t.Fatalf("a refused start leaves no partial reservation: %s resources=%d", agg.Run.Status, len(agg.Resources))
			}
			r.s.Drive.Start(la)
			if _, err := r.s.Drive.Succeed(la, nil, ""); err != nil {
				t.Fatal(err)
			}
			r.s.Svc.ReconcileTenant(ctx, SharedScope.TenantID)
			if _, err := r.claim(); err != nil && !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatal(err)
			}
			if agg := r.get(); agg.Run.Status != execution.RunRunning || r.s.RunReservations(SharedScope.TenantID) != 1 {
				t.Fatalf("after release: %s held=%d", agg.Run.Status, r.s.RunReservations(SharedScope.TenantID))
			}
		})
	})

	t.Run("a gate is eligible only when its predecessors settled and released the workspace", func(t *testing.T) {
		doc := SharedSpecDoc(false, SharedStep("a"), "{id: review, kind: gate}", SharedStep("c", "needs: [review]"))
		r := newRig(t)
		r.admit(doc)
		r.up()
		ready := func() []string {
			var ids []string
			for _, s := range r.get().ReadyGates() {
				ids = append(ids, s.ID)
			}
			return ids
		}
		bind := func() error {
			_, _, err := r.s.Store.Update(ctx, SharedScope, r.run.ID, execution.UpdateOptions{}, execution.BindGateMutation("review", "gate_1", nil, nil))
			return err
		}
		complete := func() error {
			_, _, err := r.s.Store.Update(ctx, SharedScope, r.run.ID, execution.UpdateOptions{LockWork: true}, execution.CompleteGateMutation("review",
				func(context.Context, execution.Env, *execution.Aggregate, *execution.StepRun) error { return nil }))
			return err
		}
		if len(ready()) != 0 {
			t.Fatalf("the independent later gate is ready before its predecessor ran: %v", ready())
		}
		a := r.mustClaim()
		r.started(a)
		if len(ready()) != 0 {
			t.Fatalf("the gate is ready while its predecessor runs: %v", ready())
		}
		if err := bind(); !errors.Is(err, execution.ErrStale) {
			t.Fatalf("binding while the predecessor runs: %v", err)
		}
		r.s.Resources.Hold(true)
		r.settledSucceed(a, "")
		r.tick(2 * time.Second)
		r.passes(2)
		agg := r.get()
		if agg.Step("a").Status != execution.StepSucceeded || agg.ActiveHolder() == nil || len(ready()) != 0 {
			t.Fatalf("terminal but unreleased: holder=%v ready=%v", agg.ActiveHolder() != nil, ready())
		}
		if err := bind(); !errors.Is(err, execution.ErrStale) {
			t.Fatalf("binding while the predecessor holds the volume: %v", err)
		}
		r.s.Resources.Hold(false)
		r.tick(2 * time.Second)
		r.passes(2)
		if got := ready(); len(got) != 1 || got[0] != "review" {
			t.Fatalf("after settlement and release: %v", got)
		}
		if err := bind(); err != nil {
			t.Fatal(err)
		}
		if err := complete(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.claim(); err != nil {
			t.Fatalf("the step after the gate: %v", err)
		}
	})

	t.Run("a source-free cleanup run is admitted, provisioned, ordered and cleaned up under the same gate", func(t *testing.T) {
		clean := SharedSpecDoc(false, SharedStep("one", "workspace: {access: none}"), SharedStep("two", "needs: [one]"))
		admitCleanup := func(t *testing.T, r *rig, svc *execution.Service, doc string) (*execution.Run, error) {
			t.Helper()
			def := r.s.DefineShared(doc)
			return svc.AdmitCleanup(ctx, execution.CleanupCommand{Scope: SharedScope, DeploymentID: "dep_1", DefinitionID: def.ID})
		}
		t.Run("default and unconfigured composition refuse both paths", func(t *testing.T) {
			r := newRig(t)
			def := r.s.DefineShared(clean)
			closed := r.s.NewClosedCleanup()
			if _, err := closed.PrepareCleanup(ctx, execution.CleanupCommand{Scope: SharedScope, DeploymentID: "dep_1", DefinitionID: def.ID}); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("cleanup: %v", err)
			}
			if _, err := closed.Admit(ctx, sharedCaller, execution.AdmitCommand{Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID,
				ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA}); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("work: %v", err)
			}
		})
		t.Run("capability, policy and authorization gates apply", func(t *testing.T) {
			r := newRig(t)
			r.s.Resources.SetCapabilities(FullResourceSet &^ execution.ResourceCapabilitySet(execution.CapEgressDefaultDeny))
			if _, err := admitCleanup(t, r, r.s.Svc, clean); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("a backend lacking egress_default_deny: %v", err)
			}
			r.s.Resources.SetCapabilities(FullResourceSet)
			p := TestPolicy()
			p.Admission.RunShared = execution.PolicyClosed
			r.s.Policies.Set(p)
			if _, err := admitCleanup(t, r, r.s.Svc, clean); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("a closed policy: %v", err)
			}
			r.s.Policies.Set(TestPolicy())
			if _, err := admitCleanup(t, r, r.s.Svc, SharedSpecDoc(false, SharedStep("w", "workspace: {access: write}"))); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("a source-free step with workspace access: %v", err)
			}
		})
		t.Run("the tenant network model needs neither run isolation nor default deny (O-30)", func(t *testing.T) {
			r := newRig(t)
			r.s.Resources.SetCapabilities(FullResourceSet &^ execution.Set(execution.CapRunNetworkIsolation, execution.CapEgressDefaultDeny))
			if _, err := admitCleanup(t, r, r.s.Svc, clean); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("the strict model on a tenant-only backend: %v", err)
			}
			r.s.Policies.Set(TenantPolicy())
			run, err := admitCleanup(t, r, r.s.Svc, clean)
			if err != nil {
				t.Fatalf("the tenant model on a tenant-only backend: %v", err)
			}
			r.run = run
			sh := r.get().Run.Shared
			if sh.ResourceRequires.Has(execution.CapRunNetworkIsolation) || sh.ResourceRequires.Has(execution.CapEgressDefaultDeny) || sh.Policy.ID != "run-test-tenant" {
				t.Fatalf("pins %v %s", sh.ResourceRequires, sh.Policy.ID)
			}
			specs, err := execution.BuildResourceSpecs(r.get().Run, time.Now().Add(time.Hour))
			if err != nil || specs[0].Kind != execution.KindNetwork || specs[0].Network.Isolation != execution.IsolationTenant || !specs[0].Network.PublicEgress {
				t.Fatalf("the network spec does not carry the pinned model: %+v %v", specs[0].Network, err)
			}
			// A later policy change never rewrites the pin.
			r.s.Policies.Set(TestPolicy())
			if got := r.get().Run.Shared; got.Policy.ID != "run-test-tenant" || got.PolicyBody.Network.Isolation != execution.IsolationTenant {
				t.Fatalf("the pin changed: %+v", got.Policy)
			}
		})
		t.Run("lifecycle without work item, source, checkout or preparation", func(t *testing.T) {
			r := newRig(t)
			calls := r.s.SourceCalls()
			run, err := admitCleanup(t, r, r.s.Svc, clean)
			if err != nil {
				t.Fatal(err)
			}
			r.run = run
			agg := r.get()
			sh := agg.Run.Shared
			if run.Purpose != execution.PurposeCleanup || run.WorkItemID != "" || run.WorkspaceID != "" || run.SourceCommit != "" || sh == nil || sh.SourceBacked ||
				run.SourceRetention != execution.RetentionNotRequired || sh.Access["one"] != execution.AccessNone || r.s.SourceCalls() != calls {
				t.Fatalf("source-free pins: %+v sourceCalls=%d", sh, r.s.SourceCalls()-calls)
			}
			if sh.ResourceRequires.Has(execution.CapWorkspacePreparation) || !sh.ResourceRequires.Has(execution.CapRunNetworkIsolation) || !sh.ResourceRequires.Has(execution.CapEgressDefaultDeny) {
				t.Fatalf("requirements %v", sh.ResourceRequires)
			}
			r.s.Resources.AutoReady = true
			r.start()
			var keys []string
			for _, x := range r.get().Resources {
				keys = append(keys, x.Key)
			}
			if strings.Join(keys, ",") != "network,workspace" {
				t.Fatalf("no preparation resource: %v", keys)
			}
			r.passes(3)
			if r.s.Resources.Exists("preparation") || r.s.SourceCalls() != calls || !r.get().WorkspaceReady() {
				t.Fatalf("no preparation, no source call, an empty scratch ready: live=%v", r.s.Resources.LiveKeys())
			}
			if _, err := r.receipt(); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("a source-free run has no receipt: %v", err)
			}
			a := r.mustClaim()
			ac, err := a.DecodeContextV2()
			if err != nil || ac.Workspace.Source != nil || ac.Workspace.Preparation != nil || len(ac.Workspace.Lineage) != 0 || ac.Workspace.Access != runtimev1.AccessNone {
				t.Fatalf("first context %+v %v", ac.Workspace, err)
			}
			if err := ac.Validate(); err != nil {
				t.Fatal(err)
			}
			r.started(a)
			ack, err := r.s.Svc.RecordBegin(ctx, SharedScope, a.AttemptID, runtimev2.BeginRequest{Workspace: nil})
			if err != nil || ack.WorkspaceState != runtimev2.StateNone || ack.IgnoredPresent {
				t.Fatalf("begin %+v %v", ack, err)
			}
			if _, err := r.s.Svc.RecordBegin(ctx, SharedScope, a.AttemptID, beginReq()); !errors.Is(err, execution.ErrInvalid) && !errors.Is(err, execution.ErrBeginConflict) {
				t.Fatalf("a source-backed begin body for a source-free attempt: %v", err)
			}
			r.settledSucceed(a, "")
			r.tick(2 * time.Second)
			r.passes(3)
			b := r.mustClaim()
			bc, err := b.DecodeContextV2()
			if err != nil || len(bc.Workspace.Lineage) != 1 {
				t.Fatalf("the second step sees the first step's lifecycle: %+v %v", bc.Workspace.Lineage, err)
			}
			if e := bc.Workspace.Lineage[0]; e.StepID != "one" || e.Access != runtimev1.AccessNone || e.WorkspaceState != runtimev2.StateNone || e.IgnoredPresent || e.PublishedCommit != nil {
				t.Fatalf("lineage %+v", e)
			}
			if err := bc.Validate(); err != nil || bc.Workspace.Source != nil || bc.Workspace.Preparation != nil {
				t.Fatalf("second context: %v", err)
			}
			if r.s.SourceCalls() != calls {
				t.Fatal("the lifecycle consulted the Git observation seam")
			}
			r.started(b)
			r.settledSucceed(b, "")
			r.drain(r.s.Svc, 8)
			if agg := r.get(); agg.Run.Status != execution.RunSucceeded || !agg.ResourcesRemoved() || r.s.RunReservations(SharedScope.TenantID) != 0 {
				t.Fatalf("completion: %s", agg.Run.Status)
			}
		})
		t.Run("cancellation and restart clean the scratch and the network", func(t *testing.T) {
			r := newRig(t)
			run, err := admitCleanup(t, r, r.s.Svc, clean)
			if err != nil {
				t.Fatal(err)
			}
			r.run = run
			r.s.Resources.AutoReady = true
			r.start()
			r.passes(3)
			a := r.mustClaim()
			r.started(a)
			r.cancel()
			r.drain(r.s.Restart(), 10)
			if agg := r.get(); agg.Run.Status != execution.RunCancelled || !agg.ResourcesRemoved() || len(r.s.Resources.LiveKeys()) != 0 {
				t.Fatalf("cancelled cleanup: %s live=%v", agg.Run.Status, r.s.Resources.LiveKeys())
			}
		})
	})

	t.Run("every resource transition is appended to history, audit and the outbox atomically", func(t *testing.T) {
		t.Run("events carry safe, scoped, phase-consistent observations", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.up()
			a := r.mustClaim()
			r.started(a)
			r.cancel()
			r.drain(r.s.Svc, 10)
			evs := r.s.Events(eventsv1.TypeRunResourceStateChanged)
			if len(evs) < 12 {
				t.Fatalf("%d resource events", len(evs))
			}
			seen := map[string]bool{}
			last := map[string]eventsv1.RunResourceStateChanged{}
			for _, ev := range evs {
				if err := ev.Validate(); err != nil {
					t.Fatalf("the event violates the T-119 contract: %v", err)
				}
				var v eventsv1.RunResourceStateChanged
				if err := json.Unmarshal(ev.Data, &v); err != nil {
					t.Fatal(err)
				}
				if id, _ := eventsv1.RunResourceID(ev.TenantID, ev.ProjectID, domain.RunID(v.RunID), v.Key); ev.Resource.ID != id || ev.Resource.Type != "run_resource" {
					t.Fatalf("the resource ID is not scoped: %s", ev.Resource.ID)
				}
				if seen[ev.ID] {
					t.Fatalf("event ID %s repeated", ev.ID)
				}
				seen[ev.ID] = true
				var members map[string]json.RawMessage
				_ = json.Unmarshal(ev.Data, &members)
				if len(members) != 8 {
					t.Fatalf("the body has %d members: %s", len(members), ev.Data)
				}
				for _, secret := range []string{"uid-", "kr-", "s-", "registry", "token"} {
					if strings.Contains(string(ev.Data), "\"specFingerprint\"") && strings.Contains(strings.ReplaceAll(string(ev.Data), v.SpecFingerprint, ""), secret) && secret != "token" {
						t.Fatalf("the body leaks %q: %s", secret, ev.Data)
					}
				}
				if prev, ok := last[v.Key]; ok && prev.To != v.From && v.From != "none" {
					t.Fatalf("%s: phase chain broken %s -> %s after %s", v.Key, v.From, v.To, prev.To)
				}
				last[v.Key] = v
			}
			for _, key := range []string{"network", "workspace", "preparation", "service.db"} {
				if got := last[key]; got.To != "absent" {
					t.Fatalf("%s ends at %s, want absent after verified removal", key, got.To)
				}
			}
			ready := false
			for _, ev := range evs {
				var v eventsv1.RunResourceStateChanged
				_ = json.Unmarshal(ev.Data, &v)
				if v.To == "ready" {
					ready = ready || v.UIDBound
					if !v.UIDBound {
						t.Fatal("ready without a bound identity")
					}
				}
			}
			if !ready {
				t.Fatal("no ready event")
			}
		})
		t.Run("an outbox refusal rolls back the whole transition", func(t *testing.T) {
			boom := errors.New("injected outbox failure")
			r := newRig(t)
			r.admit(withDB)
			r.s.FailEvent(eventsv1.TypeRunResourceStateChanged, boom)
			if _, err := r.s.Svc.ClaimRunnable(ctx, SharedScope.TenantID); !errors.Is(err, boom) {
				t.Fatalf("start: %v", err)
			}
			agg := r.get()
			if agg.Run.Status != execution.RunQueued || len(agg.Resources) != 0 || agg.Run.DeadlineAt != nil || r.s.RunReservations(SharedScope.TenantID) != 0 {
				t.Fatalf("a refused resource event left state: %s resources=%d held=%d", agg.Run.Status, len(agg.Resources), r.s.RunReservations(SharedScope.TenantID))
			}
			r.s.FailEvent(eventsv1.TypeRunResourceStateChanged, nil)
			r.s.Resources.AutoReady = true
			r.start()
			r.pass()
			// The receipt evidence is a resource transition of its own.
			r.s.FailEvent(eventsv1.TypeRunResourceStateChanged, boom)
			if _, err := r.receipt(); !errors.Is(err, boom) {
				t.Fatalf("receipt: %v", err)
			}
			if prep := r.res("preparation"); prep.Receipt != nil || prep.ReceiptDigest != "" {
				t.Fatal("the receipt survived its refused event")
			}
			before := len(r.s.Events(eventsv1.TypeRunResourceStateChanged))
			r.s.FailEvent(eventsv1.TypeRunResourceStateChanged, nil)
			ack, err := r.receipt()
			if err != nil || ack.Replayed {
				t.Fatalf("retry: %+v %v", ack, err)
			}
			if got := len(r.s.Events(eventsv1.TypeRunResourceStateChanged)); got != before+1 {
				t.Fatalf("the committed receipt has %d new events", got-before)
			}
			// A refused observation event rolls back the observed state and the generation.
			r.passes(2)
			gen := r.res("service.db").AppliedGen
			r.s.FailEvent(eventsv1.TypeRunResourceStateChanged, boom)
			r.s.Resources.SetPhase("service.db", execution.PhaseFailed)
			r.tick(2 * time.Second)
			_, _ = r.s.Svc.ReconcileRun(ctx, r.ref())
			if got := r.res("service.db"); got.Phase == execution.PhaseFailed || got.AppliedGen != gen {
				t.Fatalf("a refused observation event left state: phase=%s gen %d -> %d", got.Phase, gen, got.AppliedGen)
			}
		})
	})

	t.Run("a stuck cleanup after success stays visible and nonterminal", func(t *testing.T) {
		r := newRig(t)
		r.admit(svcOnly)
		r.up()
		a := r.mustClaim()
		r.started(a)
		r.s.Resources.HoldRemove("workspace", 100)
		r.settledSucceed(a, "")
		r.drain(r.s.Svc, 6)
		agg := r.get()
		if agg.Run.Status.Terminal() || agg.CleanupReason() != execution.ReasonCleanupPending {
			t.Fatalf("a successful run with stuck cleanup: %s %q", agg.Run.Status, agg.CleanupReason())
		}
		if v := agg.SharedView(); v == nil || v.CleanupReason != execution.ReasonCleanupPending {
			t.Fatalf("view %+v", v)
		}
		if r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatal("the reservation was released with cleanup pending")
		}
		r.s.Resources.HoldRemove("workspace", 0)
		r.drain(r.s.Restart(), 8)
		if agg := r.get(); agg.Run.Status != execution.RunSucceeded || !agg.ResourcesRemoved() || r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatalf("after cleanup: %s", agg.Run.Status)
		}
	})

	sharedRepairRegressions(t, open)

	t.Run("the store rejects every rewrite of written-once resource evidence, in memory and in SQL alike", func(t *testing.T) {
		r := newRig(t)
		r.admit(svcOnly)
		r.s.Resources.AutoReady = true
		r.start() // all intents, nothing bound yet
		tamper := func(name string, key string, fn func(res *execution.RunResource), want error) {
			t.Helper()
			before := r.res(key)
			_, _, err := r.s.Store.Update(ctx, SharedScope, r.run.ID, execution.UpdateOptions{}, func(c context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				res := agg.Resource(key)
				res.RequestObservation(env.Now()) // marks the row dirty so it is saved
				fn(res)
				return execution.Effects{}, nil
			})
			if !errors.Is(err, want) {
				t.Errorf("%s: got %v, want %v", name, err, want)
			}
			after := r.res(key)
			if after.Version != before.Version || after.UID != before.UID || after.ObserveGen != before.ObserveGen {
				t.Errorf("%s: the rejected update changed state", name)
			}
		}
		bound := func(key, uid string) {
			t.Helper()
			if _, _, err := r.s.Store.Update(ctx, SharedScope, r.run.ID, execution.UpdateOptions{}, func(c context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				return execution.Effects{}, agg.Resource(key).Bind(uid, env.Now())
			}); err != nil {
				t.Fatal(err)
			}
		}
		// Two unbound resources of one backend cannot be bound to one provider instance.
		bound("network", "uid-dup-1")
		tamper("a second resource bound to the same provider instance", "workspace", func(res *execution.RunResource) { _ = res.Bind("uid-dup-1", time.Now()) }, execution.ErrConflict)
		r.cancel()
		r.drain(r.s.Svc, 4)

		r = newRig(t)
		r.admit(svcOnly)
		r.s.Resources.AutoReady = true
		r.start()
		r.pass()
		r.pass()
		later := time.Now().Add(time.Hour).UTC()
		tamper = func(name string, key string, fn func(res *execution.RunResource), want error) {
			t.Helper()
			before := r.res(key)
			histBefore, eventsBefore := r.s.HistoryCount(r.run.ID), len(r.s.Events(eventsv1.TypeRunResourceStateChanged))
			_, _, err := r.s.Store.Update(ctx, SharedScope, r.run.ID, execution.UpdateOptions{}, func(c context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				res := agg.Resource(key)
				res.RequestObservation(env.Now())
				fn(res)
				return execution.Effects{}, nil
			})
			if !errors.Is(err, want) {
				t.Errorf("%s: got %v, want %v", name, err, want)
			}
			after := r.res(key)
			if after.Version != before.Version || after.UID != before.UID || after.ObserveGen != before.ObserveGen || after.AppliedGen != before.AppliedGen {
				t.Errorf("%s: the rejected update changed state", name)
			}
			if r.s.HistoryCount(r.run.ID) != histBefore || len(r.s.Events(eventsv1.TypeRunResourceStateChanged)) != eventsBefore {
				t.Errorf("%s: history or outbox rows survived the rejected update", name)
			}
		}
		tamper("the UID binding time", "network", func(res *execution.RunResource) { res.BoundAt = &later }, execution.ErrInvalidTransition)
		tamper("the bound UID", "network", func(res *execution.RunResource) { res.UID = "uid-other" }, execution.ErrInvalidTransition)
		tamper("the spec", "network", func(res *execution.RunResource) { res.Spec.RoleShape = "sha256:" + strings.Repeat("e", 64) }, execution.ErrInvalidTransition)
		tamper("the fingerprint", "network", func(res *execution.RunResource) { res.Fingerprint = "sha256:" + strings.Repeat("d", 64) }, execution.ErrInvalidTransition)
		tamper("the spec and its recomputed, self-consistent fingerprint together", "network", func(res *execution.RunResource) {
			res.Spec.RoleShape = "sha256:" + strings.Repeat("9", 64)
			res.Fingerprint, _ = res.Spec.Fingerprint()
		}, execution.ErrInvalidTransition)
		tamper("the name", "network", func(res *execution.RunResource) { res.Name = "kr-" + strings.Repeat("0", 24) }, execution.ErrInvalidTransition)
		tamper("the backend", "network", func(res *execution.RunResource) { res.Backend = "someone-else" }, execution.ErrInvalidTransition)
		tamper("the kind", "network", func(res *execution.RunResource) { res.Kind = execution.KindService }, execution.ErrInvalidTransition)
		tamper("the ensure intent", "network", func(res *execution.RunResource) { res.EnsureRequestedAt = nil }, execution.ErrInvalidTransition)
		tamper("the observation generation", "network", func(res *execution.RunResource) { res.ObserveGen = 0 }, execution.ErrInvalidTransition)
		tamper("the applied generation past the requested one", "network", func(res *execution.RunResource) { res.AppliedGen = res.ObserveGen + 5 }, execution.ErrInvalidTransition)
		if _, err := r.receipt(); err != nil {
			t.Fatal(err)
		}
		r.passes(2) // the preparation is stopped after its receipt
		tamper("the receipt body with its digest unchanged", "preparation", func(res *execution.RunResource) { res.Receipt.TreeID = CommitB }, execution.ErrInvalidTransition)
		tamper("the receipt time", "preparation", func(res *execution.RunResource) { t2 := later; res.ReceiptAt = &t2 }, execution.ErrInvalidTransition)
		tamper("the receipt digest", "preparation", func(res *execution.RunResource) { res.ReceiptDigest = "sha256:" + strings.Repeat("c", 64) }, execution.ErrInvalidTransition)
		tamper("the stop reason", "preparation", func(res *execution.RunResource) { res.StopReason = "tampered" }, execution.ErrInvalidTransition)
		tamper("the stop launch evidence", "preparation", func(res *execution.RunResource) { res.StopLaunch = execution.LaunchNotStarted }, execution.ErrInvalidTransition)
		tamper("the verified stop time", "preparation", func(res *execution.RunResource) { t2 := later; res.StoppedAt = &t2 }, execution.ErrInvalidTransition)
		tamper("starting a stopped resource again", "preparation", func(res *execution.RunResource) { res.Phase = execution.PhaseReady }, execution.ErrInvalidTransition)
		tamper("the first ready time", "network", func(res *execution.RunResource) { t2 := later; res.ReadyAt = &t2 }, execution.ErrInvalidTransition)
		tamper("removing without a verified stop", "network", func(res *execution.RunResource) { t2 := later; res.RemovedAt = &t2 }, execution.ErrInvalidTransition)
		r.cancel()
		r.drain(r.s.Svc, 10)
		tamper("a removed resource", "network", func(res *execution.RunResource) { res.Reason = "x_after_removal" }, execution.ErrInvalidTransition)
	})
}

func (r *rig) getRun(run *execution.Run) *execution.Aggregate {
	r.t.Helper()
	agg, err := r.s.Store.Get(context.Background(), SharedScope, run.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return agg
}

// snapshot is everything a refused controller must leave unchanged: the run, its
// attempts and bookkeeping, its resources, the history rows and the published
// events.
type runSnapshot struct {
	runVersion                     uint64
	status                         execution.RunStatus
	failure                        bool
	attempts, controls, resources  string
	history, resourceEvents, runEv int
}

func (r *rig) snapshot() runSnapshot {
	r.t.Helper()
	agg := r.get()
	var a, c, x strings.Builder
	for _, at := range agg.Attempts {
		c2 := agg.Control(at.ID)
		a.WriteString(string(at.ID) + ":" + string(at.Status) + ":" + itoa(uint64(at.Version)) + ";")
		c.WriteString(string(at.ID) + ":" + boolS(c2.RevokedAt != nil) + boolS(c2.TerminationRequestedAt != nil) + boolS(c2.Settled()) + ";")
	}
	for _, res := range agg.Resources {
		x.WriteString(res.Key + ":" + itoa(uint64(res.Version)) + ":" + boolS(res.Stopping()) + ":" + itoa(uint64(res.ObserveGen)) + ";")
	}
	return runSnapshot{uint64(agg.Run.Version), agg.Run.Status, agg.Run.FailureIntentAt != nil, a.String(), c.String(), x.String(),
		r.s.HistoryCount(r.run.ID), len(r.s.Events(eventsv1.TypeRunResourceStateChanged)), len(r.s.Events(eventsv1.TypeRunStateChanged))}
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func boolS(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sharedRepairRegressions are the schedules of the second review round.
func sharedRepairRegressions(t *testing.T, open func(testing.TB) SharedSubject) {
	t.Helper()
	svcOnly := SharedSpecDoc(false, SharedStep("job"))
	withDB := SharedSpecDoc(true, SharedStep("job"))
	newRig := func(t *testing.T) *rig { t.Helper(); return &rig{t: t, s: open(t)} }
	ctx := context.Background()

	t.Run("a delayed Ensure result of any kind never supersedes a newer request", func(t *testing.T) {
		t.Run("an older ready after a newer outage that committed unknown", func(t *testing.T) {
			r := newRig(t)
			r.admit(svcOnly)
			r.s.Resources.AutoReady = true
			r.start()
			entered, release := r.s.Resources.HoldNextEnsure("network")
			other := r.s.Restart()
			var lateErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, lateErr = r.s.Svc.ReconcileRun(ctx, r.ref()) // Ensure generation 1 creates the object and is delivered late
			}()
			<-entered
			r.s.Resources.FailEnsure("network", errors.New("api outage"))
			r.passOn(other) // Ensure generation 2: an outage with an unknown effect
			before := r.res("network")
			if before.Phase != execution.PhaseUnknown || before.UnknownSince == nil || before.Bound() || before.Failures != 1 || before.AppliedGen != before.ObserveGen || before.NextActionAt == nil {
				t.Fatalf("the newer outage must persist an unknown observation of its generation with its backoff: %+v", before)
			}
			release()
			<-done
			if lateErr != nil {
				t.Fatalf("a stale result is discarded, not applied and rejected: %v", lateErr)
			}
			after := r.res("network")
			if after.Phase != execution.PhaseUnknown || after.Bound() || after.UnknownSince == nil || after.AppliedGen != before.AppliedGen || after.Failures != 1 || after.IsReady() {
				t.Fatalf("the older ready superseded the newer outage: %+v", after)
			}
			if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("dispatch must remain blocked: %v", err)
			}
		})
		t.Run("an older outage after a newer ready adoption", func(t *testing.T) {
			r := newRig(t)
			r.admit(svcOnly)
			r.s.Resources.AutoReady = true
			r.s.Resources.LoseEnsure("network", errors.New("connection reset"))
			r.start()
			entered, release := r.s.Resources.HoldNextEnsure("network")
			other := r.s.Restart()
			var lateErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, lateErr = r.s.Svc.ReconcileRun(ctx, r.ref()) // creates, loses the response and is delivered late
			}()
			<-entered
			r.s.Resources.LoseEnsure("network", nil)
			r.passOn(other) // Ensure generation 2 adopts the object: ready, bound
			before := r.res("network")
			if !before.Bound() || before.Phase != execution.PhaseReady || before.Failures != 0 {
				t.Fatalf("the newer ensure must adopt: %+v", before)
			}
			release()
			<-done
			if lateErr != nil {
				t.Fatalf("%v", lateErr)
			}
			after := r.res("network")
			if !after.IsReady() || after.Failures != 0 || after.NextActionAt != nil || after.UnknownSince != nil || after.UID != before.UID || after.AppliedGen != before.AppliedGen {
				t.Fatalf("the older outage degraded the newer ready state or wrote a backoff: %+v", after)
			}
		})
	})

	t.Run("an unbound observation of another instance or spec is a collision that binds nothing foreign", func(t *testing.T) {
		unbound := func(t *testing.T) *rig {
			r := newRig(t)
			r.admit(svcOnly)
			r.s.Resources.AutoReady = true
			r.s.Resources.LoseEnsure("network", errors.New("connection reset"))
			r.start()
			r.pass()
			if net := r.res("network"); net.Bound() || net.EnsureRequestedAt == nil {
				t.Fatalf("fixture: %+v", net)
			}
			r.s.Resources.LoseEnsure("network", nil)
			return r
		}
		t.Run("a foreign UID with a mismatching fingerprint", func(t *testing.T) {
			r := unbound(t)
			r.s.Resources.MisreportUID("network", "foreign-uid-1")
			r.s.Resources.MisreportFingerprint("network", "sha256:"+strings.Repeat("7", 64), false)
			r.tick(10 * time.Second)
			r.pass()
			agg := r.get()
			net := agg.Resource("network")
			if net.UID != "" || net.BoundAt != nil || net.Reason != execution.ReasonResourceCollision || agg.Run.FailureReason != execution.Reason(execution.ReasonResourceCollision) {
				t.Fatalf("the foreign UID was bound or the collision missed: %+v", net)
			}
			if r.s.Resources.Count("ensure:network") < 2 {
				t.Fatal("fixture: the object was never re-ensured")
			}
		})
		t.Run("an empty fingerprint is unknown, binds nothing and never opens the barrier", func(t *testing.T) {
			r := unbound(t)
			r.s.Resources.MisreportFingerprint("network", "", false)
			r.tick(10 * time.Second)
			r.pass()
			net := r.res("network")
			if net.UID != "" || net.Phase != execution.PhaseUnknown || net.UnknownSince == nil || net.IsReady() || net.Reason != "resource_unverified" {
				t.Fatalf("an unverifiable observation must stay unknown and unbound: %+v", net)
			}
			if r.get().Run.FailureIntentAt != nil {
				t.Fatal("unknown is not loss")
			}
			// Affirmative evidence adopts it.
			r.s.Resources.MisreportFingerprint("network", "", true)
			r.tick(10 * time.Second)
			r.passes(2)
			if net := r.res("network"); !net.IsReady() || !net.Bound() {
				t.Fatalf("a matching fingerprint adopts the object: %+v", net)
			}
		})
		t.Run("a ready service with an empty fingerprint never opens the barrier or releases the holder", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.up()
			r.s.Resources.MisreportFingerprint("service.db", "", false)
			r.tick(2 * time.Second)
			r.pass()
			if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("dispatch on unverified readiness: %v", err)
			}
			if db := r.res("service.db"); db.Phase != execution.PhaseUnknown || db.IsReady() {
				t.Fatalf("%+v", db)
			}
		})
	})

	t.Run("a preparation that is ready without a trusted receipt fails at its own budget", func(t *testing.T) {
		r := newRig(t)
		r.admit(svcOnly)
		r.s.Resources.AutoReady = true
		r.start()
		r.passes(2)
		if p := r.res("preparation"); p.Phase != execution.PhaseReady || p.Receipt != nil {
			t.Fatalf("fixture: %+v", p)
		}
		r.tick(300 * time.Second) // inside the preparation budget
		r.pass()
		if r.get().Run.FailureIntentAt != nil {
			t.Fatal("expired early")
		}
		r.tick(400 * time.Second) // past the 600s budget, far inside the 3600s run deadline
		r.pass()
		if got := r.get().Run.FailureReason; got != execution.Reason(execution.ReasonPreparationFailed) {
			t.Fatalf("a ready preparation without a receipt must fail at its budget, got %q", got)
		}
		r.drain(r.s.Svc, 10)
		if agg := r.get(); agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() {
			t.Fatalf("cleanup: %s", agg.Run.Status)
		}
	})

	t.Run("a controller that cannot own the run model changes nothing, even past the deadline", func(t *testing.T) {
		refuse := func(name string, prepare func(r *rig) *execution.Service) {
			t.Run(name, func(t *testing.T) {
				r := newRig(t)
				r.admit(withDB)
				r.up()
				a := r.mustClaim()
				r.started(a)
				svc := prepare(r)
				r.tick(2 * time.Hour) // the total deadline has passed
				before := r.snapshot()
				calls := len(r.s.Resources.Calls())
				starts := r.starts()
				if _, err := svc.ReconcileRun(ctx, r.ref()); !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("reconcile: %v", err)
				}
				if _, err := svc.ReconcileTenant(ctx, SharedScope.TenantID); err != nil && !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("tenant reconcile: %v", err)
				}
				if _, err := svc.RecordBegin(ctx, SharedScope, a.AttemptID, beginReq()); !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("begin: %v", err)
				}
				if _, err := svc.RecordPreparationReceipt(ctx, SharedScope, r.run.ID, runtimev2.PreparationReceiptRequest{
					Commit: CommitA, TreeID: CommitC, HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1}); !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("receipt: %v", err)
				}
				zero := int64(0)
				res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
				h, _ := runtimev1.ResultHash(res)
				if _, err := svc.CommitResult(ctx, execution.ResultCommit{Scope: SharedScope, AttemptID: a.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res}}); !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("result: %v", err)
				}
				if err := svc.ObserveAttempt(ctx, SharedScope, a.AttemptID, execution.Observation{Kind: execution.ObservedTerminated, WorkloadName: a.WorkloadName, ObservedAt: time.Now()}); !errors.Is(err, execution.ErrNotConfigured) {
					t.Fatalf("observation: %v", err)
				}
				if after := r.snapshot(); after != before {
					t.Fatalf("the refused controller changed the run:\n before %+v\n after  %+v", before, after)
				}
				if len(r.s.Resources.Calls()) != calls || r.starts() != starts {
					t.Fatal("a peer effect was made")
				}
			})
		}
		refuse("an unsupported persisted run model", func(r *rig) *execution.Service {
			r.s.CorruptModel(SharedScope, r.run.ID)
			return r.s.Svc
		})
		refuse("a controller without the run_shared composition", func(r *rig) *execution.Service { return r.s.Closed() })
	})
}
