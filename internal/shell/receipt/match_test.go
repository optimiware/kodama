package receipt_test

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/receipt"
)

// TestReceiptMatchWithoutStoredRecordIsNew asserts a first invocation is new,
// never a replay.
func TestReceiptMatchWithoutStoredRecordIsNew(t *testing.T) {
	outcome, err := receipt.Match(nil, committedRecord())
	if err != nil {
		t.Fatalf("Match(nil, candidate): %v", err)
	}
	if outcome != receipt.OutcomeNew {
		t.Fatalf("outcome = %q, want %q", outcome, receipt.OutcomeNew)
	}
}

// TestReceiptMatchIdenticalRetryReplaysOriginalResult is E-RECEIPT's
// lost-response recovery: an identical retry matches the durable receipt, and
// the durable record it resolves to is the stored original, not the freshly
// computed candidate. The stored original is never overwritten.
func TestReceiptMatchIdenticalRetryReplaysOriginalResult(t *testing.T) {
	stored := committedRecord()
	original := cloneRecord(stored)
	candidate := retryCandidate()

	outcome, err := receipt.Match(&stored, candidate)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if outcome != receipt.OutcomeReplay {
		t.Fatalf("outcome = %q, want %q", outcome, receipt.OutcomeReplay)
	}

	// The caller answers the retry with *existing, so the original result and
	// execution identity survive a crash after commit/before response.
	if string(stored.Result) != string(original.Result) {
		t.Errorf("stored result = %q, want the original %q", stored.Result, original.Result)
	}
	if stored.ExecutionID != original.ExecutionID {
		t.Errorf("stored execution = %q, want the original %q", stored.ExecutionID, original.ExecutionID)
	}
	if string(stored.Result) == string(candidate.Result) {
		t.Error("replay resolved to the candidate result; the effect would run twice")
	}
	if stored.ExecutionID == candidate.ExecutionID {
		t.Error("replay resolved to the candidate execution; the effect would run twice")
	}
	if !reflect.DeepEqual(stored, original) {
		t.Errorf("Match mutated the stored record:\n got %#v\nwant %#v", stored, original)
	}
}

// TestReceiptMatchChangedContentIsConflict asserts a caller key reused with
// different normalized content is refused with changed_idempotency_content and
// never replays the old result.
func TestReceiptMatchChangedContentIsConflict(t *testing.T) {
	stored := committedRecord()
	original := cloneRecord(stored)

	outcome, err := receipt.Match(&stored, changedCandidate())
	if outcome != receipt.OutcomeConflict {
		t.Fatalf("outcome = %q, want %q", outcome, receipt.OutcomeConflict)
	}
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)
	if !reflect.DeepEqual(stored, original) {
		t.Errorf("a conflict mutated the stored record:\n got %#v\nwant %#v", stored, original)
	}
}

// TestReceiptMatchDifferentIdentityNeverMatches asserts every member of the
// receipt identity separates receipts. A different scope is a different
// receipt, never a conflict against the stored one.
func TestReceiptMatchDifferentIdentityNeverMatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*receipt.Key)
	}{
		{"tenant", func(key *receipt.Key) { key.TenantID = otherTenant }},
		{"project", func(key *receipt.Key) { key.ProjectID = otherProject }},
		{"tenant-scoped", func(key *receipt.Key) { key.ProjectID = "" }},
		{"principal", func(key *receipt.Key) { key.PrincipalID = otherPrincipal }},
		{"operation", func(key *receipt.Key) { key.Operation = "task.start" }},
		{"caller key", func(key *receipt.Key) { key.CallerKey = "caller-key-2" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stored := committedRecord()
			original := cloneRecord(stored)

			// Same content hash but a different identity: still a new receipt.
			candidate := retryCandidate()
			testCase.mutate(&candidate.Key)

			outcome, err := receipt.Match(&stored, candidate)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if outcome != receipt.OutcomeNew {
				t.Fatalf("outcome = %q, want %q for a different receipt identity", outcome, receipt.OutcomeNew)
			}
			if !reflect.DeepEqual(stored, original) {
				t.Errorf("a different identity mutated the stored record:\n got %#v\nwant %#v", stored, original)
			}
		})
	}
}

// TestReceiptMatchRefusesUnreadableReceipts asserts a malformed candidate or a
// malformed stored record is refused instead of being reported as new or as a
// replay. An unknown outcome is never success.
func TestReceiptMatchRefusesUnreadableReceipts(t *testing.T) {
	cases := []struct {
		name      string
		existing  *receipt.Record
		candidate receipt.Record
	}{
		{
			name:      "candidate empty content hash",
			candidate: withCandidateHash(""),
		},
		{
			name:      "candidate empty tenant",
			candidate: withCandidateKey(func(key *receipt.Key) { key.TenantID = "" }),
		},
		{
			name:      "candidate empty principal",
			candidate: withCandidateKey(func(key *receipt.Key) { key.PrincipalID = "" }),
		},
		{
			name:      "candidate empty operation",
			candidate: withCandidateKey(func(key *receipt.Key) { key.Operation = "" }),
		},
		{
			name:      "candidate empty caller key",
			candidate: withCandidateKey(func(key *receipt.Key) { key.CallerKey = "" }),
		},
		{
			name:      "stored empty content hash",
			existing:  storedWithHash(""),
			candidate: committedRecord(),
		},
		{
			name:      "stored empty caller key",
			existing:  storedWithKey(func(key *receipt.Key) { key.CallerKey = "" }),
			candidate: committedRecord(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			outcome, err := receipt.Match(testCase.existing, testCase.candidate)
			assertFaultClass(t, err, fault.InvalidInput)
			if outcome == receipt.OutcomeReplay {
				t.Error("an unreadable receipt was reported as a replay")
			}
		})
	}
}

// TestReceiptMatchConcurrentRetriesAreStable asserts concurrent identical and
// changed retries against one stored receipt resolve consistently and never
// mutate the stored original. It is -race friendly.
func TestReceiptMatchConcurrentRetriesAreStable(t *testing.T) {
	stored := committedRecord()
	original := cloneRecord(stored)

	const goroutines = 64
	start := make(chan struct{})
	results := make(chan string, goroutines)
	var wait sync.WaitGroup
	for index := range goroutines {
		wait.Go(func() {
			<-start

			candidate := retryCandidate()
			wantOutcome := receipt.OutcomeReplay
			if index%2 == 1 {
				candidate = changedCandidate()
				wantOutcome = receipt.OutcomeConflict
			}
			outcome, err := receipt.Match(&stored, candidate)
			switch wantOutcome {
			case receipt.OutcomeReplay:
				if err != nil {
					results <- fmt.Sprintf("goroutine %d: identical retry: %v", index, err)
					return
				}
				if outcome != receipt.OutcomeReplay {
					results <- fmt.Sprintf("goroutine %d: identical retry outcome = %q", index, outcome)
				}
			case receipt.OutcomeConflict:
				if outcome != receipt.OutcomeConflict {
					results <- fmt.Sprintf("goroutine %d: changed retry outcome = %q", index, outcome)
					return
				}
				if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
					results <- fmt.Sprintf("goroutine %d: changed retry fault = %q", index, class)
				}
			}
		})
	}
	close(start)
	wait.Wait()
	close(results)
	for message := range results {
		t.Error(message)
	}
	if !reflect.DeepEqual(stored, original) {
		t.Errorf("concurrent matching mutated the stored record:\n got %#v\nwant %#v", stored, original)
	}
}

func withCandidateHash(hash string) receipt.Record {
	candidate := retryCandidate()
	candidate.ContentHash = hash
	return candidate
}

func withCandidateKey(mutate func(*receipt.Key)) receipt.Record {
	candidate := retryCandidate()
	mutate(&candidate.Key)
	return candidate
}

func storedWithHash(hash string) *receipt.Record {
	record := committedRecord()
	record.ContentHash = hash
	return &record
}

func storedWithKey(mutate func(*receipt.Key)) *receipt.Record {
	record := committedRecord()
	mutate(&record.Key)
	return &record
}
