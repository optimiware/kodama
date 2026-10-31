package natsbus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/shell/bus"
)

// Profile is the deployment profile that decides whether a broker is required.
// The values equal the KODAMA_ENV environments.
type Profile string

// The profiles. There is no default.
const (
	// Development may use the in-process bus when no NATS server is configured.
	Development Profile = "development"
	// Test requires NATS.
	Test Profile = "test"
	// Production requires NATS.
	Production Profile = "production"
)

var (
	// ErrBrokerRequired reports that a deployed profile was given no NATS
	// connection or URL.
	ErrBrokerRequired = errors.New("natsbus: the test and production profiles require NATS; there is no in-process fallback")
	// ErrBrokerUnavailable reports that the configured NATS server could not be
	// reached or has no JetStream stream for the events.
	ErrBrokerUnavailable = errors.New("natsbus: the NATS broker is unavailable")
	// ErrProfileUnknown reports a profile that is not one of the three.
	ErrProfileUnknown = errors.New("natsbus: unknown profile (development, test or production)")
)

// Valid reports whether p is one of the three profiles.
func (p Profile) Valid() bool { return p == Development || p == Test || p == Production }

// RequiresBroker reports whether p must run on NATS.
func (p Profile) RequiresBroker() bool { return p == Test || p == Production }

// ParseProfile matches a profile exactly (no case folding, no default).
func ParseProfile(s string) (Profile, error) {
	if p := Profile(s); p.Valid() {
		return p, nil
	}
	return "", fmt.Errorf("%w: %q", ErrProfileUnknown, s)
}

// Options is what Open needs. Exactly one of Conn and URL names the broker; both
// empty means "no broker configured".
type Options struct {
	Profile Profile
	// Conn is an injected connection the caller owns and closes.
	Conn *nats.Conn
	// URL is dialled by Open, which then owns the connection (Broker.Close).
	URL string
	// Dial replaces nats.Connect (tests).
	Dial func(url string, opts ...nats.Option) (*nats.Conn, error)
	// JetStream configures the stream and consumers.
	JetStream JetStreamConfig
}

// Broker is the bus a process composes.
type Broker struct {
	bus.Bus
	profile   Profile
	inProcess bool
	nc        *nats.Conn
	ownsConn  bool
}

// Profile is the profile the broker was opened for.
func (b *Broker) Profile() Profile { return b.profile }

// InProcess reports that the broker is the development-only in-process double.
func (b *Broker) InProcess() bool { return b.inProcess }

// DevelopmentOnly flags the in-process double for the composition root's check
// that production refuses development adapters.
func (b *Broker) DevelopmentOnly() bool { return b.inProcess }

// Close releases a connection Open dialled; an injected connection is left to
// its owner.
func (b *Broker) Close() {
	if b.ownsConn && b.nc != nil {
		b.nc.Close()
	}
}

// Open composes the bus for the profile. For test and production it returns an
// error when no NATS is configured, when it cannot be reached, or when the
// stream cannot be ensured: the caller must stop startup. For development with
// nothing configured it returns the in-process bus; development with a
// configured NATS uses it, and a failure to reach it is still an error.
func Open(ctx context.Context, o Options) (*Broker, error) {
	if !o.Profile.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrProfileUnknown, string(o.Profile))
	}
	if o.Conn == nil && o.URL == "" {
		if o.Profile.RequiresBroker() {
			return nil, fmt.Errorf("%w (profile %s)", ErrBrokerRequired, o.Profile)
		}
		return &Broker{Bus: bus.NewMemory(), profile: o.Profile, inProcess: true}, nil
	}
	nc, owns := o.Conn, false
	if nc == nil {
		dial := o.Dial
		if dial == nil {
			dial = nats.Connect
		}
		var err error
		nc, err = dial(o.URL, nats.Timeout(10*time.Second), nats.MaxReconnects(-1), nats.ReconnectBufSize(-1))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
		}
		owns = true
	}
	js, err := NewJetStream(ctx, nc, o.JetStream)
	if err != nil {
		if owns {
			nc.Close()
		}
		return nil, fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
	}
	return &Broker{Bus: js, profile: o.Profile, nc: nc, ownsConn: owns}, nil
}
