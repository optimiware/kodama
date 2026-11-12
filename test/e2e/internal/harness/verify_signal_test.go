//go:build e2e

package harness

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHarnessVerifyGoTestTerminatesOnSignal proves a TERM or INT delivered to
// the verifier ends the run promptly, takes the wrapped command and its whole
// process group with it, and removes the temporary result file.
func TestHarnessVerifyGoTestTerminatesOnSignal(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{name: "term", signal: syscall.SIGTERM, code: 143},
		{name: "int", signal: syscall.SIGINT, code: 130},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runVerifySignalCase(t, testCase.signal, testCase.code)
		})
	}
}

// TestHarnessVerifyGoTestKillsTermIgnoringGroup proves cleanup decides on
// process-group liveness, not only on the job-leader subshell: a command and
// grandchild that ignore TERM/INT/HUP are still gone after the bounded
// escalation.
func TestHarnessVerifyGoTestKillsTermIgnoringGroup(t *testing.T) {
	verifier := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "verify-go-test.sh")
	for _, testCase := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{name: "term", signal: syscall.SIGTERM, code: 143},
		{name: "int", signal: syscall.SIGINT, code: 130},
		{name: "hup", signal: syscall.SIGHUP, code: 129},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tempDir := t.TempDir()
			pidFile := filepath.Join(t.TempDir(), "command.pid")
			groupFile := filepath.Join(t.TempDir(), "grandchild.pid")
			command := exec.Command(verifier,
				"--expect", "TestNeverFinishes",
				"--", "bash", "-c", `trap '' TERM INT HUP
printf '%s' "$$" > "$PIDFILE"
bash -c 'trap "" TERM INT HUP; exec sleep 30' &
printf '%s' "$!" > "$GROUPFILE"
while :; do sleep 0.2; done`)
			command.Env = append(os.Environ(),
				"TMPDIR="+tempDir,
				"PIDFILE="+pidFile,
				"GROUPFILE="+groupFile,
			)
			if err := command.Start(); err != nil {
				t.Fatalf("start verifier: %v", err)
			}
			waitForPIDFiles(t, command, pidFile, groupFile)
			started := time.Now()
			command.Process.Signal(testCase.signal)
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				code := 0
				if err != nil {
					exit, ok := err.(*exec.ExitError)
					if !ok {
						t.Fatalf("wait for verifier: %v", err)
					}
					code = exit.ExitCode()
				}
				if code != testCase.code {
					t.Fatalf("verifier exit = %d, want %d", code, testCase.code)
				}
				if elapsed := time.Since(started); elapsed > 8*time.Second {
					t.Fatalf("escalation took %s; want bounded", elapsed)
				}
			case <-time.After(8 * time.Second):
				_ = command.Process.Kill()
				t.Fatal("the verifier did not exit after the bounded escalation")
			}
			for _, path := range []string{pidFile, groupFile} {
				pid := readPID(t, path)
				if err := syscall.Kill(pid, 0); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("process %d survived the verifier cleanup", pid)
				} else if !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("probe process %d: %v", pid, err)
				}
			}
			matches, err := filepath.Glob(filepath.Join(tempDir, "verify-go-test*"))
			if err != nil {
				t.Fatalf("glob temp files: %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("verifier left temporary files: %v", matches)
			}
		})
	}
}

// TestHarnessVerifyGoTestRefusesFailedTee proves a tee failure cannot be read
// as success even when the command itself passed and a valid prefix was
// captured.
func TestHarnessVerifyGoTestRefusesFailedTee(t *testing.T) {
	verifier := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "verify-go-test.sh")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tee"), []byte("#!/bin/sh\ncat >/dev/null\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write failing tee: %v", err)
	}
	emitter := filepath.Join(t.TempDir(), "emit.sh")
	if err := os.WriteFile(emitter, []byte(`#!/bin/sh
printf '%s\n' '{"Action":"run","Test":"TestA"}'
printf '%s\n' '{"Action":"pass","Test":"TestA"}'
`), 0o700); err != nil {
		t.Fatalf("write emitter: %v", err)
	}
	command := exec.Command(verifier, "--expect", "TestA", "--", emitter)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	var stderr strings.Builder
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("the verifier accepted a failed tee")
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "tee failed") {
		t.Fatalf("verifier output %q does not name the tee failure", stderr.String())
	}
}

func waitForPIDFiles(t *testing.T, command *exec.Cmd, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, path := range paths {
		for {
			if data, err := os.ReadFile(path); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
					break
				}
			}
			if time.Now().After(deadline) {
				if command != nil && command.Process != nil {
					_ = command.Process.Kill()
				}
				t.Fatalf("pid file %s was not populated", path)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func runVerifySignalCase(t *testing.T, signal syscall.Signal, wantCode int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to run the verifier")
	}
	root := repoRootForTest(t)
	verifier := filepath.Join(root, "test", "e2e", "internal", "harness", "verify-go-test.sh")
	tempDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	groupFile := filepath.Join(t.TempDir(), "group.pid")
	command := exec.Command(verifier,
		"--expect", "TestNeverFinishes",
		"--", "bash", "-c", `printf '%s' "$$" > "$PIDFILE"
sleep 30 &
printf '%s' "$!" > "$GROUPFILE"
wait`)
	command.Env = append(os.Environ(),
		"TMPDIR="+tempDir,
		"PIDFILE="+pidFile,
		"GROUPFILE="+groupFile,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start verifier: %v", err)
	}
	// Wait for the wrapped command to record its identities before signalling.
	waitForPIDFiles(t, command, pidFile, groupFile)
	command.Process.Signal(signal)

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("wait for verifier: %v", err)
			}
			code = exit.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("verifier exit = %d, want %d", code, wantCode)
		}
	case <-time.After(8 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("the verifier did not exit promptly after a signal")
	}

	for _, path := range []string{pidFile, groupFile} {
		pid := readPID(t, path)
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d from the wrapped command survived the verifier signal", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("probe process %d: %v", pid, err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(tempDir, "verify-go-test.*"))
	if err != nil {
		t.Fatalf("glob verifier temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("verifier left temporary result files: %v", matches)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid pid %q in %s", data, path)
	}
	return pid
}
