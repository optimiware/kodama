package gitopstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
)

// Env is one environment of the durable-state Contract: the three stores of
// package gitops over one state and the clock they share.
type Env interface {
	Leases() gitops.BackupLeases
	Recoveries() gitops.RecoveryStore
	Intents() gitops.PruneIntents
	Clock() *Clock
	// Seed makes the repositories exist in the scope (a no-op for memory; the
	// SQL adapter references the repositories table).
	Seed(t testing.TB, scope domain.Scope, repositoryIDs ...string)
	// Reopen is the same state seen by new store instances: a restart, or
	// another control-plane instance.
	Reopen(t testing.TB) Env
}

// Open provisions a fresh Env.
type Open func(t testing.TB) Env

// Contract tenants, projects and repositories.
const (
	ContractTenantA domain.TenantID  = "t138c_acme"
	ContractTenantB domain.TenantID  = "t138c_globex"
	ContractProjA   domain.ProjectID = "p138c_inventory"
	ContractProjB   domain.ProjectID = "p138c_inventory_b"
	ContractRepo1                    = "r138c_one"
	ContractRepo2                    = "r138c_two"
)

// ContractScopeA and ContractScopeB are the scopes the contract uses.
func ContractScopeA() domain.Scope {
	return domain.Scope{TenantID: ContractTenantA, ProjectID: ContractProjA}
}
func ContractScopeB() domain.Scope {
	return domain.Scope{TenantID: ContractTenantB, ProjectID: ContractProjB}
}

// Scenario is one named case of the Contract.
type Scenario struct {
	Name string
	Run  func(*testing.T, Env)
}

// Scenarios are the shared Contract: the same code runs on the in-memory stores
// (unit layer) and on PostgreSQL (adapter integration layer), so the memory
// stand-ins are held to their SQL twins.
func Scenarios() []Scenario {
	return []Scenario{
		{"backup_lease_serializes_and_expires", BackupLeaseSerializesAndExpires},
		{"recovery_is_durable", RecoveryIsDurable},
		{"prune_intent_reserves", PruneIntentReserves},
	}
}

// Contract runs every scenario, each on a fresh Env.
func Contract(t *testing.T, open Open) {
	for _, s := range Scenarios() {
		t.Run(s.Name, func(t *testing.T) { s.Run(t, open(t)) })
	}
}

var bg = context.Background()

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func wantState(t testing.TB, l gitops.BackupLeases, tenant domain.TenantID, id string, want gitops.BackupState) {
	t.Helper()
	got, known, err := l.State(bg, tenant, id)
	if err != nil || !known || got != want {
		t.Fatalf("backup %s: state %q known=%v err=%v, want %q", id, got, known, err, want)
	}
}

// BackupLeaseSerializesAndExpires: one active run per tenant across instances; a
// live lease (renewed) refuses a second run; an expired one is abandoned by the
// next Acquire and its owner loses the lease; only an active run completes;
// tenants are independent (GR-15).
func BackupLeaseSerializesAndExpires(t *testing.T, e Env) {
	a, b := ContractTenantA, ContractTenantB
	other := e.Reopen(t) // another control-plane instance over the same state
	l, l2 := e.Leases(), other.Leases()
	clock := e.Clock()
	ttl := 10 * time.Minute

	must(t, l.Acquire(bg, a, "bk_1", ttl))
	wantState(t, l, a, "bk_1", gitops.BackupActive)
	wantErr(t, l2.Acquire(bg, a, "bk_2", ttl), gitops.ErrBackupInProgress)
	if _, known, err := l.State(bg, a, "bk_2"); err != nil || known {
		t.Fatalf("a refused run leaves no row: known=%v err=%v", known, err)
	}
	must(t, l.Acquire(bg, b, "bk_b1", ttl)) // another tenant is independent
	if _, known, _ := l.State(bg, b, "bk_1"); known {
		t.Fatal("another tenant sees the run")
	}

	// A renewed lease stays live past the first expiry.
	clock.Advance(8 * time.Minute)
	must(t, l.Renew(bg, a, "bk_1", ttl))
	clock.Advance(8 * time.Minute)
	wantErr(t, l2.Acquire(bg, a, "bk_2", ttl), gitops.ErrBackupInProgress)

	// An expired, never-completed run is abandoned by the next Acquire; the owner
	// loses its lease and can neither renew nor publish.
	clock.Advance(3 * time.Minute)
	must(t, l2.Acquire(bg, a, "bk_2", ttl))
	wantState(t, l, a, "bk_1", gitops.BackupFailed)
	wantState(t, l, a, "bk_2", gitops.BackupActive)
	wantErr(t, l.Renew(bg, a, "bk_1", ttl), gitops.ErrLeaseLost)
	wantErr(t, l.Complete(bg, a, "bk_1"), gitops.ErrLeaseLost)
	must(t, l.Fail(bg, a, "bk_1")) // idempotent
	wantState(t, l, a, "bk_1", gitops.BackupFailed)

	// Only the active run completes, once; Fail then is a no-op.
	must(t, l2.Complete(bg, a, "bk_2"))
	wantState(t, l, a, "bk_2", gitops.BackupComplete)
	wantErr(t, l.Complete(bg, a, "bk_2"), gitops.ErrLeaseLost)
	wantErr(t, l.Renew(bg, a, "bk_2", ttl), gitops.ErrLeaseLost)
	must(t, l.Fail(bg, a, "bk_2"))
	wantState(t, l, a, "bk_2", gitops.BackupComplete)

	// With nothing active the next run starts at once; an unknown run is unknown.
	must(t, l.Acquire(bg, a, "bk_3", ttl))
	must(t, l.Fail(bg, a, "bk_3"))
	wantState(t, l, a, "bk_3", gitops.BackupFailed)
	must(t, l.Fail(bg, a, "bk_unknown"))
	if _, known, err := l.State(bg, a, "bk_unknown"); err != nil || known {
		t.Fatalf("an unknown run: known=%v err=%v", known, err)
	}
	wantErr(t, l.Renew(bg, a, "bk_unknown", ttl), gitops.ErrLeaseLost)
}

// RecoveryIsDurable: the selected source and every completed step survive a
// restart and another instance; a different source cannot replace an active
// recovery; steps are recorded once and only while the recovery is active;
// a resolution is recorded once; a snapshot recovery completes only after its
// volume restore (GR-15).
func RecoveryIsDurable(t *testing.T, e Env) {
	a, b := ContractTenantA, ContractTenantB
	clock := e.Clock()
	r := e.Recoveries()
	if _, ok, err := r.Current(bg, a); err != nil || ok {
		t.Fatalf("a tenant without recovery: ok=%v err=%v", ok, err)
	}

	src := gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "bk_9"}
	rec, err := r.Begin(bg, a, "rcv_1", src, clock.Now())
	must(t, err)
	if rec.ID != "rcv_1" || rec.Status != gitops.RecoveryActive || rec.VolumeRestored || rec.SnapshotID != "snap-1" || rec.BackupID != "bk_9" || len(rec.Replays) != 0 {
		t.Fatalf("begin: %+v", rec)
	}
	again, err := r.Begin(bg, a, "rcv_other", src, clock.Now()) // the identical source resumes
	if err != nil || again.ID != "rcv_1" {
		t.Fatalf("resume: %+v, %v", again, err)
	}
	_, err = r.Begin(bg, a, "rcv_x", gitops.RestoreSource{SnapshotID: "snap-2", BackupID: "bk_9"}, clock.Now())
	wantErr(t, err, gitops.ErrRecoveryActive)
	_, err = r.Begin(bg, a, "rcv_x", gitops.RestoreSource{BackupID: "bk_9"}, clock.Now())
	wantErr(t, err, gitops.ErrRecoveryActive)

	// A snapshot recovery does not complete before its volume was restored.
	if r.Complete(bg, a, "rcv_1", clock.Now()) == nil {
		t.Fatal("a recovery completed before its volume was restored")
	}
	must(t, r.MarkVolumeRestored(bg, a, "rcv_1", clock.Now()))
	must(t, r.MarkReplayed(bg, a, "rcv_1", ContractRepo1, gitops.ReplayResult{Created: 3, Unchanged: 1, Conflicts: []gitops.RefConflict{
		{Ref: "refs/heads/main", Have: "a1" + pad(38), Want: "b2" + pad(38), Relation: gitops.RepositoryAhead}}}, clock.Now()))
	must(t, r.MarkReplayed(bg, a, "rcv_1", ContractRepo1, gitops.ReplayResult{Created: 99}, clock.Now())) // the first record stands
	must(t, r.MarkReplayed(bg, a, "rcv_1", ContractRepo2, gitops.ReplayResult{}, clock.Now()))
	d := gitops.Discrepancy{Repository: ContractRepo1, Ref: "refs/heads/main", Kind: gitops.DiscrepancyDiffers, Expected: "b2" + pad(38), Actual: "a1" + pad(38)}
	res := gitops.DiscrepancyResolution{Discrepancy: d, By: "usr_operator", Reason: "the volume is newer", At: clock.Now()}
	must(t, r.Resolve(bg, a, "rcv_1", res))
	must(t, r.Resolve(bg, a, "rcv_1", gitops.DiscrepancyResolution{Discrepancy: d, By: "usr_other", Reason: "again", At: clock.Now()})) // recorded once
	extra := gitops.Discrepancy{Repository: ContractRepo2, Ref: "refs/heads/old", Kind: gitops.DiscrepancyExtra, Actual: "c3" + pad(38)}
	must(t, r.Resolve(bg, a, "rcv_1", gitops.DiscrepancyResolution{Discrepancy: extra, By: "usr_operator", Reason: "deleted after the bundle", At: clock.Now()}))

	// A restart or another instance sees every recorded step.
	for _, store := range []gitops.RecoveryStore{r, e.Reopen(t).Recoveries()} {
		cur, ok, err := store.Current(bg, a)
		if err != nil || !ok || cur.ID != "rcv_1" || cur.Status != gitops.RecoveryActive || !cur.VolumeRestored {
			t.Fatalf("current: %+v ok=%v err=%v", cur, ok, err)
		}
		got := cur.Replays[ContractRepo1]
		if len(cur.Replays) != 2 || got.Created != 3 || got.Unchanged != 1 || len(got.Conflicts) != 1 || got.Conflicts[0].Relation != gitops.RepositoryAhead ||
			got.Conflicts[0].Ref != "refs/heads/main" {
			t.Fatalf("replays: %+v", cur.Replays)
		}
		if len(cur.Resolutions) != 2 {
			t.Fatalf("resolutions: %+v", cur.Resolutions)
		}
		for _, x := range cur.Resolutions {
			if x.Repository == ContractRepo1 && (x.By != "usr_operator" || x.Discrepancy != d) {
				t.Fatalf("the first resolution stands: %+v", x)
			}
		}
	}
	if _, ok, _ := r.Current(bg, b); ok {
		t.Fatal("another tenant sees the recovery")
	}

	// Complete is final and idempotent; a complete recovery takes no more steps.
	must(t, r.Complete(bg, a, "rcv_1", clock.Now()))
	must(t, r.Complete(bg, a, "rcv_1", clock.Now()))
	if r.MarkReplayed(bg, a, "rcv_1", "r138c_three", gitops.ReplayResult{}, clock.Now()) == nil ||
		r.Resolve(bg, a, "rcv_1", gitops.DiscrepancyResolution{Discrepancy: extra, By: "usr", Reason: "late", At: clock.Now()}) == nil ||
		r.MarkVolumeRestored(bg, a, "rcv_1", clock.Now()) == nil {
		t.Fatal("a complete recovery took another step")
	}
	cur, _, _ := r.Current(bg, a)
	if cur.Status != gitops.RecoveryComplete || len(cur.Replays) != 2 {
		t.Fatalf("complete: %+v", cur)
	}

	// The next recovery is a new one; a backup-only source needs no volume restore
	// and its own Current is the latest.
	clock.Advance(time.Minute)
	next, err := r.Begin(bg, a, "rcv_2", gitops.RestoreSource{BackupID: "bk_10"}, clock.Now())
	if err != nil || next.ID != "rcv_2" || !next.VolumeRestored || next.SnapshotID != "" {
		t.Fatalf("next: %+v, %v", next, err)
	}
	if cur, ok, _ := r.Current(bg, a); !ok || cur.ID != "rcv_2" || cur.Status != gitops.RecoveryActive {
		t.Fatalf("the latest recovery is current: %+v", cur)
	}
	must(t, r.Complete(bg, a, "rcv_2", clock.Now()))
	if r.MarkVolumeRestored(bg, a, "rcv_nope", clock.Now()) == nil {
		t.Fatal("an unknown recovery took a step")
	}
}

func pad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// PruneIntentReserves: a declared intent reserves its ref and its tip commit in
// its repository until it is released or expires; another repository, another
// tenant and unrelated refs and commits are not affected; a repeat extends and a
// new tip replaces it (GR-13).
func PruneIntentReserves(t *testing.T, e Env) {
	sa, sb := ContractScopeA(), ContractScopeB()
	e.Seed(t, sa, ContractRepo1, ContractRepo2)
	e.Seed(t, sb, ContractRepo1)
	clock := e.Clock()
	i, i2 := e.Intents(), e.Reopen(t).Intents()
	const ownerA, ownerB = "pruner_a", "pruner_b"
	t1, t2, other := "a1"+pad(38), "b2"+pad(38), "c3"+pad(38)
	ref := "refs/heads/kodama/work/wi_1"
	conflicts := func(in gitops.PruneIntents, scope domain.Scope, repo, ref, commit string) bool {
		t.Helper()
		got, err := in.Conflicts(bg, scope, repo, ref, commit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if conflicts(i, sa, ContractRepo1, ref, t1) {
		t.Fatal("nothing is reserved yet")
	}
	must(t, i.Declare(bg, sa, ContractRepo1, ref, t1, ownerA, 5*time.Minute))
	for _, in := range []gitops.PruneIntents{i, i2} { // visible to every instance
		if !conflicts(in, sa, ContractRepo1, ref, other) || !conflicts(in, sa, ContractRepo1, "refs/heads/kodama/work/wi_2", t1) {
			t.Fatal("the reservation covers its ref and its tip commit")
		}
		if conflicts(in, sa, ContractRepo1, "refs/heads/kodama/work/wi_2", other) || conflicts(in, sa, ContractRepo2, ref, t1) || conflicts(in, sb, ContractRepo1, ref, t1) {
			t.Fatal("another ref and commit, repository or tenant is not reserved")
		}
	}

	// Release drops it (idempotent); a declaration again reserves again.
	must(t, i2.Release(bg, sa, ContractRepo1, ref, ownerA))
	must(t, i2.Release(bg, sa, ContractRepo1, ref, ownerA))
	if conflicts(i, sa, ContractRepo1, ref, t1) {
		t.Fatal("a released intent reserves nothing")
	}
	must(t, i.Declare(bg, sa, ContractRepo1, ref, t1, ownerA, 5*time.Minute))
	must(t, i.Declare(bg, sa, ContractRepo1, ref, t1, ownerA, 5*time.Minute)) // idempotent
	if !conflicts(i2, sa, ContractRepo1, ref, other) {
		t.Fatal("the repeated declaration reserves")
	}

	// It expires on its own (a crash or an unknown outcome never reserves for good).
	clock.Advance(6 * time.Minute)
	if conflicts(i, sa, ContractRepo1, ref, t1) || conflicts(i2, sa, ContractRepo1, "refs/heads/kodama/work/wi_2", t1) {
		t.Fatal("an expired intent reserves nothing")
	}
	// A new declaration replaces an expired one, and a new tip replaces the tip.
	must(t, i.Declare(bg, sa, ContractRepo1, ref, t1, ownerA, 5*time.Minute))
	must(t, i2.Declare(bg, sa, ContractRepo1, ref, t2, ownerA, 5*time.Minute))
	if !conflicts(i, sa, ContractRepo1, "refs/heads/kodama/work/wi_2", t2) || conflicts(i, sa, ContractRepo1, "refs/heads/kodama/work/wi_2", t1) {
		t.Fatal("the intent follows the declared tip")
	}
	// Two refs are reserved independently.
	must(t, i.Declare(bg, sa, ContractRepo1, "refs/heads/kodama/work/wi_3", other, ownerA, time.Minute))
	must(t, i.Release(bg, sa, ContractRepo1, ref, ownerA))
	if conflicts(i, sa, ContractRepo1, ref, t2) || !conflicts(i, sa, ContractRepo1, "refs/heads/kodama/work/wi_9", other) {
		t.Fatal("releasing one ref leaves the other reserved")
	}

	// Another owner (another control-plane instance) cannot take a live
	// reservation, cannot release it, and does not extend it; the owner's repeat
	// does; an expired one is anyone's.
	busy := "refs/heads/kodama/work/wi_busy"
	must(t, i.Declare(bg, sa, ContractRepo1, busy, t1, ownerA, 5*time.Minute))
	wantErr(t, i2.Declare(bg, sa, ContractRepo1, busy, t2, ownerB, 5*time.Minute), gitops.ErrPruneBusy)
	wantErr(t, i2.Declare(bg, sa, ContractRepo1, busy, t1, ownerB, 5*time.Minute), gitops.ErrPruneBusy)
	must(t, i2.Release(bg, sa, ContractRepo1, busy, ownerB)) // not its reservation: untouched
	if !conflicts(i, sa, ContractRepo1, busy, other) {
		t.Fatal("another owner released a live reservation")
	}
	must(t, i.Declare(bg, sa, ContractRepo1, busy, t1, ownerA, 5*time.Minute)) // the owner's repeat
	clock.Advance(4 * time.Minute)
	must(t, i.Declare(bg, sa, ContractRepo1, busy, t1, ownerA, 5*time.Minute)) // extended
	clock.Advance(4 * time.Minute)
	wantErr(t, i2.Declare(bg, sa, ContractRepo1, busy, t2, ownerB, 5*time.Minute), gitops.ErrPruneBusy)
	clock.Advance(2 * time.Minute) // expired
	must(t, i2.Declare(bg, sa, ContractRepo1, busy, t2, ownerB, 5*time.Minute))
	must(t, i.Release(bg, sa, ContractRepo1, busy, ownerA)) // the previous owner no longer holds it
	if !conflicts(i2, sa, ContractRepo1, busy, other) {
		t.Fatal("a previous owner released the new owner's reservation")
	}
	must(t, i2.Release(bg, sa, ContractRepo1, busy, ownerB))
	must(t, i.Declare(bg, sa, ContractRepo1, busy, t1, ownerA, time.Minute)) // released: anyone may declare
	// A declaration in the wrong project is an error, never a silent success.
	wrong := domain.Scope{TenantID: sa.TenantID, ProjectID: "p138c_other"}
	e.Seed(t, wrong)
	if err := i.Declare(bg, wrong, ContractRepo1, busy, t1, ownerA, time.Minute); err == nil {
		t.Fatal("a declaration under another project reported success")
	}
	if err := i.Declare(bg, wrong, ContractRepo1, "refs/heads/kodama/work/wi_other", t1, ownerA, time.Minute); err == nil {
		t.Fatal("a declaration of a repository under another project reported success")
	}
}
