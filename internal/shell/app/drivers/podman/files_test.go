package podman

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

func newTestFiles(t *testing.T) *Files {
	t.Helper()
	files, err := NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	return files
}

func TestFilesWriteIsAtomicAndPreservesMode(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	path := filepath.Join(files.Root(), "workload", "kodama", "inputs", "key")
	if err := files.MkdirAll(ctx, filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := files.WriteFile(ctx, path, []byte("first"), 0o444); err != nil {
		t.Fatalf("WriteFile first: %v", err)
	}
	info, err := files.Lstat(ctx, path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("mode = %o; want 444", info.Mode().Perm())
	}
	if err := files.WriteFile(ctx, path, []byte("second"), 0o444); err != nil {
		t.Fatalf("WriteFile replace: %v", err)
	}
	data, err := files.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("data = %q; want second", data)
	}
	entries, err := files.ReadDir(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries after atomic write; want 1", len(entries))
	}
}

func TestFilesRefusesSymlinkEscape(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(files.Root(), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A parent symlink must be refused.
	if err := files.WriteFile(ctx, filepath.Join(link, "secret"), []byte("x"), 0o600); err == nil {
		t.Fatal("write through a parent symlink was accepted")
	} else {
		assertForbidden(t, err)
	}
	if _, err := files.ReadFile(ctx, filepath.Join(link, "secret")); err == nil {
		t.Fatal("read through a parent symlink was accepted")
	}
	// A final symlink must be refused, not followed.
	final := filepath.Join(files.Root(), "final")
	if err := os.Symlink(target, final); err != nil {
		t.Fatalf("symlink final: %v", err)
	}
	if _, err := files.ReadFile(ctx, final); err == nil {
		t.Fatal("read of a final symlink was accepted")
	}
	if _, err := files.Lstat(ctx, final); err == nil {
		t.Fatal("lstat of a final symlink was accepted")
	}
}

func TestFilesRefusesPathOutsideRoot(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(files.Root()), "outside")
	if err := files.WriteFile(ctx, outside, []byte("x"), 0o600); err == nil {
		t.Fatal("write outside the adapter root was accepted")
	} else {
		assertForbidden(t, err)
	}
	if _, err := files.ReadFile(ctx, outside); err == nil {
		t.Fatal("read outside the adapter root was accepted")
	}
}

func TestFilesRenameAndRemoveAll(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	dir := filepath.Join(files.Root(), "workload")
	if err := files.MkdirAll(ctx, dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	from := filepath.Join(dir, "from")
	to := filepath.Join(dir, "to")
	if err := files.WriteFile(ctx, from, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := files.Rename(ctx, from, to); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := files.Lstat(ctx, from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	data, err := files.ReadFile(ctx, to)
	if err != nil || string(data) != "data" {
		t.Fatalf("destination = %q, %v", data, err)
	}
	if err := files.RemoveAll(ctx, dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := files.Lstat(ctx, dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still exists: %v", err)
	}
	if err := files.RemoveAll(ctx, dir); err != nil {
		t.Fatalf("RemoveAll absent: %v", err)
	}
}

func TestFilesHonorCancellation(t *testing.T) {
	files := newTestFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(files.Root(), "x")
	if err := files.WriteFile(ctx, path, []byte("x"), 0o600); err == nil {
		t.Fatal("cancelled write succeeded")
	}
	if _, err := files.Lstat(ctx, path); err == nil {
		t.Fatal("cancelled lstat succeeded")
	}
}

func assertForbidden(t *testing.T, err error) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.Forbidden {
		t.Fatalf("fault class = %v (%v); want forbidden", class, err)
	}
}

func TestFilesWriteFileIsAtomicForConcurrentReaders(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	path := filepath.Join(files.Root(), "credential")
	first := bytes.Repeat([]byte("a"), 1<<20)
	second := bytes.Repeat([]byte("b"), 1<<20)
	if err := files.WriteFile(ctx, path, first, 0o444); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := range 200 {
			payload := first
			if index%2 == 1 {
				payload = second
			}
			if err := files.WriteFile(ctx, path, payload, 0o444); err != nil {
				t.Errorf("WriteFile: %v", err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		data, err := files.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if len(data) != len(first) {
			t.Fatalf("atomic write exposed a %d-byte file; want %d", len(data), len(first))
		}
		if data[0] != first[0] && data[0] != second[0] {
			t.Fatalf("atomic write exposed an unexpected byte %q", data[0])
		}
	}
}

func TestFilesWriteAndRenameSyncDirectories(t *testing.T) {
	files := newTestFiles(t)
	ctx := context.Background()
	srcDir := filepath.Join(files.Root(), "src")
	dstDir := filepath.Join(files.Root(), "dst")
	if err := files.MkdirAll(ctx, srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	if err := files.MkdirAll(ctx, dstDir, 0o755); err != nil {
		t.Fatalf("MkdirAll dst: %v", err)
	}

	var mu sync.Mutex
	var synced []string
	files.syncDir = func(dir string) error {
		mu.Lock()
		synced = append(synced, dir)
		mu.Unlock()
		return syncDirectory(dir)
	}

	from := filepath.Join(srcDir, "token")
	to := filepath.Join(dstDir, "token")
	if err := files.WriteFile(ctx, from, []byte("secret"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mu.Lock()
	writeSynced := append([]string(nil), synced...)
	synced = nil
	mu.Unlock()
	if !slices.Contains(writeSynced, srcDir) {
		t.Fatalf("WriteFile synced %v; want %q", writeSynced, srcDir)
	}

	if err := files.Rename(ctx, from, to); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	mu.Lock()
	renameSynced := append([]string(nil), synced...)
	mu.Unlock()
	if !slices.Contains(renameSynced, dstDir) || !slices.Contains(renameSynced, srcDir) {
		t.Fatalf("Rename synced %v; want both %q and %q", renameSynced, dstDir, srcDir)
	}
}
