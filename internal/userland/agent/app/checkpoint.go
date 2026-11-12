// Package app wires the native agent and durable checkpoint boundaries. It is
// composition glue only: it starts no process, opens no network connection of
// its own and holds no planning or tool logic.
//
// The package is a Userland app process boundary. It may import only neutral
// packages, the public API/client contracts and other Userland packages; it
// never imports Shell, Kernel or installation code.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/session"
)

// ErrCorruptCheckpoint reports unreadable or inconsistent checkpoint bytes. It
// is never reported as a missing checkpoint.
var ErrCorruptCheckpoint = errors.New("checkpoint is corrupt")

// StorageConfig configures the durable checkpoint file store.
type StorageConfig struct {
	// Root is the owner-only checkpoint directory.
	Root string
	// Now is the injected time boundary. A nil value uses time.Now.
	Now func() time.Time
}

// CheckpointStorage implements session.Storage over a durable, owner-only
// directory. Records are written through a temporary file, fsynced and renamed
// so a reader never observes a partial checkpoint. CompareAndSwap serializes
// one session at a time and commits only when the stored head revision equals
// the expected revision.
type CheckpointStorage struct {
	root string
	now  func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// beforeRename is a test seam that simulates a crash between writing the
	// temporary file and publishing it.
	beforeRename func() error
}

var _ session.Storage = (*CheckpointStorage)(nil)

// NewCheckpointStorage creates the owner-only checkpoint directory.
func NewCheckpointStorage(config StorageConfig) (*CheckpointStorage, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, fmt.Errorf("checkpoint root is empty")
	}
	absolute, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint root %q: %w", config.Root, err)
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint root: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("restrict checkpoint root permissions: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &CheckpointStorage{root: absolute, now: now, locks: make(map[string]*sync.Mutex)}, nil
}

// Root returns the absolute checkpoint directory.
func (storage *CheckpointStorage) Root() string { return storage.root }

// Read returns one immutable checkpoint revision.
func (storage *CheckpointStorage) Read(ctx context.Context, ref session.Ref) (session.Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return session.Record{}, err
	}
	key, revision, ok := parseCheckpointRef(ref)
	if !ok {
		return session.Record{}, missing(ref)
	}
	path := filepath.Join(storage.root, recordName(key, revision))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return session.Record{}, missing(ref)
		}
		return session.Record{}, fmt.Errorf("stat checkpoint %q: %w", ref, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return session.Record{}, fmt.Errorf("%w: %q is not owner-only", ErrCorruptCheckpoint, ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return session.Record{}, missing(ref)
		}
		return session.Record{}, fmt.Errorf("read checkpoint %q: %w", ref, err)
	}
	var record session.Record
	if err := json.Unmarshal(data, &record); err != nil {
		return session.Record{}, fmt.Errorf("%w: decode %q: %v", ErrCorruptCheckpoint, ref, err)
	}
	if err := record.Validate(); err != nil {
		return session.Record{}, fmt.Errorf("%w: %q: %v", ErrCorruptCheckpoint, ref, err)
	}
	if checkpointKey(record.SessionID) != key {
		return session.Record{}, fmt.Errorf("%w: %q names a different session", ErrCorruptCheckpoint, ref)
	}
	if record.Revision != revision {
		return session.Record{}, fmt.Errorf("%w: %q carries revision %d", ErrCorruptCheckpoint, ref, record.Revision)
	}
	return record, nil
}

// CompareAndSwap commits record as revision expectedRevision+1 of its session
// stream when the stored head revision equals expectedRevision.
func (storage *CheckpointStorage) CompareAndSwap(ctx context.Context, record session.Record, expectedRevision uint64) (session.Ref, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := record.Validate(); err != nil {
		return "", err
	}
	if record.SessionID == "" {
		return "", fmt.Errorf("checkpoint record has no session id")
	}
	if expectedRevision == ^uint64(0) {
		return "", fmt.Errorf("checkpoint revision cannot be advanced")
	}
	key := checkpointKey(record.SessionID)

	unlock := storage.lock(key)
	defer unlock()

	head, err := storage.readHead(key)
	if err != nil {
		return "", err
	}
	if head != expectedRevision {
		return "", fault.New(fault.VersionConflict, "stale session revision")
	}

	committed := record
	committed.Revision = expectedRevision + 1
	if err := committed.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(committed)
	if err != nil {
		return "", fmt.Errorf("encode checkpoint: %w", err)
	}
	ref := checkpointRef(key, committed.Revision)
	if err := storage.writeAtomic(recordName(key, committed.Revision), data); err != nil {
		return "", err
	}
	headBytes, err := json.Marshal(checkpointHead{Revision: committed.Revision, Ref: ref})
	if err != nil {
		return "", fmt.Errorf("encode checkpoint head: %w", err)
	}
	if err := storage.writeAtomic(headName(key), headBytes); err != nil {
		return "", err
	}
	return ref, nil
}

// checkpointHead is the durable per-session head.
type checkpointHead struct {
	Revision uint64      `json:"revision"`
	Ref      session.Ref `json:"ref"`
}

// readHead returns the stored head revision, or zero when the session has no
// committed checkpoint yet.
func (storage *CheckpointStorage) readHead(key string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(storage.root, headName(key)))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read checkpoint head: %w", err)
	}
	var head checkpointHead
	if err := json.Unmarshal(data, &head); err != nil {
		return 0, fmt.Errorf("%w: decode checkpoint head: %v", ErrCorruptCheckpoint, err)
	}
	if _, err := session.ParseRef(string(head.Ref)); err != nil {
		return 0, fmt.Errorf("%w: checkpoint head reference: %v", ErrCorruptCheckpoint, err)
	}
	return head.Revision, nil
}

// writeAtomic writes data through a temporary file, fsync and rename.
func (storage *CheckpointStorage) writeAtomic(name string, data []byte) error {
	temporary, err := os.CreateTemp(storage.root, ".checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("create checkpoint temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("restrict checkpoint temporary permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write checkpoint temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync checkpoint temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close checkpoint temporary file: %w", err)
	}
	if storage.beforeRename != nil {
		if err := storage.beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryName, filepath.Join(storage.root, name)); err != nil {
		return fmt.Errorf("publish checkpoint file: %w", err)
	}
	return syncDirectory(storage.root)
}

// syncDirectory fsyncs one directory so a rename survives a crash.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open checkpoint directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync checkpoint directory: %w", err)
	}
	return nil
}

// lock serializes one session key.
func (storage *CheckpointStorage) lock(key string) func() {
	storage.mu.Lock()
	entry, ok := storage.locks[key]
	if !ok {
		entry = &sync.Mutex{}
		storage.locks[key] = entry
	}
	storage.mu.Unlock()
	entry.Lock()
	return entry.Unlock
}

// checkpointKey derives a bounded, path-safe session key.
func checkpointKey(id session.SessionID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

// checkpointRef derives the immutable reference of one revision.
func checkpointRef(key string, revision uint64) session.Ref {
	return session.Ref(session.RefPrefix + session.RefVersion + "/" + key + "-" + strconv.FormatUint(revision, 10))
}

// parseCheckpointRef parses this store's reference shape.
func parseCheckpointRef(ref session.Ref) (string, uint64, bool) {
	value := string(ref)
	prefix := session.RefPrefix + session.RefVersion + "/"
	if !strings.HasPrefix(value, prefix) {
		return "", 0, false
	}
	payload := strings.TrimPrefix(value, prefix)
	separator := strings.LastIndexByte(payload, '-')
	if separator < 0 {
		return "", 0, false
	}
	key := payload[:separator]
	revisionText := payload[separator+1:]
	if len(key) != 32 || revisionText == "" {
		return "", 0, false
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		if (character < 'a' || character > 'f') && (character < '0' || character > '9') {
			return "", 0, false
		}
	}
	if revisionText[0] == '0' || len(revisionText) > 20 {
		return "", 0, false
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, revision, true
}

func recordName(key string, revision uint64) string {
	return "record-" + key + "-" + strconv.FormatUint(revision, 10) + ".json"
}

func headName(key string) string {
	return "head-" + key + ".json"
}

func missing(ref session.Ref) error {
	return fmt.Errorf("checkpoint %q: %w", ref, session.ErrCheckpointMissing)
}
