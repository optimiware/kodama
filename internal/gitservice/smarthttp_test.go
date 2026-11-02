package gitservice

import (
	"io"
	"net/http"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

func (e *testEnv) request(method, path, token string, headers map[string]string, body io.Reader) (int, http.Header, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(AssertionHeader, token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, string(data)
}

// Test_unit_gitservice_smart_http_only (GR-05, GR-16): only the three smart
// endpoints are served; dumb HTTP, upload-archive and every other path or method
// are 404 even with a valid assertion, and the backend never runs for them.
func Test_unit_gitservice_smart_http_only(t *testing.T) {
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	tok := e.token("repo_1", true)

	status, header, body := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, nil, nil)
	if status != 200 || header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" || !strings.Contains(body, "# service=git-upload-pack") {
		t.Fatalf("upload-pack advertisement: %d %v %q", status, header, body)
	}
	if status, header, _ = e.request("GET", "/repo_1.git/info/refs?service=git-receive-pack", tok, nil, nil); status != 200 || header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" {
		t.Fatalf("receive-pack advertisement: %d %v", status, header)
	}

	for _, tc := range []struct{ method, path string }{
		{"GET", "/repo_1.git/info/refs"},                                                  // dumb HTTP
		{"GET", "/repo_1.git/info/refs?service="},                                         // no service
		{"GET", "/repo_1.git/info/refs?service=git-upload-archive"},                       // another service
		{"GET", "/repo_1.git/info/refs?service=git-upload-pack&service=git-receive-pack"}, // two services
		{"GET", "/repo_1.git/info/refs?service=git-upload-pack&extra=1"},                  // an extra parameter
		{"POST", "/repo_1.git/info/refs?service=git-upload-pack"},                         // wrong method
		{"GET", "/repo_1.git/git-upload-pack"},                                            // wrong method
		{"POST", "/repo_1.git/git-upload-pack?service=git-upload-pack"},                   // a query on a POST
		{"POST", "/repo_1.git/git-upload-archive"},                                        // upload-archive
		{"GET", "/repo_1.git/HEAD"},                                                       // dumb HTTP
		{"GET", "/repo_1.git/config"},                                                     // repository internals
		{"GET", "/repo_1.git/kodama/journal.jsonl"},                                       // the journal
		{"GET", "/repo_1.git/objects/info/packs"},                                         // dumb HTTP
		{"GET", "/repo_1.git/refs/heads/main"},                                            // dumb HTTP
		{"GET", "/repo_1.git/"},                                                           // the repository itself
		{"GET", "/repo_1.git"},                                                            // no endpoint
		{"GET", "/"},                                                                      // nothing
		{"GET", "/repo_1/info/refs?service=git-upload-pack"},                              // no .git
		{"GET", "/repo_1.git/info/refs/?service=git-upload-pack"},                         // trailing slash
		{"GET", "/repo_1.git/info//refs?service=git-upload-pack"},                         // empty segment
		{"DELETE", "/repo_1.git/git-receive-pack"},                                        // another method
		{"GET", gitv1.WorkerBasePath + "x/repositories/repo_1/refs/list"},                 // near the RPC prefix
		{"GET", "/internal/worker/v1"},                                                    // the RPC root is not a Git path
		{"POST", "/repo_1.git/info/refs?service=git-receive-pack"},                        // wrong method
	} {
		if status, _, body := e.request(tc.method, tc.path, tok, nil, nil); status != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404 (%s)", tc.method, tc.path, status, body)
		}
	}
	if status, _, _ := e.request("GET", "/healthz", "", nil, nil); status != 200 {
		t.Errorf("/healthz: %d", status)
	}
	if n := e.control.calls(); n != 0 {
		t.Errorf("the control plane was called %d times by read-only probes", n)
	}
	// A real clone and fetch round trip through the endpoints.
	w := e.workdir()
	e.seed("repo_1", w, "main:refs/heads/main")
	dst := t.TempDir()
	e.mustGit(dst, tok, "clone", "-q", e.url("repo_1"), "c")
}

// Test_unit_gitservice_protocol_v2 (GR-05): the Git-Protocol header is passed
// through to the backend, so a v2 client gets a v2 advertisement and a real
// clone negotiates protocol v2; a malformed header is refused.
func Test_unit_gitservice_protocol_v2(t *testing.T) {
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	e.seed("repo_1", w, "main:refs/heads/main")
	tok := e.token("repo_1", true)

	status, _, body := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, map[string]string{"Git-Protocol": "version=2"}, nil)
	if status != 200 || !strings.Contains(body, "version 2") || strings.Contains(body, "refs/heads/main") {
		t.Fatalf("v2 advertisement: %d %q", status, body)
	}
	status, _, body = e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, nil, nil)
	if status != 200 || !strings.Contains(body, "refs/heads/main") || strings.Contains(body, "version 2") {
		t.Fatalf("v0 advertisement: %d %q", status, body)
	}
	for _, bad := range []string{"version=3", "version=2;evil", "x", "version=2:a=b c"} {
		if status, _, _ := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, map[string]string{"Git-Protocol": bad}, nil); status != http.StatusBadRequest {
			t.Errorf("Git-Protocol %q: status %d, want 400", bad, status)
		}
	}

	dst := t.TempDir()
	e.mustGit(dst, tok, "-c", "protocol.version=2", "clone", "-q", e.url("repo_1"), "v2")
	clone := dst + "/v2"
	out, err := e.gitTrace(clone, tok, "-c", "protocol.version=2", "fetch", "origin")
	if err != nil || !strings.Contains(out, "version 2") {
		t.Fatalf("v2 fetch did not negotiate protocol v2: %v\n%s", err, out)
	}
	// A push with protocol.version=2 configured works too (receive-pack keeps
	// its own framing).
	e.commit(clone, "second", "2\n")
	e.mustGit(clone, tok, "-c", "protocol.version=2", "push", "-q", "origin", "HEAD:refs/heads/kodama/work/wi_1")
}
