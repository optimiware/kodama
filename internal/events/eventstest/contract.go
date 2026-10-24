package eventstest

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// The tenants and projects every Open implementation seeds.
const (
	TenantA  domain.TenantID  = "t088_acme"
	ProjectA domain.ProjectID = "p088_web"
	TenantB  domain.TenantID  = "t088_globex"
	ProjectB domain.ProjectID = "p088_ops"

	// OperatorID is the one operator the adapter's authorizer accepts for replay.
	OperatorID = "ops_t088"
	// ConsumerName is the consumer the contract registers.
	ConsumerName = "ledger"
)

// ScopeA is the project scope of TenantA's events.
var ScopeA = events.Scope{TenantID: TenantA, ProjectID: ProjectA}

// Unit is what a producer sees inside one tenant transaction: the event
// append joined to its own writes, and a marker row standing for the
// producer's (or a consumer's) local state.
type Unit interface {
	Append(ctx context.Context, env eventsv1.Envelope) (AppendResult, error)
	// Note writes a marker that commits or rolls back with the transaction.
	Note(ctx context.Context, key string) error
	Watermark(ctx context.Context, scope events.Scope) (int64, error)
	// Observations returns the retained payloads of the scope after a sequence.
	Observations(ctx context.Context, scope events.Scope, after int64, limit int) ([][]byte, error)
}

// Consumer is a consumer the adapter under contract must register: its local
// effect receives the consumer's own Unit, so Note is atomic with the inbox.
type Consumer struct {
	Spec   events.ConsumerSpec
	Handle func(ctx context.Context, env eventsv1.Envelope, u Unit) error
}

// Bus is the event bus under contract: the outbox publication store, the inbox
// processor, subject resolution, tenant enumeration and replay, plus the
// transaction the producer's append joins.
type Bus interface {
	events.PublicationStore
	events.StatsReader
	events.TenantSource
	events.SubjectResolver
	events.Processor

	// Update runs fn in one tenant transaction; a nil return commits everything
	// fn wrote, any error discards all of it.
	Update(ctx context.Context, tenant domain.TenantID, fn func(u Unit) error) error
	// Notes lists the tenant's committed markers in key order.
	Notes(ctx context.Context, tenant domain.TenantID) ([]string, error)
	DryRun(ctx context.Context, req events.ReplayRequest) (events.ReplayClassification, error)
	// Replay requests a replay and runs it to completion.
	Replay(ctx context.Context, req events.ReplayRequest) (ReplayResult, error)
}

// Open builds an empty bus with TenantA/ProjectA and TenantB/ProjectB seeded,
// the consumers registered, OperatorID authorized for replay, retry policy
// DefaultOptions().Retry and clock as its only source of time.
type Open func(t testing.TB, clock *Clock, consumers ...Consumer) Bus

var retryPolicy = DefaultOptions().Retry

// Contract is the behavior every events.Bus adapter shares: atomic append,
// idempotent identity, tenant scoping, fenced publication claims, an inbox that
// runs an effect once, durable quarantine and authorized replay. It asserts
// outcomes, not lock order or isolation levels. The memory adapter runs it
// without a service; the PostgreSQL adapter runs it in the `test` environment.
func Contract(t *testing.T, open Open) {
	t.Helper()
	ctx := context.Background()

	var failPermanent, failTransient atomic.Bool
	ledger := Consumer{
		Spec: events.ConsumerSpec{Name: ConsumerName, Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, env eventsv1.Envelope, u Unit) error {
			if err := u.Note(ctx, "ledger:"+env.ID); err != nil {
				return err
			}
			switch {
			case failPermanent.Load():
				return events.Permanent(errors.New("cannot apply"))
			case failTransient.Load():
				return events.Transient(errors.New("downstream unavailable"))
			}
			return nil
		},
	}
	fresh := func(t *testing.T) (Bus, *Clock) {
		t.Helper()
		failPermanent.Store(false)
		failTransient.Store(false)
		clock := &Clock{}
		return open(t, clock, ledger), clock
	}
	envA := func(id string, version int64) eventsv1.Envelope {
		return WorkItemTransition(string(TenantA), string(ProjectA), id, "wi_"+id, version)
	}
	envB := func(id string, version int64) eventsv1.Envelope {
		return WorkItemTransition(string(TenantB), string(ProjectB), id, "wi_"+id, version)
	}
	appendOne := func(t *testing.T, bus Bus, env eventsv1.Envelope) AppendResult {
		t.Helper()
		var res AppendResult
		if err := bus.Update(ctx, env.TenantID, func(u Unit) (err error) { res, err = u.Append(ctx, env); return err }); err != nil {
			t.Fatalf("append %s: %v", env.ID, err)
		}
		return res
	}
	claimAll := func(t *testing.T, bus Bus, tenant domain.TenantID, now time.Time) []events.Claim {
		t.Helper()
		claims, err := bus.ClaimDue(ctx, tenant, now, time.Minute, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return claims
	}
	t.Run("an event commits with the update that wrote it", func(t *testing.T) {
		bus, clock := fresh(t)
		if err := bus.Update(ctx, TenantA, func(u Unit) error {
			if err := u.Note(ctx, "state:wi_1"); err != nil {
				return err
			}
			_, err := u.Append(ctx, envA("evt_c1", 1))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 1 || notes[0] != "state:wi_1" {
			t.Fatalf("notes after commit: %v", notes)
		}
		claims := claimAll(t, bus, TenantA, clock.Now())
		if len(claims) != 1 || claims[0].EventID != "evt_c1" || claims[0].Attempt != 1 || claims[0].Token < 1 {
			t.Fatalf("claims: %+v", claims)
		}
		if claims[0].EventType != eventsv1.TypeWorkItemTransitioned || events.ValidateSubjectToken(claims[0].SubjectToken) != nil {
			t.Fatalf("claim identity: %+v", claims[0])
		}
		env, err := eventsv1.Decode(claims[0].Payload)
		if err != nil || env.ID != "evt_c1" || env.TenantID != TenantA {
			t.Fatalf("claimed payload: %+v %v", env, err)
		}
		var high int64
		if err := bus.Update(ctx, TenantA, func(u Unit) (err error) { high, err = u.Watermark(ctx, ScopeA); return err }); err != nil || high != 1 {
			t.Fatalf("watermark %d: %v", high, err)
		}
	})

	t.Run("an event rolls back with the update that wrote it", func(t *testing.T) {
		bus, clock := fresh(t)
		boom := errors.New("producer failed after the append")
		err := bus.Update(ctx, TenantA, func(u Unit) error {
			if err := u.Note(ctx, "state:wi_2"); err != nil {
				return err
			}
			if _, err := u.Append(ctx, envA("evt_r1", 1)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("update: %v", err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 0 {
			t.Fatalf("a rolled-back update left state behind: %v", notes)
		}
		if claims := claimAll(t, bus, TenantA, clock.Now().Add(time.Hour)); len(claims) != 0 {
			t.Fatalf("a rolled-back update left a publication: %+v", claims)
		}
		st, err := bus.PublicationStats(ctx, TenantA, clock.Now())
		if err != nil || st.Pending != 0 || st.Claimed != 0 {
			t.Fatalf("stats after rollback: %+v %v", st, err)
		}
		// The identity is free again: the same event can be appended later.
		if res := appendOne(t, bus, envA("evt_r1", 1)); res.Duplicate || res.Sequence != 1 {
			t.Fatalf("append after rollback: %+v", res)
		}
	})

	t.Run("returned and supplied buffers are copies of the retained events", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_b1", 1))
		first := claimAll(t, bus, TenantA, clock.Now())
		if len(first) != 1 {
			t.Fatalf("claims: %+v", first)
		}
		want := append([]byte(nil), first[0].Payload...)
		scribble := func(b []byte) {
			for i := range b {
				b[i] = 'x'
			}
		}
		scribble(first[0].Payload)
		observations := func() [][]byte {
			t.Helper()
			var out [][]byte
			if err := bus.Update(ctx, TenantA, func(u Unit) (err error) { out, err = u.Observations(ctx, ScopeA, 0, 10); return err }); err != nil {
				t.Fatalf("observations: %v", err)
			}
			return out
		}
		obs := observations()
		if len(obs) != 1 || !bytes.Equal(obs[0], want) {
			t.Fatalf("a claim's buffer reached the retained observation: %q", obs)
		}
		scribble(obs[0])
		if again := observations(); len(again) != 1 || !bytes.Equal(again[0], want) {
			t.Fatalf("an observation's buffer reached the retained observation: %q", again)
		}
		// The outbox row is intact too: the claim, after its lease, carries the original bytes.
		if err := bus.Fail(ctx, first[0], events.ClassTransient, "retry", clock.Now()); err != nil {
			t.Fatal(err)
		}
		later := claimAll(t, bus, TenantA, clock.Now().Add(time.Hour))
		if len(later) != 1 || !bytes.Equal(later[0].Payload, want) {
			t.Fatalf("a scribbled buffer reached the outbox: %+v", later)
		}
		if env, err := eventsv1.Decode(later[0].Payload); err != nil || env.ID != "evt_b1" {
			t.Fatalf("outbox payload: %+v %v", env, err)
		}
		// A delivery buffer the caller keeps using changes nothing already retained.
		failTransient.Store(true)
		supplied := append([]byte(nil), want...)
		if out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: supplied}); err != nil || out.Kind != events.OutcomeRetry {
			t.Fatalf("delivery: %+v %v", out, err)
		}
		scribble(supplied)
		if again := observations(); len(again) != 1 || !bytes.Equal(again[0], want) {
			t.Fatalf("a delivery buffer reached the retained observation: %q", again)
		}
	})

	t.Run("append is idempotent and refuses changed content", func(t *testing.T) {
		bus, clock := fresh(t)
		first := appendOne(t, bus, envA("evt_i1", 1))
		again := appendOne(t, bus, envA("evt_i1", 1))
		if first.Duplicate || !again.Duplicate || again.Sequence != first.Sequence || again.Digest != first.Digest {
			t.Fatalf("first %+v, repeat %+v", first, again)
		}
		err := bus.Update(ctx, TenantA, func(u Unit) error { _, err := u.Append(ctx, envA("evt_i1", 2)); return err })
		if !errors.Is(err, events.ErrConflict) {
			t.Fatalf("changed content under the same ID: %v", err)
		}
		if second := appendOne(t, bus, envA("evt_i2", 2)); second.Sequence != first.Sequence+1 {
			t.Fatalf("sequences are gapless per scope: %d then %d", first.Sequence, second.Sequence)
		}
		if claims := claimAll(t, bus, TenantA, clock.Now()); len(claims) != 2 {
			t.Fatalf("a repeated append must not add a publication: %d claims", len(claims))
		}
	})

	t.Run("a tenant cannot append or claim another tenant's events", func(t *testing.T) {
		bus, clock := fresh(t)
		err := bus.Update(ctx, TenantA, func(u Unit) error { _, err := u.Append(ctx, envB("evt_x1", 1)); return err })
		if !errors.Is(err, events.ErrScopeMismatch) {
			t.Fatalf("a transaction of tenant A appended an event of tenant B: %v", err)
		}
		appendOne(t, bus, envA("evt_x2", 1))
		appendOne(t, bus, envB("evt_x3", 1))
		a, b := claimAll(t, bus, TenantA, clock.Now()), claimAll(t, bus, TenantB, clock.Now())
		if len(a) != 1 || a[0].EventID != "evt_x2" || len(b) != 1 || b[0].EventID != "evt_x3" {
			t.Fatalf("claims: A %+v, B %+v", a, b)
		}
		if a[0].SubjectToken == b[0].SubjectToken {
			t.Fatal("tenants share a subject token")
		}
		for want, token := range map[domain.TenantID]string{TenantA: a[0].SubjectToken, TenantB: b[0].SubjectToken} {
			got, err := bus.ResolveSubjectToken(ctx, token)
			if err != nil || got != want {
				t.Fatalf("token of %s resolved to %q: %v", want, got, err)
			}
		}
		for _, bad := range []string{"0123456789abcdef0123456789abcdef", "NOT A TOKEN", ""} {
			if _, err := bus.ResolveSubjectToken(ctx, bad); !errors.Is(err, events.ErrUnknownSubject) {
				t.Fatalf("token %q: %v", bad, err)
			}
		}
		tenants, err := bus.TenantIDs(ctx)
		if err != nil || !containsTenant(tenants, TenantA) || !containsTenant(tenants, TenantB) {
			t.Fatalf("tenants: %v %v", tenants, err)
		}
	})

	t.Run("claims are leased and fenced by a token", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_l1", 1))
		now := clock.Now()
		first := claimAll(t, bus, TenantA, now)
		if len(first) != 1 {
			t.Fatalf("first claim: %+v", first)
		}
		if again := claimAll(t, bus, TenantA, now); len(again) != 0 {
			t.Fatalf("a leased row was claimed twice: %+v", again)
		}
		st, _ := bus.PublicationStats(ctx, TenantA, now)
		if st.Claimed != 1 || st.Pending != 0 || st.BacklogBytes == 0 {
			t.Fatalf("stats while claimed: %+v", st)
		}
		later := now.Add(2 * time.Minute)
		second := claimAll(t, bus, TenantA, later)
		if len(second) != 1 || second[0].Token <= first[0].Token || second[0].Attempt != 2 {
			t.Fatalf("reclaim after the lease: %+v then %+v", first, second)
		}
		if err := bus.Complete(ctx, first[0], later); !errors.Is(err, events.ErrStaleClaim) {
			t.Fatalf("a stale holder completed the row: %v", err)
		}
		if err := bus.Complete(ctx, second[0], later); err != nil {
			t.Fatal(err)
		}
		if err := bus.Complete(ctx, second[0], later); err == nil {
			t.Fatal("a published row was completed twice")
		}
		if after := claimAll(t, bus, TenantA, later.Add(time.Hour)); len(after) != 0 {
			t.Fatalf("a published row was claimed: %+v", after)
		}
		if st, _ := bus.PublicationStats(ctx, TenantA, later); st.Pending != 0 || st.Claimed != 0 || st.BacklogBytes != 0 {
			t.Fatalf("stats after publication: %+v", st)
		}
	})

	t.Run("a classified failure schedules a retry", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_f1", 1))
		now := clock.Now()
		c := claimAll(t, bus, TenantA, now)
		if err := bus.Fail(ctx, c[0], events.ClassTransient, "broker unavailable", now); err != nil {
			t.Fatal(err)
		}
		if err := bus.Fail(ctx, c[0], events.ClassTransient, "again", now); !errors.Is(err, events.ErrStaleClaim) && !errors.Is(err, events.ErrInvalidTransition) {
			t.Fatalf("a released claim was failed again: %v", err)
		}
		if early := claimAll(t, bus, TenantA, now); len(early) != 0 {
			t.Fatalf("a retry scheduled in the future was claimed now: %+v", early)
		}
		st, _ := bus.PublicationStats(ctx, TenantA, now)
		if st.Pending != 1 || st.LastClass != events.ClassTransient || st.LastError != "broker unavailable" || st.Attempts != 1 {
			t.Fatalf("stats after failure: %+v", st)
		}
		due := claimAll(t, bus, TenantA, now.Add(retryPolicy.Max+time.Second))
		if len(due) != 1 || due[0].Attempt != 2 || due[0].Token <= c[0].Token {
			t.Fatalf("retry: %+v", due)
		}
		if err := bus.Fail(ctx, due[0], events.FailureClass(""), "no class", now); !errors.Is(err, events.ErrUnclassified) {
			t.Fatalf("an unclassified failure was recorded: %v", err)
		}
	})

	t.Run("a delivery runs its effect once", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_d1", 1))
		c := claimAll(t, bus, TenantA, clock.Now())[0]
		d := events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload}
		out, err := bus.Process(ctx, d)
		if err != nil || out.Kind != events.OutcomeCompleted {
			t.Fatalf("first delivery: %+v %v", out, err)
		}
		for i := 0; i < 2; i++ {
			out, err = bus.Process(ctx, d)
			if err != nil || out.Kind != events.OutcomeDuplicate {
				t.Fatalf("redelivery %d: %+v %v", i, out, err)
			}
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 1 || notes[0] != "ledger:evt_d1" {
			t.Fatalf("the effect ran %d times: %v", len(notes), notes)
		}
		// A consumer that is not registered is a configuration error, never a pass.
		if _, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: "unknown", Payload: c.Payload}); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Fatalf("unregistered consumer: %v", err)
		}
	})

	t.Run("a delivery is processed under the trusted tenant only", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envB("evt_t1", 1))
		c := claimAll(t, bus, TenantB, clock.Now())[0]
		out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload})
		if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != ReasonScopeMismatch || out.QuarantineID == "" {
			t.Fatalf("an envelope of tenant B delivered under tenant A: %+v %v", out, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 0 {
			t.Fatalf("a foreign envelope ran an effect: %v", notes)
		}
		// Redelivering the same poison is idempotent: it stays one decision.
		again, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload})
		if err != nil || again.Kind != events.OutcomeQuarantined || again.QuarantineID != out.QuarantineID {
			t.Fatalf("redelivered poison: %+v %v", again, err)
		}
		bad, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: []byte(`{"id":"evt_garbage"`)})
		if err != nil || bad.Kind != events.OutcomeQuarantined || bad.Reason != ReasonMalformed {
			t.Fatalf("malformed delivery: %+v %v", bad, err)
		}
		if notes, _ := bus.Notes(ctx, TenantB); len(notes) != 0 {
			t.Fatalf("tenant B saw an effect: %v", notes)
		}
	})

	t.Run("a permanent failure quarantines and discards the effect", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_p1", 1))
		c := claimAll(t, bus, TenantA, clock.Now())[0]
		failPermanent.Store(true)
		out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload})
		if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != ReasonPermanentFailure || out.QuarantineID == "" {
			t.Fatalf("permanent failure: %+v %v", out, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 0 {
			t.Fatalf("a failed handler left its effect behind: %v", notes)
		}
		failPermanent.Store(false)
		again, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload})
		if err != nil || again.Kind != events.OutcomeQuarantined {
			t.Fatalf("a quarantined event ran without a replay: %+v %v", again, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 0 {
			t.Fatalf("ordinary redelivery reopened a quarantined event: %v", notes)
		}
	})

	t.Run("a transient failure backs off, retries and then quarantines", func(t *testing.T) {
		bus, clock := fresh(t)
		appendOne(t, bus, envA("evt_t2", 1))
		c := claimAll(t, bus, TenantA, clock.Now())[0]
		d := events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: c.Payload}
		failTransient.Store(true)
		out, err := bus.Process(ctx, d)
		if err != nil || out.Kind != events.OutcomeRetry || out.RetryAfter != retryPolicy.Delay(1) {
			t.Fatalf("attempt 1: %+v %v", out, err)
		}
		// During backoff a duplicate neither runs the handler nor spends an
		// attempt. The clock steps back so the verdict never depends on how fast
		// the adapter answered.
		clock.Advance(-time.Hour)
		if out, err = bus.Process(ctx, d); err != nil || out.Kind != events.OutcomeRetry {
			t.Fatalf("duplicate during backoff: %+v %v", out, err)
		}
		clock.Advance(time.Hour + time.Second)
		if out, err = bus.Process(ctx, d); err != nil || out.Kind != events.OutcomeRetry || out.RetryAfter != retryPolicy.Delay(2) {
			t.Fatalf("attempt 2: %+v %v", out, err)
		}
		clock.Advance(time.Second)
		if out, err = bus.Process(ctx, d); err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != ReasonRetriesExhausted {
			t.Fatalf("attempt %d: %+v %v", retryPolicy.MaxAttempts, out, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 0 {
			t.Fatalf("failed attempts left effects: %v", notes)
		}
		// A recovered dependency completes a fresh event on the first attempt.
		failTransient.Store(false)
		appendOne(t, bus, envA("evt_t3", 2))
		var fresh events.Claim
		for _, cl := range claimAll(t, bus, TenantA, clock.Now()) {
			if cl.EventID == "evt_t3" {
				fresh = cl
			}
		}
		if out, err = bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: fresh.Payload}); err != nil || out.Kind != events.OutcomeCompleted {
			t.Fatalf("recovered delivery: %+v %v", out, err)
		}
	})

	t.Run("replay reopens quarantine once and never repeats a completed effect", func(t *testing.T) {
		bus, clock := fresh(t)
		first, second := appendOne(t, bus, envA("evt_y1", 1)), appendOne(t, bus, envA("evt_y2", 2))
		if first.Sequence != 1 || second.Sequence != 2 {
			t.Fatalf("sequences %d, %d", first.Sequence, second.Sequence)
		}
		payloads := map[string][]byte{}
		for _, c := range claimAll(t, bus, TenantA, clock.Now()) {
			payloads[c.EventID] = c.Payload
		}
		// evt_y1 completes; evt_y2 hits a permanent failure and is quarantined.
		if out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: payloads["evt_y1"]}); err != nil || out.Kind != events.OutcomeCompleted {
			t.Fatalf("evt_y1: %+v %v", out, err)
		}
		failPermanent.Store(true)
		if out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: payloads["evt_y2"]}); err != nil || out.Kind != events.OutcomeQuarantined {
			t.Fatalf("evt_y2: %+v %v", out, err)
		}
		failPermanent.Store(false)

		req := events.ReplayRequest{Scope: ScopeA, Consumer: ConsumerName, FromSeq: 1, ToSeq: 2, Reason: "dependency repaired", Operator: events.Operator{ID: OperatorID}}
		class, err := bus.DryRun(ctx, req)
		if err != nil || class.Events != 2 || class.AlreadyCompleted != 1 || class.Quarantined != 1 || class.Effect != events.EffectLocal {
			t.Fatalf("dry run: %+v %v", class, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 1 {
			t.Fatalf("a dry run changed state: %v", notes)
		}

		stranger := req
		stranger.Operator = events.Operator{ID: "usr_nobody"}
		if _, err := bus.Replay(ctx, stranger); !errors.Is(err, events.ErrUnauthorized) {
			t.Fatalf("an unauthorized operator replayed: %v", err)
		}
		beyond := req
		beyond.ToSeq = 9
		if _, err := bus.Replay(ctx, beyond); !errors.Is(err, events.ErrReplayUnsafe) {
			t.Fatalf("a range past the scope's last event: %v", err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 1 {
			t.Fatalf("a refused replay changed state: %v", notes)
		}

		res, err := bus.Replay(ctx, req)
		if err != nil || res.State != "completed" || res.Processed != 1 || res.Skipped != 1 || res.Failed != 0 {
			t.Fatalf("replay: %+v %v", res, err)
		}
		notes, _ := bus.Notes(ctx, TenantA)
		if len(notes) != 2 || notes[0] != "ledger:evt_y1" || notes[1] != "ledger:evt_y2" {
			t.Fatalf("effects after replay: %v", notes)
		}
		// Replaying again changes nothing: both events are completed now.
		res, err = bus.Replay(ctx, req)
		if err != nil || res.Processed != 0 || res.Skipped != 2 {
			t.Fatalf("second replay: %+v %v", res, err)
		}
		if notes, _ := bus.Notes(ctx, TenantA); len(notes) != 2 {
			t.Fatalf("a repeated replay repeated an effect: %v", notes)
		}
		// The poison event completes as an ordinary duplicate afterwards.
		if out, err := bus.Process(ctx, events.Delivery{Tenant: TenantA, Consumer: ConsumerName, Payload: payloads["evt_y2"]}); err != nil || out.Kind != events.OutcomeDuplicate {
			t.Fatalf("delivery after replay: %+v %v", out, err)
		}
	})
}

func containsTenant(list []domain.TenantID, t domain.TenantID) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}
