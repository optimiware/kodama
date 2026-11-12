package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// interruptGrace is how long an interrupted lane gets to stop after SIGTERM
// before its process group is killed.
const interruptGrace = 30 * time.Second

// Executor runs one lane and returns its exit code. A lane that could not be
// started returns an error.
type Executor interface {
	RunLane(ctx context.Context, lane Lane, output io.Writer) (int, error)
}

// OSExecutor runs a lane's command directly, without a shell, in Dir with the
// process environment plus the lane's settings.
type OSExecutor struct {
	Dir string
}

// RunLane runs the lane in its own process group. When ctx ends, the whole
// group receives SIGTERM, then SIGKILL after interruptGrace, so an interrupted
// run leaves no go test binary or helper behind.
func (executor OSExecutor) RunLane(ctx context.Context, lane Lane, output io.Writer) (int, error) {
	if len(lane.Argv) == 0 {
		return 0, errors.New("the lane has no command")
	}
	command := exec.CommandContext(ctx, lane.Argv[0], lane.Argv[1:]...)
	command.Dir = executor.Dir
	command.Env = append(os.Environ(), lane.Env...)
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
	command.WaitDelay = interruptGrace
	err := command.Run()
	if ctx.Err() != nil && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	case errors.Is(err, exec.ErrWaitDelay):
		// The command exited; a process it left behind still held its output.
		return command.ProcessState.ExitCode(), nil
	default:
		return 0, err
	}
}

// LaneResult is the outcome of one lane.
type LaneResult struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Passed     bool      `json:"passed"`
	// Error names a lane that could not be started.
	Error string `json:"error,omitempty"`
	// Log is the file holding the lane's complete output.
	Log string `json:"log,omitempty"`
}

// WaveResult is the outcome of one wave.
type WaveResult struct {
	Name       string       `json:"name"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	DurationMS int64        `json:"duration_ms"`
	Passed     bool         `json:"passed"`
	Lanes      []LaneResult `json:"lanes"`
}

// Report is the outcome of one plan. It records names, timings and verdicts
// only: never a command environment.
type Report struct {
	Target     Target       `json:"target"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	DurationMS int64        `json:"duration_ms"`
	Passed     bool         `json:"passed"`
	Waves      []WaveResult `json:"waves"`
	// SkippedWaves names the waves a failed earlier wave kept from starting.
	SkippedWaves []string `json:"skipped_waves,omitempty"`
}

// RunOptions controls one plan run.
type RunOptions struct {
	// KeepGoing starts the remaining waves after a failed one. By default a
	// failed wave stops the run, so the chaos wave never disrupts a stage that
	// already failed its baseline.
	KeepGoing bool
	// Output receives every lane's output, line by line, prefixed with the
	// lane name, and the progress lines. Nil discards it.
	Output io.Writer
	// LogDir, when set, receives one private <lane>.log per lane.
	LogDir string
}

// RunPlan runs the waves in order and every lane of a wave concurrently. It
// returns an error when a lane failed or could not be started; the report is
// complete either way.
func RunPlan(ctx context.Context, plan Plan, executor Executor, options RunOptions) (Report, error) {
	output := &lineWriter{out: options.Output}
	if options.Output == nil {
		output.out = io.Discard
	}
	if options.LogDir != "" {
		if err := os.MkdirAll(options.LogDir, 0o700); err != nil {
			return Report{}, fmt.Errorf("create the lane log directory: %w", err)
		}
	}
	report := Report{Target: plan.Target, StartedAt: time.Now().UTC(), Passed: true}
	for index, wave := range plan.Waves {
		output.printf("suite: wave %s: starting %d lane(s)\n", wave.Name, len(wave.Lanes))
		result := WaveResult{
			Name:      wave.Name,
			StartedAt: time.Now().UTC(),
			Passed:    true,
			Lanes:     make([]LaneResult, len(wave.Lanes)),
		}
		var group sync.WaitGroup
		for laneIndex, lane := range wave.Lanes {
			group.Add(1)
			go func() {
				defer group.Done()
				result.Lanes[laneIndex] = runLane(ctx, lane, executor, output, options.LogDir)
			}()
		}
		group.Wait()
		result.FinishedAt = time.Now().UTC()
		result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		for _, lane := range result.Lanes {
			result.Passed = result.Passed && lane.Passed
		}
		report.Waves = append(report.Waves, result)
		report.Passed = report.Passed && result.Passed
		output.printf("suite: wave %s: %s in %s\n", wave.Name, verdict(result.Passed, 0),
			result.FinishedAt.Sub(result.StartedAt).Round(100*time.Millisecond))
		if result.Passed || index == len(plan.Waves)-1 {
			continue
		}
		if !options.KeepGoing || ctx.Err() != nil {
			for _, skipped := range plan.Waves[index+1:] {
				report.SkippedWaves = append(report.SkippedWaves, skipped.Name)
			}
			break
		}
	}
	report.FinishedAt = time.Now().UTC()
	report.DurationMS = report.FinishedAt.Sub(report.StartedAt).Milliseconds()
	switch {
	case report.Passed:
		return report, nil
	case ctx.Err() != nil:
		return report, fmt.Errorf("the run was interrupted: %w", ctx.Err())
	case len(report.SkippedWaves) > 0:
		return report, fmt.Errorf("a lane failed; wave(s) %v did not start (--keep-going starts them anyway)", report.SkippedWaves)
	default:
		return report, errors.New("a lane failed")
	}
}

func runLane(ctx context.Context, lane Lane, executor Executor, output *lineWriter, logDir string) LaneResult {
	result := LaneResult{Name: lane.Name, StartedAt: time.Now().UTC()}
	prefixed := output.lane(lane.Name)
	var sink io.Writer = prefixed
	var closeLog func() error
	if logDir != "" {
		result.Log = filepath.Join(logDir, lane.Name+".log")
		file, err := os.OpenFile(result.Log, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			result.Error = fmt.Sprintf("open the lane log: %v", err)
			result.Log = ""
		} else {
			sink = io.MultiWriter(file, prefixed)
			closeLog = file.Close
		}
	}
	if result.Error == "" {
		code, err := executor.RunLane(ctx, lane, sink)
		result.ExitCode = code
		if err != nil {
			result.Error = err.Error()
		}
		result.Passed = err == nil && code == 0
	}
	prefixed.flushPending()
	if closeLog != nil {
		_ = closeLog()
	}
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	status := verdict(result.Passed, result.ExitCode)
	if result.Error != "" {
		status = "FAIL (" + result.Error + ")"
	}
	output.printf("suite: lane %-36s %s in %s\n", lane.Name, status,
		result.FinishedAt.Sub(result.StartedAt).Round(100*time.Millisecond))
	return result
}

func verdict(passed bool, exitCode int) string {
	switch {
	case passed:
		return "PASS"
	case exitCode != 0:
		return fmt.Sprintf("FAIL (exit %d)", exitCode)
	default:
		return "FAIL"
	}
}

// WriteReport writes the report as private JSON through a temporary file and a
// rename, so a reader never sees a partial summary.
func WriteReport(path string, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the summary: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the summary directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".summary-*")
	if err != nil {
		return fmt.Errorf("create the temporary summary: %w", err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write the summary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write the summary: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("publish the summary: %w", err)
	}
	return nil
}

// lineWriter serializes whole lines from concurrent lanes onto one writer.
type lineWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (writer *lineWriter) printf(format string, arguments ...any) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	fmt.Fprintf(writer.out, format, arguments...)
}

// lane returns a writer that prefixes each complete line with the lane name.
func (writer *lineWriter) lane(name string) *laneWriter {
	return &laneWriter{parent: writer, prefix: "[" + name + "] "}
}

// laneWriter buffers one lane's output up to the next line break. It has one
// writer at a time: os/exec serializes a command's shared stdout and stderr.
type laneWriter struct {
	parent  *lineWriter
	prefix  string
	pending []byte
}

func (writer *laneWriter) Write(data []byte) (int, error) {
	writer.pending = append(writer.pending, data...)
	for {
		end := bytes.IndexByte(writer.pending, '\n')
		if end < 0 {
			return len(data), nil
		}
		writer.emit(writer.pending[:end+1])
		writer.pending = writer.pending[end+1:]
	}
}

// flushPending emits an unterminated last line.
func (writer *laneWriter) flushPending() {
	if len(writer.pending) > 0 {
		writer.emit(append(writer.pending, '\n'))
		writer.pending = nil
	}
}

func (writer *laneWriter) emit(line []byte) {
	writer.parent.mu.Lock()
	defer writer.parent.mu.Unlock()
	_, _ = io.WriteString(writer.parent.out, writer.prefix)
	_, _ = writer.parent.out.Write(line)
}
