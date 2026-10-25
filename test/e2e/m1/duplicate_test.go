package m1_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestM1DuplicateEventJob (m1_duplicate_event_job): duplication never repeats an
// effect. Two replicas of the process dispatch the same runs and start exactly
// one container per attempt; an event delivered a second time, under a new
// broker message ID, is deduplicated by the consumer's inbox and cancels the
// run once. PX-A03, PX-05, PX-03, PX-12.
func TestM1DuplicateEventJob(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	started := time.Now().Add(-2 * time.Second)

	t.Run("two replicas start one container per attempt", func(t *testing.T) {
		s.StartReplica()
		def := w.PodmanDefinition("replicated", kit.Step{Script: `sleep 2; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`, Outputs: []string{"v"}})
		const runs = 4
		var ids []string
		for i := 0; i < runs; i++ {
			item := w.WorkItem("Replicated", "# r")
			w.Provision(item.Id)
			ids = append(ids, w.Run("replicated-"+string(rune('a'+i)), item, def.Id).Id)
		}
		for _, id := range ids {
			if final := w.WaitRun(id, 3*time.Minute); final.Status != client.RunStatusSucceeded {
				t.Fatalf("run %s %s (%s)", id, final.Status, final.Reason)
			}
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = ANY($1)`, ids); n != runs {
			t.Fatalf("%d attempts for %d single-attempt runs", n, runs)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = ANY($1) AND status = 'succeeded'`, ids); n != runs {
			t.Fatalf("%d succeeded attempts", n)
		}
		// Podman's own event log: one container was created per attempt, never two.
		out, err := exec.Command("podman", "events", "--since", started.Format(time.RFC3339), "--until", "0s", "--stream=false",
			"--filter", "event=create", "--filter", "label=kodama.optimi.com/tenant="+kit.TenantA, "--format", "{{.Name}}").Output()
		if err != nil {
			t.Fatalf("podman events: %v", err)
		}
		names := map[string]int{}
		for _, n := range strings.Fields(string(out)) {
			names[n]++
		}
		if len(names) != runs {
			t.Fatalf("containers created %v for %d attempts", names, runs)
		}
		for n, c := range names {
			if c != 1 {
				t.Fatalf("container %s was created %d times", n, c)
			}
		}
		for _, id := range ids {
			w.WaitRemoved(w.Attempts(id)[0].Id)
		}
	})

	t.Run("a duplicate event delivery is deduplicated and cancels once", func(t *testing.T) {
		sleeper := w.PodmanDefinition("dup-sleeper", kit.Step{Script: `sleep 300`})
		item := w.WorkItem("Duplicate cancel", "# d")
		w.Provision(item.Id)
		run := w.Run("dup-cancel", item, sleeper.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
		kit.WaitFor(t, "the outbox to drain", 30*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })

		w.CancelItem(item)
		if final := w.WaitRun(run.Id, 2*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s", final.Status)
		}
		w.WaitRemoved(attempt)
		kit.WaitFor(t, "the consumer to record the cancellation event", 30*time.Second, func() bool {
			return s.TenantCount(kit.TenantA, `SELECT count(*) FROM event_inbox WHERE consumer = 'work-cancellation' AND state = 'completed'`) > 0
		})
		kit.WaitFor(t, "the outbox to drain", 30*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })

		inbox := s.TenantCount(kit.TenantA, `SELECT count(*) FROM event_inbox WHERE consumer = 'work-cancellation'`)
		intents := s.TenantCount(kit.TenantA, `SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1`, item.Id)
		history := s.TenantCount(kit.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, run.Id)
		attempts := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id)
		base := s.StreamMessages()
		if intents != 1 {
			t.Fatalf("%d cancellation intents for one cancellation", intents)
		}

		if n := s.RepublishEvents(item.Id); n == 0 {
			t.Fatal("no event of the work item was found on the stream")
		}
		if got := s.StreamMessages(); got <= base {
			t.Fatalf("the stream still holds %d messages: nothing was duplicated", got)
		}
		// The consumer fetches the copies, finds each event in its inbox and acknowledges it.
		kit.WaitFor(t, "the consumer to be delivered and to acknowledge the duplicates", 30*time.Second, func() bool {
			c := s.Consumer("kodama-work-cancellation")
			return c.Delivered == c.Last && c.Pending == 0 && c.AckPending == 0
		})
		time.Sleep(time.Second)
		if got := s.TenantCount(kit.TenantA, `SELECT count(*) FROM event_inbox WHERE consumer = 'work-cancellation'`); got != inbox {
			t.Fatalf("the inbox grew from %d to %d: a duplicate was processed as new", inbox, got)
		}
		if got := s.TenantCount(kit.TenantA, `SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1`, item.Id); got != intents {
			t.Fatalf("%d cancellation intents after a duplicate delivery", got)
		}
		if got := s.TenantCount(kit.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, run.Id); got != history {
			t.Fatalf("the run history grew from %d to %d rows", history, got)
		}
		if got := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); got != attempts {
			t.Fatalf("%d attempts after a duplicate delivery", got)
		}
		if got := w.GetRun(run.Id); got.Status != client.RunStatusCancelled {
			t.Fatalf("run %s after a duplicate delivery", got.Status)
		}
	})
}
