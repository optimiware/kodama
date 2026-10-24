package gateway_test

// Model budget unit qualification (T-083): the real gateway Service over the
// in-memory store (gatewaytest.MemStore on memdb), a controlled clock and
// scripted provider streams. No database, TLS server, secret store or
// Kubernetes object exists, and no model is called, so nothing here is provider
// qualification (T-085) or OpenCode qualification (T-031).
//
// Requirements exercised: GM-03/GM-04 (limits and bounded charging), GM-05/GM-06
// (failure and interruption accounting), GM-12 (only the pinned protocol subset
// is forwarded, and no provider or harness support is claimed), with tenant
// attribution and credential custody (GM-01, GM-02, GM-11).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/gateway/openai"
)

const (
	budgetAllowance = 100
	budgetMaxInput  = 200
	budgetMaxOutput = 64
)

func tight(e *gateway.SettingsEdit) {
	e.Limits = gateway.Limits{MaxRequests: 50, TokenAllowance: budgetAllowance, MaxRequestBytes: 2048, MaxInputTokens: budgetMaxInput, MaxOutputTokens: budgetMaxOutput}
}

// roomy is tight with an allowance that one uncertain charge cannot use up.
func roomy(e *gateway.SettingsEdit) {
	tight(e)
	e.Limits.TokenAllowance = 5000
}

func pad(n int) string { return strings.Repeat("a", n) }

// bound is the gateway's conservative input estimate of a request body.
func bound(t *testing.T, raw []byte) int64 {
	t.Helper()
	c, err := openai.NewDecoder().Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := gatewaytest.Estimator(t).InputBound(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func configured(t *testing.T, mut func(*gateway.SettingsEdit)) *gatewaytest.Unit {
	t.Helper()
	u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})
	u.Configure(gatewaytest.ScopeA, mut)
	return u
}

func shortTimeouts(mut func(*gateway.Timeouts)) gateway.Timeouts {
	to := gateway.DefaultTimeouts()
	to.Recheck, to.FinalizeBackoff = time.Minute, time.Millisecond
	mut(&to)
	return to
}

func kind(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// slotHeld reports whether another request of the attempt's run would be refused
// because one is unresolved. It asks the store directly, so the provider is not
// involved.
func slotHeld(t *testing.T, u *gatewaytest.Unit, a gatewaytest.UnitAttempt) bool {
	t.Helper()
	// A sibling attempt of the run probes, so the answer does not depend on the
	// asking attempt still being authorized.
	a = u.NextAttempt(a)
	id, err := gateway.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	adm, err := u.Store.Admit(context.Background(), gateway.AdmitCommand{
		Workload: a.Workload, RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 1, OutputMax: 1, Now: u.Clock.Now(),
	})
	switch {
	case errors.Is(err, gateway.ErrBusy):
		return true
	case err != nil:
		t.Fatalf("probing the slot: %v", err)
	}
	// The probe consumed an admission; give the slot back unsent.
	_, err = u.Store.Finalize(context.Background(), gateway.FinalizeCommand{
		Scope: a.Workload.Scope, RunID: a.Workload.RunID, AttemptID: a.Workload.AttemptID, RequestID: adm.Request.ID,
		Outcome: gateway.Outcome{Result: gateway.StateFailed, Code: "probe", Settlement: gateway.SettlementNotSent}, Now: u.Clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return false
}

// Test_unit_model_budget: GM-03/GM-04/GM-05/GM-06/GM-12. Limits are verified
// before provider traffic; observed usage is charged exactly; uncertain usage
// is charged the request bound and an uncertain remote completion keeps the
// run's single slot; usage is attributed to the verified workload and its
// tenant; and a run's budget is never touched by another run or tenant.
func Test_unit_model_budget(t *testing.T) {
	ctx := context.Background()

	t.Run("size, input and output bounds refuse before any provider traffic and consume nothing", func(t *testing.T) {
		u := configured(t, tight)
		a := u.NewAttempt(gatewaytest.ScopeA)
		for name, raw := range map[string][]byte{
			"input bound":  gatewaytest.Body("gpt-a", pad(400), 32),
			"output bound": gatewaytest.Body("gpt-a", "hi", budgetMaxOutput+1),
			"body size":    gatewaytest.Body("gpt-a", pad(3000), 32),
		} {
			if _, err := u.Invoke(a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrLimitExceeded) {
				t.Errorf("%s: %v, want ErrLimitExceeded", name, err)
			}
		}
		if n := len(u.Provider.Calls()); n != 0 {
			t.Fatalf("%d provider calls", n)
		}
		if reqs := u.Store.Requests(a.Workload.Scope, a.Workload.RunID); len(reqs) != 0 {
			t.Fatalf("%d requests were consumed by refusals", len(reqs))
		}
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		if _, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", budgetMaxOutput), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("a request at the output bound: %v", err)
		}
	})

	t.Run("observed usage is charged exactly and admission stops at the allowance within the published overshoot", func(t *testing.T) {
		u := configured(t, tight)
		a := u.NewAttempt(gatewaytest.ScopeA)
		raw := gatewaytest.Body("gpt-a", pad(100), budgetMaxOutput)
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 50, 10, "x"), gatewaytest.Reply("gpt-a-2026", 50, 10, "x"), gatewaytest.Reply("gpt-a-2026", 50, 10, "x"))
		for i := 1; i <= 2; i++ {
			res, err := u.Invoke(a, raw, &gatewaytest.Sink{})
			if err != nil || res.Usage.Charged != 60 || res.Usage.Basis != gateway.BasisObserved || res.Usage.Uncertainty != gateway.UncertaintyNone {
				t.Fatalf("request %d: %v %+v", i, err, res.Usage)
			}
		}
		if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged != 120 {
			t.Fatalf("charged %d", charged)
		}
		_, err := u.Invoke(a, raw, &gatewaytest.Sink{})
		kind(t, err, gateway.ErrLimitExceeded)
		if n := len(u.Provider.Calls()); n != 2 {
			t.Fatalf("the refused request reached the provider: %d calls", n)
		}
		if reqs := u.Store.Requests(a.Workload.Scope, a.Workload.RunID); len(reqs) != 2 {
			t.Fatalf("%d requests", len(reqs))
		}
		// The published bound: charged < allowance + one request's bound.
		limits := gateway.Limits{MaxInputTokens: budgetMaxInput, MaxOutputTokens: budgetMaxOutput}
		if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged >= budgetAllowance+limits.Overshoot() {
			t.Fatalf("charge %d exceeds the published overshoot bound", charged)
		}
	})

	t.Run("the request cap counts every admission whatever its outcome", func(t *testing.T) {
		u := configured(t, func(e *gateway.SettingsEdit) {
			e.Limits = gateway.Limits{MaxRequests: 2, TokenAllowance: 10000, MaxRequestBytes: 2048, MaxInputTokens: 200, MaxOutputTokens: 64}
		})
		a := u.NewAttempt(gatewaytest.ScopeA)
		u.Provider.Script(gatewaytest.ReplyStatus(400, true), gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		raw := gatewaytest.Body("gpt-a", "hi", 32)
		_, err := u.Invoke(a, raw, &gatewaytest.Sink{})
		kind(t, err, gateway.ErrProvider)
		if _, err := u.Invoke(a, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		_, err = u.Invoke(a, raw, &gatewaytest.Sink{})
		kind(t, err, gateway.ErrLimitExceeded)
		if admitted, _ := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); admitted != 2 {
			t.Fatalf("admitted %d", admitted)
		}
		// Another run of the same project starts with its own budget.
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		if _, err := u.Invoke(u.NewAttempt(gatewaytest.ScopeA), raw, &gatewaytest.Sink{}); err != nil {
			t.Fatalf("another run: %v", err)
		}
	})

	t.Run("an uncertain outcome is charged the request bound and keeps the slot until settled", func(t *testing.T) {
		raw := gatewaytest.Body("gpt-a", pad(100), budgetMaxOutput)
		for name, tc := range map[string]struct {
			step    gatewaytest.Step
			err     bool
			charged func(b int64) int64
			result  gateway.RequestState
			code    string
			basis   gateway.Basis
			unc     string
			slot    bool
		}{
			"a complete stream without usage": {gatewaytest.ReplyNoUsage("gpt-a-2026", "x"), false, func(b int64) int64 { return b }, gateway.StateSucceeded,
				"usage_missing", gateway.BasisEstimated, gateway.UncertaintyUsageMissing, false},
			"malformed usage": {gatewaytest.ReplyInvalidUsage("gpt-a-2026"), false, func(b int64) int64 { return b }, gateway.StateSucceeded,
				"usage_invalid", gateway.BasisEstimated, gateway.UncertaintyUsageInvalid, false},
			"a truncated stream": {gatewaytest.ReplyTruncated("gpt-a-2026", "par"), true, func(b int64) int64 { return b }, gateway.StateUnknown,
				"stream_truncated", gateway.BasisEstimated, gateway.UncertaintyStreamInterrupted, true},
			"a provider error status after the send": {gatewaytest.ReplyStatus(503, true), true, func(b int64) int64 { return b }, gateway.StateUnknown,
				"provider_error", gateway.BasisEstimated, gateway.UncertaintyProviderError, true},
			"a provider timeout": {gatewaytest.ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderTimeout, Sent: true}), true, func(b int64) int64 { return b }, gateway.StateUnknown,
				"provider_timeout", gateway.BasisEstimated, gateway.UncertaintyDeadlineExceeded, true},
			"a protocol violation": {gatewaytest.ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderProtocol, Sent: true}), true, func(b int64) int64 { return b }, gateway.StateUnknown,
				"protocol_violation", gateway.BasisEstimated, gateway.UncertaintyProtocol, true},
			"usage above the verified bounds": {gatewaytest.Reply("gpt-a-2026", budgetMaxInput+50, 10, "x"), true, func(int64) int64 { return budgetMaxInput + 50 + 10 }, gateway.StateFailed,
				"bound_exceeded", gateway.BasisEstimated, gateway.UncertaintyBoundExceeded, false},
		} {
			t.Run(name, func(t *testing.T) {
				u := configured(t, roomy)
				a := u.NewAttempt(gatewaytest.ScopeA)
				b := bound(t, raw) + budgetMaxOutput
				u.Provider.Script(tc.step)
				res, err := u.Invoke(a, raw, &gatewaytest.Sink{})
				if (err != nil) != tc.err {
					t.Fatalf("error = %v, want error: %v", err, tc.err)
				}
				if res.Usage == nil {
					t.Fatalf("no usage recorded: %+v", res)
				}
				got := res.Usage
				if got.Charged != tc.charged(b) || got.Result != tc.result || got.Code != tc.code || got.Basis != tc.basis || got.Uncertainty != tc.unc {
					t.Fatalf("usage %+v, want charged %d result %s code %s uncertainty %s", got, tc.charged(b), tc.result, tc.code, tc.unc)
				}
				if slotHeld(t, u, a) != tc.slot {
					t.Fatalf("slot held = %v, want %v", !tc.slot, tc.slot)
				}
				if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged < got.Charged {
					t.Fatalf("the run was charged %d, less than the record's %d", charged, got.Charged)
				}
			})
		}
	})

	t.Run("a failure before any send is free and releases the slot", func(t *testing.T) {
		raw := gatewaytest.Body("gpt-a", "hi", 32)
		for name, tc := range map[string]struct {
			step   gatewaytest.Step
			want   error
			code   string
			result gateway.RequestState
		}{
			"an unapproved destination": {gatewaytest.ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderDestination}), gateway.ErrDestination, "destination_not_approved", gateway.StateFailed},
			"no connection":             {gatewaytest.ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderConnect}), gateway.ErrProvider, "provider_unreachable", gateway.StateFailed},
			"a rejected request":        {gatewaytest.ReplyStatus(400, true), gateway.ErrProvider, "provider_rejected", gateway.StateFailed},
			"a redirect never followed": {gatewaytest.ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderRedirect, Sent: true}), gateway.ErrProvider, "provider_redirect", gateway.StateFailed},
		} {
			t.Run(name, func(t *testing.T) {
				u := configured(t, nil)
				a := u.NewAttempt(gatewaytest.ScopeA)
				u.Provider.Script(tc.step)
				res, err := u.Invoke(a, raw, &gatewaytest.Sink{})
				kind(t, err, tc.want)
				if res.Usage == nil || res.Usage.Charged != 0 || res.Usage.Code != tc.code || res.Usage.Result != tc.result {
					t.Fatalf("usage: %+v", res.Usage)
				}
				if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged != 0 || slotHeld(t, u, a) {
					t.Fatalf("charged %d", charged)
				}
			})
		}

		t.Run("an unusable credential sends nothing", func(t *testing.T) {
			u := configured(t, nil)
			a := u.NewAttempt(gatewaytest.ScopeA)
			u.Credentials.Fail(gatewaytest.ErrInjected)
			res, err := u.Invoke(a, raw, &gatewaytest.Sink{})
			kind(t, err, gateway.ErrCredential)
			if strings.Contains(err.Error(), "CAUSE-SENTINEL") || strings.Contains(gateway.PublicMessage(err), "CAUSE-SENTINEL") {
				t.Fatalf("the cause reached the message: %v", err)
			}
			if n := len(u.Provider.Calls()); n != 0 || res.Usage == nil || res.Usage.Charged != 0 || res.Usage.Code != "credential_unavailable" {
				t.Fatalf("calls %d usage %+v", n, res.Usage)
			}
		})
	})

	t.Run("an interrupted call charges its bound, stops the upstream and holds the slot until an operator settles it", func(t *testing.T) {
		u := configured(t, roomy)
		a := u.NewAttempt(gatewaytest.ScopeA)
		raw := gatewaytest.Body("gpt-a", pad(100), budgetMaxOutput)
		started := make(chan struct{})
		var closes atomic.Int64
		u.Provider.Script(gatewaytest.ReplyHang("gpt-a-2026", started, &closes, "partial"))

		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		sink := &gatewaytest.Sink{}
		type outcome struct {
			res gateway.InvokeResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := u.Svc.Invoke(cctx, gateway.InvokeRequest{Attempt: a.Workload.AttemptID, Bearer: a.Bearer, Body: raw, Sink: sink})
			done <- outcome{res, err}
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the provider stream never started")
		}
		cancel()
		var out outcome
		select {
		case out = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the cancelled call did not return")
		}
		if out.err == nil || out.res.Usage == nil {
			t.Fatalf("cancelled call: %v %+v", out.err, out.res)
		}
		b := bound(t, raw) + budgetMaxOutput
		if got := out.res.Usage; got.Result != gateway.StateUnknown || got.Code != "client_cancelled" || got.Uncertainty != gateway.UncertaintyClientCancelled || got.Charged != b {
			t.Fatalf("usage %+v, want a charge of the bound %d", got, b)
		}
		if closes.Load() != 1 {
			t.Fatalf("the upstream stream was closed %d times", closes.Load())
		}
		if frames, finished, failCode := sink.Snapshot(); len(frames) != 1 || finished || failCode == "" {
			t.Fatalf("the partial stream must fail visibly, never finish: %v %v %q", frames, finished, failCode)
		}
		next := u.NextAttempt(a)
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		_, err := u.Invoke(next, raw, &gatewaytest.Sink{})
		kind(t, err, gateway.ErrBusy)
		if n := len(u.Provider.Calls()); n != 1 {
			t.Fatalf("a refused request reached the provider: %d calls", n)
		}

		id := out.res.RequestID
		kind(t, u.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Viewer}, a.Workload.Scope, id), gateway.ErrDenied)
		kind(t, u.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Stranger}, a.Workload.Scope, id), gateway.ErrHidden)
		if err := u.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, a.Workload.Scope, id); err != nil {
			t.Fatal(err)
		}
		if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged != b {
			t.Fatalf("settlement must not change the charge: %d", charged)
		}
		if _, err := u.Invoke(next, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatalf("after settlement: %v", err)
		}
	})

	t.Run("a deadline or a revoked attempt interrupts a hanging stream and charges the bound", func(t *testing.T) {
		raw := gatewaytest.Body("gpt-a", "hi", 32)
		for name, tc := range map[string]struct {
			timeouts gateway.Timeouts
			code     string
			unc      string
			setup    func(u *gatewaytest.Unit, a gatewaytest.UnitAttempt, started <-chan struct{})
		}{
			"the request deadline": {shortTimeouts(func(to *gateway.Timeouts) { to.Request = 50 * time.Millisecond }), "deadline_exceeded", gateway.UncertaintyDeadlineExceeded,
				func(*gatewaytest.Unit, gatewaytest.UnitAttempt, <-chan struct{}) {}},
			"a revoked attempt": {shortTimeouts(func(to *gateway.Timeouts) { to.Recheck = 10 * time.Millisecond }), "attempt_cancelled", gateway.UncertaintyAttemptCancelled,
				func(u *gatewaytest.Unit, a gatewaytest.UnitAttempt, started <-chan struct{}) {
					<-started
					u.Workloads.Revoke(a.Workload.AttemptID)
				}},
		} {
			t.Run(name, func(t *testing.T) {
				u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{Timeouts: tc.timeouts})
				u.Configure(gatewaytest.ScopeA, nil)
				a := u.NewAttempt(gatewaytest.ScopeA)
				started := make(chan struct{})
				var closes atomic.Int64
				u.Provider.Script(gatewaytest.ReplyHang("gpt-a-2026", started, &closes, "partial"))
				go tc.setup(u, a, started)
				res, err := u.Invoke(a, raw, &gatewaytest.Sink{})
				if err == nil || res.Usage == nil {
					t.Fatalf("%v %+v", err, res)
				}
				if res.Usage.Code != tc.code || res.Usage.Uncertainty != tc.unc || res.Usage.Charged == 0 || res.Usage.Result != gateway.StateUnknown {
					t.Fatalf("usage %+v", res.Usage)
				}
				if closes.Load() != 1 {
					t.Fatalf("the upstream stream was closed %d times", closes.Load())
				}
				if !slotHeld(t, u, a) {
					t.Fatal("an interrupted request keeps the slot")
				}
			})
		}
	})

	t.Run("one request is in flight per run, across attempts", func(t *testing.T) {
		u := configured(t, nil)
		a := u.NewAttempt(gatewaytest.ScopeA)
		started := make(chan struct{})
		u.Provider.Script(gatewaytest.ReplyHang("gpt-a-2026", started, nil, "partial"))
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := u.Svc.Invoke(cctx, gateway.InvokeRequest{Attempt: a.Workload.AttemptID, Bearer: a.Bearer, Body: gatewaytest.Body("gpt-a", "hi", 32), Sink: &gatewaytest.Sink{}})
			done <- err
		}()
		<-started

		var wg sync.WaitGroup
		var busy, other atomic.Int64
		for i := 0; i < 8; i++ {
			peer := u.NextAttempt(a)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := u.Invoke(peer, gatewaytest.Body("gpt-a", "hi", 32), &gatewaytest.Sink{})
				if errors.Is(err, gateway.ErrBusy) {
					busy.Add(1)
				} else {
					other.Add(1)
				}
			}()
		}
		wg.Wait()
		cancel()
		<-done
		if busy.Load() != 8 || other.Load() != 0 {
			t.Fatalf("%d busy, %d other", busy.Load(), other.Load())
		}
		if n := len(u.Provider.Calls()); n != 1 {
			t.Fatalf("%d provider calls while one was in flight", n)
		}
		if admitted, _ := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); admitted != 1 {
			t.Fatalf("refused requests consumed admissions: %d", admitted)
		}
	})

	t.Run("usage is attributed to the verified workload and tenant, and budgets never mix", func(t *testing.T) {
		u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})
		u.Configure(gatewaytest.ScopeA, tight)
		u.Configure(gatewaytest.ScopeB, tight)
		a, b := u.NewAttempt(gatewaytest.ScopeA), u.NewAttempt(gatewaytest.ScopeB)
		const secretPrompt = "PROMPT-SENTINEL-do-not-store"
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 30, 10, "ans"), gatewaytest.Reply("gpt-b-2026", 20, 5, "ans"))
		ra, err := u.Invoke(a, gatewaytest.Body("gpt-a", secretPrompt, 32), &gatewaytest.Sink{})
		if err != nil {
			t.Fatal(err)
		}
		rb, err := u.Invoke(b, gatewaytest.Body("gpt-b", secretPrompt, 32), &gatewaytest.Sink{})
		if err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			res gateway.InvokeResult
			att gatewaytest.UnitAttempt
			in  int64
		}{"tenant A": {ra, a, 30}, "tenant B": {rb, b, 20}} {
			us := tc.res.Usage
			if us.Scope != tc.att.Workload.Scope || us.RunID != tc.att.Workload.RunID || us.AttemptID != tc.att.Workload.AttemptID || *us.InputTokens != tc.in {
				t.Errorf("%s: usage attribution %+v", name, us)
			}
		}
		if us := ra.Usage; us.RequestedModel != "gpt-a" || us.ActualModel != "gpt-a-2026" || !us.ModelConfirmed {
			t.Fatalf("model attribution: %+v", us)
		}
		if _, ca := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); ca != 40 {
			t.Fatalf("A charged %d", ca)
		}
		if _, cb := u.Store.Budget(b.Workload.Scope, b.Workload.RunID); cb != 25 {
			t.Fatalf("B charged %d", cb)
		}
		// Reads are tenant scoped, and management of one project is not another's.
		if _, err := u.Svc.UsageOf(ctx, gateway.Actor{ID: gatewaytest.Admin}, gatewaytest.ScopeB, ra.RequestID); !errors.Is(err, gateway.ErrNotFound) {
			t.Fatalf("usage of A's request read through B: %v", err)
		}
		if us, err := u.Svc.UsageOf(ctx, gateway.Actor{ID: gatewaytest.Admin}, gatewaytest.ScopeA, ra.RequestID); err != nil || us.Charged != 40 {
			t.Fatalf("usage read: %+v %v", us, err)
		}

		// Events and audit rows carry attribution and numbers, never content or credentials.
		for tenant, att := range map[domain.TenantID]gatewaytest.UnitAttempt{gatewaytest.ScopeA.TenantID: a, gatewaytest.ScopeB.TenantID: b} {
			var usage int
			for _, env := range u.Store.Events(tenant) {
				if env.TenantID != tenant {
					t.Fatalf("an event of %s was written to %s's outbox", env.TenantID, tenant)
				}
				if env.Type != eventsv1.TypeUsageRecorded {
					continue
				}
				usage++
				if env.Actor.ID != string(att.Workload.AttemptID) || env.Actor.Type != eventsv1.ActorWorkload || env.ProjectID != att.Workload.Scope.ProjectID {
					t.Errorf("event actor %+v", env.Actor)
				}
				var data eventsv1.UsageRecorded
				if err := json.Unmarshal(env.Data, &data); err != nil || data.RunID != string(att.Workload.RunID) || data.Basis != "observed" {
					t.Errorf("event data %s %v", env.Data, err)
				}
			}
			if usage != 1 {
				t.Errorf("tenant %s has %d usage events", tenant, usage)
			}
			dump := fmt.Sprintf("%+v %+v", u.Store.Events(tenant), u.Store.Audits(tenant))
			for _, secret := range []string{secretPrompt, gatewaytest.UnitBearer, "ans"} {
				if strings.Contains(dump, secret) {
					t.Errorf("tenant %s: %q leaked into events or audit rows", tenant, secret)
				}
			}
		}
		if len(u.Store.Audits(gatewaytest.ScopeA.TenantID)) == 0 {
			t.Fatal("expected audit rows")
		}
	})

	t.Run("a run keeps the exact credential version and policy it was bound to", func(t *testing.T) {
		u := configured(t, nil)
		v2 := gateway.CredentialRef{Ref: gatewaytest.Secret.Ref, Version: 2}
		u.Credentials.Put(v2, "sk-t083-v2")
		early := u.NewAttempt(gatewaytest.ScopeA)
		raw := gatewaytest.Body("gpt-a", "hi", 32)
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"), gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"), gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		if _, err := u.Invoke(early, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if pins := u.Store.Pins(early.Workload.Scope, early.Workload.RunID); len(pins) != 1 || pins[0] != gatewaytest.Secret {
			t.Fatalf("pins: %v", pins)
		}
		// The secret is rotated and the model removed from the project's list.
		u.Configure(gatewaytest.ScopeA, func(e *gateway.SettingsEdit) { e.Credential = v2; e.Models = []string{"gpt-b"} })
		if _, err := u.Invoke(early, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatalf("the bound run keeps its allow-list: %v", err)
		}
		late := u.NewAttempt(gatewaytest.ScopeA)
		_, err := u.Invoke(late, raw, &gatewaytest.Sink{})
		kind(t, err, gateway.ErrModelNotAllowed)
		if _, err := u.Invoke(late, gatewaytest.Body("gpt-b", "hi", 32), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		calls := u.Provider.Calls()
		if len(calls) != 3 || calls[0].Bearer != gatewaytest.UnitBearer || calls[1].Bearer != gatewaytest.UnitBearer || calls[2].Bearer != "sk-t083-v2" {
			t.Fatalf("each run must use its own pinned version: %+v", calls)
		}
		if opened := u.Credentials.Opened(); len(opened) != 3 || opened[0] != gatewaytest.Secret || opened[2] != v2 {
			t.Fatalf("opened %v", opened)
		}
	})

	t.Run("reconciliation charges a lost worker's bound, frees an unsent slot and is idempotent", func(t *testing.T) {
		u := configured(t, tight)
		unsent, sent := u.NewAttempt(gatewaytest.ScopeA), u.NewAttempt(gatewaytest.ScopeA)
		admit := func(a gatewaytest.UnitAttempt) gateway.ModelRequest {
			id, _ := gateway.NewRequestID()
			adm, err := u.Store.Admit(ctx, gateway.AdmitCommand{Workload: a.Workload, RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 30, OutputMax: 20, Now: u.Clock.Now()})
			if err != nil {
				t.Fatal(err)
			}
			return adm.Request
		}
		admit(unsent)
		r := admit(sent)
		if err := u.Store.BeginForwarding(ctx, sent.Workload, r.ID, u.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := u.Svc.Reconcile(ctx, time.Second); !errors.Is(err, gateway.ErrInvalid) {
			t.Fatalf("a quiet period shorter than several heartbeats: %v", err)
		}
		if rep, err := u.Svc.Reconcile(ctx, 5*time.Minute); err != nil || rep != (gateway.ReconcileReport{}) {
			t.Fatalf("nothing is abandoned yet: %+v %v", rep, err)
		}
		u.Clock.Advance(10 * time.Minute)
		rep, err := u.Svc.Reconcile(ctx, 5*time.Minute)
		if err != nil || rep.Unsent != 1 || rep.Unknown != 1 || rep.Failed != 0 {
			t.Fatalf("report: %+v %v", rep, err)
		}
		if _, charged := u.Store.Budget(unsent.Workload.Scope, unsent.Workload.RunID); charged != 0 {
			t.Fatalf("an unsent request is free: %d", charged)
		}
		if _, charged := u.Store.Budget(sent.Workload.Scope, sent.Workload.RunID); charged != 50 {
			t.Fatalf("a lost worker's request is charged its bound: %d", charged)
		}
		if slotHeld(t, u, unsent) || !slotHeld(t, u, sent) {
			t.Fatal("the unsent slot is free and the possibly sent one is held")
		}
		if rep, err := u.Svc.Reconcile(ctx, 5*time.Minute); err != nil || rep != (gateway.ReconcileReport{}) {
			t.Fatalf("a second pass changes nothing: %+v %v", rep, err)
		}
		if got := u.Store.EventsOfType(gatewaytest.ScopeA.TenantID, string(eventsv1.TypeUsageRecorded)); got < 2 {
			t.Fatalf("%d usage events", got)
		}
	})

	t.Run("a lost or failed commit is recovered by rereading, never by calling the provider again", func(t *testing.T) {
		u := configured(t, nil)
		a := u.NewAttempt(gatewaytest.ScopeA)
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 20, 5, "ok"))
		// Commits: admission, begin forwarding, then the finalization is lost after it landed.
		u.Store.LoseNextCommits(2, 1)
		res, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", 32), &gatewaytest.Sink{})
		if err != nil || res.Usage == nil || res.Usage.Charged != 25 {
			t.Fatalf("a lost commit that landed is found again: %+v %v", res.Usage, err)
		}
		if n := len(u.Provider.Calls()); n != 1 {
			t.Fatalf("%d provider calls", n)
		}
		if got := u.Store.EventsOfType(a.Workload.Scope.TenantID, string(eventsv1.TypeUsageRecorded)); got != 1 {
			t.Fatalf("%d usage events", got)
		}
		if _, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); charged != 25 {
			t.Fatalf("charged %d", charged)
		}

		// A finalization that rolled back is retried as a database write only.
		b := u.NewAttempt(gatewaytest.ScopeA)
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 20, 5, "ok"))
		u.Store.FailCommits(2, 1)
		res, err = u.Invoke(b, gatewaytest.Body("gpt-a", "hi", 32), &gatewaytest.Sink{})
		if err != nil || res.Usage == nil || res.Usage.Charged != 25 {
			t.Fatalf("a rolled-back finalization is retried: %+v %v", res.Usage, err)
		}
		if n := len(u.Provider.Calls()); n != 2 {
			t.Fatalf("%d provider calls: the retry must not call the provider again", n)
		}
		if _, charged := u.Store.Budget(b.Workload.Scope, b.Workload.RunID); charged != 25 {
			t.Fatalf("charged %d", charged)
		}
	})

	t.Run("an admission whose commit is unknown sends nothing", func(t *testing.T) {
		u := configured(t, nil)
		a := u.NewAttempt(gatewaytest.ScopeA)
		u.Store.FailNextCommits(1)
		_, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", 32), &gatewaytest.Sink{})
		kind(t, err, gateway.ErrUnavailable)
		if n := len(u.Provider.Calls()); n != 0 {
			t.Fatalf("a request was sent although admission did not commit: %d", n)
		}
		if admitted, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); admitted != 0 || charged != 0 {
			t.Fatalf("a failed admission consumed %d/%d", admitted, charged)
		}
	})

	t.Run("workload identity is verified before any admission", func(t *testing.T) {
		u := configured(t, nil)
		a, other := u.NewAttempt(gatewaytest.ScopeA), u.NewAttempt(gatewaytest.ScopeA)
		raw := gatewaytest.Body("gpt-a", "hi", 32)
		run := func(attempt domain.AttemptID, bearer string) error {
			_, err := u.Svc.Invoke(ctx, gateway.InvokeRequest{Attempt: attempt, Bearer: bearer, Body: raw, Sink: &gatewaytest.Sink{}})
			return err
		}
		kind(t, run(a.Workload.AttemptID, "wrong"), gateway.ErrUnauthenticated)
		kind(t, run(a.Workload.AttemptID, other.Bearer), gateway.ErrUnauthenticated) // another attempt's token
		kind(t, run("bad id!", a.Bearer), gateway.ErrUnauthenticated)
		u.Workloads.Revoke(other.Workload.AttemptID)
		kind(t, run(other.Workload.AttemptID, other.Bearer), gateway.ErrForbidden)
		u.Workloads.Fail(gatewaytest.ErrInjected)
		err := run(a.Workload.AttemptID, a.Bearer)
		kind(t, err, gateway.ErrUnavailable)
		if strings.Contains(gateway.PublicMessage(err), "CAUSE-SENTINEL") {
			t.Fatal("an outage cause reached the message")
		}
		u.Workloads.Fail(nil)
		if n := len(u.Provider.Calls()); n != 0 {
			t.Fatalf("%d provider calls", n)
		}
		for _, id := range []domain.RunID{a.Workload.RunID, other.Workload.RunID} {
			if admitted, _ := u.Store.Budget(gatewaytest.ScopeA, id); admitted != 0 {
				t.Fatalf("a refused identity consumed an admission of %s", id)
			}
		}
	})

	t.Run("settings are authorized and validated before they are stored", func(t *testing.T) {
		u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})
		edit := u.Edit()
		replace := func(actor string, edit gateway.SettingsEdit) error {
			_, err := u.Svc.ReplaceSettings(ctx, gateway.Actor{ID: actor}, gatewaytest.ScopeA, 1, edit, "")
			return err
		}
		kind(t, replace(gatewaytest.Viewer, edit), gateway.ErrDenied)
		kind(t, replace(gatewaytest.Stranger, edit), gateway.ErrHidden)
		unapproved := edit
		unapproved.Endpoint = "https://other.t083.example.invalid/v1"
		kind(t, replace(gatewaytest.Admin, unapproved), gateway.ErrDestination)
		missing := edit
		missing.Credential = gateway.CredentialRef{Ref: "secret://project/absent", Version: 1}
		kind(t, replace(gatewaytest.Admin, missing), gateway.ErrCredential)
		recording := edit
		recording.Recording = true
		kind(t, replace(gatewaytest.Admin, recording), gateway.ErrRecording)
		if got, _ := u.Svc.GetSettings(ctx, gateway.Actor{ID: gatewaytest.Viewer}, gatewaytest.ScopeA); got.Revision != 1 || got.Enabled {
			t.Fatalf("a refused change stored settings: %+v", got)
		}
		if len(u.Store.Revisions(gatewaytest.ScopeA)) != 0 || len(u.Store.Audits(gatewaytest.ScopeA.TenantID)) != 0 {
			t.Fatal("a refused change wrote a revision or an audit row")
		}
		if err := replace(gatewaytest.Admin, edit); err != nil {
			t.Fatal(err)
		}
		if revs := u.Store.Revisions(gatewaytest.ScopeA); len(revs) != 1 || revs[0].Revision != 2 {
			t.Fatalf("revisions: %+v", revs)
		}
	})

	t.Run("only the pinned protocol subset is forwarded and no provider or harness support is claimed (GM-12)", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "chat-completions", "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			ProtocolRevision string `json:"protocolRevision"`
			Qualification    struct {
				Harness  string `json:"harness"`
				Provider string `json:"provider"`
			} `json:"qualification"`
			Rejected []struct {
				File string `json:"file"`
			} `json:"rejected"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.ProtocolRevision != openai.ProtocolRevision {
			t.Fatalf("manifest revision %q != code revision %q", m.ProtocolRevision, openai.ProtocolRevision)
		}
		if !strings.HasPrefix(m.Qualification.Harness, "pending") || m.Qualification.Provider != "none claimed" {
			t.Fatalf("the fixtures must not claim harness or provider support: %+v", m.Qualification)
		}
		if len(m.Rejected) < 10 {
			t.Fatalf("only %d rejected fixtures", len(m.Rejected))
		}
		u := configured(t, nil)
		a := u.NewAttempt(gatewaytest.ScopeA)
		for _, r := range m.Rejected {
			body, err := os.ReadFile(filepath.Join("testdata", "chat-completions", r.File))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := u.Invoke(a, body, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", r.File, err)
			}
		}
		if n := len(u.Provider.Calls()); n != 0 {
			t.Fatalf("%d unsupported requests reached the provider", n)
		}
		if admitted, _ := u.Store.Budget(a.Workload.Scope, a.Workload.RunID); admitted != 0 {
			t.Fatalf("unsupported requests consumed %d admissions", admitted)
		}
		// A supported request still flows, and only its accepted members are sent.
		minimal, err := os.ReadFile(filepath.Join("testdata", "chat-completions", "requests", "minimal.json"))
		if err != nil {
			t.Fatal(err)
		}
		u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 5, 5, "ok"))
		if _, err := u.Invoke(a, minimal, &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if calls := u.Provider.Calls(); len(calls) != 1 || calls[0].Model != "gpt-a" || calls[0].MaxTokens != budgetMaxOutputDefault(u) {
			t.Fatalf("calls: %+v", calls)
		}
	})
}

// budgetMaxOutputDefault is the output limit a request without max_tokens gets:
// the run's own.
func budgetMaxOutputDefault(u *gatewaytest.Unit) int64 { return u.Edit().Limits.MaxOutputTokens }
