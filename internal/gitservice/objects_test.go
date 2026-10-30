package gitservice

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- helpers ----

// commitAt commits a file with the given committer date (the author date is a
// day earlier, so a test that mixes them up fails).
func (e *testEnv) commitAt(dir, name, content, committerDate string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.mustPlain(dir, "add", name)
	cmd := exec.Command("git", "commit", "-q", "-m", "add "+name)
	cmd.Dir = dir
	cmd.Env = append(e.clientEnv(), "GIT_COMMITTER_DATE="+committerDate, "GIT_AUTHOR_DATE=1600000000 +0000")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git commit: %v\n%s", err, out)
	}
	return e.mustPlain(dir, "rev-parse", "HEAD")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// createBundle calls CreateBundle and splits the answer into the status, the
// manifest line, the bundle bytes (200) or the error body.
func (e *testEnv) createBundleRaw(repo string, refs []string) (int, gitv1.CreateBundleResponse, []byte, []byte) {
	e.t.Helper()
	// The server releases its concurrency slot just after the response ends: a
	// 429 right after a previous bundle is retried.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		status, m, bundle, body := e.createBundleOnce(repo, refs)
		if status != http.StatusTooManyRequests || time.Now().After(deadline) {
			return status, m, bundle, body
		}
	}
}

func (e *testEnv) createBundleOnce(repo string, refs []string) (int, gitv1.CreateBundleResponse, []byte, []byte) {
	e.t.Helper()
	status, data := e.rpc("/repositories/"+repo+"/bundles/create", gitv1.CreateBundleRequest{RepositoryID: repo, Refs: refs})
	if status != http.StatusOK {
		return status, gitv1.CreateBundleResponse{}, nil, data
	}
	br := bufio.NewReader(bytes.NewReader(data))
	line, err := gitv1.ReadFrameLine(br)
	if err != nil {
		e.t.Fatalf("manifest line: %v", err)
	}
	m, err := gitv1.DecodeResponse[gitv1.CreateBundleResponse](line)
	if err != nil {
		e.t.Fatalf("manifest: %v\n%s", err, line)
	}
	bundle, _ := io.ReadAll(br)
	return status, m, bundle, nil
}

func (e *testEnv) createBundle(repo string, refs []string) (gitv1.CreateBundleResponse, []byte) {
	e.t.Helper()
	status, m, bundle, body := e.createBundleRaw(repo, refs)
	if status != http.StatusOK {
		e.t.Fatalf("CreateBundle: %d %s", status, body)
	}
	if int64(len(bundle)) != m.Size || sha256Hex(bundle) != m.SHA256 {
		e.t.Fatalf("the manifest does not describe the bundle: %d bytes, sha %s vs %+v", len(bundle), sha256Hex(bundle), m)
	}
	return m, bundle
}

// replayRaw posts a replay: one JSON line, then the bundle bytes.
func (e *testEnv) replayRaw(repo, op string, sha *string, bundle []byte, contentType string) (int, []byte, error) {
	e.t.Helper()
	line, err := gitv1.FrameLine(gitv1.ReplayBundleRequest{RepositoryID: repo, OperationID: op, SHA256: sha})
	if err != nil {
		e.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+gitv1.WorkerBasePath+"/repositories/"+repo+"/bundles/replay", bytes.NewReader(append(line, bundle...)))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func (e *testEnv) replay(repo, op string, bundle []byte) gitv1.ReplayBundleResponse {
	e.t.Helper()
	sum := sha256Hex(bundle)
	status, data, err := e.replayRaw(repo, op, &sum, bundle, gitv1.BundleMediaType)
	if err != nil || status != http.StatusOK {
		e.t.Fatalf("ReplayBundle: %d %v %s", status, err, data)
	}
	r, err := gitv1.DecodeResponse[gitv1.ReplayBundleResponse](data)
	if err != nil {
		e.t.Fatalf("ReplayBundle answer: %v\n%s", err, data)
	}
	return r
}

// replayRefused expects a contract error with the status and code and that the
// repository's refs did not change.
func (e *testEnv) replayRefused(repo, op string, sha *string, bundle []byte, wantStatus int, wantCode domain.ErrorCode) {
	e.t.Helper()
	before := e.allRefs(repo)
	status, data, err := e.replayRaw(repo, op, sha, bundle, gitv1.BundleMediaType)
	if err != nil {
		e.t.Fatalf("a refused replay was a transport failure: %v", err)
	}
	if status != wantStatus || !strings.Contains(string(data), `"`+string(wantCode)+`"`) {
		e.t.Fatalf("replay: %d %s, want %d %s", status, data, wantStatus, wantCode)
	}
	if after := e.allRefs(repo); after != before {
		e.t.Fatalf("a refused replay changed refs:\n%s\n--\n%s", before, after)
	}
}

func (e *testEnv) allRefs(repo string) string {
	e.t.Helper()
	dir := e.repoDir(repo)
	out, err := e.gitPlain(dir, "--git-dir="+dir, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		e.t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	return out
}

// setHidden creates a refs/kodama ref directly (a push cannot: they are hidden).
func (e *testEnv) setHidden(repo, ref, commit string) {
	e.t.Helper()
	dir := e.repoDir(repo)
	e.mustPlain(dir, "-c", "core.hooksPath="+os.DevNull, "--git-dir="+dir, "update-ref", ref, commit)
}

func (e *testEnv) fence(repo string, on bool) gitv1.RestoreFenceResponse {
	e.t.Helper()
	return rpcOK[gitv1.RestoreFenceResponse](e, "/repositories/"+repo+"/restore-fence", gitv1.RestoreFenceRequest{RepositoryID: repo, Fenced: on})
}

func (e *testEnv) hasCommit(repo, commit string) bool {
	e.t.Helper()
	return rpcOK[gitv1.HasCommitResponse](e, "/repositories/"+repo+"/objects/has-commit", gitv1.HasCommitRequest{RepositoryID: repo, Commit: commit}).Present
}

func refMap(tips []gitv1.RefTip) map[string]string {
	m := map[string]string{}
	for _, t := range tips {
		m[t.Ref] = t.Commit
	}
	return m
}

func (e *testEnv) bundleRefs(bundle []byte) string {
	e.t.Helper()
	f := filepath.Join(e.t.TempDir(), "b.bundle")
	if err := os.WriteFile(f, bundle, 0o600); err != nil {
		e.t.Fatal(err)
	}
	repo := e.t.TempDir()
	e.mustPlain(repo, "init", "-q", "--bare", ".")
	return e.mustPlain(repo, "bundle", "list-heads", f)
}

// ---- IsAncestor ----

// Test_unit_gitservice_is_ancestor: ancestry is answered by Git for commits the
// repository holds (a commit is its own ancestor), a missing or non-commit
// object is commit_not_found rather than false, and a pushed refs/replace/*
// cannot change the answer.
func Test_unit_gitservice_is_ancestor(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	c2 := e.commit(w, "two", "2\n")
	c3 := e.commit(w, "three", "3\n")
	e.mustPlain(w, "checkout", "-q", "-b", "side", c1)
	d := e.commit(w, "side", "s\n")
	e.mustPlain(w, "checkout", "-q", "--orphan", "other")
	r := e.commit(w, "other", "o\n")
	e.mustPlain(w, "checkout", "-q", "main")
	e.mustPlain(w, "tag", "-a", "-m", "t", "v1", c2)
	tag := e.mustPlain(w, "rev-parse", "refs/tags/v1")
	tree := e.mustPlain(w, "rev-parse", c3+"^{tree}")
	e.seed("repo_1", w, "main:refs/heads/main", "side:refs/heads/side", "other:refs/heads/other", "refs/tags/v1:refs/tags/v1")

	const path = "/repositories/repo_1/objects/is-ancestor"
	is := func(a, d string) bool {
		t.Helper()
		return rpcOK[gitv1.IsAncestorResponse](e, path, gitv1.IsAncestorRequest{RepositoryID: "repo_1", Ancestor: a, Descendant: d}).IsAncestor
	}
	for _, c := range []struct {
		name, a, d string
		want       bool
	}{
		{"parent", c2, c3, true}, {"grandparent", c1, c3, true}, {"child is not an ancestor", c3, c1, false}, {"itself", c2, c2, true},
		{"diverged", d, c3, false}, {"diverged reversed", c3, d, false}, {"unrelated roots", r, c3, false}, {"common ancestor", c1, d, true},
	} {
		if got := is(c.a, c.d); got != c.want {
			t.Errorf("%s: is-ancestor(%s, %s) = %v, want %v", c.name, c.a[:7], c.d[:7], got, c.want)
		}
	}

	// A commit the repository lacks, an object that is not a commit (a tree, an
	// annotated tag), are commit_not_found whichever side they are on.
	missing := strings.Repeat("a", 40)
	for _, bad := range []string{missing, tree, tag} {
		rpcError(e, path, gitv1.IsAncestorRequest{RepositoryID: "repo_1", Ancestor: bad, Descendant: c3}, http.StatusNotFound, gitv1.CodeCommitNotFound)
		rpcError(e, path, gitv1.IsAncestorRequest{RepositoryID: "repo_1", Ancestor: c1, Descendant: bad}, http.StatusNotFound, gitv1.CodeCommitNotFound)
	}
	// Request refusals.
	rpcError(e, "/repositories/repo_x/objects/is-ancestor", gitv1.IsAncestorRequest{RepositoryID: "repo_x", Ancestor: c1, Descendant: c3}, http.StatusNotFound, domain.CodeNotFound)
	rpcError(e, "/repositories/repo_2/objects/is-ancestor", gitv1.IsAncestorRequest{RepositoryID: "repo_1", Ancestor: c1, Descendant: c3}, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, path, `{"repositoryId":"repo_1","ancestor":"--all","descendant":"`+c3+`"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, path, `{"repositoryId":"repo_1","ancestor":"HEAD","descendant":"`+c3+`"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, path, `{"repositoryId":"repo_1","ancestor":"`+c1+`","descendant":"`+c3+`","path":"/etc"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	if status, _ := e.rpcWith(http.MethodPost, path, gitv1.IsAncestorRequest{RepositoryID: "repo_1", Ancestor: c1, Descendant: c3}, ""); status != http.StatusUnauthorized {
		t.Errorf("an unauthenticated call: %d", status)
	}

	// A client can push refs/replace/<commit>. Git would then answer about the
	// replacement; the worker must not.
	e.seed("repo_1", w, r+":refs/replace/"+c3)
	if out, err := e.gitPlain(e.repoDir("repo_1"), "--git-dir="+e.repoDir("repo_1"), "merge-base", "--is-ancestor", c1, c3); err == nil {
		t.Fatalf("the replace ref has no effect on plain git (%s): the test proves nothing", out)
	}
	if !is(c1, c3) || is(r, c3) {
		t.Error("a pushed replace ref changed the ancestry answer")
	}
	if !e.hasCommit("repo_1", c3) {
		t.Error("a replaced commit is still present")
	}
}

// ---- HasCommit and CommitTime ----

// Test_unit_gitservice_has_commit_and_time: HasCommit is exact about the object
// type, CommitTime is the committer time (not the author time) in whole seconds,
// UTC, and is not affected by replacement objects.
func Test_unit_gitservice_has_commit_and_time(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.commitAt(w, "one", "1\n", "1700000000 +0000")
	c2 := e.commitAt(w, "two", "2\n", "2023-11-15T09:00:00+02:00") // 07:00:00Z
	e.mustPlain(w, "tag", "-a", "-m", "t", "v1", c1)
	tag := e.mustPlain(w, "rev-parse", "refs/tags/v1")
	tree := e.mustPlain(w, "rev-parse", c2+"^{tree}")
	blob := e.mustPlain(w, "rev-parse", c2+":two")
	e.seed("repo_1", w, "main:refs/heads/main", "refs/tags/v1:refs/tags/v1")

	for _, c := range []struct {
		name, id string
		want     bool
	}{{"commit", c1, true}, {"second commit", c2, true}, {"missing", strings.Repeat("b", 40), false}, {"tree", tree, false}, {"blob", blob, false}, {"annotated tag object", tag, false}} {
		if got := e.hasCommit("repo_1", c.id); got != c.want {
			t.Errorf("has-commit(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	hasPath := "/repositories/repo_1/objects/has-commit"
	rpcError(e, hasPath, `{"repositoryId":"repo_1","commit":"`+strings.Repeat("0", 40)+`"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, hasPath, `{"repositoryId":"repo_1","commit":"main"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, "/repositories/repo_x/objects/has-commit", gitv1.HasCommitRequest{RepositoryID: "repo_x", Commit: c1}, http.StatusNotFound, domain.CodeNotFound)
	rpcError(e, "/repositories/repo_2/objects/has-commit", gitv1.HasCommitRequest{RepositoryID: "repo_1", Commit: c1}, http.StatusBadRequest, domain.CodeInvalidRequest)

	const path = "/repositories/repo_1/objects/commit-time"
	at := func(c string) gitv1.Timestamp {
		t.Helper()
		return rpcOK[gitv1.CommitTimeResponse](e, path, gitv1.CommitTimeRequest{RepositoryID: "repo_1", Commit: c}).CommittedAt
	}
	if got := at(c1); got != "2023-11-14T22:13:20Z" {
		t.Errorf("commit time of c1: %s", got)
	}
	if got := at(c2); got != "2023-11-15T07:00:00Z" {
		t.Errorf("commit time of c2 (committer +02:00): %s", got)
	}
	for _, bad := range []string{strings.Repeat("c", 40), tree, tag} {
		rpcError(e, path, gitv1.CommitTimeRequest{RepositoryID: "repo_1", Commit: bad}, http.StatusNotFound, gitv1.CodeCommitNotFound)
	}
	rpcError(e, "/repositories/repo_x/objects/commit-time", gitv1.CommitTimeRequest{RepositoryID: "repo_x", Commit: c1}, http.StatusNotFound, domain.CodeNotFound)

	// A replacement object does not change the time: replace c1 by a commit of
	// another time.
	e.mustPlain(w, "checkout", "-q", "--orphan", "late")
	late := e.commitAt(w, "late", "l\n", "1800000000 +0000")
	e.seed("repo_1", w, "late:refs/heads/late", late+":refs/replace/"+c1)
	if got := at(c1); got != "2023-11-14T22:13:20Z" {
		t.Errorf("a replace ref changed the commit time: %s", got)
	}

	// A far-future committer time (Git itself refuses to author one, a crafted
	// commit can carry it) is clamped to a valid contract timestamp, the safe
	// direction for age-based pruning.
	for ts, want := range map[string]gitv1.Timestamp{"253402300800": "9999-12-31T23:59:59Z", "0": "1970-01-01T00:00:00Z"} {
		future := e.craftCommit("repo_1", ts)
		got := at(future)
		if got != want {
			t.Errorf("commit time %s: %s, want %s", ts, got, want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("the answer is not a contract timestamp: %v", err)
		}
	}
}

// craftCommit writes a commit with the committer timestamp straight into the
// repository's object store (without fsck) and returns its ID.
func (e *testEnv) craftCommit(repo, timestamp string) string {
	e.t.Helper()
	dir := e.repoDir(repo)
	body := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@example.invalid> 1600000000 +0000\ncommitter A <a@example.invalid> " + timestamp + " +0000\n\ncrafted " + timestamp + "\n"
	cmd := exec.Command("git", "--git-dir="+dir, "hash-object", "-w", "-t", "commit", "--literally", "--stdin")
	cmd.Env = e.clientEnv()
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("hash-object: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---- CreateBundle ----

// Test_unit_gitservice_bundle_create_under_shared_gate: a bundle holds exactly
// the heads, tags and hidden refs and names them in its manifest with the
// journal head at that moment; it runs under the shared gate (it excludes
// maintenance and the restore replay, never a receive or a ref operation) and
// says it was not taken at one exact boundary when a mutation overlapped.
func Test_unit_gitservice_bundle_create_under_shared_gate(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) { c.Limits.MaxConcurrentBundles = 1 })
	e.initRepo("repo_1", "main")

	// A repository without refs has nothing to bundle.
	status, _, _, body := e.createBundleRaw("repo_1", nil)
	if status != http.StatusConflict || !strings.Contains(string(body), string(gitv1.CodeRepositoryEmpty)) {
		t.Fatalf("an empty repository: %d %s", status, body)
	}

	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	c2 := e.commit(w, "two", "2\n")
	e.mustPlain(w, "tag", "-a", "-m", "t", "v1", c2)
	tag := e.mustPlain(w, "rev-parse", "refs/tags/v1")
	e.mustPlain(w, "update-ref", "refs/notes/x", c1)
	e.mustPlain(w, "update-ref", "refs/kodama/runs/run_1/1", c1)
	e.seed("repo_1", w, "main:refs/heads/main", "refs/tags/v1:refs/tags/v1", "refs/notes/x:refs/notes/x")
	e.setHidden("repo_1", "refs/kodama/runs/run_1/1", c1)
	if created := rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: "refs/heads/kodama/work/wi_1", Commit: c1}); !created.Created {
		t.Fatal("setup")
	}
	head := e.journalHead("repo_1")
	if head != 2 {
		t.Fatalf("journal head: %d", head)
	}

	m, bundle := e.createBundle("repo_1", nil)
	want := map[string]string{
		"refs/heads/main": c2, "refs/heads/kodama/work/wi_1": c1, "refs/tags/v1": tag, "refs/kodama/runs/run_1/1": c1,
	}
	if got := refMap(m.Refs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("manifest refs: %v, want %v", got, want)
	}
	if !sort.SliceIsSorted(m.Refs, func(i, j int) bool { return m.Refs[i].Ref < m.Refs[j].Ref }) {
		t.Errorf("manifest refs are not sorted: %v", m.Refs)
	}
	if m.JournalHead != head || !m.Consistent || m.WorkerInstance != e.svc.WorkerInstance() || m.RepositoryID != "repo_1" {
		t.Errorf("manifest: %+v", m)
	}
	// The bytes are a real bundle holding exactly the manifest's refs (not HEAD,
	// not refs/notes), and it restores into a new repository.
	listed := e.bundleRefs(bundle)
	if strings.Contains(listed, "refs/notes/x") || strings.Contains(listed, "HEAD") || strings.Count(listed, "\n") != len(want)-1 {
		t.Errorf("bundle heads:\n%s", listed)
	}
	f := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(f, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	clone := t.TempDir()
	e.mustPlain(clone, "init", "-q", "--bare", ".")
	e.mustPlain(clone, "fetch", "-q", f, "refs/heads/main:refs/heads/main", "refs/tags/v1:refs/tags/v1")
	if got := e.mustPlain(clone, "rev-parse", "refs/heads/main"); got != c2 {
		t.Errorf("fetched main: %s", got)
	}

	// Explicit refs: exactly those; a missing ref is not_found; refs outside the
	// bundle namespaces are refused.
	m2, _ := e.createBundle("repo_1", []string{"refs/tags/v1", "refs/heads/main"})
	if got := refMap(m2.Refs); len(got) != 2 || got["refs/heads/main"] != c2 || got["refs/tags/v1"] != tag {
		t.Errorf("explicit refs: %+v", m2.Refs)
	}
	if status, _, _, body := e.createBundleRaw("repo_1", []string{"refs/heads/main", "refs/heads/nope"}); status != http.StatusNotFound || !strings.Contains(string(body), string(domain.CodeNotFound)) {
		t.Errorf("a missing ref: %d %s", status, body)
	}
	if status, _, _, body := e.createBundleRaw("repo_1", []string{"refs/notes/x"}); status != http.StatusBadRequest || !strings.Contains(string(body), string(domain.CodeInvalidRequest)) {
		t.Errorf("a ref outside the namespaces: %d %s", status, body)
	}
	if status, _, _, _ := e.createBundleRaw("repo_x", nil); status != http.StatusNotFound {
		t.Errorf("an unknown repository: %d", status)
	}

	// The shared gate. A bundle in progress is held by the hook; meanwhile a ref
	// operation (a shared holder) is admitted, maintenance (exclusive) waits for
	// the bundle, and a second bundle is refused (the concurrency bound).
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(ctx context.Context, _ string) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	e.svc.bundleHook.Store(&hook)
	type result struct {
		status int
		m      gitv1.CreateBundleResponse
	}
	bundled := make(chan result, 1)
	go func() {
		s, m, _, _ := e.createBundleRaw("repo_1", nil)
		bundled <- result{s, m}
	}()
	select {
	case <-entered:
	case res := <-bundled:
		t.Fatalf("the bundle finished without reaching the hook: %d", res.status)
	case <-time.After(30 * time.Second):
		t.Fatal("the bundle never reached the hook")
	}
	if status, _, _, body := e.createBundleOnce("repo_1", nil); status != http.StatusTooManyRequests || !strings.Contains(string(body), string(domain.CodeRateLimited)) {
		t.Errorf("a second concurrent bundle: %d %s", status, body)
	}
	rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_2", Ref: "refs/heads/kodama/work/wi_2", Commit: c2})
	maintained := make(chan error, 1)
	go func() {
		status, data := e.rpc("/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_m"})
		if status != http.StatusOK {
			maintained <- fmt.Errorf("maintenance: %d %s", status, data)
			return
		}
		maintained <- nil
	}()
	select {
	case err := <-maintained:
		t.Fatalf("maintenance ran while a bundle was being created: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	res := <-bundled
	if res.status != http.StatusOK {
		t.Fatalf("the bundle in progress: %d", res.status)
	}
	if res.m.Consistent {
		t.Error("a bundle overlapped by a ref operation says it is one exact boundary")
	}
	if res.m.JournalHead != head {
		t.Errorf("the manifest's journal head is the one at capture time: %d, want %d", res.m.JournalHead, head)
	}
	if err := <-maintained; err != nil {
		t.Fatal(err)
	}
	e.svc.bundleHook.Store(nil)

	// Quiescent again: the next bundle is one exact boundary at the new head.
	m3, _ := e.createBundle("repo_1", nil)
	if !m3.Consistent || m3.JournalHead != e.journalHead("repo_1") || m3.JournalHead != head+2 || refMap(m3.Refs)["refs/heads/kodama/work/wi_2"] != c2 {
		t.Errorf("manifest after the operation: %+v", m3)
	}
	// A mutation holder already running when the bundle starts (a receive in
	// flight) makes it inconsistent too.
	g := e.svc.gates.of("repo_1")
	rel, err := g.acquireShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m4, _ := e.createBundle("repo_1", nil)
	rel()
	if m4.Consistent {
		t.Error("a bundle taken while a receive was running says it is one exact boundary")
	}
	// Bundles are admitted during a restore fence (they only read).
	e.fence("repo_1", true)
	if m5, _ := e.createBundle("repo_1", nil); !m5.Consistent {
		t.Errorf("a bundle of a fenced repository: %+v", m5)
	}
	e.fence("repo_1", false)

	// Bounded size: nothing is left behind.
	small := newTestEnv(t, func(c *Config) { c.Limits.MaxBundleBytes = 200 })
	small.initRepo("repo_1", "main")
	w2 := small.workdir()
	small.seed("repo_1", w2, "main:refs/heads/main")
	status, _, _, body = small.createBundleRaw("repo_1", nil)
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(body), string(gitv1.CodeBundleTooLarge)) {
		t.Errorf("an oversized bundle: %d %s", status, body)
	}
	if entries, _ := os.ReadDir(small.svc.layout.tmp); len(entries) != 0 {
		t.Errorf("a refused bundle left %d files in the temporary directory", len(entries))
	}
}

// ---- ReplayBundle and the restore fence ----

// Test_unit_gitservice_bundle_replay_only_while_fenced: a replay is refused
// unless the repository is fenced for restore; while fenced the worker refuses
// receives, ref operations and maintenance (initialization and reads stay);
// the replay creates the refs through the worker's hooks (a real repository
// with core.hooksPath accepts it and the journal records it), is idempotent by
// operation ID and verifies the digest. The fence outlives a worker restart and
// the entry waits for running holders.
func Test_unit_gitservice_bundle_replay_only_while_fenced(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_a", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	c2 := e.commit(w, "two", "2\n")
	e.mustPlain(w, "tag", "-a", "-m", "t", "v1", c2)
	tag := e.mustPlain(w, "rev-parse", "refs/tags/v1")
	e.mustPlain(w, "update-ref", "refs/kodama/runs/run_1/1", c1)
	e.seed("repo_a", w, "main:refs/heads/main", "refs/tags/v1:refs/tags/v1")
	e.setHidden("repo_a", "refs/kodama/runs/run_1/1", c1)
	manifest, bundle := e.createBundle("repo_a", nil)
	sum := sha256Hex(bundle)

	// The target is a repository the worker initialized (real hooks, hooks path
	// in its config).
	e.initRepo("repo_b", "main")
	cfg, err := os.ReadFile(filepath.Join(e.repoDir("repo_b"), "config"))
	if err != nil || !strings.Contains(string(cfg), "hooksPath") {
		t.Fatalf("the target repository has no hooks path: %v\n%s", err, cfg)
	}
	e.replayRefused("repo_b", "op_r1", &sum, bundle, http.StatusConflict, gitv1.CodeRestoreFenceRequired)
	if e.journalHead("repo_b") != 0 {
		t.Fatal("a refused replay journaled")
	}

	// Enter the fence: idempotent, durable.
	if r := e.fence("repo_b", true); !r.Fenced || !r.Changed || r.RepositoryID != "repo_b" || r.WorkerInstance != e.svc.WorkerInstance() {
		t.Errorf("enter: %+v", r)
	}
	if r := e.fence("repo_b", true); !r.Fenced || r.Changed {
		t.Errorf("enter again: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.root, "restore-fences", "repo_b")); err != nil {
		t.Errorf("the fence has no durable marker: %v", err)
	}
	// What a fenced repository refuses and admits.
	rpcError(e, "/repositories/repo_b/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_b", OperationID: "op_x", Ref: "refs/heads/kodama/work/wi_1", Commit: c1}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	rpcError(e, "/repositories/repo_b/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_b", OperationID: "op_m"}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	tok := e.token("repo_b", true)
	if out, err := e.git(w, tok, "push", "-q", e.url("repo_b"), "main:refs/heads/pushed"); err == nil {
		t.Fatalf("a receive was admitted into a fenced repository:\n%s", out)
	}
	if e.bareRef("repo_b", "refs/heads/pushed") != "" {
		t.Fatal("a refused receive changed a ref")
	}
	if got := e.initRepo("repo_b", "main"); got.Created {
		t.Errorf("initialization during a restore: %+v", got)
	}
	// An unfenced repository refuses nothing of this kind, and the fence is
	// per repository.
	if got := e.initRepo("repo_c", "main"); !got.Created {
		t.Errorf("another repository: %+v", got)
	}

	// Refusals before anything is applied.
	wrong := strings.Repeat("0", 64)
	e.replayRefused("repo_b", "op_r1", &wrong, bundle, http.StatusBadRequest, domain.CodeInvalidRequest)
	e.replayRefused("repo_b", "op_r1", &sum, []byte("not a bundle"), http.StatusBadRequest, domain.CodeInvalidRequest)
	e.replayRefused("repo_b", "op_r1", nil, nil, http.StatusBadRequest, domain.CodeInvalidRequest)
	if status, data, err := e.replayRaw("repo_b", "op_r1", &sum, bundle, "application/json"); err != nil || status != http.StatusBadRequest {
		t.Errorf("a body of the wrong media type: %d %v %s", status, err, data)
	}
	if status, data, err := e.replayRaw("repo_x", "op_r1", &sum, bundle, gitv1.BundleMediaType); err != nil || status != http.StatusNotFound {
		t.Errorf("an unknown repository: %d %v %s", status, err, data)
	}
	// A bundle with a ref outside the namespaces, and a bundle with a missing
	// prerequisite.
	e.mustPlain(w, "update-ref", "refs/notes/x", c1)
	notes := filepath.Join(t.TempDir(), "notes.bundle")
	e.mustPlain(w, "bundle", "create", notes, "refs/notes/x")
	notesBytes, _ := os.ReadFile(notes)
	e.replayRefused("repo_b", "op_r1", nil, notesBytes, http.StatusBadRequest, domain.CodeInvalidRequest)
	incremental := filepath.Join(t.TempDir(), "inc.bundle")
	e.mustPlain(w, "bundle", "create", incremental, "refs/heads/main", "^"+c1)
	incBytes, _ := os.ReadFile(incremental)
	e.replayRefused("repo_b", "op_r1", nil, incBytes, http.StatusBadRequest, domain.CodeInvalidRequest)
	if e.hasCommit("repo_b", c2) {
		t.Error("a refused replay left a commit reachable by the API")
	}

	// The replay.
	got := e.replay("repo_b", "op_r1", bundle)
	if got.OperationID != "op_r1" || got.Created != int64(len(manifest.Refs)) || got.Unchanged != 0 || len(got.Conflicting) != 0 || got.WorkerInstance != e.svc.WorkerInstance() {
		t.Fatalf("replay: %+v", got)
	}
	for _, tip := range manifest.Refs {
		if e.bareRef("repo_b", tip.Ref) != tip.Commit {
			t.Errorf("%s is %q, want %s", tip.Ref, e.bareRef("repo_b", tip.Ref), tip.Commit)
		}
	}
	if !e.hasCommit("repo_b", c2) || e.bareRef("repo_b", "refs/tags/v1") != tag {
		t.Error("the objects were not restored")
	}
	// Created through the worker's hooks: every ref has prepared and committed
	// entries attributed to the operation.
	entries := e.journalAll("repo_b")
	if len(entries) != 2*len(manifest.Refs) {
		t.Fatalf("journal entries: %d, want %d", len(entries), 2*len(manifest.Refs))
	}
	for _, tip := range manifest.Refs {
		if st := e.branchStates("repo_b", tip.Ref); st != "[prepared committed]" {
			t.Errorf("%s journal: %s", tip.Ref, st)
		}
	}
	for _, en := range entries {
		if en.OperationID == nil || *en.OperationID != "op_r1" || en.ReceiveID != nil {
			t.Errorf("entry attribution: %+v", en)
		}
	}
	// Idempotent by operation ID: the same call finds everything unchanged and
	// journals nothing.
	if again := e.replay("repo_b", "op_r1", bundle); again.Created != 0 || again.Unchanged != int64(len(manifest.Refs)) || len(again.Conflicting) != 0 {
		t.Errorf("repeat: %+v", again)
	}
	if n := len(e.journalAll("repo_b")); n != len(entries) {
		t.Errorf("a repeat journaled again: %d entries", n)
	}

	// Leave: idempotent; replay and the fence's refusals are reversed.
	if r := e.fence("repo_b", false); r.Fenced || !r.Changed {
		t.Errorf("leave: %+v", r)
	}
	if r := e.fence("repo_b", false); r.Fenced || r.Changed {
		t.Errorf("leave again: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.root, "restore-fences", "repo_b")); !os.IsNotExist(err) {
		t.Errorf("the marker outlived the fence: %v", err)
	}
	e.replayRefused("repo_b", "op_r2", &sum, bundle, http.StatusConflict, gitv1.CodeRestoreFenceRequired)
	rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_b/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_b", OperationID: "op_x", Ref: "refs/heads/kodama/work/wi_1", Commit: c1})
	e.mustGit(w, tok, "push", "-q", e.url("repo_b"), "main:refs/heads/pushed")
	if e.bareRef("repo_b", "refs/heads/pushed") != c2 {
		t.Error("a receive after the fence was left")
	}
	// A fence of a repository that does not exist yet (a restore raises it before
	// it initializes), and the path must name the body's repository.
	if r := e.fence("repo_new", true); !r.Changed {
		t.Errorf("fence of a repository that does not exist: %+v", r)
	}
	if e.initRepo("repo_new", "main").Created != true {
		t.Error("initialization into a fenced, new repository")
	}
	e.fence("repo_new", false)
	rpcError(e, "/repositories/repo_2/restore-fence", gitv1.RestoreFenceRequest{RepositoryID: "repo_1", Fenced: true}, http.StatusBadRequest, domain.CodeInvalidRequest)
}

// Test_unit_gitservice_restore_fence_races: entering the fence waits for the
// running receives and operations and refuses the ones that queued behind it;
// leaving waits for a replay; a fence entered before a restart is still raised
// after it.
func Test_unit_gitservice_restore_fence_races(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	m, bundle := e.createBundle("repo_1", nil)
	_ = m
	g := e.svc.gates.of("repo_1")

	// A receive is running (a shared holder): entering waits for it, and an
	// operation that queued behind the entry is refused once the fence is up.
	hold, err := g.acquireShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan gitv1.RestoreFenceResponse, 1)
	go func() {
		entered <- rpcOK[gitv1.RestoreFenceResponse](e, "/repositories/repo_1/restore-fence", gitv1.RestoreFenceRequest{RepositoryID: "repo_1", Fenced: true})
	}()
	eventually(t, 5*time.Second, "the fence entry to queue", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.waiting > 0
	})
	type answer struct {
		status int
		body   []byte
	}
	queued := make(chan answer, 1)
	go func() {
		s, b := e.rpc("/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_q", Ref: "refs/heads/kodama/work/wi_q", Commit: c1})
		queued <- answer{s, b}
	}()
	select {
	case r := <-entered:
		t.Fatalf("the fence was entered while a receive was running: %+v", r)
	case a := <-queued:
		t.Fatalf("an operation ran while the fence entry was waiting: %d %s", a.status, a.body)
	case <-time.After(300 * time.Millisecond):
	}
	hold()
	if r := <-entered; !r.Fenced || !r.Changed {
		t.Fatalf("enter: %+v", r)
	}
	if a := <-queued; a.status != http.StatusServiceUnavailable {
		t.Fatalf("an operation queued behind the fence entry: %d %s", a.status, a.body)
	}
	if e.bareRef("repo_1", "refs/heads/kodama/work/wi_q") != "" {
		t.Fatal("a queued operation changed a ref after the fence was entered")
	}
	if _, err := g.acquireShared(context.Background()); err == nil {
		t.Fatal("a receive was admitted into a fenced repository")
	}

	// Leaving waits for a running replay.
	rel, err := g.acquireReplay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	left := make(chan gitv1.RestoreFenceResponse, 1)
	go func() {
		left <- rpcOK[gitv1.RestoreFenceResponse](e, "/repositories/repo_1/restore-fence", gitv1.RestoreFenceRequest{RepositoryID: "repo_1", Fenced: false})
	}()
	select {
	case r := <-left:
		t.Fatalf("the fence was left while a replay was running: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if !g.isRestoring() {
		t.Fatal("the fence dropped before the replay ended")
	}
	rel()
	if r := <-left; r.Fenced || !r.Changed {
		t.Fatalf("leave: %+v", r)
	}
	// Replay holds the gate exclusively: a bundle replay cannot be admitted twice
	// at once, and is refused outright once the fence is left.
	if _, err := g.acquireReplay(context.Background()); err != errRestoreRequired {
		t.Fatalf("a replay outside the fence: %v", err)
	}

	// A fence outlives the worker: the replacement comes up closed.
	e.fence("repo_1", true)
	e.srv.Close()
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{
		TenantID: testTenant, DataRoot: e.root, ControlPlane: e.control, Assertions: e.verifier, Callers: fakeCallers{}, HookBinary: testHookBinary(t),
		Limits: Limits{GateWait: 3 * time.Second, ControlTimeout: 10 * time.Second, LockWait: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, e.srv = svc, httptest.NewServer(svc.Handler())
	rpcError(e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_r", Ref: "refs/heads/kodama/work/wi_r", Commit: c1}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	rpcError(e, "/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_m"}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	if r := e.fence("repo_1", true); r.Changed {
		t.Errorf("the restarted worker did not know the fence: %+v", r)
	}
	if got := e.replay("repo_1", "op_after_restart", bundle); got.Unchanged != int64(len(m.Refs)) {
		t.Errorf("replay after the restart: %+v", got)
	}
	if r := e.fence("repo_1", false); !r.Changed {
		t.Errorf("leave after the restart: %+v", r)
	}
	rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_r", Ref: "refs/heads/kodama/work/wi_r", Commit: c1})
	// A leave is durable too: another restart comes up open.
	e.srv.Close()
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc2, err := New(Config{
		TenantID: testTenant, DataRoot: e.root, ControlPlane: e.control, Assertions: e.verifier, Callers: fakeCallers{}, HookBinary: testHookBinary(t),
		Limits: Limits{GateWait: 3 * time.Second, ControlTimeout: 10 * time.Second, LockWait: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, e.srv = svc2, httptest.NewServer(svc2.Handler())
	rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_s", Ref: "refs/heads/kodama/work/wi_s", Commit: c1})
}

// Test_unit_gitservice_replay_never_rewinds: a replay creates the refs that
// are missing and nothing else. A ref at another commit is reported (with how it
// relates to the bundle) and left alone, a ref the bundle does not name is never
// deleted, the bundle's objects are stored either way, and a failure after the
// first ref is created is an unknown outcome whose identical retry completes.
func Test_unit_gitservice_replay_never_rewinds(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_b", "main")

	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	c2 := e.commit(w, "two", "2\n")
	c3 := e.commit(w, "three", "3\n")
	e.mustPlain(w, "checkout", "-q", "-b", "sidebranch", c1)
	d := e.commit(w, "side", "s\n")
	e.mustPlain(w, "checkout", "-q", "-b", "chain", c1)
	e.commit(w, "e1", "e1\n")
	e2 := e.commit(w, "e2", "e2\n")
	e.mustPlain(w, "checkout", "-q", "main")
	e.mustPlain(w, "tag", "-a", "-m", "t", "v1", c2)
	tag := e.mustPlain(w, "rev-parse", "refs/tags/v1")
	for ref, commit := range map[string]string{
		"refs/heads/new": c2, "refs/heads/same": c2, "refs/heads/ahead": c2, "refs/heads/behind": e2, "refs/heads/div": c2,
		"refs/heads/zlast": c1, "refs/kodama/runs/run_1/1": c1,
	} {
		e.mustPlain(w, "update-ref", ref, commit)
	}
	bundleFile := filepath.Join(t.TempDir(), "restore.bundle")
	e.mustPlain(w, "bundle", "create", bundleFile, "refs/heads/new", "refs/heads/same", "refs/heads/ahead", "refs/heads/behind", "refs/heads/div",
		"refs/heads/zlast", "refs/tags/v1", "refs/kodama/runs/run_1/1")
	bundle, err := os.ReadFile(bundleFile)
	if err != nil {
		t.Fatal(err)
	}
	// What the repository holds: newer, older, divergent and equal values, and a
	// ref the bundle does not mention.
	e.seed("repo_b", w, c3+":refs/heads/ahead", c1+":refs/heads/behind", d+":refs/heads/div", c2+":refs/heads/same", c3+":refs/heads/extra")
	if e.hasCommit("repo_b", e2) {
		t.Fatal("setup: the repository already has the bundle's objects")
	}
	e.fence("repo_b", true)
	sum := sha256Hex(bundle)
	kept := map[string]string{"refs/heads/ahead": c3, "refs/heads/behind": c1, "refs/heads/div": d, "refs/heads/same": c2, "refs/heads/extra": c3}
	checkKept := func(when string) {
		t.Helper()
		for ref, commit := range kept {
			if got := e.bareRef("repo_b", ref); got != commit {
				t.Errorf("%s: %s is %q, want %s (a replay must never advance, rewind or delete)", when, ref, got, commit)
			}
		}
	}

	// The first attempt fails on its last created head (a lock another process
	// holds): it is an unknown outcome (the response is aborted), and the refs
	// created before are kept.
	lock := filepath.Join(e.repoDir("repo_b"), "refs", "heads", "zlast.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	status, data, err := e.replayRaw("repo_b", "op_r", &sum, bundle, gitv1.BundleMediaType)
	if err == nil {
		t.Fatalf("a replay that failed after it changed a ref answered %d %s, want a transport failure", status, data)
	}
	if e.bareRef("repo_b", "refs/heads/new") != c2 {
		t.Fatal("the ref created before the failure was lost")
	}
	if e.bareRef("repo_b", "refs/heads/zlast") != "" || e.bareRef("repo_b", "refs/tags/v1") != "" {
		t.Fatal("the replay went past its failure")
	}
	checkKept("after the failed attempt")

	// The identical request converges.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	got := e.replay("repo_b", "op_r", bundle)
	wantConflicts := []gitv1.BundleConflict{
		{Ref: "refs/heads/ahead", Current: c3, Bundle: c2, Relation: gitv1.RelationRepositoryAhead},
		{Ref: "refs/heads/behind", Current: c1, Bundle: e2, Relation: gitv1.RelationBundleAhead},
		{Ref: "refs/heads/div", Current: d, Bundle: c2, Relation: gitv1.RelationDiverged},
	}
	if got.Created != 3 || got.Unchanged != 2 || fmt.Sprint(got.Conflicting) != fmt.Sprint(wantConflicts) {
		t.Fatalf("replay: %+v\nwant 3 created (zlast, v1, run), 2 unchanged (new, same) and %v", got, wantConflicts)
	}
	for ref, commit := range map[string]string{"refs/heads/new": c2, "refs/heads/zlast": c1, "refs/tags/v1": tag, "refs/kodama/runs/run_1/1": c1} {
		if e.bareRef("repo_b", ref) != commit {
			t.Errorf("%s is %q, want %s", ref, e.bareRef("repo_b", ref), commit)
		}
	}
	checkKept("after the replay")
	// The objects came in even for the refs that were not touched.
	if !e.hasCommit("repo_b", e2) || !e.hasCommit("repo_b", c1) {
		t.Error("the bundle's objects were not stored")
	}
	// Only the created refs are journaled, each closed committed; nothing for a
	// conflicting or untouched ref.
	journaled := map[string]string{}
	for _, en := range e.journalAll("repo_b") {
		journaled[en.Ref] += string(en.State[0])
		if en.OperationID == nil || *en.OperationID != "op_r" {
			t.Errorf("entry attribution: %+v", en)
		}
	}
	for _, ref := range []string{"refs/heads/new", "refs/heads/zlast", "refs/tags/v1", "refs/kodama/runs/run_1/1"} {
		if !strings.HasSuffix(journaled[ref], "pc") {
			t.Errorf("%s journal: %q", ref, journaled[ref])
		}
	}
	for ref := range kept {
		if journaled[ref] != "" {
			t.Errorf("a ref the replay did not change was journaled: %s %q", ref, journaled[ref])
		}
	}

	// Repeats are stable: the same conflicts, nothing created, nothing journaled.
	headBefore := e.journalHead("repo_b")
	again := e.replay("repo_b", "op_r", bundle)
	if again.Created != 0 || again.Unchanged != 5 || fmt.Sprint(again.Conflicting) != fmt.Sprint(wantConflicts) {
		t.Errorf("repeat: %+v", again)
	}
	if e.journalHead("repo_b") != headBefore {
		t.Error("a repeat journaled")
	}
	// A different operation ID over the same state is the same replay.
	if other := e.replay("repo_b", "op_other", bundle); other.Created != 0 || fmt.Sprint(other.Conflicting) != fmt.Sprint(wantConflicts) {
		t.Errorf("another operation: %+v", other)
	}
	checkKept("after the repeats")

	// A bundle taken from the worker restores into a fresh repository with
	// nothing conflicting (the backup/restore path).
	m, fromWorker := e.createBundle("repo_b", nil)
	e.initRepo("repo_d", "main")
	e.fence("repo_d", true)
	if r := e.replay("repo_d", "op_d", fromWorker); r.Created != int64(len(m.Refs)) || len(r.Conflicting) != 0 {
		t.Errorf("restore into a fresh repository: %+v", r)
	}
	for _, tip := range m.Refs {
		if e.bareRef("repo_d", tip.Ref) != tip.Commit {
			t.Errorf("repo_d %s is %q, want %s", tip.Ref, e.bareRef("repo_d", tip.Ref), tip.Commit)
		}
	}
}

// ---- review follow-ups ----

func (e *testEnv) workerConfig() Config {
	return Config{
		TenantID: testTenant, DataRoot: e.root, ControlPlane: e.control, Assertions: e.verifier, Callers: fakeCallers{}, HookBinary: testHookBinary(e.t),
		Limits: Limits{GateWait: 3 * time.Second, ControlTimeout: 10 * time.Second, LockWait: 5 * time.Second},
	}
}

// stop releases the worker (the generation lock included).
func (e *testEnv) stop() {
	e.srv.Close()
	if err := e.svc.Close(); err != nil {
		e.t.Fatal(err)
	}
}

// start runs a replacement worker on the same data root.
func (e *testEnv) start() {
	e.t.Helper()
	svc, err := New(e.workerConfig())
	if err != nil {
		e.t.Fatal(err)
	}
	e.svc, e.srv = svc, httptest.NewServer(svc.Handler())
}

func (e *testEnv) bundleOf(dir string, refs ...string) []byte {
	e.t.Helper()
	f := filepath.Join(e.t.TempDir(), "b.bundle")
	e.mustPlain(dir, append([]string{"bundle", "create", f}, refs...)...)
	b, err := os.ReadFile(f)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// writeRefFile points a ref of a working repository at any object, which
// git update-ref refuses for a branch.
func writeRefFile(t *testing.T, workdir, ref, oid string) {
	t.Helper()
	path := filepath.Join(workdir, ".git", filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(oid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Test_unit_gitservice_restore_fence_fails_closed: the fence's durable state is
// authoritative. A marker directory that cannot be read stops the worker from
// starting, a marker that is not a regular file still fences, a fence that
// cannot be persisted does not change, a stale marker is removed by a leave, and
// a leave of a repository that is not fenced never stalls receives.
func Test_unit_gitservice_restore_fence_fails_closed(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not bind root")
	}
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	fences := e.svc.layout.fences
	marker := filepath.Join(fences, "repo_1")
	create := func(op string) (int, []byte) {
		return e.rpc("/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: op, Ref: "refs/heads/kodama/work/" + op, Commit: c1})
	}

	// A fence that cannot be persisted: the entry fails and nothing changed.
	if err := os.Chmod(fences, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fences, 0o700) })
	status, body := e.rpc("/repositories/repo_1/restore-fence", gitv1.RestoreFenceRequest{RepositoryID: "repo_1", Fenced: true})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("an entry that could not be persisted: %d %s", status, body)
	}
	if e.svc.gates.of("repo_1").isRestoring() {
		t.Fatal("the flag was set although the marker could not be written")
	}
	if status, body := create("wi_1"); status != http.StatusOK {
		t.Fatalf("the repository was refused after a failed entry: %d %s", status, body)
	}
	if err := os.Chmod(fences, 0o700); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Fatal("a failed entry left a marker")
	}

	// A stale marker without the flag (what a failed entry could leave): a leave
	// removes it and reports no change.
	if err := os.WriteFile(marker, []byte("restoring\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.fence("repo_1", false); r.Fenced || r.Changed {
		t.Errorf("leave with a stale marker: %+v", r)
	}
	if exists(marker) {
		t.Error("the stale marker survived the leave")
	}
	// A leave that has nothing to do does not take the gate: it answers while a
	// receive is running.
	hold, err := e.svc.gates.of("repo_1").acquireShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan gitv1.RestoreFenceResponse, 1)
	go func() { done <- e.fence("repo_1", false) }()
	select {
	case r := <-done:
		if r.Changed {
			t.Errorf("a no-op leave: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Error("a no-op leave waited for the running receive")
	}
	hold()

	// A marker that is not a regular file still fences after a restart; the
	// leave removes it.
	e.stop()
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	e.start()
	if status, body := create("wi_2"); status != http.StatusServiceUnavailable {
		t.Fatalf("a non-regular marker did not fence: %d %s", status, body)
	}
	if r := e.fence("repo_1", false); !r.Changed || exists(marker) {
		t.Errorf("leave: %+v, marker present: %v", r, exists(marker))
	}
	if status, body := create("wi_2"); status != http.StatusOK {
		t.Fatalf("after the leave: %d %s", status, body)
	}

	// The marker directory cannot be read: the worker does not start (it would
	// not know what is fenced).
	e.stop()
	if err := os.Chmod(fences, 0); err != nil {
		t.Fatal(err)
	}
	if svc, err := New(e.workerConfig()); err == nil {
		_ = svc.Close()
		t.Fatal("a worker started with an unreadable restore-fence directory")
	} else if !strings.Contains(err.Error(), "restore fences") {
		t.Errorf("start failure: %v", err)
	}
	if err := os.Chmod(fences, 0o700); err != nil {
		t.Fatal(err)
	}
	e.start()
	if status, body := create("wi_3"); status != http.StatusOK {
		t.Fatalf("after the directory was readable again: %d %s", status, body)
	}
}

// Test_unit_gitservice_bundle_create_is_definite_about_what_it_holds: a ref the
// contract cannot name is a definite refusal (a backup never silently loses a
// ref), and a bundle during which a killed Git process fenced the repository is
// not reported as one exact boundary. A bundle is consistent whatever the
// restore state: consistency is about concurrent mutation.
func Test_unit_gitservice_bundle_create_is_definite_about_what_it_holds(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	dir := e.repoDir("repo_1")
	update := func(args ...string) {
		e.mustPlain(dir, append([]string{"-c", "core.hooksPath=" + os.DevNull, "--git-dir=" + dir}, args...)...)
	}

	update("update-ref", "refs/heads/a+b", c1)
	status, _, _, body := e.createBundleRaw("repo_1", nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), string(domain.CodeInvalidRequest)) || !strings.Contains(string(body), "a+b") {
		t.Fatalf("a repository with a ref outside the ref syntax: %d %s", status, body)
	}
	// Naming the refs explicitly still works; the odd ref is not named.
	if m, _ := e.createBundle("repo_1", []string{"refs/heads/main"}); !m.Consistent || len(m.Refs) != 1 {
		t.Errorf("explicit bundle: %+v", m)
	}
	update("update-ref", "-d", "refs/heads/a+b")

	g := e.svc.gates.of("repo_1")
	const pgid = 424242
	hook := func(context.Context, string) { g.fence(pgid, "") }
	e.svc.bundleHook.Store(&hook)
	m, _ := e.createBundle("repo_1", nil)
	e.svc.bundleHook.Store(nil)
	if m.Consistent {
		t.Error("a bundle during which the repository was fenced for a killed Git says it is one exact boundary")
	}
	if status, _, _, body := e.createBundleRaw("repo_1", nil); status != http.StatusServiceUnavailable {
		t.Errorf("a bundle of a repository fenced for a killed Git: %d %s", status, body)
	}
	g.unfence(pgid)
	if m, _ := e.createBundle("repo_1", nil); !m.Consistent {
		t.Errorf("after the fence: %+v", m)
	}
	// The restore fence does not make a bundle inconsistent.
	e.fence("repo_1", true)
	if m, _ := e.createBundle("repo_1", nil); !m.Consistent {
		t.Errorf("a bundle while the restore fence is raised: %+v", m)
	}
	e.fence("repo_1", false)
}

// Test_unit_gitservice_replay_refuses_deterministically_before_changing: what
// Git would refuse on every attempt is a definite invalid_request before the
// first ref exists (never an unknown outcome every retry repeats): a head whose
// tip is not a commit, a ref that collides with another as directory and file
// (in the repository or in the bundle), and a pack that strict object checking
// rejects. No ref changes, and a valid replay still goes through afterwards.
func Test_unit_gitservice_replay_refuses_deterministically_before_changing(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_b", "main")
	w1 := e.workdir()
	c1 := e.mustPlain(w1, "rev-parse", "main")
	e.seed("repo_b", w1, c1+":refs/heads/a", c1+":refs/heads/x/y", c1+":refs/heads/main")
	e.fence("repo_b", true)

	w2 := e.workdir()
	d := e.mustPlain(w2, "rev-parse", "main")
	tree := e.mustPlain(w2, "rev-parse", "HEAD^{tree}")
	e.mustPlain(w2, "update-ref", "refs/heads/good", d)

	// A directory/file collision with a ref of the repository, both ways.
	e.mustPlain(w2, "update-ref", "refs/heads/a/b", d)
	e.replayRefused("repo_b", "op_df1", nil, e.bundleOf(w2, "refs/heads/good", "refs/heads/a/b"), http.StatusBadRequest, domain.CodeInvalidRequest)
	e.mustPlain(w2, "update-ref", "-d", "refs/heads/a/b")
	e.mustPlain(w2, "update-ref", "refs/heads/x", d)
	e.replayRefused("repo_b", "op_df2", nil, e.bundleOf(w2, "refs/heads/good", "refs/heads/x"), http.StatusBadRequest, domain.CodeInvalidRequest)
	e.mustPlain(w2, "update-ref", "-d", "refs/heads/x")
	// The message names the collision.
	status, data, err := e.replayRaw("repo_b", "op_df3", nil, func() []byte {
		e.mustPlain(w2, "update-ref", "refs/heads/a/b", d)
		defer e.mustPlain(w2, "update-ref", "-d", "refs/heads/a/b")
		return e.bundleOf(w2, "refs/heads/a/b")
	}(), gitv1.BundleMediaType)
	if err != nil || status != http.StatusBadRequest || !strings.Contains(string(data), "collides") {
		t.Errorf("collision answer: %d %v %s", status, err, data)
	}

	// A head that would point to a tree.
	writeRefFile(t, w2, "refs/heads/treehead", tree)
	e.replayRefused("repo_b", "op_tree", nil, e.bundleOf(w2, "refs/heads/good", "refs/heads/treehead"), http.StatusBadRequest, domain.CodeInvalidRequest)
	if err := os.Remove(filepath.Join(w2, ".git", "refs", "heads", "treehead")); err != nil {
		t.Fatal(err)
	}

	// A pack with an object that strict checking rejects (a malformed author).
	cmd := exec.Command("git", "hash-object", "-w", "-t", "commit", "--literally", "--stdin")
	cmd.Dir = w2
	cmd.Env = e.clientEnv()
	cmd.Stdin = strings.NewReader("tree " + tree + "\nauthor bad\ncommitter A <a@example.invalid> 1600000000 +0000\n\nbad\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object: %v\n%s", err, out)
	}
	writeRefFile(t, w2, "refs/heads/bad", strings.TrimSpace(string(out)))
	badBundle := e.bundleOf(w2, "refs/heads/good", "refs/heads/bad")
	scratch := t.TempDir()
	e.mustPlain(scratch, "init", "-q", "--bare", ".")
	badFile := filepath.Join(t.TempDir(), "bad.bundle")
	if err := os.WriteFile(badFile, badBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustPlain(scratch, "bundle", "unbundle", badFile) // plain Git takes it: only strict checking rejects it
	strict := t.TempDir()
	e.mustPlain(strict, "init", "-q", "--bare", ".")
	packStart := bytes.Index(badBundle, []byte("\n\nPACK")) + 2
	cmd2 := exec.Command("git", "index-pack", "--stdin", "--strict")
	cmd2.Dir = strict
	cmd2.Env = e.clientEnv()
	cmd2.Stdin = bytes.NewReader(badBundle[packStart:])
	if out, err := cmd2.CombinedOutput(); err == nil {
		t.Fatalf("the test object is not rejected by strict checking (%s): the test proves nothing", out)
	}
	e.replayRefused("repo_b", "op_fsck", nil, badBundle, http.StatusBadRequest, domain.CodeInvalidRequest)
	if e.bareRef("repo_b", "refs/heads/good") != "" {
		t.Error("a ref was created by a refused replay")
	}
	if n := len(e.journalAll("repo_b")); n != 0 {
		t.Errorf("a refused replay journaled %d entries", n)
	}

	// Nothing is wedged: the valid part goes through.
	if err := os.Remove(filepath.Join(w2, ".git", "refs", "heads", "bad")); err != nil {
		t.Fatal(err)
	}
	if got := e.replay("repo_b", "op_ok", e.bundleOf(w2, "refs/heads/good")); got.Created != 1 || len(got.Conflicting) != 0 || e.bareRef("repo_b", "refs/heads/good") != d {
		t.Errorf("valid replay: %+v", got)
	}
}
