// Package kubefake is a minimal Kubernetes API server for tests: the Job, the
// Pod list and the Pod log subresource, recording every request so a test can
// prove what the log reader asked for. It stands in for a cluster only where a
// test needs controlled faults (a slow, endless, failing or oversized log);
// ownership and wire semantics of a real API server are checked separately by
// the real-cluster test.
package kubefake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// API is the fake's state. Set fields before issuing requests, or change them
// under Set while requests are in flight.
type API struct {
	mu       sync.Mutex
	requests []string

	Job      map[string]any // nil: the Job is absent (404)
	Pods     []map[string]any
	LogCode  int    // 0: 200
	LogBody  string // written verbatim
	JobBytes int    // when > 0 the Job response is this large (oversized object)
	// LogHandler replaces the log handler entirely (endless, slow or failing
	// streams).
	LogHandler http.HandlerFunc
	// Hold, when set, keeps the log stream open after LogBody until the client
	// leaves; Held is closed when streaming has started and Gone when the client
	// went away.
	Hold chan struct{}
	Held chan struct{}
	Gone chan struct{}

	srv *httptest.Server
}

// New starts a fake serving the standard Job and one Pod.
func New(t testing.TB, namespace, job, uid string, containers ...string) *API {
	t.Helper()
	a := &API{Job: JobObject(namespace, job, uid), LogBody: "one\ntwo\n"}
	a.Pods = []map[string]any{PodObject(namespace, job, "pod-a", "2026-01-01T00:00:00Z", uid, true, containers...)}
	a.srv = httptest.NewServer(a.handler())
	t.Cleanup(a.srv.Close)
	return a
}

// URL is the API server base URL (http).
func (a *API) URL() string { return a.srv.URL }

// Client is an HTTP client for the fake.
func (a *API) Client() *http.Client { return a.srv.Client() }

// Set runs fn under the fake's lock.
func (a *API) Set(fn func(*API)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(a)
}

// Requests returns every request line seen so far.
func (a *API) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

// LogRequests returns only the log subresource requests.
func (a *API) LogRequests() []string {
	var out []string
	for _, r := range a.Requests() {
		if strings.Contains(r, "/log") {
			out = append(out, r)
		}
	}
	return out
}

func (a *API) record(r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI())
	a.mu.Unlock()
}

func (a *API) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		a.record(r)
		a.mu.Lock()
		job, size := a.Job, a.JobBytes
		a.mu.Unlock()
		if job == nil {
			http.Error(w, `{"message":"jobs not found, secret detail"}`, http.StatusNotFound)
			return
		}
		b, _ := json.Marshal(job)
		if size > 0 {
			b = []byte(`{"pad":"` + strings.Repeat("x", size) + `"}`)
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		a.record(r)
		a.mu.Lock()
		pods := a.Pods
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"items": pods})
	})
	mux.HandleFunc("/api/v1/namespaces/{ns}/pods/{pod}/log", func(w http.ResponseWriter, r *http.Request) {
		a.record(r)
		// A real API server rejects an Accept header that does not also admit a
		// wildcard for the log subresource.
		if !strings.Contains(r.Header.Get("Accept"), "*/*") {
			http.Error(w, "not acceptable", http.StatusNotAcceptable)
			return
		}
		a.mu.Lock()
		h, code, body, hold, held, gone := a.LogHandler, a.LogCode, a.LogBody, a.Hold, a.Held, a.Gone
		a.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		if code != 0 && code != http.StatusOK {
			http.Error(w, "kubelet said: internal credentials=abc", code)
			return
		}
		_, _ = io.WriteString(w, body)
		if hold != nil {
			w.(http.Flusher).Flush()
			if held != nil {
				close(held)
			}
			<-r.Context().Done()
			if gone != nil {
				close(gone)
			}
		}
	})
	return mux
}

// JobObject is a Job with its own selector labels, as the Job controller sets.
func JobObject(namespace, name, uid string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": uid},
		"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]string{"batch.kubernetes.io/controller-uid": uid}}},
	}
}

// PodObject is a Pod owned by the named Job.
func PodObject(namespace, job, name, created, ownerUID string, controller bool, containers ...string) map[string]any {
	cs := []map[string]string{}
	for _, c := range containers {
		cs = append(cs, map[string]string{"name": c})
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace, "creationTimestamp": created,
			"ownerReferences": []map[string]any{{"kind": "Job", "name": job, "uid": ownerUID, "controller": controller}}},
		"spec": map[string]any{"containers": cs},
	}
}
