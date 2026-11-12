// Package userlandfs implements the Userland-declared kodactl filesystem port
// over the operating system file API.
//
// The wrapper is deliberately tiny: it exposes only the operations the
// consumer-owned cli.FileSystem port declares and adds no policy of its own.
// Command logic still maps a missing file, a permission failure and a
// directory read error.
package userlandfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// FileSystem implements cli.FileSystem.
type FileSystem struct{}

var _ cli.FileSystem = FileSystem{}

// ReadFile reads one file.
func (FileSystem) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// WriteFile writes one file with the requested mode.
func (FileSystem) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// ReplaceFile atomically replaces one file with data and the final mode perm.
//
// Missing parent directories are created with mode 0700; an existing directory
// keeps its mode. The bytes go to an exclusively created 0600 temporary file
// in the same directory, are fsynced, the temporary file is chmodded to perm,
// and it is renamed over name before the directory is fsynced. A failure
// removes the temporary file and leaves the previous file untouched. A
// symlink at name is replaced as a link, not followed; a directory at name is
// refused by the rename.
func (FileSystem) ReplaceFile(name string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := createTempFile(dir, filepath.Base(name))
	if err != nil {
		return err
	}
	tempName := temp.Name()
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Chmod(perm); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, name); err != nil {
		return err
	}
	committed = true
	return syncDirectory(dir)
}

// createTempFile creates one exclusive 0600 temporary file in dir.
func createTempFile(dir, base string) (*os.File, error) {
	return os.CreateTemp(dir, "."+base+".tmp-*")
}

// syncDirectory fsyncs a directory so a completed rename is durable.
func syncDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// ReadDir lists one directory in lexical order.
func (FileSystem) ReadDir(name string) ([]cli.DirEntry, error) {
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, err
	}
	result := make([]cli.DirEntry, len(entries))
	for position, entry := range entries {
		result[position] = cli.DirEntry{Name: entry.Name(), IsDir: entry.IsDir()}
	}
	return result, nil
}

// Remove deletes one file or symlink. A directory is refused and is never
// removed recursively. A symlink is removed as a link, never followed. A
// missing target is returned as an error the caller classifies.
func (FileSystem) Remove(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &os.PathError{Op: "remove", Path: name, Err: errIsDirectory}
	}
	return os.Remove(name)
}

// errIsDirectory reports a refused directory removal.
var errIsDirectory = errors.New("is a directory")
