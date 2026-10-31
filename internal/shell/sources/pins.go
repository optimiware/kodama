package sources

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var pinKeyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FilePins records pins as files in a host directory: the durable state a
// restarted engine reads to compute the same preparation digest. A pin is one
// small file named by its key, created exclusively, so the first writer wins
// across processes and a pin never changes once recorded.
type FilePins struct{ Dir string }

var _ Pins = FilePins{}

// Get returns the recorded commit of key.
func (f FilePins) Get(_ context.Context, key string) (string, bool, error) {
	if !pinKeyRE.MatchString(key) {
		return "", false, fmt.Errorf("sources: pin key %q", key)
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, key))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

// PutIfAbsent records commit under key unless one is recorded, and returns the
// recorded commit. The file appears complete or not at all (written aside, synced
// and linked into place).
func (f FilePins) PutIfAbsent(ctx context.Context, key, commit string) (string, error) {
	if !pinKeyRE.MatchString(key) {
		return "", fmt.Errorf("sources: pin key %q", key)
	}
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("sources: pin %q is not a full commit", commit)
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(f.Dir, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(commit + "\n")
	serr := tmp.Sync()
	if err := errors.Join(werr, serr, tmp.Close()); err != nil {
		return "", err
	}
	switch err := os.Link(tmp.Name(), filepath.Join(f.Dir, key)); {
	case err == nil:
		return commit, nil
	case errors.Is(err, fs.ErrExist):
		got, ok, gerr := f.Get(ctx, key)
		if gerr != nil || !ok {
			return "", fmt.Errorf("sources: pin %s: %w", key, errors.Join(gerr, fs.ErrNotExist))
		}
		return got, nil
	default:
		return "", err
	}
}
