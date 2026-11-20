// Package userlandfs implements the Userland-declared kodema filesystem port
// over the operating system file API.
//
// The wrapper is deliberately tiny: it exposes only the operations the
// consumer-owned cli.FileSystem port declares and adds no policy of its own.
// Command logic still maps a missing file, a permission failure and a
// directory read error.
package userlandfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// FileSystem implements cli.FileSystem.
type FileSystem struct{}

var _ cli.FileSystem = FileSystem{}

// ReadFile reads one file.
func (FileSystem) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// ReadRegularFile checks the opened descriptor, not the earlier directory
// listing: entries can change between the two. A FIFO must not block open.
func (FileSystem) ReadRegularFile(name string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(file, limit))
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

// CheckPrivateDir validates that dir can safely hold a human session file at
// mode 0700 without mutating the filesystem. Missing directories are allowed
// (they will be created at mode 0700 on save). Non-directories, symlinks, and
// shared/system directories (such as /, /tmp, $HOME, ~/.config, or sticky-bit
// directories) whose permissions are not already 0700 are refused with an
// actionable error.
func (FileSystem) CheckPrivateDir(dir string) error {
	cleaned := filepath.Clean(dir)
	info, err := os.Lstat(cleaned)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "the human session directory is unreadable", err)
	}
	if !info.IsDir() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("the human session directory %q is not a directory", cleaned))
	}
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	if isSharedSessionDir(cleaned, info) {
		return fault.New(
			fault.InvalidInput,
			fmt.Sprintf(
				"the human session directory %q has mode %04o and cannot be restricted to 0700; place the session file in a dedicated 0700 subdirectory",
				cleaned, info.Mode().Perm(),
			),
		)
	}
	return nil
}

// EnsurePrivateDir ensures dir exists as a regular directory with mode 0700.
// Missing directories are created at 0700; an existing non-shared user
// directory is tightened to 0700; shared/system directories or directories
// that cannot be restricted to 0700 are refused with an actionable error.
func (files FileSystem) EnsurePrivateDir(dir string) error {
	cleaned := filepath.Clean(dir)
	if err := files.CheckPrivateDir(cleaned); err != nil {
		return err
	}
	if err := os.MkdirAll(cleaned, 0o700); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, fmt.Sprintf("the human session directory %q could not be created", cleaned), err)
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "the human session directory is unreadable", err)
	}
	if !info.IsDir() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("the human session directory %q is not a directory", cleaned))
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(cleaned, 0o700); err != nil {
			return fault.Wrap(
				fault.DependencyUnavailable,
				fmt.Sprintf(
					"the human session directory %q has mode %04o and could not be restricted to 0700; use a dedicated 0700 subdirectory or run chmod 0700 %q",
					cleaned, info.Mode().Perm(), cleaned,
				),
				err,
			)
		}
		info, err = os.Lstat(cleaned)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "the human session directory is unreadable", err)
		}
		if info.Mode().Perm() != 0o700 {
			return fault.New(
				fault.DependencyUnavailable,
				fmt.Sprintf(
					"the human session directory %q has mode %04o and could not be restricted to 0700; use a dedicated 0700 subdirectory",
					cleaned, info.Mode().Perm(),
				),
			)
		}
	}
	return nil
}

// isSharedSessionDir reports whether dir is a shared or system directory whose
// permissions must never be silently tightened to 0700.
func isSharedSessionDir(dir string, info fs.FileInfo) bool {
	if info.Mode()&os.ModeSticky != 0 {
		return true
	}
	canonical := canonicalDirPath(dir)
	if canonical == string(filepath.Separator) {
		return true
	}
	var shared []string
	if tmp := os.TempDir(); tmp != "" {
		shared = append(shared, tmp)
	}
	shared = append(shared, "/tmp", "/var/tmp")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		shared = append(shared, home)
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		shared = append(shared, cfgDir)
	}
	for _, candidate := range shared {
		if canonical == canonicalDirPath(candidate) {
			return true
		}
	}
	return false
}

func canonicalDirPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// errIsDirectory reports a refused directory removal.
var errIsDirectory = errors.New("is a directory")
