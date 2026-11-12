//go:build qualification

package production

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHarnessPortForwardCloseJoinsAndKills proves Close terminates the fake
// kubectl process, waits for it, and that both the started and terminated
// transitions actually happened.
func TestHarnessPortForwardCloseJoinsAndKills(t *testing.T) {
	marker := t.TempDir() + "/forward.log"
	t.Setenv("Q02_FAKE_MARKER", marker)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
trap 'echo terminated >> "$Q02_FAKE_MARKER"; exit 0' TERM INT
echo started >> "$Q02_FAKE_MARKER"
while :; do sleep 0.05; done
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	forward, err := StartPortForward(context.Background(), "kubectl", guard, ReleaseNamespace, "kodama", 18443, 8443)
	if err != nil {
		t.Fatalf("start port-forward: %v", err)
	}
	waitForMarker(t, marker, "started", 5*time.Second)

	pid := forward.PID()
	started := time.Now()
	if err := forward.Close(); err != nil {
		t.Fatalf("close port-forward: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("close took %s; the port-forward was not joined", elapsed)
	}
	waitForMarker(t, marker, "terminated", 5*time.Second)

	select {
	case <-forward.Done():
	case <-time.After(time.Second):
		t.Fatal("port-forward done channel was not closed by Close")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("port-forward process %d is still alive after Close", pid)
	} else if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("probe port-forward process %d: %v", pid, err)
	}
	if err := forward.Close(); err != nil {
		t.Fatalf("second Close is not idempotent: %v", err)
	}
}

// TestHarnessPortForwardCloseEscalates proves a port-forward that ignores TERM
// is still killed and joined within the grace bound.
func TestHarnessPortForwardCloseEscalates(t *testing.T) {
	marker := t.TempDir() + "/forward.log"
	t.Setenv("Q02_FAKE_MARKER", marker)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
trap '' TERM INT
echo started >> "$Q02_FAKE_MARKER"
while :; do sleep 0.05; done
`)
	guard, err := NewGuard("kodama-stage-k8s")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	forward, err := StartPortForward(context.Background(), "kubectl", guard, ReleaseNamespace, "kodama", 18444, 8443)
	if err != nil {
		t.Fatalf("start port-forward: %v", err)
	}
	forward.Grace = 300 * time.Millisecond
	waitForMarker(t, marker, "started", 5*time.Second)

	started := time.Now()
	if err := forward.Close(); err != nil {
		t.Fatalf("close stubborn port-forward: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close of a stubborn port-forward took %s", elapsed)
	}
	if err := syscall.Kill(forward.PID(), 0); err == nil {
		t.Fatalf("stubborn port-forward process %d survived Close", forward.PID())
	}
}

// waitForMarker waits until the marker file contains the wanted line.
func waitForMarker(t *testing.T, path, wanted string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), wanted) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("marker %q did not appear in %s within %s: %q", wanted, path, limit, data)
}
