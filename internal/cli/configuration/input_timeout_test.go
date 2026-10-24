//go:build unix

package configuration_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"bytes"
	"net/http"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/configuration"
	"github.com/optimiware/kodama/internal/cli/root"
)

// runCtx is run with an explicit invocation context.
func runCtx(t *testing.T, ctx context.Context, srv *clienttest.Server, stdin *os.File, dir string, args ...string) result {
	t.Helper()
	store := root.NewFileContextStore(filepath.Join(dir, "kodama", "contexts.json"))
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams: root.Streams{In: stdin, Out: &out, Err: &errb}, Contexts: store, Tokens: tokens{}, AllowInsecureLoopback: true,
	}, configuration.NewVariableCommands(), configuration.NewSecretCommands())
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
	return result{code, out.String(), errb.String()}
}

// Test_input_timeout_no_request: a redirected pipe that stalls cannot defeat
// --timeout or an interrupt, and data that arrives afterwards is never
// submitted.
func Test_input_timeout_no_request(t *testing.T) {
	srv := neverCalled(t)
	args := []string{"secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1", "--value-file", "-"}

	stalled := func(t *testing.T) (*os.File, *os.File) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		return pr, pw
	}

	t.Run("timeout with partial data", func(t *testing.T) {
		pr, pw := stalled(t)
		if _, err := pw.WriteString("partial-secret"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		r := runCtx(t, context.Background(), srv, pr, t.TempDir(), append(args, "--timeout", "200ms")...)
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("took %v", d)
		}
		if bytes.Contains([]byte(r.err), []byte("partial-secret")) {
			t.Fatalf("diagnostic echoes input: %q", r.err)
		}
		if r.err != "" && bytes.Contains([]byte(r.err), []byte("outcomeUncertain")) {
			t.Fatalf("a cancelled read is not an uncertain mutation: %q", r.err)
		}
		// Late input must not trigger a submission.
		_, _ = pw.WriteString("late")
		pw.Close()
		time.Sleep(150 * time.Millisecond)
	})

	t.Run("interrupt", func(t *testing.T) {
		pr, _ := stalled(t)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		r := runCtx(t, ctx, srv, pr, t.TempDir(), args...)
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		pr, _ := stalled(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if r := runCtx(t, ctx, srv, pr, t.TempDir(), args...); r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})

	t.Run("fifo is refused without opening", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip(err)
		}
		pr, _ := stalled(t)
		a := append([]string{}, args[:len(args)-1]...)
		a = append(a, p)
		done := make(chan result, 1)
		go func() { done <- runCtx(t, context.Background(), srv, pr, t.TempDir(), a...) }()
		select {
		case r := <-done:
			if r.code != 2 || r.out != "" {
				t.Fatalf("exit %d err %q", r.code, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("command blocked on a FIFO")
		}
	})

	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

// A command that completes normally after reading from a real pipe still
// submits the exact bytes (guards the goroutine hand-off).
func Test_input_pipe_still_submits(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, secretMeta(1), nil)
	}))
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	go func() { pw.Write(binarySecret); pw.Close() }()
	r := runCtx(t, context.Background(), srv, pr, t.TempDir(), "secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1", "--value-file", "-", "--timeout", "10s")
	if r.code != 0 || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d err %q requests %d", r.code, r.err, len(srv.Requests()))
	}
}
