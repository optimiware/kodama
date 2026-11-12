//go:build integration

package recovery_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestClientDisconnectBeforePreReceiveLeavesNoIntentOrVerification proves that
// a client disconnect while the receive-pack request body is still incomplete
// never starts the native receive-pack/pre-receive phase: no intent, no
// verification, no report and no ref effect.
func TestClientDisconnectBeforePreReceiveLeavesNoIntentOrVerification(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()
	baselineEntries := len(harness.JournalEntries(t))
	refsBefore := harness.Refs(t, recoveryfix.Repository)

	harness.DrainConnectionCloses()
	connection, err := net.Dial("tcp", harness.Address())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	request := fmt.Sprintf(
		"POST /%s.git/git-receive-pack HTTP/1.1\r\nHost: %s\r\n%s\r\nContent-Type: application/x-git-receive-pack-request\r\nContent-Length: 4096\r\n\r\n",
		recoveryfix.Repository, harness.Address(), recoveryfix.BasicHeader(recoveryfix.ExecutionToken),
	)
	if _, err := connection.Write([]byte(request)); err != nil {
		connection.Close()
		t.Fatalf("write request head: %v", err)
	}
	if _, err := connection.Write([]byte("0032")); err != nil {
		connection.Close()
		t.Fatalf("write partial body: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Wait for the server to finish the aborted exchange instead of sleeping.
	select {
	case <-harness.ConnectionClosed():
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not finish the aborted request")
	}

	if got := harness.Contract.VerificationCount(); got != baselineVerifications {
		t.Fatalf("verification count = %d; want %d: a canceled request must not verify", got, baselineVerifications)
	}
	if got := len(harness.JournalEntries(t)); got != baselineEntries {
		t.Fatalf("journal entries = %d; want %d: a canceled request must not journal an intent", got, baselineEntries)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	if got := harness.CountReports(); got != baselineReports {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("a canceled request changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t)
}

// TestCanceledPreparePerformsNoVerification proves the service-level
// cancellation boundary: an already-canceled attempt performs no verification
// and persists no intent.
func TestCanceledPreparePerformsNoVerification(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEntries := len(harness.JournalEntries(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := harness.Service().Prepare(ctx, git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       strings.Repeat("a", 40),
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "canceledrequest1",
	})
	if err == nil {
		t.Fatal("a canceled attempt was prepared")
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications)
	}
	if got := len(harness.JournalEntries(t)); got != baselineEntries {
		t.Fatalf("journal entries = %d; want %d", got, baselineEntries)
	}
	harness.RequireNoSecrets(t)
}

// TestAfterAcceptanceDisconnectReconcilesEntry proves that a disconnect after
// the native apply but before reporting is recovered from observation: the ref
// stays applied and the entry settles with exactly one report and no second
// verification.
func TestAfterAcceptanceDisconnectReconcilesEntry(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()

	barrier := filepath.Join(harness.Root, "post-receive-barrier.fifo")
	ack := filepath.Join(harness.Root, "post-receive-ack.fifo")
	done := filepath.Join(harness.Root, "post-receive-done.txt")
	recoveryfix.MakeFifo(t, barrier)
	recoveryfix.MakeFifo(t, ack)
	harness.WriteHook(t, recoveryfix.Repository, "post-receive", postReceiveBarrierScript(harness, barrier, ack, done))

	candidate := harness.Commit(t, directory, "after-acceptance")
	harness.DrainConnectionCloses()
	push := harness.StartPush(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")

	if ready := readFifo(t, barrier, 15*time.Second); !strings.Contains(ready, "ready") {
		t.Fatalf("post-receive barrier = %q; want ready", ready)
	}
	// post-receive runs after receive-pack applied the ref.
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want the applied %q before the disconnect", observed, candidate)
	}

	if err := push.Kill(); err != nil {
		t.Fatalf("kill push: %v", err)
	}
	stderr, code := push.Wait()
	requireFailure(t, stderr, code)

	if recoveryfix.ReleaseFifo(t, ack, "go\n", 2*time.Second) {
		if !recoveryfix.WaitForFile(t, done, 15*time.Second) {
			t.Fatal("the post-receive wrapper did not finish")
		}
	}
	// The server-side request must have finished before the app restarts.
	select {
	case <-harness.ConnectionClosed():
	case <-time.After(5 * time.Second):
	}

	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the disconnected push left no journal entry")
	}
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Entry.State != git.JournalReported || !recovery.Reported {
		t.Fatalf("recovery = %+v; want a reported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d: recovery must not verify again", got, baselineVerifications+1)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}
