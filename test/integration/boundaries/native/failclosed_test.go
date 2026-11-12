//go:build integration

package native_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

type pushOutcome struct {
	stderr string
	code   int
}

func TestPushRejectedWhenTheHookChannelIsKilled(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	harness.InitBareRepository(t, nativefix.Repository)
	harness.InstallProductHooks(t, nativefix.Repository)

	barrier := filepath.Join(harness.Root, "barrier.fifo")
	ack := filepath.Join(harness.Root, "ack.fifo")
	for _, path := range []string{barrier, ack} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatalf("Mkfifo %q: %v", path, err)
		}
	}
	harness.WriteHook(t, nativefix.Repository, "pre-receive", barrierScript(harness, barrier, ack))

	directory := cloneFor(t, harness, nativefix.ExecutionToken, "killed-clone")
	commit := harness.Commit(t, directory, "first")

	outcomes := make(chan pushOutcome, 1)
	go func() {
		stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main")
		outcomes <- pushOutcome{stderr: stderr, code: code}
	}()

	reader, err := os.OpenFile(barrier, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open barrier: %v", err)
	}
	ready := make([]byte, 16)
	read, err := reader.Read(ready)
	reader.Close()
	if err != nil || !strings.Contains(string(ready[:read]), "ready") {
		t.Fatalf("barrier read = (%q, %v); want ready", ready[:read], err)
	}

	if err := harness.Server.CloseHooks(); err != nil {
		t.Fatalf("CloseHooks: %v", err)
	}
	writer, err := os.OpenFile(ack, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open ack: %v", err)
	}
	if _, err := writer.WriteString("go\n"); err != nil {
		writer.Close()
		t.Fatalf("write ack: %v", err)
	}
	writer.Close()

	var outcome pushOutcome
	select {
	case outcome = <-outcomes:
	case <-time.After(15 * time.Second):
		t.Fatal("push did not finish after the hook channel was killed")
	}
	requireFailure(t, outcome.stderr, outcome.code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != "" {
		t.Fatalf("a rejected push moved main to %q (candidate %q)", observed, commit)
	}
}

func TestSlowVerificationTimesOutAndRejectsThePush(t *testing.T) {
	gated := newGatedVerification(nil)
	harness := newHarness(t, nativefix.Options{
		Verification: gated,
		HookTimeout:  200 * time.Millisecond,
	})
	harness.InitBareRepository(t, nativefix.Repository)
	harness.InstallProductHooks(t, nativefix.Repository)

	directory := cloneFor(t, harness, nativefix.ExecutionToken, "slow-clone")
	harness.Commit(t, directory, "first")

	outcomes := make(chan pushOutcome, 1)
	go func() {
		stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main")
		outcomes <- pushOutcome{stderr: stderr, code: code}
	}()

	waitFor(t, gated.entered, "the service verification to block")
	var outcome pushOutcome
	select {
	case outcome = <-outcomes:
	case <-time.After(15 * time.Second):
		close(gated.release)
		t.Fatal("push did not time out")
	}
	close(gated.release)
	requireFailure(t, outcome.stderr, outcome.code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != "" {
		t.Fatalf("a timed-out push moved main to %q", observed)
	}
}
