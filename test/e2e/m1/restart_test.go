package m1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestM1Restart (m1_restart): the installation survives the loss of its own
// process and of each dependency without repeating or losing an effect. A new
// process adopts the running container instead of creating another (PX-A02,
// PX-03); a workload that finishes while no server is up retries its callback
// and the run still completes once; a PostgreSQL outage during the step delays
// the result but never duplicates it; an admission key replays across the
// restart and still conflicts on a changed request (AC-A02); and an event
// cursor resumes (AC-A04). Real PostgreSQL, JetStream, S3, Git and Podman.
func TestM1Restart(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	finish := `printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`

	t.Run("a new process adopts the running container; keys and cursors survive", func(t *testing.T) {
		item := w.WorkItem("Adopt", "# adopt")
		w.Provision(item.Id)
		def := w.PodmanDefinition("adopt", kit.Step{Script: `sleep 12; ` + finish, Outputs: []string{"v"}})
		other := w.PodmanDefinition("adopt-other", kit.Step{Script: `true`})
		events := s.OpenEvents("engineer", w.ProjectEvents(), "")
		events.Until(t, "the snapshot", 10*time.Second, func(f kit.Frame) bool { return f.Event == "snapshot" })
		run := w.Run("restart-key", item, def.Id)
		created := events.Until(t, "run.created", 20*time.Second, kit.OfType("run.created"))
		events.Cancel()

		attempt := w.WaitAttempt(run.Id, time.Minute)
		before := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
		s.WaitLaunched(before.ID)

		s.Restart()

		after := s.Containers(kit.TenantA, attempt)
		if len(after) != 1 || after[0].ID != before.ID {
			t.Fatalf("the restarted process did not adopt container %s: %+v", before.ID, after)
		}
		replay := w.CreateRun("engineer", "restart-key", item, def.Id)
		if replay.StatusCode() != http.StatusCreated || replay.JSON201.Id != run.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay after the restart: %d %s", replay.StatusCode(), replay.Body)
		}
		if c := w.CreateRun("engineer", "restart-key", item, other.Id); c.StatusCode() != http.StatusConflict || kit.ErrorCode(c.Body) != "idempotency_conflict" {
			t.Fatalf("a changed request under the key: %d %s", c.StatusCode(), c.Body)
		}

		// The cursor of a frame received before the restart resumes after it.
		resumed := s.OpenEvents("engineer", w.ProjectEvents(), created.ID)
		if resumed.Status != http.StatusOK {
			t.Fatalf("resume: %d", resumed.Status)
		}
		later := w.WorkItem("After the restart", "x")
		got := resumed.Until(t, "the work item created after the restart", 20*time.Second, func(f kit.Frame) bool {
			if f.Type() == "run.created" {
				t.Fatalf("an already-delivered event was repeated: %s", f.Data)
			}
			return f.Type() == "work_item.created"
		})
		if !strings.Contains(got.Data, later.Id) {
			t.Fatalf("resumed with %s, want the event of %s", got.Data, later.Id)
		}
		resumed.Cancel()
		if bad := s.OpenEvents("engineer", w.ProjectEvents(), "AAAA.bad"); bad.Status != http.StatusBadRequest {
			t.Fatalf("a malformed cursor: %d", bad.Status)
		}

		final := w.WaitRun(run.Id, 3*time.Minute)
		if final.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts after an adopted workload", n)
		}
		w.WaitRemoved(attempt)
	})

	t.Run("a workload that finishes while no server is up retries its callback", func(t *testing.T) {
		item := w.WorkItem("Server down", "# down")
		w.Provision(item.Id)
		def := w.PodmanDefinition("server-down", kit.Step{Script: `sleep 6; ` + finish, Outputs: []string{"v"}})
		run := w.Run("server-down-key", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		s.WaitLaunched(s.WaitContainer(kit.TenantA, attempt, "running", time.Minute).ID)

		s.Stop()
		served := s.Front.Requests()
		kit.WaitFor(t, "the container to finish its command and call back a server that is down", 90*time.Second, func() bool {
			return s.Front.Requests() > served+1
		})
		s.Start()
		s.WaitReady()
		final := w.WaitRun(run.Id, 3*time.Minute)
		if final.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts", n)
		}
		w.WaitRemoved(attempt)
	})

	t.Run("a PostgreSQL outage during the step delays the result and never duplicates it", func(t *testing.T) {
		item := w.WorkItem("Database outage", "# outage")
		w.Provision(item.Id)
		def := w.PodmanDefinition("db-outage", kit.Step{Script: `sleep 5; ` + finish, Outputs: []string{"v"}})
		run := w.Run("db-outage-key", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		s.WaitLaunched(s.WaitContainer(kit.TenantA, attempt, "running", time.Minute).ID)

		s.PG.Stop()
		if resp, _ := s.Raw("GET", "/readyz", "", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("readiness during the outage: %d", resp.StatusCode)
		}
		time.Sleep(9 * time.Second) // the step ends and calls back while the database is away
		s.PG.Start()
		kit.WaitFor(t, "readiness after the database came back", 30*time.Second, func() bool {
			resp, _ := s.Raw("GET", "/readyz", "", nil, nil)
			return resp.StatusCode == http.StatusOK
		})
		final := w.WaitRun(run.Id, 3*time.Minute)
		if final.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts", n)
		}
		w.WaitRemoved(attempt)
	})

	t.Run("a broker outage delays events; the outbox drains with and without a restart", func(t *testing.T) {
		kit.WaitFor(t, "the outbox to drain", 30*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })
		base := s.StreamMessages()
		s.NATS.Stop()
		w.WorkItem("During outage A", "x")
		w.WorkItem("During outage B", "x")
		if n := s.PendingEvents(kit.TenantA); n < 2 {
			t.Fatalf("%d pending events after two commits", n)
		}
		if got := s.StreamMessages(); got != base {
			t.Fatalf("the stream grew from %d to %d during the outage", base, got)
		}
		s.NATS.Start()
		kit.WaitFor(t, "the outbox to drain after the broker recovered", 60*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })
		recovered := s.StreamMessages()
		if recovered < base+2 {
			t.Fatalf("stream %d after recovery, expected at least %d", recovered, base+2)
		}
		s.NATS.Stop()
		w.WorkItem("Across restart C", "x")
		s.Stop() // nothing publishes while the process is away
		s.NATS.Start()
		s.Start()
		kit.WaitFor(t, "the restarted workers to deliver the backlog", 60*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })
		if got := s.StreamMessages(); got < recovered+1 {
			t.Fatalf("stream %d, expected at least %d", got, recovered+1)
		}
	})
}
