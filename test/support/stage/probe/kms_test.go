package probe

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// These wire tests are offline procedure regressions, not live-provider qualification.
func TestKMSProcedureRequiresDenialAndRecoversFromOutage(t *testing.T) {
	for _, wrongDenial := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrong_denial_%v", wrongDenial), func(t *testing.T) {
			old := http.DefaultTransport
			defer func() { http.DefaultTransport = old }()
			keys := map[string]cipher.AEAD{}
			const a = "11111111-1111-4111-8111-111111111111"
			const b = "22222222-2222-4222-8222-222222222222"
			for _, id := range []string{a, b} {
				key := make([]byte, 32)
				rand.Read(key)
				block, _ := aes.NewCipher(key)
				keys[id], _ = cipher.NewGCM(block)
			}
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				respond := func(code int, v any) (*http.Response, error) {
					raw, _ := json.Marshal(v)
					return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
				}
				if r.Header.Get("X-Auth-Token") == "denied-fixture" {
					if wrongDenial {
						return respond(503, map[string]string{"message": "provider-body-must-not-leak"})
					}
					return respond(403, nil)
				}
				parts := strings.Split(r.URL.Path, "/")
				id, op := parts[len(parts)-2], parts[len(parts)-1]
				if op == "unprotect" {
					return respond(403, nil)
				}
				key := keys[id]
				if key == nil {
					return respond(404, nil)
				}
				var req struct {
					Plaintext, Ciphertext []byte
					AssociatedData        []byte `json:"associated_data"`
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					return respond(400, nil)
				}
				if op == "encrypt" {
					nonce := make([]byte, key.NonceSize())
					rand.Read(nonce)
					data := key.Seal(nonce, nonce, req.Plaintext, req.AssociatedData)
					return respond(200, map[string]any{"key_id": id, "ciphertext": data})
				}
				if len(req.Ciphertext) < key.NonceSize() {
					return respond(400, nil)
				}
				data, err := key.Open(nil, req.Ciphertext[:key.NonceSize()], req.Ciphertext[key.NonceSize():], req.AssociatedData)
				if err != nil {
					return respond(400, nil)
				}
				return respond(200, map[string]any{"key_id": id, "plaintext": data})
			})
			clients := production.Clients{ScalewayToken: "workload-fixture"}
			freshCalled := false
			err := runKMS(context.Background(), KMSConfig{A: a, B: b, Installation: "installation", Tenant: "t_fixture"}, clients, "denied-fixture", func(in freshInput) error { freshCalled = true; return checkFresh(context.Background(), in, clients) })
			if (err != nil) != wrongDenial {
				t.Fatalf("err=%v", err)
			}
			if !freshCalled {
				t.Fatal("retained envelope check did not run")
			}
			if err != nil && strings.Contains(err.Error(), "provider-body") {
				t.Fatal("provider body leaked")
			}
		})
	}
}
