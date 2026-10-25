package kube_full_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyCleanupChecksStatuses(t *testing.T) {
	for _, tc := range []struct {
		name                string
		unprotect, deletion int
		wantError           bool
		calls               int
	}{
		{"deleted", 200, 204, false, 2}, {"already absent", 404, 0, false, 1}, {"unprotect denied", 403, 0, true, 1}, {"delete protected", 200, 412, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method == http.MethodPost {
					w.WriteHeader(tc.unprotect)
				} else {
					w.WriteHeader(tc.deletion)
				}
				io.WriteString(w, "provider-body-fixture-secret")
			}))
			defer s.Close()
			o := operator{base: s.URL, client: s.Client(), token: "fixture-token"}
			err := deleteKey(context.Background(), o, "key-id")
			if (err != nil) != tc.wantError || calls != tc.calls {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("provider body leaked")
			}
		})
	}
}

func TestCreateKeyRequestsUnprotectedAndRegistersCleanup(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	deleted := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/unprotect") {
			io.WriteString(w, `{}`)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["unprotected"] != true {
			t.Error("create did not request an unprotected key")
		}
		io.WriteString(w, `{"id":"`+id+`","protected":false}`)
	}))
	defer s.Close()
	t.Run("resources clean up even when next operation stops", func(t *testing.T) {
		o := operator{base: s.URL, client: s.Client(), token: "fixture-token"}
		if createKey(t, o, id, "t028-fixture") != id {
			t.Fatal("wrong created ID")
		}
		// End here, just as a failed second acquisition would: cleanup for A is already registered.
	})
	if !deleted {
		t.Fatal("first acquisition leaked")
	}
}
