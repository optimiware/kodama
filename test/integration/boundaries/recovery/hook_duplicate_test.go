//go:build integration

package recovery_test

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestDuplicateDeliveryOfAcceptedEntryRetriesReportThroughPreReceive drives the
// pre-receive duplicate-delivery path directly: an accepted entry delivered
// again must retry its report from the existing entry, with no verification and
// no ref effect. It isolates the batch settlement path from post-receive.
func TestDuplicateDeliveryOfAcceptedEntryRetriesReportThroughPreReceive(t *testing.T) {
	events := recoveryfix.NewSourceEvents()
	harness := newHarness(t, recoveryfix.Options{SourceEvents: events.Next})
	directory, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	events.Set("source-hook-dup-1")
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	candidate := harness.Commit(t, directory, "hook-duplicate")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil || entry.State != git.JournalAccepted {
		t.Fatalf("journal entry = %+v; want an accepted entry", entry)
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)

	ctx := t.Context()
	handle, err := harness.Registry().IssueActor(ctx, recoveryfix.ExecutionToken)
	if err != nil {
		t.Fatalf("IssueActor: %v", err)
	}
	environment := map[string]string{
		git.EnvActorRef:    handle,
		git.EnvRepository:  recoveryfix.Repository,
		git.EnvResourceID:  recoveryfix.ResourceID,
		git.EnvSourceEvent: "source-hook-dup-1",
	}
	hook, err := git.NewHook(git.HookConfig{
		Service: harness.Service(),
		Actors:  harness.Registry(),
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	lines := first + " " + candidate + " refs/heads/main\n"
	result, err := hook.RunPreReceive(ctx, strings.NewReader(lines))
	if err != nil || !result.Accepted() {
		t.Fatalf("duplicate pre-receive = %+v, %v; want accepted", result, err)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d: duplicate delivery must not verify", got, baselineVerifications+1)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d: the accepted entry must retry its report", got, baselineEvents+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports+2)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "reported" {
		t.Fatalf("raw journal state = %q; want reported", state)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want %q: pre-receive performs no effect", observed, first)
	}
	harness.RequireNoSecrets(t, stderr)
}
