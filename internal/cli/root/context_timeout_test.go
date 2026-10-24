package root

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func Test_context_store_timeout_and_cancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "contexts.json")
	if err := os.WriteFile(path+".lock", []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileContextStore(path).WithLockWait(time.Second)
	run := func(ctx context.Context, args ...string) (ExitCode, string, string) {
		t.Helper()
		var out, err bytes.Buffer
		r, rerr := NewRunner(RunnerConfig{Streams: Streams{Out: &out, Err: &err}, Contexts: store}, NewContextCommands())
		if rerr != nil {
			t.Fatal(rerr)
		}
		code := r.Run(ctx, args)
		return code, out.String(), err.String()
	}
	args := []string{"context", "set", "--output", "json", "--context", "local", "--factory", "https://factory.example", "--timeout", "30ms"}

	code, stdout, stderr := run(context.Background(), args...)
	if code != ExitTimeout || stdout != "" || !bytes.Contains([]byte(stderr), []byte(`"code":"timeout"`)) {
		t.Fatalf("deadline: code=%d stdout=%q stderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deadline wrote contexts.json: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, stdout, stderr = run(ctx, "context", "set", "--output", "json", "--context", "local", "--factory", "https://factory.example")
	if code != ExitTimeout || stdout != "" || !bytes.Contains([]byte(stderr), []byte(`"code":"interrupted"`)) {
		t.Fatalf("cancel: code=%d stdout=%q stderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancel wrote contexts.json: %v", err)
	}
}
