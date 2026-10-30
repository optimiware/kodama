package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

func newProvider(t *testing.T) *secretkeys.Local {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := secretkeys.InitKeyfile(path); err != nil {
		t.Fatalf("init keyfile: %v", err)
	}
	p, err := secretkeys.NewKeyfile(path, false)
	if err != nil {
		t.Fatalf("load keyfile: %v", err)
	}
	return p
}

func projectContext() Context {
	return Context{
		InstallationID: "inst-1",
		TenantID:       "t_acme",
		ProjectID:      "p_inventory",
		SecretID:       "sec_1",
		Name:           "erp-token",
		Version:        1,
	}
}

func tenantContext() Context {
	c := projectContext()
	c.ProjectID = ""
	c.SecretID = "sec_tenant"
	c.Name = "smtp-password"
	return c
}

func seal(t *testing.T, p secretkeys.Provider, c Context, plaintext []byte) ([]byte, []byte, string) {
	t.Helper()
	ct, env, keyID, err := Seal(context.Background(), p, c, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return ct, env, keyID
}

func open(t *testing.T, p secretkeys.Provider, c Context, ct, env []byte) ([]byte, error) {
	t.Helper()
	return Open(context.Background(), p, c, ct, env)
}

func decodeMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return m
}

func encodeMap(t *testing.T, m map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return data
}

func nestedMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	n, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("envelope %q is not an object", key)
	}
	return n
}

func Test_roundtrip(t *testing.T) {
	p := newProvider(t)
	cases := map[string]Context{
		"project": projectContext(),
		"tenant":  tenantContext(),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, pt := range [][]byte{nil, []byte("s3cr3t-value"), bytes.Repeat([]byte{0xa5}, 4096)} {
				ct, env, keyID := seal(t, p, c, pt)
				if keyID == "" {
					t.Fatal("seal returned an empty key reference")
				}
				doc := decodeMap(t, env)
				wrapped := nestedMap(t, doc, "wrapped")
				if got := wrapped["keyRef"]; got != keyID {
					t.Fatalf("keyRef = %v, returned keyID = %q", got, keyID)
				}
				sealed := nestedMap(t, doc, "sealed")
				if _, dup := sealed["ciphertext"]; dup {
					t.Fatal("envelope duplicates the value ciphertext")
				}
				got, err := open(t, p, c, ct, env)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				if !bytes.Equal(got, pt) {
					t.Fatalf("plaintext mismatch: got %d bytes, want %d", len(got), len(pt))
				}
			}
		})
	}
}

func Test_two_seals_differ(t *testing.T) {
	p := newProvider(t)
	c := projectContext()
	ct1, env1, _ := seal(t, p, c, []byte("same-plaintext"))
	ct2, env2, _ := seal(t, p, c, []byte("same-plaintext"))
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two seals produced equal ciphertexts")
	}
	if bytes.Equal(env1, env2) {
		t.Fatal("two seals produced equal envelopes")
	}
}

func Test_context_is_authenticated(t *testing.T) {
	p := newProvider(t)
	c := projectContext()
	const sentinel = "sentinel-secret-value"
	ct, env, _ := seal(t, p, c, []byte(sentinel))

	mutations := map[string]func(*Context){
		"installation": func(c *Context) { c.InstallationID = "inst-2" },
		"tenant":       func(c *Context) { c.TenantID = "t_globex" },
		"project":      func(c *Context) { c.ProjectID = "p_billing" },
		"tenant scope": func(c *Context) { c.ProjectID = "" },
		"secret id":    func(c *Context) { c.SecretID = "sec_2" },
		"name":         func(c *Context) { c.Name = "erp-token-2" },
		"version":      func(c *Context) { c.Version = 2 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			got, err := open(t, p, bad, ct, env)
			if err == nil {
				t.Fatalf("open with changed %s succeeded", name)
			}
			if !errors.Is(err, envelope.ErrAuthentication) && !errors.Is(err, envelope.ErrMalformed) {
				t.Fatalf("changed %s: unexpected error %v", name, err)
			}
			if got != nil {
				t.Fatalf("changed %s returned plaintext", name)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error leaks plaintext: %v", err)
			}
		})
	}
}

func Test_tampered_ciphertext(t *testing.T) {
	p := newProvider(t)
	c := projectContext()
	ct, env, _ := seal(t, p, c, []byte("tamper-me"))
	for i := range ct {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x01
		got, err := open(t, p, c, bad, env)
		if !errors.Is(err, envelope.ErrAuthentication) {
			t.Fatalf("flip at %d: %v", i, err)
		}
		if got != nil {
			t.Fatalf("flip at %d returned plaintext", i)
		}
	}
}

func Test_tampered_envelope(t *testing.T) {
	p := newProvider(t)
	c := projectContext()
	ct, env, _ := seal(t, p, c, []byte("envelope-secret"))

	t.Run("nonce invalid base64", func(t *testing.T) {
		m := decodeMap(t, env)
		nestedMap(t, m, "sealed")["nonce"] = "not base64!!"
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("invalid base64: %v", err)
		}
	})

	t.Run("nonce altered", func(t *testing.T) {
		m := decodeMap(t, env)
		sealed := nestedMap(t, m, "sealed")
		nonce, err := base64.StdEncoding.DecodeString(sealed["nonce"].(string))
		if err != nil {
			t.Fatal(err)
		}
		nonce[0] ^= 0x80
		sealed["nonce"] = base64.StdEncoding.EncodeToString(nonce)
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrAuthentication) {
			t.Fatalf("altered nonce: %v", err)
		}
	})

	t.Run("wrapped key altered", func(t *testing.T) {
		m := decodeMap(t, env)
		nestedMap(t, m, "wrapped")["ciphertext"] = base64.StdEncoding.EncodeToString(make([]byte, 60))
		_, err := open(t, p, c, ct, encodeMap(t, m))
		if !errors.Is(err, envelope.ErrAuthentication) && !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("altered wrapped key: %v", err)
		}
	})
}

func Test_envelope_is_strict(t *testing.T) {
	p := newProvider(t)
	c := projectContext()
	ct, env, _ := seal(t, p, c, []byte("strict"))

	t.Run("unknown top-level field", func(t *testing.T) {
		m := decodeMap(t, env)
		m["extra"] = true
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("unknown field: %v", err)
		}
	})

	t.Run("unknown nested field", func(t *testing.T) {
		m := decodeMap(t, env)
		nestedMap(t, m, "sealed")["ciphertext"] = "AAAA"
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("nested unknown field: %v", err)
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		bad := append(append([]byte(nil), env...), []byte("{}")...)
		if _, err := open(t, p, c, ct, bad); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("trailing data: %v", err)
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		m := decodeMap(t, env)
		m["v"] = 2
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrUnsupported) {
			t.Fatalf("unknown version: %v", err)
		}
	})

	t.Run("missing version", func(t *testing.T) {
		m := decodeMap(t, env)
		delete(m, "v")
		if _, err := open(t, p, c, ct, encodeMap(t, m)); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("missing version: %v", err)
		}
	})

	t.Run("oversized envelope", func(t *testing.T) {
		bad := bytes.Repeat([]byte(" "), maxEnvelopeBytes+1)
		if _, err := open(t, p, c, ct, bad); !errors.Is(err, envelope.ErrMalformed) {
			t.Fatalf("oversized envelope: %v", err)
		}
	})
}

func Test_size_limits(t *testing.T) {
	p := newProvider(t)
	c := projectContext()

	ct, env, _ := seal(t, p, c, bytes.Repeat([]byte{0x5a}, envelope.MaxPlaintextBytes))
	got, err := open(t, p, c, ct, env)
	if err != nil || len(got) != envelope.MaxPlaintextBytes {
		t.Fatalf("limit roundtrip: %v", err)
	}

	if _, _, _, err := Seal(context.Background(), p, c, make([]byte, envelope.MaxPlaintextBytes+1)); !errors.Is(err, envelope.ErrMalformed) {
		t.Fatalf("over limit: %v", err)
	}
}

func Test_invalid_context(t *testing.T) {
	p := newProvider(t)
	ct, env, _ := seal(t, p, projectContext(), []byte("v"))

	for name, mutate := range map[string]func(*Context){
		"installation": func(c *Context) { c.InstallationID = "" },
		"tenant":       func(c *Context) { c.TenantID = "" },
		"secret id":    func(c *Context) { c.SecretID = "" },
		"name":         func(c *Context) { c.Name = "" },
		"version":      func(c *Context) { c.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := projectContext()
			mutate(&bad)
			if _, _, _, err := Seal(context.Background(), p, bad, []byte("v")); !errors.Is(err, envelope.ErrMalformed) {
				t.Fatalf("seal with invalid %s: %v", name, err)
			}
			if _, err := open(t, p, bad, ct, env); !errors.Is(err, envelope.ErrMalformed) {
				t.Fatalf("open with invalid %s: %v", name, err)
			}
		})
	}
}

type stubProvider struct{ err error }

func (s stubProvider) Wrap(context.Context, []byte, []byte) (secretkeys.Wrapped, error) {
	return secretkeys.Wrapped{}, s.err
}

func (s stubProvider) Unwrap(context.Context, secretkeys.Wrapped, []byte) ([]byte, error) {
	return nil, s.err
}

func Test_provider_outage(t *testing.T) {
	stub := stubProvider{err: errors.New("provider is down")}

	ct, env, keyID, err := Seal(context.Background(), stub, projectContext(), []byte("v"))
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("seal provider error: %v", err)
	}
	if ct != nil || env != nil || keyID != "" {
		t.Fatal("failed seal returned partial output")
	}

	p := newProvider(t)
	ct, env, _ = seal(t, p, projectContext(), []byte("v"))
	if _, err := open(t, stub, projectContext(), ct, env); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("open provider error: %v", err)
	}
}

func Test_context_mismatch_keeps_wire_shape(t *testing.T) {
	// A tenant-scoped secret and a project-scoped secret with the same
	// identifiers must not share a binding.
	p := newProvider(t)
	ct, env, _ := seal(t, p, tenantContext(), []byte("x"))
	project := tenantContext()
	project.ProjectID = "p_inventory"
	if _, err := open(t, p, project, ct, env); !errors.Is(err, envelope.ErrAuthentication) {
		t.Fatalf("tenant secret opened as project secret: %v", err)
	}
}
