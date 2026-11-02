package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
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
	outboxEvents
	dispatch.Store
}

// outboxEvents is the part of EventStore the publisher's adapter uses.
type outboxEvents interface {
	store.OutboxBackend
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
type storeOutbox struct{ st outboxEvents }

var _ outbox.Source = storeOutbox{}

func (o storeOutbox) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	return o.st.TenantIDs(ctx)
}

func (o storeOutbox) Pending(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]outbox.Row, error) {
	events, err := o.st.PendingEvents(ctx, tenant, now, limit)
	return rows(events), err
}

// cursorOf is the opaque outbox.Cursor of an event: its (created_at, id)
// position, which a failed publication never changes. It sorts as a string in
// that order.
func cursorOf(e store.OutboxEvent) string {
	return fmt.Sprintf("%020d/%s", max(e.CreatedAt.UnixNano(), 0), e.ID)
}

// parseCursor is the inverse of cursorOf; the empty cursor is the start.
func parseCursor(c string) (store.OutboxCursor, error) {
	if c == "" {
		return store.OutboxCursor{}, nil
	}
	n, id, ok := strings.Cut(c, "/")
	ns, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil {
		return store.OutboxCursor{}, fmt.Errorf("kernel: malformed outbox cursor %q", c)
	}
	return store.OutboxCursor{CreatedAt: time.Unix(0, ns).UTC(), ID: id}, nil
}

// Unpublished is the reconciliation page: the store applies the age cutoff and
// the (created_at, id) cursor before the limit, so a page is at most limit rows
// whatever the backlog, a stale deferred event is never crowded out by younger
// ones, and a retry never moves a row across the cursor.
func (o storeOutbox) Unpublished(ctx context.Context, tenant domain.TenantID, cutoff time.Time, after string, limit int) ([]outbox.Row, error) {
	cur, err := parseCursor(after)
	if err != nil {
		return nil, err
	}
	events, err := o.st.UnpublishedEvents(ctx, tenant, cutoff, cur, limit)
	if err != nil {
		return nil, err
	}
	return rows(events), nil
}

func rows(events []store.OutboxEvent) []outbox.Row {
	out := make([]outbox.Row, 0, len(events))
	for _, e := range events {
		out = append(out, outbox.Row{
			Message: bus.Message{ID: e.ID, TenantID: e.TenantID, ProjectID: e.ProjectID, Type: e.Type, ResourceType: e.ResourceType,
				ResourceID: e.ResourceID, Actor: bus.Actor{Type: e.ActorType, ID: e.ActorID}, OccurredAt: e.CreatedAt.UTC(),
				Data: json.RawMessage(e.Payload)},
			TenantKey: e.TenantKey, ProjectKey: e.ProjectKey, Attempts: e.Attempts, CreatedAt: e.CreatedAt, Cursor: cursorOf(e),
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
// The profile is validated first, so an unknown environment stops startup
// whatever else is configured. The test and production profiles require NATS:
// a missing URL, an unreachable server or one without JetStream fails startup,
// and an injected bus is accepted only when natsbus.VerifyDeployed finds an
// initialized JetStream adapter whose broker answers for the stream; there is no
// silent in-process fallback. Development accepts an
// injected bus, and without a NATS URL delivers in process, logged once.
// closeFn releases the connection.
func connectBus(ctx context.Context, cfg Config, log *slog.Logger) (b bus.Bus, closeFn func(), err error) {
	profile := brokerProfile(cfg.Env)
	if !profile.Valid() {
		return nil, nil, fmt.Errorf("kernel: event bus: %w: %q", natsbus.ErrProfileUnknown, cfg.Env)
	}
	if cfg.Bus != nil {
		if profile.RequiresBroker() {
			verify, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := natsbus.VerifyDeployed(verify, cfg.Bus); err != nil {
				return nil, nil, fmt.Errorf("kernel: event bus: injected bus refused in the %s profile: %w", profile, err)
			}
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
