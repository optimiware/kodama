package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchSendsCursorAndReturnsNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "c1" {
			t.Fatalf("cursor = %q", r.URL.Query().Get("cursor"))
		}
		w.Header().Set("X-Next-Cursor", "c2")
		_, _ = w.Write([]byte("page"))
	}))
	defer srv.Close()
	body, next, err := fetch(context.Background(), srv.URL, "c1", time.Second)
	if err != nil || string(body) != "page" || next != "c2" {
		t.Fatalf("fetch = %q/%q %v", body, next, err)
	}
}
