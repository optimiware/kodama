package assemblyagentdelivery_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// gate starts a run of the review pipeline and returns it with its pending gate.
func gate(t *testing.T, e *kit.Env, def client.PipelineDefinition, name string) (client.WorkItem, client.Run, client.Gate) {
	t.Helper()
	item := e.Item(name, "# "+name)
	run := e.StartRun(name+"-key", item, def.Id, e.AgentRunInputs("completed"))
	return item, run, e.WaitGate(run.Id, 2*time.Minute)
}

// transitions counts the run-history rows that moved the step to the status.
func transitions(e *kit.Env, run, step, status string) int {
	return e.TenantCount(kit.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = $2 AND to_status = $3`, run, step, status)
}

// requireConsumedOnce asserts the end state of one consumed approval: the gate
// step moved exactly once, the protected step ran once, the gate allocated no
// compute, and the gate recorded exactly its request and its decision.
func requireConsumedOnce(t *testing.T, e *kit.Env, run client.Run, g client.Gate) {
	t.Helper()
	if f := e.World.WaitRun(run.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", f.Status, f.Reason)
	}
	if n := transitions(e, run.Id, "review", "succeeded"); n != 1 {
		t.Fatalf("the gate step moved to succeeded %d times", n)
	}
	build := e.StepOf(run.Id, "build")
	if len(build.Attempts) != 1 || build.Attempts[0].Ordinal != 1 {
		t.Fatalf("build attempts %+v", build.Attempts)
	}
	if n := e.Attempts(run.Id); n != 3 {
		t.Fatalf("%d attempts: agent, test and build only (a gate allocates none)", n)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'review'`, run.Id); n != 0 {
		t.Fatalf("the gate step has %d attempts", n)
	}
	if got := strings.Join(e.GateEvents(g.Id), ","); got != "gate.requested,gate.decided" {
		t.Fatalf("gate events %q", got)
	}
	var status string
	var decisions int
	e.MustScan(&status, `SELECT status FROM gates WHERE id = $1`, g.Id)
	e.MustScan(&decisions, `SELECT count(*) FROM gates WHERE id = $1 AND decided_by_id IS NOT NULL`, g.Id)
	if status != "approved" || decisions != 1 {
		t.Fatalf("stored gate %s decisions %d", status, decisions)
	}
}

// TestGateConsumptionAtomicity (gate_consumption_atomicity) runs the gate path
// of the assembled core against real PostgreSQL, JetStream and Podman and
// breaks it where the transactions end: a decision's acknowledgment is lost
// after commit; many identical decisions race; the database is out between the
// decision and its consumption and flaps during it; the wakeup event is
// duplicated; a run is cancelled between an approval and its consumption. The
// decision and the step transition are two transactions with the approved gate
// as the durable intent, so every case must end with exactly one decision, one
// transition, one protected execution and no compute for the gate, and a
// cancelled run must not run what the gate protected. Covers DQ-03, DQ-04,
// DQ-A02, DQ-A03, PX-13, SD-04, SD-10, SD-11 and AC-12 (replay).
func TestGateConsumptionAtomicity(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.ReviewPipeline("atomicity")

	t.Run("a lost acknowledgment of the decision replays the stored response", func(t *testing.T) {
		_, run, g := gate(t, e, def, "lost-ack")
		lossy := newLossyFront(t, e.Base(), isDecision)
		body, _ := json.Marshal(map[string]any{"decision": "approve", "subjectDigest": string(g.SubjectDigest), "reason": "reviewed"})
		send := func(base, key string) (*http.Response, []byte, error) {
			req, _ := http.NewRequest("POST", base+e.ProjectPath()+"/gates/"+g.Id+"/decisions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+e.Token("reviewer"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprintf(`"%d"`, g.Version))
			req.Header.Set("Idempotency-Key", key)
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			if err != nil {
				return nil, nil, err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, b, nil
		}
		if _, _, err := send(lossy.URL, "lost-ack-key"); err == nil {
			t.Fatal("the lossy front answered")
		}
		if lossy.dropped.Load() != 1 {
			t.Fatalf("%d acknowledgments dropped", lossy.dropped.Load())
		}
		// The caller cannot know: but the server committed exactly one decision.
		committed := e.GetGate("reviewer", g.Id)
		if committed.Status != "approved" || committed.Decision == nil {
			t.Fatalf("the decision did not commit: %+v", committed)
		}
		resp, again, err := send(e.Base(), "lost-ack-key")
		if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("the retry: %v %v %s", err, resp, again)
		}
		var got client.Gate
		if err := json.Unmarshal(again, &got); err != nil || got.Status != "approved" || got.Decision == nil || got.Decision.DecidedAt != committed.Decision.DecidedAt {
			t.Fatalf("the replayed response %s (%v)", again, err)
		}
		// A different key is a different request: it still carries the version the lost
		// response would have taught, so the changed resource refuses it; with the
		// current version the immutable decision does.
		if resp, b, err := send(e.Base(), "lost-ack-other"); err != nil || resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("a second decision under another key and the old version: %v %v %s", err, resp, b)
		}
		if code, b := e.Decide("reviewer", committed, "approve", "reviewed", "lost-ack-other-2"); code != http.StatusUnprocessableEntity || kit.ErrorCode(b) != "invalid_transition" {
			t.Fatalf("a second decision under another key and the current version: %d %s", code, b)
		}
		requireConsumedOnce(t, e, run, g)
	})

	t.Run("identical keyed decisions racing commit once", func(t *testing.T) {
		_, run, g := gate(t, e, def, "keyed-race")
		const n = 8
		type result struct {
			code int
			body string
		}
		results := make([]result, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, b := e.Decide("reviewer", g, "approve", "identical", "keyed-race-key")
				results[i] = result{code, string(b)}
			}()
		}
		close(start)
		wg.Wait()
		for i, r := range results {
			if r.code != http.StatusOK || r.body != results[0].body {
				t.Fatalf("response %d: %d %s (first %s)", i, r.code, r.body, results[0].body)
			}
		}
		requireConsumedOnce(t, e, run, g)
	})

	t.Run("a database outage between the decision and its consumption", func(t *testing.T) {
		_, run, g := gate(t, e, def, "outage")
		e.HoldProgress()
		if code, b := e.Decide("reviewer", g, "approve", "reviewed", "outage-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		if st := e.StepOf(run.Id, "review"); st.Status != client.StepStatusPending {
			t.Fatalf("the held process consumed the approval: %s", st.Status)
		}
		e.PG.Stop()
		time.Sleep(time.Second)
		e.PG.Start()
		e.ResumeProgress()
		requireConsumedOnce(t, e, run, g)
	})

	t.Run("a flapping database during consumption", func(t *testing.T) {
		_, run, g := gate(t, e, def, "flap")
		e.HoldProgress()
		if code, b := e.Decide("reviewer", g, "approve", "reviewed", "flap-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		e.NATS.Start()
		e.Retune(nil)
		e.Restart() // a normal-paced process consumes now ...
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) { // ... while the database goes away and comes back
			e.PG.Stop()
			time.Sleep(60 * time.Millisecond)
			e.PG.Start()
			time.Sleep(140 * time.Millisecond)
		}
		requireConsumedOnce(t, e, run, g)
	})

	t.Run("duplicated decision wakeups move nothing", func(t *testing.T) {
		_, run, g := gate(t, e, def, "dup-wakeup")
		if code, b := e.Decide("reviewer", g, "approve", "reviewed", "dup-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		requireConsumedOnce(t, e, run, g)
		// The decision event reaches the consumer again under fresh broker identities.
		kit.WaitFor(t, "the outbox to publish", 20*time.Second, func() bool { return e.PendingEvents(kit.TenantA) == 0 })
		if n := e.RepublishEvents(g.Id); n == 0 {
			t.Fatal("no event of the gate was republished")
		}
		time.Sleep(3 * time.Second)
		requireConsumedOnce(t, e, run, g)
	})

	t.Run("cancellation between the approval and its consumption", func(t *testing.T) {
		_, run, g := gate(t, e, def, "cancel")
		e.HoldProgress()
		if code, b := e.Decide("reviewer", g, "approve", "reviewed", "cancel-d"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		if r := e.World.CancelRun(run.Id); r.StatusCode() != http.StatusOK && r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %s %s", r.Status(), r.Body)
		}
		e.ResumeProgress()
		f := e.World.WaitRun(run.Id, time.Minute)
		if f.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
		time.Sleep(2 * time.Second)
		if st := e.StepOf(run.Id, "build"); len(st.Attempts) != 0 || st.Status == client.StepStatusSucceeded {
			t.Fatalf("build %+v ran after the run was cancelled", st)
		}
		if n := e.Attempts(run.Id); n != 2 {
			t.Fatalf("%d attempts after the cancellation", n)
		}
		kept := e.GetGate("reviewer", g.Id)
		if kept.Status != "approved" || kept.Decision == nil || kept.Decision.Reason != "reviewed" {
			t.Fatalf("the approval changed: %+v", kept)
		}
		if n := transitions(e, run.Id, "review", "succeeded"); n != 0 {
			t.Fatalf("a cancelled run's gate step succeeded %d times", n)
		}
	})
}
