package recovery

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/optimiware/kodama/internal/fault"
)

// OSFiles is the operating-system Files boundary. It never follows a symlink
// when reading, writing or snapshotting, so a workspace escape is refused
// instead of captured.
type OSFiles struct{}

var _ Files = OSFiles{}

// EnsureEmpty creates the destination when absent and refuses a non-empty one.
func (OSFiles) EnsureEmpty(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fault.New(fault.Forbidden, "restore destination is a symlink")
		}
		if !info.IsDir() {
			return fault.New(fault.InvalidInput, "restore destination is not a directory")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "restore destination is unreadable", err)
		}
		if len(entries) != 0 {
			return fault.New(fault.InvalidInput, "restore destination is not empty")
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "restore destination could not be created", err)
		}
		return nil
	default:
		return fault.Wrap(fault.DependencyUnavailable, "restore destination is unreachable", err)
	}
}

// Snapshot copies one source tree into a fresh destination and reports the
// number of regular files captured.
func (OSFiles) Snapshot(ctx context.Context, source, destination string) (int, error) {
	return copyTree(ctx, source, destination)
}

// Restore copies one captured tree into an already-empty destination.
func (OSFiles) Restore(ctx context.Context, source, destination string) error {
	_, err := copyTree(ctx, source, destination)
	return err
}

// Stat is lstat: it never follows a symlink.
func (OSFiles) Stat(ctx context.Context, path string) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return FileInfo{}, err
	}
	switch {
	case info.IsDir():
		return FileInfo{Kind: FileDirectory}, nil
	case info.Mode().IsRegular():
		return FileInfo{Kind: FileRegular, Size: info.Size()}, nil
	case info.Mode()&os.ModeSymlink != 0:
		return FileInfo{}, fault.New(fault.Forbidden, "path is a symlink")
	default:
		return FileInfo{}, fault.New(fault.InvalidInput, "path is not a regular file or directory")
	}
}

// ReadFile reads one file without following a symlink at the final component.
func (OSFiles) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "file read failed", err)
	}
	return data, nil
}

// WriteFile creates parent directories and atomically replaces the target. The
// replacement is staged in the target directory and renamed over it, so a
// concurrent reader observes either the whole previous file or the whole new
// file.
func (OSFiles) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file directory could not be created", err)
	}
	existing, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		if existing.Mode()&os.ModeSymlink != 0 {
			return fault.New(fault.Forbidden, "write target is a symlink")
		}
		if existing.IsDir() {
			return fault.New(fault.InvalidInput, "write target is a directory")
		}
	case !errors.Is(statErr, os.ErrNotExist):
		return fault.Wrap(fault.DependencyUnavailable, "write target is unreachable", statErr)
	}

	staged, err := os.CreateTemp(directory, ".kodama-recovery-*")
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be staged", err)
	}
	stagedName := staged.Name()
	defer func() { _ = os.Remove(stagedName) }()
	if _, err := staged.Write(data); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file write failed", err)
	}
	if err := staged.Chmod(0o644); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file mode could not be applied", err)
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be synced", err)
	}
	if err := staged.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be completed", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stagedName, path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be committed", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file directory could not be opened for sync", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file directory could not be synced", err)
	}
	if err := dir.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file directory could not be closed after sync", err)
	}
	return nil
}

// RemoveAll removes one tree. It is idempotent for an absent path.
func (OSFiles) RemoveAll(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "removal failed", err)
	}
	return nil
}

// copyTree copies one directory tree and reports the number of regular files
// copied. Symlinks and special files are refused so an escape is never
// captured.
func copyTree(ctx context.Context, source, destination string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return 0, fault.Wrap(fault.DependencyUnavailable, "source tree is unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fault.New(fault.Forbidden, "source tree is a symlink")
	}
	if !info.IsDir() {
		return 0, fault.New(fault.InvalidInput, "source tree is not a directory")
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return 0, fault.Wrap(fault.DependencyUnavailable, "destination tree could not be created", err)
	}
	count := 0
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if relative == "." {
				return nil
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fault.New(fault.Forbidden, "source tree contains a symlink")
		}
		if !entry.Type().IsRegular() {
			return fault.New(fault.Forbidden, "source tree contains a non-regular file")
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		if _, ok := errors.AsType[*fault.Error](err); ok {
			return 0, err
		}
		return 0, fault.Wrap(fault.DependencyUnavailable, "tree copy failed", err)
	}
	return count, nil
}

// copyFile copies one regular file and preserves its permission bits.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}
