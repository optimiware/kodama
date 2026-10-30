package gitservice

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Test_unit_gitservice_initialize_idempotent (GR-02, GR-A02): initialization
// creates the bare repository at the ID-derived path with the server-owned
// configuration, journal and default branch; repeated and concurrent calls
// converge on one repository and never disturb its refs.
func Test_unit_gitservice_initialize_idempotent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	first := e.initRepo("repo_1", "main")
	if !first.Created || !first.Empty || first.RepositoryID != "repo_1" || first.DefaultBranch != "main" {
		t.Fatalf("first initialize: %+v", first)
	}
	dir := e.repoDir("repo_1")
	if head, _ := os.ReadFile(filepath.Join(dir, "HEAD")); strings.TrimSpace(string(head)) != "ref: refs/heads/main" {
		t.Errorf("HEAD: %q", head)
	}
	for key, want := range map[string]string{
		"core.logAllRefUpdates": "always", "receive.fsckObjects": "true", "uploadpack.hideRefs": "refs/kodama",
		"receive.hideRefs": "refs/kodama", "core.hooksPath": e.svc.layout.hooks, "receive.maxInputSize": "1073741824",
		"uploadpack.allowAnySHA1InWant": "false",
	} {
		if got := e.mustPlain(dir, "config", "--file", filepath.Join(dir, "config"), "--get", key); got != want {
			t.Errorf("repository config %s = %q, want %q", key, got, want)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "kodama", "journal.jsonl")); err != nil || st.Size() != 0 {
		t.Errorf("journal: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "hooks")); len(entries) != 0 {
		t.Errorf("the repository carries sample hooks: %v", entries)
	}
	for _, name := range HookNames() {
		st, err := os.Stat(filepath.Join(e.svc.layout.hooks, name))
		if err != nil || st.Mode()&0o100 == 0 {
			t.Errorf("hook %s: %v", name, err)
		}
	}

	// A repeat finds it.
	if again := e.initRepo("repo_1", "main"); again.Created || !again.Empty {
		t.Errorf("repeat: %+v", again)
	}
	// Concurrent calls for a new ID create exactly one repository.
	var wg sync.WaitGroup
	created := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created <- e.initRepo("repo_2", "main").Created
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d concurrent initializations reported Created", n)
	}
	// A repeat never disturbs refs, and reports the repository no longer empty.
	w := e.workdir()
	e.seed("repo_1", w, "main:refs/heads/main")
	tip := e.bareRef("repo_1", "refs/heads/main")
	if again := e.initRepo("repo_1", "main"); again.Created || again.Empty {
		t.Errorf("repeat after a push: %+v", again)
	}
	if e.bareRef("repo_1", "refs/heads/main") != tip {
		t.Error("initialize changed a ref")
	}
	// The default branch follows a later request (HEAD only; no ref is renamed).
	if again := e.initRepo("repo_1", "trunk"); again.Created || again.DefaultBranch != "trunk" {
		t.Errorf("repeat with another default: %+v", again)
	}
	if head, _ := os.ReadFile(filepath.Join(dir, "HEAD")); strings.TrimSpace(string(head)) != "ref: refs/heads/trunk" {
		t.Errorf("HEAD after the change: %q", head)
	}
	// A repeat repairs configuration an operator changed on the volume.
	e.mustPlain(dir, "config", "--file", filepath.Join(dir, "config"), "receive.fsckObjects", "false")
	e.initRepo("repo_1", "trunk")
	if got := e.mustPlain(dir, "config", "--file", filepath.Join(dir, "config"), "--get", "receive.fsckObjects"); got != "true" {
		t.Errorf("config not repaired: %q", got)
	}

	// A crash between creating the directory and the rename leaves only an
	// .init-* directory, which a restart removes and which never serves.
	leftover := filepath.Join(e.svc.layout.repositories, ".init-i_crashed")
	if err := os.MkdirAll(filepath.Join(leftover, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.svc.cleanStale()
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Error("an interrupted initialization was not cleaned")
	}
	// One worker per data root: a second one cannot start while this one runs.
	if second, err := New(e.svc.cfg); err == nil {
		second.Close()
		t.Error("a second worker started on the same data root")
	}

	// Refusals.
	rpcError(e, "/repositories/repo_3/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_3", DefaultBranch: "kodama/x"}, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, "/repositories/repo_3/initialize", `{"repositoryId":"repo_3","defaultBranch":"main","extra":1}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, "/repositories/repo_3/initialize", `{"repositoryId":"repo_3"}`, http.StatusBadRequest, domain.CodeInvalidRequest)
	if status, _ := e.rpcWith(http.MethodPost, "/repositories/repo_3/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_3", DefaultBranch: "main"}, ""); status != http.StatusUnauthorized {
		t.Errorf("an unauthenticated initialize: %d", status)
	}
	if status, _ := e.rpcWith(http.MethodPost, "/repositories/repo_3/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: "repo_3", DefaultBranch: "main"}, "Bearer wrong"); status != http.StatusUnauthorized {
		t.Errorf("a wrong credential: %d", status)
	}
	if _, err := os.Stat(e.repoDir("repo_3")); !os.IsNotExist(err) {
		t.Error("a refused initialize created a repository")
	}
	// Operations on a repository that does not exist are not_found.
	rpcError(e, "/repositories/repo_3/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_3", Prefix: "refs/heads/", Limit: 10}, http.StatusNotFound, domain.CodeNotFound)
	if status, _ := e.rpcWith(http.MethodGet, "/repositories/repo_3/receives/rcv_1", nil, "Bearer "+controlToken); status != http.StatusNotFound {
		t.Errorf("liveness of an unknown repository: %d", status)
	}
}

// Test_unit_gitservice_refs_and_journal_reads (GR-09, GR-A13): the control
// plane reads refs (paged, per namespace), exact values under the mutation
// gate with an honest Quiescent flag and the journal head, journal entries by
// receive or by ref with paging, and receive liveness.
func Test_unit_gitservice_refs_and_journal_reads(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	e.initRepo("repo_empty", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	tip := e.mustPlain(w, "rev-parse", "main")
	for _, b := range []string{"b1", "b2", "b3"} {
		e.mustPlain(w, "branch", b)
	}
	e.mustPlain(w, "tag", "v1")
	e.seed("repo_1", w, "main:refs/heads/main", "b1:refs/heads/b1", "b2:refs/heads/b2", "b3:refs/heads/b3", "v1:refs/tags/v1", "main:refs/heads/kodama/work/wi_1")
	rpcOK[gitv1.SetRetainedRefResponse](e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: "refs/kodama/runs/run_1/1", Commit: tip})

	list := func(prefix string, cursor *string, limit int64) gitv1.RefsResponse {
		return rpcOK[gitv1.RefsResponse](e, "/repositories/repo_1/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_1", Prefix: prefix, Cursor: cursor, Limit: limit})
	}
	page := list("refs/heads/", nil, 2)
	if page.Empty || len(page.Refs) != 2 || page.Refs[0].Ref != "refs/heads/b1" || page.Refs[1].Ref != "refs/heads/b2" || page.NextCursor == nil || *page.NextCursor != "refs/heads/b2" || page.Refs[0].Commit != tip {
		t.Fatalf("first page: %+v", page)
	}
	page = list("refs/heads/", page.NextCursor, 2)
	if len(page.Refs) != 2 || page.Refs[0].Ref != "refs/heads/b3" || page.Refs[1].Ref != "refs/heads/kodama/work/wi_1" || page.NextCursor == nil {
		t.Fatalf("second page: %+v", page)
	}
	page = list("refs/heads/", page.NextCursor, 2)
	if len(page.Refs) != 1 || page.Refs[0].Ref != "refs/heads/main" || page.NextCursor != nil {
		t.Fatalf("last page: %+v", page)
	}
	if page = list("refs/tags/", nil, 10); len(page.Refs) != 1 || page.Refs[0].Ref != "refs/tags/v1" {
		t.Errorf("tags: %+v", page)
	}
	if page = list("refs/kodama/", nil, 10); len(page.Refs) != 1 || page.Refs[0].Ref != "refs/kodama/runs/run_1/1" {
		t.Errorf("server refs: %+v", page)
	}
	if empty := rpcOK[gitv1.RefsResponse](e, "/repositories/repo_empty/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_empty", Prefix: "refs/heads/", Limit: 10}); !empty.Empty || len(empty.Refs) != 0 {
		t.Errorf("an empty repository: %+v", empty)
	}
	rpcError(e, "/repositories/repo_1/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_1", Prefix: "refs/notes/", Limit: 10}, http.StatusBadRequest, domain.CodeInvalidRequest)
	rpcError(e, "/repositories/repo_1/refs/list", gitv1.ReadRefsRequest{RepositoryID: "repo_1", Prefix: "refs/heads/", Limit: 0}, http.StatusBadRequest, domain.CodeInvalidRequest)

	// Exact values, quiescent, with the journal head.
	values := func() gitv1.ReadRefValuesResponse {
		return rpcOK[gitv1.ReadRefValuesResponse](e, "/repositories/repo_1/refs/values", gitv1.ReadRefValuesRequest{RepositoryID: "repo_1", Refs: []string{"refs/heads/main", "refs/heads/absent", "refs/kodama/runs/run_1/1", "refs/heads/kodama"}})
	}
	v := values()
	if !v.Quiescent || v.WorkerInstance != e.svc.WorkerInstance() || len(v.Values) != 4 || v.Values[0].Commit == nil || *v.Values[0].Commit != tip ||
		v.Values[1].Commit != nil || v.Values[2].Commit == nil || v.Values[3].Commit != nil {
		t.Fatalf("values: %+v", v)
	}
	head := e.journalHead("repo_1")
	if v.JournalHead != head || head != 2 { // the retained ref's prepared and committed entries
		t.Errorf("journal head %d (disk %d)", v.JournalHead, head)
	}

	// Journal reads after two real receives.
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/r1a", "main:refs/heads/r1b")
	e.commit(w, "two", "2\n")
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/r2")
	auth, _, _ := e.control.snapshot()
	r1, r2 := auth[0].ReceiveID, auth[1].ReceiveID
	byReceive := e.journal("repo_1", 0, &r1, nil)
	if len(byReceive.Entries) != 4 || byReceive.HasMore || byReceive.WorkerInstance != e.svc.WorkerInstance() {
		t.Fatalf("receive 1: %+v", byReceive)
	}
	for _, en := range byReceive.Entries {
		if en.ReceiveID == nil || *en.ReceiveID != r1 {
			t.Errorf("an entry of another receive: %+v", en)
		}
	}
	if byReceive.HeadSequence != e.journalHead("repo_1") { // the head is the journal's, not the filtered page's
		t.Errorf("head sequence %d", byReceive.HeadSequence)
	}
	byRef := e.journal("repo_1", 0, nil, strp("refs/heads/r2"))
	if len(byRef.Entries) != 2 || *byRef.Entries[0].ReceiveID != r2 || byRef.HeadSequence != e.journalHead("repo_1") {
		t.Fatalf("by ref: %+v", byRef)
	}
	if retained := e.journal("repo_1", 0, nil, strp("refs/kodama/runs/run_1/1")); len(retained.Entries) != 2 || retained.Entries[0].OperationID == nil || *retained.Entries[0].OperationID != "op_1" || retained.Entries[0].ReceiveID != nil {
		t.Errorf("an operation's entries are attributed to the operation: %+v", retained.Entries)
	}
	paged := rpcOK[gitv1.ReadJournalResponse](e, "/repositories/repo_1/journal", gitv1.ReadJournalRequest{RepositoryID: "repo_1", ReceiveID: &r1, Limit: 1})
	if len(paged.Entries) != 1 || !paged.HasMore {
		t.Fatalf("a limited page: %+v", paged)
	}
	next := e.journal("repo_1", paged.Entries[0].Sequence, &r1, nil)
	if len(next.Entries) != 3 || next.Entries[0].Sequence <= paged.Entries[0].Sequence {
		t.Errorf("since: %+v", next)
	}
	if none := e.journal("repo_1", byReceive.HeadSequence+100, &r1, nil); len(none.Entries) != 0 || none.HeadSequence != e.journalHead("repo_1") {
		t.Errorf("since beyond the head: %+v", none)
	}
	rpcError(e, "/repositories/repo_1/journal", `{"repositoryId":"repo_1","receiveId":null,"ref":null,"sinceSequence":0,"limit":10}`, http.StatusBadRequest, domain.CodeInvalidRequest)

	// Liveness and quiescence across a running receive.
	if live := e.liveness("repo_1", r1); live.Running || live.ReceiveID != r1 || live.WorkerInstance != e.svc.WorkerInstance() {
		t.Errorf("a finished receive: %+v", live)
	}
	if e.liveness("repo_1", "rcv_never_heard_of").Running {
		t.Error("an unknown receive is not running")
	}
	block := newBlockingIntent()
	e.control.mu.Lock()
	e.control.intentFn = block.fn
	e.control.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/r3")
		done <- err
	}()
	select {
	case <-block.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the push never reached commit intent")
	}
	auth, _, _ = e.control.snapshot()
	r3 := auth[len(auth)-1].ReceiveID
	if !e.liveness("repo_1", r3).Running {
		t.Error("a running receive is not reported running")
	}
	if v := values(); v.Quiescent {
		t.Error("a read during a receive claims to be quiescent")
	}
	// The prepared entry is already durable while the receive waits.
	if prepared := e.journal("repo_1", 0, &r3, nil); len(prepared.Entries) != 1 || prepared.Entries[0].State != gitv1.JournalPrepared {
		t.Errorf("the prepared entry is not durable before commit intent: %+v", prepared.Entries)
	}
	close(block.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the receive to end", func() bool { return !e.liveness("repo_1", r3).Running })
	if v := values(); !v.Quiescent {
		t.Error("a read with nothing running is not quiescent")
	}
}

// Test_unit_gitservice_hidden_refs_not_fetchable (GR-A05): refs/kodama/* is
// invisible to clients: not advertised in any protocol version, not fetchable
// by name, and not writable by a push.
func Test_unit_gitservice_hidden_refs_not_fetchable(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	e.seed("repo_1", w, "main:refs/heads/main")
	pinned := e.commit(w, "pinned", "pinned\n")
	e.seed("repo_1", w, "main:refs/heads/kodama/work/wi_1")
	const pin = "refs/kodama/runs/run_1/1"
	rpcOK[gitv1.SetRetainedRefResponse](e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_1", Ref: pin, Commit: pinned})
	// Drop the branch: the pinned commit is now reachable only through the hidden ref.
	rpcOK[gitv1.DeleteBranchResponse](e, "/repositories/repo_1/branches/delete", gitv1.DeleteBranchRequest{RepositoryID: "repo_1", OperationID: "op_2", Ref: "refs/heads/kodama/work/wi_1", ExpectedCommit: pinned})
	if e.bareRef("repo_1", pin) != pinned {
		t.Fatal("the retained ref is missing on disk")
	}

	dst := t.TempDir()
	for _, version := range []string{"0", "2"} {
		out := e.mustGit(dst, tok, "-c", "protocol.version="+version, "ls-remote", e.url("repo_1"))
		if strings.Contains(out, "kodama") || strings.Contains(out, pinned) || !strings.Contains(out, "refs/heads/main") {
			t.Errorf("protocol v%s advertisement:\n%s", version, out)
		}
		out, err := e.git(dst, tok, "-c", "protocol.version="+version, "ls-remote", e.url("repo_1"), "refs/kodama/*")
		if err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("protocol v%s ls-remote of the hidden namespace: %v %q", version, err, out)
		}
	}
	e.mustPlain(dst, "init", "-q")
	for _, version := range []string{"0", "2"} {
		if out, err := e.git(dst, tok, "-c", "protocol.version="+version, "fetch", "-q", e.url("repo_1"), pin); err == nil {
			t.Errorf("protocol v%s: the hidden ref was fetched by name\n%s", version, out)
		}
	}
	// By object ID: refused for protocol v0/v1 (the pinned commit is no advertised tip).
	if out, err := e.git(dst, tok, "-c", "protocol.version=0", "fetch", "-q", e.url("repo_1"), pinned); err == nil {
		t.Errorf("protocol v0: the pinned commit was fetched by ID\n%s", out)
	}
	// A push cannot write the hidden namespace, whatever the control plane would say.
	authBefore, _, _ := e.control.snapshot()
	if out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/kodama/runs/run_1/2"); err == nil {
		t.Errorf("a push into refs/kodama succeeded\n%s", out)
	}
	if e.bareRef("repo_1", "refs/kodama/runs/run_1/2") != "" {
		t.Error("a client created a server ref")
	}
	if out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), ":"+pin); err == nil {
		t.Errorf("a push deleted the retained ref\n%s", out)
	}
	if e.bareRef("repo_1", pin) != pinned {
		t.Error("a client deleted the retained ref")
	}
	// (Git runs pre-receive before it refuses a hidden ref, so the control plane
	// is asked; T-130's policy denies refs/kodama as well.)
	_ = authBefore
	if out, err := e.git(dst, tok, "-c", "protocol.version=2", "fetch", "-q", e.url("repo_1"), pinned); err == nil {
		t.Logf("protocol v2 serves an object by ID (the protocol lets a client want any object it knows the ID of)\n%s", out)
	}
}

// Test_unit_gitservice_maintenance_serialized (GR-13, GR-16): maintenance holds
// the repository's mutation gate exclusively: it waits for a running receive, no
// receive starts while it runs, and a real maintenance run keeps pinned commits.
func Test_unit_gitservice_maintenance_serialized(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)

	var mu sync.Mutex
	var running, overlap int
	enter := make(chan struct{}, 4)
	hold := make(chan struct{})
	e.svc.maintainHook = func(ctx context.Context, repo string) error {
		mu.Lock()
		running++
		mu.Unlock()
		enter <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
		}
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}
	maintain := func(op string) chan gitv1.MaintainResponse {
		out := make(chan gitv1.MaintainResponse, 1)
		go func() {
			out <- rpcOK[gitv1.MaintainResponse](e, "/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: op})
		}()
		return out
	}

	// 1. Maintenance requested while a receive runs waits for it.
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
	m1 := maintain("op_1")
	select {
	case <-enter:
		t.Fatal("maintenance started while a receive was running")
	case <-time.After(500 * time.Millisecond):
	}
	close(block.release)
	if err := <-pushDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-enter:
	case <-time.After(20 * time.Second):
		t.Fatal("maintenance did not start after the receive ended")
	}

	// 2. While maintenance runs, a receive does not start (the control plane is
	// not even asked) and proceeds once it ends.
	e.control.mu.Lock()
	e.control.intentFn = nil
	e.control.mu.Unlock()
	e.commit(w, "two", "2\n")
	authBefore, _, _ := e.control.snapshot()
	push2 := make(chan error, 1)
	go func() {
		_, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
		push2 <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	if a, _, _ := e.control.snapshot(); len(a) != len(authBefore) {
		t.Fatal("a receive started during maintenance")
	}
	mu.Lock()
	if running != 1 {
		overlap++
	}
	mu.Unlock()
	close(hold)
	select {
	case <-m1:
	case <-time.After(20 * time.Second):
		t.Fatal("maintenance did not finish")
	}
	if err := <-push2; err != nil {
		t.Fatalf("the receive after maintenance: %v", err)
	}
	if overlap != 0 {
		t.Error("maintenance overlapped")
	}
	// A second maintenance request of the same operation is simply run again.
	e.svc.maintainHook = nil

	// 3. Real maintenance keeps every pinned commit.
	pinned := e.bareRef("repo_1", "refs/heads/main")
	rpcOK[gitv1.SetRetainedRefResponse](e, "/repositories/repo_1/retained-refs/set", gitv1.SetRetainedRefRequest{RepositoryID: "repo_1", OperationID: "op_pin", Ref: "refs/kodama/runs/run_1/1", Commit: pinned})
	m := rpcOK[gitv1.MaintainResponse](e, "/repositories/repo_1/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_1", OperationID: "op_real"})
	if m.OperationID != "op_real" {
		t.Errorf("maintenance: %+v", m)
	}
	e.mustPlain(e.repoDir("repo_1"), "--git-dir="+e.repoDir("repo_1"), "cat-file", "-e", pinned+"^{commit}")
	if e.bareRef("repo_1", "refs/kodama/runs/run_1/1") != pinned {
		t.Error("maintenance dropped a retained ref")
	}
	rpcError(e, "/repositories/repo_9/maintenance", gitv1.MaintainRequest{RepositoryID: "repo_9", OperationID: "op_1"}, http.StatusNotFound, domain.CodeNotFound)
}
