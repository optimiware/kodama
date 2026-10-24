package run_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clirun "github.com/optimiware/kodama/internal/cli/run"
)

func terminalGets(final string) func(n int) (int, any) {
	return func(n int) (int, any) {
		if n == 1 {
			return 200, runDoc("running", 2)
		}
		return 200, runDoc(final, 9)
	}
}

func fastOpts(s *sleeps) clirun.WatchOptions {
	return clirun.WatchOptions{MaxReconnects: 3, BackoffMin: 10 * time.Millisecond, BackoffMax: 80 * time.Millisecond, Sleep: s.sleep}
}

func watchJSON(t *testing.T, f *factory, opts clirun.WatchOptions, extra ...string) result {
	t.Helper()
	srv := clienttest.NewServer(t, f.handler())
	args := append([]string{"run", "watch", runID, "--output", "json", "--timeout", "20s"}, extra...)
	return runCtx(t.Context(), t, srv, opts, args...)
}

func Test_watch_resume_resync(t *testing.T) {
	t.Run("disconnect resumes after the last complete frame and replays are deduplicated", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			switch n {
			case 1:
				sw.snapshot("c1.s", "running", 2)
				sw.runEvent("evt_2", "c2.s", "running", 3)
				// A frame cut off mid-way must not advance the cursor.
				sw.raw("event: run.state_changed\nid: c3.s\ndata: {\"id\":\"evt_3\"")
			case 2:
				sw.runEvent("evt_2", "c2.s", "running", 3) // at-least-once replay
				sw.runEvent("evt_4", "c4.s", "succeeded", 4)
				<-r.Context().Done()
			}
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		n, ids := f.connections()
		if n != 2 || ids[0] != "" || ids[1] != "c2.s" {
			t.Fatalf("connections %d, Last-Event-ID %q", n, ids)
		}
		obs := decodeJSON(t, r.out)["observation"].(map[string]any)
		if obs["cursor"] != "c4.s" || obs["events"].(interface{ String() string }).String() != "2" || obs["reconnects"].(interface{ String() string }).String() != "1" {
			t.Fatalf("observation %v", obs)
		}
		if s.count() != 1 {
			t.Fatalf("%d backoffs", s.count())
		}
	})

	t.Run("checkpoint cursors are resume points", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("failed"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			if n == 1 {
				sw.snapshot("c1.s", "running", 2)
				sw.frame("checkpoint", "c5.s", map[string]any{"cursor": "c5.s"})
				return // clean end of stream
			}
			sw.runEvent("evt_6", "c6.s", "failed", 4)
			<-r.Context().Done()
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if _, ids := f.connections(); len(ids) != 2 || ids[1] != "c5.s" {
			t.Fatalf("Last-Event-ID %q", ids)
		}
	})

	t.Run("an expired cursor is resolved through the scoped snapshot, never a fresh stream", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded")}
		f.events = func(n int, w http.ResponseWriter, r *http.Request) {
			switch n {
			case 1:
				sw := startSSE(w)
				sw.snapshot("c1.s", "running", 2)
				sw.runEvent("evt_2", "c2.s", "running", 3)
			case 2:
				clienttest.WriteError(w, 410, "cursor_expired", "older than retained history", "req_410", nil)
			case 3:
				sw := startSSE(w)
				sw.runEvent("evt_10", "c10.s", "succeeded", 10)
				<-r.Context().Done()
			}
		}
		f.snapshot = func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"cursor": "c9.s", "resources": []any{
				map[string]any{"type": "run", "id": runID, "version": 9, "state": "running"},
			}}, nil)
		}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		n, ids := f.connections()
		if n != 3 || ids[1] != "c2.s" || ids[2] != "c9.s" {
			t.Fatalf("connections %d, Last-Event-ID %q: after expiry the stream must resume from the snapshot's cursor", n, ids)
		}
		if f.snaps != 1 {
			t.Fatalf("%d snapshots", f.snaps)
		}
		for _, a := range f.accepts {
			if a != "text/event-stream" && a != "application/json" {
				t.Fatalf("Accept %q", a)
			}
		}
		if obs := decodeJSON(t, r.out)["observation"].(map[string]any); obs["resyncs"].(interface{ String() string }).String() != "1" || obs["cursor"] != "c10.s" {
			t.Fatalf("observation %v", obs)
		}
	})

	t.Run("resync_required frame fetches a snapshot and resumes from it", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("cancelled")}
		f.events = func(n int, w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			if n == 1 {
				sw.snapshot("c1.s", "running", 2)
				sw.raw("event: resync_required\ndata: {\"code\":\"cursor_expired\",\"message\":\"x\"}\n\n")
				return
			}
			sw.runEvent("evt_11", "c11.s", "cancelled", 11)
			<-r.Context().Done()
		}
		f.snapshot = func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"cursor": "c7.s", "resources": []any{}}, nil)
		}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if _, ids := f.connections(); len(ids) != 2 || ids[1] != "c7.s" {
			t.Fatalf("Last-Event-ID %q", ids)
		}
		if s.count() != 0 {
			t.Fatalf("a resynchronization is not a failed reconnect: %d backoffs", s.count())
		}
	})

	t.Run("other cursor errors never become a fresh subscription", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				sw := startSSE(w)
				sw.snapshot("c1.s", "running", 2)
				return
			}
			clienttest.WriteError(w, 400, "invalid_cursor", "the cursor belongs to another scope", "req_400", nil)
		}, snapshot: func(w http.ResponseWriter, r *http.Request) { t.Error("snapshot fetched") }}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 1 || r.out != "" || errorOf(t, r.err)["code"] != "invalid_cursor" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if n, _ := f.connections(); n != 2 {
			t.Fatalf("%d connections: the CLI must not subscribe afresh", n)
		}
	})

	t.Run("a repeatedly expiring cursor gives up", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded")}
		f.events = func(n int, w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 410, "cursor_expired", "old", "req_410", nil)
		}
		f.snapshot = func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"cursor": "c3.s", "resources": []any{}}, nil)
		}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 1 || errorOf(t, r.err)["code"] != "resync_loop" {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
	})
}

func Test_watch_version_never_regresses(t *testing.T) {
	var s sleeps
	f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
		sw := startSSE(w)
		sw.snapshot("c1.s", "running", 5)
		sw.runEvent("evt_old", "c2.s", "failed", 3) // older than the snapshot: stale
		sw.heartbeat()
		sw.runEvent("evt_new", "c3.s", "succeeded", 6)
		<-r.Context().Done()
	}}
	r := watchJSON(t, f, fastOpts(&s))
	if r.code != 0 {
		t.Fatalf("exit %d err %q", r.code, r.err)
	}
	if f.runGets != 2 {
		t.Fatalf("%d run reads: the stale terminal event must not trigger a confirmation", f.runGets)
	}
}

func Test_watch_confirms_terminal_with_the_run_resource(t *testing.T) {
	var s sleeps
	f := &factory{t: t, events: func(n int, w http.ResponseWriter, r *http.Request) {
		sw := startSSE(w)
		if n == 1 {
			sw.snapshot("c1.s", "running", 2)
			sw.runEvent("evt_3", "c3.s", "succeeded", 3) // ahead of the read model
			return
		}
		sw.runEvent("evt_4", "c4.s", "succeeded", 4)
		<-r.Context().Done()
	}, runGet: func(n int) (int, any) {
		if n <= 2 {
			return 200, runDoc("running", 2) // the resource has not caught up
		}
		return 200, runDoc("succeeded", 4)
	}}
	r := watchJSON(t, f, fastOpts(&s))
	if r.code != 0 || decodeJSON(t, r.out)["run"].(map[string]any)["status"] != "succeeded" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
}

func Test_watch_stream_failures_terminate_without_stdout(t *testing.T) {
	cases := map[string]struct {
		events func(w http.ResponseWriter, r *http.Request)
		code   string
		exit   int
	}{
		"scope mismatch": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			e := runEvent("evt_x", "succeeded", 3)
			e["tenantId"] = "t_other"
			sw.frame("run.state_changed", "c2.s", e)
		}, "scope_mismatch", 1},
		"project mismatch": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			e := runEvent("evt_x", "succeeded", 3)
			e["projectId"] = "p_other"
			sw.frame("run.state_changed", "c2.s", e)
		}, "scope_mismatch", 1},
		"event before snapshot": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.runEvent("evt_x", "c2.s", "succeeded", 3)
		}, "malformed_stream", 1},
		"event without a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.frame("run.state_changed", "", runEvent("evt_x", "succeeded", 3))
		}, "malformed_stream", 1},
		"event name differs from its type": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.frame("run.created", "c2.s", runEvent("evt_x", "succeeded", 3))
		}, "malformed_stream", 1},
		"malformed event data": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.raw("event: run.state_changed\nid: c2.s\ndata: not-json\n\n")
		}, "malformed_stream", 1},
		"snapshot without a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.raw("event: snapshot\ndata: {\"resources\":[]}\n\n")
		}, "malformed_stream", 1},
		"mismatched snapshot id": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "other.s", map[string]any{"cursor": "c1.s", "resources": []any{}})
		}, "malformed_stream", 1},
		"oversized frame": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.raw("event: snapshot\ndata: " + strings.Repeat("x", 4096) + "\n\n")
		}, "frame_too_large", 1},
		"second snapshot": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.snapshot("c2.s", "running", 2)
		}, "malformed_stream", 1},
		"snapshot cursor not a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "", map[string]any{"cursor": "not a cursor", "resources": []any{}})
		}, "malformed_stream", 1},
		"snapshot without resources": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "c1.s", map[string]any{"cursor": "c1.s"})
		}, "malformed_stream", 1},
		"snapshot resources not an array": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "c1.s", map[string]any{"cursor": "c1.s", "resources": "none"})
		}, "malformed_stream", 1},
		"snapshot resource with version zero": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 0)
		}, "malformed_stream", 1},
		"snapshot resource without state": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "c1.s", map[string]any{"cursor": "c1.s", "resources": []any{map[string]any{"type": "run", "id": runID, "version": 2}}})
		}, "malformed_stream", 1},
		"snapshot resource of unknown type": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.frame("snapshot", "c1.s", map[string]any{"cursor": "c1.s", "resources": []any{map[string]any{"type": "gate", "id": "g_1", "version": 1, "state": "open"}}})
		}, "malformed_stream", 1},
		"checkpoint without a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.frame("checkpoint", "c2.s", map[string]any{})
		}, "malformed_stream", 1},
		"checkpoint cursor not a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.frame("checkpoint", "", map[string]any{"cursor": "bad cursor!"})
		}, "malformed_stream", 1},
		"resync with the wrong code": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.raw("event: resync_required\ndata: {\"code\":\"other\",\"message\":\"x\"}\n\n")
		}, "malformed_stream", 1},
		"resync without a payload": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.raw("event: resync_required\n\n")
		}, "malformed_stream", 1},
		"event cursor not a cursor": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.runEvent("evt_x", "bad cursor", "succeeded", 3)
		}, "malformed_stream", 1},
		"run event without a version": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			e := runEvent("evt_x", "succeeded", 3)
			e["resource"] = map[string]any{"type": "run", "id": runID}
			sw.frame("run.state_changed", "c2.s", e)
		}, "malformed_stream", 1},
		"run event with version zero": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.runEvent("evt_x", "c2.s", "succeeded", 0)
		}, "malformed_stream", 1},
		"run event with a fractional version": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.raw(`event: run.state_changed` + "\nid: c2.s\n" + `data: {"id":"evt_x","type":"run.state_changed","tenantId":"` + tenant + `","resource":{"type":"run","id":"` + runID + `","version":2.5},"data":{"to":"succeeded"}}` + "\n\n")
		}, "malformed_stream", 1},
		"run event without a target state": {func(w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			e := runEvent("evt_x", "succeeded", 3)
			e["data"] = map[string]any{"from": "running"}
			sw.frame("run.state_changed", "c2.s", e)
		}, "malformed_stream", 1},
		"not an event stream": {func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"ok": true}, nil)
		}, "malformed_response", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var s sleeps
			f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) { tc.events(w, r) }}
			opts := fastOpts(&s)
			opts.MaxFrameBytes = 1024
			r := watchJSON(t, f, opts)
			if int(r.code) != tc.exit || r.out != "" || errorOf(t, r.err)["code"] != tc.code {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			if n, _ := f.connections(); n != 1 {
				t.Fatalf("%d connections: an uninterpretable stream is not retried", n)
			}
		})
	}
}

func Test_watch_authorization_and_availability_on_reconnect(t *testing.T) {
	t.Run("permission revoked while reconnecting terminates", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				sw := startSSE(w)
				sw.snapshot("c1.s", "running", 2)
				return
			}
			clienttest.WriteError(w, 403, "forbidden", "run:read was revoked", "req_403", nil)
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 3 || r.out != "" || errorOf(t, r.err)["requestId"] != "req_403" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if n, _ := f.connections(); n != 2 {
			t.Fatalf("%d connections", n)
		}
	})
	t.Run("hidden or foreign run terminates", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 404, "not_found", "no such run", "req_404", nil)
		}}
		if r := watchJSON(t, f, fastOpts(&s)); r.code != 1 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if s.count() != 0 {
			t.Fatalf("%d backoffs for a 404", s.count())
		}
	})
	t.Run("throttling and outages are retried within bounds", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			switch n {
			case 1:
				clienttest.WriteError(w, 429, "rate_limited", "slow down", "req_429", map[string]string{"Retry-After": "1"})
			case 2:
				clienttest.WriteError(w, 503, "unavailable", "later", "req_503", nil)
			default:
				sw := startSSE(w)
				sw.snapshot("c1.s", "running", 2)
				sw.runEvent("evt_5", "c5.s", "succeeded", 5)
				<-r.Context().Done()
			}
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if s.count() != 2 {
			t.Fatalf("%d backoffs", s.count())
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.d[0] != 80*time.Millisecond { // Retry-After 1s, capped at BackoffMax
			t.Fatalf("delays %v", s.d)
		}
	})
	t.Run("an endless outage gives up after the bound", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 503, "unavailable", "down", "req_503", map[string]string{"Retry-After": "2"})
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 1 || r.out != "" || !strings.Contains(r.err, `"retryAfterSeconds":2`) {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if n, _ := f.connections(); n != 4 || s.count() != 3 {
			t.Fatalf("%d connections, %d backoffs", n, s.count())
		}
	})
	t.Run("a stream that keeps ending without progress gives up", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			startSSE(w)
		}}
		r := watchJSON(t, f, fastOpts(&s))
		if r.code != 1 || errorOf(t, r.err)["code"] != "observation_unavailable" {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
	})
}

func Test_watch_idle_stream_reconnects_and_heartbeats_keep_it_open(t *testing.T) {
	t.Run("silence reconnects", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			if n == 1 {
				sw.snapshot("c1.s", "running", 2)
				<-r.Context().Done() // goes silent: the client must drop and resume
				return
			}
			sw.runEvent("evt_3", "c3.s", "succeeded", 3)
			<-r.Context().Done()
		}}
		opts := fastOpts(&s)
		opts.IdleTimeout = 120 * time.Millisecond
		r := watchJSON(t, f, opts)
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if _, ids := f.connections(); len(ids) != 2 || ids[1] != "c1.s" {
			t.Fatalf("Last-Event-ID %q", ids)
		}
	})
	t.Run("heartbeats are liveness", func(t *testing.T) {
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			for i := 0; i < 12; i++ {
				time.Sleep(40 * time.Millisecond)
				sw.heartbeat()
			}
			sw.runEvent("evt_3", "c3.s", "succeeded", 3)
			<-r.Context().Done()
		}}
		opts := fastOpts(&s)
		opts.IdleTimeout = 150 * time.Millisecond
		if r := watchJSON(t, f, opts); r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if n, _ := f.connections(); n != 1 {
			t.Fatalf("%d connections", n)
		}
	})
}

func Test_watch_cancellation_releases_the_stream(t *testing.T) {
	closed := make(chan struct{})
	f := &factory{t: t, runGet: func(int) (int, any) { return 200, runDoc("running", 2) }, events: func(n int, w http.ResponseWriter, r *http.Request) {
		sw := startSSE(w)
		sw.snapshot("c1.s", "running", 2)
		<-r.Context().Done()
		close(closed)
	}}
	srv := clienttest.NewServer(t, f.handler())
	r := runCtx(t.Context(), t, srv, clirun.WatchOptions{IdleTimeout: time.Hour}, "run", "watch", runID, "--timeout", "250ms")
	if r.code != 5 || r.out != "" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the server still sees an open stream after the command ended")
	}
}

func Test_watch_refuses_redirects_and_foreign_destinations(t *testing.T) {
	target := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a redirected request reached another destination with %q", r.Header.Get("Authorization"))
	}))
	f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	}}
	r := watchJSON(t, f, fastOpts(&sleeps{}))
	if r.code != 1 || r.out != "" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if n := len(target.Requests()); n != 0 {
		t.Fatalf("%d requests followed the redirect", n)
	}
}

func Test_watch_requests_are_scoped_reads(t *testing.T) {
	var s sleeps
	f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("Accept") != "text/event-stream" || r.URL.RawQuery != "" {
			t.Errorf("stream request %s %q %v", r.Method, r.URL.RawQuery, r.Header)
		}
		sw := startSSE(w)
		sw.snapshot("c1.s", "running", 2)
		sw.runEvent("evt_2", "c2.s", "succeeded", 3)
		<-r.Context().Done()
	}}
	srv := clienttest.NewServer(t, f.handler())
	if r := runCtx(t.Context(), t, srv, fastOpts(&s), "run", "watch", runID, "--timeout", "10s"); r.code != 0 {
		t.Fatalf("exit %d err %q", r.code, r.err)
	}
	for _, rq := range srv.Requests() {
		if rq.Method != "GET" || !strings.HasPrefix(rq.Path, base+"/runs/"+runID) {
			t.Fatalf("request %s %s is outside the run's scope", rq.Method, rq.Path)
		}
	}
}

// A snapshot read for resynchronization that is invalid is refused without
// adopting its cursor or retrying the read.
func Test_watch_invalid_resync_snapshot_is_refused(t *testing.T) {
	for name, snap := range map[string]map[string]any{
		"bad cursor":      {"cursor": "bad cursor", "resources": []any{}},
		"no resources":    {"cursor": "c9.s"},
		"bad resource":    {"cursor": "c9.s", "resources": []any{map[string]any{"type": "run", "id": runID, "version": "x", "state": "running"}}},
		"zero version":    {"cursor": "c9.s", "resources": []any{map[string]any{"type": "run", "id": runID, "version": 0, "state": "running"}}},
		"resources typed": {"cursor": "c9.s", "resources": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			var s sleeps
			f := &factory{t: t, runGet: terminalGets("succeeded"),
				events: func(n int, w http.ResponseWriter, r *http.Request) {
					clienttest.WriteError(w, 410, "cursor_expired", "old", "req_410", nil)
				},
				snapshot: func(w http.ResponseWriter, r *http.Request) { clienttest.WriteJSON(w, 200, snap, nil) }}
			r := watchJSON(t, f, fastOpts(&s))
			if r.code != 1 || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			if n, _ := f.connections(); n != 1 || s.count() != 0 {
				t.Fatalf("%d connections, %d backoffs: nothing may be adopted or retried", n, s.count())
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.snaps != 1 {
				t.Fatalf("%d snapshot reads", f.snaps)
			}
		})
	}
}

// Test_observer_retry_after_parity: the observation adapter reads Retry-After
// exactly as the shared client does.
func Test_observer_retry_after_parity(t *testing.T) {
	future := time.Now().Add(40 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	opts := func(s *sleeps) clirun.WatchOptions {
		return clirun.WatchOptions{MaxReconnects: 3, BackoffMin: 10 * time.Millisecond, BackoffMax: time.Hour, Sleep: s.sleep}
	}
	delay := func(t *testing.T, header string) time.Duration {
		t.Helper()
		var s sleeps
		f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				h := map[string]string{}
				if header != "" {
					h["Retry-After"] = header
				}
				clienttest.WriteError(w, 503, "unavailable", "later", "req_503", h)
				return
			}
			sw := startSSE(w)
			sw.snapshot("c1.s", "running", 2)
			sw.runEvent("evt_5", "c5.s", "succeeded", 5)
			<-r.Context().Done()
		}}
		if r := watchJSON(t, f, opts(&s)); r.code != 0 {
			t.Fatalf("%q: exit %d err %q", header, r.code, r.err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.d) != 1 {
			t.Fatalf("%q: delays %v", header, s.d)
		}
		return s.d[0]
	}
	floor := 10 * time.Millisecond
	for header, want := range map[string]time.Duration{
		"3": 3 * time.Second, " 7 ": 7 * time.Second, "+4": 4 * time.Second, "0": floor,
		"2abc": floor, "1.5": floor, "-3": floor, "soon": floor, "": floor, "99999999999999": floor, past: floor,
	} {
		if got := delay(t, header); got != want {
			t.Errorf("Retry-After %q: waited %v, want %v", header, got, want)
		}
	}
	if got := delay(t, future); got < 30*time.Second || got > 40*time.Second {
		t.Errorf("HTTP-date guidance: waited %v", got)
	}
}

// Backoff is interruptible: a long Retry-After cannot outlast --timeout.
func Test_observer_cancellation_during_backoff(t *testing.T) {
	f := &factory{t: t, runGet: terminalGets("succeeded"), events: func(n int, w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 503, "unavailable", "later", "req_503", map[string]string{"Retry-After": "3600"})
	}}
	srv := clienttest.NewServer(t, f.handler())
	start := time.Now()
	r := runCtx(t.Context(), t, srv, clirun.WatchOptions{MaxReconnects: 3, BackoffMin: time.Hour, BackoffMax: time.Hour},
		"run", "watch", runID, "--output", "json", "--timeout", "300ms")
	if r.code != 5 || r.out != "" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the backoff was not interrupted")
	}
}
