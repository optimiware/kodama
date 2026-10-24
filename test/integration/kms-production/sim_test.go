package kmsproduction

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gcpkms "cloud.google.com/go/kms/apiv1"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"google.golang.org/api/option"

	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

// A cloud under test: SDK-compatible local endpoint plus the configuration
// that selects it. These exercise the real SDK serialization and the real
// adapters; they are offline evidence, not a live-provider qualification.
type cloud struct {
	name    string
	keys    [3]string // three immutable key resources
	clients func(t *testing.T, s *sim) production.Clients
}

// sim is a deterministic KMS: AES-256-GCM per key resource, authenticating the
// associated data. down makes every request fail with a provider outage.
type sim struct {
	down  atomic.Bool
	calls atomic.Int64
	known map[string]bool
}

func aead(resource string) cipher.AEAD {
	k := sha256.Sum256([]byte("kms-sim|" + resource))
	b, _ := aes.NewCipher(k[:])
	a, _ := cipher.NewGCMWithRandomNonce(b)
	return a
}

func (s *sim) awsHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if s.down.Load() {
			w.Header().Set("X-Amzn-Errortype", "KMSInternalException")
			w.WriteHeader(500)
			return
		}
		var in struct {
			KeyID             string            `json:"KeyId"`
			Plaintext         []byte            `json:"Plaintext"`
			CiphertextBlob    []byte            `json:"CiphertextBlob"`
			EncryptionContext map[string]string `json:"EncryptionContext"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("bad AWS request")
		}
		aad := []byte(in.EncryptionContext["binding"])
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		if !s.known[in.KeyID] {
			w.Header().Set("X-Amzn-Errortype", "NotFoundException")
			w.WriteHeader(400)
			return
		}
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.Encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": aead(in.KeyID).Seal(nil, nil, in.Plaintext, aad)})
		case "TrentService.Decrypt":
			pt, err := aead(in.KeyID).Open(nil, nil, in.CiphertextBlob, aad)
			if err != nil {
				w.Header().Set("X-Amzn-Errortype", "InvalidCiphertextException")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": pt})
		}
	})
}

func (s *sim) gcpHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, status string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "status": status, "message": "diagnostic"}})
		}
		if s.down.Load() {
			fail(503, "UNAVAILABLE")
			return
		}
		name, op, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/"), ":")
		var in struct {
			Plaintext  []byte `json:"plaintext"`
			Ciphertext []byte `json:"ciphertext"`
			AAD        []byte `json:"additionalAuthenticatedData"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("bad GCP request")
		}
		if !s.known[name] {
			fail(404, "NOT_FOUND")
			return
		}
		switch op {
		case "encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "ciphertext": aead(name).Seal(nil, nil, in.Plaintext, in.AAD)})
		case "decrypt":
			pt, err := aead(name).Open(nil, nil, in.Ciphertext, in.AAD)
			if err != nil {
				fail(400, "INVALID_ARGUMENT")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": pt})
		}
	})
}

func clouds() []cloud {
	awsKeys := [3]string{
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}
	gcpKeys := [3]string{
		"projects/p/locations/l/keyRings/r/cryptoKeys/k1", "projects/p/locations/l/keyRings/r/cryptoKeys/k2", "projects/p/locations/l/keyRings/r/cryptoKeys/k3"}
	return []cloud{
		{name: production.AWS, keys: awsKeys, clients: func(t *testing.T, s *sim) production.Clients {
			s.known = map[string]bool{awsKeys[0]: true, awsKeys[1]: true, awsKeys[2]: true}
			srv := httptest.NewServer(s.awsHandler(t))
			t.Cleanup(srv.Close)
			return production.Clients{AWS: awskms.New(awskms.Options{
				Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1,
				Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", ""), HTTPClient: srv.Client()})}
		}},
		{name: production.GCP, keys: gcpKeys, clients: func(t *testing.T, s *sim) production.Clients {
			s.known = map[string]bool{gcpKeys[0]: true, gcpKeys[1]: true, gcpKeys[2]: true}
			srv := httptest.NewServer(s.gcpHandler(t))
			t.Cleanup(srv.Close)
			c, err := gcpkms.NewKeyManagementRESTClient(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			c.CallOptions.Encrypt, c.CallOptions.Decrypt = nil, nil // no retry delays in tests
			t.Cleanup(func() { _ = c.Close() })
			return production.Clients{GCP: c}
		}},
	}
}

// config selects current among the cloud's three keys; retained are the other labels.
func (c cloud) config(current string, labels ...string) production.Config {
	res := map[string]string{}
	for i, l := range []string{"v1", "v2", "v3"} {
		for _, want := range labels {
			if want == l {
				res[l] = c.keys[i]
			}
		}
	}
	return production.Config{Provider: c.name, KEKID: "kodama-prod", Current: current, Resources: res}
}
