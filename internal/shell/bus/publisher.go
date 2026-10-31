package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Row is one outbox event with the keys that name its subject.
type Row struct {
	Message
	TenantKey  string
	ProjectKey string // empty for a tenant-wide event
	Attempts   int
}

// Outbox is the table the publisher drains (audit_events with published_at,
// publish_attempts and next_attempt_at). It is the consumer-owned port; the
// kernel's store implements it through an adapter.
type Outbox interface {
	Tenants(ctx context.Context) ([]domain.TenantID, error)
	// Pending lists up to limit unpublished rows of a tenant whose next attempt is
	// due at now, ordered by next attempt and ID.
	Pending(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]Row, error)
	// Published records a successful publication of the row.
	Published(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error
	// Failed counts a failed attempt and defers the next one to next; the row
	// stays in the outbox.
	Failed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error
}

// PublisherConfig configures a Publisher.
type PublisherConfig struct {
	Outbox Outbox
	Bus    Sink
	// Interval is the poll period (default 2s); Kick wakes the publisher earlier.
	Interval time.Duration
	// Batch bounds the rows read per tenant per round (default 100).
	Batch int
	// BaseBackoff and MaxBackoff bound the delay before a failed row is retried:
	// BaseBackoff doubled per failed attempt, capped at MaxBackoff (default 1s and
	// 5m).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Now         func() time.Time
	Logger      *slog.Logger
}

// Publisher drains the outbox to the bus. A row is published to
// kodama.v1.<tenant-key>.<project-key>.<event-type> and then marked published; a
// failed publication counts an attempt and moves the row's next attempt out by
// the backoff, and the row is never dropped. A crash between the publication and
// the mark repeats the publication (at least once): the message ID is the stable
// event ID, which is what consumers deduplicate on.
//
// Superseded by outbox.Publisher in internal/shell/outbox, which owns
// publication and reconciliation. This copy remains only until
// internal/shell/serve is repointed (T-188 follow-up); do not add users.
type Publisher struct {
	cfg  PublisherConfig
	kick chan struct{}
	mu   sync.Mutex // one drain at a time
}

// NewPublisher validates the configuration.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Outbox == nil || cfg.Bus == nil {
		return nil, errors.New("bus: the publisher needs an outbox and a bus")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	if cfg.MaxBackoff < cfg.BaseBackoff {
		cfg.MaxBackoff = cfg.BaseBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Publisher{cfg: cfg, kick: make(chan struct{}, 1)}, nil
}

// Kick asks the publisher to drain now instead of at the next interval.
func (p *Publisher) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Backoff is the delay before the retry of a row that has failed attempts times.
func (p *Publisher) Backoff(attempts int) time.Duration {
	d := p.cfg.BaseBackoff
	for i := 0; i < attempts && d < p.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, p.cfg.MaxBackoff)
}

// DrainResult counts one drain.
type DrainResult struct {
	Published int
	Failed    int
}

// Drain publishes every due row once and returns. A row that fails is deferred,
// not retried within the drain. An error is returned only when the outbox itself
// cannot be read or updated.
func (p *Publisher) Drain(ctx context.Context) (DrainResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var res DrainResult
	tenants, err := p.cfg.Outbox.Tenants(ctx)
	if err != nil {
		return res, fmt.Errorf("bus: list tenants: %w", err)
	}
	var firstErr error
	for _, tenant := range tenants {
		for ctx.Err() == nil {
			now := p.cfg.Now()
			rows, err := p.cfg.Outbox.Pending(ctx, tenant, now, p.cfg.Batch)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("bus: read outbox of %s: %w", tenant, err)
				}
				break
			}
			pageFailed := false
			for _, row := range rows {
				if ctx.Err() != nil {
					break
				}
				if err := p.publish(ctx, row); err != nil {
					res.Failed++
					pageFailed = true
					next := p.cfg.Now().Add(p.Backoff(row.Attempts))
					p.cfg.Logger.Warn("event publication failed; it will be retried", "tenant", tenant, "event", row.ID,
						"type", row.Type, "attempts", row.Attempts+1, "retry_at", next, "error", err)
					if ferr := p.cfg.Outbox.Failed(ctx, tenant, row.ID, next); ferr != nil && firstErr == nil {
						firstErr = fmt.Errorf("bus: record the failed publication of %s: %w", row.ID, ferr)
					}
					continue
				}
				if err := p.cfg.Outbox.Published(ctx, tenant, row.ID, p.cfg.Now()); err != nil {
					// The event was delivered; the mark failed, so it repeats.
					if firstErr == nil {
						firstErr = fmt.Errorf("bus: mark %s published: %w", row.ID, err)
					}
					res.Failed++
					pageFailed = true
					continue
				}
				res.Published++
			}
			// A failed row is deferred out of the due window; a page with a failure
			// ends this tenant's round, as does a short page.
			if len(rows) < p.cfg.Batch || pageFailed {
				break
			}
		}
	}
	return res, firstErr
}

func (p *Publisher) publish(ctx context.Context, row Row) error {
	subject, err := Subject(row.TenantKey, row.ProjectKey, row.Type)
	if err != nil {
		return err
	}
	return p.cfg.Bus.Publish(ctx, subject, row.Message)
}

// Run drains at the configured interval, and when kicked, until ctx ends.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := p.Drain(ctx); err != nil && ctx.Err() == nil {
			p.cfg.Logger.Warn("event outbox drain failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
		}
	}
}
