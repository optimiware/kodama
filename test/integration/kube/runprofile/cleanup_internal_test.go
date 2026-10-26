package runprofile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// kubectlStub is a controlled stand-in for kubectl: no cluster is contacted.
type kubectlStub struct {
	path string
}

// newKubectlStub writes a script answering the few commands teardown issues.
// pvList/nsList are shell fragments printed for the PV and final namespace
// listings (use `exit 1` to simulate a failing lookup); nsJSON is the
// namespace object returned to `get namespace NAME -o json`.
func newKubectlStub(t *testing.T, port, pvList, nsList, nsJSON string, proxyOK bool) *kubectlStub {
	t.Helper()
	proxy := fmt.Sprintf(`echo "Starting to serve on 127.0.0.1:%s"; exec sleep 30`, port)
	if !proxyOK {
		proxy = `echo "proxy unavailable" >&2; exit 1`
	}
	script := fmt.Sprintf(`#!/bin/bash
args="$*"
case "$args" in
  *" proxy "*) %s ;;
  *" get pv -o jsonpath"*) %s ;;
  *" get pv "*) ;;
  *" get volumeattachments"*) echo '{"items":[]}' ;;
  *" get namespace -l "*) %s ;;
  *" get namespace "*"--ignore-not-found"*) ;;
  *" get namespace "*" -o json"*) cat <<'JSON'
%s
JSON
  ;;
  *) echo "unexpected: $args" >&2; exit 2 ;;
esac
`, proxy, pvList, nsList, nsJSON)
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &kubectlStub{path: p}
}

func nsObject(uid, rv, run string) string {
	return fmt.Sprintf(`{"metadata":{"uid":%q,"resourceVersion":%q,"labels":{%q:%q,%q:%q}}}`, uid, rv, LabelTask, TaskValue, LabelRun, run)
}

type apiRecorder struct {
	mu      sync.Mutex
	deletes []map[string]any
	paths   []string
	status  func(n int) int
}

func (a *apiRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	a.mu.Lock()
	a.deletes = append(a.deletes, parsed)
	a.paths = append(a.paths, r.Method+" "+r.URL.Path)
	n := len(a.deletes)
	a.mu.Unlock()
	w.WriteHeader(a.status(n))
}

func runTeardown(t *testing.T, stub *kubectlStub, ns, uid string) *Report {
	t.Helper()
	rec, err := NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	q := &Qualifier{
		c:   &Cluster{K: &Kubectl{Binary: stub.path, Kubeconfig: "/kc", Rec: rec}, RunID: "abc", Namespaces: map[string]string{ns: uid}},
		rep: &Report{},
	}
	q.teardown()
	return q.rep
}

func serve(t *testing.T, status func(int) int) (*apiRecorder, string) {
	t.Helper()
	rec := &apiRecorder{status: status}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return rec, u.Port()
}

const testNS = NamespacePrefix + "abc"

func Test_unit_runprofile_cleanup_verified_only_when_proven(t *testing.T) {
	api, port := serve(t, func(int) int { return 200 })
	stub := newKubectlStub(t, port, `echo "pv-1 `+testNS+`"`, `true`, nsObject("uid-1", "7", "abc"), true)
	rep := runTeardown(t, stub, testNS, "uid-1")
	if !rep.Cleanup.Verified || len(rep.Cleanup.Leftovers) != 0 {
		t.Fatalf("expected verified cleanup, got %+v", rep.Cleanup)
	}
	if len(api.deletes) != 1 || api.paths[0] != "DELETE /api/v1/namespaces/"+testNS {
		t.Fatalf("unexpected API calls %v", api.paths)
	}
	pre, _ := api.deletes[0]["preconditions"].(map[string]any)
	if pre["uid"] != "uid-1" || pre["resourceVersion"] != "7" {
		t.Fatalf("delete lacked uid/resourceVersion preconditions: %v", api.deletes[0])
	}
}

func Test_unit_runprofile_cleanup_lookup_failures_are_unverified(t *testing.T) {
	for name, tc := range map[string]struct{ pv, ns string }{
		"pv listing fails":     {`echo boom >&2; exit 1`, `true`},
		"final listing fails":  {`true`, `echo boom >&2; exit 1`},
		"leftover labelled ns": {`true`, `echo namespace/` + testNS},
	} {
		_, port := serve(t, func(int) int { return 200 })
		stub := newKubectlStub(t, port, tc.pv, tc.ns, nsObject("uid-1", "7", "abc"), true)
		rep := runTeardown(t, stub, testNS, "uid-1")
		if rep.Cleanup.Verified {
			t.Errorf("%s: cleanup reported verified", name)
		}
		if len(rep.Cleanup.Leftovers) == 0 {
			t.Errorf("%s: no leftover recorded", name)
		}
	}
}

func Test_unit_runprofile_cleanup_never_deletes_foreign_replacement(t *testing.T) {
	// 1. The namespace has another UID or another run's labels at lookup time.
	for name, obj := range map[string]string{
		"different uid": nsObject("uid-FOREIGN", "7", "abc"),
		"different run": nsObject("uid-1", "7", "zzz"),
		"unlabelled":    `{"metadata":{"uid":"uid-1","resourceVersion":"1","labels":{}}}`,
	} {
		api, port := serve(t, func(int) int { return 200 })
		stub := newKubectlStub(t, port, `true`, `true`, obj, true)
		rep := runTeardown(t, stub, testNS, "uid-1")
		if len(api.deletes) != 0 {
			t.Errorf("%s: delete was sent", name)
		}
		if rep.Cleanup.Verified || !strings.Contains(rep.Cleanup.Namespaces[0].Detail, "refused") {
			t.Errorf("%s: not refused/unverified: %+v", name, rep.Cleanup)
		}
	}
	// 2. The object is replaced after the check: the API's precondition
	// answers 409 on every attempt, so the run refuses instead of deleting.
	api, port := serve(t, func(int) int { return 409 })
	stub := newKubectlStub(t, port, `true`, `true`, nsObject("uid-1", "7", "abc"), true)
	rep := runTeardown(t, stub, testNS, "uid-1")
	if len(api.deletes) != 3 {
		t.Errorf("expected 3 bounded attempts, got %d", len(api.deletes))
	}
	if rep.Cleanup.Verified || rep.Cleanup.Namespaces[0].Deleted || !strings.Contains(rep.Cleanup.Namespaces[0].Detail, "refused") {
		t.Errorf("conflicting namespace not refused: %+v", rep.Cleanup)
	}
	// 3. One transient conflict (version bump) is retried after re-verifying identity.
	api, port = serve(t, func(n int) int {
		if n == 1 {
			return 409
		}
		return 200
	})
	stub = newKubectlStub(t, port, `true`, `true`, nsObject("uid-1", "7", "abc"), true)
	rep = runTeardown(t, stub, testNS, "uid-1")
	if len(api.deletes) != 2 || !rep.Cleanup.Verified {
		t.Errorf("retry after version conflict failed: calls=%d %+v", len(api.deletes), rep.Cleanup)
	}
}

func Test_unit_runprofile_cleanup_refuses_without_precondition_channel(t *testing.T) {
	api, port := serve(t, func(int) int { return 200 })
	stub := newKubectlStub(t, port, `true`, `true`, nsObject("uid-1", "7", "abc"), false)
	rep := runTeardown(t, stub, testNS, "uid-1")
	if len(api.deletes) != 0 || rep.Cleanup.Verified {
		t.Fatalf("name-only deletion fallback happened: calls=%d %+v", len(api.deletes), rep.Cleanup)
	}
	if !strings.Contains(rep.Cleanup.Namespaces[0].Detail, "refused") {
		t.Fatalf("detail %q", rep.Cleanup.Namespaces[0].Detail)
	}
}

type statusAPI struct {
	url string
	rec *apiRecorder
}

func (s *statusAPI) count() int { s.rec.mu.Lock(); defer s.rec.mu.Unlock(); return len(s.rec.deletes) }

func serveStatus(t *testing.T, status int) (*statusAPI, string) {
	t.Helper()
	rec := &apiRecorder{status: func(int) int { return status }}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &statusAPI{url: srv.URL, rec: rec}, u.Port()
}
