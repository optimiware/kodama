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
	"strings"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

var snapshotKeyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxSnapshotBytes bounds a recorded input set when it is read back.
const maxSnapshotBytes = 1 << 20

// FileSnapshots records preparation snapshots as files in a host directory: the
// durable state a restarted engine reads to compute the same inputs. A snapshot is
// one file named by its key, written aside and synced, then linked into place
// exclusively: the first writer wins across processes and a record never changes.
//
// Durability is established, not assumed, on every path that hands a snapshot to
// a caller. Before a record is linked, and before one is returned by Get or by the
// race path of PutIfAbsent, the whole directory chain it hangs from is
// synchronized: each directory, and the entry that names it in its parent, from the
// snapshot directory up to Root (the filesystem root when Root is empty). A
// directory that already exists is not trusted, because the call that created it
// may have failed before its entry reached the disk, so a retry synchronizes the
// chain again. A failure to synchronize is returned and nothing is acknowledged; a
// record left linked by a failed publication is acknowledged only by a later call
// that synchronizes successfully.
type FileSnapshots struct {
	Dir string
	// Root bounds the chain that is synchronized: it and everything above it are
	// assumed durable, as an operator-provisioned state directory is. Empty means
	// the filesystem root. Dir must be inside it.
	Root string

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

// chain returns the directories to synchronize, deepest first: Dir, then each
// ancestor up to and including the top of the chain. Synchronizing a directory
// makes its own entries durable, so the list covers every entry from Dir's
// contents up to the entry that names the first directory under the top.
func (f FileSnapshots) chain() ([]string, error) {
	dir, err := filepath.Abs(f.Dir)
	if err != nil {
		return nil, err
	}
	top := string(filepath.Separator)
	if vol := filepath.VolumeName(dir); vol != "" {
		top = vol + top
	}
	if f.Root != "" {
		if top, err = filepath.Abs(f.Root); err != nil {
			return nil, err
		}
	}
	if rel, err := filepath.Rel(top, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("sources: snapshot directory %s is not inside %s", dir, top)
	}
	out := []string{dir}
	for d := dir; d != top; {
		parent := filepath.Dir(d)
		out = append(out, parent)
		if parent == d {
			break
		}
		d = parent
	}
	return out, nil
}

// ensureDurable synchronizes the whole chain, deepest first, and returns the
// first failure.
func (f FileSnapshots) ensureDurable() error {
	dirs, err := f.chain()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := f.sync(d); err != nil {
			return fmt.Errorf("sources: snapshot directory %s is not durable: %w", d, err)
		}
	}
	return nil
}

// Get returns the recorded snapshot of key. A record that is visible is returned
// only after the chain it hangs from has been synchronized, so a snapshot found
// beside a writer that has not finished (or whose sync failed) is never
// acknowledged before it is durable.
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
	if err := f.ensureDurable(); err != nil {
		return zero, false, err
	}
	var in runtime.PreparationInputs
	if err := json.Unmarshal(b, &in); err != nil {
		return zero, false, fmt.Errorf("sources: snapshot %s is unreadable", key)
	}
	return in, true, nil
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
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return zero, err
	}
	// Nothing is linked into a directory chain that is not durable, including
	// directories this call found already there.
	if err := f.ensureDurable(); err != nil {
		return zero, err
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
		// Acknowledged only once the directory entry survives a crash. On failure
		// the record stays linked and is acknowledged by whichever call (Get or
		// PutIfAbsent) next manages to synchronize.
		if err := f.sync(f.Dir); err != nil {
			return zero, fmt.Errorf("sources: snapshot %s is not durable: %w", key, err)
		}
		return in, nil
	case errors.Is(err, fs.ErrExist):
		got, ok, gerr := f.Get(ctx, key) // synchronizes before it returns
		if gerr != nil || !ok {
			return zero, fmt.Errorf("sources: snapshot %s: %w", key, errors.Join(gerr, fs.ErrNotExist))
		}
		return got, nil
	default:
		return zero, err
	}
}
