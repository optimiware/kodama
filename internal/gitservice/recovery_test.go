package gitservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// sleepingHook wraps the real hook binary in a script that, while the flag file
// exists, runs `: > started; sleep secs; : > done` before the real hook in the
// given reference-transaction state. The sleeping shell and its sleep are
// descendants of the Git process, so they hold whatever Git's children hold.
func sleepingHook(t *testing.T, state, flag, started, done string, secs int, extra ...string) string {
	t.Helper()
	real := testHookBinary(t)
	script := filepath.Join(t.TempDir(), "hook.sh")
	wait := "sleep " + fmt.Sprint(secs)
	if secs == 0 { // block until the flag file is removed
		wait = "while [ -e '" + flag + "' ]; do sleep 0.05; done"
	}
	body := "#!/bin/sh\n" +
		"if [ \"$2\" = reference-transaction ] && [ \"$3\" = " + state + " ] && [ -e '" + flag + "' ]; then\n" +
		strings.Join(extra, "\n") + "\n" +
		"  : > '" + started + "'\n  " + wait + "\n  : > '" + done + "'\nfi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rpcRawErr is an authenticated RPC returning the transport error, if any, the
// status, headers and body.
func (e *testEnv) rpcRawErr(path string, body any) (int, http.Header, []byte, error) {
	e.t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+gitv1.WorkerBasePath+path, bytes.NewReader(data))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out, nil
}

func (e *testEnv) rpcRaw(path string, body any) (int, http.Header, []byte) {
	e.t.Helper()
	status, header, out, err := e.rpcRawErr(path, body)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	return status, header, out
}

func (e *testEnv) branchStates(repo, ref string) string {
	e.t.Helper()
	var states []gitv1.JournalState
	for _, en := range e.journal(repo, 0, nil, &ref).Entries {
		states = append(states, en.State)
	}
	return fmt.Sprint(states)
}

// restart starts a worker on a data root another one released, waiting (as a
// real replacement does) for the generation lock.
func restart(t *testing.T, cfg Config) *Service {
	t.Helper()
	cfg.Limits.LockWait = 60 * time.Second
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Test_gitservice_unknown_outcome_is_ambiguous (S2): a mutation interrupted
// after Git published the ref (here: the committed hook killed by the RPC
// deadline) is answered by aborting the response, which the contract classifies
// as Outcome Unknown; the ref may be published, and repeating the identical
// request converges: success, not created, the open journal transaction closed.
func Test_gitservice_unknown_outcome_is_ambiguous(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	flag := filepath.Join(dir, "flag")
	e := newTestEnv(t, func(c *Config) {
		c.HookBinary = sleepingHook(t, "committed", flag, filepath.Join(dir, "started"), filepath.Join(dir, "done"), 120)
		c.Limits.OperationTimeout = 5 * time.Second
	})
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	const branch = "refs/heads/kodama/work/wi_1"
	req := gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: branch, Commit: c1}

	touch(t, flag)
	status, _, body, err := e.rpcRawErr("/repositories/repo_1/branches/create", req)
	if err == nil {
		t.Fatalf("an interrupted mutation answered %d %s, want a transport failure", status, body)
	}
	if e.bareRef("repo_1", branch) != c1 {
		t.Fatal("the ref was not published before the hook was interrupted")
	}
	if got := e.branchStates("repo_1", branch); got != "[prepared]" {
		t.Fatalf("journal after the interruption: %s", got)
	}

	// The identical request converges: success, not created, outcome closed.
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	got := rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", req)
	if got.Created || got.Commit != c1 || got.Ref != branch {
		t.Errorf("retry: %+v", got)
	}
	if st := e.branchStates("repo_1", branch); st != "[prepared committed]" {
		t.Errorf("journal after the retry: %s", st)
	}
	if again := rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", req); again.Created {
		t.Errorf("second retry: %+v", again)
	}

	// Definite answers stay definite: a refusal is a contract error with its
	// status, an unreadable journal is the definite 409 journal_unreadable (nothing changed).
	status, _, body = e.rpcRaw("/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_2", Ref: branch, Commit: strings.Repeat("a", 40)})
	if status != http.StatusConflict || !strings.Contains(string(body), string(gitv1.CodeBranchExists)) {
		t.Errorf("a refusal: %d %s", status, body)
	}
	if err := os.WriteFile(filepath.Join(e.repoDir("repo_1"), "kodama", "journal.jsonl"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, _, body = e.rpcRaw("/repositories/repo_1/journal", gitv1.ReadJournalRequest{RepositoryID: "repo_1", ReceiveID: strp("rcv_1"), Limit: 10})
	if status != http.StatusConflict || !strings.Contains(string(body), string(gitv1.CodeJournalUnreadable)) {
		t.Errorf("an affirmatively unreadable journal is definite: %d %s", status, body)
	}
}

// Test_gitservice_maintenance_interrupted_is_ambiguous: maintenance cancelled
// after it was launched and after an observable effect is a transport failure
// (Unknown); a retry runs it again.
func Test_gitservice_maintenance_interrupted_is_ambiguous(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) { c.Limits.MaintenanceTimeout = 2 * time.Second })
	e.initRepo("repo_1", "main")
	marker := filepath.Join(t.TempDir(), "effect")
	e.svc.maintainHook = func(ctx context.Context, _ string) error {
		touch(t, marker) // the observable effect
		<-ctx.Done()
		return ctx.Err()
	}
	status, _, body, err := e.rpcRawErr("/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_1"})
	if err == nil {
		t.Fatalf("an interrupted maintenance answered %d %s", status, body)
	}
	if !exists(marker) {
		t.Fatal("maintenance did not run")
	}
	e.svc.maintainHook = nil
	if m := rpcOK[gitv1.MaintainResponse](e, "/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_1"}); m.OperationID != "op_1" {
		t.Errorf("retry: %+v", m)
	}
}

// Test_gitservice_killed_git_fences_the_repository (GR-04): an update-ref killed
// at the RPC deadline while it holds a ref lock leaves the repository fenced
// until its process group is verified gone: reads are not quiescent, receives,
// operations, maintenance and the repair are refused, and the lock stays. Once
// the group is gone the repair removes the lock and the retry completes.
func Test_gitservice_killed_git_fences_the_repository(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	flag := filepath.Join(dir, "flag")
	e := newTestEnv(t, func(c *Config) {
		c.HookBinary = sleepingHook(t, "prepared", flag, filepath.Join(dir, "started"), filepath.Join(dir, "done"), 120)
		c.Limits.OperationTimeout = 3 * time.Second
	})
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	gone := make(chan struct{})
	e.svc.groupGoneHook = func(pgid int) bool { // the group is "still alive" until released
		select {
		case <-gone:
			return !processGroupAlive(pgid)
		default:
			return false
		}
	}
	const branch = "refs/heads/kodama/work/wi_2"
	lock := filepath.Join(e.repoDir("repo_1"), filepath.FromSlash(branch)+".lock")
	req := gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: branch, Commit: c1}
	gate := e.svc.gates.of("repo_1")

	touch(t, flag)
	errc := make(chan error, 1)
	go func() {
		_, _, _, err := e.rpcRawErr("/repositories/repo_1/branches/create", req)
		errc <- err
	}()
	eventually(t, 30*time.Second, "the transaction to hold its ref lock", func() bool { return exists(lock) })
	if err := <-errc; err == nil {
		t.Fatal("the killed mutation was answered")
	}
	if !gate.isFenced() {
		t.Fatal("the repository is not fenced although its killed group is not verified gone")
	}
	time.Sleep(300 * time.Millisecond)
	if !exists(lock) {
		t.Fatal("a lock was deleted while a tracked group was not verified gone")
	}
	if v := rpcOK[gitv1.ReadRefValuesResponse](e, "/repositories/repo_1/refs/values", gitv1.ReadRefValuesRequest{RepositoryID: "repo_1", Refs: []string{"refs/heads/main"}}); v.Quiescent {
		t.Error("a fenced repository claims to be quiescent")
	}
	if err := e.svc.repairRepository("repo_1", e.repoDir("repo_1")); !errors.Is(err, errFenced) {
		t.Errorf("a repair while the group is alive: %v", err)
	}
	if !exists(lock) {
		t.Fatal("the refused repair removed the lock")
	}
	other := req
	other.OperationID, other.Ref = "op_2", "refs/heads/kodama/work/wi_3"
	rpcError(e, "/repositories/repo_1/branches/create", other, http.StatusServiceUnavailable, domain.CodeUnavailable)
	rpcError(e, "/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_3"}, http.StatusServiceUnavailable, domain.CodeUnavailable)
	e.mustPlain(w, "branch", "pushed")
	if out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "pushed:refs/heads/pushed"); err == nil {
		t.Fatalf("a receive was admitted on a fenced repository\n%s", out)
	}
	if !exists(lock) {
		t.Fatal("a refused request removed the lock")
	}

	close(gone)
	eventually(t, 15*time.Second, "the fence to clear and the lock to be repaired", func() bool { return !gate.isFenced() && !exists(lock) })
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if got := rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", req); !got.Created {
		t.Errorf("retry after the repair: %+v", got)
	}
	if v := rpcOK[gitv1.ReadRefValuesResponse](e, "/repositories/repo_1/refs/values", gitv1.ReadRefValuesRequest{RepositoryID: "repo_1", Refs: []string{"refs/heads/main"}}); !v.Quiescent {
		t.Error("the repaired repository is not quiescent")
	}
}

// Test_gitservice_killed_receive_stays_running_until_group_gone: a receive
// killed at its deadline keeps reporting running while its process group is not
// verified gone, so settlement cannot treat it as stopped; the fenced repository
// is not quiescent; both clear when the group is gone.
func Test_gitservice_killed_receive_stays_running_until_group_gone(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	gone := make(chan struct{})
	e.svc.groupGoneHook = func(pgid int) bool {
		select {
		case <-gone:
			return !processGroupAlive(pgid)
		default:
			return false
		}
	}
	block := newBlockingIntent()
	e.control.mu.Lock()
	e.control.intentFn = block.fn
	e.control.deadline = func() time.Time { return time.Now().Add(3 * time.Second) }
	e.control.mu.Unlock()
	pushDone := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		pushDone <- err
	}()
	if err := <-pushDone; err == nil {
		t.Fatal("the push survived its deadline")
	}
	rid := e.firstReceiveID()
	gate := e.svc.gates.of("repo_1")
	eventually(t, 15*time.Second, "the repository to be fenced", gate.isFenced)
	time.Sleep(300 * time.Millisecond)
	if !e.liveness("repo_1", rid).Running {
		t.Error("a killed receive whose group is not verified gone is reported stopped")
	}
	if v := rpcOK[gitv1.ReadRefValuesResponse](e, "/repositories/repo_1/refs/values", gitv1.ReadRefValuesRequest{RepositoryID: "repo_1", Refs: []string{"refs/heads/main"}}); v.Quiescent {
		t.Error("a fenced repository claims to be quiescent")
	}
	close(gone)
	eventually(t, 15*time.Second, "the receive to stop running", func() bool { return !e.liveness("repo_1", rid).Running })
	eventually(t, 15*time.Second, "the lock to be repaired", func() bool { return !exists(filepath.Join(e.repoDir("repo_1"), "refs", "heads", "main.lock")) })
	if v := rpcOK[gitv1.ReadRefValuesResponse](e, "/repositories/repo_1/refs/values", gitv1.ReadRefValuesRequest{RepositoryID: "repo_1", Refs: []string{"refs/heads/main"}}); !v.Quiescent {
		t.Error("not quiescent after the group is gone")
	}
}

// Test_gitservice_hook_holds_the_generation_lock (GR-04): the worker passes the
// data root's lock to every Git process, so the hook (a descendant of
// receive-pack) holds it: file descriptor 3 of the hook is the lock file, a
// second worker on the data root cannot start while it runs and touches nothing
// when it gives up, and a replacement starts once the hook has exited.
func Test_gitservice_hook_holds_the_generation_lock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	flag, started, done, holds := filepath.Join(dir, "flag"), filepath.Join(dir, "started"), filepath.Join(dir, "done"), filepath.Join(dir, "holds")
	root := filepath.Join(t.TempDir(), "data")
	// Descriptor 3 is the lock file when it has the lock file's inode (the device
	// number of /dev/fd/N differs on some systems).
	lockFile := filepath.Join(root, "worker.lock")
	probe := "  fd=$(ls -Li /dev/fd/3 2>/dev/null | awk '{print $1}'); lk=$(ls -i '" + lockFile + "' | awk '{print $1}')\n" +
		"  if [ -n \"$fd\" ] && [ \"$fd\" = \"$lk\" ]; then : > '" + holds + "'; fi"
	e := newTestEnv(t, func(c *Config) {
		c.DataRoot = root
		c.HookBinary = sleepingHook(t, "prepared", flag, started, done, 3, probe)
	})
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	touch(t, flag)
	pushDone := make(chan struct{})
	go func() {
		_, _ = e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		close(pushDone)
	}()
	eventually(t, 30*time.Second, "the hook to run", func() bool { return exists(started) })
	if !exists(holds) {
		t.Fatal("the hook process does not hold the generation lock (its descriptor 3 is not the lock file)")
	}
	refLock := filepath.Join(root, "repositories", "repo_1.git", "refs", "heads", "main.lock")
	if !exists(refLock) {
		t.Fatal("the transaction holds no ref lock")
	}
	if s, err := New(e.svc.cfg); !errors.Is(err, ErrDataRootBusy) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("a second worker on the data root while the hook runs: %v", err)
	}
	if !exists(refLock) {
		t.Fatal("a worker that could not start touched the repository")
	}
	<-pushDone
}

// Test_gitservice_replacement_waits_for_surviving_git: the worker process is
// killed (SIGKILL, itself only) while a hook of a receive survives; a new worker
// blocks on the generation lock until that process exits, then removes the
// abandoned lock, keeps the journal and serves the repository again.
func Test_gitservice_replacement_waits_for_surviving_git(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	flag, started, done := filepath.Join(dir, "flag"), filepath.Join(dir, "started"), filepath.Join(dir, "done")
	script := sleepingHook(t, "prepared", flag, started, done, 8)
	root := filepath.Join(t.TempDir(), "data")
	addrFile := filepath.Join(dir, "addr")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envTestWorker+"="+root, envTestHook+"="+script, envTestAddrFile+"="+addrFile)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	eventually(t, 60*time.Second, "the worker process to listen", func() bool {
		b, err := os.ReadFile(addrFile) // the worker writes the file non-atomically: wait for its content
		return err == nil && len(b) > 0
	})
	addr, _ := os.ReadFile(addrFile)
	base := "http://" + string(addr)

	e := newTestEnv(t) // the Git client side only
	initReq, _ := json.Marshal(gitv1.InitializeRepositoryRequest{RepositoryID: "repo_1", DefaultBranch: "main"})
	req, _ := http.NewRequest(http.MethodPost, base+gitv1.WorkerBasePath+"/repositories/repo_1/initialize", bytes.NewReader(initReq))
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize through the worker process: %v", err)
	}
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize through the worker process: %d %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	w := e.workdir()
	touch(t, flag)
	pushDone := make(chan struct{})
	go func() {
		_, _ = e.git(w, subprocessToken, "push", "-q", base+"/repo_1.git", "main:refs/heads/main")
		close(pushDone)
	}()
	eventually(t, 60*time.Second, "the hook to run", func() bool { return exists(started) })
	refLock := filepath.Join(root, "repositories", "repo_1.git", "refs", "heads", "main.lock")
	if !exists(refLock) {
		t.Fatal("the transaction holds no ref lock")
	}

	// Kill the worker process only: receive-pack and the hook survive it.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true
	if !exists(refLock) {
		t.Fatal("the lock disappeared with the worker")
	}
	cfg := Config{TenantID: testTenant, DataRoot: root, ControlPlane: newFakeControl(), Assertions: e.verifier, Callers: fakeCallers{},
		HookBinary: testHookBinary(t), Limits: Limits{LockWait: 60 * time.Second}}
	type result struct {
		s   *Service
		err error
		at  time.Time
	}
	started0 := time.Now()
	res := make(chan result, 1)
	go func() {
		s, err := New(cfg)
		res <- result{s, err, time.Now()}
	}()
	select {
	case r := <-res:
		if r.s != nil {
			r.s.Close()
		}
		t.Fatalf("a replacement started while a process of the old generation lived (after %v, %v)", r.at.Sub(started0), r.err)
	case <-time.After(1500 * time.Millisecond):
	}
	if !exists(refLock) {
		t.Fatal("a waiting replacement touched the repository")
	}
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.s.Close()
	if !exists(done) {
		t.Error("the replacement started before the surviving hook exited")
	}
	if exists(refLock) {
		t.Error("the replacement did not remove the abandoned lock")
	}
	<-pushDone
	// The repository is served again by the replacement.
	srv := httptest.NewServer(r.s.Handler())
	defer srv.Close()
	tok := e.tokenWith("repo_1", true, func(*gitv1.ForwardingAssertion) {})
	e.mustGit(w, tok, "push", "-q", srv.URL+"/repo_1.git", "main:refs/heads/main")
	if e.bareRefAt(root, "repo_1", "refs/heads/main") == "" {
		t.Error("the ref could not be pushed after the replacement started")
	}
}

// Test_gitservice_branch_exists_at_another_commit: a branch that now points
// elsewhere is branch_exists for every request that names another commit,
// including the retry of the operation that created it; nothing changes and no
// response names a commit the branch does not point to.
func Test_gitservice_branch_exists_at_another_commit(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	c1 := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/main")
	const branch = "refs/heads/kodama/work/wi_1"
	req := gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: branch, Commit: c1}
	if got := rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", req); !got.Created {
		t.Fatalf("create: %+v", got)
	}
	// The branch advances through an ordinary push.
	c2 := e.commit(w, "two", "2\n")
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:"+branch)
	if e.bareRef("repo_1", branch) != c2 {
		t.Fatal("the branch did not advance")
	}
	before := e.journalHead("repo_1")
	for _, op := range []string{"op_1", "op_other"} {
		r := req
		r.OperationID = op
		rpcError(e, "/repositories/repo_1/branches/create", r, http.StatusConflict, gitv1.CodeBranchExists)
	}
	if e.bareRef("repo_1", branch) != c2 {
		t.Error("a refused create moved the branch")
	}
	if e.journalHead("repo_1") != before {
		t.Error("a refused create wrote the journal")
	}
	// The same rule for a retained ref.
	const pin = "refs/kodama/runs/run_1/1"
	rpcOK[gitv1.SetRetainedRefResponse](e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_p", Ref: pin, Commit: c1})
	e.mustPlain(w, "--git-dir="+e.repoDir("repo_1"), "-c", "core.hooksPath="+os.DevNull, "update-ref", pin, c2)
	rpcError(e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_p", Ref: pin, Commit: c1}, http.StatusConflict, gitv1.CodeRetainedRefExists)
}

// Test_gitservice_outcome_found_in_a_large_journal: the outcome of a receive is
// recorded however much unrelated traffic has grown the journal since its
// prepared entry (no tail cutoff), and a blocked real receive proves it.
func Test_gitservice_outcome_found_in_a_large_journal(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	block := newBlockingIntent()
	e.control.mu.Lock()
	e.control.intentFn = block.fn
	e.control.mu.Unlock()
	pushDone := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		pushDone <- err
	}()
	select {
	case <-block.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the push never reached commit intent")
	}
	rid := e.firstReceiveID()

	// Unrelated transactions grow the journal far past a megabyte while the
	// receive waits.
	j := journalAt(e.repoDir("repo_1"))
	tip := e.mustPlain(w, "rev-parse", "main")
	updates := make([]gitv1.RefUpdate, 4000)
	observed := make([]string, len(updates))
	for i := range updates {
		updates[i] = gitv1.RefUpdate{Ref: fmt.Sprintf("refs/heads/bulk/b%04d", i), Old: zeroSHA, New: tip}
		observed[i] = zeroSHA
	}
	bulk := attribution{operationID: "op_bulk"}
	if _, err := j.appendPrepared(bulk, updates, observed); err != nil {
		t.Fatal(err)
	}
	if _, err := j.appendOutcome(bulk, gitv1.JournalCommitted, updates); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(j.path); err != nil || st.Size() < 3<<19 { // well past the old 1 MiB window
		t.Fatalf("the journal is only %v bytes", st.Size())
	}

	close(block.release)
	if err := <-pushDone; err != nil {
		t.Fatal(err)
	}
	entries := receiveEntries(e.journalAll("repo_1"), rid)
	if len(entries) != 2 || entries[0].State != gitv1.JournalPrepared || entries[1].State != gitv1.JournalCommitted {
		t.Fatalf("the receive's entries after the journal grew: %+v", entries)
	}
}

// Test_gitservice_journal_backward_scan: pending preparations are located
// across block boundaries, from the oldest entry of a multi-block journal to
// the newest, and the newest entry of a tuple decides.
func Test_gitservice_journal_backward_scan(t *testing.T) {
	t.Parallel()
	j := journalAt(t.TempDir())
	tip := strings.Repeat("1", 40)
	other := attribution{receiveID: "rcv_other"}
	a := attribution{receiveID: "rcv_a"}
	updates := make([]gitv1.RefUpdate, 3000) // about a megabyte: several blocks
	observed := make([]string, len(updates))
	for i := range updates {
		updates[i] = gitv1.RefUpdate{Ref: fmt.Sprintf("refs/heads/s%04d", i), Old: zeroSHA, New: tip}
		observed[i] = zeroSHA
	}
	if _, err := j.appendPrepared(a, updates, observed); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 1499, 2999} {
		got, err := j.appendOutcome(a, gitv1.JournalCommitted, updates[i:i+1])
		if err != nil || len(got) != 1 || got[0].Ref != updates[i].Ref {
			t.Fatalf("outcome of entry %d: %+v %v", i, got, err)
		}
		if again, _ := j.appendOutcome(a, gitv1.JournalAborted, updates[i:i+1]); len(again) != 0 {
			t.Errorf("entry %d closed twice", i)
		}
	}
	if got, _ := j.appendOutcome(other, gitv1.JournalAborted, updates[2:3]); len(got) != 0 {
		t.Error("another receive's transaction was closed")
	}
	if got, _ := j.appendOutcome(a, gitv1.JournalAborted, []gitv1.RefUpdate{{Ref: "refs/heads/never", Old: zeroSHA, New: tip}}); len(got) != 0 {
		t.Error("a tuple that never appeared was closed")
	}
	// All lines survived the block boundaries intact.
	if res, err := j.read(0, 1, nil); err != nil || res.Head != 3004 {
		t.Fatalf("journal head %d: %v", res.Head, err)
	}
}
