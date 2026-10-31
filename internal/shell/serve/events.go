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
	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/dispatch"
	"github.com/optimiware/kodama/internal/shell/outbox"
	"github.com/optimiware/kodama/internal/shell/store"
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
	Publisher  *outbox.Publisher
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
	pub, err := outbox.NewPublisher(outbox.PublisherConfig{Store: storeOutbox{cfg.Store}, Bus: b, Interval: cfg.Interval, Now: cfg.Now, Logger: cfg.Logger})
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

// storeOutbox adapts the store to the publisher's outbox.Source port. The rows
// are audit_events rows the store wrote in the same transaction as the state
// change they describe, so this adapter only drains them; it has no Transact.
// A row's id is its audit_events id, stable across every republication; the
// derivation from event type and deduplication key (outbox.EventID, EC-01) moves
// to the store's intent writer.
type storeOutbox struct{ st EventStore }

var _ outbox.Source = storeOutbox{}

func (o storeOutbox) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	return o.st.TenantIDs(ctx)
}

func (o storeOutbox) Pending(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]outbox.Row, error) {
	events, err := o.st.PendingEvents(ctx, tenant, now, limit)
	return rows(events), err
}

// farFuture makes every unpublished event due, whatever its backoff.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Unpublished lists unpublished events regardless of their next attempt for the
// reconciliation pass. The store has no sequence, so the cursor is ignored and
// Seq stays zero; the publisher attempts a row once per pass.
func (o storeOutbox) Unpublished(ctx context.Context, tenant domain.TenantID, cutoff time.Time, _ int64, limit int) ([]outbox.Row, error) {
	events, err := o.st.PendingEvents(ctx, tenant, farFuture, limit)
	if err != nil {
		return nil, err
	}
	old := events[:0:0]
	for _, e := range events {
		if !e.CreatedAt.After(cutoff) {
			old = append(old, e)
		}
	}
	return rows(old), nil
}

func rows(events []store.OutboxEvent) []outbox.Row {
	out := make([]outbox.Row, 0, len(events))
	for _, e := range events {
		out = append(out, outbox.Row{
			Message: bus.Message{ID: e.ID, TenantID: e.TenantID, ProjectID: e.ProjectID, Type: e.Type, ResourceType: e.ResourceType,
				ResourceID: e.ResourceID, Actor: bus.Actor{Type: e.ActorType, ID: e.ActorID}, OccurredAt: e.CreatedAt.UTC(),
				Data: json.RawMessage(e.Payload)},
			TenantKey: e.TenantKey, ProjectKey: e.ProjectKey, Attempts: e.Attempts, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

func (o storeOutbox) MarkPublished(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error {
	return o.st.MarkPublished(ctx, tenant, id, at)
}

func (o storeOutbox) MarkFailed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error {
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

// brokerProfile is the natsbus profile of the process: the environment, with an
// unset one meaning development (the kernel assembly's own default).
func brokerProfile(env string) natsbus.Profile {
	if env == "" {
		return natsbus.Development
	}
	return natsbus.Profile(env)
}

// connectBus returns the broker the kernel publishes to, through natsbus.Open.
// The test and production profiles require NATS: a missing URL, an unreachable
// server or one without JetStream fails startup, and so does an injected
// in-process bus; there is no silent fallback. Development without a NATS URL
// delivers in process, which is logged once. closeFn releases the connection.
func connectBus(ctx context.Context, cfg Config, log *slog.Logger) (b bus.Bus, closeFn func(), err error) {
	profile := brokerProfile(cfg.Env)
	if cfg.Bus != nil {
		if _, inProcess := cfg.Bus.(*bus.Memory); inProcess && profile.RequiresBroker() {
			return nil, nil, fmt.Errorf("kernel: %w", natsbus.ErrBrokerRequired)
		}
		return cfg.Bus, func() {}, nil
	}
	if cfg.NATSURL != "" {
		if err := ValidateNATSURL(cfg.NATSURL); err != nil {
			return nil, nil, err
		}
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opts := natsbus.Options{Profile: profile, URL: cfg.NATSURL, JetStream: natsbus.JetStreamConfig{Logger: log}}
	if cfg.NATSURL != "" {
		extra := []nats.Option{nats.Name("kodama-kernel"), nats.Timeout(5 * time.Second)}
		if cfg.NATSToken != "" {
			extra = append(extra, nats.Token(cfg.NATSToken))
		}
		opts.Dial = func(url string, o ...nats.Option) (*nats.Conn, error) {
			return nats.Connect(url, append(o, extra...)...)
		}
	}
	br, err := natsbus.Open(setup, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("kernel: event bus: %w", err)
	}
	if br.InProcess() {
		log.Info("no NATS URL is configured (KODAMA_NATS_URL): events are delivered in process; the outbox rows remain the durable state")
	} else {
		log.Info("events are delivered through NATS JetStream")
	}
	return br.Bus, br.Close, nil
}
