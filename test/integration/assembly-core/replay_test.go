package assembly_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
)

// frame is one server-sent event.
type frame struct {
	Event string
	ID    string
	Data  string
}

// eventType is the event type of an event frame (the frame name is the type; a
// control frame such as snapshot has none).
func (f frame) eventType() string {
	var ev struct{ Type string }
	_ = json.Unmarshal([]byte(f.Data), &ev)
	return ev.Type
}

// stream is an open project event stream read in the background.
type stream struct {
	frames chan frame
	cancel context.CancelFunc
	status int
}

func (s *stack) openEvents(persona, project, lastEventID string) *stream {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", s.base()+"/api/v1/tenants/"+TenantA+"/projects/"+project+"/events", nil)
	if err != nil {
		cancel()
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token(persona))
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		s.t.Fatal(err)
	}
	st := &stream{frames: make(chan frame, 256), cancel: cancel, status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return st
	}
	go func() {
		defer resp.Body.Close()
		defer close(st.frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					select {
					case st.frames <- cur:
					case <-ctx.Done():
						return
					}
				}
				cur = frame{}
			case strings.HasPrefix(line, "event:"):
				cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"):
				cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				cur.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return st
}

// until reads frames until match accepts one.
func (st *stream) until(t testing.TB, what string, within time.Duration, match func(frame) bool) frame {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				t.Fatalf("the stream ended while waiting for %s", what)
			}
			if match(f) {
				return f
			}

		case <-timer.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func ofType(typ string) func(frame) bool {
	return func(f frame) bool {
		return f.Event == typ && f.eventType() == typ
	}
}

func (w *world) createRun(persona, key string, item client.WorkItem, def string) *client.CreateRunResponse {
	t := w.s.t
	t.Helper()
	r, err := w.s.api(persona).CreateRunWithResponse(context.Background(), TenantA, w.project, &client.CreateRunParams{IdempotencyKey: key},
		client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: def, ContentRevision: int64(item.ContentRevision), SourceCommit: w.s.Repo.Initial})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (w *world) cancelItem(item client.WorkItem) {
	t := w.s.t
	t.Helper()
	reason := "t070 cancellation"
	r, err := w.s.api("engineer").TransitionWorkItemWithResponse(context.Background(), TenantA, w.project, item.Id,
		&client.TransitionWorkItemParams{IfMatch: fmt.Sprintf(`"%d"`, item.Version)}, client.TransitionRequest{To: "cancelled", Reason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, r, r.Body, http.StatusOK, "cancel work item")
}

func (w *world) runStatus(id string) (string, bool) {
	r, err := w.s.api("engineer").GetRunWithResponse(context.Background(), TenantA, w.project, id)
	if err != nil || r.StatusCode() != 200 {
		return "", false
	}
	return string(r.JSON200.Status), r.JSON200.CancelRequestedAt != nil
}

func (s *stack) pendingEvents() int {
	return s.tenantCount(TenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`)
}

func (s *stack) noAttempts(t testing.TB) {
	t.Helper()
	if n := s.ownerCount(`SELECT count(*) FROM step_attempts`) + s.ownerCount(`SELECT count(*) FROM attempt_controls`) +
		s.ownerCount(`SELECT count(*) FROM runtime_grants`) + s.ownerCount(`SELECT count(*) FROM execution_reservations`); n != 0 {
		t.Fatalf("%d attempts, controls, grants or reservations exist: a run waiting at a gate holds an attempt", n)
	}
}

// Test_core_assembly_replay (PX-05, AC-06, AC-10, AC-12, SD-10, SD-11): the
// composition survives its own restart and its dependencies' outages without
// repeating or losing an effect. An admission key replays across a restart and
// still conflicts on a changed request; an event stream resumes from its cursor
// across the restart; the outbox drains after a broker outage (with and without
// a worker restart); a cancellation reaches the run through the durable event
// consumer and, independently, through the periodic scan when the event is lost;
// an uncertain source-retention push is repaired by the reconciler; and a run
// waiting at a gate allocates no attempt, whatever the restarts and outages (the
// dispatcher is composed and passes at every start; container steps are proven
// by core_assembly_current_runtime).
func Test_core_assembly_replay(t *testing.T) {
	// The first phases must not be repaired by the periodic scan: the event
	// consumer is the only way a cancellation can reach a run until the interval
	// is shortened for the scan phase.
	slow := true
	s := newStack(t, options{Tune: func(tn *app.Tuning) {
		if slow {
			tn.ReconcileInterval = time.Hour
		} else {
			// The scan phase proves the periodic scan alone repairs a lost event: the
			// consumer's own redelivery of a published-but-unconsumed event (its repair
			// grace) must not win the race on a loaded machine.
			tn.Events.RepairGrace = time.Hour
		}
	}})
	s.start()
	w := s.seed(true)
	ctx := context.Background()

	item1 := w.workItem("Replay subject", "# Plan\nReplay.")
	def := w.holdDefinition("replay")
	other := w.holdDefinition("replay-other")
	w.provision(item1.Id)

	// A stream opened before the run exists observes its creation.
	st := s.openEvents("engineer", w.project, "")
	if st.status != 200 {
		t.Fatalf("event stream: %d", st.status)
	}
	st.until(t, "the initial snapshot", 10*time.Second, func(f frame) bool { return f.Event == "snapshot" })

	run := w.createRun("engineer", "t070-replay-key", item1, def.Id)
	if run.StatusCode() != http.StatusCreated {
		t.Fatalf("admission: %d %s", run.StatusCode(), run.Body)
	}
	created := st.until(t, "run.created on the stream", 20*time.Second, ofType("run.created"))
	if created.ID == "" {
		t.Fatal("an event frame without a cursor")
	}
	st.cancel()
	waitFor(t, "the run's source retention", 20*time.Second, func() bool {
		r, _ := s.api("engineer").GetRunWithResponse(ctx, TenantA, w.project, run.JSON201.Id)
		return r.StatusCode() == 200 && r.JSON200.SourceRetention == "confirmed"
	})

	t.Run("an admission key and an event cursor survive a restart of the process", func(t *testing.T) {
		s.stop()
		s.start()
		waitFor(t, "readiness after restart", 15*time.Second, func() bool { return readyz(t, s.base()) == 200 })

		replay := w.createRun("engineer", "t070-replay-key", item1, def.Id)
		if replay.StatusCode() != http.StatusCreated || replay.JSON201.Id != run.JSON201.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay after restart: %d %s", replay.StatusCode(), replay.Body)
		}
		if c := w.createRun("engineer", "t070-replay-key", item1, other.Id); c.StatusCode() != http.StatusConflict || errorCode(c.Body) != "idempotency_conflict" {
			t.Fatalf("a changed request after restart: %d %s", c.StatusCode(), c.Body)
		}
		if n := s.ownerCount(`SELECT count(*) FROM runs`); n != 1 {
			t.Fatalf("%d runs", n)
		}
		if status, _ := w.runStatus(run.JSON201.Id); status != "queued" {
			t.Fatalf("the run is %s", status)
		}

		// The cursor of a frame received before the restart resumes after the restart:
		// the already-seen run.created is not repeated, a later event is delivered.
		resumed := s.openEvents("engineer", w.project, created.ID)
		if resumed.status != 200 {
			t.Fatalf("resume: %d", resumed.status)
		}
		later := w.workItem("After the restart", "x")
		got := resumed.until(t, "the work item created after the restart", 20*time.Second, func(f frame) bool {
			if f.eventType() == "run.created" {
				t.Fatalf("the already-delivered event was repeated: %s", f.Data)
			}
			return f.eventType() == "work_item.created"
		})
		if !strings.Contains(got.Data, later.Id) {
			t.Fatalf("resumed with %s, want the event of %s", got.Data, later.Id)
		}
		resumed.cancel()
		// A cursor from nowhere is refused, not guessed.
		if bad := s.openEvents("engineer", w.project, "AAAA.bad"); bad.status != http.StatusBadRequest {
			t.Fatalf("a malformed cursor: %d", bad.status)
		}
		s.noAttempts(t)
	})

	t.Run("a broker outage delays delivery; the outbox drains with the running workers and after a worker restart", func(t *testing.T) {
		waitFor(t, "the outbox to drain", 20*time.Second, func() bool { return s.pendingEvents() == 0 })
		base := s.streamMessages()

		s.NATS.Stop()
		a := w.workItem("During outage A", "x")
		w.workItem("During outage B", "x")
		if n := s.pendingEvents(); n < 2 {
			t.Fatalf("%d pending events after two commits", n)
		}
		// Publication attempts fail (after the publish timeout) while the broker is unreachable.
		waitFor(t, "a recorded failed publication attempt", 20*time.Second, func() bool {
			return s.tenantCount(TenantA, `SELECT count(*) FROM outbox_attempts WHERE outcome = 'transient'`) > 0
		})
		if got := s.streamMessages(); got != base {
			t.Fatalf("the stream grew from %d to %d during the outage", base, got)
		}
		// The work itself is not blocked by the broker: state commits and is readable.
		if r, _ := s.api("engineer").GetWorkItemWithResponse(ctx, TenantA, w.project, a.Id); r.StatusCode() != 200 {
			t.Fatalf("work is unavailable during a broker outage: %d", r.StatusCode())
		}

		// Recovery without a restart: the same workers retry.
		s.NATS.Start()
		waitFor(t, "the outbox to drain after the broker recovered", 40*time.Second, func() bool { return s.pendingEvents() == 0 })
		recovered := s.streamMessages()
		if recovered < base+2 {
			t.Fatalf("stream %d after recovery, expected at least %d", recovered, base+2)
		}

		// Recovery across a worker restart: events committed while the broker is down are
		// delivered by a different process lifetime.
		s.NATS.Stop()
		w.workItem("Across restart C", "x")
		w.workItem("Across restart D", "x")
		pending := s.pendingEvents()
		if pending < 2 {
			t.Fatalf("%d pending events", pending)
		}
		s.stop() // the workers stop and are awaited; nothing publishes
		if got := s.pendingEvents(); got < pending {
			t.Fatalf("pending events shrank from %d to %d with the process stopped", pending, got)
		}
		s.NATS.Start()
		s.start()
		waitFor(t, "the restarted workers to deliver the backlog", 40*time.Second, func() bool { return s.pendingEvents() == 0 })
		if got := s.streamMessages(); got < recovered+2 {
			t.Fatalf("stream %d, expected at least %d", got, recovered+2)
		}
		s.noAttempts(t)
	})

	t.Run("a cancellation reaches the run through the durable event consumer", func(t *testing.T) {
		waitFor(t, "ready", 15*time.Second, func() bool { return readyz(t, s.base()) == 200 })
		item := w.workItem("Cancelled through events", "x")
		w.provision(item.Id)
		r := w.createRun("engineer", "t070-cancel-events", item, def.Id)
		if r.StatusCode() != http.StatusCreated {
			t.Fatalf("admission: %d %s", r.StatusCode(), r.Body)
		}
		waitFor(t, "the outbox to drain", 20*time.Second, func() bool { return s.pendingEvents() == 0 })

		// The broker is down when the work item is cancelled: the committed state is the intent.
		s.NATS.Stop()
		w.cancelItem(item)
		time.Sleep(1500 * time.Millisecond)
		if status, requested := w.runStatus(r.JSON201.Id); status != "queued" || requested {
			t.Fatalf("the run changed without an event or a scan: %s %v", status, requested)
		}
		if n := s.tenantCount(TenantA, `SELECT count(*) FROM work_cancellation_requests`); n != 0 {
			t.Fatalf("%d cancellation intents before delivery", n)
		}
		// The periodic scan is an hour away: only the consumer can deliver this.
		s.NATS.Start()
		waitFor(t, "the cancellation to reach the run", 60*time.Second, func() bool {
			status, _ := w.runStatus(r.JSON201.Id)
			return status == "cancelled"
		})
		if n := s.tenantCount(TenantA, `SELECT count(*) FROM work_cancellation_requests`); n != 1 {
			t.Fatalf("%d cancellation intents", n)
		}
		if n := s.tenantCount(TenantA, `SELECT count(*) FROM event_inbox WHERE consumer = 'work-cancellation' AND state = 'completed'`); n < 1 {
			t.Fatalf("the consumer recorded no completed inbox entry")
		}
		history := s.tenantCount(TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, r.JSON201.Id)
		if history < 2 {
			t.Fatalf("the cancellation left %d history rows", history)
		}
		s.noAttempts(t)
	})

	slow = false
	t.Run("a lost cancellation event is repaired by the periodic scan", func(t *testing.T) {
		s.stop()
		s.start() // the reconcile interval is now short
		waitFor(t, "ready", 15*time.Second, func() bool { return readyz(t, s.base()) == 200 })
		item := w.workItem("Cancelled through the scan", "x")
		w.provision(item.Id)
		r := w.createRun("engineer", "t070-cancel-scan", item, def.Id)
		if r.StatusCode() != http.StatusCreated {
			t.Fatalf("admission: %d %s", r.StatusCode(), r.Body)
		}
		waitFor(t, "the outbox to drain", 20*time.Second, func() bool { return s.pendingEvents() == 0 })

		s.NATS.Stop()
		w.cancelItem(item)
		// The cancellation event is lost before it ever leaves the outbox (an operator
		// settling a poisoned row as published, a restored database): nothing but the
		// committed state remains. Event tables answer only inside a tenant context,
		// even for their owner.
		lostIDs := s.tenantStrings(TenantA, `SELECT event_id FROM outbox WHERE state <> 'published' AND event_type = 'work_item.transitioned'`)
		if len(lostIDs) == 0 {
			t.Fatal("no pending cancellation event to lose")
		}
		if n := s.ownerTenantExec(TenantA, `UPDATE outbox SET state = 'published', published_at = now(), lease_expires_at = NULL
			WHERE state <> 'published' AND event_type = 'work_item.transitioned'`); int(n) != len(lostIDs) {
			t.Fatalf("settled %d of %d pending events", n, len(lostIDs))
		}
		s.NATS.Start()
		waitFor(t, "the scan to cancel the run without an event", 60*time.Second, func() bool {
			status, _ := w.runStatus(r.JSON201.Id)
			return status == "cancelled"
		})
		if n := s.tenantCount(TenantA, `SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1`, item.Id); n != 1 {
			t.Fatalf("%d cancellation intents for the item", n)
		}
		if n := s.tenantCount(TenantA, `SELECT count(*) FROM event_inbox WHERE consumer = 'work-cancellation' AND event_id = ANY($1)`, lostIDs); n != 0 {
			t.Fatalf("the lost event reached the consumer (%d inbox entries): the scan was not the repair path", n)
		}
		if n := s.ownerCount(`SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1`, item.Id); n != 1 {
			t.Fatalf("%d cancellation intents for the item", n)
		}
		s.noAttempts(t)
	})

	t.Run("an uncertain source-retention push is repaired by the reconciler without a wakeup", func(t *testing.T) {
		// Only the admission handler may try to confirm the reference: the periodic
		// repair is an hour away until the process is restarted below.
		slow = true
		s.stop()
		s.start()
		waitFor(t, "ready", 15*time.Second, func() bool { return readyz(t, s.base()) == 200 })
		item := w.workItem("Retention redrive", "x")
		w.provision(item.Id)
		// The retained-reference push is parked past the Git deadline: the control
		// plane gives up with an unknown outcome while the remote completes the
		// update later.
		hold := s.Git.HoldReceivePack()
		r := w.createRun("engineer", "t070-retention", item, def.Id)
		if r.StatusCode() != http.StatusCreated {
			t.Fatalf("admission: %d %s", r.StatusCode(), r.Body)
		}
		select {
		case <-hold.Reached:
		case <-time.After(20 * time.Second):
			t.Fatal("the retained-reference push never started")
		}
		if got, _ := s.api("engineer").GetRunWithResponse(ctx, TenantA, w.project, r.JSON201.Id); got.JSON200.SourceRetention != "pending" {
			t.Fatalf("with the push parked the retention is %s", got.JSON200.SourceRetention)
		}
		time.Sleep(s.gitDeadline() + 2*time.Second)
		hold.Release()
		s.Git.WaitIdle(t)
		time.Sleep(time.Second)
		if got, _ := s.api("engineer").GetRunWithResponse(ctx, TenantA, w.project, r.JSON201.Id); got.JSON200.SourceRetention != "pending" {
			t.Fatalf("a push that timed out was assumed to have succeeded: %s", got.JSON200.SourceRetention)
		}
		before := s.Repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads/kodama/retained/")

		// A restart with the periodic repair enabled finishes what the first process could not.
		slow = false
		s.stop()
		s.start()
		waitFor(t, "the reconciler to confirm the retained reference", 60*time.Second, func() bool {
			got, err := s.api("engineer").GetRunWithResponse(ctx, TenantA, w.project, r.JSON201.Id)
			return err == nil && got.StatusCode() == 200 && got.JSON200.SourceRetention == "confirmed"
		})
		after := s.Repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads/kodama/retained/")
		if !strings.Contains(after, "refs/heads/kodama/retained/") || strings.Count(after, "\n") != strings.Count(before, "\n") {
			t.Fatalf("the redrive created another reference: before %q after %q", before, after)
		}
		if pins := s.tenantCount(TenantA, `SELECT count(*) FROM workspace_retained_refs WHERE run_id = $1`, r.JSON201.Id); pins != 1 {
			t.Fatalf("%d retained-reference records for the run", pins)
		}
		s.noAttempts(t)
	})

	t.Run("the process stops its workers before releasing its connections", func(t *testing.T) {
		s.stop()
		s.noAttempts(t)
	})
}

// Test_core_assembly_stream_outlives_the_write_timeout: the server bounds the
// writing of a response, but an event stream, which never ends by itself, keeps
// its own per-write deadline and heartbeats, so it stays open far beyond that
// bound and still delivers the event that follows.
func Test_core_assembly_stream_outlives_the_write_timeout(t *testing.T) {
	s := newStack(t, options{Tune: func(tn *app.Tuning) {
		tn.HTTPWriteTimeout = 2 * time.Second
		tn.ObservationHeartbeat = 500 * time.Millisecond
	}})
	s.start()
	w := s.seed(false)
	st := s.openEvents("engineer", w.project, "")
	if st.status != 200 {
		t.Fatalf("event stream: %d", st.status)
	}
	defer st.cancel()
	st.until(t, "the initial snapshot", 10*time.Second, func(f frame) bool { return f.Event == "snapshot" })
	time.Sleep(6 * time.Second) // three times the write timeout, idle
	item := w.workItem("Delivered after the write timeout elapsed", "x")
	got := st.until(t, "an event on the long-lived stream", 20*time.Second, ofType("work_item.created"))
	if !strings.Contains(got.Data, item.Id) {
		t.Fatalf("%s", got.Data)
	}
}
