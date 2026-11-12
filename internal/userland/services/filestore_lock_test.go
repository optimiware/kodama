package services

// Recovery: the cross-process coordination lock must belong to the live writer
// that holds its open file descriptor, not to a wall-clock age. Age-based
// reclamation lets a second writer steal a lock from a writer that is merely
// slow (or paused), so two writers can enter the same critical section. These
// tests pin two properties:
//
//   - a lock held by a live writer is never reclaimed, no matter how old its
//     file timestamp looks;
//   - a lock is released by process death, so a crashed writer cannot strand
//     the record.
//
// It also pins durability of the store directory: the record rename and the
// lock file's first creation are only durable once their directory entry is
// flushed, so a crash cannot revert the applied result or the in-flight marker
// and let a takeover repeat the merge.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The helper-process contract. The child test acquires the lock and blocks
// until the parent kills it or closes its stdin.
const (
	fileLockHelperEnv  = "KODAMA_FILE_LOCK_HELPER"
	fileLockHelperPath = "KODAMA_FILE_LOCK_PATH"
)

// TestFileLockIsNotReclaimedByAge proves that an old lock-file timestamp is not
// evidence that its writer died. The holder stays alive and its release decides
// when the second contender may enter.
func TestFileLockIsNotReclaimedByAge(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "record.lock")
	release, err := acquireFileLock(lockPath, nil)
	if err != nil {
		t.Fatalf("acquireFileLock: %v", err)
	}
	// A paused writer can leave an arbitrarily old timestamp behind. Reclaiming
	// on age alone would hand the lock to a second writer while the first one
	// still runs.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		release()
		t.Fatalf("age the lock file: %v", err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, err := acquireFileLock(lockPath, nil)
		if err != nil {
			return
		}
		acquired <- release
	}()

	select {
	case second := <-acquired:
		second()
		release()
		t.Fatal("a lock held by a live writer was reclaimed after only an age change")
	case <-time.After(250 * time.Millisecond):
	}

	// The barrier is the first holder's release, never a timeout: only now may
	// the second contender enter.
	release()
	select {
	case second := <-acquired:
		second()
	case <-time.After(5 * time.Second):
		t.Fatal("the second writer did not acquire the lock after the holder released it")
	}

	// The lock file itself is never unlinked: removing it would let a waiter
	// that already opened the old inode and a fresh contender believe they own
	// the same record.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file disappeared after release: %v", err)
	}
}

// TestFileLockIsReleasedByProcessDeath proves the lock is owned by the live
// file descriptor: a process that dies without releasing it does not strand the
// record.
func TestFileLockIsReleasedByProcessDeath(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "record.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestFileLockHelperProcess$")
	command.Env = append(os.Environ(), fileLockHelperEnv+"=1", fileLockHelperPath+"="+lockPath)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Wait()
	}()

	locked := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "locked" {
				close(locked)
				return
			}
		}
	}()
	select {
	case <-locked:
	case <-time.After(30 * time.Second):
		t.Fatal("helper process did not acquire the lock")
	}

	attempt := make(chan error, 1)
	go func() {
		release, err := acquireFileLock(lockPath, nil)
		if release != nil {
			release()
		}
		attempt <- err
	}()
	select {
	case err := <-attempt:
		t.Fatalf("the lock was released while its holder was still alive (acquisition error: %v)", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The holder dies abruptly. Its open descriptor closes, so the advisory
	// lock is released by the operating system even though no clean release
	// ran.
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill helper process: %v", err)
	}
	select {
	case err := <-attempt:
		if err != nil {
			t.Fatalf("acquiring the lock after the holder died: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was not released by process death")
	}
}

// TestFileLockHelperProcess is the child side of
// TestFileLockIsReleasedByProcessDeath. It is inert unless the parent marks the
// environment.
func TestFileLockHelperProcess(t *testing.T) {
	if os.Getenv(fileLockHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	path := os.Getenv(fileLockHelperPath)
	if path == "" {
		fmt.Fprintln(os.Stderr, "helper: missing lock path")
		os.Exit(2)
	}
	release, err := acquireFileLock(path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(2)
	}
	defer release()
	fmt.Println("locked")
	// Block until the parent kills this process or closes stdin.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// TestFileStoreLockSerializesBarrieredWriters pins the store-level consequence:
// two writers racing the same record produce two committed generations, never a
// lost update or a doubled claim.
func TestFileStoreLockSerializesBarrieredWriters(t *testing.T) {
	directory := t.TempDir()
	first, err := NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("first NewFileCoordinationStore: %v", err)
	}
	second, err := NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("second NewFileCoordinationStore: %v", err)
	}
	key := MergeKey{Repository: "shop/web", Ref: "refs/heads/work/merge-lock"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, store := range []*FileCoordinationStore{first, second} {
		go func(index int, store *FileCoordinationStore) {
			<-start
			_, claimErr := store.CreateClaim(context.Background(), "tenant-a", key, fmt.Sprintf("owner-%d", index), "op-1", time.Now().Add(time.Hour), time.Now())
			results <- claimErr
		}(index, store)
	}
	close(start)

	winners, refusals := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			winners++
		case errors.Is(err, ErrClaimed):
			refusals++
		default:
			t.Fatalf("CreateClaim error = %v; want a claim or ErrClaimed", err)
		}
	}
	if winners != 1 || refusals != 1 {
		t.Fatalf("barriered claims: %d winners, %d refused; want exactly one exclusive owner", winners, refusals)
	}
}

// TestFileStoreSyncsDirectoryEntries proves the store flushes the directory
// after publishing a renamed record and after creating the lock file for the
// first time. Without that flush a crash can revert the rename, so the
// in-flight marker or the applied result disappears and a takeover repeats the
// merge under a fresh operation identity.
func TestFileStoreSyncsDirectoryEntries(t *testing.T) {
	directory := t.TempDir()
	store, err := NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	key := MergeKey{Repository: "shop/web", Ref: "refs/heads/work/sync"}
	now := time.Now()

	type syncObservation struct {
		path string
		// records is how many published record files the directory held when
		// the flush ran. A lock-creation flush sees none; a rename flush sees
		// the record it just published.
		records int
	}
	var syncs []syncObservation
	store.syncDir = func(path string) error {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		count := 0
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
				count++
			}
		}
		syncs = append(syncs, syncObservation{path: path, records: count})
		return nil
	}

	created, err := store.CreateClaim(context.Background(), "tenant-a", key, "owner-a", "op-1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if len(syncs) < 2 {
		t.Fatalf("directory syncs = %d; want one for the new lock file and one for the published record", len(syncs))
	}
	if syncs[0].records != 0 {
		t.Fatalf("first directory sync saw %d published records; want the lock file's first creation before any record is renamed", syncs[0].records)
	}
	published := 0
	for _, observation := range syncs {
		if observation.path != store.directory {
			t.Fatalf("directory sync path = %q; want the store directory %q", observation.path, store.directory)
		}
		if observation.records > 0 {
			published++
		}
	}
	if published == 0 {
		t.Fatal("no directory sync ran after the renamed record was published")
	}

	// The lock file now exists, so a later write flushes only the record's
	// rename, never a first creation again.
	before := len(syncs)
	next := created
	next.UpdatedAt = now.Add(time.Second)
	if err := store.Transition(context.Background(), "tenant-a", created, next); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if len(syncs) == before {
		t.Fatal("Transition published a record without flushing the directory")
	}
	for _, observation := range syncs[before:] {
		if observation.records == 0 {
			t.Fatalf("an existing lock file was flushed as a first creation")
		}
	}
}

// TestFileStoreReturnsDirectorySyncFailure proves a failed directory flush after
// the rename is returned to the caller, which must treat the write as not
// durable rather than as a committed record.
func TestFileStoreReturnsDirectorySyncFailure(t *testing.T) {
	failure := errors.New("directory sync failed")
	key := MergeKey{Repository: "shop/web", Ref: "refs/heads/work/sync-failure"}
	now := time.Now()

	directory := t.TempDir()
	store, err := NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	created, err := store.CreateClaim(context.Background(), "tenant-a", key, "owner-a", "op-1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}

	// The lock file already exists, so this failure comes from the record's
	// directory flush after the rename.
	store.syncDir = func(string) error { return failure }
	next := created
	next.UpdatedAt = now.Add(time.Second)
	if err := store.Transition(context.Background(), "tenant-a", created, next); !errors.Is(err, failure) {
		t.Fatalf("Transition error = %v; want the directory sync failure", err)
	}

	t.Run("lock file creation", func(t *testing.T) {
		fresh, err := NewFileCoordinationStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewFileCoordinationStore: %v", err)
		}
		fresh.syncDir = func(string) error { return failure }
		if _, err := fresh.CreateClaim(context.Background(), "tenant-a", key, "owner-a", "op-1", now.Add(time.Minute), now); !errors.Is(err, failure) {
			t.Fatalf("CreateClaim error = %v; want the directory sync failure", err)
		}
	})
}
