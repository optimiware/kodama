// OS file and directory operations behind the process.Files seam.
//
// Stat is lstat, so a symlink is reported as a symlink and never followed.
// ReadDir reports entry type bits without following a symlinked entry. Reads
// refuse to follow a symlink at the final path component. A write stages a
// sibling temporary with owner-only permissions, writes and fsyncs it, applies
// the requested mode and renames it over the target, so a running workload that
// rereads a prepared credential sees the complete previous version or the
// complete replacement and never a partial file.
package processdriver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/optimiware/kodama/internal/fault"
)

// writeTempPrefix names the staged sibling of one atomic write. It is distinct
// from the bookkeeping temporary prefix so a leftover is identifiable.
const writeTempPrefix = ".kodama-write-"

// MkdirAll creates one confined directory tree.
func (d *Driver) MkdirAll(ctx context.Context, path string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "directory creation failed", err)
	}
	return nil
}

// WriteFile atomically replaces one file with the supplied bytes and final
// mode. The replacement is staged in the target's own directory and renamed
// over it, so a concurrent reader observes either the whole previous file or
// the whole new file. An existing read-only prepared file is replaced the same
// way, so its inode is not mutated in place. On any failure before the commit
// the staged file is removed and the previous content is untouched.
func (d *Driver) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Refuse an unusable target before staging anything. The final rename never
	// follows a symlink, so a swap after this check cannot redirect the write.
	existing, statErr := os.Lstat(path)
	if statErr == nil {
		if existing.IsDir() {
			return fault.New(fault.InvalidInput, "write target is a directory")
		}
		if existing.Mode()&os.ModeSymlink != 0 {
			return fault.New(fault.Forbidden, "write target is a symlink")
		}
	} else if !os.IsNotExist(statErr) {
		return fault.Wrap(fault.DependencyUnavailable, "write target is unreachable", statErr)
	}

	directory := filepath.Dir(path)
	staged, err := createWriteTemp(directory)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be staged", err)
	}
	stagedName := staged.Name()
	defer func() { _ = os.Remove(stagedName) }()

	if _, err := staged.Write(data); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file write failed", err)
	}
	// Apply the requested mode through the descriptor, so the staged path
	// cannot be swapped for a symlink between the write and the mode change.
	if err := staged.Chmod(mode); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file mode could not be applied", err)
	}
	// Sync after the mode change so the published inode has both its bytes and
	// its final permissions durable before the rename makes it visible.
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be synced", err)
	}
	if err := staged.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be completed", err)
	}
	// The replacement is complete; commit it only while the caller still has
	// budget. A cancelled commit removes the staged file and keeps the previous
	// version visible.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.renameFile(stagedName, path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be committed", err)
	}
	// The rename is already visible to readers. A directory sync failure only
	// makes the rename's durability unknown, so the new content stays in place.
	if err := d.syncDir(directory); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "file write could not be made durable", err)
	}
	return nil
}

// createWriteTemp creates one new staged sibling with owner-only permissions.
// O_EXCL refuses an existing path, O_NOFOLLOW refuses a symlink at that path,
// and a crypto/rand name makes a collision negligible.
func createWriteTemp(directory string) (*os.File, error) {
	for range 8 {
		var raw [12]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		name := writeTempPrefix + hex.EncodeToString(raw[:])
		staged, err := os.OpenFile(filepath.Join(directory, name),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			return staged, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, errors.New("staged file name collision")
}

// syncDirectory fsyncs one directory so a completed rename survives a crash.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

// ReadFile reads one file without following a symlink at the final component.
func (d *Driver) ReadFile(ctx context.Context, path string) ([]byte, error) {
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

// Stat is lstat: it never follows a symlink.
func (d *Driver) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

// ReadDir lists one directory without following symlinked entries.
func (d *Driver) ReadDir(ctx context.Context, path string) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "directory read failed", err)
	}
	return entries, nil
}

// Rename replaces the destination atomically on one filesystem.
func (d *Driver) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "rename failed", err)
	}
	toDir := filepath.Clean(filepath.Dir(to))
	if err := d.syncDir(toDir); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "rename could not be made durable", err)
	}
	if fromDir := filepath.Clean(filepath.Dir(from)); fromDir != toDir {
		if err := d.syncDir(fromDir); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "rename could not be made durable", err)
		}
	}
	return nil
}

// RemoveAll removes one tree. It is idempotent for an absent path.
func (d *Driver) RemoveAll(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "removal failed", err)
	}
	return nil
}
