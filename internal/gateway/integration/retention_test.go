package integration_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

const (
	promptSentinel     = "PROMPT-SENTINEL-t032"
	completionSentinel = "COMPLETION-SENTINEL-t032"
	toolArgSentinel    = "TOOLARG-SENTINEL-t032"
	toolResultSentinel = "TOOLRESULT-SENTINEL-t032"
)

var sentinels = []string{promptSentinel, completionSentinel, toolArgSentinel, toolResultSentinel, gatewaytest.Bearer, "CAUSE-SENTINEL"}

// toolBody is a request carrying a prompt, a tool call with arguments and a tool
// result: every kind of content the gateway must not retain.
func toolBody() []byte {
	return []byte(`{"model":"gpt-a","stream":true,"max_tokens":64,"messages":[` +
		`{"role":"user","content":"` + promptSentinel + `"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"` + toolArgSentinel + `\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"` + toolResultSentinel + `"}],` +
		`"tools":[{"type":"function","function":{"name":"run","description":"runs","parameters":{"type":"object"}}}]}`)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func absent(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range sentinels {
		if strings.Contains(text, s) {
			t.Errorf("%s contains the sentinel %q", what, s)
		}
	}
}

// Test_content_retention_off: GM-04, GM-11, GM-A08. With recording off, no
// prompt, completion, tool argument, provider error text or credential is
// persisted, enqueued, logged, handed to the recorder or returned in an error,
// while attributable usage remains; a workload cannot enable recording.
func Test_content_retention_off(t *testing.T) {
	e := newWorld(t)
	logs := &syncBuffer{}
	rec := &gatewaytest.Recorder{}
	r := e.NewReplica(gatewaytest.ReplicaOptions{Recorder: rec, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	a := e.NewAttempt(scopeA)

	e.Provider.Script(
		gatewaytest.OK("gpt-a-2026", 40, 3, completionSentinel),
		gatewaytest.Status(500),
		gatewaytest.Truncated("gpt-a-2026", completionSentinel),
	)
	var errs []string
	var ids []gateway.RequestID
	var attempts []gatewaytest.Attempt
	for i := 0; i < 3; i++ {
		if i > 0 {
			a = e.NewAttempt(scopeA) // an uncertain failure keeps its own run blocked
		}
		attempts = append(attempts, a)
		res, err := e.Invoke(r, a, toolBody(), &gatewaytest.Sink{})
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i > 0 && err == nil {
			t.Fatalf("request %d unexpectedly succeeded", i)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
		ids = append(ids, res.RequestID)
	}
	absent(t, "errors", strings.Join(errs, "\n"))
	absent(t, "service diagnostics", logs.String())
	absent(t, "stored state, audit and outbox", e.Dump(scopeA.TenantID))
	if rec.Count() != 0 {
		t.Fatalf("the recorder received %d exchanges although recording is off", rec.Count())
	}
	// Attributable usage remains for every request: run, attempt, model, uncertainty.
	for i, id := range ids {
		u := usageOf(t, e, id)
		if u.RunID != attempts[i].RunID || u.AttemptID != attempts[i].ID || u.RequestedModel != "gpt-a" || u.Uncertainty == "" {
			t.Fatalf("usage lost its attribution: %+v", u)
		}
	}
	if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 3 {
		t.Fatalf("usage events %d, want 3", n)
	}
	if n := e.Count(`SELECT count(*) FROM model_requests WHERE recording`); n != 0 {
		t.Fatalf("%d requests claim recording", n)
	}

	t.Run("a workload cannot enable recording", func(t *testing.T) {
		before := len(e.Provider.Calls())
		for _, extra := range []string{`,"record":true`, `,"metadata":{"kodama_record":true}`, `,"store":true`} {
			raw := []byte(strings.Replace(string(body("x")), `"stream":true`, `"stream":true`+extra, 1))
			if _, err := e.Invoke(r, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", extra, err)
			}
		}
		if len(e.Provider.Calls()) != before || rec.Count() != 0 {
			t.Fatal("a rejected request reached the provider or the recorder")
		}
	})
}

// Test_recording_authorization: GM-11. Only an authorized policy change enables
// recording, an absent recorder fails closed, a recording failure stays visible
// without touching the workload's result or the usage record, and a run keeps
// its own policy binding.
func Test_recording_authorization(t *testing.T) {
	t.Run("recording needs a recorder and project settings authority", func(t *testing.T) {
		e := newWorld(t)
		withRecording := e.Edit()
		withRecording.Recording = true

		// No recorder wired: enabling recording is refused and nothing is stored.
		if _, err := e.TryConfigure(e.Replica, scopeA, withRecording); !errors.Is(err, gateway.ErrRecording) {
			t.Fatalf("recording without a recorder: %v", err)
		}
		rec := &gatewaytest.Recorder{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Recorder: rec})
		cur, err := r.Svc.GetSettings(context.Background(), gateway.Actor{ID: gatewaytest.Viewer}, scopeA)
		if err != nil || cur.Recording {
			t.Fatalf("viewer read: %+v %v", cur, err)
		}
		// A viewer may read but not enable recording.
		_, err = r.Svc.ReplaceSettings(context.Background(), gateway.Actor{ID: gatewaytest.Viewer}, scopeA, cur.Revision, withRecording, "req")
		if !errors.Is(err, gateway.ErrDenied) && !errors.Is(err, gateway.ErrHidden) {
			t.Fatalf("viewer enabling recording: %v", err)
		}
		if got, _ := r.Svc.GetSettings(context.Background(), gateway.Actor{ID: gatewaytest.Admin}, scopeA); got.Recording || got.Revision != cur.Revision {
			t.Fatalf("a refused change was stored: %+v", got)
		}
		if res, err := e.TryConfigure(r, scopeA, withRecording); err != nil || !res.Settings.Recording || !res.Changed {
			t.Fatalf("admin enabling recording: %+v %v", res, err)
		}
	})

	t.Run("recorded exchanges carry ownership and failures stay visible", func(t *testing.T) {
		e := newWorld(t)
		rec := &gatewaytest.Recorder{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Recorder: rec, MaxRecordedBytes: 1 << 20})
		on := e.Edit()
		on.Recording = true
		e.Configure(r, scopeA, on)
		a := e.NewAttempt(scopeA)

		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 40, 3, completionSentinel), gatewaytest.OK("gpt-a-2026", 5, 3, "second"))
		res, err := e.Invoke(r, a, toolBody(), &gatewaytest.Sink{})
		if err != nil || res.RecordingFailed {
			t.Fatalf("recorded request: %+v %v", res, err)
		}
		if rec.Count() != 1 {
			t.Fatalf("recorder calls %d", rec.Count())
		}
		x := rec.Calls[0]
		if x.Scope != scopeA || x.RunID != a.RunID || x.AttemptID != a.ID || x.RequestID != res.RequestID || len(x.Frames) == 0 || x.Completion.Model != "gpt-a" {
			t.Fatalf("exchange without ownership or content: %+v", x)
		}
		var outcome string
		e.Scalar(&outcome, `SELECT recording_outcome FROM model_requests WHERE id = $1`, string(res.RequestID))
		if outcome != "recorded" {
			t.Fatalf("recording outcome %q", outcome)
		}
		// Even then the gateway's own rows keep no content.
		absent(t, "stored state", strings.ReplaceAll(e.Dump(scopeA.TenantID), "CAUSE-SENTINEL", ""))

		// A recorder failure is attributable, and changes neither result nor usage.
		rec.Err = gatewaytest.ErrInjected
		sink := &gatewaytest.Sink{}
		res, err = e.Invoke(r, a, body("x"), sink)
		if err != nil || !res.RecordingFailed || res.State != gateway.StateSucceeded {
			t.Fatalf("with a failing recorder: %+v %v", res, err)
		}
		if _, fin, _ := sink.Snapshot(); !fin {
			t.Fatal("a recording failure changed the workload's stream")
		}
		e.Scalar(&outcome, `SELECT recording_outcome FROM model_requests WHERE id = $1`, string(res.RequestID))
		if outcome != "failed" || usageOf(t, e, res.RequestID).Charged != 8 {
			t.Fatalf("recording outcome %q or usage lost", outcome)
		}
	})

	t.Run("a run keeps its binding and a recorder is required at serve time", func(t *testing.T) {
		e := newWorld(t)
		rec := &gatewaytest.Recorder{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Recorder: rec})

		// run1 binds while recording is off: enabling it later never records run1.
		run1 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(r, run1, body("a"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		on := e.Edit()
		on.Recording = true
		e.Configure(r, scopeA, on)
		if _, err := e.Invoke(r, run1, body("b"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if rec.Count() != 0 {
			t.Fatal("a run bound before recording was enabled was recorded")
		}

		// run2 binds with recording on, so it records; a replica without a
		// recorder serving it records nothing (fail closed, no error).
		run2 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(r, run2, body("c"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if rec.Count() != 1 {
			t.Fatalf("recorder calls %d, want 1", rec.Count())
		}
		if _, err := e.Invoke(e.Replica, run2, body("d"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if rec.Count() != 1 {
			t.Fatal("a replica without a recorder recorded")
		}

		// Turning recording off stops it at once, even for a run bound with it on.
		off := e.Edit()
		e.Configure(r, scopeA, off)
		if _, err := e.Invoke(r, run2, body("e"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if rec.Count() != 1 {
			t.Fatal("recording continued after the project turned it off")
		}
	})

	t.Run("the recording bound truncates and says so", func(t *testing.T) {
		e := newWorld(t)
		rec := &gatewaytest.Recorder{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Recorder: rec, MaxRecordedBytes: 200})
		on := e.Edit()
		on.Recording = true
		e.Configure(r, scopeA, on)
		a := e.NewAttempt(scopeA)
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 5, 5, strings.Repeat("z", 300)))
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if rec.Count() != 1 || !rec.Calls[0].Truncated {
			t.Fatalf("exchange %+v should be marked truncated", rec.Calls)
		}
		total := 0
		for _, f := range rec.Calls[0].Frames {
			total += len(f)
		}
		if total > 200 {
			t.Fatalf("recorded %d bytes over the bound", total)
		}
	})
}

var _ = domain.Scope{}
var _ = executiontest.ScopeA
