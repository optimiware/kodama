package gitservice

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type sweepMarker struct{}

// lostControl loses the hook's outcome report of the next dropNext receives for
// good (every retry fails as well); a call made under sweepMarker, as the sweep's
// test calls are, goes through to the fake control plane.
type lostControl struct {
	*fakeControl
	mu       sync.Mutex
	dropNext int
	lost     map[string]bool
	swept    []gitv1.OutcomeRequest // reports made under sweepMarker, in order
}

func (l *lostControl) Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	if ctx.Value(sweepMarker{}) != nil {
		l.mu.Lock()
		l.swept = append(l.swept, req)
		l.mu.Unlock()
	}
	if ctx.Value(sweepMarker{}) == nil {
		l.mu.Lock()
		drop := l.lost[req.ReceiveID]
		if !drop && l.dropNext > 0 {
			l.dropNext--
			l.lost[req.ReceiveID] = true
			drop = true
		}
		l.mu.Unlock()
		if drop {
			l.fakeControl.mu.Lock()
			l.fakeControl.outcomes = append(l.fakeControl.outcomes, req)
			l.fakeControl.mu.Unlock()
			return gitv1.OutcomeResponse{}, &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
		}
	}
	return l.fakeControl.Outcome(ctx, pushID, req)
}

func reconcileEnv(t *testing.T) (*testEnv, *lostControl, string, string) {
	t.Helper()
	ctl := &lostControl{fakeControl: newFakeControl(), lost: map[string]bool{}}
	e := newTestEnv(t, func(c *Config) { c.ControlPlane = ctl })
	e.initRepo("repo_1", "main")
	return e, ctl, e.workdir(), e.url("repo_1")
}

// push sends one ref update under a grant and returns the receive's ID.
func push(e *testEnv, ctl *lostControl, work, url, refspec string) string {
	e.t.Helper()
	before, _, _ := ctl.snapshot()
	e.mustGit(work, e.tokenWith("repo_1", true, func(a *gitv1.ForwardingAssertion) { a.GrantID = "gg_7" }), "push", url, refspec)
	after, _, _ := ctl.snapshot()
	if len(after) != len(before)+1 {
		e.t.Fatalf("authorizations %d -> %d", len(before), len(after))
	}
	return after[len(after)-1].ReceiveID
}

// craft appends journal entries without a running receive, as a worker killed
// at the wrong moment leaves them, and returns the receive's ID.
func craft(e *testEnv, state gitv1.JournalState, receive string, u gitv1.RefUpdate, observed string) {
	e.t.Helper()
	dir := e.repoDir("repo_1")
	a := attribution{receiveID: receive}
	var err error
	switch state {
	case gitv1.JournalPrepared:
		_, err = journalAt(dir).appendPrepared(a, []gitv1.RefUpdate{u}, []string{observed})
	default:
		_, err = journalAt(dir).appendOutcome(a, state, []gitv1.RefUpdate{u})
	}
	if err != nil {
		e.t.Fatal(err)
	}
}

func setRef(e *testEnv, ref, value string) {
	e.t.Helper()
	dir := e.repoDir("repo_1")
	args := []string{"-c", "core.hooksPath=" + os.DevNull, "--git-dir=" + dir, "update-ref", ref, value}
	if value == "" {
		args = []string{"-c", "core.hooksPath=" + os.DevNull, "--git-dir=" + dir, "update-ref", "-d", ref}
	}
	e.mustPlain(dir, args...)
}

// sweepOnce runs one reconcile pass at the given acknowledged sequence and
// returns the outcome reports the sweep itself sent.
func sweepOnce(e *testEnv, ctl *lostControl, ack int64) []gitv1.OutcomeRequest {
	e.t.Helper()
	ctl.fakeControl.mu.Lock()
	ctl.fakeControl.assignments = []gitv1.Assignment{{Kind: gitv1.AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: &ack}}
	ctl.fakeControl.mu.Unlock()
	ctl.mu.Lock()
	ctl.swept = nil
	ctl.mu.Unlock()
	if err := e.svc.Reconcile(context.WithValue(e.t.Context(), sweepMarker{}, true)); err != nil {
		e.t.Fatal(err)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return append([]gitv1.OutcomeRequest(nil), ctl.swept...)
}

func outcomesOf(ctl *lostControl, receive string) (out []gitv1.OutcomeRequest) {
	_, _, all := ctl.snapshot()
	for _, o := range all {
		if o.ReceiveID == receive {
			out = append(out, o)
		}
	}
	return out
}

// Test_unit_gitservice_receive_id_carries_grant: the receive ID the worker
// generates names the grant the receive was made under, so the journal alone
// attributes a receive whose outcome was lost.
func Test_unit_gitservice_receive_id_carries_grant(t *testing.T) {
	e, ctl, work, url := reconcileEnv(t)
	id := push(e, ctl, work, url, "main:refs/heads/main")
	if grant, ok := gitv1.GrantOfReceive(id); !ok || grant != "gg_7" {
		t.Fatalf("receive %s carries grant %q, %v", id, grant, ok)
	}
	res := e.journal("repo_1", 0, strp(id), nil)
	if len(res.Entries) < 2 {
		t.Fatalf("journal entries of the receive: %+v", res.Entries)
	}
}

// Test_unit_gitservice_reconcile_rereports_lost_outcome: a receive whose outcome
// report never arrived is re-reported from the journal after the acknowledged
// sequence, with the applied updates, and the window is acknowledged only when
// nothing in it is unreported.
func Test_unit_gitservice_reconcile_rereports_lost_outcome(t *testing.T) {
	e, ctl, work, url := reconcileEnv(t)
	ctl.dropNext = 1
	lost := push(e, ctl, work, url, "main:refs/heads/main")
	head := e.bareRef("repo_1", "refs/heads/main")

	// The hook's first report carried an acknowledgement (nothing earlier is unreported), but it was lost.
	first := outcomesOf(ctl, lost)
	if len(first) == 0 || first[0].JournalSequence == nil {
		t.Fatalf("the live report = %+v", first)
	}

	sweep := func(ack int64) {
		t.Helper()
		ctl.fakeControl.mu.Lock()
		ctl.fakeControl.assignments = []gitv1.Assignment{{Kind: gitv1.AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: &ack}}
		ctl.fakeControl.mu.Unlock()
		if err := e.svc.Reconcile(context.WithValue(t.Context(), sweepMarker{}, true)); err != nil {
			t.Fatal(err)
		}
	}
	sweep(0)
	var swept []gitv1.OutcomeRequest
	for _, o := range outcomesOf(ctl, lost) {
		if len(o.Applied) == 1 && o.Applied[0].Ref == "refs/heads/main" && o.Applied[0].New == head {
			swept = append(swept, o)
		}
	}
	if len(swept) < 2 { // the lost live report and the sweep's
		t.Fatalf("the receive was not re-reported with its applied update: %+v", outcomesOf(ctl, lost))
	}
	last := swept[len(swept)-1]
	headSeq := e.journal("repo_1", 0, nil, strp("refs/heads/main")).HeadSequence
	if last.JournalSequence == nil || *last.JournalSequence != headSeq {
		t.Fatalf("the sweep acknowledges the journal head %d: %+v", headSeq, last)
	}

	// With the head acknowledged there is nothing left to report.
	n := len(outcomesOf(ctl, lost))
	sweep(headSeq)
	if got := len(outcomesOf(ctl, lost)); got != n {
		t.Fatalf("an acknowledged window was reported again (%d reports)", got-n)
	}
}

// Test_unit_gitservice_ack_never_skips_an_unreported_receive: a live outcome
// carries a journal sequence only when every earlier receive of the window is
// reported, so an acknowledgement can never hide a receive that still needs one.
func Test_unit_gitservice_ack_never_skips_an_unreported_receive(t *testing.T) {
	e, ctl, work, url := reconcileEnv(t)

	// 1. Nothing earlier: the live outcome acknowledges its own entries.
	a := push(e, ctl, work, url, "main:refs/heads/main")
	if o := outcomesOf(ctl, a); len(o) != 1 || o[0].JournalSequence == nil {
		t.Fatalf("first receive = %+v", o)
	}
	ackA := *outcomesOf(ctl, a)[0].JournalSequence

	// 2. A receive whose report is lost.
	ctl.dropNext = 1
	e.commit(work, "b.txt", "b\n")
	b := push(e, ctl, work, url, "main:refs/heads/main")

	// 3. The next receive reports fine but must not acknowledge past the lost one.
	e.commit(work, "c.txt", "c\n")
	c := push(e, ctl, work, url, "main:refs/heads/c")
	if o := outcomesOf(ctl, c); len(o) != 1 || o[0].JournalSequence != nil {
		t.Fatalf("a receive after an unreported one carried a journal sequence: %+v", o)
	}
	if ackA < 1 {
		t.Fatal("no acknowledgement of the first receive")
	}

	// 4. The sweep re-reports the lost receive and acknowledges the whole window.
	ack := ackA
	ctl.fakeControl.mu.Lock()
	ctl.fakeControl.assignments = []gitv1.Assignment{{Kind: gitv1.AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: &ack}}
	ctl.fakeControl.mu.Unlock()
	if err := e.svc.Reconcile(context.WithValue(t.Context(), sweepMarker{}, true)); err != nil {
		t.Fatal(err)
	}
	var acknowledged int64
	_, _, all := ctl.snapshot()
	for _, o := range all {
		if o.JournalSequence != nil && *o.JournalSequence > acknowledged {
			acknowledged = *o.JournalSequence
		}
	}
	head := e.journal("repo_1", 0, nil, strp("refs/heads/main")).HeadSequence
	if acknowledged != head {
		t.Fatalf("the sweep acknowledged %d, the journal head is %d", acknowledged, head)
	}
	if reported := outcomesOf(ctl, b); len(reported) < 2 {
		t.Fatalf("the lost receive was not re-reported: %+v", reported)
	}
}

// commits pushes two commits into the repository (without hooks) and returns them.
func commits(e *testEnv, work string) (c1, c2 string) {
	e.t.Helper()
	c1 = e.mustPlain(work, "rev-parse", "HEAD")
	c2 = e.commit(work, "second.txt", "2\n")
	e.seed("repo_1", work, c2+":refs/heads/keep")
	return c1, c2
}

// Test_unit_gitservice_reconcile_recovers_prepared_only_entries: an update with
// no outcome entry is decided by the ref's next value. An applied deletion (the
// ref is absent, not the zero ID) is recovered, an update that a later receive
// overtook is attributed from that receive's observed value, and an outcome the
// evidence cannot decide is left unreported and unacknowledged.
func Test_unit_gitservice_reconcile_recovers_prepared_only_entries(t *testing.T) {
	e, ctl, work, _ := reconcileEnv(t)
	c1, c2 := commits(e, work)
	zero := gitv1.ZeroObjectID(len(c1))

	t.Run("an applied deletion", func(t *testing.T) {
		setRef(e, "refs/heads/gone", c1)
		r := gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, r, gitv1.RefUpdate{Ref: "refs/heads/gone", Old: c1, New: zero}, c1)
		setRef(e, "refs/heads/gone", "") // Git applied the delete; the committed hook never ran
		got := sweepOnce(e, ctl, 0)
		if len(got) != 1 || got[0].ReceiveID != r || len(got[0].Applied) != 1 || got[0].Applied[0] != (gitv1.RefUpdate{Ref: "refs/heads/gone", Old: c1, New: zero}) {
			t.Fatalf("the deletion was not recovered: %+v", got)
		}
		if got[0].JournalSequence == nil || *got[0].JournalSequence != 1 {
			t.Fatalf("the settled window is acknowledged: %+v", got[0].JournalSequence)
		}
	})

	t.Run("a deletion that did not apply is not reported as applied", func(t *testing.T) {
		setRef(e, "refs/heads/kept", c1)
		r := gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, r, gitv1.RefUpdate{Ref: "refs/heads/kept", Old: c1, New: zero}, c1)
		got := sweepOnce(e, ctl, 1)
		if len(got) != 1 || len(got[0].Applied) != 0 {
			t.Fatalf("an unapplied deletion: %+v", got)
		}
	})

	t.Run("an update overtaken by a later receive", func(t *testing.T) {
		first, second := gitv1.NewReceiveID("gg_7"), gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, first, gitv1.RefUpdate{Ref: "refs/heads/moving", Old: zero, New: c1}, zero)
		craft(e, gitv1.JournalPrepared, second, gitv1.RefUpdate{Ref: "refs/heads/moving", Old: c1, New: c2}, c1)
		setRef(e, "refs/heads/moving", c2) // the current value is the second receive's: it is not the first's new value
		got := sweepOnce(e, ctl, 2)
		if len(got) != 2 || got[0].ReceiveID != first || got[1].ReceiveID != second {
			t.Fatalf("both receives are reported in order: %+v", got)
		}
		if len(got[0].Applied) != 1 || got[0].Applied[0].New != c1 || len(got[1].Applied) != 1 || got[1].Applied[0].New != c2 {
			t.Fatalf("applied updates: %+v / %+v", got[0].Applied, got[1].Applied)
		}
		// A receive whose update a later transaction found unchanged did not apply.
		third, fourth := gitv1.NewReceiveID("gg_7"), gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, third, gitv1.RefUpdate{Ref: "refs/heads/stay", Old: zero, New: c1}, zero)
		craft(e, gitv1.JournalPrepared, fourth, gitv1.RefUpdate{Ref: "refs/heads/stay", Old: zero, New: c2}, zero)
		setRef(e, "refs/heads/stay", c2)
		got = sweepOnce(e, ctl, 4)
		if len(got) != 2 || len(got[0].Applied) != 0 || len(got[1].Applied) != 1 {
			t.Fatalf("the overtaken update that never applied: %+v", got)
		}
	})

	t.Run("an undecidable outcome stays unacknowledged", func(t *testing.T) {
		r := gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, r, gitv1.RefUpdate{Ref: "refs/heads/odd", Old: zero, New: c1}, zero)
		setRef(e, "refs/heads/odd", c2) // neither the value before nor the new value, and no later transaction
		after := gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, after, gitv1.RefUpdate{Ref: "refs/heads/later", Old: zero, New: c1}, zero)
		craft(e, gitv1.JournalCommitted, after, gitv1.RefUpdate{Ref: "refs/heads/later", Old: zero, New: c1}, "")
		got := sweepOnce(e, ctl, 6)
		for _, o := range got {
			if o.ReceiveID == r || o.JournalSequence != nil {
				t.Fatalf("an ambiguous receive was reported or acknowledged past: %+v", o)
			}
		}
		if len(got) != 0 {
			t.Fatalf("nothing past an ambiguous receive is reported either: %+v", got)
		}
		// Once a later transaction proves what the ref was, the receive is decided.
		later := gitv1.NewReceiveID("gg_7")
		craft(e, gitv1.JournalPrepared, later, gitv1.RefUpdate{Ref: "refs/heads/odd", Old: zero, New: c2}, zero) // it found the ref still absent
		got = sweepOnce(e, ctl, 6)
		if len(got) == 0 || got[0].ReceiveID != r || len(got[0].Applied) != 0 {
			t.Fatalf("the evidence now decides the receive as not applied: %+v", got)
		}
	})
}

// Test_unit_gitservice_reconcile_never_reports_a_partial_receive: a multi-ref
// receive that finishes between the sweep's page read and its liveness check is
// reported whole, and one still running is not reported at all.
func Test_unit_gitservice_reconcile_never_reports_a_partial_receive(t *testing.T) {
	e, ctl, work, _ := reconcileEnv(t)
	c1, _ := commits(e, work)
	zero := gitv1.ZeroObjectID(len(c1))
	// A receive of two refs, each its own ref transaction, as Git runs a
	// non-atomic push with a directory/file conflict: registered as running, with
	// the first ref's transaction complete and the second's only prepared.
	rc := e.svc.startReceive("repo_1", "gg_7", e.repoDir("repo_1"))
	a := gitv1.RefUpdate{Ref: "refs/heads/a", Old: zero, New: c1}
	b := gitv1.RefUpdate{Ref: "refs/heads/b", Old: zero, New: c1}
	craft(e, gitv1.JournalPrepared, rc.id, a, zero)
	craft(e, gitv1.JournalCommitted, rc.id, a, "")
	craft(e, gitv1.JournalPrepared, rc.id, b, zero)
	setRef(e, "refs/heads/a", c1)

	// Running: nothing is reported, whatever the journal shows so far.
	if got := sweepOnce(e, ctl, 0); len(got) != 0 {
		t.Fatalf("a running receive was reported: %+v", got)
	}
	// The receive ends between the sweep's page read and its liveness check: the
	// second transaction commits and the receive finishes. Its complete evidence
	// is read after the liveness check, so the report holds both refs.
	e.svc.sweepHook = func(string) {
		setRef(e, "refs/heads/b", c1)
		craft(e, gitv1.JournalCommitted, rc.id, b, "")
		rc.finish()
	}
	got := sweepOnce(e, ctl, 0)
	e.svc.sweepHook = nil
	if len(got) != 1 || got[0].ReceiveID != rc.id || len(got[0].Applied) != 2 {
		t.Fatalf("the finished receive must be reported whole: %+v", got)
	}
	for _, o := range outcomesOf(ctl, rc.id) {
		if len(o.Applied) == 1 {
			t.Fatalf("a partial report of the receive: %+v", o)
		}
	}
}

// Test_unit_gitservice_reconcile_backlog_makes_progress: a backlog larger than a
// page is settled page by page, each pass acknowledging the prefix it settled,
// and a receive that straddles a page boundary is read whole.
func Test_unit_gitservice_reconcile_backlog_makes_progress(t *testing.T) {
	e, ctl, work, url := reconcileEnv(t)
	e.svc.sweepEntries = 5 // two entries per receive: a receive straddles every second page
	ctl.dropNext = 6
	var receives []string
	for i := 0; i < 6; i++ {
		e.commit(work, "f"+strconv.Itoa(i), "x\n")
		receives = append(receives, push(e, ctl, work, url, "main:refs/heads/r"+strconv.Itoa(i)))
	}
	head := e.journal("repo_1", 0, nil, strp("refs/heads/r0")).HeadSequence
	if head != 12 {
		t.Fatalf("journal head = %d", head)
	}
	reported := map[string]int{}
	var ack int64
	passes := 0
	for ; ack < head && passes < 10; passes++ {
		before := ack
		for _, o := range sweepOnce(e, ctl, ack) {
			reported[o.ReceiveID]++
			if len(o.Applied) != 1 {
				t.Fatalf("a receive was reported truncated: %+v", o)
			}
			if o.JournalSequence != nil && *o.JournalSequence > ack {
				ack = *o.JournalSequence
			}
		}
		if ack == before {
			t.Fatalf("pass %d made no progress at ack %d", passes, ack)
		}
	}
	if ack != head || passes > 4 {
		t.Fatalf("ack %d of %d after %d passes", ack, head, passes)
	}
	for _, r := range receives {
		if reported[r] != 1 {
			t.Errorf("receive %s reported %d times", r, reported[r])
		}
	}
	// A backlog of server operations alone has no report to carry its
	// acknowledgement, yet the sweep still moves past it.
	for i := 0; i < 12; i++ {
		_, err := journalAt(e.repoDir("repo_1")).appendPrepared(attribution{operationID: "op_" + strconv.Itoa(i)},
			[]gitv1.RefUpdate{{Ref: "refs/kodama/runs/r/" + strconv.Itoa(i+1), Old: gitv1.ZeroObjectID(40), New: strings.Repeat("1", 40)}}, []string{gitv1.ZeroObjectID(40)})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		sweepOnce(e, ctl, head)
	}
	e.svc.mu.Lock()
	cursor := e.svc.ackOf("repo_1").cursor
	e.svc.mu.Unlock()
	if cursor != head+12 {
		t.Fatalf("the sweep cursor %d did not pass the operation entries (want %d)", cursor, head+12)
	}
}
