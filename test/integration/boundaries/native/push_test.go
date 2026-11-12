//go:build integration

package native_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestPreReceiveJournalsOnlyThenReceivePackAppliesAndPostReceiveReports(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	directory, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := countReports(harness)

	probePath := filepath.Join(harness.Root, "pre-receive-probe.txt")
	harness.WriteHook(t, nativefix.Repository, "pre-receive", probeScript(harness, probePath))

	candidate := harness.Commit(t, directory, "second")
	if stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("push: %s", stderr)
	}

	probe, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("read pre-receive probe: %v", err)
	}
	text := string(probe)
	if !strings.Contains(text, "exit=0") {
		t.Fatalf("pre-receive probe = %q; want exit=0", text)
	}
	// At pre-receive completion receive-pack has applied nothing: the ref is
	// still the previous base.
	if !strings.Contains(text, "\nref="+first+"\n") {
		t.Fatalf("pre-receive probe = %q; want ref unchanged at %q", text, first)
	}
	journalSection := between(text, "journal-start\n", "journal-end\n")
	if !strings.Contains(journalSection, `"state":"intended"`) {
		t.Fatalf("pre-receive journal snapshot = %q; want an intended entry", journalSection)
	}
	if !strings.Contains(journalSection, `"expected_base":"`+first+`"`) ||
		!strings.Contains(journalSection, `"candidate":"`+candidate+`"`) {
		t.Fatalf("pre-receive journal snapshot lacks the exact update: %q", journalSection)
	}
	environmentSection := between(text, "env-start\n", "env-end\n")
	for _, secret := range []string{nativefix.ExecutionToken, harness.HumanToken, nativefix.ServiceToken} {
		if strings.Contains(environmentSection, secret) {
			t.Fatalf("the hook process environment leaked %q", secret)
		}
	}

	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q after receive-pack", observed, candidate)
	}
	var applied *git.JournalEntry
	for _, entry := range harness.JournalEntriesFor(t, "refs/heads/main") {
		if entry.Candidate == candidate {
			copied := entry
			applied = &copied
		}
	}
	if applied == nil {
		t.Fatal("the fast-forward push left no journal entry")
	}
	if applied.State != git.JournalReported {
		t.Fatalf("final journal state = %q; want reported", applied.State)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications+1)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents+1)
	}
	if got := countReports(harness); got != baselineReports+1 {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports+1)
	}
}

func countReports(harness *nativefix.Harness) int {
	reports := 0
	for _, record := range harness.Contract.Recorded() {
		if record.Operation == "resource.report" {
			reports++
		}
	}
	return reports
}

func TestProductInstalledHooksPushFastForward(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	directory, first := seedMain(t, harness)

	entries := harness.JournalEntriesFor(t, "refs/heads/main")
	if len(entries) != 1 || entries[0].State != git.JournalReported {
		t.Fatalf("seed journal = %+v; want one reported entry", entries)
	}
	second := harness.Commit(t, directory, "second")
	if stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("fast-forward push: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("remote main = %q; want %q", observed, second)
	}
	entries = harness.JournalEntriesFor(t, "refs/heads/main")
	if len(entries) != 2 {
		t.Fatalf("journal entries = %d; want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.State != git.JournalReported {
			t.Fatalf("journal entry %q state = %q; want reported", entry.SourceEventID, entry.State)
		}
	}
	var create, fastForward bool
	for _, entry := range entries {
		if entry.ExpectedBase == "" && entry.Candidate == first {
			create = true
		}
		if entry.ExpectedBase == first && entry.Candidate == second {
			fastForward = true
		}
	}
	if !create || !fastForward {
		t.Fatalf("journal entries = %+v; want the create and the fast-forward", entries)
	}
	if harness.Contract.EventCount() != 2 {
		t.Fatalf("report event count = %d; want 2", harness.Contract.EventCount())
	}
}

func between(text, start, end string) string {
	startIndex := strings.Index(text, start)
	if startIndex < 0 {
		return ""
	}
	startIndex += len(start)
	endIndex := strings.Index(text[startIndex:], end)
	if endIndex < 0 {
		return ""
	}
	return text[startIndex : startIndex+endIndex]
}
