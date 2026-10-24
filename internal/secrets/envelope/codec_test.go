package envelope

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func testContext() Context {
	return Context{
		InstallationID: "inst-1", TenantID: "t_acme", ProjectID: "p_inventory",
		SecretID: "sec_1", Name: "erp-token", Version: 3,
		ScopeKeyID: "sk_1", KeyGeneration: 1,
	}
}

func testNonce() []byte { return bytes.Repeat([]byte{7}, NonceSize) }

func Test_roundtrip(t *testing.T) {
	c := testContext()
	for _, pt := range [][]byte{{}, []byte("x"), bytes.Repeat([]byte{0xff}, MaxPlaintextBytes)} {
		e, err := Seal(testKey(), c, pt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Open(testKey(), c, e)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("roundtrip failed: %v", err)
		}
	}
	a, _ := Seal(testKey(), c, []byte("same"))
	b, _ := Seal(testKey(), c, []byte("same"))
	if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("two seals reused a nonce")
	}
}

// Test_context_swap is acceptance context_swap (VS-06, VS-A04): changing the
// ciphertext, the nonce or any authenticated ownership/version field fails
// authentication.
func Test_context_swap(t *testing.T) {
	c := testContext()
	e, err := Seal(testKey(), c, []byte("sentinel-secret"))
	if err != nil {
		t.Fatal(err)
	}

	contextMutations := map[string]func(*Context){
		"installation": func(c *Context) { c.InstallationID = "inst-2" },
		"tenant":       func(c *Context) { c.TenantID = "t_globex" },
		"project":      func(c *Context) { c.ProjectID = "p_billing" },
		"secret id":    func(c *Context) { c.SecretID = "sec_2" },
		"name":         func(c *Context) { c.Name = "erp-token2" },
		"version":      func(c *Context) { c.Version = 4 },
		"scope key id": func(c *Context) { c.ScopeKeyID = "sk_2" },
		"generation":   func(c *Context) { c.KeyGeneration = 2 },
	}
	for name, mutate := range contextMutations {
		t.Run("context/"+name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			if _, err := Open(testKey(), bad, e); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("swapped %s opened or wrong error: %v", name, err)
			}
		})
	}

	t.Run("ciphertext bit flip", func(t *testing.T) {
		for i := range e.Ciphertext {
			bad := e
			bad.Ciphertext = append([]byte(nil), e.Ciphertext...)
			bad.Ciphertext[i] ^= 0x01
			if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("flip at %d accepted: %v", i, err)
			}
		}
	})
	t.Run("nonce bit flip", func(t *testing.T) {
		for i := range e.Nonce {
			bad := e
			bad.Nonce = append([]byte(nil), e.Nonce...)
			bad.Nonce[i] ^= 0x80
			if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("nonce flip at %d accepted: %v", i, err)
			}
		}
	})
	t.Run("another secret's ciphertext", func(t *testing.T) {
		other := c
		other.SecretID, other.Name = "sec_9", "other"
		oe, _ := Seal(testKey(), other, []byte("other-secret"))
		if _, err := Open(testKey(), c, oe); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("swapped ciphertext accepted: %v", err)
		}
	})
	t.Run("wrong scope key", func(t *testing.T) {
		k := testKey()
		k[0] ^= 1
		if _, err := Open(k, c, e); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("wrong key accepted: %v", err)
		}
	})
	t.Run("errors never carry the value", func(t *testing.T) {
		bad := e
		bad.Ciphertext = append([]byte(nil), e.Ciphertext...)
		bad.Ciphertext[0] ^= 1
		_, err := Open(testKey(), c, bad)
		if err == nil || strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("error %v", err)
		}
	})
}

func Test_unsupported_and_malformed(t *testing.T) {
	c := testContext()
	e, _ := Seal(testKey(), c, []byte("v"))

	bad := e
	bad.FormatVersion = 2
	if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown format: %v", err)
	}
	bad = e
	bad.Algorithm = "chacha20"
	if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown algorithm: %v", err)
	}
	bad = e
	bad.Nonce = e.Nonce[:5]
	if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short nonce: %v", err)
	}
	bad = e
	bad.Ciphertext = e.Ciphertext[:3]
	if _, err := Open(testKey(), c, bad); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short ciphertext: %v", err)
	}
	if _, err := Open(testKey()[:16], c, e); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short scope key: %v", err)
	}
	z := c
	z.Version = 0
	if _, err := Seal(testKey(), z, []byte("v")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("zero version: %v", err)
	}
	if _, err := Seal(testKey(), c, make([]byte, MaxPlaintextBytes+1)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized value: %v", err)
	}
}

// Test_rewrap_stability: derivation depends on ownership and version only, so a
// scope key that is merely rewrapped keeps decrypting every retained value.
func Test_derivation_ignores_scope_key_identity(t *testing.T) {
	a, b := testContext(), testContext()
	b.ScopeKeyID, b.KeyGeneration = "sk_other", 9
	ka, _ := valueKey(testKey(), a)
	kb, _ := valueKey(testKey(), b)
	if !bytes.Equal(ka, kb) {
		t.Fatal("value key depends on scope key identity")
	}
	c := testContext()
	c.Version++
	kc, _ := valueKey(testKey(), c)
	if bytes.Equal(ka, kc) {
		t.Fatal("value keys of two versions are equal")
	}
}

// Test_deterministic_vector freezes the framing. A change to this vector is a
// change of the persisted format and needs a new FormatVersion.
func Test_deterministic_vector(t *testing.T) {
	e, err := sealWithNonce(testKey(), testContext(), []byte("kodama-vector"), testNonce())
	if err != nil {
		t.Fatal(err)
	}
	const want = "87d4ea3b2ee94bce00e37e9a9f74b8fee9773dfa07ed83c32222b02db0"
	if got := hex.EncodeToString(e.Ciphertext); got != want {
		t.Fatalf("ciphertext vector = %s", got)
	}
	got, err := Open(testKey(), testContext(), e)
	if err != nil || string(got) != "kodama-vector" {
		t.Fatalf("vector does not open: %v", err)
	}
}

func Test_new_scope_key(t *testing.T) {
	a, _ := NewScopeKey()
	b, _ := NewScopeKey()
	if len(a) != KeySize || bytes.Equal(a, b) {
		t.Fatal("scope keys are not 32 random bytes")
	}
}
