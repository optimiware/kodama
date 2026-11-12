package recovery

// Direct tests for the real operating-system Files boundary. The fake Files
// used by the driver tests cannot exercise symlink refusal, atomic replacement
// or staging-file cleanup, so these cases run against t.TempDir.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// TestOSFilesEnsureEmpty covers the real destination guard: an absent path is
// created, a non-empty directory and a symlink are refused.
func TestOSFilesEnsureEmpty(t *testing.T) {
	files := OSFiles{}
	ctx := t.Context()
	root := t.TempDir()

	absent := filepath.Join(root, "absent", "nested")
	if err := files.EnsureEmpty(ctx, absent); err != nil {
		t.Fatalf("EnsureEmpty absent path: %v", err)
	}
	info, err := os.Lstat(absent)
	if err != nil || !info.IsDir() {
		t.Fatalf("EnsureEmpty did not create %s: %v", absent, err)
	}
	if err := files.EnsureEmpty(ctx, absent); err != nil {
		t.Fatalf("EnsureEmpty empty directory: %v", err)
	}

	nonEmpty := filepath.Join(root, "non-empty")
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "stale"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := files.EnsureEmpty(ctx, nonEmpty); err == nil {
		t.Fatal("EnsureEmpty accepted a non-empty directory")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("non-empty class = %v; want invalid_input (%v)", class, err)
	}

	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := files.EnsureEmpty(ctx, link); err == nil {
		t.Fatal("EnsureEmpty accepted a symlink")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("symlink class = %v; want forbidden (%v)", class, err)
	}
}

// TestOSFilesSnapshotRefusesSymlink proves a symlink inside a source tree is
// never captured.
func TestOSFilesSnapshotRefusesSymlink(t *testing.T) {
	files := OSFiles{}
	ctx := t.Context()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "regular.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write regular: %v", err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	destination := filepath.Join(root, "captured")
	if _, err := files.Snapshot(ctx, source, destination); err == nil {
		t.Fatal("Snapshot captured a source tree containing a symlink")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("snapshot symlink class = %v; want forbidden (%v)", class, err)
	}
}

// TestOSFilesWriteFileReplacesAtomically proves the target is replaced as one
// file and no staging file is left behind.
func TestOSFilesWriteFileReplacesAtomically(t *testing.T) {
	files := OSFiles{}
	ctx := t.Context()
	root := t.TempDir()
	target := filepath.Join(root, "nested", "value.txt")

	if err := files.WriteFile(ctx, target, []byte("first")); err != nil {
		t.Fatalf("WriteFile first: %v", err)
	}
	if err := files.WriteFile(ctx, target, []byte("second")); err != nil {
		t.Fatalf("WriteFile second: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("target = %q; want second", data)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "value.txt" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory entries = %v; want only value.txt (no stage file)", names)
	}
}

// TestOSFilesReadFileRefusesFinalSymlink proves a symlink is never followed at
// the final path component.
func TestOSFilesReadFileRefusesFinalSymlink(t *testing.T) {
	files := OSFiles{}
	ctx := t.Context()
	root := t.TempDir()
	real := filepath.Join(root, "real.txt")
	if err := os.WriteFile(real, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	data, err := files.ReadFile(ctx, real)
	if err != nil {
		t.Fatalf("ReadFile regular: %v", err)
	}
	if string(data) != "secret" {
		t.Fatalf("ReadFile regular = %q; want secret", data)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if data, err := files.ReadFile(ctx, link); err == nil {
		t.Fatalf("ReadFile followed a final-component symlink and read %q", data)
	}
}
