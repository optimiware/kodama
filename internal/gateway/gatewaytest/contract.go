package gatewaytest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

// Scopes are the three projects the contract addresses: two projects of one
// tenant and a project of another.
type Scopes struct{ A, A2, B domain.Scope }

// Harness is what the contract needs from one adapter under test: the store
// itself and the minimum of fixtures that make workloads and actors real for
// that adapter.
type Harness struct {
	Store  gateway.Store
	Scopes Scopes
	// Admin holds project:settings in every scope; Viewer is known but lacks it;
	// Stranger is known nowhere.
	Admin, Viewer, Stranger gateway.Actor
	// NewWorkload returns the identity of a fresh attempt of a fresh run in the
	// scope, currently authorized for model.invoke.
	NewWorkload func(domain.Scope) gateway.Workload
	// Revoke ends the workload's authority.
	Revoke func(gateway.Workload)
	// Events and Audits count a tenant's outbox events of a type and audit rows
	// of an action.
	Events func(tenant domain.TenantID, eventType string) int
	Audits func(tenant domain.TenantID, action string) int
	// Epoch is the instant the suite treats as "now": the clock the adapter's
	// workload authority runs on (zero selects UnitEpoch). It is truncated to a
	// second by the suite's own arithmetic, so a database adapter should pass a
	// whole-second instant.
	Epoch time.Time
	// FailEvents, when set, makes every event append fail with the error (nil
	// restores it). It lets the contract prove all-or-nothing writes; an adapter
	// without such a fault leaves it nil and the case is skipped.
	FailEvents func(error)
}

// Contract is the one exported suite of the gateway.Store port (SD-13, SD-A11).
// The in-memory store and the PostgreSQL store both run it. It asserts
// settings revisions, atomic admission with the run's single slot, the request
// cap and token allowance, charge accounting by outcome, exactly-once
// finalization, reconciliation, operator settlement, tenant and project
// isolation, workload authority and all-or-nothing writes. It does not assert
// SQL, row level security, locking, lock order or isolation levels.
//
// open returns a fresh, empty store (and the fixtures around it) for the test
// it is given.
func Contract(t *testing.T, open func(testing.TB) Harness) {
	t.Helper()
	ctx := context.Background()
	t0 := UnitEpoch
	// Each test starts from its own adapter and clock; the suite's times follow it.
	opened := open
	open = func(tb testing.TB) Harness {
		h := opened(tb)
		t0 = UnitEpoch
		if !h.Epoch.IsZero() {
			t0 = h.Epoch
		}
		return h
	}

	edit := func(mut func(*gateway.SettingsEdit)) gateway.SettingsEdit {
		e := gateway.SettingsEdit{
			Enabled: true, Endpoint: DefaultEndpoint, Credential: Secret, Models: []string{"gpt-a", "gpt-b"},
			Limits: gateway.Limits{MaxRequests: 5, TokenAllowance: 1000, MaxRequestBytes: 4096, MaxInputTokens: 300, MaxOutputTokens: 100},
		}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	configure := func(tb testing.TB, h Harness, scope domain.Scope, mut func(*gateway.SettingsEdit)) gateway.ModelSettings {
		tb.Helper()
		cur, err := h.Store.Settings(ctx, scope)
		if err != nil {
			tb.Fatal(err)
		}
		res, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: scope, Expected: cur.Revision, Edit: edit(mut), Now: t0})
		if err != nil {
			tb.Fatalf("configure: %v", err)
		}
		return res.Settings
	}
	admit := func(h Harness, w gateway.Workload, model string, in, out int64, now time.Time) (gateway.Admission, error) {
		id, err := gateway.NewRequestID()
		if err != nil {
			panic(err)
		}
		return h.Store.Admit(ctx, gateway.AdmitCommand{
			Workload: w, RequestID: id, Model: model, RequestBytes: 100, InputBound: in, OutputMax: out, Now: now, RecordingAvailable: true,
		})
	}
	mustAdmit := func(tb testing.TB, h Harness, w gateway.Workload, now time.Time) gateway.Admission {
		tb.Helper()
		a, err := admit(h, w, "gpt-a", 50, 20, now)
		if err != nil {
			tb.Fatalf("admit: %v", err)
		}
		return a
	}
	finalize := func(h Harness, w gateway.Workload, id gateway.RequestID, o gateway.Outcome, now time.Time) (gateway.Usage, error) {
		return h.Store.Finalize(ctx, gateway.FinalizeCommand{Scope: w.Scope, RunID: w.RunID, AttemptID: w.AttemptID, RequestID: id, Outcome: o, Now: now})
	}
	dest := gateway.Destination{Origin: "https://models.t083.example.invalid", IP: "203.0.113.7"}
	observed := func(in, out int64) gateway.Outcome {
		return gateway.Outcome{Result: gateway.StateSucceeded, Code: "completed", Settlement: gateway.SettlementObserved, StreamStarted: true,
			Observed: &gateway.TokenUsage{Input: in, Output: out}, Destination: dest}
	}
	interrupted := gateway.Outcome{Result: gateway.StateFailed, Code: "stream_truncated", Settlement: gateway.SettlementUncertain,
		Uncertainty: gateway.UncertaintyStreamInterrupted, StreamStarted: true, Destination: dest}
	unsent := func(code string) gateway.Outcome {
		return gateway.Outcome{Result: gateway.StateCancelled, Code: code, Settlement: gateway.SettlementNotSent, Destination: gateway.Destination{Origin: dest.Origin}}
	}
	begin := func(tb testing.TB, h Harness, w gateway.Workload, id gateway.RequestID, now time.Time) {
		tb.Helper()
		if err := h.Store.BeginForwarding(ctx, w, id, now); err != nil {
			tb.Fatalf("begin forwarding: %v", err)
		}
	}
	state := func(tb testing.TB, h Harness, w gateway.Workload, id gateway.RequestID) gateway.RequestState {
		tb.Helper()
		m, err := h.Store.Request(ctx, w.Scope, id)
		if err != nil {
			tb.Fatalf("request: %v", err)
		}
		return m.State
	}
	is := func(tb testing.TB, err, want error) {
		tb.Helper()
		if !errors.Is(err, want) {
			tb.Fatalf("got %v, want %v", err, want)
		}
	}

	t.Run("settings start disabled and change by revision with an audit row and an event", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		cur, err := h.Store.Settings(ctx, sc.A)
		if err != nil || cur.Revision != 1 || cur.Enabled || cur.Stored() {
			t.Fatalf("a project without settings is the virtual disabled revision one: %+v %v", cur, err)
		}
		res, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 1, Edit: edit(nil), Now: t0})
		if err != nil || !res.Changed || res.Settings.Revision != 2 || !res.Settings.Enabled || res.Settings.UpdatedBy != h.Admin.ID {
			t.Fatalf("first write: %+v %v", res, err)
		}
		if got, _ := h.Store.Settings(ctx, sc.A); got.Revision != 2 || got.Credential != Secret || got.Endpoint.Origin() != "https://models.t083.example.invalid" {
			t.Fatalf("stored settings: %+v", got)
		}
		if h.Audits(sc.A.TenantID, "model_settings.update") != 1 || h.Events(sc.A.TenantID, string(eventsv1.TypeProjectSettingsChanged)) != 1 {
			t.Fatal("a change writes one audit row and one event with it")
		}

		_, err = h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 1, Edit: edit(nil), Now: t0})
		is(t, err, gateway.ErrVersionConflict)
		same, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 2, Edit: edit(nil), Now: t0.Add(time.Hour)})
		if err != nil || same.Changed || same.Settings.Revision != 2 {
			t.Fatalf("an unchanged replacement changes nothing: %+v %v", same, err)
		}
		if h.Audits(sc.A.TenantID, "model_settings.update") != 1 || h.Events(sc.A.TenantID, string(eventsv1.TypeProjectSettingsChanged)) != 1 {
			t.Fatal("a refused or unchanged write must leave no audit row or event")
		}
		next, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 2, Edit: edit(func(e *gateway.SettingsEdit) { e.Models = []string{"gpt-b"} }), Now: t0.Add(time.Hour)})
		if err != nil || next.Settings.Revision != 3 || next.Settings.Allows("gpt-a") {
			t.Fatalf("a changed replacement is the next revision: %+v %v", next, err)
		}

		for _, other := range []domain.Scope{sc.A2, sc.B} {
			if got, _ := h.Store.Settings(ctx, other); got.Revision != 1 || got.Enabled {
				t.Fatalf("settings of %v leaked from another project: %+v", other, got)
			}
		}
		if _, err := h.Store.Settings(ctx, domain.Scope{}); !errors.Is(err, gateway.ErrInvalid) {
			t.Fatalf("a malformed scope: %v", err)
		}
	})

	t.Run("settings need the permission and are refused without effect", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		for name, tc := range map[string]struct {
			actor gateway.Actor
			want  error
		}{"a viewer": {h.Viewer, gateway.ErrDenied}, "a stranger": {h.Stranger, gateway.ErrHidden}} {
			_, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: tc.actor, Scope: sc.A, Expected: 1, Edit: edit(nil), Now: t0})
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		if got, _ := h.Store.Settings(ctx, sc.A); got.Revision != 1 || h.Audits(sc.A.TenantID, "model_settings.update") != 0 ||
			h.Events(sc.A.TenantID, string(eventsv1.TypeProjectSettingsChanged)) != 0 {
			t.Fatal("a refused actor must change nothing")
		}
		// Invalid replacements are refused before any write.
		_, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 1, Edit: edit(func(e *gateway.SettingsEdit) { e.Endpoint = "http://insecure.example.invalid" }), Now: t0})
		is(t, err, gateway.ErrInvalid)
		if got, _ := h.Store.Settings(ctx, sc.A); got.Revision != 1 {
			t.Fatal("an invalid replacement wrote settings")
		}
	})

	t.Run("racing first writers: exactly one wins and the other conflicts", func(t *testing.T) {
		h := open(t)
		const racers = 6
		errs := make(chan error, racers)
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: h.Scopes.A2, Expected: 1, Edit: edit(nil), Now: t0})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		won, conflicts := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, gateway.ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		if won != 1 || conflicts != racers-1 {
			t.Fatalf("%d winners and %d conflicts", won, conflicts)
		}
		if h.Audits(h.Scopes.A2.TenantID, "model_settings.update") != 1 {
			t.Fatal("exactly one audit row")
		}
	})

	t.Run("admission needs enabled settings and checks the model and every bound before consuming anything", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		w := h.NewWorkload(sc.A)
		_, err := admit(h, w, "gpt-a", 50, 20, t0)
		is(t, err, gateway.ErrNotConfigured)

		configure(t, h, sc.A, nil)
		for name, tc := range map[string]struct {
			model   string
			in, out int64
			bytes   int64
			want    error
		}{
			"a model off the allow-list": {"gpt-z", 50, 20, 100, gateway.ErrModelNotAllowed},
			"input over the bound":       {"gpt-a", 301, 20, 100, gateway.ErrLimitExceeded},
			"output over the bound":      {"gpt-a", 50, 101, 100, gateway.ErrLimitExceeded},
			"a body over the size limit": {"gpt-a", 50, 20, 4097, gateway.ErrLimitExceeded},
			"an empty body":              {"gpt-a", 50, 20, 0, gateway.ErrLimitExceeded},
		} {
			id, _ := gateway.NewRequestID()
			_, err := h.Store.Admit(ctx, gateway.AdmitCommand{Workload: w, RequestID: id, Model: tc.model, RequestBytes: tc.bytes, InputBound: tc.in, OutputMax: tc.out, Now: t0})
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		// Nothing was consumed by the refusals: this is the run's first request.
		a := mustAdmit(t, h, w, t0)
		if a.Request.Ordinal != 1 || a.Request.State != gateway.StateAdmitted || a.Request.ChargeBound != 70 ||
			a.Request.Scope != w.Scope || a.Request.RunID != w.RunID || a.Request.AttemptID != w.AttemptID {
			t.Fatalf("admission: %+v", a.Request)
		}
		if a.Binding.SettingsRevision != 2 || a.Binding.Credential != Secret || a.Binding.Limits.MaxRequests != 5 {
			t.Fatalf("binding: %+v", a.Binding)
		}
		// An output limit of zero selects the run's own limit.
		id2, _ := gateway.NewRequestID()
		if _, err := finalize(h, w, a.Request.ID, unsent("client_cancelled"), t0); err != nil {
			t.Fatal(err)
		}
		d, err := h.Store.Admit(ctx, gateway.AdmitCommand{Workload: w, RequestID: id2, Model: "gpt-b", RequestBytes: 100, InputBound: 10, OutputMax: 0, Now: t0})
		if err != nil || d.Request.OutputMax != 100 {
			t.Fatalf("the default output limit: %+v %v", d.Request, err)
		}
	})

	t.Run("one request is unresolved per run and a success releases the slot exactly once", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		if _, err := admit(h, w, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("a second request while one is unresolved: %v", err)
		}
		// Another run is independent.
		other := h.NewWorkload(sc.A)
		mustAdmit(t, h, other, t0)

		begin(t, h, w, a.Request.ID, t0)
		if err := h.Store.BeginForwarding(ctx, w, a.Request.ID, t0); !errors.Is(err, gateway.ErrInvalidTransition) {
			t.Fatalf("forwarding twice: %v", err)
		}
		u, err := finalize(h, w, a.Request.ID, observed(30, 10), t0.Add(time.Second))
		if err != nil || u.Charged != 40 || u.Basis != gateway.BasisObserved || u.Uncertainty != gateway.UncertaintyNone || u.Result != gateway.StateSucceeded {
			t.Fatalf("finalize: %+v %v", u, err)
		}
		if *u.InputTokens != 30 || *u.OutputTokens != 10 || u.RunID != w.RunID || u.AttemptID != w.AttemptID || u.Scope != w.Scope {
			t.Fatalf("usage attribution: %+v", u)
		}
		got, err := h.Store.Usage(ctx, w.Scope, a.Request.ID)
		if err != nil || got.Charged != 40 || got.RequestID != a.Request.ID {
			t.Fatalf("stored usage: %+v %v", got, err)
		}
		if h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 1 || h.Audits(sc.A.TenantID, "model.request.finalized") != 1 {
			t.Fatal("one usage event and one audit row")
		}
		if state(t, h, w, a.Request.ID) != gateway.StateSucceeded {
			t.Fatal("the request must be succeeded")
		}

		// Accounting is exactly once: a replay changes nothing.
		_, err = finalize(h, w, a.Request.ID, observed(1, 1), t0.Add(2*time.Second))
		is(t, err, gateway.ErrSuperseded)
		if again, _ := h.Store.Usage(ctx, w.Scope, a.Request.ID); again.Charged != 40 {
			t.Fatalf("a duplicate finalization rewrote usage: %+v", again)
		}
		if h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 1 || h.Audits(sc.A.TenantID, "model.request.finalized") != 1 {
			t.Fatal("a duplicate finalization must not add events or audit rows")
		}
		next := mustAdmit(t, h, w, t0.Add(3*time.Second))
		if next.Request.Ordinal != 2 {
			t.Fatalf("ordinal %d", next.Request.Ordinal)
		}
	})

	t.Run("the request cap and the token allowance stop admission", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, func(e *gateway.SettingsEdit) {
			e.Limits = gateway.Limits{MaxRequests: 2, TokenAllowance: 100, MaxRequestBytes: 4096, MaxInputTokens: 300, MaxOutputTokens: 100}
		})
		w := h.NewWorkload(sc.A)
		for i := 0; i < 2; i++ {
			a := mustAdmit(t, h, w, t0)
			if _, err := finalize(h, w, a.Request.ID, unsent("client_cancelled"), t0); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admit(h, w, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrLimitExceeded) {
			t.Fatalf("the request cap counts admissions, whatever their outcome: %v", err)
		}

		w2 := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w2, t0)
		begin(t, h, w2, a.Request.ID, t0)
		if u, err := finalize(h, w2, a.Request.ID, observed(60, 50), t0); err != nil || u.Charged != 110 {
			t.Fatalf("observed usage is charged exactly: %+v %v", u, err)
		}
		if _, err := admit(h, w2, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrLimitExceeded) {
			t.Fatalf("a spent allowance stops new admission: %v", err)
		}
		// Another run of the same project has its own budget.
		mustAdmit(t, h, h.NewWorkload(sc.A), t0)
	})

	t.Run("charges follow what the gateway knows of the remote effect", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		for name, tc := range map[string]struct {
			outcome gateway.Outcome
			forward bool
			charged int64
			result  gateway.RequestState
			slot    bool
		}{
			"unsent cancellation": {unsent("attempt_cancelled"), false, 0, gateway.StateFailed, false},
			"provider rejection": {gateway.Outcome{Result: gateway.StateFailed, Code: "provider_rejected", Settlement: gateway.SettlementRejected, Destination: dest},
				true, 0, gateway.StateFailed, false},
			"complete stream without usage": {gateway.Outcome{Result: gateway.StateSucceeded, Code: "usage_missing", Settlement: gateway.SettlementUncertain,
				Uncertainty: gateway.UncertaintyUsageMissing, StreamStarted: true, Destination: dest}, true, 70, gateway.StateSucceeded, false},
			"invalid usage": {gateway.Outcome{Result: gateway.StateSucceeded, Code: "usage_invalid", Settlement: gateway.SettlementUncertain,
				Uncertainty: gateway.UncertaintyUsageInvalid, StreamStarted: true, Destination: dest}, true, 70, gateway.StateSucceeded, false},
			"truncated stream":   {interrupted, true, 70, gateway.StateUnknown, true},
			"usage over a bound": {observed(500, 10), true, 510, gateway.StateFailed, false},
		} {
			w := h.NewWorkload(sc.A)
			a := mustAdmit(t, h, w, t0)
			if tc.forward {
				begin(t, h, w, a.Request.ID, t0)
			}
			u, err := finalize(h, w, a.Request.ID, tc.outcome, t0)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if u.Charged != tc.charged || u.Result != tc.result {
				t.Errorf("%s: charged %d result %s, want %d %s", name, u.Charged, u.Result, tc.charged, tc.result)
			}
			_, err = admit(h, w, "gpt-a", 50, 20, t0)
			if tc.slot != errors.Is(err, gateway.ErrBusy) {
				t.Errorf("%s: slot held = %v, next admission: %v", name, tc.slot, err)
			}
		}
		// Finalization that was never begun cannot claim a sent outcome.
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		if _, err := finalize(h, w, a.Request.ID, observed(10, 10), t0); !errors.Is(err, gateway.ErrInvalidTransition) {
			t.Fatalf("a request that never started forwarding was not sent: %v", err)
		}
		if state(t, h, w, a.Request.ID) != gateway.StateAdmitted {
			t.Fatal("a refused finalization must not move the request")
		}
	})

	t.Run("an unknown request keeps its slot until an authorized operator settles it", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		begin(t, h, w, a.Request.ID, t0)
		u, err := finalize(h, w, a.Request.ID, interrupted, t0)
		if err != nil || u.Result != gateway.StateUnknown || u.Charged != 70 || u.Uncertainty != gateway.UncertaintyStreamInterrupted {
			t.Fatalf("interrupted stream: %+v %v", u, err)
		}
		if _, err := admit(h, w, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("an unknown request holds the slot: %v", err)
		}
		settle := func(actor gateway.Actor, sc domain.Scope) error {
			return h.Store.Settle(ctx, gateway.SettleCommand{Actor: actor, Scope: sc, RequestID: a.Request.ID, Now: t0.Add(time.Minute)})
		}
		is(t, settle(h.Viewer, sc.A), gateway.ErrDenied)
		is(t, settle(h.Stranger, sc.A), gateway.ErrHidden)
		if state(t, h, w, a.Request.ID) != gateway.StateUnknown || h.Audits(sc.A.TenantID, "model.request.settled") != 0 {
			t.Fatal("a refused settlement changes nothing")
		}
		// Management in another project is not management here.
		is(t, settle(h.Admin, sc.B), gateway.ErrNotFound)

		if err := settle(h.Admin, sc.A); err != nil {
			t.Fatal(err)
		}
		if state(t, h, w, a.Request.ID) != gateway.StateSettled || h.Audits(sc.A.TenantID, "model.request.settled") != 1 {
			t.Fatal("settled with one attributable audit row")
		}
		var settled gateway.Usage
		if settled, err = h.Store.Usage(ctx, w.Scope, a.Request.ID); err != nil || settled.Charged != 70 {
			t.Fatalf("the charge stays: %+v %v", settled, err)
		}
		if h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 1 {
			t.Fatal("settlement must not emit a second usage event")
		}
		is(t, settle(h.Admin, sc.A), gateway.ErrInvalidTransition)
		mustAdmit(t, h, w, t0.Add(2*time.Minute))
	})

	t.Run("reconciliation settles abandoned requests once and never reopens a possibly sent one", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)

		unsentW := h.NewWorkload(sc.A)
		unsentReq := mustAdmit(t, h, unsentW, t0).Request
		sentW := h.NewWorkload(sc.A)
		sentReq := mustAdmit(t, h, sentW, t0).Request
		begin(t, h, sentW, sentReq.ID, t0)
		liveW := h.NewWorkload(sc.A)
		liveReq := mustAdmit(t, h, liveW, t0).Request
		begin(t, h, liveW, liveReq.ID, t0)
		if err := h.Store.Heartbeat(ctx, liveW, liveReq.ID, t0.Add(50*time.Second)); err != nil {
			t.Fatal(err)
		}

		before := t0.Add(time.Minute)
		list, err := h.Store.Abandoned(ctx, sc.A.TenantID, before, 10)
		if err != nil || len(list) != 3 {
			t.Fatalf("abandoned: %d %v", len(list), err)
		}
		if got, _ := h.Store.Abandoned(ctx, sc.A.TenantID, t0, 10); len(got) != 0 {
			t.Fatal("a request that beat the cutoff is not abandoned")
		}
		if got, _ := h.Store.Abandoned(ctx, sc.B.TenantID, before, 10); len(got) != 0 {
			t.Fatal("another tenant lists nothing of this one")
		}
		if _, err := h.Store.Abandoned(ctx, sc.A.TenantID, before, 0); !errors.Is(err, gateway.ErrInvalid) {
			t.Fatalf("limit: %v", err)
		}
		// The live worker's heartbeat is newer than a stricter cutoff.
		if _, err := h.Store.Abandon(ctx, sc.A, liveReq.ID, t0.Add(30*time.Second), t0.Add(time.Minute)); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("a request with a recent heartbeat is not abandoned: %v", err)
		}

		u, err := h.Store.Abandon(ctx, sc.A, unsentReq.ID, before, before)
		if err != nil || u.Result != gateway.StateFailed || u.Charged != 0 || u.Uncertainty != gateway.UncertaintyNotSent {
			t.Fatalf("an unsent request is free: %+v %v", u, err)
		}
		mustAdmit(t, h, unsentW, before) // its slot was released

		u, err = h.Store.Abandon(ctx, sc.A, sentReq.ID, before, before)
		if err != nil || u.Result != gateway.StateUnknown || u.Charged != 70 || u.Uncertainty != gateway.UncertaintyWorkerLost {
			t.Fatalf("a possibly sent request is charged its bound: %+v %v", u, err)
		}
		if _, err := admit(h, sentW, "gpt-a", 50, 20, before); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("lease expiry alone never reopens the slot: %v", err)
		}
		if _, err := h.Store.Abandon(ctx, sc.A, sentReq.ID, before, before); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("a second reconciliation: %v", err)
		}
		// The late worker can neither finalize nor beat.
		if _, err := finalize(h, sentW, sentReq.ID, observed(30, 10), before); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("late finalize: %v", err)
		}
		if err := h.Store.Heartbeat(ctx, sentW, sentReq.ID, before); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("late heartbeat: %v", err)
		}
		if got, _ := h.Store.Usage(ctx, sentW.Scope, sentReq.ID); got.Charged != 70 || got.Code != "worker_lost" {
			t.Fatalf("the reconciled record stands: %+v", got)
		}
	})

	t.Run("a run keeps its binding and settings changes reach later runs", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		early := h.NewWorkload(sc.A)
		first := mustAdmit(t, h, early, t0)
		if _, err := finalize(h, early, first.Request.ID, unsent("client_cancelled"), t0); err != nil {
			t.Fatal(err)
		}

		configure(t, h, sc.A, func(e *gateway.SettingsEdit) {
			e.Models = []string{"gpt-b"}
			e.Credential = gateway.CredentialRef{Ref: "secret://project/model-key", Version: 2}
			e.Limits.MaxRequests = 1
		})
		next, err := admit(h, early, "gpt-a", 50, 20, t0)
		if err != nil {
			t.Fatalf("the bound run keeps its allow-list: %v", err)
		}
		if next.Binding.SettingsRevision != 2 || next.Binding.Credential != Secret || next.Binding.Limits.MaxRequests != 5 {
			t.Fatalf("the binding is immutable: %+v", next.Binding)
		}
		late := h.NewWorkload(sc.A)
		if _, err := admit(h, late, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrModelNotAllowed) {
			t.Fatalf("a later run follows the new settings: %v", err)
		}
		got := mustAdmitModel(t, h, late, "gpt-b", t0, admit)
		if got.Binding.SettingsRevision != 3 || got.Binding.Credential.Version != 2 {
			t.Fatalf("a later run binds the later revision: %+v", got.Binding)
		}

		// Turning model access off stops even the runs bound earlier.
		configure(t, h, sc.A, func(e *gateway.SettingsEdit) { e.Enabled = false })
		if _, err := finalize(h, early, next.Request.ID, unsent("client_cancelled"), t0); err != nil {
			t.Fatal(err)
		}
		if _, err := admit(h, early, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("a disabled project admits nothing: %v", err)
		}
		if _, err := admit(h, h.NewWorkload(sc.A), "gpt-b", 50, 20, t0); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("a disabled project binds no new run: %v", err)
		}
	})

	t.Run("requests, usage and settlement are scoped to their project, run and attempt", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		configure(t, h, sc.B, nil)
		wA, wB := h.NewWorkload(sc.A), h.NewWorkload(sc.B)
		a := mustAdmit(t, h, wA, t0)
		b := mustAdmit(t, h, wB, t0)
		begin(t, h, wA, a.Request.ID, t0)
		if _, err := finalize(h, wA, a.Request.ID, observed(30, 10), t0); err != nil {
			t.Fatal(err)
		}

		for _, foreign := range []domain.Scope{sc.A2, sc.B} {
			if _, err := h.Store.Request(ctx, foreign, a.Request.ID); !errors.Is(err, gateway.ErrNotFound) {
				t.Errorf("request read from %v: %v", foreign, err)
			}
			if _, err := h.Store.Usage(ctx, foreign, a.Request.ID); !errors.Is(err, gateway.ErrNotFound) {
				t.Errorf("usage read from %v: %v", foreign, err)
			}
		}
		// A finalization must name the request's own project, run and attempt.
		o := unsent("client_cancelled")
		if _, err := h.Store.Finalize(ctx, gateway.FinalizeCommand{Scope: sc.B, RunID: wA.RunID, AttemptID: wA.AttemptID, RequestID: b.Request.ID, Outcome: o, Now: t0}); !errors.Is(err, gateway.ErrNotFound) {
			t.Errorf("another run's identity: %v", err)
		}
		sibling := h.NewWorkload(sc.B)
		if _, err := h.Store.Finalize(ctx, gateway.FinalizeCommand{Scope: sc.B, RunID: wB.RunID, AttemptID: sibling.AttemptID, RequestID: b.Request.ID, Outcome: o, Now: t0}); !errors.Is(err, gateway.ErrNotFound) {
			t.Errorf("another attempt's identity: %v", err)
		}
		if _, err := h.Store.Finalize(ctx, gateway.FinalizeCommand{Scope: sc.B, RunID: wB.RunID, AttemptID: wB.AttemptID, RequestID: "mrq_unknown", Outcome: o, Now: t0}); !errors.Is(err, gateway.ErrNotFound) {
			t.Errorf("an unknown request: %v", err)
		}
		if _, err := h.Store.Finalize(ctx, gateway.FinalizeCommand{Outcome: o}); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("a malformed finalization: %v", err)
		}
		if state(t, h, wB, b.Request.ID) != gateway.StateAdmitted {
			t.Fatal("refused finalizations must not move the request")
		}
		// A forged identity (another project's request through this attempt) is no authority.
		forged := wA
		forged.Scope = sc.B
		id, _ := gateway.NewRequestID()
		if _, err := h.Store.Admit(ctx, gateway.AdmitCommand{Workload: forged, RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 5, OutputMax: 5, Now: t0}); err == nil {
			t.Fatal("a workload cannot admit in a project that does not hold its attempt")
		}
		// A workload cannot touch a request of its own project that is not its own.
		if err := h.Store.BeginForwarding(ctx, wB, a.Request.ID, t0); err == nil {
			t.Fatal("a workload began forwarding another project's request")
		}
	})

	t.Run("workload authority is rechecked for new work but never blocks recording a cost", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		h.Revoke(w)
		if err := h.Store.BeginForwarding(ctx, w, a.Request.ID, t0); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("begin after revocation: %v", err)
		}
		if err := h.Store.Heartbeat(ctx, w, a.Request.ID, t0); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("heartbeat after revocation: %v", err)
		}
		if u, err := finalize(h, w, a.Request.ID, unsent("attempt_cancelled"), t0); err != nil || u.Charged != 0 || u.Result != gateway.StateFailed {
			t.Fatalf("a cancelled attempt still records what its request cost: %+v %v", u, err)
		}
		if _, err := admit(h, w, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("admission after revocation: %v", err)
		}
		fresh := h.NewWorkload(sc.A)
		mustAdmit(t, h, fresh, t0)
	})

	t.Run("content recording is effective only when the binding, the settings and a recorder agree", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, func(e *gateway.SettingsEdit) { e.Recording = true })
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		if !a.Recording || !a.Request.Recording {
			t.Fatalf("recording was authorized: %+v", a)
		}
		if err := h.Store.RecordingOutcome(ctx, w.Scope, a.Request.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.RecordingOutcome(ctx, w.Scope, a.Request.ID, false); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("the recording outcome is written once: %v", err)
		}
		if m, _ := h.Store.Request(ctx, w.Scope, a.Request.ID); m.RecordingOutcome != "recorded" {
			t.Fatalf("outcome %q", m.RecordingOutcome)
		}

		// No recorder wired: nothing is recorded, and no outcome can be stored.
		w2 := h.NewWorkload(sc.A)
		id, _ := gateway.NewRequestID()
		b, err := h.Store.Admit(ctx, gateway.AdmitCommand{Workload: w2, RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 5, OutputMax: 5, Now: t0})
		if err != nil || b.Recording || b.Request.Recording {
			t.Fatalf("without a recorder: %+v %v", b, err)
		}
		if err := h.Store.RecordingOutcome(ctx, w2.Scope, id, true); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("an outcome for an unrecorded request: %v", err)
		}
	})

	t.Run("writes are all or nothing", func(t *testing.T) {
		h := open(t)
		if h.FailEvents == nil {
			t.Skip("the adapter offers no event fault")
		}
		sc := h.Scopes
		full := gateway.Failf(gateway.ErrBackpressure, "the event backlog is full")

		h.FailEvents(full)
		_, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 1, Edit: edit(nil), Now: t0})
		is(t, err, gateway.ErrBackpressure)
		if got, _ := h.Store.Settings(ctx, sc.A); got.Revision != 1 || h.Audits(sc.A.TenantID, "model_settings.update") != 0 {
			t.Fatal("a settings change whose event cannot be written must leave no revision and no audit row")
		}
		h.FailEvents(nil)
		configure(t, h, sc.A, nil)

		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		begin(t, h, w, a.Request.ID, t0)
		h.FailEvents(full)
		_, err = finalize(h, w, a.Request.ID, observed(30, 10), t0)
		is(t, err, gateway.ErrBackpressure)
		if state(t, h, w, a.Request.ID) != gateway.StateForwarding {
			t.Fatal("a settlement whose event cannot be written leaves the request forwarding")
		}
		if _, err := h.Store.Usage(ctx, w.Scope, a.Request.ID); !errors.Is(err, gateway.ErrNotFound) {
			t.Fatalf("no usage was recorded: %v", err)
		}
		if h.Audits(sc.A.TenantID, "model.request.finalized") != 0 || h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 0 {
			t.Fatal("no audit row or event survived the rollback")
		}
		h.FailEvents(nil)
		u, err := finalize(h, w, a.Request.ID, observed(30, 10), t0)
		if err != nil || u.Charged != 40 {
			t.Fatalf("the retry settles once: %+v %v", u, err)
		}
		if h.Audits(sc.A.TenantID, "model.request.finalized") != 1 || h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 1 {
			t.Fatal("exactly one audit row and one event")
		}
		// The rolled-back attempt charged nothing: the retry's 40 is the run's whole charge.
		next := mustAdmit(t, h, w, t0)
		begin(t, h, w, next.Request.ID, t0)
		if u, err := finalize(h, w, next.Request.ID, observed(10, 10), t0); err != nil || u.Charged != 20 {
			t.Fatalf("%+v %v", u, err)
		}
	})

	t.Run("a request identity is inserted once: it never replaces another run's request or is reused after settlement", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes
		configure(t, h, sc.A, nil)
		wA, wB := h.NewWorkload(sc.A), h.NewWorkload(sc.A)
		id, _ := gateway.NewRequestID()
		cmd := func(w gateway.Workload) gateway.AdmitCommand {
			return gateway.AdmitCommand{Workload: w, RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 50, OutputMax: 20, Now: t0}
		}
		first, err := h.Store.Admit(ctx, cmd(wA))
		if err != nil {
			t.Fatal(err)
		}
		// Another run, same project, same identity: refused without effect.
		if _, err := h.Store.Admit(ctx, cmd(wB)); !errors.Is(err, gateway.ErrInvalid) {
			t.Fatalf("a request identity of another run's request: %v", err)
		}
		if m, err := h.Store.Request(ctx, sc.A, id); err != nil || m.RunID != wA.RunID || m.AttemptID != wA.AttemptID || m.State != gateway.StateAdmitted {
			t.Fatalf("the original request must stand: %+v %v", m, err)
		}
		if _, err := admit(h, wA, "gpt-a", 50, 20, t0); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("the original request must still hold its run's slot: %v", err)
		}
		// The refusal consumed nothing of run B: its first admission is ordinal one.
		if b := mustAdmit(t, h, wB, t0); b.Request.Ordinal != 1 {
			t.Fatalf("a refused identity consumed run B's admission: ordinal %d", b.Request.Ordinal)
		}

		// After finalization the identity is still taken: usage and accounting stay.
		begin(t, h, wA, id, t0)
		u, err := finalize(h, wA, id, observed(30, 10), t0)
		if err != nil || u.Charged != 40 {
			t.Fatalf("%+v %v", u, err)
		}
		for name, w := range map[string]gateway.Workload{"the same run": wA, "another run": h.NewWorkload(sc.A)} {
			if _, err := h.Store.Admit(ctx, cmd(w)); !errors.Is(err, gateway.ErrInvalid) {
				t.Fatalf("reusing a settled identity from %s: %v", name, err)
			}
		}
		if m, err := h.Store.Request(ctx, sc.A, id); err != nil || m.State != gateway.StateSucceeded || m.RunID != wA.RunID || m.Ordinal != 1 {
			t.Fatalf("a settled request must not be overwritten: %+v %v", m, err)
		}
		if got, err := h.Store.Usage(ctx, sc.A, id); err != nil || got.Charged != 40 || got.Result != gateway.StateSucceeded || got.RunID != wA.RunID {
			t.Fatalf("the usage record must stand: %+v %v", got, err)
		}
		if next := mustAdmit(t, h, wA, t0); next.Request.Ordinal != 2 {
			t.Fatalf("a refused reuse consumed an admission: ordinal %d", next.Request.Ordinal)
		}
		if first.Request.ID != id || h.Events(sc.A.TenantID, string(eventsv1.TypeUsageRecorded)) != 1 {
			t.Fatal("exactly one usage event")
		}
		// The identity belongs to its project: the same text in another tenant is another request.
		configure(t, h, sc.B, nil)
		if _, err := h.Store.Admit(ctx, gateway.AdmitCommand{Workload: h.NewWorkload(sc.B), RequestID: id, Model: "gpt-a", RequestBytes: 100, InputBound: 5, OutputMax: 5, Now: t0}); err != nil {
			t.Fatalf("an identity is unique per project: %v", err)
		}
	})

	t.Run("returned and supplied values are copies: mutating them changes no stored policy, usage or history", func(t *testing.T) {
		h := open(t)
		sc := h.Scopes

		// Supplied: the edit's slices after the call.
		in := edit(nil)
		res, err := h.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: h.Admin, Scope: sc.A, Expected: 1, Edit: in, Now: t0})
		if err != nil {
			t.Fatal(err)
		}
		in.Models[0] = "gpt-evil"
		// Returned: the result's, then a read's, allow-list.
		res.Settings.Models[0] = "gpt-evil"
		read, err := h.Store.Settings(ctx, sc.A)
		if err != nil || len(read.Models) != 2 || read.Models[0] != "gpt-a" || read.Models[1] != "gpt-b" {
			t.Fatalf("stored settings changed through a supplied or returned slice: %+v %v", read.Models, err)
		}
		read.Models[1] = "gpt-evil"
		if again, _ := h.Store.Settings(ctx, sc.A); again.Models[1] != "gpt-b" {
			t.Fatalf("stored settings changed through a read: %v", again.Models)
		}

		// The run's binding: mutate the admission's copy of the allow-list.
		w := h.NewWorkload(sc.A)
		a := mustAdmit(t, h, w, t0)
		if len(a.Binding.Models) != 2 {
			t.Fatalf("binding: %+v", a.Binding)
		}
		a.Binding.Models[0], a.Binding.Models[1] = "gpt-evil", "gpt-evil"
		a.Binding.Limits.MaxRequests = 1000
		begin(t, h, w, a.Request.ID, t0)

		// Request pointers.
		if m, err := h.Store.Request(ctx, w.Scope, a.Request.ID); err != nil || m.ForwardingAt == nil {
			t.Fatalf("%+v %v", m, err)
		} else {
			*m.ForwardingAt = t0.Add(100 * time.Hour)
		}
		if m, _ := h.Store.Request(ctx, w.Scope, a.Request.ID); !m.ForwardingAt.Equal(t0) {
			t.Fatalf("a stored timestamp changed through a returned pointer: %v", m.ForwardingAt)
		}

		// Usage pointers, supplied and returned.
		obs := &gateway.TokenUsage{Input: 30, Output: 10}
		o := observed(0, 0)
		o.Observed = obs
		u, err := finalize(h, w, a.Request.ID, o, t0)
		if err != nil {
			t.Fatal(err)
		}
		obs.Input, obs.Output = 9999, 9999
		*u.InputTokens, *u.OutputTokens = 9999, 9999
		got, err := h.Store.Usage(ctx, w.Scope, a.Request.ID)
		if err != nil || *got.InputTokens != 30 || *got.OutputTokens != 10 || got.Charged != 40 {
			t.Fatalf("stored usage changed through a supplied or returned pointer: %+v %v", got, err)
		}
		*got.InputTokens = 1
		if again, _ := h.Store.Usage(ctx, w.Scope, a.Request.ID); *again.InputTokens != 30 {
			t.Fatal("stored usage changed through a read")
		}

		// The mutated binding copy changed nothing: the model policy is the run's own.
		if _, err := admit(h, w, "gpt-evil", 50, 20, t0); !errors.Is(err, gateway.ErrModelNotAllowed) {
			t.Fatalf("a mutated binding copy changed admission policy: %v", err)
		}
		next, err := admit(h, w, "gpt-b", 50, 20, t0)
		if err != nil || len(next.Binding.Models) != 2 || next.Binding.Models[0] != "gpt-a" || next.Binding.Limits.MaxRequests != 5 {
			t.Fatalf("the run's binding must be unchanged: %+v %v", next.Binding, err)
		}
	})
}

func mustAdmitModel(tb testing.TB, h Harness, w gateway.Workload, model string, now time.Time,
	admit func(Harness, gateway.Workload, string, int64, int64, time.Time) (gateway.Admission, error)) gateway.Admission {
	tb.Helper()
	a, err := admit(h, w, model, 50, 20, now)
	if err != nil {
		tb.Fatalf("admit %s: %v", model, err)
	}
	return a
}
