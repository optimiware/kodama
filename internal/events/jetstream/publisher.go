package jetstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/events"
)

var _ events.BrokerPublisher = (*Publisher)(nil)

// Publisher publishes outbox messages to the event stream and waits for
// durable acknowledgment. It implements events.BrokerPublisher.
type Publisher struct {
	js  natsjs.JetStream
	cfg Config
}

// NewPublisher wraps an injected connection.
func NewPublisher(nc *nats.Conn, cfg Config) (*Publisher, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: a NATS connection is required", events.ErrInvalidConfiguration)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &Publisher{js: js, cfg: cfg}, nil
}

// EnsureStream creates the bounded file-backed stream, or validates an existing
// one and refuses incompatible settings (it never updates or deletes it).
func (p *Publisher) EnsureStream(ctx context.Context) error {
	return ensureStream(ctx, p.js, p.cfg)
}

func ensureStream(ctx context.Context, js natsjs.JetStream, cfg Config) error {
	stream, err := js.Stream(ctx, cfg.Stream)
	if errors.Is(err, natsjs.ErrStreamNotFound) {
		if _, err := js.CreateStream(ctx, cfg.streamConfig()); err != nil {
			return fmt.Errorf("jetstream: create stream %s: %w", cfg.Stream, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("jetstream: look up stream %s: %w", cfg.Stream, err)
	}
	return cfg.compatible(stream.CachedInfo().Config)
}

// Publish sends one message with the event ID as the broker's message-ID and
// returns nil only after the stream's durable acknowledgment. Everything else,
// including a timeout after the request left (the outcome is then unknown), is
// an error that keeps the outbox row retryable; the exception is a message the
// broker can never accept, reported as permanent.
func (p *Publisher) Publish(ctx context.Context, m events.Message) error {
	subject, err := events.Subject(p.cfg.Namespace, m.SubjectToken, m.EventType)
	if err != nil {
		return events.Permanent(err)
	}
	msg := &nats.Msg{Subject: subject, Data: m.Data}
	ack, err := p.js.PublishMsg(ctx, msg, natsjs.WithMsgID(m.ID), natsjs.WithExpectStream(p.cfg.Stream))
	if err != nil {
		if errors.Is(err, nats.ErrMaxPayload) {
			return events.Permanent(err)
		}
		return events.Transient(err)
	}
	if ack.Stream != p.cfg.Stream {
		return events.Transient(fmt.Errorf("published to stream %q, want %q", ack.Stream, p.cfg.Stream))
	}
	return nil
}
