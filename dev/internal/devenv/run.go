package devenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result is the outcome of one command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Cmd describes one command invocation. The zero value runs in the repository
// root with the caller's environment and the process's standard streams.
type Cmd struct {
	Args    []string
	Env     []string // nil inherits os.Environ()
	Dir     string   // "" means Root()
	Stdin   io.Reader
	Capture bool // capture stdout and stderr instead of inheriting them
	Timeout time.Duration
}

// execFunc is the seam tests replace to record commands without a container
// engine.
type execFunc func(Cmd) (Result, error)

var runCmd execFunc = osExec

func osExec(c Cmd) (Result, error) {
	ctx := context.Background()
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	name := c.Args[0]
	cmd := exec.CommandContext(ctx, name, c.Args[1:]...)
	cmd.Dir = c.Dir
	if cmd.Dir == "" {
		cmd.Dir = Root()
	}
	if c.Env == nil {
		cmd.Env = os.Environ()
	} else {
		cmd.Env = c.Env
	}
	cmd.Stdin = c.Stdin
	var stdout, stderr bytes.Buffer
	if c.Capture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, fmt.Errorf("%s failed with exit %d%s", describe(c.Args), exit.ExitCode(), detail(result.Stderr))
	}
	return result, fmt.Errorf("%s: %w", describe(c.Args), err)
}

func describe(args []string) string {
	if len(args) > 4 {
		args = args[:4]
	}
	return strings.Join(args, " ")
}

func detail(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}

// exec runs a command in the repository root and fails on a non-zero exit.
func execCmd(args ...string) error {
	_, err := runCmd(Cmd{Args: args})
	return err
}

// output runs a command and returns its captured combined output.
func output(args ...string) (string, error) {
	result, err := runCmd(Cmd{Args: args, Capture: true})
	if err != nil {
		return result.Stdout + result.Stderr, err
	}
	return result.Stdout, nil
}

// sleep is the seam tests replace to avoid real waiting.
var sleep = time.Sleep

// reachableURLFn is the seam tests replace to avoid a real listener.
var reachableURLFn = reachableURL

// reachableHostPortFn is the seam tests replace to avoid a real listener.
var reachableHostPortFn = ReachableHostPort

// waitFor polls url until it answers or seconds pass.
func waitFor(url string, seconds int) error {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		if reachableURLFn(url, 5*time.Second) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", url)
		}
		sleep(time.Second)
	}
}
