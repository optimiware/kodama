//go:build unix

package work_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Test_input_timeout_no_request: a stalled redirected pipe or FIFO cannot
// defeat --timeout or an interrupt, produces empty stdout, and never leads to
// a request.
func Test_input_timeout_no_request(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the server: %s %s", r.Method, r.URL.Path)
	}))
	// A timeout leaves a read deadline on the pipe, so each case gets its own.
	var pw *os.File
	fresh := func(t *testing.T) {
		pr, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); w.Close() })
		c.in, pw = pr, w
	}
	var pipes []*os.File
	fresh(t)
	pipes = append(pipes, pw)

	args := []string{"work", "create", "--kind", "story", "--title", "T", "--spec-file", "-"}

	t.Run("timeout", func(t *testing.T) {
		fresh(t)
		pipes = append(pipes, pw)
		_, _ = pw.WriteString("# partial spec")
		start := time.Now()
		r := c.run(append(args, "--timeout", "200ms")...)
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("took %v", d)
		}
	})
	t.Run("interrupt", func(t *testing.T) {
		fresh(t)
		pipes = append(pipes, pw)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		r := c.runWith(ctx, args...)
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("transition review claim", func(t *testing.T) {
		fresh(t)
		pipes = append(pipes, pw)
		r := c.run("work", "transition", "wi_1", "--to", "review", "--if-version", "3", "--review-file", "-", "--timeout", "150ms")
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("fifo is refused without blocking", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip(err)
		}
		done := make(chan result, 1)
		go func() {
			done <- c.run("work", "create", "--kind", "story", "--title", "T", "--spec-file", p)
		}()
		select {
		case r := <-done:
			if r.code != 2 || r.out != "" {
				t.Fatalf("exit %d err %q", r.code, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("command blocked on a FIFO")
		}
	})
	// Late input is discarded.
	for _, p := range pipes {
		_, _ = p.WriteString("late")
		p.Close()
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}
