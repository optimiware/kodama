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
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/userland/services"
)

// DefaultGitOutputBytes bounds one native Git stream when the configuration
// leaves the bound unset.
const DefaultGitOutputBytes = 1 << 20

// gitCancelWaitDelay bounds how long Wait keeps reading the output pipes after
// the process group was killed on cancellation. A lingering git-remote-* child
// inherited from the killed Git process can otherwise hold the pipes open.
const gitCancelWaitDelay = 5 * time.Second

// OSGitExecutorConfig configures the native Git command executor.
type OSGitExecutorConfig struct {
	// GitPath is the native git binary. It defaults to "git".
	GitPath string
	// MaxOutputBytes bounds stdout and stderr independently.
	MaxOutputBytes int
}

// OSGitExecutor implements services.GitCommandExecutor over os/exec. It never
// runs a shell and never inherits ambient Git configuration or credentials:
// only an explicit minimal environment and the command's own variables reach
// the child.
type OSGitExecutor struct {
	path    string
	maxOut  int
	command func(context.Context, string, ...string) *exec.Cmd
}

var _ services.GitCommandExecutor = (*OSGitExecutor)(nil)

// NewOSGitExecutor validates the configuration and returns the executor.
func NewOSGitExecutor(config OSGitExecutorConfig) (*OSGitExecutor, error) {
	path := strings.TrimSpace(config.GitPath)
	if path == "" {
		path = "git"
	}
	if strings.ContainsAny(path, " \t\n;|&$`'\"<>") {
		return nil, fmt.Errorf("git path %q is not a plain executable path", path)
	}
	maxOut := config.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultGitOutputBytes
	}
	return &OSGitExecutor{path: path, maxOut: maxOut, command: exec.CommandContext}, nil
}

// Run executes one native Git command. Arguments are passed verbatim. A
// non-zero exit is a command result; only a process or context failure is an
// error.
func (executor *OSGitExecutor) Run(ctx context.Context, command services.GitCommand) (services.GitCommandResult, error) {
	if executor == nil {
		return services.GitCommandResult{}, fmt.Errorf("git executor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return services.GitCommandResult{}, err
	}
	args := make([]string, 0, len(command.Args)+2)
	if command.Directory != "" {
		args = append(args, "-C", command.Directory)
	}
	args = append(args, command.Args...)
	for _, argument := range args {
		if strings.ContainsAny(argument, "\x00\n\r") {
			return services.GitCommandResult{}, fmt.Errorf("native git argument contains a control character")
		}
	}

	child := executor.command(ctx, executor.path, args...)
	child.Env = append(gitBaseEnvironment(), command.Env...)
	child.Stdin = nil
	// Run Git in its own process group so cancellation kills the whole tree,
	// including a git-remote-* helper that would otherwise outlive the direct
	// child and keep the output pipes open. WaitDelay then bounds the wait for
	// those pipes after the kill.
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Cancel = func() error {
		if child.Process == nil {
			return nil
		}
		if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	child.WaitDelay = gitCancelWaitDelay
	stdout := newBoundedBuffer(executor.maxOut)
	stderr := newBoundedBuffer(executor.maxOut)
	child.Stdout = stdout
	child.Stderr = stderr
	err := child.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return services.GitCommandResult{}, ctxErr
	}
	if stdout.exceeded() || stderr.exceeded() {
		return services.GitCommandResult{}, fmt.Errorf("native git output exceeds the configured bound")
	}
	result := services.GitCommandResult{Stdout: stdout.bytes(), Stderr: stderr.bytes()}
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// gitBaseEnvironment returns the explicit child environment. Only PATH and HOME
// are selected from the parent; every ambient Git setting is dropped so a
// caller cannot smuggle policy or credentials through the environment.
func gitBaseEnvironment() []string {
	environment := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Kodama Merge Coordinator",
		"GIT_AUTHOR_EMAIL=merge-coordinator@kodama.invalid",
		"GIT_COMMITTER_NAME=Kodama Merge Coordinator",
		"GIT_COMMITTER_EMAIL=merge-coordinator@kodama.invalid",
	}
	if path, ok := os.LookupEnv("PATH"); ok && path != "" {
		environment = append(environment, "PATH="+path)
	}
	if home, ok := os.LookupEnv("HOME"); ok && home != "" {
		environment = append(environment, "HOME="+home)
	}
	return environment
}

// boundedBuffer keeps at most limit bytes and records that the bound was
// exceeded without blocking the child.
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
