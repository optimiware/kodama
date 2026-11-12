package publiccontract

import (
	"errors"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"testing"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestFileSystemSeamShape pins the consumer-owned kodactl filesystem seam:
// ReadFile, WriteFile, ReadDir, ReplaceFile and Remove. ReplaceFile exists
// because WriteFile cannot create a missing parent directory or replace a
// pre-existing looser session file. Remove exists because logout must delete
// the session file through the same injected boundary.
func TestFileSystemSeamShape(t *testing.T) {
	typ := reflect.TypeOf((*cli.FileSystem)(nil)).Elem()
	want := map[string]reflect.Type{
		"ReadFile":    reflect.TypeOf(func(string) ([]byte, error) { return nil, nil }),
		"WriteFile":   reflect.TypeOf(func(string, []byte, fs.FileMode) error { return nil }),
		"ReplaceFile": reflect.TypeOf(func(string, []byte, fs.FileMode) error { return nil }),
		"Remove":      reflect.TypeOf(func(string) error { return nil }),
		"ReadDir":     reflect.TypeOf(func(string) ([]cli.DirEntry, error) { return nil, nil }),
	}
	if typ.NumMethod() != len(want) {
		t.Fatalf("cli.FileSystem has %d methods; want exactly %d", typ.NumMethod(), len(want))
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		method, ok := typ.MethodByName(name)
		if !ok {
			t.Errorf("cli.FileSystem is missing %s", name)
			continue
		}
		if method.Type != want[name] {
			t.Errorf("%s signature = %v; want %v", name, method.Type, want[name])
		}
	}
}

// TestFileSystemReplaceFileSemantics pins the documented ReplaceFile behavior
// over a small reference in-memory FileSystem: a missing parent is created
// 0700 without changing an existing directory, a pre-existing 0644 file is
// replaced by the requested mode and new bytes, the temporary file is created
// 0600, a failure keeps the previous bytes and a directory target is refused.
func TestFileSystemReplaceFileSemantics(t *testing.T) {
	files := newReferenceFileSystem()
	var seam cli.FileSystem = files
	target := "/home/engineer/.config/kodama/session.json"

	// An existing ancestor keeps its mode; only missing components are 0700.
	files.dirs["/home"] = 0o755
	files.dirs["/home/engineer"] = 0o755
	if err := seam.ReplaceFile(target, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("ReplaceFile(first login): %v", err)
	}
	for _, dir := range []string{"/home/engineer/.config", "/home/engineer/.config/kodama"} {
		if mode, ok := files.dirs[dir]; !ok {
			t.Errorf("ReplaceFile did not create missing parent %s", dir)
		} else if mode != 0o700 {
			t.Errorf("created parent %s mode = %o; want 700", dir, mode)
		}
	}
	if mode := files.dirs["/home/engineer"]; mode != 0o755 {
		t.Errorf("existing parent mode = %o; want 755 unchanged", mode)
	}
	if got := string(files.files[target].data); got != `{"token":"first"}` {
		t.Errorf("replaced bytes = %q", got)
	}
	if mode := files.files[target].mode; mode != 0o600 {
		t.Errorf("replaced mode = %o; want 600", mode)
	}

	// A pre-existing world-readable session file is replaced, not rewritten:
	// both the old bytes and the old mode are gone.
	files.files[target] = referenceFile{data: []byte(`{"token":"old"}`), mode: 0o644}
	if err := seam.ReplaceFile(target, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("ReplaceFile(rotate): %v", err)
	}
	if got := string(files.files[target].data); got != `{"token":"rotated"}` {
		t.Errorf("rotated bytes = %q; want the new payload", got)
	}
	if mode := files.files[target].mode; mode != 0o600 {
		t.Errorf("rotated mode = %o; want 600", mode)
	}

	// The temporary file is never looser than 0600.
	if len(files.tempModes) == 0 {
		t.Fatal("ReplaceFile never created a temporary file")
	}
	for _, mode := range files.tempModes {
		if mode != 0o600 {
			t.Errorf("temporary file mode = %o; want 600", mode)
		}
	}

	// A failure after the temporary file is created removes it and leaves the
	// previous bytes and mode untouched.
	before := files.files[target]
	files.failReplace = true
	if err := seam.ReplaceFile(target, []byte(`{"token":"lost"}`), 0o600); err == nil {
		t.Fatal("ReplaceFile accepted an injected failure")
	}
	if got := files.files[target]; string(got.data) != string(before.data) || got.mode != before.mode {
		t.Errorf("failed replace changed the previous file: %+v; want %+v", got, before)
	}

	// A directory target is refused and never replaced by a file.
	delete(files.files, target)
	files.dirs[target] = 0o700
	if err := seam.ReplaceFile(target, []byte("data"), 0o600); err == nil {
		t.Fatal("ReplaceFile accepted a directory target")
	}
	if _, ok := files.files[target]; ok {
		t.Error("ReplaceFile replaced a directory with a file")
	}
}

type referenceFile struct {
	data []byte
	mode fs.FileMode
}

// referenceFileSystem is a deterministic in-memory reference for the
// documented cli.FileSystem seam. It is a vector fixture, never product code.
type referenceFileSystem struct {
	dirs        map[string]fs.FileMode
	files       map[string]referenceFile
	tempModes   []fs.FileMode
	failReplace bool
}

func newReferenceFileSystem() *referenceFileSystem {
	return &referenceFileSystem{
		dirs:  map[string]fs.FileMode{"/": 0o755},
		files: make(map[string]referenceFile),
	}
}

var _ cli.FileSystem = (*referenceFileSystem)(nil)

func (files *referenceFileSystem) ReadFile(name string) ([]byte, error) {
	file, ok := files.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), file.data...), nil
}

func (files *referenceFileSystem) WriteFile(name string, data []byte, perm fs.FileMode) error {
	files.files[name] = referenceFile{data: append([]byte(nil), data...), mode: perm}
	return nil
}

func (files *referenceFileSystem) ReplaceFile(name string, data []byte, perm fs.FileMode) error {
	if _, isDir := files.dirs[name]; isDir {
		return errors.New("replace: target is a directory")
	}
	var missing []string
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		if _, ok := files.dirs[dir]; ok {
			break
		}
		missing = append(missing, dir)
		if dir == "/" || dir == "." {
			break
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		files.dirs[missing[index]] = 0o700
	}
	files.tempModes = append(files.tempModes, 0o600)
	if files.failReplace {
		return errors.New("replace: injected failure")
	}
	files.files[name] = referenceFile{data: append([]byte(nil), data...), mode: perm}
	return nil
}

func (files *referenceFileSystem) Remove(name string) error {
	if _, ok := files.files[name]; !ok {
		return fs.ErrNotExist
	}
	delete(files.files, name)
	return nil
}

func (files *referenceFileSystem) ReadDir(name string) ([]cli.DirEntry, error) {
	if _, ok := files.dirs[name]; !ok {
		return nil, fs.ErrNotExist
	}
	var entries []cli.DirEntry
	for file := range files.files {
		if path.Dir(file) == name {
			entries = append(entries, cli.DirEntry{Name: path.Base(file)})
		}
	}
	for dir := range files.dirs {
		if dir != name && path.Dir(dir) == name {
			entries = append(entries, cli.DirEntry{Name: path.Base(dir), IsDir: true})
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	return entries, nil
}
