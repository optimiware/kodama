package secretkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var scopeKey = bytes.Repeat([]byte{0x5a}, 32)

func mustWrap(t *testing.T, p Provider, binding string) Wrapped {
	t.Helper()
	w, err := p.Wrap(context.Background(), scopeKey, []byte(binding))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func noLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{leak, simToken, "secret-sim", string(scopeKey)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("diagnostic leaked %q: %v", secret, err)
		}
	}
}

// VS-06/VS-A04: a wrap is bound to its context, so an altered binding or
// ciphertext, or a foreign provider, key or format, is rejected.
func Test_kms_context_tamper(t *testing.T) {
	for _, r := range newRigs(t) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			p := r.provider(r.keyA)
			w := mustWrap(t, p, "install|tenant-a|sk_1|1")
			if bytes.Contains(w.Ciphertext, scopeKey) {
				t.Fatal("wrapped bytes contain the scope key")
			}
			got, err := p.Unwrap(ctx, w, []byte("install|tenant-a|sk_1|1"))
			if err != nil || !bytes.Equal(got, scopeKey) {
				t.Fatal("round trip failed", err)
			}
			// Altered binding reaches the provider and is rejected there.
			for _, binding := range []string{"install|tenant-b|sk_1|1", "install|tenant-a|sk_1|2", "other|tenant-a|sk_1|1", ""} {
				key, err := p.Unwrap(ctx, w, []byte(binding))
				if !errors.Is(err, ErrCiphertext) || key != nil {
					t.Fatalf("binding %q: key=%v err=%v", binding, key != nil, err)
				}
				noLeak(t, err)
			}
			// Every altered ciphertext byte is rejected.
			for _, i := range []int{0, len(w.Ciphertext) / 2, len(w.Ciphertext) - 1} {
				bad := w
				bad.Ciphertext = bytes.Clone(w.Ciphertext)
				bad.Ciphertext[i] ^= 1
				if key, err := p.Unwrap(ctx, bad, []byte("install|tenant-a|sk_1|1")); !errors.Is(err, ErrCiphertext) || key != nil {
					t.Fatalf("ciphertext byte %d accepted: %v", i, err)
				}
			}
			// Foreign metadata is refused before any network call.
			before := r.sim.count()
			foreign := []Wrapped{}
			for _, mutate := range []func(*Wrapped){
				func(w *Wrapped) { w.Provider = "other" },
				func(w *Wrapped) { w.Provider = "random" },
				func(w *Wrapped) { w.Format = "aes256gcm-wrap-v1" },
				func(w *Wrapped) { w.Format += "-x" },
				func(w *Wrapped) { w.KeyRef = r.refOf(r.keyB) },
				func(w *Wrapped) { w.KeyRef = "" },
				func(w *Wrapped) { w.Ciphertext = nil },
				func(w *Wrapped) { w.Ciphertext = make([]byte, 8193) },
			} {
				c := w
				mutate(&c)
				foreign = append(foreign, c)
			}
			for _, c := range foreign {
				if key, err := p.Unwrap(ctx, c, []byte("install|tenant-a|sk_1|1")); !errors.Is(err, ErrCiphertext) || key != nil {
					t.Fatalf("foreign wrap %+v accepted: %v", c.Provider+"/"+c.Format+"/"+c.KeyRef, err)
				}
			}
			if r.sim.count() != before {
				t.Fatal("a foreign wrap reached the provider")
			}
			// Another cloud's envelope is not accepted.
			for _, other := range newRigs(t) {
				if other.name == r.name {
					continue
				}
				ow := mustWrap(t, other.provider(other.keyA), "install|tenant-a|sk_1|1")
				if _, err := p.Unwrap(ctx, ow, []byte("install|tenant-a|sk_1|1")); !errors.Is(err, ErrCiphertext) {
					t.Fatalf("%s envelope accepted by %s: %v", other.name, r.name, err)
				}
			}
		})
	}
}

// VS-07/VS-A05: rotation unwraps under the retained key and wraps under the
// current key, recovering identical material.
func Test_kms_rewrap(t *testing.T) {
	for _, r := range newRigs(t) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			binding := []byte("install|tenant-a|sk_1|1")
			old := mustWrap(t, r.provider(r.keyA), string(binding))
			if old.KeyRef != r.refOf(r.keyA) {
				t.Fatalf("old reference %q", old.KeyRef)
			}

			next := r.provider(r.keyB, r.keyA)
			material, err := next.Unwrap(ctx, old, binding)
			if err != nil || !bytes.Equal(material, scopeKey) {
				t.Fatal("retained unwrap failed", err)
			}
			rewrapped, err := next.Wrap(ctx, material, binding)
			if err != nil {
				t.Fatal(err)
			}
			if rewrapped.KeyRef != r.refOf(r.keyB) || rewrapped.Provider != old.Provider || rewrapped.Format != old.Format {
				t.Fatalf("rewrap metadata %+v", rewrapped)
			}
			again, err := next.Unwrap(ctx, rewrapped, binding)
			if err != nil || !bytes.Equal(again, scopeKey) {
				t.Fatal("rewrapped material differs", err)
			}
			// New wraps never use a retained key.
			for i := 0; i < 3; i++ {
				w, err := next.Wrap(ctx, scopeKey, binding)
				if err != nil || w.KeyRef != r.refOf(r.keyB) {
					t.Fatalf("new wrap used %q: %v", w.KeyRef, err)
				}
			}
			for _, c := range r.sim.log() {
				if c.Op == "Encrypt" || c.Op == "encrypt" {
					if c.Key != r.keyA && c.Key != r.keyB {
						t.Fatalf("encrypt used unconfigured key %q", c.Key)
					}
				}
			}
			encryptKeys := map[string]int{}
			for _, c := range r.sim.log() {
				if strings.EqualFold(c.Op, "encrypt") {
					encryptKeys[c.Key]++
				}
			}
			if encryptKeys[r.keyA] != 1 || encryptKeys[r.keyB] != 4 {
				t.Fatalf("encrypt calls by key: %v", encryptKeys)
			}
			if c := r.sim.log()[0]; c.PTLen != 32 {
				t.Fatalf("plaintext length on the wire %d", c.PTLen)
			}
			// The old key's provider cannot read the new wrap.
			if _, err := r.provider(r.keyA).Unwrap(ctx, rewrapped, binding); !errors.Is(err, ErrCiphertext) {
				t.Fatal("old-only provider unwrapped the new key", err)
			}
		})
	}
}

// VS-09/VS-A07: provider failures are classified and never return plaintext, and no
// other provider or cloud is tried.
func Test_kms_outage(t *testing.T) {
	for _, r := range newRigs(t) {
		t.Run(r.name, func(t *testing.T) {
			binding := []byte("install|tenant-a|sk_1|1")
			p := r.provider(r.keyA)
			w := mustWrap(t, p, string(binding))
			for _, tc := range []struct {
				fail string
				want error
			}{
				{"throttle", ErrUnavailable},
				{"outage", ErrUnavailable},
				{"drop", ErrUnavailable},
				{"denied", ErrDenied},
				{"missing", ErrNotFound},
			} {
				t.Run(tc.fail, func(t *testing.T) {
					r.sim.setFail(tc.fail)
					defer r.sim.setFail("")
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					key, err := p.Unwrap(ctx, w, binding)
					if !errors.Is(err, tc.want) || key != nil {
						t.Fatalf("unwrap: want %v, got %v (key %v)", tc.want, err, key != nil)
					}
					noLeak(t, err)
					out, err := p.Wrap(ctx, scopeKey, binding)
					if !errors.Is(err, tc.want) || out.Ciphertext != nil {
						t.Fatalf("wrap: want %v, got %v", tc.want, err)
					}
					noLeak(t, err)
				})
			}
			// A stalled provider ends with the caller's deadline as unavailable.
			r.sim.setFail("hang")
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			start := time.Now()
			key, err := p.Unwrap(ctx, w, binding)
			cancel()
			r.sim.setFail("")
			if !errors.Is(err, ErrUnavailable) || key != nil || time.Since(start) > 5*time.Second {
				t.Fatalf("hung provider: %v after %s", err, time.Since(start))
			}
			// A cancelled context never reaches the provider.
			before := r.sim.count()
			cancelled, stop := context.WithCancel(context.Background())
			stop()
			if _, err := p.Unwrap(cancelled, w, binding); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("cancelled: %v", err)
			}
			if r.sim.count() > before+1 {
				t.Fatal("cancelled request retried")
			}
			// Recovery: the same envelope unwraps once the provider returns.
			if got, err := p.Unwrap(context.Background(), w, binding); err != nil || !bytes.Equal(got, scopeKey) {
				t.Fatal("no recovery after outage", err)
			}
		})
	}
}

// VS-10/VS-A08: a restored envelope serialized before rotation unwraps with a fresh
// provider that still retains its key, and fails closed without it.
func Test_kms_restored_envelope(t *testing.T) {
	for _, r := range newRigs(t) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			binding := []byte("install|tenant-a|sk_1|1")
			stored, err := json.Marshal(mustWrap(t, r.provider(r.keyA), string(binding)))
			if err != nil {
				t.Fatal(err)
			}
			// Two rotations later.
			var restored Wrapped
			if err := json.Unmarshal(stored, &restored); err != nil {
				t.Fatal(err)
			}
			fresh := r.provider(r.keyC, r.keyB, r.keyA)
			got, err := fresh.Unwrap(ctx, restored, binding)
			if err != nil || !bytes.Equal(got, scopeKey) {
				t.Fatal("restored envelope did not unwrap", err)
			}
			before := r.sim.count()
			dropped := r.provider(r.keyC, r.keyB)
			if key, err := dropped.Unwrap(ctx, restored, binding); !errors.Is(err, ErrCiphertext) || key != nil {
				t.Fatalf("removed retained key accepted: %v", err)
			}
			if r.sim.count() != before {
				t.Fatal("removed retained key reached the provider")
			}
			// A retained key that the cloud no longer has fails as not found.
			r.sim.setFail("missing")
			defer r.sim.setFail("")
			if _, err := fresh.Unwrap(ctx, restored, binding); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted cloud key: %v", err)
			}
		})
	}
}
