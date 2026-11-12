//go:build integration

package factorytest

// Composed Git uncertain-effect recovery.
//
// The scenario proves AC20.4 on the real factory composition, beyond a
// report-only sweep. Three real engineer pushes are applied natively while the
// installed post-receive hook is temporarily unable to run, so each leaves a
// durable `intended` journal entry, a real verification and a moved ref but no
// report. The hook is restored and the Git service restarted over the same
// storage and journal. The restarted process has no receive-pack exchange in
// flight, so its startup sweep reconciles each intent from real repository
// evidence (the sweep never settles an intent while an exchange is active):
//
//   - ref equals the journaled candidate -> accepted and reported exactly once;
//   - ref equals the journaled expected base -> refused (never reported);
//   - ref equals neither -> uncertain and never guessed, then reported exactly
//     once when the explicit recovery boundary observes the candidate.
//
// Recovery never runs a second verification and never performs a ref effect.
// The only fixture seam is the temporarily failed post-receive hook and direct
// ref writes that model the observed repository state; no journal entry,
// verification, approval or report is fabricated.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	uncertainRecoveryAppliedRef = "refs/heads/work/uncertain-applied"
	uncertainRecoveryRefusedRef = "refs/heads/work/uncertain-refused"
	uncertainRecoveryUnknownRef = "refs/heads/work/uncertain-unknown"
)

// TestFactoryGitUncertainEffectRecovery proves a journaled effect is settled
// from real repository evidence, not from a timer or a guessed outcome.
func TestFactoryGitUncertainEffectRecovery(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "uncertain")

	// Seed one base commit per ref through normal served pushes. These are the
	// expected bases the refused observation must prove unchanged.
	baseApplied := fixture.commit(t, directory, "uncertain applied base")
	baseRefused := fixture.commit(t, directory, "uncertain refused base")
	baseUnknown := fixture.commit(t, directory, "uncertain unknown base")
	for ref, base := range map[string]string{
		uncertainRecoveryAppliedRef: baseApplied,
		uncertainRecoveryRefusedRef: baseRefused,
		uncertainRecoveryUnknownRef: baseUnknown,
	} {
		if stderr, code := fixture.push(directory, fixture.engineerToken, base+":"+ref); code != 0 {
			t.Fatalf("seed %s: exit %d: %s", ref, code, stderr)
		}
		if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != base {
			t.Fatalf("seeded %s = %q; want %q", ref, remote, base)
		}
		fixture.waitJournal(t, ref, base)
	}

	candidateApplied := fixture.commit(t, directory, "uncertain applied candidate")
	candidateRefused := fixture.commit(t, directory, "uncertain refused candidate")
	candidateUnknown := fixture.commit(t, directory, "uncertain unknown candidate")

	restorePostReceive := suspendPostReceive(t, fixture)
	defer restorePostReceive()
	for ref, candidate := range map[string]string{
		uncertainRecoveryAppliedRef: candidateApplied,
		uncertainRecoveryRefusedRef: candidateRefused,
		uncertainRecoveryUnknownRef: candidateUnknown,
	} {
		if stderr, code := fixture.push(directory, fixture.engineerToken, candidate+":"+ref); code != 0 {
			t.Fatalf("append %s without post-receive: exit %d: %s", ref, code, stderr)
		}
		if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != candidate {
			t.Fatalf("appended %s = %q; want the native apply %q", ref, remote, candidate)
		}
	}
	restorePostReceive()

	applied := waitProtectedRefJournalState(t, fixture, uncertainRecoveryAppliedRef, candidateApplied, git.JournalIntended)
	refused := waitProtectedRefJournalState(t, fixture, uncertainRecoveryRefusedRef, candidateRefused, git.JournalIntended)
	unknown := waitProtectedRefJournalState(t, fixture, uncertainRecoveryUnknownRef, candidateUnknown, git.JournalIntended)
	verificationsBefore := fixture.countVerificationRows(t, "git.ref.update")
	for _, entry := range []git.JournalEntry{applied, refused, unknown} {
		if entry.VerificationID == "" || entry.SourceEventID == "" {
			t.Fatalf("intended journal entry has no verification/source identity: %+v", entry)
		}
		if event, ok, err := fixture.git.ReportEvent(t.Context(), entry.VerificationID); err != nil {
			t.Fatalf("read report event for intended entry: %v", err)
		} else if ok {
			t.Fatalf("intended entry %s already has report event %+v", entry.SourceEventID, event)
		}
	}

	// Model the three observable repository states with direct ref writes:
	// the refused push never applied, and the unknown push left a third commit.
	fixture.git.SetRef(t, factory.GitProjectRepository, uncertainRecoveryRefusedRef, baseRefused)
	fixture.git.SetRef(t, factory.GitProjectRepository, uncertainRecoveryUnknownRef, baseApplied)

	// A restart reuses the durable journal. No receive-pack exchange is in
	// flight in the new process, so the startup sweep settles each intent from
	// the repository evidence it observes, never from a timer or a guess:
	// the candidate is accepted and reported, the unchanged base is refused,
	// and a third commit is recorded as uncertain.
	fixture.git.Restart(t)

	// Candidate observed: acceptance and exactly one report.
	recovered := waitProtectedRefJournalState(t, fixture, uncertainRecoveryAppliedRef, candidateApplied, git.JournalReported)
	if recovered.VerificationID != applied.VerificationID {
		t.Fatalf("applied recovery verification = %q; want the journaled %q",
			recovered.VerificationID, applied.VerificationID)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, uncertainRecoveryAppliedRef); remote != candidateApplied {
		t.Fatalf("applied recovery moved the ref to %q; want the observed candidate %q", remote, candidateApplied)
	}
	if event, ok, err := fixture.git.ReportEvent(t.Context(), applied.VerificationID); err != nil {
		t.Fatalf("read the applied recovery event: %v", err)
	} else if !ok || event.CausationID != applied.VerificationID {
		t.Fatalf("applied recovery event = %+v present=%t; want the journaled verification as causation",
			event, ok)
	}

	// Expected base observed: the intent is refused and never reported.
	refusedEntry := waitProtectedRefJournalState(t, fixture, uncertainRecoveryRefusedRef, candidateRefused, git.JournalRefused)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, uncertainRecoveryRefusedRef); remote != baseRefused {
		t.Fatalf("not-applied recovery moved the ref to %q; want the observed base %q", remote, baseRefused)
	}
	if event, ok, err := fixture.git.ReportEvent(t.Context(), refusedEntry.VerificationID); err != nil {
		t.Fatalf("read the not-applied event: %v", err)
	} else if ok {
		t.Fatalf("not-applied recovery reported event %+v; want no report", event)
	}

	// Settled entries are no-ops for the explicit recovery boundary.
	for _, settled := range []struct {
		name  string
		entry git.JournalEntry
		state git.JournalState
	}{{"reported", recovered, git.JournalReported}, {"refused", refusedEntry, git.JournalRefused}} {
		result, err := fixture.git.Recover(t, settled.entry.SourceEventID)
		if err != nil {
			t.Fatalf("recover the settled %s entry: %v", settled.name, err)
		}
		if result.Entry.State != settled.state || result.Outcome != "" {
			t.Fatalf("recovering a settled %s entry = outcome %q state %q; want a no-op in %q",
				settled.name, result.Outcome, result.Entry.State, settled.state)
		}
	}

	// A third observed commit is not evidence of either outcome: stay
	// uncertain and never report or re-push. Once the candidate is observed,
	// the same durable intent settles exactly once.
	waitProtectedRefJournalState(t, fixture, uncertainRecoveryUnknownRef, candidateUnknown, git.JournalUncertain)
	unknownResult, err := fixture.git.Recover(t, unknown.SourceEventID)
	if err != nil {
		t.Fatalf("recover the unknown observed state: %v", err)
	}
	if unknownResult.Outcome != git.RecoveryUnknown || unknownResult.Entry.State != git.JournalUncertain ||
		unknownResult.Reported {
		t.Fatalf("unknown recovery = outcome %q state %q reported=%t; want unknown/uncertain",
			unknownResult.Outcome, unknownResult.Entry.State, unknownResult.Reported)
	}
	if event, ok, err := fixture.git.ReportEvent(t.Context(), unknown.VerificationID); err != nil {
		t.Fatalf("read the unknown event: %v", err)
	} else if ok {
		t.Fatalf("unknown recovery reported event %+v; want no report", event)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, uncertainRecoveryUnknownRef); remote != baseApplied {
		t.Fatalf("unknown recovery moved the ref to %q; want the observed third commit %q", remote, baseApplied)
	}

	fixture.git.SetRef(t, factory.GitProjectRepository, uncertainRecoveryUnknownRef, candidateUnknown)
	final, err := fixture.git.Recover(t, unknown.SourceEventID)
	if err != nil {
		t.Fatalf("recover after the candidate was observed: %v", err)
	}
	if final.Outcome != git.RecoveryApplied || final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("final unknown recovery = outcome %q state %q reported=%t; want applied/reported",
			final.Outcome, final.Entry.State, final.Reported)
	}
	if final.Entry.VerificationID != unknown.VerificationID {
		t.Fatalf("final recovery verification = %q; want the journaled %q",
			final.Entry.VerificationID, unknown.VerificationID)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, uncertainRecoveryUnknownRef); remote != candidateUnknown {
		t.Fatalf("final recovery moved the ref to %q; want the observed candidate %q", remote, candidateUnknown)
	}
	if event, ok, err := fixture.git.ReportEvent(t.Context(), unknown.VerificationID); err != nil {
		t.Fatalf("read the final recovery event: %v", err)
	} else if !ok || event.CausationID != unknown.VerificationID {
		t.Fatalf("final recovery event = %+v present=%t; want the journaled verification as causation",
			event, ok)
	}

	// Recovery is reconciliation, never a second verification.
	if after := fixture.countVerificationRows(t, "git.ref.update"); after != verificationsBefore {
		t.Fatalf("verification rows after recovery = %d; want the unchanged %d: recovery must not re-verify",
			after, verificationsBefore)
	}
}

// journalEntryBySourceEvent reads the current durable entry of one source
// event from the fixture's journal directory.
func journalEntryBySourceEvent(t *testing.T, fixture *gitHarness, sourceEventID string) git.JournalEntry {
	t.Helper()
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.SourceEventID == sourceEventID {
			return entry
		}
	}
	t.Fatalf("journal entry %s is absent", sourceEventID)
	return git.JournalEntry{}
}

// suspendPostReceive replaces the project repository's managed post-receive
// hook with a failing script, so a real native apply leaves its journal intent
// unreconciled. It returns a restore function that reinstalls the exact managed
// hook bytes. The change is test-owned and never survives the test.
func suspendPostReceive(t *testing.T, fixture *gitHarness) (restore func()) {
	t.Helper()
	path := filepath.Join(fixture.git.RepositoryPath(factory.GitProjectRepository), "hooks", "post-receive")
	managed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the managed post-receive hook: %v", err)
	}
	if len(managed) == 0 {
		t.Fatalf("the managed post-receive hook is empty")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("replace the post-receive hook with the crash boundary: %v", err)
	}
	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		if err := os.WriteFile(path, managed, 0o755); err != nil {
			t.Fatalf("restore the managed post-receive hook: %v", err)
		}
	}
}
