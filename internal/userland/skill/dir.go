package skill

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DirSource is a confined directory of Markdown knowledge documents. It holds
// an open handle to the root and reads every leaf through that handle, so
// replacing the root path or the leaf after construction cannot redirect a
// read outside the root. It never follows a symlink in any path component.
type DirSource struct {
	root *os.Root
	path string
}

// NewDirSource resolves root to an absolute path, validates every component and
// pins an open handle to the directory. An empty root, a missing root, a
// symlinked component or a non-directory root is refused.
func NewDirSource(root string) (*DirSource, error) {
	if root == "" {
		return nil, fmt.Errorf("skill root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve skill root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	if err := checkNoSymlinkComponents(absolute); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat skill root %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill root %q is not a directory", absolute)
	}
	opened, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open skill root %q: %w", absolute, err)
	}
	// The handle must still name the directory that was inspected; a path
	// replaced between the check and the open is refused rather than pinned.
	openedInfo, err := opened.Stat(".")
	if err != nil {
		_ = opened.Close()
		return nil, fmt.Errorf("stat open skill root %q: %w", absolute, err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = opened.Close()
		return nil, fmt.Errorf("skill root %q changed while it was opened", absolute)
	}
	return &DirSource{root: opened, path: absolute}, nil
}

// Close releases the pinned root handle. A closed source refuses every read.
func (source *DirSource) Close() error {
	if source == nil || source.root == nil {
		return nil
	}
	return source.root.Close()
}

// checkNoSymlinkComponents Lstat's every component of an absolute path so a
// symlinked parent can never redirect a confined read.
func checkNoSymlinkComponents(absolute string) error {
	volume := filepath.VolumeName(absolute)
	rest := strings.TrimPrefix(absolute, volume)
	rest = strings.TrimPrefix(rest, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for component := range strings.SplitSeq(rest, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("stat skill root component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill root component %q is a symlink", current)
		}
	}
	return nil
}

// Read loads one bounded Markdown skill by validated name. The name maps to
// <root>/<name>.md only after ParseName, and the leaf must be a regular file
// opened through the pinned root, so no component can be redirected by a
// symlink and a leaf swapped after the check is detected before it is read.
func (source *DirSource) Read(ctx context.Context, name Name) (Skill, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	parsed, err := ParseName(string(name))
	if err != nil {
		return Skill{}, err
	}
	fileName := string(parsed) + ".md"

	info, err := source.root.Lstat(fileName)
	if err != nil {
		return Skill{}, fmt.Errorf("stat skill %q: %w", parsed, err)
	}
	if !info.Mode().IsRegular() {
		return Skill{}, fmt.Errorf("skill %q is not a regular file", parsed)
	}
	if info.Size() > MaxBodyBytes {
		return Skill{}, fmt.Errorf("skill %q is %d bytes; at most %d are allowed", parsed, info.Size(), MaxBodyBytes)
	}

	file, err := source.root.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		return Skill{}, fmt.Errorf("open skill %q: %w", parsed, err)
	}
	defer func() { _ = file.Close() }()

	// The opened inode must be the one just inspected. Root.OpenFile confines
	// the path, and this identity check refuses a leaf swapped for a symlink
	// between the stat and the open instead of following it.
	opened, err := file.Stat()
	if err != nil {
		return Skill{}, fmt.Errorf("stat open skill %q: %w", parsed, err)
	}
	if !opened.Mode().IsRegular() {
		return Skill{}, fmt.Errorf("skill %q is not a regular file", parsed)
	}
	if !os.SameFile(info, opened) {
		return Skill{}, fmt.Errorf("skill %q changed while it was opened", parsed)
	}

	body, err := io.ReadAll(io.LimitReader(file, MaxBodyBytes+1))
	if err != nil {
		return Skill{}, fmt.Errorf("read skill %q: %w", parsed, err)
	}
	if len(body) > MaxBodyBytes {
		return Skill{}, fmt.Errorf("skill %q is larger than %d bytes", parsed, MaxBodyBytes)
	}
	return Skill{Name: parsed, Path: string(parsed) + ".md", Body: body}, nil
}

// List reports the names of regular .md files whose stem parses as a Name. It
// enumerates through the pinned root, never follows a symlink and never
// descends into a directory.
func (source *DirSource) List(ctx context.Context) ([]Name, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := source.root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("read skill root %q: %w", source.path, err)
	}
	defer func() { _ = directory.Close() }()

	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read skill root %q: %w", source.path, err)
	}
	var names []Name
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Lstat through the root is authoritative even when the directory
		// entry type is unknown, and it never follows a symlink.
		info, err := source.root.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		fileName := entry.Name()
		if !strings.HasSuffix(fileName, ".md") {
			continue
		}
		name, err := ParseName(strings.TrimSuffix(fileName, ".md"))
		if err != nil {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

var _ Source = (*DirSource)(nil)
