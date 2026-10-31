package natsbus

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

	"github.com/optimiware/kodama/internal/shell/bus"
)

// JetStreamConfig describes the event stream.
type JetStreamConfig struct {
	// Stream is the stream name (default KODAMA).
	Stream string
	// MaxAge bounds how long a message is kept (default 7 days): the longest a
	// consumer can be down and still receive it, and the horizon of a replay.
	MaxAge time.Duration
	// Duplicates is the broker's message-ID deduplication window (default 10
	// minutes): an optimization, correctness never depends on it.
	Duplicates time.Duration
	// AckWait is how long a delivery may take before it is redelivered (default 1m).
	AckWait time.Duration
	// RetryDelay is the redelivery delay after a handler error (default 5s).
	RetryDelay time.Duration
	// Subjects is the subject filter the stream captures (default
	// kodama.v1.>, every tenant). Another filter, which must lie under
	// kodama.v1., exists so that streams of parallel tests do not overlap.
	Subjects string
	Logger   *slog.Logger
}

var (
	streamName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	consumerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}[a-z0-9]$`)
	nameToken    = regexp.MustCompile(`^[a-z][a-z0-9]{0,23}(-[a-z0-9]{1,24}){0,2}$`)
)

// ConsumerName is the durable consumer name of a purpose inside a service, for
// example ConsumerName("shell", "run-admission") = "kodama-shell-run-admission".
// Every process of a service uses the same name so that deliveries are shared
// and a restart resumes where it stopped; a different purpose never shares
// another's cursor. Each part is lower-case letters, digits and single hyphens.
func ConsumerName(service, purpose string) (string, error) {
	if !nameToken.MatchString(service) || !nameToken.MatchString(purpose) {
		return "", fmt.Errorf("natsbus: consumer parts %q and %q must be lower-case words", truncate(service), truncate(purpose))
	}
	return "kodama-" + service + "-" + purpose, nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

func (c *JetStreamConfig) defaults() error {
	if c.Stream == "" {
		c.Stream = "KODAMA"
	}
	if !streamName.MatchString(c.Stream) {
		return fmt.Errorf("natsbus: stream name %q is invalid", c.Stream)
	}
	if c.Subjects == "" {
		c.Subjects = bus.SubjectRoot + ".>"
	}
	if !strings.HasPrefix(c.Subjects, bus.SubjectRoot+".") {
		return fmt.Errorf("natsbus: stream subjects %q must lie under %s", c.Subjects, bus.SubjectRoot)
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 7 * 24 * time.Hour
	}
	if c.Duplicates <= 0 {
		c.Duplicates = 10 * time.Minute
	}
	if c.Duplicates > c.MaxAge {
		return errors.New("natsbus: the duplicate window cannot exceed the stream's max age")
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

// JetStream is the NATS JetStream bus. Publish waits for the stream's durable
// acknowledgment under the event's stable ID as the broker message ID; Subscribe
// is a durable explicit-acknowledgment consumer that acknowledges after the
// handler returns nil.
type JetStream struct {
	js  natsjs.JetStream
	cfg JetStreamConfig
}

var _ bus.Bus = (*JetStream)(nil)

// NewJetStream wraps an injected connection and ensures the stream exists (file
// storage, limits retention).
func NewJetStream(ctx context.Context, nc *nats.Conn, cfg JetStreamConfig) (*JetStream, error) {
	if nc == nil {
		return nil, errors.New("natsbus: a NATS connection is required")
	}
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natsbus: jetstream: %w", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, natsjs.StreamConfig{
		Name: cfg.Stream, Subjects: []string{cfg.Subjects}, Storage: natsjs.FileStorage,
		Retention: natsjs.LimitsPolicy, Discard: natsjs.DiscardOld, MaxAge: cfg.MaxAge, Duplicates: cfg.Duplicates,
	}); err != nil {
		return nil, fmt.Errorf("natsbus: stream %s: %w", cfg.Stream, err)
	}
	return &JetStream{js: js, cfg: cfg}, nil
}

// Publish sends the message and returns nil only after the stream acknowledged it.
func (j *JetStream) Publish(ctx context.Context, subject string, m bus.Message) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}
	ack, err := j.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: data}, natsjs.WithMsgID(m.ID), natsjs.WithExpectStream(j.cfg.Stream))
	if err != nil {
		return fmt.Errorf("natsbus: publish %s: %w", m.ID, err)
	}
	if ack.Stream != j.cfg.Stream {
		return fmt.Errorf("natsbus: published to stream %q, want %q", ack.Stream, j.cfg.Stream)
	}
	return nil
}

// Subscribe starts the durable consumer name on the filter. A name is a
// ConsumerName; it is checked here so that no consumer shares a cursor by
// accident. The handler runs under a context that ends when stop is called.
func (j *JetStream) Subscribe(ctx context.Context, name, filter string, h bus.Handler) (func(), error) {
	if filter == "" || h == nil {
		return nil, errors.New("natsbus: a consumer needs a name, a filter and a handler")
	}
	if !consumerName.MatchString(name) {
		return nil, fmt.Errorf("natsbus: consumer name %q is not a durable name (use ConsumerName)", truncate(name))
	}
	cons, err := j.js.CreateOrUpdateConsumer(ctx, j.cfg.Stream, natsjs.ConsumerConfig{
		Durable: name, FilterSubject: filter, AckPolicy: natsjs.AckExplicitPolicy, AckWait: j.cfg.AckWait,
		DeliverPolicy: natsjs.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("natsbus: consumer %s: %w", name, err)
	}
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cc, err := cons.Consume(func(msg natsjs.Msg) {
		m, err := bus.DecodeMessage(msg.Data())
		if err != nil {
			j.cfg.Logger.Warn("event message terminated", "consumer", name, "error", err)
			_ = msg.Term() // a malformed message cannot be fixed by redelivery
			return
		}
		if err := h(hctx, m); err != nil {
			j.cfg.Logger.Warn("event handler failed; the delivery repeats", "consumer", name, "event", m.ID, "error", err)
			_ = msg.NakWithDelay(j.cfg.RetryDelay)
			return
		}
		if err := msg.Ack(); err != nil {
			j.cfg.Logger.Warn("event acknowledgment failed; the delivery may repeat", "consumer", name, "event", m.ID, "error", err)
		}
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("natsbus: consume %s: %w", name, err)
	}
	return func() { cc.Stop(); cancel() }, nil
}
