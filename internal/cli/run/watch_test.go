package run_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clirun "github.com/optimiware/kodama/internal/cli/run"
)

// progressing builds a factory whose run is queued at first read and terminal
// (status) afterwards, with a stream that reports queued -> running -> status.
func progressing(t *testing.T, terminal string) *factory {
	return &factory{
		t: t,
		runGet: func(n int) (int, any) {
			if n == 1 {
				return 200, runDoc("queued", 1)
			}
			d := runDoc(terminal, 4)
			d["finishedAt"] = "2026-10-19T10:09:00Z"
			return 200, d
		},
		events: func(n int, w http.ResponseWriter, r *http.Request) {
			s := startSSE(w)
			s.snapshot("c1.s", "queued", 1)
			s.runEvent("evt_2", "c2.s", "running", 2)
			s.frame("step_attempt.state_changed", "c3.s", map[string]any{
				"id": "evt_3", "schemaVersion": 1, "type": "step_attempt.state_changed", "tenantId": tenant, "projectId": project,
				"resource": map[string]any{"type": "step_attempt", "id": "att_1", "version": 2}, "data": map[string]any{"to": "running"},
			})
			s.runEvent("evt_4", "c4.s", terminal, 4)
			<-r.Context().Done() // the client closes the stream once it is terminal
		},
	}
}

// Test_watch_terminal_exit: queued/running observations reach each terminal
// state; watching and reading succeed for every terminal run, --exit-on-terminal
// fails (exit 1) only for failed and cancelled runs, and an interruption or
// deadline exits 5 with nothing on stdout.
func Test_watch_terminal_exit(t *testing.T) {
	for _, tc := range []struct {
		terminal string
		exitOn   int
	}{{"succeeded", 0}, {"failed", 1}, {"cancelled", 1}} {
		t.Run(tc.terminal, func(t *testing.T) {
			f := progressing(t, tc.terminal)
			srv := clienttest.NewServer(t, f.handler())

			r := runCLI(t, srv, "run", "watch", runID, "--output", "json")
			if r.code != 0 || r.err != "" {
				t.Fatalf("watch: exit %d err %q", r.code, r.err)
			}
			out := decodeJSON(t, r.out)
			run := out["run"].(map[string]any)
			obs := out["observation"].(map[string]any)
			if run["status"] != tc.terminal || obs["cursor"] != "c4.s" || obs["events"].(interface{ String() string }).String() != "3" {
				t.Fatalf("output %s", r.out)
			}

			f = progressing(t, tc.terminal)
			srv = clienttest.NewServer(t, f.handler())
			x := runCLI(t, srv, "run", "watch", runID, "--exit-on-terminal", "--output", "json")
			if x.code != 0 && tc.exitOn == 0 || int(x.code) != tc.exitOn {
				t.Fatalf("--exit-on-terminal: exit %d out %q err %q", x.code, x.out, x.err)
			}
			if tc.exitOn == 1 {
				if x.out != "" {
					t.Errorf("a failing watch wrote to stdout: %q", x.out)
				}
				if e := errorOf(t, x.err); e["code"] != "run_"+tc.terminal {
					t.Errorf("envelope %v", e)
				}
			}

			// Reading a failed or cancelled record is an ordinary success.
			g := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteJSON(w, 200, runDoc(tc.terminal, 4), nil)
			}))
			if rr := runCLI(t, g, "run", "get", runID, "--output", "json"); rr.code != 0 {
				t.Errorf("get: exit %d", rr.code)
			}
		})
	}
}

func Test_watch_already_terminal_opens_no_stream(t *testing.T) {
	f := &factory{t: t, runGet: func(int) (int, any) { return 200, runDoc("failed", 5) },
		events: func(int, http.ResponseWriter, *http.Request) { t.Error("a stream was opened") }}
	srv := clienttest.NewServer(t, f.handler())
	r := runCLI(t, srv, "run", "watch", runID, "--exit-on-terminal")
	if r.code != 1 || r.out != "" || !strings.Contains(r.err, "ended failed") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if n, _ := f.connections(); n != 0 {
		t.Fatalf("%d connections", n)
	}
}

func Test_watch_timeout_and_interruption_exit_5_without_stdout(t *testing.T) {
	hold := func(t *testing.T) *factory {
		return &factory{
			t:      t,
			runGet: func(int) (int, any) { return 200, runDoc("running", 2) },
			events: func(n int, w http.ResponseWriter, r *http.Request) {
				s := startSSE(w)
				s.snapshot("c1.s", "running", 2)
				for {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
						s.heartbeat()
					}
				}
			},
		}
	}
	// A deadline.
	srv := clienttest.NewServer(t, hold(t).handler())
	r := runCLI(t, srv, "run", "watch", runID, "--timeout", "300ms", "--output", "json")
	if r.code != 5 || r.out != "" || errorOf(t, r.err)["code"] != "timeout" {
		t.Fatalf("deadline: exit %d out %q err %q", r.code, r.out, r.err)
	}
	// An interruption.
	srv = clienttest.NewServer(t, hold(t).handler())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	r = runCtx(ctx, t, srv, clirun.WatchOptions{}, "run", "watch", runID, "--output", "json")
	if r.code != 5 || r.out != "" || errorOf(t, r.err)["code"] != "interrupted" {
		t.Fatalf("interrupt: exit %d out %q err %q", r.code, r.out, r.err)
	}
}

func Test_watch_does_not_transition_the_work_item(t *testing.T) {
	// The fake rejects every route but the run read and the event stream, so a
	// work transition after success would fail the test.
	f := progressing(t, "succeeded")
	srv := clienttest.NewServer(t, f.handler())
	r := runCLI(t, srv, "run", "watch", runID)
	if r.code != 0 || !strings.Contains(r.out, "never completes a work item") {
		t.Fatalf("exit %d out %q", r.code, r.out)
	}
	for _, rq := range srv.Requests() {
		if strings.Contains(rq.Path, "work-items") || rq.Method != "GET" {
			t.Fatalf("unexpected request %s %s", rq.Method, rq.Path)
		}
	}
}
