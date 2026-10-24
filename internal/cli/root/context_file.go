package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	maxContextFileBytes = 1 << 20
	defaultLockWait     = 3 * time.Second
)

// DefaultContextPath is <user config dir>/kodama/contexts.json.
func DefaultContextPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user configuration directory: %w", err)
	}
	return filepath.Join(dir, "kodama", "contexts.json"), nil
}

// FileContextStore persists contexts in one JSON file. The file holds no
// credentials. Writes go to a temporary file in the same directory that is
// flushed and renamed over the target, so a crash leaves the previous complete
// file. Read-modify-write is serialized across processes by an exclusive lock
// file; contention is reported, never merged over, and an abandoned lock is
// never taken over automatically.
type FileContextStore struct {
	path     string
	lockWait time.Duration
	rename   func(oldpath, newpath string) error // replaceable in tests
}

var _ ContextStore = (*FileContextStore)(nil)

// NewFileContextStore stores contexts at path.
func NewFileContextStore(path string) *FileContextStore {
	return &FileContextStore{path: path, lockWait: defaultLockWait, rename: os.Rename}
}

// WithLockWait sets how long Update waits for a held lock before failing.
func (s *FileContextStore) WithLockWait(d time.Duration) *FileContextStore {
	s.lockWait = d
	return s
}

// Path returns the file location.
func (s *FileContextStore) Path() string { return s.path }

func (s *FileContextStore) lockPath() string { return s.path + ".lock" }

// Load reads the complete current state; a missing file is an empty set.
func (s *FileContextStore) Load(ctx context.Context) (ContextSet, error) {
	if err := ctx.Err(); err != nil {
		return ContextSet{}, err
	}
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewContextSet(), nil
	}
	if err != nil {
		return ContextSet{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxContextFileBytes+1))
	if err != nil {
		return ContextSet{}, err
	}
	if len(b) > maxContextFileBytes {
		return ContextSet{}, fmt.Errorf("context file %s is too large", s.path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var set ContextSet
	if err := dec.Decode(&set); err != nil {
		return ContextSet{}, fmt.Errorf("context file %s is not valid (it was not modified): %w", s.path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return ContextSet{}, fmt.Errorf("context file %s has trailing data (it was not modified)", s.path)
	}
	if set.Contexts == nil {
		set.Contexts = map[string]Context{}
	}
	if err := set.Validate(); err != nil {
		return ContextSet{}, fmt.Errorf("context file %s is not valid (it was not modified): %w", s.path, err)
	}
	return set, nil
}

// Update applies fn to the latest complete state and replaces the file
// atomically. If fn or any write step fails, the previous file is untouched.
func (s *FileContextStore) Update(ctx context.Context, fn func(*ContextSet) error) error {
	if err := ctx.Err(); err != nil { // cancelled before any side effect
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	set, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if err := fn(&set); err != nil {
		return err
	}
	if err := set.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	return s.replace(ctx, append(b, '\n'))
}

func (s *FileContextStore) replace(ctx context.Context, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".contexts-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Last point of no return: cancellation is honoured only before the
	// rename; after it the new file is committed and success is reported.
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.rename(name, s.path); err != nil {
		return err
	}
	if d, derr := os.Open(filepath.Dir(s.path)); derr == nil { // best-effort directory flush
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// lock takes the exclusive lock file, polling until lockWait elapses.
func (s *FileContextStore) lock(ctx context.Context) (func(), error) {
	deadline := time.Now().Add(s.lockWait)
	for {
		f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(s.lockPath()) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s exists; if no other kodama command is running, remove it and retry", ErrContextLocked, s.lockPath())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
