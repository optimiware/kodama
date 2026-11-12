//go:build integration

package recovery_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// nativeRejectionMessage mirrors the product's single sanitized diagnostic.
const nativeRejectionMessage = "kodama git: update rejected"

func newHarness(t *testing.T, options recoveryfix.Options) *recoveryfix.Harness {
	t.Helper()
	if options.BinaryPath == "" {
		options.BinaryPath = kodamaBinary
	}
	harness, err := recoveryfix.NewHarness(t, options)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return harness
}

// seedMain creates shop/web with one commit on refs/heads/main.
func seedMain(t *testing.T, harness *recoveryfix.Harness) (string, string) {
	t.Helper()
	harness.InitBareRepository(t, recoveryfix.Repository)
	harness.InstallProductHooks(t, recoveryfix.Repository)
	directory := filepath.Join(harness.Root, "seed-clone")
	if stderr, code := harness.Clone(t, recoveryfix.ExecutionToken, recoveryfix.Repository, directory); code != 0 {
		t.Fatalf("seed clone: %s", stderr)
	}
	commit := harness.Commit(t, directory, "initial")
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("seed push: %s", stderr)
	}
	return directory, commit
}

func cloneFor(t *testing.T, harness *recoveryfix.Harness, token, name string) string {
	t.Helper()
	directory := filepath.Join(harness.Root, name)
	if stderr, code := harness.Clone(t, token, recoveryfix.Repository, directory); code != 0 {
		t.Fatalf("clone %s: %s", name, stderr)
	}
	return directory
}

func requireSuccess(t *testing.T, stderr string, code int) {
	t.Helper()
	if code != 0 {
		t.Fatalf("command failed with %d: %s", code, stderr)
	}
}

func requireFailure(t *testing.T, stderr string, code int) {
	t.Helper()
	if code == 0 {
		t.Fatalf("command unexpectedly succeeded: %s", stderr)
	}
}

func requireRejectionMessage(t *testing.T, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, nativeRejectionMessage) {
		t.Fatalf("stderr = %q; want %q", stderr, nativeRejectionMessage)
	}
}

func entryForCandidate(t *testing.T, harness *recoveryfix.Harness, ref, candidate string) *git.JournalEntry {
	t.Helper()
	for _, entry := range harness.JournalEntriesFor(t, ref) {
		if entry.Candidate == candidate {
			copied := entry
			return &copied
		}
	}
	return nil
}

func mustEntry(t *testing.T, harness *recoveryfix.Harness, sourceEventID string) git.JournalEntry {
	t.Helper()
	for _, entry := range harness.JournalEntries(t) {
		if entry.SourceEventID == sourceEventID {
			return entry
		}
	}
	t.Fatalf("no journal entry for source event %q", sourceEventID)
	return git.JournalEntry{}
}

// rawJournalFields decodes one durable journal file without the product's
// JournalEntry struct or Validate, so a test can assert the exact persisted
// state and identity rather than the product's in-memory view.
func rawJournalFields(t *testing.T, harness *recoveryfix.Harness, sourceEventID string) map[string]any {
	t.Helper()
	raw := harness.JournalRaw(t)
	data, ok := raw[sourceEventID+".json"]
	if !ok {
		t.Fatalf("no raw journal file for source event %q", sourceEventID)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("decode raw journal %q: %v", sourceEventID, err)
	}
	return decoded
}

// rawJournalState returns the exact persisted `state` value of one journal
// entry.
func rawJournalState(t *testing.T, harness *recoveryfix.Harness, sourceEventID string) string {
	t.Helper()
	state, _ := rawJournalFields(t, harness, sourceEventID)["state"].(string)
	if state == "" {
		t.Fatalf("raw journal %q carries no state", sourceEventID)
	}
	return state
}

// readFifo opens a named pipe without blocking and reads the first bytes a
// writer sends. It is an explicit barrier, never a fixed sleep.
func readFifo(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var reader *os.File
	for {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			reader = file
			break
		}
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("open fifo %q: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for fifo %q", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer reader.Close()
	buffer := make([]byte, 64)
	for {
		read, err := reader.Read(buffer)
		if err == nil && read > 0 {
			return string(buffer[:read])
		}
		// A nonblocking read on a FIFO with no writer yields EOF or EAGAIN.
		// Both mean "no data yet"; keep waiting for the explicit barrier.
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, io.EOF) {
			t.Fatalf("read fifo %q: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out reading fifo %q", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// postReceiveBarrierScript writes a test-only post-receive wrapper that
// announces itself, waits for the test to release it, then runs the real
// product post-receive hook. The ref has already been applied when this script
// runs.
func postReceiveBarrierScript(harness *recoveryfix.Harness, barrierPath, ackPath, donePath string) string {
	return "#!/bin/sh\n" +
		"printf 'ready\\n' > " + shellQuote(barrierPath) + "\n" +
		"read ack < " + shellQuote(ackPath) + "\n" +
		shellQuote(harness.BinaryPath) + " git hook post-receive\n" +
		"code=$?\n" +
		"printf '%s\\n' \"$code\" > " + shellQuote(donePath) + "\n" +
		"exit $code\n"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// waitForReports waits until the contract fixture has recorded want report
// requests. It is a bounded condition poll used when a request may still be in
// flight after the client saw a timeout.
func waitForReports(t *testing.T, harness *recoveryfix.Harness, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := harness.CountReports(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource.report calls = %d; want %d", harness.CountReports(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recoverEntry drives one recovery pass on the current app instance.
func recoverEntry(t *testing.T, harness *recoveryfix.Harness, sourceEventID string) git.RecoveryResult {
	t.Helper()
	result, err := harness.Service().Recover(context.Background(), sourceEventID)
	if err != nil {
		t.Fatalf("Recover(%q): %v", sourceEventID, err)
	}
	return result
}
