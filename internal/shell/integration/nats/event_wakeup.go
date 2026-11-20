package nats

import (
	"context"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// SubscribeEventWakeup opens a lightweight, scope-qualified broker wakeup
// subscription on EventSubject(scope). It invokes onWakeup whenever any event
// message is published for that scope so server-side HTTP SSE streams can drain
// newly committed rows from the durable events table immediately, including
// across multi-replica deployments.
//
// Broker messages are purely an acceleration signal: payloads are ignored here,
// and callers remain responsible for authenticating/authorizing subscribers and
// reading committed rows from the durable event table.
func (broker *Broker) SubscribeEventWakeup(ctx context.Context, scope tenancy.Scope, onWakeup func()) (Subscription, error) {
	if onWakeup == nil {
		return nil, fault.New(fault.InvalidInput, "event wakeup handler is nil")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DeadlineExpired, "event wakeup subscription was canceled", err)
	}
	subject, err := EventSubject(scope)
	if err != nil {
		return nil, err
	}
	client, err := broker.ensureClient(ctx)
	if err != nil {
		return nil, dependencyError("event wakeup subscription failed", err)
	}
	clientSub, err := client.Subscribe(ctx, subject, func(context.Context, []byte) error {
		onWakeup()
		return nil
	})
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "event wakeup subscription failed", err)
	}
	return clientSub, nil
}
