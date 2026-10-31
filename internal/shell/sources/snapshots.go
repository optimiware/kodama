package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

var snapshotKeyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxSnapshotBytes bounds a recorded input set when it is read back.
const maxSnapshotBytes = 1 << 20

// FileSnapshots records preparation snapshots as files in a host directory: the
// durable state a restarted engine reads to compute the same inputs. A snapshot is
// one file named by its key, written aside and synced, linked into place
// exclusively (the first writer wins across processes and a record never changes)
// and published only once the containing directory, and every directory created
// for it, has been synced. A failure to sync is returned, never acknowledged.
type FileSnapshots struct {
	Dir string

	// syncDir is os-level directory synchronization; tests replace it.
	syncDir func(string) error
}

var _ Snapshots = FileSnapshots{}

func (f FileSnapshots) sync(dir string) error {
	if f.syncDir != nil {
		return f.syncDir(dir)
	}
	return syncDirectory(dir)
}

// syncDirectory fsyncs a directory so the entries created in it survive a crash.
func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// Get returns the recorded snapshot of key.
func (f FileSnapshots) Get(_ context.Context, key string) (runtime.PreparationInputs, bool, error) {
	var zero runtime.PreparationInputs
	if !snapshotKeyRE.MatchString(key) {
		return zero, false, fmt.Errorf("sources: snapshot key %q", key)
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, key))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return zero, false, nil
	case err != nil:
		return zero, false, err
	case len(b) > maxSnapshotBytes:
		return zero, false, fmt.Errorf("sources: snapshot %s is too large", key)
	}
	var in runtime.PreparationInputs
	if err := json.Unmarshal(b, &in); err != nil {
		return zero, false, fmt.Errorf("sources: snapshot %s is unreadable", key)
	}
	return in, true, nil
}

// mkdirAll creates dir and every missing ancestor, syncing the parent of each
// directory it created, so the new entries are durable before anything is linked
// into them.
func (f FileSnapshots) mkdirAll(dir string) error {
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("sources: %s is not a directory", dir)
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := f.mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	// Sync the new directory itself, then its parent, which holds the new entry.
	return errors.Join(f.sync(dir), f.sync(parent))
}

// PutIfAbsent records in under key unless one is recorded and returns the
// recorded snapshot, durably.
func (f FileSnapshots) PutIfAbsent(ctx context.Context, key string, in runtime.PreparationInputs) (runtime.PreparationInputs, error) {
	var zero runtime.PreparationInputs
	if !snapshotKeyRE.MatchString(key) {
		return zero, fmt.Errorf("sources: snapshot key %q", key)
	}
	if err := in.Validate(); err != nil {
		return zero, err
	}
	doc, err := json.Marshal(in)
	if err != nil {
		return zero, err
	}
	if err := f.mkdirAll(f.Dir); err != nil {
		return zero, err
	}
	// A directory an earlier, crashed call created may not have reached its parent
	// on disk: make the entry for the snapshot directory durable too.
	if err := f.sync(filepath.Dir(filepath.Clean(f.Dir))); err != nil {
		return zero, fmt.Errorf("sources: snapshot directory is not durable: %w", err)
	}
	tmp, err := os.CreateTemp(f.Dir, ".tmp-")
	if err != nil {
		return zero, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(doc)
	serr := tmp.Sync()
	if err := errors.Join(werr, serr, tmp.Close()); err != nil {
		return zero, err
	}
	switch err := os.Link(tmp.Name(), filepath.Join(f.Dir, key)); {
	case err == nil:
		// Acknowledged only once the directory entry survives a crash.
		if err := f.sync(f.Dir); err != nil {
			return zero, fmt.Errorf("sources: snapshot %s is not durable: %w", key, err)
		}
		return in, nil
	case errors.Is(err, fs.ErrExist):
		// Another writer's record may have been linked and not yet synced when it
		// died: this call makes the directory entry durable before relying on it.
		if err := f.sync(f.Dir); err != nil {
			return zero, fmt.Errorf("sources: snapshot %s is not durable: %w", key, err)
		}
		got, ok, gerr := f.Get(ctx, key)
		if gerr != nil || !ok {
			return zero, fmt.Errorf("sources: snapshot %s: %w", key, errors.Join(gerr, fs.ErrNotExist))
		}
		return got, nil
	default:
		return zero, err
	}
}
