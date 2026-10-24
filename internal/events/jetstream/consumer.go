package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// Consumer pulls deliveries from a durable explicit-ack consumer, resolves each
// message's trusted tenant from its subject token, hands it to the Processor and
// settles the message only according to the durable outcome:
//
//   - completed, duplicate, quarantined: acknowledged (double ack) after the
//     database commit;
//   - retry: negatively acknowledged with the recorded backoff;
//   - no durable outcome (processor error): negatively acknowledged, so the
//     broker redelivers;
//   - subject token unmapped or malformed subject: terminated, never processed,
//     because no trusted tenant exists to own a quarantine record.
type Consumer struct {
	js       natsjs.JetStream
	cfg      Config
	cc       ConsumerConfig
	resolver events.SubjectResolver
	proc     events.Processor
	logger   *slog.Logger

	mu       sync.Mutex
	tenants  map[string]domain.TenantID // positive cache: tokens are immutable
	counters ConsumerCounters
}

// ConsumerCounters are cumulative, process-local metrics.
type ConsumerCounters struct {
	Acked      int64
	Retried    int64
	Terminated int64
	Errors     int64
}

// NewConsumer wraps an injected connection.
func NewConsumer(nc *nats.Conn, cfg Config, cc ConsumerConfig, resolver events.SubjectResolver, proc events.Processor, logger *slog.Logger) (*Consumer, error) {
	if nc == nil || resolver == nil || proc == nil {
		return nil, fmt.Errorf("%w: consumer needs a connection, a resolver and a processor", events.ErrInvalidConfiguration)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cc.Validate(); err != nil {
		return nil, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Consumer{js: js, cfg: cfg, cc: cc, resolver: resolver, proc: proc, logger: logger, tenants: map[string]domain.TenantID{}}, nil
}

// Counters returns a snapshot of the metrics.
func (c *Consumer) Counters() ConsumerCounters {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counters
}

func (c *Consumer) count(f func(*ConsumerCounters)) {
	c.mu.Lock()
	f(&c.counters)
	c.mu.Unlock()
}

// Provision ensures the stream and the durable consumer exist, validating
// existing ones and refusing incompatible settings.
func (c *Consumer) Provision(ctx context.Context) error {
	if err := ensureStream(ctx, c.js, c.cfg); err != nil {
		return err
	}
	filter := c.cfg.SubjectFilter()
	existing, err := c.js.Consumer(ctx, c.cfg.Stream, c.cc.Durable)
	if errors.Is(err, natsjs.ErrConsumerNotFound) {
		if _, err := c.js.CreateConsumer(ctx, c.cfg.Stream, c.cc.consumerConfig(filter)); err != nil {
			return fmt.Errorf("jetstream: create consumer %s: %w", c.cc.Durable, err)
		}
		return nil
	}
	if errors.Is(err, natsjs.ErrNotPullConsumer) {
		return fmt.Errorf("%w: consumer %s is a push consumer; this adapter requires a pull consumer; reconfigure deliberately instead of resetting", events.ErrInvalidConfiguration, c.cc.Durable)
	}
	if err != nil {
		return fmt.Errorf("jetstream: look up consumer %s: %w", c.cc.Durable, err)
	}
	return c.cc.compatible(existing.CachedInfo().Config, filter, c.cfg.Replicas)
}

// RunOnce fetches and processes at most one batch, returning how many messages
// it settled. It returns early with ctx's error or a fetch error.
func (c *Consumer) RunOnce(ctx context.Context) (int, error) {
	cons, err := c.js.Consumer(ctx, c.cfg.Stream, c.cc.Durable)
	if err != nil {
		return 0, fmt.Errorf("jetstream: consumer %s: %w", c.cc.Durable, err)
	}
	batch, err := cons.Fetch(c.cc.FetchBatch, natsjs.FetchMaxWait(c.cc.FetchWait))
	if err != nil {
		return 0, fmt.Errorf("jetstream: fetch: %w", err)
	}
	n := 0
	for msg := range batch.Messages() {
		if ctx.Err() != nil {
			break
		}
		c.handle(ctx, msg)
		n++
	}
	if err := batch.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, nats.ErrTimeout) {
		return n, fmt.Errorf("jetstream: fetch batch: %w", err)
	}
	return n, nil
}

// Run processes batches until ctx ends. Fetch errors (a broker outage) are
// logged and retried after a short pause; nothing is lost because unsettled
// messages are redelivered.
func (c *Consumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.count(func(k *ConsumerCounters) { k.Errors++ })
			c.logger.Warn("event consumer fetch failed", "consumer", c.cc.Consumer, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(c.cc.RetryDelay):
			}
		}
	}
	return ctx.Err()
}

func (c *Consumer) tenantFor(ctx context.Context, token string) (domain.TenantID, error) {
	c.mu.Lock()
	t, ok := c.tenants[token]
	c.mu.Unlock()
	if ok {
		return t, nil
	}
	t, err := c.resolver.ResolveSubjectToken(ctx, token)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tenants[token] = t
	c.mu.Unlock()
	return t, nil
}

func (c *Consumer) handle(ctx context.Context, msg natsjs.Msg) {
	settle := func(what string, err error) {
		if err != nil {
			c.count(func(k *ConsumerCounters) { k.Errors++ })
			c.logger.Warn("event message settlement failed", "consumer", c.cc.Consumer, "settle", what, "error", err)
		}
	}
	token, _, err := events.ParseSubject(c.cfg.Namespace, msg.Subject())
	if err != nil {
		c.count(func(k *ConsumerCounters) { k.Terminated++ })
		c.logger.Warn("event message terminated: untrusted subject", "consumer", c.cc.Consumer)
		settle("term", msg.Term())
		return
	}
	tenant, err := c.tenantFor(ctx, token)
	switch {
	case errors.Is(err, events.ErrUnknownSubject):
		c.count(func(k *ConsumerCounters) { k.Terminated++ })
		c.logger.Warn("event message terminated: subject token maps to no tenant", "consumer", c.cc.Consumer)
		settle("term", msg.Term())
		return
	case err != nil:
		settle("nak", msg.NakWithDelay(c.cc.RetryDelay))
		return
	}
	id := events.DeliveryIdentity{Stream: c.cfg.Stream, Subject: msg.Subject()}
	if meta, err := msg.Metadata(); err == nil {
		id.StreamSeq, id.DeliveryNum = meta.Sequence.Stream, int(meta.NumDelivered)
	}
	out, err := c.proc.Process(ctx, events.Delivery{Tenant: tenant, Consumer: c.cc.Consumer, Payload: msg.Data(), Identity: id})
	if err != nil {
		c.count(func(k *ConsumerCounters) { k.Errors++ })
		c.logger.Warn("event processing recorded no durable outcome", "consumer", c.cc.Consumer, "error", err)
		settle("nak", msg.NakWithDelay(c.cc.RetryDelay))
		return
	}
	if out.Kind == events.OutcomeRetry {
		c.count(func(k *ConsumerCounters) { k.Retried++ })
		settle("nak", msg.NakWithDelay(out.RetryAfter))
		return
	}
	c.count(func(k *ConsumerCounters) { k.Acked++ })
	settle("ack", msg.DoubleAck(ctx))
}
