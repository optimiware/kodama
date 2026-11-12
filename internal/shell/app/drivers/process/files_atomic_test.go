package processdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// TestWriteFileIsAtomicForConcurrentReaders proves the credential-refresh
// contract: a running workload reading a prepared file concurrently with a
// replacement observes either the complete previous payload or the complete
// replacement, never a truncated or mixed version.
func TestWriteFileIsAtomicForConcurrentReaders(t *testing.T) {
	driver := newTestDriver(t, 0)
	path := filepath.Join(t.TempDir(), "KODAMA_TOKEN")
	first := bytes.Repeat([]byte("a"), 1<<20)
	second := bytes.Repeat([]byte("b"), 1<<20)
	if err := driver.WriteFile(context.Background(), path, first, 0o444); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	const replacements = 200
	writerDone := make(chan error, 1)
	go func() {
		for index := range replacements {
			payload := first
			if index%2 == 1 {
				payload = second
			}
			if err := driver.WriteFile(context.Background(), path, payload, 0o444); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	reads := 0
	var failure string
	for failure == "" {
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("concurrent WriteFile: %v", err)
			}
			data, err := driver.ReadFile(context.Background(), path)
			if err != nil {
				t.Fatalf("final ReadFile: %v", err)
			}
			if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
				t.Fatalf("final ReadFile exposed a %d-byte torn payload", len(data))
			}
			if reads == 0 {
				t.Fatal("the reader never observed the file")
			}
			return
		default:
		}
		data, err := driver.ReadFile(context.Background(), path)
		if err != nil {
			failure = fmt.Sprintf("concurrent ReadFile: %v", err)
			break
		}
		if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
			failure = fmt.Sprintf("WriteFile exposed a %d-byte torn payload: want one complete %d-byte version",
				len(data), len(first))
			break
		}
		reads++
	}
	// Let the writer finish so the test never logs after completion.
	if err := <-writerDone; err != nil {
		t.Fatalf("concurrent WriteFile: %v", err)
	}
	t.Fatal(failure)
}

// TestWriteFileConcurrentWritersNeverTear proves two concurrent replacements
// of the same credential never mix: every observed version is one writer's
// complete payload, because the last completed rename wins atomically.
func TestWriteFileConcurrentWritersNeverTear(t *testing.T) {
	driver := newTestDriver(t, 0)
	path := filepath.Join(t.TempDir(), "KODAMA_TOKEN")
	first := bytes.Repeat([]byte("c"), 512<<10)
	second := bytes.Repeat([]byte("d"), 512<<10)
	if err := driver.WriteFile(context.Background(), path, first, 0o444); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	const writes = 60
	var writers sync.WaitGroup
	errs := make(chan error, 2)
	for _, payload := range [][]byte{first, second} {
		writers.Go(func() {
			for range writes {
				if err := driver.WriteFile(context.Background(), path, payload, 0o444); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	finished := make(chan struct{})
	go func() {
		writers.Wait()
		close(finished)
	}()

	var failure string
	for failure == "" {
		select {
		case <-finished:
			data, err := driver.ReadFile(context.Background(), path)
			if err != nil {
				t.Fatalf("final ReadFile: %v", err)
			}
			if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
				t.Fatalf("final ReadFile exposed a %d-byte torn payload", len(data))
			}
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent WriteFile: %v", err)
			}
			return
		default:
		}
		data, err := driver.ReadFile(context.Background(), path)
		if err != nil {
			failure = fmt.Sprintf("concurrent ReadFile: %v", err)
			break
		}
		if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
			failure = fmt.Sprintf("concurrent writers exposed a %d-byte torn payload", len(data))
			break
		}
	}
	<-finished
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent WriteFile: %v", err)
	}
	t.Fatal(failure)
}

// TestWriteFileFailedCommitKeepsPreviousContent proves a commit failure removes
// the staged temporary and leaves the old credential bytes readable.
func TestWriteFileFailedCommitKeepsPreviousContent(t *testing.T) {
	driver := newTestDriver(t, 0)
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	original := []byte("old-credential")
	if err := driver.WriteFile(context.Background(), path, original, 0o444); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}
	driver.renameFile = func(string, string) error {
		return errors.New("injected rename failure")
	}
	err := driver.WriteFile(context.Background(), path, []byte("new-credential"), 0o444)
	if err == nil {
		t.Fatal("WriteFile succeeded although the commit failed")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("fault class = %v (%v); want dependency_unavailable", class, err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile after a failed commit: %v", readErr)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("content after a failed commit = %q; want the previous %q", data, original)
	}
	assertOnlyFile(t, dir, "credential")
}

// TestWriteFileUnwritableDirectoryKeepsPreviousContent injects an ENOSPC-like
// staging failure through a directory the writer cannot add entries to.
func TestWriteFileUnwritableDirectoryKeepsPreviousContent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not constrain the root user")
	}
	driver := newTestDriver(t, 0)
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	path := filepath.Join(dir, "credential")
	original := []byte("old-credential")
	if err := driver.WriteFile(context.Background(), path, original, 0o600); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := driver.WriteFile(context.Background(), path, []byte("new-credential"), 0o600)
	if err == nil {
		t.Fatal("WriteFile succeeded in an unwritable directory")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("fault class = %v (%v); want dependency_unavailable", class, err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile after a failed write: %v", readErr)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("content after a failed write = %q; want the previous %q", data, original)
	}
	assertOnlyFile(t, dir, "credential")
}

// TestWriteFileRefusesDirectoryAndSymlinkTargets pins the refusals: a
// directory is invalid input and a symlink is forbidden, and neither target is
// modified.
func TestWriteFileRefusesDirectoryAndSymlinkTargets(t *testing.T) {
	driver := newTestDriver(t, 0)
	dir := t.TempDir()
	ctx := context.Background()

	directoryTarget := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(directoryTarget, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err := driver.WriteFile(ctx, directoryTarget, []byte("x"), 0o600)
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("directory target fault class = %v (%v); want invalid_input", class, err)
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err = driver.WriteFile(ctx, link, []byte("overwrite"), 0o600)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("symlink target fault class = %v (%v); want forbidden", class, err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read symlink target: %v", readErr)
	}
	if string(data) != "secret" {
		t.Fatalf("symlink target content = %q; want secret", data)
	}
}

// TestWriteFileAppliesRequestedMode proves the published file has exactly the
// requested permission bits, including a replacement of an existing file.
func TestWriteFileAppliesRequestedMode(t *testing.T) {
	driver := newTestDriver(t, 0)
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o600, 0o444, 0o640} {
		path := filepath.Join(dir, fmt.Sprintf("file-%o", mode))
		if err := driver.WriteFile(context.Background(), path, []byte("payload"), mode); err != nil {
			t.Fatalf("WriteFile(%o): %v", mode, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat(%o): %v", mode, err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("mode = %o; want %o", info.Mode().Perm(), mode)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("mode %o produced a symlink", mode)
		}
	}
}

// TestWriteFileStagesTemporaryWithOwnerOnlyPermissions proves the staged file
// is owner-only at commit time: it is never created with the process umask's
// default (usually 0644) while it holds credential bytes.
func TestWriteFileStagesTemporaryWithOwnerOnlyPermissions(t *testing.T) {
	driver := newTestDriver(t, 0)
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	payload := bytes.Repeat([]byte("s"), 1<<20)

	var mu sync.Mutex
	var observed []os.FileMode
	var sizes []int64
	driver.renameFile = func(from, to string) error {
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		mu.Lock()
		observed = append(observed, info.Mode().Perm())
		sizes = append(sizes, info.Size())
		mu.Unlock()
		return os.Rename(from, to)
	}

	const writes = 20
	for range writes {
		if err := driver.WriteFile(context.Background(), path, payload, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != writes {
		t.Fatalf("observed %d staged files; want %d", len(observed), writes)
	}
	for index, perm := range observed {
		if perm != 0o600 {
			t.Fatalf("staged file %d mode = %o; want owner-only 600", index, perm)
		}
		if sizes[index] != int64(len(payload)) {
			t.Fatalf("staged file %d size = %d; want the complete %d-byte payload",
				index, sizes[index], len(payload))
		}
	}
}

// assertOnlyFile proves a directory contains exactly the named file: no staged
// temporary survived a failed write.
func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, len(entries))
		for index, entry := range entries {
			names[index] = entry.Name()
		}
		t.Fatalf("directory entries = %v; want only %q (no staged temporary)", names, name)
	}
}
