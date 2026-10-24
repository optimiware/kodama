package secretkeys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gcpkms "cloud.google.com/go/kms/apiv1"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"google.golang.org/api/option"
)

// leak is placed in every simulated provider diagnostic. No error returned by
// the package may contain it.
const leak = "PROVIDER-DIAGNOSTIC-LEAK"

const (
	simAWSKeyA = "11111111-1111-4111-8111-111111111111"
	simAWSKeyB = "22222222-2222-4222-8222-222222222222"
	simAWSKeyC = "33333333-3333-4333-8333-333333333333"
	simSCWKeyA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	simSCWKeyB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	simSCWKeyC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	simGCPBase = "projects/p/locations/l/keyRings/r/cryptoKeys/"
	simToken   = "sim-credential-must-not-leak"
)

type simCall struct {
	Op, Key string
	AAD     string // normalized authenticated data as received on the wire
	PTLen   int
}

// sim is a deterministic KMS that speaks the wire protocols of AWS KMS, Cloud
// KMS (REST) and Scaleway Key Manager. Its encryption authenticates the key
// identity and associated data, like a real KMS.
type sim struct {
	mu    sync.Mutex
	calls []simCall
	fail  string // "", throttle, outage, denied, missing, drop, hang
}

func (s *sim) setFail(kind string) { s.mu.Lock(); s.fail = kind; s.mu.Unlock() }
func (s *sim) failure() string     { s.mu.Lock(); defer s.mu.Unlock(); return s.fail }
func (s *sim) count() int          { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }
func (s *sim) record(c simCall)    { s.mu.Lock(); s.calls = append(s.calls, c); s.mu.Unlock() }
func (s *sim) log() []simCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]simCall(nil), s.calls...)
}

func simAEAD(key string) cipher.AEAD {
	k := sha256.Sum256([]byte("sim-kek|" + key))
	block, _ := aes.NewCipher(k[:])
	aead, _ := cipher.NewGCMWithRandomNonce(block)
	return aead
}

func simSeal(key, aad string, pt []byte) []byte {
	return simAEAD(key).Seal(nil, nil, pt, []byte(aad))
}

// simOpen returns false when the key, the associated data or any byte differs.
func simOpen(key, aad string, ct []byte) ([]byte, bool) {
	pt, err := simAEAD(key).Open(nil, nil, ct, []byte(aad))
	return pt, err == nil
}

// intercept applies the injected failure. It reports whether the request was
// consumed.
func (s *sim) intercept(w http.ResponseWriter, r *http.Request, awsType, gcpStatus string, code int) bool {
	switch s.failure() {
	case "":
		return false
	case "drop":
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case "hang":
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client leaving
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	default:
		if awsType != "" {
			w.Header().Set("X-Amzn-Errortype", awsType)
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"__type": awsType, "message": leak + " " + simToken})
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if gcpStatus != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "status": gcpStatus, "message": leak + " " + simToken}})
		} else {
			_, _ = w.Write([]byte(leak + " " + simToken))
		}
	}
	return true
}

type failShape struct {
	awsType, gcpStatus string
	code               int
}

func (s *sim) shape(aws, gcp bool) failShape {
	switch s.failure() {
	case "throttle":
		if aws {
			return failShape{"ThrottlingException", "", 400}
		}
		return failShape{"", "RESOURCE_EXHAUSTED", 429}
	case "outage":
		if aws {
			return failShape{"KMSInternalException", "", 500}
		}
		return failShape{"", "UNAVAILABLE", 503}
	case "denied":
		if aws {
			return failShape{"AccessDeniedException", "", 400}
		}
		return failShape{"", "PERMISSION_DENIED", 403}
	case "missing":
		if aws {
			return failShape{"NotFoundException", "", 400}
		}
		return failShape{"", "NOT_FOUND", 404}
	}
	return failShape{"InternalFailure", "INTERNAL", 500}
}

func (s *sim) awsHandler(t *testing.T, existing ...string) http.Handler {
	known := set(existing)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sh := s.shape(true, false)
		if s.intercept(w, r, sh.awsType, "", sh.code) {
			return
		}
		var in struct {
			KeyID             string            `json:"KeyId"`
			Plaintext         []byte            `json:"Plaintext"`
			CiphertextBlob    []byte            `json:"CiphertextBlob"`
			EncryptionContext map[string]string `json:"EncryptionContext"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("AWS wire request is not JSON")
		}
		op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.")
		aad := in.EncryptionContext["binding"]
		s.record(simCall{Op: op, Key: in.KeyID, AAD: aad, PTLen: len(in.Plaintext)})
		fail := func(typ string) {
			w.Header().Set("X-Amzn-Errortype", typ)
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": leak})
		}
		if !known[in.KeyID] {
			fail("NotFoundException")
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch op {
		case "Encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": simSeal(in.KeyID, aad, in.Plaintext), "KeyId": in.KeyID})
		case "Decrypt":
			pt, ok := simOpen(in.KeyID, aad, in.CiphertextBlob)
			if !ok {
				fail("InvalidCiphertextException")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": pt, "KeyId": in.KeyID})
		default:
			t.Errorf("unexpected AWS operation %q", op)
		}
	})
}

func (s *sim) gcpHandler(t *testing.T, existing ...string) http.Handler {
	known := set(existing)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sh := s.shape(false, true)
		if s.intercept(w, r, "", sh.gcpStatus, sh.code) {
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		name, op, _ := strings.Cut(path, ":")
		var in struct {
			Plaintext  []byte `json:"plaintext"`
			Ciphertext []byte `json:"ciphertext"`
			AAD        []byte `json:"additionalAuthenticatedData"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("GCP wire request is not JSON")
		}
		aad := base64.StdEncoding.EncodeToString(in.AAD)
		s.record(simCall{Op: op, Key: name, AAD: aad, PTLen: len(in.Plaintext)})
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, status string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "status": status, "message": leak}})
		}
		if !known[name] {
			fail(404, "NOT_FOUND")
			return
		}
		switch op {
		case "encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "ciphertext": simSeal(name, aad, in.Plaintext)})
		case "decrypt":
			pt, ok := simOpen(name, aad, in.Ciphertext)
			if !ok {
				fail(400, "INVALID_ARGUMENT")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": pt})
		default:
			t.Errorf("unexpected GCP operation %q", op)
		}
	})
}

func (s *sim) scalewayHandler(t *testing.T, existing ...string) http.Handler {
	known := set(existing)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sh := s.shape(false, false)
		if s.intercept(w, r, "", "", sh.code) {
			return
		}
		if r.Header.Get("X-Auth-Token") != simToken {
			t.Error("Scaleway credential header missing")
		}
		const prefix = "/key-manager/v1alpha1/regions/fr-par/keys/"
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		id, op, _ := strings.Cut(rest, "/")
		var in struct {
			Plaintext  []byte `json:"plaintext"`
			Ciphertext []byte `json:"ciphertext"`
			AAD        []byte `json:"associated_data"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("Scaleway wire request is not JSON")
		}
		aad := base64.StdEncoding.EncodeToString(in.AAD)
		s.record(simCall{Op: op, Key: id, AAD: aad, PTLen: len(in.Plaintext)})
		if !known[id] {
			http.Error(w, leak, 404)
			return
		}
		switch op {
		case "encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"key_id": id, "ciphertext": simSeal(id, aad, in.Plaintext)})
		case "decrypt":
			pt, ok := simOpen(id, aad, in.Ciphertext)
			if !ok {
				http.Error(w, leak, 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"key_id": id, "plaintext": pt})
		default:
			t.Errorf("unexpected Scaleway operation %q", op)
		}
	})
}

func set(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// rig builds providers of one cloud against a simulator.
type rig struct {
	name       string
	keyA, keyB string
	keyC       string
	sim        *sim
	build      func(active string, retained ...string) *Cloud
	scw        func(active string, retained ...string) *Scaleway
}

func (r rig) provider(active string, retained ...string) Provider {
	if r.scw != nil {
		return r.scw(active, retained...)
	}
	return r.build(active, retained...)
}

func newRigs(t *testing.T) []rig {
	t.Helper()
	var rigs []rig

	a := &sim{}
	awsKeys := []string{simAWSKeyA, simAWSKeyB, simAWSKeyC}
	as := httptest.NewServer(a.awsHandler(t, awsKeys...))
	t.Cleanup(as.Close)
	rigs = append(rigs, rig{name: "aws", keyA: simAWSKeyA, keyB: simAWSKeyB, keyC: simAWSKeyC, sim: a,
		build: func(active string, retained ...string) *Cloud {
			client := awskms.New(awskms.Options{
				Region: "eu-west-1", BaseEndpoint: aws.String(as.URL), RetryMaxAttempts: 1,
				Credentials: credentials.NewStaticCredentialsProvider("AKIDSIM", "secret-sim", ""),
				HTTPClient:  as.Client(),
			})
			p, err := NewAWS(client, active, retained)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}})

	g := &sim{}
	gcpKeys := []string{simGCPBase + "k1", simGCPBase + "k2", simGCPBase + "k3"}
	gs := httptest.NewServer(g.gcpHandler(t, gcpKeys...))
	t.Cleanup(gs.Close)
	rigs = append(rigs, rig{name: "gcp", keyA: gcpKeys[0], keyB: gcpKeys[1], keyC: gcpKeys[2], sim: g,
		build: func(active string, retained ...string) *Cloud {
			client, err := gcpkms.NewKeyManagementRESTClient(context.Background(), option.WithEndpoint(gs.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			client.CallOptions.Encrypt, client.CallOptions.Decrypt = nil, nil // no retry delays in tests
			p, err := NewGCP(client, active, retained)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}})

	s := &sim{}
	scwKeys := []string{simSCWKeyA, simSCWKeyB, simSCWKeyC}
	ss := httptest.NewServer(s.scalewayHandler(t, scwKeys...))
	t.Cleanup(ss.Close)
	rigs = append(rigs, rig{name: "scaleway", keyA: simSCWKeyA, keyB: simSCWKeyB, keyC: simSCWKeyC, sim: s,
		scw: func(active string, retained ...string) *Scaleway {
			p, err := NewScaleway(simToken, active, retained)
			if err != nil {
				t.Fatal(err)
			}
			p.endpoint = ss.URL
			return p
		}})
	return rigs
}

// refOf is the persisted key reference of a resource for this cloud.
func (r rig) refOf(resource string) string {
	if r.name == "scaleway" {
		return "fr-par/" + resource
	}
	return resource
}
