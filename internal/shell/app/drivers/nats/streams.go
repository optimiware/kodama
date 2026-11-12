package natsdriver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
)

// Stream names and subject filters. They are the single source of truth shared
// by the composition root and the test support: the broker
// adapter publishes to concrete scope-qualified subjects and these wildcard
// filters are the only streams that capture them.
const (
	EventsStreamName        = "KODAMA_EVENTS"
	InvalidationsStreamName = "KODAMA_INVALIDATIONS"

	EventsStreamSubjects        = "kodama.v1.*.*.events"
	InvalidationsStreamSubjects = "kodama.v1.*.*.invalidations"
)

// Default stream retention. Events are kept for a week; invalidations are
// short-lived live facts. Both byte bounds are guardrails, not capacity plans.
const (
	DefaultEventsMaxAge          = 7 * 24 * time.Hour
	DefaultInvalidationsMaxAge   = time.Hour
	DefaultEventsMaxBytes        = int64(4) << 30
	DefaultInvalidationsMaxBytes = int64(512) << 20

	// DefaultDuplicateWindow is the JetStream message-ID deduplication window.
	// The outbox publisher retries a committed event with the same stable event
	// id until the broker acknowledges it, so the window must cover the whole
	// retry window. The composition's injected publication cadence is far below
	// this default; a composition may raise it through StreamConfig, but the
	// window can never exceed the stream's MaxAge because the broker refuses
	// that configuration.
	DefaultDuplicateWindow = time.Hour

	// DefaultReplicas keeps one copy on a single broker. A clustered deployment
	// selects its replica count through StreamConfig when the streams are first
	// created; an existing stream with a different count is refused rather than
	// reconfigured.
	DefaultReplicas = 1
)

// Stream storage and retention values the adapter requires. They are named
// constants so a configuration comparison and a creation stay one source.
const (
	streamStorageFile     = jetstream.FileStorage
	streamRetentionLimits = jetstream.LimitsPolicy
	streamDiscardOld      = jetstream.DiscardOld
)

// errStreamNotFound and errStreamExists are the seam's stream-administration
// outcomes. The production adapter translates the nats.go sentinels; the unit
// fake returns them directly.
var (
	errStreamNotFound = errors.New("jetstream stream not found")
	errStreamExists   = errors.New("jetstream stream already exists")
)

// streamAdmin is the minimal JetStream stream-administration surface this
// wrapper consumes. It is consumer-owned so the unit suite can fake it, and it
// deliberately has no consumer, delete or list method: provisioning can only
// create or update the two named streams.
type streamAdmin interface {
	// StreamConfig returns the stored configuration of one stream. A missing
	// stream is reported as errStreamNotFound.
	StreamConfig(ctx context.Context, name string) (jetstream.StreamConfig, error)
	// CreateStream creates one stream. A concurrent creation of the same name
	// is reported as errStreamExists.
	CreateStream(ctx context.Context, config jetstream.StreamConfig) error
	// UpdateStream replaces one stream's configuration.
	UpdateStream(ctx context.Context, config jetstream.StreamConfig) error
}

// StreamConfig configures the two streams EnsureStreams provisions. The zero
// value selects the documented defaults.
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

// EnsureStreams creates or updates exactly the events and invalidations
// streams the broker adapter publishes into. It is idempotent: a repeat call
// with the same configuration performs no write, and a changed MaxAge, MaxBytes
// or DuplicateWindow is the only update it applies. An existing stream whose
// subjects, storage, retention, discard policy, replicas, append-only flags or
// any unmanaged setting that would weaken delivery or retention differ from the
// declared configuration is refused with VersionConflict and left untouched; it
// is never silently reconfigured into the contract shape or a weaker one.
//
// The call is serialized on the client and is safe to invoke from more than one
// composition root: a concurrent creator that loses the create race is adopted
// after re-reading the stream.
func (c *Client) EnsureStreams(ctx context.Context, config StreamConfig) error {
	if c == nil {
		return fault.New(fault.InvalidInput, "broker client is nil")
	}
	if ctx == nil {
		return fault.New(fault.InvalidInput, "broker context is nil")
	}
	if err := config.validate(); err != nil {
		return err
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if err := c.ready(ctx); err != nil {
		return err
	}
	if c.streams == nil {
		return fault.New(fault.DependencyUnavailable, "broker stream administration is unavailable")
	}
	for _, required := range []jetstream.StreamConfig{
		eventsStreamConfig(config),
		invalidationsStreamConfig(config),
	} {
		if err := c.ensureStream(ctx, required); err != nil {
			return err
		}
	}
	return nil
}

// ensureStream converges one named stream on the required configuration.
func (c *Client) ensureStream(ctx context.Context, required jetstream.StreamConfig) error {
	if err := ctx.Err(); err != nil {
		return mapError("broker stream provisioning", err)
	}
	existing, err := c.streams.StreamConfig(ctx, required.Name)
	switch {
	case err == nil:
	case errors.Is(err, errStreamNotFound):
		createErr := c.streams.CreateStream(ctx, required)
		if createErr == nil {
			return nil
		}
		if !errors.Is(createErr, errStreamExists) {
			return mapError(fmt.Sprintf("broker stream %q creation", required.Name), createErr)
		}
		existing, err = c.streams.StreamConfig(ctx, required.Name)
		if err != nil {
			return mapError(fmt.Sprintf("broker stream %q lookup", required.Name), err)
		}
	default:
		return mapError(fmt.Sprintf("broker stream %q lookup", required.Name), err)
	}
	if reason := identityMismatch(existing, required); reason != "" {
		return fault.New(fault.VersionConflict, fmt.Sprintf(
			"existing stream %q conflicts with the required configuration (%s); refusing to reconfigure it",
			required.Name, reason))
	}
	if !limitsDiffer(existing, required) {
		return nil
	}
	// Start from the stored configuration so the update preserves every field
	// this adapter does not own; only the three limits are replaced.
	updated := existing
	updated.MaxAge = required.MaxAge
	updated.MaxBytes = required.MaxBytes
	updated.Duplicates = required.Duplicates
	if err := c.streams.UpdateStream(ctx, updated); err != nil {
		return mapError(fmt.Sprintf("broker stream %q update", required.Name), err)
	}
	return nil
}

// eventsStreamConfig returns the required events stream. Events are
// append-only facts, so delete, purge and rollup are denied.
func eventsStreamConfig(config StreamConfig) jetstream.StreamConfig {
	retention := config.Events
	if retention.MaxAge == 0 {
		retention.MaxAge = DefaultEventsMaxAge
	}
	if retention.MaxBytes == 0 {
		retention.MaxBytes = DefaultEventsMaxBytes
	}
	if retention.DuplicateWindow == 0 {
		retention.DuplicateWindow = DefaultDuplicateWindow
	}
	return jetstream.StreamConfig{
		Name:        EventsStreamName,
		Subjects:    []string{EventsStreamSubjects},
		Retention:   streamRetentionLimits,
		MaxAge:      retention.MaxAge,
		MaxBytes:    retention.MaxBytes,
		Storage:     streamStorageFile,
		Replicas:    resolveReplicas(config.Replicas),
		Duplicates:  retention.DuplicateWindow,
		Discard:     streamDiscardOld,
		DenyDelete:  true,
		DenyPurge:   true,
		AllowRollup: false,
	}
}

// invalidationsStreamConfig returns the required invalidation stream.
func invalidationsStreamConfig(config StreamConfig) jetstream.StreamConfig {
	retention := config.Invalidations
	if retention.MaxAge == 0 {
		retention.MaxAge = DefaultInvalidationsMaxAge
	}
	if retention.MaxBytes == 0 {
		retention.MaxBytes = DefaultInvalidationsMaxBytes
	}
	if retention.DuplicateWindow == 0 {
		retention.DuplicateWindow = DefaultDuplicateWindow
	}
	return jetstream.StreamConfig{
		Name:       InvalidationsStreamName,
		Subjects:   []string{InvalidationsStreamSubjects},
		Retention:  streamRetentionLimits,
		MaxAge:     retention.MaxAge,
		MaxBytes:   retention.MaxBytes,
		Storage:    streamStorageFile,
		Replicas:   resolveReplicas(config.Replicas),
		Duplicates: retention.DuplicateWindow,
		Discard:    streamDiscardOld,
	}
}

// identityMismatch names the first immutable difference between the stored and
// required stream, or returns the empty string when the identity matches.
// Limits are handled separately because they are the only allowed update.
func identityMismatch(existing, required jetstream.StreamConfig) string {
	switch {
	case existing.Name != required.Name:
		return "name"
	case !sameSubjects(existing.Subjects, required.Subjects):
		return "subjects"
	case existing.Storage != required.Storage:
		return "storage"
	case existing.Retention != required.Retention:
		return "retention"
	case existing.Discard != required.Discard:
		return "discard policy"
	case existing.Replicas != required.Replicas:
		return "replicas"
	case existing.DenyDelete != required.DenyDelete:
		return "deny-delete"
	case existing.DenyPurge != required.DenyPurge:
		return "deny-purge"
	case existing.AllowRollup != required.AllowRollup:
		return "rollup"
	}
	// The adapter does not manage these settings, but a stored stream that has
	// them can be weaker than the required contract: no acknowledgement, an
	// unchangeable sealed stream, a different source, a subject rewrite, a
	// republish side effect, or a finite message limit that drops facts before
	// the configured age/byte bounds.
	switch {
	case existing.NoAck:
		return "no-ack"
	case existing.Sealed:
		return "sealed"
	case existing.Mirror != nil || len(existing.Sources) > 0:
		return "mirror/sources"
	case existing.SubjectTransform != nil:
		return "subject transform"
	case existing.RePublish != nil:
		return "republish"
	case existing.MaxMsgs > 0:
		return "message count limit"
	case existing.MaxMsgsPerSubject > 0:
		return "per-subject message limit"
	case existing.MaxMsgSize > 0:
		return "message size limit"
	default:
		return ""
	}
}

// limitsDiffer reports whether the three allowed update fields changed.
func limitsDiffer(existing, required jetstream.StreamConfig) bool {
	return existing.MaxAge != required.MaxAge ||
		existing.MaxBytes != required.MaxBytes ||
		existing.Duplicates != required.Duplicates
}

// sameSubjects compares stream subject filters as a set: the broker does not
// promise to preserve the declaration order of an equivalent filter list.
func sameSubjects(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := slices.Clone(left)
	b := slices.Clone(right)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func resolveReplicas(replicas int) int {
	if replicas == 0 {
		return DefaultReplicas
	}
	return replicas
}

// validate refuses a configuration the broker cannot accept, before any stream
// is touched.
func (config StreamConfig) validate() error {
	if err := config.Events.validate("events", DefaultEventsMaxAge); err != nil {
		return err
	}
	if err := config.Invalidations.validate("invalidations", DefaultInvalidationsMaxAge); err != nil {
		return err
	}
	if config.Replicas < 0 || config.Replicas > 5 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("stream replicas %d is outside the broker range 0-5", config.Replicas))
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
	if retention.DuplicateWindow > 0 && retention.DuplicateWindow < 100*time.Millisecond {
		return fault.New(fault.InvalidInput, kind+" stream duplicate window is below the broker minimum of 100ms")
	}
	maxAge := retention.MaxAge
	if maxAge == 0 {
		maxAge = defaultMaxAge
	}
	window := retention.DuplicateWindow
	if window == 0 {
		window = DefaultDuplicateWindow
	}
	if window > maxAge {
		return fault.New(fault.InvalidInput, kind+" stream duplicate window exceeds its retention age")
	}
	return nil
}

var _ streamAdmin = (*natsJetStream)(nil)

// StreamConfig reads one stream's stored configuration through the nats.go
// stream manager and translates the not-found sentinel into the seam's own.
func (publisher *natsJetStream) StreamConfig(ctx context.Context, name string) (jetstream.StreamConfig, error) {
	stream, err := publisher.stream.Stream(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return jetstream.StreamConfig{}, errStreamNotFound
		}
		return jetstream.StreamConfig{}, err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return jetstream.StreamConfig{}, err
	}
	return info.Config, nil
}

// CreateStream creates one stream and translates a concurrent creation into the
// seam's already-exists outcome.
func (publisher *natsJetStream) CreateStream(ctx context.Context, config jetstream.StreamConfig) error {
	_, err := publisher.stream.CreateStream(ctx, config)
	if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return errStreamExists
	}
	return err
}

// UpdateStream replaces one stream's configuration.
func (publisher *natsJetStream) UpdateStream(ctx context.Context, config jetstream.StreamConfig) error {
	_, err := publisher.stream.UpdateStream(ctx, config)
	return err
}
