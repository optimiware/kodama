package m2_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// gated starts a run of the review pipeline and returns it with its pending gate.
func gated(t *testing.T, e *kit.Env, def client.PipelineDefinition, item client.WorkItem, key string) (client.Run, client.Gate) {
	t.Helper()
	run := e.StartRun(key, item, def.Id, e.AgentRunInputs("completed"))
	return run, e.WaitGate(run.Id, 2*time.Minute)
}

// requireBlocked asserts that nothing the gate protects ran: the review step
// still waits, the build has no attempt, and no workload was allocated.
func requireBlocked(t *testing.T, e *kit.Env, run client.Run, why string) {
	t.Helper()
	if st := e.StepOf(run.Id, "review"); st.Status != client.StepStatusPending || len(st.Attempts) != 0 {
		t.Fatalf("%s: review step %s with %d attempts", why, st.Status, len(st.Attempts))
	}
	if st := e.StepOf(run.Id, "build"); st.Status != client.StepStatusPending || len(st.Attempts) != 0 {
		t.Fatalf("%s: build step %s with %d attempts", why, st.Status, len(st.Attempts))
	}
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 2 {
		t.Fatalf("%s: %d attempts, want only the agent's and the test's", why, n)
	}
}

// TestM2StaleReview (m2_stale_review) is the real-persistence form of the
// applicability rules: an approval is current only for the subject it was given
// for and only while its reviewer still holds the authority. Changed
// requirements before or after the decision, a reviewer removed between the
// decision and its consumption, an expired gate and racing decisions all leave
// the build unstarted and every decision immutable, and a fresh review of the
// current subject then proceeds. The decision and its consumption are two
// transactions, so each scenario that needs the window between them restarts
// the process with the scan and the broker held, changes the world through the
// public API, and lets a new process recover from durable state. Covers DQ-A02,
// DQ-A03, DQ-03, DQ-04, DQ-05, DQ-11, IT-A04's authority half and PX-13.
func TestM2StaleReview(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.ReviewPipeline("stale-review")

	t.Run("content changed before the decision", func(t *testing.T) {
		item := e.Item("Stale before decision", "# v1")
		run, g := gated(t, e, def, item, "stale-before")
		revised := e.EditSpec(item, "# v2: the requirement changed")
		code, body := e.Decide("reviewer", g, "approve", "reviewed v1", "stale-before-d")
		if code != http.StatusConflict || kit.ErrorCode(body) != "stale_gate_subject" {
			t.Fatalf("an approval of superseded requirements: %d %s", code, body)
		}
		now := e.GetGate("reviewer", g.Id)
		if now.Status != "pending" || now.Decision != nil || now.Applicability.Applicable || now.Invalidation == nil || now.Invalidation.Reason != "content_changed" ||
			now.Invalidation.SupersedingRevision == nil || *now.Invalidation.SupersedingRevision != int64(revised.ContentRevision) {
			t.Fatalf("the stale gate %+v", now)
		}
		requireBlocked(t, e, run, "stale before decision")
		// The stale run is cancelled by a human; nothing was allocated for the gate or the build.
		if r := e.World.CancelRun(run.Id); r.StatusCode() != http.StatusOK && r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: HTTP %s %s", r.Status(), r.Body)
		}
		if f := e.World.WaitRun(run.Id, time.Minute); f.Status != client.RunStatusCancelled {
			t.Fatalf("run %s", f.Status)
		}
		if n := e.Attempts(run.Id); n != 2 {
			t.Fatalf("%d attempts after cancellation", n)
		}
		// A fresh review of the current subject proceeds.
		run2, g2 := gated(t, e, def, revised, "stale-before-2")
		if g2.Subject.ContentRevision != int64(revised.ContentRevision) || g2.SubjectDigest == g.SubjectDigest {
			t.Fatalf("the fresh gate %+v", g2)
		}
		if code, b := e.Decide("reviewer", g2, "approve", "reviewed v2", "stale-before-d2"); code != http.StatusOK {
			t.Fatalf("approve v2: %d %s", code, b)
		}
		if f := e.World.WaitRun(run2.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
	})

	t.Run("content changed after the decision, before its consumption", func(t *testing.T) {
		item := e.Item("Stale after decision", "# v1")
		run, g := gated(t, e, def, item, "stale-after")
		e.HoldProgress()
		if code, b := e.Decide("reviewer", g, "approve", "reviewed v1", "stale-after-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		if st := e.StepOf(run.Id, "review"); st.Status != client.StepStatusPending {
			t.Fatalf("the held process consumed the decision: review is %s", st.Status)
		}
		revised := e.EditSpec(item, "# v2: changed after the approval")
		e.ResumeProgress()
		kit.WaitFor(t, "the stale approval to be invalidated", time.Minute, func() bool {
			return e.GetGate("reviewer", g.Id).Invalidation != nil
		})
		now := e.GetGate("reviewer", g.Id)
		if now.Status != "approved" || now.Decision == nil || now.Decision.Reason != "reviewed v1" || now.Applicability.Applicable ||
			now.Invalidation.Reason != "content_changed" || now.Invalidation.SupersedingRevision == nil || *now.Invalidation.SupersedingRevision != int64(revised.ContentRevision) {
			t.Fatalf("the invalidated gate must keep its decision and stop being applicable: %+v", now)
		}
		time.Sleep(2 * time.Second) // more scans: still nothing
		requireBlocked(t, e, run, "stale after decision")
		if got := len(e.GateEvents(g.Id)); got != 3 { // requested, decided, invalidated
			t.Fatalf("%d gate events: %v", got, e.GateEvents(g.Id))
		}
		e.World.CancelRun(run.Id)
		e.World.WaitRun(run.Id, time.Minute)
	})

	t.Run("reviewer removed between the decision and its consumption", func(t *testing.T) {
		item := e.Item("Removed reviewer", "# v1")
		run, g := gated(t, e, def, item, "removed")
		e.HoldProgress()
		// analyst is the second reviewer: its approval is the one that goes stale.
		if code, b := e.Decide("analyst", g, "approve", "approved by a reviewer who is then removed", "removed-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		e.RemoveMember("analyst")
		t.Cleanup(func() { e.AddMember("analyst", "reviewer") }) // restore for the later scenarios
		e.ResumeProgress()
		// A revoked reviewer's approval is never consumed, whatever the scan finds:
		// give the recovered process many scans, then require that nothing ran.
		time.Sleep(4 * time.Second)
		now := e.GetGate("reviewer", g.Id)
		if now.Status != "approved" || now.Decision == nil || now.Decision.Reason != "approved by a reviewer who is then removed" {
			t.Fatalf("the decision of the removed reviewer was rewritten: %+v", now)
		}
		requireBlocked(t, e, run, "removed reviewer")
		if code, _ := e.Do("analyst", "GET", e.ProjectPath()+"/gates/"+g.Id, nil, nil); code != http.StatusNotFound {
			t.Fatalf("the removed reviewer still reads the gate: %d", code)
		}
		// The still-current reviewers cannot launder it: the decision is immutable, so
		// another review of this subject needs a new run.
		if code, b := e.Decide("reviewer", now, "approve", "second approval", "removed-d2"); code != http.StatusUnprocessableEntity || kit.ErrorCode(b) != "invalid_transition" {
			t.Fatalf("a second decision on the removed reviewer's gate: %d %s", code, b)
		}
		e.World.CancelRun(run.Id)
		e.World.WaitRun(run.Id, time.Minute)
	})

	t.Run("an expired gate is not decided", func(t *testing.T) {
		e.Retune(func(tu *app.Tuning) { tu.GateTTL = 4 * time.Second })
		e.Restart()
		item := e.Item("Expired gate", "# v1")
		run, g := gated(t, e, def, item, "expired")
		time.Sleep(6 * time.Second)
		code, body := e.Decide("reviewer", g, "approve", "too late", "expired-d")
		if code != http.StatusConflict || kit.ErrorCode(body) != "stale_gate_subject" {
			t.Fatalf("a decision after the deadline: %d %s", code, body)
		}
		now := e.GetGate("reviewer", g.Id)
		if now.Applicability.Applicable || (now.Decision != nil && now.Status == "approved") {
			t.Fatalf("the expired gate %+v", now)
		}
		requireBlocked(t, e, run, "expired")
		e.World.CancelRun(run.Id)
		e.World.WaitRun(run.Id, time.Minute)
		e.Retune(nil)
		e.Restart()
	})

	t.Run("racing decisions leave one immutable result", func(t *testing.T) {
		item := e.Item("Racing reviewers", "# v1")
		run, g := gated(t, e, def, item, "race")
		type result struct {
			code int
			err  string
		}
		var wg sync.WaitGroup
		results := make([]result, 2)
		start := make(chan struct{})
		for i, c := range []struct{ persona, decision string }{{"reviewer", "approve"}, {"analyst", "reject"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, body := e.Decide(c.persona, g, c.decision, c.persona+" decides", "race-"+c.persona)
				results[i] = result{code, kit.ErrorCode(body)}
			}()
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, r := range results {
			switch {
			case r.code == http.StatusOK:
				ok++
			case r.code == http.StatusPreconditionFailed || r.code == http.StatusUnprocessableEntity || r.code == http.StatusConflict:
				refused++
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("racing decisions answered %+v, want exactly one winner and one refusal", results)
		}
		var n int
		e.MustScan(&n, `SELECT count(*) FROM gates WHERE id = $1 AND status IN ('approved', 'rejected') AND decided_by_id IS NOT NULL`, g.Id)
		if n != 1 || len(e.GateEvents(g.Id)) != 2 {
			t.Fatalf("stored decisions %d, events %v", n, e.GateEvents(g.Id))
		}
		final := e.GetGate("reviewer", g.Id)
		// The loser cannot overwrite the winner afterwards either.
		other := "approve"
		if final.Status == "approved" {
			other = "reject"
		}
		if code, _ := e.Decide("reviewer", final, other, "second thoughts", "race-after"); code != http.StatusUnprocessableEntity {
			t.Fatalf("a decision after the race: %d", code)
		}
		if again := e.GetGate("reviewer", g.Id); again.Status != final.Status || again.Decision.Reason != final.Decision.Reason {
			t.Fatalf("the immutable result changed: %+v", again)
		}
		f := e.World.WaitRun(run.Id, 3*time.Minute)
		want := client.RunStatusSucceeded
		if final.Status == "rejected" {
			want = client.RunStatusFailed
		}
		if f.Status != want {
			t.Fatalf("run %s after a %s gate", f.Status, final.Status)
		}
	})
}
