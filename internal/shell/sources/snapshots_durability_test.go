package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// syncLog records every directory synchronization and whether the snapshot entry
// already existed when it happened.
type syncLog struct {
	mu     sync.Mutex
	key    string
	dir    string
	calls  []string
	linked []bool // for each call: the snapshot entry existed at that moment
	fail   func(dir string) error
}

func (s *syncLog) sync(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := os.Lstat(filepath.Join(s.dir, s.key))
	s.calls = append(s.calls, dir)
	s.linked = append(s.linked, err == nil)
	if s.fail != nil {
		return s.fail(dir)
	}
	return nil
}

func snapshotInputs() runtime.PreparationInputs {
	return runtime.PreparationInputs{Scope: domain.Scope{TenantID: "t", ProjectID: "p"}, RunID: "run1", Digest: "d",
		Sources: []runtime.SourceInput{{Name: "s", Locator: "https://x/git/a/b.git", Revision: strings.Repeat("a", 40), Path: "."}}}
}

// Test_unit_shell_source_snapshot_durability proves a snapshot is acknowledged
// only after the directory entries it depends on were synchronized, and that a
// failure to synchronize is reported rather than acknowledged.
func Test_unit_shell_source_snapshot_durability(t *testing.T) {
	key := strings.Repeat("ab", 32)
	ctx := context.Background()

	t.Run("the entry and every created directory are synced before the acknowledgement", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "state", "snapshots")
		log := &syncLog{key: key, dir: dir}
		f := FileSnapshots{Dir: dir, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); err != nil {
			t.Fatal(err)
		}
		synced := map[string]bool{}
		for _, c := range log.calls {
			synced[c] = true
		}
		// Each created directory, and the parent that holds its entry.
		for _, want := range []string{filepath.Join(base, "state"), dir, base} {
			if !synced[want] {
				t.Errorf("%s was not synced: %v", want, log.calls)
			}
		}
		// The last synchronization is of the snapshot directory, after the entry was linked.
		last := len(log.calls) - 1
		if log.calls[last] != dir || !log.linked[last] {
			t.Fatalf("the snapshot's directory was not synced after linking: %v %v", log.calls, log.linked)
		}
	})

	t.Run("a sync failure is returned, and a later call makes the record durable before using it", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "snapshots")
		log := &syncLog{key: key, dir: dir}
		boom := errors.New("fsync: input/output error")
		log.fail = func(d string) error {
			if d == dir && log.linkedNow() {
				return boom
			}
			return nil
		}
		f := FileSnapshots{Dir: dir, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); !errors.Is(err, boom) {
			t.Fatalf("an unsynced publication was acknowledged: %v", err)
		}
		// The entry is there but was never acknowledged. A retry that finds it must
		// sync the directory again, and while that fails it must not acknowledge it.
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); !errors.Is(err, boom) {
			t.Fatalf("an existing, unsynced snapshot was acknowledged: %v", err)
		}
		log.mu.Lock()
		log.fail = nil
		before := len(log.calls)
		log.mu.Unlock()
		got, err := f.PutIfAbsent(ctx, key, snapshotInputs())
		if err != nil || got.Digest != "d" {
			t.Fatalf("the retry: %+v %v", got, err)
		}
		if len(log.calls) == before || log.calls[len(log.calls)-1] != dir {
			t.Fatalf("the existing-record path did not sync the directory: %v", log.calls[before:])
		}
	})

	t.Run("a failure to sync a created directory is returned and nothing is linked", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "a", "snapshots")
		log := &syncLog{key: key, dir: dir}
		boom := errors.New("fsync failed")
		log.fail = func(d string) error {
			if d == filepath.Join(base, "a") {
				return boom
			}
			return nil
		}
		f := FileSnapshots{Dir: dir, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); !errors.Is(err, boom) {
			t.Fatalf("a directory that could not be synced was acknowledged: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, key)); err == nil {
			t.Fatal("a snapshot was linked into a directory that is not durable")
		}
	})

	t.Run("the real directory sync works", func(t *testing.T) {
		f := FileSnapshots{Dir: filepath.Join(t.TempDir(), "x", "snapshots")}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := f.Get(ctx, key); err != nil || !ok || got.Digest != "d" {
			t.Fatalf("%+v %v %v", got, ok, err)
		}
		if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("syncing a missing directory succeeded")
		}
	})
}

func (s *syncLog) linkedNow() bool {
	_, err := os.Lstat(filepath.Join(s.dir, s.key))
	return err == nil
}
