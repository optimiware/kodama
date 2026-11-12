// Package harness runs pinned infrastructure containers for component tests.
//
// It is test support code under test/, never product code. Every Podman
// interaction goes through os/exec with an explicit, non-nil environment and
// no shell, so arguments cannot be re-interpreted by a shell. The Runner seam
// lets unit tests exercise argument construction, error mapping and cleanup
// ordering without a real container runtime.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Container labels used to scope cleanup. Every container, network and volume
// created by this package carries all three.
const (
	LabelTest     = "kodama.test"
	LabelShard    = "kodama.shard"
	LabelOwnerPID = "kodama.owner-pid"
)

const (
	// DefaultCPUs and DefaultMemory bound every container unless a RunSpec
	// overrides them.
	DefaultCPUs   = "1"
	DefaultMemory = "1g"

	// DefaultProbeTimeout is the bounded readiness window.
	DefaultProbeTimeout = 90 * time.Second

	// DefaultShardTimeout bounds one isolated shard's lifetime.
	DefaultShardTimeout = 15 * time.Minute
)

var (
	// ErrImageUnavailable distinguishes a failed pull (or a missing image that
	// could not be pulled) from other Podman failures. Callers pause or skip
	// on this error; they never treat it as a passing check.
	ErrImageUnavailable = errors.New("harness: image unavailable")

	// ErrProbeTimeout reports that a readiness barrier exhausted its bounded
	// window without the probe succeeding.
	ErrProbeTimeout = errors.New("harness: readiness probe timed out")
)

// Command is one process invocation. Name is resolved through PATH; Args are
// passed verbatim with no shell. Env is always non-nil: an explicit
// environment is part of the contract because a nil Cmd.Env would inherit the
// host environment implicitly.
type Command struct {
	Name string
	Args []string
	Env  []string
}

// Result is the captured output of one command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes one Command. Tests substitute a fake; production harness
// code uses ExecRunner.
type Runner interface {
	Run(ctx context.Context, cmd Command) (Result, error)
}

// ExecRunner runs commands with os/exec and no shell.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, cmd Command) (Result, error) {
	if cmd.Name == "" {
		return Result{}, errors.New("harness: empty command name")
	}
	if cmd.Env == nil {
		return Result{}, errors.New("harness: nil environment; refusing to inherit the host environment")
	}
	proc := exec.CommandContext(ctx, cmd.Name, cmd.Args...)
	proc.Env = cmd.Env
	var stdout, stderr bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stderr
	err := proc.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			res.ExitCode = exit.ExitCode()
		} else {
			res.ExitCode = -1
		}
		// Only the binary and its subcommand are named: argument values may
		// carry credentials (for example --env PGPASSWORD=...).
		subcommand := ""
		if len(cmd.Args) > 0 {
			subcommand = " " + cmd.Args[0]
		}
		return res, fmt.Errorf("harness: %s%s failed: %w: %s", cmd.Name, subcommand, err, strings.TrimSpace(stderr.String()))
	}
	return res, nil
}

// BaseEnv returns the explicit environment the harness passes to Podman. It is
// always non-nil and contains the variables rootless Podman needs to find its
// binary, storage and runtime directory.
func BaseEnv() []string {
	env := make([]string, 0, 5)
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	env = append(env, "PATH="+path)
	home := os.Getenv("HOME")
	if home == "" {
		home = os.TempDir()
	}
	env = append(env, "HOME="+home)
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	return env
}

// Podman is the Podman lifecycle client. All operations are bounded by a
// context and run through Runner.
type Podman struct {
	binary         string
	runner         Runner
	env            []string
	clock          Clock
	logf           func(format string, args ...any)
	evidenceDir    string
	processAlive   func(pid int) bool
	operationLimit time.Duration
	stopLimit      time.Duration
	pullLimit      time.Duration
	stopGrace      time.Duration
}

// Option configures a Podman client.
type Option func(*Podman)

// WithRunner replaces the process runner (unit tests).
func WithRunner(r Runner) Option {
	return func(p *Podman) {
		if r != nil {
			p.runner = r
		}
	}
}

// WithLogger receives human-readable harness records (image digests, cleanup
// failures). In tests this is normally t.Logf.
func WithLogger(f func(format string, args ...any)) Option {
	return func(p *Podman) {
		if f != nil {
			p.logf = f
		}
	}
}

// WithEvidenceDir writes image tag/digest records as JSON lines to
// <dir>/images.jsonl. An empty dir disables the file and leaves logging only.
func WithEvidenceDir(dir string) Option {
	return func(p *Podman) { p.evidenceDir = dir }
}

// WithProcessAlive replaces the liveness check used by orphan cleanup.
func WithProcessAlive(f func(pid int) bool) Option {
	return func(p *Podman) {
		if f != nil {
			p.processAlive = f
		}
	}
}

// WithStopTimeout bounds Stop.
func WithStopTimeout(d time.Duration) Option {
	return func(p *Podman) {
		if d > 0 {
			p.stopLimit = d
		}
	}
}

// NewPodman builds a client with the given options.
func NewPodman(opts ...Option) *Podman {
	p := &Podman{
		binary:         "podman",
		runner:         ExecRunner{},
		env:            BaseEnv(),
		clock:          RealClock{},
		logf:           func(string, ...any) {},
		evidenceDir:    os.Getenv("KODAMA_EVIDENCE_DIR"),
		processAlive:   ProcessAlive,
		operationLimit: 2 * time.Minute,
		stopLimit:      30 * time.Second,
		pullLimit:      10 * time.Minute,
		stopGrace:      5 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	if p.env == nil {
		p.env = BaseEnv()
	}
	if p.runner == nil {
		p.runner = ExecRunner{}
	}
	return p
}

// Clock is the injectable time source for probe barriers.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the production clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep implements Clock and honors context cancellation.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Logf records one human-readable line.
func (p *Podman) Logf(format string, args ...any) { p.logf(format, args...) }

// run executes one Podman command with the explicit environment. A positive
// limit adds an operation timeout on top of the caller's context.
func (p *Podman) run(ctx context.Context, limit time.Duration, args ...string) (Result, error) {
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	env := append([]string(nil), p.env...)
	if env == nil {
		env = BaseEnv()
	}
	return p.runner.Run(ctx, Command{Name: p.binary, Args: append([]string(nil), args...), Env: env})
}

// Version checks that the Podman binary is usable.
func (p *Podman) Version(ctx context.Context) error {
	_, err := p.run(ctx, p.operationLimit, "version", "--format", "{{.Client.Version}}")
	if err != nil {
		return fmt.Errorf("harness: podman is not available: %w", err)
	}
	return nil
}

// registryMu guards the active shard registry. It exists so tests can run in
// parallel without racing on shared state.
var (
	registryMu sync.Mutex
	shards     = map[*Shard]struct{}{}
)

func registerShard(s *Shard) {
	registryMu.Lock()
	defer registryMu.Unlock()
	shards[s] = struct{}{}
}

func unregisterShard(s *Shard) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(shards, s)
}

func snapshotShards() []*Shard {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]*Shard, 0, len(shards))
	for s := range shards {
		out = append(out, s)
	}
	return out
}
