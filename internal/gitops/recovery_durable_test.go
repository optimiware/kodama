package gitops_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

// newRecovery builds another Recovery instance over the same collaborators: a
// restart, or another control-plane instance.
func (r *restoreRig) newRecovery() *gitops.Recovery {
	r.t.Helper()
	rec, err := gitops.NewRecovery(gitops.RecoveryConfig{Worker: r.worker, Catalog: r.catalog, Fence: r.fence, Pushes: r.pushes, Holds: r.holds,
		Objects: r.objects, Store: r.recoveries, Snapshots: r.snaps, Backups: r.b})
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// callsOf lists the recorded worker calls of one repository, in order, without
// operation IDs.
func callsOf(w *gitopstest.Worker, repo string) []string {
	var out []string
	for _, c := range w.Calls {
		f := strings.Fields(c)
		if len(f) >= 2 && f[1] == repo {
			name := f[0]
			if name == "setRestoreFence" && len(f) > 2 {
				name += " " + f[2]
			}
			out = append(out, name)
		}
	}
	return out
}

// Test_unit_gitops_restore_survives_failure_and_restart: the selected recovery
// source and each completed step are durable. A replay that failed leaves the
// tenant closed, and a restart (a new Recovery over the same store) reconciling
// the half-restored volume never reopens it; resuming repeats only what did not
// complete; a different source cannot replace the active recovery; a worker fence
// that could not be left is not recorded as a completed replay (GR-15, GR-A10).
func Test_unit_gitops_restore_survives_failure_and_restart(t *testing.T) {
	ctx := context.Background()
	r := newRestoreRig(t)
	r.loseVolume()
	src := gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "latest"}

	// r_rs1 replays; r_rs2's replay fails.
	r.worker.Hook = func(op, repo string) {
		if op == "initialize" && repo == "r_rs2" { // r_rs1 replayed before it
			r.worker.Fail["replayBundle"] = errors.New("worker restarting")
		}
	}
	rep, err := r.rec.Restore(ctx, tenantA, src)
	if err == nil || rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatalf("a failed replay: open=%v closed=%v err=%v", rep.Open, r.fence.IsClosed(tenantA), err)
	}
	if got := r.vol.Refs(tenantA, "r_rs2"); len(got) != 0 {
		t.Fatalf("the failed repository has refs: %v", got)
	}

	// The control plane restarts: a new instance over the same durable store. The
	// worker is healthy again, but the repository was never replayed, so
	// reconciliation keeps access closed instead of reopening an empty repository.
	delete(r.worker.Fail, "replayBundle")
	r.worker.Hook = nil
	restarted := r.newRecovery()
	for range 2 {
		rep, err = restarted.Reconcile(ctx, tenantA)
		if err != nil || rep.Open || !r.fence.IsClosed(tenantA) {
			t.Fatalf("reconcile after the restart: open=%v err=%v", rep.Open, err)
		}
	}
	if !strings.Contains(strings.Join(rep.Blockers(), "\n"), "r_rs2: the bundle replay of backup "+r.backup.ID+" has not completed") {
		t.Fatalf("blockers: %v", rep.Blockers())
	}
	if !statusOf(rep, "r_rs1").Replayed || statusOf(rep, "r_rs2").Replayed {
		t.Fatalf("recorded replays: r_rs1=%v r_rs2=%v", statusOf(rep, "r_rs1").Replayed, statusOf(rep, "r_rs2").Replayed)
	}
	if rep.RecoveryID == "" {
		t.Fatal("the report names the recovery")
	}

	// Another source cannot take over the active recovery.
	if _, err := restarted.Restore(ctx, tenantA, gitops.RestoreSource{SnapshotID: "snap-other", BackupID: r.backup.ID}); !errors.Is(err, gitops.ErrRecoveryActive) {
		t.Fatalf("another source: %v", err)
	}
	if _, err := restarted.Restore(ctx, tenantA, gitops.RestoreSource{BackupID: r.backup.ID}); !errors.Is(err, gitops.ErrRecoveryActive) {
		t.Fatalf("another source (no snapshot): %v", err)
	}

	// Resuming repeats only the repository that did not complete: the volume is
	// not restored again and r_rs1 is not replayed again.
	restoredBefore, replaysBefore := len(r.snaps.Restored), r.worker.Count("replayBundle r_rs1")
	rep, err = restarted.Restore(ctx, tenantA, src)
	if err != nil || !rep.Open || r.fence.IsClosed(tenantA) {
		t.Fatalf("resume: open=%v blockers=%v err=%v", rep.Open, rep.Blockers(), err)
	}
	if len(r.snaps.Restored) != restoredBefore || r.worker.Count("replayBundle r_rs1") != replaysBefore {
		t.Fatalf("completed steps were repeated: restores %d->%d, r_rs1 replays %d->%d", restoredBefore, len(r.snaps.Restored), replaysBefore, r.worker.Count("replayBundle r_rs1"))
	}
	for id, want := range r.refs {
		for ref, c := range want {
			if got := r.vol.Refs(tenantA, id)[ref]; got != c {
				t.Errorf("%s %s = %q, want %q", id, ref, got, c)
			}
		}
	}
	// Reconcile on a finished recovery needs the fence closed: it is a restore-only action.
	if _, err := restarted.Reconcile(ctx, tenantA); !errors.Is(err, gitops.ErrNotRestoring) {
		t.Fatalf("reconcile after the recovery: %v", err)
	}
}

// Test_unit_gitops_restore_fence_left_before_recording: the worker's restore fence
// is entered before and left after every replay; a fence that cannot be left means
// the replay is not recorded, so the next pass repeats it (idempotently) and
// access stays closed meanwhile.
func Test_unit_gitops_restore_fence_left_before_recording(t *testing.T) {
	ctx := context.Background()
	r := newRestoreRig(t)
	r.loseVolume()
	src := gitops.RestoreSource{BackupID: "latest"}

	// Leaving r_rs1's fence fails once the replay ran.
	r.worker.Hook = func(op, repo string) {
		if op == "replayBundle" && repo == "r_rs1" {
			r.worker.Fail["setRestoreFence"] = errors.New("worker unreachable")
		}
	}
	rep, err := r.rec.Restore(ctx, tenantA, src)
	if err == nil || rep.Open {
		t.Fatalf("an unleavable fence: open=%v err=%v", rep.Open, err)
	}
	if !r.worker.Fenced(tenantA, "r_rs1") {
		t.Fatal("the test expects r_rs1 to be left fenced")
	}
	if statusOf(rep, "r_rs1").Replayed {
		t.Fatal("a replay whose fence was not left was recorded")
	}
	delete(r.worker.Fail, "setRestoreFence")
	r.worker.Hook = nil
	rep, err = r.newRecovery().Restore(ctx, tenantA, src)
	if err != nil || !rep.Open {
		t.Fatalf("resume: %v %v", rep.Blockers(), err)
	}
	for _, id := range []string{"r_rs1", "r_rs2"} {
		if r.worker.Fenced(tenantA, id) {
			t.Errorf("%s is still fenced", id)
		}
	}
	// Each replay ran inside its fence: initialize, enter, replay, leave.
	want := []string{"initialize", "setRestoreFence true", "replayBundle", "setRestoreFence false"}
	if got := callsOf(r.worker, "r_rs2"); !strings.Contains(","+strings.Join(got, ",")+",", ","+strings.Join(want, ",")+",") {
		t.Fatalf("r_rs2 calls %v, want the sequence %v", got, want)
	}
	// The worker refuses a replay outside the fence.
	if _, err := r.worker.ReplayBundle(ctx, tenantA, gitops.ReplayRequest{RepositoryID: "r_rs2", OperationID: "op_x"}, strings.NewReader("")); err == nil {
		t.Fatal("a replay outside the restore fence was accepted")
	}
}

// Test_unit_gitops_restore_initializes_every_repository: a lost volume is
// recovered for every repository of the manifest, populated or empty; only the
// replay is skipped for an empty one (GR-15).
func Test_unit_gitops_restore_initializes_every_repository(t *testing.T) {
	ctx := context.Background()
	br := newBackupRig(t, 3)
	br.fence.Log = &br.log
	r := &restoreRig{backupRig: br,
		pushes: &gitopstest.Pushes{InFlight: map[string]int{}, Unknown: map[string]int{}, Known: map[string]int64{}}, refs: map[string]map[string]string{}}
	r.refs["r_pop"] = r.seed(tenantA, "r_pop")
	r.vol.Init(tenantA, "r_empty")
	r.catalog.Add(repo(tenantA, "r_empty"))
	m, err := r.b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.backup = m
	r.rec = r.newRecovery()
	r.loseVolume()
	if _, err := os.Stat(filepath.Join(r.vol.Root, string(tenantA))); !os.IsNotExist(err) {
		t.Fatal("the volume was not lost")
	}
	rep, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{BackupID: "latest"})
	if err != nil || !rep.Open {
		t.Fatalf("restore: open=%v blockers=%v err=%v", rep.Open, rep.Blockers(), err)
	}
	for _, id := range []string{"r_pop", "r_empty"} {
		if _, err := os.Stat(r.vol.RepositoryDir(tenantA, id)); err != nil {
			t.Errorf("%s was not initialized: %v", id, err)
		}
		if _, done := rep.Replays[id]; !done {
			t.Errorf("%s has no recorded completion", id)
		}
	}
	if got := r.vol.Refs(tenantA, "r_empty"); len(got) != 0 {
		t.Fatalf("an empty repository has refs: %v", got)
	}
	for ref, c := range r.refs["r_pop"] {
		if got := r.vol.Refs(tenantA, "r_pop")[ref]; got != c {
			t.Errorf("r_pop %s = %q, want %q", ref, got, c)
		}
	}
	if n := r.worker.Count("replayBundle r_empty"); n != 0 || r.worker.Count("setRestoreFence r_empty") != 0 {
		t.Fatal("an empty repository was replayed or fenced")
	}
	if r.worker.Count("initialize r_empty") != 1 || r.worker.Count("replayBundle r_pop") != 1 {
		t.Fatalf("calls: %v", r.worker.Calls)
	}
}

// copyDir copies a repository directory (a volume snapshot of it).
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-R", from, to).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
}

// Test_unit_gitops_restore_discrepancies_block_until_resolved: a volume snapshot
// older than the bundle brings back what the recovery point no longer has (a
// branch deleted after the snapshot), cannot be advanced by the replay (main) and
// disagrees where history was rewritten. Each difference keeps Git access closed
// until an audited operator resolution; a resolution names the exact observed
// value, so a stale one never applies (GR-15, GR-A10).
func Test_unit_gitops_restore_discrepancies_block_until_resolved(t *testing.T) {
	ctx := context.Background()
	br := newBackupRig(t, 3)
	br.fence.Log = &br.log
	r := &restoreRig{backupRig: br,
		pushes: &gitopstest.Pushes{InFlight: map[string]int{}, Unknown: map[string]int{}, Known: map[string]int64{}}, refs: map[string]map[string]string{}}
	for _, id := range []string{"r_rs1", "r_rs2"} {
		r.refs[id] = r.seed(tenantA, id)
	}
	r.rec = r.newRecovery()

	// The volume snapshot: the seeded state.
	snap := t.TempDir()
	for _, id := range []string{"r_rs1", "r_rs2"} {
		copyDir(t, r.vol.RepositoryDir(tenantA, id), filepath.Join(snap, id))
	}
	// After the snapshot: main advances, a branch is deleted, one is created and
	// one is rewritten to an unrelated history; then the backup is taken.
	at := r.clock.Now()
	m1 := r.refs["r_rs1"]["refs/heads/main"]
	m2 := r.vol.Commit(tenantA, "r_rs1", m1, "main advanced after the snapshot", at)
	r.vol.SetRef(tenantA, "r_rs1", "refs/heads/main", m2)
	r.vol.SetRef(tenantA, "r_rs1", "refs/heads/kodama/work/wi_deleted_later", m1) // exists in the snapshot only
	r.vol.SetRef(tenantA, "r_rs1", "refs/heads/kodama/work/wi_created_later", m2)
	rewritten := r.vol.Commit(tenantA, "r_rs2", "", "unrelated history", at)
	r.vol.SetRef(tenantA, "r_rs2", "refs/heads/kodama/work/wi_1", rewritten)
	// The snapshot did contain wi_deleted_later: add it to the snapshot copy and delete it live.
	gitopstest.Git(t, filepath.Join(snap, "r_rs1"), nil, "fetch", "--quiet", "--no-tags", r.vol.RepositoryDir(tenantA, "r_rs1"), m1)
	gitopstest.Git(t, filepath.Join(snap, "r_rs1"), nil, "update-ref", "refs/heads/kodama/work/wi_deleted_later", m1)
	r.vol.DelRef(tenantA, "r_rs1", "refs/heads/kodama/work/wi_deleted_later")
	m, err := r.b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.backup = m

	// Disaster: the volume is replaced by the (older) snapshot.
	r.loseVolume()
	r.snaps.OnRestore = func() {
		for _, id := range []string{"r_rs1", "r_rs2"} {
			copyDir(t, filepath.Join(snap, id), r.vol.RepositoryDir(tenantA, id))
		}
	}
	rep, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{SnapshotID: "snap-old", BackupID: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Open || !r.fence.IsClosed(tenantA) {
		t.Fatal("access reopened although the volume differs from the recovery point")
	}
	// The replay created what the volume lacked and changed nothing else.
	if got := r.vol.Ref(tenantA, "r_rs1", "refs/heads/kodama/work/wi_created_later"); got != m2 {
		t.Fatalf("the branch created after the snapshot was not replayed: %q", got)
	}
	if got := r.vol.Ref(tenantA, "r_rs1", "refs/heads/main"); got != m1 {
		t.Fatalf("the replay moved main to %s", got)
	}
	if got := r.vol.Ref(tenantA, "r_rs1", "refs/heads/kodama/work/wi_deleted_later"); got != m1 {
		t.Fatalf("the replay deleted a branch: %q", got)
	}
	if res := rep.Replays["r_rs1"]; len(res.Conflicts) != 1 || res.Conflicts[0].Ref != "refs/heads/main" || res.Conflicts[0].Relation != gitops.BundleAhead {
		t.Fatalf("r_rs1 replay %+v", res)
	}
	if res := rep.Replays["r_rs2"]; len(res.Conflicts) != 1 || res.Conflicts[0].Relation != gitops.Diverged {
		t.Fatalf("r_rs2 replay %+v", res)
	}

	ds, err := r.rec.Discrepancies(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	want := []gitops.Discrepancy{
		{Repository: "r_rs1", Ref: "refs/heads/kodama/work/wi_deleted_later", Kind: gitops.DiscrepancyExtra, Actual: m1},
		{Repository: "r_rs1", Ref: "refs/heads/main", Kind: gitops.DiscrepancyDiffers, Expected: m2, Actual: m1},
		{Repository: "r_rs2", Ref: "refs/heads/kodama/work/wi_1", Kind: gitops.DiscrepancyDiffers, Expected: rewritten, Actual: r.refs["r_rs2"]["refs/heads/kodama/work/wi_1"]},
	}
	if len(ds) != len(want) {
		t.Fatalf("discrepancies %+v", ds)
	}
	for i := range want {
		if ds[i] != want[i] {
			t.Errorf("discrepancy %d = %+v, want %+v", i, ds[i], want[i])
		}
	}

	// A resolution must name a difference the volume shows exactly; nothing is
	// recorded otherwise.
	stale := want[1]
	stale.Actual = strings.Repeat("9", 40)
	if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "invented", stale); !errors.Is(err, gitops.ErrNotDiscrepancy) {
		t.Fatalf("an invented discrepancy: %v", err)
	}
	if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "", "no operator", want[0]); err == nil {
		t.Fatal("a resolution without an operator")
	}
	if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "", want[0]); err == nil {
		t.Fatal("a resolution without a reason")
	}
	if cur, _, _ := r.recoveries.Current(ctx, tenantA); len(cur.Resolutions) != 0 {
		t.Fatalf("a refused resolution was recorded: %+v", cur.Resolutions)
	}

	// Each resolution is stored; another instance sees it; access opens with the last.
	if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "the branch was deleted after the snapshot; keep it dead later", want[0]); err != nil {
		t.Fatal(err)
	}
	other := r.newRecovery()
	if rep, _ = other.Reconcile(ctx, tenantA); rep.Open || len(statusOf(rep, "r_rs1").Discrepancies) != 1 {
		t.Fatalf("after one resolution: open=%v %v", rep.Open, rep.Blockers())
	}
	// A ref that changes after it was resolved is a new discrepancy.
	moved := r.vol.Commit(tenantA, "r_rs1", m1, "moved again", at.Add(1))
	r.vol.SetRef(tenantA, "r_rs1", "refs/heads/kodama/work/wi_deleted_later", moved)
	if rep, _ = other.Reconcile(ctx, tenantA); len(statusOf(rep, "r_rs1").Discrepancies) != 2 {
		t.Fatalf("a changed ref reuses a stale resolution: %+v", statusOf(rep, "r_rs1").Discrepancies)
	}
	r.vol.SetRef(tenantA, "r_rs1", "refs/heads/kodama/work/wi_deleted_later", m1)
	for _, d := range want[1:] {
		if rep.Open {
			t.Fatal("reopened before the last resolution")
		}
		if err := other.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "inspected: the volume is the truth", d); err != nil {
			t.Fatal(err)
		}
		rep, _ = other.Reconcile(ctx, tenantA)
	}
	if !rep.Open || r.fence.IsClosed(tenantA) {
		t.Fatalf("all resolved: open=%v %v", rep.Open, rep.Blockers())
	}
	if cur, _, _ := r.recoveries.Current(ctx, tenantA); len(cur.Resolutions) != 3 || cur.Status != gitops.RecoveryComplete || cur.Resolutions[0].By != "usr_operator" {
		t.Fatalf("recorded: %+v", cur)
	}
	if err := r.rec.ResolveDiscrepancy(ctx, tenantA, "usr_operator", "late", want[0]); !errors.Is(err, gitops.ErrNotRestoring) {
		t.Fatalf("a resolution outside a restore: %v", err)
	}
}

// Test_unit_gitops_restore_skips_unlisted_repositories: a repository of the backup
// that the catalog no longer lists (deleted since) is not recreated on the worker,
// is reported, and does not block the recovery; the repositories that are listed
// are restored (GR-15).
func Test_unit_gitops_restore_skips_unlisted_repositories(t *testing.T) {
	ctx := context.Background()
	r := newRestoreRig(t)
	r.catalog.Remove(tenantA, "r_rs2") // deleted since the backup
	r.loseVolume()
	rep, err := r.rec.Restore(ctx, tenantA, gitops.RestoreSource{BackupID: "latest"})
	if err != nil || !rep.Open {
		t.Fatalf("restore: open=%v blockers=%v err=%v", rep.Open, rep.Blockers(), err)
	}
	if len(rep.Unlisted) != 1 || rep.Unlisted[0] != "r_rs2" {
		t.Fatalf("unlisted %v", rep.Unlisted)
	}
	if _, err := os.Stat(r.vol.RepositoryDir(tenantA, "r_rs2")); !os.IsNotExist(err) {
		t.Fatalf("a deleted repository was recreated: %v", err)
	}
	if n := r.worker.Count("initialize r_rs2") + r.worker.Count("replayBundle r_rs2") + r.worker.Count("setRestoreFence r_rs2"); n != 0 {
		t.Fatalf("the worker was asked about a deleted repository %d times", n)
	}
	if _, done := rep.Replays["r_rs2"]; done {
		t.Fatal("a replay was recorded for an unlisted repository")
	}
	for ref, c := range r.refs["r_rs1"] {
		if got := r.vol.Refs(tenantA, "r_rs1")[ref]; got != c {
			t.Errorf("r_rs1 %s = %q, want %q", ref, got, c)
		}
	}
}
