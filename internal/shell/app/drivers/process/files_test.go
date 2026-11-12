package processdriver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestFilesReplaceReadOnlyPreparedFile(t *testing.T) {
	driver := newTestDriver(t, 0)
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("first"), 0o444); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := driver.WriteFile(context.Background(), path, []byte("second"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := driver.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("content = %q; want second", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("mode = %o; want 444", info.Mode().Perm())
	}
}

func TestFilesNeverFollowSymlinks(t *testing.T) {
	driver := newTestDriver(t, 0)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := driver.ReadFile(context.Background(), link); err == nil {
		t.Fatal("ReadFile followed a symlink")
	}
	if err := driver.WriteFile(context.Background(), link, []byte("overwrite"), 0o600); err == nil {
		t.Fatal("WriteFile followed a symlink")
	}
	info, err := driver.Stat(context.Background(), link)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Stat followed the symlink instead of reporting it")
	}
	entries, err := driver.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == "link" {
			found = true
			if entry.Type()&os.ModeSymlink == 0 {
				t.Fatal("ReadDir reported the symlink as a followed entry")
			}
		}
	}
	if !found {
		t.Fatal("ReadDir did not report the symlink")
	}
}

func TestFilesMkdirRenameRemove(t *testing.T) {
	driver := newTestDriver(t, 0)
	root := filepath.Join(t.TempDir(), "a", "b")
	if err := driver.MkdirAll(context.Background(), root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	from := filepath.Join(root, "from")
	to := filepath.Join(root, "to")
	if err := driver.WriteFile(context.Background(), from, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := driver.Rename(context.Background(), from, to); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	if err := driver.RemoveAll(context.Background(), filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("RemoveAll on a missing path: %v", err)
	}
	if err := driver.RemoveAll(context.Background(), root); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("tree still exists: %v", err)
	}
}

func TestFilesHonorCancellation(t *testing.T) {
	driver := newTestDriver(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := driver.MkdirAll(ctx, filepath.Join(t.TempDir(), "x"), 0o755); err == nil {
		t.Fatal("MkdirAll ignored cancellation")
	}
	if err := driver.WriteFile(ctx, filepath.Join(t.TempDir(), "x"), nil, 0o600); err == nil {
		t.Fatal("WriteFile ignored cancellation")
	}
	if _, err := driver.ReadFile(ctx, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("ReadFile ignored cancellation")
	}
	if _, err := driver.Stat(ctx, t.TempDir()); err == nil {
		t.Fatal("Stat ignored cancellation")
	}
	if _, err := driver.ReadDir(ctx, t.TempDir()); err == nil {
		t.Fatal("ReadDir ignored cancellation")
	}
	if err := driver.Rename(ctx, "a", "b"); err == nil {
		t.Fatal("Rename ignored cancellation")
	}
	if err := driver.RemoveAll(ctx, t.TempDir()); err == nil {
		t.Fatal("RemoveAll ignored cancellation")
	}
}

func TestFilesRenameSyncsSourceAndDestinationDirectories(t *testing.T) {
	driver := newTestDriver(t, 0)
	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	dstDir := filepath.Join(base, "dst")
	if err := driver.MkdirAll(context.Background(), srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	if err := driver.MkdirAll(context.Background(), dstDir, 0o755); err != nil {
		t.Fatalf("MkdirAll dst: %v", err)
	}
	from := filepath.Join(srcDir, "file")
	to := filepath.Join(dstDir, "file")
	if err := driver.WriteFile(context.Background(), from, []byte("payload"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var synced []string
	driver.syncDir = func(path string) error {
		mu.Lock()
		synced = append(synced, path)
		mu.Unlock()
		return syncDirectory(path)
	}
	if err := driver.Rename(context.Background(), from, to); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(synced, dstDir) || !slices.Contains(synced, srcDir) {
		t.Fatalf("Rename synced %v; want both %q and %q", synced, dstDir, srcDir)
	}
}
