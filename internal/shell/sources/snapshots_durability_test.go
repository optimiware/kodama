package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/store"
)

// syncLog is the injected directory synchronization. It records every call with
// whether the snapshot entry existed at that moment, and can fail or block calls.
type syncLog struct {
	mu     sync.Mutex
	key    string
	dir    string
	calls  []string
	linked []bool
	// fail, when set, decides the result of a call; it runs with the log unlocked.
	fail func(dir string, linked bool) error
}

func (s *syncLog) sync(dir string) error {
	s.mu.Lock()
	_, err := os.Lstat(filepath.Join(s.dir, s.key))
	linked := err == nil
	s.calls = append(s.calls, dir)
	s.linked = append(s.linked, linked)
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return fail(dir, linked)
	}
	return nil
}

func (s *syncLog) setFail(f func(dir string, linked bool) error) {
	s.mu.Lock()
	s.fail = f
	s.mu.Unlock()
}

func (s *syncLog) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func (s *syncLog) since(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls[n:]...)
}

func (s *syncLog) reset() { s.mu.Lock(); s.calls, s.linked = nil, nil; s.mu.Unlock() }

func snapshotInputs() runtime.PreparationInputs {
	return runtime.PreparationInputs{Scope: domain.Scope{TenantID: "t", ProjectID: "p"}, RunID: "run1", Digest: "d",
		Sources: []runtime.SourceInput{{Name: "s", Locator: "https://x/git/a/b.git", Revision: strings.Repeat("a", 40), Path: "."}}}
}

func contains(calls []string, dir string) bool {
	for _, c := range calls {
		if c == dir {
			return true
		}
	}
	return false
}

// Test_unit_shell_source_snapshot_durability proves a snapshot is handed out only
// after the whole directory chain it hangs from was synchronized, on every path
// (publication, the race with another writer, and reading), and that a failure to
// synchronize is returned, retried and never acknowledged.
func Test_unit_shell_source_snapshot_durability(t *testing.T) {
	key := strings.Repeat("ab", 32)
	ctx := context.Background()
	boom := errors.New("fsync: input/output error")

	t.Run("the chain is synced deepest first before linking and the entry after", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "state", "snapshots")
		log := &syncLog{key: key, dir: dir}
		f := FileSnapshots{Dir: dir, Root: base, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); err != nil {
			t.Fatal(err)
		}
		want := []string{dir, filepath.Join(base, "state"), base, dir}
		if strings.Join(log.calls, "\n") != strings.Join(want, "\n") {
			t.Fatalf("syncs:\n%s\nwant:\n%s", strings.Join(log.calls, "\n"), strings.Join(want, "\n"))
		}
		if log.linked[0] || log.linked[1] || log.linked[2] || !log.linked[3] {
			t.Fatalf("the chain must be durable before the link and the entry synced after it: %v", log.linked)
		}
	})

	t.Run("an unbounded chain reaches the filesystem root", func(t *testing.T) {
		base := t.TempDir()
		log := &syncLog{key: key, dir: filepath.Join(base, "snapshots")}
		f := FileSnapshots{Dir: log.dir, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); err != nil {
			t.Fatal(err)
		}
		if !contains(log.calls, base) || !contains(log.calls, string(filepath.Separator)) {
			t.Fatalf("an ancestor was left out: %v", log.calls)
		}
		if _, err := (FileSnapshots{Dir: log.dir, Root: filepath.Join(base, "snapshots", "deeper")}).chain(); err == nil {
			t.Fatal("a directory outside the root was accepted")
		}
	})

	t.Run("reading a visible snapshot establishes durability before returning it", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "snapshots")
		log := &syncLog{key: key, dir: dir}
		f := FileSnapshots{Dir: dir, Root: base, syncDir: log.sync}
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); err != nil {
			t.Fatal(err)
		}
		log.reset()
		if got, ok, err := f.Get(ctx, key); err != nil || !ok || got.Digest != "d" {
			t.Fatalf("%+v %v %v", got, ok, err)
		}
		if !contains(log.calls, dir) || !contains(log.calls, base) {
			t.Fatalf("Get acknowledged a record without synchronizing its chain: %v", log.calls)
		}
		log.reset()
		log.setFail(func(string, bool) error { return boom })
		if got, ok, err := f.Get(ctx, key); !errors.Is(err, boom) || ok || got.Digest != "" {
			t.Fatalf("Get returned a record whose directory could not be synced: %+v %v %v", got, ok, err)
		}
		// An absent record needs no synchronization and is simply absent.
		log.reset()
		if _, ok, err := f.Get(ctx, strings.Repeat("cd", 32)); ok || err != nil || len(log.calls) != 0 {
			t.Fatalf("an absent record: %v %v %v", ok, err, log.calls)
		}
	})

	t.Run("a failed publication is not acknowledged by Preparer.Inputs until durability is established", func(t *testing.T) {
		mem := store.NewMemoryGit()
		mem.AddTenant("t", "acme")
		mem.AddProject("t", "p", "erp")
		mem.SetRun(store.RunGitInfo{TenantID: "t", ProjectID: "p", RunID: "run1", PipelineName: "build", Status: "running", Sources: []byte("[]")})
		base := t.TempDir()
		dir := filepath.Join(base, "snapshots")
		req := runtime.PrepareRequest{Scope: domain.Scope{TenantID: "t", ProjectID: "p"}, RunID: "run1", PlanDigest: "plan"}
		key := snapshotKey(req)
		log := &syncLog{key: key, dir: dir}
		p, err := NewPreparer(PreparerConfig{Runs: mem, Repositories: mem, Keys: fixedKeys{}, Tips: noTips{}, BaseURL: "https://kodama.example",
			Snapshots: FileSnapshots{Dir: dir, Root: base, syncDir: log.sync}})
		if err != nil {
			t.Fatal(err)
		}
		// The directory entry cannot be synced once the record is linked.
		log.setFail(func(d string, linked bool) error {
			if linked {
				return boom
			}
			return nil
		})
		for i := range 3 {
			if _, err := p.Inputs(ctx, req); !errors.Is(err, ErrSourceUnavailable) || !strings.Contains(err.Error(), "input/output error") {
				t.Fatalf("call %d acknowledged inputs that are not durable: %v", i+1, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, key)); err != nil {
			t.Fatalf("the failed publication did not leave the record linked: %v", err)
		}
		// Durability is established: the same record is returned, and not before.
		log.setFail(nil)
		first, err := p.Inputs(ctx, req)
		if err != nil || first.Digest == "" {
			t.Fatalf("recovery: %v", err)
		}
		again, err := p.Inputs(ctx, req)
		if err != nil || again.Digest != first.Digest {
			t.Fatalf("the recovered record changed: %v", err)
		}
		// And it is checked again: a later failure stops the next call.
		log.setFail(func(string, bool) error { return boom })
		if _, err := p.Inputs(ctx, req); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("an established record was acknowledged without a successful sync: %v", err)
		}
	})

	t.Run("a reader that finds a record before its writer synced does not acknowledge it unsynced", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "snapshots")
		log := &syncLog{key: key, dir: dir}
		f := FileSnapshots{Dir: dir, Root: base, syncDir: log.sync}
		reached, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		log.setFail(func(d string, linked bool) error {
			if linked {
				first := false
				once.Do(func() { first = true })
				if first { // the writer's post-link sync: held, then failed
					close(reached)
					<-release
					return boom
				}
			}
			return nil
		})
		writer := make(chan error, 1)
		go func() { _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); writer <- err }()
		<-reached
		// The record is visible while its writer has not finished: the reader's own
		// synchronization is what acknowledges it.
		before := log.count()
		got, ok, err := f.Get(ctx, key)
		if err != nil || !ok || got.Digest != "d" {
			t.Fatalf("the concurrent reader: %+v %v %v", got, ok, err)
		}
		if after := log.since(before); !contains(after, dir) || !contains(after, base) {
			t.Fatalf("the reader returned without synchronizing the chain itself: %v", after)
		}
		// A reader whose synchronization fails never acknowledges.
		log.setFail(func(string, bool) error { return boom })
		if _, ok, err := f.Get(ctx, key); ok || !errors.Is(err, boom) {
			t.Fatalf("a reader acknowledged a record it could not sync: %v %v", ok, err)
		}
		close(release)
		select {
		case err := <-writer:
			if !errors.Is(err, boom) {
				t.Fatalf("the writer's failed sync was acknowledged: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the writer never finished")
		}
	})

	t.Run("an ancestor's unsynced entry is retried even though the directory exists", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "a", "snapshots")
		log := &syncLog{key: key, dir: dir}
		f := FileSnapshots{Dir: dir, Root: base, syncDir: log.sync}
		// Creating base/a works; syncing base, which holds its entry, fails.
		log.setFail(func(d string, _ bool) error {
			if d == base {
				return boom
			}
			return nil
		})
		if _, err := f.PutIfAbsent(ctx, key, snapshotInputs()); !errors.Is(err, boom) {
			t.Fatalf("a chain whose ancestor entry could not be synced was acknowledged: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, key)); err == nil {
			t.Fatal("a snapshot was linked under a chain that is not durable")
		}
		if st, err := os.Stat(filepath.Join(base, "a")); err != nil || !st.IsDir() {
			t.Fatalf("the directory the failed call created is gone: %v", err)
		}
		// The retry finds base/a and base/a/snapshots present and must still sync base.
		log.setFail(nil)
		n := log.count()
		got, err := f.PutIfAbsent(ctx, key, snapshotInputs())
		if err != nil || got.Digest != "d" {
			t.Fatalf("the retry: %+v %v", got, err)
		}
		retry := log.since(n)
		if !contains(retry, base) || !contains(retry, filepath.Join(base, "a")) || !contains(retry, dir) {
			t.Fatalf("the retry trusted existing directories: %v", retry)
		}
		// A different process, finding the record, establishes the same chain.
		n = log.count()
		if _, ok, err := f.Get(ctx, key); err != nil || !ok || !contains(log.since(n), base) {
			t.Fatalf("a reader trusted the existing directories: %v %v %v", ok, err, log.since(n))
		}
	})

	t.Run("the real directory sync works and reports failures", func(t *testing.T) {
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

type fixedKeys struct{}

func (fixedKeys) ScopeKeys(context.Context, domain.Scope) (string, string, error) {
	return "acme", "erp", nil
}

type noTips struct{}

func (noTips) Tip(context.Context, domain.Scope, store.Repository, string) (string, error) {
	return "", errors.New("no tips in this test")
}
