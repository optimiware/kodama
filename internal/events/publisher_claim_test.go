package events

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

type scriptedStore struct {
	claims    []Claim
	completed []Claim
	failed    []Claim
}

func (s *scriptedStore) ClaimDue(context.Context, domain.TenantID, time.Time, time.Duration, int) ([]Claim, error) {
	return s.claims, nil
}
func (s *scriptedStore) Complete(_ context.Context, c Claim, _ time.Time) error {
	s.completed = append(s.completed, c)
	return nil
}
func (s *scriptedStore) Fail(_ context.Context, c Claim, _ FailureClass, _ string, _ time.Time) error {
	s.failed = append(s.failed, c)
	return nil
}

type oneTenant struct{}

func (oneTenant) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{"t_1"}, nil
}

// deadlineBroker records each send's context deadline and can block until it ends.
type deadlineBroker struct {
	sent      []string
	deadlines []time.Time
	block     bool
}

func (b *deadlineBroker) Publish(ctx context.Context, m Message) error {
	b.sent = append(b.sent, m.ID)
	d, _ := ctx.Deadline()
	b.deadlines = append(b.deadlines, d)
	if b.block {
		<-ctx.Done()
		return Transient(ctx.Err())
	}
	return nil
}

// TestPublicationClaimLifetime: a claim is honored only until its absolute
// lease expiry. Claims that expired while queued behind others are not sent,
// and no send may outlive the lease, so a publisher's last possible broker
// acceptance precedes any reclaimer's publication.
func TestPublicationClaimLifetime(t *testing.T) {
	now := t0
	cfg := PublisherConfig{Lease: time.Minute, PublishTimeout: 10 * time.Second, BatchSize: 10, Interval: time.Second, Retry: policy, Clock: func() time.Time { return now }}
	claim := func(id string, expires time.Time) Claim {
		return Claim{TenantID: "t_1", EventID: id, Token: 1, Attempt: 1, LeaseExpiresAt: expires}
	}
	run := func(t *testing.T, store *scriptedStore, broker *deadlineBroker) PublishResult {
		t.Helper()
		p, err := NewPublisher(oneTenant{}, store, broker, cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	t.Run("expired claims are not sent and not settled", func(t *testing.T) {
		store := &scriptedStore{claims: []Claim{claim("evt_late", now), claim("evt_later", now.Add(-time.Hour)), claim("evt_ok", now.Add(time.Minute))}}
		broker := &deadlineBroker{}
		res := run(t, store, broker)
		if len(broker.sent) != 1 || broker.sent[0] != "evt_ok" {
			t.Fatalf("sent %v, want only the live claim", broker.sent)
		}
		if res.Claimed != 3 || res.Expired != 2 || res.Published != 1 || res.Failed != 0 {
			t.Fatalf("result: %+v", res)
		}
		if len(store.completed) != 1 || len(store.failed) != 0 {
			t.Fatalf("expired claims must be left to expire: completed=%v failed=%v", store.completed, store.failed)
		}
	})

	t.Run("a send never outlives the lease", func(t *testing.T) {
		store := &scriptedStore{claims: []Claim{claim("evt_tail", now.Add(300*time.Millisecond))}}
		broker := &deadlineBroker{block: true}
		start := time.Now()
		res := run(t, store, broker)
		if res.Failed != 1 || res.Published != 0 {
			t.Fatalf("result: %+v", res)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("the send ran %s past a 300ms lease (publish timeout 10s)", took)
		}
		if limit := time.Now().Add(400 * time.Millisecond); broker.deadlines[0].After(limit) {
			t.Fatalf("send deadline %s is beyond the lease", broker.deadlines[0])
		}
		if len(store.failed) != 1 {
			t.Fatalf("an unknown outcome must stay retryable")
		}
	})

	t.Run("a long lease keeps the publish timeout", func(t *testing.T) {
		store := &scriptedStore{claims: []Claim{claim("evt_long", now.Add(time.Hour))}}
		broker := &deadlineBroker{}
		run(t, store, broker)
		if limit := time.Now().Add(11 * time.Second); broker.deadlines[0].After(limit) {
			t.Fatalf("deadline %s exceeds the publish timeout", broker.deadlines[0])
		}
	})
}
