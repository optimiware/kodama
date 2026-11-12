package app

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/token"
)

// inProcessBroker is the explicitly named development/test event and
// invalidation broker. It keeps published identities in memory and can be
// switched unavailable to prove that domain progression is broker-independent.
//
// It is ephemeral: dropping the composition drops every published message.
type inProcessBroker struct {
	mu            sync.Mutex
	down          bool
	published     map[id.EventID]event.Envelope
	invalidations []token.Invalidation
}

var (
	_ event.Publisher        = (*inProcessBroker)(nil)
	_ token.InvalidationPort = (*inProcessBroker)(nil)
)

func newInProcessBroker(clock Clock) (*inProcessBroker, error) {
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "in-process broker requires a clock")
	}
	return &inProcessBroker{published: make(map[id.EventID]event.Envelope)}, nil
}

// Publish records one committed event. An unavailable broker retains nothing;
// the durable outbox keeps the publication intent and retries the same identity.
func (broker *inProcessBroker) Publish(ctx context.Context, envelope event.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.down {
		return fault.New(fault.DependencyUnavailable, "in-process broker is unavailable")
	}
	broker.published[envelope.ID] = envelope
	return nil
}

// Invalidate records one committed credential invalidation.
func (broker *inProcessBroker) Invalidate(ctx context.Context, invalidation token.Invalidation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.down {
		return fault.New(fault.DependencyUnavailable, "in-process broker is unavailable")
	}
	broker.invalidations = append(broker.invalidations, invalidation)
	return nil
}
