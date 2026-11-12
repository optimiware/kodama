package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileJournal is the durable filesystem receive journal. It stores exactly one
// JSON file per stable source-event ID directly under its directory and
// replaces it atomically. A state never regresses and unreadable bytes are a
// load error, never a silent missing entry.
type FileJournal struct {
	directory string
	mutex     sync.Mutex
}

// NewFileJournal creates the journal directory and returns its store.
func NewFileJournal(directory string) (*FileJournal, error) {
	if directory == "" {
		return nil, fmt.Errorf("journal directory is empty")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve journal directory %q: %w", directory, err)
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	return &FileJournal{directory: absolute}, nil
}

// Load returns the latest durable entry for the stable source-event ID. A
// missing entry is (zero, false, nil); unreadable or invalid bytes are an
// error.
func (journal *FileJournal) Load(ctx context.Context, key string) (JournalEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return JournalEntry{}, false, err
	}
	if !validJournalID(key) {
		return JournalEntry{}, false, fmt.Errorf("journal key %q is invalid", key)
	}
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	return journal.loadLocked(key)
}

// Record validates the entry and atomically replaces its durable state. A stale
// or illegal transition is an error and leaves the durable state unchanged.
func (journal *FileJournal) Record(ctx context.Context, entry JournalEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	journal.mutex.Lock()
	defer journal.mutex.Unlock()

	existing, ok, err := journal.loadLocked(entry.SourceEventID)
	if err != nil {
		return err
	}
	if ok {
		if !sameJournalIdentity(existing, entry) {
			return fmt.Errorf("journal entry %q changed its immutable identity", entry.SourceEventID)
		}
		if !journalTransitionAllowed(existing.State, entry.State) {
			return fmt.Errorf("journal entry %q cannot move from %q to %q", entry.SourceEventID, existing.State, entry.State)
		}
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode journal entry %q: %w", entry.SourceEventID, err)
	}
	return journal.writeLocked(entry.SourceEventID, data)
}

// loadLocked reads one durable entry with the journal mutex held.
func (journal *FileJournal) loadLocked(key string) (JournalEntry, bool, error) {
	data, err := os.ReadFile(journal.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return JournalEntry{}, false, nil
		}
		return JournalEntry{}, false, fmt.Errorf("read journal entry %q: %w", key, err)
	}
	var entry JournalEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return JournalEntry{}, false, fmt.Errorf("decode journal entry %q: %w", key, err)
	}
	if err := entry.Validate(); err != nil {
		return JournalEntry{}, false, fmt.Errorf("journal entry %q: %w", key, err)
	}
	if entry.SourceEventID != key {
		return JournalEntry{}, false, fmt.Errorf("journal entry %q carries source event %q", key, entry.SourceEventID)
	}
	return entry, true, nil
}

// writeLocked writes the entry bytes through a temporary file and an atomic
// rename, so a reader never observes a partial entry.
func (journal *FileJournal) writeLocked(key string, data []byte) error {
	temporary, err := os.CreateTemp(journal.directory, ".journal-*.tmp")
	if err != nil {
		return fmt.Errorf("create journal temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write journal temporary file: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set journal entry mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync journal temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close journal temporary file: %w", err)
	}
	if err := os.Rename(name, journal.path(key)); err != nil {
		return fmt.Errorf("replace journal entry %q: %w", key, err)
	}
	if err := syncJournalDirectory(journal.directory); err != nil {
		return fmt.Errorf("sync journal directory: %w", err)
	}
	return nil
}

// syncJournalDirectory fsyncs the journal directory so a renamed or removed
// entry survives a crash.
func syncJournalDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// PruneTerminalEntry removes one terminal (reported or refused) journal file
// whose recorded timestamp is strictly before cutoff. Non-terminal, unreadable,
// missing or newer entries are left untouched.
func (journal *FileJournal) PruneTerminalEntry(ctx context.Context, key string, cutoff time.Time) (bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	if cutoff.IsZero() || !validJournalID(key) {
		return false, nil
	}
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	removed, err := journal.pruneKeyLocked(key, cutoff)
	if err != nil || !removed {
		return false, err
	}
	if err := syncJournalDirectory(journal.directory); err != nil {
		return true, fmt.Errorf("sync journal directory: %w", err)
	}
	return true, nil
}

// PruneTerminal scans the journal directory and removes terminal (reported or
// refused) journal files whose recorded timestamp is strictly before cutoff.
// Corrupt or non-terminal entries are never deleted.
func (journal *FileJournal) PruneTerminal(ctx context.Context, cutoff time.Time) (int, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	if cutoff.IsZero() {
		return 0, nil
	}
	entries, err := os.ReadDir(journal.directory)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, dirEntry := range entries {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return pruned, err
			}
		}
		if dirEntry.IsDir() {
			continue
		}
		key, ok := strings.CutSuffix(dirEntry.Name(), ".json")
		if !ok || !validJournalID(key) {
			continue
		}
		journal.mutex.Lock()
		removed, removeErr := journal.pruneKeyLocked(key, cutoff)
		journal.mutex.Unlock()
		if removeErr != nil {
			return pruned, removeErr
		}
		if removed {
			pruned++
		}
	}
	if pruned > 0 {
		journal.mutex.Lock()
		syncErr := syncJournalDirectory(journal.directory)
		journal.mutex.Unlock()
		if syncErr != nil {
			return pruned, fmt.Errorf("sync journal directory: %w", syncErr)
		}
	}
	return pruned, nil
}

func (journal *FileJournal) pruneKeyLocked(key string, cutoff time.Time) (bool, error) {
	entry, ok, err := journal.loadLocked(key)
	if err != nil || !ok {
		return false, nil
	}
	if entry.State != JournalReported && entry.State != JournalRefused {
		return false, nil
	}
	if entry.At.IsZero() || !entry.At.Before(cutoff) {
		return false, nil
	}
	if err := os.Remove(journal.path(key)); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("remove pruned journal entry %q: %w", key, err)
	}
	return true, nil
}

func (journal *FileJournal) path(key string) string {
	return filepath.Join(journal.directory, key+".json")
}

// journalTransitionAllowed reports whether a durable state may advance to the
// next state. A state never regresses; repeating the same state is idempotent.
func journalTransitionAllowed(from, to JournalState) bool {
	if from == to {
		return from.Valid()
	}
	switch from {
	case JournalIntended:
		return to == JournalAccepted || to == JournalRefused || to == JournalUncertain
	case JournalUncertain:
		return to == JournalAccepted || to == JournalRefused
	case JournalAccepted:
		return to == JournalReported
	default:
		// reported and refused are terminal.
		return false
	}
}

// sameJournalIdentity reports whether two entries describe the same attempt.
func sameJournalIdentity(first, second JournalEntry) bool {
	return first.ResourceID == second.ResourceID &&
		first.Repository == second.Repository &&
		first.Ref == second.Ref &&
		first.ExpectedBase == second.ExpectedBase &&
		first.Candidate == second.Candidate &&
		first.SourceEventID == second.SourceEventID &&
		first.VerificationID == second.VerificationID
}
