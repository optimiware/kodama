package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/optimiware/kodama/internal/cli/root"
)

// fakeKeyring is an in-memory Keyring with an outage switch.
type fakeKeyring struct {
	mu     sync.Mutex
	data   map[string]string
	outage error
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{data: map[string]string{}} }

func (f *fakeKeyring) Set(service, user, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outage != nil {
		return f.outage
	}
	f.data[service+"/"+user] = secret
	return nil
}

func (f *fakeKeyring) dataCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}

func (f *fakeKeyring) Get(service, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outage != nil {
		return "", f.outage
	}
	v, ok := f.data[service+"/"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outage != nil {
		return f.outage
	}
	if _, ok := f.data[service+"/"+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.data, service+"/"+user)
	return nil
}

var testCred = Credential{
	AccessToken: "tok-t017", TokenType: "Bearer",
	Expiry:     time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	ObtainedAt: time.Now().UTC().Truncate(time.Second),
}

func ctxbg() context.Context { return context.Background() }

func Test_credential_storage(t *testing.T) {
	const key = "0123abcd"

	t.Run("keyring is the default and round-trips", func(t *testing.T) {
		kr := newFakeKeyring()
		svc := NewService(ServiceConfig{Keyring: NewKeyringStore(kr, "t017-svc")})
		ec := root.EffectiveContext{Context: root.Context{Factory: "https://f.t017.example.invalid"}, CredentialStore: root.StoreKeyring}
		store, err := svc.store("")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(ctxbg(), ec.CredentialKey(), testCred); err != nil {
			t.Fatal(err)
		}
		if len(kr.data) != 1 {
			t.Fatalf("keyring holds %d entries", len(kr.data))
		}
		for k := range kr.data {
			if !strings.HasPrefix(k, "t017-svc/") {
				t.Fatalf("namespace = %q", k)
			}
		}
		got, err := store.Load(ctxbg(), ec.CredentialKey())
		if err != nil || got.AccessToken != testCred.AccessToken || !got.Expiry.Equal(testCred.Expiry) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("unavailable keyring fails and never falls back to a file", func(t *testing.T) {
		kr := newFakeKeyring()
		kr.outage = errors.New("dbus: no secret service")
		dir := filepath.Join(t.TempDir(), "creds")
		svc := NewService(ServiceConfig{Keyring: NewKeyringStore(kr, "t017"), File: NewFileStore(dir)})
		ks, _ := svc.store(root.StoreKeyring)
		if err := ks.Save(ctxbg(), key, testCred); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("save: %v", err)
		}
		if _, err := ks.Load(ctxbg(), key); !errors.Is(err, ErrStoreUnavailable) || errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("an outage must not look like a missing credential: %v", err)
		}
		if err := ks.Delete(ctxbg(), key); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("delete: %v", err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the file store must not have been touched")
		}
		// A command sees the outage as exit 1, not as "log in".
		src, _ := svc.TokenSource(root.EffectiveContext{CredentialStore: root.StoreKeyring})
		_, err := src.AccessToken(ctxbg())
		if p := root.Classify(err); p.Exit != root.ExitFailure || p.Code != "credential_store_unavailable" {
			t.Fatalf("classified %+v", p)
		}
		if strings.Contains(root.Classify(err).Message, "dbus") {
			t.Fatalf("backend detail leaked: %v", err)
		}
	})

	t.Run("file store is selected only by explicit opt-in", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		kr := newFakeKeyring()
		svc := NewService(ServiceConfig{Keyring: NewKeyringStore(kr, "t017"), File: NewFileStore(dir)})
		def, _ := svc.store("")
		_ = def.Save(ctxbg(), key, testCred)
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("default store wrote a file")
		}
		fs, err := svc.store(root.StoreFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.Save(ctxbg(), key, testCred); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, key+".json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("credential file: %v %v", fi, err)
		}
		if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %o", di.Mode().Perm())
		}
		got, err := fs.Load(ctxbg(), key)
		if err != nil || got.AccessToken != testCred.AccessToken {
			t.Fatalf("%+v %v", got, err)
		}
		if _, err := svc.store("memory"); err == nil {
			t.Fatal("unknown store accepted")
		}
	})

	t.Run("insecure destinations are rejected not repaired", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		fs := NewFileStore(dir)
		if err := fs.Save(ctxbg(), key, testCred); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, key+".json")

		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Load(ctxbg(), key); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("0644 file: %v", err)
		}
		if err := fs.Save(ctxbg(), key, testCred); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("saving over a 0644 file: %v", err)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
			t.Fatal("permissions were silently changed")
		}
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}

		// symlinked credential file
		target := filepath.Join(t.TempDir(), "elsewhere")
		_ = os.WriteFile(target, []byte("{}"), 0o600)
		_ = os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Load(ctxbg(), key); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("symlink file: %v", err)
		}
		if err := fs.Save(ctxbg(), key, testCred); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("saving through a symlink: %v", err)
		}
		if b, _ := os.ReadFile(target); string(b) != "{}" {
			t.Fatal("the symlink target was written")
		}
		_ = os.Remove(p)

		// loose directory
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.Save(ctxbg(), key, testCred); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("0755 dir: %v", err)
		}
		if _, err := fs.Load(ctxbg(), key); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("0755 dir load: %v", err)
		}
		_ = os.Chmod(dir, 0o700)

		// symlinked directory
		link := filepath.Join(t.TempDir(), "linkdir")
		realDir := t.TempDir()
		_ = os.Chmod(realDir, 0o700)
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		if err := NewFileStore(link).Save(ctxbg(), key, testCred); !errors.Is(err, ErrInsecureStore) {
			t.Fatalf("symlink dir: %v", err)
		}
		if p := root.Classify(&StoreError{Kind: ErrInsecureStore}); p.Code != "credential_store_insecure" || p.Exit != root.ExitFailure {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("persistence failure preserves the previous credential", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		fs := NewFileStore(dir)
		if err := fs.Save(ctxbg(), key, testCred); err != nil {
			t.Fatal(err)
		}
		fs.rename = func(string, string) error { return errors.New("disk on fire") }
		next := testCred
		next.AccessToken = "tok-new"
		if err := fs.Save(ctxbg(), key, next); !errors.Is(err, ErrPersistenceFailed) {
			t.Fatalf("save: %v", err)
		}
		fs.rename = os.Rename
		got, err := fs.Load(ctxbg(), key)
		if err != nil || got.AccessToken != testCred.AccessToken {
			t.Fatalf("previous credential lost: %+v %v", got, err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("temporary files left behind: %v", entries)
		}
		kr := newFakeKeyring()
		kr.outage = keyring.ErrSetDataTooBig
		if err := NewKeyringStore(kr, "t017").Save(ctxbg(), key, testCred); !errors.Is(err, ErrPersistenceFailed) {
			t.Fatalf("too big: %v", err)
		}
	})

	t.Run("logout is idempotent", func(t *testing.T) {
		kr := newFakeKeyring()
		svc := NewService(ServiceConfig{Keyring: NewKeyringStore(kr, "t017"), File: NewFileStore(filepath.Join(t.TempDir(), "c"))})
		for _, kind := range []string{root.StoreKeyring, root.StoreFile} {
			ec := root.EffectiveContext{Context: root.Context{Factory: "https://f.t017.example.invalid"}, CredentialStore: kind}
			st, _ := svc.store(kind)
			_ = st.Save(ctxbg(), ec.CredentialKey(), testCred)
			for i := 0; i < 2; i++ {
				if err := svc.Logout(ctxbg(), ec); err != nil {
					t.Fatalf("%s logout %d: %v", kind, i, err)
				}
			}
			if _, err := st.Load(ctxbg(), ec.CredentialKey()); !errors.Is(err, ErrCredentialNotFound) {
				t.Fatalf("%s still holds a credential: %v", kind, err)
			}
		}
	})

	t.Run("keys are validated and credentials never render", func(t *testing.T) {
		fs := NewFileStore(filepath.Join(t.TempDir(), "c"))
		for _, bad := range []string{"", "../x", "a/b", ".hidden", strings.Repeat("a", 200)} {
			if err := fs.Save(ctxbg(), bad, testCred); err == nil {
				t.Errorf("key %q accepted", bad)
			}
		}
		s := fmt.Sprintf("%v %+v %#v %s", testCred, testCred, testCred, testCred)
		if strings.Contains(s, "tok-t017") {
			t.Fatalf("credential rendered a token: %s", s)
		}
		if _, err := encodeCredential(Credential{}); err == nil {
			t.Fatal("empty credential encoded")
		}
		if _, err := decodeCredential([]byte(`{"version":2,"accessToken":"x"}`)); err == nil {
			t.Fatal("unknown version accepted")
		}
	})

	t.Run("distinct factories use distinct keys", func(t *testing.T) {
		a := root.Context{Factory: "https://a.t017.example.invalid"}
		b := root.Context{Factory: "https://b.t017.example.invalid"}
		if a.CredentialKey() == b.CredentialKey() {
			t.Fatal("credential keys collide across factories")
		}
	})
}

// promptKeyring simulates a locked OS keyring: while locked, every backend
// operation blocks the way an Unlock prompt does, until release is closed.
type promptKeyring struct {
	fakeKeyring
	locked   bool
	probeErr error
	release  chan struct{}
	calls    int32
	probes   int32
}

func newPromptKeyring(locked bool) *promptKeyring {
	return &promptKeyring{fakeKeyring: *newFakeKeyring(), locked: locked, release: make(chan struct{})}
}

func (p *promptKeyring) block() {
	atomic.AddInt32(&p.calls, 1)
	if p.locked {
		<-p.release
	}
}
func (p *promptKeyring) Set(s, u, v string) error { p.block(); return p.fakeKeyring.Set(s, u, v) }
func (p *promptKeyring) Get(s, u string) (string, error) {
	p.block()
	return p.fakeKeyring.Get(s, u)
}
func (p *promptKeyring) Delete(s, u string) error { p.block(); return p.fakeKeyring.Delete(s, u) }

// probingKeyring adds the PromptProber capability to a promptKeyring.
type probingKeyring struct{ *promptKeyring }

func (p probingKeyring) PromptFree(context.Context) error {
	atomic.AddInt32(&p.probes, 1)
	if p.probeErr != nil {
		return p.probeErr
	}
	if p.locked {
		return ErrInteractionRequired
	}
	return nil
}

func (p *promptKeyring) backendCalls() int { return int(atomic.LoadInt32(&p.calls)) }

func Test_keyring_prompt_policy(t *testing.T) {
	const key = "0123abcd"
	ec := func(nonInteractive bool) root.EffectiveContext {
		return root.EffectiveContext{
			Context:         root.Context{Factory: "https://f.t017.example.invalid"},
			CredentialStore: root.StoreKeyring, NonInteractive: nonInteractive,
		}
	}
	svcFor := func(kr Keyring) *Service {
		return NewService(ServiceConfig{Keyring: NewKeyringStore(kr, "t017-prompt"), File: NewFileStore(filepath.Join(t.TempDir(), "c"))})
	}
	requireRefusal := func(t *testing.T, err error, kr *promptKeyring) {
		t.Helper()
		if !errors.Is(err, ErrInteractionRequired) {
			t.Fatalf("want interaction required, got %v", err)
		}
		if p := root.Classify(err); p.Exit != root.ExitUsage || p.Code != "credential_store_interaction_required" ||
			strings.Contains(p.Message, "dbus") {
			t.Fatalf("classified %+v", p)
		}
		if n := kr.backendCalls(); n != 0 {
			t.Fatalf("the backend was invoked %d times; it may have prompted", n)
		}
	}

	t.Run("a locked keyring fails token lookup and logout non-interactively before any backend call", func(t *testing.T) {
		kr := newPromptKeyring(true)
		defer close(kr.release)
		svc := svcFor(probingKeyring{kr})

		src, err := svc.TokenSource(ec(true))
		if err != nil {
			t.Fatal(err)
		}
		_, err = src.AccessToken(ctxbg())
		requireRefusal(t, err, kr)

		requireRefusal(t, svc.Logout(ctxbg(), ec(true)), kr)
		if n := atomic.LoadInt32(&kr.probes); n != 2 {
			t.Fatalf("probed %d times", n)
		}
	})

	t.Run("an unlocked keyring works non-interactively", func(t *testing.T) {
		kr := newPromptKeyring(false)
		svc := svcFor(probingKeyring{kr})
		store, _ := svc.store(root.StoreKeyring)
		if err := store.Save(ctxbg(), ec(false).CredentialKey(), testCred); err != nil {
			t.Fatal(err)
		}
		src, _ := svc.TokenSource(ec(true))
		if tok, err := src.AccessToken(ctxbg()); err != nil || tok != testCred.AccessToken {
			t.Fatalf("%q %v", tok, err)
		}
		if err := svc.Logout(ctxbg(), ec(true)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a keyring that cannot prove it will not prompt is refused non-interactively", func(t *testing.T) {
		kr := newPromptKeyring(false) // no PromptProber
		svc := svcFor(kr)
		src, _ := svc.TokenSource(ec(true))
		_, err := src.AccessToken(ctxbg())
		requireRefusal(t, err, kr)
		requireRefusal(t, svc.Logout(ctxbg(), ec(true)), kr)
		store, err := svc.store(root.StoreKeyring)
		if err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, store.Save(WithNonInteractive(ctxbg()), key, testCred), kr)
	})

	t.Run("a probe failure is an outage, not a prompt, and still no backend call", func(t *testing.T) {
		kr := newPromptKeyring(false)
		kr.probeErr = errors.New("dbus: connection refused")
		svc := svcFor(probingKeyring{kr})
		src, _ := svc.TokenSource(ec(true))
		_, err := src.AccessToken(ctxbg())
		if !errors.Is(err, ErrStoreUnavailable) || kr.backendCalls() != 0 {
			t.Fatalf("%v (calls %d)", err, kr.backendCalls())
		}
		if strings.Contains(root.Classify(err).Message, "dbus") {
			t.Fatal("backend detail leaked")
		}
	})

	t.Run("a keyring prompt cannot outlive the timeout or an interrupt", func(t *testing.T) {
		kr := newPromptKeyring(true)
		defer close(kr.release)
		svc := svcFor(kr) // interactive: the prompt is allowed but bounded
		src, _ := svc.TokenSource(ec(false))

		for name, mk := range map[string]func() (context.Context, context.CancelFunc){
			"timeout": func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 50*time.Millisecond)
			},
			"interrupt": func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(50*time.Millisecond, cancel)
				return ctx, cancel
			},
		} {
			ctx, cancel := mk()
			start := time.Now()
			_, err := src.AccessToken(ctx)
			cancel()
			if time.Since(start) > 2*time.Second {
				t.Fatalf("%s: the call outlived its context", name)
			}
			if p := root.Classify(err); p.Exit != root.ExitTimeout {
				t.Fatalf("%s: classified %+v (%v)", name, p, err)
			}
			if err := svc.Logout(ctx, ec(false)); root.Classify(err).Exit != root.ExitTimeout {
				t.Fatalf("%s logout: %v", name, err)
			}
		}
		// An already-expired context never reaches the backend.
		before := kr.backendCalls()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = src.AccessToken(ctx)
		if kr.backendCalls() != before {
			t.Fatal("a cancelled context still invoked the backend")
		}
	})

	t.Run("the probe is bounded too", func(t *testing.T) {
		kr := newPromptKeyring(false)
		hung := hangingProbe{kr}
		svc := svcFor(hung)
		src, _ := svc.TokenSource(ec(true))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := src.AccessToken(ctx)
		if p := root.Classify(err); p.Exit != root.ExitTimeout || kr.backendCalls() != 0 {
			t.Fatalf("%+v calls=%d", p, kr.backendCalls())
		}
	})

	t.Run("the real keyring adapter exposes a prompt probe", func(t *testing.T) {
		if _, ok := SystemKeyring().(PromptProber); !ok {
			t.Fatal("the OS keyring must implement PromptProber on every platform")
		}
	})

	t.Run("the runner refuses logout non-interactively and keeps the stream contract", func(t *testing.T) {
		kr := newPromptKeyring(true)
		defer close(kr.release)
		svc := svcFor(probingKeyring{kr})
		var out, errb strings.Builder
		runner, err := root.NewRunner(root.RunnerConfig{
			Streams:  root.Streams{Out: &out, Err: &errb},
			Contexts: root.NewFileContextStore(filepath.Join(t.TempDir(), "contexts.json")),
			Tokens:   svc,
		}, NewCommands(svc))
		if err != nil {
			t.Fatal(err)
		}
		code := runner.Run(ctxbg(), []string{"auth", "logout", "--factory", "https://f.t017.example.invalid", "--non-interactive", "--output", "json"})
		if code != root.ExitUsage || out.Len() != 0 || !strings.Contains(errb.String(), `"code":"credential_store_interaction_required"`) || kr.backendCalls() != 0 {
			t.Fatalf("exit %d out %q err %q calls %d", code, out.String(), errb.String(), kr.backendCalls())
		}
	})
}

type hangingProbe struct{ *promptKeyring }

func (hangingProbe) PromptFree(context.Context) error { select {} }
