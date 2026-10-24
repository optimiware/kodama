package root

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newFile(t *testing.T) *FileContextStore {
	t.Helper()
	return NewFileContextStore(filepath.Join(t.TempDir(), "cfg", "contexts.json"))
}

func define(name, factory string) func(*ContextSet) error {
	return func(s *ContextSet) error { return s.Define(name, Context{Factory: factory}) }
}

func Test_context_file_concurrency(t *testing.T) {
	ctx := context.Background()

	t.Run("concurrent updates are serialized and none is lost", func(t *testing.T) {
		s := newFile(t).WithLockWait(20 * time.Second)
		const n = 16
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// each writer has its own store value, as separate processes would
				w := NewFileContextStore(s.Path()).WithLockWait(20 * time.Second)
				errs <- w.Update(ctx, define(fmt.Sprintf("c%02d", i), fmt.Sprintf("https://f%02d.example.invalid", i)))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Load(ctx)
		if err != nil || len(got.Contexts) != n {
			t.Fatalf("%d contexts, %v", len(got.Contexts), err)
		}
		if _, err := os.Stat(s.lockPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("lock file left behind")
		}
	})

	t.Run("a held lock fails visibly and is never stolen", func(t *testing.T) {
		s := newFile(t).WithLockWait(50 * time.Millisecond)
		if err := s.Update(ctx, define("a", "https://a.example.invalid")); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(s.Path())
		if err := os.WriteFile(s.lockPath(), nil, 0o600); err != nil { // abandoned lock
			t.Fatal(err)
		}
		err := s.Update(ctx, define("b", "https://b.example.invalid"))
		if !errors.Is(err, ErrContextLocked) || !strings.Contains(err.Error(), s.lockPath()) {
			t.Fatalf("want ErrContextLocked naming the lock, got %v", err)
		}
		if _, statErr := os.Stat(s.lockPath()); statErr != nil {
			t.Fatal("abandoned lock was removed automatically")
		}
		after, _ := os.ReadFile(s.Path())
		if string(before) != string(after) {
			t.Fatal("file changed under contention")
		}
		// operator recovery: remove the lock, retry
		_ = os.Remove(s.lockPath())
		if err := s.Update(ctx, define("b", "https://b.example.invalid")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failed update leaves the previous complete file and no debris", func(t *testing.T) {
		s := newFile(t)
		if err := s.Update(ctx, define("a", "https://a.example.invalid")); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(s.Path())

		s.rename = func(string, string) error { return errors.New("injected crash before rename") }
		if err := s.Update(ctx, define("b", "https://b.example.invalid")); err == nil {
			t.Fatal("expected failure")
		}
		s.rename = os.Rename
		after, _ := os.ReadFile(s.Path())
		if string(before) != string(after) {
			t.Fatal("previous file not preserved")
		}
		entries, _ := os.ReadDir(filepath.Dir(s.Path()))
		for _, e := range entries {
			if e.Name() != "contexts.json" {
				t.Errorf("leftover %s", e.Name())
			}
		}
		// an error from the callback stores nothing
		sentinel := errors.New("boom")
		if err := s.Update(ctx, func(*ContextSet) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		// an invalid result is not stored
		if err := s.Update(ctx, func(cs *ContextSet) error { cs.Current = "ghost"; return nil }); err == nil {
			t.Fatal("dangling selection stored")
		}
		if again, _ := os.ReadFile(s.Path()); string(again) != string(before) {
			t.Fatal("file changed")
		}
	})

	t.Run("an interrupted write's temporary file never corrupts the state", func(t *testing.T) {
		s := newFile(t)
		if err := s.Update(ctx, define("a", "https://a.example.invalid")); err != nil {
			t.Fatal(err)
		}
		want, _ := s.Load(ctx)
		partial := filepath.Join(filepath.Dir(s.Path()), ".contexts-crashed.tmp")
		if err := os.WriteFile(partial, []byte(`{"version":1,"contexts":{"zz":`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := s.Load(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%v %+v", err, got)
		}
		if err := s.Update(ctx, define("b", "https://b.example.invalid")); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Load(ctx)
		if len(got.Contexts) != 2 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("corrupt files fail visibly and are never overwritten", func(t *testing.T) {
		for name, content := range map[string]string{
			"truncated":     `{"version":1,"contexts":`,
			"unknown field": `{"version":1,"contexts":{},"extra":1}`,
			"wrong version": `{"version":9,"contexts":{}}`,
			"trailing":      `{"version":1,"contexts":{}} {}`,
			"dangling":      `{"version":1,"current":"x","contexts":{}}`,
			"project only":  `{"version":1,"contexts":{"a":{"auth":{},"project":"p"}}}`,
			"bad name":      `{"version":1,"contexts":{"a b":{"auth":{}}}}`,
			"non canonical": `{"version":1,"contexts":{"a":{"factory":"https://F.example.invalid/","auth":{}}}}`,
		} {
			s := newFile(t)
			_ = os.MkdirAll(filepath.Dir(s.Path()), 0o700)
			if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(ctx); err == nil {
				t.Errorf("%s: loaded", name)
			}
			if err := s.Update(ctx, define("n", "https://n.example.invalid")); err == nil {
				t.Errorf("%s: update over corrupt file succeeded", name)
			}
			if b, _ := os.ReadFile(s.Path()); string(b) != content {
				t.Errorf("%s: file rewritten", name)
			}
		}
	})

	t.Run("files are private and a missing file is an empty set", func(t *testing.T) {
		s := newFile(t)
		got, err := s.Load(ctx)
		if err != nil || len(got.Contexts) != 0 || got.Version != ContextSetVersion {
			t.Fatalf("%+v %v", got, err)
		}
		if err := s.Update(ctx, define("a", "https://a.example.invalid")); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(s.Path())
		di, _ := os.Stat(filepath.Dir(s.Path()))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
		}
		b, _ := os.ReadFile(s.Path())
		if strings.Contains(strings.ToLower(string(b)), "token") && !strings.Contains(string(b), "tokenEndpoint") {
			t.Fatal("unexpected token content")
		}
	})
}
