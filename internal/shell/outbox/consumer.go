package outbox

import (
	"context"
	"errors"
	"sync"

	"github.com/optimiware/kodama/internal/shell/bus"
)

// Ledger records which events a named consumer has applied. Production adapters
// store the entry in the same transaction as the consumer's effect, so "applied"
// and "recorded" cannot diverge; a consumer that owns an idempotent resource
// (a run keyed by its trigger key) needs no ledger.
type Ledger interface {
	// Once runs apply unless the consumer already applied the event. It records
	// the event only when apply returns nil, so a failure leaves the event to be
	// delivered and applied again. It returns whether apply ran.
	Once(ctx context.Context, consumer, eventID string, apply func(ctx context.Context) error) (ran bool, err error)
}

// Consumer is the handler contract of a durable consumer: duplicates are
// settled without effect, a failure leaves the delivery for redelivery.
type Consumer struct {
	// Name is the durable consumer name (natsbus.ConsumerName); it scopes the
	// ledger so two consumers of one event each apply it.
	Name   string
	Ledger Ledger
	Apply  bus.Handler
}

// Handle is the bus.Handler of the consumer.
func (c Consumer) Handle(ctx context.Context, m bus.Message) error {
	if c.Name == "" || c.Ledger == nil || c.Apply == nil {
		return errors.New("outbox: a consumer needs a name, a ledger and an apply function")
	}
	_, err := c.Ledger.Once(ctx, c.Name, m.ID, func(ctx context.Context) error { return c.Apply(ctx, m) })
	return err
}

// MemoryLedger is the in-process Ledger of development tests. Calls for one
// event are serialized.
type MemoryLedger struct {
	mu   sync.Mutex
	done map[string]bool
}

var _ Ledger = (*MemoryLedger)(nil)

// DevelopmentOnly flags the ledger as a development adapter.
func (*MemoryLedger) DevelopmentOnly() bool { return true }

// Once implements Ledger.
func (l *MemoryLedger) Once(ctx context.Context, consumer, eventID string, apply func(context.Context) error) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := consumer + "\x00" + eventID
	if l.done[key] {
		return false, nil
	}
	if err := apply(ctx); err != nil {
		return true, err
	}
	if l.done == nil {
		l.done = map[string]bool{}
	}
	l.done[key] = true
	return true, nil
}

// Projection guards a view that events update with a monotonic version: an event
// older than, or equal to, the version already held never overwrites it, so
// reordered or replayed delivery cannot regress the view.
type Projection struct {
	mu       sync.Mutex
	versions map[string]int64
}

// DevelopmentOnly flags the guard as an in-process adapter; a deployed
// projection compares the version stored with the row in its own transaction.
func (*Projection) DevelopmentOnly() bool { return true }

// Advance reports whether the event of key at version is newer than what was
// applied, and records it when so.
func (p *Projection) Advance(key string, version int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.versions[key]; ok && version <= cur {
		return false
	}
	if p.versions == nil {
		p.versions = map[string]int64{}
	}
	p.versions[key] = version
	return true
}

// Version is the version applied for key (0 when none).
func (p *Projection) Version(key string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.versions[key]
}
