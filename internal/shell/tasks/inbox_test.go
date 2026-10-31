package tasks

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func (r *rig) inbox(principal string) []InboxEntry {
	r.t.Helper()
	out, _, err := r.svc.Inbox(r.ctx, InboxRequest{Caller: r.caller(scopeA, principal)})
	if err != nil {
		r.t.Fatalf("inbox of %s: %v", principal, err)
	}
	return out
}

func (r *rig) resolve(principal, key string, e InboxEntry, outcome string) (runtime.SignalResult, error) {
	return r.svc.Resolve(r.ctx, ResolveRequest{Caller: r.caller(scopeA, principal), TaskID: e.Task.ID, RunID: e.RunID, StepKey: e.StepKey,
		WaitID: e.WaitID, ExpectedVersion: e.WaitVersion, Key: key, Outcome: outcome})
}

// Test_unit_tasks_inbox_visibility holds the role inbox to WM-19 and WM-A08: only
// assignment grants an entry, correlation grants nothing, the role is checked now,
// and the entry is bound to the exact wait and version.
func Test_unit_tasks_inbox_visibility(t *testing.T) {
	r := newRig(t)
	task, w := r.waiting(r.task("review me"))
	run := task.Progress.RunID

	t.Run("the assigned role sees one entry bound to the exact wait", func(t *testing.T) {
		got := r.inbox("alice")
		if len(got) != 1 {
			t.Fatalf("inbox = %+v", got)
		}
		e := got[0]
		if e.Task.ID != task.ID || e.RunID != run || e.StepKey != "review" || e.WaitID != w.ID || e.WaitVersion != w.Version ||
			e.Assignment != "reviewer" || e.Request != "please review c1" || e.Correlation["candidate"] != "c1" ||
			e.Input != "approval" || len(e.Outcomes) != 2 || e.Deadline.IsZero() {
			t.Fatalf("entry = %+v (wait %+v)", e, w)
		}
	})

	t.Run("another role, a non-member and another project see nothing", func(t *testing.T) {
		for _, p := range []string{"view", "ops", "carol", "admin"} {
			if got := r.inbox(p); len(got) != 0 {
				t.Errorf("%s sees %+v", p, got)
			}
		}
		if _, _, err := r.svc.Inbox(r.ctx, InboxRequest{Caller: r.caller(scopeA, "stranger")}); !errors.Is(err, ErrNotFound) {
			t.Errorf("a non-member: %v", err)
		}
		r.auth.grant(scopeC, "alice", RoleReviewer)
		if got, _, err := r.svc.Inbox(r.ctx, InboxRequest{Caller: r.caller(scopeC, "alice")}); err != nil || len(got) != 0 {
			t.Errorf("alice in another project: %v %v", got, err)
		}
	})

	t.Run("a caller who knows the identities and correlation but lacks the role cannot resolve", func(t *testing.T) {
		e := r.inbox("alice")[0]
		for _, p := range []string{"view", "ops", "carol", "admin"} {
			res, err := r.resolve(p, "k-"+p, e, "approved")
			if !errors.Is(err, ErrNotFound) || res.Applied {
				t.Errorf("%s resolved: %+v, %v", p, res, err)
			}
		}
		// A refused caller leaves no signal record, so its key is still free.
		lookup, _ := r.runs.LookupSignal(r.ctx, runtime.SignalIdentity{Scope: scopeA, Caller: "ops", Key: "k-ops"})
		if lookup.Found() {
			t.Errorf("an unauthorized attempt was recorded: %+v", lookup)
		}
	})

	t.Run("a user who loses the role stops seeing the wait at once", func(t *testing.T) {
		r.auth.grant(scopeA, "dave", RoleReviewer)
		if len(r.inbox("dave")) != 1 {
			t.Fatal("dave should see the wait")
		}
		r.auth.grant(scopeA, "dave", RoleViewer)
		if got := r.inbox("dave"); len(got) != 0 {
			t.Fatalf("dave still sees %+v", got)
		}
		e := r.inbox("alice")[0]
		if _, err := r.resolve("dave", "kd", e, "approved"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("dave resolves without the role: %v", err)
		}
	})

	t.Run("the entry comes from the wait's records, not the projection alone", func(t *testing.T) {
		// A projection that still says waiting after the wait was decided yields no entry.
		r2 := newRig(t)
		task2, w2 := r2.waiting(r2.task("lagging"))
		e := r2.inbox("alice")[0]
		if res, err := r2.resolve("alice", "k", e, "approved"); err != nil || !res.Applied {
			t.Fatalf("resolve = %+v, %v", res, err)
		}
		if got := r2.get(task2.ID).Progress; got.Status != StatusWaiting {
			t.Fatalf("setup: the projection should lag: %+v", got)
		}
		if got := r2.inbox("alice"); len(got) != 0 {
			t.Fatalf("a decided wait is still listed: %+v", got)
		}
		_ = w2
	})

	t.Run("a wait with no assignment has no inbox entry and is never resolved through it", func(t *testing.T) {
		r3 := newRig(t)
		r3.plan.plan = reviewPlan("")
		task3, _ := r3.waiting(r3.task("correlated only"))
		for _, p := range []string{"alice", "admin", "eng"} {
			if got := r3.inbox(p); len(got) != 0 {
				t.Errorf("%s sees an unassigned wait: %+v", p, got)
			}
		}
		rec, _ := r3.runs.Load(r3.ctx, scopeA, task3.Progress.RunID)
		w3 := rec.Waits[indexWait(rec, "review")]
		_, err := r3.svc.Resolve(r3.ctx, ResolveRequest{Caller: r3.caller(scopeA, "alice"), TaskID: task3.ID, RunID: task3.Progress.RunID,
			StepKey: "review", WaitID: w3.ID, ExpectedVersion: w3.Version, Key: "k", Outcome: "approved"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("resolve through the inbox = %v", err)
		}
	})

	t.Run("the inbox pages", func(t *testing.T) {
		r4 := newRig(t)
		for _, n := range []string{"one", "two", "three"} {
			r4.waiting(r4.task(n))
		}
		first, next, err := r4.svc.Inbox(r4.ctx, InboxRequest{Caller: r4.caller(scopeA, "alice"), Limit: 2})
		if err != nil || len(first) != 2 || next == "" {
			t.Fatalf("first page = %d, %q, %v", len(first), next, err)
		}
		rest, next2, err := r4.svc.Inbox(r4.ctx, InboxRequest{Caller: r4.caller(scopeA, "alice"), Limit: 2, After: next})
		if err != nil || len(rest) != 1 || next2 != "" || rest[0].Task.ID == first[0].Task.ID {
			t.Fatalf("second page = %d, %q, %v", len(rest), next2, err)
		}
	})
}

// Test_unit_tasks_resolution holds resolution to WM-20 and WM-22 (WM-A09 to A12):
// one decision under a caller key through the kernel signal path, recorded with
// its actor, never by changing the task.
func Test_unit_tasks_resolution(t *testing.T) {
	t.Run("a resolution records the immutable input and actor against the exact run, step and wait", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("decide"))
		e := r.inbox("alice")[0]
		before := r.get(task.ID)
		res, err := r.svc.Resolve(r.ctx, ResolveRequest{Caller: r.caller(scopeA, "alice"), TaskID: task.ID, RunID: e.RunID, StepKey: e.StepKey,
			WaitID: e.WaitID, ExpectedVersion: e.WaitVersion, Key: "k1", Outcome: "approved", Answer: "looks good"})
		if err != nil || !res.Applied || res.Replayed || res.Target != (runtime.SignalTarget{RunID: e.RunID, StepKey: e.StepKey, WaitID: e.WaitID}) {
			t.Fatalf("resolve = %+v, %v", res, err)
		}
		rec, _ := r.runs.Load(r.ctx, scopeA, e.RunID)
		w := rec.Waits[indexWait(rec, "review")]
		d := w.Decision
		if w.State != runtime.WaitResolved || d.Kind != runtime.DecisionResolved || d.Actor != "alice" || d.Outcome != "approved" ||
			d.Answer != "looks good" || d.SignalID != (runtime.SignalIdentity{Scope: scopeA, Caller: "alice", Key: "k1"}).ID() {
			t.Fatalf("recorded wait = %+v", w)
		}
		// The task itself is untouched: resolution is not a task mutation.
		if after := r.get(task.ID); after.Version != before.Version || after.Progress != before.Progress || after.Content.Title != before.Content.Title {
			t.Fatalf("resolve changed the task: %+v -> %+v", before, after)
		}
		if got := r.inbox("alice"); len(got) != 0 {
			t.Fatalf("the decided wait is still in the inbox: %+v", got)
		}
	})

	t.Run("two reviewers racing: the first decides, the other gets the recorded decision unapplied", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("race"))
		e := r.inbox("alice")[0]
		won, err := r.resolve("alice", "ka", e, "approved")
		if err != nil || !won.Applied {
			t.Fatalf("alice = %+v, %v", won, err)
		}
		lost, err := r.resolve("bob", "kb", e, "rejected")
		if err != nil || lost.Applied || lost.Refusal != runtime.RefusalDecided || lost.Decision.Outcome != "approved" || lost.Decision.Actor != "alice" {
			t.Fatalf("bob = %+v, %v", lost, err)
		}
		// The winner's retry returns its recorded result.
		retry, err := r.resolve("alice", "ka", e, "approved")
		if err != nil || !retry.Applied || !retry.Replayed || retry.Decision != won.Decision {
			t.Fatalf("alice's retry = %+v, %v", retry, err)
		}
		rec, _ := r.runs.Load(r.ctx, scopeA, task.Progress.RunID)
		if w := rec.Waits[indexWait(rec, "review")]; w.Decision.Outcome != "approved" || w.Version != e.WaitVersion+1 {
			t.Fatalf("the wait = %+v", w)
		}
	})

	t.Run("an undeclared or reserved outcome is refused and leaves the wait open", func(t *testing.T) {
		r := newRig(t)
		r.waiting(r.task("outcomes"))
		e := r.inbox("alice")[0]
		for i, outcome := range []string{"merged", "expired", "cancelled"} {
			res, err := r.resolve("alice", "bad"+string(rune('a'+i)), e, outcome)
			if err != nil || res.Applied || res.Refusal != runtime.RefusalInvalidOutcome {
				t.Fatalf("%s: %+v, %v", outcome, res, err)
			}
		}
		if got := r.inbox("alice"); len(got) != 1 || got[0].WaitVersion != e.WaitVersion {
			t.Fatalf("a refusal changed the wait: %+v", got)
		}
		if res, err := r.resolve("alice", "good", e, "approved"); err != nil || !res.Applied {
			t.Fatalf("a declared outcome after the refusals: %+v, %v", res, err)
		}
	})

	t.Run("the wait version is required and a stale one is refused", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("versions"))
		e := r.inbox("alice")[0]
		req := ResolveRequest{Caller: r.caller(scopeA, "alice"), TaskID: task.ID, RunID: e.RunID, StepKey: e.StepKey, WaitID: e.WaitID,
			Key: "kv", Outcome: "approved"}
		if _, err := r.svc.Resolve(r.ctx, req); !errors.Is(err, ErrVersionRequired) {
			t.Fatalf("no version: %v", err)
		}
		req.ExpectedVersion = e.WaitVersion + 5
		res, err := r.svc.Resolve(r.ctx, req)
		if err != nil || res.Applied || res.Refusal != runtime.RefusalVersionConflict {
			t.Fatalf("a stale version: %+v, %v", res, err)
		}
		req.Key, req.ExpectedVersion, req.Answer = "ka", e.WaitVersion, strings.Repeat("x", runtime.MaxAnswerBytes+1)
		if _, err := r.svc.Resolve(r.ctx, req); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("an oversized answer: %v", err)
		}
		req.Answer, req.Key = "", ""
		if _, err := r.svc.Resolve(r.ctx, req); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("no caller key: %v", err)
		}
	})

	t.Run("the run, step and wait must be the task's own", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("mine"))
		other, _ := r.waiting(r.task("theirs"))
		e := r.inbox("alice")[0]
		if e.Task.ID != task.ID {
			e = r.inbox("alice")[1]
		}
		base := ResolveRequest{Caller: r.caller(scopeA, "alice"), TaskID: task.ID, RunID: e.RunID, StepKey: e.StepKey, WaitID: e.WaitID,
			ExpectedVersion: e.WaitVersion, Key: "kx", Outcome: "approved"}
		for name, mutate := range map[string]func(*ResolveRequest){
			"another task":    func(q *ResolveRequest) { q.TaskID = other.ID },
			"another run":     func(q *ResolveRequest) { q.RunID = other.Progress.RunID },
			"another step":    func(q *ResolveRequest) { q.StepKey = "build" },
			"an unknown wait": func(q *ResolveRequest) { q.WaitID = "nope" },
			"an unknown task": func(q *ResolveRequest) { q.TaskID = "tsk_nope" },
		} {
			req := base
			mutate(&req)
			if res, err := r.svc.Resolve(r.ctx, req); !errors.Is(err, ErrNotFound) || res.Applied {
				t.Errorf("%s: %+v, %v", name, res, err)
			}
		}
		// A caller of another project cannot reach it, with every identity right.
		r.auth.grant(scopeC, "alice", RoleReviewer)
		req := base
		req.Caller = r.caller(scopeC, "alice")
		if _, err := r.svc.Resolve(r.ctx, req); !errors.Is(err, ErrNotFound) {
			t.Errorf("another project: %v", err)
		}
	})

	t.Run("a retry of a recorded key survives the wait closing and a later identical correlation", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("replay"))
		e := r.inbox("alice")[0]
		won, err := r.resolve("alice", "k1", e, "approved")
		if err != nil || !won.Applied {
			t.Fatalf("approve = %+v, %v", won, err)
		}
		// The first run ends after the approval, and a rerun opens a later wait with the identical correlation.
		r.finishApproved(task)
		task2, err := r.svc.Rerun(r.ctx, RerunRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedRunID: task.Progress.RunID})
		if err != nil {
			t.Fatal(err)
		}
		e2 := r.engine(task2.Progress.RunID)
		e2.build()
		e2.openReview()
		r.apply(task2.Progress.RunID)
		later := r.inbox("alice")
		if len(later) != 1 || later[0].RunID != task2.Progress.RunID || later[0].Correlation["candidate"] != e.Correlation["candidate"] || later[0].WaitID == e.WaitID {
			t.Fatalf("the later wait = %+v", later)
		}

		// The recorded key still returns its recorded approval, not the new wait.
		replay, err := r.resolve("alice", "k1", e, "approved")
		if err != nil || !replay.Applied || !replay.Replayed || replay.Target.WaitID != e.WaitID || replay.Decision != won.Decision {
			t.Fatalf("replay = %+v, %v", replay, err)
		}
		if got := r.inbox("alice"); len(got) != 1 || got[0].WaitVersion != later[0].WaitVersion {
			t.Fatalf("the replay touched the later wait: %+v", got)
		}
		// A fresh key against the closed wait is refused as decided; the earlier review is never applied to the new wait.
		old, err := r.resolve("alice", "k2", e, "approved")
		if err != nil || old.Applied || old.Refusal != runtime.RefusalDecided || old.Decision.Outcome != "approved" {
			t.Fatalf("a fresh key against the closed wait = %+v, %v", old, err)
		}
		// The same key with a changed outcome is key reuse and alters nothing.
		if _, err := r.resolve("alice", "k1", e, "rejected"); !errors.Is(err, runtime.ErrKeyReuse) {
			t.Fatalf("a changed outcome under a recorded key: %v, want key reuse", err)
		}
		rec, _ := r.runs.Load(r.ctx, scopeA, e.RunID)
		if w := rec.Waits[indexWait(rec, "review")]; w.Decision.Outcome != "approved" || w.Decision.Actor != "alice" {
			t.Fatalf("the recorded decision changed: %+v", w.Decision)
		}
		// The old wait's identity cannot be used against the new run.
		cross := ResolveRequest{Caller: r.caller(scopeA, "alice"), TaskID: task.ID, RunID: task2.Progress.RunID, StepKey: "review", WaitID: e.WaitID,
			ExpectedVersion: e.WaitVersion, Key: "k3", Outcome: "approved"}
		if _, err := r.svc.Resolve(r.ctx, cross); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an earlier wait presented as the new one: %v", err)
		}
	})

	t.Run("another reviewer submitting the winner's key is an independent signal", func(t *testing.T) {
		r := newRig(t)
		r.waiting(r.task("keys"))
		e := r.inbox("alice")[0]
		won, err := r.resolve("alice", "shared", e, "approved")
		if err != nil || !won.Applied {
			t.Fatalf("alice = %+v, %v", won, err)
		}
		bob, err := r.resolve("bob", "shared", e, "approved")
		if err != nil || bob.Applied || bob.Replayed || bob.Refusal != runtime.RefusalDecided || bob.SignalID == won.SignalID {
			t.Fatalf("bob with alice's key = %+v, %v", bob, err)
		}
		again, err := r.resolve("alice", "shared", e, "approved")
		if err != nil || !again.Applied || !again.Replayed || again.Decision != won.Decision {
			t.Fatalf("alice's recorded result changed: %+v, %v", again, err)
		}
	})

	t.Run("a caller who lost every role cannot replay either, and the stranger's key is not recorded", func(t *testing.T) {
		r := newRig(t)
		r.waiting(r.task("revoked"))
		e := r.inbox("alice")[0]
		if _, err := r.resolve("alice", "k", e, "approved"); err != nil {
			t.Fatal(err)
		}
		r.auth.grant(scopeA, "alice") // no longer a member
		if _, err := r.resolve("alice", "k", e, "approved"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a former member replays: %v", err)
		}
	})
}

// finishApproved ends the task's current run after its review wait was approved:
// the engine consumes the decision, runs ship and settles the run.
func (r *rig) finishApproved(task Task) {
	r.t.Helper()
	run := task.Progress.RunID
	e := r.engine(run)
	e.step(func(x *runtime.Run) error { return x.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone) })
	e.step(func(x *runtime.Run) error { return x.AdvanceStep("ship", runtime.StepRunning, runtime.ReasonNone) })
	e.step(func(x *runtime.Run) error {
		return x.FinishStep("ship", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	})
	e.step(func(x *runtime.Run) error { return x.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
	e.step(func(x *runtime.Run) error { return x.CompleteCleanup() })
	r.apply(run)
}
