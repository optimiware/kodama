package gitservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Test_gitservice_branch_and_retained_ref_operations: server-side ref changes
// go through git update-ref with the worker's hooks, so they are journaled
// under the operation ID (prepared with the observed value, then committed);
// every call is idempotent and converges after an interrupted attempt.
func Test_gitservice_branch_and_retained_ref_operations(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	c1 := e.mustPlain(w, "rev-parse", "main")
	c2 := e.commit(w, "two", "2\n")
	e.seed("repo_1", w, "main:refs/heads/main")
	const branch = "refs/heads/kodama/work/wi_1"
	create := func(op, ref, commit string) gitv1.CreateBranchResponse {
		return rpcOK[gitv1.CreateBranchResponse](e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: op, Ref: ref, Commit: commit})
	}

	if got := create("op_1", branch, c1); !got.Created || got.Commit != c1 {
		t.Fatalf("create: %+v", got)
	}
	if e.bareRef("repo_1", branch) != c1 {
		t.Fatal("the branch was not created")
	}
	entries := e.journal("repo_1", 0, nil, &[]string{branch}[0]).Entries
	if len(entries) != 2 {
		t.Fatalf("journal: %+v", entries)
	}
	p, c := entries[0], entries[1]
	if p.OperationID == nil || *p.OperationID != "op_1" || p.ReceiveID != nil || p.State != gitv1.JournalPrepared || p.Old != zeroSHA || p.New != c1 || p.Observed == nil || *p.Observed != zeroSHA ||
		c.State != gitv1.JournalCommitted || c.Observed != nil || *c.OperationID != "op_1" {
		t.Errorf("operation entries: %+v %+v", p, c)
	}
	// Idempotent: the same call, and another operation naming the same state.
	if got := create("op_1", branch, c1); got.Created {
		t.Errorf("repeat: %+v", got)
	}
	if got := create("op_9", branch, c1); got.Created {
		t.Errorf("a different operation observing the same state: %+v", got)
	}
	if n := len(e.journal("repo_1", 0, nil, &[]string{branch}[0]).Entries); n != 2 {
		t.Errorf("a repeat journaled again: %d entries", n)
	}
	// The branch at another commit is a conflict, nothing changes.
	rpcError(e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_2", Ref: branch, Commit: c2}, http.StatusConflict, gitv1.CodeBranchExists)
	if e.bareRef("repo_1", branch) != c1 {
		t.Error("a conflicting create moved the branch")
	}
	// Only managed branches, and only commits that exist.
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/kodama/runs/run_1/1", "refs/heads/feature"} {
		rpcError(e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_3", Ref: ref, Commit: c1}, http.StatusBadRequest, domain.CodeInvalidRequest)
	}
	rpcError(e, "/repositories/repo_1/branches/create", gitv1.CreateBranchRequest{RepositoryID: "repo_1", OperationID: "op_4", Ref: "refs/heads/kodama/work/wi_2", Commit: strings.Repeat("a", 40)}, http.StatusBadRequest, domain.CodeInvalidRequest)
	if e.bareRef("repo_1", "refs/heads/kodama/work/wi_2") != "" {
		t.Error("a branch was created at a missing commit")
	}

	// An attempt that created the ref but died before journaling committed
	// converges: the retry observes the ref at the commit and closes the entry.
	const crashed = "refs/heads/kodama/work/wi_3"
	journal := journalAt(e.repoDir("repo_1"))
	if _, err := journal.appendPrepared(attribution{operationID: "op_crash"}, []gitv1.RefUpdate{{Ref: crashed, Old: zeroSHA, New: c1}}, []string{zeroSHA}); err != nil {
		t.Fatal(err)
	}
	e.mustPlain(w, "-c", "core.hooksPath="+os.DevNull, "--git-dir="+e.repoDir("repo_1"), "update-ref", crashed, c1)
	if got := create("op_crash", crashed, c1); got.Created {
		t.Errorf("convergence: %+v", got)
	}
	states := []gitv1.JournalState{}
	for _, en := range e.journal("repo_1", 0, nil, &[]string{crashed}[0]).Entries {
		states = append(states, en.State)
	}
	if fmt.Sprint(states) != "[prepared committed]" {
		t.Errorf("the interrupted operation's entries: %v", states)
	}
	// An attempt that died before the ref changed is closed aborted and redone.
	const redo = "refs/heads/kodama/work/wi_4"
	if _, err := journal.appendPrepared(attribution{operationID: "op_redo"}, []gitv1.RefUpdate{{Ref: redo, Old: zeroSHA, New: c1}}, []string{zeroSHA}); err != nil {
		t.Fatal(err)
	}
	if got := create("op_redo", redo, c1); !got.Created {
		t.Errorf("redo: %+v", got)
	}
	states = states[:0]
	for _, en := range e.journal("repo_1", 0, nil, &[]string{redo}[0]).Entries {
		states = append(states, en.State)
	}
	if fmt.Sprint(states) != "[prepared aborted prepared committed]" {
		t.Errorf("the redone operation's entries: %v", states)
	}

	// Delete: wrong expected value, the right one, then a repeat.
	del := func(op, ref, expected string) gitv1.DeleteBranchResponse {
		return rpcOK[gitv1.DeleteBranchResponse](e, "/repositories/repo_1/branches/delete", gitv1.DeleteBranchRequest{RepositoryID: "repo_1", OperationID: op, Ref: ref, ExpectedCommit: expected})
	}
	rpcError(e, "/repositories/repo_1/branches/delete", gitv1.DeleteBranchRequest{RepositoryID: "repo_1", OperationID: "op_5", Ref: branch, ExpectedCommit: c2}, http.StatusConflict, gitv1.CodeRefChanged)
	if e.bareRef("repo_1", branch) != c1 {
		t.Fatal("a delete with the wrong expected value removed the branch")
	}
	if got := del("op_6", branch, c1); !got.Deleted || e.bareRef("repo_1", branch) != "" {
		t.Errorf("delete: %+v", got)
	}
	if got := del("op_6", branch, c1); got.Deleted {
		t.Errorf("repeat of a delete: %+v", got)
	}
	last := e.journal("repo_1", 0, nil, &[]string{branch}[0]).Entries
	if n := len(last); n != 4 || last[2].Old != c1 || last[2].New != zeroSHA || *last[2].Observed != c1 || last[3].State != gitv1.JournalCommitted {
		t.Errorf("a delete's entries: %+v", last)
	}
	rpcError(e, "/repositories/repo_1/branches/delete", gitv1.DeleteBranchRequest{RepositoryID: "repo_1", OperationID: "op_7", Ref: "refs/heads/main", ExpectedCommit: c2}, http.StatusBadRequest, domain.CodeInvalidRequest)

	// Retained refs.
	const pin = "refs/kodama/runs/run_1/1"
	set := func(op, commit string) gitv1.SetRetainedRefResponse {
		return rpcOK[gitv1.SetRetainedRefResponse](e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: op, Ref: pin, Commit: commit})
	}
	if got := set("op_8", c1); !got.Created || e.bareRef("repo_1", pin) != c1 {
		t.Errorf("set: %+v", got)
	}
	if got := set("op_8", c1); got.Created {
		t.Errorf("repeat of a set: %+v", got)
	}
	rpcError(e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_9", Ref: pin, Commit: c2}, http.StatusConflict, gitv1.CodeRetainedRefExists)
	rpcError(e, "/repositories/repo_1/retained-refs/delete", gitv1.DeleteRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_10", Ref: pin, ExpectedCommit: c2}, http.StatusConflict, gitv1.CodeRefChanged)
	if got := rpcOK[gitv1.DeleteRetainedRefResponse](e, "/repositories/repo_1/retained-refs/delete", gitv1.DeleteRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_11", Ref: pin, ExpectedCommit: c1}); !got.Deleted || e.bareRef("repo_1", pin) != "" {
		t.Errorf("release: %+v", got)
	}
	rpcError(e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_12", Ref: "refs/heads/main", Commit: c1}, http.StatusBadRequest, domain.CodeInvalidRequest)
	// The whole journal is gap-free.
	if _, err := journal.read(0, 1<<20, nil); err != nil {
		t.Errorf("journal: %v", err)
	}
}

// Test_gitservice_journal_storage: appends are sequential and fsynced, a torn
// final line left by a crash is truncated by the next append and ignored by
// readers, and anything that is not a contiguous, strictly valid journal fails
// closed.
func Test_gitservice_journal_storage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := journalAt(dir)
	a := attribution{receiveID: "rcv_1"}
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	u := gitv1.RefUpdate{Ref: "refs/heads/x", Old: zeroSHA, New: c1}

	if head, err := j.head(); err != nil || head != 0 {
		t.Fatalf("a missing journal: %d %v", head, err)
	}
	if _, err := j.appendPrepared(a, []gitv1.RefUpdate{u}, []string{zeroSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.appendOutcome(a, gitv1.JournalCommitted, []gitv1.RefUpdate{u}); err != nil {
		t.Fatal(err)
	}
	// An outcome with no pending prepared entry writes nothing, and neither
	// does a second outcome for an already closed transaction.
	for _, un := range []gitv1.RefUpdate{{Ref: "refs/heads/y", Old: zeroSHA, New: c1}, u} {
		if got, err := j.appendOutcome(a, gitv1.JournalAborted, []gitv1.RefUpdate{un}); err != nil || len(got) != 0 {
			t.Errorf("an outcome without a pending prepared entry: %+v %v", got, err)
		}
	}
	// The same tuple of another receive is not confused with the first one.
	b := attribution{receiveID: "rcv_2"}
	if _, err := j.appendPrepared(b, []gitv1.RefUpdate{u}, []string{c1}); err != nil {
		t.Fatal(err)
	}
	if got, err := j.appendOutcome(a, gitv1.JournalAborted, []gitv1.RefUpdate{u}); err != nil || len(got) != 0 {
		t.Errorf("receive 1 closed receive 2's transaction: %+v %v", got, err)
	}
	if got, err := j.appendOutcome(b, gitv1.JournalAborted, []gitv1.RefUpdate{u}); err != nil || len(got) != 1 || got[0].Sequence != 4 {
		t.Errorf("receive 2's abort: %+v %v", got, err)
	}

	// A torn final line (a crash mid-append) is ignored by readers and repaired
	// by the next append, which continues the sequence.
	f, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"receiveId":"rcv_3","operationId":nul`)
	f.Close()
	if res, err := j.read(0, 100, nil); err != nil || res.Head != 4 || len(res.Entries) != 4 {
		t.Fatalf("read with a torn tail: %+v %v", res, err)
	}
	got, err := j.appendPrepared(attribution{operationID: "op_1"}, []gitv1.RefUpdate{{Ref: "refs/heads/z", Old: c1, New: c2}}, []string{c1})
	if err != nil || len(got) != 1 || got[0].Sequence != 5 {
		t.Fatalf("append after a torn tail: %+v %v", got, err)
	}
	if res, err := j.read(0, 100, nil); err != nil || res.Head != 5 || len(res.Entries) != 5 {
		t.Fatalf("read after the repair: %+v %v", res, err)
	}

	// Concurrent appenders never share or skip a sequence.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := attribution{receiveID: fmt.Sprintf("rcv_c%d", i)}
			for k := 0; k < 5; k++ {
				if _, err := j.appendPrepared(r, []gitv1.RefUpdate{{Ref: fmt.Sprintf("refs/heads/c%d", k), Old: zeroSHA, New: c1}}, []string{zeroSHA}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if res, err := j.read(0, 1000, nil); err != nil || res.Head != 45 || len(res.Entries) != 45 {
		t.Fatalf("after concurrent appends: head %d, %d entries, %v", res.Head, len(res.Entries), err)
	}

	// Corruption fails closed, for readers and appenders.
	for name, content := range map[string]string{
		"garbage":    "not json\n",
		"gap":        `{"receiveId":"rcv_1","operationId":null,"ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":"` + zeroSHA + `","state":"prepared","sequence":2}` + "\n",
		"unknown":    `{"receiveId":"rcv_1","operationId":null,"ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":"` + zeroSHA + `","state":"prepared","sequence":1,"extra":1}` + "\n",
		"both ids":   `{"receiveId":"rcv_1","operationId":"op_1","ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":"` + zeroSHA + `","state":"prepared","sequence":1}` + "\n",
		"observed":   `{"receiveId":"rcv_1","operationId":null,"ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":null,"state":"prepared","sequence":1}` + "\n",
		"long line":  strings.Repeat("x", 2*maxJournalLine) + "\n",
		"long torn":  strings.Repeat("x", 2*maxJournalLine),
		"regression": "",
	} {
		bad := journalAt(t.TempDir())
		if err := os.MkdirAll(filepath.Dir(bad.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if name == "regression" {
			content = `{"receiveId":"rcv_1","operationId":null,"ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":"` + zeroSHA + `","state":"prepared","sequence":1}` + "\n" +
				`{"receiveId":"rcv_1","operationId":null,"ref":"refs/heads/x","old":"` + zeroSHA + `","new":"` + c1 + `","observed":null,"state":"committed","sequence":1}` + "\n"
		}
		if err := os.WriteFile(bad.path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := bad.read(0, 10, nil); !errors.Is(err, errJournalCorrupt) && name != "long torn" {
			t.Errorf("%s: read error %v", name, err)
		}
		if _, err := bad.appendPrepared(a, []gitv1.RefUpdate{u}, []string{zeroSHA}); !errors.Is(err, errJournalCorrupt) && name != "regression" && name != "gap" { // appenders trust the last entry; readers check continuity
			t.Errorf("%s: append error %v", name, err)
		}
	}
}

// Test_gitservice_gate: shared holders (receives, ref operations) run together,
// the exclusive holder (maintenance) waits for them and keeps new ones out, a
// waiting exclusive holder is not starved, cancellation releases waiters, and
// a snapshot reports quiescence honestly.
func Test_gitservice_gate(t *testing.T) {
	t.Parallel()
	g := newGate()
	ctx := context.Background()
	s1, err := g.acquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := g.acquireShared(ctx)
	_ = g.snapshot(func(q bool) error {
		if q {
			t.Error("quiescent with two shared holders")
		}
		return nil
	})
	got := make(chan func(), 1)
	go func() {
		release, err := g.acquireExclusive(ctx)
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("exclusive granted with shared holders")
	default:
	}
	// A new shared request queues behind the waiting exclusive one.
	late := make(chan func(), 1)
	go func() {
		release, err := g.acquireShared(ctx)
		if err != nil {
			t.Error(err)
		}
		late <- release
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-late:
		t.Fatal("a shared holder overtook a waiting exclusive holder")
	default:
	}
	s1()
	s1() // idempotent
	select {
	case <-got:
		t.Fatal("exclusive granted while one shared holder remains")
	case <-time.After(100 * time.Millisecond):
	}
	s2()
	release := <-got
	select {
	case <-late:
		t.Fatal("a shared holder ran during maintenance")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	(<-late)()
	_ = g.snapshot(func(q bool) error {
		if !q {
			t.Error("not quiescent when idle")
		}
		return nil
	})
	// Cancellation.
	rel, _ := g.acquireExclusive(ctx)
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := g.acquireShared(cctx); err == nil {
		t.Error("a shared acquire succeeded during maintenance")
	}
	if _, err := g.acquireExclusive(cctx); err == nil {
		t.Error("a second exclusive acquire succeeded")
	}
	rel()
	if r, err := g.acquireShared(ctx); err != nil {
		t.Error(err)
	} else {
		r()
	}
}

func Test_gitservice_hook_scripts(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "hooks")
	binary := "/opt/kodama's bin/kodama"
	if err := InstallHooks(dir, binary); err != nil {
		t.Fatal(err)
	}
	for _, name := range HookNames() {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(b), `exec '/opt/kodama'\''s bin/kodama' git-hook `+name+` "$@"`) || !strings.HasPrefix(string(b), "#!/bin/sh\n") {
			t.Errorf("%s: %v\n%s", name, err, b)
		}
	}
	if err := InstallHooks(dir, "relative/kodama"); err == nil {
		t.Error("a relative hook binary was accepted")
	}
	var out, errb strings.Builder
	if code := HookMain(nil, strings.NewReader(""), &out, &errb, nil); code != 2 {
		t.Errorf("no hook name: %d", code)
	}
	if code := HookMain([]string{"update"}, strings.NewReader(""), &out, &errb, nil); code != 2 {
		t.Errorf("an unknown hook: %d", code)
	}
	// Outside a worker-started process every ref change is refused and a
	// receive's outcome is never reported.
	for _, args := range [][]string{{HookPreReceive}, {HookReferenceTransaction, "prepared"}} {
		in := strings.NewReader(zeroSHA + " " + strings.Repeat("1", 40) + " refs/heads/x\n")
		if code := HookMain(args, in, &out, &errb, nil); code != 1 {
			t.Errorf("%v without the worker's environment: %d", args, code)
		}
	}
	if code := HookMain([]string{HookReferenceTransaction, "preparing"}, strings.NewReader(""), &out, &errb, nil); code != 0 {
		t.Errorf("preparing: %d", code)
	}
}

// Test_gitservice_control_client: the HTTP ControlPlane client speaks the
// contract's routes under the tenant's control path with the worker identity,
// maps contract errors back and treats anything else as unavailable.
func Test_gitservice_control_client(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		base := strings.ReplaceAll(gitv1.ControlBasePath, "{tenantId}", testTenant)
		switch strings.TrimPrefix(r.URL.Path, base) {
		case "/pushes":
			var req gitv1.PushAuthorizeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			writeJSON(w, 200, gitv1.PushAuthorizeResponse{PushID: "push_1", Deadline: "2030-01-01T00:00:00Z", Updates: []gitv1.UpdateVerdict{{Ref: req.Updates[0].Ref, Verdict: gitv1.VerdictAllow}}})
		case "/pushes/push_1/commit-intent":
			writeJSON(w, 200, gitv1.CommitIntentResponse{Committing: true})
		case "/pushes/push_2/commit-intent":
			writeContractError(w, contractError(gitv1.CodePushSettled, "settled"), "req_1")
		case "/pushes/push_1/outcome":
			writeJSON(w, 200, gitv1.OutcomeResponse{PushID: "push_1", Status: gitv1.PushApplied})
		case "/repositories/repo_1/initialized":
			writeJSON(w, 200, gitv1.InitializedResponse{RepositoryID: "repo_1", Status: gitv1.StatusReady, Version: 2})
		case "/assignments":
			writeJSON(w, 200, gitv1.AssignmentsResponse{Items: []gitv1.Assignment{{Kind: gitv1.AssignInitialize, RepositoryID: "repo_1", DefaultBranch: "main"}}})
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c, err := NewControlPlaneClient(srv.URL, testTenant, func(context.Context) (string, error) { return "worker-token", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := gitv1.RefUpdate{Ref: "refs/heads/x", Old: zeroSHA, New: strings.Repeat("1", 40)}
	if resp, err := c.Authorize(ctx, gitv1.PushAuthorizeRequest{RepositoryID: "repo_1", GrantID: "gg_1", ReceiveID: "rcv_1", Updates: []gitv1.RefUpdate{u}, PushOptions: []string{}, GitFacts: []gitv1.GitFact{{Ref: u.Ref, RepositoryEmpty: true}}}); err != nil || resp.PushID != "push_1" {
		t.Errorf("authorize: %+v %v", resp, err)
	}
	if _, err := c.CommitIntent(ctx, "push_1", gitv1.CommitIntentRequest{ReceiveID: "rcv_1", Updates: []gitv1.RefUpdate{u}}); err != nil {
		t.Errorf("commit intent: %v", err)
	}
	_, err = c.CommitIntent(ctx, "push_2", gitv1.CommitIntentRequest{ReceiveID: "rcv_1", Updates: []gitv1.RefUpdate{u}})
	if ce, ok := asContract(err); !ok || ce.Code != gitv1.CodePushSettled {
		t.Errorf("a refusal: %v", err)
	}
	if _, err := c.Outcome(ctx, "push_1", gitv1.OutcomeRequest{ReceiveID: "rcv_1", Applied: []gitv1.RefUpdate{u}}); err != nil {
		t.Errorf("outcome: %v", err)
	}
	if _, err := c.Initialized(ctx, gitv1.InitializedRequest{RepositoryID: "repo_1", DefaultBranch: "main", Empty: true}); err != nil {
		t.Errorf("initialized: %v", err)
	}
	if a, err := c.Assignments(ctx); err != nil || len(a.Items) != 1 {
		t.Errorf("assignments: %+v %v", a, err)
	}
	_, err = c.Outcome(ctx, "push_9", gitv1.OutcomeRequest{ReceiveID: "rcv_1", Applied: []gitv1.RefUpdate{}})
	if ce, ok := asContract(err); !ok || ce.Code != domain.CodeUnavailable {
		t.Errorf("an unexpected answer: %v", err)
	}
	if _, err := c.CommitIntent(ctx, "../x", gitv1.CommitIntentRequest{}); err == nil {
		t.Error("a path-injecting push ID was sent")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range seen {
		if !strings.Contains(line, "/internal/git/v1/tenants/"+testTenant+"/") || !strings.HasSuffix(line, "Bearer worker-token") {
			t.Errorf("request %q", line)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://h/?q=1"} {
		if _, err := NewControlPlaneClient(bad, testTenant, func(context.Context) (string, error) { return "", nil }, nil); err == nil {
			t.Errorf("base URL %q accepted", bad)
		}
	}
}

// Test_gitservice_serve_command: ServeCommand serves the Git endpoints, the RPC
// and readiness on one listener, polls the control plane's assignments (an
// initialize assignment is performed and confirmed), and stops on cancellation.
func Test_gitservice_serve_command(t *testing.T) {
	t.Parallel()
	control := newFakeControl()
	verifier := &fakeVerifier{claims: map[string]gitv1.ForwardingAssertion{}}
	control.assignments = []gitv1.Assignment{{Kind: gitv1.AssignInitialize, RepositoryID: "repo_7", DefaultBranch: "trunk"}, {Kind: gitv1.AssignReconcile, RepositoryID: "repo_8"}}
	root := filepath.Join(t.TempDir(), "data")
	cmd, err := ServeCommand(Config{TenantID: testTenant, DataRoot: root, ControlPlane: control, Assertions: verifier, Callers: fakeCallers{},
		HookBinary: testHookBinary(t), PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()
	eventually(t, 10*time.Second, "the worker to answer", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	eventually(t, 20*time.Second, "the initialize assignment to be confirmed", func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return len(control.initialized) > 0
	})
	control.mu.Lock()
	init0 := control.initialized[0]
	control.mu.Unlock()
	if init0.RepositoryID != "repo_7" || init0.DefaultBranch != "trunk" || !init0.Empty {
		t.Errorf("confirmation: %+v", init0)
	}
	if head, _ := os.ReadFile(filepath.Join(root, "repositories", "repo_7.git", "HEAD")); strings.TrimSpace(string(head)) != "ref: refs/heads/trunk" {
		t.Errorf("HEAD: %q", head)
	}
	if _, err := os.Stat(filepath.Join(root, "repositories", "repo_8.git")); !os.IsNotExist(err) {
		t.Error("a reconcile assignment created a repository")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not stop")
	}
	// Configuration is validated up front and defaults to deny.
	for name, mod := range map[string]func(*Config){
		"no tenant":      func(c *Config) { c.TenantID = "" },
		"relative root":  func(c *Config) { c.DataRoot = "data" },
		"no control":     func(c *Config) { c.ControlPlane = nil },
		"no verifier":    func(c *Config) { c.Assertions = nil },
		"no caller auth": func(c *Config) { c.Callers = nil },
		"missing git":    func(c *Config) { c.GitBinary = "no-such-git-binary" },
		"relative hook":  func(c *Config) { c.HookBinary = "kodama" },
	} {
		cfg := Config{TenantID: testTenant, DataRoot: root, ControlPlane: control, Assertions: verifier, Callers: fakeCallers{}}
		mod(&cfg)
		if _, err := ServeCommand(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (&Command{cfg: Config{}}).Run(context.Background()); err == nil {
		t.Error("Run without an address")
	}
}
