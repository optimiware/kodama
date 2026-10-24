package chatops_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// memWork is an in-memory WorkStore over the Delivery entity, so the worker's
// handling of a sender can be tested without services. Claim and Finish follow
// the entity's transitions and fencing exactly; the PostgreSQL adapter's own
// semantics are covered by its real-database tests.
type memWork struct {
	mu       sync.Mutex
	delivery chatops.Delivery
	dest     chatops.Destination
}

func (m *memWork) ClaimDue(_ context.Context, _ domain.TenantID, now time.Time, lease time.Duration, _ int, p chatops.RetryPolicy) (chatops.ClaimBatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.delivery.Due(now) {
		return chatops.ClaimBatch{}, nil
	}
	res, err := m.delivery.Claim(now, lease, p)
	if err != nil || !res.Claimed {
		return chatops.ClaimBatch{}, err
	}
	return chatops.ClaimBatch{Claims: []chatops.Claim{{
		Delivery: m.delivery, Destination: m.dest, Attempt: m.delivery.Attempts,
		Token: m.delivery.ClaimToken, LeaseExpiresAt: m.delivery.LeaseExpiresAt,
	}}}, nil
}

func (m *memWork) Finish(_ context.Context, c chatops.Claim, o chatops.Outcome, now time.Time, p chatops.RetryPolicy) (chatops.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.Delivered {
		return m.delivery, m.delivery.Succeed(c.Token, now, o)
	}
	_, err := m.delivery.Fail(c.Token, now, o, p)
	return m.delivery, err
}

type oneTenant struct{}

func (oneTenant) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{scope.TenantID}, nil
}

// effectSender records an external effect for every request and then panics on
// the first call, as a sender that reached the receiver before failing would.
type effectSender struct {
	mu       sync.Mutex
	requests []chatops.SendRequest
}

func (s *effectSender) Send(_ context.Context, r chatops.SendRequest) chatops.Outcome {
	s.mu.Lock()
	s.requests = append(s.requests, r)
	n := len(s.requests)
	s.mu.Unlock()
	if n == 1 {
		panic("receiver already processed " + string(r.Payload) + " with secret-token-9")
	}
	return chatops.Outcome{Delivered: true, HTTPStatus: 200}
}

// TestWorkerPanicIsUncertain: a panicking sender may already have had an
// external effect, so the failure is recorded as an unknown outcome with a
// bounded class and no panic text; the retry carries the exact payload and key
// and success is only recorded when the sender reports it.
func TestWorkerPanicIsUncertain(t *testing.T) {
	d := pending(t)
	store := &memWork{delivery: d, dest: destination(t)}
	sender := &effectSender{}
	now := t0
	w, err := chatops.NewWorker(chatops.WorkerConfig{
		Work: store, Tenants: oneTenant{}, Sender: sender, Retry: retry,
		Interval: time.Hour, Batch: 1, Lease: 30 * time.Second, AttemptTimeout: 10 * time.Second, Concurrency: 1,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 1 || res.Retried != 1 || res.Delivered != 0 || res.Failed != 0 {
		t.Fatalf("first pass = %+v", res)
	}
	got := store.delivery
	if got.State != chatops.StatePending || got.LastClass != chatops.ClassInternal || !got.LastUncertain || got.Receipt != "" || !got.DeliveredAt.IsZero() {
		t.Fatalf("after the panic = %+v", got)
	}
	for _, field := range []string{string(got.LastClass), got.Receipt, string(got.FailedReason)} {
		if strings.Contains(field, "secret-token-9") || strings.Contains(field, "receiver already") {
			t.Fatalf("panic text retained: %q", field)
		}
	}

	now = got.NextAttemptAt
	res, err = w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered != 1 {
		t.Fatalf("retry pass = %+v", res)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("requests = %d", len(sender.requests))
	}
	a, b := sender.requests[0], sender.requests[1]
	if string(a.Payload) != string(b.Payload) || a.IdempotencyKey != b.IdempotencyKey || a.DeliveryID != b.DeliveryID || a.IdempotencyKey != string(d.ID) {
		t.Fatalf("retry identity changed: %+v %+v", a, b)
	}
	if a.Attempt != 1 || b.Attempt != 2 {
		t.Fatalf("attempt numbers = %d %d", a.Attempt, b.Attempt)
	}
	if store.delivery.State != chatops.StateDelivered || store.delivery.LastUncertain {
		t.Fatalf("final = %+v", store.delivery)
	}
}
