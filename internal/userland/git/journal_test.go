package git_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/optimiware/kodama/internal/userland/git"
)

func sampleEntry(state git.JournalState) git.JournalEntry {
	return git.JournalEntry{
		Version:        git.JournalVersion,
		ResourceID:     testResourceID,
		Repository:     "shop/web",
		Ref:            testRef,
		ExpectedBase:   testBase,
		Candidate:      testCandidate,
		SourceEventID:  testSourceEventID,
		VerificationID: testVerificationID,
		State:          state,
		At:             testNow,
	}
}

func journalFiles(t *testing.T, directory string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk journal directory: %v", err)
	}
	return files
}

// TestFileJournalDurableStates pins the durable journal format: one entry per
// stable source-event ID survives a store restart, and each accepted state
// replaces the previous durable state atomically.
func TestFileJournalDurableStates(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()

	intended := sampleEntry(git.JournalIntended)
	if err := store.Record(ctx, intended); err != nil {
		t.Fatalf("record intended: %v", err)
	}
	loaded, ok, err := store.Load(ctx, intended.SourceEventID)
	if err != nil || !ok {
		t.Fatalf("load intended = %v,%v", ok, err)
	}
	if loaded.State != git.JournalIntended {
		t.Fatalf("loaded state = %q; want intended", loaded.State)
	}

	accepted := intended
	accepted.State = git.JournalAccepted
	if err := store.Record(ctx, accepted); err != nil {
		t.Fatalf("record accepted: %v", err)
	}

	// A restart reads the latest durable state, not the first record.
	reopened, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	loaded, ok, err = reopened.Load(ctx, intended.SourceEventID)
	if err != nil || !ok || loaded.State != git.JournalAccepted {
		t.Fatalf("reopened load = %q,%v,%v; want accepted", loaded.State, ok, err)
	}

	reported := accepted
	reported.State = git.JournalReported
	if err := reopened.Record(ctx, reported); err != nil {
		t.Fatalf("record reported: %v", err)
	}
	loaded, ok, err = reopened.Load(ctx, intended.SourceEventID)
	if err != nil || !ok || loaded.State != git.JournalReported {
		t.Fatalf("final load = %q,%v,%v; want reported", loaded.State, ok, err)
	}

	// One file per key, no leftover temporary files.
	files := journalFiles(t, directory)
	if len(files) != 1 {
		t.Fatalf("journal files = %v; want exactly one", files)
	}
	for _, path := range files {
		if strings.Contains(strings.ToLower(filepath.Base(path)), ".tmp") {
			t.Fatalf("journal left a temporary file %q", path)
		}
	}

	if _, ok, err := store.Load(ctx, "git-event-missing"); err != nil || ok {
		t.Fatalf("load missing = %v,%v; want false,nil", ok, err)
	}
}

// TestFileJournalRefusesStaleTransitions pins that a durable state never
// regresses: a late intent cannot overwrite an acceptance, a reported entry
// cannot return to accepted, and a refusal is terminal.
func TestFileJournalRefusesStaleTransitions(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()

	entry := sampleEntry(git.JournalIntended)
	if err := store.Record(ctx, entry); err != nil {
		t.Fatalf("record intended: %v", err)
	}

	// intended may advance to uncertain, accepted or refused.
	uncertain := entry
	uncertain.State = git.JournalUncertain
	if err := store.Record(ctx, uncertain); err != nil {
		t.Fatalf("intended -> uncertain refused: %v", err)
	}
	if err := store.Record(ctx, uncertain); err != nil {
		t.Fatalf("re-recording uncertain refused: %v", err)
	}

	accepted := uncertain
	accepted.State = git.JournalAccepted
	if err := store.Record(ctx, accepted); err != nil {
		t.Fatalf("uncertain -> accepted refused: %v", err)
	}

	// A stale intent or uncertainty must not overwrite acceptance.
	for _, stale := range []git.JournalState{git.JournalIntended, git.JournalUncertain} {
		candidate := accepted
		candidate.State = stale
		if err := store.Record(ctx, candidate); err == nil {
			t.Fatalf("accepted -> %q was accepted", stale)
		}
	}
	if loaded, _, err := store.Load(ctx, entry.SourceEventID); err != nil || loaded.State != git.JournalAccepted {
		t.Fatalf("state after stale writes = %q,%v; want accepted", loaded.State, err)
	}

	reported := accepted
	reported.State = git.JournalReported
	if err := store.Record(ctx, reported); err != nil {
		t.Fatalf("accepted -> reported refused: %v", err)
	}
	if err := store.Record(ctx, reported); err != nil {
		t.Fatalf("re-recording reported refused: %v", err)
	}
	if err := store.Record(ctx, accepted); err == nil {
		t.Fatal("reported -> accepted was accepted")
	}
	if err := store.Record(ctx, entry); err == nil {
		t.Fatal("reported -> intended was accepted")
	}

	refusedDirectory := t.TempDir()
	refusedStore, err := git.NewFileJournal(refusedDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	refused := sampleEntry(git.JournalRefused)
	if err := refusedStore.Record(ctx, refused); err != nil {
		t.Fatalf("record refused: %v", err)
	}
	for _, stale := range []git.JournalState{git.JournalIntended, git.JournalUncertain, git.JournalAccepted} {
		candidate := refused
		candidate.State = stale
		if err := refusedStore.Record(ctx, candidate); err == nil {
			t.Fatalf("refused -> %q was accepted", stale)
		}
	}
}

// TestFileJournalRefusesInvalidEntries pins fail-closed storage: an entry that
// the journal format refuses is never written and a later load stays absent.
func TestFileJournalRefusesInvalidEntries(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()

	invalid := []git.JournalEntry{
		func() git.JournalEntry { entry := sampleEntry(git.JournalIntended); entry.Version = 0; return entry }(),
		func() git.JournalEntry {
			entry := sampleEntry(git.JournalIntended)
			entry.SourceEventID = ""
			return entry
		}(),
		func() git.JournalEntry {
			entry := sampleEntry(git.JournalIntended)
			entry.Repository = "../escape"
			return entry
		}(),
		func() git.JournalEntry { entry := sampleEntry(git.JournalIntended); entry.Candidate = ""; return entry }(),
		func() git.JournalEntry { entry := sampleEntry(git.JournalIntended); entry.State = "done"; return entry }(),
		func() git.JournalEntry {
			entry := sampleEntry(git.JournalIntended)
			entry.At = time.Time{}
			return entry
		}(),
	}
	for index, entry := range invalid {
		if err := store.Record(ctx, entry); err == nil {
			t.Errorf("invalid entry %d was accepted", index)
		}
	}
	if files := journalFiles(t, directory); len(files) != 0 {
		t.Fatalf("invalid entries created files: %v", files)
	}
	if _, ok, err := store.Load(ctx, testSourceEventID); err != nil || ok {
		t.Fatalf("load after invalid writes = %v,%v; want false,nil", ok, err)
	}

	if _, err := git.NewFileJournal(""); err == nil {
		t.Fatal("NewFileJournal accepted an empty directory")
	}
}

// TestFileJournalDetectsCorruption pins that unreadable durable state is an
// error, never a silent "no entry": recovery must not mistake corruption for a
// never-attempted effect.
func TestFileJournalDetectsCorruption(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()
	entry := sampleEntry(git.JournalIntended)
	if err := store.Record(ctx, entry); err != nil {
		t.Fatalf("record: %v", err)
	}
	files := journalFiles(t, directory)
	if len(files) != 1 {
		t.Fatalf("journal files = %v; want one", files)
	}
	if err := os.WriteFile(files[0], []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt journal file: %v", err)
	}
	if _, ok, err := store.Load(ctx, entry.SourceEventID); err == nil {
		t.Fatalf("Load of corrupt entry = %v,%v; want an error", ok, err)
	}
	reopened, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, _, err := reopened.Load(ctx, entry.SourceEventID); err == nil {
		t.Fatal("reopened store silently ignored corruption")
	}
}

// TestFileJournalNeverStoresCredentials pins that journal bytes contain only
// the declared entry fields and no actor credential or token material.
func TestFileJournalNeverStoresCredentials(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()
	entry := sampleEntry(git.JournalAccepted)
	if err := store.Record(ctx, entry); err != nil {
		t.Fatalf("record: %v", err)
	}
	files := journalFiles(t, directory)
	if len(files) != 1 {
		t.Fatalf("journal files = %v; want one", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read journal file: %v", err)
	}
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"credential", "password", "secret", "token", testCredential} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Fatalf("journal bytes contain %q: %s", forbidden, data)
		}
	}
}

// TestFileJournalIsolatesKeysAndConcurrentWriters pins that distinct stable
// source events never overwrite each other, including under concurrent writes.
func TestFileJournalIsolatesKeysAndConcurrentWriters(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()

	const writers = 16
	var wait sync.WaitGroup
	wait.Add(writers)
	errs := make(chan error, writers)
	for index := range writers {
		go func(index int) {
			defer wait.Done()
			entry := sampleEntry(git.JournalIntended)
			entry.SourceEventID = "git-event-" + strconv.Itoa(index)
			entry.Candidate = "cand-" + strconv.Itoa(index)
			if err := store.Record(ctx, entry); err != nil {
				errs <- err
				return
			}
			entry.State = git.JournalAccepted
			if err := store.Record(ctx, entry); err != nil {
				errs <- err
				return
			}
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent record: %v", err)
	}
	for index := range writers {
		key := "git-event-" + strconv.Itoa(index)
		loaded, ok, err := store.Load(ctx, key)
		if err != nil || !ok {
			t.Fatalf("load %q = %v,%v", key, ok, err)
		}
		if loaded.State != git.JournalAccepted || loaded.Candidate != "cand-"+strconv.Itoa(index) {
			t.Fatalf("load %q = %+v", key, loaded)
		}
	}
	if files := journalFiles(t, directory); len(files) != writers {
		t.Fatalf("journal files = %d; want %d", len(files), writers)
	}
}

func TestFileJournalPrunesOnlyExpiredTerminalEntries(t *testing.T) {
	directory := t.TempDir()
	store, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	ctx := context.Background()
	oldAt := testNow.Add(-25 * time.Hour)
	recentAt := testNow.Add(-time.Hour)

	records := []struct {
		id       string
		state    git.JournalState
		at       time.Time
		survives bool
	}{
		{"prune-old-reported", git.JournalReported, oldAt, false},
		{"prune-old-refused", git.JournalRefused, oldAt, false},
		{"prune-recent-reported", git.JournalReported, recentAt, true},
		{"prune-recent-refused", git.JournalRefused, recentAt, true},
		{"prune-old-intended", git.JournalIntended, oldAt, true},
		{"prune-old-accepted", git.JournalAccepted, oldAt, true},
		{"prune-old-uncertain", git.JournalUncertain, oldAt, true},
	}
	for _, item := range records {
		entry := sampleEntry(item.state)
		entry.SourceEventID = item.id
		entry.At = item.at
		if err := store.Record(ctx, entry); err != nil {
			t.Fatalf("record %s: %v", item.id, err)
		}
	}

	pruned, err := store.PruneTerminal(ctx, testNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d; want 2", pruned)
	}
	for _, item := range records {
		_, ok, loadErr := store.Load(ctx, item.id)
		if loadErr != nil {
			t.Fatalf("load %s: %v", item.id, loadErr)
		}
		if ok != item.survives {
			t.Fatalf("entry %s present = %v; want %v", item.id, ok, item.survives)
		}
	}
}
