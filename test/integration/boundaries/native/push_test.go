//go:build integration

package native_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/git"
	idfix "github.com/optimiware/kodema/test/support/boundaries/identity"
	nativefix "github.com/optimiware/kodema/test/support/boundaries/native"
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

// TestProtectedRefDiagnosticsAndUnbornExpectedBase pins:
//  1. Pushing to a protected ref without approval or with a mismatched
//     candidate/base logs the exact refusal reason on the Git server and
//     surfaces a safe reason category to the Git client.
//  2. Creating an unborn protected ref succeeds whether the approved
//     git.ref.update subject specifies expected_base "" or Git's 40-zero null
//     OID, while subsequent updates of the existing ref enforce exact SHA
//     equality.
func TestProtectedRefDiagnosticsAndUnbornExpectedBase(t *testing.T) {
	var logs lockedLogBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	harness := newHarness(t, nativefix.Options{Logger: logger})
	directory, _ := seedMain(t, harness)

	guardedSubject := func(ref, expectedBase, candidate string) api.Subject {
		return api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
			{Name: "resource", Kind: "string", Value: nativefix.ResourceID},
			{Name: "ref", Kind: "string", Value: ref},
			{Name: "expected_base", Kind: "string", Value: expectedBase},
			{Name: "candidate", Kind: "string", Value: candidate},
		}}
	}

	const zero40 = "0000000000000000000000000000000000000000"

	// 1. Direct human push to protected ref -> refused with approval_required.
	logs.Reset()
	firstCandidate := harness.Commit(t, directory, "guarded-init-1")
	stderr, code := harness.Push(t, directory, harness.HumanToken, "origin", "HEAD:refs/heads/guarded/init-empty")
	requireFailure(t, stderr, code)
	if !strings.Contains(stderr, "kodema git: update rejected: protected ref requires role/reviewer approval") {
		t.Fatalf("human protected push stderr = %q; want approval_required diagnostic", stderr)
	}
	if logged := logs.String(); !strings.Contains(logged, "reason=approval_required") || !strings.Contains(logged, "ref=refs/heads/guarded/init-empty") {
		t.Fatalf("human protected push server logs = %q; want reason=approval_required and ref", logged)
	}

	// 2. Execution push without approval -> refused with approval_mismatch.
	logs.Reset()
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-empty")
	requireFailure(t, stderr, code)
	if !strings.Contains(stderr, "kodema git: update rejected: approval subject mismatch or missing role/reviewer approval") {
		t.Fatalf("unapproved execution push stderr = %q; want approval_mismatch diagnostic", stderr)
	}
	if logged := logs.String(); !strings.Contains(logged, "reason=approval_mismatch") || !strings.Contains(logged, "no matching approval evidence") || !strings.Contains(logged, "ref=refs/heads/guarded/init-empty") {
		t.Fatalf("unapproved execution push server logs = %q; want exact refusal reason", logged)
	}

	// 3. Mismatched candidate approval -> refused with approval_mismatch.
	logs.Reset()
	harness.Contract.AddApproval(idfix.ApprovalFact{
		ExecutionID: nativefix.ExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  nativefix.ResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     guardedSubject("refs/heads/guarded/init-empty", "", strings.Repeat("f", 40)),
		Decision:    "pass",
	})
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-empty")
	requireFailure(t, stderr, code)
	if !strings.Contains(stderr, "approval subject mismatch or missing role/reviewer approval") {
		t.Fatalf("mismatched candidate push stderr = %q; want approval_mismatch diagnostic", stderr)
	}
	if logged := logs.String(); !strings.Contains(logged, "reason=approval_mismatch") {
		t.Fatalf("mismatched candidate server logs = %q; want reason=approval_mismatch", logged)
	}

	// 4. Unborn protected ref with approval expected_base: "" -> succeeds.
	harness.Contract.AddApproval(idfix.ApprovalFact{
		ExecutionID: nativefix.ExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  nativefix.ResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     guardedSubject("refs/heads/guarded/init-empty", "", firstCandidate),
		Decision:    "pass",
	})
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-empty")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/guarded/init-empty"); observed != firstCandidate {
		t.Fatalf("guarded/init-empty = %q; want %q", observed, firstCandidate)
	}

	// 5. Unborn protected ref with approval expected_base: 40-zero null OID -> succeeds.
	harness.Contract.AddApproval(idfix.ApprovalFact{
		ExecutionID: nativefix.ExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  nativefix.ResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     guardedSubject("refs/heads/guarded/init-zero40", zero40, firstCandidate),
		Decision:    "pass",
	})
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-zero40")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/guarded/init-zero40"); observed != firstCandidate {
		t.Fatalf("guarded/init-zero40 = %q; want %q", observed, firstCandidate)
	}

	// 6. Existing (non-unborn) protected ref still enforces exact SHA equality:
	// an approval with 40-zero expected_base must be refused.
	logs.Reset()
	secondCandidate := harness.Commit(t, directory, "guarded-ff-2")
	harness.Contract.AddApproval(idfix.ApprovalFact{
		ExecutionID: nativefix.ExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  nativefix.ResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     guardedSubject("refs/heads/guarded/init-zero40", zero40, secondCandidate),
		Decision:    "pass",
	})
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-zero40")
	requireFailure(t, stderr, code)
	if !strings.Contains(stderr, "approval subject mismatch or missing role/reviewer approval") {
		t.Fatalf("non-unborn push with 40-zero approval stderr = %q; want approval_mismatch", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/guarded/init-zero40"); observed != firstCandidate {
		t.Fatalf("non-unborn ref moved to %q; want unchanged %q", observed, firstCandidate)
	}

	// 7. Existing protected ref with exact base SHA succeeds.
	harness.Contract.AddApproval(idfix.ApprovalFact{
		ExecutionID: nativefix.ExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  nativefix.ResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     guardedSubject("refs/heads/guarded/init-zero40", firstCandidate, secondCandidate),
		Decision:    "pass",
	})
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/guarded/init-zero40")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/guarded/init-zero40"); observed != secondCandidate {
		t.Fatalf("guarded/init-zero40 = %q; want %q", observed, secondCandidate)
	}

	// 8. Policy-denied push logs reason=policy_denied, ref and verified
	// actor_id without leaking any credential.
	logs.Reset()
	stderr, code = harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/protected")
	requireFailure(t, stderr, code)
	if !strings.Contains(stderr, "kodema git: update rejected: push denied by repository policy") {
		t.Fatalf("policy-denied push stderr = %q; want policy_denied diagnostic", stderr)
	}
	logged := logs.String()
	for _, want := range []string{"reason=policy_denied", "ref=refs/heads/protected", "actor_id=" + nativefix.ExecutionID} {
		if !strings.Contains(logged, want) {
			t.Fatalf("policy-denied server logs = %q; want %q", logged, want)
		}
	}
	for _, secret := range []string{nativefix.ExecutionToken, harness.HumanToken, harness.AdminToken, nativefix.ServiceToken} {
		if strings.Contains(stderr, secret) || strings.Contains(logged, secret) {
			t.Fatalf("credential leaked in refusal stderr or server logs")
		}
	}
}
