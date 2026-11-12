//go:build integration

package recovery_test

import (
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestSecretFreeJournalsAndLogsAcrossRecoveryScenarios runs every reporting and
// recovery shape in one harness (reported, human, refused, late report, lost
// response, unknown mutation, post-restart push) and then scans the raw journal
// bytes, the app server log, the contract/OIDC fixture logs, the captured git
// stdout/stderr and the hook environment for credentials and actor handles.
func TestSecretFreeJournalsAndLogsAcrossRecoveryScenarios(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	harness.InitBareRepository(t, recoveryfix.Repository)
	harness.InstallProductHooks(t, recoveryfix.Repository)
	probePath := filepath.Join(harness.Root, "hook-env-probe.txt")
	harness.WriteEnvProbeHook(t, recoveryfix.Repository, probePath)

	directory := cloneFor(t, harness, recoveryfix.ExecutionToken, "secret-scan-clone")
	var diagnostics []string
	push := func(token string, extra ...string) {
		t.Helper()
		stderr, code := harness.Push(t, directory, token, extra...)
		diagnostics = append(diagnostics, stderr)
		requireSuccess(t, stderr, code)
	}

	// A reported execution push.
	first := harness.Commit(t, directory, "reported")
	push(recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")

	// A reported human push through the OIDC credential.
	harness.Commit(t, directory, "human")
	push(harness.HumanToken, "origin", "HEAD:refs/heads/main")

	// A refused push.
	harness.Commit(t, directory, "denied")
	deniedStderr, deniedCode := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/protected")
	diagnostics = append(diagnostics, deniedStderr)
	requireFailure(t, deniedStderr, deniedCode)
	requireRejectionMessage(t, deniedStderr)

	// A late report, recovered on the same instance.
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	late := harness.Commit(t, directory, "late")
	lateStderr, lateCode := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	diagnostics = append(diagnostics, lateStderr)
	requireSuccess(t, lateStderr, lateCode)
	lateEntry := entryForCandidate(t, harness, "refs/heads/main", late)
	if lateEntry == nil {
		t.Fatal("the late-report push left no journal entry")
	}
	if recovery := recoverEntry(t, harness, lateEntry.SourceEventID); !recovery.Reported {
		t.Fatalf("late-report recovery = %+v; want reported", recovery)
	}

	// A lost reporting response, replayed by reporting only.
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDropAfterCommit})
	lost := harness.Commit(t, directory, "lost")
	lostStderr, lostCode := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	diagnostics = append(diagnostics, lostStderr)
	requireSuccess(t, lostStderr, lostCode)
	lostEntry := entryForCandidate(t, harness, "refs/heads/main", lost)
	if lostEntry == nil {
		t.Fatal("the lost-response push left no journal entry")
	}
	if recovery := recoverEntry(t, harness, lostEntry.SourceEventID); !recovery.Reported || !recovery.Report.Replayed {
		t.Fatalf("lost-response recovery = %+v; want a replayed report", recovery)
	}

	// An unknown mutation reconciled on restart.
	unreachablePostReceive(t, harness)
	unknown := harness.Commit(t, directory, "unknown")
	unknownStderr, unknownCode := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	diagnostics = append(diagnostics, unknownStderr)
	requireSuccess(t, unknownStderr, unknownCode)
	unknownEntry := entryForCandidate(t, harness, "refs/heads/main", unknown)
	if unknownEntry == nil {
		t.Fatal("the unknown push left no journal entry")
	}
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, unknown, "unknown observed state")
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)
	harness.RestoreProductHooks(t, recoveryfix.Repository)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if recovery := recoverEntry(t, harness, unknownEntry.SourceEventID); recovery.Outcome != git.RecoveryUnknown {
		t.Fatalf("unknown recovery = %+v; want unknown", recovery)
	}

	// A fresh push after the restart is verified and reported.
	harness.SetOrigin(t, directory, recoveryfix.Repository)
	harness.Commit(t, directory, "after-restart")
	push(recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/after-restart")

	if len(harness.RecordedHandles()) == 0 {
		t.Fatal("the hook environment probe recorded no actor handle")
	}
	harness.RequireNoSecrets(t, diagnostics...)
	if first == third {
		t.Fatal("fixture commits are identical")
	}
}
