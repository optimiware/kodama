package gateway_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

var (
	t0    = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	scope = domain.Scope{TenantID: "t032_acme", ProjectID: "p032_inventory"}
)

func goodEdit() gateway.SettingsEdit {
	return gateway.SettingsEdit{
		Enabled: true, Endpoint: "https://Models.Example.com:443/v1/", Credential: gateway.CredentialRef{Ref: "secret://project/model-key", Version: 1},
		Models: []string{"gpt-b", "gpt-a"},
		Limits: gateway.Limits{MaxRequests: 3, TokenAllowance: 1000, MaxRequestBytes: 1 << 16, MaxInputTokens: 300, MaxOutputTokens: 200},
	}
}

func storedSettings(t *testing.T) gateway.ModelSettings {
	t.Helper()
	s, changed, err := gateway.Disabled(scope).Apply(1, goodEdit(), "u1", t0)
	if err != nil || len(changed) == 0 {
		t.Fatalf("apply: %v %v", changed, err)
	}
	return s
}

func is(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("got %v, want %v", err, kind)
	}
}

func Test_gateway_transitions(t *testing.T) {
	t.Run("settings start disabled at revision one and edit under the expected revision", func(t *testing.T) {
		d := gateway.Disabled(scope)
		if d.Enabled || d.Revision != 1 || d.Stored() {
			t.Fatalf("virtual settings: %+v", d)
		}
		s := storedSettings(t)
		if s.Revision != 2 || !s.Stored() || s.Endpoint.String() != "https://models.example.com/v1" || s.Models[0] != "gpt-a" {
			t.Fatalf("normalization: %+v", s)
		}
		_, _, err := s.Apply(1, goodEdit(), "u1", t0)
		is(t, err, gateway.ErrVersionConflict)
		same, changed, err := s.Apply(2, goodEdit(), "u1", t0.Add(time.Hour))
		if err != nil || len(changed) != 0 || same.Revision != 2 || !same.UpdatedAt.Equal(t0) {
			t.Fatalf("an unchanged edit must not move the revision: %+v %v %v", same, changed, err)
		}
		e := goodEdit()
		e.Recording, e.Models = true, []string{"gpt-a"}
		next, changed, err := s.Apply(2, e, "u2", t0.Add(time.Hour))
		if err != nil || next.Revision != 3 || strings.Join(changed, ",") != "model.models,model.recording" {
			t.Fatalf("changed = %v %v", changed, err)
		}
	})

	t.Run("settings validation", func(t *testing.T) {
		for name, mut := range map[string]func(*gateway.SettingsEdit){
			"http endpoint":      func(e *gateway.SettingsEdit) { e.Endpoint = "http://models.example.com/v1" },
			"credentials in url": func(e *gateway.SettingsEdit) { e.Endpoint = "https://u:p@models.example.com/v1" },
			"query":              func(e *gateway.SettingsEdit) { e.Endpoint = "https://models.example.com/v1?x=1" },
			"fragment":           func(e *gateway.SettingsEdit) { e.Endpoint = "https://models.example.com/v1#x" },
			"dot segment":        func(e *gateway.SettingsEdit) { e.Endpoint = "https://models.example.com/a/../v1" },
			"encoded slash":      func(e *gateway.SettingsEdit) { e.Endpoint = "https://models.example.com/a%2fb" },
			"ipv6":               func(e *gateway.SettingsEdit) { e.Endpoint = "https://[::1]/v1" },
			"bad port":           func(e *gateway.SettingsEdit) { e.Endpoint = "https://models.example.com:99999/v1" },
			"credential scope":   func(e *gateway.SettingsEdit) { e.Credential.Ref = "secret://tenant/model-key" },
			"credential version": func(e *gateway.SettingsEdit) { e.Credential.Version = 0 },
			"no models":          func(e *gateway.SettingsEdit) { e.Models = nil },
			"duplicate model":    func(e *gateway.SettingsEdit) { e.Models = []string{"a", "a"} },
			"bad model":          func(e *gateway.SettingsEdit) { e.Models = []string{"a b"} },
			"too many models": func(e *gateway.SettingsEdit) {
				e.Models = nil
				for i := 0; i <= gateway.MaxModels; i++ {
					e.Models = append(e.Models, fmt.Sprintf("m%d", i))
				}
			},
			"zero requests":   func(e *gateway.SettingsEdit) { e.Limits.MaxRequests = 0 },
			"huge allowance":  func(e *gateway.SettingsEdit) { e.Limits.TokenAllowance = gateway.MaxAllowance + 1 },
			"huge input":      func(e *gateway.SettingsEdit) { e.Limits.MaxInputTokens = gateway.MaxInputCeiling + 1 },
			"negative output": func(e *gateway.SettingsEdit) { e.Limits.MaxOutputTokens = -1 },
		} {
			e := goodEdit()
			mut(&e)
			if _, _, err := gateway.Disabled(scope).Apply(1, e, "u1", t0); !errors.Is(err, gateway.ErrInvalid) {
				t.Errorf("%s: got %v", name, err)
			}
		}
		if _, _, err := gateway.Disabled(scope).Apply(1, goodEdit(), "", t0); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("an editor is required: %v", err)
		}
		ep, err := gateway.ParseEndpoint("https://models.example.com:8443/v1")
		if err != nil || ep.Origin() != "https://models.example.com:8443" || ep.ChatURL() != "https://models.example.com:8443/v1/chat/completions" {
			t.Fatalf("endpoint = %+v %v", ep, err)
		}
	})

	t.Run("a binding is immutable and requires enabled stored settings", func(t *testing.T) {
		if _, err := gateway.BindRun(gateway.Disabled(scope), "run_1", t0); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("virtual settings bind: %v", err)
		}
		s := storedSettings(t)
		b, err := gateway.BindRun(s, "run_1", t0)
		if err != nil || b.SettingsRevision != 2 || !b.Allows("gpt-a") || b.Allows("gpt-z") {
			t.Fatalf("binding = %+v %v", b, err)
		}
		// Later edits never reach an existing binding: the models slice is a copy.
		s.Models[0] = "mutated"
		if !b.Allows("gpt-a") || b.Allows("mutated") {
			t.Fatal("binding shares memory with settings")
		}
		e := goodEdit()
		e.Enabled = false
		off, _, err := storedSettings(t).Apply(2, e, "u1", t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.BindRun(off, "run_1", t0); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("disabled settings bind: %v", err)
		}
	})

	t.Run("budget admits one request at a time within the caps", func(t *testing.T) {
		b := budget(t, gateway.Limits{MaxRequests: 2, TokenAllowance: 100, MaxRequestBytes: 1000, MaxInputTokens: 50, MaxOutputTokens: 30})
		if bound, err := b.Admit("mrq_1", 100, 40, 20); err != nil || bound != 60 || b.Admitted != 1 || b.Unresolved != "mrq_1" {
			t.Fatalf("first admit: %d %v %+v", bound, err, b)
		}
		_, err := b.Admit("mrq_2", 100, 1, 1)
		is(t, err, gateway.ErrBusy)
		if b.Admitted != 1 {
			t.Fatal("a refused admission consumed a request")
		}
		if err := b.Settle("mrq_1", 60, true); err != nil || b.Charged != 60 || b.Unresolved != "" {
			t.Fatalf("settle: %v %+v", err, b)
		}
		if _, err := b.Admit("mrq_2", 100, 1, 1); err != nil {
			t.Fatalf("below the allowance: %v", err)
		}
		if err := b.Settle("mrq_2", 80, true); err != nil {
			t.Fatal(err)
		}
		// request cap is reached before the allowance matters
		_, err = b.Admit("mrq_3", 100, 1, 1)
		is(t, err, gateway.ErrLimitExceeded)
	})

	t.Run("allowance gates admission and bounds the overshoot", func(t *testing.T) {
		l := gateway.Limits{MaxRequests: 10, TokenAllowance: 100, MaxRequestBytes: 1000, MaxInputTokens: 50, MaxOutputTokens: 30}
		b := budget(t, l)
		charged := int64(0)
		for i := 0; ; i++ {
			id := gateway.RequestID(fmt.Sprintf("mrq_%d", i))
			bound, err := b.Admit(id, 100, l.MaxInputTokens, l.MaxOutputTokens)
			if err != nil {
				is(t, err, gateway.ErrLimitExceeded)
				break
			}
			if err := b.Settle(id, bound, true); err != nil {
				t.Fatal(err)
			}
			charged += bound
		}
		if b.Charged < l.TokenAllowance || b.Charged >= l.TokenAllowance+l.Overshoot() || b.Charged != charged {
			t.Fatalf("charged %d, allowance %d, bound %d", b.Charged, l.TokenAllowance, l.Overshoot())
		}
	})

	t.Run("limits and overflow", func(t *testing.T) {
		b := budget(t, gateway.Limits{MaxRequests: 5, TokenAllowance: 100, MaxRequestBytes: 1000, MaxInputTokens: 50, MaxOutputTokens: 30})
		for _, c := range [][2]int64{{51, 10}, {-1, 10}, {10, 31}, {10, 0}} {
			if _, err := b.Admit("mrq_x", 100, c[0], c[1]); !errors.Is(err, gateway.ErrLimitExceeded) {
				t.Errorf("bounds %v: %v", c, err)
			}
		}
		for _, bytes := range []int64{0, -1, 1001} {
			if _, err := b.Admit("mrq_x", bytes, 10, 10); !errors.Is(err, gateway.ErrLimitExceeded) {
				t.Errorf("request bytes %d: %v", bytes, err)
			}
		}
		if b.Admitted != 0 || b.Unresolved != "" {
			t.Fatal("refusals consumed state")
		}
		if _, err := b.Admit("not valid!", 100, 1, 1); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("malformed id: %v", err)
		}
		if _, err := b.Admit("mrq_1", 100, 1, 1); err != nil {
			t.Fatal(err)
		}
		b.Charged = domain.MaxSafeInteger - 1
		if err := b.Settle("mrq_1", math.MaxInt64, true); err != nil || b.Charged != domain.MaxSafeInteger {
			t.Fatalf("charge saturates: %v %d", err, b.Charged)
		}
		is(t, b.Settle("mrq_1", 1, true), gateway.ErrSuperseded)
		is(t, b.Settle("", 1, true), gateway.ErrSuperseded)
	})

	t.Run("request transitions are explicit", func(t *testing.T) {
		r := request(t)
		is(t, r.Beat(t0), nil)
		if err := r.BeginForwarding(t0); err != nil {
			t.Fatal(err)
		}
		is(t, r.BeginForwarding(t0), gateway.ErrInvalidTransition)
		is(t, r.Settle(t0), gateway.ErrInvalidTransition)
		_, err := r.AbandonUnsent(t0)
		is(t, err, gateway.ErrInvalidTransition)
		u, err := r.Finalize(observed(10, 5), t0.Add(time.Second))
		if err != nil || r.State != gateway.StateSucceeded || u.Charged != 15 || u.Basis != gateway.BasisObserved || u.Uncertainty != gateway.UncertaintyNone {
			t.Fatalf("finalize: %+v %v %+v", u, err, r)
		}
		_, err = r.Finalize(observed(10, 5), t0.Add(time.Second))
		is(t, err, gateway.ErrSuperseded)
		_, err = r.MarkUnknown(t0)
		is(t, err, gateway.ErrInvalidTransition)
		is(t, r.Beat(t0), gateway.ErrSuperseded)
	})

	t.Run("a request that never started forwarding was not sent", func(t *testing.T) {
		r := request(t)
		_, err := r.Finalize(observed(1, 1), t0)
		is(t, err, gateway.ErrInvalidTransition)
		u, err := r.AbandonUnsent(t0)
		if err != nil || u.Charged != 0 || u.Uncertainty != gateway.UncertaintyNotSent || r.State != gateway.StateFailed || r.State.HoldsSlot() {
			t.Fatalf("abandon: %+v %v", u, err)
		}
	})

	t.Run("a cancellation before any send ends failed with its reason and no charge", func(t *testing.T) {
		// The persisted lifecycle allows admitted -> failed only; an unsent request
		// that was cancelled keeps its cancellation reason code and not-sent
		// uncertainty, never the cancelled state.
		for _, code := range []string{"client_cancelled", "attempt_cancelled"} {
			r := request(t)
			u, err := r.Finalize(gateway.Outcome{Result: gateway.StateCancelled, Code: code, Settlement: gateway.SettlementNotSent}, t0)
			if err != nil || r.State != gateway.StateFailed || r.Code != code || r.State.HoldsSlot() {
				t.Fatalf("%s: %+v %v", code, r, err)
			}
			if u.Result != gateway.StateFailed || u.Code != code || u.Charged != 0 || u.Uncertainty != gateway.UncertaintyNotSent {
				t.Fatalf("%s: usage %+v", code, u)
			}
		}
	})

	t.Run("accounting by settlement", func(t *testing.T) {
		cases := []struct {
			name    string
			o       gateway.Outcome
			charged int64
			basis   gateway.Basis
			unc     string
			state   gateway.RequestState
		}{
			{"observed", observed(30, 20), 50, gateway.BasisObserved, "none", gateway.StateSucceeded},
			{"missing usage on a completed stream", uncertain(gateway.StateSucceeded, gateway.UncertaintyUsageMissing), 60, gateway.BasisEstimated, "usage_missing", gateway.StateSucceeded},
			{"truncated stream", uncertain(gateway.StateFailed, gateway.UncertaintyStreamInterrupted), 60, gateway.BasisEstimated, "stream_interrupted", gateway.StateUnknown},
			{"client cancel", uncertain(gateway.StateCancelled, gateway.UncertaintyClientCancelled), 60, gateway.BasisEstimated, "client_cancelled", gateway.StateUnknown},
			{"provider rejected", gateway.Outcome{Result: gateway.StateFailed, Code: "provider_rejected", Settlement: gateway.SettlementRejected}, 0, gateway.BasisEstimated, "provider_rejected", gateway.StateFailed},
			{"provider exceeded the bound", observed(30, 31), 61, gateway.BasisEstimated, "bound_exceeded", gateway.StateFailed},
			{"provider exceeded the input bound", observed(41, 1), 60, gateway.BasisEstimated, "bound_exceeded", gateway.StateFailed},
		}
		for _, c := range cases {
			r := request(t)
			_ = r.BeginForwarding(t0)
			c.o.ActualModel = "gpt-a-2026"
			u, err := r.Finalize(c.o, t0)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			// Only an unknown remote completion keeps the slot; every other
			// outcome, including a completed stream without usage, releases it.
			if r.State.HoldsSlot() != (c.state == gateway.StateUnknown) {
				t.Errorf("%s: state %s holds the slot = %v", c.name, r.State, r.State.HoldsSlot())
			}
			if u.Charged != c.charged || u.Basis != c.basis || u.Uncertainty != c.unc || r.State != c.state || u.Result != c.state {
				t.Errorf("%s: %+v state %s", c.name, u, r.State)
			}
			if !u.ModelConfirmed || u.ActualModel != "gpt-a-2026" || u.RequestedModel != "gpt-a" || u.EventModel() != "gpt-a-2026" {
				t.Errorf("%s: model attribution %+v", c.name, u)
			}
		}
		r := request(t)
		_ = r.BeginForwarding(t0)
		u, err := r.Finalize(uncertain(gateway.StateFailed, gateway.UncertaintyProviderError), t0)
		if err != nil || u.ModelConfirmed || u.ActualModel != "" || u.EventModel() != "gpt-a" {
			t.Fatalf("an unconfirmed model must stay distinguishable: %+v %v", u, err)
		}
	})

	t.Run("invalid outcomes are rejected", func(t *testing.T) {
		for name, o := range map[string]gateway.Outcome{
			"no code":                {Result: gateway.StateFailed, Settlement: gateway.SettlementUncertain, Uncertainty: "provider_error"},
			"bad result":             {Result: gateway.StateAdmitted, Code: "x", Settlement: gateway.SettlementUncertain, Uncertainty: "provider_error"},
			"unknown uncertainty":    {Result: gateway.StateFailed, Code: "x", Settlement: gateway.SettlementUncertain, Uncertainty: "whatever"},
			"none is not uncertain":  {Result: gateway.StateFailed, Code: "x", Settlement: gateway.SettlementUncertain, Uncertainty: "none"},
			"observed without usage": {Result: gateway.StateSucceeded, Code: "x", Settlement: gateway.SettlementObserved},
			"observed failed":        {Result: gateway.StateFailed, Code: "x", Settlement: gateway.SettlementObserved, Observed: &gateway.TokenUsage{Input: 1}},
			"negative usage":         {Result: gateway.StateSucceeded, Code: "x", Settlement: gateway.SettlementObserved, Observed: &gateway.TokenUsage{Input: -1}},
			"unsent with a stream":   {Result: gateway.StateFailed, Code: "x", Settlement: gateway.SettlementNotSent, StreamStarted: true},
			"unknown settlement":     {Result: gateway.StateFailed, Code: "x", Settlement: "maybe"},
		} {
			r := request(t)
			_ = r.BeginForwarding(t0)
			if _, err := r.Finalize(o, t0); err == nil {
				t.Errorf("%s: accepted", name)
			}
			if r.State != gateway.StateForwarding {
				t.Errorf("%s: a refused outcome changed the state to %s", name, r.State)
			}
		}
		r := request(t)
		_ = r.BeginForwarding(t0)
		_, err := r.Finalize(gateway.Outcome{Result: gateway.StateFailed, Code: "x", Settlement: gateway.SettlementNotSent, Destination: gateway.Destination{Origin: "https://x", IP: "203.0.113.1"}}, t0)
		is(t, err, gateway.ErrInvalidTransition)
	})

	t.Run("a lost worker leaves the request unknown and the slot held", func(t *testing.T) {
		r := request(t)
		_ = r.BeginForwarding(t0)
		u, err := r.MarkUnknown(t0.Add(time.Minute))
		if err != nil || r.State != gateway.StateUnknown || !r.State.HoldsSlot() || u.Charged != 60 || u.Uncertainty != gateway.UncertaintyWorkerLost || u.Result != gateway.StateUnknown {
			t.Fatalf("unknown: %+v %v", u, err)
		}
		_, err = r.Finalize(observed(1, 1), t0)
		is(t, err, gateway.ErrSuperseded)
		if err := r.Settle(t0); err != nil || r.State != gateway.StateSettled || r.State.HoldsSlot() {
			t.Fatalf("settle: %v %s", err, r.State)
		}
		is(t, r.Settle(t0), gateway.ErrInvalidTransition)

		b := budget(t, gateway.Limits{MaxRequests: 5, TokenAllowance: 1000, MaxRequestBytes: 1000, MaxInputTokens: 50, MaxOutputTokens: 30})
		_, _ = b.Admit("mrq_1", 100, 40, 20)
		_ = b.Settle("mrq_1", 60, false)
		_, err = b.Admit("mrq_2", 100, 1, 1)
		is(t, err, gateway.ErrBusy)
		if err := b.Release("mrq_1"); err != nil || b.Charged != 60 {
			t.Fatalf("release keeps the charge: %v %d", err, b.Charged)
		}
		is(t, b.Release("mrq_1"), gateway.ErrSuperseded)
	})

	t.Run("credentials never print", func(t *testing.T) {
		c := gateway.NewCredential([]byte("sk-sentinel-secret"))
		for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
			if strings.Contains(s, "sentinel") {
				t.Fatalf("credential leaked: %s", s)
			}
		}
		c.Destroy()
		if len(c.Reveal()) != 0 {
			t.Fatal("destroy must clear the bytes")
		}
	})

	t.Run("classification fails closed", func(t *testing.T) {
		if err := gateway.Classify(errors.New("driver: password=hunter2")); !errors.Is(err, gateway.ErrUnavailable) || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("classify: %v", err)
		}
		if err := gateway.Classify(gateway.Failf(gateway.ErrForbidden, "no")); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("classify known: %v", err)
		}
	})
}

func budget(t *testing.T, l gateway.Limits) *gateway.RunBudget {
	t.Helper()
	e := goodEdit()
	e.Limits = l
	s, _, err := gateway.Disabled(scope).Apply(1, e, "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := gateway.BindRun(s, "run_1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return &gateway.RunBudget{Binding: b}
}

var worker = gateway.Workload{Scope: scope, RunID: "run_1", AttemptID: "att_1", Session: "ses_x"}

func request(t *testing.T) gateway.ModelRequest {
	t.Helper()
	r, err := gateway.NewModelRequest("mrq_1", worker, 1, "gpt-a", 40, 20, false, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func observed(in, out int64) gateway.Outcome {
	return gateway.Outcome{Result: gateway.StateSucceeded, Code: "completed", Settlement: gateway.SettlementObserved, Observed: &gateway.TokenUsage{Input: in, Output: out}}
}

func uncertain(result gateway.RequestState, code string) gateway.Outcome {
	return gateway.Outcome{Result: result, Code: code, Settlement: gateway.SettlementUncertain, Uncertainty: code}
}
