// Package jetstream is the NATS JetStream adapter of the durable event bus: a
// publisher that waits for durable broker acknowledgment under the stable event
// ID, and an explicit-acknowledgment durable consumer that acknowledges only
// after the PostgreSQL adapter recorded a durable outcome.
//
// The caller injects the NATS connection (the token comes from the existing
// `nats-token` secret contract and endpoint from KODAMA_NATS_URL); this package
// reads no environment and adds no process configuration. Streams are
// file-backed and bounded. Provisioning validates an existing stream or
// consumer and refuses incompatible settings instead of silently resetting it.
package jetstream

import (
	"fmt"
	"regexp"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
)

var (
	streamNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	namespacePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}(\.[a-z0-9][a-z0-9_-]{0,31}){0,3}$`)
	durableNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Config describes the event stream. Every limit is explicit; there are no
// defaults, so a bounded stream is a deliberate operator choice.
type Config struct {
	// Namespace is an optional subject prefix (for example a test or
	// environment scope). The tail after it is always `factory.v1.tenant.>`.
	Namespace string
	// Stream is the stream name.
	Stream string
	// MaxAge, MaxBytes and MaxMsgs bound the stream; the oldest messages are
	// discarded. MaxAge is the longest a message can be redelivered, so the
	// inbox retention must cover it (see ValidateAgainst).
	MaxAge   time.Duration
	MaxBytes int64
	MaxMsgs  int64
	// DuplicateWindow is the broker's message-ID deduplication window. It is an
	// optimization: correctness never depends on it.
	DuplicateWindow time.Duration
	// Replicas is the stream replica count.
	Replicas int
}

// Validate checks the stream settings.
func (c Config) Validate() error {
	switch {
	case !streamNamePattern.MatchString(c.Stream):
		return fmt.Errorf("%w: stream name must match %s", events.ErrInvalidConfiguration, streamNamePattern)
	case c.Namespace != "" && !namespacePattern.MatchString(c.Namespace):
		return fmt.Errorf("%w: subject namespace must match %s", events.ErrInvalidConfiguration, namespacePattern)
	case c.MaxAge <= 0 || c.MaxBytes <= 0 || c.MaxMsgs <= 0:
		return fmt.Errorf("%w: stream max age, bytes and messages must be positive", events.ErrInvalidConfiguration)
	case c.DuplicateWindow <= 0 || c.DuplicateWindow > c.MaxAge:
		return fmt.Errorf("%w: duplicate window must be positive and not exceed the stream max age", events.ErrInvalidConfiguration)
	case c.Replicas < 1 || c.Replicas > 5:
		return fmt.Errorf("%w: replicas must be between 1 and 5", events.ErrInvalidConfiguration)
	}
	return nil
}

// ValidateAgainst requires the broker redelivery horizon (stream max age) to be
// covered by the inbox deduplication retention, so a late redelivery can never
// repeat an effect whose inbox evidence was already pruned.
func (c Config) ValidateAgainst(r events.RetentionPolicy) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if c.MaxAge > r.BrokerRedelivery {
		return fmt.Errorf("%w: stream max age %s exceeds the configured broker redelivery bound %s", events.ErrInvalidConfiguration, c.MaxAge, r.BrokerRedelivery)
	}
	return nil
}

// SubjectFilter is the stream's subject: every tenant and event type.
func (c Config) SubjectFilter() string {
	root := events.SubjectRoot + ".>"
	if c.Namespace != "" {
		return c.Namespace + "." + root
	}
	return root
}

func (c Config) streamConfig() natsjs.StreamConfig {
	return natsjs.StreamConfig{
		Name:       c.Stream,
		Subjects:   []string{c.SubjectFilter()},
		Storage:    natsjs.FileStorage,
		Retention:  natsjs.LimitsPolicy,
		Discard:    natsjs.DiscardOld,
		MaxAge:     c.MaxAge,
		MaxBytes:   c.MaxBytes,
		MaxMsgs:    c.MaxMsgs,
		Duplicates: c.DuplicateWindow,
		Replicas:   c.Replicas,
	}
}

// compatible reports the first difference between an existing stream and the
// wanted configuration.
func (c Config) compatible(got natsjs.StreamConfig) error {
	want := c.streamConfig()
	diff := func(name string, a, b any) error {
		return fmt.Errorf("%w: stream %s has %s %v, want %v; reconfigure deliberately instead of resetting", events.ErrInvalidConfiguration, c.Stream, name, a, b)
	}
	switch {
	case len(got.Subjects) != 1 || got.Subjects[0] != want.Subjects[0]:
		return diff("subjects", got.Subjects, want.Subjects)
	case got.Storage != want.Storage:
		return diff("storage", got.Storage, want.Storage)
	case got.Retention != want.Retention:
		return diff("retention", got.Retention, want.Retention)
	case got.MaxAge != want.MaxAge:
		return diff("max age", got.MaxAge, want.MaxAge)
	case got.MaxBytes != want.MaxBytes:
		return diff("max bytes", got.MaxBytes, want.MaxBytes)
	case got.MaxMsgs != want.MaxMsgs:
		return diff("max messages", got.MaxMsgs, want.MaxMsgs)
	case got.Duplicates != want.Duplicates:
		return diff("duplicate window", got.Duplicates, want.Duplicates)
	case got.Replicas != want.Replicas:
		return diff("replicas", got.Replicas, want.Replicas)
	// The settings below change what the stream stores or whether a publish
	// is acknowledged, not just how much it keeps. A stream that matches every
	// limit above but sets one of them is still unusable by this adapter.
	case got.NoAck:
		// Publishes would never be acknowledged: every attempt times out
		// while the message is stored, so the outbox could not drain.
		return diff("no-ack", got.NoAck, false)
	case got.SubjectTransform != nil:
		// Stored subjects would no longer carry the tenant token and event type
		// the consumer filter and trusted tenant resolution rely on.
		return diff("subject transform", *got.SubjectTransform, "none")
	case got.Mirror != nil || len(got.Sources) != 0:
		return diff("mirror or sources", got.Mirror, "an origin stream that accepts direct publishes")
	case got.Sealed:
		return diff("sealed", got.Sealed, false)
	case got.Discard != want.Discard:
		return diff("discard policy", got.Discard, want.Discard)
	case got.DiscardNewPerSubject:
		return diff("discard new per subject", got.DiscardNewPerSubject, false)
	case got.MaxMsgsPerSubject > 0:
		return diff("max messages per subject", got.MaxMsgsPerSubject, "unlimited (only the stream bounds apply)")
	case got.MaxMsgSize > 0 && got.MaxMsgSize < eventsv1.MaxEnvelopeBytes+maxMessageOverhead:
		return diff("max message size", got.MaxMsgSize, "at least the largest envelope")
	case got.MaxConsumers > 0:
		return diff("max consumers", got.MaxConsumers, "unlimited")
	case got.AllowMsgCounter:
		return diff("message counter", got.AllowMsgCounter, false)
	case got.PersistMode != natsjs.DefaultPersistMode:
		// Asynchronous persistence acknowledges before the message is durable.
		return diff("persist mode", got.PersistMode, natsjs.DefaultPersistMode)
	case got.ConsumerLimits.InactiveThreshold != 0 || got.ConsumerLimits.MaxAckPending != 0:
		return diff("consumer limits", got.ConsumerLimits, "none (durable consumers must not expire or be capped)")
	}
	return nil
}

// maxMessageOverhead is the room above the largest envelope that a stream's
// per-message size limit must leave for the subject and headers.
const maxMessageOverhead = 4 << 10

// ConsumerConfig describes one durable pull consumer.
type ConsumerConfig struct {
	// Consumer is the inbox consumer identity (events.ConsumerSpec.Name).
	Consumer string
	// Durable is the JetStream durable name.
	Durable string
	// AckWait is how long an unacknowledged delivery waits before redelivery.
	AckWait time.Duration
	// MaxAckPending and FetchBatch bound in-flight work; FetchBatch must not
	// exceed MaxAckPending.
	MaxAckPending int
	FetchBatch    int
	// FetchWait is the longest one fetch blocks.
	FetchWait time.Duration
	// RetryDelay is the redelivery delay after the processor recorded no durable
	// outcome (for example PostgreSQL was unavailable).
	RetryDelay time.Duration
}

// The adapter always asks for unlimited broker redelivery and refuses an
// existing consumer that limits it. A finite limit would make the broker stop
// delivering a message the processor could not settle durably (for example
// PostgreSQL being down while a poison message is quarantined), and nothing
// else records such a delivery: only the inbox attempt policy, which runs after
// a durable outcome exists, may decide when to quarantine.

// Validate checks the consumer settings.
func (c ConsumerConfig) Validate() error {
	switch {
	case c.Consumer == "" || !durableNamePattern.MatchString(c.Durable):
		return fmt.Errorf("%w: consumer needs an identity and a durable name matching %s", events.ErrInvalidConfiguration, durableNamePattern)
	case c.AckWait <= 0 || c.FetchWait <= 0 || c.RetryDelay <= 0:
		return fmt.Errorf("%w: ack wait, fetch wait and retry delay must be positive", events.ErrInvalidConfiguration)
	case c.MaxAckPending < 1 || c.FetchBatch < 1 || c.FetchBatch > c.MaxAckPending:
		return fmt.Errorf("%w: fetch batch must be between 1 and max ack pending", events.ErrInvalidConfiguration)
	}
	return nil
}

func (c ConsumerConfig) consumerConfig(filter string) natsjs.ConsumerConfig {
	return natsjs.ConsumerConfig{
		Durable:       c.Durable,
		AckPolicy:     natsjs.AckExplicitPolicy,
		DeliverPolicy: natsjs.DeliverAllPolicy,
		AckWait:       c.AckWait,
		MaxAckPending: c.MaxAckPending,
		MaxDeliver:    -1,
		FilterSubject: filter,
	}
}

func (c ConsumerConfig) compatible(got natsjs.ConsumerConfig, filter string, replicas int) error {
	want := c.consumerConfig(filter)
	bad := func(name string, a, b any) error {
		return fmt.Errorf("%w: consumer %s has %s %v, want %v; reconfigure deliberately instead of resetting", events.ErrInvalidConfiguration, c.Durable, name, a, b)
	}
	switch {
	case got.AckPolicy != natsjs.AckExplicitPolicy:
		return bad("ack policy", got.AckPolicy, want.AckPolicy)
	case got.FilterSubject != filter || len(got.FilterSubjects) != 0:
		return bad("filter", got.FilterSubject, filter)
	case got.AckWait != want.AckWait:
		return bad("ack wait", got.AckWait, want.AckWait)
	case got.MaxAckPending != want.MaxAckPending:
		return bad("max ack pending", got.MaxAckPending, want.MaxAckPending)
	case got.MaxDeliver != -1: // unlimited; see ConsumerConfig
		return bad("max deliver", got.MaxDeliver, "unlimited (-1)")
	// The settings below change what or how the adapter receives, not just
	// its limits. A matching durable name does not make such a consumer safe.
	case got.DeliverSubject != "" || got.DeliverGroup != "" || got.FlowControl || got.IdleHeartbeat != 0:
		return bad("delivery mode (push consumer)", got.DeliverSubject, "a pull consumer without a delivery subject")
	case got.HeadersOnly:
		return bad("headers only", got.HeadersOnly, false)
	case got.DeliverPolicy != want.DeliverPolicy:
		return bad("deliver policy", got.DeliverPolicy, want.DeliverPolicy)
	case got.OptStartSeq != 0 || got.OptStartTime != nil:
		return bad("start position", got.OptStartSeq, "none")
	case got.ReplayPolicy != natsjs.ReplayInstantPolicy:
		return bad("replay policy", got.ReplayPolicy, natsjs.ReplayInstantPolicy)
	case len(got.BackOff) != 0:
		return bad("backoff", got.BackOff, "none (ack wait governs redelivery)")
	case got.RateLimit != 0:
		return bad("rate limit", got.RateLimit, 0)
	case got.MemoryStorage:
		return bad("memory storage", got.MemoryStorage, false)
	case got.Replicas != 0 && got.Replicas != replicas: // 0 inherits the stream
		return bad("replicas", got.Replicas, replicas)
	case got.PauseUntil != nil && got.PauseUntil.After(time.Now()):
		return bad("paused until", got.PauseUntil, "not paused")
	case got.PriorityPolicy != natsjs.PriorityPolicyNone || len(got.PriorityGroups) != 0:
		return bad("priority policy", got.PriorityPolicy, "none")
	case got.InactiveThreshold != 0:
		return bad("inactive threshold", got.InactiveThreshold, "durable consumers must not expire")
	case got.MaxRequestBatch != 0 && got.MaxRequestBatch < c.FetchBatch:
		return bad("max request batch", got.MaxRequestBatch, "at least the fetch batch")
	case got.MaxRequestMaxBytes != 0:
		return bad("max request bytes", got.MaxRequestMaxBytes, 0)
	case got.MaxRequestExpires != 0 && got.MaxRequestExpires < c.FetchWait:
		return bad("max request expires", got.MaxRequestExpires, "at least the fetch wait")
	}
	return nil
}
