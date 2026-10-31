package secret

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// The cross-process tests re-execute the test binary as a second custody process
// over the same root. The in-process lock cannot order two processes; only the
// OS lock can, so these tests fail if Custody does not hold flock.

const helperEnv = "KODAMA_CUSTODY_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(custodyHelper(mode))
	}
	os.Exit(m.Run())
}

// custodyHelper is the child process. Its protocol is lines on stdout; in "hold"
// mode it stops inside the publication critical section until stdin yields a line.
func custodyHelper(mode string) int {
	out := bufio.NewWriter(os.Stdout)
	say := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...); _ = out.Flush() }
	c, err := NewCustody(CustodyConfig{Root: os.Getenv(helperEnv + "_ROOT"), Source: newVault()})
	if err != nil {
		say("error %v", err)
		return 2
	}
	ref, err := runtime.NewMaterializationRef(scopeA, "run1", domain.StepID(os.Getenv(helperEnv+"_STEP")), 1)
	if err != nil {
		say("error %v", err)
		return 2
	}
	if mode == "hold" {
		c.underLock = func() {
			say("locked")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}
	_, err = c.Resolve(context.Background(), request(ref, "TOKEN"))
	switch {
	case err == nil:
		say("published")
	case errors.Is(err, runtime.ErrMaterializationRevoked):
		say("revoked")
	default:
		say("error %v", err)
		return 1
	}
	return 0
}

type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

func startChild(t *testing.T, mode, root, step string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperEnv+"_ROOT="+root, helperEnv+"_STEP="+step)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &child{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
}

func (c *child) next(t *testing.T) string {
	t.Helper()
	if !c.lines.Scan() {
		t.Fatalf("the custody process ended without reporting: %v", c.lines.Err())
	}
	return c.lines.Text()
}

func (c *child) finish(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("the custody process failed: %v", err)
	}
}

// Test_unit_shell_secret_revocation_across_processes proves publication and
// revocation are serialized by the OS lock, not by anything one process holds.
func Test_unit_shell_secret_revocation_across_processes(t *testing.T) {
	t.Run("a revocation waits for a publication in another process", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		worker := startChild(t, "hold", h.root, "build")
		if got := worker.next(t); got != "locked" {
			t.Fatalf("the worker reported %q", got)
		}
		// The worker is inside the critical section, the identity confirmed open.
		revoked := make(chan error, 1)
		go func() { revoked <- c.Revoke(context.Background(), ref) }()
		select {
		case err := <-revoked:
			t.Fatalf("the revocation did not wait for the other process's publication (%v)", err)
		case <-time.After(400 * time.Millisecond):
		}
		if closed, err := c.Revoked(ref); closed || err != nil {
			t.Fatalf("the identity was closed while the lock was held elsewhere: %v %v", closed, err)
		}
		if _, err := os.Stat(filepath.Join(h.root, "material")); err != nil {
			t.Fatal(err)
		}
		_ = worker.stdin.Close() // let the worker publish
		if got := worker.next(t); got != "published" {
			t.Fatalf("the worker reported %q", got)
		}
		worker.finish(t)
		select {
		case err := <-revoked:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the revocation never got the lock")
		}
		// The publication happened first, so the revocation removed it.
		if len(c.Published()) != 0 {
			t.Fatalf("the revocation left what the other process published: %v", c.Published())
		}
		if closed, err := c.Revoked(ref); !closed || err != nil {
			t.Fatalf("the identity is not closed: %v %v", closed, err)
		}
	})

	t.Run("publication races revocation across processes and nothing is left", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		for round := 1; round <= 4; round++ {
			step := fmt.Sprintf("race%d", round)
			ref := ident(t, domain.StepID(step), 1)
			var kids []*child
			for range 4 {
				kids = append(kids, startChild(t, "race", h.root, step))
			}
			if err := c.Revoke(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			for _, k := range kids {
				got := k.next(t)
				if got != "published" && got != "revoked" {
					t.Fatalf("round %d: a custody process reported %q", round, got)
				}
				k.finish(t)
			}
			// Every publication either preceded the revocation, which removed it, or was
			// refused: after the revocation and every process, nothing is readable.
			if err := c.Revoke(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			if got := c.Published(); len(got) != 0 {
				t.Fatalf("round %d left material behind: %v", round, got)
			}
			files := everything(t, h.root)
			for p, content := range files {
				if strings.Contains(content, "s3cret") {
					t.Fatalf("round %d: %s holds a value", round, p)
				}
			}
		}
	})
}
