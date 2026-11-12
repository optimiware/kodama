package app

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
)

// StreamConfig configures the two JetStream streams the production composition
// provisions before any publication loop starts. The zero value selects the
// driver's documented defaults, so an installation that does not tune
// retention still gets the declared stream shape.
type StreamConfig struct {
	Events        StreamRetention
	Invalidations StreamRetention
	Replicas      int
}

// StreamRetention configures one stream's bounded retention and deduplication
// window. A zero field selects that stream's default.
type StreamRetention struct {
	MaxAge          time.Duration
	MaxBytes        int64
	DuplicateWindow time.Duration
}

// streamMinimumDuplicateWindow mirrors the broker minimum the driver enforces,
// so the composition refuses an impossible window before dialing.
const streamMinimumDuplicateWindow = 100 * time.Millisecond

// streamMaxReplicas is the broker's accepted replica range upper bound. Zero
// selects the driver default.
const streamMaxReplicas = 5

// validate refuses a stream configuration the broker cannot accept. It runs in
// Config.validate, before any adapter is constructed, and is nil-safe.
func (config *StreamConfig) validate() error {
	if config == nil {
		return nil
	}
	if err := config.Events.validate("events", natsdriver.DefaultEventsMaxAge); err != nil {
		return err
	}
	if err := config.Invalidations.validate("invalidations", natsdriver.DefaultInvalidationsMaxAge); err != nil {
		return err
	}
	if config.Replicas < 0 || config.Replicas > streamMaxReplicas {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"stream replicas %d is outside the broker range 0-%d", config.Replicas, streamMaxReplicas))
	}
	return nil
}

func (retention StreamRetention) validate(kind string, defaultMaxAge time.Duration) error {
	if retention.MaxAge < 0 {
		return fault.New(fault.InvalidInput, kind+" stream max age is negative")
	}
	if retention.MaxBytes < 0 {
		return fault.New(fault.InvalidInput, kind+" stream max bytes is negative")
	}
	if retention.DuplicateWindow < 0 {
		return fault.New(fault.InvalidInput, kind+" stream duplicate window is negative")
	}
	if retention.DuplicateWindow > 0 && retention.DuplicateWindow < streamMinimumDuplicateWindow {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"%s stream duplicate window is below the broker minimum of %s", kind, streamMinimumDuplicateWindow))
	}
	maxAge := retention.MaxAge
	if maxAge == 0 {
		maxAge = defaultMaxAge
	}
	window := retention.DuplicateWindow
	if window == 0 {
		window = natsdriver.DefaultDuplicateWindow
	}
	if window > maxAge {
		return fault.New(fault.InvalidInput, kind+" stream duplicate window exceeds its retention age")
	}
	return nil
}

// driverConfig converts the composition configuration into the driver's
// stream-administration input. A nil configuration selects every default.
func (config *StreamConfig) driverConfig() natsdriver.StreamConfig {
	if config == nil {
		return natsdriver.StreamConfig{}
	}
	return natsdriver.StreamConfig{
		Events: natsdriver.StreamRetention{
			MaxAge:          config.Events.MaxAge,
			MaxBytes:        config.Events.MaxBytes,
			DuplicateWindow: config.Events.DuplicateWindow,
		},
		Invalidations: natsdriver.StreamRetention{
			MaxAge:          config.Invalidations.MaxAge,
			MaxBytes:        config.Invalidations.MaxBytes,
			DuplicateWindow: config.Invalidations.DuplicateWindow,
		},
		Replicas: config.Replicas,
	}
}

// natsClient is the composition-owned NATS driver seam. It is the broker client
// plus the JetStream stream administration the composition root performs before
// the publication loops start. The real driver (*natsdriver.Client) implements
// it; the composition tests inject a deterministic fake through natsDial.
type natsClient interface {
	natsbroker.Client
	EnsureStreams(context.Context, natsdriver.StreamConfig) error
}

// natsDial opens one NATS driver connection. Production uses dialNATSDriver;
// the unit suite injects a fake so construction never opens a socket.
type natsDial func(context.Context, natsdriver.Config) (natsClient, error)

// dialNATSDriver is the production dial: the only place the composition
// constructs the real NATS driver.
func dialNATSDriver(ctx context.Context, config natsdriver.Config) (natsClient, error) {
	client, err := natsdriver.Dial(ctx, config)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fault.New(fault.DependencyUnavailable, "broker connection returned no client")
	}
	return client, nil
}

// provisionStreams creates or converges the two streams the broker publishes
// into. It runs during production startup, after the driver connects and before
// any publication loop can start, and it closes its own connection so the
// broker's lazy publication connection stays independent. Only the production
// composition constructs a real NATS driver; the development and test profiles
// use the in-process broker or an injected publisher, so they never provision
// streams and a test injector that supplies a real broker owns its own stream
// administration.
//
// A refusal (including a configuration conflict), a dial failure or a failed
// close is a startup failure. The returned fault keeps the adapter's class but
// renders only a stable message, so a URL or credential in the underlying cause
// never reaches logs or the caller.
func provisionStreams(ctx context.Context, broker natsdriver.Config, streams *StreamConfig, dial natsDial) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "production stream provisioning requires a startup context")
	}
	if err := streams.validate(); err != nil {
		return err
	}
	client, err := dial(ctx, broker)
	if err != nil {
		return streamProvisionFault("broker stream provisioning connection failed", err)
	}
	if client == nil {
		return fault.New(fault.DependencyUnavailable, "broker stream provisioning connection returned no client")
	}
	ensureErr := client.EnsureStreams(ctx, streams.driverConfig())
	closeErr := client.Close(context.WithoutCancel(ctx))
	if ensureErr != nil {
		return streamProvisionFault("broker stream provisioning failed", ensureErr)
	}
	if closeErr != nil {
		return streamProvisionFault("broker stream provisioning connection close failed", closeErr)
	}
	return nil
}

// streamProvisionFault keeps the adapter's fault class but replaces its message
// with a stable, secret-free one. The cause stays reachable for diagnostics and
// is never rendered.
func streamProvisionFault(message string, err error) error {
	class := fault.DependencyUnavailable
	if classified, ok := fault.ClassOf(err); ok {
		class = classified
	}
	return fault.Wrap(class, message, err)
}
