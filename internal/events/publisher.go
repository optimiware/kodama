package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// PublisherConfig configures the publisher worker. All fields are explicit.
type PublisherConfig struct {
	// Lease is how long a claim stays exclusive. It must exceed PublishTimeout
	// so a healthy publisher finishes before its lease can be reclaimed.
	Lease time.Duration
	// PublishTimeout bounds one broker publish.
	PublishTimeout time.Duration
	// BatchSize bounds claims per tenant per pass.
	BatchSize int
	// Interval is the periodic repair pass, independent of wakeups.
	Interval time.Duration
	Retry    RetryPolicy
	Clock    func() time.Time
	Logger   *slog.Logger
}

// Validate checks the configuration.
func (c PublisherConfig) Validate() error {
	switch {
	case c.Lease <= 0 || c.PublishTimeout <= 0 || c.Interval <= 0:
		return fmt.Errorf("%w: publisher lease, publish timeout and interval must be positive", ErrInvalidConfiguration)
	case c.Lease <= c.PublishTimeout:
		return fmt.Errorf("%w: publisher lease must exceed the publish timeout", ErrInvalidConfiguration)
	case c.BatchSize < 1:
		return fmt.Errorf("%w: publisher batch size must be at least 1", ErrInvalidConfiguration)
	}
	return c.Retry.Validate()
}

// PublishResult counts one pass.
type PublishResult struct {
	Claimed   int
	Published int
	Failed    int
	// Expired counts claims whose lease ended before they could be sent. They
	// are not sent (another publisher may already own the row) and are left to
	// be reclaimed.
	Expired int
}

// Publisher drives outbox rows to the broker: claim under a fencing token
// (short transaction, no locks held across the network), publish with the
// stable event ID and wait for durable acknowledgment, then mark published for
// the current claim only. Any unknown outcome leaves the row retryable;
// duplicates are absorbed by broker suppression and, finally, the inbox.
type Publisher struct {
	tenants TenantSource
	store   PublicationStore
	broker  BrokerPublisher
	cfg     PublisherConfig
	wake    chan struct{}

	mu       sync.Mutex
	counters PublisherCounters
}

// PublisherCounters are cumulative, process-local publication metrics.
type PublisherCounters struct {
	Attempts  int64
	Published int64
	Failures  int64
	// StaleCompletions counts acknowledgments whose claim had been superseded.
	StaleCompletions int64
	LastClass        FailureClass
}

// NewPublisher validates the configuration and wires the ports.
func NewPublisher(tenants TenantSource, store PublicationStore, broker BrokerPublisher, cfg PublisherConfig) (*Publisher, error) {
	if tenants == nil || store == nil || broker == nil {
		return nil, fmt.Errorf("%w: publisher needs tenants, a store and a broker", ErrInvalidConfiguration)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Publisher{tenants: tenants, store: store, broker: broker, cfg: cfg, wake: make(chan struct{}, 1)}, nil
}

// Wake requests a prompt pass. Wakeups coalesce and are only an optimization;
// the periodic pass repairs missed ones.
func (p *Publisher) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Counters returns a snapshot of the process-local metrics.
func (p *Publisher) Counters() PublisherCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counters
}

// RunOnce makes one bounded pass over every tenant. A failure for one tenant
// or event does not stop the others; the first error is returned after the pass.
func (p *Publisher) RunOnce(ctx context.Context) (PublishResult, error) {
	var res PublishResult
	ids, err := p.tenants.TenantIDs(ctx)
	if err != nil {
		return res, fmt.Errorf("events: list tenants: %w", err)
	}
	var errs []error
	for _, tenant := range ids {
		if ctx.Err() != nil {
			break
		}
		now := p.cfg.Clock()
		claims, err := p.store.ClaimDue(ctx, tenant, now, p.cfg.Lease, p.cfg.BatchSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("claim for tenant %s: %w", tenant, err))
			continue
		}
		res.Claimed += len(claims)
		for _, claim := range claims {
			if ctx.Err() != nil {
				break
			}
			if err := p.publishOne(ctx, claim, &res); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return res, errors.Join(errs...)
}

func (p *Publisher) publishOne(ctx context.Context, claim Claim, res *PublishResult) error {
	// A claim is only valid until its absolute lease expiry. A batch is claimed
	// upfront and sent sequentially, so a claim can be stale by the time its
	// turn comes (a paused or slow publisher): sending it then would usually
	// republish an event another publisher already owns. Expired claims are not
	// sent, and no send's context outlives the lease. This is hygiene, not
	// fencing: cancelling a context does not recall a message the client already
	// buffered (a reconnect) or handed to the network. The guarantee that such
	// a late copy is harmless is the consumer's evidence-horizon check
	// (internal/events/postgres: checkEvidenceHorizon).
	remaining := claim.LeaseExpiresAt.Sub(p.cfg.Clock())
	if remaining <= 0 {
		res.Expired++
		p.cfg.Logger.Warn("event claim expired before publication; leaving it for reclaim", "tenant", claim.TenantID, "event", claim.EventID, "attempt", claim.Attempt)
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, min(p.cfg.PublishTimeout, remaining))
	err := p.broker.Publish(pctx, Message{ID: claim.EventID, SubjectToken: claim.SubjectToken, EventType: claim.EventType, Data: claim.Payload})
	cancel()
	p.mu.Lock()
	p.counters.Attempts++
	p.mu.Unlock()

	// Settle with a context that outlives cancellation of the pass so an
	// acknowledged publication is not needlessly left for a duplicate.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.PublishTimeout)
	defer scancel()
	if err == nil {
		if cerr := p.store.Complete(sctx, claim, p.cfg.Clock()); cerr != nil {
			if errors.Is(cerr, ErrStaleClaim) {
				p.mu.Lock()
				p.counters.StaleCompletions++
				p.mu.Unlock()
				return nil
			}
			// The broker acknowledged but the mark failed: the claim expires and
			// the row is republished under the same event ID.
			return fmt.Errorf("mark event %s published: %w", claim.EventID, cerr)
		}
		res.Published++
		p.mu.Lock()
		p.counters.Published++
		p.mu.Unlock()
		return nil
	}
	class := ClassOrTransient(err)
	res.Failed++
	p.mu.Lock()
	p.counters.Failures++
	p.counters.LastClass = class
	p.mu.Unlock()
	p.cfg.Logger.Warn("event publication failed", "tenant", claim.TenantID, "event", claim.EventID, "attempt", claim.Attempt, "class", class)
	if ferr := p.store.Fail(sctx, claim, class, err.Error(), p.cfg.Clock()); ferr != nil && !errors.Is(ferr, ErrStaleClaim) {
		return fmt.Errorf("record failure of event %s: %w", claim.EventID, ferr)
	}
	return nil
}

// Run performs a pass at startup, then on every interval or wakeup until ctx
// ends. Errors are logged; the loop never stops on them.
func (p *Publisher) Run(ctx context.Context) error {
	return runLoop(ctx, p.cfg.Interval, p.wake, func(ctx context.Context) {
		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.cfg.Logger.Warn("event publication pass failed", "error", err)
		}
	})
}

// runLoop runs fn immediately, then every interval or on a wakeup, returning
// ctx.Err() when ctx ends.
func runLoop(ctx context.Context, interval time.Duration, wake <-chan struct{}, fn func(context.Context)) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		case <-wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		fn(ctx)
		timer.Reset(interval)
	}
}
