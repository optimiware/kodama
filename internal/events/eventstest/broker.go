package eventstest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// InProcessBroker is the `development` events.BrokerPublisher: an in-process
// at-least-once broker. Publish returns nil only once the message is queued
// (the durable acknowledgment of the memory side), suppresses a message ID it
// has already accepted, and Pump delivers queued messages to the subscribed
// consumers through the same events.Processor and events.SubjectResolver ports
// the JetStream consumer uses.
//
// It is development-only and holds nothing across a restart. Like JetStream it
// leaves exactly-once to the consumer's inbox: Redeliver forces a duplicate the
// broker itself would not suppress.
type InProcessBroker struct {
	resolver events.SubjectResolver

	mu       sync.Mutex
	accepted map[string]struct{}
	queue    []queued
	log      []events.Message
	subs     []subscription
	down     error
	counters BrokerCounters
}

type subscription struct {
	consumer string
	proc     events.Processor
}

type queued struct {
	msg events.Message
	// pending lists the consumers that still owe a settled outcome.
	pending []string
}

// BrokerCounters are cumulative, process-local metrics of the broker.
type BrokerCounters struct {
	Published  int // messages accepted, after message-ID suppression
	Suppressed int // publications of an already accepted message ID
	Acked      int // deliveries settled (completed, duplicate or quarantined)
	Retried    int // deliveries left for redelivery
	Terminated int // deliveries dropped because no trusted tenant exists
}

var _ events.BrokerPublisher = (*InProcessBroker)(nil)

// NewInProcessBroker returns a broker that maps subject tokens to tenants with
// resolver.
func NewInProcessBroker(resolver events.SubjectResolver) (*InProcessBroker, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: the broker needs a subject resolver", events.ErrInvalidConfiguration)
	}
	return &InProcessBroker{resolver: resolver, accepted: map[string]struct{}{}}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*InProcessBroker) DevelopmentOnly() bool { return true }

// Subscribe adds a consumer that receives every accepted message from now on and
// every message still queued.
func (b *InProcessBroker) Subscribe(consumer string, proc events.Processor) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, subscription{consumer: consumer, proc: proc})
	for i := range b.queue {
		b.queue[i].pending = append(b.queue[i].pending, consumer)
	}
}

// SetDown makes Publish fail like a broker outage (nil restores service).
func (b *InProcessBroker) SetDown(err error) {
	b.mu.Lock()
	b.down = err
	b.mu.Unlock()
}

// Publish queues the message. A repeated message ID is acknowledged without
// being queued again.
func (b *InProcessBroker) Publish(ctx context.Context, m events.Message) error {
	if err := ctx.Err(); err != nil {
		return events.Transient(err)
	}
	if _, err := events.Subject("", m.SubjectToken, m.EventType); err != nil {
		return events.Permanent(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down != nil {
		return events.Transient(b.down)
	}
	if _, dup := b.accepted[m.ID]; dup {
		b.counters.Suppressed++
		return nil
	}
	b.accepted[m.ID] = struct{}{}
	b.counters.Published++
	b.log = append(b.log, m)
	b.enqueue(m)
	return nil
}

func (b *InProcessBroker) enqueue(m events.Message) {
	q := queued{msg: m}
	for _, s := range b.subs {
		q.pending = append(q.pending, s.consumer)
	}
	b.queue = append(b.queue, q)
}

// Redeliver queues a copy of an already accepted message again, as an
// at-least-once broker may after a lost acknowledgment. It reports whether the
// message ID is known.
func (b *InProcessBroker) Redeliver(messageID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.log {
		if m.ID == messageID {
			b.enqueue(m)
			return true
		}
	}
	return false
}

// Counters returns a snapshot of the metrics.
func (b *InProcessBroker) Counters() BrokerCounters {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters
}

// Pump delivers every queued message once to each consumer that still owes an
// outcome and returns how many deliveries it settled. A delivery whose
// processor recorded no durable outcome, or asked for a retry, stays queued for
// the next Pump. A message whose subject token maps to no tenant is dropped:
// no trusted tenant exists to own a quarantine record.
func (b *InProcessBroker) Pump(ctx context.Context) (int, error) {
	b.mu.Lock()
	work := b.queue
	b.queue = nil
	subs := append([]subscription(nil), b.subs...)
	b.mu.Unlock()

	settled := 0
	var errs []error
	var keep []queued
	for _, q := range work {
		tenant, err := b.resolver.ResolveSubjectToken(ctx, q.msg.SubjectToken)
		if err != nil {
			b.mu.Lock()
			if errors.Is(err, events.ErrUnknownSubject) {
				b.counters.Terminated++
			} else {
				b.counters.Retried++
				keep = append(keep, q)
			}
			b.mu.Unlock()
			if !errors.Is(err, events.ErrUnknownSubject) {
				errs = append(errs, err)
			}
			continue
		}
		var still []string
		for _, consumer := range q.pending {
			proc := procFor(subs, consumer)
			if proc == nil {
				continue
			}
			out, err := proc.Process(ctx, events.Delivery{Tenant: tenant, Consumer: consumer, Payload: q.msg.Data,
				Identity: events.DeliveryIdentity{Subject: q.msg.EventType}})
			switch {
			case err != nil:
				errs = append(errs, err)
				still = append(still, consumer)
				b.count(func(c *BrokerCounters) { c.Retried++ })
			case out.Kind == events.OutcomeRetry:
				still = append(still, consumer)
				b.count(func(c *BrokerCounters) { c.Retried++ })
			default:
				settled++
				b.count(func(c *BrokerCounters) { c.Acked++ })
			}
		}
		if len(still) > 0 {
			keep = append(keep, queued{msg: q.msg, pending: still})
		}
	}
	b.mu.Lock()
	b.queue = append(keep, b.queue...)
	b.mu.Unlock()
	return settled, errors.Join(errs...)
}

func (b *InProcessBroker) count(f func(*BrokerCounters)) {
	b.mu.Lock()
	f(&b.counters)
	b.mu.Unlock()
}

func procFor(subs []subscription, consumer string) events.Processor {
	for _, s := range subs {
		if s.consumer == consumer {
			return s.proc
		}
	}
	return nil
}

// Queued reports how many messages still await delivery.
func (b *InProcessBroker) Queued() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queue)
}

// TenantOf is a convenience for tests: it resolves a message's tenant.
func (b *InProcessBroker) TenantOf(ctx context.Context, m events.Message) (domain.TenantID, error) {
	return b.resolver.ResolveSubjectToken(ctx, m.SubjectToken)
}
