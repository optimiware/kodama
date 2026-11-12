package userlandfs

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileSystemReadWriteReadDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	files := FileSystem{}
	if err := files.WriteFile(path, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := files.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"token":"x"}` {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o; want 600", info.Mode().Perm())
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entries, err := files.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d; want 2", len(entries))
	}
	if entries[0].Name != "session.json" || entries[0].IsDir {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].Name != "sub" || !entries[1].IsDir {
		t.Fatalf("second entry = %+v", entries[1])
	}
}

func TestFileSystemReportsMissingFile(t *testing.T) {
	if _, err := (FileSystem{}).ReadFile(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("ReadFile error = %v; want not-exist", err)
	}
	if _, err := (FileSystem{}).ReadDir(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("ReadDir error = %v; want not-exist", err)
	}
}

// TestReplaceFileCreatesMissingParent pins that a first login creates the
// missing parent directory 0700 while leaving an existing directory mode
// untouched, and that the final file has the requested mode.
func TestReplaceFileCreatesMissingParent(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(existing, 0o750); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	target := filepath.Join(existing, "config", "kodama", "session.json")
	files := FileSystem{}
	if err := files.ReplaceFile(target, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	for _, dir := range []string{filepath.Join(existing, "config"), filepath.Join(existing, "config", "kodama")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%s): %v", dir, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("created directory %s mode = %o; want 700", dir, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(existing); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("existing directory mode changed: info=%v err=%v", info, err)
	}
	data, err := files.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"token":"x"}` {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %o; want 600", info.Mode().Perm())
	}
}

// TestReplaceFileReplacesLooserFile pins that a pre-existing 0644 session file
// is replaced: the old bytes and the old mode are both gone.
func TestReplaceFileReplacesLooserFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "session.json")
	if err := os.WriteFile(target, []byte(`{"token":"old"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	files := FileSystem{}
	if err := files.ReplaceFile(target, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"token":"rotated"}` {
		t.Fatalf("content = %q; want the new payload", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %o; want 600", info.Mode().Perm())
	}
}

// TestReplaceFileFailureRemovesTempAndKeepsOld pins the failure path: a
// directory target is refused, the previous target is untouched and no
// temporary file is left behind.
func TestReplaceFileFailureRemovesTempAndKeepsOld(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	marker := filepath.Join(target, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if err := (FileSystem{}).ReplaceFile(target, []byte("data"), 0o600); err == nil {
		t.Fatal("ReplaceFile accepted a directory target")
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory target was replaced: info=%v err=%v", info, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("previous target changed: data=%q err=%v", data, err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("directory entries = %d; want %d (temporary file leaked)", len(after), len(before))
	}
}

// TestReplaceFileTempIsPrivate0600 pins that the temporary file is created in
// the target directory with mode 0600 from the start.
func TestReplaceFileTempIsPrivate0600(t *testing.T) {
	dir := t.TempDir()
	file, err := createTempFile(dir, "session.json")
	if err != nil {
		t.Fatalf("createTempFile: %v", err)
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if filepath.Dir(name) != dir {
		t.Errorf("temporary file %s is not in %s", name, dir)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary file mode = %o; want 600", info.Mode().Perm())
	}
}

// TestReplaceFileReplacesSymlinkAsLink pins that a symlink target is replaced
// as a link, never followed to its destination.
func TestReplaceFileReplacesSymlinkAsLink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("victim"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target := filepath.Join(dir, "session.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := (FileSystem{}).ReplaceFile(target, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "victim" {
		t.Fatalf("symlink destination changed: data=%q err=%v", data, err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ReplaceFile kept the symlink instead of replacing it with a file")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != `{"token":"x"}` {
		t.Fatalf("replaced content = %q err=%v", data, err)
	}
}

// TestReplaceFileConcurrentReplaceLeavesOneWholePayload pins that concurrent
// atomic replaces leave exactly one complete payload and no temporary file.
func TestReplaceFileConcurrentReplaceLeavesOneWholePayload(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "session.json")
	payloads := [][]byte{
		bytes.Repeat([]byte("a"), 64*1024),
		bytes.Repeat([]byte("b"), 64*1024),
		bytes.Repeat([]byte("c"), 64*1024),
		bytes.Repeat([]byte("d"), 64*1024),
	}
	files := FileSystem{}
	var wait sync.WaitGroup
	errs := make(chan error, len(payloads))
	for _, payload := range payloads {
		wait.Go(func() {
			errs <- files.ReplaceFile(target, payload, 0o600)
		})
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ReplaceFile: %v", err)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	matched := false
	for _, payload := range payloads {
		if bytes.Equal(data, payload) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final content is %d bytes and matches no complete payload", len(data))
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %o; want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "session.json" {
		t.Fatalf("directory entries = %v; want only session.json", entries)
	}
}

// TestRemoveSessionFile pins the injected deletion seam: a regular file is
// removed, a missing target is reported classifiably, a directory is refused
// and never removed recursively, and a symlink is removed as a link so its
// target survives.
func TestRemoveSessionFile(t *testing.T) {
	root := t.TempDir()
	files := FileSystem{}
	target := filepath.Join(root, "session.json")
	if err := files.WriteFile(target, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := files.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("Remove left the file: %v", err)
	}
	if err := files.Remove(target); !os.IsNotExist(err) {
		t.Fatalf("Remove(missing) error = %v; want not-exist", err)
	}

	directory := filepath.Join(root, "keep")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := files.Remove(directory); err == nil {
		t.Fatal("Remove accepted a directory")
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("Remove changed the directory: %v", err)
	}

	regular := filepath.Join(root, "credential")
	if err := files.WriteFile(regular, []byte("credential"), 0o600); err != nil {
		t.Fatalf("WriteFile(regular): %v", err)
	}
	link := filepath.Join(root, "session-link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := files.Remove(link); err != nil {
		t.Fatalf("Remove(symlink): %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("Remove left the symlink: %v", err)
	}
	if _, err := os.Stat(regular); err != nil {
		t.Fatalf("Remove followed the symlink and removed its target: %v", err)
	}
}
