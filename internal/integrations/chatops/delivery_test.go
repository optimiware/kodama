package chatops_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

var (
	t0    = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	scope = domain.Scope{TenantID: "t041_acme", ProjectID: "p041_inventory"}
	retry = chatops.RetryPolicy{Initial: time.Second, Max: 8 * time.Second, MaxAttempts: 3}
)

func destination(t testing.TB) chatops.Destination {
	t.Helper()
	d, err := chatops.NewDestination(scope, "cdst_t041_1", chatops.DestinationInput{
		Name: "ops", Endpoint: "https://hooks.t041.example/a", Events: []string{"gate.requested", "gate.decided"}, Enabled: true}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pending(t testing.TB) chatops.Delivery {
	t.Helper()
	d, err := chatops.NewDelivery(destination(t), "evt_1", "gate.requested", []byte(`{"schemaVersion":1}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func is(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func mustClaim(t testing.TB, d *chatops.Delivery, now time.Time) chatops.ClaimResult {
	t.Helper()
	res, err := d.Claim(now, 30*time.Second, retry)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return res
}

func transient(class chatops.FailureClass) chatops.Outcome {
	return chatops.Outcome{Class: class, Uncertain: class == chatops.ClassTimeout}
}

// TestDeliveryTransitions: valid and invalid transitions, bounded retry
// exhaustion, unknown outcomes, replay rules and stale fencing tokens. No services.
func TestDeliveryTransitions(t *testing.T) {
	t.Run("identity is deterministic and payload digest is recorded", func(t *testing.T) {
		a, b := pending(t), pending(t)
		if a.ID != b.ID || a.ID != chatops.DeliveryIDFor(scope, "cdst_t041_1", "evt_1") {
			t.Fatalf("ids differ: %s %s", a.ID, b.ID)
		}
		if other := chatops.DeliveryIDFor(scope, "cdst_t041_2", "evt_1"); other == a.ID {
			t.Fatal("another destination shares the delivery identity")
		}
		if other := chatops.DeliveryIDFor(domain.Scope{TenantID: scope.TenantID, ProjectID: "p041_other"}, "cdst_t041_1", "evt_1"); other == a.ID {
			t.Fatal("another project shares the delivery identity")
		}
		if a.State != chatops.StatePending || a.PayloadDigest != chatops.PayloadDigest(a.Payload) || a.Version != 1 {
			t.Fatalf("new delivery = %+v", a)
		}
		if _, err := chatops.NewDelivery(destination(t), "evt_1", "usage.recorded", []byte("{}"), t0); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("an unforwarded event type was accepted: %v", err)
		}
	})

	t.Run("pending is claimed only when due and success is final", func(t *testing.T) {
		d := pending(t)
		d.NextAttemptAt = t0.Add(time.Minute)
		if _, err := d.Claim(t0, time.Second, retry); !errors.Is(err, chatops.ErrInvalidTransition) {
			t.Fatalf("claim before due: %v", err)
		}
		res := mustClaim(t, &d, t0.Add(time.Minute))
		if !res.Claimed || res.Reclaimed || d.State != chatops.StateClaimed || d.Attempts != 1 || d.CycleAttempts != 1 || d.ClaimToken != 1 {
			t.Fatalf("claimed = %+v %+v", res, d)
		}
		if err := d.Succeed(d.ClaimToken, t0.Add(time.Minute), chatops.Outcome{Delivered: true, HTTPStatus: 200, Receipt: " rcpt-9 "}); err != nil {
			t.Fatal(err)
		}
		if d.State != chatops.StateDelivered || d.Receipt != "rcpt-9" || d.DeliveredAt.IsZero() || d.LeaseExpiresAt != (time.Time{}) {
			t.Fatalf("delivered = %+v", d)
		}
		// Terminal: nothing moves a delivered notification.
		if _, err := d.Claim(t0.Add(time.Hour), time.Second, retry); !errors.Is(err, chatops.ErrInvalidTransition) {
			t.Fatalf("claim of a delivered delivery: %v", err)
		}
		is(t, d.Succeed(1, t0, chatops.Outcome{Delivered: true}), chatops.ErrStaleClaim)
		if _, err := d.Fail(1, t0, transient(chatops.ClassTimeout), retry); !errors.Is(err, chatops.ErrStaleClaim) {
			t.Fatalf("fail of a delivered delivery: %v", err)
		}
		is(t, d.Replay(t0, "again", 1), chatops.ErrInvalidTransition)
		if got := (chatops.Outcome{Delivered: true, Receipt: strings.Repeat("x", 200)}); chatops.SanitizeReceipt(got.Receipt) != "" {
			t.Fatal("an oversized receipt was kept")
		}
		if chatops.SanitizeReceipt("has space") != "" || chatops.SanitizeReceipt("bad\x01") != "" || chatops.SanitizeReceipt("") != "" {
			t.Fatal("an unsafe receipt was kept")
		}
	})

	t.Run("a transient failure schedules bounded backoff and exhaustion fails visibly", func(t *testing.T) {
		d := pending(t)
		now := t0
		var delays []time.Duration
		for attempt := 1; attempt <= 3; attempt++ {
			mustClaim(t, &d, now)
			rt, err := d.Fail(d.ClaimToken, now, transient(chatops.ClassUnreachable), retry)
			if err != nil {
				t.Fatal(err)
			}
			if attempt < 3 {
				if !rt || d.State != chatops.StatePending {
					t.Fatalf("attempt %d: retry=%v state=%s", attempt, rt, d.State)
				}
				delays = append(delays, d.NextAttemptAt.Sub(now))
				now = d.NextAttemptAt
				continue
			}
			if rt || d.State != chatops.StateFailed || d.FailedReason != chatops.FailedExhausted || d.LastClass != chatops.ClassUnreachable {
				t.Fatalf("exhaustion: retry=%v %+v", rt, d)
			}
		}
		if delays[0] != time.Second || delays[1] != 2*time.Second {
			t.Fatalf("backoff = %v", delays)
		}
		if got := retry.Delay(10); got != 8*time.Second {
			t.Fatalf("delay is not capped: %s", got)
		}
		// A server-supplied Retry-After lengthens the wait but never past the maximum.
		e := pending(t)
		mustClaim(t, &e, t0)
		if _, err := e.Fail(e.ClaimToken, t0, chatops.Outcome{Class: chatops.ClassRateLimited, RetryAfter: time.Hour}, retry); err != nil {
			t.Fatal(err)
		}
		if got := e.NextAttemptAt.Sub(t0); got != retry.Max {
			t.Fatalf("Retry-After wait = %s, want the policy maximum", got)
		}
	})

	t.Run("permanent classes and unknown classes", func(t *testing.T) {
		for _, class := range []chatops.FailureClass{chatops.ClassRejected, chatops.ClassPolicyRefused, chatops.ClassCredentialUnavailable,
			chatops.ClassRedirectRefused, chatops.ClassDestinationChanged} {
			d := pending(t)
			mustClaim(t, &d, t0)
			rt, err := d.Fail(d.ClaimToken, t0, chatops.Outcome{Class: class}, retry)
			if err != nil || rt || d.State != chatops.StateFailed || d.FailedReason != chatops.FailedPermanent {
				t.Fatalf("%s: retry=%v err=%v %+v", class, rt, err, d)
			}
		}
		d := pending(t)
		mustClaim(t, &d, t0)
		if _, err := d.Fail(d.ClaimToken, t0, chatops.Outcome{Class: "made_up"}, retry); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("an unknown class was recorded: %v", err)
		}
		if _, err := d.Fail(d.ClaimToken, t0, chatops.Outcome{Delivered: true, Class: chatops.ClassTimeout}, retry); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("a delivered outcome was recorded as a failure: %v", err)
		}
		if err := d.Succeed(d.ClaimToken, t0, chatops.Outcome{Class: chatops.ClassTimeout}); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("a failure completed a delivery: %v", err)
		}
	})

	t.Run("a timeout is an unknown outcome and is recorded as uncertain", func(t *testing.T) {
		d := pending(t)
		mustClaim(t, &d, t0)
		if _, err := d.Fail(d.ClaimToken, t0, transient(chatops.ClassTimeout), retry); err != nil {
			t.Fatal(err)
		}
		if !d.LastUncertain || d.State != chatops.StatePending {
			t.Fatalf("a timeout must stay retryable and uncertain: %+v", d)
		}
	})

	t.Run("an expired claim is reclaimed under a new fencing token and the old holder is stale", func(t *testing.T) {
		d := pending(t)
		mustClaim(t, &d, t0)
		old := d.ClaimToken
		if _, err := d.Claim(t0.Add(10*time.Second), time.Minute, retry); !errors.Is(err, chatops.ErrInvalidTransition) {
			t.Fatalf("a live claim was taken over: %v", err)
		}
		res := mustClaim(t, &d, t0.Add(31*time.Second))
		if !res.Claimed || !res.Reclaimed || d.ClaimToken != old+1 || d.Attempts != 2 {
			t.Fatalf("reclaim = %+v %+v", res, d)
		}
		// The previous holder's late outcome changes nothing.
		is(t, d.Succeed(old, t0.Add(40*time.Second), chatops.Outcome{Delivered: true}), chatops.ErrStaleClaim)
		if _, err := d.Fail(old, t0.Add(40*time.Second), transient(chatops.ClassTimeout), retry); !errors.Is(err, chatops.ErrStaleClaim) {
			t.Fatalf("stale fail: %v", err)
		}
		if d.State != chatops.StateClaimed || d.ClaimToken != old+1 {
			t.Fatalf("a stale holder changed the delivery: %+v", d)
		}
		if err := d.Succeed(d.ClaimToken, t0.Add(41*time.Second), chatops.Outcome{Delivered: true, HTTPStatus: 204}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an expired last attempt is exhausted, not sent again", func(t *testing.T) {
		d := pending(t)
		now := t0
		for i := 0; i < retry.MaxAttempts; i++ {
			res := mustClaim(t, &d, now)
			if i == retry.MaxAttempts-1 && !res.Claimed {
				t.Fatal("the last attempt was not claimed")
			}
			now = now.Add(31 * time.Second)
		}
		res := mustClaim(t, &d, now)
		if res.Claimed || !res.Exhausted || d.State != chatops.StateFailed || d.FailedReason != chatops.FailedExhausted || d.LastClass != chatops.ClassLeaseExpired || !d.LastUncertain {
			t.Fatalf("expired crash loop = %+v %+v", res, d)
		}
	})

	t.Run("replay reopens only failed deliveries with a reason and keeps the history", func(t *testing.T) {
		d := pending(t)
		mustClaim(t, &d, t0)
		if _, err := d.Fail(d.ClaimToken, t0, chatops.Outcome{Class: chatops.ClassRejected, HTTPStatus: 400}, retry); err != nil {
			t.Fatal(err)
		}
		attempts, token := d.Attempts, d.ClaimToken
		if err := d.Replay(t0, "  ", 1); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("replay without a reason: %v", err)
		}
		if err := d.Replay(t0, strings.Repeat("r", chatops.MaxReasonBytes+1), 1); !errors.Is(err, chatops.ErrInvalid) {
			t.Fatalf("replay with an oversized reason: %v", err)
		}
		if err := d.Replay(t0.Add(time.Hour), "receiver fixed", 7); err != nil {
			t.Fatal(err)
		}
		if d.State != chatops.StatePending || d.CycleAttempts != 0 || d.Attempts != attempts || d.ClaimToken != token || d.Replays != 1 ||
			d.BindingRevision != 7 || d.FailedReason != "" || !d.NextAttemptAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("replayed = %+v", d)
		}
		// A pending or claimed delivery is already moving.
		is(t, d.Replay(t0, "again", 7), chatops.ErrInvalidTransition)
		mustClaim(t, &d, t0.Add(time.Hour))
		is(t, d.Replay(t0, "again", 7), chatops.ErrInvalidTransition)
		// The replay opened a new cycle with a full set of attempts.
		if d.CycleAttempts != 1 || d.Attempts != attempts+1 {
			t.Fatalf("cycle = %d total = %d", d.CycleAttempts, d.Attempts)
		}
	})

	t.Run("a changed binding refuses a due delivery instead of redirecting it", func(t *testing.T) {
		d := pending(t)
		if err := d.Refuse(t0); err != nil {
			t.Fatal(err)
		}
		if d.State != chatops.StateFailed || d.FailedReason != chatops.FailedRefused || d.LastClass != chatops.ClassDestinationChanged || d.Attempts != 0 {
			t.Fatalf("refused = %+v", d)
		}
		x := pending(t)
		x.NextAttemptAt = t0.Add(time.Hour)
		is(t, x.Refuse(t0), chatops.ErrInvalidTransition)
	})

	t.Run("retry policy validation", func(t *testing.T) {
		for _, p := range []chatops.RetryPolicy{{}, {Initial: time.Second, Max: time.Millisecond, MaxAttempts: 1}, {Initial: time.Second, Max: time.Second, MaxAttempts: 0},
			{Initial: time.Second, Max: time.Second, MaxAttempts: 101}} {
			if err := p.Validate(); !errors.Is(err, chatops.ErrInvalid) {
				t.Fatalf("%+v accepted: %v", p, err)
			}
		}
	})
}

// TestRetryDelayBounds: every valid policy yields a positive delay no larger
// than its maximum, including near the Duration limit where doubling overflows.
func TestRetryDelayBounds(t *testing.T) {
	const maxD = time.Duration(1<<63 - 1)
	policies := []chatops.RetryPolicy{
		retry,
		{Initial: time.Second, Max: time.Second, MaxAttempts: 100},
		{Initial: maxD/2 + 1, Max: maxD, MaxAttempts: 100},
		{Initial: maxD / 3, Max: maxD, MaxAttempts: 100},
		{Initial: maxD, Max: maxD, MaxAttempts: 100},
		{Initial: time.Nanosecond, Max: maxD, MaxAttempts: 100},
		{Initial: 1, Max: 3, MaxAttempts: 100},
	}
	for _, p := range policies {
		if err := p.Validate(); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		prev := time.Duration(0)
		for a := 1; a <= 100; a++ {
			got := p.Delay(a)
			if got <= 0 || got > p.Max || got < prev {
				t.Fatalf("%+v attempt %d: delay %s (previous %s)", p, a, got, prev)
			}
			prev = got
		}
	}
	if got := retry.Delay(1); got != time.Second {
		t.Fatalf("normal schedule changed: %s", got)
	}
	if got := retry.Delay(3); got != 4*time.Second {
		t.Fatalf("normal schedule changed: %s", got)
	}
	// A near-limit Retry-After never pushes the next attempt past the maximum or wraps.
	d := pending(t)
	res, err := d.Claim(t0, time.Minute, retry)
	if err != nil || !res.Claimed {
		t.Fatal(res, err)
	}
	if _, err := d.Fail(d.ClaimToken, t0, chatops.Outcome{Class: chatops.ClassRateLimited, RetryAfter: maxD}, retry); err != nil {
		t.Fatal(err)
	}
	if !d.NextAttemptAt.Equal(t0.Add(retry.Max)) {
		t.Fatalf("next attempt %s", d.NextAttemptAt)
	}
}

// TestDestinationEdit: versions, binding revisions and validation.
func TestDestinationEdit(t *testing.T) {
	d := destination(t)
	str := func(s string) *string { return &s }
	flag := func(b bool) *bool { return &b }

	if _, err := d.Edit(2, chatops.DestinationEdit{Name: str("x")}, t0); !errors.Is(err, chatops.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	changed, err := d.Edit(1, chatops.DestinationEdit{Enabled: flag(true)}, t0)
	if err != nil || changed || d.Version != 1 {
		t.Fatalf("a no-op edit changed the version: %v %v %+v", changed, err, d)
	}
	if changed, err = d.Edit(1, chatops.DestinationEdit{Enabled: flag(false), Name: str("renamed")}, t0.Add(time.Second)); err != nil || !changed ||
		d.Version != 2 || d.Enabled || d.BindingRevision != 1 {
		t.Fatalf("disable: %v %v %+v", changed, err, d)
	}
	if _, err = d.Edit(2, chatops.DestinationEdit{Endpoint: str("https://hooks.t041.example/b")}, t0); err != nil || d.BindingRevision != 2 {
		t.Fatalf("a new endpoint must advance the binding: %v %+v", err, d)
	}
	if _, err = d.Edit(3, chatops.DestinationEdit{Credential: str("cred/ops")}, t0); err != nil || d.BindingRevision != 3 || d.Credential != "cred/ops" {
		t.Fatalf("a credential change must advance the binding: %v %+v", err, d)
	}
	for name, edit := range map[string]chatops.DestinationEdit{
		"unsupported event": {Events: []string{"usage.recorded"}},
		"no events":         {Events: []string{}},
		"bad endpoint":      {Endpoint: str("http://hooks.t041.example/b")},
		"bad name":          {Name: str("Not Valid")},
		"bad credential":    {Credential: str("with space")},
	} {
		before := d
		if _, err := d.Edit(d.Version, edit, t0); !errors.Is(err, chatops.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
		if d.Version != before.Version || d.Endpoint != before.Endpoint {
			t.Errorf("%s: a refused edit changed the destination", name)
		}
	}
	// Events are normalized (sorted, de-duplicated).
	if _, err := d.Edit(d.Version, chatops.DestinationEdit{Events: []string{"gate.decided", "gate.requested", "gate.decided"}}, t0); err != nil ||
		len(d.Events) != 2 || d.Events[0] != "gate.decided" {
		t.Fatalf("events = %v %v", d.Events, err)
	}
}

// TestEndpointShape: the generic shape rules enforced before any policy.
func TestEndpointShape(t *testing.T) {
	good := []string{"https://hooks.example.com/a", "https://hooks.example.com:8443/a/b", "https://203.0.113.9/x", "https://[2606:4700::1]/x"}
	bad := []string{"", "http://hooks.example.com/a", "ftp://x/y", "//hooks.example.com/a", "https:///a", "https://user:pw@hooks.example.com/a",
		"https://user@hooks.example.com/a", "https://hooks.example.com/a#frag", "https://hooks.example.com/a?token=1", "https://hooks.example.com/a?",
		"https://hooks.example.com:0/a", "https://hooks.example.com:99999/a", "https://hooks example.com/a", "https://hooks.example.com/a\n",
		"mailto:a@b", "https://" + strings.Repeat("a", 2100)}
	for _, u := range good {
		if err := chatops.ValidateEndpoint(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := chatops.ValidateEndpoint(u); !errors.Is(err, chatops.ErrInvalid) {
			t.Errorf("%q accepted: %v", u, err)
		}
	}
}
