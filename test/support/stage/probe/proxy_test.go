package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyLostCreateAndReplacement(t *testing.T) {
	var creates atomic.Int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("workload authorization not forwarded")
		}
		if r.Method == http.MethodPost {
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
		}
		fmt.Fprint(w, `{"metadata":{"name":"job-a","uid":"uid-original"}}`)
	}))
	defer up.Close()
	origin, _ := url.Parse(up.URL)
	client := up.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p := &Proxy{Origin: origin, Client: client}
	srv := httptest.NewTLSServer(p)
	defer srv.Close()
	call := func(method, path, body string) (*http.Response, error) {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-token")
		return srv.Client().Do(r)
	}
	r, err := call(http.MethodPost, "/__fault/arm", `{"namespace":"kodama-q-fixture-a","name":"job-a"}`)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("arm HTTP %d", r.StatusCode)
	}
	path := "/apis/batch/v1/namespaces/kodama-q-fixture-a/jobs"
	if r, err := call(http.MethodPost, path, `{"metadata":{"name":"job-a"}}`); err == nil {
		r.Body.Close()
		t.Fatal("create acknowledgment was not lost")
	}
	if creates.Load() != 1 {
		t.Fatalf("creates=%d", creates.Load())
	}
	for _, suffix := range []string{"", "/job-a"} {
		r, err := call(http.MethodGet, path+suffix, "")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 503 {
			t.Fatalf("read-back HTTP %d", r.StatusCode)
		}
	}
	r, err = call(http.MethodGet, "/__fault/status", "")
	if err != nil {
		t.Fatal(err)
	}
	var status FaultStatus
	if json.NewDecoder(r.Body).Decode(&status) != nil {
		t.Fatal("invalid fault status")
	}
	r.Body.Close()
	if !status.Lost || status.UID != "uid-original" || status.Creates != 1 {
		t.Fatalf("status=%+v", status)
	}
	// A new sidecar has no in-memory fault. It observes the existing object, never creates again.
	replacement := httptest.NewTLSServer(&Proxy{Origin: origin, Client: client})
	defer replacement.Close()
	req, _ := http.NewRequest(http.MethodGet, replacement.URL+path+"/job-a", nil)
	req.Header.Set("Authorization", "Bearer fixture-token")
	r, err = replacement.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "uid-original") || creates.Load() != 1 {
		t.Fatal("replacement did not observe the original Job")
	}
}

func TestProxyScopesFaultAndStreamsLogs(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "follow=true" {
			t.Error("query lost")
		}
		fmt.Fprint(w, "first\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "last\n")
	}))
	defer up.Close()
	origin, _ := url.Parse(up.URL)
	p := &Proxy{Origin: origin, Client: up.Client(), status: FaultStatus{Target: Target{Namespace: "kodama-q-a", Name: "job"}, Lost: true}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/v1/namespaces/kodama-q-b/pods/other/log?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if string(b) != "first\nlast\n" {
		t.Fatalf("log %q", b)
	}
	bad := httptest.NewRecorder()
	p.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/__fault/arm", strings.NewReader(`{"namespace":"production","name":"job"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatal("armed a non-qualification namespace")
	}
}

func TestProxyRejectsUntrustedUpstream(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted upstream was reached") }))
	defer up.Close()
	origin, _ := url.Parse(up.URL)
	p := &Proxy{Origin: origin, Client: &http.Client{}}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/version", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("untrusted upstream HTTP %d", w.Code)
	}
}

func TestProxyLosesHTTP2Acknowledgment(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"metadata":{"name":"job-a","uid":"uid-original"}}`)
	}))
	defer up.Close()
	origin, _ := url.Parse(up.URL)
	p := &Proxy{Origin: origin, Client: up.Client(), status: FaultStatus{Target: Target{Namespace: "kodama-q-fixture-a", Name: "job-a"}}}
	srv := httptest.NewUnstartedServer(p)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	r, err := srv.Client().Post(srv.URL+"/apis/batch/v1/namespaces/kodama-q-fixture-a/jobs", "application/json", strings.NewReader(`{}`))
	if err == nil {
		r.Body.Close()
		t.Fatal("HTTP/2 create response was not lost")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.status.Lost || p.status.UID != "uid-original" {
		t.Fatal("HTTP/2 create did not commit upstream")
	}
}

func TestProxyArmedWithoutNameWithholdsOnlyTheFirstJobCreate(t *testing.T) {
	var creates atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			n := creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"metadata":{"name":"job-%d","uid":"uid-%d"}}`, n, n)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer up.Close()
	origin, _ := url.Parse(up.URL)
	srv := httptest.NewTLSServer(&Proxy{Origin: origin, Client: up.Client()})
	defer srv.Close()
	call := func(method, path, body string) (*http.Response, error) {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		return srv.Client().Do(r)
	}
	r, err := call(http.MethodPost, "/__fault/arm", `{"namespace":"kodama-q-fixture-a"}`)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("arm without a name: %v", err)
	}
	r.Body.Close()
	if r, err := call(http.MethodPost, "/__fault/arm", `{"namespace":"kodama-q-fixture-a"}`); err != nil || r.StatusCode != http.StatusBadRequest {
		t.Fatal("an armed proxy accepted a second arming")
	}
	if r, err := call(http.MethodPost, "/apis/batch/v1/namespaces/kodama-q-other/jobs", `{}`); err != nil || r.StatusCode != http.StatusCreated {
		t.Fatal("another namespace was intercepted")
	}
	if r, err := call(http.MethodPost, "/apis/batch/v1/namespaces/kodama-q-fixture-a/jobs", `{}`); err == nil {
		r.Body.Close()
		t.Fatal("create acknowledgment was not lost")
	}
	r, err = call(http.MethodGet, "/__fault/status", "")
	if err != nil {
		t.Fatal(err)
	}
	var status FaultStatus
	if json.NewDecoder(r.Body).Decode(&status) != nil {
		t.Fatal("invalid fault status")
	}
	r.Body.Close()
	if !status.Lost || status.Target.Name != "job-2" || status.UID != "uid-2" || status.Creates != 1 {
		t.Fatalf("status=%+v", status)
	}
}
