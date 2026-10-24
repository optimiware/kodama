package deliverycleanup_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

const preview = "dep_preview"

// cleanup_after_work_done: a deployment's cleanup is scheduled by its own expiry,
// whatever became of the work item that produced it. The cleanup run has no work
// item, workspace or business WIP, so a done, cancelled or busy item neither
// blocks nor is touched by it (DQ-09, PX-A09).
func TestCleanupAfterWorkDone(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deliverycleanup.Policy{})
	w.rec.add(scopeA, preview, 24*time.Hour)

	// The originating work is over before the preview expires.
	w.m.CancelWork(scopeA, w.proj.Item)
	before, err := w.m.World.Snapshot(ctx, scopeA, w.proj.Item)
	if err != nil {
		t.Fatal(err)
	}

	// Not yet due: nothing is requested and nothing is admitted.
	w.m.Advance(23*time.Hour + 59*time.Minute)
	if rep := w.sweep(w.sched); len(rep.Items) != 0 || len(w.cleanupRuns()) != 0 || w.rec.snapshot(scopeA, preview).RequestedAt != nil {
		t.Fatalf("a deployment before its expiry was acted on: %+v", rep)
	}

	// Past the expiry the request and the run follow, with no work item left.
	w.m.Advance(2 * time.Minute)
	it := one(t, w.sweep(w.sched))
	wantOutcome(t, it, deliverycleanup.OutcomeAdmitted)
	if it.Round != 1 || it.RunID != execution.CleanupRunID(scopeA, preview, 1) {
		t.Fatalf("run identity %+v", it)
	}
	if got := w.rec.snapshot(scopeA, preview); got.RequestedAt == nil || got.State != deliverycleanup.RecordRequested {
		t.Fatalf("the cleanup request was not recorded: %+v", got)
	}
	runs := w.cleanupRuns()
	if len(runs) != 1 {
		t.Fatalf("%d cleanup runs", len(runs))
	}
	r := runs[0]
	if r.Purpose != execution.PurposeCleanup || r.DeploymentID != preview || r.WorkItemID != "" || r.WorkspaceID != "" || r.SourceCommit != "" ||
		r.Specification != nil || r.SourceRetention != execution.RetentionNotRequired || r.RequestedBy != execution.System || r.Status != execution.RunQueued {
		t.Fatalf("cleanup run %+v", r)
	}

	// It runs, holds no writer slot and leaves the work item as it was.
	in := w.claim()
	if in.Access != execution.AccessNone || in.WorkspaceID != "" {
		t.Fatalf("a cleanup attempt carries a workspace: %+v", in)
	}
	if writerOf(w, w.proj) != "" {
		t.Fatal("a cleanup run took the workspace writer slot")
	}
	if _, err := w.m.Succeed(in, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.m.Terminate(in, execution.LaunchStarted); err != nil {
		t.Fatal(err)
	}
	after, err := w.m.World.Snapshot(ctx, scopeA, w.proj.Item)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("cleanup changed the work item: %+v -> %+v", before, after)
	}

	// The cleanup script reports through the record; from then on the
	// deployment is done and the scheduler leaves it alone.
	w.rec.report(scopeA, preview, deliverycleanup.RecordDone)
	if rep := w.sweep(w.sched); len(rep.Items) != 0 {
		t.Fatalf("a deleted deployment was scheduled again: %+v", rep.Items)
	}
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("a second cleanup run exists")
	}
}

// A cleanup run is also scheduled while the work item's own run is busy, and a
// human-requested cleanup of a deployment without expiry (a stage) is admitted
// the same way; the clock alone never cleans a stage.
func TestCleanupIsIndependentOfBusinessWork(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	w.rec.add(scopeA, "dep_stage", 0)

	// A running work run holds the project's writer slot and a WIP place.
	work := w.m.MustAdmit(w.proj, w.m.Define(scopeA, workDefinition()), nil)
	working := w.m.MustClaim(tenant)
	if working.RunID != work.ID {
		t.Fatalf("claimed %s", working.RunID)
	}
	w.m.Start(working)

	w.m.Advance(30 * 24 * time.Hour)
	if rep := w.sweep(w.sched); len(rep.Items) != 0 {
		t.Fatalf("a deployment without an expiry was cleaned: %+v", rep.Items)
	}
	w.rec.with(scopeA, "dep_stage", func(d *recorded) {
		now := w.m.Clock.Now()
		d.RequestedAt, d.State = &now, deliverycleanup.RecordRequested
	})
	wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("the requested cleanup was not admitted")
	}
	// The work run is untouched and still holds its writer slot; the cleanup run
	// dispatches beside it without waiting for the slot.
	if got := w.m.Get(work); got.Run.Status != execution.RunRunning || writerOf(w, w.proj) != working.AttemptID {
		t.Fatalf("the work run changed: %+v", got.Run)
	}
	in := w.claim()
	if in.RunID == work.ID || in.Access != execution.AccessNone {
		t.Fatalf("cleanup dispatch %+v", in)
	}
	if writerOf(w, w.proj) != working.AttemptID {
		t.Fatal("the cleanup run disturbed the writer slot")
	}
}

// A deployment that is still applying cannot be cleaned: the request is refused
// by the record, nothing is admitted and a later pass retries.
func TestExpiredUnsettledDeploymentWaits(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	w.rec.add(scopeA, preview, time.Hour).Settled = false
	w.m.Advance(2 * time.Hour)
	wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeNotSettled)
	if len(w.cleanupRuns()) != 0 || w.rec.snapshot(scopeA, preview).RequestedAt != nil {
		t.Fatal("an unsettled deployment was cleaned")
	}
	w.rec.with(scopeA, preview, func(d *recorded) { d.Settled = true })
	wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
}

// An outage of the record or of admission is visible, admits nothing and the
// next pass recovers.
func TestOutagesRecoverOnTheNextPass(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	w.rec.add(scopeA, preview, time.Hour)
	w.m.Advance(2 * time.Hour)

	w.rec.outage = errors.New("records down")
	if _, err := w.sched.Sweep(context.Background(), scopeA); err == nil {
		t.Fatal("a record outage was not reported")
	}
	if len(w.cleanupRuns()) != 0 {
		t.Fatal("a run was admitted during the outage")
	}
	w.rec.outage = nil

	// Authorization unavailable at admission: the request is recorded, the run is not.
	calls := 0
	flaky := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) {
		c.Runs = failingAdmit{Runs: w.m.Svc, fail: func() error {
			calls++
			return execution.Unavailablef(errors.New("authorization down"), "down")
		}}
	})
	wantOutcome(t, one(t, w.sweep(flaky)), deliverycleanup.OutcomeError)
	if len(w.cleanupRuns()) != 0 || w.rec.snapshot(scopeA, preview).RequestedAt == nil || calls != 1 {
		t.Fatalf("outage state: runs=%d calls=%d", len(w.cleanupRuns()), calls)
	}
	wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
	if len(w.cleanupRuns()) != 1 {
		t.Fatal("the recovered pass did not admit exactly one run")
	}
}

// Authority revoked between the request and the admission: the record owner no
// longer authorizes the run, nothing is created, and the denial is visible.
func TestAuthorizationIsCurrentAtAdmission(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	d := w.rec.add(scopeA, preview, time.Hour)
	w.m.Advance(2 * time.Hour)
	// The scheduler read the record as requested; by admission it is deleted.
	stale := deliverycleanup.Candidate{Scope: scopeA, Deployment: preview, RequestedAt: ptr(w.m.Clock.Now()), State: deliverycleanup.RecordRequested}
	d.Deleted = true
	w.rec.foreign = false
	s := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) { c.Records = staleRecords{Records: w.rec, stale: stale} })
	wantOutcome(t, one(t, w.sweep(s)), deliverycleanup.OutcomeDenied)
	if len(w.cleanupRuns()) != 0 {
		t.Fatal("a deleted deployment was admitted")
	}
}

type staleRecords struct {
	deliverycleanup.Records
	stale  deliverycleanup.Candidate
	getErr error
}

func (s staleRecords) Get(ctx context.Context, scope domain.Scope, id string) (deliverycleanup.Candidate, error) {
	if s.getErr != nil {
		return deliverycleanup.Candidate{}, s.getErr
	}
	return s.Records.Get(ctx, scope, id)
}

func (s staleRecords) Scan(context.Context, domain.Scope, string, int) ([]deliverycleanup.Candidate, string, error) {
	return []deliverycleanup.Candidate{s.stale}, "", nil
}

func ptr[T any](v T) *T { return &v }

type failingAdmit struct {
	deliverycleanup.Runs
	fail func() error
}

func (f failingAdmit) AdmitCleanupRound(context.Context, execution.CleanupCommand, int) (*execution.Run, bool, error) {
	return nil, false, f.fail()
}

// lostAck admits the run and then loses the answer.
type lostAck struct{ deliverycleanup.Runs }

func (l lostAck) AdmitCleanupRound(ctx context.Context, cmd execution.CleanupCommand, round int) (*execution.Run, bool, error) {
	if _, _, err := l.Runs.AdmitCleanupRound(ctx, cmd, round); err != nil {
		return nil, false, err
	}
	return nil, false, execution.Unavailablef(errors.New("connection reset"), "the commit outcome is unknown")
}

// The scheduler's cursor state is safe under concurrent sweeps (run with -race).
func TestConcurrentSweepsShareNoState(t *testing.T) {
	w := newWorld(t, deliverycleanup.Policy{})
	for _, id := range []string{"dep_a", "dep_b", "dep_c"} {
		w.rec.add(scopeA, id, time.Hour)
	}
	w.m.Advance(2 * time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.sched.Sweep(context.Background(), scopeA)
		}()
	}
	wg.Wait()
	if n := len(w.cleanupRuns()); n != 3 {
		t.Fatalf("%d runs for 3 deployments", n)
	}
}
