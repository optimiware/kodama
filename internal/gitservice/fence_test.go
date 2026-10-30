package gitservice

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// fenceSeam makes every terminated process group "still alive" until released
// (SIGKILL cannot leave a live member, so the fence is held artificially; the
// kills, the locks and the repair are real).
type fenceSeam struct{ gone atomic.Pointer[chan struct{}] }

func newFenceSeam(e *testEnv) *fenceSeam {
	f := &fenceSeam{}
	f.arm()
	e.svc.groupGoneHook = func(pgid int) bool {
		select {
		case <-*f.gone.Load():
			return !processGroupAlive(pgid)
		default:
			return false
		}
	}
	return f
}

func (f *fenceSeam) arm()     { c := make(chan struct{}); f.gone.Store(&c) }
func (f *fenceSeam) release() { close(*f.gone.Load()) }

// Test_gitservice_queued_requests_recheck_the_fence (GR-04): requests that were
// admitted or queued BEFORE a repository was fenced do not act on it once it
// is: maintenance waiting for the exclusive gate, and an operation holding the
// shared gate while it waits for the operation mutex behind a predecessor, are
// refused with 503 and change nothing.
func Test_gitservice_queued_requests_recheck_the_fence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	flag, started, done := filepath.Join(dir, "flag"), filepath.Join(dir, "started"), filepath.Join(dir, "done")
	e := newTestEnv(t, func(c *Config) { c.HookBinary = sleepingHook(t, "prepared", flag, started, done, 0) })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	seam := newFenceSeam(e)
	gate := e.svc.gates.of("repo_1")
	e.control.mu.Lock()
	e.control.deadline = func() time.Time { return time.Now().Add(2 * time.Second) }
	e.control.mu.Unlock()
	lockOf := func(ref string) string { return filepath.Join(e.repoDir("repo_1"), filepath.FromSlash(ref)+".lock") }

	// A receive that will be killed at its deadline while blocked in its hook.
	touch(t, flag)
	pushDone := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/rcva")
		pushDone <- err
	}()
	eventually(t, 30*time.Second, "the receive's hook to block", func() bool { return exists(started) })

	// (a) Maintenance queues for the exclusive gate behind the running receive.
	entered := make(chan struct{}, 1)
	e.svc.maintainHook = func(ctx context.Context, _ string) error { entered <- struct{}{}; return nil }
	type answer struct {
		status int
		body   []byte
	}
	maint := make(chan answer, 1)
	go func() {
		status, _, body := e.rpcRaw("/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_m"})
		maint <- answer{status, body}
	}()
	time.Sleep(500 * time.Millisecond) // queued behind the receive
	if err := <-pushDone; err == nil {
		t.Fatal("the receive survived its deadline")
	}
	ans := <-maint
	if ans.status != http.StatusServiceUnavailable || !strings.Contains(string(ans.body), "unavailable") {
		t.Fatalf("maintenance queued before the fence: %d %s", ans.status, ans.body)
	}
	select {
	case <-entered:
		t.Fatal("maintenance ran on a fenced repository")
	default:
	}
	if !gate.isFenced() {
		t.Fatal("the repository is not fenced")
	}
	seam.release()
	eventually(t, 15*time.Second, "the repair", func() bool { return !gate.isFenced() && !exists(lockOf("refs/heads/rcva")) })

	// (b) An operation queues behind another one (holding the shared gate)...
	seam.arm()
	e.svc.maintainHook = nil
	if err := os.Remove(started); err != nil {
		t.Fatal(err)
	}
	type created struct {
		resp gitv1.CreateBranchResponse
		code int
		body []byte
	}
	op := func(id, ref string) chan created {
		out := make(chan created, 1)
		go func() {
			status, _, body := e.rpcRaw("/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: id, Ref: ref, Commit: c1})
			c := created{code: status, body: body}
			if status == http.StatusOK {
				c.resp, _ = gitv1.DecodeResponse[gitv1.CreateBranchResponse](body)
			}
			out <- c
		}()
		return out
	}
	const first, second = "refs/heads/kodama/work/wi_1", "refs/heads/kodama/work/wi_2"
	op1 := op("op_1", first) // blocks in its prepared hook, holding the operation mutex
	eventually(t, 30*time.Second, "the first operation's hook to block", func() bool { return exists(started) })
	op2 := op("op_2", second) // queued behind it
	time.Sleep(700 * time.Millisecond)
	// ...and only then a receive is killed and the repository fenced.
	pushDone2 := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/rcvb")
		pushDone2 <- err
	}()
	eventually(t, 30*time.Second, "the repository to be fenced", gate.isFenced)
	if err := os.Remove(flag); err != nil { // the predecessor proceeds and finishes
		t.Fatal(err)
	}
	r1 := <-op1
	if r1.code != http.StatusOK || !r1.resp.Created {
		t.Fatalf("the predecessor: %d %s", r1.code, r1.body)
	}
	r2 := <-op2
	if r2.code != http.StatusServiceUnavailable || !strings.Contains(string(r2.body), "unavailable") {
		t.Fatalf("an operation queued before the fence: %d %s", r2.code, r2.body)
	}
	if e.bareRef("repo_1", second) != "" || e.branchStates("repo_1", second) != "[]" {
		t.Error("the refused operation changed the ref or the journal")
	}
	<-pushDone2
	seam.release()
}

// Test_gitservice_initialize_under_the_gate (GR-04): initialization and
// reconfiguration run under the repository's mutation gate: they wait for a
// running repair (which could otherwise delete the HEAD.lock they hold), are
// refused while the repository is fenced, including when they were queued
// before the fence, and every mutating Git command they run is process-group
// tracked although no hook is involved.
func Test_gitservice_initialize_under_the_gate(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	dir := e.repoDir("repo_1")
	gate := e.svc.gates.of("repo_1")
	head := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "HEAD"))
		return strings.TrimSpace(string(b))
	}
	init := func(branch string) chan int {
		out := make(chan int, 1)
		go func() {
			status, _, _ := e.rpcRaw("/repositories/repo_1/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_1", DefaultBranch: branch})
			out <- status
		}()
		return out
	}

	// Concurrent with a repair (the repair holds the gate exclusively): the
	// initialization waits, then completes.
	repair, err := gate.acquireExclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pending := init("trunk")
	select {
	case status := <-pending:
		t.Fatalf("initialization ran during a repair: %d", status)
	case <-time.After(500 * time.Millisecond):
	}
	if head() != "ref: refs/heads/main" {
		t.Fatal("HEAD changed during the repair")
	}
	repair()
	if status := <-pending; status != http.StatusOK || head() != "ref: refs/heads/trunk" {
		t.Fatalf("initialization after the repair: %d %s", status, head())
	}

	// Queued before the fence: refused once the repository is fenced.
	repair, _ = gate.acquireExclusive(context.Background())
	queued := init("main")
	time.Sleep(300 * time.Millisecond)
	gate.fence(424242, "")
	repair()
	if status := <-queued; status != http.StatusServiceUnavailable {
		t.Fatalf("an initialization queued before the fence: %d", status)
	}
	if head() != "ref: refs/heads/trunk" {
		t.Fatal("a refused initialization changed HEAD")
	}
	// Refused while fenced, whatever it asks for.
	rpcError(e, "/repositories/repo_1/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_1", DefaultBranch: "main"}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	if head() != "ref: refs/heads/trunk" {
		t.Fatal("a fenced repository's HEAD changed")
	}
	gate.unfence(424242)
	if status := <-init("main"); status != http.StatusOK || head() != "ref: refs/heads/main" {
		t.Fatalf("initialization after the fence cleared: %d %s", status, head())
	}

	// Mutating commands without hooks (config writes, symbolic-ref, init) are
	// process-group tracked.
	var mu sync.Mutex
	checked := 0
	e.svc.groupGoneHook = func(pgid int) bool {
		mu.Lock()
		checked++
		mu.Unlock()
		return !processGroupAlive(pgid)
	}
	e.initRepo("repo_1", "other") // config writes and symbolic-ref on an existing repository
	mu.Lock()
	reconfigure := checked
	checked = 0
	mu.Unlock()
	if want := len(repoInitConfig("", 0)) + 1; reconfigure < want {
		t.Errorf("%d tracked commands for a reconfiguration, want at least %d", reconfigure, want)
	}
	e.initRepo("repo_2", "main") // git init and the config writes of a new repository
	mu.Lock()
	created := checked
	mu.Unlock()
	if want := len(repoInitConfig("", 0)) + 1; created < want {
		t.Errorf("%d tracked commands for an initialization, want at least %d", created, want)
	}
}

// Test_gitservice_hook_git_children_hold_the_generation_lock: the Go hook passes
// the generation descriptor to the Git processes it starts (os/exec would drop
// it), so they hold the lock too: a Git binary wrapper records whether its
// descriptor 3 is the lock file whenever a hook started it.
func Test_gitservice_hook_git_children_hold_the_generation_lock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	ok, bad := filepath.Join(dir, "ok"), filepath.Join(dir, "bad")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	lockFile := filepath.Join(root, "worker.lock")
	script := filepath.Join(dir, "git.sh")
	body := "#!/bin/sh\n" +
		"if [ -n \"$KODAMA_HOOK_ADDR\" ] && [ \"$1\" != http-backend ]; then\n" +
		"  fd=$(ls -Li /dev/fd/3 2>/dev/null | awk '{print $1}'); lk=$(ls -i '" + lockFile + "' | awk '{print $1}')\n" +
		"  if [ -n \"$fd\" ] && [ \"$fd\" = \"$lk\" ]; then echo \"$@\" >> '" + ok + "'; else echo \"$@\" >> '" + bad + "'; fi\n" +
		"fi\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, func(c *Config) { c.DataRoot = root; c.GitBinary = script })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	e.mustGit(w, e.token("repo_1", true), "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if b, _ := os.ReadFile(bad); len(b) > 0 {
		t.Errorf("Git children of the hook without the generation lock:\n%s", b)
	}
	b, _ := os.ReadFile(ok)
	if !strings.Contains(string(b), "for-each-ref") {
		t.Errorf("no Git child of the hook was observed holding the lock: %q", b)
	}
}
