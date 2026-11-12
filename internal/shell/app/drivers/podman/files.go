// Host file and directory operations behind the podman.Files seam.
//
// Every operation is confined under one adapter root: a path outside the root
// is refused, a symlink in any component is refused instead of followed, and a
// write is staged in the destination directory and renamed into place so a
// partial write never becomes the prepared file. Reads use O_NOFOLLOW so the
// final component cannot be swapped for a symlink between the check and the
// read.
package podman

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/optimiware/kodama/internal/fault"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// Files is the confined host file boundary. It implements podman.Files.
type Files struct {
	root    string
	syncDir func(string) error
}

var _ runtimepodman.Files = (*Files)(nil)

// NewFiles validates the adapter root and creates it when absent. It opens no
// other path.
func NewFiles(root string) (*Files, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, fault.New(fault.InvalidInput, "podman file boundary requires an absolute adapter root")
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "podman adapter root is unavailable", err)
	}
	return &Files{root: clean, syncDir: syncDirectory}, nil
}

// Root returns the confined adapter root.
func (f *Files) Root() string { return f.root }

// confine validates that path is inside the adapter root and that no existing
// component below the root is a symlink. A missing component ends the walk: a
// path that does not exist yet cannot traverse an existing symlink.
func (f *Files) confine(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", fault.New(fault.InvalidInput, "podman file path is not absolute")
	}
	clean := filepath.Clean(path)
	relative, err := filepath.Rel(f.root, clean)
	if err != nil {
		return "", fault.Wrap(fault.Forbidden, "podman file path is not confined", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fault.New(fault.Forbidden, "podman file path is outside the adapter root")
	}
	if relative == "." {
		return clean, nil
	}
	current := f.root
	for segment := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return "", fault.New(fault.Forbidden, "podman file path is not confined")
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return clean, nil
			}
			return "", fault.Wrap(fault.DependencyUnavailable, "podman file path is unreachable", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fault.New(fault.Forbidden, "podman file path traverses a symlink")
		}
	}
	return clean, nil
}

// MkdirAll creates one confined directory tree.
func (f *Files) MkdirAll(ctx context.Context, path string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	confined, err := f.confine(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(confined, mode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman directory creation failed", err)
	}
	return nil
}

// WriteFile atomically replaces one file with the supplied bytes and mode. A
// temporary sibling is written, chmodded and renamed over the target, so the
// previous bytes survive any failure before the rename.
func (f *Files) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	confined, err := f.confine(path)
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(confined); statErr == nil && info.IsDir() {
		return fault.New(fault.InvalidInput, "podman write target is a directory")
	}
	directory := filepath.Dir(confined)
	temp, err := os.CreateTemp(directory, ".kodama-write-*")
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman write could not be staged", err)
	}
	tempName := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "podman write failed", err)
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "podman write mode could not be applied", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "podman write could not be synced", err)
	}
	if err := temp.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman write could not be completed", err)
	}
	if err := os.Rename(tempName, confined); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman write could not be committed", err)
	}
	committed = true
	if err := f.syncDirectory(directory); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman write could not be made durable", err)
	}
	return nil
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

func (f *Files) syncDirectory(path string) error {
	if f.syncDir != nil {
		return f.syncDir(path)
	}
	return syncDirectory(path)
}

// ReadFile reads one confined file without following a final symlink.
func (f *Files) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	confined, err := f.confine(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(confined, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "podman file read failed", err)
	}
	return data, nil
}

// Lstat never follows a symlink at the final component.
func (f *Files) Lstat(ctx context.Context, path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	confined, err := f.confine(path)
	if err != nil {
		return nil, err
	}
	return os.Lstat(confined)
}

// ReadDir lists one confined directory without following symlinked entries.
func (f *Files) ReadDir(ctx context.Context, path string) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	confined, err := f.confine(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(confined)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "podman directory read failed", err)
	}
	return entries, nil
}

// Rename atomically replaces the destination on one filesystem.
func (f *Files) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := f.confine(from)
	if err != nil {
		return err
	}
	destination, err := f.confine(to)
	if err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman rename failed", err)
	}
	destinationDir := filepath.Dir(destination)
	if err := f.syncDirectory(destinationDir); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman rename could not be made durable", err)
	}
	if sourceDir := filepath.Dir(source); sourceDir != destinationDir {
		if err := f.syncDirectory(sourceDir); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "podman rename could not be made durable", err)
		}
	}
	return nil
}

// RemoveAll removes one confined tree. An absent path is clean.
func (f *Files) RemoveAll(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	confined, err := f.confine(path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(confined); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "podman removal failed", err)
	}
	return nil
}
