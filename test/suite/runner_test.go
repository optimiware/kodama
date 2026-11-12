package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// scriptedExecutor records the lanes it runs. Lanes named gate-* block until
// every gate lane of their wave is running, which only concurrent lanes can do.
type scriptedExecutor struct {
	gate     sync.WaitGroup
	fail     map[string]int
	unstart  map[string]error
	mu       sync.Mutex
	order    []string
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (executor *scriptedExecutor) RunLane(_ context.Context, lane Lane, output io.Writer) (int, error) {
	now := executor.inFlight.Add(1)
	defer executor.inFlight.Add(-1)
	for {
		seen := executor.peak.Load()
		if now <= seen || executor.peak.CompareAndSwap(seen, now) {
			break
		}
	}
	if strings.HasPrefix(lane.Name, "gate-") {
		executor.gate.Done()
		executor.gate.Wait()
	}
	fmt.Fprintf(output, "ran %s\npartial line of %s", lane.Name, lane.Name)
	executor.mu.Lock()
	executor.order = append(executor.order, lane.Name)
	executor.mu.Unlock()
	if err := executor.unstart[lane.Name]; err != nil {
		return 0, err
	}
	return executor.fail[lane.Name], nil
}

func twoWavePlan() Plan {
	return Plan{Target: TargetStage, Waves: []Wave{
		{Name: "wave-1", Lanes: []Lane{{Name: "gate-a"}, {Name: "gate-b"}, {Name: "gate-c"}}},
		{Name: "wave-2", Lanes: []Lane{{Name: "chaos"}}},
	}}
}

// TestRunPlanRunsLanesTogetherAndWavesInOrder proves the lanes of a wave run
// concurrently, the next wave starts only after the previous one finished, and
// every lane's output reaches the shared output and its private log whole.
func TestRunPlanRunsLanesTogetherAndWavesInOrder(t *testing.T) {
	executor := &scriptedExecutor{}
	executor.gate.Add(3)
	var output bytes.Buffer
	logs := filepath.Join(t.TempDir(), "logs")
	report, err := RunPlan(context.Background(), twoWavePlan(), executor, RunOptions{Output: &output, LogDir: logs})
	if err != nil || !report.Passed {
		t.Fatalf("RunPlan = %+v, %v; want a passing report", report, err)
	}
	if peak := executor.peak.Load(); peak != 3 {
		t.Fatalf("peak concurrent lanes = %d; want the 3 lanes of wave 1 together", peak)
	}
	if last := executor.order[len(executor.order)-1]; last != "chaos" || len(executor.order) != 4 {
		t.Fatalf("lane order = %v; want the chaos lane last, after wave 1", executor.order)
	}
	if len(report.Waves) != 2 || len(report.Waves[0].Lanes) != 3 || report.Waves[1].Lanes[0].Name != "chaos" {
		t.Fatalf("report waves = %+v", report.Waves)
	}
	for _, line := range []string{"[gate-a] ran gate-a\n", "[gate-b] partial line of gate-b\n", "[chaos] ran chaos\n"} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("shared output lacks the whole line %q:\n%s", line, output.String())
		}
	}
	logPath := filepath.Join(logs, "gate-a.log")
	if report.Waves[0].Lanes[0].Log != logPath {
		t.Errorf("gate-a log path = %q; want %q", report.Waves[0].Lanes[0].Log, logPath)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || string(data) != "ran gate-a\npartial line of gate-a" {
		t.Errorf("gate-a log = %q, %v; want the lane's exact output", data, err)
	}
	if info, err := os.Stat(logPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("gate-a log is not a private file: %v", err)
	}
}

// TestRunPlanStopsBeforeTheNextWaveAfterAFailure proves a failed wave keeps
// the later waves from starting unless the caller asked to keep going.
func TestRunPlanStopsBeforeTheNextWaveAfterAFailure(t *testing.T) {
	executor := &scriptedExecutor{fail: map[string]int{"gate-b": 3}}
	executor.gate.Add(3)
	report, err := RunPlan(context.Background(), twoWavePlan(), executor, RunOptions{})
	if err == nil || report.Passed {
		t.Fatalf("RunPlan passed although gate-b failed: %+v", report)
	}
	if slices.Contains(executor.order, "chaos") || !slices.Equal(report.SkippedWaves, []string{"wave-2"}) {
		t.Fatalf("after a failed wave 1: ran %v, skipped %v; want wave-2 not started", executor.order, report.SkippedWaves)
	}
	failed := report.Waves[0].Lanes[1]
	if failed.Name != "gate-b" || failed.Passed || failed.ExitCode != 3 || !report.Waves[0].Lanes[0].Passed {
		t.Fatalf("wave 1 lane results = %+v; want only gate-b failed with exit 3", report.Waves[0].Lanes)
	}

	executor = &scriptedExecutor{unstart: map[string]error{"gate-a": errors.New("no such command")}}
	executor.gate.Add(3)
	report, err = RunPlan(context.Background(), twoWavePlan(), executor, RunOptions{KeepGoing: true})
	if err == nil || report.Passed {
		t.Fatalf("RunPlan passed although gate-a could not start: %+v", report)
	}
	if !slices.Contains(executor.order, "chaos") || len(report.SkippedWaves) != 0 {
		t.Fatalf("with keep-going: ran %v, skipped %v; want wave-2 to run", executor.order, report.SkippedWaves)
	}
	if unstarted := report.Waves[0].Lanes[0]; unstarted.Passed || unstarted.Error != "no such command" {
		t.Fatalf("unstarted lane result = %+v; want its error recorded", unstarted)
	}
}

// TestWriteReportIsPrivateAndCarriesNoEnvironment proves the summary is a
// private file holding verdicts and timings only.
func TestWriteReportIsPrivateAndCarriesNoEnvironment(t *testing.T) {
	plan := Plan{Target: TargetPodman, Waves: []Wave{{Name: "wave-1", Lanes: []Lane{{
		Name: "lane", Argv: []string{"command"}, Env: []string{"Q01_ADMIN_TOKEN_FILE=/private/admin-token"},
	}}}}}
	report, err := RunPlan(context.Background(), plan, &scriptedExecutor{}, RunOptions{})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "evidence", "summary.json")
	if err := WriteReport(path, report); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("summary is not a private file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the summary: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil || !decoded.Passed || decoded.Waves[0].Lanes[0].Name != "lane" {
		t.Fatalf("summary does not decode to the report: %v\n%s", err, data)
	}
	if strings.Contains(string(data), "admin-token") {
		t.Fatalf("summary carries a lane environment:\n%s", data)
	}
}

// TestOSExecutorRunsACommandWithTheLaneEnvironment proves the real executor
// passes the lane settings, captures both streams and returns the exit code.
func TestOSExecutorRunsACommandWithTheLaneEnvironment(t *testing.T) {
	var output bytes.Buffer
	lane := Lane{
		Name: "shell",
		Argv: []string{"sh", "-c", `echo "out=$SUITE_TEST_VALUE"; echo "err=$(pwd -P)" >&2; exit 7`},
		Env:  []string{"SUITE_TEST_VALUE=from-the-lane"},
	}
	directory := t.TempDir()
	code, err := OSExecutor{Dir: directory}.RunLane(context.Background(), lane, &output)
	if err != nil || code != 7 {
		t.Fatalf("RunLane = exit %d, %v; want exit 7", code, err)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for _, want := range []string{"out=from-the-lane\n", "err=" + resolved + "\n"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output %q lacks %q", output.String(), want)
		}
	}
	if _, err := (OSExecutor{}).RunLane(context.Background(), Lane{Name: "absent", Argv: []string{"/nonexistent/suite-command"}}, io.Discard); err == nil {
		t.Error("a command that cannot start returned no error")
	}
	if _, err := (OSExecutor{}).RunLane(context.Background(), Lane{Name: "empty"}, io.Discard); err == nil {
		t.Error("a lane without a command returned no error")
	}
}

// TestOSExecutorStopsTheWholeProcessGroup proves an interrupted lane takes its
// descendants with it: a background child of the command is gone once RunLane
// returns.
func TestOSExecutorStopsTheWholeProcessGroup(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := OSExecutor{}.RunLane(ctx, Lane{
			Name: "group",
			Argv: []string{"sh", "-c", `sleep 300 & echo "$!"; wait`},
		}, writer)
		writer.Close()
		done <- err
	}()
	line := make([]byte, 32)
	count, err := reader.Read(line)
	if err != nil {
		t.Fatalf("read the background child's pid: %v", err)
	}
	go io.Copy(io.Discard, reader)
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:count])))
	if err != nil || child <= 1 {
		t.Fatalf("background child pid = %q: %v", line[:count], err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(interruptGrace):
		t.Fatal("RunLane did not return after its context ended")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(child, syscall.SIGKILL)
			t.Fatalf("the lane's background child %d survived the interrupt", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
