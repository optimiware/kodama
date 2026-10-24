package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/gateway/openai"
)

func fastTimeouts(request time.Duration) gateway.Timeouts {
	return gateway.Timeouts{Request: request, Recheck: 200 * time.Millisecond, Finalize: 10 * time.Second, FinalizeAttempts: 4,
		FinalizeBackoff: 20 * time.Millisecond, Record: 5 * time.Second}
}

func usageOf(t *testing.T, e *gatewaytest.Env, id gateway.RequestID) gateway.Usage {
	t.Helper()
	u, err := e.Replica.Store.Usage(context.Background(), scopeA, id)
	if err != nil {
		t.Fatalf("usage of %s: %v", id, err)
	}
	return u
}

// Test_partial_stream: GM-05, GM-06, GM-A03. A stream interrupted after bytes
// reached the workload is visible as a failure, is never replaced or retried,
// keeps its uncertain usage, and disconnects, deadlines and run cancellation
// stop the upstream request.
func Test_partial_stream(t *testing.T) {
	t.Run("truncation after delivered chunks is visible and never retried", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		raw := body("truncate me")
		bound := inputBound(t, e, raw) + 64
		e.Provider.Script(gatewaytest.Truncated("gpt-a-2026", "par", "tial"))

		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(e.Replica, a, raw, sink)
		if !errors.Is(err, gateway.ErrProvider) {
			t.Fatalf("err %v, want ErrProvider", err)
		}
		frames, finished, failCode := sink.Snapshot()
		if finished || failCode != "stream_truncated" || len(frames) < 2 {
			t.Fatalf("frames=%d finished=%v failCode=%q: a truncated stream must show its failure and never a terminator", len(frames), finished, failCode)
		}
		u := usageOf(t, e, res.RequestID)
		if res.State != gateway.StateUnknown || u.Uncertainty != gateway.UncertaintyStreamInterrupted || u.Charged != bound || u.Basis != gateway.BasisEstimated {
			t.Fatalf("state %s usage %+v, want an uncertain charge of %d", res.State, u, bound)
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d: a partial stream was retried", n)
		}
		// Whether the provider finished the call is unknown, so the run stays
		// blocked: nothing is admitted or sent until trusted evidence settles it.
		before := append([]string(nil), frames...)
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("a retry before settlement: %v, want ErrBusy", err)
		}
		if adm, _ := e.Budget(a.RunID); adm != 1 || len(e.Provider.Calls()) != 1 {
			t.Fatalf("admitted=%d calls=%d: a blocked run admitted or sent a request", adm, len(e.Provider.Calls()))
		}
		if err := e.Replica.Svc.SettleUnknown(context.Background(), gateway.Actor{ID: gatewaytest.Admin}, scopeA, res.RequestID); err != nil {
			t.Fatalf("settle: %v", err)
		}
		// The delivered output stays as it was; an explicit retry is a new request.
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 10, 3, "ok"))
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatalf("explicit retry after settlement: %v", err)
		}
		after, _, _ := sink.Snapshot()
		if strings.Join(before, "") != strings.Join(after, "") {
			t.Fatal("the first sink was changed by the retry")
		}
		if adm, _ := e.Budget(a.RunID); adm != 2 || len(e.Provider.Calls()) != 2 {
			t.Fatalf("admitted=%d calls=%d: the retry must consume another request", adm, len(e.Provider.Calls()))
		}
	})

	t.Run("client disconnect cancels the upstream request", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		started := make(chan struct{})
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", started, nil, "one"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sink := &gatewaytest.Sink{OnFrame: func(int) { cancel() }}
		res, err := e.Replica.Svc.Invoke(ctx, gateway.InvokeRequest{Attempt: a.ID, Bearer: a.Token, Body: body("x"), Sink: sink})
		if err == nil {
			t.Fatal("a disconnected request reported success")
		}
		if res.State != gateway.StateUnknown || res.Code != "client_cancelled" {
			t.Fatalf("state %s code %s", res.State, res.Code)
		}
		if !gatewaytest.Wait(5*time.Second, func() bool { return e.Provider.Cancelled() == 1 }) {
			t.Fatal("the upstream request was not cancelled")
		}
		u := usageOf(t, e, res.RequestID)
		if u.Uncertainty != gateway.UncertaintyClientCancelled || u.Charged == 0 || u.Basis != gateway.BasisEstimated {
			t.Fatalf("usage %+v: closing a connection is not proof the provider stopped, so the bound is charged", u)
		}
		if _, err := e.Invoke(e.Replica, a, body("again"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("a disconnect is not proof of remote settlement, so the run stays blocked: %v", err)
		}
	})

	t.Run("a write failure to the workload ends the request as cancelled", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 10, 3, "a", "b", "c"))
		sink := &gatewaytest.Sink{FailAfter: 2}
		res, err := e.Invoke(e.Replica, a, body("x"), sink)
		if err == nil || res.Code != "client_cancelled" || res.State != gateway.StateUnknown {
			t.Fatalf("res %+v err %v", res, err)
		}
		if _, fin, _ := sink.Snapshot(); fin {
			t.Fatal("a broken sink received a terminator")
		}
	})

	t.Run("the upstream request is stopped before a blocked finalization is released", func(t *testing.T) {
		e := newWorld(t)
		to := fastTimeouts(20 * time.Second)
		to.Recheck = time.Hour // no heartbeat transaction may race the blocked finalization
		r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: to})
		a := e.NewAttempt(scopeA)
		var armed atomic.Bool
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		r.DB.OnBegin(func() {
			if !armed.Load() {
				return
			}
			once.Do(func() { close(entered) })
			<-release
		})
		defer r.DB.OnBegin(nil)
		var unblock sync.Once
		unblockFinalize := func() { unblock.Do(func() { close(release) }) }
		defer unblockFinalize()

		// The provider holds the stream open after two frames. The workload sink
		// accepts the first frame and then fails the second one.
		started := make(chan struct{})
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", started, nil, "one", "two"))
		sink := &gatewaytest.Sink{FailAfter: 2, OnFrame: func(int) { armed.Store(true) }}
		done := make(chan struct{})
		var res gateway.InvokeResult
		var err error
		go func() {
			defer close(done)
			res, err = e.Invoke(r, a, body("x"), sink)
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("finalization never began")
		}
		// Finalization is now blocked in the database. The provider call must
		// nevertheless already have been cancelled.
		if !gatewaytest.Wait(5*time.Second, func() bool { return e.Provider.Cancelled() == 1 }) {
			t.Fatal("the upstream request kept running while finalization was blocked")
		}
		select {
		case <-done:
			t.Fatal("the request returned before its finalization was released")
		default:
		}
		unblockFinalize()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("the request did not finish after finalization was released")
		}
		if err == nil || res.Code != "client_cancelled" || res.State != gateway.StateUnknown {
			t.Fatalf("res %+v err %v: closing the stream early must not change the recorded outcome", res, err)
		}
		u := usageOf(t, e, res.RequestID)
		if u.Uncertainty != gateway.UncertaintyClientCancelled || u.Charged == 0 || u.Basis != gateway.BasisEstimated {
			t.Fatalf("usage %+v: cancellation is not proof of remote completion", u)
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d", n)
		}
		if _, err := e.Invoke(r, a, body("again"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("the unresolved slot must be retained: %v", err)
		}
	})

	t.Run("the request deadline cancels the upstream request", func(t *testing.T) {
		e := newWorld(t)
		short := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: fastTimeouts(700 * time.Millisecond)})
		a := e.NewAttempt(scopeA)
		started := make(chan struct{})
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", started, nil, "one"))
		sink := &gatewaytest.Sink{}
		begin := time.Now()
		res, err := e.Invoke(short, a, body("x"), sink)
		if err == nil || res.Code != "deadline_exceeded" || res.State != gateway.StateUnknown {
			t.Fatalf("res %+v err %v", res, err)
		}
		if time.Since(begin) > 10*time.Second {
			t.Fatal("the deadline did not stop the request")
		}
		if !gatewaytest.Wait(5*time.Second, func() bool { return e.Provider.Cancelled() == 1 }) {
			t.Fatal("the upstream request was not cancelled at the deadline")
		}
		if _, fin, failCode := sink.Snapshot(); fin || failCode != "deadline_exceeded" {
			t.Fatalf("finished=%v failCode=%q", fin, failCode)
		}
		if u := usageOf(t, e, res.RequestID); u.Uncertainty != gateway.UncertaintyDeadlineExceeded {
			t.Fatalf("usage %+v", u)
		}
		if _, err := e.Invoke(short, a, body("again"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("a post-send deadline leaves the remote outcome unknown, so the run stays blocked: %v", err)
		}
	})

	t.Run("run cancellation stops forwarding and the upstream request", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		started := make(chan struct{})
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", started, nil, "one"))
		sink := &gatewaytest.Sink{}
		done := make(chan struct{})
		var res gateway.InvokeResult
		var err error
		go func() {
			defer close(done)
			res, err = e.Invoke(e.Replica, a, body("x"), sink)
		}()
		<-started
		e.CancelRun(a)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("the request outlived its cancelled run")
		}
		if err == nil || res.Code != "attempt_cancelled" || res.State != gateway.StateUnknown {
			t.Fatalf("res %+v err %v", res, err)
		}
		if !gatewaytest.Wait(5*time.Second, func() bool { return e.Provider.Cancelled() == 1 }) {
			t.Fatal("the upstream request was not cancelled")
		}
		if _, fin, failCode := sink.Snapshot(); fin || failCode != "attempt_cancelled" {
			t.Fatalf("finished=%v failCode=%q", fin, failCode)
		}
		if u := usageOf(t, e, res.RequestID); u.Uncertainty != gateway.UncertaintyAttemptCancelled || u.Charged == 0 {
			t.Fatalf("usage %+v", u)
		}
		// The cancelled attempt may not ask again.
		if _, err := e.Invoke(e.Replica, a, body("again"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("after cancellation: %v", err)
		}
	})

	t.Run("an idle provider stream is cut", func(t *testing.T) {
		e := newWorld(t)
		lim := openai.DefaultStreamLimits()
		lim.IdleTimeout = 500 * time.Millisecond
		idle := e.NewReplica(gatewaytest.ReplicaOptions{Stream: lim})
		a := e.NewAttempt(scopeA)
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", nil, nil, "one"))
		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(idle, a, body("x"), sink)
		if err == nil || res.Code != "stream_idle" {
			t.Fatalf("res %+v err %v", res, err)
		}
		if _, fin, failCode := sink.Snapshot(); fin || failCode != "stream_idle" {
			t.Fatalf("finished=%v failCode=%q", fin, failCode)
		}
	})

	t.Run("provider failures before any byte are plain errors, not streams", func(t *testing.T) {
		e := newWorld(t)
		raw := body("prompt-PROMPT-SENTINEL")
		bound := inputBound(t, e, raw) + 64
		cases := []struct {
			name     string
			behavior gatewaytest.Behavior
			code     string
			charge   int64
			unc      string
		}{
			{"server error", gatewaytest.Status(500), "provider_error", bound, gateway.UncertaintyProviderError},
			{"client error", gatewaytest.Status(400), "provider_rejected", 0, gateway.UncertaintyProviderRejected},
			{"rate limited", gatewaytest.Status(429), "provider_rejected", 0, gateway.UncertaintyProviderRejected},
			{"redirect", gatewaytest.Redirect(307, "https://elsewhere.invalid/steal"), "provider_redirect", 0, gateway.UncertaintyProviderRejected},
		}
		for _, c := range cases {
			a := e.NewAttempt(scopeA) // an uncertain failure keeps its run blocked
			e.Provider.Script(c.behavior)
			sink := &gatewaytest.Sink{}
			res, err := e.Invoke(e.Replica, a, raw, sink)
			if !errors.Is(err, gateway.ErrProvider) || sink.Started() {
				t.Fatalf("%s: err %v started=%v", c.name, err, sink.Started())
			}
			if strings.Contains(err.Error(), gatewaytest.Bearer) || strings.Contains(err.Error(), "PROMPT-SENTINEL") {
				t.Fatalf("%s: the error leaks provider-quoted secrets: %v", c.name, err)
			}
			u := usageOf(t, e, res.RequestID)
			if res.Code != c.code || u.Charged != c.charge || u.Uncertainty != c.unc {
				t.Fatalf("%s: code %s usage %+v, want %s charge %d %s", c.name, res.Code, u, c.code, c.charge, c.unc)
			}
		}
		if n := len(e.Provider.Calls()); n != len(cases) {
			t.Fatalf("provider calls %d, want exactly one per explicit request", n)
		}
	})
}

// blockingCredentials is a credential seam whose Open waits for the request's
// cancellation, so the interruption lands before any provider send.
type blockingCredentials struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingCredentials) Open(ctx context.Context, _ gateway.Workload, _ gateway.RunBinding, _ gateway.RequestID) (*gateway.Credential, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingCredentials) Check(context.Context, domain.Scope, gateway.CredentialRef) error {
	return nil
}

// Test_pre_forward_cancellation: GM-05, GM-06. An interruption while the
// credential is being prepared sends nothing, persists one failed/unsent outcome
// with the interruption's reason code at once (the lifecycle leaves admitted only
// to forwarding or failed), charges nothing and releases the run's slot.
func Test_pre_forward_cancellation(t *testing.T) {
	check := func(t *testing.T, e *gatewaytest.Env, a gatewaytest.Attempt, res gateway.InvokeResult, code string) {
		t.Helper()
		if res.State != gateway.StateFailed || res.Code != code {
			t.Fatalf("result %s/%s, want failed/%s", res.State, res.Code, code)
		}
		u := usageOf(t, e, res.RequestID)
		if u.Result != gateway.StateFailed || u.Code != code || u.Uncertainty != gateway.UncertaintyNotSent || u.Charged != 0 || u.Basis != gateway.BasisEstimated {
			t.Fatalf("usage %+v", u)
		}
		if got := e.Requests(a.RunID); len(got) != 1 || got[0] != [2]string{"failed", code} {
			t.Fatalf("requests %v: the original reason must persist, not a reconciliation replacement", got)
		}
		if adm, charged := e.Budget(a.RunID); adm != 1 || charged != 0 {
			t.Fatalf("admitted=%d charged=%d: the request count is consumed and nothing is charged", adm, charged)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d: nothing may be sent", n)
		}
		// The slot is released: the next request is admitted and sent.
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 10, 3, "ok"))
		if _, err := e.Invoke(e.Replica, a, body("next"), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("the slot was not released: %v", err)
		}
	}

	t.Run("client cancellation during credential preparation", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		creds := &blockingCredentials{entered: make(chan struct{})}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Credentials: creds, Timeouts: quietTimeouts()})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-creds.entered; cancel() }()
		sink := &gatewaytest.Sink{}
		res, err := r.Svc.Invoke(ctx, gateway.InvokeRequest{Attempt: a.ID, Bearer: a.Token, Body: body("x"), Sink: sink})
		if !errors.Is(err, gateway.ErrUnavailable) {
			t.Fatalf("err %v, want ErrUnavailable", err)
		}
		check(t, e, a, res, "client_cancelled")
	})

	t.Run("deadline during credential preparation", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		creds := &blockingCredentials{entered: make(chan struct{})}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Credentials: creds, Timeouts: fastTimeouts(500 * time.Millisecond)})
		res, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{})
		if !errors.Is(err, gateway.ErrUnavailable) {
			t.Fatalf("err %v, want ErrUnavailable", err)
		}
		check(t, e, a, res, "deadline_exceeded")
	})
}
