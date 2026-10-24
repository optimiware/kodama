package entrypoint

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func baseSpec(t *testing.T, out *bytes.Buffer, argv []string) procSpec {
	return procSpec{argv: argv, dir: t.TempDir(), env: []string{"PATH=/usr/bin:/bin"}, out: out, grace: 500 * time.Millisecond}
}

func TestRunProcessExitCodes(t *testing.T) {
	var out bytes.Buffer
	res := runProcess(context.Background(), baseSpec(t, &out, sh("echo hi; echo err >&2; exit 3")))
	if !res.started || res.exitCode == nil || *res.exitCode != 3 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "hi") || !strings.Contains(out.String(), "err") {
		t.Fatalf("combined output = %q", out.String())
	}
	res = runProcess(context.Background(), baseSpec(t, &out, []string{"/nonexistent/binary"}))
	if res.started || res.startErr == nil {
		t.Fatalf("expected start error, got %+v", res)
	}
}

func TestRunProcessExplicitEnvOnly(t *testing.T) {
	t.Setenv("KODAMA_TEST_LEAK", "leak")
	var out bytes.Buffer
	spec := baseSpec(t, &out, sh(`echo "leak=[$KODAMA_TEST_LEAK] x=[$X]"`))
	spec.env = append(spec.env, "X=1")
	runProcess(context.Background(), spec)
	if got := strings.TrimSpace(out.String()); got != "leak=[] x=[1]" {
		t.Fatalf("env = %q", got)
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s never appeared", path)
	return 0
}

// A command that spawns a grandchild and then ignores SIGTERM must lose the
// whole tree on cancellation: SIGTERM first, SIGKILL after the grace period.
func TestRunProcessCancelKillsProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := `trap '' TERM; (trap '' TERM; echo $$ > ` + pidFile + `; while :; do sleep 1; done) & while :; do sleep 1; done`
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan procResult, 1)
	go func() { done <- runProcess(ctx, baseSpec(t, &out, sh(script))) }()
	grandchild := readPID(t, pidFile)
	if !alive(grandchild) {
		t.Fatal("grandchild did not start")
	}
	start := time.Now()
	cancel()
	select {
	case res := <-done:
		if !res.started {
			t.Fatalf("result = %+v", res)
		}
		if res.exitCode != nil {
			t.Fatalf("killed child must have no exit code, got %d", *res.exitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runProcess did not return after cancellation")
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatalf("SIGKILL came before the grace period elapsed (%v)", time.Since(start))
	}
	waitDead(t, grandchild)
}

func TestRunProcessTermIsEnoughWhenHonored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	spec := baseSpec(t, &out, sh("sleep 30"))
	spec.grace = 5 * time.Second
	start := time.Now()
	runProcess(ctx, spec)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; SIGTERM should have ended the command", d)
	}
}

// Grandchildren that outlive a command which exits normally must not survive.
func TestRunProcessReapsOrphansAfterNormalExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	var out bytes.Buffer
	res := runProcess(context.Background(), baseSpec(t, &out, sh(`(sleep 60 >/dev/null 2>&1 & echo $! > `+pidFile+`); exit 0`)))
	if res.exitCode == nil || *res.exitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	waitDead(t, readPID(t, pidFile))
}

func TestBoundedLogTruncates(t *testing.T) {
	const limit = 1024
	b, err := newBoundedLog(t.TempDir(), "log-*", limit)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if n, err := b.Write(bytes.Repeat([]byte("x"), 100)); err != nil || n != 100 {
			t.Fatalf("Write = %d, %v: writes must always be accepted", n, err)
		}
	}
	path, size, err := b.finish(limit)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if int64(len(data)) != size || size > limit {
		t.Fatalf("file %d bytes, reported %d, limit %d", len(data), size, limit)
	}
	if !strings.HasSuffix(string(data), truncationMarker) {
		t.Fatalf("missing truncation marker: %q", data[len(data)-40:])
	}
}

func TestBoundedLogUntruncatedHasNoMarker(t *testing.T) {
	b, _ := newBoundedLog(t.TempDir(), "log-*", 1024)
	b.Write([]byte("small"))
	path, size, _ := b.finish(1024)
	data, _ := os.ReadFile(path)
	if string(data) != "small" || size != 5 {
		t.Fatalf("data = %q size %d", data, size)
	}
}

func TestRunProcessLargeOutputIsBounded(t *testing.T) {
	b, _ := newBoundedLog(t.TempDir(), "log-*", 4096)
	spec := procSpec{argv: sh("head -c 5000000 /dev/zero | tr '\\0' a"), dir: t.TempDir(), env: []string{"PATH=/usr/bin:/bin"}, out: b, grace: time.Second}
	res := runProcess(context.Background(), spec)
	if res.exitCode == nil || *res.exitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	_, size, _ := b.finish(4096)
	if size > 4096 {
		t.Fatalf("log is %d bytes", size)
	}
}
