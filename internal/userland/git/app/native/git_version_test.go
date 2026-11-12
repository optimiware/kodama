package native

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
)

// versionProbeExecutor scripts the one `git --version` command. It records the
// command and does not interpret the context so the product function itself has
// to propagate cancellation and the caller's context.
type versionProbeExecutor struct {
	mu       sync.Mutex
	commands []git.Command
	result   git.CommandResult
	err      error
}

func (executor *versionProbeExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, command)
	return executor.result, executor.err
}

func (executor *versionProbeExecutor) recorded() []git.Command {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]git.Command(nil), executor.commands...)
}

func versionOutput(version string) git.CommandResult {
	return git.CommandResult{Stdout: []byte("git version " + version + "\n")}
}

// TestRequireGitVersionBoundary pins the numeric minimum at Git 2.44.0: the
// first release with `rev-parse --show-ref-format`, which the committed admin
// policy needs. It accepts newer minors and majors, accepts the documented
// vendor suffixes, and refuses every output that cannot be parsed as a concrete
// numeric version.
func TestRequireGitVersionBoundary(t *testing.T) {
	accepted := []struct {
		name    string
		output  string
		version string
	}{
		{name: "exact minimum", output: "git version 2.44.0\n", version: "2.44.0"},
		{name: "minimum without trailing newline", output: "git version 2.44.0", version: "2.44.0"},
		{name: "minimum patch", output: "git version 2.44.7\n", version: "2.44.7"},
		{name: "newer minor", output: "git version 2.45.0\n", version: "2.45.0"},
		{name: "newer minor that is lexically smaller", output: "git version 2.100.1\n", version: "2.100.1"},
		{name: "newer major", output: "git version 3.0.0\n", version: "3.0.0"},
		{name: "apple vendor suffix", output: "git version 2.45.1 (Apple Git-155)\n", version: "2.45.1"},
		{name: "windows vendor suffix", output: "git version 2.44.0.windows.1\n", version: "2.44.0"},
	}
	for _, test := range accepted {
		t.Run("accept "+test.name, func(t *testing.T) {
			executor := &versionProbeExecutor{result: git.CommandResult{Stdout: []byte(test.output)}}
			if err := RequireGitVersion(context.Background(), executor); err != nil {
				t.Fatalf("RequireGitVersion(%q) = %v; want nil", test.output, err)
			}
			commands := executor.recorded()
			if len(commands) != 1 {
				t.Fatalf("commands = %v; want exactly one git --version probe", commands)
			}
			if commands[0].Name != "git" || len(commands[0].Args) != 1 || commands[0].Args[0] != "--version" {
				t.Fatalf("command = %+v; want git --version", commands[0])
			}
		})
	}

	refused := []struct {
		name   string
		result git.CommandResult
		err    error
	}{
		{name: "older patch release", result: versionOutput("2.43.99")},
		{name: "older minor", result: versionOutput("2.43.0")},
		{name: "older major", result: versionOutput("1.99.99")},
		{name: "empty output", result: git.CommandResult{}},
		{name: "blank output", result: git.CommandResult{Stdout: []byte("\n")}},
		{name: "missing version", result: git.CommandResult{Stdout: []byte("git version\n")}},
		{name: "missing patch component", result: versionOutput("2.44")},
		{name: "unparsable components", result: versionOutput("two.forty-four.zero")},
		{name: "unparsable patch", result: versionOutput("2.44.x")},
		{name: "trailing version junk", result: versionOutput("2.44.0-rc1")},
		{name: "empty vendor component", result: versionOutput("2.44.0.")},
		{name: "overflowed major", result: versionOutput("99999999999999999999.0.0")},
		{name: "overflowed patch", result: versionOutput("2.44.99999999999999999999")},
		{name: "unrelated output", result: git.CommandResult{Stdout: []byte("not a git version\n")}},
		{name: "multiple lines", result: git.CommandResult{Stdout: []byte("git version 2.43.0\ngit version 2.44.0\n")}},
		// A supported first line must not mask the one-line contract: these
		// three outputs are refused only by the exactly-one-line rule, because
		// the version the parser would otherwise see is 2.44.0.
		{name: "multiple lines after a supported first line", result: git.CommandResult{Stdout: []byte("git version 2.44.0\ngit version 2.43.0\n")}},
		{name: "warning after a supported line", result: git.CommandResult{Stdout: []byte("git version 2.44.0\nwarning: x\n")}},
		{name: "garbage line before a supported line", result: git.CommandResult{Stdout: []byte("warning: x\ngit version 2.44.0\n")}},
		{name: "failed command", result: git.CommandResult{ExitCode: 127, Stderr: []byte("git: command not found\n")}},
		{name: "failed command with plausible output", result: git.CommandResult{
			Stdout:   []byte("git version 2.44.0\n"),
			Stderr:   []byte("fatal: probe failed\n"),
			ExitCode: 1,
		}},
		{name: "executor error", result: git.CommandResult{}, err: errors.New("spawn failed")},
	}
	for _, test := range refused {
		t.Run("refuse "+test.name, func(t *testing.T) {
			executor := &versionProbeExecutor{result: test.result, err: test.err}
			err := RequireGitVersion(context.Background(), executor)
			if err == nil {
				t.Fatalf("RequireGitVersion(%+v) = nil; want a refusal", test.result)
			}
		})
	}
}

// TestRequireGitVersionProbeIsContextAware kills a probe that runs the command
// and only checks the context afterwards: a cancelled startup must not start
// the native executable at all.
func TestRequireGitVersionProbeIsContextAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := &versionProbeExecutor{result: versionOutput("2.44.0")}
	err := RequireGitVersion(ctx, executor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RequireGitVersion on a cancelled context = %v; want context.Canceled", err)
	}
	if commands := executor.recorded(); len(commands) != 0 {
		t.Fatalf("commands = %v; want no probe after cancellation", commands)
	}
}

// versionDeadlineExecutor records the deadline of the context the product
// passes to the executable seam.
type versionDeadlineExecutor struct {
	mu       sync.Mutex
	deadline time.Time
	bounded  bool
	called   bool
}

func (executor *versionDeadlineExecutor) Run(ctx context.Context, _ git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.deadline, executor.bounded = ctx.Deadline()
	executor.called = true
	return versionOutput("2.44.0"), nil
}

// TestRequireGitVersionBoundsTheProbe kills the mutant that drops the default
// timeout for an unbounded caller and the mutant that replaces a caller's
// earlier deadline with a later one: the probe must always be bounded, and it
// must never extend a bound the caller already set.
func TestRequireGitVersionBoundsTheProbe(t *testing.T) {
	t.Run("caller without deadline gets VersionProbeTimeout", func(t *testing.T) {
		executor := &versionDeadlineExecutor{}
		if err := RequireGitVersion(context.Background(), executor); err != nil {
			t.Fatalf("RequireGitVersion: %v", err)
		}
		if !executor.called {
			t.Fatal("RequireGitVersion did not probe the executable")
		}
		if !executor.bounded {
			t.Fatal("probe ran without a deadline; want VersionProbeTimeout")
		}
		if remaining := time.Until(executor.deadline); remaining <= 0 || remaining > VersionProbeTimeout {
			t.Fatalf("probe deadline is %v away; want (0, %v]", remaining, VersionProbeTimeout)
		}
	})

	t.Run("caller deadline preserved", func(t *testing.T) {
		executor := &versionDeadlineExecutor{}
		callerDeadline := time.Now().Add(time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
		defer cancel()
		if err := RequireGitVersion(ctx, executor); err != nil {
			t.Fatalf("RequireGitVersion: %v", err)
		}
		if !executor.called {
			t.Fatal("RequireGitVersion did not probe the executable")
		}
		if !executor.bounded {
			t.Fatal("probe ran without a deadline; want the caller deadline")
		}
		if !executor.deadline.Equal(callerDeadline) {
			t.Fatalf("probe deadline = %v; want the caller deadline %v preserved", executor.deadline, callerDeadline)
		}
	})
}

// TestRequireGitVersionRefusesMissingExecutor keeps the check fail-closed when
// no command seam is wired.
func TestRequireGitVersionRefusesMissingExecutor(t *testing.T) {
	if err := RequireGitVersion(context.Background(), nil); err == nil {
		t.Fatal("RequireGitVersion accepted a nil executor")
	}
}

// TestRequireGitVersionErrorNamesTheMinimum keeps the startup refusal
// actionable: the operator must see both the observed version and the declared
// minimum.
func TestRequireGitVersionErrorNamesTheMinimum(t *testing.T) {
	executor := &versionProbeExecutor{result: versionOutput("2.43.0")}
	err := RequireGitVersion(context.Background(), executor)
	if err == nil {
		t.Fatal("RequireGitVersion accepted Git 2.43.0")
	}
	if !strings.Contains(err.Error(), "2.43.0") || !strings.Contains(err.Error(), MinimumGitVersion) {
		t.Fatalf("error = %q; want the observed and minimum versions", err)
	}
	if MinimumGitVersion != "2.44.0" {
		t.Fatalf("MinimumGitVersion = %q; want 2.44.0", MinimumGitVersion)
	}
}
