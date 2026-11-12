//go:build integration

package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git/app/native"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestHookChannelSecurityAndSecretFreeState(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	seedMain(t, harness)

	socket := harness.Server.HookSocketPath()
	info, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatalf("Stat socket directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("socket directory mode = %04o; want 0700", mode)
	}
	info, err = os.Stat(socket)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %04o; want 0600", mode)
	}

	request := native.Request{
		Phase:       native.PhasePreReceive,
		ActorRef:    "kah_forged_by_client",
		Repository:  nativefix.Repository,
		ResourceID:  nativefix.ResourceID,
		SourceEvent: "event-forged",
		Lines:       []string{strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " refs/heads/main"},
	}
	forged, err := native.Call(context.Background(), socket, request, 2*time.Second)
	if err != nil {
		t.Fatalf("forged handle call: %v", err)
	}
	if forged.Accepted() || forged.Message != native.RejectionMessage {
		t.Fatalf("forged handle response = %+v; want the uniform rejection", forged)
	}

	ctx, cancel := context.WithCancel(context.Background())
	handle, err := harness.Registry.IssueActor(ctx, nativefix.ExecutionToken)
	if err != nil {
		cancel()
		t.Fatalf("IssueActor: %v", err)
	}
	cancel()
	request.ActorRef = handle
	expired, err := native.Call(context.Background(), socket, request, 2*time.Second)
	if err != nil {
		t.Fatalf("expired handle call: %v", err)
	}
	if expired.Accepted() || expired.Message != forged.Message {
		t.Fatalf("expired handle response = %+v; want the same uniform rejection", expired)
	}

	// Negative control: the scanner must find a planted canary.
	const canary = "kodama-canary-native-secret"
	canaryPath := filepath.Join(harness.Root, "canary.txt")
	if err := os.WriteFile(canaryPath, []byte(canary), 0o600); err != nil {
		t.Fatalf("WriteFile canary: %v", err)
	}
	hits, err := nativefix.ScanTree(harness.Root, canary)
	if err != nil {
		t.Fatalf("ScanTree canary: %v", err)
	}
	if len(hits) != 1 || hits[0] != canaryPath {
		t.Fatalf("canary hits = %v; want %q", hits, canaryPath)
	}
	if err := os.Remove(canaryPath); err != nil {
		t.Fatalf("Remove canary: %v", err)
	}

	for _, secret := range []string{nativefix.ExecutionToken, harness.HumanToken, nativefix.ServiceToken} {
		for _, root := range []string{harness.Root, harness.ClientHome, harness.SocketBase} {
			hits, err := nativefix.ScanTree(root, secret)
			if err != nil {
				t.Fatalf("ScanTree(%q): %v", root, err)
			}
			if len(hits) != 0 {
				t.Fatalf("credential %q persisted under %q: %v", secret, root, hits)
			}
		}
		if strings.Contains(harness.Contract.RecordedJSON(), secret) {
			t.Fatalf("the contract fixture recorded credential %q", secret)
		}
		if strings.Contains(harness.Contract.LogText(), secret) {
			t.Fatalf("the contract fixture log contains credential %q", secret)
		}
	}

	for _, entry := range harness.JournalEntries(t) {
		encoded := entry.ResourceID + entry.Repository + entry.Ref + entry.ExpectedBase + entry.Candidate
		for _, secret := range []string{nativefix.ExecutionToken, harness.HumanToken, nativefix.ServiceToken} {
			if strings.Contains(encoded, secret) {
				t.Fatalf("journal entry %q contains credential %q", entry.SourceEventID, secret)
			}
		}
	}
}
