package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	t0     = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	policy = RetryPolicy{Initial: time.Second, Max: 8 * time.Second, MaxAttempts: 3}
)

func TestDeliveryTransitions(t *testing.T) {
	t.Run("a scheduled inbox retry is not due early", func(t *testing.T) {
		e := NewInboxEntry("t_1", "audit", "evt_1")
		if !e.Due(t0) || e.RetryIn(t0) != 0 {
			t.Fatal("a fresh entry is due")
		}
		_ = e.Begin()
		if q, err := e.Fail(ClassTransient, "later", t0, RetryPolicy{Initial: time.Minute, Max: time.Hour, MaxAttempts: 3}); err != nil || q {
			t.Fatalf("fail: %v %v", q, err)
		}
		if e.Due(t0.Add(30*time.Second)) || e.RetryIn(t0.Add(30*time.Second)) != 30*time.Second {
			t.Fatalf("a retry scheduled for a minute must not be due after 30s (in %s)", e.RetryIn(t0.Add(30*time.Second)))
		}
		if !e.Due(t0.Add(time.Minute)) {
			t.Fatal("the entry is due at its scheduled time")
		}
	})
	t.Run("publication lifecycle with fencing", func(t *testing.T) {
		p := Publication{State: PublicationPending, EventID: "evt_1"}
		first, err := p.Claim(t0, time.Minute)
		if err != nil || first != 1 || p.State != PublicationClaimed || p.Attempts != 1 {
			t.Fatalf("claim: token=%d err=%v state=%s attempts=%d", first, err, p.State, p.Attempts)
		}
		// A live lease is exclusive.
		if _, err := p.Claim(t0.Add(30*time.Second), time.Minute); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("claim of a live lease: %v", err)
		}
		// An expired lease is reclaimable and fences the previous holder.
		second, err := p.Claim(t0.Add(2*time.Minute), time.Minute)
		if err != nil || second != 2 {
			t.Fatalf("reclaim: %d %v", second, err)
		}
		if err := p.Complete(first, t0.Add(2*time.Minute)); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale completion: %v", err)
		}
		if err := p.Fail(first, ClassTransient, "x", t0, policy); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale failure: %v", err)
		}
		if err := p.Complete(second, t0.Add(2*time.Minute)); err != nil {
			t.Fatalf("current completion: %v", err)
		}
		if p.State != PublicationPublished {
			t.Fatalf("state = %s", p.State)
		}
		// Published is terminal.
		if _, err := p.Claim(t0.Add(time.Hour), time.Minute); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("claim of a published event: %v", err)
		}
		if err := p.Complete(second, t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("second completion: %v", err)
		}
	})

	t.Run("publication retry schedule and unclassified failure", func(t *testing.T) {
		p := Publication{State: PublicationPending}
		token, _ := p.Claim(t0, time.Minute)
		if err := p.Fail(token, "", "boom", t0, policy); !errors.Is(err, ErrUnclassified) {
			t.Fatalf("unclassified failure accepted: %v", err)
		}
		if p.State != PublicationClaimed {
			t.Fatalf("a rejected failure must not change state, got %s", p.State)
		}
		if err := p.Fail(token, ClassTransient, "broker down", t0, policy); err != nil {
			t.Fatal(err)
		}
		if p.State != PublicationPending || !p.NextAttemptAt.Equal(t0.Add(time.Second)) {
			t.Fatalf("retry: state=%s next=%s", p.State, p.NextAttemptAt)
		}
		if p.Due(t0) || !p.Due(t0.Add(time.Second)) {
			t.Fatal("retry not scheduled for its delay")
		}
		if _, err := p.Claim(t0, time.Minute); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("claim before the retry is due: %v", err)
		}
		if err := p.Complete(token, t0); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("completion of a released claim: %v", err)
		}
	})

	t.Run("backoff is bounded", func(t *testing.T) {
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
		for i, w := range want {
			if got := policy.Delay(i + 1); got != w {
				t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
			}
		}
	})

	t.Run("inbox completion is final", func(t *testing.T) {
		e := NewInboxEntry("t_acme", "audit", "evt_1")
		if err := e.Begin(); err != nil {
			t.Fatal(err)
		}
		if err := e.Complete(t0); err != nil {
			t.Fatal(err)
		}
		if err := e.Begin(); !errors.Is(err, ErrAlreadyCompleted) {
			t.Fatalf("Begin after completion: %v", err)
		}
		if err := e.Complete(t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("second Complete: %v", err)
		}
		if _, err := e.Fail(ClassTransient, "x", t0, policy); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Fail after completion: %v", err)
		}
		if err := e.Reopen(authorizedGrant(t)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a completed entry must not be reopened: %v", err)
		}
	})

	t.Run("inbox retry then quarantine on exhaustion", func(t *testing.T) {
		e := NewInboxEntry("t_acme", "audit", "evt_1")
		for attempt := 1; attempt <= 3; attempt++ {
			if err := e.Begin(); err != nil {
				t.Fatal(err)
			}
			q, err := e.Fail(ClassTransient, "db down", t0, policy)
			if err != nil {
				t.Fatal(err)
			}
			if q != (attempt == 3) {
				t.Fatalf("attempt %d quarantined=%v", attempt, q)
			}
		}
		if e.State != InboxQuarantined {
			t.Fatalf("state = %s", e.State)
		}
		if err := e.Begin(); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("Begin while quarantined: %v", err)
		}
	})

	t.Run("permanent failure and unclassified failure", func(t *testing.T) {
		e := NewInboxEntry("t_acme", "audit", "evt_1")
		_ = e.Begin()
		if _, err := e.Fail("", "x", t0, policy); !errors.Is(err, ErrUnclassified) {
			t.Fatalf("unclassified retry accepted: %v", err)
		}
		if q, err := e.Fail(ClassPermanent, "poison", t0, policy); err != nil || !q {
			t.Fatalf("permanent failure must quarantine immediately: %v %v", q, err)
		}
		direct := NewInboxEntry("t", "c", "e")
		if err := direct.Quarantine("bad"); err != nil || direct.State != InboxQuarantined {
			t.Fatalf("direct quarantine: %v", err)
		}
	})

	t.Run("replay needs an authorized grant", func(t *testing.T) {
		e := NewInboxEntry("t_acme", "audit", "evt_1")
		_ = e.Begin()
		_, _ = e.Fail(ClassPermanent, "poison", t0, policy)
		if err := e.Reopen(ReplayGrant{}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("zero grant reopened a quarantined entry: %v", err)
		}
		if e.State != InboxQuarantined {
			t.Fatal("state changed without authorization")
		}
		if err := e.Reopen(authorizedGrant(t)); err != nil {
			t.Fatal(err)
		}
		if e.State != InboxPending || e.Attempts != 0 || e.Replays != 1 {
			t.Fatalf("after replay: %+v", e)
		}
	})
}

type allow struct{ err error }

func (a allow) AuthorizeReplay(context.Context, Operator, Scope, string) error { return a.err }

func validRequest() ReplayRequest {
	return ReplayRequest{Scope: Scope{TenantID: "t_acme", ProjectID: "p_1"}, Consumer: "audit", FromSeq: 1, ToSeq: 10, Reason: "fix handler", Operator: Operator{ID: "ops_1"}}
}

func authorizedGrant(t *testing.T) ReplayGrant {
	t.Helper()
	g, err := Authorize(context.Background(), allow{}, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestReplayAuthorizationAndRequest(t *testing.T) {
	ctx := context.Background()
	if _, err := Authorize(ctx, nil, validRequest()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("nil authorizer must fail closed: %v", err)
	}
	if _, err := Authorize(ctx, allow{err: errors.New("no")}, validRequest()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("denied operator: %v", err)
	}
	bad := map[string]func(*ReplayRequest){
		"no scope":        func(r *ReplayRequest) { r.Scope = Scope{} },
		"no consumer":     func(r *ReplayRequest) { r.Consumer = "" },
		"no range":        func(r *ReplayRequest) { r.FromSeq, r.ToSeq = 0, 0 },
		"inverted range":  func(r *ReplayRequest) { r.FromSeq, r.ToSeq = 10, 2 },
		"unbounded range": func(r *ReplayRequest) { r.ToSeq = MaxReplaySpan + 10 },
		"no reason":       func(r *ReplayRequest) { r.Reason = "  " },
		"no operator":     func(r *ReplayRequest) { r.Operator = Operator{} },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if _, err := Authorize(ctx, allow{}, r); err == nil {
				t.Fatal("request accepted")
			}
		})
	}
	g, err := Authorize(ctx, allow{}, validRequest())
	if err != nil || g.Validate() != nil || g.Request().Consumer != "audit" {
		t.Fatalf("grant: %+v %v", g, err)
	}
}

func TestReplayClassificationCheck(t *testing.T) {
	local := ConsumerSpec{Name: "audit", Mode: ModeHistorical, Effect: EffectLocal}
	cases := []struct {
		name  string
		c     ReplayClassification
		spec  ConsumerSpec
		allow bool
		want  error
	}{
		{"safe", ReplayClassification{Events: 3, Quarantined: 1}, local, false, nil},
		{"expired range", ReplayClassification{Expired: 1}, local, false, ErrReplayRangeExpired},
		{"fenced", ReplayClassification{Fenced: true}, local, false, ErrReplayFenced},
		{"unknown effects", ReplayClassification{}, ConsumerSpec{Name: "x", Mode: ModeHistorical, Effect: EffectUnknown}, false, ErrReplayUnsafe},
		{"beyond horizon refused", ReplayClassification{BeyondHorizon: 1}, local, true, ErrReplayBeyondHorizon},
		{"beyond horizon needs the flag", ReplayClassification{BeyondHorizon: 1}, ConsumerSpec{Name: "audit", Mode: ModeHistorical, Effect: EffectLocal, IdempotentBeyondHorizon: true}, false, ErrReplayBeyondHorizon},
		{"beyond horizon declared safe", ReplayClassification{BeyondHorizon: 1}, ConsumerSpec{Name: "audit", Mode: ModeHistorical, Effect: EffectLocal, IdempotentBeyondHorizon: true}, true, nil},
		{"external intents never repeat beyond horizon", ReplayClassification{BeyondHorizon: 1}, ConsumerSpec{Name: "n", Mode: ModeHistorical, Effect: EffectExternalIntent}, true, ErrReplayBeyondHorizon},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Check(tc.spec, tc.allow); !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
				t.Fatalf("Check = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestConsumerSpecAndModes(t *testing.T) {
	if err := (ConsumerSpec{Name: "Bad Name", Mode: ModeHistorical, Effect: EffectLocal}).Validate(); err == nil {
		t.Error("bad name accepted")
	}
	if err := (ConsumerSpec{Name: "x", Effect: EffectLocal}).Validate(); err == nil {
		t.Error("missing mode accepted")
	}
	if err := (ConsumerSpec{Name: "x", Mode: ModeHistorical, Effect: EffectExternalIntent, IdempotentBeyondHorizon: true}).Validate(); err == nil {
		t.Error("external intent cannot be idempotent beyond the horizon")
	}
	if ModeCurrentState.Observes(7, 5) || !ModeCurrentState.Observes(5, 7) {
		t.Error("current-state mode must never regress")
	}
	if !ModeHistorical.Observes(7, 5) || !ModeHistorical.Observes(7, 7) {
		t.Error("historical mode processes every event")
	}
	spec := ConsumerSpec{Name: "x", Mode: ModeHistorical, Effect: EffectLocal, Types: []string{"run.created"}}
	if !spec.Accepts("run.created") || spec.Accepts("gate.decided") {
		t.Error("type filter")
	}
}

func TestRetentionPolicyRelations(t *testing.T) {
	good := RetentionPolicy{PublishedOutbox: time.Hour, Observation: 48 * time.Hour, Inbox: 48 * time.Hour, ResolvedQuarantine: time.Hour, CompletedReplay: time.Hour,
		BrokerRedelivery: 24 * time.Hour, ReplayWindow: 24 * time.Hour, MaxClockSkew: time.Minute, BatchSize: 10}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*RetentionPolicy){
		"unset field":                     func(p *RetentionPolicy) { p.PublishedOutbox = 0 },
		"inbox shorter than redelivery":   func(p *RetentionPolicy) { p.Inbox = time.Hour },
		"observation shorter than replay": func(p *RetentionPolicy) { p.Observation = time.Hour },
		"inbox shorter than replay":       func(p *RetentionPolicy) { p.BrokerRedelivery = time.Minute; p.Inbox = time.Hour },
		"no batch":                        func(p *RetentionPolicy) { p.BatchSize = 0 },
		"no clock skew bound":             func(p *RetentionPolicy) { p.MaxClockSkew = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := good
			mutate(&p)
			if err := p.Validate(); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	if err := (BacklogLimits{}).Validate(); err == nil {
		t.Error("zero backlog limits accepted")
	}
}

func TestSubjects(t *testing.T) {
	token := "a1b2c3d4e5f6a7b8c9d0e1f2"
	s, err := Subject("t005x", token, "work_item.transitioned")
	if err != nil || s != "t005x.factory.v1.tenant."+token+".work_item.transitioned" {
		t.Fatalf("Subject = %q, %v", s, err)
	}
	gotToken, gotType, err := ParseSubject("t005x", s)
	if err != nil || gotToken != token || gotType != "work_item.transitioned" {
		t.Fatalf("ParseSubject = %q %q %v", gotToken, gotType, err)
	}
	for _, bad := range []string{"t_acme", "A1B2C3D4E5F6A7B8C9D0E1F2", "a1b2c3d4e5f6a7b8.*", "a1b2c3d4e5f6a7b8.c", "a1b2c3d4e5f6a7b8>", "short", ""} {
		if _, err := Subject("", bad, "run.created"); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	if _, err := Subject("", token, "run.exploded"); err == nil {
		t.Error("unknown type accepted")
	}
	if _, _, err := ParseSubject("t005x", "other.factory.v1.tenant."+token+".run.created"); err == nil {
		t.Error("foreign namespace accepted")
	}
	if _, _, err := ParseSubject("", "factory.v1.tenant.UPPERCASETOKENUPPERCASE.run.created"); err == nil {
		t.Error("unsafe token accepted")
	}
}

func TestClassification(t *testing.T) {
	if ClassOf(errors.New("plain")) != "" || ClassOrTransient(errors.New("plain")) != ClassTransient {
		t.Error("unclassified errors default to transient only through ClassOrTransient")
	}
	if ClassOf(Permanent(errors.New("x"))) != ClassPermanent || ClassOf(Transient(errors.New("x"))) != ClassTransient {
		t.Error("explicit classes")
	}
	if got := len(BoundedMessage(strings.Repeat("a", 5000))); got != MaxFailureMessage {
		t.Errorf("bounded message length %d", got)
	}
}

func TestSafeText(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      "plain",
		"bad\x00typ": "bad\uFFFDtyp",
		"x\xffy":     "x\uFFFDy",
		"héllo":      "héllo",
	} {
		if got := SafeText(in); got != want {
			t.Errorf("SafeText(%q) = %q, want %q", in, got, want)
		}
		if IsSafeText(in) != (in == want) {
			t.Errorf("IsSafeText(%q) disagrees with SafeText", in)
		}
	}
	if got := len(BoundedMessage(strings.Repeat("\x00", 5000))); got > MaxFailureMessage {
		t.Errorf("sanitized message exceeds the bound: %d", got)
	}
	if got := BoundedMessage("a\x00"); got != "a\uFFFD" {
		t.Errorf("BoundedMessage must sanitize: %q", got)
	}
}
