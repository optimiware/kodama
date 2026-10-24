package secretstest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
)

// WrapperEnv is a secrets.KeyWrapper under contract.
type WrapperEnv struct {
	Wrapper secrets.KeyWrapper
	// Rotate makes a new KEK version current and returns a wrapper that still
	// unwraps everything the previous one wrapped (it may be the same value).
	Rotate func(t testing.TB) secrets.KeyWrapper
}

// OpenWrapper builds a fresh wrapper.
type OpenWrapper func(testing.TB) WrapperEnv

func contractWC() secrets.WrapContext {
	return secrets.WrapContext{InstallationID: "inst-t088", TenantID: "t088_acme", Generation: 1, KeyID: "sk_t088"}
}

func keyClass(t *testing.T, err error) secrets.KeyErrorClass {
	t.Helper()
	var ke *secrets.KeyError
	if !errors.As(err, &ke) {
		t.Fatalf("not a classified key error: %v", err)
	}
	return ke.Class
}

// WrapperContract is the behavior every secrets.KeyWrapper adapter shares: a
// wrap round-trips only under the context it was made for (installation, tenant,
// generation and key identity are all bound), tampered or foreign wraps are
// classified failures, a wrap names the KEK that made it and is unwrapped by it
// after a rotation, and a cancelled call is an unavailable provider. A failure
// never carries key material and nothing falls back to another provider.
func WrapperContract(t *testing.T, open OpenWrapper) {
	t.Helper()
	ctx := context.Background()
	scopeKey := bytes.Repeat([]byte{0x42}, 32)

	t.Run("a wrap round-trips under its context and hides the key", func(t *testing.T) {
		env := open(t)
		w, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		if w.KEK.Validate() != nil || w.Algorithm == "" || len(w.Bytes) == 0 || bytes.Contains(w.Bytes, scopeKey) {
			t.Fatalf("wrapped key: %+v", w)
		}
		got, err := env.Wrapper.Unwrap(ctx, contractWC(), w)
		if err != nil || !bytes.Equal(got, scopeKey) {
			t.Fatalf("unwrap: %x %v", got, err)
		}
		again, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil || bytes.Equal(again.Bytes, w.Bytes) {
			t.Fatalf("two wraps of one key must differ: %v", err)
		}
	})

	t.Run("a wrap is bound to installation, tenant, generation and key identity", func(t *testing.T) {
		env := open(t)
		w, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		for name, mutate := range map[string]func(*secrets.WrapContext){
			"installation": func(c *secrets.WrapContext) { c.InstallationID = "inst-other" },
			"tenant":       func(c *secrets.WrapContext) { c.TenantID = "t088_globex" },
			"generation":   func(c *secrets.WrapContext) { c.Generation = 2 },
			"key identity": func(c *secrets.WrapContext) { c.KeyID = "sk_other" },
		} {
			wc := contractWC()
			mutate(&wc)
			got, err := env.Wrapper.Unwrap(ctx, wc, w)
			if err == nil || got != nil || keyClass(t, err) != secrets.KeyCorrupt {
				t.Errorf("%s: unwrapped under another context: %x %v", name, got, err)
			}
		}
	})

	t.Run("tampered, foreign and malformed wraps are classified", func(t *testing.T) {
		env := open(t)
		w, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		tampered := w
		tampered.Bytes = append([]byte(nil), w.Bytes...)
		tampered.Bytes[len(tampered.Bytes)-1] ^= 0xff
		if _, err := env.Wrapper.Unwrap(ctx, contractWC(), tampered); keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("tampered: %v", err)
		}
		short := w
		short.Bytes = w.Bytes[:4]
		if _, err := env.Wrapper.Unwrap(ctx, contractWC(), short); keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("short: %v", err)
		}
		foreign := w
		foreign.KEK.Provider = "another-provider"
		if _, err := env.Wrapper.Unwrap(ctx, contractWC(), foreign); keyClass(t, err) != secrets.KeyNotFound {
			t.Errorf("another provider's wrap: %v", err)
		}
		unknown := w
		unknown.KEK.Version = "v-never-held"
		if _, err := env.Wrapper.Unwrap(ctx, contractWC(), unknown); keyClass(t, err) != secrets.KeyNotFound {
			t.Errorf("an unknown KEK version: %v", err)
		}
		if _, err := env.Wrapper.Wrap(ctx, contractWC(), []byte("short")); keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("a scope key that is not 32 bytes: %v", err)
		}
	})

	t.Run("an older wrap still unwraps after the KEK rotates", func(t *testing.T) {
		env := open(t)
		old, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		rotated := env.Rotate(t)
		got, err := rotated.Unwrap(ctx, contractWC(), old)
		if err != nil || !bytes.Equal(got, scopeKey) {
			t.Fatalf("unwrap after rotation: %v", err)
		}
		fresh, err := rotated.Wrap(ctx, contractWC(), scopeKey)
		if err != nil || fresh.KEK == old.KEK || fresh.KEK.Provider != old.KEK.Provider || fresh.KEK.ID != old.KEK.ID {
			t.Fatalf("a wrap after rotation must name the new KEK version: %+v vs %+v (%v)", fresh.KEK, old.KEK, err)
		}
	})

	t.Run("a cancelled call is an unavailable provider", func(t *testing.T) {
		env := open(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := env.Wrapper.Wrap(cancelled, contractWC(), scopeKey); keyClass(t, err) != secrets.KeyUnavailable {
			t.Errorf("wrap: %v", err)
		}
		w, err := env.Wrapper.Wrap(ctx, contractWC(), scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Wrapper.Unwrap(cancelled, contractWC(), w); keyClass(t, err) != secrets.KeyUnavailable {
			t.Errorf("unwrap: %v", err)
		}
	})
}

// OpenEphemeral is the OpenWrapper of the in-memory wrapper.
func OpenEphemeral(t testing.TB) WrapperEnv {
	t.Helper()
	w, err := NewEphemeralWrapper()
	if err != nil {
		t.Fatal(err)
	}
	return WrapperEnv{Wrapper: w, Rotate: func(t testing.TB) secrets.KeyWrapper {
		t.Helper()
		if _, err := w.Rotate(); err != nil {
			t.Fatal(err)
		}
		return w
	}}
}

// OpenKeyfile is the OpenWrapper of the development keyfile provider over key
// files in a temporary directory (the `test` environment's custody). It needs no
// service.
func OpenKeyfile(t testing.TB) WrapperEnv {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	add := func(version string) {
		key := make([]byte, keyfile.KeySize)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "master-key-"+version)
		if err := os.WriteFile(p, key, 0o600); err != nil {
			t.Fatal(err)
		}
		files[version] = p
	}
	provider := func(current string) *keyfile.Provider {
		set := map[string]string{}
		for v, p := range files {
			set[v] = p
		}
		p, err := keyfile.New(keyfile.Config{Development: true, Current: current, Files: set})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	add("v1")
	return WrapperEnv{Wrapper: provider("v1"), Rotate: func(t testing.TB) secrets.KeyWrapper {
		t.Helper()
		add("v2")
		return provider("v2")
	}}
}
