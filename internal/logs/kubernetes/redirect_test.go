package kubernetes

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestReaderDoesNotFollowRedirects: a redirect from the API server is never
// followed, so the client's credential cannot be forwarded elsewhere, and the
// caller's own client keeps its redirect policy.
func TestReaderDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer sink.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	callers := &http.Client{}
	r, err := NewReader(Config{Endpoint: api.URL, Client: callers, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.client.Get(api.URL + "/apis/batch/v1/namespaces/n/jobs/j")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || hits.Load() != 0 {
		t.Fatalf("status %d, %d requests reached the redirect target", resp.StatusCode, hits.Load())
	}
	if callers.CheckRedirect != nil {
		t.Fatal("the reader changed the caller's client")
	}
}
