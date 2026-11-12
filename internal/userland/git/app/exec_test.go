package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
)

// TestMain doubles as the harmless re-exec helper for the native-command
// tests. It never starts a Git server; the helper only prints deterministic
// bytes so the executor and CGI adapter can be exercised without a repository.
func TestMain(m *testing.M) {
	if os.Getenv("KODAMA_GIT_EXEC_HELPER") == "1" {
		os.Exit(helperMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helperMain(args []string) int {
	if len(args) == 0 {
		return 1
	}
	switch args[0] {
	case "--version":
		fmt.Print("git version 0.0.0-helper")
		return 0
	case "print-large":
		fmt.Print(strings.Repeat("x", 8192))
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	case "echo-args":
		for _, argument := range args[1:] {
			fmt.Println(argument)
		}
		return 0
	case "print-env":
		if len(args) < 2 {
			return 1
		}
		fmt.Print(os.Getenv(args[1]))
		return 0
	case "exit7":
		return 7
	case "http-backend":
		stdinBytes := int64(0)
		if os.Getenv("KODAMA_HELPER_READ_STDIN") == "1" {
			n, _ := io.Copy(io.Discard, os.Stdin)
			stdinBytes = n
		}
		fmt.Printf("Status: 200 OK\r\nContent-Type: application/x-git-upload-pack-result\r\n")
		fmt.Printf("X-Request-Method: %s\r\n", os.Getenv("REQUEST_METHOD"))
		fmt.Printf("X-Path-Info: %s\r\n", os.Getenv("PATH_INFO"))
		fmt.Printf("X-Query: %s\r\n", os.Getenv("QUERY_STRING"))
		fmt.Printf("X-Content-Length: %s\r\n", os.Getenv("CONTENT_LENGTH"))
		fmt.Printf("X-Content-Type: %s\r\n", os.Getenv("CONTENT_TYPE"))
		if os.Getenv("KODAMA_HELPER_READ_STDIN") == "1" {
			fmt.Printf("X-Stdin-Bytes: %d\r\n", stdinBytes)
		}
		fmt.Printf("\r\n")
		if rawSize := os.Getenv("KODAMA_HELPER_BODY_SIZE"); rawSize != "" {
			size, err := strconv.Atoi(rawSize)
			if err != nil {
				return 1
			}
			chunk := strings.Repeat("p", 4096)
			for size > 0 {
				step := min(size, len(chunk))
				_, _ = os.Stdout.WriteString(chunk[:step])
				size -= step
			}
			return 0
		}
		fmt.Print("backend-body")
		return 0
	default:
		return 1
	}
}

func helperExecutor(t *testing.T, maxOut int) *Executor {
	t.Helper()
	executor, err := NewExecutor(ExecutorConfig{
		GitPath:        os.Args[0],
		Env:            append(MinimalEnvironment(), "KODAMA_GIT_EXEC_HELPER=1"),
		MaxOutputBytes: maxOut,
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return executor
}

func TestExecutorRunsNativeCommand(t *testing.T) {
	executor := helperExecutor(t, 0)
	result, err := executor.Run(context.Background(), git.Command{Name: "git", Args: []string{"--version"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d; want 0", result.ExitCode)
	}
	if !strings.Contains(string(result.Stdout), "git version") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestExecutorNeverInterpretsShellMetacharacters(t *testing.T) {
	executor := helperExecutor(t, 0)
	marker := t.TempDir() + "/pwned"
	arguments := []string{
		"echo-args",
		"$(touch " + marker + ")",
		"; touch " + marker,
		"`touch " + marker + "`",
		"| cat",
		"&& id",
		">/dev/null",
	}
	result, err := executor.Run(context.Background(), git.Command{Name: "git", Args: arguments})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	lines := strings.Split(strings.TrimSuffix(string(result.Stdout), "\n"), "\n")
	if len(lines) != len(arguments)-1 {
		t.Fatalf("helper received %d arguments: %q", len(lines), lines)
	}
	for index, expected := range arguments[1:] {
		if lines[index] != expected {
			t.Fatalf("argument %d = %q; want %q", index, lines[index], expected)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a shell metacharacter created %q: %v", marker, err)
	}
}

func TestExecutorBoundsOutput(t *testing.T) {
	executor := helperExecutor(t, 32)
	_, err := executor.Run(context.Background(), git.Command{Name: "git", Args: []string{"print-large"}})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("Run error = %v; want ErrOutputTooLarge", err)
	}
}

func TestExecutorHonorsCancellation(t *testing.T) {
	executor := helperExecutor(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := executor.Run(ctx, git.Command{Name: "git", Args: []string{"sleep"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v; want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestExecutorUsesExplicitEnvironment(t *testing.T) {
	t.Setenv("KODAMA_TEST_SECRET", "must-not-leak")
	executor := helperExecutor(t, 0)
	result, err := executor.Run(context.Background(), git.Command{Name: "git", Args: []string{"print-env", "KODAMA_TEST_SECRET"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Stdout) != 0 {
		t.Fatalf("the child inherited an ambient variable: %q", result.Stdout)
	}
}

// TestExecutorKeepsExplicitEmptyEnvironmentEmpty proves that a caller-provided
// empty (but non-nil) Env reaches the child as an empty environment. A nil
// child Env makes os/exec inherit the whole parent environment, so an explicit
// empty request must never collapse to nil.
func TestExecutorKeepsExplicitEmptyEnvironmentEmpty(t *testing.T) {
	envPath, err := exec.LookPath("env")
	if err != nil {
		t.Skipf("no env executable to observe the child environment: %v", err)
	}
	t.Setenv("KODAMA_LEAKED_SECRET", "host-secret")
	executor, err := NewExecutor(ExecutorConfig{GitPath: envPath, Env: []string{}})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	result, err := executor.Run(context.Background(), git.Command{Name: "git"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(string(result.Stdout), "KODAMA_LEAKED_SECRET=host-secret") {
		t.Fatalf("an explicit empty environment inherited the host value: %q", result.Stdout)
	}
	if executor.env == nil {
		t.Fatal("NewExecutor stored a nil environment for an explicit empty Env")
	}
}

func TestExecutorRefusesNonGitCommand(t *testing.T) {
	executor := helperExecutor(t, 0)
	if _, err := executor.Run(context.Background(), git.Command{Name: "sh"}); !errors.Is(err, ErrUnsupportedCommand) {
		t.Fatalf("Run error = %v; want ErrUnsupportedCommand", err)
	}
}
