package gitservice

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// partialReceive runs one non-atomic receive against a repository that already
// holds refs/heads/keep/x: creating refs/heads/keep is a directory/file conflict
// Git rejects while the other refs of the same receive apply.
func partialReceive(t *testing.T) (e *testEnv, receiveID, tip string, out string) {
	t.Helper()
	e = newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tok := e.token("repo_1", true)
	tip = e.mustPlain(w, "rev-parse", "main")
	e.mustPlain(w, "branch", "ok1")
	e.mustPlain(w, "branch", "ok2")
	e.mustPlain(w, "branch", "keep/x")
	e.mustGit(w, tok, "push", "-q", e.url("repo_1"), "keep/x:refs/heads/keep/x")
	before, _, _ := e.control.snapshot()

	var err error
	out, err = e.git(w, tok, "push", "--porcelain", e.url("repo_1"),
		"ok1:refs/heads/ok1", "main:refs/heads/keep", "ok2:refs/heads/ok2")
	if err == nil {
		t.Fatalf("a push with a directory/file conflict succeeded\n%s", out)
	}
	auth, _, _ := e.control.snapshot()
	if len(auth) != len(before)+1 {
		t.Fatalf("one more receive expected: %d -> %d\n%s", len(before), len(auth), out)
	}
	return e, auth[len(auth)-1].ReceiveID, tip, out
}

// Test_unit_gitservice_committed_only_for_applied_refs (GR-09): Git runs the
// reference-transaction committed hook for refs it rejected inside a non-atomic
// receive; the worker verifies each update under the repository's ref state and
// journals committed only for a ref that holds the new value, aborted otherwise.
func Test_unit_gitservice_committed_only_for_applied_refs(t *testing.T) {
	t.Parallel()
	e, rid, tip, out := partialReceive(t)

	for _, ref := range []string{"refs/heads/ok1", "refs/heads/ok2"} {
		if got := e.bareRef("repo_1", ref); got != tip {
			t.Fatalf("%s is %q, want %s\n%s", ref, got, tip, out)
		}
	}
	if got := exactRef(e, "repo_1", "refs/heads/keep"); got != "" {
		t.Fatalf("the conflicting ref exists: %s", got)
	}

	states := map[string][]gitv1.JournalState{}
	for _, en := range receiveEntries(e.journalAll("repo_1"), rid) {
		states[en.Ref] = append(states[en.Ref], en.State)
	}
	for _, ref := range []string{"refs/heads/ok1", "refs/heads/ok2"} {
		if s := states[ref]; len(s) != 2 || s[0] != gitv1.JournalPrepared || s[1] != gitv1.JournalCommitted {
			t.Errorf("%s: journal states %v, want [prepared committed]", ref, s)
		}
	}
	for ref, s := range states {
		for _, st := range s {
			if st == gitv1.JournalCommitted && exactRef(e, "repo_1", ref) != tip {
				t.Errorf("%s is journaled committed but does not hold the new value", ref)
			}
		}
	}
	for _, en := range receiveEntries(e.journalAll("repo_1"), rid) {
		if en.Ref == "refs/heads/keep" && en.State == gitv1.JournalCommitted {
			t.Errorf("the rejected ref is journaled committed: %+v", en)
		}
	}
}

// Test_unit_gitservice_partial_receive_journal (GR-09): a partially rejected
// non-atomic receive journals committed for the applied refs, never committed for
// the rejected one (aborted, or no outcome when Git never prepared it), and
// post-receive lists exactly the applied refs.
func Test_unit_gitservice_partial_receive_journal(t *testing.T) {
	t.Parallel()
	e, rid, tip, _ := partialReceive(t)

	_, _, outcomes := e.control.snapshot()
	var last *gitv1.OutcomeRequest
	for i := range outcomes {
		if outcomes[i].ReceiveID == rid {
			last = &outcomes[i]
		}
	}
	if last == nil {
		t.Fatalf("no outcome for receive %s: %+v", rid, outcomes)
	}
	applied := map[string]bool{}
	for _, u := range last.Applied {
		applied[u.Ref] = true
		if u.New != tip {
			t.Errorf("applied %+v", u)
		}
	}
	if len(applied) != 2 || !applied["refs/heads/ok1"] || !applied["refs/heads/ok2"] || applied["refs/heads/keep"] {
		t.Errorf("post-receive listed %v, want exactly ok1 and ok2", applied)
	}

	committed, rejected := map[string]bool{}, map[string][]gitv1.JournalState{}
	for _, en := range receiveEntries(e.journalAll("repo_1"), rid) {
		switch {
		case en.State == gitv1.JournalCommitted:
			committed[en.Ref] = true
		case en.Ref == "refs/heads/keep":
			rejected[en.Ref] = append(rejected[en.Ref], en.State)
		}
	}
	if len(committed) != 2 || !committed["refs/heads/ok1"] || !committed["refs/heads/ok2"] {
		t.Errorf("committed refs %v, want exactly ok1 and ok2", committed)
	}
	// Whatever Git did for the rejected ref (prepared it or not), it never
	// reached committed, and a prepared entry is closed by aborted.
	if s := rejected["refs/heads/keep"]; len(s) != 0 && (len(s) != 2 || s[0] != gitv1.JournalPrepared || s[1] != gitv1.JournalAborted) {
		t.Errorf("rejected ref journal states %v, want none or [prepared aborted]", s)
	}
}

// The committed hook itself: updates that Git reports committed but whose refs
// do not hold the new value are journaled aborted, and a delete is verified by
// the ref's absence.
func Test_unit_gitservice_committed_hook_verifies_each_update(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	tip := e.mustPlain(w, "rev-parse", "main")
	e.seed("repo_1", w, "main:refs/heads/applied", "main:refs/heads/gone")
	dir := e.repoDir("repo_1")

	rid := "rcv_verify"
	a := attribution{receiveID: rid}
	ups := []gitv1.RefUpdate{
		{Ref: "refs/heads/applied", Old: zeroSHA, New: tip},                                 // exists with new: applied
		{Ref: "refs/heads/never", Old: zeroSHA, New: tip},                                   // absent: rejected
		{Ref: "refs/heads/gone", Old: tip, New: zeroSHA},                                    // still exists: rejected delete
		{Ref: "refs/heads/deleted", Old: tip, New: zeroSHA},                                 // absent: applied delete
		{Ref: "refs/heads/applied-other", Old: zeroSHA, New: strings.Repeat("b", len(tip))}, // absent: rejected
	}
	obs := make([]string, len(ups))
	for i := range obs {
		obs[i] = zeroSHA
	}
	if _, err := journalAt(dir).appendPrepared(a, ups, obs); err != nil {
		t.Fatal(err)
	}
	runCommittedHook(t, dir, rid, ups)
	want := map[string]gitv1.JournalState{
		"refs/heads/applied": gitv1.JournalCommitted, "refs/heads/never": gitv1.JournalAborted,
		"refs/heads/gone": gitv1.JournalAborted, "refs/heads/deleted": gitv1.JournalCommitted,
		"refs/heads/applied-other": gitv1.JournalAborted,
	}
	got := map[string]gitv1.JournalState{}
	for _, en := range receiveEntries(e.journalAll("repo_1"), rid) {
		if en.State != gitv1.JournalPrepared {
			got[en.Ref] = en.State
		}
	}
	for ref, st := range want {
		if got[ref] != st {
			t.Errorf("%s: outcome %q, want %q", ref, got[ref], st)
		}
	}
}

// Test_unit_gitservice_journal_unreadable_is_definite: the journal read answers
// an affirmatively unreadable journal (corrupt, undecodable, a sequence gap or a
// file that is missing from an initialized repository) with the definite
// 409 journal_unreadable, never the transient 503 unavailable; an I/O failure
// that says nothing about the journal stays 503 unavailable.
func Test_unit_gitservice_journal_unreadable_is_definite(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	path := filepath.Join(e.repoDir("repo_1"), "kodama", "journal.jsonl")
	req := gitv1.ReadJournalRequest{RepositoryID: "repo_1", ReceiveID: strp("rcv_1"), Limit: 10}
	read := func(wantStatus int, wantCode domain.ErrorCode) {
		t.Helper()
		rpcError(e, "/repositories/repo_1/journal", req, wantStatus, wantCode)
	}

	// Intact (empty) journal reads.
	if r := e.journal("repo_1", 0, strp("rcv_1"), nil); len(r.Entries) != 0 || r.HeadSequence != 0 {
		t.Fatalf("a fresh journal: %+v", r)
	}

	for name, content := range map[string]string{
		"undecodable":   "garbage\n",
		"not an object": "[1,2]\n",
		"sequence gap":  gapEntry(t),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(*testing.T) { read(409, gitv1.CodeJournalUnreadable) })
	}

	// Missing where it must exist.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	read(409, gitv1.CodeJournalUnreadable)

	// A file that cannot be read for a reason unrelated to its content is
	// transient: the journal was not found damaged.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	read(503, domain.CodeUnavailable)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Repaired: reads succeed again and the other error classes are unchanged.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.journal("repo_1", 0, strp("rcv_1"), nil); len(r.Entries) != 0 {
		t.Fatalf("after repair: %+v", r)
	}
	rpcError(e, "/repositories/missing/journal", gitv1.ReadJournalRequest{RepositoryID: "missing", ReceiveID: strp("rcv_1"), Limit: 10}, 404, domain.CodeNotFound)
}

// gapEntry is a valid journal line whose sequence skips 1.
func gapEntry(t *testing.T) string {
	t.Helper()
	obs := zeroSHA
	rid := "rcv_1"
	en := gitv1.JournalEntry{Sequence: 2, Ref: "refs/heads/main", Old: zeroSHA, New: strings.Repeat("a", 40), Observed: &obs, State: gitv1.JournalPrepared, ReceiveID: &rid}
	b, err := json.Marshal(en)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// exactRef reads one ref by its exact name ("" when absent): bareRef lists with a
// pattern, which also matches the refs below a directory of the same name.
func exactRef(e *testEnv, repo, ref string) string {
	e.t.Helper()
	dir := e.repoDir(repo)
	out, err := e.gitPlain(dir, "--git-dir="+dir, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		e.t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	return exactRefValue([]byte(out), ref)
}

// runCommittedHook runs the reference-transaction hook in its committed state as
// a separate process, as Git starts it, for the transaction of receive rid.
func runCommittedHook(t *testing.T, dir, rid string, ups []gitv1.RefUpdate) {
	t.Helper()
	var in strings.Builder
	for _, u := range ups {
		in.WriteString(u.Old + " " + u.New + " " + u.Ref + "\n")
	}
	env := append(os.Environ(),
		envHookKind+"="+kindReceive, envReceiveID+"="+rid, envRepoDir+"="+dir, envHookAddr+"=127.0.0.1:1", envHookToken+"=x")
	cmd := exec.Command(testHookBinary(t), "git-hook", HookReferenceTransaction, "committed")
	cmd.Stdin, cmd.Env = strings.NewReader(in.String()), env
	lock, err := os.Open(dir) // descriptor 3 of the hook, as the worker's generation lock is
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd.ExtraFiles = []*os.File{lock}
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook: %v %q", err, out)
	}
}

// Test_unit_gitservice_committed_hook_concurrent_receives: A's committed hook
// runs late, after other receives changed the same ref (Git has released its ref
// locks by then and receives hold the gate shared). Each scenario builds the
// journal and ref state deterministically: the transactions' prepared entries,
// their ref changes, then A's delayed hook. A's journal evidence and the
// control plane's decision (gitv1.Settle) must stay right.
func Test_unit_gitservice_committed_hook_concurrent_receives(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	x := e.mustPlain(w, "rev-parse", "main")
	y := e.commit(w, "y", "y\n")
	z := e.commit(w, "z", "z\n")
	e.seed("repo_1", w, x+":refs/heads/obj-x", y+":refs/heads/obj-y", z+":refs/heads/obj-z")
	dir := e.repoDir("repo_1")

	move := func(ref, value string) {
		t.Helper()
		e.mustPlain(dir, "-c", "core.hooksPath="+os.DevNull, "--git-dir="+dir, "update-ref", ref, value) // outside the worker: no hooks
	}
	prepare := func(rid, ref, old, newV, observed string) []gitv1.RefUpdate {
		t.Helper()
		ups := []gitv1.RefUpdate{{Ref: ref, Old: old, New: newV}}
		if _, err := journalAt(dir).appendPrepared(attribution{receiveID: rid}, ups, []string{observed}); err != nil {
			t.Fatal(err)
		}
		return ups
	}
	outcomeOf := func(rid, ref string) []gitv1.JournalState {
		var states []gitv1.JournalState
		for _, en := range receiveEntries(e.journalAll("repo_1"), rid) {
			if en.Ref == ref && en.State != gitv1.JournalPrepared {
				states = append(states, en.State)
			}
		}
		return states
	}
	settle := func(rid, ref, old, newV string) gitv1.Decision {
		t.Helper()
		all := e.journalAll("repo_1")
		cur := e.bareRef("repo_1", ref)
		if cur == "" {
			cur = zeroSHA
		}
		d, err := gitv1.Settle(gitv1.SettleEntry{ReceiveID: rid, Ref: ref, Old: old, New: newV, State: gitv1.EntryCommitting},
			gitv1.SettleEvidence{Liveness: gitv1.LivenessEnded, JournalReadable: true, JournalHead: int64(len(all)), Journal: all,
				Current: &gitv1.CurrentRef{Commit: cur, Quiescent: true}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	want := func(t *testing.T, got gitv1.Decision, r gitv1.Resolution, reason string) {
		t.Helper()
		if got.Resolution != r || got.Reason != reason {
			t.Errorf("settlement %+v, want %s %q", got, r, reason)
		}
	}

	t.Run("another receive changes the ref A applied", func(t *testing.T) {
		const ref = "refs/heads/s1"
		move(ref, x)
		upsA := prepare("rcv_s1a", ref, x, y, x)
		move(ref, y) // A applied X -> Y
		upsB := prepare("rcv_s1b", ref, y, z, y)
		move(ref, z) // B applied Y -> Z before A's hook runs
		runCommittedHook(t, dir, "rcv_s1a", upsA)
		if got := outcomeOf("rcv_s1a", ref); len(got) != 0 {
			t.Errorf("A journaled %v although the ref moved on: only prepared evidence is sound", got)
		}
		want(t, settle("rcv_s1a", ref, x, y), gitv1.ResolveApplied, "")
		runCommittedHook(t, dir, "rcv_s1b", upsB)
		if got := outcomeOf("rcv_s1b", ref); len(got) != 1 || got[0] != gitv1.JournalCommitted {
			t.Errorf("B (the latest transaction) journaled %v, want committed", got)
		}
		want(t, settle("rcv_s1b", ref, y, z), gitv1.ResolveApplied, "")
	})

	t.Run("another receive installs the value A's rejected update asked for", func(t *testing.T) {
		const ref = "refs/heads/s2"
		move(ref, x)
		upsA := prepare("rcv_s2a", ref, x, y, x) // A is rejected: the ref stays at X
		upsB := prepare("rcv_s2b", ref, x, y, x) // the identical tuple, another receive
		move(ref, y)                             // B applies X -> Y
		runCommittedHook(t, dir, "rcv_s2a", upsA)
		if got := outcomeOf("rcv_s2a", ref); len(got) != 0 {
			t.Errorf("A's rejected update journaled %v because B installed its value", got)
		}
		want(t, settle("rcv_s2a", ref, x, y), gitv1.ResolveRefused, gitv1.ReasonRefUnchanged)
		runCommittedHook(t, dir, "rcv_s2b", upsB)
		if got := outcomeOf("rcv_s2b", ref); len(got) != 1 || got[0] != gitv1.JournalCommitted {
			t.Errorf("B journaled %v, want committed", got)
		}
		want(t, settle("rcv_s2b", ref, x, y), gitv1.ResolveApplied, "")
	})

	t.Run("ABA: the ref returns to the value A started from", func(t *testing.T) {
		const ref = "refs/heads/s3"
		move(ref, x)
		upsA := prepare("rcv_s3a", ref, x, y, x)
		move(ref, y) // A applied X -> Y
		prepare("rcv_s3b", ref, y, x, y)
		move(ref, x) // B applied Y -> X: the ref holds A's old value again
		runCommittedHook(t, dir, "rcv_s3a", upsA)
		if got := outcomeOf("rcv_s3a", ref); len(got) != 0 {
			t.Errorf("A journaled %v although the ref holds its old value again after B", got)
		}
		want(t, settle("rcv_s3a", ref, x, y), gitv1.ResolveApplied, "")
	})

	t.Run("no later transaction: the value decides", func(t *testing.T) {
		const ref = "refs/heads/s4"
		move(ref, x)
		upsA := prepare("rcv_s4a", ref, x, y, x) // rejected: the ref still holds the old value
		runCommittedHook(t, dir, "rcv_s4a", upsA)
		if got := outcomeOf("rcv_s4a", ref); len(got) != 1 || got[0] != gitv1.JournalAborted {
			t.Errorf("rejected update journaled %v, want aborted", got)
		}
		want(t, settle("rcv_s4a", ref, x, y), gitv1.ResolveRefused, gitv1.ReasonReceiveAborted)

		upsC := prepare("rcv_s4c", ref, x, z, x)
		move(ref, y) // a value neither old nor new (written outside any journaled transaction)
		runCommittedHook(t, dir, "rcv_s4c", upsC)
		if got := outcomeOf("rcv_s4c", ref); len(got) != 0 {
			t.Errorf("an unexplained value journaled %v, want nothing", got)
		}
	})
}
