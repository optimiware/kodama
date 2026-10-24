package keyfile

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
)

func writeKey(t *testing.T, dir, name string) string {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, k, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func wc() secrets.WrapContext {
	return secrets.WrapContext{InstallationID: "inst-1", TenantID: "t_acme", Generation: 1, KeyID: "sk_1"}
}

func scopeKey() []byte { return bytes.Repeat([]byte{0x42}, KeySize) }

func keyClass(t *testing.T, err error) secrets.KeyErrorClass {
	t.Helper()
	var ke *secrets.KeyError
	if !errors.As(err, &ke) {
		t.Fatalf("not a classified key error: %v", err)
	}
	return ke.Class
}

func Test_wrap_roundtrip_and_binding(t *testing.T) {
	dir := t.TempDir()
	p, err := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": writeKey(t, dir, "master-key")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, err := p.Wrap(ctx, wc(), scopeKey())
	if err != nil {
		t.Fatal(err)
	}
	if w.KEK != (secrets.KEKRef{Provider: ProviderName, ID: DefaultKeyID, Version: "v1"}) || w.Algorithm != WrapAlgorithm {
		t.Fatalf("KEK reference %+v", w.KEK)
	}
	if bytes.Contains(w.Bytes, scopeKey()) {
		t.Fatal("wrapped bytes contain the scope key")
	}
	got, err := p.Unwrap(ctx, wc(), w)
	if err != nil || !bytes.Equal(got, scopeKey()) {
		t.Fatalf("unwrap: %v", err)
	}
	// A wrap cannot be moved to another installation, tenant, generation or key.
	for name, mutate := range map[string]func(*secrets.WrapContext){
		"installation": func(c *secrets.WrapContext) { c.InstallationID = "inst-2" },
		"tenant":       func(c *secrets.WrapContext) { c.TenantID = "t_globex" },
		"generation":   func(c *secrets.WrapContext) { c.Generation = 2 },
		"key id":       func(c *secrets.WrapContext) { c.KeyID = "sk_2" },
	} {
		c := wc()
		mutate(&c)
		if _, err := p.Unwrap(ctx, c, w); keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("%s swap not rejected as corrupt: %v", name, err)
		}
	}
	// A tampered wrap fails authentication.
	bad := w
	bad.Bytes = append([]byte(nil), w.Bytes...)
	bad.Bytes[len(bad.Bytes)-1] ^= 1
	if _, err := p.Unwrap(ctx, wc(), bad); keyClass(t, err) != secrets.KeyCorrupt {
		t.Fatalf("tampered wrap: %v", err)
	}
}

// Test_key_provider_failure covers VS-09/VS-A07: classified failures and no
// fallback of any kind.
func Test_key_provider_failure(t *testing.T) {
	ctx := context.Background()

	t.Run("development selection is explicit", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := New(Config{Current: "v1", Files: map[string]string{"v1": writeKey(t, dir, "k")}}); err == nil {
			t.Fatal("provider constructed without explicit development selection")
		}
	})

	t.Run("missing and malformed keys fail construction and are never generated", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "absent")
		_, err := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": missing}})
		if err == nil || keyClass(t, err) != secrets.KeyNotFound {
			t.Fatalf("missing key: %v", err)
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("a replacement key file was generated")
		}
		short := filepath.Join(dir, "short")
		if err := os.WriteFile(short, []byte("too short"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": short}}); err == nil || keyClass(t, err) != secrets.KeyCorrupt {
			t.Fatalf("malformed key: %v", err)
		}
		if _, err := New(Config{Development: true, Current: "v2", Files: map[string]string{"v1": writeKey(t, dir, "ok")}}); err == nil {
			t.Fatal("current version outside the configured files accepted")
		}
		if _, err := New(Config{Development: true}); err == nil {
			t.Fatal("empty configuration accepted")
		}
	})

	t.Run("unavailable retained version has no fallback", func(t *testing.T) {
		dir := t.TempDir()
		k1, k2 := writeKey(t, dir, "k1"), writeKey(t, dir, "k2")
		old, _ := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": k1}})
		w, err := old.Wrap(ctx, wc(), scopeKey())
		if err != nil {
			t.Fatal(err)
		}
		// The new configuration forgot the retained v1 key.
		cur, err := New(Config{Development: true, Current: "v2", Files: map[string]string{"v2": k2}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cur.Unwrap(ctx, wc(), w); keyClass(t, err) != secrets.KeyNotFound {
			t.Fatalf("missing retained version: %v", err)
		}
		// Retaining v1 restores access; v2 wraps new keys.
		both, _ := New(Config{Development: true, Current: "v2", Files: map[string]string{"v1": k1, "v2": k2}})
		if got, err := both.Unwrap(ctx, wc(), w); err != nil || !bytes.Equal(got, scopeKey()) {
			t.Fatalf("retained v1: %v", err)
		}
		w2, _ := both.Wrap(ctx, wc(), scopeKey())
		if w2.KEK.Version != "v2" {
			t.Fatalf("new wrap used %s", w2.KEK.Version)
		}
		// The old-only provider cannot read v2 wraps either.
		if _, err := old.Unwrap(ctx, wc(), w2); keyClass(t, err) != secrets.KeyNotFound {
			t.Fatalf("v1-only provider read a v2 wrap: %v", err)
		}
	})

	t.Run("key removed or corrupted after startup is classified", func(t *testing.T) {
		dir := t.TempDir()
		path := writeKey(t, dir, "k")
		p, _ := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": path}})
		w, _ := p.Wrap(ctx, wc(), scopeKey())
		if err := os.WriteFile(path, []byte("corrupted"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Unwrap(ctx, wc(), w); keyClass(t, err) != secrets.KeyCorrupt {
			t.Fatalf("corrupted key: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Wrap(ctx, wc(), scopeKey()); keyClass(t, err) != secrets.KeyNotFound {
			t.Fatalf("removed key: %v", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("a replacement key was generated")
		}
	})

	t.Run("denial is classified", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("file permissions do not restrict root")
		}
		dir := t.TempDir()
		path := writeKey(t, dir, "k")
		p, _ := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": path}})
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		_, err := p.Wrap(ctx, wc(), scopeKey())
		if keyClass(t, err) != secrets.KeyDenied {
			t.Fatalf("unreadable key: %v", err)
		}
		var ke *secrets.KeyError
		if errors.As(err, &ke) && ke.Retryable() {
			t.Fatal("denial reported as retryable")
		}
	})

	t.Run("cancellation is retryable unavailability", func(t *testing.T) {
		dir := t.TempDir()
		p, _ := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": writeKey(t, dir, "k")}})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := p.Wrap(cctx, wc(), scopeKey())
		var ke *secrets.KeyError
		if !errors.As(err, &ke) || ke.Class != secrets.KeyUnavailable || !ke.Retryable() {
			t.Fatalf("cancelled wrap: %v", err)
		}
	})

	t.Run("foreign provider wraps are refused", func(t *testing.T) {
		dir := t.TempDir()
		p, _ := New(Config{Development: true, Current: "v1", Files: map[string]string{"v1": writeKey(t, dir, "k")}})
		w, _ := p.Wrap(ctx, wc(), scopeKey())
		w.KEK.Provider = "aws-kms"
		if _, err := p.Unwrap(ctx, wc(), w); keyClass(t, err) != secrets.KeyNotFound {
			t.Fatalf("foreign provider wrap: %v", err)
		}
	})
}
