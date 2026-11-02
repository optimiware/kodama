package gitservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// blockingIntent makes commit intent wait until released (or the hook's
// request is cancelled). entered is closed when the first request arrives.
type blockingIntent struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingIntent() *blockingIntent {
	return &blockingIntent{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingIntent) fn(ctx context.Context, _ string, _ gitv1.CommitIntentRequest) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(60 * time.Second):
		return errors.New("test timeout")
	}
}

func (e *testEnv) liveness(repo, receive string) gitv1.ReceiveLivenessResponse {
	e.t.Helper()
	status, data := e.rpcWith(http.MethodGet, "/repositories/"+repo+"/receives/"+receive, nil, "Bearer "+controlToken)
	if status != http.StatusOK {
		e.t.Fatalf("liveness: %d %s", status, data)
	}
	r, err := gitv1.DecodeResponse[gitv1.ReceiveLivenessResponse](data)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *testEnv) firstReceiveID() string {
	a, _, _ := e.control.snapshot()
	if len(a) == 0 {
		return ""
	}
	return a[0].ReceiveID
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Test_unit_gitservice_receive_deadline_enforced (GR-09, GR-16, GR-A13): the
// worker terminates an overrunning receive's whole process tree at the deadline
// the control plane set, reports the receive as running until then, leaves the
// prepared journal entry for settlement, removes the stale locks of the killed
// transaction, and still serves the ref afterwards. The per-request duration
// bound ends a receive that never got a deadline.
func Test_unit_gitservice_receive_deadline_enforced(t *testing.T) {
	t.Parallel()
	real := testHookBinary(t)
	pids := filepath.Join(t.TempDir(), "pids")
	script := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ >> '"+pids+"'\nexec '"+real+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, func(c *Config) { c.HookBinary = script })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)

	block := newBlockingIntent()
	var deadline time.Time
	e.control.mu.Lock()
	e.control.intentFn = block.fn
	e.control.deadline = func() time.Time {
		deadline = time.Now().Add(4 * time.Second)
		return deadline
	}
	e.control.mu.Unlock()

	type result struct {
		out string
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		done <- result{out, err, time.Now()}
	}()
	select {
	case <-block.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the push never reached commit intent")
	}
	rid := e.firstReceiveID()
	if rid == "" || !e.liveness("repo_1", rid).Running {
		t.Fatal("a receive waiting for commit intent is not reported running")
	}
	lock := filepath.Join(e.repoDir("repo_1"), "refs", "heads", "main.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the transaction holds no ref lock while prepared: %v", err)
	}

	res := <-done
	if res.err == nil {
		t.Fatalf("the push succeeded past its deadline\n%s", res.out)
	}
	if res.at.Before(deadline) {
		t.Errorf("the push ended %v before its deadline", deadline.Sub(res.at))
	}
	if res.at.Sub(start) > 25*time.Second {
		t.Errorf("the push took %v, the worker did not enforce the deadline", res.at.Sub(start))
	}
	eventually(t, 10*time.Second, "the receive to stop running", func() bool { return !e.liveness("repo_1", rid).Running })
	// Every process of the tree is gone: the hook (a grandchild of the CGI).
	raw, _ := os.ReadFile(pids)
	pidList := strings.Fields(string(raw))
	if len(pidList) == 0 {
		t.Fatal("no hook process was recorded")
	}
	for _, p := range pidList {
		pid, _ := strconv.Atoi(p)
		eventually(t, 5*time.Second, "hook process "+p+" to die", func() bool { return !processAlive(pid) })
	}
	// The journal keeps the unfinished transaction for settlement: prepared
	// with its observed value, and no outcome.
	entries := receiveEntries(e.journalAll("repo_1"), rid)
	if len(entries) != 1 || entries[0].State != gitv1.JournalPrepared || entries[0].Observed == nil || *entries[0].Observed != zeroSHA {
		t.Errorf("journal of the terminated receive: %+v", entries)
	}
	if e.bareRef("repo_1", "refs/heads/main") != "" {
		t.Error("the terminated receive changed the ref")
	}
	// The killed Git left its ref lock; the worker clears it once idle, so the
	// ref can be pushed again.
	eventually(t, 10*time.Second, "the stale ref lock to be removed", func() bool {
		_, err := os.Stat(lock)
		return errors.Is(err, os.ErrNotExist)
	})
	e.control.mu.Lock()
	e.control.intentFn, e.control.deadline = nil, nil
	e.control.mu.Unlock()
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if e.bareRef("repo_1", "refs/heads/main") == "" {
		t.Error("the ref could not be pushed after the termination")
	}
}

// A receive that never obtains a deadline (the control plane hangs in
// pre-receive) is still bounded by the per-request duration limit.
func Test_gitservice_request_duration_bounded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) { c.Limits.MaxDuration = 3 * time.Second; c.Limits.ControlTimeout = 30 * time.Second })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	entered := make(chan struct{})
	e.control.mu.Lock()
	e.control.authorizeFn = func(ctx context.Context, _ gitv1.PushAuthorizeRequest) error {
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Second):
			return errors.New("test timeout")
		}
	}
	e.control.mu.Unlock()
	start := time.Now()
	out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if err == nil {
		t.Fatalf("the push succeeded\n%s", out)
	}
	<-entered
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the push ran %v with a 3s duration bound", d)
	}
	rid := e.firstReceiveID()
	eventually(t, 10*time.Second, "the receive to stop", func() bool { return !e.liveness("repo_1", rid).Running })
}

// Test_gitservice_concurrency_bounded: requests above the tenant's concurrency
// bound are refused with 429 and Retry-After, not queued or dropped silently.
func Test_gitservice_concurrency_bounded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) { c.Limits.MaxConcurrent = 1 })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	block := newBlockingIntent()
	e.control.mu.Lock()
	e.control.intentFn = block.fn
	e.control.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		done <- err
	}()
	select {
	case <-block.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the push never reached commit intent")
	}
	status, header, body := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, nil, nil)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" || !strings.Contains(body, "rate_limited") {
		t.Errorf("a request above the bound: %d %v %s", status, header, body)
	}
	close(block.release)
	if err := <-done; err != nil {
		t.Fatalf("the admitted push failed: %v", err)
	}
	if status, _, _ := e.request("GET", "/repo_1.git/info/refs?service=git-upload-pack", tok, nil, nil); status != http.StatusOK {
		t.Errorf("after the push ended the slot was not released: %d", status)
	}
}

// Test_gitservice_outcome_retried_in_background: a lost post-receive
// confirmation is retried with the same body, and the pusher is not held by it.
func Test_gitservice_outcome_retried_in_background(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	var mu sync.Mutex
	failures := 2
	e.control.mu.Lock()
	e.control.outcomeFn = func() error {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return &gitv1.ContractError{Code: "unavailable", Message: "down"}
		}
		return nil
	}
	e.control.mu.Unlock()
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	eventually(t, 30*time.Second, "the outcome to be delivered after retries", func() bool {
		_, _, o := e.control.snapshot()
		return len(o) >= 3
	})
	_, _, outcomes := e.control.snapshot()
	for _, o := range outcomes {
		if fmt.Sprint(o) != fmt.Sprint(outcomes[0]) {
			t.Errorf("a retry changed the body: %+v vs %+v", o, outcomes[0])
		}
	}
}

// Test_gitservice_update_bound: a receive of more refs than the installation's
// bound is refused explicitly (too_many_updates) before the control plane is
// asked, and nothing changes.
func Test_gitservice_update_bound(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) { c.Limits.MaxRefUpdates = 3 })
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	for i := 0; i < 4; i++ {
		e.mustPlain(w, "branch", fmt.Sprintf("b%d", i))
	}
	out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "--all")
	if err == nil || !strings.Contains(out, "too_many_updates") {
		t.Fatalf("an oversized receive: %v\n%s", err, out)
	}
	if a, _, _ := e.control.snapshot(); len(a) != 0 {
		t.Error("the oversized receive reached the control plane")
	}
	if e.bareRef("repo_1", "refs/heads/b0") != "" {
		t.Error("the oversized receive changed refs")
	}
	// Within the bound it works, in batches.
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "b0:refs/heads/b0", "b1:refs/heads/b1")
}
