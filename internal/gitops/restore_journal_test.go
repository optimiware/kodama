package gitops_test

import (
	"context"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
	"github.com/optimiware/kodama/internal/gitops/localgit"
	"github.com/optimiware/kodama/internal/gitpush"
	pt "github.com/optimiware/kodama/internal/gitpush/gitpushtest"
)

// journalWorker answers the journal reads of Recovery from the push module's
// scripted worker (the same volume the reconciler reads evidence from).
type journalWorker struct {
	*gitopstest.Worker
	evidence *pt.Worker
}

func (w journalWorker) ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	return w.evidence.ReadJournal(ctx, tenant, req)
}

// Test_unit_gitops_restore_reconciles_journal: a volume restored from an older
// snapshot reconciles with the real push records. An entry in flight whose own
// journal entries are gone settles unknown (journal_regressed), never refused and
// never applied; no event is emitted for it; it keeps blocking its writer and
// the seed until an operator resolves it; the restored journal becomes the
// baseline (so a later receive settles normally); access reopens only then; and a
// repository that did not regress is untouched (GR-15, GR-A10, GR-A13).
func Test_unit_gitops_restore_reconciles_journal(t *testing.T) {
	ctx := context.Background()
	f := pt.New(t, pt.OpenMem)
	w := f.Worker
	eng := f.Human(pt.Engineer, pt.RepoMain)
	b := pt.OID("b")
	repoID := pt.RepoMain

	// receive journals one create the way the hooks would, optionally reconciling.
	receive := func(repo, receiveID, branch string) (gitv1.PushAuthorizeResponse, string) {
		t.Helper()
		ref := pt.Branch(branch)
		req := pt.Request(repo, eng, receiveID, false, pt.Create(ref, b))
		resp := f.MustAuthorize(req)
		f.MustIntent(resp.PushID, req)
		w.Prepared(pt.TenantA, repo, receiveID, ref, pt.Zero, b, pt.Zero)
		w.Committed(pt.TenantA, repo, receiveID, ref, pt.Zero, b)
		w.SetRef(pt.TenantA, repo, ref, b)
		return resp, ref
	}

	// Entries 1-2: before the snapshot. Entries 3-4: an applied push the control
	// plane settles from the journal (it has now seen head 4 ... and more below).
	first, _ := receive(repoID, "rcv_g1", "kept")
	if err := f.Reconcile(repoID); err != nil {
		t.Fatal(err)
	}
	pt.WantStatus(t, f.Record(repoID, first.PushID), gitv1.PushApplied)
	snapshotHead := w.Head(pt.TenantA, repoID) // the snapshot is taken here
	// After the snapshot: an applied push (the control plane sees the higher head), then a
	// receive still committing whose journal entries exist only beyond the snapshot.
	second, _ := receive(repoID, "rcv_g2", "newer")
	if err := f.Reconcile(repoID); err != nil {
		t.Fatal(err)
	}
	pt.WantStatus(t, f.Record(repoID, second.PushID), gitv1.PushApplied)
	late, lateRef := receive(repoID, "rcv_g3", "late")
	// A repository that did not regress.
	w.Prepared(pt.TenantA, pt.RepoDirect, "srv_d", pt.Branch("filler"), pt.Zero, b, pt.Zero)
	w.Committed(pt.TenantA, pt.RepoDirect, "srv_d", pt.Branch("filler"), pt.Zero, b)

	known, err := f.Svc.Settlement(ctx, pt.TenantA, repoID)
	if err != nil || known.KnownHead <= snapshotHead || known.InFlight != 1 {
		t.Fatalf("before the loss %+v %v", known, err)
	}
	eventsBefore := len(f.Env.Events(t, pt.ScopeA(), repoID))

	catalog := &gitopstest.Catalog{}
	catalog.Add(gitops.Repository{ID: repoID, Scope: pt.ScopeA(), DefaultBranch: "main"})
	catalog.Add(gitops.Repository{ID: pt.RepoDirect, Scope: pt.ScopeA(), DefaultBranch: "main"})
	fence := &gitopstest.Fence{}
	snaps := &gitopstest.Snapshots{OnRestore: func() { w.Restore(pt.TenantA, repoID, int(snapshotHead)) }} // an older volume
	vol := gitopstest.NewVolume(t)
	rec, err := gitops.NewRecovery(gitops.RecoveryConfig{
		Worker: journalWorker{Worker: gitopstest.NewWorker(vol), evidence: w}, Catalog: catalog, Fence: fence, Pushes: gitops.PushesOf(f.Svc),
		Holds: &gitopstest.Holds{}, Objects: localgit.Objects{Locator: vol}, Store: &gitopstest.Recoveries{}, Snapshots: snaps,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := rec.Restore(ctx, pt.TenantA, gitops.RestoreSource{SnapshotID: "snap-old"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Open || fence.IsClosed(pt.TenantA) {
		t.Fatalf("a reconciled restore reopens access: %v", report.Blockers())
	}
	st := statusOf(report, repoID)
	if !st.Regressed || !st.Acknowledged || st.JournalHead != snapshotHead || st.KnownHead <= snapshotHead || st.InFlight != 0 || st.Unknown != 1 {
		t.Fatalf("status %+v", st)
	}
	if other := statusOf(report, pt.RepoDirect); other.Regressed || other.Acknowledged {
		t.Fatalf("a repository that did not regress was touched: %+v", other)
	}
	// The in-flight entry is unknown, never refused and never applied; no event.
	got := f.Record(repoID, late.PushID)
	pt.WantState(t, got, lateRef, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	if after := len(f.Env.Events(t, pt.ScopeA(), repoID)); after != eventsBefore {
		t.Fatalf("a regressed entry emitted an event (%d -> %d)", eventsBefore, after)
	}
	// It blocks its writer and the seed until an operator resolves it.
	if blocked, _ := f.Svc.UnknownBlocksWriter(ctx, pt.ScopeA(), repoID, lateRef); !blocked {
		t.Fatal("an unknown entry must block its writer")
	}
	if busy, _ := f.Svc.UnsettledPush(ctx, pt.ScopeA(), repoID); !busy {
		t.Fatal("an unknown entry must block the seed")
	}
	// The restored journal is the baseline: a later receive settles normally.
	after, _ := receive(repoID, "rcv_g4", "after")
	if err := f.Reconcile(repoID); err != nil {
		t.Fatal(err)
	}
	pt.WantStatus(t, f.Record(repoID, after.PushID), gitv1.PushApplied)
	// The operator inspects the restored volume and resolves the entry.
	v, err := f.Svc.GetVersioned(ctx, "usr_admin", pt.ScopeA(), repoID, late.PushID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Svc.ResolveUnknown(ctx, gitpush.ResolveCommand{Principal: "usr_admin", Scope: pt.ScopeA(), RepositoryID: repoID, PushID: late.PushID,
		Ref: lateRef, Resolution: gitv1.EntryRefused, Reason: "the restored volume has no such branch", Expected: v.Version})
	if err != nil || res.Record.Status != gitv1.PushRefused {
		t.Fatalf("operator resolution: %+v %v", res, err)
	}
	if blocked, _ := f.Svc.UnknownBlocksWriter(ctx, pt.ScopeA(), repoID, lateRef); blocked {
		t.Fatal("a resolved entry no longer blocks its writer")
	}
}
