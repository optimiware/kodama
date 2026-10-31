package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
)

// PublisherConfig configures a Publisher.
type PublisherConfig struct {
	Store Store
	// Bus accepts a message durably or fails (natsbus.JetStream in a deployed
	// profile).
	Bus bus.Sink
	// Interval is the poll period of Run (default 2s); Kick wakes it earlier.
	Interval time.Duration
	// ReconcileEvery is how often Run also reconciles (default 1m).
	ReconcileEvery time.Duration
	// Grace is the age after which an unpublished row is considered missed by the
	// normal drain and is republished by Reconcile whatever its backoff
	// (default 30s).
	Grace time.Duration
	// Batch bounds the rows read per tenant per page (default 100).
	Batch int
	// BaseBackoff and MaxBackoff bound the delay before a failed row is retried
	// (default 1s and 5m).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Now         func() time.Time
	Logger      *slog.Logger
}

// Publisher drains committed intent to the bus. A row is published to
// kodama.v1.<tenant-key>.<project-key>.<event-type> and then marked published. A
// failed publication counts an attempt and defers the row; it is never dropped.
// A crash between the acknowledgment and the mark publishes the row again with
// the same event id.
type Publisher struct {
	cfg  PublisherConfig
	kick chan struct{}
	mu   sync.Mutex // one drain or reconciliation at a time
}

// NewPublisher validates the configuration.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Store == nil || cfg.Bus == nil {
		return nil, errors.New("outbox: the publisher needs a store and a bus")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.ReconcileEvery <= 0 {
		cfg.ReconcileEvery = time.Minute
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 30 * time.Second
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
	cfg.MaxBackoff = max(cfg.MaxBackoff, cfg.BaseBackoff)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Publisher{cfg: cfg, kick: make(chan struct{}, 1)}, nil
}

// Kick asks Run to drain now.
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

// Result counts one pass.
type Result struct {
	Published int
	Failed    int
}

// Drain publishes every due row once. A failed row is deferred, not retried in
// the same drain. An error is returned only when the store cannot be read or
// updated.
func (p *Publisher) Drain(ctx context.Context) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pass(ctx, func(tenant domain.TenantID) ([]Row, error) {
		return p.cfg.Store.Pending(ctx, tenant, p.cfg.Now(), p.cfg.Batch)
	}, true)
}

// Reconcile finds committed rows that are still unpublished after the grace
// period, whether they wait out a backoff or their wakeup was lost, and
// republishes them now. The event ids are unchanged, so a row the broker
// already has is a duplicate its consumers deduplicate. It repairs a publisher
// that was down, a lost Kick and a long outage of the broker.
func (p *Publisher) Reconcile(ctx context.Context) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := p.cfg.Now().Add(-p.cfg.Grace)
	var after = map[domain.TenantID]int64{}
	return p.pass(ctx, func(tenant domain.TenantID) ([]Row, error) {
		rows, err := p.cfg.Store.Unpublished(ctx, tenant, cutoff, after[tenant], p.cfg.Batch)
		if len(rows) > 0 {
			after[tenant] = rows[len(rows)-1].Seq
		}
		return rows, err
	}, false)
}

func (p *Publisher) pass(ctx context.Context, page func(domain.TenantID) ([]Row, error), stopOnFailure bool) (Result, error) {
	var res Result
	tenants, err := p.cfg.Store.Tenants(ctx)
	if err != nil {
		return res, fmt.Errorf("outbox: list tenants: %w", err)
	}
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, tenant := range tenants {
		for ctx.Err() == nil {
			rows, err := page(tenant)
			if err != nil {
				note(fmt.Errorf("outbox: read outbox of %s: %w", tenant, err))
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
					if ferr := p.cfg.Store.MarkFailed(ctx, tenant, row.ID, next); ferr != nil {
						note(fmt.Errorf("outbox: record the failed publication of %s: %w", row.ID, ferr))
					}
					continue
				}
				if err := p.cfg.Store.MarkPublished(ctx, tenant, row.ID, p.cfg.Now()); err != nil {
					// Delivered but not marked: the row repeats with the same id.
					note(fmt.Errorf("outbox: mark %s published: %w", row.ID, err))
					res.Failed++
					pageFailed = true
					continue
				}
				res.Published++
			}
			if len(rows) < p.cfg.Batch || (pageFailed && stopOnFailure) {
				break
			}
		}
	}
	return res, firstErr
}

func (p *Publisher) publish(ctx context.Context, row Row) error {
	subject, err := row.Subject()
	if err != nil {
		return err
	}
	return p.cfg.Bus.Publish(ctx, subject, row.Message)
}

// Run drains at the interval and when kicked, and reconciles every
// ReconcileEvery, until ctx ends.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	lastReconcile := p.cfg.Now()
	for {
		if _, err := p.Drain(ctx); err != nil && ctx.Err() == nil {
			p.cfg.Logger.Warn("event outbox drain failed", "error", err)
		}
		if p.cfg.Now().Sub(lastReconcile) >= p.cfg.ReconcileEvery {
			lastReconcile = p.cfg.Now()
			if res, err := p.Reconcile(ctx); err != nil && ctx.Err() == nil {
				p.cfg.Logger.Warn("event outbox reconciliation failed", "error", err)
			} else if res.Published > 0 {
				p.cfg.Logger.Info("event outbox reconciliation republished missed events", "published", res.Published)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
		}
	}
}
