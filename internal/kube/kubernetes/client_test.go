package kubernetes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

type rec struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (r *rec) add(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.body = append(r.body, b)
	r.mu.Unlock()
}

func (r *rec) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.reqs) }

func client(t *testing.T, h http.HandlerFunc, mod ...func(*kubernetes.Config)) (*kubernetes.Client, *rec) {
	t.Helper()
	r := &rec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.add(req)
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	cfg := kubernetes.Config{Origin: srv.URL, HTTP: srv.Client(), RequestTimeout: 2 * time.Second}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := kubernetes.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

func status(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Failure", "reason": reason, "message": message, "code": code})
}

const ns, name = "t021-ns", "job-1"

// Test_kubernetes_failure_semantics: absence, conflict, forbidden, timeout,
// throttling and unknown mutation outcomes are distinct classes; listings are
// bounded; deletes carry UID preconditions; errors never echo bodies.
func Test_kubernetes_failure_semantics(t *testing.T) {
	ctx := context.Background()
	t.Run("status mapping", func(t *testing.T) {
		cases := []struct {
			code   int
			reason string
			want   error
		}{
			{404, "NotFound", kubernetes.ErrNotFound}, {403, "Forbidden", kubernetes.ErrForbidden}, {401, "Unauthorized", kubernetes.ErrForbidden},
			{400, "BadRequest", kubernetes.ErrInvalid}, {422, "Invalid", kubernetes.ErrInvalid}, {410, "Expired", kubernetes.ErrExpired},
			{429, "TooManyRequests", kubernetes.ErrThrottled}, {500, "InternalError", kubernetes.ErrUnavailable}, {503, "ServiceUnavailable", kubernetes.ErrUnavailable},
		}
		for _, c := range cases {
			cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) { status(w, c.code, c.reason, "m") })
			_, err := cl.GetJob(ctx, ns, name)
			if !errors.Is(err, c.want) {
				t.Fatalf("GET %d: %v", c.code, err)
			}
		}
	})
	t.Run("create outcomes", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) { status(w, 409, "AlreadyExists", "m") })
		if _, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); !errors.Is(err, kubernetes.ErrAlreadyExists) {
			t.Fatalf("%v", err)
		}
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) { status(w, 409, "Conflict", "m") })
		if _, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); !errors.Is(err, kubernetes.ErrConflict) {
			t.Fatalf("%v", err)
		}
		// A 5xx after sending a mutation may have been applied.
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) { status(w, 500, "InternalError", "m") })
		_, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}})
		if !errors.Is(err, kubernetes.ErrUnknownOutcome) || kubernetes.Definite(err) {
			t.Fatalf("5xx on create: %v", err)
		}
		// 429 definitely was not applied.
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "3")
			status(w, 429, "TooManyRequests", "m")
		})
		_, err = cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}})
		var ke *kubernetes.Error
		if !errors.Is(err, kubernetes.ErrThrottled) || !kubernetes.Definite(err) || !errors.As(err, &ke) || ke.RetryAfter != 3*time.Second {
			t.Fatalf("429: %v", err)
		}
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "999999")
			status(w, 429, "TooManyRequests", "m")
		})
		_, err = cl.GetJob(ctx, ns, name)
		if errors.As(err, &ke); ke.RetryAfter != 0 {
			t.Fatal("an absurd Retry-After is honored")
		}
	})
	t.Run("lost responses", func(t *testing.T) {
		drop := func(w http.ResponseWriter, r *http.Request) {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
		cl, _ := client(t, drop)
		if _, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("create: %v", err)
		}
		if err := cl.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{UID: "u"}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("delete: %v", err)
		}
		if _, err := cl.UpdateSecret(ctx, ns, kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: name, ResourceVersion: "1"}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("update: %v", err)
		}
		// A lost read concludes nothing about the object.
		_, err := cl.GetJob(ctx, ns, name)
		if !errors.Is(err, kubernetes.ErrUnavailable) || errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("get: %v", err)
		}
		// A timeout is unavailable for a read and unknown for a mutation.
		slow, _ := client(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }, func(c *kubernetes.Config) { c.RequestTimeout = 50 * time.Millisecond })
		if _, err := slow.GetJob(ctx, ns, name); !errors.Is(err, kubernetes.ErrUnavailable) {
			t.Fatalf("slow get: %v", err)
		}
		if _, err := slow.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("slow create: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		release := make(chan struct{})
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) { <-release })
		defer close(release)
		cctx, cancel := context.WithCancel(ctx)
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		if _, err := cl.GetJob(cctx, ns, name); !errors.Is(err, kubernetes.ErrCanceled) {
			t.Fatalf("get: %v", err)
		}
		cctx, cancel = context.WithCancel(ctx)
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		if _, err := cl.CreateJob(cctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("a cancelled mutation may have been applied: %v", err)
		}
	})
	t.Run("responses are bounded and decoded strictly", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 4096))) }, func(c *kubernetes.Config) { c.MaxBody = 1024 })
		if _, err := cl.GetJob(ctx, ns, name); !errors.Is(err, kubernetes.ErrTooLarge) {
			t.Fatalf("large body: %v", err)
		}
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) })
		if _, err := cl.GetJob(ctx, ns, name); !errors.Is(err, kubernetes.ErrUnavailable) {
			t.Fatalf("garbage: %v", err)
		}
	})
	t.Run("deletes carry UID preconditions", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("{}")) })
		if err := cl.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{}); !errors.Is(err, kubernetes.ErrInvalid) || r.n() != 0 {
			t.Fatalf("a delete without UID was sent: %v (%d)", err, r.n())
		}
		if err := cl.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{UID: "uid-1", ResourceVersion: "7"}); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal(r.body[0], &body)
		pre := body["preconditions"].(map[string]any)
		if r.reqs[0].Method != http.MethodDelete || pre["uid"] != "uid-1" || pre["resourceVersion"] != "7" || body["propagationPolicy"] != "Foreground" {
			t.Fatalf("%s %v", r.reqs[0].Method, body)
		}
		if r.reqs[0].URL.Path != "/apis/batch/v1/namespaces/"+ns+"/jobs/"+name {
			t.Fatal(r.reqs[0].URL.Path)
		}
		if err := cl.DeletePod(ctx, ns, "p", kubernetes.DeleteOptions{UID: "u2", Propagation: "Background"}); err != nil {
			t.Fatal(err)
		}
		if err := cl.DeleteSecret(ctx, ns, "s", kubernetes.DeleteOptions{UID: "u3"}); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(r.body[2], &body)
		if body["propagationPolicy"] != "Background" {
			t.Fatalf("secrets are deleted in the background: %v", body)
		}
		// A precondition failure is a conflict, not absence.
		cl, _ = client(t, func(w http.ResponseWriter, req *http.Request) { status(w, 409, "Conflict", "uid mismatch") })
		if err := cl.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{UID: "x"}); !errors.Is(err, kubernetes.ErrConflict) || errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("updates require a resource version", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("{}")) })
		if _, err := cl.UpdateSecret(ctx, ns, kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: "s"}}); !errors.Is(err, kubernetes.ErrInvalid) || r.n() != 0 {
			t.Fatalf("an unconditional update was sent: %v", err)
		}
		if _, err := cl.UpdateSecret(ctx, ns, kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: "s", ResourceVersion: "5"}, Data: map[string][]byte{"k": []byte("v")}}); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		_ = json.Unmarshal(r.body[0], &doc)
		if doc["metadata"].(map[string]any)["resourceVersion"] != "5" || r.reqs[0].Method != http.MethodPut {
			t.Fatalf("%v", doc)
		}
		if doc["data"].(map[string]any)["k"] != "dg==" {
			t.Fatalf("secret data is base64 on the wire: %v", doc["data"])
		}
		cl, _ = client(t, func(w http.ResponseWriter, req *http.Request) { status(w, 409, "Conflict", "m") })
		if _, err := cl.UpdateSecret(ctx, ns, kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: "s", ResourceVersion: "5"}}); !errors.Is(err, kubernetes.ErrConflict) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("pagination is bounded", func(t *testing.T) {
		var page atomic.Int32
		h := func(w http.ResponseWriter, r *http.Request) {
			p := page.Add(1)
			next := ""
			if r.URL.Query().Get("continue") == "" && p != 1 {
				t.Errorf("page %d without a continuation token", p)
			}
			if p < 3 {
				next = fmt.Sprintf("tok%d", p)
			}
			items := fmt.Sprintf(`{"metadata":{"name":"p%d"}}`, p)
			_, _ = fmt.Fprintf(w, `{"metadata":{"continue":%q},"items":[%s]}`, next, items)
		}
		cl, r := client(t, h, func(c *kubernetes.Config) { c.PageSize = 1 })
		pods, err := cl.ListPods(ctx, ns, "a=b")
		if err != nil || len(pods) != 3 {
			t.Fatalf("%d %v", len(pods), err)
		}
		q := r.reqs[0].URL.Query()
		if q.Get("limit") != "1" || q.Get("labelSelector") != "a=b" {
			t.Fatalf("%v", q)
		}
		if r.reqs[2].URL.Query().Get("continue") != "tok2" {
			t.Fatal("did not follow the continuation")
		}
		// An unbounded listing is cut off.
		page.Store(-1000)
		inf := func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(`{"metadata":{"continue":"more"},"items":[]}`))
		}
		cl, r = client(t, inf, func(c *kubernetes.Config) { c.MaxPages = 4 })
		if _, err := cl.ListJobs(ctx, ns, ""); !errors.Is(err, kubernetes.ErrTooLarge) || r.n() != 4 {
			t.Fatalf("%v after %d pages", err, r.n())
		}
		// An expired continuation restarts nothing silently.
		cl, _ = client(t, func(w http.ResponseWriter, req *http.Request) { status(w, 410, "Expired", "m") })
		if _, err := cl.ListJobs(ctx, ns, ""); !errors.Is(err, kubernetes.ErrExpired) {
			t.Fatalf("%v", err)
		}
		// A page failing midway fails the listing: no partial list is returned as complete.
		var n atomic.Int32
		cl, _ = client(t, func(w http.ResponseWriter, req *http.Request) {
			if n.Add(1) == 2 {
				status(w, 500, "InternalError", "m")
				return
			}
			_, _ = w.Write([]byte(`{"metadata":{"continue":"x"},"items":[{"metadata":{"name":"a"}}]}`))
		})
		jobs, err := cl.ListJobs(ctx, ns, "")
		if err == nil {
			t.Fatalf("partial listing accepted (%d items)", len(jobs))
		}
	})
	t.Run("names are validated before any request", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("{}")) })
		for _, bad := range []string{"../etc", "A", "a/b", "", "x?y=z", strings.Repeat("a", 300)} {
			if _, err := cl.GetJob(ctx, ns, bad); !errors.Is(err, kubernetes.ErrInvalid) {
				t.Fatalf("%q: %v", bad, err)
			}
			if _, err := cl.GetSecret(ctx, bad, name); !errors.Is(err, kubernetes.ErrInvalid) {
				t.Fatalf("%q: %v", bad, err)
			}
		}
		if r.n() != 0 {
			t.Fatalf("%d requests were sent for invalid names", r.n())
		}
	})
	t.Run("errors are redacted", func(t *testing.T) {
		secret := "adm_THE-SECRET-VALUE-0123456789"
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
			status(w, 422, "Invalid", "Secret \"x\" is invalid: data[admission]: "+secret)
		})
		_, err := cl.CreateSecret(ctx, ns, kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: "s"}, Data: map[string][]byte{"admission": []byte(secret)}})
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), secret) {
			t.Fatalf("the error echoes the response: %v", err)
		}
		if !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("%v", err)
		}
		// A hostile reason string is dropped.
		cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) { status(w, 404, "bad reason with spaces "+secret, "m") })
		_, err = cl.GetJob(ctx, ns, name)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("redirects are not followed", func(t *testing.T) {
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("a redirect was followed") }))
		defer other.Close()
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		})
		if _, err := cl.GetJob(ctx, ns, name); err == nil {
			t.Fatal("a redirect is not success")
		}
		if _, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}}); err == nil {
			t.Fatal("a redirect is not success")
		}
	})
	t.Run("configuration is validated", func(t *testing.T) {
		hc := &http.Client{}
		for _, origin := range []string{"", "http://example.com", "ftp://x", "https://u:p@host", "https://host/path", "https://host?x=1", "host:1"} {
			if _, err := kubernetes.NewClient(kubernetes.Config{Origin: origin, HTTP: hc}); err == nil {
				t.Fatalf("%q accepted", origin)
			}
		}
		if _, err := kubernetes.NewClient(kubernetes.Config{Origin: "https://10.0.0.1:6443"}); err == nil {
			t.Fatal("no authenticated client accepted")
		}
		for _, ok := range []string{"https://10.0.0.1:6443", "http://127.0.0.1:8001", "http://localhost:8001", "http://[::1]:8001"} {
			if _, err := kubernetes.NewClient(kubernetes.Config{Origin: ok, HTTP: hc}); err != nil {
				t.Fatalf("%q: %v", ok, err)
			}
		}
	})
	t.Run("job round trip keeps the raw document", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"metadata":{"name":"job-1","uid":"u1"},"spec":{"template":{"spec":{"hostNetwork":true,"containers":[]}},"somethingNew":1}}`))
		})
		j, err := cl.GetJob(ctx, ns, name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(j.Raw), "somethingNew") || j.Metadata.UID != "u1" {
			t.Fatalf("%s", j.Raw)
		}
		created, err := cl.CreateJob(ctx, ns, kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: name}})
		if err != nil || !strings.Contains(string(created.Raw), "somethingNew") {
			t.Fatalf("%v", err)
		}
		var sent map[string]any
		_ = json.Unmarshal(r.body[1], &sent)
		if sent["kind"] != "Job" || sent["apiVersion"] != "batch/v1" || sent["metadata"].(map[string]any)["namespace"] != ns {
			t.Fatalf("%v", sent)
		}
		if r.reqs[1].Header.Get("Content-Type") != "application/json" || r.reqs[1].Header.Get("User-Agent") == "" {
			t.Fatal("headers")
		}
	})
	t.Run("access reviews are reads", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(`{"status":{"allowed":true}}`))
		})
		ok, err := cl.Allowed(ctx, "system:serviceaccount:a:b", nil, kubernetes.ResourceAttributes{Namespace: "a", Verb: "get", Resource: "secrets"})
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		var doc map[string]any
		_ = json.Unmarshal(r.body[0], &doc)
		if doc["kind"] != "SubjectAccessReview" {
			t.Fatalf("%v", doc)
		}
		if _, err := cl.Allowed(ctx, " ", nil, kubernetes.ResourceAttributes{}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("%v", err)
		}
		lost, _ := client(t, func(w http.ResponseWriter, req *http.Request) {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
		})
		if _, err := lost.Allowed(ctx, "u", nil, kubernetes.ResourceAttributes{Verb: "get", Resource: "pods"}); !errors.Is(err, kubernetes.ErrUnavailable) || errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("a lost access review is an inconclusive read: %v", err)
		}
	})
}

// Test_kubernetes_policy_decoding: the policy fields preflight enforces survive
// decoding of real API documents; dropping them would turn a restricted policy
// into an apparently compliant one.
func Test_kubernetes_policy_decoding(t *testing.T) {
	ctx := context.Background()
	t.Run("network policy keeps port ranges", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"metadata":{},"items":[{"metadata":{"name":"p"},"spec":{
				"podSelector":{"matchLabels":{"a":"b"},"matchExpressions":[{"key":"k","operator":"Exists"}]},
				"policyTypes":["Egress"],
				"egress":[{"to":[{"ipBlock":{"cidr":"192.0.2.0/24","except":["192.0.2.9/32"]}}],
				"ports":[{"protocol":"TCP","port":8000,"endPort":9000},{"protocol":"UDP","port":53},{"port":"https"}]}]}}]}`)
		})
		pols, err := cl.ListNetworkPolicies(ctx, ns)
		if err != nil || len(pols) != 1 {
			t.Fatalf("%d %v", len(pols), err)
		}
		ports := pols[0].Spec.Egress[0].Ports
		if len(ports) != 3 {
			t.Fatalf("ports: %+v", ports)
		}
		if ports[0].EndPort == nil || *ports[0].EndPort != 9000 || ports[0].Port != float64(8000) {
			t.Fatalf("the port range was lost: %+v", ports[0])
		}
		if ports[1].EndPort != nil || ports[2].EndPort != nil || ports[2].Port != "https" {
			t.Fatalf("single and named ports: %+v", ports[1:])
		}
		if len(pols[0].Spec.PodSelector.MatchExpressions) != 1 || pols[0].Spec.Egress[0].To[0].IPBlock.Except[0] != "192.0.2.9/32" {
			t.Fatalf("selector or exceptions lost: %+v", pols[0].Spec)
		}
	})
	t.Run("resource quota keeps scopes", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"metadata":{},"items":[
				{"metadata":{"name":"plain"},"spec":{"hard":{"pods":"10"}}},
				{"metadata":{"name":"scopes"},"spec":{"hard":{"pods":"10"},"scopes":["BestEffort"]}},
				{"metadata":{"name":"selector"},"spec":{"hard":{"pods":"10"},"scopeSelector":{"matchExpressions":[
					{"scopeName":"PriorityClass","operator":"In","values":["high"]}]}}}]}`)
		})
		qs, err := cl.ListResourceQuotas(ctx, ns)
		if err != nil || len(qs) != 3 {
			t.Fatalf("%d %v", len(qs), err)
		}
		if qs[0].Scoped() || qs[0].Spec.Hard["pods"] != "10" {
			t.Fatalf("plain quota: %+v", qs[0])
		}
		if !qs[1].Scoped() || qs[1].Spec.Scopes[0] != "BestEffort" {
			t.Fatalf("scopes lost: %+v", qs[1])
		}
		sel := qs[2].Spec.ScopeSelector
		if !qs[2].Scoped() || sel == nil || sel.MatchExpressions[0].ScopeName != "PriorityClass" || sel.MatchExpressions[0].Values[0] != "high" {
			t.Fatalf("scope selector lost: %+v", qs[2])
		}
	})
}
