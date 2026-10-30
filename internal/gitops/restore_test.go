package gitops_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

type restoreRig struct {
	*backupRig
	pushes  *gitopstest.Pushes
	rec     *gitops.Recovery
	refs    map[string]map[string]string
	backup  gitops.Manifest
	started bool
}

func newRestoreRig(t *testing.T) *restoreRig {
	br := newBackupRig(t, 3)
	br.fence.Log = &br.log
	r := &restoreRig{backupRig: br,
		pushes: &gitopstest.Pushes{InFlight: map[string]int{}, Unknown: map[string]int{}, Known: map[string]int64{}}, refs: map[string]map[string]string{}}
	for _, id := range []string{"r_rs1", "r_rs2"} {
		r.refs[id] = r.seed(tenantA, id)
	}
	m, err := r.b.Backup(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.backup = m
	r.log = nil
	rec, err := gitops.NewRecovery(gitops.RecoveryConfig{Worker: r.worker, Catalog: r.catalog, Fence: r.fence, Pushes: r.pushes, Holds: r.holds,
		Objects: r.objects, Store: r.recoveries, Snapshots: r.snaps, Backups: r.b})
	if err != nil {
		t.Fatal(err)
	}
	r.rec = rec
	return r
}

// loseVolume deletes every repository of the tenant: the volume is gone.
func (r *restoreRig) loseVolume() {
	if err := os.RemoveAll(filepath.Join(r.vol.Root, string(tenantA))); err != nil {
		r.t.Fatal(err)
	}
}

// Test_unit_gitops_restore_closed_until_consistent: a restore closes Git access
// before it touches anything, keeps it closed through every failure and blocker
// (bundles that do not verify, entries in flight, an unreadable journal or
// worker, a pinned commit the volume lost) and reopens it only when every
// repository reconciled; a replayed volume has identical refs; Reconcile outside
// a restore changes nothing (GR-15, GR-A10).
func Test_unit_gitops_restore_closed_until_consistent(t *testing.T) {
	ctx := context.Background()
	r := newRestoreRig(t)
	r.loseVolume()
	r.worker.Hook = func(op, _ string) {
		if (op == "initialize" || op == "replayBundle") && !r.fence.IsClosed(tenantA) {
			t.Error("the volume was touched while Git access was open")
		}
	}

	// Closing the fence fails: nothing is touched.
	r.fence.FailClose = errors.New("fence store down")
	if _, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "latest"}); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("an unclosable fence: %v", err)
	}
	if len(r.snaps.Restored) != 0 || r.worker.Count("initialize") != 0 {
		t.Fatal("a restore started with Git access open")
	}
	r.fence.FailClose = nil

	// A bundle that does not verify aborts the replay; access stays closed and no
	// ref of the damaged repository appears.
	e1 := manifestEntry(r.backup, "r_rs1")
	r.store.Corrupt(e1.Key)
	rep, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "latest"})
	if !errors.Is(err, gitops.ErrBackupCorrupt) || rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatalf("a corrupted bundle: open=%v closed=%v %v", rep.Open, r.fence.IsClosed(tenantA), err)
	}
	if len(r.vol.Refs(tenantA, "r_rs1")) != 0 {
		t.Fatal("refs of a bundle that did not verify were applied")
	}
	r.store.Corrupt(e1.Key) // flip the same byte back

	// In-flight entries keep access closed; unknown ones do not.
	r.pushes.InFlight["r_rs2"] = 1
	r.pushes.Unknown["r_rs1"] = 2
	r.holds.Set("r_rs1", gitops.Held{Pins: []gitops.PinnedRef{
		{Ref: "refs/kodama/runs/run_1/1", Commit: r.refs["r_rs1"]["refs/kodama/runs/run_1/1"]}, // restored by the bundle
		{Ref: "refs/kodama/runs/run_2/1", Commit: strings.Repeat("e", 40)},                     // lost with the volume
	}})
	rep, err = r.rec.Restore(ctx, tenantA, gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatalf("access reopened with blockers: %v", rep.Blockers())
	}
	blockers := strings.Join(rep.Blockers(), "\n")
	for _, want := range []string{"r_rs2: 1 push entries are still in flight", "r_rs1: the commit " + strings.Repeat("e", 40)} {
		if !strings.Contains(blockers, want) {
			t.Errorf("blockers lack %q:\n%s", want, blockers)
		}
	}
	for id, want := range r.refs { // the replay restored every ref of both repositories
		got := r.vol.Refs(tenantA, id)
		for ref, c := range want {
			if got[ref] != c {
				t.Errorf("%s %s = %q, want %q", id, ref, got[ref], c)
			}
		}
		if res := rep.Replays[id]; res.Created+res.Unchanged != len(want) || len(res.Conflicts) != 0 {
			t.Errorf("%s replay %+v", id, res)
		}
	}
	if st := statusOf(rep, "r_rs1"); st.Unknown != 2 || len(st.LostPins) != 1 {
		t.Fatalf("r_rs1: %+v", st)
	}

	// Resolve the blockers one by one; each repeated Reconcile stays closed until the last.
	r.pushes.InFlight["r_rs2"] = 0
	rep, err = r.rec.Reconcile(ctx, tenantA)
	if err != nil || rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatalf("the lost pin still blocks: open=%v %v", rep.Open, err)
	}
	r.holds.Set("r_rs1", gitops.Held{Pins: []gitops.PinnedRef{{Ref: "refs/kodama/runs/run_1/1", Commit: r.refs["r_rs1"]["refs/kodama/runs/run_1/1"]}}})
	r.worker.Fail["journal"] = &gitv1.ContractError{Code: "journal_unreadable", Message: "damaged"}
	if rep, _ = r.rec.Reconcile(ctx, tenantA); rep.Open || !statusOf(rep, "r_rs1").JournalUnreadable {
		t.Fatalf("an unreadable journal: %+v", rep)
	}
	r.worker.Fail["journal"] = errors.New("worker restarting")
	if rep, _ = r.rec.Reconcile(ctx, tenantA); rep.Open {
		t.Fatal("an unreachable worker reopened access")
	}
	delete(r.worker.Fail, "journal")
	rep, err = r.rec.Reconcile(ctx, tenantA)
	if err != nil || !rep.Open || r.fence.IsClosed(tenantA) {
		t.Fatalf("consistent: %+v %v", rep.Blockers(), err)
	}
	if got := strings.Join(r.log, ","); !strings.HasPrefix(got, "close,restoreVolume") || !strings.HasSuffix(got, ",open") || strings.Count(got, "open") != 1 {
		t.Fatalf("fence order %s", got)
	}
	// Once open, Reconcile refuses: it lowers journal heads, so it runs only in a restore.
	if _, err := r.rec.Reconcile(ctx, tenantA); !errors.Is(err, gitops.ErrNotRestoring) {
		t.Fatalf("reconcile outside a restore: %v", err)
	}
	// A fence closed for another reason is not reopened by a restore.
	other := tenantB
	if _, err := r.rec.Reconcile(ctx, other); !errors.Is(err, gitops.ErrNotRestoring) {
		t.Fatalf("another tenant: %v", err)
	}
}

func manifestEntry(m gitops.Manifest, id string) gitops.ManifestRepository {
	for _, e := range m.Repositories {
		if e.ID == id {
			return e
		}
	}
	panic("no manifest entry " + id)
}

func statusOf(rep gitops.Report, id string) gitops.RepositoryStatus {
	for _, s := range rep.Repositories {
		if s.ID == id {
			return s
		}
	}
	panic("no status " + id)
}

// Test_unit_gitops_restore_pins_repaired_and_never_rewinds: a retained ref the
// volume lost is recreated when its commit exists; a bundle replay fast-forwards
// but never rewinds or deletes, reporting a diverged ref.
func Test_unit_gitops_restore_pins_repaired_and_never_rewinds(t *testing.T) {
	ctx := context.Background()
	r := newRestoreRig(t)
	id := "r_rs1"
	pinRef := "refs/kodama/runs/run_1/1"
	pinned := r.refs[id][pinRef]
	// A newer state than the backup: main advanced and a branch diverged; the pin was lost.
	newer := r.vol.Commit(tenantA, id, r.refs[id]["refs/heads/main"], "newer than the backup", r.clock.Now())
	r.vol.SetRef(tenantA, id, "refs/heads/main", newer)
	other := r.vol.Commit(tenantA, id, "", "unrelated root", r.clock.Now())
	r.vol.SetRef(tenantA, id, "refs/heads/kodama/work/wi_1", other)
	r.vol.DelRef(tenantA, id, "refs/tags/v1")
	r.vol.DelRef(tenantA, id, pinRef)
	r.holds.Set(id, gitops.Held{Pins: []gitops.PinnedRef{{Ref: pinRef, Commit: pinned}}})

	rep, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{BackupID: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	// The replay never rewinds: main stays newer, the diverged branch stays, and
	// both are discrepancies that keep access closed until an operator resolves them.
	if rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatal("access reopened with the volume ahead of and diverged from the recovery point")
	}
	if got := r.vol.Ref(tenantA, id, pinRef); got != pinned {
		t.Fatalf("the pin was not restored: %q", got)
	}
	if got := r.vol.Ref(tenantA, id, "refs/heads/main"); got != newer {
		t.Fatalf("the replay rewound main to %s", got)
	}
	res := rep.Replays[id]
	if len(res.Conflicts) != 2 || res.Conflicts[0].Ref != "refs/heads/kodama/work/wi_1" || res.Conflicts[0].Relation != gitops.Diverged ||
		res.Conflicts[1].Ref != "refs/heads/main" || res.Conflicts[1].Relation != gitops.RepositoryAhead || r.vol.Ref(tenantA, id, "refs/heads/kodama/work/wi_1") != other {
		t.Fatalf("replay %+v", res)
	}
	if r.vol.Ref(tenantA, id, "refs/tags/v1") == "" {
		t.Fatal("a ref the volume lacks was not recreated")
	}
	st := statusOf(rep, id)
	if len(st.Discrepancies) != 2 || st.Discrepancies[0].Kind != gitops.DiscrepancyDiffers || st.Discrepancies[1].Actual != newer {
		t.Fatalf("discrepancies %+v", st.Discrepancies)
	}
	for _, d := range st.Discrepancies {
		if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "inspected: the volume is newer than the backup", d); err != nil {
			t.Fatal(err)
		}
	}
	rep, err = r.rec.Reconcile(ctx, tenantA)
	if err != nil || !rep.Open {
		t.Fatalf("%v %v", rep.Blockers(), err)
	}
}
