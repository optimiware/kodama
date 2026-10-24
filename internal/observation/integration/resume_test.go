package integration_test

import (
	"fmt"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
)

// drain reads one connection until the stream is idle and returns the event
// frames and the last cursor (event, checkpoint or snapshot) it carried.
func drain(t *testing.T, url, token, resumeFrom string) (events []ot.Frame, last string, checkpoints int) {
	t.Helper()
	var hdr map[string]string
	if resumeFrom != "" {
		hdr = map[string]string{"Last-Event-ID": resumeFrom}
	}
	s := ot.Connect(t, url, token, hdr)
	if s.Resp.StatusCode != 200 {
		t.Fatalf("connect: %d %s", s.Resp.StatusCode, s.Body)
	}
	last = resumeFrom
	for {
		fr, ok := s.Next(300 * time.Millisecond)
		if !ok {
			break
		}
		if fr.ID != "" {
			last = fr.ID
		}
		switch fr.Event {
		case "snapshot":
		case "checkpoint":
			checkpoints++
		default:
			events = append(events, fr)
		}
	}
	s.Close()
	return events, last, checkpoints
}

// Test_filtered_resume: run filtering, empty matching pages, checkpoint
// cursors, reconnect and handler restart preserve ordering without skipping
// matching observations or depending on broker delivery.
func Test_filtered_resume(t *testing.T) {
	f := newFixture(t, ot.Options{})
	token := string(f.viewer)
	f.SeedRun(ot.TenantA, ot.ProjectA, "run_1", "running", 1)
	f.SeedRun(ot.TenantA, ot.ProjectA, "run_2", "running", 1)

	// Two independent handlers over the same database and key: a reconnect may
	// reach either, as after a restart or behind a load balancer.
	srvA := f.NewServer(f.Service(), f.Authority(), ot.HandlerOptions())
	srvB := f.NewServer(f.Service(), f.Authority(), ot.HandlerOptions())
	urls := []string{ot.RunURL(srvA.URL, ot.TenantA, ot.ProjectA, "run_1"), ot.RunURL(srvB.URL, ot.TenantA, ot.ProjectA, "run_1")}

	var want []string // the events run_1's stream must deliver, in order
	seq := 0
	next := func() string { seq++; return fmt.Sprintf("t024_fr_%03d", seq) }
	commitMatching := func(kind string) {
		id := next()
		switch kind {
		case "run":
			f.Commit(ot.TenantA, nil, ot.RunEvent(ot.TenantA, ot.ProjectA, id, "run_1", int64(seq)))
		case "step":
			f.Commit(ot.TenantA, nil, ot.StepEvent(ot.TenantA, ot.ProjectA, id, "att_1", "run_1", int64(seq)))
		}
		want = append(want, id)
	}
	commitNoise := func(n int) {
		for i := 0; i < n; i++ {
			id := next()
			switch i % 4 {
			case 0:
				f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, id, "wi_noise", int64(seq)))
			case 1:
				f.Commit(ot.TenantA, nil, ot.RunEvent(ot.TenantA, ot.ProjectA, id, "run_2", int64(seq)))
			case 2:
				f.Commit(ot.TenantA, nil, ot.StepEvent(ot.TenantA, ot.ProjectA, id, "att_9", "run_2", int64(seq)))
			case 3:
				// A decoy that names run_1 only in its correlation and causation IDs.
				ev := ot.StepEvent(ot.TenantA, ot.ProjectA, id, "att_9", "run_2", int64(seq))
				ev.CorrelationID = "run_1"
				cause := "run_1"
				ev.CausationID = &cause
				f.Commit(ot.TenantA, nil, ev)
			}
		}
	}
	f.Commit(ot.TenantA, nil, ot.GateEvent(ot.TenantA, ot.ProjectA, next())) // an unobservable type

	var got []string
	var cursor string
	totalCheckpoints := 0
	round := 0
	reconnect := func() {
		t.Helper()
		evs, last, cps := drain(t, urls[round%2], token, cursor)
		round++
		for _, e := range evs {
			var env struct{ ID string }
			if err := jsonUnmarshal([]byte(e.Data), &env); err != nil {
				t.Fatal(err)
			}
			got = append(got, env.ID)
		}
		cursor, totalCheckpoints = last, totalCheckpoints+cps
	}

	// Round 1: nothing matches yet; the snapshot cursor is the only position.
	commitNoise(8)
	reconnect()
	if len(got) != 0 || cursor == "" {
		t.Fatalf("after the first connect: %v, cursor %q", got, cursor)
	}
	// Round 2: a matching event after an empty (fully filtered) stretch.
	commitNoise(12) // more than two pages of PageSize 5 with no match
	commitMatching("run")
	reconnect()
	// Round 3: a long quiet stretch, then two matches; resume goes through a checkpoint.
	commitNoise(13)
	reconnect() // idle stretch only: the checkpoint becomes the continuation
	if totalCheckpoints == 0 {
		t.Fatal("a filtered stream that scanned only excluded events must issue a checkpoint cursor")
	}
	commitMatching("step")
	commitMatching("run")
	reconnect()
	// Round 4: alternating handlers, repeated reconnects with new data each time.
	for i := 0; i < 5; i++ {
		commitNoise(3 + i)
		commitMatching([]string{"run", "step"}[i%2])
		reconnect()
	}
	reconnect() // nothing new: must deliver nothing and keep its place

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delivered %v\nwant      %v", got, want)
	}

	t.Run("a type filter composes with the run filter", func(t *testing.T) {
		url := urls[0] + "?type=step_attempt.state_changed"
		evs, _, _ := drain(t, url, token, "")
		if len(evs) != 0 { // a fresh stream starts at a snapshot: nothing in the past
			t.Fatalf("a snapshot-started stream replayed %d events", len(evs))
		}
		_, snapCursor, _ := drain(t, url, token, "")
		commitMatching("run") // excluded by the filter
		commitMatching("step")
		evs, _, _ = drain(t, url, token, snapCursor)
		if len(evs) != 1 || evs[0].Event != eventsv1.TypeStepAttemptStateChange {
			t.Fatalf("frames = %+v", evs)
		}
		// The filtered cursor is bound to its filter set.
		s := ot.Connect(t, urls[0], token, map[string]string{"Last-Event-ID": snapCursor})
		if s.Resp.StatusCode != 400 || ot.ErrorCode(s.Body) != "invalid_cursor" {
			t.Fatalf("a cursor of another filter set: %d %s", s.Resp.StatusCode, s.Body)
		}
	})

	t.Run("no broker is involved", func(t *testing.T) {
		// This package builds no NATS connection, outbox publisher or consumer:
		// the events above were never published, yet every observation was
		// delivered from the retained PostgreSQL history. The outbox rows are
		// still unpublished, which is the broker-outage condition itself.
		pending := f.CountIn(ot.TenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`)
		if pending < len(want) {
			t.Fatalf("only %d unpublished outbox rows for %d delivered events", pending, len(want))
		}
	})
}
