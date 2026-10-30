package gitservice

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// journalAll reads the whole journal of a repository from disk (the RPC reads
// one receive or one ref at a time).
func (e *testEnv) journalAll(repo string) []gitv1.JournalEntry {
	e.t.Helper()
	res, err := journalAt(e.repoDir(repo)).read(0, 1<<20, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Entries
}

// receiveEntries are the journal entries of one receive, in order.
func (e *testEnv) journalHead(repo string) int64 {
	e.t.Helper()
	head, err := journalAt(e.repoDir(repo)).head()
	if err != nil {
		e.t.Fatal(err)
	}
	return head
}

func receiveEntries(entries []gitv1.JournalEntry, receiveID string) []gitv1.JournalEntry {
	var out []gitv1.JournalEntry
	for _, en := range entries {
		if en.ReceiveID != nil && *en.ReceiveID == receiveID {
			out = append(out, en)
		}
	}
	return out
}

// Test_unit_gitservice_ref_journal_attributes_receive (GR-09, GR-A06): every ref
// transaction of a receive is journaled (fsynced, monotonic sequence) under the
// receive ID the control plane saw in pre-receive, prepared with the value
// observed under the lock and then committed; the git facts reach the control
// plane; creates, fast-forwards, rewrites and deletes are all attributed.
func Test_unit_gitservice_ref_journal_attributes_receive(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	tip0 := e.mustPlain(w, "rev-parse", "main")

	// One receive of many refs, with a push option.
	const refs = 12
	for i := 0; i < refs-1; i++ {
		e.mustPlain(w, "branch", fmt.Sprintf("b%02d", i))
	}
	e.mustGit(w, tok, "push", "-q", "-o", "purpose=test", e.url("repo_1"), "--all")

	auth, intents, outcomes := e.control.snapshot()
	if len(auth) != 1 || len(outcomes) != 1 {
		t.Fatalf("one receive expected: %d authorizations, %d outcomes", len(auth), len(outcomes))
	}
	req, rid := auth[0], auth[0].ReceiveID
	if err := req.Validate(); err != nil {
		t.Fatalf("the authorize request is not the contract's: %v", err)
	}
	if req.RepositoryID != "repo_1" || req.GrantID != "gg_1" || len(req.Updates) != refs || len(req.PushOptions) != 1 || req.PushOptions[0] != "purpose=test" {
		t.Fatalf("authorize request: %+v", req)
	}
	for i, u := range req.Updates {
		f := req.GitFacts[i]
		if u.Old != zeroSHA || u.New != tip0 || f.Ref != u.Ref || !f.RepositoryEmpty || f.FastForward != nil {
			t.Errorf("update %d: %+v facts %+v", i, u, f)
		}
	}
	seen := map[string]bool{}
	for _, in := range intents {
		if in.ReceiveID != rid {
			t.Errorf("commit intent for receive %q, want %q", in.ReceiveID, rid)
		}
		for _, u := range in.Updates {
			seen[u.Ref] = true
		}
	}
	if len(seen) != refs {
		t.Errorf("commit intent covered %d refs, want %d", len(seen), refs)
	}
	if outcomes[0].ReceiveID != rid || len(outcomes[0].Applied) != refs {
		t.Errorf("outcome: %+v", outcomes[0])
	}

	entries := e.journalAll("repo_1")
	if len(entries) != 2*refs {
		t.Fatalf("journal has %d entries, want %d", len(entries), 2*refs)
	}
	prepared := map[string]int64{}
	for i, en := range entries {
		if en.Sequence != int64(i+1) {
			t.Fatalf("entry %d has sequence %d", i, en.Sequence)
		}
		if en.ReceiveID == nil || *en.ReceiveID != rid || en.OperationID != nil || en.New != tip0 || en.Old != zeroSHA {
			t.Errorf("entry %d is not attributed to receive %s: %+v", i, rid, en)
		}
		switch en.State {
		case gitv1.JournalPrepared:
			if en.Observed == nil || *en.Observed != zeroSHA {
				t.Errorf("prepared %s observed %v, want the absent ref", en.Ref, en.Observed)
			}
			prepared[en.Ref] = en.Sequence
		case gitv1.JournalCommitted:
			if en.Observed != nil {
				t.Errorf("committed entry carries observed")
			}
			if p, ok := prepared[en.Ref]; !ok || p > en.Sequence {
				t.Errorf("committed %s without an earlier prepared entry", en.Ref)
			}
		default:
			t.Errorf("unexpected state %s", en.State)
		}
	}
	if len(prepared) != refs {
		t.Errorf("%d refs prepared, want %d", len(prepared), refs)
	}

	// A fast-forward: the worker passes fastForward true and the entry observes
	// the old tip.
	tip1 := e.commit(w, "two", "2\n")
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	auth, _, _ = e.control.snapshot()
	if f := auth[1].GitFacts[0]; f.RepositoryEmpty || f.FastForward == nil || !*f.FastForward {
		t.Errorf("fast-forward facts: %+v", f)
	}
	assertTransaction(t, receiveEntries(e.journalAll("repo_1"), auth[1].ReceiveID), "refs/heads/main", tip0, tip1, tip0)

	// A forced rewrite: fastForward false, observed the previous tip.
	e.mustPlain(w, "reset", "-q", "--hard", tip0)
	tip2 := e.commit(w, "three", "3\n")
	e.mustGit(w, tok, "push", "-q", "-f", e.url("repo_1"), "main:refs/heads/main")
	auth, _, _ = e.control.snapshot()
	if f := auth[2].GitFacts[0]; f.FastForward == nil || *f.FastForward {
		t.Errorf("rewrite facts: %+v", f)
	}
	assertTransaction(t, receiveEntries(e.journalAll("repo_1"), auth[2].ReceiveID), "refs/heads/main", tip1, tip2, tip1)
	if got := e.bareRef("repo_1", "refs/heads/main"); got != tip2 {
		t.Errorf("main is %s, want %s", got, tip2)
	}

	// A delete: new is all-zero, observed the deleted tip.
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), ":refs/heads/b00")
	auth, _, _ = e.control.snapshot()
	if f := auth[3].GitFacts[0]; f.FastForward != nil || auth[3].Updates[0].New != zeroSHA {
		t.Errorf("delete: %+v %+v", auth[3].Updates[0], f)
	}
	assertTransaction(t, receiveEntries(e.journalAll("repo_1"), auth[3].ReceiveID), "refs/heads/b00", tip0, zeroSHA, tip0)
	if e.bareRef("repo_1", "refs/heads/b00") != "" {
		t.Error("the deleted ref remains")
	}
	// Each receive has its own ID.
	ids := map[string]bool{}
	for _, a := range auth {
		ids[a.ReceiveID] = true
	}
	if len(ids) != len(auth) {
		t.Error("receive IDs repeat")
	}
}

// assertTransaction expects exactly a prepared and a committed entry.
func assertTransaction(t *testing.T, entries []gitv1.JournalEntry, ref, old, newValue, observed string) {
	t.Helper()
	if len(entries) != 2 {
		t.Fatalf("%s: %d journal entries, want prepared and committed: %+v", ref, len(entries), entries)
	}
	p, c := entries[0], entries[1]
	if p.State != gitv1.JournalPrepared || p.Ref != ref || p.Old != old || p.New != newValue || p.Observed == nil || *p.Observed != observed {
		t.Errorf("%s: prepared entry %+v (observed %v), want old %s new %s observed %s", ref, p, p.Observed, old, newValue, observed)
	}
	if c.State != gitv1.JournalCommitted || c.Ref != ref || c.Old != old || c.New != newValue || c.Observed != nil || c.Sequence != p.Sequence+1 {
		t.Errorf("%s: committed entry %+v", ref, c)
	}
}

// Test_unit_gitservice_prepared_hook_aborts_without_intent (GR-09, GR-A06): a
// transaction whose commit intent is refused, or whose journal entry cannot be
// made durable, aborts: no ref changes, the journal says prepared then aborted
// (or nothing), and a partial push applies exactly the refs that obtained
// intent.
func Test_unit_gitservice_prepared_hook_aborts_without_intent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	tip := e.mustPlain(w, "rev-parse", "main")
	journalPath := filepath.Join(e.repoDir("repo_1"), "kodama", "journal.jsonl")

	// 1. The journal cannot be written: the hook aborts before asking for
	// commit intent.
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// (Git dies when a prepared hook refuses, so the client sees the connection
	// end; the reason is in the worker's log.)
	out, err := e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if err == nil {
		t.Fatalf("a push without a writable journal succeeded\n%s", out)
	}
	if _, intents, _ := e.control.snapshot(); len(intents) != 0 {
		t.Errorf("commit intent was requested without a durable prepared entry: %v", intents)
	}
	if e.bareRef("repo_1", "refs/heads/main") != "" {
		t.Error("the ref changed although the journal was unwritable")
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Commit intent refused (a contract refusal): the transaction aborts and
	// the journal records prepared then aborted for the receive.
	e.control.mu.Lock()
	e.control.intentFn = func(context.Context, string, gitv1.CommitIntentRequest) error {
		return &gitv1.ContractError{Code: gitv1.CodePushSettled, Message: "the push record was settled"}
	}
	e.control.mu.Unlock()
	out, err = e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if err == nil {
		t.Fatalf("a push whose commit intent was refused succeeded\n%s", out)
	}
	if e.bareRef("repo_1", "refs/heads/main") != "" {
		t.Error("the ref changed although commit intent was refused")
	}
	auth, _, outcomes := e.control.snapshot()
	entries := receiveEntries(e.journalAll("repo_1"), auth[len(auth)-1].ReceiveID)
	if len(entries) != 2 || entries[0].State != gitv1.JournalPrepared || entries[1].State != gitv1.JournalAborted ||
		entries[0].Observed == nil || *entries[0].Observed != zeroSHA || entries[1].Observed != nil {
		t.Errorf("journal after a refused intent: %+v", entries)
	}
	if len(outcomes) != 0 {
		t.Errorf("an outcome was reported for a push that applied nothing: %v", outcomes)
	}

	// 3. The control plane unreachable at commit intent: the same.
	e.control.mu.Lock()
	e.control.intentFn = func(context.Context, string, gitv1.CommitIntentRequest) error {
		return errors.New("connection refused")
	}
	e.control.mu.Unlock()
	if out, err = e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main"); err == nil {
		t.Fatalf("a push without commit intent succeeded\n%s", out)
	}
	if e.bareRef("repo_1", "refs/heads/main") != "" {
		t.Error("the ref changed although the control plane was unreachable")
	}

	// 4. A denied update fails the whole receive before any transaction.
	headBefore := e.journalHead("repo_1")
	e.control.mu.Lock()
	e.control.intentFn = nil
	e.control.deny = map[string]string{"refs/heads/main": gitv1.ReasonRefProtected}
	e.control.mu.Unlock()
	e.mustPlain(w, "branch", "other")
	out, err = e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main", "other:refs/heads/other")
	if err == nil || !strings.Contains(out, "ref_protected") || !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("a denied update: %v\n%s", err, out)
	}
	if e.bareRef("repo_1", "refs/heads/main") != "" || e.bareRef("repo_1", "refs/heads/other") != "" {
		t.Error("a denied receive changed a ref (the allowed ref must not apply either)")
	}
	if head := e.journalHead("repo_1"); head != headBefore {
		t.Errorf("a denied receive wrote the journal: head %d, was %d", head, headBefore)
	}

	// 5. The control plane down in pre-receive refuses the push.
	e.control.mu.Lock()
	e.control.deny = map[string]string{}
	e.control.authorizeErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
	e.control.mu.Unlock()
	if out, err = e.git(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main"); err == nil {
		t.Fatalf("a push while the control plane is down succeeded\n%s", out)
	}
	e.control.mu.Lock()
	e.control.authorizeErr = nil
	e.control.mu.Unlock()

	// 6. A multi-ref push where one ref's commit intent is refused. Git aborts
	// the transaction that holds it and ends the receive. Depending on the Git
	// version a receive's refs are one batched transaction or one per ref, so the
	// assertion is the invariant, not the split: a ref is journaled committed
	// exactly when it changed, aborted exactly when it did not, the refused ref
	// is aborted, and no outcome is reported (post-receive never ran): the push
	// record is settled from this journal by reconciliation.
	e.mustPlain(w, "branch", "keep1")
	e.mustPlain(w, "branch", "keep2")
	e.mustPlain(w, "branch", "zrefused")
	e.control.mu.Lock()
	e.control.intentFn = func(_ context.Context, _ string, req gitv1.CommitIntentRequest) error {
		for _, u := range req.Updates {
			if u.Ref == "refs/heads/zrefused" {
				return &gitv1.ContractError{Code: gitv1.CodeAttemptInactive, Message: "the attempt is no longer live"}
			}
		}
		return nil
	}
	e.control.mu.Unlock()
	_, _, outcomesBefore := e.control.snapshot()
	out, err = e.git(w, tok, "push", "-q", e.url("repo_1"), "keep1:refs/heads/keep1", "keep2:refs/heads/keep2", "zrefused:refs/heads/zrefused")
	if err == nil {
		t.Fatalf("a push with a refused ref reports success\n%s", out)
	}
	auth, _, outcomes = e.control.snapshot()
	last := auth[len(auth)-1].ReceiveID
	states := map[string][]gitv1.JournalState{}
	for _, en := range receiveEntries(e.journalAll("repo_1"), last) {
		states[en.Ref] = append(states[en.Ref], en.State)
	}
	for _, ref := range []string{"refs/heads/keep1", "refs/heads/keep2", "refs/heads/zrefused"} {
		got := e.bareRef("repo_1", ref)
		switch fmt.Sprint(states[ref]) {
		case "[prepared committed]":
			if got != tip {
				t.Errorf("%s is journaled committed but is %q", ref, got)
			}
		case "[prepared aborted]":
			if got != "" {
				t.Errorf("%s is journaled aborted but is %q", ref, got)
			}
		default:
			t.Errorf("%s: journal states %v", ref, states[ref])
		}
	}
	if fmt.Sprint(states["refs/heads/zrefused"]) != "[prepared aborted]" || e.bareRef("repo_1", "refs/heads/zrefused") != "" {
		t.Errorf("the refused ref: %v", states["refs/heads/zrefused"])
	}
	if len(outcomes) != len(outcomesBefore) {
		t.Errorf("an outcome was reported for a receive that never reached post-receive: %+v", outcomes[len(outcomesBefore):])
	}
}

// Test_unit_gitservice_hooks_not_overridable (GR-08, GR-A05): the server-owned
// hooks and limits come from command-scope configuration and a worker-owned
// environment, so repository-local hooks and config, and the process's own
// GIT_* variables, change nothing; a Git process the worker did not start
// cannot change a ref.
func Test_unit_gitservice_hooks_not_overridable(t *testing.T) {
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	dir := e.repoDir("repo_1")
	testHookBinary(t) // build it before the environment is polluted below

	// The configuration at command scope wins over the repository's own file.
	marker := filepath.Join(t.TempDir(), "marker")
	local := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-receive", "update", "post-receive", "post-update", "reference-transaction", "pre-auto-gc"} {
		if err := os.WriteFile(filepath.Join(local, name), []byte("#!/bin/sh\necho "+name+" >> "+marker+"\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := os.OpenFile(filepath.Join(dir, "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(cfg, "[core]\n\thooksPath = %s\n[uploadpack]\n\tallowAnySHA1InWant = true\n\tallowTipSHA1InWant = true\n\tallowReachableSHA1InWant = true\n[receive]\n\tfsckObjects = false\n\tmaxInputSize = 1\n[http]\n\treceivepack = true\n", local)
	cfg.Close()

	ctx := context.Background()
	for key, want := range map[string]string{
		"core.hooksPath": e.svc.layout.hooks, "uploadpack.allowAnySHA1InWant": "false", "uploadpack.allowReachableSHA1InWant": "false",
		"uploadpack.allowTipSHA1InWant": "false", "receive.fsckObjects": "true", "uploadpack.hideRefs": "refs/kodama",
	} {
		res, err := e.svc.runGit(ctx, dir, true, nil, "config", "--show-scope", "--get", key)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "command\t"+want {
			t.Errorf("%s resolves to %q, want command scope %q", key, got, want)
		}
	}

	// The process environment does not leak into Git, and the worker's own
	// variables are the only GIT_* ones.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+local+"'")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GIT_EXTERNAL_DIFF", "false")
	t.Setenv("GIT_SSH_COMMAND", "false")
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LC_ALL": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_CONFIG_GLOBAL": true,
		"GIT_TERMINAL_PROMPT": true, "GIT_ASKPASS": true, "GIT_CONFIG_COUNT": true}
	for _, kv := range e.svc.gitEnv(true) {
		name, value, _ := strings.Cut(kv, "=")
		if allowed[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		t.Errorf("unexpected variable in the Git environment: %s=%s", name, value)
	}
	if v := envValue(e.svc.gitEnv(true), "GIT_CONFIG_GLOBAL"); v != e.svc.layout.globalConfig {
		t.Errorf("GIT_CONFIG_GLOBAL = %q", v)
	}
	global, err := os.ReadFile(e.svc.layout.globalConfig)
	if err != nil || !strings.Contains(string(global), "hooksPath") || !strings.Contains(string(global), `allowAnySHA1InWant = "false"`) {
		t.Errorf("worker-owned global configuration: %v\n%s", err, global)
	}

	// A push through the worker runs the server's hooks (the control plane is
	// asked), not the repository's; the pack is accepted despite the local
	// receive.maxInputSize = 1.
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if a, i, o := e.control.snapshot(); len(a) != 1 || len(i) == 0 || len(o) != 1 {
		t.Errorf("the server's hooks did not run: %d/%d/%d", len(a), len(i), len(o))
	}
	tipA := e.mustPlain(w, "rev-parse", "main")
	tipB := e.commit(w, "two", "2\n")
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		b, _ := os.ReadFile(marker)
		t.Errorf("a repository-local hook ran: %s", b)
	}

	// uploadpack.allowAnySHA1InWant = true in the repository is ignored: under
	// protocol v0 a client cannot fetch an object that is not a ref tip.
	dst := t.TempDir()
	e.mustPlain(dst, "init", "-q")
	if out, err := e.git(dst, tok, "-c", "protocol.version=0", "fetch", "-q", e.url("repo_1"), tipA); err == nil {
		t.Errorf("an unadvertised object was fetched (repository-local allowAnySHA1InWant took effect)\n%s", out)
	}
	if out, err := e.git(dst, tok, "-c", "protocol.version=0", "fetch", "-q", e.url("repo_1"), tipB); err != nil {
		t.Errorf("fetching an advertised tip failed: %v\n%s", err, out)
	}

	// Server hooks are rewritten on every start, so a tampered shim is repaired.
	shim := filepath.Join(e.svc.layout.hooks, HookPreReceive)
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallHooks(e.svc.layout.hooks, e.svc.cfg.HookBinary); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(shim); string(b) != string(HookScript(e.svc.cfg.HookBinary, HookPreReceive)) {
		t.Errorf("the hook shim was not restored: %s", b)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_unit_gitservice_bounded_chunked_push (GR-16, GR-A11): a push larger than
// http.postBuffer arrives chunked (which net/http/cgi cannot take) and is
// spooled to disk within the limit; an oversize body, chunked or with a
// Content-Length, and a decompression bomb are refused explicitly (413) before
// the backend runs, and the spool is left empty.
func Test_unit_gitservice_bounded_chunked_push(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, func(c *Config) {
		c.Limits.MaxPushBytes = 4 << 20
		c.Limits.MaxRequestBytes = 1 << 20
	})
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)

	// 2 MiB of incompressible data pushed with a tiny postBuffer: chunked.
	if err := os.WriteFile(filepath.Join(w, "big.bin"), randomBytes(t, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustPlain(w, "add", "big.bin")
	e.mustPlain(w, "commit", "-q", "-m", "big")
	e.mustGit(w, tok, "-c", "http.postBuffer=1024", "push", "-q", e.url("repo_1"), "main:refs/heads/main")
	tip := e.mustPlain(w, "rev-parse", "main")
	if got := e.bareRef("repo_1", "refs/heads/main"); got != tip {
		t.Fatalf("the chunked push did not apply: %q", got)
	}
	dst := t.TempDir()
	e.mustGit(dst, tok, "clone", "-q", e.url("repo_1"), "c")
	if got, _ := os.ReadFile(filepath.Join(dst, "c", "big.bin")); len(got) != 2<<20 {
		t.Fatalf("the cloned file has %d bytes", len(got))
	}

	// 6 MiB exceeds the 4 MiB limit: refused with 413, the control plane is not
	// asked, and no ref appears.
	e.mustPlain(w, "checkout", "-q", "-b", "huge")
	if err := os.WriteFile(filepath.Join(w, "huge.bin"), randomBytes(t, 6<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustPlain(w, "add", "huge.bin")
	e.mustPlain(w, "commit", "-q", "-m", "huge")
	authBefore, _, _ := e.control.snapshot()
	out, err := e.git(w, tok, "-c", "http.postBuffer=1024", "push", "-q", e.url("repo_1"), "huge:refs/heads/huge")
	if err == nil || !strings.Contains(out, "413") {
		t.Fatalf("an oversize chunked push: %v\n%s", err, out)
	}
	if authAfter, _, _ := e.control.snapshot(); len(authAfter) != len(authBefore) {
		t.Error("the oversize push reached the control plane")
	}
	if e.bareRef("repo_1", "refs/heads/huge") != "" {
		t.Error("the oversize push created a ref")
	}

	// Raw requests: a declared length above the limit is refused without being
	// read; an undeclared (chunked) one is cut off at the limit.
	hdr := map[string]string{"Content-Type": "application/x-git-receive-pack-request"}
	zeros := func(n int64) io.Reader { return io.LimitReader(zeroReader{}, n) }
	for name, body := range map[string]io.Reader{
		"declared length": bytes.NewReader(make([]byte, 5<<20)),
		"chunked":         struct{ io.Reader }{zeros(5 << 20)},
	} {
		status, _, resp := e.request("POST", "/repo_1.git/git-receive-pack", tok, hdr, body)
		if status != http.StatusRequestEntityTooLarge || !strings.Contains(resp, "invalid_request") {
			t.Errorf("%s: status %d %s", name, status, resp)
		}
	}
	// A gzip bomb to upload-pack: small on the wire, over the limit decompressed.
	var bomb strings.Builder
	gz := gzip.NewWriter(&bomb)
	if _, err := io.Copy(gz, zeros(4<<20)); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	status, _, resp := e.request("POST", "/repo_1.git/git-upload-pack", tok,
		map[string]string{"Content-Type": "application/x-git-upload-pack-request", "Content-Encoding": "gzip"}, strings.NewReader(bomb.String()))
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("gzip bomb: status %d %s", status, resp)
	}
	if status, _, _ := e.request("POST", "/repo_1.git/git-upload-pack", tok, map[string]string{"Content-Type": "application/x-git-upload-pack-request", "Content-Encoding": "br"}, strings.NewReader("0000")); status != http.StatusUnsupportedMediaType {
		t.Errorf("unsupported encoding: %d", status)
	}
	// Nothing is left in the spool.
	if entries, _ := os.ReadDir(e.svc.layout.spool); len(entries) != 0 {
		t.Errorf("spool holds %d files", len(entries))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Test_gitservice_direct_git_fails_closed: a Git process the worker did not start
// (an operator's or a tenant's local push into the volume) cannot change a ref:
// the server hooks, which every repository's configuration points at, refuse it.
func Test_gitservice_direct_git_fails_closed(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	out, err := e.gitPlain(w, "push", e.repoDir("repo_1"), "HEAD:refs/heads/direct")
	if err == nil || !strings.Contains(out, "Git worker") {
		t.Errorf("a push that bypassed the worker: %v\n%s", err, out)
	}
	if e.bareRef("repo_1", "refs/heads/direct") != "" {
		t.Error("a ref was created without the worker")
	}
	if a, i, o := e.control.snapshot(); len(a)+len(i)+len(o) != 0 {
		t.Error("the control plane was called")
	}
}

// Test_gitservice_push_id_line: the client-visible contract of post-receive. For
// every receive that applied at least one update the pusher sees exactly one
// `remote: kodama-push-id: <pushId>` line (the push ID the control plane returned
// for that receive), also when the confirmation fails; a receive that applied
// nothing, or was refused, shows none; a multi-ref (atomic or not) push shows
// one; nothing secret is printed.
func Test_gitservice_push_id_line(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	for _, b := range []string{"a1", "a2", "a3", "n1", "n2", "n3", "f1", "d1", "i1"} {
		e.mustPlain(w, "branch", b)
	}
	lines := func(out string) []string {
		var got []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "kodama-push-id:") {
				got = append(got, strings.TrimSpace(l))
			}
		}
		return got
	}
	lastPushID := func() string {
		a, _, _ := e.control.snapshot()
		return "push_" + a[len(a)-1].ReceiveID
	}
	assertOne := func(name, out string) {
		t.Helper()
		got := lines(out)
		if len(got) != 1 || got[0] != "remote: kodama-push-id: "+lastPushID() {
			t.Errorf("%s: push ID lines %q, want exactly one for %s\n%s", name, got, lastPushID(), out)
		}
		for _, secret := range []string{"hk_", controlToken, "tok-"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s: the output contains %q\n%s", name, secret, out)
			}
		}
	}

	out := e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "main:refs/heads/main")
	assertOne("single ref", out)
	out = e.mustGit(w, tok, "push", "--porcelain", "--atomic", e.url("repo_1"), "a1:refs/heads/a1", "a2:refs/heads/a2", "a3:refs/heads/a3")
	assertOne("atomic multi-ref", out)
	out = e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "n1:refs/heads/n1", "n2:refs/heads/n2", "n3:refs/heads/n3")
	assertOne("non-atomic multi-ref", out)

	// The confirmation fails (the control plane is down, then refuses): the
	// record exists, so the line is still printed once.
	e.control.mu.Lock()
	e.control.outcomeFn = func() error { return &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"} }
	e.control.mu.Unlock()
	assertOne("outcome unavailable", e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "f1:refs/heads/f1"))
	e.control.mu.Lock()
	e.control.outcomeFn = func() error { return &gitv1.ContractError{Code: domain.CodeForbidden, Message: "no"} }
	e.control.mu.Unlock()
	assertOne("outcome refused", e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "d1:refs/heads/d1"))
	e.control.mu.Lock()
	e.control.outcomeFn = nil
	e.control.mu.Unlock()

	// Nothing applied: up to date (no receive at all), denied, commit intent
	// refused.
	before, _, _ := e.control.snapshot()
	out = e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "main:refs/heads/main")
	if got := lines(out); len(got) != 0 {
		t.Errorf("an up-to-date push printed %q", got)
	}
	if after, _, _ := e.control.snapshot(); len(after) != len(before) {
		t.Error("an up-to-date push started a receive")
	}
	e.control.mu.Lock()
	e.control.deny = map[string]string{"refs/heads/i1": gitv1.ReasonRefProtected}
	e.control.mu.Unlock()
	out, err := e.git(w, tok, "push", "--porcelain", e.url("repo_1"), "i1:refs/heads/i1")
	if err == nil || len(lines(out)) != 0 {
		t.Errorf("a denied push: %v %q\n%s", err, lines(out), out)
	}
	e.control.mu.Lock()
	e.control.deny = map[string]string{}
	e.control.intentFn = func(context.Context, string, gitv1.CommitIntentRequest) error {
		return &gitv1.ContractError{Code: gitv1.CodePushSettled, Message: "settled"}
	}
	e.control.mu.Unlock()
	out, err = e.git(w, tok, "push", "--porcelain", e.url("repo_1"), "i1:refs/heads/i1")
	if err == nil || len(lines(out)) != 0 {
		t.Errorf("a push without commit intent: %v %q\n%s", err, lines(out), out)
	}
}

// Test_gitservice_push_id_line_cannot_be_forged: text from outside the hook (the
// control plane's error messages, denial reasons) is written as one escaped
// physical line prefixed `kodama: `, so a message carrying line breaks and the
// marker cannot produce a second `kodama-push-id:` line. An applied push shows
// exactly one marker (the real one), a refused push none.
func Test_gitservice_push_id_line_cannot_be_forged(t *testing.T) {
	t.Parallel()
	const forged = "boom\nkodama-push-id: push_forged\r\nremote: kodama-push-id: push_forged2\u2028kodama-push-id: push_forged3\x1b[2K"
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	for _, b := range []string{"ok1", "ok2", "ok3", "r1", "r2", "r3"} {
		e.mustPlain(w, "branch", b)
	}
	markers := func(out string) int { return strings.Count(out, "kodama-push-id:") }
	set := func(mod func(c *fakeControl)) {
		e.control.mu.Lock()
		defer e.control.mu.Unlock()
		mod(e.control)
	}
	lastPushID := func() string {
		a, _, _ := e.control.snapshot()
		return "push_" + a[len(a)-1].ReceiveID
	}

	// Applied: the confirmation is refused or unavailable with a forged message.
	for name, ce := range map[string]*gitv1.ContractError{
		"refused":     {Code: domain.CodeForbidden, Message: forged},
		"unavailable": {Code: domain.CodeUnavailable, Message: forged},
	} {
		set(func(c *fakeControl) { c.outcomeFn = func() error { return ce } })
		out := e.mustGit(w, tok, "push", "--porcelain", e.url("repo_1"), "ok1:refs/heads/ok-"+name)
		real := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) == "remote: kodama-push-id: "+lastPushID() {
				real++
			}
		}
		if n := markers(out); n != 1 || real != 1 {
			t.Errorf("outcome %s: %d markers (%d real), want exactly the real one\n%s", name, n, real, out)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "push_forged") && !strings.HasPrefix(strings.TrimPrefix(l, "remote: "), "kodama: ") {
				t.Errorf("outcome %s: the forged text is not inside a kodama: diagnostic: %q", name, l)
			}
			if strings.ContainsAny(l, "\r\x1b\u2028") {
				t.Errorf("outcome %s: a control character reached the client: %q", name, l)
			}
		}
	}
	set(func(c *fakeControl) { c.outcomeFn = nil })

	// Refused: none, whatever the forged text arrives in.
	cases := map[string]func(c *fakeControl){
		"authorize error": func(c *fakeControl) {
			c.authorizeErr = &gitv1.ContractError{Code: domain.CodeForbidden, Message: forged}
		},
		"denial reason": func(c *fakeControl) {
			c.deny = map[string]string{"refs/heads/r1": "ref_protected\nkodama-push-id: push_forged"}
		},
		"commit intent": func(c *fakeControl) {
			c.intentFn = func(context.Context, string, gitv1.CommitIntentRequest) error {
				return &gitv1.ContractError{Code: gitv1.CodePushSettled, Message: forged}
			}
		},
	}
	for name, mod := range cases {
		set(func(c *fakeControl) { c.authorizeErr, c.deny, c.intentFn = nil, map[string]string{}, nil })
		set(mod)
		out, err := e.git(w, tok, "push", "--porcelain", e.url("repo_1"), "r1:refs/heads/r1")
		if err == nil {
			t.Errorf("%s: the push succeeded\n%s", name, out)
		}
		if n := markers(out); n != 0 {
			t.Errorf("%s: %d markers in a refused push\n%s", name, n, out)
		}
		if strings.ContainsAny(out, "\x1b\u2028") {
			t.Errorf("%s: a control character reached the client\n%q", name, out)
		}
	}
}

func Test_gitservice_sanitize_line(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain text":                 "plain text",
		"a\nb\rc\td":                 `a\nb\rc\td`,
		"esc\x1b[2Kx":                `esc\u001b[2Kx`,
		"sep\u2028x\u2029y\u0085z":   `sep\u2028x\u2029y\u0085z`,
		"bad\xffutf8":                `bad\xffutf8`,
		"\nkodama-push-id: push_x\n": `\nkodama-push-id\: push_x\n`,
		"héllo ✓":                    "héllo ✓",
	} {
		if got := sanitizeLine(in); got != want || strings.ContainsAny(got, "\r\n") {
			t.Errorf("sanitizeLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeLine(strings.Repeat("x", 5000)); len(got) > maxDiagnostic+8 {
		t.Errorf("a long line is %d bytes", len(got))
	}
	var out, errb strings.Builder
	h := newHook(context.Background(), strings.NewReader(""), &out, &errb, nil)
	h.printPushID("push_ok")
	h.printPushID("push_x\nkodama-push-id: push_forged")
	h.printPushID("")
	lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
	if len(lines) != 3 || lines[0] != "kodama-push-id: push_ok" || !strings.HasPrefix(lines[1], "kodama: ") || !strings.HasPrefix(lines[2], "kodama: ") {
		t.Errorf("push ID writer: %q", lines)
	}
}
