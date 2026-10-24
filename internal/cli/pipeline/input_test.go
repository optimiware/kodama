//go:build unix

package pipeline_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/cli/root"
	"net/http"
)

func runPipe(t *testing.T, ctx context.Context, srv *clienttest.Server, stdin *os.File, args ...string) result {
	t.Helper()
	contrib, err := clipipeline.NewCommands(clipipeline.Config{Validator: m1Validator(t), Actor: diagnosticActor})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams: root.Streams{In: stdin, Out: &out, Err: &errb}, Tokens: tokens{}, AllowInsecureLoopback: true,
	}, contrib)
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
	return result{code, out.String(), errb.String()}
}

// Test_input_timeout_no_request: a stalled pipe cannot defeat --timeout or an
// interrupt for validate or register; nothing is sent afterwards.
func Test_input_timeout_no_request(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the server: %s %s", r.Method, r.URL.Path)
	}))
	// A timeout leaves a read deadline on the pipe, so each case gets its own.
	var writers []*os.File
	fresh := func(t *testing.T) *os.File {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		_, _ = pw.WriteString("apiVersion: kodama/v1alpha1\n")
		writers = append(writers, pw)
		return pr
	}

	for _, verb := range []string{"validate", "register"} {
		t.Run(verb+" timeout", func(t *testing.T) {
			start := time.Now()
			r := runPipe(t, context.Background(), srv, fresh(t), "pipeline", verb, "--file", "-", "--timeout", "200ms")
			if r.code != 5 || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %v", d)
			}
		})
		t.Run(verb+" interrupt", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			if r := runPipe(t, ctx, srv, fresh(t), "pipeline", verb, "--file", "-"); r.code != 5 || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
		})
	}
	t.Run("fifo is refused without blocking", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fifo.yaml")
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip(err)
		}
		done := make(chan result, 1)
		go func() { done <- runPipe(t, context.Background(), srv, fresh(t), "pipeline", "validate", "--file", p) }()
		select {
		case r := <-done:
			if r.code != 2 || r.out != "" {
				t.Fatalf("exit %d err %q", r.code, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("command blocked on a FIFO")
		}
	})
	for _, w := range writers {
		_, _ = w.WriteString("late")
		w.Close()
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}
