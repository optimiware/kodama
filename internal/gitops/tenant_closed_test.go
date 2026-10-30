package gitops_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
)

// Test_unit_gitops_backup_refused_while_tenant_closed: a backup started while the
// tenant has an active recovery or closed Git access is refused (retryable),
// records no run and writes nothing; one overtaken by a recovery before it
// publishes is not published; unreadable state fails closed; the collaborators
// are required (GR-15).
func Test_unit_gitops_backup_refused_while_tenant_closed(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_q1")
	b := r.instance("bk_", 3)
	assertNothing := func(what string) {
		t.Helper()
		if keys := keysOf(t, r); len(keys) != 0 {
			t.Fatalf("%s wrote objects: %v", what, keys)
		}
		// No run row is left behind: the lease is free.
		if err := r.lease.Acquire(ctx, tenantA, "bk_probe", time.Minute); err != nil {
			t.Fatalf("%s left an active run: %v", what, err)
		}
		_ = r.lease.Fail(ctx, tenantA, "bk_probe")
		if len(r.snaps.Taken) != 0 {
			t.Fatalf("%s took a volume snapshot", what)
		}
	}

	// An active recovery.
	if _, err := r.recoveries.Begin(ctx, tenantA, "rcv_q", gitops.RestoreSource{SnapshotID: "snap-1", BackupID: "bk_9"}, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrTenantClosed) {
		t.Fatalf("a backup during a recovery: %v", err)
	}
	assertNothing("a backup during a recovery")
	if err := r.recoveries.MarkVolumeRestored(ctx, tenantA, "rcv_q", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.recoveries.Complete(ctx, tenantA, "rcv_q", r.clock.Now()); err != nil {
		t.Fatal(err)
	}

	// A closed fence, whichever the reason.
	for _, reason := range []gitops.FenceReason{gitops.FenceRestore, gitops.FenceOffboarding} {
		if err := r.fence.Close(ctx, tenantA, reason); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrTenantClosed) {
			t.Fatalf("a backup with access closed for %s: %v", reason, err)
		}
		assertNothing("a backup with access closed for " + string(reason))
		if err := r.fence.Open(ctx, tenantA, reason); err != nil {
			t.Fatal(err)
		}
	}
	// Another tenant is not affected by this tenant's closure.
	r.seed(tenantB, "r_qb")
	if err := r.fence.Close(ctx, tenantA, gitops.FenceRestore); err != nil {
		t.Fatal(err)
	}
	if _, err := r.instance("bk_b_", 3).Backup(ctx, tenantB); err != nil {
		t.Fatalf("another tenant: %v", err)
	}
	_ = r.fence.Open(ctx, tenantA, gitops.FenceRestore)
	r.snaps.Taken = nil // the tenant B backup took a snapshot

	// State that cannot be read fails closed.
	r.recoveries.FailRead = errors.New("store down")
	if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("an unreadable recovery: %v", err)
	}
	r.recoveries.FailRead = nil
	r.fence.FailRead = errors.New("fence store down")
	if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("an unreadable fence: %v", err)
	}
	r.fence.FailRead = nil

	// A recovery that began and ended while the bundles were taken: the backup
	// bundled a volume that was being restored and is not published.
	recoverDuring := func(complete bool) {
		id := "rcv_during_" + map[bool]string{false: "open", true: "done"}[complete]
		var once sync.Once
		r.store.OnPut = func(key string) {
			once.Do(func() {
				if _, err := r.recoveries.Begin(ctx, tenantA, id, gitops.RestoreSource{BackupID: "bk_9"}, r.clock.Now()); err != nil {
					t.Error(err)
				}
				if complete {
					if err := r.recoveries.Complete(ctx, tenantA, id, r.clock.Now()); err != nil {
						t.Error(err)
					}
				}
			})
		}
	}
	for _, complete := range []bool{false, true} {
		before := keysOf(t, r)
		recoverDuring(complete)
		_, err := b.Backup(ctx, tenantA)
		r.store.OnPut = nil
		if !errors.Is(err, gitops.ErrTenantClosed) {
			t.Fatalf("a backup overtaken by a recovery (completed=%v): %v", complete, err)
		}
		if after := keysOf(t, r); strings.Join(after, ",") != strings.Join(before, ",") {
			t.Fatalf("an overtaken backup left objects:\n%v\n%v", before, after)
		}
		if ids, _ := b.Backups(ctx, tenantA); len(ids) != 0 {
			t.Fatalf("an overtaken backup was published: %v", ids)
		}
		if !complete {
			if err := r.recoveries.Complete(ctx, tenantA, "rcv_during_open", r.clock.Now()); err != nil {
				t.Fatal(err)
			}
		}
		r.clock.Advance(time.Minute)
	}
	// Once the recovery is over backups work again.
	if _, err := b.Backup(ctx, tenantA); err != nil {
		t.Fatalf("a backup after the recoveries: %v", err)
	}

	for _, c := range []gitops.BackupConfig{
		{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: r.lease, Fence: r.fence},
		{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: r.lease, Recoveries: r.recoveries},
	} {
		if _, err := gitops.NewBackups(c); !errors.Is(err, gitops.ErrNotConfigured) {
			t.Fatalf("a backup without its tenant-state collaborators: %v", err)
		}
	}
}

// Test_unit_gitops_prune_and_maintenance_skip_closed_tenant: maintenance and
// pruning skip a tenant that has an active recovery or closed Git access (the
// report says so, nothing is done, nothing is recorded as done), fail closed when
// the state cannot be read, and resume when the tenant is open again; another
// tenant is maintained meanwhile (GR-13, GR-15).
func Test_unit_gitops_prune_and_maintenance_skip_closed_tenant(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	ra, rb := repo(tenantA, "r_sk_a"), repo(tenantB, "r_sk_b")
	old := r.clock.Now().AddDate(0, 0, -90)
	for _, x := range []gitops.Repository{ra, rb} {
		r.vol.Init(x.Scope.TenantID, x.ID)
		r.catalog.Add(x)
		base := r.vol.Commit(x.Scope.TenantID, x.ID, "", "base", old)
		r.vol.SetRef(x.Scope.TenantID, x.ID, "refs/heads/main", base)
		r.vol.SetRef(x.Scope.TenantID, x.ID, "refs/heads/kodama/work/wi_old", base)
	}
	s := r.scheduler(gitops.Options{BranchRetention: time.Hour, MaintenanceInterval: time.Hour})
	branch := "refs/heads/kodama/work/wi_old"

	// Between a repository's replay and the reconciliation a prune could delete a
	// branch the recovery point expects: the active recovery stops it.
	if _, err := r.recoveries.Begin(ctx, tenantA, "rcv_sk", gitops.RestoreSource{BackupID: "bk_9"}, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	reports, err := s.RunOnce(ctx)
	if err != nil || len(reports) != 2 {
		t.Fatalf("pass: %d %v", len(reports), err)
	}
	for _, rep := range reports {
		switch rep.Repository.ID {
		case "r_sk_a":
			if !errors.Is(rep.Closed, gitops.ErrTenantClosed) || rep.Maintained || len(rep.Prune.Pruned) != 0 || rep.Err != nil {
				t.Fatalf("the recovering tenant: %+v", rep)
			}
		case "r_sk_b":
			if rep.Closed != nil || !rep.Maintained || len(rep.Prune.Pruned) != 1 {
				t.Fatalf("another tenant: %+v", rep)
			}
		}
	}
	if r.vol.Ref(tenantA, ra.ID, branch) == "" || r.worker.Count("maintain r_sk_a") != 0 || r.worker.Count("deleteBranch r_sk_a") != 0 {
		t.Fatal("the recovering tenant was pruned or maintained")
	}
	if _, err := s.PruneRepository(ctx, ra); !errors.Is(err, gitops.ErrTenantClosed) {
		t.Fatalf("prune during a recovery: %v", err)
	}

	// A closed fence stops them too, whatever the reason.
	if err := r.recoveries.MarkVolumeRestored(ctx, tenantA, "rcv_sk", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.recoveries.Complete(ctx, tenantA, "rcv_sk", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []gitops.FenceReason{gitops.FenceRestore, gitops.FenceOffboarding} {
		if err := r.fence.Close(ctx, tenantA, reason); err != nil {
			t.Fatal(err)
		}
		rep := s.MaintainRepository(ctx, ra)
		if !errors.Is(rep.Closed, gitops.ErrTenantClosed) || rep.Maintained || r.vol.Ref(tenantA, ra.ID, branch) == "" {
			t.Fatalf("access closed for %s: %+v", reason, rep)
		}
		_ = r.fence.Open(ctx, tenantA, reason)
	}

	// Unreadable state fails closed and is an error, not a skip.
	r.fence.FailRead = errors.New("fence store down")
	if rep := s.MaintainRepository(ctx, ra); !errors.Is(rep.Err, gitops.ErrUnavailable) || rep.Maintained || r.vol.Ref(tenantA, ra.ID, branch) == "" {
		t.Fatalf("an unreadable fence: %+v", rep)
	}
	r.fence.FailRead = nil

	// Open again: the pass proceeds (nothing was marked done while skipped).
	rep := s.MaintainRepository(ctx, ra)
	if rep.Closed != nil || rep.Err != nil || !rep.Maintained || len(rep.Prune.Pruned) != 1 || r.vol.Ref(tenantA, ra.ID, branch) != "" {
		t.Fatalf("after the tenant opened: %+v", rep)
	}
	if _, err := gitops.NewScheduler(gitops.SchedulerConfig{Worker: r.worker, Catalog: r.catalog}); !errors.Is(err, gitops.ErrNotConfigured) {
		t.Fatalf("a scheduler without the tenant state: %v", err)
	}
}

// completeFault wraps the lease store: Complete reports an error after (or
// without) committing.
type completeFault struct {
	gitops.BackupLeases
	commit    bool  // the run is recorded complete although Complete reports the error
	stateFail error // State fails afterwards
}

func (c *completeFault) Complete(ctx context.Context, tenant domain.TenantID, id string) error {
	if c.commit {
		if err := c.BackupLeases.Complete(ctx, tenant, id); err != nil {
			return err
		}
	}
	return errors.New("connection reset")
}

func (c *completeFault) State(ctx context.Context, tenant domain.TenantID, id string) (gitops.BackupState, bool, error) {
	if c.stateFail != nil {
		return "", false, c.stateFail
	}
	return c.BackupLeases.State(ctx, tenant, id)
}

// Test_unit_gitops_backup_unknown_complete_outcome: when Complete returns an error
// the run's state is read before anything is deleted. A run recorded complete
// keeps its manifest and bundles and is returned as published; a run recorded
// active is cleaned up and failed; when the state cannot be read nothing is
// deleted (GR-15).
func Test_unit_gitops_backup_unknown_complete_outcome(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_u1")
	mk := func(f *completeFault) *gitops.Backups {
		f.BackupLeases = r.lease
		b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: f,
			Recoveries: r.recoveries, Fence: r.fence, Options: gitops.BackupOptions{Clock: r.clock.Now}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Committed, but the answer was lost: published, objects intact.
	m, err := mk(&completeFault{commit: true}).Backup(ctx, tenantA)
	if err != nil {
		t.Fatalf("a committed run whose answer was lost: %v", err)
	}
	ids, _ := mk(&completeFault{}).Backups(ctx, tenantA)
	if len(ids) != 1 || ids[0] != m.ID {
		t.Fatalf("backups %v", ids)
	}
	back := mk(&completeFault{})
	if _, err := back.Manifest(ctx, tenantA, m.ID); err != nil {
		t.Fatalf("the manifest of a run recorded complete: %v", err)
	}
	for _, e := range m.Repositories {
		if err := back.VerifyBundle(ctx, e); err != nil {
			t.Fatalf("the bundle of a run recorded complete: %v", err)
		}
	}

	// Not committed and recorded active: cleaned up and failed.
	r.clock.Advance(time.Hour)
	before := keysOf(t, r)
	if _, err := mk(&completeFault{}).Backup(ctx, tenantA); err == nil {
		t.Fatal("a run that did not commit was published")
	}
	if after := keysOf(t, r); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a failed run left objects:\n%v\n%v", before, after)
	}

	// The state cannot be read: nothing may be deleted (the run may be complete).
	r.clock.Advance(time.Hour)
	before = keysOf(t, r)
	_, err = mk(&completeFault{commit: true, stateFail: errors.New("store down")}).Backup(ctx, tenantA)
	if !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("an unknown outcome: %v", err)
	}
	if after := keysOf(t, r); len(after) <= len(before) {
		t.Fatalf("the objects of a possibly complete run were deleted:\n%v\n%v", before, after)
	}
	// That run is recorded complete (it committed): its manifest verifies.
	ids, _ = mk(&completeFault{}).Backups(ctx, tenantA)
	if len(ids) != 2 {
		t.Fatalf("backups %v", ids)
	}
	last := ids[len(ids)-1]
	back = mk(&completeFault{})
	man, err := back.Manifest(ctx, tenantA, last)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man.Repositories {
		if err := back.VerifyBundle(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

// Test_unit_gitops_backup_renews_lease_during_upload: a bundle that streams for
// longer than the lease keeps it alive through the background renewal, so another
// instance is still refused and the run is published; the renewal is bound to the
// run (GR-15).
func Test_unit_gitops_backup_renews_lease_during_upload(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_rn1")
	counting := &renewFault{BackupLeases: r.lease}
	mk := func(prefix string, renew time.Duration) *gitops.Backups {
		b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: counting,
			Recoveries: r.recoveries, Fence: r.fence, Options: gitops.BackupOptions{LeaseTTL: 10 * time.Minute, RenewEvery: renew, Clock: r.clock.Now,
				NewID: func(now time.Time) string { return prefix + now.Format("20060102T150405Z") }}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first, second := mk("bk_1_", 5*time.Millisecond), mk("bk_2_", time.Hour/8)
	stalled, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.store.OnPut = func(key string) {
		if strings.Contains(key, "bk_1_") && strings.HasSuffix(key, ".bundle") {
			once.Do(func() {
				close(stalled)
				<-resume
			})
		}
	}
	type result struct {
		m   gitops.Manifest
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := first.Backup(ctx, tenantA)
		done <- result{m, err}
	}()
	<-stalled
	// The upload runs far longer than the lease: the renewer keeps extending it.
	r.clock.Advance(8 * time.Minute)
	waitRenewals(t, counting, counting.renews.Load()+3) // the renewer extended it after the clock moved
	r.clock.Advance(8 * time.Minute)                    // 16 minutes in: the first lease (10) would have lapsed
	if _, err := second.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrBackupInProgress) {
		t.Fatalf("the long upload lost its lease: %v", err)
	}
	close(resume)
	res := <-done
	if res.err != nil {
		t.Fatalf("the long backup: %v", res.err)
	}
	if ids, _ := second.Backups(ctx, tenantA); len(ids) != 1 || ids[0] != res.m.ID {
		t.Fatalf("backups %v", ids)
	}
	// The renewer ended with the run: the lease is not extended any more.
	r.clock.Advance(time.Hour)
	if _, err := mk("bk_3_", 5*time.Millisecond).Backup(ctx, tenantA); err != nil {
		t.Fatalf("the next backup: %v", err)
	}
}

// renewFault wraps the lease store: it counts renewals and, in a mode, fails them.
type renewFault struct {
	gitops.BackupLeases
	renews atomic.Int64
	mode   atomic.Int32 // 0 delegates, 1 fails transiently, 2 reports the lease lost
}

var errTransient = errors.New("lease store: connection reset")

func (f *renewFault) Renew(ctx context.Context, tenant domain.TenantID, id string, ttl time.Duration) error {
	f.renews.Add(1)
	switch f.mode.Load() {
	case 1:
		return errTransient
	case 2:
		return gitops.ErrLeaseLost
	}
	return f.BackupLeases.Renew(ctx, tenant, id, ttl)
}

// waitRenewals polls, with a deadline, until the renewer made at least n renewals.
func waitRenewals(t *testing.T, f *renewFault, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.renews.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the renewer made %d renewals, want %d", f.renews.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Test_unit_gitops_backup_renewal_failures: a transient renewal error does not
// cancel the backup while the lease has periods left; the renewer gives up (and
// the run fails, publishing nothing) once the lease is within a period of expiry;
// a lost lease cancels at once (GR-15).
func Test_unit_gitops_backup_renewal_failures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		ttl     time.Duration
		mode    int32
		stall   time.Duration // how long the upload is stalled with renewals failing
		wantErr error
	}{
		{"transient errors within the lease are tolerated", 2 * time.Second, 1, 80 * time.Millisecond, nil},
		{"transient errors until the lease is nearly gone cancel the run", 100 * time.Millisecond, 1, 400 * time.Millisecond, errTransient},
		{"a lost lease cancels at once", 10 * time.Minute, 2, 100 * time.Millisecond, gitops.ErrLeaseLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBackupRig(t, 3)
			r.seed(tenantA, "r_rf1")
			f := &renewFault{BackupLeases: r.lease}
			b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: f,
				Recoveries: r.recoveries, Fence: r.fence, Options: gitops.BackupOptions{LeaseTTL: tc.ttl, RenewEvery: 10 * time.Millisecond, Clock: r.clock.Now}})
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			r.store.OnPut = func(key string) {
				if !strings.HasSuffix(key, ".bundle") {
					return
				}
				once.Do(func() {
					f.mode.Store(tc.mode)
					time.Sleep(tc.stall) // the renewals fail meanwhile
					f.mode.Store(0)      // the store is back before the run next looks at it
				})
			}
			m, err := b.Backup(ctx, tenantA)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("the run was cancelled by transient errors that the lease outlasted: %v", err)
				}
				if ids, _ := b.Backups(ctx, tenantA); len(ids) != 1 || ids[0] != m.ID {
					t.Fatalf("backups %v", ids)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if ids, _ := b.Backups(ctx, tenantA); len(ids) != 0 {
				t.Fatalf("a cancelled run was published: %v", ids)
			}
			if keys := keysOf(t, r); len(keys) != 0 {
				t.Fatalf("a cancelled run left objects: %v", keys)
			}
		})
	}
}
