package gitservice

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// Test_unit_gitservice_forwarding_assertion_required (GR-03, GR-05, S3): no
// Git request reaches the backend without a verified forwarding assertion for
// this tenant's worker, the repository and the service; each refusal is the
// contract's error and none touches the control plane.
func Test_unit_gitservice_forwarding_assertion_required(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	e.initRepo("repo_2", "main")
	w := e.workdir()
	e.seed("repo_1", w, "main:refs/heads/main")

	const upload = "/repo_1.git/info/refs?service=git-upload-pack"
	const receive = "/repo_1.git/info/refs?service=git-receive-pack"
	past := time.Now().UTC().Add(-time.Hour)
	for _, tc := range []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"no assertion", upload, "", http.StatusUnauthorized},
		{"unverifiable token", upload, "garbage", http.StatusUnauthorized},
		{"another tenant's worker", upload, e.tokenWith("repo_1", true, func(a *gitv1.ForwardingAssertion) {
			a.TenantID, a.Audience = "t_other", gitv1.WorkerAudience("t_other")
		}), http.StatusForbidden},
		{"another audience", upload, e.tokenWith("repo_1", true, func(a *gitv1.ForwardingAssertion) { a.Audience = gitv1.GrantAudience }), http.StatusUnauthorized},
		{"expired", upload, e.tokenWith("repo_1", true, func(a *gitv1.ForwardingAssertion) {
			a.IssuedAt = gitv1.Timestamp(past.Add(-time.Minute).Format(time.RFC3339Nano))
			a.ExpiresAt = gitv1.Timestamp(past.Format(time.RFC3339Nano))
		}), http.StatusUnauthorized},
		{"too long lived", upload, e.tokenWith("repo_1", true, func(a *gitv1.ForwardingAssertion) {
			a.ExpiresAt = gitv1.Timestamp(time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
		}), http.StatusUnauthorized},
		{"read-only assertion on receive-pack", receive, e.token("repo_1", false), http.StatusForbidden},
		{"assertion for another repository", upload, e.token("repo_2", true), http.StatusNotFound},
		{"unknown repository", "/repo_9.git/info/refs?service=git-upload-pack", e.token("repo_9", true), http.StatusNotFound},
	} {
		status, _, body := e.request("GET", tc.path, tc.token, nil, nil)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, status, tc.status, body)
		}
		if strings.Contains(body, "refs/heads/main") || strings.Contains(body, "# service") {
			t.Errorf("%s: the backend answered: %s", tc.name, body)
		}
	}
	if status, _, _ := e.request("POST", "/repo_1.git/git-receive-pack", e.token("repo_1", false), map[string]string{"Content-Type": "application/x-git-receive-pack-request"}, strings.NewReader("0000")); status != http.StatusForbidden {
		t.Errorf("receive-pack POST with a read-only assertion: %d", status)
	}
	if n := e.control.calls(); n != 0 {
		t.Errorf("the control plane was called %d times by refused requests", n)
	}

	// Real clients: the credential-less clone fails, the asserted one works; a
	// read-only assertion can fetch but its push is refused, leaving no ref.
	dst := t.TempDir()
	if out, err := e.git(dst, "", "clone", "-q", e.url("repo_1"), "none"); err == nil {
		t.Fatalf("a clone without an assertion succeeded:\n%s", out)
	}
	e.mustGit(dst, e.token("repo_1", false), "clone", "-q", e.url("repo_1"), "ro")
	e.commit(dst+"/ro", "x", "x\n")
	if out, err := e.git(dst+"/ro", e.token("repo_1", false), "push", "-q", "origin", "HEAD:refs/heads/ro"); err == nil {
		t.Fatalf("a read-only assertion pushed:\n%s", out)
	}
	if e.bareRef("repo_1", "refs/heads/ro") != "" {
		t.Fatal("the refused push created a ref")
	}
	if a, _, _ := e.control.snapshot(); len(a) != 0 {
		t.Fatal("a refused push reached the control plane")
	}
}

// Test_unit_gitservice_path_from_id_only (GR-02, GR-05): the repository's
// on-disk path is derived from the opaque repository ID alone; nothing in a
// request path, name or traversal sequence selects another location.
func Test_unit_gitservice_path_from_id_only(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	e.initRepo("repo_2", "main")
	tok := e.token("repo_1", true)

	if got, want := e.repoDir("repo_1"), filepath.Join(e.svc.layout.repositories, "repo_1.git"); got != want {
		t.Fatalf("test layout drifted: %s", got)
	}
	// repoDir: every valid ID is a direct child of the repositories directory;
	// anything that could climb, nest or hide is refused before any path exists.
	for _, id := range []string{"repo_1", "A", "a-b_c", strings.Repeat("a", 128)} {
		dir, err := e.svc.repoDir(id)
		if err != nil || filepath.Dir(dir) != e.svc.layout.repositories || filepath.Base(dir) != id+".git" {
			t.Errorf("repoDir(%q) = %q, %v", id, dir, err)
		}
	}
	for _, id := range []string{"", "..", ".", "../repo_1", "repo_1/..", "a/b", "/etc", ".hidden", "repo.1", "repo_1\x00", "repo 1", strings.Repeat("a", 129), "-x", "repo_1.git"} {
		if dir, err := e.svc.repoDir(id); err == nil {
			t.Errorf("repoDir(%q) = %q, want an error", id, dir)
		}
	}

	for _, path := range []string{
		"/..%2frepo_2.git/info/refs?service=git-upload-pack",
		"/%2e%2e/repo_2.git/info/refs?service=git-upload-pack",
		"/repo_1.git/../repo_2.git/info/refs?service=git-upload-pack",
		"//repo_1.git/info/refs?service=git-upload-pack",
		"/repo_1.git%2f..%2frepo_2.git/info/refs?service=git-upload-pack",
		"/repo.1.git/info/refs?service=git-upload-pack",
		"/repo_1.git.git/info/refs?service=git-upload-pack",
		"/.git/info/refs?service=git-upload-pack",
		"/sub/repo_1.git/info/refs?service=git-upload-pack",
		"/repo_1.git/info/refs/../refs?service=git-upload-pack",
		"/" + strings.Repeat("a", 200) + ".git/info/refs?service=git-upload-pack",
	} {
		if status, _, body := e.request("GET", path, tok, nil, nil); status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (%s)", path, status, body)
		}
	}
	// A valid assertion for repo_1 cannot be used on the path of repo_2, and
	// the path of repo_1 is served from repo_1's own directory.
	if status, _, _ := e.request("GET", "/repo_2.git/info/refs?service=git-upload-pack", tok, nil, nil); status != http.StatusNotFound {
		t.Errorf("an assertion for repo_1 on repo_2's path: %d", status)
	}
	w := e.workdir()
	e.seed("repo_1", w, "main:refs/heads/only-in-1")
	status, _, body := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, nil, nil)
	if status != 200 || !strings.Contains(body, "only-in-1") {
		t.Errorf("repo_1: %d %q", status, body)
	}
	if status, _, body = e.request("GET", "/repo_2.git/info/refs?service=git-upload-pack", e.token("repo_2", true), nil, nil); status != 200 || strings.Contains(body, "only-in-1") {
		t.Errorf("repo_2 shows repo_1's refs: %d %q", status, body)
	}
	// RPC: the body and the path must name the same ID, and a path outside the
	// ID syntax never matches a route.
	rpcError(e, "/repositories/repo_1/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_2", Prefix: "refs/heads/", Limit: 10}, http.StatusBadRequest, "invalid_request")
	rpcError(e, "/repositories/repo_1/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_2", DefaultBranch: "main"}, http.StatusBadRequest, "invalid_request")
	rpcError(e, "/repositories/a.b/refs/list", gitv1.ReadRefsRequest{RepositoryID: "a.b", Prefix: "refs/heads/", Limit: 10}, http.StatusBadRequest, "invalid_request")
	// Nothing was created outside the repositories directory.
	entries, _ := os.ReadDir(e.svc.layout.repositories)
	if len(entries) != 2 {
		t.Errorf("repositories directory holds %v", entries)
	}
}
