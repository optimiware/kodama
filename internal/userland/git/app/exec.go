// Package app wires the independent Git service to operating-system and
// public-client boundaries. It is composition glue only: it starts no server
// and holds no business logic of its own.
//
// The package is a Userland app process boundary. It may import only neutral
// packages, the public API/client contracts and other Userland packages; it
// never imports Shell, Kernel or installation code.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// ErrOutputTooLarge reports a native command whose bounded output was
// exceeded. The command is not retried: an unbounded response is a defect, not
// a transient failure.
var ErrOutputTooLarge = errors.New("native git output exceeds the configured bound")

// ErrUnsupportedCommand reports a command the executor will not run. The
// executor only runs the native `git` binary; it never interprets a shell.
var ErrUnsupportedCommand = errors.New("unsupported native command")

// ExecutorConfig configures the native Git command executor.
type ExecutorConfig struct {
	// GitPath is the native git binary. It defaults to "git".
	GitPath string
	// Env is the exact child environment. A nil slice selects the documented
	// minimal environment; os.Environ is never inherited implicitly.
	Env []string
	// MaxOutputBytes bounds stdout and stderr independently. A zero value
	// selects DefaultMaxOutputBytes.
	MaxOutputBytes int
}

// DefaultMaxOutputBytes bounds one native command stream when the
// configuration leaves the bound unset.
const DefaultMaxOutputBytes = 1 << 20

// Executor implements git.CommandExecutor over os/exec without a shell.
type Executor struct {
	path    string
	env     []string
	maxOut  int
	command func(context.Context, string, ...string) *exec.Cmd
}

var _ git.CommandExecutor = (*Executor)(nil)

// NewExecutor validates the configuration and returns the executor.
func NewExecutor(config ExecutorConfig) (*Executor, error) {
	path := strings.TrimSpace(config.GitPath)
	if path == "" {
		path = "git"
	}
	if strings.ContainsAny(path, " \t\n;|&$`'\"<>") {
		return nil, fmt.Errorf("%w: git path %q is not a plain executable path", ErrUnsupportedCommand, path)
	}
	env := config.Env
	if env == nil {
		env = MinimalEnvironment()
	}
	maxOut := config.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputBytes
	}
	return &Executor{
		path:    path,
		env:     append(make([]string, 0, len(env)), env...),
		maxOut:  maxOut,
		command: exec.CommandContext,
	}, nil
}

// MinimalEnvironment returns the explicit environment a native command
// receives by default. Only PATH and HOME are selected from the parent; every
// other inherited variable is dropped so a caller cannot smuggle policy or
// credentials into the child through the ambient environment.
func MinimalEnvironment() []string {
	env := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if path, ok := os.LookupEnv("PATH"); ok && path != "" {
		env = append(env, "PATH="+path)
	}
	if home, ok := os.LookupEnv("HOME"); ok && home != "" {
		env = append(env, "HOME="+home)
	}
	return env
}

// Run executes one native Git command. Arguments are passed verbatim: there is
// no shell, no word splitting and no metacharacter interpretation. Context
// cancellation terminates the child and returns the context error.
func (executor *Executor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if executor == nil {
		return git.CommandResult{}, fmt.Errorf("%w: executor is nil", ErrUnsupportedCommand)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return git.CommandResult{}, err
	}
	if command.Name != "git" {
		return git.CommandResult{}, fmt.Errorf("%w: %q", ErrUnsupportedCommand, command.Name)
	}
	if err := validateArgs(command.Args); err != nil {
		return git.CommandResult{}, err
	}

	child := executor.command(ctx, executor.path, command.Args...)
	extra := native.CommandEnvironment(ctx)
	environment := make([]string, 0, len(executor.env)+len(extra))
	environment = append(environment, executor.env...)
	environment = append(environment, extra...)
	child.Env = environment
	child.Stdin = nil
	stdout := newBoundedBuffer(executor.maxOut)
	stderr := newBoundedBuffer(executor.maxOut)
	child.Stdout = stdout
	child.Stderr = stderr

	err := child.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return git.CommandResult{}, ctxErr
	}
	if stdout.exceeded() || stderr.exceeded() {
		return git.CommandResult{}, fmt.Errorf("%w: %s", ErrOutputTooLarge, command.Name)
	}
	result := git.CommandResult{
		Stdout: stdout.bytes(),
		Stderr: stderr.bytes(),
	}
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// validateArgs refuses a NUL or newline-bearing argument. A native Git argument
// may carry shell metacharacters because no shell ever sees it, but an embedded
// NUL or newline cannot be represented safely.
func validateArgs(args []string) error {
	for _, argument := range args {
		if strings.ContainsAny(argument, "\x00\n\r") {
			return fmt.Errorf("%w: argument contains a control character", ErrUnsupportedCommand)
		}
	}
	return nil
}

// boundedBuffer keeps at most limit bytes and records that the bound was
// exceeded. It never blocks the child: excess bytes are discarded after the
// bound so os/exec's copy goroutine can keep draining the pipe.
type boundedBuffer struct {
	mutex    sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (bounded *boundedBuffer) Write(data []byte) (int, error) {
	bounded.mutex.Lock()
	defer bounded.mutex.Unlock()
	remaining := bounded.limit - bounded.buffer.Len()
	if remaining < len(data) {
		bounded.overflow = true
		if remaining > 0 {
			_, _ = bounded.buffer.Write(data[:remaining])
		}
		return len(data), nil
	}
	_, _ = bounded.buffer.Write(data)
	return len(data), nil
}

func (bounded *boundedBuffer) exceeded() bool {
	bounded.mutex.Lock()
	defer bounded.mutex.Unlock()
	return bounded.overflow
}

func (bounded *boundedBuffer) bytes() []byte {
	bounded.mutex.Lock()
	defer bounded.mutex.Unlock()
	return append([]byte(nil), bounded.buffer.Bytes()...)
}
