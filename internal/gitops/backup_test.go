package gitops_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

type backupRig struct {
	*rig
	store *gitopstest.Store
	snaps *gitopstest.Snapshots
	log   []string
	b     *gitops.Backups
	lease *gitopstest.Leases
}

func newBackupRig(t *testing.T, keep int) *backupRig {
	r := &backupRig{rig: newRig(t), store: &gitopstest.Store{}}
	r.lease = &gitopstest.Leases{Now: r.clock.Now}
	r.snaps = &gitopstest.Snapshots{Log: &r.log}
	n := 0
	b, err := gitops.NewBackups(gitops.BackupConfig{Worker: r.worker, Objects: r.objects, Catalog: r.catalog, Store: r.store, Snapshots: r.snaps, Leases: r.lease, Recoveries: r.recoveries, Fence: r.fence,
		Options: gitops.BackupOptions{Keep: keep, Clock: r.clock.Now, NewID: func(now time.Time) string {
			n++
			return "bk_" + now.Format("20060102T150405Z") + "_" + string(rune('a'+n))
		}}})
	if err != nil {
		t.Fatal(err)
	}
	r.b = b
	return r
}

// seed creates a repository with history on several refs (a merge commit
// included) and returns its refs.
func (r *rig) seed(tenant domain.TenantID, id string) map[string]string {
	now := r.clock.Now().AddDate(0, 0, -5)
	r.catalog.Add(repo(tenant, id))
	base := r.vol.Commit(tenant, id, "", "base", now)
	m1 := r.vol.Commit(tenant, id, base, "main 1", now.Add(time.Hour))
	f1 := r.vol.Commit(tenant, id, base, "feature 1", now.Add(2*time.Hour))
	r.vol.SetRef(tenant, id, "refs/heads/main", m1)
	r.vol.SetRef(tenant, id, "refs/heads/kodama/work/wi_1", f1)
	r.vol.SetRef(tenant, id, "refs/tags/v1", base)
	r.vol.SetRef(tenant, id, "refs/kodama/runs/run_1/1", f1)
	return r.vol.Refs(tenant, id)
}

// Test_unit_gitops_backup_bundles: one real bundle per non-empty repository in
// object storage, taken after the volume snapshot and described by a manifest
// written last with digests, sizes, journal heads and the exact refs; the bundle
// clones to identical commits, an empty repository has no bundle, a failing
// repository leaves no manifest and no leftover, and old backups expire only
// after a newer complete one (GR-15, GR-A10).
func Test_unit_gitops_backup_bundles(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 2)
	refsA := r.seed(tenantA, "r_bk1")
	r.seed(tenantA, "r_bk2")
	r.vol.Init(tenantA, "r_empty")
	r.catalog.Add(repo(tenantA, "r_empty"))
	r.seed(tenantB, "r_other")
	r.worker.SetHead(tenantA, "r_bk1", 17)

	m, err := r.b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.snaps.Taken) != 1 || m.Snapshot.ID != r.snaps.Taken[0] || m.Tenant != string(tenantA) || len(m.Repositories) != 3 {
		t.Fatalf("manifest %+v, snapshots %v", m, r.snaps.Taken)
	}
	if r.log[0] != "snapshot" {
		t.Fatalf("the snapshot comes first: %v", r.log)
	}
	byID := map[string]gitops.ManifestRepository{}
	for _, e := range m.Repositories {
		byID[e.ID] = e
	}
	if e := byID["r_empty"]; !e.Empty || e.Key != "" || len(r.store.Bytes(e.Key)) != 0 {
		t.Fatalf("empty repository: %+v", e)
	}
	e := byID["r_bk1"]
	data := r.store.Bytes(e.Key)
	sum := sha256.Sum256(data)
	if len(data) == 0 || int64(len(data)) != e.Size || hex.EncodeToString(sum[:]) != e.SHA256 || e.JournalHead != 17 || !strings.HasPrefix(e.Key, gitops.BackupPrefix(tenantA)) {
		t.Fatalf("entry %+v (%d bytes)", e, len(data))
	}
	got := map[string]string{}
	for _, tip := range e.Refs {
		got[tip.Ref] = tip.Commit
	}
	for ref, c := range refsA {
		if got[ref] != c {
			t.Errorf("manifest ref %s = %q, want %q", ref, got[ref], c)
		}
	}
	// The stored bytes are a real bundle that verifies and clones to identical commits.
	dir := t.TempDir()
	file := filepath.Join(dir, "r.bundle")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	gitopstest.Git(t, dir, nil, "init", "--quiet")
	gitopstest.Git(t, dir, nil, "bundle", "verify", file)
	clone := filepath.Join(dir, "clone")
	gitopstest.Git(t, dir, nil, "clone", "--quiet", "--bare", "--mirror", file, clone)
	for ref, c := range refsA {
		if out := gitopstest.Git(t, clone, nil, "rev-parse", ref); out != c {
			t.Errorf("clone %s = %s, want %s", ref, out, c)
		}
	}
	if _, err := os.Stat(filepath.Join(r.vol.Root, string(tenantB))); err != nil {
		t.Fatal(err)
	}
	// Tenant B's repositories are not in tenant A's backup.
	for _, k := range mustList(t, r.store, "") {
		if strings.Contains(k, "r_other") && strings.HasPrefix(k, gitops.BackupPrefix(tenantA)) {
			t.Fatalf("another tenant's repository in this tenant's backup: %s", k)
		}
	}

	// The manifest reads back and is validated against the tenant it was read for.
	back, err := r.b.Manifest(ctx, tenantA, "")
	if err != nil || back.ID != m.ID {
		t.Fatalf("manifest read: %+v %v", back, err)
	}
	if err := r.b.VerifyBundle(ctx, e); err != nil {
		t.Fatal(err)
	}
	r.store.Corrupt(e.Key)
	if err := r.b.VerifyBundle(ctx, e); !errors.Is(err, gitops.ErrBackupCorrupt) {
		t.Fatalf("a corrupted bundle verifies: %v", err)
	}
	if _, err := r.b.Manifest(ctx, tenantB, m.ID); !errors.Is(err, gitops.ErrBackupNotFound) {
		t.Fatalf("another tenant reads the manifest: %v", err)
	}

	// A failing repository leaves no manifest and no leftover object.
	r.clock.Advance(time.Hour)
	before := mustList(t, r.store, gitops.BackupPrefix(tenantA))
	r.store.FailPut = "r_bk2"
	if _, err := r.b.Backup(ctx, tenantA); err == nil {
		t.Fatal("a failing repository produced a backup")
	}
	r.store.FailPut = ""
	if after := mustList(t, r.store, gitops.BackupPrefix(tenantA)); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a failed backup left objects:\n%v\n%v", before, after)
	}
	// Retention of backups: keep two; the oldest goes only after a newer complete one exists.
	r.clock.Advance(time.Hour)
	second, err := r.b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := r.b.Backups(ctx, tenantA); len(ids) != 2 {
		t.Fatalf("backups %v", ids)
	}
	r.clock.Advance(time.Hour)
	third, err := r.b.Backup(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := r.b.Backups(ctx, tenantA)
	if len(ids) != 2 || ids[0] != second.ID || ids[1] != third.ID {
		t.Fatalf("backups after expiry %v, want [%s %s]", ids, second.ID, third.ID)
	}
	for _, k := range mustList(t, r.store, gitops.BackupPrefix(tenantA)) {
		if strings.Contains(k, m.ID) {
			t.Fatalf("the expired backup left %s", k)
		}
	}
}

func mustList(t *testing.T, s *gitopstest.Store, prefix string) []string {
	t.Helper()
	keys, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// Test_unit_gitops_backup_requires_snapshot_and_validates: no snapshot, no
// backup; a manifest of another tenant or with a wrong key is refused.
func Test_unit_gitops_backup_requires_snapshot_and_validates(t *testing.T) {
	ctx := context.Background()
	r := newBackupRig(t, 3)
	r.seed(tenantA, "r_v1")
	r.snaps.Fail = errors.New("no snapshot class")
	if _, err := r.b.Backup(ctx, tenantA); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("snapshot failure: %v", err)
	}
	if keys := mustList(t, r.store, ""); len(keys) != 0 {
		t.Fatalf("objects written without a snapshot: %v", keys)
	}
	if _, err := gitops.NewBackups(gitops.BackupConfig{}); !errors.Is(err, gitops.ErrNotConfigured) {
		t.Fatal(err)
	}
	m := gitops.Manifest{Version: 1, Tenant: string(tenantB), ID: "bk_x", Snapshot: gitops.ManifestSnapshot{ID: "s"}}
	if err := m.Validate(tenantA); !errors.Is(err, gitops.ErrBackupCorrupt) {
		t.Fatalf("a manifest of another tenant: %v", err)
	}
	m.Tenant = string(tenantA)
	m.Repositories = []gitops.ManifestRepository{{ID: "r", Key: "git-backups/elsewhere/r.bundle", Size: 1, SHA256: strings.Repeat("a", 64)}}
	if err := m.Validate(tenantA); !errors.Is(err, gitops.ErrBackupCorrupt) {
		t.Fatalf("a bundle key outside the backup: %v", err)
	}
}
