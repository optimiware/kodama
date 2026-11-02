package integration_test

import (
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
)

type scenario struct {
	name string
	run  func(*testing.T, *rig)
}

var scenarios = []scenario{
	{"refusal_changes_no_ref", refusalChangesNoRef},
	{"applied_push_one_event", appliedPushOneEvent},
	{"lost_confirmation_reconciled_from_the_journal", lostConfirmationReconciled},
	{"partial_receive", partialReceive},
	{"partial_receive_lost_confirmation", partialReceiveLostConfirmation},
	{"seed_is_a_single_ref_transaction", seedSingleRef},
}

func runAll(t *testing.T, open gitpushtest.Open) {
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) { s.run(t, newRig(t, open)) })
	}
}

// Test_integration_gitpush_real_git runs the scenarios on the memory adapter
// with real Git, the real worker and its hooks; it needs only the git binary.
func Test_integration_gitpush_real_git(t *testing.T) { runAll(t, gitpushtest.OpenMem) }

// Test_postgres_gitpush_real_git runs the same scenarios over PostgreSQL (the
// services stack); it skips in the unit layer and without PostgreSQL.
func Test_postgres_gitpush_real_git(t *testing.T) { runAll(t, gitpushtest.OpenPostgres) }

// A refused update changes no ref: the control plane denies the default branch
// (the repository has not opted in to direct pushes), the worker aborts the
// receive in pre-receive, the repository is exactly as before, nothing reached
// the journal and no event was emitted (GR-07, GR-19, GR-A05, GR-A15).
func refusalChangesNoRef(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoMain)
	dir := r.workdir()
	base := r.mustGit(dir, "rev-parse", "HEAD")
	r.prefill(gitpushtest.RepoMain, dir, "main:refs/heads/main")
	next := r.commit(dir, "next", "next\n")
	grant := r.f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	out, err := r.push(dir, gitpushtest.RepoMain, grant, "main:refs/heads/main")
	if err == nil {
		t.Fatalf("a push to the default branch succeeded:\n%s", out)
	}
	if !strings.Contains(out, gitv1.ReasonRefProtected) {
		t.Fatalf("the client was not told why:\n%s", out)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/main"); got != base {
		t.Fatalf("main = %s, want the unchanged %s (the refused push was %s)", got, base, next)
	}
	recs := r.records(gitpushtest.RepoMain)
	if len(recs) != 1 || recs[0].Status != gitv1.PushRefused || recs[0].Entries[0].State != gitv1.EntryRefused ||
		recs[0].Entries[0].Reason == nil || *recs[0].Entries[0].Reason != gitv1.ReasonRefProtected {
		t.Fatalf("records: %+v", recs)
	}
	if j := r.journal(gitpushtest.RepoMain, recs[0].ReceiveID); len(j) != 0 {
		t.Fatalf("a refused receive reached the journal: %+v", j)
	}
	if n := r.events(gitpushtest.RepoMain); n != 0 {
		t.Fatalf("%d events for a refused push", n)
	}
}

// An applied push has its record settled by the worker's own outcome report,
// attributed in the journal to its receive, with exactly one event (GR-09,
// GR-10, GR-A06).
func appliedPushOneEvent(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoMain)
	dir := r.workdir()
	r.prefill(gitpushtest.RepoMain, dir, "main:refs/heads/main")
	head := r.commit(dir, "feature", "feature\n")
	grant := r.f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	out, err := r.push(dir, gitpushtest.RepoMain, grant, "main:refs/heads/feature")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/feature"); got != head {
		t.Fatalf("feature = %s, want %s", got, head)
	}
	recs := r.records(gitpushtest.RepoMain)
	if len(recs) != 1 || recs[0].Status != gitv1.PushApplied || recs[0].Entries[0].Kind != gitv1.KindCreate || recs[0].Entries[0].New != head {
		t.Fatalf("records: %+v", recs)
	}
	if id := pushID(out); id != recs[0].PushID {
		t.Fatalf("the client was told push %q, the record is %q:\n%s", id, recs[0].PushID, out)
	}
	states := map[gitv1.JournalState]int{}
	for _, j := range r.journal(gitpushtest.RepoMain, recs[0].ReceiveID) {
		states[j.State]++
		if j.ReceiveID == nil || *j.ReceiveID != recs[0].ReceiveID || j.Ref != "refs/heads/feature" {
			t.Fatalf("journal entry %+v", j)
		}
	}
	if states[gitv1.JournalPrepared] != 1 || states[gitv1.JournalCommitted] != 1 {
		t.Fatalf("journal states: %v", states)
	}
	if n := r.events(gitpushtest.RepoMain); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	// Reconciliation of a settled record changes nothing and emits nothing.
	if err := r.f.Reconcile(gitpushtest.RepoMain); err != nil {
		t.Fatal(err)
	}
	if n := r.events(gitpushtest.RepoMain); n != 1 {
		t.Fatalf("%d events after reconciliation", n)
	}
}

// A lost post-receive confirmation leaves the entry committing; reconciliation
// settles it from the worker's real journal (own committed entry) and the event
// is emitted once (GR-09, GR-10, GR-A06, GR-A13).
func lostConfirmationReconciled(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoMain)
	dir := r.workdir()
	r.prefill(gitpushtest.RepoMain, dir, "main:refs/heads/main")
	head := r.commit(dir, "feature", "feature\n")
	grant := r.f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	r.dropOutcome.Store(true)
	out, err := r.push(dir, gitpushtest.RepoMain, grant, "main:refs/heads/feature")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/feature"); got != head {
		t.Fatalf("the update is applied in Git whatever the confirmation: %s", got)
	}
	recs := r.records(gitpushtest.RepoMain)
	if len(recs) != 1 || recs[0].Status != gitv1.PushAuthorized || recs[0].Entries[0].State != gitv1.EntryCommitting {
		t.Fatalf("before reconciliation: %+v", recs)
	}
	if n := r.events(gitpushtest.RepoMain); n != 0 {
		t.Fatalf("%d events before the record settled", n)
	}
	if err := r.f.Reconcile(gitpushtest.RepoMain); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec := r.records(gitpushtest.RepoMain)[0]
	if rec.Status != gitv1.PushApplied || rec.Entries[0].State != gitv1.EntryApplied {
		t.Fatalf("after reconciliation: %+v", rec)
	}
	for range 2 {
		if err := r.f.Reconcile(gitpushtest.RepoMain); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.events(gitpushtest.RepoMain); n != 1 {
		t.Fatalf("%d events, want exactly 1", n)
	}
}

// A partial receive applies exactly the refs Git applied: the ref that
// conflicts with an existing ref (a directory/file conflict) fails inside Git
// after the receive was authorized, the other ref applies, the record settles
// applied with the conflicting entry refused, and the event counts one update
// (GR-09, GR-10, GR-A06).
func partialReceive(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoMain)
	dir := r.workdir()
	r.prefill(gitpushtest.RepoMain, dir, "main:refs/heads/main", "main:refs/heads/df")
	head := r.commit(dir, "more", "more\n")
	grant := r.f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	out, err := r.push(dir, gitpushtest.RepoMain, grant, "main:refs/heads/good", "main:refs/heads/df/child")
	if err == nil {
		t.Fatalf("a push with a conflicting ref reported success:\n%s", out)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/good"); got != head {
		t.Fatalf("the applicable ref was not applied (%q):\n%s", got, out)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/df/child"); got != "" {
		t.Fatalf("the conflicting ref exists: %s", got)
	}
	recs := r.records(gitpushtest.RepoMain)
	if len(recs) != 1 {
		t.Fatalf("records: %+v", recs)
	}
	st, re := gitpushtest.States(recs[0]), gitpushtest.Reasons(recs[0])
	if recs[0].Status != gitv1.PushApplied || st["refs/heads/good"] != gitv1.EntryApplied ||
		st["refs/heads/df/child"] != gitv1.EntryRefused || re["refs/heads/df/child"] != gitv1.ReasonReceiveAborted {
		t.Fatalf("record: %+v", recs[0].Entries)
	}
	if n := r.events(gitpushtest.RepoMain); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	evs := r.f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)
	if strings.Count(string(evs[0].Data), `"kind":`) != 1 || !strings.Contains(string(evs[0].Data), `"ref":"refs/heads/good"`) {
		t.Fatalf("the event counts the applied updates only: %s", evs[0].Data)
	}
}

// The same partial receive with the post-receive confirmation lost: Git runs
// the committed hook for the rejected ref too, and the worker journals only
// updates that took effect (T-159), so the real journal says committed for the
// applied ref and aborted for the rejected one. The reconciler settles the
// applied ref, refuses the other and emits one event for the applied record,
// never publishing a ref that was never created (GR-09, GR-A06, GR-A13).
func partialReceiveLostConfirmation(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoMain)
	dir := r.workdir()
	r.prefill(gitpushtest.RepoMain, dir, "main:refs/heads/main", "main:refs/heads/df")
	grant := r.f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)

	r.dropOutcome.Store(true)
	if out, err := r.push(dir, gitpushtest.RepoMain, grant, "main:refs/heads/good", "main:refs/heads/df/child"); err == nil {
		t.Fatalf("a push with a conflicting ref reported success:\n%s", out)
	}
	if err := r.f.Reconcile(gitpushtest.RepoMain); err != nil {
		t.Fatal(err)
	}
	rec := r.records(gitpushtest.RepoMain)[0]
	st := gitpushtest.States(rec)
	if st["refs/heads/good"] != gitv1.EntryApplied || st["refs/heads/df/child"] != gitv1.EntryRefused || rec.Status != gitv1.PushApplied {
		t.Fatalf("record: %+v", rec.Entries)
	}
	if got := r.ref(gitpushtest.RepoMain, "refs/heads/df/child"); got != "" {
		t.Fatalf("the conflicting ref exists: %s", got)
	}
	if n := r.events(gitpushtest.RepoMain); n != 1 {
		t.Fatalf("%d events for one applied record", n)
	}
}

// A seed is a single-ref transaction: a seed receive with another ref is refused
// whole and the repository stays empty; the same grant then seeds the default
// branch once, the repository module is told it is no longer empty and the grant
// cannot be used again (GR-18, GR-A14).
func seedSingleRef(t *testing.T, r *rig) {
	r.initRepo(gitpushtest.RepoEmpty)
	dir := r.workdir()
	grant, err := r.f.Seed(gitpushtest.Engineer, gitpushtest.RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}

	out, err := r.push(dir, gitpushtest.RepoEmpty, grant, "main:refs/heads/main", "main:refs/heads/other")
	if err == nil {
		t.Fatalf("a seed with an extra ref succeeded:\n%s", out)
	}
	if !strings.Contains(out, gitv1.ReasonSeedOnly) {
		t.Fatalf("the client was not told why:\n%s", out)
	}
	if refs := r.refs(gitpushtest.RepoEmpty); refs != "" {
		t.Fatalf("the repository is not empty after a refused seed: %s", refs)
	}
	recs := r.records(gitpushtest.RepoEmpty)
	if len(recs) != 1 || recs[0].Status != gitv1.PushRefused {
		t.Fatalf("records: %+v", recs)
	}

	// The refused receive did not consume the grant.
	head := r.mustGit(dir, "rev-parse", "HEAD")
	if out, err := r.push(dir, gitpushtest.RepoEmpty, grant, "main:refs/heads/main"); err != nil {
		t.Fatalf("the seed: %v\n%s", err, out)
	}
	if got := r.ref(gitpushtest.RepoEmpty, "refs/heads/main"); got != head {
		t.Fatalf("main = %s, want %s: commit IDs are preserved", got, head)
	}
	if r.f.Repos.IsEmpty(gitpushtest.TenantA, gitpushtest.RepoEmpty) {
		t.Fatal("the repository module was not told the repository is no longer empty")
	}
	if n := r.events(gitpushtest.RepoEmpty); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}

	// The grant is single-use, and a repository with refs is never seedable.
	more := r.commit(dir, "second", "second\n")
	out, err = r.push(dir, gitpushtest.RepoEmpty, grant, "main:refs/heads/second")
	if err == nil || !strings.Contains(out, gitv1.ReasonSeedOnly) && !strings.Contains(out, gitv1.ReasonSeedConsumed) {
		t.Fatalf("the seed grant was reused (err %v):\n%s", err, out)
	}
	if got := r.ref(gitpushtest.RepoEmpty, "refs/heads/second"); got != "" {
		t.Fatalf("second = %s (%s)", got, more)
	}
	if _, err := r.f.Seed(gitpushtest.Engineer, gitpushtest.RepoEmpty); err == nil {
		t.Fatal("a seed grant was issued for a repository with refs")
	}
}
