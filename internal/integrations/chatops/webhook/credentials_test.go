package webhook_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
)

const token = "tok-SECRET-4f9a"

// TestCredentialAndDestinationChanges: missing, revoked and wrong-recipient
// credentials prevent sends; disabled or changed destinations do not silently
// retarget old intents; secrets never enter diagnostics.
func TestCredentialAndDestinationChanges(t *testing.T) {
	t.Run("a credential is resolved for exactly the recipient and sent as a bearer", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		src := &source{fn: func(ref string, r chatops.Recipient) (chatops.Credential, error) {
			return chatops.Credential{Bearer: token}, nil
		}}
		out := send(t, client(t, webhook.Config{Credentials: src}, rc), request(scopeA, rc.URL(), "cred/ops"), 3*time.Second)
		if !out.Delivered {
			t.Fatalf("outcome = %+v", out)
		}
		if got := rc.Requests()[0].Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("authorization = %q", got)
		}
		if len(src.calls) != 1 || src.refs[0] != "cred/ops" {
			t.Fatalf("source calls = %v %v", src.calls, src.refs)
		}
		r := src.calls[0]
		if r.Scope != scopeA || r.DestinationID != "cdst_t041_1" || r.BindingRevision != 3 || r.Host != "127.0.0.1" || r.Port != int(rc.AddrPort().Port()) {
			t.Fatalf("recipient = %+v", r)
		}
	})

	t.Run("a credential-free destination never asks the source", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		src := &source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
			t.Error("source consulted")
			return chatops.Credential{}, nil
		}}
		if out := send(t, client(t, webhook.Config{Credentials: src}, rc), request(scopeA, rc.URL(), ""), 3*time.Second); !out.Delivered {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("missing, revoked, wrong-recipient and unsafe credentials prevent the send", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		bound := "203.0.113.50" // the host the credential was issued for
		cases := map[string]struct {
			src  chatops.CredentialSource
			want chatops.FailureClass
		}{
			"no source configured": {nil, chatops.ClassCredentialUnavailable},
			"revoked": {&source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
				return chatops.Credential{}, fmt.Errorf("version revoked: %w", chatops.ErrCredentialUnavailable)
			}}, chatops.ClassCredentialUnavailable},
			"bound to another recipient": {&source{fn: func(_ string, r chatops.Recipient) (chatops.Credential, error) {
				if r.Host != bound {
					return chatops.Credential{}, chatops.ErrCredentialUnavailable
				}
				return chatops.Credential{Bearer: token}, nil
			}}, chatops.ClassCredentialUnavailable},
			"empty material": {&source{fn: func(string, chatops.Recipient) (chatops.Credential, error) { return chatops.Credential{}, nil }}, chatops.ClassCredentialUnavailable},
			"header injection": {&source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
				return chatops.Credential{Bearer: token + "\r\nX-Evil: 1"}, nil
			}}, chatops.ClassCredentialUnavailable},
			"embedded space": {&source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
				return chatops.Credential{Bearer: "a b"}, nil
			}}, chatops.ClassCredentialUnavailable},
			"source outage": {&source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
				return chatops.Credential{}, errors.New("dial tcp 10.1.2.3:8200: connection refused " + token)
			}}, chatops.ClassCredentialSourceDown},
		}
		for name, tc := range cases {
			cfg := webhook.Config{}
			if tc.src != nil {
				cfg.Credentials = tc.src
			}
			out := send(t, client(t, cfg, rc), request(scopeA, rc.URL(), "cred/ops"), 3*time.Second)
			if out.Delivered || out.Class != tc.want || out.Uncertain {
				t.Errorf("%s: %+v", name, out)
			}
			// Credential failures are classified, and only the permanent ones need a person.
			if wantPerm := tc.want == chatops.ClassCredentialUnavailable; out.Class.Permanent() != wantPerm {
				t.Errorf("%s: permanence = %v", name, out.Class.Permanent())
			}
			// Secrets and source diagnostics never enter the recorded outcome.
			if s := fmt.Sprintf("%+v", out); strings.Contains(s, "SECRET") || strings.Contains(s, "10.1.2.3") || strings.Contains(s, "cred/ops") {
				t.Errorf("%s: the outcome leaks: %s", name, s)
			}
		}
		if rc.Count() != 0 {
			t.Fatalf("a send went out without a valid credential: %d requests", rc.Count())
		}
	})

	t.Run("an authenticated destination cannot be activated without a credential source", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{}) // no credential source
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "cred")
		_, err := e.Svc.CreateDestination(context.Background(), a.Admin, scopeA, chatops.DestinationInput{
			Name: "authed", Endpoint: "https://hooks.t041.example/a", Events: chatopstest.AllEvents(), Credential: "cred/ops", Enabled: true})
		if !errors.Is(err, chatops.ErrNotConfigured) {
			t.Fatalf("got %v, want a fail-closed refusal", err)
		}
		if n := e.Count(`SELECT count(*) FROM chatops_destinations`); n != 0 {
			t.Fatalf("a refused destination was stored (%d)", n)
		}
		// A credential-free destination still works.
		if d := e.Destination(a.Admin, scopeA, "plain", "https://hooks.t041.example/b"); d.Credential != "" {
			t.Fatalf("destination = %+v", d)
		}
	})

	t.Run("activation proves the credential resolves for this recipient", func(t *testing.T) {
		src := &source{fn: func(ref string, r chatops.Recipient) (chatops.Credential, error) {
			if ref == "cred/good" && r.Host == "hooks.t041.example" {
				return chatops.Credential{Bearer: token}, nil
			}
			return chatops.Credential{}, chatops.ErrCredentialUnavailable
		}}
		e := chatopstest.New(t, chatopstest.Options{Credentials: src})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "cred2")
		create := func(name, host, ref string, enabled bool) error {
			_, err := e.Svc.CreateDestination(context.Background(), a.Admin, scopeA, chatops.DestinationInput{
				Name: name, Endpoint: "https://" + host + "/a", Events: chatopstest.AllEvents(), Credential: ref, Enabled: enabled})
			return err
		}
		if err := create("good", "hooks.t041.example", "cred/good", true); err != nil {
			t.Fatal(err)
		}
		if err := create("revoked", "hooks.t041.example", "cred/revoked", true); !errors.Is(err, chatops.ErrCredentialUnavailable) {
			t.Fatalf("revoked: %v", err)
		}
		if err := create("wronghost", "other.t041.example", "cred/good", true); !errors.Is(err, chatops.ErrCredentialUnavailable) {
			t.Fatalf("wrong recipient: %v", err)
		}
		// A disabled draft is stored but cannot be enabled until the credential resolves.
		if err := create("draft", "other.t041.example", "cred/good", false); err != nil {
			t.Fatalf("draft: %v", err)
		}
		list, err := e.Svc.ListDestinations(context.Background(), a.Admin, scopeA, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		var draft chatops.Destination
		for _, d := range list {
			if d.Name == "draft" {
				draft = d
			}
		}
		on := true
		if _, err := e.Svc.UpdateDestination(context.Background(), a.Admin, scopeA, draft.ID, draft.Version, chatops.DestinationEdit{Enabled: &on}); !errors.Is(err, chatops.ErrCredentialUnavailable) {
			t.Fatalf("enabling a draft with a wrong-recipient credential: %v", err)
		}
	})

	t.Run("disabled and changed destinations do not silently retarget old intents", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "changes")
		oldRC, newRC := chatopstest.NewReceiver(t, nil), chatopstest.NewReceiver(t, nil)
		e.Allow(oldRC.AddrPort())
		e.Allow(newRC.AddrPort())
		w := e.Worker(e.Client(oldRC))
		ctx := context.Background()
		process := func(env eventsv1.Envelope) {
			t.Helper()
			e.Append(env)
			payload, _ := eventsv1.Encode(env)
			if out, err := e.Events.Process(ctx, events.Delivery{Tenant: env.TenantID, Consumer: chatops.ConsumerName, Payload: payload}); err != nil || out.Kind != events.OutcomeCompleted {
				t.Fatalf("process: %+v %v", out, err)
			}
		}

		// 1. A changed endpoint refuses the old intent; neither receiver is contacted.
		d := e.Destination(a.Admin, scopeA, "moving", oldRC.URL())
		process(chatopstest.GateRequested(scopeA, "evt_move", "gate_1", 1, 'a'))
		url := newRC.URL()
		d2, err := e.Svc.UpdateDestination(ctx, a.Admin, scopeA, d.ID, d.Version, chatops.DestinationEdit{Endpoint: &url})
		if err != nil || d2.BindingRevision != 2 {
			t.Fatalf("update: %v %+v", err, d2)
		}
		res, err := w.RunOnce(ctx)
		if err != nil || res.Refused != 1 || res.Claimed != 0 {
			t.Fatalf("worker = %+v %v", res, err)
		}
		got := e.DeliveryOf(d.ID, "evt_move")
		if got.State != chatops.StateFailed || got.FailedReason != chatops.FailedRefused || got.LastClass != chatops.ClassDestinationChanged || got.Attempts != 0 {
			t.Fatalf("delivery = %+v", got)
		}
		if oldRC.Count()+newRC.Count() != 0 {
			t.Fatalf("an old intent was sent to %d/%d receivers after the endpoint changed", oldRC.Count(), newRC.Count())
		}
		// New events use the new binding and reach the new receiver only.
		process(chatopstest.GateRequested(scopeA, "evt_after", "gate_2", 1, 'b'))
		if fresh := e.DeliveryOf(d.ID, "evt_after"); fresh.BindingRevision != 2 {
			t.Fatalf("a new intent was created under binding %d", fresh.BindingRevision)
		}
		wNew := e.Worker(e.Client(newRC))
		if res, err := wNew.RunOnce(ctx); err != nil || res.Claimed != 1 {
			t.Fatalf("worker = %+v %v", res, err)
		}
		if newRC.Count() != 1 || oldRC.Count() != 0 {
			t.Fatalf("receivers: new=%d old=%d", newRC.Count(), oldRC.Count())
		}
		// The explicit, audited replay is the only way an old intent follows the new binding.
		view, err := e.Svc.GetDelivery(ctx, a.Viewer, scopeA, got.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, got.ID, view.Delivery.Version, "endpoint moved; resend"); err != nil {
			t.Fatal(err)
		}
		if res, err := wNew.RunOnce(ctx); err != nil || res.Delivered != 1 {
			t.Fatalf("replayed worker = %+v %v", res, err)
		}
		if newRC.Count() != 2 || oldRC.Count() != 0 {
			t.Fatalf("after replay receivers: new=%d old=%d", newRC.Count(), oldRC.Count())
		}

		// 2. A disabled destination prevents new sends; its intent waits, unchanged.
		off := e.Destination(a.Admin, scopeA, "pausable", newRC.URL())
		process(chatopstest.GateRequested(scopeA, "evt_pause", "gate_3", 1, 'c'))
		disable := false
		off2, err := e.Svc.UpdateDestination(ctx, a.Admin, scopeA, off.ID, off.Version, chatops.DestinationEdit{Enabled: &disable})
		if err != nil || off2.Enabled || off2.BindingRevision != off.BindingRevision {
			t.Fatalf("disable: %v %+v", err, off2)
		}
		for i := 0; i < 3; i++ {
			if res, err := wNew.RunOnce(ctx); err != nil || res.Refused != 0 || res.Claimed > 1 {
				t.Fatalf("worker pass %d = %+v %v", i, res, err)
			}
		}
		heldID := string(chatops.DeliveryIDFor(scopeA, off.ID, "evt_pause"))
		for _, q := range newRC.Requests() {
			if q.Header.Get("Kodama-Delivery-Id") == heldID {
				t.Fatalf("a disabled destination received its intent")
			}
		}
		held := e.DeliveryOf(off.ID, "evt_pause")
		if held.State != chatops.StatePending || held.Attempts != 0 {
			t.Fatalf("a disabled destination's intent moved: %+v", held)
		}
		// Re-enabling resumes the very same intent under the same binding.
		enable := true
		if _, err := e.Svc.UpdateDestination(ctx, a.Admin, scopeA, off.ID, off2.Version, chatops.DestinationEdit{Enabled: &enable}); err != nil {
			t.Fatal(err)
		}
		if res, err := wNew.RunOnce(ctx); err != nil || res.Delivered != 1 {
			t.Fatalf("resumed worker = %+v %v", res, err)
		}
		if got := e.DeliveryOf(off.ID, "evt_pause"); got.State != chatops.StateDelivered || got.Attempts != 1 {
			t.Fatalf("resumed delivery = %+v", got)
		}
	})
}
