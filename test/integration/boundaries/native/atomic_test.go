//go:build integration

package native_test

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestNonFastForwardPushIsRefusedAndDoesNotMoveTheRef(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	seedMain(t, harness)

	loser := cloneFor(t, harness, nativefix.ExecutionToken, "stale-loser")
	loserCommit := harness.Commit(t, loser, "loser")
	winner := cloneFor(t, harness, nativefix.ExecutionToken, "stale-winner")
	winnerCommit := harness.Commit(t, winner, "winner")

	if stderr, code := harness.Push(t, winner, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("winner push: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != winnerCommit {
		t.Fatalf("remote main = %q; want %q", observed, winnerCommit)
	}

	// A forced update bypasses git's client-side fast-forward check, so the
	// server's ordered rules see the rewind and refuse it.
	if stderr, code := harness.Push(t, loser, nativefix.ExecutionToken, "--force", "origin", "HEAD:refs/heads/main"); code == 0 {
		t.Fatal("a non-fast-forward push was accepted")
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != winnerCommit {
		t.Fatalf("rejected push moved main to %q; want %q", observed, winnerCommit)
	}
	var rejected *git.JournalEntry
	for _, entry := range harness.JournalEntriesFor(t, "refs/heads/main") {
		if entry.Candidate == loserCommit {
			copied := entry
			rejected = &copied
		}
	}
	if rejected == nil {
		t.Fatal("the rejected push left no journal entry")
	}
	if rejected.State != git.JournalRefused {
		t.Fatalf("rejected journal state = %q; want refused", rejected.State)
	}
}

func TestMultiRefPushWithOneDisallowedRefRejectsTheWholeBatch(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	seedDirectory, first := seedMain(t, harness)
	candidate := harness.Commit(t, seedDirectory, "batch")

	for _, atomic := range []bool{false, true} {
		extra := []string{"origin", "HEAD:refs/heads/good", "HEAD:refs/heads/protected"}
		if atomic {
			extra = append([]string{"--atomic"}, extra...)
		}
		if stderr, code := harness.Push(t, seedDirectory, nativefix.ExecutionToken, extra...); code == 0 {
			t.Fatalf("atomic=%v batch was accepted: %s", atomic, stderr)
		} else {
			requireRejectionMessage(t, stderr)
		}
		if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/good"); observed != "" {
			t.Fatalf("atomic=%v created good = %q; want nothing applied", atomic, observed)
		}
		if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != first {
			t.Fatalf("atomic=%v moved main to %q; want %q", atomic, observed, first)
		}
		if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/protected"); observed != "" {
			t.Fatalf("atomic=%v created protected = %q", atomic, observed)
		}
		if candidate == first {
			t.Fatal("fixture commits are identical")
		}
	}
}

func TestAtomicPushWithARefusedRefAppliesNothing(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	_, first := seedMain(t, harness)
	clone := cloneFor(t, harness, nativefix.ExecutionToken, "atomic-stale")
	harness.Commit(t, clone, "advance")

	// Move main on the server, then push an atomic batch whose main update is
	// a denied rewind and whose new ref is otherwise allowed.
	other := cloneFor(t, harness, nativefix.ExecutionToken, "atomic-other")
	winner := harness.Commit(t, other, "other")
	if stderr, code := harness.Push(t, other, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("advance main: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, clone, "reset", "--hard", first); code != 0 {
		t.Fatalf("reset clone: %s", stderr)
	}
	if stderr, code := harness.Push(t, clone, nativefix.ExecutionToken, "--atomic", "--force", "origin",
		"HEAD:refs/heads/main", "HEAD:refs/heads/atomic-new"); code == 0 {
		t.Fatalf("atomic refused batch was accepted: %s", stderr)
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != winner {
		t.Fatalf("atomic refused batch moved main to %q; want %q", observed, winner)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/atomic-new"); observed != "" {
		t.Fatalf("atomic refused batch created atomic-new = %q", observed)
	}
}

func TestConcurrentPushesExactlyOneApplies(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	_, first := seedMain(t, harness)

	left := cloneFor(t, harness, nativefix.ExecutionToken, "race-left")
	leftCommit := harness.Commit(t, left, "left")
	right := cloneFor(t, harness, nativefix.ExecutionToken, "race-right")
	rightCommit := harness.Commit(t, right, "right")
	if leftCommit == rightCommit {
		t.Fatal("fixture commits are identical")
	}

	start := make(chan struct{})
	type result struct {
		stderr string
		code   int
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, candidate := range []struct {
		directory string
	}{{left}, {right}} {
		wait.Add(1)
		go func(directory string) {
			defer wait.Done()
			<-start
			stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main")
			results <- result{stderr: stderr, code: code}
		}(candidate.directory)
	}
	close(start)
	wait.Wait()
	close(results)

	successes, failures := 0, 0
	for outcome := range results {
		if outcome.code == 0 {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("race results = %d successes, %d failures; want exactly one applied", successes, failures)
	}
	final := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main")
	if final != leftCommit && final != rightCommit {
		t.Fatalf("remote main = %q; want one of the two candidates", final)
	}

	entries := harness.JournalEntriesFor(t, "refs/heads/main")
	reported, pending := 0, 0
	for _, entry := range entries {
		switch entry.State {
		case git.JournalReported:
			reported++
		case git.JournalIntended, git.JournalRefused:
			pending++
			if entry.State == git.JournalIntended {
				// The rejected receive-pack transaction leaves a pre-effect
				// intent; recovery reconciles it from observation only.
				if _, err := harness.Server.Service().Recover(context.Background(), entry.SourceEventID); err != nil {
					t.Fatalf("recover %q: %v", entry.SourceEventID, err)
				}
			}
		default:
			t.Fatalf("unexpected race journal state %q", entry.State)
		}
	}
	if reported != 2 || pending != 1 {
		t.Fatalf("race journal = %d reported, %d pending; want 2 reported (seed + winner) and 1 pending", reported, pending)
	}
	// Recovery of the losing intent must not move the ref.
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != final {
		t.Fatalf("recovery moved main to %q; want %q", observed, final)
	}
	if first == final {
		t.Fatal("the winning push did not move main")
	}
}
