package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"
)

// JetStreamConfig describes the kernel's event stream.
//
// Superseded by natsbus.JetStreamConfig in internal/natsbus. This copy remains
// only until internal/shell/serve is repointed (T-188 follow-up); do not add
// users.
type JetStreamConfig struct {
	// Subjects is the subject filter the stream captures (default kodama.v1.>).
	// A narrower filter under kodama.v1. keeps the streams of parallel suites
	// that share a broker from overlapping.
	Subjects string
	// Stream is the stream name (default KODAMA).
	Stream string
	// MaxAge bounds how long a message is kept (default 7 days): the longest a
	// consumer can be down and still receive it.
	MaxAge time.Duration
	// Duplicates is the broker's message-ID deduplication window (default 10
	// minutes): an optimization, correctness never depends on it.
	Duplicates time.Duration
	// AckWait is how long a delivery may take before it is redelivered (default 1m).
	AckWait time.Duration
	// RetryDelay is the redelivery delay after a handler error (default 5s).
	RetryDelay time.Duration
	Logger     *slog.Logger
}

var streamName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (c *JetStreamConfig) defaults() error {
	if c.Stream == "" {
		c.Stream = "KODAMA"
	}
	if !streamName.MatchString(c.Stream) {
		return fmt.Errorf("bus: stream name %q is invalid", c.Stream)
	}
	if c.Subjects == "" {
		c.Subjects = SubjectRoot + ".>"
	}
	if !strings.HasPrefix(c.Subjects, SubjectRoot+".") {
		return fmt.Errorf("bus: stream subjects %q must lie under %s", c.Subjects, SubjectRoot)
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 7 * 24 * time.Hour
	}
	if c.Duplicates <= 0 {
		c.Duplicates = 10 * time.Minute
	}
	if c.Duplicates > c.MaxAge {
		return errors.New("bus: the duplicate window cannot exceed the stream's max age")
	}
	if c.AckWait <= 0 {
		c.AckWait = time.Minute
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return nil
}

// JetStream is the NATS JetStream Bus. Publish waits for the stream's durable
// acknowledgment under the event's stable ID as the broker message ID; Subscribe
// is a durable explicit-acknowledgment consumer that acknowledges after the
// handler returns nil.
type JetStream struct {
	js  natsjs.JetStream
	cfg JetStreamConfig
}

var _ Bus = (*JetStream)(nil)

// NewJetStream wraps an injected connection and ensures the stream exists (file
// storage, limits retention).
func NewJetStream(ctx context.Context, nc *nats.Conn, cfg JetStreamConfig) (*JetStream, error) {
	if nc == nil {
		return nil, errors.New("bus: a NATS connection is required")
	}
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return nil, fmt.Errorf("bus: jetstream: %w", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, natsjs.StreamConfig{
		Name: cfg.Stream, Subjects: []string{cfg.Subjects}, Storage: natsjs.FileStorage,
		Retention: natsjs.LimitsPolicy, Discard: natsjs.DiscardOld, MaxAge: cfg.MaxAge, Duplicates: cfg.Duplicates,
	}); err != nil {
		return nil, fmt.Errorf("bus: stream %s: %w", cfg.Stream, err)
	}
	return &JetStream{js: js, cfg: cfg}, nil
}

// Publish sends the message and returns nil only after the stream acknowledged it.
func (j *JetStream) Publish(ctx context.Context, subject string, m Message) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}
	ack, err := j.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: data}, natsjs.WithMsgID(m.ID), natsjs.WithExpectStream(j.cfg.Stream))
	if err != nil {
		return fmt.Errorf("bus: publish %s: %w", m.ID, err)
	}
	if ack.Stream != j.cfg.Stream {
		return fmt.Errorf("bus: published to stream %q, want %q", ack.Stream, j.cfg.Stream)
	}
	return nil
}

// Subscribe starts a durable consumer named name on the filter.
func (j *JetStream) Subscribe(ctx context.Context, name, filter string, h Handler) (func(), error) {
	if name == "" || filter == "" || h == nil {
		return nil, errors.New("bus: a consumer needs a name, a filter and a handler")
	}
	cons, err := j.js.CreateOrUpdateConsumer(ctx, j.cfg.Stream, natsjs.ConsumerConfig{
		Durable: name, FilterSubject: filter, AckPolicy: natsjs.AckExplicitPolicy, AckWait: j.cfg.AckWait,
		DeliverPolicy: natsjs.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("bus: consumer %s: %w", name, err)
	}
	cc, err := cons.Consume(func(msg natsjs.Msg) {
		m, err := DecodeMessage(msg.Data())
		if err != nil {
			j.cfg.Logger.Warn("event message terminated", "consumer", name, "error", err)
			_ = msg.Term() // a malformed message cannot be fixed by redelivery
			return
		}
		if err := h(context.Background(), m); err != nil {
			j.cfg.Logger.Warn("event handler failed; the delivery repeats", "consumer", name, "event", m.ID, "error", err)
			_ = msg.NakWithDelay(j.cfg.RetryDelay)
			return
		}
		if err := msg.Ack(); err != nil {
			j.cfg.Logger.Warn("event acknowledgment failed; the delivery may repeat", "consumer", name, "event", m.ID, "error", err)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("bus: consume %s: %w", name, err)
	}
	return cc.Stop, nil
}
