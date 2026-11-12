package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// hookClientTimeout bounds one hook-to-service callback from the hook process.
// It is deliberately longer than the service-side bound so the service's
// bounded rejection wins; an unreachable channel still fails fast.
const hookClientTimeout = 2 * native.DefaultTimeout

// runGitHookEntry runs `kodama git hook <update|pre-receive|post-receive>`.
//
// The hook process is a separate OS process spawned by git. It reads git's
// inputs and the non-secret KODAMA_* environment, forwards one request over the
// private socket and exits with the returned code. Every failure (missing
// socket, timeout, malformed reply, refused handle) rejects the push.
func runGitHookEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runGitHook(ctx, args, os.Stdin, stdout, stderr)
}

// runGitHook is the injectable form used by tests. stdin is git's hook input.
func runGitHook(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr, "git hook requires a phase")
		return 2
	}
	phase := native.Phase(args[0])
	if !phase.Valid() {
		usage(stderr, fmt.Sprintf("unknown git hook phase %q", args[0]))
		return 2
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request := native.Request{
		Phase:       phase,
		ActorRef:    os.Getenv(git.EnvActorRef),
		Repository:  os.Getenv(git.EnvRepository),
		ResourceID:  os.Getenv(git.EnvResourceID),
		SourceEvent: os.Getenv(git.EnvSourceEvent),
	}
	request.ObjectEnvironment = hookObjectEnvironment()
	switch phase {
	case native.PhaseUpdate:
		if len(args) != 4 {
			return rejectHookProcess(stderr)
		}
		request.Ref = args[1]
		request.Old = args[2]
		request.New = args[3]
	default:
		if len(args) != 1 {
			return rejectHookProcess(stderr)
		}
		lines, err := readHookLines(stdin)
		if err != nil {
			return rejectHookProcess(stderr)
		}
		request.Lines = lines
	}
	socket := strings.TrimSpace(os.Getenv(native.EnvHookSocket))
	if socket == "" {
		return rejectHookProcess(stderr)
	}
	response, err := native.Call(ctx, socket, request, hookClientTimeout)
	if err != nil {
		return rejectHookProcess(stderr)
	}
	if response.Message != "" {
		fmt.Fprintln(stderr, response.Message)
	}
	if response.ExitCode != 0 {
		return 1
	}
	return 0
}

// hookObjectEnvironment forwards only the receive-pack quarantine object
// variables so the service's ancestry check can see the pushed objects. They
// are paths, never credentials.
func hookObjectEnvironment() []string {
	var environment []string
	for _, name := range []string{
		native.EnvObjectDirectory,
		native.EnvAlternateObjectDirectories,
		native.EnvQuarantinePath,
	} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

// readHookLines reads git's bounded stdin batch.
func readHookLines(stdin io.Reader) ([]string, error) {
	if stdin == nil {
		return nil, fmt.Errorf("hook stdin is missing")
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, native.DefaultMaxLineBytes), native.DefaultMaxLineBytes)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return nil, fmt.Errorf("hook stdin has an empty line")
		}
		lines = append(lines, line)
		if len(lines) > native.DefaultMaxLines {
			return nil, fmt.Errorf("hook stdin exceeds %d lines", native.DefaultMaxLines)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("hook stdin is empty")
	}
	return lines, nil
}

// rejectHookProcess prints the single sanitized diagnostic and fails closed.
// It never prints environment values or credentials.
func rejectHookProcess(stderr io.Writer) int {
	fmt.Fprintln(stderr, native.RejectionMessage)
	return 1
}
