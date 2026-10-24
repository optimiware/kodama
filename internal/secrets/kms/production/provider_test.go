package production

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

func Test_provider_roundtrip_and_metadata(t *testing.T) {
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	p := bridgeWith(t, awsConfig(), f)
	w, err := p.Wrap(context.Background(), wctx(), scopeKey())
	if err != nil {
		t.Fatal(err)
	}
	if w.KEK != (secrets.KEKRef{Provider: "aws", ID: "kodama-prod", Version: "v2"}) || w.Algorithm != secretkeys.FormatAWS {
		t.Fatalf("persisted metadata %+v", w.KEK)
	}
	if err := w.KEK.Validate(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(w.Bytes, scopeKey()) || len(w.Bytes) > 4096 {
		t.Fatal("wrapped bytes unsafe")
	}
	// The persisted form is a valid ScopeKey wrap.
	if _, err := secrets.NewScopeKey("t_acme", "sk_abc", 3, w, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := p.Unwrap(context.Background(), wctx(), w)
	if err != nil || !bytes.Equal(got, scopeKey()) {
		t.Fatal("round trip", err)
	}
	w2, _ := p.Wrap(context.Background(), wctx(), scopeKey())
	if bytes.Equal(w.Bytes, w2.Bytes) {
		t.Fatal("two wraps produced identical ciphertext")
	}
	// The caller's key and the stored bytes are never aliased.
	w.Bytes[0] ^= 1
	if _, err := p.Unwrap(context.Background(), wctx(), w2); err != nil {
		t.Fatal("wraps share storage", err)
	}
}

// Companion of kms_context_tamper: every WrapContext and KEK field is bound.
func Test_provider_context_tamper(t *testing.T) {
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	p := bridgeWith(t, awsConfig(), f)
	ctx := context.Background()
	w, err := p.Wrap(ctx, wctx(), scopeKey())
	if err != nil {
		t.Fatal(err)
	}
	contexts := map[string]func(*secrets.WrapContext){
		"installation": func(c *secrets.WrapContext) { c.InstallationID = "inst-2" },
		"tenant":       func(c *secrets.WrapContext) { c.TenantID = "t_other" },
		"generation":   func(c *secrets.WrapContext) { c.Generation++ },
		"key id":       func(c *secrets.WrapContext) { c.KeyID = "sk_other" },
	}
	for name, mutate := range contexts {
		wc := wctx()
		mutate(&wc)
		key, err := p.Unwrap(ctx, wc, w)
		if err == nil || key != nil || keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("%s swap: key=%v err=%v", name, key != nil, err)
		}
	}
	// KEK metadata: version relabel reaches the KMS and fails authentication;
	// every other mutation is refused before a network call.
	before := f.calls.Load()
	refused := map[string]func(*secrets.WrappedKey){
		"provider":      func(w *secrets.WrappedKey) { w.KEK.Provider = "gcp" },
		"kek id":        func(w *secrets.WrappedKey) { w.KEK.ID = "other" },
		"unknown ver":   func(w *secrets.WrappedKey) { w.KEK.Version = "v9" },
		"algorithm":     func(w *secrets.WrappedKey) { w.Algorithm = secretkeys.FormatGCP },
		"empty":         func(w *secrets.WrappedKey) { w.Bytes = nil },
		"oversize":      func(w *secrets.WrappedKey) { w.Bytes = make([]byte, 4097) },
		"keyfile wrap":  func(w *secrets.WrappedKey) { w.KEK.Provider, w.Algorithm = "keyfile-dev", "aes-256-gcm/wrap-v1" },
		"empty version": func(w *secrets.WrappedKey) { w.KEK.Version = "" },
	}
	for name, mutate := range refused {
		c := w
		c.Bytes = bytes.Clone(w.Bytes)
		mutate(&c)
		key, err := p.Unwrap(ctx, wctx(), c)
		if err == nil || key != nil {
			t.Errorf("%s accepted", name)
			continue
		}
		// Unknown provider, KEK, algorithm or version label is a missing key;
		// malformed ciphertext is corrupt.
		want := secrets.KeyNotFound
		if name == "empty" || name == "oversize" {
			want = secrets.KeyCorrupt
		}
		if got := keyClass(t, err); got != want {
			t.Errorf("%s: class %v, want %v", name, got, want)
		}
	}
	if f.calls.Load() != before {
		t.Fatal("a foreign or malformed wrap reached the provider")
	}
	relabel := w
	relabel.KEK.Version = "v1" // retained key, but the binding names v2
	if key, err := p.Unwrap(ctx, wctx(), relabel); err == nil || key != nil || keyClass(t, err) != secrets.KeyCorrupt {
		t.Fatalf("relabelled version accepted: %v", err)
	}
	for _, i := range []int{0, len(w.Bytes) - 1} {
		c := w
		c.Bytes = bytes.Clone(w.Bytes)
		c.Bytes[i] ^= 1
		if key, err := p.Unwrap(ctx, wctx(), c); err == nil || key != nil || keyClass(t, err) != secrets.KeyCorrupt {
			t.Fatalf("tampered byte %d: %v", i, err)
		}
	}
	// Invalid context and key sizes fail before any call.
	before = f.calls.Load()
	if _, err := p.Wrap(ctx, secrets.WrapContext{}, scopeKey()); err == nil {
		t.Fatal("empty context accepted")
	}
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := p.Wrap(ctx, wctx(), make([]byte, n)); err == nil || keyClass(t, err) != secrets.KeyCorrupt {
			t.Fatalf("key length %d accepted", n)
		}
	}
	if _, err := p.Unwrap(ctx, secrets.WrapContext{}, w); err == nil {
		t.Fatal("empty context accepted on unwrap")
	}
	if f.calls.Load() != before {
		t.Fatal("invalid input reached the provider")
	}
}

func Test_provider_rejects_misbehaving_inner(t *testing.T) {
	for name, edit := range map[string]func(*fakeInner){
		"other key reference": func(f *fakeInner) { f.forceKR = awsKeyA },
		"other provider":      func(f *fakeInner) { f.cloud = "gcp" },
		"other format":        func(f *fakeInner) { f.format = "x" },
	} {
		f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
		edit(f)
		p := bridgeWith(t, awsConfig(), f)
		if w, err := p.Wrap(context.Background(), wctx(), scopeKey()); err == nil || keyClass(t, err) != secrets.KeyCorrupt || w.Bytes != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A retained version resolves only through its own mapping; new wraps never use it.
func Test_provider_rotation_resolution(t *testing.T) {
	ctx := context.Background()
	oldCfg := Config{Provider: AWS, KEKID: "kodama-prod", Current: "v1", Resources: map[string]string{"v1": awsKeyA}}
	oldF := newFake(AWS, secretkeys.FormatAWS, awsKeyA)
	oldW, err := bridgeWith(t, oldCfg, oldF).Wrap(ctx, wctx(), scopeKey())
	if err != nil || oldW.KEK.Version != "v1" {
		t.Fatal(err)
	}
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	cur := bridgeWith(t, awsConfig(), f)
	got, err := cur.Unwrap(ctx, wctx(), oldW)
	if err != nil || !bytes.Equal(got, scopeKey()) {
		t.Fatal("retained version did not unwrap", err)
	}
	nw, err := cur.Wrap(ctx, wctx(), got)
	if err != nil || nw.KEK.Version != "v2" {
		t.Fatalf("new wrap used %v: %v", nw.KEK, err)
	}
	// Same material, a different wrap.
	again, err := cur.Unwrap(ctx, wctx(), nw)
	if err != nil || !bytes.Equal(again, got) {
		t.Fatal("rewrapped material differs")
	}
	// Dropping the retained version fails closed as not found.
	dropped := with(awsConfig(), func(c *Config) { delete(c.Resources, "v1") })
	before := f.calls.Load()
	if _, err := bridgeWith(t, dropped, f).Unwrap(ctx, wctx(), oldW); err == nil || keyClass(t, err) != secrets.KeyNotFound {
		t.Fatalf("dropped retained version: %v", err)
	}
	if f.calls.Load() != before {
		t.Fatal("dropped version reached the provider")
	}
}

func Test_provider_concurrency_and_cancellation(t *testing.T) {
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	p := bridgeWith(t, awsConfig(), f)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wc := wctx()
			wc.TenantID = domain.TenantID(fmt.Sprintf("t_%d", i))
			w, err := p.Wrap(context.Background(), wc, scopeKey())
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := p.Unwrap(context.Background(), wc, w); err != nil {
				t.Error(err)
			}
			other := wc
			other.TenantID = domain.TenantID(fmt.Sprintf("t_%d", i+1000))
			if _, err := p.Unwrap(context.Background(), other, w); err == nil {
				t.Error("tenant substitution accepted")
			}
		}()
	}
	wg.Wait()

	// Cancelled contexts never reach the provider and are retryable.
	before := f.calls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Wrap(ctx, wctx(), scopeKey())
	var ke *secrets.KeyError
	if !errors.As(err, &ke) || !ke.Retryable() || !errors.Is(err, context.Canceled) || f.calls.Load() != before {
		t.Fatalf("cancelled wrap: %v", err)
	}
	// A stalled provider ends with the caller's deadline, classified retryable.
	f.mu.Lock()
	f.block = make(chan struct{})
	f.mu.Unlock()
	dctx, dcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer dcancel()
	key, err := p.Unwrap(dctx, wctx(), mustWrapped(t, p))
	if err == nil || key != nil || !errors.As(err, &ke) || !ke.Retryable() {
		t.Fatalf("stalled provider: %v", err)
	}
}

func mustWrapped(t *testing.T, p *Provider) secrets.WrappedKey {
	t.Helper()
	// The fake may be blocked; wrap with a clean fake of the same shape.
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	w, err := bridgeWith(t, awsConfig(), f).Wrap(context.Background(), wctx(), scopeKey())
	if err != nil {
		t.Fatal(err)
	}
	return w
}
