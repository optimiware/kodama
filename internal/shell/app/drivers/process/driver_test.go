package processdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime/process"
)

// TestHelperProcess is re-executed as a harmless child by the spawn tests. It
// does nothing during a normal test run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("KODAMA_DRIVER_HELPER") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "echo-args":
		fmt.Println(strings.Join(os.Args[1:], "|"))
	case "env":
		fmt.Printf("HELPER_VAR=%s\n", os.Getenv("HELPER_VAR"))
		fmt.Printf("LEAKED=%s\n", os.Getenv("LEAKED"))
	case "output":
		fmt.Print(strings.Repeat("x", 4096))
		fmt.Fprint(os.Stderr, strings.Repeat("y", 4096))
	case "exit":
		os.Exit(7)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "write":
		fmt.Print("abcdef")
	}
	os.Exit(0)
}

func newTestDriver(t *testing.T, maxOutput int64) *Driver {
	t.Helper()
	driver, err := New(Config{StateDir: t.TempDir(), MaxOutputBytes: maxOutput})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	return driver
}

func helperRequest(t *testing.T, group string, mode string, args ...string) process.SpawnRequest {
	t.Helper()
	return helperRequestEnv(t, group, mode, nil, args...)
}

func helperRequestEnv(t *testing.T, group string, mode string, extraEnv []string, args ...string) process.SpawnRequest {
	t.Helper()
	command := []string{os.Args[0], "-test.run=TestHelperProcess"}
	command = append(command, args...)
	env := []string{"KODAMA_DRIVER_HELPER=1", "HELPER_MODE=" + mode}
	env = append(env, extraEnv...)
	return process.SpawnRequest{
		Group:      process.GroupID(group),
		Command:    command,
		Env:        env,
		Dir:        t.TempDir(),
		Root:       t.TempDir(),
		StdoutPath: filepath.Join(t.TempDir(), "stdout"),
		StderrPath: filepath.Join(t.TempDir(), "stderr"),
	}
}

func waitStatus(t *testing.T, driver *Driver, group process.GroupID, done func(process.GroupStatus) bool) process.GroupStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, err := driver.Status(context.Background(), group)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if done(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for process group %q; last status %+v", group, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSpawnExitAndBoundedLogs(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-exit", "write")
	result, err := driver.Spawn(context.Background(), request)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if result.Outcome != process.SpawnStarted {
		t.Fatalf("Spawn outcome = %q; want started", result.Outcome)
	}
	status := waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	if status.ExitCode != 0 {
		t.Fatalf("exit code = %d; want 0", status.ExitCode)
	}

	page, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if string(page.Data) != "abc" || !page.Truncated {
		t.Fatalf("first page = %q truncated=%v; want abc/true", page.Data, page.Truncated)
	}
	page, err = driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 3, Limit: 16,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if string(page.Data) != "def" || page.Truncated {
		t.Fatalf("second page = %q truncated=%v; want def/false", page.Data, page.Truncated)
	}
}

func TestSpawnNonZeroExit(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-exit-7", "exit")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	status := waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	if status.ExitCode != 7 {
		t.Fatalf("exit code = %d; want 7", status.ExitCode)
	}
}

func TestSpawnCancelledBeforeEffect(t *testing.T) {
	driver := newTestDriver(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := helperRequest(t, "group-cancelled", "write")
	result, err := driver.Spawn(ctx, request)
	if err == nil {
		t.Fatal("Spawn with a canceled context succeeded")
	}
	if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
		t.Fatalf("fault class = %q; want deadline_expired", class)
	}
	if result.Outcome != process.SpawnRefused {
		t.Fatalf("outcome = %q; want refused", result.Outcome)
	}
	status, err := driver.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.NeverCreated {
		t.Fatal("a canceled spawn reported affirmative never-created evidence")
	}
	if status.Present || status.MembersAlive || status.GroupGone {
		t.Fatalf("canceled spawn produced process evidence: %+v", status)
	}
}

func TestSpawnAmbiguousStartIsUncertain(t *testing.T) {
	driver := newTestDriver(t, 0)
	// The hook really starts the child and then reports a lost response, so a
	// process exists while the start call fails.
	driver.start = func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		return errors.New("injected lost start response")
	}
	request := helperRequest(t, "group-ambiguous", "write")
	result, err := driver.Spawn(context.Background(), request)
	if err == nil {
		t.Fatal("ambiguous spawn returned no error")
	}
	if class, _ := fault.ClassOf(err); class != fault.UncertainExternalEffect {
		t.Fatalf("fault class = %q; want uncertain_external_effect", class)
	}
	if result.Outcome != process.SpawnUncertain {
		t.Fatalf("outcome = %q; want uncertain", result.Outcome)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	status, err := driver.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.NeverCreated {
		t.Fatal("an ambiguous start collapsed into affirmative never-created evidence")
	}
}

func TestSpawnBoundsOutput(t *testing.T) {
	driver := newTestDriver(t, 64)
	request := helperRequest(t, "group-bounded", "output")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	for _, path := range []string{request.StdoutPath, request.StderrPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q): %v", path, err)
		}
		if info.Size() > 64 {
			t.Fatalf("captured stream %q has %d bytes; want at most 64", path, info.Size())
		}
	}
	page, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: 1024,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !page.Truncated {
		t.Fatal("a bounded capture did not report truncation")
	}
}

func TestSpawnNeverInterpretsShell(t *testing.T) {
	driver := newTestDriver(t, 0)
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	request := helperRequest(t, "group-no-shell", "echo-args",
		"--", "$(touch "+marker+")", "a;b", "`echo injected`")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	page, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: MaxLogReadBytes,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	output := string(page.Data)
	for _, literal := range []string{"$(touch " + marker + ")", "a;b", "`echo injected`"} {
		if !strings.Contains(output, literal) {
			t.Fatalf("literal argument %q was interpreted; output %q", literal, output)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell substitution created %q (err=%v)", marker, err)
	}
}

func TestSpawnUsesExplicitEnvironment(t *testing.T) {
	driver := newTestDriver(t, 0)
	t.Setenv("LEAKED", "parent-value")
	request := helperRequestEnv(t, "group-env", "env", []string{"HELPER_VAR=child-value"})
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	page, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: MaxLogReadBytes,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	output := string(page.Data)
	if !strings.Contains(output, "HELPER_VAR=child-value") {
		t.Fatalf("explicit environment missing; output %q", output)
	}
	if strings.Contains(output, "parent-value") {
		t.Fatalf("parent environment leaked into the child; output %q", output)
	}
}

// TestSpawnEmptyEnvironmentDoesNotInheritHost pins that a nil or empty request
// environment yields a child with an empty environment. os/exec treats a nil
// Cmd.Env as "inherit the parent environment", so the driver must always pass
// a non-nil slice; otherwise an empty request leaks host secrets.
func TestSpawnEmptyEnvironmentDoesNotInheritHost(t *testing.T) {
	envCommand, err := exec.LookPath("env")
	if err != nil {
		t.Fatalf("look up env executable: %v", err)
	}
	t.Setenv("LEAKED", "host-secret")
	cases := []struct {
		name string
		env  []string
	}{
		{name: "nil", env: nil},
		{name: "empty", env: []string{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			driver := newTestDriver(t, 0)
			request := process.SpawnRequest{
				Group:      process.GroupID("group-empty-env-" + testCase.name),
				Command:    []string{envCommand},
				Env:        testCase.env,
				Dir:        t.TempDir(),
				Root:       t.TempDir(),
				StdoutPath: filepath.Join(t.TempDir(), "stdout"),
				StderrPath: filepath.Join(t.TempDir(), "stderr"),
			}
			if _, err := driver.Spawn(context.Background(), request); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
				return status.GroupGone && status.LeaderExited
			})
			page, err := driver.Logs(context.Background(), process.GroupLogsRequest{
				Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: MaxLogReadBytes,
			})
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			output := string(page.Data)
			if strings.Contains(output, "LEAKED=") {
				t.Fatalf("host environment leaked into the child; output %q", output)
			}
			if strings.TrimSpace(output) != "" {
				t.Fatalf("child environment is not empty; output %q", output)
			}
		})
	}
}

func TestSignalGroupAndRemove(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-signal", "sleep")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.Present || status.MembersAlive
	})
	if err := driver.SignalGroup(context.Background(), request.Group); err != nil {
		t.Fatalf("SignalGroup: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	if err := driver.SignalGroup(context.Background(), request.Group); err != nil {
		t.Fatalf("SignalGroup on a terminated group: %v", err)
	}
	if err := driver.RemoveGroup(context.Background(), request.Group); err != nil {
		t.Fatalf("RemoveGroup: %v", err)
	}
	if err := driver.RemoveGroup(context.Background(), request.Group); err != nil {
		t.Fatalf("second RemoveGroup: %v", err)
	}
	status, err := driver.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status after removal: %v", err)
	}
	if status.Present || status.MembersAlive || status.GroupGone || status.NeverCreated || status.LeaderExited {
		t.Fatalf("removed group still reports evidence: %+v", status)
	}
}

func TestSpawnDuplicateReusesGroup(t *testing.T) {
	driver := newTestDriver(t, 0)
	var starts atomic.Int32
	driver.start = func(cmd *exec.Cmd) error {
		starts.Add(1)
		return cmd.Start()
	}
	request := helperRequest(t, "group-duplicate", "write")
	first, err := driver.Spawn(context.Background(), request)
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	second, err := driver.Spawn(context.Background(), request)
	if err != nil {
		t.Fatalf("second Spawn: %v", err)
	}
	if first.Outcome != process.SpawnStarted || second.Outcome != process.SpawnStarted {
		t.Fatalf("outcomes = %q/%q; want started/started", first.Outcome, second.Outcome)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("start calls = %d; want 1", got)
	}
}

func TestCloseJoinsProcessWaiters(t *testing.T) {
	driver, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := helperRequest(t, "group-close", "sleep")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.Present || status.MembersAlive
	})
	if err := driver.SignalGroup(context.Background(), request.Group); err != nil {
		t.Fatalf("SignalGroup: %v", err)
	}
	if err := driver.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	status, err := driver.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status after Close: %v", err)
	}
	if !status.LeaderExited || !status.GroupGone {
		t.Fatalf("Close returned before the waiter recorded the exit: %+v", status)
	}
	if err := driver.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSpawnRefusedPersistsAffirmativeEvidence(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-refused", "write")
	request.Command = []string{filepath.Join(t.TempDir(), "does-not-exist")}
	result, err := driver.Spawn(context.Background(), request)
	if err == nil {
		t.Fatal("Spawn with a missing executable succeeded")
	}
	if result.Outcome != process.SpawnRefused {
		t.Fatalf("outcome = %q; want refused", result.Outcome)
	}
	status, err := driver.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.NeverCreated {
		t.Fatalf("refused spawn did not report affirmative never-created evidence: %+v", status)
	}
}

func TestStatusWithoutBookkeepingProvesNothing(t *testing.T) {
	driver := newTestDriver(t, 0)
	status, err := driver.Status(context.Background(), "unknown-group")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Present || status.NeverCreated || status.LeaderExited || status.MembersAlive || status.GroupGone {
		t.Fatalf("unknown group reported evidence: %+v", status)
	}
}

func TestSpawnRejectsInvalidRequest(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-invalid", "write")
	request.Command = nil
	result, err := driver.Spawn(context.Background(), request)
	if err == nil {
		t.Fatal("Spawn with no command succeeded")
	}
	if class, _ := fault.ClassOf(err); class != fault.InvalidInput {
		t.Fatalf("fault class = %q; want invalid_input", class)
	}
	if result.Outcome != process.SpawnRefused {
		t.Fatalf("outcome = %q; want refused", result.Outcome)
	}

	request = helperRequest(t, "group-invalid", "write")
	request.Dir = "relative"
	if _, err := driver.Spawn(context.Background(), request); err == nil {
		t.Fatal("Spawn with a relative working directory succeeded")
	}

	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no state directory succeeded")
	}
	if _, err := New(Config{StateDir: t.TempDir(), MaxOutputBytes: -1}); err == nil {
		t.Fatal("New with a negative output bound succeeded")
	}
}

func TestLogsRejectsUnboundedReads(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-logs-invalid", "write")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: 0,
	}); err == nil {
		t.Fatal("Logs with a zero limit succeeded")
	}
	if _, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: -1, Limit: 10,
	}); err == nil {
		t.Fatal("Logs with a negative offset succeeded")
	}
	if _, err := driver.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: "other", Offset: 0, Limit: 10,
	}); err == nil {
		t.Fatal("Logs with an unknown stream succeeded")
	}
}

func TestStateSurvivesDriverReconstruction(t *testing.T) {
	stateDir := t.TempDir()
	first, err := New(Config{StateDir: stateDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := helperRequest(t, "group-restart", "write")
	if _, err := first.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, first, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := New(Config{StateDir: stateDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = second.Close(context.Background()) }()
	status, err := second.Status(context.Background(), request.Group)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.GroupGone || !status.LeaderExited || status.ExitCode != 0 {
		t.Fatalf("reconstructed driver lost process evidence: %+v", status)
	}
	page, err := second.Logs(context.Background(), process.GroupLogsRequest{
		Group: request.Group, Stream: engine.LogStdout, Offset: 0, Limit: 16,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if string(page.Data) != "abcdef" {
		t.Fatalf("reconstructed driver logs = %q; want abcdef", page.Data)
	}
}

func TestParseProcStatStartTime(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want uint64
	}{
		{
			name: "ordinary comm",
			stat: "101 (sh) S 1 101 101 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 123456789 4096 1",
			want: 123456789,
		},
		{
			name: "comm containing spaces and closing parens",
			stat: "202 (weird ) name (v2)) R 1 202 202 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 987654321 4096 1",
			want: 987654321,
		},
		{
			name: "missing closing paren",
			stat: "202 (unterminated S 1 202",
			want: 0,
		},
		{
			name: "truncated fields",
			stat: "202 (sh) S 1 202 202",
			want: 0,
		},
		{
			name: "non numeric start time",
			stat: "202 (sh) S 1 202 202 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 bad 4096",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseProcStatStartTime(tc.stat); got != tc.want {
				t.Fatalf("parseProcStatStartTime = %d; want %d", got, tc.want)
			}
		})
	}
}

func startUnrelatedProcessGroup(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = []string{"KODAMA_DRIVER_HELPER=1", "HELPER_MODE=sleep"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start unrelated sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func TestRecycledPIDIsNeverSignaledAndReportsGone(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, group process.GroupID, victimPID int) record
	}{
		{
			name: "persisted group_gone",
			build: func(_ *testing.T, group process.GroupID, victimPID int) record {
				return record{
					Group:        group,
					State:        stateStarted,
					PID:          victimPID,
					LeaderExited: true,
					GroupGone:    true,
					ExitCode:     0,
				}
			},
		},
		{
			name: "leader reaped and pid recycled",
			build: func(_ *testing.T, group process.GroupID, victimPID int) record {
				return record{
					Group:        group,
					State:        stateStarted,
					PID:          victimPID,
					LeaderExited: true,
					ExitCode:     0,
				}
			},
		},
		{
			name: "start time mismatch on recycled pid",
			build: func(t *testing.T, group process.GroupID, victimPID int) record {
				if runtime.GOOS != "linux" {
					t.Skip("start-time PID reuse guard uses /proc/<pid>/stat on Linux")
				}
				victimStart := processStartTime(victimPID)
				if victimStart == 0 {
					t.Fatalf("processStartTime(%d) = 0 on Linux", victimPID)
				}
				return record{
					Group:     group,
					State:     stateUnknown,
					PID:       victimPID,
					StartTime: victimStart + 1,
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			victim := startUnrelatedProcessGroup(t)
			victimPID := victim.Process.Pid

			stateDir := t.TempDir()
			group := process.GroupID("group-recycled")
			seeded := tc.build(t, group, victimPID)
			encoded, err := json.Marshal(seeded)
			if err != nil {
				t.Fatalf("marshal record: %v", err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, string(group)+stateSuffix), encoded, 0o600); err != nil {
				t.Fatalf("seed state record: %v", err)
			}

			driver, err := New(Config{StateDir: stateDir})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() { _ = driver.Close(context.Background()) }()

			if err := driver.SignalGroup(context.Background(), group); err != nil {
				t.Fatalf("SignalGroup: %v", err)
			}
			// Victim must remain alive and untouched.
			if err := syscall.Kill(victimPID, 0); err != nil {
				t.Fatalf("recycled PID %d was killed by SignalGroup: %v", victimPID, err)
			}

			status, err := driver.Status(context.Background(), group)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.Present || status.MembersAlive || !status.GroupGone {
				t.Fatalf("Status for recycled PID = %+v; want GroupGone without Present/MembersAlive", status)
			}

			if err := driver.RemoveGroup(context.Background(), group); err != nil {
				t.Fatalf("RemoveGroup: %v", err)
			}
			if err := syscall.Kill(victimPID, 0); err != nil {
				t.Fatalf("recycled PID %d was affected by RemoveGroup: %v", victimPID, err)
			}
		})
	}
}

func TestSignalGroupConcurrentWithExitHasNoRace(t *testing.T) {
	driver := newTestDriver(t, 0)
	request := helperRequest(t, "group-race-signal", "write")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = driver.SignalGroup(context.Background(), request.Group)
			_, _ = driver.Status(context.Background(), request.Group)
		})
	}
	wg.Wait()
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
}

func TestPersistSyncsStateDirectory(t *testing.T) {
	stateDir := t.TempDir()
	driver, err := New(Config{StateDir: stateDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	var synced atomic.Int32
	driver.syncDir = func(dir string) error {
		if dir == stateDir {
			synced.Add(1)
		}
		return syncDirectory(dir)
	}
	request := helperRequest(t, "group-sync", "write")
	if _, err := driver.Spawn(context.Background(), request); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitStatus(t, driver, request.Group, func(status process.GroupStatus) bool {
		return status.GroupGone && status.LeaderExited
	})
	if synced.Load() < 2 {
		t.Fatalf("state directory sync count = %d; want at least 2", synced.Load())
	}
}
