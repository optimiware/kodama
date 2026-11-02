package gitops_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/localgit"
)

// instance builds another control-plane instance's Backups over the same store,
// snapshots, catalog and lease table.
func (r *backupRig) instance(prefix string, keep int) *gitops.Backups {
	r.t.Helper()
	n := 0
	b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: r.lease,
		Recoveries: r.recoveries, Fence: r.fence, Options: gitops.BackupOptions{Keep: keep, LeaseTTL: 10 * time.Minute, Clock: r.clock.Now, NewID: func(now time.Time) string {
			n++
			return prefix + now.Format("20060102T150405Z") + "_" + string(rune('a'+n))
		}}})
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func keysOf(t *testing.T, r *backupRig) []string {
	t.Helper()
	return mustList(t, r.store, gitops.BackupPrefix(tenantA))
}

// Test_unit_gitops_backup_leases_serialize: two control-plane instances never
// back up one tenant at once. While the first holds a live lease the second is
// refused and writes nothing; once the first stalls past its lease the second
// abandons it, publishes its own backup and expiry removes the abandoned run's
// objects; the stalled instance, resuming, loses its lease and publishes nothing
// (GR-15).
func Test_unit_gitops_backup_leases_serialize(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_l1")
	r.seed(tenantA, "r_l2")
	first, second := r.instance("bk_1_", 3), r.instance("bk_2_", 3)

	// The first instance stalls inside its upload of the second repository.
	stalled, resume := make(chan struct{}), make(chan struct{})
	var once bool
	r.store.OnPut = func(key string) {
		if strings.Contains(key, "bk_1_") && strings.HasSuffix(key, "r_l2.bundle") && !once {
			once = true
			close(stalled)
			<-resume
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

	// A second instance is refused while the lease is live, and writes nothing.
	before := keysOf(t, r)
	if _, err := second.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrBackupInProgress) {
		t.Fatalf("a concurrent backup: %v", err)
	}
	if after := keysOf(t, r); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a refused backup wrote objects:\n%v\n%v", before, after)
	}
	// Another tenant is independent.
	r.seed(tenantB, "r_lb")
	if _, err := r.instance("bk_b_", 3).Backup(ctx, tenantB); err != nil {
		t.Fatalf("another tenant: %v", err)
	}

	// The first instance stalls past its lease: the second abandons it, publishes
	// and expires the abandoned run's objects (the run is recorded failed).
	r.clock.Advance(11 * time.Minute)
	m2, err := second.Backup(ctx, tenantA)
	if err != nil {
		t.Fatalf("the second backup after the lease lapsed: %v", err)
	}
	for _, k := range keysOf(t, r) {
		if strings.Contains(k, "bk_1_") {
			t.Errorf("the abandoned run left %s", k)
		}
	}
	// The stalled instance resumes: it lost the lease and publishes nothing.
	close(resume)
	res := <-done
	if !errors.Is(res.err, gitops.ErrLeaseLost) {
		t.Fatalf("the stalled backup: %+v, %v", res.m, res.err)
	}
	ids, err := second.Backups(ctx, tenantA)
	if err != nil || len(ids) != 1 || ids[0] != m2.ID {
		t.Fatalf("complete backups %v, want [%s]: %v", ids, m2.ID, err)
	}
	for _, k := range keysOf(t, r) {
		if !strings.Contains(k, m2.ID) {
			t.Errorf("an object outside the published backup: %s", k)
		}
	}
	if _, err := second.Manifest(ctx, tenantA, ""); err != nil {
		t.Fatalf("the published manifest: %v", err)
	}
	for _, e := range m2.Repositories {
		if err := second.VerifyBundle(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

// Test_unit_gitops_backup_expiry_only_by_recorded_state: expiry removes a backup
// beyond the retention only when its run is recorded complete, and the objects of
// a run recorded failed; never an upload that merely has an older ID than a newer
// manifest, never a run whose lease is live, and never the backup an active
// recovery reads (GR-15).
func Test_unit_gitops_backup_expiry_only_by_recorded_state(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 1)
	r.seed(tenantA, "r_x1")
	b := r.instance("bk_", 1)

	// An upload with an older ID and no run row (another writer's, or an adapter
	// that lost the row): never expired because a newer manifest exists.
	stray := gitops.BackupPrefix(tenantA) + "bk_0000_stray/r_x1.bundle"
	if err := r.store.Put(ctx, stray, strings.NewReader("partial")); err != nil {
		t.Fatal(err)
	}
	// An active run of another instance with a live lease: its objects are its own.
	if err := r.lease.Acquire(ctx, tenantA, "bk_0001_active", time.Hour); err != nil {
		t.Fatal(err)
	}
	live := gitops.BackupPrefix(tenantA) + "bk_0001_active/r_x1.bundle"
	if err := r.store.Put(ctx, live, strings.NewReader("uploading")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrBackupInProgress) {
		t.Fatalf("a backup beside a live run: %v", err)
	}
	// Its lease lapses without completing: the next backup abandons it.
	r.clock.Advance(2 * time.Hour)
	m1, err := b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if r.store.Bytes(live) != nil {
		t.Fatal("the abandoned run's object survived its expiry")
	}
	if r.store.Bytes(stray) == nil {
		t.Fatal("an upload without a run row was removed because a newer manifest exists")
	}

	// An active recovery reads m1: a backup is refused meanwhile (it would bundle a
	// half-restored volume), records no run and writes nothing; once the recovery
	// completed, retention keeps one complete backup.
	if _, err := r.recoveries.Begin(ctx, tenantA, "rcv_x", gitops.RestoreSource{BackupID: m1.ID}, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	before := keysOf(t, r)
	if _, err := b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrTenantClosed) {
		t.Fatalf("a backup during a recovery: %v", err)
	}
	if after := keysOf(t, r); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a refused backup wrote objects:\n%v\n%v", before, after)
	}
	if err := r.recoveries.MarkVolumeRestored(ctx, tenantA, "rcv_x", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.recoveries.Complete(ctx, tenantA, "rcv_x", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	m2, err := b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	m3, err := b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := b.Backups(ctx, tenantA)
	if len(ids) != 1 || ids[0] != m3.ID {
		t.Fatalf("backups %v, want [%s] (%s and %s expired)", ids, m3.ID, m1.ID, m2.ID)
	}
	for _, k := range keysOf(t, r) {
		if strings.Contains(k, m2.ID) || strings.Contains(k, m1.ID) {
			t.Errorf("an expired backup left %s", k)
		}
	}
}

// inconsistentObjects reports the first n bundles as not taken at one boundary.
type inconsistentObjects struct {
	localgit.Objects
	n, calls int
}

func (o *inconsistentObjects) CreateBundle(ctx context.Context, tenant domain.TenantID, repositoryID string, w io.Writer) (gitops.Bundle, error) {
	var buf bytes.Buffer
	b, err := o.Objects.CreateBundle(ctx, tenant, repositoryID, &buf)
	if err != nil {
		return b, err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return b, err
	}
	o.calls++
	b.Consistent = o.calls > o.n
	return b, nil
}

// Test_unit_gitops_backup_requires_consistent_bundles: a bundle the worker could
// not take at one boundary (a mutation ran meanwhile) is retaken; a repository
// that never settles fails the backup, which leaves no manifest and no object.
func Test_unit_gitops_backup_requires_consistent_bundles(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_c1")
	flaky := &inconsistentObjects{Objects: r.objects, n: 2}
	mk := func(o gitops.Objects) *gitops.Backups {
		b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: o, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: r.lease,
			Recoveries: r.recoveries, Fence: r.fence, Options: gitops.BackupOptions{Clock: r.clock.Now}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	m, err := mk(flaky).Backup(ctx, tenantA)
	if err != nil || flaky.calls != 3 || m.Repositories[0].Empty {
		t.Fatalf("retaken bundle: calls=%d err=%v", flaky.calls, err)
	}
	if err := mk(flaky).VerifyBundle(ctx, m.Repositories[0]); err != nil {
		t.Fatal(err)
	}

	r.clock.Advance(time.Hour)
	before := keysOf(t, r)
	never := &inconsistentObjects{Objects: r.objects, n: 100}
	if _, err := mk(never).Backup(ctx, tenantA); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("a repository that keeps changing: %v", err)
	}
	if after := keysOf(t, r); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a failed backup left objects:\n%v\n%v", before, after)
	}
}

// bundleKeys lists the objects of one backup.
func bundleKeys(t *testing.T, r *backupRig, id string) []string {
	t.Helper()
	var out []string
	for _, k := range keysOf(t, r) {
		if strings.Contains(k, "/"+id+"/") {
			out = append(out, k)
		}
	}
	return out
}

// Test_unit_gitops_backup_expiry_protects_recovery_source: expiry runs after the
// backup completed and without its lease, so a recovery can begin after the
// backup's own quiet check. The backup an active recovery selected is never
// expired, whether the recovery began before expiry listed the store or while it
// was deleting; a restore that closed Git access but has not selected its source
// yet defers every deletion; once the recovery completed the backup expires
// (GR-15).
func Test_unit_gitops_backup_expiry_protects_recovery_source(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 1)
	r.seed(tenantA, "r_ps1")
	b := r.instance("bk_", 1)
	next := func() gitops.Manifest {
		t.Helper()
		r.clock.Advance(time.Hour)
		m, err := b.Backup(ctx, tenantA)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	begin := func(id, backup string) {
		t.Helper()
		if _, err := r.recoveries.Begin(ctx, tenantA, id, gitops.RestoreSource{BackupID: backup}, r.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(id string) {
		t.Helper()
		if err := r.recoveries.Complete(ctx, tenantA, id, r.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	m1 := next()
	m2 := next() // m1 expired: keep 1

	// (1) A recovery selecting m2 — beyond Keep once m3 completes — begins between
	// m3's Complete and expiry's listing: m2 survives, m1... is already gone.
	var once sync.Once
	r.store.OnList = func(string) { once.Do(func() { begin("rcv_a", m2.ID) }) }
	m3 := next()
	r.store.OnList = nil
	if len(bundleKeys(t, r, m1.ID)) != 0 {
		t.Fatalf("an unprotected expired backup survived: %v", bundleKeys(t, r, m1.ID))
	}
	if _, err := b.Manifest(ctx, tenantA, m2.ID); err != nil {
		t.Fatalf("the backup an active recovery selected was expired: %v", err)
	}
	for _, e := range m2.Repositories {
		if err := b.VerifyBundle(ctx, e); err != nil {
			t.Fatalf("its bundle was expired: %v", err)
		}
	}
	finish("rcv_a")
	m4 := next() // the recovery is over: m2 and m3 expire
	if ids, _ := b.Backups(ctx, tenantA); len(ids) != 1 || ids[0] != m4.ID {
		t.Fatalf("backups %v, want [%s] (%s and %s expire once the recovery completed)", ids, m4.ID, m2.ID, m3.ID)
	}
	if len(bundleKeys(t, r, m2.ID))+len(bundleKeys(t, r, m3.ID)) != 0 {
		t.Fatal("the backups did not expire after the recovery")
	}

	// (2) A restore closed Git access after the backup's quiet check but has not
	// selected its source yet: expiry defers every deletion of complete backups.
	var closeOnce sync.Once
	r.store.OnList = func(string) {
		closeOnce.Do(func() {
			if err := r.fence.Close(ctx, tenantA, gitops.FenceRestore); err != nil {
				t.Error(err)
			}
		})
	}
	m5 := next()
	r.store.OnList = nil
	if err := r.fence.Open(ctx, tenantA, gitops.FenceRestore); err != nil {
		t.Fatal(err)
	}
	if ids, _ := b.Backups(ctx, tenantA); len(ids) != 2 || ids[0] != m4.ID || ids[1] != m5.ID {
		t.Fatalf("backups %v, want [%s %s]: expiry must wait for a restore that is selecting its source", ids, m4.ID, m5.ID)
	}

	// (3) A recovery begins while expiry is already deleting: the guard is read
	// again before each complete backup, so the second doomed backup survives.
	var deleteOnce sync.Once
	r.store.OnDelete = func(key string) {
		if strings.Contains(key, "/"+m4.ID+"/") {
			deleteOnce.Do(func() { begin("rcv_b", m5.ID) })
		}
	}
	m6 := next() // m4 and m5 are both beyond Keep
	r.store.OnDelete = nil
	if len(bundleKeys(t, r, m4.ID)) != 0 {
		t.Fatalf("the unprotected backup survived: %v", bundleKeys(t, r, m4.ID))
	}
	if _, err := b.Manifest(ctx, tenantA, m5.ID); err != nil {
		t.Fatalf("the backup selected while expiry deleted was expired: %v", err)
	}
	for _, e := range m5.Repositories {
		if err := b.VerifyBundle(ctx, e); err != nil {
			t.Fatalf("its bundle was expired: %v", err)
		}
	}
	finish("rcv_b")
	m7 := next()
	if ids, _ := b.Backups(ctx, tenantA); len(ids) != 1 || ids[0] != m7.ID {
		t.Fatalf("backups %v, want [%s] (%s and %s expire once the recovery completed)", ids, m7.ID, m5.ID, m6.ID)
	}
}
