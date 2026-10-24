package secretkeys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLocalKeyringRestartRotationAndContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := InitKeyfile(path); err != nil {
		t.Fatal(err)
	}
	if err := InitKeyfile(path); !errors.Is(err, ErrConflict) {
		t.Fatal("overwrote existing key", err)
	}
	p, err := NewKeyfile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	binding := []byte("tenant-a")
	w, err := p.Wrap(context.Background(), key, binding)
	if err != nil {
		t.Fatal(err)
	}
	w2, _ := p.Wrap(context.Background(), key, binding)
	if bytes.Equal(w.Ciphertext, w2.Ciphertext) {
		t.Fatal("nonce reuse")
	}
	restarted, err := NewKeyfile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := restarted.Unwrap(context.Background(), w, binding)
	if err != nil || !bytes.Equal(raw, key) {
		t.Fatal("restart decrypt failed", err)
	}
	if _, err := p.Unwrap(context.Background(), w, []byte("tenant-b")); !errors.Is(err, ErrCiphertext) {
		t.Fatal("wrong binding accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewKeyfile(path, true); !errors.Is(err, ErrInvalid) {
		t.Fatal("world-readable production key accepted")
	}
	one, _ := NewEphemeral()
	two, _ := NewEphemeral()
	w, _ = one.Wrap(context.Background(), key, binding)
	if !one.Ephemeral() {
		t.Fatal("random key not marked ephemeral")
	}
	if _, err := two.Unwrap(context.Background(), w, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatal("random keys shared across instances")
	}
}

func TestScalewayWireContractAndFailures(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	const credential = "test-credential-must-not-leak"
	key := bytes.Repeat([]byte{5}, 32)
	binding := []byte("scope-binding")
	var status atomic.Int64
	status.Store(200)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Auth-Token") != credential || r.Method != "POST" {
			t.Error("invalid auth/method")
		}
		if status.Load() != 200 {
			http.Error(w, credential, int(status.Load()))
			return
		}
		var input map[string]string
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("invalid wire JSON")
		}
		if input["associated_data"] != base64.StdEncoding.EncodeToString(binding) {
			t.Error("AAD not base64 bytes")
		}
		result := kmsResponse{KeyID: id}
		switch r.URL.Path {
		case "/key-manager/v1alpha1/regions/fr-par/keys/" + id + "/encrypt":
			result.Ciphertext = []byte("opaque-kms-ciphertext")
		case "/key-manager/v1alpha1/regions/fr-par/keys/" + id + "/decrypt":
			result.Plaintext = key
		default:
			t.Error("unexpected path")
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	p, err := NewScaleway(credential, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.endpoint = server.URL
	w, err := p.Wrap(context.Background(), key, binding)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Unwrap(context.Background(), w, binding)
	if err != nil || !bytes.Equal(raw, key) {
		t.Fatal(err)
	}
	before := calls.Load()
	bad := w
	bad.KeyRef = "fr-par/00000000-0000-4000-8000-000000000002"
	if _, err := p.Unwrap(context.Background(), bad, binding); !errors.Is(err, ErrCiphertext) || calls.Load() != before {
		t.Fatal("unconfigured key contacted KMS")
	}
	for _, tc := range []struct {
		code int64
		want error
	}{{403, ErrDenied}, {400, ErrCiphertext}, {429, ErrUnavailable}, {500, ErrUnavailable}} {
		status.Store(tc.code)
		_, err := p.Unwrap(context.Background(), w, binding)
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), credential) {
			t.Fatalf("status %d: %v", tc.code, err)
		}
	}
}

type fakeKMS struct {
	fail error
}

func (f fakeKMS) Encrypt(_ context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return append(append([]byte(keyID+":"), aad...), plaintext...), nil
}

func (f fakeKMS) Decrypt(_ context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	prefix := keyID + ":" + string(aad)
	rest, ok := strings.CutPrefix(string(ciphertext), prefix)
	if !ok || len(rest) != 32 {
		return nil, ErrCiphertext
	}
	return []byte(rest), nil
}

func TestCloudRejectsOtherKeyAndBinding(t *testing.T) {
	p, err := newCloud("aws", awsFormat, fakeKMS{}, "key-1", []string{"key-old"})
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{3}, 32)
	binding := []byte("install")
	w, err := p.Wrap(context.Background(), key, binding)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Unwrap(context.Background(), w, binding)
	if err != nil || !bytes.Equal(raw, key) {
		t.Fatal(err)
	}
	if _, err := p.Unwrap(context.Background(), w, []byte("other")); !errors.Is(err, ErrCiphertext) {
		t.Fatal("wrong binding accepted", err)
	}
	w.KeyRef = "key-other"
	if _, err := p.Unwrap(context.Background(), w, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatal("unconfigured key accepted")
	}
}

func TestScalewayClassificationSlotsAndConcurrency(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	key := bytes.Repeat([]byte{5}, 32)
	s := &sim{}
	server := httptest.NewServer(s.scalewayHandler(t, id))
	defer server.Close()
	p, err := NewScaleway(simToken, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.endpoint = server.URL
	w, err := p.Wrap(context.Background(), key, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	// A key that the cloud no longer has is classified, with no body leaked.
	gone, _ := NewScaleway(simToken, "00000000-0000-4000-8000-000000000009", nil)
	gone.endpoint = server.URL
	if _, err := gone.Wrap(context.Background(), key, []byte("b")); !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), leak) {
		t.Fatalf("missing key: %v", err)
	}
	// Failures and cancellation release the concurrency slots.
	for _, kind := range []string{"throttle", "outage", "denied", "drop"} {
		s.setFail(kind)
		for i := 0; i < 20; i++ {
			_, _ = p.Unwrap(context.Background(), w, []byte("b"))
		}
	}
	s.setFail("")
	if len(p.slots) != 0 {
		t.Fatalf("%d slots leaked", len(p.slots))
	}
	// Independent bindings run in parallel and never mix.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			binding := []byte(fmt.Sprintf("tenant-%d", i))
			w, err := p.Wrap(context.Background(), key, binding)
			if err != nil {
				t.Error(err)
				return
			}
			if got, err := p.Unwrap(context.Background(), w, binding); err != nil || !bytes.Equal(got, key) {
				t.Error("round trip", err)
			}
			if _, err := p.Unwrap(context.Background(), w, []byte("other")); !errors.Is(err, ErrCiphertext) {
				t.Error("binding mixed", err)
			}
		}()
	}
	wg.Wait()
}
