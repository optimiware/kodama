// Package natsdriver implements the NATS broker seams
// (nats.Client and nats.Subscriber) over nats.go.
//
// Committed events and credential invalidations are published to JetStream with
// the stable event id as the Nats-Msg-Id deduplication header; live progress
// uses Core NATS. Subscriptions are scope-qualified by the caller and this
// wrapper adds no subject interpretation of its own.
//
// Core NATS has no acknowledgement path: a subscription handler error is
// reported to the configured observer and never silently retried. Durable
// at-least-once event delivery and consumer acknowledgement remain the
// JetStream design proven by the component suite; this wrapper makes no such
// claim.
//
// The wrapper starts no server and opens no socket in its unit suite: tests
// drive a small consumer-owned driver surface. Real broker conformance belongs
// to the component suite.
//
// EnsureStreams provisions the two JetStream streams the adapter publishes
// into. Provisioning is left to composition, so the composition root calls it
// once after Dial and before publishing.
package natsdriver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Config configures one broker client. Every dependency and bound is explicit;
// construction never dials the broker.
type Config struct {
	// URL is the NATS server URL or comma-separated server list.
	URL string
	// Name is the client connection name. It is optional and carries no
	// authority.
	Name string
	// Token is a bearer token. It is mutually exclusive with User/Password.
	Token string
	// User and Password are the optional user credentials. A password without
	// a user is refused.
	User     string
	Password string
	// ConnectTimeout bounds one dial. Zero uses the nats.go default.
	ConnectTimeout time.Duration
	// ReconnectWait bounds the delay between reconnect attempts. Zero uses the
	// nats.go default.
	ReconnectWait time.Duration
	// MaxReconnects is the reconnect attempt count; -1 means unlimited. Zero
	// uses the nats.go default.
	MaxReconnects int
	// DrainTimeout bounds one subscription or connection drain. Zero uses
	// DefaultDrainTimeout.
	DrainTimeout time.Duration
	// TLS enables verified TLS against a private CA. A nil TLS keeps the
	// existing plaintext composition.
	TLS *TLSConfig
	// HandlerErrors, when non-nil, observes a subscription handler error. Core
	// NATS has no acknowledgement path, so this wrapper never claims
	// redelivery; it does not silently discard the error either.
	HandlerErrors func(subject string, err error)
}

// DefaultDrainTimeout bounds one drain when Config does not set a value.
const DefaultDrainTimeout = 5 * time.Second

// driverSubscription is the minimal nats subscription surface this wrapper
// consumes.
type driverSubscription interface {
	Drain() error
	Unsubscribe() error
}

// coreConn is the minimal Core NATS surface this wrapper consumes.
type coreConn interface {
	Publish(subject string, data []byte) error
	PublishMsg(subject, reply string, data []byte) error
	Subscribe(subject string, handler func(subject string, data []byte)) (driverSubscription, error)
	SubscribeMsg(subject string, handler func(subject, reply string, data []byte)) (driverSubscription, error)
	Drain() error
	Close()
}

// jetStreamPublisher is the minimal JetStream publication surface this wrapper
// consumes. msgID is the deduplication identity.
type jetStreamPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte, msgID string) error
}

// Client implements broker.Client and broker.Subscriber.
type Client struct {
	core         coreConn
	jetStream    jetStreamPublisher
	streams      streamAdmin
	drainTimeout time.Duration
	handlerError func(string, error)

	mu       sync.Mutex
	streamMu sync.Mutex
	closed   bool
	subs     map[*subscription]struct{}
}

var (
	_ broker.Client            = (*Client)(nil)
	_ broker.Subscriber        = (*Client)(nil)
	_ broker.CalloutConnection = (*Client)(nil)
)

// Dial validates the configuration and connects one broker client.
func Dial(ctx context.Context, config Config) (*Client, error) {
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, "broker context is nil")
	}
	if strings.TrimSpace(config.URL) == "" {
		return nil, fault.New(fault.InvalidInput, "broker URL is empty")
	}
	if config.Token != "" && (config.User != "" || config.Password != "") {
		return nil, fault.New(fault.InvalidInput, "broker credentials mix a token with user/password")
	}
	if config.Password != "" && config.User == "" {
		return nil, fault.New(fault.InvalidInput, "broker password has no user")
	}
	if config.ConnectTimeout < 0 || config.ReconnectWait < 0 || config.DrainTimeout < 0 {
		return nil, fault.New(fault.InvalidInput, "broker durations are negative")
	}
	if config.MaxReconnects < -1 {
		return nil, fault.New(fault.InvalidInput, "broker reconnect count is below the unlimited sentinel")
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError("broker connection", err)
	}

	tlsConfig, err := config.clientTLS()
	if err != nil {
		return nil, err
	}

	drainTimeout := resolveDrainTimeout(config.DrainTimeout)
	options := make([]natsgo.Option, 0, 8)
	if config.Name != "" {
		options = append(options, natsgo.Name(config.Name))
	}
	if config.Token != "" {
		options = append(options, natsgo.Token(config.Token))
	}
	if config.User != "" {
		options = append(options, natsgo.UserInfo(config.User, config.Password))
	}
	if tlsConfig != nil {
		// Installed once on the connection: nats.go reuses this configuration
		// for every reconnect, so trust, the client certificate and
		// authentication survive a server restart.
		options = append(options, natsgo.Secure(tlsConfig))
	}
	if config.ConnectTimeout > 0 {
		options = append(options, natsgo.Timeout(config.ConnectTimeout))
	}
	if config.ReconnectWait > 0 {
		options = append(options, natsgo.ReconnectWait(config.ReconnectWait))
	}
	if config.MaxReconnects != 0 {
		options = append(options, natsgo.MaxReconnects(config.MaxReconnects))
	}
	options = append(options, natsgo.DrainTimeout(drainTimeout))
	conn, err := natsgo.Connect(config.URL, options...)
	if err != nil {
		// The cause is private and never rendered, so a URL or token cannot
		// reach an error message.
		return nil, fault.Wrap(fault.DependencyUnavailable, "broker connection failed", err)
	}
	stream, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fault.Wrap(fault.DependencyUnavailable, "broker JetStream initialization failed", err)
	}
	js := &natsJetStream{stream: stream}
	client := newClient(&natsCore{conn: conn}, js, config)
	client.streams = js
	return client, nil
}

// resolveDrainTimeout returns the configured drain bound or the default.
func resolveDrainTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultDrainTimeout
	}
	return timeout
}

// effectiveDrainTimeout returns the client's drain bound or the default when
// the client was constructed directly in a test.
func (c *Client) effectiveDrainTimeout() time.Duration {
	if c == nil || c.drainTimeout <= 0 {
		return DefaultDrainTimeout
	}
	return c.drainTimeout
}

// newClient builds one client over an injected driver surface.
func newClient(core coreConn, jetStream jetStreamPublisher, config Config) *Client {
	return &Client{
		core:         core,
		jetStream:    jetStream,
		drainTimeout: resolveDrainTimeout(config.DrainTimeout),
		handlerError: config.HandlerErrors,
		subs:         make(map[*subscription]struct{}),
	}
}

// PublishJetStream publishes one identified message to JetStream. The stable
// event id is the broker deduplication identity, so a retry with the same id is
// not stored twice.
func (c *Client) PublishJetStream(ctx context.Context, subject broker.Subject, eventID id.EventID, payload []byte) error {
	if c == nil {
		return fault.New(fault.InvalidInput, "broker client is nil")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(string(eventID)) == "" {
		return fault.New(fault.InvalidInput, "broker publication identity is empty")
	}
	if err := c.ready(ctx); err != nil {
		return err
	}
	if err := c.jetStream.Publish(ctx, string(subject), payload, string(eventID)); err != nil {
		return mapError("broker publication failed", err)
	}
	return nil
}

// PublishCore publishes one live-progress message on Core NATS. It carries no
// JetStream identity.
func (c *Client) PublishCore(ctx context.Context, subject broker.Subject, payload []byte) error {
	if c == nil {
		return fault.New(fault.InvalidInput, "broker client is nil")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	if err := c.ready(ctx); err != nil {
		return err
	}
	if err := c.core.Publish(string(subject), payload); err != nil {
		return mapError("live publication failed", err)
	}
	return nil
}

// Subscribe opens one core subscription. The handler receives a context derived
// from the subscription context; a handler error is reported to the configured
// observer and never panics the client.
func (c *Client) Subscribe(ctx context.Context, subject broker.Subject, handler func(context.Context, []byte) error) (broker.Subscription, error) {
	if c == nil {
		return nil, fault.New(fault.InvalidInput, "broker client is nil")
	}
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, fault.New(fault.InvalidInput, "broker subscription handler is nil")
	}
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{client: c, cancel: cancel}
	driverSub, err := c.core.Subscribe(string(subject), func(delivered string, payload []byte) {
		if err := handler(handlerCtx, payload); err != nil && c.handlerError != nil {
			c.handlerError(delivered, err)
		}
	})
	if err != nil {
		cancel()
		return nil, mapError("broker subscription failed", err)
	}
	sub.driver = driverSub

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		_ = driverSub.Unsubscribe()
		return nil, fault.New(fault.DependencyUnavailable, "broker client is closed")
	}
	c.subs[sub] = struct{}{}
	c.mu.Unlock()
	return sub, nil
}

// SubscribeInvalidations opens one scope-qualified invalidation subscription.
// The payload is decoded and checked against the requested scope before the
// consumer handler runs.
func (c *Client) SubscribeInvalidations(ctx context.Context, scope tenancy.Scope, handler broker.InvalidationHandler) (broker.Subscription, error) {
	if handler == nil {
		return nil, fault.New(fault.InvalidInput, "invalidation handler is nil")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	subject, err := broker.InvalidationSubject(scope)
	if err != nil {
		return nil, err
	}
	return c.Subscribe(ctx, subject, func(handlerCtx context.Context, payload []byte) error {
		var invalidation token.Invalidation
		if err := json.Unmarshal(payload, &invalidation); err != nil {
			return fault.Wrap(fault.InvalidInput, "invalidation payload is not JSON", err)
		}
		if err := invalidation.Validate(); err != nil {
			return err
		}
		if invalidation.Scope != scope {
			return fault.New(fault.InvalidInput, "invalidation names another scope")
		}
		return handler(handlerCtx, invalidation)
	})
}

// SubscribeCallout subscribes to the broker's raw authorization-request
// subject. The subject is not a scope-qualified Kodama subject: only the
// authorization service uses this path, and the broker protects the subject so
// only the server can publish requests.
func (c *Client) SubscribeCallout(ctx context.Context, subject string, handler func(context.Context, broker.CalloutRequest) error) (broker.Subscription, error) {
	if c == nil {
		return nil, fault.New(fault.InvalidInput, "broker client is nil")
	}
	if strings.TrimSpace(subject) == "" || handler == nil {
		return nil, fault.New(fault.InvalidInput, "callout subscription is incomplete")
	}
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{client: c, cancel: cancel}
	driverSub, err := c.core.SubscribeMsg(subject, func(delivered, reply string, payload []byte) {
		if err := handler(handlerCtx, broker.CalloutRequest{Subject: delivered, Reply: reply, Data: payload}); err != nil && c.handlerError != nil {
			c.handlerError(delivered, err)
		}
	})
	if err != nil {
		cancel()
		return nil, mapError("callout subscription failed", err)
	}
	sub.driver = driverSub

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		_ = driverSub.Unsubscribe()
		return nil, fault.New(fault.DependencyUnavailable, "broker client is closed")
	}
	c.subs[sub] = struct{}{}
	c.mu.Unlock()
	return sub, nil
}

// PublishCallout publishes one raw reply to the broker's reply inbox.
func (c *Client) PublishCallout(ctx context.Context, reply string, data []byte) error {
	if c == nil {
		return fault.New(fault.InvalidInput, "broker client is nil")
	}
	if err := c.ready(ctx); err != nil {
		return err
	}
	if err := c.core.PublishMsg(reply, "", data); err != nil {
		return mapError("callout reply failed", err)
	}
	return nil
}

// Close drains and closes every subscription and the connection. It is
// idempotent and joins its drain goroutines.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := make([]*subscription, 0, len(c.subs))
	for sub := range c.subs {
		subs = append(subs, sub)
	}
	c.mu.Unlock()

	var firstErr error
	for _, sub := range subs {
		if err := sub.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.core == nil {
		return firstErr
	}

	drainCtx, cancelDrain := context.WithTimeout(ctx, c.effectiveDrainTimeout())
	defer cancelDrain()

	done := make(chan error, 1)
	go func() { done <- c.core.Drain() }()
	select {
	case err := <-done:
		c.core.Close()
		if err != nil && firstErr == nil {
			if errors.Is(err, natsgo.ErrDrainTimeout) {
				firstErr = fault.Wrap(fault.DeadlineExpired, "broker drain timed out", err)
			} else {
				firstErr = mapError("broker drain failed", err)
			}
		}
		return firstErr
	case <-drainCtx.Done():
		c.core.Close()
		<-done
		if firstErr == nil {
			if err := ctx.Err(); err != nil {
				firstErr = mapError("broker close", err)
			} else {
				firstErr = fault.Wrap(fault.DeadlineExpired, "broker drain timed out", drainCtx.Err())
			}
		}
		return firstErr
	}
}

// ready refuses a closed client or an ended context.
func (c *Client) ready(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "broker context is nil")
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return fault.New(fault.DependencyUnavailable, "broker client is closed")
	}
	if err := ctx.Err(); err != nil {
		return mapError("broker operation", err)
	}
	return nil
}

// subscription is one open client subscription.
type subscription struct {
	client *Client
	driver driverSubscription
	cancel context.CancelFunc

	once   sync.Once
	mu     sync.Mutex
	closed bool
	err    error
}

var _ broker.Subscription = (*subscription)(nil)

// Close drains the subscription and joins its drain. It is idempotent.
func (s *subscription) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		s.client.mu.Lock()
		delete(s.client.subs, s)
		s.client.mu.Unlock()

		if s.driver == nil {
			return
		}
		drainCtx, cancelDrain := context.WithTimeout(ctx, s.client.effectiveDrainTimeout())
		defer cancelDrain()

		done := make(chan error, 1)
		go func() { done <- s.driver.Drain() }()
		select {
		case err := <-done:
			if err != nil {
				s.mu.Lock()
				if errors.Is(err, natsgo.ErrDrainTimeout) {
					s.err = fault.Wrap(fault.DeadlineExpired, "broker subscription drain timed out", err)
				} else {
					s.err = mapError("broker subscription drain failed", err)
				}
				s.mu.Unlock()
			}
		case <-drainCtx.Done():
			_ = s.driver.Unsubscribe()
			<-done
			s.mu.Lock()
			if err := ctx.Err(); err != nil {
				s.err = mapError("broker subscription close", err)
			} else {
				s.err = fault.Wrap(fault.DeadlineExpired, "broker subscription drain timed out", drainCtx.Err())
			}
			s.mu.Unlock()
		}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// natsCore adapts a nats.go connection.
type natsCore struct {
	conn *natsgo.Conn
}

func (core *natsCore) Publish(subject string, data []byte) error {
	return core.conn.Publish(subject, data)
}

func (core *natsCore) PublishMsg(subject, reply string, data []byte) error {
	return core.conn.PublishMsg(&natsgo.Msg{Subject: subject, Reply: reply, Data: data})
}

func (core *natsCore) Subscribe(subject string, handler func(string, []byte)) (driverSubscription, error) {
	sub, err := core.conn.Subscribe(subject, func(message *natsgo.Msg) {
		handler(message.Subject, message.Data)
	})
	if err != nil {
		return nil, err
	}
	return &natsSubscription{sub: sub}, nil
}

func (core *natsCore) SubscribeMsg(subject string, handler func(subject, reply string, data []byte)) (driverSubscription, error) {
	sub, err := core.conn.Subscribe(subject, func(message *natsgo.Msg) {
		handler(message.Subject, message.Reply, message.Data)
	})
	if err != nil {
		return nil, err
	}
	return &natsSubscription{sub: sub}, nil
}

func (core *natsCore) Drain() error {
	return core.conn.Drain()
}

func (core *natsCore) Close() {
	core.conn.Close()
}

// natsSubscription adapts a nats.go subscription.
type natsSubscription struct {
	sub *natsgo.Subscription
}

func (sub *natsSubscription) Drain() error {
	return sub.sub.Drain()
}

func (sub *natsSubscription) Unsubscribe() error {
	return sub.sub.Unsubscribe()
}

// natsJetStream adapts the nats.go JetStream publisher.
type natsJetStream struct {
	stream jetstream.JetStream
}

func (publisher *natsJetStream) Publish(ctx context.Context, subject string, payload []byte, msgID string) error {
	_, err := publisher.stream.Publish(ctx, subject, payload, jetstream.WithMsgID(msgID))
	return err
}

// mapError maps a driver error to the seam's fault class. The public message
// never renders the cause, so a URL, token or payload cannot leak.
func mapError(message string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(message, " was canceled"), " failed")
		return fault.Wrap(fault.DeadlineExpired, trimmed+" was canceled", err)
	}
	return fault.Wrap(fault.DependencyUnavailable, message, err)
}
