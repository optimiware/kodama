package jetstream_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
)

func TestConfigValidation(t *testing.T) {
	good := jetstream.Config{Namespace: "t005x", Stream: "t005_x", MaxAge: time.Hour, MaxBytes: 1 << 20, MaxMsgs: 1000, DuplicateWindow: time.Minute, Replicas: 1}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*jetstream.Config){
		"stream name":      func(c *jetstream.Config) { c.Stream = "a b" },
		"namespace":        func(c *jetstream.Config) { c.Namespace = "a.>" },
		"unbounded age":    func(c *jetstream.Config) { c.MaxAge = 0 },
		"unbounded bytes":  func(c *jetstream.Config) { c.MaxBytes = 0 },
		"unbounded msgs":   func(c *jetstream.Config) { c.MaxMsgs = 0 },
		"duplicate window": func(c *jetstream.Config) { c.DuplicateWindow = 2 * time.Hour },
		"replicas":         func(c *jetstream.Config) { c.Replicas = 0 },
	}
	for name, mutate := range bad {
		c := good
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := good.SubjectFilter(); got != "t005x.factory.v1.tenant.>" {
		t.Errorf("filter %q", got)
	}
	policy := events.RetentionPolicy{PublishedOutbox: time.Hour, Observation: 48 * time.Hour, Inbox: 48 * time.Hour, ResolvedQuarantine: time.Hour, CompletedReplay: time.Hour,
		BrokerRedelivery: 2 * time.Hour, ReplayWindow: 24 * time.Hour, MaxClockSkew: time.Minute, BatchSize: 10}
	if err := good.ValidateAgainst(policy); err != nil {
		t.Fatal(err)
	}
	policy.BrokerRedelivery = 30 * time.Minute // shorter than the stream max age
	if err := good.ValidateAgainst(policy); !errors.Is(err, events.ErrInvalidConfiguration) {
		t.Fatalf("a stream that can redeliver longer than the inbox covers was accepted: %v", err)
	}
	if err := (jetstream.ConsumerConfig{Consumer: "a", Durable: "d", AckWait: time.Second, FetchWait: time.Second, RetryDelay: time.Second, MaxAckPending: 5, FetchBatch: 6}).Validate(); err == nil {
		t.Error("fetch batch above max ack pending accepted")
	}
}

// TestStreamProvisioning: an existing stream or consumer with incompatible
// settings is refused rather than silently reset; a compatible one is reused.
func TestStreamProvisioning(t *testing.T) {
	broker := eventstest.NewBroker(t)
	cfg := broker.StreamConfig(t, time.Minute)
	nc := broker.Connect(t, broker.Addr())
	ctx := context.Background()
	pub, err := jetstream.NewPublisher(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.EnsureStream(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pub.EnsureStream(ctx); err != nil {
		t.Fatalf("idempotent provisioning: %v", err)
	}
	js, _ := natsjs.New(nc)
	stream, err := js.Stream(ctx, cfg.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if sc := stream.CachedInfo().Config; sc.Storage != natsjs.FileStorage || sc.MaxAge != cfg.MaxAge || sc.MaxBytes != cfg.MaxBytes || sc.Retention != natsjs.LimitsPolicy {
		t.Fatalf("stream is not file-backed and bounded: %+v", sc)
	}

	for name, mutate := range map[string]func(*jetstream.Config){
		"max age":          func(c *jetstream.Config) { c.MaxAge = 2 * time.Hour },
		"max bytes":        func(c *jetstream.Config) { c.MaxBytes = 1 << 20 },
		"duplicate window": func(c *jetstream.Config) { c.DuplicateWindow = 2 * time.Minute },
	} {
		other := cfg
		mutate(&other)
		p, err := jetstream.NewPublisher(nc, other)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.EnsureStream(ctx); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Errorf("incompatible %s: %v", name, err)
		}
	}
	// The existing stream was left untouched by the refused attempts.
	again, _ := js.Stream(ctx, cfg.Stream)
	if again.CachedInfo().Config.MaxAge != cfg.MaxAge {
		t.Fatal("an incompatible provisioning attempt changed the stream")
	}

	cc := jetstream.ConsumerConfig{Consumer: "audit", Durable: "t005_prov_" + eventstest.RandomHex(t, 3), AckWait: 5 * time.Second, MaxAckPending: 10, FetchBatch: 5, FetchWait: time.Second, RetryDelay: time.Second}
	newConsumer := func(c jetstream.ConsumerConfig) *jetstream.Consumer {
		cons, err := jetstream.NewConsumer(nc, cfg, c, resolver{}, processor{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cons
	}
	if err := newConsumer(cc).Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if err := newConsumer(cc).Provision(ctx); err != nil {
		t.Fatalf("idempotent consumer provisioning: %v", err)
	}
	changed := cc
	changed.AckWait = 9 * time.Second
	if err := newConsumer(changed).Provision(ctx); !errors.Is(err, events.ErrInvalidConfiguration) {
		t.Fatalf("incompatible consumer: %v", err)
	}

	// A durable that matches every limit but changes what or how the adapter
	// receives is refused and left untouched.
	filter := cfg.SubjectFilter()
	legacy, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	base := func(name string) natsjs.ConsumerConfig {
		return natsjs.ConsumerConfig{Durable: name, AckPolicy: natsjs.AckExplicitPolicy, DeliverPolicy: natsjs.DeliverAllPolicy,
			AckWait: cc.AckWait, MaxAckPending: cc.MaxAckPending, MaxDeliver: -1, FilterSubject: filter}
	}
	unsafe := map[string]func(*natsjs.ConsumerConfig){
		"headers only":  func(c *natsjs.ConsumerConfig) { c.HeadersOnly = true },
		"push consumer": func(c *natsjs.ConsumerConfig) { c.DeliverSubject = "t005.push." + eventstest.RandomHex(t, 3) },
		"deliver new":   func(c *natsjs.ConsumerConfig) { c.DeliverPolicy = natsjs.DeliverNewPolicy },
		"deliver last":  func(c *natsjs.ConsumerConfig) { c.DeliverPolicy = natsjs.DeliverLastPolicy },
		"start sequence": func(c *natsjs.ConsumerConfig) {
			c.DeliverPolicy = natsjs.DeliverByStartSequencePolicy
			c.OptStartSeq = 1
		},
		"replay original":    func(c *natsjs.ConsumerConfig) { c.ReplayPolicy = natsjs.ReplayOriginalPolicy },
		"backoff":            func(c *natsjs.ConsumerConfig) { c.BackOff = []time.Duration{time.Second}; c.MaxDeliver = 5 },
		"memory storage":     func(c *natsjs.ConsumerConfig) { c.MemoryStorage = true },
		"inactive threshold": func(c *natsjs.ConsumerConfig) { c.InactiveThreshold = time.Hour },
		"max request bytes":  func(c *natsjs.ConsumerConfig) { c.MaxRequestMaxBytes = 1024 },
		// A finite broker limit would strand a delivery that could not be
		// settled durably (for example while PostgreSQL is down).
		"finite max deliver": func(c *natsjs.ConsumerConfig) { c.MaxDeliver = 1 },
	}
	for name, mutate := range unsafe {
		durable := "t005_unsafe_" + eventstest.RandomHex(t, 4)
		existing := base(durable)
		mutate(&existing)
		if _, err := js.CreateConsumer(ctx, cfg.Stream, existing); err != nil {
			t.Fatalf("%s: pre-create: %v", name, err)
		}
		before, err := legacy.ConsumerInfo(cfg.Stream, durable)
		if err != nil {
			t.Fatal(err)
		}
		want := before.Config
		c := cc
		c.Durable = durable
		if err := newConsumer(c).Provision(ctx); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Errorf("%s: incompatible existing consumer accepted: %v", name, err)
		}
		after, err := legacy.ConsumerInfo(cfg.Stream, durable)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after.Config, want) {
			t.Errorf("%s: provisioning modified the existing consumer", name)
		}
	}
	// A consumer created exactly as the adapter would is accepted.
	exact := base("t005_exact_" + eventstest.RandomHex(t, 4))
	if _, err := js.CreateConsumer(ctx, cfg.Stream, exact); err != nil {
		t.Fatal(err)
	}
	c := cc
	c.Durable = exact.Durable
	if err := newConsumer(c).Provision(ctx); err != nil {
		t.Errorf("compatible hand-made consumer refused: %v", err)
	}
}

// TestStreamRefusesBehaviorChangingSettings: an existing stream that matches
// every limit the adapter configures but changes what is stored or whether a
// publish is acknowledged is refused by both provisioning paths and is left
// untouched. Real JetStream.
func TestStreamRefusesBehaviorChangingSettings(t *testing.T) {
	broker := eventstest.NewBroker(t)
	nc := broker.Connect(t, broker.Addr())
	ctx := context.Background()
	js, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(cfg jetstream.Config, sc *natsjs.StreamConfig){
		"no ack": func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.NoAck = true },
		"subject transform": func(cfg jetstream.Config, sc *natsjs.StreamConfig) {
			sc.SubjectTransform = &natsjs.SubjectTransformConfig{Source: cfg.SubjectFilter(), Destination: "moved." + cfg.Namespace + ".>"}
		},
		"discard new":            func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.Discard = natsjs.DiscardNew },
		"max messages / subject": func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.MaxMsgsPerSubject = 1 },
		"small max message":      func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.MaxMsgSize = 1024 },
		"max consumers":          func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.MaxConsumers = 1 },
		"async persistence":      func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.PersistMode = natsjs.AsyncPersistMode },
		"consumer inactivity":    func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.ConsumerLimits.InactiveThreshold = time.Hour },
		"consumer ack cap":       func(_ jetstream.Config, sc *natsjs.StreamConfig) { sc.ConsumerLimits.MaxAckPending = 3 },
		"sealed":                 nil, // sealed after creation, below
	}
	for name, mutate := range cases {
		cfg := broker.StreamConfig(t, time.Minute)
		sc := natsjs.StreamConfig{
			Name: cfg.Stream, Subjects: []string{cfg.SubjectFilter()}, Storage: natsjs.FileStorage, Retention: natsjs.LimitsPolicy,
			Discard: natsjs.DiscardOld, MaxAge: cfg.MaxAge, MaxBytes: cfg.MaxBytes, MaxMsgs: cfg.MaxMsgs, Duplicates: cfg.DuplicateWindow, Replicas: cfg.Replicas,
		}
		sealed := mutate == nil
		if !sealed {
			mutate(cfg, &sc)
		}
		stream, err := js.CreateStream(ctx, sc)
		if err != nil {
			t.Fatalf("%s: pre-create: %v", name, err)
		}
		if sealed {
			sc.Sealed = true
			if stream, err = js.UpdateStream(ctx, sc); err != nil {
				t.Fatalf("%s: seal: %v", name, err)
			}
		}
		want := stream.CachedInfo().Config

		pub, err := jetstream.NewPublisher(nc, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := pub.EnsureStream(ctx); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Errorf("%s: EnsureStream accepted the stream: %v", name, err)
		}
		cons, err := jetstream.NewConsumer(nc, cfg, jetstream.ConsumerConfig{Consumer: "audit", Durable: "t005_beh_" + eventstest.RandomHex(t, 3),
			AckWait: 5 * time.Second, MaxAckPending: 10, FetchBatch: 5, FetchWait: time.Second, RetryDelay: time.Second}, resolver{}, processor{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cons.Provision(ctx); !errors.Is(err, events.ErrInvalidConfiguration) {
			t.Errorf("%s: Provision accepted the stream: %v", name, err)
		}
		after, err := js.Stream(ctx, cfg.Stream)
		if err != nil {
			t.Fatal(err)
		}
		info := after.CachedInfo()
		if !reflect.DeepEqual(info.Config, want) {
			t.Errorf("%s: provisioning modified the stream", name)
		}
		if info.State.Consumers != 0 {
			t.Errorf("%s: provisioning created a consumer on a refused stream", name)
		}
	}
}

type resolver struct{}

func (resolver) ResolveSubjectToken(context.Context, string) (domainTenant, error) {
	return "", events.ErrUnknownSubject
}

type processor struct{}

func (processor) Process(context.Context, events.Delivery) (events.Outcome, error) {
	return events.Outcome{Kind: events.OutcomeCompleted}, nil
}

type domainTenant = domain.TenantID
