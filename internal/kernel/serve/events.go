package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/bus"
	"github.com/optimiware/kodama/internal/kernel/dispatch"
	"github.com/optimiware/kodama/internal/kernel/store"
)

// EventStore is the persistence of the event path: the outbox the publisher
// drains and what the dispatcher indexes and admits with. *store.Store and
// *store.MemoryGit implement it.
type EventStore interface {
	store.OutboxBackend
	dispatch.Store
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// EventsConfig configures the event path.
type EventsConfig struct {
	Store EventStore
	// Files reads definitions from the tenants' workers.
	Files dispatch.Files
	// Bus carries the events; nil uses the in-process bus (development, or a
	// single process without NATS).
	Bus bus.Bus
	// Interval is the outbox poll period (default 2s).
	Interval time.Duration
	// Wake is called after a dispatch admitted a run.
	Wake func()
	// Now is the publisher's clock (tests); nil uses the system clock.
	Now    func() time.Time
	Logger *slog.Logger
}

// Events is the running event path: the outbox publisher and the dispatcher that
// consumes git.refs_updated from the bus.
type Events struct {
	Publisher  *bus.Publisher
	Dispatcher *dispatch.Dispatcher
	Bus        bus.Bus
	stop       func()
}

// NewEvents builds the publisher over the store's outbox and subscribes the
// dispatcher on the bus. Run drives the publisher.
func NewEvents(ctx context.Context, cfg EventsConfig) (*Events, error) {
	if cfg.Store == nil || cfg.Files == nil {
		return nil, errors.New("kernel: the event path needs a store and a file reader")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	b := cfg.Bus
	if b == nil {
		b = bus.NewMemory()
	}
	pub, err := bus.NewPublisher(bus.PublisherConfig{Outbox: storeOutbox{cfg.Store}, Bus: b, Interval: cfg.Interval, Now: cfg.Now, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	d, err := dispatch.New(dispatch.Config{Store: cfg.Store, Files: cfg.Files, Wake: cfg.Wake, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	stop, err := d.Subscribe(ctx, b)
	if err != nil {
		return nil, err
	}
	return &Events{Publisher: pub, Dispatcher: d, Bus: b, stop: stop}, nil
}

// Run publishes until ctx ends.
func (e *Events) Run(ctx context.Context) { e.Publisher.Run(ctx) }

// Close detaches the dispatcher from the bus.
func (e *Events) Close() {
	if e.stop != nil {
		e.stop()
	}
}

// storeOutbox adapts the store to the publisher's port.
type storeOutbox struct{ st EventStore }

func (o storeOutbox) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	return o.st.TenantIDs(ctx)
}

func (o storeOutbox) Pending(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]bus.Row, error) {
	events, err := o.st.PendingEvents(ctx, tenant, now, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]bus.Row, 0, len(events))
	for _, e := range events {
		rows = append(rows, bus.Row{
			Message: bus.Message{ID: e.ID, TenantID: e.TenantID, ProjectID: e.ProjectID, Type: e.Type, ResourceType: e.ResourceType,
				ResourceID: e.ResourceID, Actor: bus.Actor{Type: e.ActorType, ID: e.ActorID}, OccurredAt: e.CreatedAt.UTC(),
				Data: json.RawMessage(e.Payload)},
			TenantKey: e.TenantKey, ProjectKey: e.ProjectKey, Attempts: e.Attempts,
		})
	}
	return rows, nil
}

func (o storeOutbox) Published(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error {
	return o.st.MarkPublished(ctx, tenant, id, at)
}

func (o storeOutbox) Failed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error {
	return o.st.PublishFailed(ctx, tenant, id, next)
}

// ValidateNATSURL checks a configured NATS URL: nats:// or tls:// with a host,
// optionally carrying the token as its user information. An empty URL is valid
// (in-process delivery).
func ValidateNATSURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("kernel: the NATS URL must be nats://[token@]host[:port] or tls://[token@]host[:port]")
	}
	return nil
}

// connectBus returns the broker the kernel publishes to. Without a NATS URL the
// events are delivered in process, which is logged once; with one, the
// JetStream adapter is used, and a URL that cannot be reached or a server
// without JetStream fails startup: there is no silent fallback. closeFn releases
// the connection.
func connectBus(ctx context.Context, cfg Config, log *slog.Logger) (b bus.Bus, closeFn func(), err error) {
	switch {
	case cfg.Bus != nil:
		return cfg.Bus, func() {}, nil
	case cfg.NATSURL == "":
		log.Info("no NATS URL is configured (KODAMA_NATS_URL): events are delivered in process; the outbox rows remain the durable state")
		return bus.NewMemory(), func() {}, nil
	}
	if err := ValidateNATSURL(cfg.NATSURL); err != nil {
		return nil, nil, err
	}
	opts := []nats.Option{nats.Name("kodama-kernel"), nats.Timeout(5 * time.Second)}
	if cfg.NATSToken != "" {
		opts = append(opts, nats.Token(cfg.NATSToken))
	}
	nc, err := nats.Connect(cfg.NATSURL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("kernel: connect to NATS: %w", err)
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	js, err := bus.NewJetStream(setup, nc, bus.JetStreamConfig{Logger: log})
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("kernel: NATS JetStream is unusable: %w", err)
	}
	log.Info("events are delivered through NATS JetStream")
	return js, nc.Close, nil
}
