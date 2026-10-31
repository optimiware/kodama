package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/outbox"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestConnectBusWithoutNATSDeliversInProcessAndSaysSo(t *testing.T) {
	var logs bytes.Buffer
	b, closeBus, err := connectBus(t.Context(), Config{}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBus()
	if _, ok := b.(*bus.Memory); !ok {
		t.Fatalf("bus = %T, want the in-process bus", b)
	}
	if n := strings.Count(logs.String(), "delivered in process"); n != 1 {
		t.Fatalf("the in-process delivery was logged %d times:\n%s", n, logs.String())
	}
}

func TestConnectBusFailsOnAnUnusableNATSURL(t *testing.T) {
	for name, url := range map[string]string{
		"nothing listens": "nats://127.0.0.1:1",
		"wrong scheme":    "http://127.0.0.1:4222",
		"no host":         "nats://",
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		b, closeBus, err := connectBus(ctx, Config{NATSURL: url}, slog.New(slog.DiscardHandler))
		cancel()
		if err == nil {
			closeBus()
			t.Errorf("%s: connected, got %T", name, b)
		}
	}
	if err := ValidateNATSURL(""); err != nil {
		t.Fatal(err)
	}
}

func TestConnectBusValidatesTheProfileBeforeAnythingElse(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	dev, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development})
	if err != nil {
		t.Fatal(err)
	}
	jetstream := &natsbus.JetStream{} // a verified JetStream adapter; never used here
	verifiedBroker := &natsbus.Broker{Bus: jetstream}

	// An unknown environment stops startup whatever is injected or configured.
	for _, env := range []string{"prod", "Production", "staging", " test"} {
		for name, cfg := range map[string]Config{
			"nothing":   {Env: env},
			"memory":    {Env: env, Bus: bus.NewMemory()},
			"url":       {Env: env, NATSURL: "nats://127.0.0.1:1"},
			"jetstream": {Env: env, Bus: jetstream},
		} {
			if b, _, err := connectBus(t.Context(), cfg, quiet); !errors.Is(err, natsbus.ErrProfileUnknown) {
				t.Errorf("env %q with %s = %T, %v; want ErrProfileUnknown", env, name, b, err)
			}
		}
	}

	// Deployed profiles refuse every injected bus that is not a verified JetStream
	// adapter: the in-process bus, the development broker wrapper, a development
	// flagged adapter, and an unknown implementation.
	for _, env := range []string{"test", "production"} {
		for name, injected := range map[string]bus.Bus{
			"memory":             bus.NewMemory(),
			"development broker": dev,
			"flagged adapter":    flaggedBus{bus.NewMemory()},
			"unknown":            plainBus{bus.NewMemory()},
			"empty broker":       &natsbus.Broker{Bus: bus.NewMemory()},
		} {
			if b, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet); !errors.Is(err, natsbus.ErrBrokerRequired) {
				t.Errorf("%s injected in %s = %T, %v; want ErrBrokerRequired", name, env, b, err)
			}
		}
		for name, injected := range map[string]bus.Bus{"jetstream": jetstream, "broker over jetstream": verifiedBroker} {
			if _, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet); err != nil {
				t.Errorf("%s injected in %s: %v", name, env, err)
			}
		}
		// The normal broker startup failures.
		if b, _, err := connectBus(t.Context(), Config{Env: env}, quiet); !errors.Is(err, natsbus.ErrBrokerRequired) {
			t.Errorf("%s without NATS = %T, %v; want ErrBrokerRequired", env, b, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		b, _, err := connectBus(ctx, Config{Env: env, NATSURL: "nats://127.0.0.1:1"}, quiet)
		cancel()
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
			t.Errorf("%s with an unreachable NATS = %T, %v; want ErrBrokerUnavailable", env, b, err)
		}
		if _, _, err := connectBus(t.Context(), Config{Env: env, NATSURL: "http://127.0.0.1:4222"}, quiet); err == nil {
			t.Errorf("%s accepted a malformed NATS URL", env)
		}
	}

	// Development accepts an injected bus, including the in-process ones.
	for name, injected := range map[string]bus.Bus{"memory": bus.NewMemory(), "development broker": dev} {
		for _, env := range []string{"", "development"} {
			if b, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet); err != nil || b != injected {
				t.Errorf("%s injected in development %q = %T, %v", name, env, b, err)
			}
		}
	}
}

type flaggedBus struct{ bus.Bus }

func (flaggedBus) DevelopmentOnly() bool { return true }

type plainBus struct{ bus.Bus }

// fakeOutbox is the store's outbox side with PendingEvents ordered by next
// attempt, as the PostgreSQL query orders it.
type fakeOutbox struct {
	events    []store.OutboxEvent
	next      map[string]time.Time
	published map[string]bool
}

func (f *fakeOutbox) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{"t_a"}, nil
}

func (f *fakeOutbox) PendingEvents(_ context.Context, _ domain.TenantID, now time.Time, limit int) ([]store.OutboxEvent, error) {
	var due []store.OutboxEvent
	for _, e := range f.events {
		if !f.published[e.ID] && !f.next[e.ID].After(now) {
			due = append(due, e)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !f.next[due[i].ID].Equal(f.next[due[j].ID]) {
			return f.next[due[i].ID].Before(f.next[due[j].ID])
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, _ domain.TenantID, id string, _ time.Time) error {
	f.published[id] = true
	return nil
}

func (f *fakeOutbox) PublishFailed(_ context.Context, _ domain.TenantID, id string, next time.Time) error {
	f.next[id] = next
	return nil
}

type sinkRecorder struct {
	failFor map[string]bool
	ids     []string
}

func (s *sinkRecorder) Publish(_ context.Context, _ string, m bus.Message) error {
	if s.failFor[m.ID] {
		return errors.New("broker down")
	}
	s.ids = append(s.ids, m.ID)
	return nil
}

// Reconciliation reaches an old deferred event behind a full page of young
// ones, which the store orders first by next attempt, and terminates.
func TestReconcileReachesAStaleDeferredEventBehindYoungOnes(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	f := &fakeOutbox{next: map[string]time.Time{}, published: map[string]bool{}}
	add := func(id string, created, next time.Time) {
		f.events = append(f.events, store.OutboxEvent{TenantID: "t_a", TenantKey: "a", ProjectKey: "p", ProjectID: "p_1", ID: id, Type: "x.y", Payload: []byte(`{}`), CreatedAt: created})
		f.next[id] = next
	}
	// The stale event is old, deferred a day out after repeated failures.
	add("old-deferred", now.Add(-2*time.Hour), now.Add(24*time.Hour))
	// Ten young events, due now, precede it in the store's order.
	for i := range 10 {
		add(fmt.Sprintf("young-%02d", i), now.Add(-time.Second), now)
	}
	sink := &sinkRecorder{}
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: storeOutbox{f}, Bus: sink, Batch: 3, Grace: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var res outbox.Result
	go func() {
		defer close(done)
		res, err = p.Reconcile(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reconciliation did not terminate")
	}
	if err != nil || res.Published != 1 || res.Failed != 0 || len(sink.ids) != 1 || sink.ids[0] != "old-deferred" || !f.published["old-deferred"] {
		t.Fatalf("reconcile = %+v, %v, published %v", res, err, sink.ids)
	}
	for id := range f.published {
		if id != "old-deferred" {
			t.Fatalf("a young event %s was republished by reconciliation", id)
		}
	}

	// Many stale events with failures still terminate and reach every one once.
	g := &fakeOutbox{next: map[string]time.Time{}, published: map[string]bool{}}
	f = g
	for i := range 8 {
		add(fmt.Sprintf("stale-%d", i), now.Add(-time.Duration(10-i)*time.Hour), now.Add(48*time.Hour))
	}
	for i := range 6 {
		add(fmt.Sprintf("fresh-%d", i), now, now)
	}
	sink = &sinkRecorder{failFor: map[string]bool{"stale-1": true, "stale-5": true}}
	p, _ = outbox.NewPublisher(outbox.PublisherConfig{Store: storeOutbox{g}, Bus: sink, Batch: 3, Grace: time.Minute, Now: func() time.Time { return now }})
	res, err = p.Reconcile(t.Context())
	if err != nil || res.Published != 6 || res.Failed != 2 {
		t.Fatalf("second reconcile = %+v, %v (%v)", res, err, sink.ids)
	}
}
