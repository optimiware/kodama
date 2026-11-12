// Fail-closed observation sink.
//
// The NATS ingress Observation carries no idempotency key, so an
// observation arriving there cannot be committed safely. Custom observations
// reach the system through the authenticated event.publish operation instead.
// Composition therefore supplies this sink, which refuses every observation
// with a stable unsupported-capability fault and records nothing. Enabling
// ingress requires an idempotency identity on the observation first.
package bridges

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
)

// ObservationSink is the fail-closed nats.ObservationSink implementation.
type ObservationSink struct{}

var _ nats.ObservationSink = (*ObservationSink)(nil)

// NewObservationSink returns the sink. It has no state and records nothing.
func NewObservationSink() *ObservationSink {
	return &ObservationSink{}
}

// Observe refuses every observation. A canceled context is reported as an
// expired deadline; otherwise the refusal is unsupported_capability.
func (*ObservationSink) Observe(ctx context.Context, _ nats.Observation) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "observation ingress was canceled", err)
	}
	return fault.New(fault.UnsupportedCapability, "custom observation ingress is disabled; use the authenticated event.publish operation")
}
