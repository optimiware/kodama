package root_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/cli/root"
)

// Test_cancelled_run_has_no_side_effects proves an already-cancelled Run never
// persists context changes when the store is uncontended.
func Test_cancelled_run_has_no_side_effects(t *testing.T) {
	store := fileStore(t)
	r, out, errb := newRunner(t, root.RunnerConfig{Contexts: store}, root.NewContextCommands())
	bg := context.Background()

	if code := r.Run(bg, []string{"context", "set", "--context", "a", "--factory", "https://a.example.invalid"}); code != root.ExitOK {
		t.Fatalf("setup exit %d: %s", code, errb.String())
	}
	if code := r.Run(bg, []string{"context", "set", "--context", "b", "--factory", "https://b.example.invalid"}); code != root.ExitOK {
		t.Fatalf("setup exit %d: %s", code, errb.String())
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()

	ctx, cancel := context.WithCancel(bg)
	cancel()
	for _, args := range [][]string{
		{"context", "set", "--output", "json", "--context", "c", "--factory", "https://c.example.invalid"},
		{"context", "set", "--output", "json", "--context", "a", "--factory", "https://changed.example.invalid"},
		{"context", "use", "--output", "json", "--context", "b"},
	} {
		out.Reset()
		errb.Reset()
		code := r.Run(ctx, args)
		var e struct{ Error struct{ Code string } }
		if code != root.ExitTimeout || out.Len() != 0 || json.Unmarshal(errb.Bytes(), &e) != nil || e.Error.Code != "interrupted" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, out.String(), errb.String())
		}
		after, err := os.ReadFile(store.Path())
		if err != nil || string(after) != string(before) {
			t.Fatalf("%v changed persisted state: %v\n%s", args, err, after)
		}
	}
	if _, err := os.Stat(store.Path() + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}

// Test_context_store_cancel_before_commit proves the file adapter itself
// checks cancellation before acquiring the lock and before the rename.
func Test_context_store_cancel_before_commit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store := root.NewFileContextStore(path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := store.Update(ctx, func(*root.ContextSet) error { called = true; return nil }); err == nil || called {
		t.Fatalf("cancelled update ran: err=%v called=%v", err, called)
	}
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("cancelled load succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created: %v", err)
	}

	// Cancelled while fn runs (after the lock, before the rename): no commit.
	ctx2, cancel2 := context.WithCancel(context.Background())
	err := store.Update(ctx2, func(set *root.ContextSet) error {
		cancel2()
		return nil
	})
	if err == nil {
		t.Fatal("update cancelled before the rename reported success")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created after cancellation: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}
