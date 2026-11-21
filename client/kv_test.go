package client_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
)

func TestClientKVPutAndKVGet(t *testing.T) {
	stored := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/api/v1/projects/proj-1/kv" {
				t.Errorf("POST path = %q; want /api/v1/projects/proj-1/kv", r.URL.Path)
			}
			var env api.Request
			if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
				t.Fatalf("decode kv.put envelope: %v", err)
			}
			var req api.KVPutRequest
			if err := json.Unmarshal(env.Input, &req); err != nil {
				t.Fatalf("decode kv.put input: %v", err)
			}
			data, _, err := req.DecodeBytes()
			if err != nil {
				t.Fatalf("DecodeBytes: %v", err)
			}
			sum := sha256.Sum256(data)
			key := api.KVKeyPrefix + hex.EncodeToString(sum[:])
			_, existed := stored[key]
			stored[key] = data
			body, _ := json.Marshal(api.KVPutResult{
				Key:         key,
				ProjectID:   "proj-1",
				SizeBytes:   int64(len(data)),
				ContentType: "application/octet-stream",
				CreatedAt:   fixedNow(),
				Existed:     existed,
			})
			_ = json.NewEncoder(w).Encode(api.Result{Operation: "kv.put", Body: body})
		case http.MethodGet:
			prefix := "/api/v1/projects/proj-1/kv/"
			key := r.URL.Path[len(prefix):]
			data := stored[key]
			body, _ := json.Marshal(api.KVGetResult{
				Key:         key,
				ProjectID:   "proj-1",
				SizeBytes:   int64(len(data)),
				ContentType: "application/octet-stream",
				CreatedBy:   "usr-1",
				CreatedAt:   fixedNow(),
				DataBase64:  base64.StdEncoding.EncodeToString(data),
			})
			_ = json.NewEncoder(w).Encode(api.Result{Operation: "kv.get", Body: body})
		}
	}))
	defer server.Close()

	transport := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source: &fakeSource{credentials: []client.Credential{
			{Kind: client.KindExecution, Token: "kodex.exec-1.tok"},
		}},
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: 1, MaxDelay: 1},
		Now:   fixedNow,
	}

	payload := []byte{0x00, 0x01, 0xfe, 0xff, 'k', 'v'}
	putRes, err := transport.KVPut(context.Background(), "proj-1", payload, "application/octet-stream")
	if err != nil {
		t.Fatalf("KVPut: %v", err)
	}
	if putRes.Existed || putRes.SizeBytes != int64(len(payload)) {
		t.Fatalf("KVPut result = %+v", putRes)
	}

	got, err := transport.KVGet(context.Background(), "proj-1", putRes.Key)
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Fatalf("KVGet bytes = %x; want %x", got.Data, payload)
	}

	// Oversized payload is refused client-side with invalid_input before any HTTP call.
	if _, err := transport.KVPut(context.Background(), "proj-1", make([]byte, api.MaxKVValueBytes+1), ""); err == nil {
		t.Fatal("KVPut accepted >16 MiB payload")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized KVPut class = %v; want invalid_input", class)
	}

	// Malformed key is refused client-side with invalid_input.
	if _, err := transport.KVGet(context.Background(), "proj-1", "../bad"); err == nil {
		t.Fatal("KVGet accepted malformed key")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("malformed KVGet class = %v; want invalid_input", class)
	}
}
